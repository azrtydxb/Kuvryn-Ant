package prewarm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service schedules and executes prewarm runs. Run must be called under the
// instance writer lease (its context is cancelled when the lease is lost), so
// only the lease holder warms. Runs that were active when the process stopped
// are resumed from their per-item checkpoints.
type Service struct {
	DB         *pgxpool.Pool
	InstanceID string
	Warmer     *Warmer
	Logger     *slog.Logger
	// PollInterval is how often schedules are evaluated and queued runs adopted.
	PollInterval time.Duration
	// Backoff is the wait before a retry of a transient item failure (doubled
	// each attempt). Default one second.
	Backoff time.Duration
	// AbortAfter is the number of consecutive upstream failures after which a
	// run stops instead of hammering a dead upstream. Default 25.
	AbortAfter int

	poke   chan struct{}
	mu     sync.Mutex
	active map[string]bool
	wg     sync.WaitGroup
}

// Notify asks the scheduler to look for queued runs now.
func (s *Service) Notify() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.poke == nil {
		s.poke = make(chan struct{}, 1)
	}
	ch := s.poke
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Run drives schedules and executes runs until ctx ends. Executors stop with
// ctx and leave their checkpoints for the next start.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.poke == nil {
		s.poke = make(chan struct{}, 1)
	}
	if s.active == nil {
		s.active = map[string]bool{}
	}
	poke := s.poke
	s.mu.Unlock()
	interval := s.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-ticker.C:
		case <-poke:
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	if err := s.schedule(ctx); err != nil && ctx.Err() == nil {
		s.logger().Warn("prewarm schedule failed", "reason", err)
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM prewarm_runs WHERE instance_id=$1 AND state IN ('queued','running') ORDER BY created_at`, s.InstanceID)
	if err != nil {
		if ctx.Err() == nil {
			s.logger().Warn("prewarm run lookup failed", "reason", err)
		}
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.mu.Lock()
		busy := s.active[id]
		if !busy {
			s.active[id] = true
		}
		s.mu.Unlock()
		if busy {
			continue
		}
		s.wg.Add(1)
		go func(id string) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.active, id)
				s.mu.Unlock()
			}()
			s.execute(ctx, id)
		}(id)
	}
}

// schedule starts a run for every due scheduled job.
func (s *Service) schedule(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT id::text,created_by::text FROM prewarm_jobs WHERE instance_id=$1 AND enabled AND interval_seconds IS NOT NULL AND next_run_at<=now() ORDER BY next_run_at LIMIT 50`, s.InstanceID)
	if err != nil {
		return err
	}
	type due struct{ id, by string }
	var jobs []due
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.id, &d.by); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, d)
	}
	rows.Close()
	for _, d := range jobs {
		tx, err := s.DB.Begin(ctx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE prewarm_jobs SET next_run_at=now()+make_interval(secs=>interval_seconds) WHERE id=$1 AND enabled AND next_run_at<=now()`, d.id)
		if err != nil || tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue
		}
		run, err := StartRun(ctx, tx, s.InstanceID, d.id, "schedule", d.by, "")
		switch {
		case err == nil:
			if err = events.Emit(ctx, tx, s.InstanceID, d.by, "prewarm.run.started", run.ID, map[string]any{"run_id": run.ID, "job_id": d.id, "trigger": "schedule", "total": run.Total}); err == nil {
				err = tx.Commit(ctx)
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
		case errors.Is(err, ErrActive), errors.Is(err, ErrEmpty), errors.Is(err, ErrTooMany):
			// The previous run is still going, or the source is empty: try again at the next interval.
			// The schedule advance is kept so the job is not retried every tick.
			_ = tx.Rollback(ctx)
			_, _ = s.DB.Exec(ctx, `UPDATE prewarm_jobs SET next_run_at=now()+make_interval(secs=>interval_seconds) WHERE id=$1 AND next_run_at<=now()`, d.id)
		default:
			_ = tx.Rollback(ctx)
			return err
		}
	}
	return nil
}

type runInfo struct {
	principal, jobID, feedID, slug, format string
	concurrency, rate                      int
	options                                Options
	cancel, principalOK                    bool
}

func (s *Service) loadRun(ctx context.Context, id string) (runInfo, error) {
	var ri runInfo
	var options []byte
	err := s.DB.QueryRow(ctx, `SELECT r.principal_id::text,r.job_id::text,r.feed_id::text,f.slug,f.format,j.concurrency,j.rate_per_second,j.options,r.cancel_requested,
 EXISTS(SELECT 1 FROM principals p WHERE p.instance_id=r.instance_id AND p.id=r.principal_id AND NOT p.disabled)
 FROM prewarm_runs r JOIN prewarm_jobs j ON j.id=r.job_id JOIN feeds f ON f.id=r.feed_id WHERE r.instance_id=$1 AND r.id=$2`, s.InstanceID, id).
		Scan(&ri.principal, &ri.jobID, &ri.feedID, &ri.slug, &ri.format, &ri.concurrency, &ri.rate, &options, &ri.cancel, &ri.principalOK)
	if err == nil {
		_ = json.Unmarshal(options, &ri.options)
	}
	return ri, err
}

// execute runs one run to completion, or until ctx ends (leaving it resumable).
func (s *Service) execute(ctx context.Context, id string) {
	info, err := s.loadRun(ctx, id)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, pgx.ErrNoRows) {
			s.logger().Warn("prewarm run cannot be loaded", "reason", err)
		}
		return
	}
	metrics.active.Add(1)
	defer metrics.active.Add(-1)
	if _, err = s.DB.Exec(ctx, `UPDATE prewarm_runs SET state='running',started_at=COALESCE(started_at,now()) WHERE id=$1 AND state IN ('queued','running')`, id); err != nil {
		return
	}
	// Items that were in flight when a previous process stopped were never
	// checkpointed, so they are still pending and are simply done again.
	if !info.principalOK {
		s.finish(ctx, id, info, "failed", "the principal that owns the run no longer exists or is disabled")
		return
	}
	target := Target{FeedID: info.feedID, Slug: info.slug, Format: info.format, Principal: identity.Principal{InstanceID: s.InstanceID, PrincipalID: info.principal}, Options: info.options}
	f := formats[info.format]
	if f == nil || s.Warmer == nil {
		s.finish(ctx, id, info, "failed", "this feed format cannot be prewarmed")
		return
	}
	work, stop := context.WithCancel(ctx)
	defer stop()
	var canceled, aborted atomic.Bool
	var wg sync.WaitGroup
	// Cancellation requests and live counters go through the database so that
	// any API call (or another process) sees and affects the run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-t.C:
				var cancel bool
				if err := s.DB.QueryRow(work, `SELECT cancel_requested FROM prewarm_runs WHERE id=$1`, id).Scan(&cancel); errors.Is(err, pgx.ErrNoRows) || (err == nil && cancel) {
					canceled.Store(true)
					stop()
					return
				}
				s.flushCounters(work, id)
			}
		}
	}()
	pace := newPacer(info.rate)
	sem := make(chan struct{}, max(1, info.concurrency))
	var consecutive atomic.Int32
	abortAfter := int32(s.AbortAfter)
	if abortAfter <= 0 {
		abortAfter = 25
	}
	if info.cancel {
		canceled.Store(true)
		stop()
	}
batches:
	for work.Err() == nil {
		rows, err := s.DB.Query(work, `SELECT seq,name,version,integrity,aux FROM prewarm_run_items WHERE run_id=$1 AND state='pending' ORDER BY seq LIMIT 200`, id)
		if err != nil {
			break
		}
		type pending struct {
			seq  int
			item Item
		}
		var batch []pending
		for rows.Next() {
			var p pending
			if rows.Scan(&p.seq, &p.item.Name, &p.item.Version, &p.item.Integrity, &p.item.Aux) == nil {
				batch = append(batch, p)
			}
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		var bwg sync.WaitGroup
		for _, p := range batch {
			if err := pace.wait(work); err != nil {
				break
			}
			select {
			case sem <- struct{}{}:
			case <-work.Done():
				break
			}
			if work.Err() != nil {
				break
			}
			bwg.Add(1)
			go func() {
				defer bwg.Done()
				defer func() { <-sem }()
				state, bytes, attempts, code := s.warmOne(work, target, f, p.item)
				if work.Err() != nil && state == ItemFailed {
					return // interrupted by cancel or shutdown: leave pending
				}
				switch {
				case state == ItemFailed && (code == ErrUnavail || code == ErrTimeout):
					if consecutive.Add(1) >= abortAfter && aborted.CompareAndSwap(false, true) {
						stop()
					}
				default:
					consecutive.Store(0)
				}
				wctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer done()
				if ctx.Err() != nil {
					return
				}
				var msg any
				if code != "" {
					msg = code
				}
				_, err := s.DB.Exec(wctx, `UPDATE prewarm_run_items SET state=$3,bytes=$4,attempts=$5,error=$6,finished_at=now() WHERE run_id=$1 AND seq=$2`, id, p.seq, state, bytes, attempts, msg)
				if err != nil {
					s.logger().Warn("prewarm checkpoint failed", "reason", err)
				}
				metrics.item(state, bytes)
			}()
		}
		bwg.Wait()
		if work.Err() != nil {
			break batches
		}
	}
	stop()
	wg.Wait()
	if ctx.Err() != nil {
		return // shutdown or lease loss: resumable from the checkpoints
	}
	switch {
	case aborted.Load():
		s.finish(context.WithoutCancel(ctx), id, info, "failed", "stopped: the upstream kept failing; the remaining items are left pending and can be retried")
	case canceled.Load():
		s.finish(context.WithoutCancel(ctx), id, info, StateCanceled, "")
	default:
		s.finish(context.WithoutCancel(ctx), id, info, "", "")
	}
}

// warmOne warms one item with bounded retries of transient failures and returns
// its final state, bytes read, attempts and failure code.
func (s *Service) warmOne(ctx context.Context, t Target, f *format, it Item) (state string, bytes int64, attempts int, code string) {
	backoff := s.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for attempts = 1; ; attempts++ {
		res := &Result{}
		err := safeWarm(ctx, f, s.Warmer, t, it, res)
		bytes += res.Bytes()
		if err == nil {
			if res.Filled() > 0 {
				return ItemWarmed, bytes, attempts, ""
			}
			return ItemCached, bytes, attempts, ""
		}
		var ie *ItemError
		if !errors.As(err, &ie) {
			ie = &ItemError{Code: ErrInternal}
		}
		if ie.Skip {
			return ItemSkipped, bytes, attempts, ie.Code
		}
		if !Retryable(err) || attempts >= 3 || ctx.Err() != nil {
			return ItemFailed, bytes, attempts, ie.Code
		}
		select {
		case <-time.After(backoff << (attempts - 1)):
		case <-ctx.Done():
			return ItemFailed, bytes, attempts, ie.Code
		}
	}
}

func safeWarm(ctx context.Context, f *format, w *Warmer, t Target, it Item, res *Result) (err error) {
	defer func() {
		if recover() != nil {
			err = itemErr(ErrInternal, "")
		}
	}()
	return f.warm(ctx, w, t, it, res)
}

// flushCounters stores live progress on the run row.
func (s *Service) flushCounters(ctx context.Context, id string) {
	_, _ = s.DB.Exec(ctx, `UPDATE prewarm_runs r SET warmed=c.warmed,cached=c.cached,failed=c.failed,skipped=c.skipped,bytes=c.bytes FROM (
 SELECT count(*) FILTER (WHERE state='warmed') AS warmed,count(*) FILTER (WHERE state='cached') AS cached,count(*) FILTER (WHERE state='failed') AS failed,count(*) FILTER (WHERE state='skipped') AS skipped,COALESCE(sum(bytes),0) AS bytes
 FROM prewarm_run_items WHERE run_id=$1) c WHERE r.id=$1`, id)
}

// finish records the final state. An empty state is derived from the counts.
func (s *Service) finish(ctx context.Context, id string, info runInfo, state, reason string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if state == StateCanceled {
		if _, err = tx.Exec(ctx, `UPDATE prewarm_run_items SET state='skipped',error='canceled',finished_at=now() WHERE run_id=$1 AND state='pending'`, id); err != nil {
			return
		}
	}
	var r Run
	err = tx.QueryRow(ctx, `UPDATE prewarm_runs r SET warmed=c.warmed,cached=c.cached,failed=c.failed,skipped=c.skipped,bytes=c.bytes FROM (
 SELECT count(*) FILTER (WHERE state='warmed') AS warmed,count(*) FILTER (WHERE state='cached') AS cached,count(*) FILTER (WHERE state='failed') AS failed,count(*) FILTER (WHERE state='skipped') AS skipped,COALESCE(sum(bytes),0) AS bytes
 FROM prewarm_run_items WHERE run_id=$1) c WHERE r.id=$1 RETURNING r.warmed,r.cached,r.failed,r.skipped,r.bytes`, id).Scan(&r.Warmed, &r.Cached, &r.Failed, &r.Skipped, &r.Bytes)
	if err != nil {
		return
	}
	if state == "" {
		switch {
		case r.Failed == 0:
			state = StateSucceeded
		case r.Warmed+r.Cached+r.Skipped > 0:
			state = StatePartial
		default:
			state = StateFailed
		}
	}
	var msg any
	if reason != "" {
		msg = clip(reason, 250)
	}
	if _, err = tx.Exec(ctx, `UPDATE prewarm_runs SET state=$2,error=$3,finished_at=now() WHERE id=$1`, id, state, msg); err != nil {
		return
	}
	if err = events.Emit(ctx, tx, s.InstanceID, info.principal, "prewarm.run.completed", id, map[string]any{"run_id": id, "job_id": info.jobID, "state": state, "warmed": r.Warmed, "cached": r.Cached, "failed": r.Failed, "skipped": r.Skipped, "bytes": r.Bytes}); err != nil {
		return
	}
	if err = tx.Commit(ctx); err != nil {
		return
	}
	metrics.run(state)
	Prune(ctx, s.DB, s.InstanceID, info.jobID)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimSpace(s)
}

// pacer spaces item starts at a fixed rate; zero means unlimited.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newPacer(perSecond int) *pacer {
	if perSecond <= 0 {
		return &pacer{}
	}
	return &pacer{interval: time.Second / time.Duration(perSecond)}
}

func (p *pacer) wait(ctx context.Context) error {
	if p.interval == 0 {
		return ctx.Err()
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	at := p.next
	p.next = at.Add(p.interval)
	p.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}
