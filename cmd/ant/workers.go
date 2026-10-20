package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/replication"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

type webhookSecrets struct{ store secrets.Store }

func (s webhookSecrets) Resolve(ctx context.Context, instance, record string) ([]byte, error) {
	_, plaintext, err := s.store.Open(ctx, instance, record, "webhook")
	return plaintext, err
}

type backgroundWorkers struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       sync.WaitGroup
	dispatcher *events.Dispatcher
	workCtx    context.Context
}

func startWorkers(ctx context.Context, pool *pgxpool.Pool, r *artifactRuntime, keys *secrets.Keyring, browser ...*browserRuntime) (*backgroundWorkers, error) {
	if r == nil {
		return nil, nil
	}
	lease := r.manager.Lease()
	if r.instanceID == "" || lease.InstanceID != r.instanceID {
		return nil, errors.New("instance ownership unavailable")
	}
	dispatcher, err := events.New(pool, nil, webhookSecrets{secrets.Store{DB: pool, Keys: keys}}, events.Options{InstanceID: r.instanceID})
	if err != nil {
		return nil, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	workers := &backgroundWorkers{ctx: workCtx, workCtx: workCtx, cancel: cancel, dispatcher: dispatcher}
	if len(browser) > 0 && browser[0] != nil {
		workers.done.Add(1)
		go func() {
			defer workers.done.Done()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				cleanupCtx, stop := context.WithTimeout(workCtx, 10*time.Second)
				_ = browser[0].Cleanup(cleanupCtx)
				stop()
				select {
				case <-workCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	workers.done.Add(3)
	go func() { defer workers.done.Done(); _ = dispatcher.Run(workCtx) }()
	go func() { defer workers.done.Done(); r.registry.Run(workCtx) }()
	go func() {
		defer workers.done.Done()
		select {
		case <-r.manager.Done():
			cancel()
		case <-workCtx.Done():
		}
	}()
	return workers, nil
}

// StartReplication runs the replication scheduler under the same context as the
// other workers, which is cancelled when the instance writer lease is lost.
func (w *backgroundWorkers) StartReplication(rr *replicationRuntime, logger *slog.Logger) {
	if w == nil || rr == nil {
		return
	}
	w.done.Add(3)
	go func() { defer w.done.Done(); rr.service.Run(w.ctx) }()
	// Consumer pull activity (outgoing replication visibility) is batched in
	// memory and persisted here, like the traffic counters.
	go func() { defer w.done.Done(); rr.exporter.Consumers.Run(w.ctx, rr.pool, 15*time.Second) }()
	// The authority side trims its change history to the configured bounds and
	// raises the low-water mark; lagging consumers then reseed automatically.
	go func() {
		defer w.done.Done()
		rr.exporter.RunPurge(w.ctx, rr.instance, replication.PurgePolicy{MaxAge: rr.history.MaxAge, MaxRows: rr.history.MaxRows}, rr.history.Interval, logger)
	}()
}
func (w *backgroundWorkers) Close() {
	if w != nil {
		w.cancel()
		w.done.Wait()
		w.dispatcher.Close()
	}
}
