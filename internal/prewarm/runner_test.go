package prewarm

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/testdb"
)

// fakeFeeds stands in for the npm data plane. Behavior is chosen by package name.
type fakeFeeds struct {
	mu       sync.Mutex
	calls    map[string]int
	filled   map[string]bool
	gate     map[string]chan struct{} // requests for these names block until the channel closes
	exists   map[string]bool          // names that stop being 404 (flipped by tests)
	requests atomic.Int32
}

func newFake() *fakeFeeds {
	return &fakeFeeds{calls: map[string]int{}, filled: map[string]bool{}, gate: map[string]chan struct{}{}, exists: map[string]bool{}}
}

func (f *fakeFeeds) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeFeeds) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ant_") {
		http.Error(w, "unauthenticated", 401)
		return
	}
	// /feeds/npmc/npm/<name>/-/<base>-<version>.tgz
	rest := strings.TrimPrefix(r.URL.Path, "/feeds/npmc/npm/")
	name, _, ok := strings.Cut(rest, "/-/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.calls[name]++
	n := f.calls[name]
	gate := f.gate[name]
	filled := f.filled[name]
	exists := f.exists[name]
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	switch name {
	case "missing":
		if !exists {
			http.NotFound(w, r)
			return
		}
	case "flaky":
		if n < 3 {
			http.Error(w, "busy", 503)
			return
		}
	case "down":
		http.Error(w, "outage", 503)
		return
	case "denied":
		http.Error(w, "no", 403)
		return
	}
	if !filled && name != "cached" {
		f.mu.Lock()
		f.filled[name] = true
		f.mu.Unlock()
		artifact.NoteFill(r.Context(), 3)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write([]byte("abc"))
}

type env struct {
	t         *testing.T
	db        testdb.Fixture
	feed      string
	principal string
	fake      *fakeFeeds
	svc       *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	ctx := context.Background()
	e := &env{t: t, db: db, fake: newFake()}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','prewarm') RETURNING id::text`, db.InstanceID).Scan(&e.principal); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'npmc','npm') RETURNING id::text`, db.InstanceID).Scan(&e.feed); err != nil {
		t.Fatal(err)
	}
	e.svc = &Service{DB: db.Pool, InstanceID: db.InstanceID, Warmer: &Warmer{Feeds: e.fake}, PollInterval: 20 * time.Millisecond, Backoff: time.Millisecond}
	return e
}

func (e *env) start(ctx context.Context, svc *Service) (wait func()) {
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	return func() { <-done }
}

