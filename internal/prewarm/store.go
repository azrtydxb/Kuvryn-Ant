package prewarm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors returned by the store.
var (
	ErrActive  = errors.New("the job already has an active run")
	ErrEmpty   = errors.New("there is nothing to warm")
	ErrTooMany = errors.New("the source references more items than allowed")
	ErrState   = errors.New("the run is not in a state that allows this")
)

// Querier is satisfied by pgx pools, connections and transactions.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Job is a stored prewarm job.
type Job struct {
	ID              string     `json:"id"`
	FeedID          string     `json:"feed_id"`
	Name            string     `json:"name"`
	SourceKind      string     `json:"source_kind"`
	LockfileType    string     `json:"lockfile_type,omitempty"`
	SourceFeedID    string     `json:"source_feed_id,omitempty"`
	ItemCount       int        `json:"item_count"`
	Options         Options    `json:"options"`
	Concurrency     int        `json:"concurrency"`
	RatePerSecond   int        `json:"rate_per_second"`
	IntervalSeconds *int       `json:"interval_seconds,omitempty"`
	Enabled         bool       `json:"enabled"`
	NextRunAt       *time.Time `json:"next_run_at,omitempty"`
	Revision        int64      `json:"revision"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastRun         *Run       `json:"last_run,omitempty"`
}

// Run is one execution of a job.
type Run struct {
	ID              string     `json:"id"`
	JobID           string     `json:"job_id"`
	FeedID          string     `json:"feed_id"`
	Trigger         string     `json:"trigger"`
	State           string     `json:"state"`
	CancelRequested bool       `json:"cancel_requested"`
	PrincipalID     string     `json:"principal_id"`
	RetryOf         *string    `json:"retry_of,omitempty"`
	Total           int        `json:"total"`
	Warmed          int        `json:"warmed"`
	Cached          int        `json:"cached"`
	Failed          int        `json:"failed"`
	Skipped         int        `json:"skipped"`
	Bytes           int64      `json:"bytes"`
	Error           *string    `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// RunItem is the checkpointed state of one item of a run.
type RunItem struct {
	Seq        int        `json:"seq"`
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	State      string     `json:"state"`
	Bytes      int64      `json:"bytes"`
	Attempts   int        `json:"attempts"`
	Error      *string    `json:"error,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// NewJob is the validated input of InsertJob.
type NewJob struct {
	FeedID          string
	Name            string
	Spec            Spec
	Options         Options
	Concurrency     int
	RatePerSecond   int
	IntervalSeconds *int
	Enabled         bool
	CreatedBy       string
}

const jobColumns = `j.id::text,j.feed_id::text,j.name,j.source_kind,COALESCE(j.lockfile_type,''),COALESCE(j.source_feed_id::text,''),jsonb_array_length(j.items),j.options,j.concurrency,j.rate_per_second,j.interval_seconds,j.enabled,j.next_run_at,j.revision,j.created_by::text,j.created_at,j.updated_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var options []byte
	if err := row.Scan(&j.ID, &j.FeedID, &j.Name, &j.SourceKind, &j.LockfileType, &j.SourceFeedID, &j.ItemCount, &options, &j.Concurrency, &j.RatePerSecond, &j.IntervalSeconds, &j.Enabled, &j.NextRunAt, &j.Revision, &j.CreatedBy, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return j, err
	}
	_ = json.Unmarshal(options, &j.Options)
	return j, nil
}

const runColumns = `r.id::text,r.job_id::text,r.feed_id::text,r.trigger,r.state,r.cancel_requested,r.principal_id::text,r.retry_of::text,r.total,r.warmed,r.cached,r.failed,r.skipped,r.bytes,r.error,r.created_at,r.started_at,r.finished_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.JobID, &r.FeedID, &r.Trigger, &r.State, &r.CancelRequested, &r.PrincipalID, &r.RetryOf, &r.Total, &r.Warmed, &r.Cached, &r.Failed, &r.Skipped, &r.Bytes, &r.Error, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	return r, err
}

// InsertJob stores a job. The first scheduled run is due one interval from now.
func InsertJob(ctx context.Context, q Querier, instance string, n NewJob) (Job, error) {
	items, _ := json.Marshal(n.Spec.Items)
	if n.Spec.Kind == KindFeed {
		items = []byte("[]")
	}
	options, _ := json.Marshal(n.Options)
	var lock, source any
	if n.Spec.LockfileType != "" {
		lock = n.Spec.LockfileType
	}
	if n.Spec.SourceFeedID != "" {
		source = n.Spec.SourceFeedID
	}
	row := q.QueryRow(ctx, `WITH j AS (INSERT INTO prewarm_jobs(instance_id,feed_id,name,source_kind,lockfile_type,source_feed_id,items,options,concurrency,rate_per_second,interval_seconds,enabled,next_run_at,created_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::integer,$12,CASE WHEN $11::integer IS NULL THEN NULL ELSE now()+make_interval(secs=>$11::integer) END,$13) RETURNING *)
 SELECT `+jobColumns+` FROM j`, instance, n.FeedID, n.Name, n.Spec.Kind, lock, source, items, options, n.Concurrency, n.RatePerSecond, n.IntervalSeconds, n.Enabled, n.CreatedBy)
	return scanJob(row)
}

// JobPatch changes a job; nil fields stay as they are. ClearSchedule removes the schedule.
type JobPatch struct {
	Name            *string
	Enabled         *bool
	Concurrency     *int
	RatePerSecond   *int
	IntervalSeconds *int
	ClearSchedule   bool
	Options         *Options
}

// UpdateJob applies a patch if the revision still matches.
func UpdateJob(ctx context.Context, tx pgx.Tx, instance, id string, revision int64, p JobPatch) (Job, error) {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM prewarm_jobs WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, id).Scan(&current); err != nil {
		return Job{}, mapNoRows(err)
	}
	if current != revision {
		return Job{}, database.ErrRevisionConflict
	}
	var options any
	if p.Options != nil {
		b, _ := json.Marshal(*p.Options)
		options = b
	}
	row := tx.QueryRow(ctx, `WITH j AS (UPDATE prewarm_jobs SET
 name=COALESCE($3,name), enabled=COALESCE($4,enabled), concurrency=COALESCE($5,concurrency), rate_per_second=COALESCE($6,rate_per_second),
 options=COALESCE($7::jsonb,options),
 interval_seconds=CASE WHEN $9 THEN NULL WHEN $8::integer IS NOT NULL THEN $8::integer ELSE interval_seconds END,
 next_run_at=CASE WHEN $9 THEN NULL WHEN $8::integer IS NOT NULL THEN now()+make_interval(secs=>$8::integer) WHEN next_run_at IS NULL AND interval_seconds IS NOT NULL THEN now()+make_interval(secs=>interval_seconds) ELSE next_run_at END,
 revision=revision+1, updated_at=now() WHERE instance_id=$1 AND id=$2 RETURNING *)
 SELECT `+jobColumns+` FROM j`, instance, id, p.Name, p.Enabled, p.Concurrency, p.RatePerSecond, options, p.IntervalSeconds, p.ClearSchedule)
	return scanJob(row)
}

// DeleteJob removes a job with its runs if the revision matches.
func DeleteJob(ctx context.Context, tx pgx.Tx, instance, id string, revision int64) error {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM prewarm_jobs WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, id).Scan(&current); err != nil {
		return mapNoRows(err)
	}
	if current != revision {
		return database.ErrRevisionConflict
	}
	_, err := tx.Exec(ctx, `DELETE FROM prewarm_jobs WHERE instance_id=$1 AND id=$2`, instance, id)
	return err
}

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return database.ErrNotFound
	}
	return err
}

// GetJob loads one job with its latest run.
func GetJob(ctx context.Context, q Querier, instance, id string) (Job, error) {
	j, err := scanJob(q.QueryRow(ctx, `SELECT `+jobColumns+` FROM prewarm_jobs j WHERE j.instance_id=$1 AND j.id=$2`, instance, id))
	if err != nil {
		return j, mapNoRows(err)
	}
	j.LastRun = lastRun(ctx, q, instance, id)
	return j, nil
}

func lastRun(ctx context.Context, q Querier, instance, jobID string) *Run {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM prewarm_runs r WHERE r.instance_id=$1 AND r.job_id=$2 ORDER BY r.created_at DESC,r.id DESC LIMIT 1`, instance, jobID))
	if err != nil {
		return nil
	}
	return &r
}

