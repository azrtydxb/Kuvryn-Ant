package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// retentionOverrides converts the environment-provided settings; they take
// precedence over, and lock, the corresponding stored instance settings.
func retentionOverrides(c config.Retention) retention.Overrides {
	return retention.Overrides{
		DefaultEnabled: c.DefaultEnabled, DefaultKeepLast: c.DefaultKeepLast, DefaultMaxAge: c.DefaultMaxAge, DefaultMaxIdle: c.DefaultMaxIdle,
		PressureEnabled: c.PressureEnabled, HighPercent: c.HighPercent, LowPercent: c.LowPercent, CapacityBytes: c.CapacityBytes,
	}
}

// routedBackend names the active blob backend through the storage route, which
// changes during a disk/S3 migration. Without a migrator the configured backend
// is used.
func routedBackend(m *storage.Migrator, configured string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if m == nil {
			return backendName(configured), nil
		}
		rt, err := m.Route(ctx)
		if err != nil {
			return "", err
		}
		return backendName(rt.Active), nil
	}
}

func retentionService(pool *pgxpool.Pool, instance string, resolve retention.Resolver, lease func() ownership.Lease, cfg config.Config, migrator *storage.Migrator) *retention.Service {
	c := cfg.Retention
	return &retention.Service{DB: pool, InstanceID: instance, Stores: resolve, Lease: lease, Options: retention.Options{
		Grace: c.Grace, BatchSize: c.BatchSize, IntegrityBatch: c.IntegrityBatch, IntegrityMaxBytes: c.IntegrityMaxBytes, PressureSweepGrace: c.PressureSweepGrace,
	}, Env: retentionOverrides(c), Backend: routedBackend(migrator, cfg.Storage.Backend), DataDir: cfg.Storage.Directory}
}

// runGC is a local maintenance operation, authorized like rotate-secrets by
// possession of the deployment's database and storage configuration. It runs one
// retention + garbage-collection + integrity pass and prints a JSON report. With
// dryRun it modifies nothing and reports exactly what a real run would do.
// Deletion safety rests on database row locks and the mark/sweep grace periods,
// so it is safe to run while the service is serving.
func runGC(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, dryRun bool, output io.Writer) error {
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		return err
	}
	if cfg.InstanceID != "" && !strings.EqualFold(cfg.InstanceID, owner.InstanceID) {
		return errors.New("gc instance does not match local recovery identity")
	}
	stores, _, err := openStores(pool, cfg, owner.InstanceID)
	if err != nil {
		return err
	}
	defer stores.Close()
	report, runErr := retentionService(pool, owner.InstanceID, stores.Resolve, nil, cfg, nil).RunOnce(ctx, dryRun)
	if err = json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	return runErr
}

// startRetention runs the periodic worker under the instance lease: every
// mutating step verifies the writer lease, so a replaced owner stops deleting.
// It is a no-op without a serving runtime. The maintenance loop runs every
// ANT_GC_INTERVAL (0 disables it); the cheap storage-pressure check has its own
// ANT_PRESSURE_INTERVAL loop, so pressure is relieved between maintenance runs.
func (w *backgroundWorkers) startRetention(pool *pgxpool.Pool, r *artifactRuntime, cfg config.Config, logger *slog.Logger) {
	c := cfg.Retention
	if w == nil || r == nil {
		return
	}
	svc := retentionService(pool, r.instanceID, r.stores.Resolve, r.manager.Lease, cfg, r.migrator)
	if c.PressureInterval > 0 {
		w.done.Add(1)
		go func() {
			defer w.done.Done()
			svc.RunPressureLoop(w.workCtx, c.PressureInterval, func(rep retention.PressureReport, err error) {
				if logger == nil {
					return
				}
				if err != nil {
					logger.Warn("storage pressure check incomplete", "reason", err)
				}
				if rep.EvictedVersions > 0 || rep.State == retention.StateUnevictable || rep.Swept > 0 {
					logger.Info("storage pressure", "state", rep.State, "usage_ratio", rep.UsageRatio, "evicted_versions", rep.EvictedVersions,
						"evicted_bytes", rep.EvictedBytes, "swept", rep.Swept, "shortfall_bytes", rep.ShortfallBytes)
				}
			})
		}()
	}
	if c.Interval <= 0 {
		return
	}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		// Equivalent to svc.Run, but timed so the run history records real start times.
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-w.workCtx.Done():
				return
			case <-timer.C:
			}
			started := time.Now()
			report, err := svc.RunOnce(w.workCtx, c.DryRun)
			if err != nil {
				logger.Warn("retention run incomplete", "reason", err)
			}
			logger.Info("retention run", "dry_run", report.DryRun,
				"versions_deleted", len(report.Retention.Deleted), "blobs_marked", len(report.GC.Marked), "blobs_swept", len(report.GC.Swept),
				"integrity_checked", report.Integrity.Checked, "integrity_mismatches", len(report.Integrity.Mismatches))
			record, stop := context.WithTimeout(context.WithoutCancel(w.workCtx), 10*time.Second)
			management.RecordRetentionRun(record, pool, r.instanceID, "scheduled", report, err, started)
			stop()
			timer.Reset(c.Interval)
		}
	}()
}
