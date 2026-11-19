package remoteruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

func slotManager(wait time.Duration) *Manager {
	return &Manager{done: make(chan struct{}), options: Options{MaxInflight: 1, AdmissionWait: wait}, requests: make(chan struct{}, 1), network: make(chan struct{}, 1)}
}

// A request that finds every slot taken is shed with ErrBusy once the admission
// wait is over: distinguishable from an upstream failure, and a capacity error
// for the callers that already treat ErrCapacity as load shedding.
func TestWaitSlotShedsWithErrBusy(t *testing.T) {
	m := slotManager(50 * time.Millisecond)
	release, err := m.admission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = m.admission(context.Background())
	if !errors.Is(err, ErrBusy) || !errors.Is(err, ErrCapacity) || errors.Is(err, ErrTooLarge) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a full slot set must answer ErrBusy, got %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatalf("shed before the admission wait elapsed: %v", time.Since(start))
	}
	if got := safeError(err); got != ErrBusy {
		t.Fatalf("safeError lost the busy class: %v", got)
	}
	if err = m.networkSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = m.networkSlot(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("network slots must shed the same way: %v", err)
	}
	var metrics bytes.Buffer
	m.WriteMetrics(&metrics)
	for _, want := range []string{"ant_remote_slot_shed_total 2", "ant_remote_slots_in_use{kind=\"request\"} 1", "ant_remote_slots_in_use{kind=\"network\"} 1", "ant_remote_slots_max 1", "ant_remote_slot_waiters 0"} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, metrics.String())
		}
	}
	release()
	if release, err = m.admission(context.Background()); err != nil {
		t.Fatalf("a released slot must be reusable: %v", err)
	}
	release()
}

// A caller that gives up is not a shed request, and a closed manager says so.
func TestWaitSlotCallerCancelAndClose(t *testing.T) {
	m := slotManager(time.Minute)
	if _, err := m.admission(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := m.admission(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, ErrBusy) {
		t.Fatalf("a canceled caller is not load shedding: %v", err)
	}
	if m.busy.Load() != 0 {
		t.Fatal("a canceled wait was counted as shed")
	}
	close(m.done)
	if _, err := m.admission(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed manager: %v", err)
	}
}

// With one upstream slot held by a slow fetch, a cached document is still
// served, concurrent requests for the document being fetched wait for that one
// fetch instead of each taking a slot, and only a request for a different,
// uncached document is shed. Before the slot moved from the request to the load,
// every one of them queued for the slot and failed after the wait.
func TestRemoteRuntimeMetadataSlotsAreHeldPerLoadNotPerRequest(t *testing.T) {
	pool, boot := runtimeFixture(t)
	hold := make(chan struct{})
	entered := make(chan struct{}, 8)
	var slowCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fast":
			io.WriteString(w, "fast")
		case "/slow-a", "/slow-b":
			slowCalls.Add(1)
			entered <- struct{}{}
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, "slow")
		default:
			w.WriteHeader(404)
		}
	}))
	defer origin.Close()
	source := insertRemoteSource(t, pool, boot.InstanceID, "runtime-slots")
	c := remoteconfig.DefaultConfig(origin.URL)
	c.CARef = "runtime-ca"
	c.AllowPrivate = true
	c.TimeoutSeconds = 20
	insertConfig(t, pool, boot.InstanceID, source, 1, c)
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	const wait = 300 * time.Millisecond
	manager, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": writeCAPEM(t, origin)}, MaxInflight: 1, AdmissionWait: wait})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	var released sync.Once
	open := func() { released.Do(func() { close(hold) }) }
	defer open()
	meta := func(path string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		v, err := manager.Metadata(ctx, Request{SourceID: source, AuthorizationDomain: "d", Path: path})
		return string(v.Body), err
	}
	if body, err := meta("fast"); err != nil || body != "fast" {
		t.Fatal(body, err)
	}

	// The only slot is now held by a fetch that has not returned.
	type result struct {
		body string
		err  error
	}
	results := make(chan result, 8)
	go func() { b, e := meta("slow-a"); results <- result{b, e} }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("slow upstream fetch never started")
	}

	// A cached document needs no slot.
	if body, err := meta("fast"); err != nil || body != "fast" {
		t.Fatalf("cache hit queued behind a slow fetch: %q %v", body, err)
	}
	// A different uncached document cannot get one and is shed, not left to time out.
	start := time.Now()
	if _, err := meta("slow-b"); !errors.Is(err, ErrBusy) {
		t.Fatalf("an uncached document with no free slot must be shed with ErrBusy: %v", err)
	}
	if time.Since(start) < wait {
		t.Fatalf("shed after %v, before the admission wait %v", time.Since(start), wait)
	}
	// Requests for the document already being fetched coalesce onto that fetch and
	// outwait the admission wait without a slot of their own.
	for range 6 {
		go func() { b, e := meta("slow-a"); results <- result{b, e} }()
	}
	time.Sleep(2 * wait)
	open()
	for range 7 {
		select {
		case r := <-results:
			if r.err != nil || r.body != "slow" {
				t.Fatalf("coalesced request: %q %v", r.body, r.err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("coalesced request never finished")
		}
	}
	if slowCalls.Load() != 1 {
		t.Fatalf("slow-a fetched %d times, want one coalesced fetch", slowCalls.Load())
	}
	var metrics bytes.Buffer
	manager.WriteMetrics(&metrics)
	if !strings.Contains(metrics.String(), "ant_remote_slot_shed_total 1") {
		t.Fatalf("exactly the uncached request should have been shed:\n%s", metrics.String())
	}
}
