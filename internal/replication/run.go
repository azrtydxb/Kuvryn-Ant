package replication

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
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
	tx, err := s.cfg.DB.Begin(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var prevFailures int64
	var mode string
	if err = tx.QueryRow(ctx, `SELECT consecutive_failures,mode FROM replication_jobs WHERE id=$1 AND lease_owner=$2 FOR UPDATE`, id, s.owner).Scan(&prevFailures, &mode); err != nil {
		return
	}
	cancelled := shuttingDown || errors.Is(runErr, context.Canceled)
	switch {
	case runErr == nil:
		_, err = tx.Exec(ctx, `UPDATE replication_jobs SET lease_owner=NULL,lease_until=NULL,last_success_at=clock_timestamp(),last_error=NULL,consecutive_failures=0,
 next_run_at=CASE WHEN NOT enabled THEN NULL WHEN run_requested THEN clock_timestamp() WHEN mode='manual' THEN NULL WHEN mode='scheduled' THEN clock_timestamp()+interval_seconds*interval '1 second' ELSE clock_timestamp()+$3::bigint*interval '1 millisecond' END
 WHERE id=$1 AND lease_owner=$2`, id, s.owner, s.cfg.ContinuousDelay.Milliseconds())
		// Continuous jobs succeed every pass; report only manual/scheduled
		// completions and recovery from a failure streak.
		if err == nil && (mode != "continuous" || prevFailures > 0) {
			err = events.Emit(ctx, tx, s.cfg.InstanceID, "", "replication.job.succeeded", id, map[string]any{"job_id": id, "mode": mode, "recovered": prevFailures > 0})
		}
	case cancelled:
		_, err = tx.Exec(ctx, `UPDATE replication_jobs SET lease_owner=NULL,lease_until=NULL,next_run_at=clock_timestamp() WHERE id=$1 AND lease_owner=$2`, id, s.owner)
	default:
		msg := runErr.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		_, err = tx.Exec(ctx, `UPDATE replication_jobs SET lease_owner=NULL,lease_until=NULL,last_error=$3,consecutive_failures=consecutive_failures+1,failures_total=failures_total+1,
 next_run_at=CASE WHEN NOT enabled THEN NULL ELSE clock_timestamp()+LEAST($4::bigint,2000*power(2,LEAST(consecutive_failures,20)))*interval '1 millisecond' END
 WHERE id=$1 AND lease_owner=$2`, id, s.owner, msg, s.cfg.MaxBackoff.Milliseconds())
		// One event per failure streak, not per retry.
		if err == nil && prevFailures == 0 {
			err = events.Emit(ctx, tx, s.cfg.InstanceID, "", "replication.job.failed", id, map[string]any{"job_id": id, "mode": mode, "error": msg})
		}
	}
	if err == nil && !cancelled && s.cfg.LagAlertThreshold > 0 {
		err = s.lagAlert(ctx, tx, id)
	}
	if err == nil {
		_ = tx.Commit(ctx)
	}
}

