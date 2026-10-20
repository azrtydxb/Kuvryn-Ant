package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/jackc/pgx/v5"
)

const (
	workerCount   = 4
	leaseDuration = 2 * time.Minute
	pollInterval  = 2 * time.Second
	purgeInterval = 10 * time.Minute
)

// claimed is a delivery taken by a worker.
type claimed struct {
	id, hookID string
	attempt    int
	payload    []byte
	headers    map[string]string
	auth       string
	delivery   string // provider delivery ref
}

// outcome is the result of running an action once.
type outcome struct {
	result string
	detail map[string]any
	err    string
	// retry asks for another attempt after a backoff (transient failures).
	retry bool
}

func okOutcome(result string, detail map[string]any) outcome {
	return outcome{result: result, detail: detail}
}
func failOutcome(code, msg string, retry bool) outcome {
	return outcome{result: code, err: msg, retry: retry, detail: map[string]any{}}
}

// Run processes queued deliveries until ctx ends. Start it only in the process
// that holds the instance writer lease; it also purges old deliveries.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() { defer wg.Done(); s.loop(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.purgeLoop(ctx) }()
	wg.Wait()
}

func (s *Service) loop(ctx context.Context) {
	for ctx.Err() == nil {
		did, err := s.processOne(ctx)
		if err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("incoming hook worker", "reason", err)
		}
		if did {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.poke:
		case <-time.After(pollInterval):
		}
	}
}

func (s *Service) purgeLoop(ctx context.Context) {
	for {
		s.Purge(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(purgeInterval):
		}
	}
}

// Purge removes deliveries older than the retention window, remembered delivery
// ids older than 24 hours and the one-off prewarm jobs created by hooks.
func (s *Service) Purge(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	days := s.cfg.LogRetentionDays
	_, _ = s.cfg.DB.Exec(cctx, `DELETE FROM incoming_hook_deliveries WHERE instance_id=$1 AND received_at<now()-make_interval(days=>$2) AND state IN ('succeeded','failed','rejected')`, s.cfg.InstanceID, days)
	_, _ = s.cfg.DB.Exec(cctx, `DELETE FROM incoming_hook_replay WHERE seen_at<now()-interval '24 hours'`)
	_, _ = s.cfg.DB.Exec(cctx, `DELETE FROM prewarm_jobs j WHERE j.instance_id=$1 AND j.name LIKE 'hook:%' AND j.interval_seconds IS NULL AND j.created_at<now()-make_interval(days=>$2) AND NOT EXISTS(SELECT 1 FROM prewarm_runs r WHERE r.job_id=j.id AND r.state IN ('queued','running'))`, s.cfg.InstanceID, days)
}

func (s *Service) claim(ctx context.Context) (claimed, bool, error) {
	var c claimed
	var headers []byte
	err := s.cfg.DB.QueryRow(ctx, `UPDATE incoming_hook_deliveries SET state='running',attempts=attempts+1,lease_until=clock_timestamp()+$2::bigint*interval '1 second'
 WHERE id=(SELECT id FROM incoming_hook_deliveries WHERE instance_id=$1 AND ((state='pending' AND available_at<=clock_timestamp()) OR (state='running' AND lease_until<clock_timestamp()))
  ORDER BY available_at,received_at FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING id::text,hook_id::text,attempts,COALESCE(payload,''::bytea),headers,auth_result,COALESCE(delivery_ref,'')`,
		s.cfg.InstanceID, int64(leaseDuration/time.Second)).Scan(&c.id, &c.hookID, &c.attempt, &c.payload, &headers, &c.auth, &c.delivery)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	_ = json.Unmarshal(headers, &c.headers)
	return c, true, nil
}

// processOne claims and runs one delivery. It reports whether one was found.
func (s *Service) processOne(ctx context.Context) (bool, error) {
	c, ok, err := s.claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	hook, err := GetHook(ctx, s.cfg.DB, s.cfg.InstanceID, c.hookID)
	var out outcome
	switch {
	case errors.Is(err, ErrNotFound):
		out = failOutcome("hook_deleted", "the hook no longer exists", false)
	case err != nil:
		out = failOutcome("storage_unavailable", "storage unavailable", true)
	case !hook.Enabled && c.auth != AuthTest && c.auth != AuthRedelivery:
		out = failOutcome("hook_disabled", "the hook was disabled before the delivery ran", false)
	default:
		actx, cancel := context.WithTimeout(ctx, 90*time.Second)
		out = s.execute(actx, hook, c)
		cancel()
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	state := StateSucceeded
	var avail any
	switch {
	case out.err == "":
	case out.retry && c.attempt < MaxAttempts:
		state = StatePending
		avail = time.Now().Add(events.Backoff(c.attempt))
	default:
		state = StateFailed
	}
	detail, _ := json.Marshal(out.detail)
	if len(detail) > 8000 {
		detail = []byte(`{"truncated":true}`)
	}
	_, uerr := s.cfg.DB.Exec(finish, `UPDATE incoming_hook_deliveries SET state=$2,action_result=$3,detail=$4,error=NULLIF($5,''),lease_until=NULL,
 available_at=COALESCE($6::timestamptz,available_at),finished_at=CASE WHEN $2 IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END
 WHERE id=$1`, c.id, state, out.result, detail, truncate(out.err, 400), avail)
	if state == StateSucceeded || state == StateFailed {
		slug := c.hookID
		if err == nil {
			slug = hook.Slug
		}
		s.record(slug, state)
	}
	return true, uerr
}

func (s *Service) execute(ctx context.Context, hook Hook, c claimed) outcome {
	a := hook.Action
	switch a.Type {
	case ActionPrewarm:
		return s.doPrewarm(ctx, hook, c)
	case ActionReplicationRun:
		return s.doReplication(ctx, hook, c)
	case ActionPurgeSource:
		return s.doPurge(ctx, hook, c)
	case ActionCleanCache:
		return s.doCleanCache(ctx, hook, c)
	case ActionUpstream:
		return s.doUpstream(ctx, hook, c)
	case ActionRelay:
		return s.doRelay(ctx, hook, c)
	}
	return failOutcome("unknown_action", "unknown action type", false)
}

func headerOf(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}
