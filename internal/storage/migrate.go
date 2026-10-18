package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrBusy means another migration or cleanup job is unfinished for the instance.
	ErrBusy = errors.New("another storage job is unfinished")
	// ErrCleanupPending means the previous migration's old store was not cleaned up yet.
	ErrCleanupPending = errors.New("previous storage cleanup is pending")
	// ErrNothingToClean means no old store is retained.
	ErrNothingToClean = errors.New("no previous storage to clean up")
	// ErrBadTarget rejects an unknown, unconfigured or already-active target.
	ErrBadTarget = errors.New("invalid migration target")
	// ErrNotFoundJob means the job does not exist.
	ErrNotFoundJob = errors.New("storage job not found")
	errPaused      = errors.New("storage job paused")
)

// Backends addresses concrete backend stores independently of routing.
type Backends interface {
	Backend(ctx context.Context, kind, domain string) (blob.Store, error)
	Available() []string
}

// Job kinds and states.
const (
	KindMigrate = "migrate"
	KindCleanup = "cleanup"

	StateRunning   = "running"
	StatePaused    = "paused"
	StateFailed    = "failed"
	StateCompleted = "completed"
)

// staleAfter is how long a running job may go without a heartbeat before
// another process may take it over.
const staleAfter = 90 * time.Second

// Job is the persisted state and progress of one migration or cleanup. For a dry
// run the counters are projections: Copied* is what a real run would copy and
// Deleted* what a cleanup would remove.
type Job struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	From         string     `json:"from"`
	To           string     `json:"to"`
	DryRun       bool       `json:"dry_run"`
	State        string     `json:"state"`
	BytesPerSec  int64      `json:"throttle_bytes_per_second"`
	TotalBlobs   int64      `json:"total_blobs"`
	TotalBytes   int64      `json:"total_bytes"`
	Processed    int64      `json:"processed_blobs"`
	ProcessedB   int64      `json:"processed_bytes"`
	Copied       int64      `json:"copied_blobs"`
	CopiedBytes  int64      `json:"copied_bytes"`
	Skipped      int64      `json:"skipped_blobs"`
	Deleted      int64      `json:"deleted_blobs"`
	DeletedBytes int64      `json:"deleted_bytes"`
	Failed       int64      `json:"failed_blobs"`
	Pass         int        `json:"pass"`
	LastError    string     `json:"last_error,omitempty"`
	RequestedBy  string     `json:"requested_by,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	SwitchedAt   *time.Time `json:"switched_at,omitempty"`

	passCopied int64
	cursor     *string
}

// Status summarizes the storage configuration and the latest job.
type Status struct {
	Active    string   `json:"active_backend"`
	Previous  string   `json:"previous_backend,omitempty"`
	Available []string `json:"available_backends"`
	Current   *Job     `json:"current_job,omitempty"`
}

// StartOptions describes a requested job.
type StartOptions struct {
	Kind        string // KindMigrate or KindCleanup
	To          string // migration target backend
	DryRun      bool
	BytesPerSec int64 // 0 = unthrottled
	RequestedBy string
}

// Migrator copies every blob of one instance between backends, verifies each
// copy, checkpoints a durable cursor, and atomically switches the active
// backend. It is safe to interrupt at any point: Run again to resume.
type Migrator struct {
	DB         *pgxpool.Pool
	InstanceID string
	Backends   Backends
	// Default is the configured backend used while no switch is recorded.
	Default string
	// OnSwitch runs after the active backend changed (cache invalidation).
	OnSwitch func()
	// Owner identifies this process in job rows.
	Owner string
	// CheckpointEvery bounds how often progress is persisted (default 1s).
	CheckpointEvery time.Duration
	// HeartbeatEvery is the liveness interval (default 10s).
	HeartbeatEvery time.Duration
	// AfterBlob, if set, runs after each processed blob (tests inject interruptions).
	AfterBlob func(Job)

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

const jobColumns = `id::text,kind,from_backend,to_backend,dry_run,state,throttle_bytes_per_second,total_blobs,total_bytes,processed_blobs,processed_bytes,copied_blobs,copied_bytes,skipped_blobs,deleted_blobs,deleted_bytes,failed_blobs,pass,pass_copied,cursor::text,COALESCE(last_error,''),COALESCE(requested_by::text,''),started_at,updated_at,finished_at,switched_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Kind, &j.From, &j.To, &j.DryRun, &j.State, &j.BytesPerSec, &j.TotalBlobs, &j.TotalBytes, &j.Processed, &j.ProcessedB, &j.Copied, &j.CopiedBytes, &j.Skipped, &j.Deleted, &j.DeletedBytes, &j.Failed, &j.Pass, &j.passCopied, &j.cursor, &j.LastError, &j.RequestedBy, &j.StartedAt, &j.UpdatedAt, &j.FinishedAt, &j.SwitchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, ErrNotFoundJob
	}
	return j, err
}

