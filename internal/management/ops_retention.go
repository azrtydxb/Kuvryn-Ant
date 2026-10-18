package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetentionPolicy targets exactly one feed or source. Content in local and
// replica sources is release content and is exempt unless AllowReleaseDelete.
type RetentionPolicy struct {
	ID                 string `json:"id"`
	FeedID             string `json:"feed_id,omitempty"`
	SourceID           string `json:"source_id,omitempty"`
	KeepLast           *int32 `json:"keep_last,omitempty"`
	MaxAgeSeconds      *int64 `json:"max_age_seconds,omitempty"`
	MaxIdleSeconds     *int64 `json:"max_idle_seconds,omitempty"`
	MinKeep            int32  `json:"min_keep"`
	IncludeTagged      bool   `json:"include_tagged"`
	AllowReleaseDelete bool   `json:"allow_release_delete"`
	Enabled            bool   `json:"enabled"`
	Revision           int64  `json:"revision"`
}
type RetentionPolicyPage struct {
	Items []RetentionPolicy `json:"items"`
	Next  string            `json:"next_after,omitempty"`
}

// CreateRetentionPolicy has exactly one of feed_id and source_id, and at least
// one of keep_last, max_age_seconds and max_idle_seconds.
type CreateRetentionPolicy struct {
	FeedID             string `json:"feed_id,omitempty"`
	SourceID           string `json:"source_id,omitempty"`
	KeepLast           *int32 `json:"keep_last,omitempty"`
	MaxAgeSeconds      *int64 `json:"max_age_seconds,omitempty"`
	MaxIdleSeconds     *int64 `json:"max_idle_seconds,omitempty"`
	MinKeep            *int32 `json:"min_keep,omitempty"`
	IncludeTagged      bool   `json:"include_tagged,omitempty"`
	AllowReleaseDelete bool   `json:"allow_release_delete,omitempty"`
	Enabled            *bool  `json:"enabled,omitempty"`
}

// PutRetentionPolicy replaces the rules of an existing policy; the target is immutable.
type PutRetentionPolicy struct {
	KeepLast           *int32 `json:"keep_last,omitempty"`
	MaxAgeSeconds      *int64 `json:"max_age_seconds,omitempty"`
	MaxIdleSeconds     *int64 `json:"max_idle_seconds,omitempty"`
	MinKeep            *int32 `json:"min_keep,omitempty"`
	IncludeTagged      bool   `json:"include_tagged,omitempty"`
	AllowReleaseDelete bool   `json:"allow_release_delete,omitempty"`
	Enabled            *bool  `json:"enabled,omitempty"`
}

func validRules(keep *int32, age, idle *int64, minKeep *int32) bool {
	if keep == nil && age == nil && idle == nil {
		return false
	}
	return (keep == nil || *keep >= 1) && (age == nil || *age >= 1) && (idle == nil || *idle >= 1) && (minKeep == nil || *minKeep >= 0)
}

const policyColumns = `id::text,COALESCE(feed_id::text,''),COALESCE(source_id::text,''),keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,allow_release_delete,enabled,revision`

func scanPolicy(row interface{ Scan(...any) error }) (RetentionPolicy, error) {
	var p RetentionPolicy
	err := row.Scan(&p.ID, &p.FeedID, &p.SourceID, &p.KeepLast, &p.MaxAgeSeconds, &p.MaxIdleSeconds, &p.MinKeep, &p.IncludeTagged, &p.AllowReleaseDelete, &p.Enabled, &p.Revision)
	return p, err
}

func (h opsHandlers) listRetentionPolicies(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	after, limit, ok := identityPage(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT `+policyColumns+` FROM retention_policies WHERE instance_id=$1 AND ($2='' OR id>NULLIF($2,'')::uuid) ORDER BY id LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := RetentionPolicyPage{Items: []RetentionPolicy{}}
	for rows.Next() {
		item, err := scanPolicy(rows)
		if err != nil {
			identityError(w, r, err)
			return
		}
		out.Items = append(out.Items, item)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].ID
	}
	JSON(w, 200, out)
}

