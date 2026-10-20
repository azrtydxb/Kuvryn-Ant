package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backupfence"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
)

// BlobRef identifies a blob inside one deduplication domain.
type BlobRef struct {
	Domain string `json:"domain"`
	Digest string `json:"digest"`
	Kind   string `json:"kind"` // row: unreferenced metadata row; object: bytes without a row
}

// GCReport describes one garbage-collection run. In dry-run mode Marked and
// Swept list exactly what a real run started now would mark and sweep.
type GCReport struct {
	DryRun             bool      `json:"dry_run"`
	Marked             []BlobRef `json:"marked"`
	Swept              []BlobRef `json:"swept"`
	Unmarked           int       `json:"unmarked"`
	SkippedReferenced  int       `json:"skipped_referenced"`
	Failed             int       `json:"failed"`
	ListingUnsupported []string  `json:"listing_unsupported,omitempty"`
	// StaleUploadsAborted counts abandoned multipart uploads (idle longer than
	// Options.StaleUploadAge) that were aborted, or in dry-run would be.
	StaleUploadsAborted int `json:"stale_uploads_aborted"`
	// Fenced counts sweeps deferred because a backup holds a fence; the candidates
	// stay marked and are swept on the first run after the fence is gone.
	Fenced int `json:"fenced,omitempty"`
}

const storeTimeout = 2 * time.Minute

// errFenced means a running backup forbids deleting bytes right now.
var errFenced = errors.New("blob deletion paused by a running backup")

// CollectGarbage runs the unmark, sweep and mark phases.
//
// Sweep runs before mark so a blob marked in this run is never swept in it, and
// sweeping additionally requires first_seen_at to predate the run start. A blob is
// therefore deleted only after surviving two runs separated by the grace period.
func (s *Service) CollectGarbage(ctx context.Context, dryRun bool) (GCReport, error) {
	report := GCReport{DryRun: dryRun, Marked: []BlobRef{}, Swept: []BlobRef{}}
	if err := s.validate(); err != nil {
		return report, err
	}
	if !dryRun {
		if err := s.requireLease(ctx); err != nil {
			return report, err
		}
	}
	var runStart time.Time
	if err := s.DB.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&runStart); err != nil {
		return report, err
	}
	var errs []error
	if !dryRun {
		tag, err := s.DB.Exec(ctx, `DELETE FROM gc_candidates c WHERE c.instance_id=$1 AND EXISTS (SELECT 1 FROM blobs b WHERE b.dedup_domain_id=c.dedup_domain_id AND b.digest=c.digest AND (c.kind='object' OR EXISTS (SELECT 1 FROM assets a WHERE a.blob_id=b.id) OR EXISTS (SELECT 1 FROM federation_route_cache rc WHERE rc.blob_id=b.id)))`, s.InstanceID)
		if err != nil {
			errs = append(errs, err)
		}
		report.Unmarked = int(tag.RowsAffected())
	}
	errs = append(errs, s.sweep(ctx, runStart, dryRun, false, &report))
	errs = append(errs, s.markRows(ctx, dryRun, &report))
	errs = append(errs, s.markObjects(ctx, runStart, dryRun, &report))
	errs = append(errs, s.abortStaleUploads(ctx, dryRun, &report))
	return report, errors.Join(errs...)
}

func (s *Service) graceInterval() string {
	return fmt.Sprintf("%d microseconds", s.opts().Grace.Microseconds())
}

