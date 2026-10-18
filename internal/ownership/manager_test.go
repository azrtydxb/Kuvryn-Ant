package ownership

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitManager(t *testing.T, m *Manager) {
	t.Helper()
	select {
	case <-m.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not stop")
	}
}

func TestManagerRenewsAndCloseHandsOver(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	m, err := Start(ctx, pool, instance, ownerA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	original := m.Lease()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for m.Lease().ExpiresAt.Equal(original.ExpiresAt) {
		select {
		case <-deadline.C:
			t.Fatal("manager did not renew")
		case <-tick.C:
		}
	}
	renewed := m.Lease()
	if renewed.Epoch != original.Epoch || !renewed.ExpiresAt.After(original.ExpiresAt) {
		t.Fatal("renewal did not preserve ownership")
	}
	select {
	case <-m.Done():
		t.Fatal("manager stopped while healthy")
	default:
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	waitManager(t, m)
	if m.Err() != nil || m.Lease().Epoch != 0 {
		t.Fatal("normal close did not fail closed cleanly", m.Err())
	}
	successor, err := Acquire(ctx, pool, instance, ownerB, time.Second)
	if err != nil || successor.Epoch <= original.Epoch {
		t.Fatal("close did not release for successor", err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if err = require(t, pool, successor); err != nil {
		t.Fatal("repeated close disturbed successor", err)
	}
}

func TestManagerParentCancellationReleases(t *testing.T) {
	pool, instance := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	m, err := Start(ctx, pool, instance, ownerA, time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	waitManager(t, m)
	if !errors.Is(m.Err(), context.Canceled) || m.Lease().Epoch != 0 {
		t.Fatal("cancellation did not stop writer", m.Err())
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Acquire(context.Background(), pool, instance, ownerB, time.Second); err != nil {
		t.Fatal("canceled manager retained ownership", err)
	}
}

func TestManagerLossNeverReacquiresOrReleasesSuccessor(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	m, err := Start(ctx, pool, instance, ownerA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Simulate a completed takeover atomically rather than racing an artificial
	// expiry against the manager's ticker. The successor is already live.
	old := m.Lease()
	if _, err = pool.Exec(ctx, "UPDATE instance_writer_leases SET owner_id=$2,epoch=epoch+1,expires_at=clock_timestamp()+interval '1 minute' WHERE instance_id=$1", instance, ownerB); err != nil {
		t.Fatal(err)
	}
	waitManager(t, m)
	if !errors.Is(m.Err(), ErrLost) || m.Lease().Epoch != 0 {
		t.Fatal("superseded manager remained writer", m.Err())
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	successor := Lease{InstanceID: instance, OwnerID: ownerB, Epoch: old.Epoch + 1}
	if err = require(t, pool, successor); err != nil {
		t.Fatal("manager cleanup disturbed successor", err)
	}
	if _, err = Acquire(ctx, pool, instance, ownerA, time.Second); !errors.Is(err, ErrHeld) {
		t.Fatal("old manager silently reacquired", err)
	}
}

func TestManagerRenewalStorageFailureFailsClosed(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	m, err := Start(ctx, pool, instance, ownerA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Removing the relation models a permanent storage failure. This fixture is
	// isolated and cleanup uses the admin pool, so no other tests are affected.
	if _, err = pool.Exec(ctx, "DROP TABLE instance_writer_leases"); err != nil {
		m.Close()
		t.Fatal(err)
	}
	waitManager(t, m)
	if !errors.Is(m.Err(), ErrUnavailable) || m.Lease().Epoch != 0 {
		t.Fatal("storage failure did not stop writer", m.Err())
	}
	if err = m.Close(); !errors.Is(err, ErrUnavailable) {
		t.Fatal("release failure not observable", err)
	}
}

func TestManagerBlockedRenewalSignalsBeforeCleanup(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	m, err := Start(ctx, pool, instance, ownerA, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT instance_id FROM instance_writer_leases WHERE instance_id=$1 FOR UPDATE", instance); err != nil {
		tx.Rollback(ctx)
		m.Close()
		t.Fatal(err)
	}
	waitManager(t, m)
	if !errors.Is(m.Err(), ErrUnavailable) || m.Lease().Epoch != 0 {
		t.Fatal("blocked renewal did not fail closed", m.Err())
	}
	// Done must not wait for cleanup's Release to obtain this transaction's lock.
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartWaitOutlastsAbandonedLease(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	// A previous owner that never released (evicted/killed) leaves a live lease.
	if _, err := Acquire(ctx, pool, instance, ownerA, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := StartWait(ctx, pool, instance, ownerB, time.Second, 0); !errors.Is(err, ErrHeld) {
		t.Fatal("zero wait must fail fast", err)
	}
	m, err := StartWait(ctx, pool, instance, ownerB, time.Second, 10*time.Second)
	if err != nil {
		t.Fatal("did not wait for expiry", err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
}
