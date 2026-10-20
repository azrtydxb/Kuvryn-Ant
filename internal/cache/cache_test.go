package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func key() Key { return Key{"source", "package@version", "metadata", "principal-scope-generation"} }
func TestFreshnessIsolationAndExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	c, _ := New(Config{Now: func() time.Time { return now }, StaleFor: time.Hour})
	calls := 0
	load := func(context.Context) (Value, error) {
		calls++
		return Value{Status: 200, Body: []byte("ok"), SignedExpiry: now.Add(6 * time.Minute)}, nil
	}
	a, e := c.Get(context.Background(), key(), GetOptions{}, load)
	if e != nil {
		t.Fatal(e)
	}
	a.Body[0] = 'X'
	a, _ = c.Get(context.Background(), key(), GetOptions{}, load)
	if string(a.Body) != "ok" || calls != 1 {
		t.Fatal(a, calls)
	}
	other := key()
	other.AuthorizationDomain = "another"
	c.Get(context.Background(), other, GetOptions{}, load)
	if calls != 2 {
		t.Fatal("cross authorization cache hit")
	}
	now = now.Add(5 * time.Minute)
	failure := func(context.Context) (Value, error) { return Value{}, errors.New("offline") }
	if v, e := c.Get(context.Background(), key(), GetOptions{AllowStale: true}, failure); e != nil || string(v.Body) != "ok" {
		t.Fatal(v, e)
	}
	now = now.Add(time.Minute)
	if _, e := c.Get(context.Background(), key(), GetOptions{AllowStale: true}, failure); e == nil {
		t.Fatal("extended signed expiry")
	}
}
func TestNegativeAndDeniedAreNotGlobalMisses(t *testing.T) {
	now := time.Unix(100, 0)
	c, _ := New(Config{Now: func() time.Time { return now }})
	calls := 0
	load := func(context.Context) (Value, error) { calls++; return Value{Status: 404}, nil }
	for i := 0; i < 2; i++ {
		c.Get(context.Background(), key(), GetOptions{}, load)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	now = now.Add(31 * time.Second)
	c.Get(context.Background(), key(), GetOptions{}, load)
	if calls != 2 {
		t.Fatal(calls)
	}
	c.Invalidate(key())
	for _, status := range []int{401, 403} {
		for i := 0; i < 2; i++ {
			c.Get(context.Background(), key(), GetOptions{}, func(context.Context) (Value, error) { calls++; return Value{Status: status}, nil })
		}
	}
	if calls != 6 || c.Stats().Entries != 0 {
		t.Fatal(calls, c.Stats())
	}
}
func TestCoalescingAndLastWaiterCancellation(t *testing.T) {
	c, _ := New(Config{})
	start := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	loader := func(ctx context.Context) (Value, error) {
		if calls.Add(1) == 1 {
			close(start)
		}
		select {
		case <-ctx.Done():
			return Value{}, ctx.Err()
		case <-release:
			return Value{Status: 200, Body: []byte("shared")}, nil
		}
	}
	first, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { _, e := c.Get(first, key(), GetOptions{}, loader); done <- e }()
	<-start
	secondReady := make(chan struct{})
	go func() {
		close(secondReady)
		_, e := c.Get(context.Background(), key(), GetOptions{}, loader)
		done <- e
	}()
	<-secondReady
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		n := c.flights[key()].waiters
		c.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second waiter not attached")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	c.Invalidate(key())
	ctx, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	cancelled := make(chan struct{})
	go c.Get(ctx, key(), GetOptions{}, func(ctx context.Context) (Value, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return Value{}, ctx.Err()
	})
	<-started
	stop()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("orphan loader not cancelled")
	}
}
func TestBoundsAndInvalidation(t *testing.T) {
	c, _ := New(Config{MaxEntries: 2, MaxBytes: 1024})
	for _, id := range []string{"a", "b", "c"} {
		k := key()
		k.Identity = id
		if _, e := c.Get(context.Background(), k, GetOptions{}, func(context.Context) (Value, error) { return Value{Status: 200, Body: make([]byte, 100)}, nil }); e != nil {
			t.Fatal(e)
		}
	}
	if s := c.Stats(); s.Entries > 2 || s.Bytes > 1024 {
		t.Fatal(s)
	}
	_, e := c.Get(context.Background(), key(), GetOptions{}, func(context.Context) (Value, error) { return Value{Status: 200, Body: make([]byte, 1025)}, nil })
	if !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	end := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Get(context.Background(), key(), GetOptions{}, func(context.Context) (Value, error) { close(start); <-end; return Value{Status: 200}, nil })
	}()
	<-start
	c.Invalidate(key())
	close(end)
	wg.Wait()
	c.mu.Lock()
	_, exists := c.entries[key()]
	c.mu.Unlock()
	if exists {
		t.Fatal("invalidated loader repopulated")
	}
}
func TestConditionalRefreshCannotExtendSignature(t *testing.T) {
	now := time.Unix(100, 0)
	c, _ := New(Config{Now: func() time.Time { return now }, MetadataTTL: time.Second})
	c.Get(context.Background(), key(), GetOptions{}, func(context.Context) (Value, error) {
		return Value{Status: 200, Body: []byte("original"), ETag: "generation", SignedExpiry: now.Add(3 * time.Second)}, nil
	})
	now = now.Add(2 * time.Second)
	v, e := c.Revalidate(context.Background(), key(), GetOptions{}, func(_ context.Context, previous *Value) (Value, error) {
		if previous == nil || previous.ETag != "generation" {
			t.Fatal(previous)
		}
		return Value{Status: 304, SignedExpiry: now.Add(time.Hour)}, nil
	})
	if e != nil || string(v.Body) != "original" {
		t.Fatal(v, e)
	}
	now = now.Add(2 * time.Second)
	if _, e = c.Revalidate(context.Background(), key(), GetOptions{}, func(context.Context, *Value) (Value, error) { return Value{Status: 304}, nil }); !errors.Is(e, ErrInput) {
		t.Fatal("304 extended signature", e)
	}
}

func TestInvalidateMatchingRemovesOnlyMatchingEntries(t *testing.T) {
	c, _ := New(Config{})
	load := func(context.Context) (Value, error) { return Value{Status: 200, Body: []byte("x")}, nil }
	for _, id := range []string{"pkg-a/1", "pkg-a/2", "pkg-b/1"} {
		if _, err := c.Get(context.Background(), Key{"s", id, "m", "d"}, GetOptions{}, load); err != nil {
			t.Fatal(err)
		}
	}
	n := c.InvalidateMatching(func(k Key) bool { return k.Identity[:5] == "pkg-a" })
	if n != 2 || c.Stats().Entries != 1 {
		t.Fatalf("removed %d, entries %d", n, c.Stats().Entries)
	}
	if c.Stats().Bytes <= 0 {
		t.Fatal("byte accounting lost")
	}
}
