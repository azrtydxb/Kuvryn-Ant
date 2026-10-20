package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/auditsink"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diagnostics"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/limits"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
	"github.com/azrtydxb/Kuvryn-Ant/internal/prewarm"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	trafficFlushInterval = 30 * time.Second
	healthFlushInterval  = 30 * time.Second
	// limitsRefreshInterval bounds how long a limit change made through another
	// instance takes to apply here.
	limitsRefreshInterval = 10 * time.Second
)

// operations binds the operational-visibility runtime (traffic counters,
// upstream health, readiness probes) to the serving instance.
type operations struct {
	pool      *pgxpool.Pool
	artifacts *artifactRuntime
	cfg       config.Config
	traffic   *observability.Traffic
	// limiter applies rate limits and carries quota rejections; nil when not serving.
	limiter *limits.Limiter
	staging stagingCache
	// background is cancelled on shutdown and writer-lease loss.
	background context.Context
	// prewarm warms remote caches through the feed routes; nil without pull-through.
	prewarm *prewarm.Service
}

func newOperations(pool *pgxpool.Pool, r *artifactRuntime, cfg config.Config) *operations {
	ops := &operations{pool: pool, artifacts: r, cfg: cfg, traffic: observability.NewTraffic(), background: context.Background()}
	if r != nil {
		ops.limiter = &limits.Limiter{DB: pool, InstanceID: r.instanceID, Resolve: limits.TokenResolver(identity.Tokens{DB: pool}), APIError: management.Error, Logger: slog.Default()}
		r.service.Downloads = ops.traffic
		if r.remote != nil {
			r.remote.SetCacheObserver(ops.traffic.Cache)
			// Prewarm requests go to the adapters directly (no listener middleware),
			// as the principal that owns the run, so every adapter applies its own
			// authorization, ownership and integrity rules.
			feeds := http.NewServeMux()
			r.Register(feeds)
			ops.prewarm = &prewarm.Service{DB: pool, InstanceID: r.instanceID, Warmer: &prewarm.Warmer{Feeds: feeds}, Logger: slog.Default()}
		}
	}
	return ops
}

// start launches the batched flushers under the worker context, which ends when
// the instance writer lease is lost, so only the lease holder persists counters.
func (o *operations) start(w *backgroundWorkers) {
	if o == nil || w == nil || o.artifacts == nil {
		return
	}
	o.background = w.workCtx
	instance := o.artifacts.instanceID
	run := func(f func()) {
		w.done.Add(1)
		go func() { defer w.done.Done(); f() }()
	}
	run(func() { o.traffic.Run(w.workCtx, o.pool, instance, trafficFlushInterval) })
	if o.limiter != nil {
		// Apply stored limits before serving; then follow changes made on other instances.
		ictx, cancel := context.WithTimeout(w.workCtx, 5*time.Second)
		_ = o.limiter.Refresh(ictx)
		cancel()
		run(func() { o.limiter.Run(w.workCtx, limitsRefreshInterval) })
	}
	if o.artifacts.remote != nil {
		run(func() { o.artifacts.remote.Health().Run(w.workCtx, o.pool, instance, healthFlushInterval) })
	}
	if o.prewarm != nil {
		run(func() { o.prewarm.Run(w.workCtx) })
	}
	if o.cfg.SecretKeys != nil {
		f := &auditsink.Forwarder{DB: o.pool, InstanceID: instance, Keys: o.cfg.SecretKeys, Hostname: hostname()}
		run(func() { f.Run(w.workCtx) })
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "-"
	}
	return h
}

