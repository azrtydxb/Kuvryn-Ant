package main

import (
	"log/slog"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/maintenance"
	"github.com/jackc/pgx/v5/pgxpool"
)

// startAuditPurge runs the audit retention purge under the instance writer
// lease (every deleting transaction is fenced by it). It is a no-op without a
// serving runtime or when retention is 0 (keep forever). Events a configured
// audit sink has not exported yet are never deleted.
func (w *backgroundWorkers) startAuditPurge(pool *pgxpool.Pool, r *artifactRuntime, c config.AuditRetention, logger *slog.Logger) {
	if w == nil || r == nil || c.Retain <= 0 {
		return
	}
	purge := maintenance.AuditPurge{DB: pool, InstanceID: r.instanceID, Retain: c.Retain, BatchSize: c.BatchSize, Lease: r.manager.Lease}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-w.workCtx.Done():
				return
			case <-timer.C:
			}
			report, err := purge.Run(w.workCtx)
			switch {
			case err != nil:
				logger.Warn("audit purge incomplete", "reason", err, "deleted", report.Deleted)
			case report.SinkLag:
				logger.Warn("audit purge holding back events the audit sink has not exported", "held", report.HeldBySink, "deleted", report.Deleted)
			case report.Deleted > 0:
				logger.Info("audit purge", "deleted", report.Deleted, "cutoff", report.Cutoff)
			}
			timer.Reset(c.Interval)
		}
	}()
}