// ListJobs pages jobs by id. feed, when set, restricts to one feed.
func ListJobs(ctx context.Context, q Querier, instance, feed, after string, limit int) ([]Job, error) {
	rows, err := q.Query(ctx, `SELECT `+jobColumns+` FROM prewarm_jobs j WHERE j.instance_id=$1 AND ($2='' OR j.feed_id=NULLIF($2,'')::uuid) AND ($3='' OR j.id>NULLIF($3,'')::uuid) ORDER BY j.id LIMIT $4`, instance, feed, after, limit)
	if err != nil {
		return nil, err
	}
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].LastRun = lastRun(ctx, q, instance, out[i].ID)
	}
	return out, nil
}

// GetRun loads one run.
func GetRun(ctx context.Context, q Querier, instance, id string) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM prewarm_runs r WHERE r.instance_id=$1 AND r.id=$2`, instance, id))
	return r, mapNoRows(err)
}

// ListRuns pages runs newest first. job and feed are optional filters.
func ListRuns(ctx context.Context, q Querier, instance, job, feed, state, after string, limit int) ([]Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM prewarm_runs r WHERE r.instance_id=$1 AND ($2='' OR r.job_id=NULLIF($2,'')::uuid) AND ($3='' OR r.feed_id=NULLIF($3,'')::uuid) AND ($4='' OR r.state=$4)
 AND ($5='' OR (r.created_at,r.id)<(SELECT created_at,id FROM prewarm_runs WHERE id=NULLIF($5,'')::uuid AND instance_id=$1)) ORDER BY r.created_at DESC,r.id DESC LIMIT $6`, instance, job, feed, state, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListItems pages the items of a run in sequence order.
