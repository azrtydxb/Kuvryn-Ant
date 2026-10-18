package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type migFixture struct {
	pool     *pgxpool.Pool
	instance string
	domains  []string
	reg      *Registry
	router   *DBRouter
	m        *Migrator
	mu       sync.Mutex
	mem      map[string]*blob.Mem // "disk/<domain>", "s3/<domain>"
	blobs    []blob.Info
	data     map[blob.Digest][]byte
	blobDom  map[blob.Digest]string
}

func newMigFixture(t *testing.T) *migFixture {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_storage_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Storage")
	if err != nil {
		t.Fatal(err)
	}
	f := &migFixture{pool: pool, instance: owner.InstanceID, mem: map[string]*blob.Mem{}, data: map[blob.Digest][]byte{}, blobDom: map[blob.Digest]string{}}
	for _, name := range []string{"a", "b"} {
		var id string
		if err = pool.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id::text`, f.instance, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.domains = append(f.domains, id)
	}
	reg, err := New(Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	for _, kind := range []string{"disk", "s3"} {
		reg.SetFactory(kind, func(domain string) (blob.Store, error) { return f.store(kind, domain), nil })
	}
	f.reg = reg
	f.router = &DBRouter{DB: pool, InstanceID: f.instance, Default: "disk", TTL: time.Millisecond}
	reg.SetRouter(f.router)
	f.m = &Migrator{DB: pool, InstanceID: f.instance, Backends: reg, Default: "disk", OnSwitch: f.router.Invalidate, Owner: "test", CheckpointEvery: time.Nanosecond}
	return f
}

func (f *migFixture) store(kind, domain string) *blob.Mem {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := kind + "/" + domain
	if f.mem[k] == nil {
		f.mem[k] = blob.NewMem()
	}
	return f.mem[k]
}

// seed commits n blobs (distinct content, a few empty/large) to the disk store
// and the database.
func (f *migFixture) seed(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		var data []byte
		switch {
		case i == 0:
			data = nil
		case i%10 == 0:
			data = bytes.Repeat([]byte{byte(i)}, 300_000)
		default:
			data = []byte(fmt.Sprintf("blob-%d-%s", i, bytes.Repeat([]byte("x"), i)))
		}
		dom := f.domains[i%2]
		info := f.store("disk", dom).Put(data)
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,'k')`, f.instance, dom, string(info.Digest), info.Size); err != nil {
			t.Fatal(err)
		}
		f.blobs = append(f.blobs, info)
		f.data[info.Digest] = data
		f.blobDom[info.Digest] = dom
	}
}

