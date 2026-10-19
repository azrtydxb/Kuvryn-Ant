package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
	"github.com/jackc/pgx/v5"
)

// SourceHealth reports upstream reachability for one source. Only remote
// sources have upstream traffic; other kinds report status not_applicable.
type SourceHealth struct {
	SourceID      string     `json:"source_id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	LastStatus    *int       `json:"last_status_code,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	P95LatencyMS  *int       `json:"p95_latency_ms,omitempty"`
	Requests      int64      `json:"requests"`
	Failures      int64      `json:"failures"`
}
type SourceHealthPage struct {
	Items []SourceHealth `json:"items"`
	Page  api.PageInfo   `json:"page"`
}

// SourceTestResult is the outcome of a connection test.
type SourceTestResult struct {
	SourceID      string `json:"source_id"`
	Reachable     bool   `json:"reachable"`
	Authenticated bool   `json:"authenticated"`
	StatusCode    *int   `json:"status_code,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	Error         string `json:"error,omitempty"`
}

// pager holds validated keyset paging state bound to the caller and endpoint.
type pager struct {
	limit        int
	after, scope string
}

func (h opsHandlers) page(w http.ResponseWriter, r *http.Request, p identity.Principal, name string, extra ...string) (pager, bool) {
	q, err := ParsePage(r.URL.Query())
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page parameters")
		return pager{}, false
	}
	scopeBytes, _ := json.Marshal(append([]string{name, p.InstanceID, p.PrincipalID, p.TokenID}, extra...))
	pg := pager{limit: q.Limit, scope: string(scopeBytes)}
	if q.Cursor != "" {
		pg.after, err = h.codec.Decode(pg.scope, q.Cursor)
		if _, ok := feedID(pg.after); err != nil || !ok {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return pager{}, false
		}
	}
	return pg, true
}

func (h opsHandlers) pageInfo(pg pager, ids []string, more bool) (api.PageInfo, error) {
	info := api.PageInfo{HasMore: more}
	if more && len(ids) > 0 {
		next, err := h.codec.Encode(pg.scope, ids[len(ids)-1])
		if err != nil {
			return info, err
		}
		info.NextCursor = &next
	}
	return info, nil
}

// permittedIDs returns up to limit permitted identifiers after pg.after from
// query (which must select a single id column ordered ascending and accept
// instance and after as $1 and $2). Authorization precedes both data and the
// next-page marker.
func (h opsHandlers) permittedIDs(ctx context.Context, p identity.Principal, pg pager, query string, permit func(id string) bool) ([]string, bool, error) {
	rows, err := h.db.Query(ctx, query, p.InstanceID, pg.after)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	ids := []string{}
	more := false
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, false, err
		}
		if !permit(id) {
			continue
		}
		if len(ids) == pg.limit {
			more = true
			break
		}
		ids = append(ids, id)
	}
	return ids, more, rows.Err()
}

const (
	// failingAfter is how long errors must have gone without any later success
	// before a source is "failing": shorter gaps are transient noise.
	failingAfter = 5 * time.Minute
	// degradedWindow is how long a source stays "degraded" after its last error.
	degradedWindow = 15 * time.Minute
	// staleAfter is how long a source whose last event was an error may sit
	// idle before its status ages to "unknown" (nothing recent proves either way).
	staleAfter = 24 * time.Hour
)

// healthStatus derives the console status from persisted/live counters.
//
//	no traffic                         -> unknown
//	never an error                     -> healthy
//	never a success                    -> failing (unknown once the error is stale)
//	last event a success, error <15m   -> degraded; older -> healthy
//	last event an error:
//	  success within failingAfter of it -> degraded, even if the source then went idle
//	  otherwise                         -> failing
//	  either ages to unknown after staleAfter without traffic
func healthStatus(s SourceHealth) string { return healthStatusAt(s, time.Now()) }

func healthStatusAt(s SourceHealth, now time.Time) string {
	if s.Requests == 0 || s.LastSuccessAt == nil && s.LastErrorAt == nil {
		return "unknown"
	}
	if s.LastErrorAt == nil {
		return "healthy"
	}
	errAt := *s.LastErrorAt
	if s.LastSuccessAt == nil || errAt.After(*s.LastSuccessAt) {
		if now.Sub(errAt) > staleAfter {
			return "unknown"
		}
		// Errors since the last success: failing only when they span a meaningful
		// period. A source that went idle right after an isolated error carries no
		// evidence of an outage, so it stays degraded until new traffic or staleness.
		if s.LastSuccessAt != nil && errAt.Sub(*s.LastSuccessAt) <= failingAfter {
			return "degraded"
		}
		return "failing"
	}
	if now.Sub(errAt) < degradedWindow {
		return "degraded"
	}
	return "healthy"
}

// loadHealth merges persisted rows with the live in-memory view.
func (h opsHandlers) loadHealth(ctx context.Context, instance string, ids []string) ([]SourceHealth, error) {
	rows, err := h.db.Query(ctx, `SELECT s.id::text,s.kind,rh.last_success_at,rh.last_error_at,rh.last_status,COALESCE(rh.last_error,''),rh.p95_ms,COALESCE(rh.requests,0),COALESCE(rh.failures,0)
 FROM sources s LEFT JOIN remote_source_health rh ON rh.instance_id=s.instance_id AND rh.source_id=s.id WHERE s.instance_id=$1 AND s.id=ANY($2::uuid[]) ORDER BY s.id`, instance, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SourceHealth{}
	for rows.Next() {
		var it SourceHealth
		if err = rows.Scan(&it.SourceID, &it.Kind, &it.LastSuccessAt, &it.LastErrorAt, &it.LastStatus, &it.LastError, &it.P95LatencyMS, &it.Requests, &it.Failures); err != nil {
			return nil, err
		}
		if it.Kind != "remote" {
			it.Status = "not_applicable"
			it.Requests, it.Failures = 0, 0
			items = append(items, it)
			continue
		}
		if snap, ok := h.d.Health.Snapshot(it.SourceID); ok {
			// The live ring is authoritative for recency and latency; counts are
			// persisted totals plus activity not yet flushed (approximated by the
			// larger of the two so a restart never shows a decrease).
			merge := func(live *time.Time, stored **time.Time) {
				if live != nil && (*stored == nil || live.After(**stored)) {
					*stored = live
				}
			}
			merge(snap.LastSuccessAt, &it.LastSuccessAt)
			merge(snap.LastErrorAt, &it.LastErrorAt)
			if snap.LastStatus > 0 {
				status := snap.LastStatus
				it.LastStatus = &status
			}
			if snap.LastError != "" && snap.LastErrorAt != nil {
				it.LastError = snap.LastError
			}
			p95 := snap.P95MS
			it.P95LatencyMS = &p95
			it.Requests = max(it.Requests, snap.Requests)
			it.Failures = max(it.Failures, snap.Failures)
		}
		it.Status = healthStatus(it)
		items = append(items, it)
	}
	return items, rows.Err()
}

func (h opsHandlers) listSourceHealth(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	pg, ok := h.page(w, r, p, "source-health")
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	ids, more, err := h.permittedIDs(ctx, p, pg, `SELECT id::text FROM sources WHERE instance_id=$1 AND kind='remote' AND ($2='' OR id>$2::uuid) ORDER BY id`, func(id string) bool { return sourceAllowed(policy, p, id, identity.Read) })
	var items []SourceHealth
	if err == nil {
		items, err = h.loadHealth(ctx, p.InstanceID, ids)
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "source health unavailable")
		return
	}
	info, err := h.pageInfo(pg, ids, more)
	if err != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "cannot encode page cursor")
		return
	}
	JSON(w, 200, SourceHealthPage{Items: items, Page: info})
}

func (h opsHandlers) getSourceHealth(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source ID")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	if !sourceAllowed(policy, p, id, identity.Read) {
		Error(w, r, 404, api.ErrorCodeNotFound, "source not found")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	items, err := h.loadHealth(ctx, p.InstanceID, []string{id})
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "source health unavailable")
		return
	}
	if len(items) == 0 {
		Error(w, r, 404, api.ErrorCodeNotFound, "source not found")
		return
	}
	JSON(w, 200, items[0])
}

// sourceAction serves POST /api/v1/sources/{id}:test. ServeMux wildcards must
// span whole segments, so the verb suffix is parsed here.
func (h opsHandlers) sourceAction(w http.ResponseWriter, r *http.Request) {
	raw, verb, found := strings.Cut(r.PathValue("target"), ":")
	id, valid := feedID(raw)
	if !found || verb != "test" || !valid {
		Error(w, r, 404, api.ErrorCodeNotFound, "management route not found")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	if !sourceAllowed(policy, p, id, identity.Configure) {
		Error(w, r, 404, api.ErrorCodeNotFound, "source not found")
		return
	}
	if h.d.Prober == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "remote source runtime unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var kind, format string
	var configured bool
	err := h.db.QueryRow(ctx, `SELECT kind,EXISTS(SELECT 1 FROM remote_source_configs c WHERE c.instance_id=s.instance_id AND c.source_id=s.id),COALESCE((SELECT f.format FROM feed_sources fs JOIN feeds f ON f.instance_id=fs.instance_id AND f.id=fs.feed_id WHERE fs.instance_id=s.instance_id AND fs.source_id=s.id ORDER BY f.slug LIMIT 1),'') FROM sources s WHERE s.instance_id=$1 AND s.id=$2`, p.InstanceID, id).Scan(&kind, &configured, &format)
	if errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, 404, api.ErrorCodeNotFound, "source not found")
		return
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "source unavailable")
		return
	}
	if kind != "remote" {
		Error(w, r, 409, api.ErrorCodeConflict, "only remote sources have an upstream to test")
		return
	}
	if !configured {
		Error(w, r, 409, api.ErrorCodeConflict, "remote source is not configured")
		return
	}
	result, err := h.d.Prober(ctx, id, format)
	if err != nil {
		switch {
		case errors.Is(err, remoteruntime.ErrInput), errors.Is(err, remoteruntime.ErrPolicy):
			Error(w, r, 409, api.ErrorCodeConflict, "remote source configuration rejected by policy")
		default:
			Error(w, r, 503, api.ErrorCodeUnavailable, "connection test unavailable")
		}
		return
	}
	_, _ = h.db.Exec(r.Context(), `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,'source.test',$3,jsonb_build_object('reachable',$4::boolean,'status_code',$5::int))`, p.InstanceID, p.PrincipalID, id, result.Reachable, result.StatusCode)
	out := SourceTestResult{SourceID: id, Reachable: result.Reachable, Authenticated: result.Authenticated, LatencyMS: result.LatencyMS, Error: result.Error}
	if result.StatusCode > 0 {
		out.StatusCode = &result.StatusCode
	}
	JSON(w, 200, out)
}