func (m *Migrator) owner() string {
	if m.Owner != "" {
		return m.Owner
	}
	return "ant"
}

// Route reports the persisted route.
func (m *Migrator) Route(ctx context.Context) (Route, error) {
	return LoadRoute(ctx, m.DB, m.InstanceID, m.Default)
}

// Status returns the route, available backends and the most recent job.
func (m *Migrator) Status(ctx context.Context) (Status, error) {
	rt, err := m.Route(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{Active: rt.Active, Previous: rt.Previous, Available: m.Backends.Available()}
	j, err := scanJob(m.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM storage_migrations WHERE instance_id=$1 ORDER BY started_at DESC LIMIT 1`, m.InstanceID))
	if err == nil {
		st.Current = &j
	} else if !errors.Is(err, ErrNotFoundJob) {
		return Status{}, err
	}
	return st, nil
}

// Get returns one job.
func (m *Migrator) Get(ctx context.Context, id string) (Job, error) {
	return scanJob(m.DB.QueryRow(ctx, `SELECT `+jobColumns+` FROM storage_migrations WHERE instance_id=$1 AND id::text=$2`, m.InstanceID, id))
}

// List returns recent jobs, newest first.
func (m *Migrator) List(ctx context.Context, limit int) ([]Job, error) {
	rows, err := m.DB.Query(ctx, `SELECT `+jobColumns+` FROM storage_migrations WHERE instance_id=$1 ORDER BY started_at DESC LIMIT $2`, m.InstanceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Start creates a job, or takes over an interrupted job with the same shape
// (paused, or running with a stale heartbeat). The job is owned by this process
// but not executed; call Run. The second result reports a takeover.
func (m *Migrator) Start(ctx context.Context, o StartOptions) (Job, bool, error) {
	rt, err := m.Route(ctx)
	if err != nil {
		return Job{}, false, err
	}
	var from, to string
	switch o.Kind {
	case KindMigrate:
		if !slices.Contains(m.Backends.Available(), o.To) || o.To == rt.Active {
			return Job{}, false, ErrBadTarget
		}
		if rt.Previous != "" {
			return Job{}, false, ErrCleanupPending
		}
		from, to = rt.Active, o.To
	case KindCleanup:
		if rt.Previous == "" {
			return Job{}, false, ErrNothingToClean
		}
		from, to = rt.Previous, rt.Active
	default:
		return Job{}, false, ErrBadTarget
	}
	if o.BytesPerSec < 0 {
		return Job{}, false, ErrBadTarget
	}
	// Take over an interrupted job of the same shape.
	j, err := scanJob(m.DB.QueryRow(ctx, `UPDATE storage_migrations SET state='running',owner=$2,updated_at=now(),throttle_bytes_per_second=$3
 WHERE id=(SELECT id FROM storage_migrations WHERE instance_id=$1 AND kind=$4 AND from_backend=$5 AND to_backend=$6 AND dry_run=$7
   AND (state='paused' OR (state='running' AND updated_at<now()-$8::interval)) LIMIT 1 FOR UPDATE SKIP LOCKED)
 RETURNING `+jobColumns, m.InstanceID, m.owner(), o.BytesPerSec, o.Kind, from, to, o.DryRun, fmt.Sprintf("%d seconds", int(staleAfter.Seconds()))))
	if err == nil {
		return j, true, nil
	}
	if !errors.Is(err, ErrNotFoundJob) {
		return Job{}, false, err
	}
	var requestedBy any
	if o.RequestedBy != "" {
		requestedBy = o.RequestedBy
	}
	var total, totalBytes int64
	if err = m.DB.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size_bytes),0)::bigint FROM blobs WHERE instance_id=$1`, m.InstanceID).Scan(&total, &totalBytes); err != nil {
		return Job{}, false, err
	}
	j, err = scanJob(m.DB.QueryRow(ctx, `INSERT INTO storage_migrations(instance_id,kind,from_backend,to_backend,dry_run,state,owner,throttle_bytes_per_second,total_blobs,total_bytes,requested_by)
 VALUES($1,$2,$3,$4,$5,'running',$6,$7,$8,$9,$10) RETURNING `+jobColumns, m.InstanceID, o.Kind, from, to, o.DryRun, m.owner(), o.BytesPerSec, total, totalBytes, requestedBy))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Job{}, false, ErrBusy
	}
	return j, false, err
}

