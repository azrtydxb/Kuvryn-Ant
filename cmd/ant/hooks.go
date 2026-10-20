package main

import (
	"context"
	"errors"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/hooks"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/replication"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newHooks builds the incoming webhook service for the serving instance, wiring
// its actions to the runtimes that already exist: the prewarm scheduler, the
// replication service, the artifact service (cache clean) and the remote
// runtime (metadata cache). It returns nil when this process serves no instance.
func newHooks(pool *pgxpool.Pool, r *artifactRuntime, ops *operations, rr *replicationRuntime, cfg config.Config, publicURL string) (*hooks.Service, error) {
	if r == nil || r.instanceID == "" {
		return nil, nil
	}
	be := hooks.Backend{}
	if rr != nil && rr.service != nil {
		be.TriggerReplication = func(ctx context.Context, instance, actor, job string) error {
			err := rr.service.Trigger(ctx, instance, actor, job)
			switch {
			case err == nil:
				return nil
			case errors.Is(err, replication.ErrNotFound):
				return hooks.ErrTargetNotFound
			}
			return err
		}
	}
	if r.service != nil {
		be.CleanCache = func(ctx context.Context, p identity.Principal, feed, prefix string) (hooks.CacheCleaned, error) {
			res, err := r.service.CleanCache(ctx, p, artifact.CacheClean{FeedID: feed, Prefix: prefix})
			var transferring *artifact.FeedTransferringError
			switch {
			case err == nil:
				return hooks.CacheCleaned{Matched: res.Matched, Tags: res.Tags, Versions: res.Versions, Components: res.Components, Bytes: res.Bytes, Truncated: res.Truncated}, nil
			case errors.Is(err, artifact.ErrNotFound), errors.Is(err, pgx.ErrNoRows), errors.Is(err, artifact.ErrNotCache):
				return hooks.CacheCleaned{}, hooks.ErrTargetNotFound
			case errors.Is(err, artifact.ErrDenied):
				return hooks.CacheCleaned{}, hooks.ErrTargetDenied
			case errors.As(err, &transferring), errors.Is(err, artifact.ErrConflict):
				return hooks.CacheCleaned{}, hooks.ErrTargetBusy
			}
			return hooks.CacheCleaned{}, err
		}
	}
	if r.remote != nil {
		be.InvalidateMetadata = r.remote.InvalidateMetadata
	}
	if ops != nil && ops.prewarm != nil {
		be.NotifyPrewarm = ops.prewarm.Notify
	}
	return hooks.New(hooks.Config{
		DB: pool, InstanceID: r.instanceID, Secrets: webhookSecrets{secrets.Store{DB: pool, Keys: cfg.SecretKeys}}, Backend: be,
		PublicURL: publicURL, MaxBody: cfg.HookMaxBodyBytes, LogRetentionDays: cfg.HookLogRetentionDays, AllowHTTP: cfg.WebhookAllowHTTP,
	})
}

// StartHooks runs the incoming hook worker under the writer lease context.
func (w *backgroundWorkers) StartHooks(svc *hooks.Service) {
	if w == nil || svc == nil {
		return
	}
	w.done.Add(1)
	go func() { defer w.done.Done(); svc.Run(w.workCtx) }()
}