func ListItems(ctx context.Context, q Querier, run, state string, after, limit int) ([]RunItem, error) {
	rows, err := q.Query(ctx, `SELECT seq,name,version,state,bytes,attempts,error,finished_at FROM prewarm_run_items WHERE run_id=$1 AND seq>$2 AND ($3='' OR state=$3) ORDER BY seq LIMIT $4`, run, after, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunItem
	for rows.Next() {
		var it RunItem
		if err = rows.Scan(&it.Seq, &it.Name, &it.Version, &it.State, &it.Bytes, &it.Attempts, &it.Error, &it.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// StartRun creates a queued run of a job and materializes its items: the job's
// stored items, the versions another feed references, or (retryOf) the items of
// an earlier run that did not complete. At most one run per job is active.
func StartRun(ctx context.Context, tx pgx.Tx, instance, jobID, trigger, principal, retryOf string) (Run, error) {
	var kind, feed, sourceFeed string
	var items []byte
	if err := tx.QueryRow(ctx, `SELECT source_kind,feed_id::text,COALESCE(source_feed_id::text,''),items FROM prewarm_jobs WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, jobID).Scan(&kind, &feed, &sourceFeed, &items); err != nil {
		return Run{}, mapNoRows(err)
	}
	var retry any
	if retryOf != "" {
		retry = retryOf
	}
	run, err := scanRun(tx.QueryRow(ctx, `WITH r AS (INSERT INTO prewarm_runs(instance_id,job_id,feed_id,trigger,state,principal_id,retry_of) VALUES($1,$2,$3,$4,'queued',$5,$6) RETURNING *) SELECT `+runColumns+` FROM r`, instance, jobID, feed, trigger, principal, retry))
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return Run{}, ErrActive
		}
		return Run{}, err
	}
	var tag pgconn.CommandTag
	switch {
	case retryOf != "":
		tag, err = tx.Exec(ctx, `INSERT INTO prewarm_run_items(run_id,seq,name,version,integrity,aux) SELECT $1,(row_number() OVER (ORDER BY seq))-1,name,version,integrity,aux FROM prewarm_run_items WHERE run_id=$2 AND (state IN ('failed','pending') OR (state='skipped' AND error='canceled')) ORDER BY seq`, run.ID, retryOf)
	case kind == KindFeed:
		tag, err = tx.Exec(ctx, `INSERT INTO prewarm_run_items(run_id,seq,name,version)
 SELECT $1,(row_number() OVER (ORDER BY coordinate,version))-1,coordinate,version FROM (
  SELECT c.coordinate,v.version FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id JOIN versions v ON v.component_id=c.id
  WHERE fs.instance_id=$2 AND fs.feed_id=$3 AND octet_length(c.coordinate)<=1024 AND octet_length(v.version)<=512 GROUP BY c.coordinate,v.version ORDER BY c.coordinate,v.version LIMIT $4) x`, run.ID, instance, sourceFeed, MaxItems+1)
	default:
		tag, err = tx.Exec(ctx, `INSERT INTO prewarm_run_items(run_id,seq,name,version,integrity,aux)
 SELECT $1,ord-1,e->>'name',COALESCE(e->>'version',''),COALESCE(e->>'integrity',''),COALESCE(e->>'aux','') FROM jsonb_array_elements($2::jsonb) WITH ORDINALITY AS t(e,ord)`, run.ID, items)
	}
	if err != nil {
		return Run{}, err
	}
	switch n := int(tag.RowsAffected()); {
	case n == 0:
		return Run{}, ErrEmpty
	case n > MaxItems:
		return Run{}, ErrTooMany
	default:
		run.Total = n
	}
	if _, err = tx.Exec(ctx, `UPDATE prewarm_runs SET total=$2 WHERE id=$1`, run.ID, run.Total); err != nil {
		return Run{}, err
	}
	return run, nil
}

// RequestCancel cancels a queued run at once and asks a running one to stop.
func RequestCancel(ctx context.Context, tx pgx.Tx, instance, id string) (Run, error) {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM prewarm_runs WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, id).Scan(&state); err != nil {
		return Run{}, mapNoRows(err)
	}
	switch state {
	case StateQueued:
		if _, err := tx.Exec(ctx, `UPDATE prewarm_run_items SET state='skipped',error='canceled',finished_at=now() WHERE run_id=$1 AND state='pending'`, id); err != nil {
			return Run{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE prewarm_runs SET state='canceled',cancel_requested=true,finished_at=now(),skipped=total WHERE id=$1`, id); err != nil {
			return Run{}, err
		}
	case StateRunning:
		if _, err := tx.Exec(ctx, `UPDATE prewarm_runs SET cancel_requested=true WHERE id=$1`, id); err != nil {
			return Run{}, err
		}
	default:
		return Run{}, ErrState
	}
	return GetRun(ctx, tx, instance, id)
}

// Prune keeps the newest KeepRuns finished runs of a job.
func Prune(ctx context.Context, q Querier, instance, jobID string) {
	_, _ = q.Exec(ctx, `DELETE FROM prewarm_runs WHERE instance_id=$1 AND job_id=$2 AND state NOT IN ('queued','running') AND id IN (SELECT id FROM prewarm_runs WHERE instance_id=$1 AND job_id=$2 AND state NOT IN ('queued','running') ORDER BY created_at DESC,id DESC OFFSET $3)`, instance, jobID, KeepRuns)
}