// Pause stops a running job at its next checkpoint; its cursor is kept.
func (m *Migrator) Pause(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	cancel := m.running[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_, err := m.DB.Exec(ctx, `UPDATE storage_migrations SET state='paused',updated_at=now() WHERE instance_id=$1 AND id::text=$2 AND state='running'`, m.InstanceID, id)
	if err != nil {
		return Job{}, err
	}
	return m.Get(ctx, id)
}

// Resume takes over a paused (or abandoned) job so Run can continue it.
func (m *Migrator) Resume(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(m.DB.QueryRow(ctx, `UPDATE storage_migrations SET state='running',owner=$3,updated_at=now() WHERE instance_id=$1 AND id::text=$2
 AND (state='paused' OR (state='running' AND updated_at<now()-$4::interval)) RETURNING `+jobColumns, m.InstanceID, id, m.owner(), fmt.Sprintf("%d seconds", int(staleAfter.Seconds()))))
	if errors.Is(err, ErrNotFoundJob) {
		if cur, gerr := m.Get(ctx, id); gerr == nil {
			if cur.State == StateRunning {
				return cur, ErrBusy
			}
			return cur, ErrBadTarget
		}
	}
	return j, err
}

// Run executes a job already claimed by Start or Resume until it finishes,
// fails, or ctx ends. Cancelling ctx pauses the job so it can be resumed.
func (m *Migrator) Run(ctx context.Context, id string) (Job, error) {
	job, err := m.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if job.State != StateRunning {
		return job, ErrBadTarget
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	if m.running == nil {
		m.running = map[string]context.CancelFunc{}
	}
	m.running[id] = cancel
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.running, id); m.mu.Unlock() }()

	hb := m.HeartbeatEvery
	if hb <= 0 {
		hb = 10 * time.Second
	}
	hbDone := make(chan struct{})
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		t := time.NewTicker(hb)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-runCtx.Done():
				return
			case <-t.C:
				tag, err := m.DB.Exec(runCtx, `UPDATE storage_migrations SET updated_at=now() WHERE id::text=$1 AND state='running' AND owner=$2`, id, m.owner())
				if err == nil && tag.RowsAffected() == 0 {
					cancel() // paused or taken over elsewhere
					return
				}
			}
		}
	}()
	execErr := m.execute(runCtx, &job)
	close(hbDone)
	hbWG.Wait()

	fin, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer stop()
	switch {
	case execErr == nil:
		return m.Get(fin, id)
	case errors.Is(execErr, errPaused) || errors.Is(execErr, context.Canceled):
		_, _ = m.DB.Exec(fin, `UPDATE storage_migrations SET state='paused',updated_at=now() WHERE id::text=$1 AND state='running' AND owner=$2`, id, m.owner())
		got, _ := m.Get(fin, id)
		if ctx.Err() != nil {
			return got, ctx.Err()
		}
		return got, nil
	default:
		msg := execErr.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		_, _ = m.DB.Exec(fin, `UPDATE storage_migrations SET state='failed',last_error=$3,finished_at=now(),updated_at=now() WHERE id::text=$1 AND state='running' AND owner=$2`, id, m.owner(), msg)
		got, _ := m.Get(fin, id)
		return got, execErr
	}
}

type blobRow struct {
	id, domain, digest string
	size               int64
}

const maxPasses = 6

func (m *Migrator) execute(ctx context.Context, job *Job) error {
	th := &throttle{bps: job.BytesPerSec}
	last := time.Now()
	every := m.CheckpointEvery
	if every <= 0 {
		every = time.Second
	}
	for {
		for {
			rows, err := m.batch(ctx, job.cursor)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			for _, b := range rows {
				if err = ctx.Err(); err != nil {
					return err
				}
				m.handle(ctx, job, b, th)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				id := b.id
				job.cursor = &id
				if job.Pass == 1 {
					job.Processed++
					job.ProcessedB += b.size
				}
				if time.Since(last) >= every {
					if err = m.checkpoint(ctx, job); err != nil {
						return err
					}
					last = time.Now()
				}
				if m.AfterBlob != nil {
					m.AfterBlob(*job)
				}
			}
			if err = m.checkpoint(ctx, job); err != nil {
				return err
			}
			last = time.Now()
		}
		// End of a pass.
		switch {
		case job.Failed > 0:
			if err := m.checkpoint(ctx, job); err != nil {
				return err
			}
			return fmt.Errorf("%d blobs failed verification or copy", job.Failed)
		case job.Kind == KindCleanup || job.DryRun:
			return m.finish(ctx, job, false)
		case job.Pass > 1 && job.passCopied == 0:
			return m.finish(ctx, job, true)
		case job.Pass >= maxPasses:
			return errors.New("catch-up passes did not converge; blobs are still being written")
		}
		// Another pass picks up blobs committed while earlier ones were copied.
		job.Pass++
		job.passCopied = 0
		job.cursor = nil
		if err := m.checkpoint(ctx, job); err != nil {
			return err
		}
	}
}

