package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
)

// Finding is one integrity mismatch. Kind is missing, size or digest.
type Finding struct {
	Domain string `json:"domain"`
	Digest string `json:"digest"`
	Kind   string `json:"kind"`
}

// IntegrityReport describes one bounded batch of the rolling integrity scan.
type IntegrityReport struct {
	DryRun     bool      `json:"dry_run"`
	Checked    int       `json:"checked"`
	Bytes      int64     `json:"bytes"`
	Mismatches []Finding `json:"mismatches"`
	Errors     int       `json:"errors"`
	Wrapped    bool      `json:"wrapped"` // scan reached the end and restarts next run
}

const integrityCursor = "integrity"

// VerifyIntegrity re-hashes blobs after the persisted cursor, bounded by
// IntegrityBatch rows and IntegrityMaxBytes (at least one blob is always checked).
// Mismatches are recorded in blob_integrity_findings (unless dryRun) and returned;
// nothing is ever deleted or repaired. Transient backend errors are counted, not
// recorded as findings, and the blob is retried on the next pass.
func (s *Service) VerifyIntegrity(ctx context.Context, dryRun bool) (IntegrityReport, error) {
	report := IntegrityReport{DryRun: dryRun, Mismatches: []Finding{}}
	if err := s.validate(); err != nil {
		return report, err
	}
	var position string
	err := s.DB.QueryRow(ctx, `SELECT position FROM retention_cursors WHERE instance_id=$1 AND name=$2`, s.InstanceID, integrityCursor).Scan(&position)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return report, err
	}
	var afterDomain, afterDigest string
	if before, after, ok := strings.Cut(position, "|"); ok {
		afterDomain, afterDigest = before, after
	}
	opts := s.opts()
	rows, err := s.DB.Query(ctx, `SELECT dedup_domain_id::text,digest,size_bytes FROM blobs WHERE instance_id=$1 AND ($2='' OR (dedup_domain_id::text,digest)>($2,$3)) ORDER BY dedup_domain_id::text,digest LIMIT $4`, s.InstanceID, afterDomain, afterDigest, opts.IntegrityBatch)
	if err != nil {
		return report, err
	}
	type item struct {
		domain, digest string
		size           int64
	}
	var items []item
	for rows.Next() {
		var it item
		if err = rows.Scan(&it.domain, &it.digest, &it.size); err != nil {
			rows.Close()
			return report, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return report, err
	}
	var errs []error
	last := -1
	for i, it := range items {
		if report.Checked > 0 && report.Bytes+it.size > opts.IntegrityMaxBytes {
			break
		}
		if err = ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		kind, err := s.verifyBlob(ctx, it.domain, it.digest, it.size)
		last = i
		report.Checked++
		report.Bytes += it.size
		switch {
		case err != nil:
			report.Errors++
			errs = append(errs, fmt.Errorf("verify %s: %w", it.digest, err))
			continue
		case kind != "":
			report.Mismatches = append(report.Mismatches, Finding{it.domain, it.digest, kind})
		}
		if !dryRun {
			if rerr := s.recordFinding(ctx, it.domain, it.digest, kind); rerr != nil {
				errs = append(errs, rerr)
			}
		}
	}
	report.Wrapped = last == len(items)-1 && len(items) < opts.IntegrityBatch
	if !dryRun {
		next := ""
		if !report.Wrapped && last >= 0 {
			next = items[last].domain + "|" + items[last].digest
		}
		if _, err = s.DB.Exec(ctx, `INSERT INTO retention_cursors(instance_id,name,position) VALUES($1,$2,$3) ON CONFLICT(instance_id,name) DO UPDATE SET position=excluded.position,updated_at=now()`, s.InstanceID, integrityCursor, next); err != nil {
			errs = append(errs, err)
		}
	}
	return report, errors.Join(errs...)
}

// verifyBlob returns a mismatch kind, or "" when the blob is intact.
func (s *Service) verifyBlob(ctx context.Context, domain, digest string, size int64) (string, error) {
	store, err := s.Stores(ctx, domain)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*storeTimeout)
	defer cancel()
	info, err := store.Stat(ctx, blob.Digest(digest))
	if errors.Is(err, blob.ErrNotFound) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if info.Size != size {
		return "size", nil
	}
	r, err := store.OpenRange(ctx, blob.Digest(digest), 0, -1)
	if errors.Is(err, blob.ErrNotFound) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return "digest", nil
	}
	return "", nil
}

// recordFinding opens a finding for a mismatch, or resolves open findings when the
// blob verified clean (kind == "").
func (s *Service) recordFinding(ctx context.Context, domain, digest, kind string) error {
	if kind == "" {
		_, err := s.DB.Exec(ctx, `UPDATE blob_integrity_findings SET resolved_at=now() WHERE dedup_domain_id=$1 AND digest=$2 AND resolved_at IS NULL`, domain, digest)
		return err
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO blob_integrity_findings(instance_id,dedup_domain_id,digest,kind) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.InstanceID, domain, digest, kind)
	return err
}

// OpenFindings lists unresolved integrity mismatches.
func (s *Service) OpenFindings(ctx context.Context) ([]Finding, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT dedup_domain_id::text,digest,kind FROM blob_integrity_findings WHERE instance_id=$1 AND resolved_at IS NULL ORDER BY id`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		var f Finding
		if err = rows.Scan(&f.Domain, &f.Digest, &f.Kind); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