func (e *env) job(spec Spec, mutate func(*NewJob)) Job {
	e.t.Helper()
	n := NewJob{FeedID: e.feed, Name: "job", Spec: spec, Concurrency: 2, RatePerSecond: 0, Enabled: true, CreatedBy: e.principal}
	if mutate != nil {
		mutate(&n)
	}
	j, err := InsertJob(context.Background(), e.db.Pool, e.db.InstanceID, n)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) run(jobID, trigger, retryOf string) Run {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := StartRun(ctx, tx, e.db.InstanceID, jobID, trigger, e.principal, retryOf)
	if err != nil {
		_ = tx.Rollback(ctx)
		e.t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) waitState(id string, states ...string) Run {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := GetRun(context.Background(), e.db.Pool, e.db.InstanceID, id)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, s := range states {
			if r.State == s {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := GetRun(context.Background(), e.db.Pool, e.db.InstanceID, id)
	e.t.Fatalf("run %s did not reach %v: %+v", id, states, r)
	return r
}

func (e *env) items(runID string) map[string]RunItem {
	e.t.Helper()
	list, err := ListItems(context.Background(), e.db.Pool, runID, "", -1, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]RunItem{}
	for _, it := range list {
		out[it.Name] = it
	}
	return out
}

func spec(items ...Item) Spec { return Spec{Kind: KindPackages, Items: items} }

func sriOf(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestRunWarmsPackageList(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := e.start(ctx, e.svc)
	defer func() { cancel(); wait() }()
	job := e.job(spec(
		Item{Name: "fresh", Version: "1.0.0", Integrity: sriOf([]byte("abc"))},
		Item{Name: "cached", Version: "1.0.0"},
		Item{Name: "missing", Version: "1.0.0"},
		Item{Name: "flaky", Version: "1.0.0"},
		Item{Name: "tampered", Version: "1.0.0", Integrity: sriOf([]byte("other bytes"))},
		Item{Name: "denied", Version: "1.0.0"},
	), nil)
	run := e.run(job.ID, "manual", "")
	e.svc.Notify()
	got := e.waitState(run.ID, StateSucceeded, StatePartial, StateFailed)
	items := e.items(run.ID)
	for name, want := range map[string]struct{ state, code string }{
		"fresh": {ItemWarmed, ""}, "cached": {ItemCached, ""}, "flaky": {ItemWarmed, ""},
		"missing": {ItemFailed, ErrNotFound}, "tampered": {ItemFailed, ErrIntegrity}, "denied": {ItemFailed, ErrDenied},
	} {
		it := items[name]
		if it.State != want.state || (want.code != "" && (it.Error == nil || *it.Error != want.code)) {
			t.Errorf("%s: %+v want %+v", name, it, want)
		}
	}
	if items["flaky"].Attempts != 3 || items["missing"].Attempts != 1 {
		t.Errorf("attempts: flaky=%d missing=%d", items["flaky"].Attempts, items["missing"].Attempts)
	}
	if got.State != StatePartial || got.Total != 6 || got.Warmed != 2 || got.Cached != 1 || got.Failed != 3 || got.Bytes != 12 {
		// bytes: fresh 3 + cached 3 + flaky 3 + tampered 3 = 12; failed 404/403 read nothing
		t.Errorf("run: %+v", got)
	}
	if got.FinishedAt == nil || got.StartedAt == nil {
		t.Errorf("timestamps: %+v", got)
	}
	if n := e.db.Count(t, `SELECT count(*) FROM audit_events WHERE action='prewarm.run.completed' AND resource_id=$1::uuid`, run.ID); n != 1 {
		t.Errorf("completion events: %d", n)
	}
	// A second run finds the first run's work already cached.
	run2 := e.run(job.ID, "manual", "")
	e.svc.Notify()
	got2 := e.waitState(run2.ID, StateSucceeded, StatePartial, StateFailed)
	if got2.Warmed != 0 || got2.Cached != 3 {
		t.Errorf("second run: %+v", got2)
	}
	// Retry covers only the failed items and picks up what changed upstream.
	e.fake.mu.Lock()
	e.fake.exists["missing"] = true
	e.fake.mu.Unlock()
	retry := e.run(job.ID, "retry", run2.ID)
	if retry.Total != 3 || retry.RetryOf == nil || *retry.RetryOf != run2.ID {
		t.Fatalf("retry set: %+v", retry)
	}
	e.svc.Notify()
	got3 := e.waitState(retry.ID, StateSucceeded, StatePartial, StateFailed)
	if got3.Warmed != 1 || got3.Failed != 2 || got3.State != StatePartial {
		t.Errorf("retry run: %+v", got3)
	}
	var old int
	_ = e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM prewarm_runs WHERE job_id=$1`, job.ID).Scan(&old)
	if old != 3 {
		t.Errorf("runs kept: %d", old)
	}
}

func TestOneActiveRunPerJob(t *testing.T) {
	e := newEnv(t)
	job := e.job(spec(Item{Name: "a", Version: "1"}), nil)
	e.run(job.ID, "manual", "")
	tx, _ := e.db.Pool.Begin(context.Background())
	defer tx.Rollback(context.Background())
	if _, err := StartRun(context.Background(), tx, e.db.InstanceID, job.ID, "manual", e.principal, ""); err != ErrActive {
		t.Fatalf("second active run: %v", err)
	}
}

func TestCancelRunningRun(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gate["slow"] = gate
	ctx, cancel := context.WithCancel(context.Background())
	wait := e.start(ctx, e.svc)
	defer func() { cancel(); wait() }()
	job := e.job(spec(Item{Name: "slow", Version: "1"}, Item{Name: "after", Version: "1"}, Item{Name: "after2", Version: "1"}), func(n *NewJob) { n.Concurrency = 1 })
	run := e.run(job.ID, "manual", "")
	e.svc.Notify()
	e.waitState(run.ID, StateRunning)
	for e.fake.count("slow") == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	tx, _ := e.db.Pool.Begin(context.Background())
	if _, err := RequestCancel(context.Background(), tx, e.db.InstanceID, run.ID); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(context.Background())
	got := e.waitState(run.ID, StateCanceled)
	if got.Skipped != 3 || got.Warmed != 0 {
		t.Fatalf("canceled run: %+v", got)
	}
	for name, it := range e.items(run.ID) {
		if it.State != ItemSkipped || it.Error == nil || *it.Error != ErrCanceled {
			t.Errorf("%s: %+v", name, it)
		}
	}
	if e.fake.count("after") != 0 {
		t.Error("items after the cancel were started")
	}
	// Cancel after the end is rejected; a queued run is canceled at once.
	tx, _ = e.db.Pool.Begin(context.Background())
	if _, err := RequestCancel(context.Background(), tx, e.db.InstanceID, run.ID); err != ErrState {
		t.Errorf("cancel finished: %v", err)
	}
	_ = tx.Rollback(context.Background())
	queued := e.run(job.ID, "manual", "")
	tx, _ = e.db.Pool.Begin(context.Background())
	r, err := RequestCancel(context.Background(), tx, e.db.InstanceID, queued.ID)
	if err != nil || r.State != StateCanceled {
		t.Errorf("cancel queued: %+v %v", r, err)
	}
	_ = tx.Commit(context.Background())
}

func TestResumeAfterRestart(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	e.fake.gate["second"] = gate
	first, stop := context.WithCancel(context.Background())
	wait := e.start(first, e.svc)
	job := e.job(spec(Item{Name: "first", Version: "1"}, Item{Name: "second", Version: "1"}, Item{Name: "third", Version: "1"}), func(n *NewJob) { n.Concurrency = 1 })
	run := e.run(job.ID, "manual", "")
	e.svc.Notify()
	for e.fake.count("second") == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	stop() // the process stops while "second" is in flight
	wait()
	mid, _ := GetRun(context.Background(), e.db.Pool, e.db.InstanceID, run.ID)
	if mid.State != StateRunning {
		t.Fatalf("interrupted run must stay resumable, got %s", mid.State)
	}
	if it := e.items(run.ID)["first"]; it.State != ItemWarmed {
		t.Fatalf("checkpoint lost: %+v", it)
	}
	// A new process (fresh Service) resumes: finished items are not redone.
	close(gate)
	svc2 := &Service{DB: e.db.Pool, InstanceID: e.db.InstanceID, Warmer: &Warmer{Feeds: e.fake}, PollInterval: 20 * time.Millisecond}
	second, stop2 := context.WithCancel(context.Background())
	wait2 := e.start(second, svc2)
	defer func() { stop2(); wait2() }()
	got := e.waitState(run.ID, StateSucceeded)
	if got.Warmed+got.Cached != 3 || e.fake.count("first") != 1 {
		t.Fatalf("resume: %+v first calls=%d", got, e.fake.count("first"))
	}
}

func TestRunStopsWhenUpstreamKeepsFailing(t *testing.T) {
	e := newEnv(t)
	e.svc.AbortAfter = 3
	ctx, cancel := context.WithCancel(context.Background())
	wait := e.start(ctx, e.svc)
	defer func() { cancel(); wait() }()
	var items []Item
	for i := 0; i < 30; i++ {
		items = append(items, Item{Name: "down", Version: fmt.Sprint(i)})
	}
	job := e.job(spec(items...), func(n *NewJob) { n.Concurrency = 1 })
	run := e.run(job.ID, "manual", "")
	e.svc.Notify()
	got := e.waitState(run.ID, StateFailed)
	if got.Error == nil || !strings.Contains(*got.Error, "upstream kept failing") || got.Failed >= 30 || got.Failed < 3 {
		t.Fatalf("aborted run: %+v", got)
	}
	pending := e.db.Count(t, `SELECT count(*) FROM prewarm_run_items WHERE run_id=$1 AND state='pending'`, run.ID)
	if pending == 0 {
		t.Fatal("remaining items must stay pending for a retry")
	}
	retry := e.run(job.ID, "retry", run.ID)
	if retry.Total != 30 {
		t.Fatalf("retry covers failed and pending items: %d", retry.Total)
	}
}

func TestScheduleStartsRuns(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := e.start(ctx, e.svc)
	defer func() { cancel(); wait() }()
	interval := 3600
	due := e.job(spec(Item{Name: "a", Version: "1"}), func(n *NewJob) { n.IntervalSeconds = &interval })
	idle := e.job(spec(Item{Name: "b", Version: "1"}), func(n *NewJob) { n.IntervalSeconds = &interval; n.Enabled = false })
	later := e.job(spec(Item{Name: "c", Version: "1"}), func(n *NewJob) { n.IntervalSeconds = &interval })
	if due.NextRunAt == nil || time.Until(*due.NextRunAt) < 59*time.Minute {
		t.Fatalf("first run due in an interval: %v", due.NextRunAt)
	}
	for _, id := range []string{due.ID, idle.ID} {
		if _, err := e.db.Pool.Exec(context.Background(), `UPDATE prewarm_jobs SET next_run_at=now()-interval '1 minute' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.Notify()
	deadline := time.Now().Add(15 * time.Second)
	var run Run
	for time.Now().Before(deadline) {
		runs, _ := ListRuns(context.Background(), e.db.Pool, e.db.InstanceID, due.ID, "", "", "", 10)
		if len(runs) > 0 && runs[0].State == StateSucceeded {
			run = runs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.ID == "" || run.Trigger != "schedule" || run.PrincipalID != e.principal {
		t.Fatalf("scheduled run: %+v", run)
	}
	j, _ := GetJob(context.Background(), e.db.Pool, e.db.InstanceID, due.ID)
	if j.NextRunAt == nil || time.Until(*j.NextRunAt) < 59*time.Minute || j.LastRun == nil || j.LastRun.ID != run.ID {
		t.Fatalf("schedule not advanced: %+v", j)
	}
	if n := e.db.Count(t, `SELECT count(*) FROM prewarm_runs WHERE job_id IN ($1,$2)`, idle.ID, later.ID); n != 0 {
		t.Fatalf("jobs that are disabled or not yet due ran: %d", n)
	}
}

func TestRunFromFeedReferences(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var other, source, component string
	if err := e.db.Pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'hosted','npm') RETURNING id::text`, e.db.InstanceID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','npm') RETURNING id::text`, e.db.InstanceID).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, e.db.InstanceID, other, source); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, version string }{{"cached", "1.0.0"}, {"cached", "2.0.0"}, {"fresh", "1.0.0"}} {
		if err := e.db.Pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,'') ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate RETURNING id::text`, e.db.InstanceID, source, c.name).Scan(&component); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3)`, e.db.InstanceID, component, c.version); err != nil {
			t.Fatal(err)
		}
	}
	job := e.job(Spec{Kind: KindFeed, SourceFeedID: other}, nil)
	if job.ItemCount != 0 || job.SourceFeedID != other {
		t.Fatalf("job: %+v", job)
	}
	run := e.run(job.ID, "manual", "")
	if run.Total != 3 {
		t.Fatalf("items resolved from the feed: %d", run.Total)
	}
	items, _ := ListItems(ctx, e.db.Pool, run.ID, "", -1, 10)
	if len(items) != 3 || items[0].Name != "cached" || items[0].Version != "1.0.0" || items[2].Name != "fresh" {
		t.Fatalf("%+v", items)
	}
	empty := e.job(Spec{Kind: KindFeed, SourceFeedID: e.feed}, nil)
	tx, _ := e.db.Pool.Begin(ctx)
	defer tx.Rollback(ctx)
	if _, err := StartRun(ctx, tx, e.db.InstanceID, empty.ID, "manual", e.principal, ""); err != ErrEmpty {
		t.Fatalf("empty source: %v", err)
	}
}