func (m *Migrator) batch(ctx context.Context, cursor *string) ([]blobRow, error) {
	rows, err := m.DB.Query(ctx, `SELECT id::text,dedup_domain_id::text,digest,size_bytes FROM blobs WHERE instance_id=$1 AND ($2::uuid IS NULL OR id>$2::uuid) ORDER BY id LIMIT 100`, m.InstanceID, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blobRow
	for rows.Next() {
		var b blobRow
		if err = rows.Scan(&b.id, &b.domain, &b.digest, &b.size); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// checkpoint persists progress atomically with the cursor. Zero rows affected
// means the job was paused or taken over, so this runner must stop.
func (m *Migrator) checkpoint(ctx context.Context, j *Job) error {
	tag, err := m.DB.Exec(ctx, `UPDATE storage_migrations SET processed_blobs=$3,processed_bytes=$4,copied_blobs=$5,copied_bytes=$6,skipped_blobs=$7,deleted_blobs=$8,deleted_bytes=$9,failed_blobs=$10,pass=$11,pass_copied=$12,cursor=$13::uuid,updated_at=now()
 WHERE id::text=$1 AND state='running' AND owner=$2`, j.ID, m.owner(), j.Processed, j.ProcessedB, j.Copied, j.CopiedBytes, j.Skipped, j.Deleted, j.DeletedBytes, j.Failed, j.Pass, j.passCopied, j.cursor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errPaused
	}
	return nil
}

// finish completes the job; for a real migration it switches the active
// backend in the same transaction, so the switch is atomic and durable.
func (m *Migrator) finish(ctx context.Context, j *Job, switchActive bool) error {
	tx, err := m.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, `UPDATE storage_migrations SET state='completed',processed_blobs=$3,processed_bytes=$4,copied_blobs=$5,copied_bytes=$6,skipped_blobs=$7,deleted_blobs=$8,deleted_bytes=$9,failed_blobs=$10,pass=$11,pass_copied=$12,cursor=$13::uuid,
 finished_at=now(),updated_at=now(),switched_at=CASE WHEN $14 THEN now() END WHERE id::text=$1 AND state='running' AND owner=$2`,
		j.ID, m.owner(), j.Processed, j.ProcessedB, j.Copied, j.CopiedBytes, j.Skipped, j.Deleted, j.DeletedBytes, j.Failed, j.Pass, j.passCopied, j.cursor, switchActive)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errPaused
	}
	switch {
	case switchActive:
		if _, err = tx.Exec(ctx, `INSERT INTO storage_active(instance_id,backend,previous_backend) VALUES($1,$2,$3)
 ON CONFLICT(instance_id) DO UPDATE SET backend=EXCLUDED.backend,previous_backend=EXCLUDED.previous_backend,switched_at=now()`, m.InstanceID, j.To, j.From); err != nil {
			return err
		}
	case j.Kind == KindCleanup && !j.DryRun:
		if _, err = tx.Exec(ctx, `UPDATE storage_active SET previous_backend=NULL WHERE instance_id=$1 AND previous_backend=$2`, m.InstanceID, j.From); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if switchActive && m.OnSwitch != nil {
		m.OnSwitch()
	}
	return nil
}

// handle processes one blob and updates the job counters.
func (m *Migrator) handle(ctx context.Context, j *Job, b blobRow, th *throttle) {
	want := blob.Info{Digest: blob.Digest(b.digest), Size: b.size}
	copied, err := m.ensure(ctx, j, b.domain, want, th)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		j.Failed++
		return
	}
	if copied {
		j.Copied++
		j.CopiedBytes += b.size
		j.passCopied++
	} else if j.Pass == 1 && j.Kind == KindMigrate {
		j.Skipped++
	}
	if j.Kind != KindCleanup {
		return
	}
	// Cleanup: the blob is now verified on the active store; remove the old copy.
	src, err := m.Backends.Backend(ctx, j.From, b.domain)
	if err != nil {
		j.Failed++
		return
	}
	if _, err = src.Stat(ctx, want.Digest); err != nil {
		if !errors.Is(err, blob.ErrNotFound) {
			j.Failed++
		}
		return
	}
	if !j.DryRun {
		del, ok := src.(blob.Deleter)
		if !ok {
			j.Failed++
			return
		}
		if err = del.Delete(ctx, want.Digest); err != nil {
			j.Failed++
			return
		}
	}
	j.Deleted++
	j.DeletedBytes += b.size
}

// ensure makes the target hold a verified copy of one blob. It reports whether
// a copy was (or, in a dry run, would be) made. A present target is verified by
// size and by hashing its content; a corrupt one is replaced.
func (m *Migrator) ensure(ctx context.Context, j *Job, domain string, want blob.Info, th *throttle) (bool, error) {
	src, err := m.Backends.Backend(ctx, j.From, domain)
	if err != nil {
		return false, err
	}
	dst, err := m.Backends.Backend(ctx, j.To, domain)
	if err != nil {
		return false, err
	}
	corrupt := false
	info, err := dst.Stat(ctx, want.Digest)
	switch {
	case err == nil && info.Size == want.Size:
		if verr := verify(ctx, dst, want, th); verr == nil {
			return false, nil
		} else if !errors.Is(verr, blob.ErrIntegrity) {
			return false, verr
		}
		corrupt = true
	case err == nil:
		corrupt = true
	case !errors.Is(err, blob.ErrNotFound):
		return false, err
	}
	if j.DryRun {
		// Same checks a real copy performs: the source must hold the exact bytes.
		if err = verify(ctx, src, want, th); err != nil {
			return false, err
		}
		return true, nil
	}
	if corrupt {
		del, ok := dst.(blob.Deleter)
		if !ok {
			return false, blob.ErrIntegrity
		}
		if err = del.Delete(ctx, want.Digest); err != nil {
			return false, err
		}
	}
	if err = copyBlob(ctx, src, dst, want, th); err != nil {
		return false, err
	}
	if err = verify(ctx, dst, want, th); err != nil {
		return false, err
	}
	return true, nil
}

// copyBlob streams one blob; the destination's Finalize checks size and digest.
func copyBlob(ctx context.Context, src, dst blob.Store, want blob.Info, th *throttle) error {
	rc, err := src.OpenRange(ctx, want.Digest, 0, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	up, err := dst.Stage(ctx)
	if err != nil {
		return err
	}
	if _, err = io.Copy(up, &throttledReader{ctx: ctx, r: rc, t: th}); err != nil {
		_ = up.Abort()
		return err
	}
	_, err = up.Finalize(ctx, want)
	return err
}

// verify hashes a stored blob and compares size and SHA-256 with the record.
func verify(ctx context.Context, s blob.Store, want blob.Info, th *throttle) error {
	rc, err := s.OpenRange(ctx, want.Digest, 0, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, &throttledReader{ctx: ctx, r: rc, t: th})
	if err != nil {
		return err
	}
	if n != want.Size || hex.EncodeToString(h.Sum(nil)) != string(want.Digest) {
		return blob.ErrIntegrity
	}
	return nil
}

// throttle limits cumulative throughput to bps bytes per second (0 = unlimited).
type throttle struct {
	bps   int64
	start time.Time
	n     int64
}

func (t *throttle) wait(ctx context.Context, n int) error {
	if t == nil || t.bps <= 0 || n <= 0 {
		return ctx.Err()
	}
	now := time.Now()
	if t.start.IsZero() || now.Sub(t.start) > time.Duration(float64(t.n)/float64(t.bps)*float64(time.Second))+time.Second {
		// Idle for over a second: do not allow a catch-up burst.
		t.start, t.n = now, 0
	}
	t.n += int64(n)
	due := t.start.Add(time.Duration(float64(t.n) / float64(t.bps) * float64(time.Second)))
	if d := time.Until(due); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

type throttledReader struct {
	ctx context.Context
	r   io.Reader
	t   *throttle
}

func (r *throttledReader) Read(p []byte) (int, error) {
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	n, err := r.r.Read(p)
	if werr := r.t.wait(r.ctx, n); werr != nil {
		return n, werr
	}
	return n, err
}
