package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
)

// Run is the scheduler loop. It must be started only by the process that holds
// the instance writer lease; every metadata commit is additionally fenced by it.
// It returns when ctx is cancelled, after in-flight jobs have released their
// claims.
func (s *Service) Run(ctx context.Context) {
	slots := make(chan struct{}, 2)
	var wg sync.WaitGroup
	defer wg.Wait()
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			select {
			case slots <- struct{}{}:
			default:
			}
			if len(slots) == 0 {
				break
			}
			id, ok := s.claim(ctx, "")
			if !ok {
				<-slots
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				_ = s.execute(ctx, id)
			}()
			if len(slots) == cap(slots) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunNow claims one job regardless of its schedule and runs it to completion
// (or first failure). It returns ErrConflict if another worker holds the job.
func (s *Service) RunNow(ctx context.Context, id string) error {
	claimed, ok := s.claim(ctx, id)
	if !ok {
		return ErrConflict
	}
	return s.execute(ctx, claimed)
}

func (s *Service) claim(ctx context.Context, only string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id string
	err := s.cfg.DB.QueryRow(cctx, `UPDATE replication_jobs SET lease_owner=$2,lease_until=clock_timestamp()+$3::bigint*interval '1 millisecond',last_attempt_at=clock_timestamp(),run_requested=false
 WHERE id=(SELECT id FROM replication_jobs WHERE instance_id=$1 AND enabled AND (($4::text='' AND next_run_at<=clock_timestamp()) OR id::text=$4::text) AND (lease_until IS NULL OR lease_until<clock_timestamp()) ORDER BY next_run_at NULLS LAST LIMIT 1 FOR UPDATE SKIP LOCKED)
 RETURNING id::text`, s.cfg.InstanceID, s.owner, s.cfg.LeaseTTL.Milliseconds(), only).Scan(&id)
	return id, err == nil
}

func (s *Service) execute(ctx context.Context, id string) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		t := time.NewTicker(s.cfg.LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				rctx, stop := context.WithTimeout(runCtx, 5*time.Second)
				tag, err := s.cfg.DB.Exec(rctx, `UPDATE replication_jobs SET lease_until=clock_timestamp()+$3::bigint*interval '1 millisecond' WHERE id=$1 AND lease_owner=$2`, id, s.owner, s.cfg.LeaseTTL.Milliseconds())
				stop()
				if err == nil && tag.RowsAffected() == 0 {
					cancel()
					return
				}
			}
		}
	}()
	err := s.runJob(runCtx, id)
	cancel()
	<-renewDone
	s.finish(id, err, ctx.Err() != nil)
	return err
}

