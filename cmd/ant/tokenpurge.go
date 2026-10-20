package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/maintenance"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tokenPurgeInterval is how often the worker looks for dead tokens. The scan
// is index-backed and a no-op when nothing is old enough, so hourly is cheap.
const tokenPurgeInterval = time.Hour

// startTokenPurge removes tokens that have been expired or revoked for longer
// than the effective token_cleanup_after_days (environment, then stored
// setting, then 30; 0 means never). It runs only under the instance writer
// lease (every deleting transaction is fenced by it), in small batches, and
// records one summary audit event per run that removed anything. The setting
// is re-read each run, so console changes apply without a restart.
func (w *backgroundWorkers) startTokenPurge(pool *pgxpool.Pool, r *artifactRuntime, envDays *int, logger *slog.Logger) {
	if w == nil || r == nil {
		return
	}
	purge := maintenance.TokenPurge{
		DB: pool, InstanceID: r.instanceID, BatchSize: 500, Lease: r.manager.Lease,
		Days: func(ctx context.Context) (int, error) {
			days, _, err := maintenance.EffectiveTokenCleanupDays(ctx, pool, r.instanceID, envDays)
			return days, err
		},
	}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		timer := time.NewTimer(2 * time.Minute)
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
				logger.Warn("token cleanup incomplete", "reason", err, "service_tokens", report.ServiceTokens, "personal_access_tokens", report.PersonalAccessTokens)
			case report.Total() > 0:
				logger.Info("token cleanup", "service_tokens", report.ServiceTokens, "personal_access_tokens", report.PersonalAccessTokens, "cutoff", report.Cutoff)
			}
			timer.Reset(tokenPurgeInterval)
		}
	}()
}