// deps assembles the management operations dependencies.
func (o *operations) deps(codec management.CursorCodec) management.OpsDeps {
	d := management.OpsDeps{
		Limits: o.limiter, Pool: o.pool, Codec: codec, Keys: o.cfg.SecretKeys, Background: o.background,
		StorageBackend: o.cfg.Storage.Backend, Readiness: o.readiness, StagingBytes: o.stagingBytes,
		GC: management.GCSettingsView{IntervalSeconds: int64(o.cfg.Retention.Interval / time.Second), GraceSeconds: int64(o.cfg.Retention.Grace / time.Second), DryRun: o.cfg.Retention.DryRun,
			PressureIntervalSeconds: int64(o.cfg.Retention.PressureInterval / time.Second), PressureSweepGraceSeconds: int64(o.cfg.Retention.PressureSweepGrace / time.Second)},
		Env: []management.EnvSetting{
			{Key: "storage_backend", Value: backendName(o.cfg.Storage.Backend), Description: "Blob storage backend (ANT_STORAGE_BACKEND)."},
			{Key: "max_requests", Value: strconv.Itoa(o.cfg.MaxRequests), Description: "Concurrent request limit (ANT_MAX_REQUESTS)."},
			{Key: "shutdown_timeout", Value: o.cfg.ShutdownTimeout.String(), Description: "Graceful shutdown timeout (ANT_SHUTDOWN_TIMEOUT)."},
			{Key: "gc_interval", Value: o.cfg.Retention.Interval.String(), Description: "Scheduled maintenance interval, 0s disables (ANT_GC_INTERVAL)."},
			{Key: "gc_grace", Value: o.cfg.Retention.Grace.String(), Description: "Orphan blob grace period (ANT_GC_GRACE)."},
			{Key: "gc_dry_run", Value: strconv.FormatBool(o.cfg.Retention.DryRun), Description: "Scheduled maintenance runs report only (ANT_GC_DRY_RUN)."},
			{Key: "pressure_interval", Value: o.cfg.Retention.PressureInterval.String(), Description: "Storage-pressure check interval, 0s disables the worker (ANT_PRESSURE_INTERVAL)."},
			{Key: "pressure_sweep_grace", Value: o.cfg.Retention.PressureSweepGrace.String(), Description: "Wait before a blob freed by pressure eviction is swept (ANT_PRESSURE_SWEEP_GRACE)."},
			{Key: "replication_history_max_age", Value: o.cfg.Replication.MaxAge.String(), Description: "Maximum age of replication change history before it is purged (ANT_REPLICATION_HISTORY_MAX_AGE)."},
			{Key: "replication_history_max_rows", Value: strconv.FormatInt(o.cfg.Replication.MaxRows, 10), Description: "Maximum replication change-history rows kept (ANT_REPLICATION_HISTORY_MAX_ROWS)."},
			{Key: "replication_purge_interval", Value: o.cfg.Replication.Interval.String(), Description: "Replication history purge interval, 0s disables (ANT_REPLICATION_PURGE_INTERVAL)."},
		},
	}
	d.WebhookAllowHTTP = o.cfg.WebhookAllowHTTP
	if o.cfg.WebhookBaseURL != "" {
		d.Env = append(d.Env, management.EnvSetting{Key: "webhook_base_url", Value: o.cfg.WebhookBaseURL, Description: "Base URL webhook names are appended to (ANT_WEBHOOK_BASE_URL)."})
	}
	if o.cfg.PublicURL != "" {
		d.Env = append([]management.EnvSetting{{Key: "public_url", Value: o.cfg.PublicURL, Description: "Public origin of this instance (ANT_PUBLIC_URL)."}}, d.Env...)
	}
	var resolve storeResolver
	if o.artifacts != nil {
		resolve = o.artifacts.stores.Resolve
	}
	d.Diagnostics = func(ctx context.Context, w io.Writer) error {
		health := func(ctx context.Context) any { return o.readiness(ctx) }
		return diagnostics.Build(ctx, w, diagnosticsInput(o.cfg, o.pool, resolve, health, "api"))
	}
	if r := o.artifacts; r != nil {
		if r.remote != nil {
			d.Health = r.remote.Health()
			d.Prober = r.remote.Probe
		}
		if o.prewarm != nil {
			d.Prewarm = o.prewarm
		}
		d.StorageMigrator = func(instance string) *storage.Migrator {
			if instance != r.instanceID {
				return nil
			}
			return r.migrator
		}
		d.Artifacts = func(instance string) *artifact.Service {
			if instance != r.instanceID {
				return nil
			}
			return r.service
		}
		d.Retention = func(instance string) *retention.Service {
			if instance != r.instanceID {
				return nil
			}
			return retentionService(o.pool, r.instanceID, r.stores.Resolve, r.manager.Lease, o.cfg, r.migrator)
		}
	}
	return d
}

func backendName(b string) string {
	if b == "" {
		return "disk"
	}
	return b
}

func ms(d time.Duration) *int64 { v := d.Milliseconds(); return &v }