func (f *migFixture) readAll(ctx context.Context, d blob.Digest) error {
	st, err := f.reg.Resolve(ctx, f.blobDom[d])
	if err != nil {
		return err
	}
	rc, err := st.OpenRange(ctx, d, 0, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != string(d) {
		return errors.New("content mismatch")
	}
	return nil
}

func (f *migFixture) countOn(kind string) int {
	n := 0
	for _, d := range f.domains {
		n += f.store(kind, d).Len()
	}
	return n
}

func (f *migFixture) start(t *testing.T, o StartOptions) Job {
	t.Helper()
	j, _, err := f.m.Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestMigrateDiskToS3AndBackWithInterruption(t *testing.T) {
	f := newMigFixture(t)
	ctx := context.Background()
	const total = 60
	f.seed(t, total)

	// Reads must succeed throughout: during copy, interruption, switch and cleanup.
	var readErrs, reads atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := f.readAll(ctx, f.blobs[i%total].Digest); err != nil {
				readErrs.Add(1)
			}
			reads.Add(1)
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
		if readErrs.Load() != 0 || reads.Load() == 0 {
			t.Errorf("reads=%d failures=%d", reads.Load(), readErrs.Load())
		}
	}()

	// Dry run reports exactly what the real run does and changes nothing.
	dj := f.start(t, StartOptions{Kind: KindMigrate, To: "s3", DryRun: true})
	dj, err := f.m.Run(ctx, dj.ID)
	if err != nil || dj.State != StateCompleted || dj.Copied != total || dj.Skipped != 0 || dj.Failed != 0 {
		t.Fatalf("dry run %+v %v", dj, err)
	}
	if f.countOn("s3") != 0 {
		t.Fatal("dry run wrote data")
	}
	if rt, _ := f.m.Route(ctx); rt.Active != "disk" {
		t.Fatalf("dry run switched: %+v", rt)
	}

	// Real run, interrupted after 25 blobs (as by Ctrl-C or a crash).
	job := f.start(t, StartOptions{Kind: KindMigrate, To: "s3", BytesPerSec: 0})
	runCtx, cancel := context.WithCancel(ctx)
	f.m.AfterBlob = func(j Job) {
		if j.Pass == 1 && j.Processed == 25 {
			cancel()
		}
	}
	got, err := f.m.Run(runCtx, job.ID)
	if !errors.Is(err, context.Canceled) || got.State != StatePaused {
		t.Fatalf("interrupt: %+v %v", got, err)
	}
	if got.Processed < 25 || got.Processed >= total {
		t.Fatalf("unexpected checkpoint %d", got.Processed)
	}
	if rt, _ := f.m.Route(ctx); rt.Active != "disk" || rt.Previous != "" {
		t.Fatalf("switched before completion: %+v", rt)
	}
	partial := f.countOn("s3")
	if partial == 0 || partial >= total {
		t.Fatalf("partial copy %d", partial)
	}
	for _, b := range f.blobs { // old store untouched, reads fine mid-migration
		if err := f.readAll(ctx, b.Digest); err != nil {
			t.Fatal(err)
		}
	}

	// A "restarted process" resumes the same job from the cursor.
	f.m.AfterBlob = nil
	again, resumed, err := f.m.Start(ctx, StartOptions{Kind: KindMigrate, To: "s3"})
	if err != nil || !resumed || again.ID != job.ID || again.Processed != got.Processed {
		t.Fatalf("resume %+v %v %v", again, resumed, err)
	}
	done, err := f.m.Run(ctx, again.ID)
	if err != nil || done.State != StateCompleted || done.SwitchedAt == nil {
		t.Fatalf("complete %+v %v", done, err)
	}
	if done.Processed != total || done.Failed != 0 || done.Copied+done.Skipped != total {
		t.Fatalf("counters %+v", done)
	}
	if done.Copied < int64(total-partial) || done.Copied > int64(total) {
		t.Fatalf("copied %d (partial %d)", done.Copied, partial)
	}
	if f.countOn("s3") != total || f.countOn("disk") != total {
		t.Fatalf("s3=%d disk=%d", f.countOn("s3"), f.countOn("disk"))
	}
	rt, _ := f.m.Route(ctx)
	if rt.Active != "s3" || rt.Previous != "disk" {
		t.Fatalf("route %+v", rt)
	}

	// Writes now land on S3; reads of anything missing there fall back to disk.
	st, _ := f.reg.Resolve(ctx, f.domains[0])
	up, _ := st.Stage(ctx)
	payload := []byte("written after switch")
	_, _ = up.Write(payload)
	sum := sha256.Sum256(payload)
	info := blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(payload))}
	if _, err = up.Finalize(ctx, info); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store("s3", f.domains[0]).Stat(ctx, info.Digest); err != nil {
		t.Fatal("write did not reach the active store")
	}
	victim := f.blobs[3]
	_ = f.store("s3", f.blobDom[victim.Digest]).Delete(ctx, victim.Digest)
	if err = f.readAll(ctx, victim.Digest); err != nil {
		t.Fatalf("fallback read: %v", err)
	}

	// Cannot migrate again until the old store is cleaned up.
	if _, _, err = f.m.Start(ctx, StartOptions{Kind: KindMigrate, To: "disk"}); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("expected cleanup pending, got %v", err)
	}
	// Cleanup dry run predicts the real run; the blob missing on S3 is repaired
	// from the old store before the old copy is removed.
	cd := f.start(t, StartOptions{Kind: KindCleanup, DryRun: true})
	cd, err = f.m.Run(ctx, cd.ID)
	if err != nil || cd.Deleted != total || cd.Copied != 1 || f.countOn("disk") != total {
		t.Fatalf("cleanup dry run %+v %v", cd, err)
	}
	cj := f.start(t, StartOptions{Kind: KindCleanup})
	cj, err = f.m.Run(ctx, cj.ID)
	if err != nil || cj.State != StateCompleted || cj.Deleted != total || cj.Copied != 1 {
		t.Fatalf("cleanup %+v %v", cj, err)
	}
	if f.countOn("disk") != 0 {
		t.Fatalf("old store not emptied: %d", f.countOn("disk"))
	}
	rt, _ = f.m.Route(ctx)
	if rt.Active != "s3" || rt.Previous != "" {
		t.Fatalf("route after cleanup %+v", rt)
	}
	for _, b := range f.blobs {
		if err := f.readAll(ctx, b.Digest); err != nil {
			t.Fatal(err)
		}
	}

	// And back: S3 to disk.
	back := f.start(t, StartOptions{Kind: KindMigrate, To: "disk"})
	back, err = f.m.Run(ctx, back.ID)
	if err != nil || back.State != StateCompleted || back.Copied != total {
		t.Fatalf("back %+v %v", back, err)
	}
	if rt, _ = f.m.Route(ctx); rt.Active != "disk" || rt.Previous != "s3" {
		t.Fatalf("route %+v", rt)
	}
	for _, b := range f.blobs {
		if _, err := f.store("disk", f.blobDom[b.Digest]).Stat(ctx, b.Digest); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateRepairsCorruptTargetAndFailsOnMissingSource(t *testing.T) {
	f := newMigFixture(t)
	ctx := context.Background()
	f.seed(t, 12)
	// A corrupt, same-size object already sits on the target.
	victim := f.blobs[5]
	bad := bytes.Repeat([]byte("z"), int(victim.Size))
	f.store("s3", f.blobDom[victim.Digest]).Corrupt(victim.Digest, bad)
	dry := f.start(t, StartOptions{Kind: KindMigrate, To: "s3", DryRun: true})
	dry, err := f.m.Run(ctx, dry.ID)
	if err != nil || dry.Copied != 12 {
		t.Fatalf("dry run should plan to replace the corrupt object: %+v %v", dry, err)
	}
	j := f.start(t, StartOptions{Kind: KindMigrate, To: "s3"})
	j, err = f.m.Run(ctx, j.ID)
	if err != nil || j.State != StateCompleted || j.Copied != 12 {
		t.Fatalf("%+v %v", j, err)
	}
	if err = f.readAll(ctx, victim.Digest); err != nil {
		t.Fatal(err)
	}
	rc, _ := f.store("s3", f.blobDom[victim.Digest]).OpenRange(ctx, victim.Digest, 0, -1)
	b, _ := io.ReadAll(rc)
	if !bytes.Equal(b, f.data[victim.Digest]) {
		t.Fatal("corrupt target was not replaced")
	}

	// Clean up, then migrate back with one source blob missing: the job fails and
	// nothing is switched.
	cj := f.start(t, StartOptions{Kind: KindCleanup})
	if _, err = f.m.Run(ctx, cj.ID); err != nil {
		t.Fatal(err)
	}
	gone := f.blobs[7]
	_ = f.store("s3", f.blobDom[gone.Digest]).Delete(ctx, gone.Digest)
	bj := f.start(t, StartOptions{Kind: KindMigrate, To: "disk"})
	bj, err = f.m.Run(ctx, bj.ID)
	if err == nil || bj.State != StateFailed || bj.Failed < 1 {
		t.Fatalf("expected failure: %+v %v", bj, err)
	}
	if rt, _ := f.m.Route(ctx); rt.Active != "s3" || rt.Previous != "" {
		t.Fatalf("switched despite failure: %+v", rt)
	}
}

func TestMigrateRejectsBadRequestsAndConcurrentJobs(t *testing.T) {
	f := newMigFixture(t)
	ctx := context.Background()
	f.seed(t, 3)
	for _, o := range []StartOptions{{Kind: KindMigrate, To: "disk"}, {Kind: KindMigrate, To: "gcs"}, {Kind: "x"}, {Kind: KindMigrate, To: "s3", BytesPerSec: -1}} {
		if _, _, err := f.m.Start(ctx, o); !errors.Is(err, ErrBadTarget) {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	if _, _, err := f.m.Start(ctx, StartOptions{Kind: KindCleanup}); !errors.Is(err, ErrNothingToClean) {
		t.Fatal(err)
	}
	first := f.start(t, StartOptions{Kind: KindMigrate, To: "s3"})
	// Live (fresh heartbeat) job blocks a second start.
	if _, _, err := f.m.Start(ctx, StartOptions{Kind: KindMigrate, To: "s3"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected busy, got %v", err)
	}
	// Pause then resume through the explicit calls.
	if j, err := f.m.Pause(ctx, first.ID); err != nil || j.State != StatePaused {
		t.Fatalf("%+v %v", j, err)
	}
	if _, err := f.m.Resume(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if j, err := f.m.Run(ctx, first.ID); err != nil || j.State != StateCompleted {
		t.Fatalf("%+v %v", j, err)
	}
	// A crashed owner's job (stale heartbeat) is taken over.
	if _, err := f.pool.Exec(ctx, `DELETE FROM storage_active`); err != nil {
		t.Fatal(err)
	}
	crashed := f.start(t, StartOptions{Kind: KindMigrate, To: "s3", DryRun: true})
	if _, err := f.pool.Exec(ctx, `UPDATE storage_migrations SET updated_at=now()-interval '5 minutes' WHERE id=$1`, crashed.ID); err != nil {
		t.Fatal(err)
	}
	took, resumed, err := f.m.Start(ctx, StartOptions{Kind: KindMigrate, To: "s3", DryRun: true})
	if err != nil || !resumed || took.ID != crashed.ID {
		t.Fatalf("takeover %+v %v %v", took, resumed, err)
	}
}

func TestThrottleLimitsRate(t *testing.T) {
	th := &throttle{bps: 100_000}
	start := time.Now()
	for range 5 {
		if err := th.wait(context.Background(), 10_000); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("50KB at 100KB/s took only %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&throttle{bps: 1}).wait(ctx, 1<<20); err == nil {
		t.Fatal("cancelled throttle must stop")
	}
}
