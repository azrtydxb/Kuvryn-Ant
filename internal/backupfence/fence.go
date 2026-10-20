// Package backupfence lets a running backup stop blob byte deletion.
//
// The backup and the sweeps may run in different processes, so the fence lives
// in PostgreSQL: a backup_fences row whose expires_at a heartbeat keeps in the
// future. A sweep takes a shared transaction advisory lock and then checks for an
// unexpired row; Acquire takes the same lock exclusively while inserting. A sweep
// that is already deleting therefore finishes (and commits) before the fence is
// visible, and every later sweep sees the fence. A crashed backup stops
// heartbeating and its fence expires after TTL.
package backupfence

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// TTL is how long a fence outlives its last heartbeat.
	TTL = 2 * time.Minute
	// Interval is the heartbeat period.
	Interval = TTL / 8

	lockKey = 0x616e74666e6365 // "antfnce"
)

// ErrLost means the fence could no longer be kept alive.
var ErrLost = errors.New("backup fence lost: garbage collection may have resumed")

// LockShared takes the sweep side of the fence lock for the transaction. Call it
// before Live and keep the transaction open across the byte deletion.
func LockShared(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1)", int64(lockKey))
	return err
}

// Live reports whether any unexpired fence exists.
func Live(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (bool, error) {
	var live bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM backup_fences WHERE expires_at>clock_timestamp())").Scan(&live)
	return live, err
}

// Fence is a held backup fence.
type Fence struct {
	pool   *pgxpool.Pool
	id     string
	cancel context.CancelCauseFunc
	stop   chan struct{}
	done   sync.WaitGroup
	once   sync.Once
}

// Acquire inserts a fence and starts its heartbeat. The returned context is
// canceled with ErrLost as its cause if the heartbeat cannot keep the fence
// alive; the backup must then be abandoned. Always call Release.
func Acquire(ctx context.Context, pool *pgxpool.Pool, holder string) (context.Context, *Fence, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ctx, nil, fmt.Errorf("backup fence: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Exclusive: waits for in-flight sweeps (which hold the shared lock until they
	// commit) and makes the new row visible to every sweep that starts after.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(lockKey)); err != nil {
		return ctx, nil, fmt.Errorf("backup fence: %w", err)
	}
	if _, err = tx.Exec(ctx, "DELETE FROM backup_fences WHERE expires_at<=clock_timestamp()"); err != nil {
		return ctx, nil, fmt.Errorf("backup fence: %w", err)
	}
	f := &Fence{pool: pool, stop: make(chan struct{})}
	err = tx.QueryRow(ctx, "INSERT INTO backup_fences(holder,expires_at) VALUES($1,clock_timestamp()+$2::interval) RETURNING id::text", holder, fmt.Sprintf("%d microseconds", TTL.Microseconds())).Scan(&f.id)
	if err != nil {
		return ctx, nil, fmt.Errorf("backup fence: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ctx, nil, fmt.Errorf("backup fence: %w", err)
	}
	held, cancel := context.WithCancelCause(ctx)
	f.cancel = cancel
	f.done.Add(1)
	go f.beat(held)
	return held, f, nil
}

func (f *Fence) beat(ctx context.Context) {
	defer f.done.Done()
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	lastOK := time.Now()
	for {
		select {
		case <-f.stop:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		call, cancel := context.WithTimeout(context.WithoutCancel(ctx), Interval)
		tag, err := f.pool.Exec(call, "UPDATE backup_fences SET expires_at=clock_timestamp()+$2::interval WHERE id=$1 AND expires_at>clock_timestamp()", f.id, fmt.Sprintf("%d microseconds", TTL.Microseconds()))
		cancel()
		switch {
		case err == nil && tag.RowsAffected() == 1:
			lastOK = time.Now()
		case err == nil, time.Since(lastOK) > TTL/2:
			// The row is gone or expired, or the database has been unreachable for
			// half the TTL: stop before sweeps could resume under the backup.
			f.cancel(ErrLost)
			return
		}
	}
}

// Release stops the heartbeat and removes the fence; sweeps resume. It is safe
// to call more than once.
func (f *Fence) Release() {
	f.once.Do(func() {
		close(f.stop)
		f.done.Wait()
		f.cancel(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = f.pool.Exec(ctx, "DELETE FROM backup_fences WHERE id=$1", f.id)
	})
}
