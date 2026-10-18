package management

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ComponentStatus is the health of one dependency. Status is ok, degraded,
// unavailable or unknown. Detail is a fixed, non-sensitive code.
type ComponentStatus struct {
	Status    string `json:"status"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// WriterLeaseStatus describes this process's instance writer lease. State is
// held, lost, expiring (under 25% of lifetime left is not tracked; reported
// only as held/lost) or not_serving (artifact serving inactive).
type WriterLeaseStatus struct {
	State       string     `json:"state"`
	OwnerID     string     `json:"owner_id,omitempty"`
	Epoch       int64      `json:"epoch,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RemainingMS *int64     `json:"remaining_ms,omitempty"`
}

// ReadinessDetail is shown only to instance administrators.
type ReadinessDetail struct {
	Ready       bool              `json:"ready"`
	Database    ComponentStatus   `json:"database"`
	BlobStorage ComponentStatus   `json:"blob_storage"`
	WriterLease WriterLeaseStatus `json:"writer_lease"`
	CheckedAt   time.Time         `json:"checked_at"`
}

// EnvSetting is a deployment-provided, read-only instance setting.
type EnvSetting struct {
	Key, Value, Description string
}

// OpsDeps carries the runtime collaborators of the operations API. Every field
// is optional; endpoints that need an absent collaborator answer 503.
type OpsDeps struct {
	Pool  *pgxpool.Pool
	Codec CursorCodec
	Keys  *secrets.Keyring
	// Background outlives requests; asynchronous retention runs use it.
	Background context.Context
	Health     *remoteruntime.Health
	// Prober performs a connection test of a remote source (remoteruntime.Manager.Probe).
	Prober func(ctx context.Context, source string) (remoteruntime.ProbeResult, error)
	// Readiness reports dependency state beyond database reachability.
	Readiness func(ctx context.Context) ReadinessDetail
	// StagingBytes reports local upload staging usage, when the backend has one.
	StagingBytes   func(ctx context.Context) (int64, bool)
	StorageBackend string
	// Env lists read-only deployment settings returned by the settings endpoint.
	Env []EnvSetting
	// Retention returns the maintenance service for the serving instance, or nil.
	Retention func(instance string) *retention.Service
	// StorageMigrator returns the storage migration service for the serving instance, or nil.
	StorageMigrator func(instance string) *storage.Migrator
	// Diagnostics writes the redacted support bundle (tar.gz) to w.
	Diagnostics func(ctx context.Context, w io.Writer) error
}

type opsHandlers struct {
	identityHandlers
	d OpsDeps
}

func (h opsHandlers) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 10*time.Second)
}

// permitted resolves the caller and answers 404 for unauthorized access so
// existence of feeds and sources is not disclosed.
func (h opsHandlers) feedRead(w http.ResponseWriter, r *http.Request, action identity.Action) (identity.Principal, string, bool) {
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return identity.Principal{}, "", false
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return p, "", false
	}
	if !feedAllowed(policy, p, id, action) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return p, "", false
	}
	return p, id, true
}

// RegisterOperations mounts the operational-visibility and lifecycle API. All
// routes require a bearer token; authorization is documented per route in
// api/openapi.yaml.
func RegisterOperations(mux *http.ServeMux, d OpsDeps) {
	h := opsHandlers{identityHandlers{feedHandlers{db: d.Pool, codec: d.Codec}}, d}
	bearer := identity.NewBearer(identity.Tokens{DB: d.Pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/source-health":                     h.listSourceHealth,
		"GET /api/v1/sources/{id}/health":               h.getSourceHealth,
		"POST /api/v1/sources/{target}":                 h.sourceAction,
		"GET /api/v1/stats/feeds":                       h.feedStats,
		"GET /api/v1/stats/sources":                     h.sourceStats,
		"GET /api/v1/storage/usage":                     h.storageUsage,
		"GET /api/v1/storage/migration":                 h.storageMigrationStatus,
		"GET /api/v1/storage/migrations":                h.listStorageJobs,
		"POST /api/v1/storage/migrations":               h.startStorageJob,
		"GET /api/v1/storage/migrations/{id}":           h.getStorageJob,
		"POST /api/v1/storage/migrations/{id}/{action}": h.storageJobAction,
		"GET /api/v1/metrics/traffic":                   h.traffic,
		"GET /api/v1/assets/{id}/provenance":            h.provenance,
		"GET /api/v1/webhooks/stats":                    h.webhookStats,
		"POST /api/v1/webhooks/{id}/deliveries/{event}": h.redeliver,
		"GET /api/v1/settings":                          h.getSettings,
		"PUT /api/v1/settings":                          h.putSettings,
		"GET /api/v1/permissions/effective":             h.effective,
		"GET /api/v1/retention/policies":                h.listRetentionPolicies,
		"POST /api/v1/retention/policies":               h.createRetentionPolicy,
		"PUT /api/v1/retention/policies/{id}":           h.putRetentionPolicy,
		"DELETE /api/v1/retention/policies/{id}":        h.deleteRetentionPolicy,
		"GET /api/v1/retention/runs":                    h.listRetentionRuns,
		"POST /api/v1/retention/runs":                   h.startRetentionRun,
		"GET /api/v1/retention/runs/{id}":               h.getRetentionRun,
		"GET /api/v1/retention/integrity-findings":      h.integrityFindings,
		"GET /api/v1/diagnostics":                       h.getDiagnostics,
		"GET /api/v1/audit-sink":                        h.getAuditSink,
		"PUT /api/v1/audit-sink":                        h.putAuditSink,
		"DELETE /api/v1/audit-sink":                     h.deleteAuditSink,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

// statusDetail serves /api/v1/status with readiness detail to administrators.
var statusDetail atomic.Pointer[http.Handler]

// EnableStatusDetail makes GET /api/v1/status include dependency readiness
// when the request carries a valid bearer token with instance Admin. The
// anonymous response is unchanged. Call before serving starts.
func EnableStatusDetail(pool *pgxpool.Pool, readiness func(context.Context) ReadinessDetail) {
	h := opsHandlers{identityHandlers{feedHandlers{db: pool}}, OpsDeps{Pool: pool, Readiness: readiness}}
	var handler http.Handler = identity.NewBearer(identity.Tokens{DB: pool}).Wrap(http.HandlerFunc(h.statusWithDetail))
	statusDetail.Store(&handler)
}

func (h opsHandlers) statusWithDetail(w http.ResponseWriter, r *http.Request) {
	type status struct {
		Name       string           `json:"name"`
		ApiVersion string           `json:"api_version"`
		Readiness  *ReadinessDetail `json:"readiness,omitempty"`
	}
	out := status{Name: "kuvryn-ant", ApiVersion: "v1"}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	if policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) && h.d.Readiness != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		detail := h.d.Readiness(ctx)
		out.Readiness = &detail
	}
	JSON(w, http.StatusOK, out)
}