func (h opsHandlers) createRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in CreateRetentionPolicy
	if !sourceJSON(w, r, &in, "feed_id", "source_id", "keep_last", "max_age_seconds", "max_idle_seconds", "min_keep", "include_tagged", "allow_release_delete", "enabled") {
		return
	}
	var valid bool
	if (in.FeedID == "") == (in.SourceID == "") || !validRules(in.KeepLast, in.MaxAgeSeconds, in.MaxIdleSeconds, in.MinKeep) {
		identityInput(w, r)
		return
	}
	target := in.FeedID + in.SourceID
	if target, valid = feedID(target); !valid {
		identityInput(w, r)
		return
	}
	minKeep, enabled := int32(1), true
	if in.MinKeep != nil {
		minKeep = *in.MinKeep
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	h.mutate(w, r, p, "retention.policy.create", target, in, 201, func(ctx context.Context, tx pgx.Tx) (any, error) {
		feed, source := in.FeedID, in.SourceID
		item, err := scanPolicy(tx.QueryRow(ctx, `INSERT INTO retention_policies(instance_id,feed_id,source_id,keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,allow_release_delete,enabled)
 VALUES($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8,$9,$10) RETURNING `+policyColumns, p.InstanceID, feed, source, in.KeepLast, in.MaxAgeSeconds, in.MaxIdleSeconds, minKeep, in.IncludeTagged, in.AllowReleaseDelete, enabled))
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, item.ID, "retention.policy.create", item.Revision)
	})
}

func (h opsHandlers) putRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	var in PutRetentionPolicy
	if !sourceJSON(w, r, &in, "keep_last", "max_age_seconds", "max_idle_seconds", "min_keep", "include_tagged", "allow_release_delete", "enabled") {
		return
	}
	if !validRules(in.KeepLast, in.MaxAgeSeconds, in.MaxIdleSeconds, in.MinKeep) {
		identityInput(w, r)
		return
	}
	minKeep, enabled := int32(1), true
	if in.MinKeep != nil {
		minKeep = *in.MinKeep
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	h.mutate(w, r, p, "retention.policy.put", id, struct {
		In       PutRetentionPolicy
		Revision int64
	}{in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var current int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM retention_policies WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id).Scan(&current); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		item, err := scanPolicy(tx.QueryRow(ctx, `UPDATE retention_policies SET keep_last=$3,max_age_seconds=$4,max_idle_seconds=$5,min_keep=$6,include_tagged=$7,allow_release_delete=$8,enabled=$9,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2 RETURNING `+policyColumns,
			p.InstanceID, id, in.KeepLast, in.MaxAgeSeconds, in.MaxIdleSeconds, minKeep, in.IncludeTagged, in.AllowReleaseDelete, enabled))
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, id, "retention.policy.put", item.Revision)
	})
}

func (h opsHandlers) deleteRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	h.mutate(w, r, p, "retention.policy.delete", id, revision, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		item, err := scanPolicy(tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM retention_policies WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id))
		if err != nil {
			return nil, err
		}
		if item.Revision != revision {
			return nil, database.ErrRevisionConflict
		}
		if _, err = tx.Exec(ctx, `DELETE FROM retention_policies WHERE instance_id=$1 AND id=$2`, p.InstanceID, id); err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, id, "retention.policy.delete", item.Revision)
	})
}

// Report list caps keep stored and returned reports bounded.
const reportListCap = 500

type RunDeletion struct {
	SourceID   string `json:"source_id"`
	Coordinate string `json:"coordinate"`
	Version    string `json:"version"`
	Reason     string `json:"reason"`
}
type RunBlob struct {
	DomainID string `json:"domain_id"`
	Digest   string `json:"digest"`
	Kind     string `json:"kind"`
}
type RunFinding struct {
	DomainID string `json:"domain_id"`
	Digest   string `json:"digest"`
	Kind     string `json:"kind"`
}

