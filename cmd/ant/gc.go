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
	"github.com/jackc/pgx/v5/pgxpool"
)

func retentionService(pool *pgxpool.Pool, instance string, resolve retention.Resolver, lease func() ownership.Lease, c config.Retention) *retention.Service {
	return &retention.Service{DB: pool, InstanceID: instance, Stores: resolve, Lease: lease, Options: retention.Options{
		Grace: c.Grace, BatchSize: c.BatchSize, IntegrityBatch: c.IntegrityBatch, IntegrityMaxBytes: c.IntegrityMaxBytes,
	}}
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
	report, runErr := retentionService(pool, owner.InstanceID, stores.Resolve, nil, cfg.Retention).RunOnce(ctx, dryRun)
	if err = json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	return runErr
}

// startRetention runs the periodic worker under the instance lease: every
// mutating step verifies the writer lease, so a replaced owner stops deleting.
// It is a no-op without serving runtime or when the interval is zero.
func (w *backgroundWorkers) startRetention(pool *pgxpool.Pool, r *artifactRuntime, c config.Retention, logger *slog.Logger) {
	if w == nil || r == nil || c.Interval <= 0 {
		return
	}
	svc := retentionService(pool, r.instanceID, r.stores.Resolve, r.manager.Lease, c)
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