func (s *Service) finish(id string, runErr error, shuttingDown bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if runErr == nil {
		_, _ = s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET lease_owner=NULL,lease_until=NULL,last_success_at=clock_timestamp(),last_error=NULL,consecutive_failures=0,
 next_run_at=CASE WHEN NOT enabled THEN NULL WHEN run_requested THEN clock_timestamp() WHEN mode='manual' THEN NULL WHEN mode='scheduled' THEN clock_timestamp()+interval_seconds*interval '1 second' ELSE clock_timestamp()+$3::bigint*interval '1 millisecond' END
 WHERE id=$1 AND lease_owner=$2`, id, s.owner, s.cfg.ContinuousDelay.Milliseconds())
		return
	}
	if shuttingDown || errors.Is(runErr, context.Canceled) {
		_, _ = s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET lease_owner=NULL,lease_until=NULL,next_run_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2`, id, s.owner)
		return
	}
	msg := runErr.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, _ = s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET lease_owner=NULL,lease_until=NULL,last_error=$3,consecutive_failures=consecutive_failures+1,
 next_run_at=CASE WHEN NOT enabled THEN NULL ELSE clock_timestamp()+LEAST($4::bigint,2000*power(2,LEAST(consecutive_failures,20)))*interval '1 millisecond' END
 WHERE id=$1 AND lease_owner=$2`, id, s.owner, msg, s.cfg.MaxBackoff.Milliseconds())
}

// run holds per-execution state.
type run struct {
	job      jobRow
	client   *peerClient
	domain   string
	store    blob.Store
	verified map[string]bool
	head     int64 // peer head observed at the start of this execution
}

// delta is the progress produced by one applied page.
type delta struct {
	assets, deletions, conflicts     int64
	transferred, deduplicated, bytes int64
}

func (s *Service) runJob(ctx context.Context, id string) error {
	rt := &run{verified: map[string]bool{}}
	var err error
	rt.job, err = scanJob(s.cfg.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2`, s.cfg.InstanceID, id))
	if err != nil {
		return dbError(err)
	}
	rt.client, err = s.peerFor(ctx, rt.job.PeerID)
	if err != nil {
		return err
	}
	if err = s.refreshHead(ctx, rt); err != nil {
		return err
	}
	if err = s.cfg.DB.QueryRow(ctx, `SELECT id::text FROM dedup_domains WHERE instance_id=$1 AND name='source:'||$2::text`, s.cfg.InstanceID, rt.job.ReplicaSourceID).Scan(&rt.domain); err != nil {
		return ErrUnavailable
	}
	if rt.store, err = s.cfg.Storage(ctx, rt.domain); err != nil || rt.store == nil {
		return ErrUnavailable
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		var done bool
		switch rt.job.Status.Phase {
		case "new":
			err = s.startSeed(ctx, rt)
		case "seed_assets":
			err = s.seedPage(ctx, rt, manifestAssets)
		case "seed_tags":
			err = s.seedPage(ctx, rt, manifestTags)
		case "incremental":
			if rt.job.DeletionPolicy == "propagate" && rt.job.held > 0 {
				err = s.applyHeld(ctx, rt)
			} else {
				done, err = s.incrementalPage(ctx, rt)
			}
		default:
			err = ErrInput
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		// Reload so counters, cursor and phase always reflect committed state.
		if rt.job, err = scanJob(s.cfg.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2`, s.cfg.InstanceID, id)); err != nil {
			return dbError(err)
		}
	}
}

func (s *Service) peerFor(ctx context.Context, peerID string) (*peerClient, error) {
	var url, secretID string
	var ca *string
	err := s.cfg.DB.QueryRow(ctx, `SELECT url,token_secret_id::text,ca_ref FROM replication_peers WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, peerID).Scan(&url, &secretID, &ca)
	if err != nil {
		return nil, dbError(err)
	}
	_, token, err := (secrets.Store{DB: s.cfg.DB, Keys: s.cfg.Keys}).Open(ctx, s.cfg.InstanceID, secretID, "replication-peer:"+peerID)
	if err != nil {
		return nil, fmt.Errorf("%w: peer credential unavailable", ErrUnavailable)
	}
	defer clear(token)
	caPath := ""
	if ca != nil {
		var ok bool
		if caPath, ok = s.cfg.CAs[*ca]; !ok {
			return nil, fmt.Errorf("%w: unknown CA reference", ErrInput)
		}
	}
	return newPeerClient(url, string(token), caPath)
}

func (s *Service) refreshHead(ctx context.Context, rt *run) error {
	head, err := rt.client.Head(ctx, rt.job.RemoteFeedID)
	if err != nil {
		return err
	}
	var destFormat string
	if err = s.cfg.DB.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, rt.job.DestFeedID).Scan(&destFormat); err != nil {
		return dbError(err)
	}
	if head.Format != destFormat {
		return fmt.Errorf("%w: feed format %q does not match destination format %q", ErrConflict, head.Format, destFormat)
	}
	_, err = s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET remote_head=$3,remote_head_at=$4,remote_checked_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2`, rt.job.ID, s.owner, head.Seq, head.ChangedAt)
	if err != nil {
		return ErrUnavailable
	}
	rt.head = head.Seq
	return nil
}

func (s *Service) startSeed(ctx context.Context, rt *run) error {
	return s.commit(ctx, rt, func(tx pgx.Tx, _ *delta) error {
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='seed_assets',seed_head=$2,seed_cursor='',seed_done=0,seed_total=NULL WHERE id=$1 AND phase='new'`, rt.job.ID, rt.head)
		return err
	})
}