// sweep deletes candidates that were marked before this run and whose wait is
// over: sweep_after when set (pressure eviction), otherwise first_seen_at plus
// the grace period. expeditedOnly restricts it to candidates with sweep_after.
// The per-candidate row-lock re-check in sweepOne does not depend on either.
func (s *Service) sweep(ctx context.Context, runStart time.Time, dryRun, expeditedOnly bool, report *GCReport) error {
	rows, err := s.DB.Query(ctx, `SELECT dedup_domain_id::text,digest,kind FROM gc_candidates WHERE instance_id=$1 AND first_seen_at<$2 AND COALESCE(sweep_after,first_seen_at+$3::interval)<=clock_timestamp() AND (NOT $5 OR sweep_after IS NOT NULL) ORDER BY first_seen_at,digest LIMIT $4`, s.InstanceID, runStart, s.graceInterval(), s.opts().BatchSize, expeditedOnly)
	if err != nil {
		return err
	}
	var candidates []BlobRef
	for rows.Next() {
		var c BlobRef
		if err = rows.Scan(&c.Domain, &c.Digest, &c.Kind); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for i, c := range candidates {
		if i%50 == 0 && !dryRun {
			if err = s.requireLease(ctx); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if err = ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if dryRun {
			var referenced bool
			err = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blobs b JOIN assets a ON a.blob_id=b.id WHERE b.dedup_domain_id=$1 AND b.digest=$2)`, c.Domain, c.Digest).Scan(&referenced)
			if err != nil {
				errs = append(errs, err)
			} else if referenced {
				report.SkippedReferenced++
			} else {
				report.Swept = append(report.Swept, c)
			}
			continue
		}
		deleted, err := s.sweepOne(ctx, c)
		switch {
		case errors.Is(err, errFenced):
			// Byte deletion is paused for every blob; leave the rest of the batch marked.
			report.Fenced += len(candidates) - i
			return errors.Join(errs...)
		case err != nil:
			report.Failed++
			errs = append(errs, err)
		case deleted:
			report.Swept = append(report.Swept, c)
		default:
			report.SkippedReferenced++
		}
	}
	return errors.Join(errs...)
}

// sweepOne is the only place bytes are deleted. The blobs row (a placeholder is
// inserted when none exists) is locked FOR UPDATE for the whole transaction, so:
//   - a publish that already upserted the row holds it; the sweep waits, then sees
//     the asset reference in its re-check and aborts;
//   - a publish arriving later blocks on the row until the sweep commits, and
//     publishes a fresh row (callers that Stat the blob after the upsert, inside
//     their transaction, therefore observe the deletion rather than commit
//     metadata for missing bytes).
//
// The placeholder never commits as live: it is deleted in the same transaction.
func (s *Service) sweepOne(ctx context.Context, c BlobRef) (bool, error) {
	store, err := s.Stores(ctx, c.Domain)
	if err != nil {
		return false, fmt.Errorf("resolve domain %s: %w", c.Domain, err)
	}
	deleter, ok := store.(blob.Deleter)
	if !ok {
		return false, &blob.CapabilityError{Feature: "object deletion"}
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='10s'"); err != nil {
		return false, err
	}
	// A backup fence (see internal/backupfence) pauses all byte deletion: the shared
	// lock orders this sweep against fence acquisition, so a sweep that passes the
	// check below commits before any later backup snapshot is taken.
	if err = backupfence.LockShared(ctx, tx); err != nil {
		return false, err
	}
	fenced, err := backupfence.Live(ctx, tx)
	if err != nil {
		return false, err
	}
	if fenced {
		return false, errFenced
	}
	if _, err = tx.Exec(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,0,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO NOTHING`, s.InstanceID, c.Domain, c.Digest); err != nil {
		return false, err
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM blobs WHERE dedup_domain_id=$1 AND digest_algorithm='sha256' AND digest=$2 AND instance_id=$3 FOR UPDATE`, c.Domain, c.Digest, s.InstanceID).Scan(&id); err != nil {
		return false, err
	}
	var referenced bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE blob_id=$1) OR EXISTS(SELECT 1 FROM federation_route_cache WHERE blob_id=$1)`, id).Scan(&referenced); err != nil {
		return false, err
	}
	if referenced {
		if _, err = tx.Exec(ctx, `DELETE FROM gc_candidates WHERE dedup_domain_id=$1 AND digest=$2`, c.Domain, c.Digest); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	storeCtx, cancel := context.WithTimeout(ctx, storeTimeout)
	err = deleter.Delete(storeCtx, blob.Digest(c.Digest))
	cancel()
	if err != nil {
		return false, fmt.Errorf("delete %s: %w", c.Digest, err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM blobs WHERE id=$1`, id); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM gc_candidates WHERE dedup_domain_id=$1 AND digest=$2`, c.Domain, c.Digest); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// markRows marks metadata rows with no asset reference older than the grace period.
func (s *Service) markRows(ctx context.Context, dryRun bool, report *GCReport) error {
	const eligible = `SELECT b.instance_id,b.dedup_domain_id,b.digest FROM blobs b WHERE b.instance_id=$1 AND b.created_at<clock_timestamp()-$2::interval AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id) AND NOT EXISTS(SELECT 1 FROM federation_route_cache rc WHERE rc.blob_id=b.id) AND NOT EXISTS(SELECT 1 FROM gc_candidates c WHERE c.dedup_domain_id=b.dedup_domain_id AND c.digest=b.digest) ORDER BY b.created_at,b.id LIMIT $3`
	var rows pgx.Rows
	var err error
	if dryRun {
		rows, err = s.DB.Query(ctx, `SELECT dedup_domain_id::text,digest FROM (`+eligible+`) e`, s.InstanceID, s.graceInterval(), s.opts().BatchSize)
	} else {
		rows, err = s.DB.Query(ctx, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind) SELECT instance_id,dedup_domain_id,digest,'row' FROM (`+eligible+`) e ON CONFLICT DO NOTHING RETURNING dedup_domain_id::text,digest`, s.InstanceID, s.graceInterval(), s.opts().BatchSize)
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		ref := BlobRef{Kind: "row"}
		if err = rows.Scan(&ref.Domain, &ref.Digest); err != nil {
			return err
		}
		report.Marked = append(report.Marked, ref)
	}
	return rows.Err()
}

// markObjects lists each domain's store (bounded, resumable) and marks objects
// older than the grace period that have no blobs row at all: bytes left behind
// by failed metadata commits.
func (s *Service) markObjects(ctx context.Context, runStart time.Time, dryRun bool, report *GCReport) error {
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM dedup_domains WHERE instance_id=$1 ORDER BY id`, s.InstanceID)
	if err != nil {
		return err
	}
	var domains []string
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		domains = append(domains, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, domain := range domains {
		if err = s.markDomainObjects(ctx, domain, runStart, dryRun, report); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) markDomainObjects(ctx context.Context, domain string, runStart time.Time, dryRun bool, report *GCReport) error {
	store, err := s.Stores(ctx, domain)
	if err != nil {
		return fmt.Errorf("resolve domain %s: %w", domain, err)
	}
	lister, ok := store.(blob.Lister)
	if !ok {
		report.ListingUnsupported = append(report.ListingUnsupported, domain)
		return nil
	}
	cursor := "gc-scan:" + domain
	var after string
	err = s.DB.QueryRow(ctx, `SELECT position FROM retention_cursors WHERE instance_id=$1 AND name=$2`, s.InstanceID, cursor).Scan(&after)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	limit := s.opts().ObjectScanLimit
	listCtx, cancel := context.WithTimeout(ctx, storeTimeout)
	objects, err := lister.List(listCtx, blob.Digest(after), limit)
	cancel()
	if err != nil {
		return fmt.Errorf("list domain %s: %w", domain, err)
	}
	cutoff := runStart.Add(-s.opts().Grace)
	var old []string
	for _, o := range objects {
		if o.ModTime.Before(cutoff) {
			old = append(old, string(o.Digest))
		}
	}
	var missing []string
	if len(old) > 0 {
		rows, err := s.DB.Query(ctx, `SELECT d FROM unnest($2::text[]) d WHERE NOT EXISTS(SELECT 1 FROM blobs b WHERE b.dedup_domain_id=$1 AND b.digest=d AND b.digest_algorithm='sha256') AND NOT EXISTS(SELECT 1 FROM gc_candidates c WHERE c.dedup_domain_id=$1 AND c.digest=d)`, domain, old)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d string
			if err = rows.Scan(&d); err != nil {
				rows.Close()
				return err
			}
			missing = append(missing, d)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
	}
	for _, d := range missing {
		if !dryRun {
			if _, err = s.DB.Exec(ctx, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind) VALUES($1,$2,$3,'object') ON CONFLICT DO NOTHING`, s.InstanceID, domain, d); err != nil {
				return err
			}
		}
		report.Marked = append(report.Marked, BlobRef{Domain: domain, Digest: d, Kind: "object"})
	}
	if dryRun {
		return nil
	}
	next := ""
	if len(objects) == limit {
		next = string(objects[len(objects)-1].Digest)
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO retention_cursors(instance_id,name,position) VALUES($1,$2,$3) ON CONFLICT(instance_id,name) DO UPDATE SET position=excluded.position,updated_at=now()`, s.InstanceID, cursor, next)
	return err
}

// abortStaleUploads reaps multipart uploads abandoned by crashed or interrupted
// writers on stores that support it (S3). Uploads with recent persisted progress
// are never touched; see blob.S3.AbortStaleUploads.
func (s *Service) abortStaleUploads(ctx context.Context, dryRun bool, report *GCReport) error {
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM dedup_domains WHERE instance_id=$1 ORDER BY id`, s.InstanceID)
	if err != nil {
		return err
	}
	var domains []string
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		domains = append(domains, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	if !dryRun {
		if err = s.requireLease(ctx); err != nil {
			return err
		}
	}
	for _, domain := range domains {
		store, err := s.Stores(ctx, domain)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve domain %s: %w", domain, err))
			continue
		}
		aborter, ok := store.(blob.StaleUploadAborter)
		if !ok {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, storeTimeout)
		n, err := aborter.AbortStaleUploads(callCtx, s.opts().StaleUploadAge, dryRun)
		cancel()
		report.StaleUploadsAborted += n
		if err != nil {
			errs = append(errs, fmt.Errorf("abort stale uploads %s: %w", domain, err))
		}
	}
	return errors.Join(errs...)
}
