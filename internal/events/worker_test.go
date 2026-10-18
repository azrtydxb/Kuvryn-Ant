package events

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcherInstanceScope(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	own, foreign := instance(t, pool), instance(t, pool)
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer endpoint.Close()
	all, err := New(pool, nil, signingKeys(), Options{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	scoped, err := New(pool, nil, signingKeys(), Options{AllowHTTP: true, AllowPrivate: true, InstanceID: strings.ToUpper(own)})
	if err != nil {
		t.Fatal(err)
	}
	defer scoped.Close()
	ownSub, err := scoped.Subscribe(ctx, own, endpoint.URL, "owned")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scoped.Subscribe(ctx, foreign, endpoint.URL, "foreign"); !errors.Is(err, ErrConfiguration) {
		t.Fatal("foreign subscription accepted")
	}
	foreignSub, err := all.Subscribe(ctx, foreign, endpoint.URL, "foreign")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event(t, tx, own)
	event(t, tx, foreign)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = scoped.Materialize(ctx, foreignSub, 200); !errors.Is(err, ErrConfiguration) {
		t.Fatal("foreign materialization accepted")
	}
	if _, err = all.Materialize(ctx, foreignSub, 200); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE event_deliveries SET attempts=8,lease_token=gen_random_uuid(),lease_until=clock_timestamp()-interval '1 second' WHERE subscription_id=$1", foreignSub); err != nil {
		t.Fatal(err)
	}
	if _, err = scoped.Materialize(ctx, ownSub, 200); err != nil {
		t.Fatal(err)
	}
	if attempted, err := scoped.DeliverOne(ctx); err != nil || !attempted {
		t.Fatal("own event not delivered", err)
	}
	if attempted, err := scoped.DeliverOne(ctx); err != nil || attempted {
		t.Fatal("foreign event claimed", err)
	}
	var state string
	if err = pool.QueryRow(ctx, "SELECT state FROM event_deliveries WHERE subscription_id=$1", foreignSub).Scan(&state); err != nil || state != "pending" {
		t.Fatal("foreign exhausted claim mutated", err)
	}
	if calls.Load() != 1 {
		t.Fatal("foreign network side effect")
	}
	if _, err = New(pool, nil, signingKeys(), Options{InstanceID: "not-a-uuid"}); !errors.Is(err, ErrConfiguration) {
		t.Fatal("invalid scope accepted")
	}
}

func TestWorkerDiscoversDeliversAndBoundsConcurrency(t *testing.T) {
	pool := fixture(t)
	owner := instance(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	var active, peak atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old; old = peak.Load() {
			if peak.CompareAndSwap(old, count) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	}))
	defer endpoint.Close()
	var discovered, attempts atomic.Int32
	d, err := New(pool, nil, signingKeys(), Options{AllowHTTP: true, AllowPrivate: true, InstanceID: owner, Observe: func(p Progress) { discovered.Add(int32(p.Materialized)); attempts.Add(int32(p.Attempts)) }})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	sub, err := d.Subscribe(ctx, owner, endpoint.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		event(t, tx, owner)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	for range deliveryWorkers {
		select {
		case <-started:
		case <-time.After(4 * time.Second):
			cancel()
			close(release)
			t.Fatal("delivery workers did not start")
		}
	}
	if err = d.Run(context.Background()); !errors.Is(err, ErrConfiguration) {
		cancel()
		close(release)
		t.Fatal("duplicate Run started more workers")
	}
	select {
	case <-started:
		cancel()
		close(release)
		t.Fatal("more than four concurrent attempts")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		var delivered int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM event_deliveries WHERE subscription_id=$1 AND state='delivered'", sub).Scan(&delivered); err != nil {
			t.Fatal(err)
		}
		if delivered == 8 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("worker pipeline did not finish")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker cancellation did not join")
	}
	if peak.Load() != deliveryWorkers || discovered.Load() != 8 || attempts.Load() != 8 {
		t.Fatalf("unexpected operational counts: peak=%d discovered=%d attempts=%d", peak.Load(), discovered.Load(), attempts.Load())
	}
}