// RunReport summarizes one maintenance run. Lists are capped at 500 entries
// (Truncated reports whether any was cut); counters are exact.
type RunReport struct {
	VersionsDeleted     int           `json:"versions_deleted"`
	ProtectedRelease    int           `json:"protected_release"`
	ProtectedTagged     int           `json:"protected_tagged"`
	BlobsMarked         int           `json:"blobs_marked"`
	BlobsSwept          int           `json:"blobs_swept"`
	BlobsUnmarked       int           `json:"blobs_unmarked"`
	SkippedReferenced   int           `json:"skipped_referenced"`
	GCFailed            int           `json:"gc_failed"`
	IntegrityChecked    int           `json:"integrity_checked"`
	IntegrityBytes      int64         `json:"integrity_bytes"`
	IntegrityErrors     int           `json:"integrity_errors"`
	IntegrityMismatches int           `json:"integrity_mismatches"`
	IntegrityWrapped    bool          `json:"integrity_wrapped"`
	Deleted             []RunDeletion `json:"deleted,omitempty"`
	Marked              []RunBlob     `json:"marked,omitempty"`
	Swept               []RunBlob     `json:"swept,omitempty"`
	Mismatches          []RunFinding  `json:"mismatches,omitempty"`
	Errors              []string      `json:"errors,omitempty"`
	Truncated           bool          `json:"truncated"`
}
type RetentionRun struct {
	ID          string     `json:"id"`
	Trigger     string     `json:"trigger"`
	DryRun      bool       `json:"dry_run"`
	State       string     `json:"state"`
	RequestedBy string     `json:"requested_by,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Report      *RunReport `json:"report,omitempty"`
}
type RetentionRunPage struct {
	Items []RetentionRun `json:"items"`
	Next  string         `json:"next_after,omitempty"`
}

func capList[T any](in []T, truncated *bool) []T {
	if len(in) > reportListCap {
		*truncated = true
		return in[:reportListCap]
	}
	return in
}

// ReportFrom converts a retention report into its bounded API form. Error
// strings are replaced by fixed stage names so no internal detail is stored.
func ReportFrom(rep retention.Report) RunReport {
	out := RunReport{
		VersionsDeleted: len(rep.Retention.Deleted), ProtectedRelease: rep.Retention.ProtectedRelease, ProtectedTagged: rep.Retention.ProtectedTagged,
		BlobsMarked: len(rep.GC.Marked), BlobsSwept: len(rep.GC.Swept), BlobsUnmarked: rep.GC.Unmarked, SkippedReferenced: rep.GC.SkippedReferenced, GCFailed: rep.GC.Failed,
		IntegrityChecked: rep.Integrity.Checked, IntegrityBytes: rep.Integrity.Bytes, IntegrityErrors: rep.Integrity.Errors, IntegrityMismatches: len(rep.Integrity.Mismatches), IntegrityWrapped: rep.Integrity.Wrapped,
	}
	for _, d := range rep.Retention.Deleted {
		out.Deleted = append(out.Deleted, RunDeletion{d.SourceID, d.Coordinate, d.Version, d.Reason})
	}
	for _, b := range rep.GC.Marked {
		out.Marked = append(out.Marked, RunBlob{b.Domain, b.Digest, b.Kind})
	}
	for _, b := range rep.GC.Swept {
		out.Swept = append(out.Swept, RunBlob{b.Domain, b.Digest, b.Kind})
	}
	for _, f := range rep.Integrity.Mismatches {
		out.Mismatches = append(out.Mismatches, RunFinding{f.Domain, f.Digest, f.Kind})
	}
	for _, e := range rep.Errors {
		stage := e
		for i := 0; i < len(e); i++ {
			if e[i] == ':' {
				stage = e[:i]
				break
			}
		}
		out.Errors = append(out.Errors, stage+" stage failed")
	}
	out.Deleted, out.Marked = capList(out.Deleted, &out.Truncated), capList(out.Marked, &out.Truncated)
	out.Swept, out.Mismatches = capList(out.Swept, &out.Truncated), capList(out.Mismatches, &out.Truncated)
	return out
}

// RecordRetentionRun stores a completed scheduled (or externally driven) run.
// It never fails the caller: history is best effort.
func RecordRetentionRun(ctx context.Context, db *pgxpool.Pool, instance, trigger string, rep retention.Report, runErr error, started time.Time) {
	report := ReportFrom(rep)
	body, err := json.Marshal(report)
	if err != nil {
		return
	}
	state := "succeeded"
	if runErr != nil {
		state = "failed"
	}
	_, _ = db.Exec(ctx, `INSERT INTO retention_runs(instance_id,trigger,dry_run,state,started_at,finished_at,report) VALUES($1,$2,$3,$4,$5,now(),$6)`, instance, trigger, rep.DryRun, state, started, body)
	_, _ = db.Exec(ctx, `DELETE FROM retention_runs WHERE instance_id=$1 AND id IN (SELECT id FROM retention_runs WHERE instance_id=$1 AND state<>'running' ORDER BY started_at DESC OFFSET 200)`, instance)
}

const runColumns = `id::text,trigger,dry_run,state,COALESCE(requested_by::text,''),started_at,finished_at`

func scanRun(row pgx.Row, withReport bool) (RetentionRun, error) {
	var run RetentionRun
	var body []byte
	var err error
	if withReport {
		err = row.Scan(&run.ID, &run.Trigger, &run.DryRun, &run.State, &run.RequestedBy, &run.StartedAt, &run.FinishedAt, &body)
	} else {
		err = row.Scan(&run.ID, &run.Trigger, &run.DryRun, &run.State, &run.RequestedBy, &run.StartedAt, &run.FinishedAt)
	}
	if err == nil && len(body) > 0 {
		run.Report = &RunReport{}
		if json.Unmarshal(body, run.Report) != nil {
			run.Report = nil
		}
	}
	return run, err
}

func (h opsHandlers) listRetentionRuns(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	after, limit, ok := identityPage(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	// Newest first; the cursor is the id of the last run returned.
	rows, err := h.db.Query(ctx, `SELECT `+runColumns+` FROM retention_runs WHERE instance_id=$1 AND ($2='' OR (started_at,id)<(SELECT started_at,id FROM retention_runs WHERE id=NULLIF($2,'')::uuid AND instance_id=$1)) ORDER BY started_at DESC,id DESC LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := RetentionRunPage{Items: []RetentionRun{}}
	for rows.Next() {
		run, err := scanRun(rows, false)
		if err != nil {
			identityError(w, r, err)
			return
		}
		out.Items = append(out.Items, run)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].ID
	}
	JSON(w, 200, out)
}