func TestDisabledPrincipalFailsRun(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := e.start(ctx, e.svc)
	defer func() { cancel(); wait() }()
	job := e.job(spec(Item{Name: "a", Version: "1"}), nil)
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE principals SET disabled=true WHERE id=$1`, e.principal); err != nil {
		t.Fatal(err)
	}
	run := e.run(job.ID, "manual", "")
	e.svc.Notify()
	got := e.waitState(run.ID, StateFailed)
	if got.Error == nil || !strings.Contains(*got.Error, "principal") || e.fake.requests.Load() != 0 {
		t.Fatalf("%+v requests=%d", got, e.fake.requests.Load())
	}
}

func TestJobCrud(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	interval := 600
	job := e.job(spec(Item{Name: "a", Version: "1"}), func(n *NewJob) { n.IntervalSeconds = &interval })
	tx, _ := e.db.Pool.Begin(ctx)
	name, off := "renamed", false
	got, err := UpdateJob(ctx, tx, e.db.InstanceID, job.ID, job.Revision, JobPatch{Name: &name, Enabled: &off, ClearSchedule: true})
	if err != nil || got.Name != "renamed" || got.Enabled || got.IntervalSeconds != nil || got.NextRunAt != nil || got.Revision != job.Revision+1 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = UpdateJob(ctx, tx, e.db.InstanceID, job.ID, job.Revision, JobPatch{Name: &name}); err != database.ErrRevisionConflict {
		t.Fatalf("stale revision: %v", err)
	}
	_ = tx.Commit(ctx)
	tx, _ = e.db.Pool.Begin(ctx)
	if err = DeleteJob(ctx, tx, e.db.InstanceID, job.ID, got.Revision); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx)
	if _, err = GetJob(ctx, e.db.Pool, e.db.InstanceID, job.ID); err != database.ErrNotFound {
		t.Fatalf("deleted job: %v", err)
	}
}

func TestPacerSpacesStarts(t *testing.T) {
	p := newPacer(50)
	start := time.Now()
	for i := 0; i < 6; i++ {
		if err := p.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Fatalf("6 starts at 50/s took %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if newPacer(1).wait(ctx) == nil {
		t.Fatal("canceled wait returned nil")
	}
	unlimited := newPacer(0)
	start = time.Now()
	for i := 0; i < 1000; i++ {
		_ = unlimited.wait(context.Background())
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("unlimited pacer slept")
	}
}