func TestWorkerSubscriptionKeysetPagesAreBoundedAndScoped(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	owner, foreign := instance(t, pool), instance(t, pool)
	for _, instance := range []string{owner, foreign} {
		if _, err := pool.Exec(ctx, "INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) SELECT $1,'https://example.test','key' FROM generate_series(1,$2)", instance, subscriptionPageSize+7); err != nil {
			t.Fatal(err)
		}
	}
	d, err := New(pool, nil, signingKeys(), Options{InstanceID: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.subscriptions(ctx, "")
	if err != nil || len(first) != subscriptionPageSize {
		t.Fatal("first keyset page unbounded", err)
	}
	second, err := d.subscriptions(ctx, first[len(first)-1])
	if err != nil || len(second) != 7 {
		t.Fatal("second keyset page crossed scope", err)
	}
	seen := map[string]bool{}
	for _, id := range first {
		seen[id] = true
	}
	for _, id := range second {
		if seen[id] {
			t.Fatal("keyset page repeated subscription")
		}
	}
	third, err := d.subscriptions(ctx, second[len(second)-1])
	if err != nil || len(third) != 0 {
		t.Fatal("keyset page did not end", err)
	}
}

func TestWorkerStorageFailureBackoffAndCancellation(t *testing.T) {
	pool := fixture(t)
	owner := instance(t, pool)
	var failures atomic.Int32
	d, err := New(pool, nil, signingKeys(), Options{InstanceID: owner, Observe: func(p Progress) { failures.Add(int32(p.StorageFailures)) }})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	timer := time.NewTimer(200 * time.Millisecond)
	<-timer.C
	if failures.Load() != deliveryWorkers+1 {
		t.Fatalf("failure polling hot-looped or workers missing: %d", failures.Load())
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
	for _, n := range []int{1, 2, 5, 6, 100} {
		delay := retryDelay(n)
		if delay < time.Second || delay > 16*time.Second {
			t.Fatal("retry delay outside bounds")
		}
	}
}

func TestExhaustedCleanupIsBoundedAndSkipsLockedRows(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	owner := instance(t, pool)
	d, err := New(pool, nil, signingKeys(), Options{InstanceID: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	sub, err := d.Subscribe(ctx, owner, "https://example.test", "key")
	if err != nil {
		t.Fatal(err)
	}
	var audit int64
	if err = pool.QueryRow(ctx, "INSERT INTO audit_events(instance_id,action) VALUES($1,'test') RETURNING id", owner).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO outbox_events(audit_id,topic,payload) SELECT $1,'test','{}'::jsonb FROM generate_series(1,202)", audit); err != nil {
		t.Fatal(err)
	}
	if count, err := d.Materialize(ctx, sub, 200); err != nil || count != 200 {
		t.Fatal("first materialization failed", err)
	}
	if count, err := d.Materialize(ctx, sub, 200); err != nil || count != 2 {
		t.Fatal("second materialization failed", err)
	}
	if _, err = pool.Exec(ctx, "UPDATE event_deliveries SET attempts=8,lease_token=gen_random_uuid(),lease_until=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	locked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(ctx)
	if _, err = locked.Exec(ctx, "SELECT event_id FROM event_deliveries ORDER BY event_id LIMIT 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.claim(ctx); err != nil || ok {
		t.Fatal("cleanup blocked or claimed exhausted delivery", err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM event_deliveries WHERE state='dead'").Scan(&count); err != nil || count != 200 {
		t.Fatalf("cleanup batch not bounded: %d %v", count, err)
	}
	if err = locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.claim(ctx); err != nil || ok {
		t.Fatal("cleanup restart failed", err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM event_deliveries WHERE state='dead'").Scan(&count); err != nil || count != 202 {
		t.Fatal("cleanup did not resume", err)
	}
}