func (h opsHandlers) getRetentionRun(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		identityInput(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	run, err := scanRun(h.db.QueryRow(ctx, `SELECT `+runColumns+`,report FROM retention_runs WHERE instance_id=$1 AND id=$2`, p.InstanceID, id), true)
	if err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	JSON(w, 200, run)
}

// StartRetentionRun requests an immediate run.
type StartRetentionRun struct {
	DryRun bool `json:"dry_run"`
}

// startRetentionRun begins a run in the background and answers 202 with the
// running record; clients poll GET /retention/runs/{id}. One manual run may be
// active at a time (409 otherwise). A dry run changes nothing; a real run is
// fenced by the writer lease and the same grace periods as scheduled runs.
func (h opsHandlers) startRetentionRun(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in StartRetentionRun
	if !sourceJSON(w, r, &in, "dry_run") {
		return
	}
	var svc *retention.Service
	if h.d.Retention != nil {
		svc = h.d.Retention(p.InstanceID)
	}
	if svc == nil || h.d.Background == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "maintenance is not available on this instance")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	// A crashed process can leave a stale running row; it must not block forever.
	_, _ = h.db.Exec(ctx, `UPDATE retention_runs SET state='failed',finished_at=now() WHERE instance_id=$1 AND state='running' AND started_at<now()-interval '2 hours'`, p.InstanceID)
	run, err := scanRun(h.db.QueryRow(ctx, `INSERT INTO retention_runs(instance_id,trigger,dry_run,state,requested_by) VALUES($1,'manual',$2,'running',$3) RETURNING `+runColumns, p.InstanceID, in.DryRun, p.PrincipalID), false)
	if err != nil {
		if errors.Is(identityDBError(err), database.ErrConflict) {
			Error(w, r, 409, api.ErrorCodeConflict, "a maintenance run is already in progress")
			return
		}
		identityError(w, r, err)
		return
	}
	_, _ = h.db.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,'retention.run',$3,jsonb_build_object('dry_run',$4::boolean))`, p.InstanceID, p.PrincipalID, run.ID, in.DryRun)
	go func() {
		// Cancelled with the server (or on writer-lease loss); the record is then closed as failed.
		bg, stop := context.WithTimeout(h.d.Background, 2*time.Hour)
		defer stop()
		rep, runErr := svc.RunOnce(bg, in.DryRun)
		body, _ := json.Marshal(ReportFrom(rep))
		state := "succeeded"
		if runErr != nil {
			state = "failed"
		}
		fin, done := context.WithTimeout(context.WithoutCancel(h.d.Background), 10*time.Second)
		defer done()
		_, _ = h.db.Exec(fin, `UPDATE retention_runs SET state=$2,finished_at=now(),report=$3 WHERE id=$1`, run.ID, state, body)
	}()
	w.Header().Set("Location", "/api/v1/retention/runs/"+run.ID)
	JSON(w, 202, run)
}

// IntegrityFinding is one unresolved blob integrity mismatch.
type IntegrityFinding struct {
	ID         int64     `json:"id"`
	DomainID   string    `json:"domain_id"`
	Digest     string    `json:"digest"`
	Kind       string    `json:"kind"`
	DetectedAt time.Time `json:"detected_at"`
}
type IntegrityFindingPage struct {
	Items []IntegrityFinding `json:"items"`
	Next  string             `json:"next_after,omitempty"`
}

func (h opsHandlers) integrityFindings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 50
	var after int64
	for k, v := range q {
		if (k != "after" && k != "limit") || len(v) != 1 {
			identityInput(w, r)
			return
		}
	}
	var err error
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 200 {
			identityInput(w, r)
			return
		}
	}
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 1 {
			identityInput(w, r)
			return
		}
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT id,dedup_domain_id::text,digest,kind,detected_at FROM blob_integrity_findings WHERE instance_id=$1 AND resolved_at IS NULL AND id>$2 ORDER BY id LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := IntegrityFindingPage{Items: []IntegrityFinding{}}
	for rows.Next() {
		var f IntegrityFinding
		if err = rows.Scan(&f.ID, &f.DomainID, &f.Digest, &f.Kind, &f.DetectedAt); err != nil {
			identityError(w, r, err)
			return
		}
		out.Items = append(out.Items, f)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = strconv.FormatInt(out.Items[limit-1].ID, 10)
	}
	JSON(w, 200, out)
}