// readiness reports database, blob storage and writer lease state.
func (o *operations) readiness(ctx context.Context) management.ReadinessDetail {
	out := management.ReadinessDetail{CheckedAt: time.Now().UTC()}
	start := time.Now()
	if err := o.pool.Ping(ctx); err != nil {
		out.Database = management.ComponentStatus{Status: "unavailable", LatencyMS: ms(time.Since(start)), Detail: "ping_failed"}
	} else {
		out.Database = management.ComponentStatus{Status: "ok", LatencyMS: ms(time.Since(start))}
	}
	r := o.artifacts
	if r == nil {
		out.BlobStorage = management.ComponentStatus{Status: "unknown", Detail: "not_serving"}
		out.WriterLease = management.WriterLeaseStatus{State: "not_serving"}
		out.Ready = out.Database.Status == "ok"
		return out
	}
	out.BlobStorage = o.probeBlob(ctx)
	out.WriterLease = o.leaseState(ctx)
	out.Ready = out.Database.Status == "ok" && out.BlobStorage.Status == "ok" && out.WriterLease.State == "held"
	if w, ok := retention.PressureWarning(); ok {
		out.Warnings = append(out.Warnings, w)
	}
	return out
}

func (o *operations) probeBlob(ctx context.Context) management.ComponentStatus {
	start := time.Now()
	domain := "00000000-0000-0000-0000-000000000000"
	var existing string
	if err := o.pool.QueryRow(ctx, `SELECT id::text FROM dedup_domains WHERE instance_id=$1 ORDER BY id LIMIT 1`, o.artifacts.instanceID).Scan(&existing); err == nil {
		domain = existing
	}
	store, err := o.artifacts.stores.Resolve(ctx, domain)
	if err != nil {
		return management.ComponentStatus{Status: "unavailable", Detail: "store_unavailable"}
	}
	// A missing object proves the backend answered; any other error is a fault.
	_, err = store.Stat(ctx, blob.Digest("0000000000000000000000000000000000000000000000000000000000000000"))
	if err != nil && !errors.Is(err, blob.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
		return management.ComponentStatus{Status: "unavailable", LatencyMS: ms(time.Since(start)), Detail: "probe_failed"}
	}
	return management.ComponentStatus{Status: "ok", LatencyMS: ms(time.Since(start))}
}

func (o *operations) leaseState(ctx context.Context) management.WriterLeaseStatus {
	local := o.artifacts.manager.Lease()
	out := management.WriterLeaseStatus{State: "lost", OwnerID: local.OwnerID, Epoch: local.Epoch}
	if o.artifacts.manager.Err() != nil {
		return out
	}
	var owner string
	var epoch int64
	var expires time.Time
	var alive bool
	err := o.pool.QueryRow(ctx, `SELECT owner_id::text,epoch,expires_at,expires_at>clock_timestamp() FROM instance_writer_leases WHERE instance_id=$1`, o.artifacts.instanceID).Scan(&owner, &epoch, &expires, &alive)
	if err != nil {
		out.State = "unavailable"
		return out
	}
	out.ExpiresAt = &expires
	if remaining := time.Until(expires).Milliseconds(); alive {
		out.RemainingMS = &remaining
	}
	switch {
	case owner != local.OwnerID || epoch != local.Epoch:
		out.State = "superseded"
	case !alive:
		out.State = "expired"
	default:
		out.State = "held"
	}
	return out
}

type stagingCache struct {
	mu    sync.Mutex
	at    time.Time
	bytes int64
}

// stagingBytes sums upload staging on the local disk (OCI uploads plus disk
// blob staging directories). Results are cached for 30 seconds and the walk is
// bounded, so the endpoint cannot be used to force repeated large scans.
func (o *operations) stagingBytes(ctx context.Context) (int64, bool) {
	dir := o.cfg.Storage.Directory
	if dir == "" {
		return 0, false
	}
	c := &o.staging
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < 30*time.Second {
		return c.bytes, true
	}
	var total int64
	entries := 0
	walk := func(root string) {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil || entries > 200000 {
				return fs.SkipAll
			}
			entries++
			if d.Type().IsRegular() {
				if info, e := d.Info(); e == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	walk(filepath.Join(dir, "uploads"))
	if o.cfg.Storage.Backend == "" || o.cfg.Storage.Backend == "disk" {
		domains, _ := filepath.Glob(filepath.Join(dir, "blobs", "*", "staging"))
		for _, d := range domains {
			walk(d)
		}
	}
	c.at, c.bytes = time.Now(), total
	return total, true
}