// commit applies one page atomically: instance-lease fencing, job-claim
// fencing, the page's metadata effects and the progress/checkpoint update all
// commit together or not at all, which makes every step restartable.
func (s *Service) commit(ctx context.Context, rt *run, fn func(tx pgx.Tx, d *delta) error) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	tx, err := s.cfg.DB.BeginTx(cctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	lease := s.cfg.Lease()
	if lease.InstanceID != s.cfg.InstanceID {
		return ErrLeaseLost
	}
	if err = ownership.Require(cctx, tx, lease); err != nil {
		return err
	}
	var locked bool
	err = tx.QueryRow(cctx, `SELECT true FROM replication_jobs WHERE id=$1 AND lease_owner=$2 AND lease_until>clock_timestamp() FOR UPDATE`, rt.job.ID, s.owner).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return ErrUnavailable
	}
	d := &delta{}
	if err = fn(tx, d); err != nil {
		return dbError(err)
	}
	if _, err = tx.Exec(cctx, `UPDATE replication_jobs SET assets_applied=assets_applied+$2,deletions_applied=deletions_applied+$3,conflicts=conflicts+$4,blobs_transferred=blobs_transferred+$5,blobs_deduplicated=blobs_deduplicated+$6,bytes_transferred=bytes_transferred+$7 WHERE id=$1`,
		rt.job.ID, d.assets, d.deletions, d.conflicts, d.transferred, d.deduplicated, d.bytes); err != nil {
		return ErrUnavailable
	}
	if err = tx.Commit(cctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (s *Service) seedPage(ctx context.Context, rt *run, kind string) error {
	page, err := rt.client.Manifest(ctx, rt.job.RemoteFeedID, kind, rt.job.seedCursor, s.cfg.PageSize, rt.job.IncludeCached, rt.job.Filters)
	if err != nil {
		return err
	}
	var items []Item
	for _, it := range page.Items {
		if s.wanted(rt, it.Origin, it.Namespace, it.Coordinate, it.Version) {
			items = append(items, it)
		}
	}
	d := delta{}
	if err = s.ensureBlobs(ctx, rt, items, &d); err != nil {
		return err
	}
	next := page.Next
	return s.commit(ctx, rt, func(tx pgx.Tx, applied *delta) error {
		*applied = d
		for _, it := range items {
			if err := s.applyAsset(ctx, tx, rt, it, applied); err != nil {
				return err
			}
		}
		for _, t := range page.Tags {
			if !s.wanted(rt, t.Origin, t.Namespace, t.Coordinate, t.Version) {
				continue
			}
			if err := s.applyTag(ctx, tx, rt, t); err != nil {
				return err
			}
		}
		count := len(page.Items) + len(page.Tags)
		if _, err := tx.Exec(ctx, `UPDATE replication_jobs SET seed_cursor=$2,seed_done=seed_done+$3,seed_total=COALESCE($4,seed_total) WHERE id=$1`, rt.job.ID, next, count, page.Total); err != nil {
			return err
		}
		if next != "" {
			return nil
		}
		if kind == manifestAssets {
			_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='seed_tags',seed_cursor='' WHERE id=$1`, rt.job.ID)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='incremental',seed_cursor='',checkpoint=COALESCE(seed_head,0),checkpoint_change_at=clock_timestamp() WHERE id=$1`, rt.job.ID)
		return err
	})
}

// wanted applies the job's include-cached option and selective filters.
func (s *Service) wanted(rt *run, o Origin, ns *string, coordinate, version string) bool {
	if o.Kind == "remote" && !rt.job.IncludeCached {
		return false
	}
	if o.Kind != "local" && o.Kind != "remote" {
		return false
	}
	return rt.job.Filters.Match(ns, coordinate, version)
}

func (s *Service) incrementalPage(ctx context.Context, rt *run) (bool, error) {
	page, err := rt.client.Changes(ctx, rt.job.RemoteFeedID, rt.job.Status.Checkpoint, s.cfg.PageSize, rt.job.IncludeCached)
	if err != nil {
		return false, err
	}
	if page.Next < rt.job.Status.Checkpoint {
		return false, fmt.Errorf("%w: peer cursor moved backwards", ErrPeer)
	}
	var items []Item
	for _, c := range page.Changes {
		if c.Op == "put" && c.Kind == "asset" && c.Asset != nil && s.wanted(rt, c.Asset.Origin, c.Asset.Namespace, c.Asset.Coordinate, c.Asset.Version) {
			items = append(items, *c.Asset)
		}
	}
	d := delta{}
	if err = s.ensureBlobs(ctx, rt, items, &d); err != nil {
		return false, err
	}
	err = s.commit(ctx, rt, func(tx pgx.Tx, applied *delta) error {
		*applied = d
		for _, c := range page.Changes {
			if err := s.applyChange(ctx, tx, rt, c, applied); err != nil {
				return err
			}
		}
		var at any
		if n := len(page.Changes); n > 0 {
			at = page.Changes[n-1].At
		}
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET checkpoint=$2,checkpoint_change_at=COALESCE($3,checkpoint_change_at,clock_timestamp()),remote_head=$4,remote_head_at=COALESCE($5,remote_head_at) WHERE id=$1`, rt.job.ID, page.Next, at, page.Head, page.HeadAt)
		return err
	})
	if err != nil {
		return false, err
	}
	rt.job.Status.Checkpoint = page.Next
	return !page.More, nil
}

// ensureBlobs makes every referenced blob present and verified in the replica
// source's store before metadata that references it commits. A blob whose digest
// and size are already present is never fetched again: stores publish only
// digest-verified content, so presence is the dedup record that survives
// interruption and restart.
func (s *Service) ensureBlobs(ctx context.Context, rt *run, items []Item, d *delta) error {
	type want struct {
		digest string
		size   int64
	}
	var todo []want
	seen := map[string]bool{}
	for _, it := range items {
		if err := validItem(it); err != nil {
			return err
		}
		if seen[it.Digest] || rt.verified[it.Digest] {
			continue
		}
		seen[it.Digest] = true
		info, err := rt.store.Stat(ctx, blob.Digest(it.Digest))
		if err == nil && info.Size == it.Size {
			rt.verified[it.Digest] = true
			d.deduplicated++
			continue
		}
		if err != nil && !errors.Is(err, blob.ErrNotFound) {
			return ErrUnavailable
		}
		todo = append(todo, want{it.Digest, it.Size})
	}
	if len(todo) == 0 {
		return nil
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, s.cfg.BlobConcurrency)
	for _, w := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.transfer(gctx, rt, w.digest, w.size)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			rt.verified[w.digest] = true
			d.transferred++
			d.bytes += w.size
		}()
	}
	wg.Wait()
	return firstErr
}

func (s *Service) transfer(ctx context.Context, rt *run, digest string, size int64) error {
	body, length, err := rt.client.Blob(ctx, rt.job.RemoteFeedID, digest, rt.job.IncludeCached)
	if err != nil {
		return err
	}
	defer body.Close()
	if length >= 0 && length != size {
		return fmt.Errorf("%w: peer announced %d bytes, manifest says %d", ErrCorrupt, length, size)
	}
	upload, err := rt.store.Stage(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer upload.Abort()
	n, err := io.Copy(upload, io.LimitReader(body, size+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: transfer interrupted", ErrPeer)
	}
	if n != size {
		return fmt.Errorf("%w: received %d bytes, expected %d", ErrCorrupt, n, size)
	}
	if _, err = upload.Finalize(ctx, blob.Info{Digest: blob.Digest(digest), Size: size}); err != nil {
		if errors.Is(err, blob.ErrIntegrity) {
			return fmt.Errorf("%w: sha256 mismatch for %s", ErrCorrupt, digest)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrUnavailable
	}
	return nil
}