// lagAlert emits replication.job.lag when the destination falls more than the
// configured number of changes behind the peer head, once per excursion.
func (s *Service) lagAlert(ctx context.Context, tx pgx.Tx, id string) error {
	var lag int64
	var alerted bool
	if err := tx.QueryRow(ctx, `SELECT GREATEST(COALESCE(remote_head,0)-COALESCE(checkpoint,0),0),lag_alerted FROM replication_jobs WHERE id=$1`, id).Scan(&lag, &alerted); err != nil {
		return err
	}
	over := lag > s.cfg.LagAlertThreshold
	if over == alerted {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE replication_jobs SET lag_alerted=$2 WHERE id=$1`, id, over); err != nil {
		return err
	}
	if !over {
		return nil
	}
	return events.Emit(ctx, tx, s.cfg.InstanceID, "", "replication.job.lag", id, map[string]any{"job_id": id, "lag_changes": lag, "threshold": s.cfg.LagAlertThreshold})
}

// run holds per-execution state.
type run struct {
	job      jobRow
	client   *peerClient
	domain   string
	store    blob.Store
	verified map[string]bool
	head     int64 // peer head observed at the start of this execution
	lowWater int64 // peer change-history low-water mark observed with head
}

// delta is the progress produced by one applied page.
type delta struct {
	assets, deletions, conflicts     int64
	transferred, deduplicated, bytes int64
	resumed                          int64 // bytes not re-transferred thanks to a staged partial
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
	rt.client.job = rt.job.Name
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
		// The peer purged history this job still needs: restartable reseed.
		if rt.job.Status.Phase == "incremental" && rt.lowWater > rt.job.Status.Checkpoint {
			if err = s.startReseed(ctx, rt); err != nil {
				return err
			}
			if rt.job, err = scanJob(s.cfg.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2`, s.cfg.InstanceID, id)); err != nil {
				return dbError(err)
			}
			continue
		}
		switch rt.job.Status.Phase {
		case "new":
			err = s.startSeed(ctx, rt)
		case "seed_assets":
			err = s.seedPage(ctx, rt, manifestAssets)
		case "seed_tags":
			err = s.seedPage(ctx, rt, manifestTags)
		case "incremental":
			switch {
			case rt.job.DeletionPolicy == "propagate" && rt.job.held > 0:
				err = s.applyHeld(ctx, rt)
			case rt.job.backfillsPending > 0:
				err = s.backfillPage(ctx, rt)
			default:
				done, err = s.incrementalPage(ctx, rt)
				if errors.Is(err, ErrHistoryPurged) {
					// The low-water mark moved after the head was read.
					if err = s.refreshHead(ctx, rt); err == nil {
						err = s.startReseed(ctx, rt)
					}
					done = false
				}
			}
		default:
			err = ErrInput
		}
		if err != nil {
			return err
		}
		// Reload so counters, cursor and phase always reflect committed state.
		if rt.job, err = scanJob(s.cfg.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM replication_jobs j WHERE j.instance_id=$1 AND j.id=$2`, s.cfg.InstanceID, id)); err != nil {
			return dbError(err)
		}
		// A caught-up page may have queued back-fills; drain them in this run.
		if done && rt.job.backfillsPending == 0 {
			return nil
		}
	}
}

func (s *Service) peerFor(ctx context.Context, peerID string) (*peerClient, error) {
	var url, secretID, mode, member string
	var ca *string
	err := s.cfg.DB.QueryRow(ctx, `SELECT url,COALESCE(token_secret_id::text,''),ca_ref,auth_mode,COALESCE(member_instance_id::text,'') FROM replication_peers WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, peerID).Scan(&url, &secretID, &ca, &mode, &member)
	if err != nil {
		return nil, dbError(err)
	}
	if mode == "federation" {
		// Member mTLS and request signature on the federation listener; the
		// URL comes from the applied membership, never from the peer row.
		fed := s.fed.Load()
		if fed == nil {
			return nil, fmt.Errorf("%w: federation transport not available", ErrUnavailable)
		}
		id, base, err := fed.ReplicationIdentity(member)
		if err != nil {
			return nil, fmt.Errorf("%w: federation member unavailable", ErrPeer)
		}
		return newFederationClient(base, member, id, time.Now), nil
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
	_, err = s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET remote_head=$3,remote_head_at=$4,remote_checked_at=clock_timestamp(),source_low_water=$5 WHERE id=$1 AND lease_owner=$2`, rt.job.ID, s.owner, head.Seq, head.ChangedAt, head.LowWater)
	if err != nil {
		return ErrUnavailable
	}
	rt.head, rt.lowWater = head.Seq, head.LowWater
	return nil
}

func (s *Service) startSeed(ctx context.Context, rt *run) error {
	return s.commit(ctx, rt, func(tx pgx.Tx, _ *delta) error {
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='seed_assets',seed_head=$2,seed_cursor='',seed_done=0,seed_total=NULL,seed_generation=seed_generation+1 WHERE id=$1 AND phase='new'`, rt.job.ID, rt.head)
		return err
	})
}

// startReseed restarts the full manifest walk because the peer purged change
// history this job had not consumed. It is the seed protocol again, with these
// differences: nothing at the destination is removed (only upserts), blobs that
// are already present are deduplicated instead of fetched, the old checkpoint
// stays until the walk completes, and a finished walk records replicated items
// the peer no longer lists as held deletions (never as direct deletions), so
// the destination's deletion policy and dry run decide what happens to them.
// The walk is as restartable as the initial seed: its cursor commits per page.
func (s *Service) startReseed(ctx context.Context, rt *run) error {
	return s.commit(ctx, rt, func(tx pgx.Tx, _ *delta) error {
		if _, err := tx.Exec(ctx, `DELETE FROM replication_backfills WHERE job_id=$1`, rt.job.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='seed_assets',reseeding=true,seed_head=$2,seed_cursor='',seed_done=0,seed_total=NULL,seed_generation=seed_generation+1,reseeds=reseeds+1,last_reseed_at=clock_timestamp(),source_low_water=$3 WHERE id=$1 AND phase='incremental'`, rt.job.ID, rt.head, rt.lowWater)
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
	if _, err = tx.Exec(cctx, `UPDATE replication_jobs SET assets_applied=assets_applied+$2,deletions_applied=deletions_applied+$3,conflicts=conflicts+$4,blobs_transferred=blobs_transferred+$5,blobs_deduplicated=blobs_deduplicated+$6,bytes_transferred=bytes_transferred+$7,resumed_bytes=resumed_bytes+$8 WHERE id=$1`,
		rt.job.ID, d.assets, d.deletions, d.conflicts, d.transferred, d.deduplicated, d.bytes, d.resumed); err != nil {
		return ErrUnavailable
	}
	if err = tx.Commit(cctx); err != nil {
		return ErrUnavailable
	}
	return nil
}
