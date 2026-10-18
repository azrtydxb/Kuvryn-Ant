package ownership

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager renews one process's ownership and never silently reacquires it.
// Done signals immediately when it stops being a writer. Close additionally waits
// for bounded release cleanup. Every metadata transaction still needs Require.
type Manager struct {
	mu              sync.RWMutex
	lease           Lease
	err, cleanupErr error
	active, closing bool
	cancel          context.CancelFunc
	done, finished  chan struct{}
}

// Start acquires ownership before returning and renews at one third of its TTL.
// The caller must stop serving writes when Done closes. Lease also fails closed
// by returning a zero lease after termination, including during release cleanup.
func Start(ctx context.Context, pool *pgxpool.Pool, instanceID, ownerUUID string, ttl time.Duration) (*Manager, error) {
	lease, err := Acquire(ctx, pool, instanceID, ownerUUID, ttl)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	m := &Manager{lease: lease, active: true, cancel: cancel, done: make(chan struct{}), finished: make(chan struct{})}
	go m.run(runCtx, pool, ttl)
	return m, nil
}
func (m *Manager) run(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration) {
	defer m.cancel()
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	var terminal error
	for {
		select {
		case <-ctx.Done():
			terminal = ctx.Err()
		case <-ticker.C:
			m.mu.RLock()
			lease := m.lease
			m.mu.RUnlock()
			// A slow renewal must fail closed before consuming the remaining TTL.
			renewCtx, cancel := context.WithTimeout(ctx, ttl/3)
			renewed, err := Renew(renewCtx, pool, lease, ttl)
			cancel()
			if err == nil {
				m.mu.Lock()
				m.lease = renewed
				m.mu.Unlock()
				continue
			}
			terminal = err
			if ctx.Err() != nil {
				terminal = ctx.Err()
			}
		}
		break
	}
	m.mu.Lock()
	if m.closing && errors.Is(terminal, context.Canceled) {
		terminal = nil
	}
	m.err = terminal
	m.active = false
	lease := m.lease
	close(m.done)
	m.mu.Unlock()
	// Release uses its own bounded context: request/process cancellation must not
	// prevent cleanup. A superseding owner is protected by owner+epoch checks.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cleanupErr := Release(cleanupCtx, pool, lease)
	cancel()
	if errors.Is(cleanupErr, ErrLost) {
		cleanupErr = nil
	}
	m.mu.Lock()
	m.cleanupErr = cleanupErr
	m.mu.Unlock()
	close(m.finished)
}

func (m *Manager) Lease() Lease {
	if m == nil {
		return Lease{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.active || m.closing {
		return Lease{}
	}
	return m.lease
}
func (m *Manager) Err() error {
	if m == nil {
		return ErrInvalid
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.err
}
func (m *Manager) Done() <-chan struct{} { return m.done }

// Close is safe to call repeatedly or concurrently and releases only this epoch.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	m.cancel()
	<-m.finished
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cleanupErr
}
