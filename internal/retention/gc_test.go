package retention

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

func TestOrphanRowGraceTwoPhaseAndDryRunAccuracy(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	info := f.orphan(t, "1.0", []byte("orphaned release bytes"))

	// A fresh unreferenced blob is protected by the grace period (in-flight publish).
	rep, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Marked) != 0 || len(rep.Swept) != 0 {
		t.Fatalf("grace not honored: %+v %v", rep, err)
	}
	f.ageBlobs(t, 2*time.Hour)

	// Dry-run reports the mark but writes nothing.
	dry, err := f.svc.CollectGarbage(ctx, true)
	if err != nil || len(dry.Marked) != 1 || dry.Marked[0].Digest != string(info.Digest) || dry.Marked[0].Kind != "row" {
		t.Fatalf("dry-run mark: %+v %v", dry, err)
	}
	if f.count(t, "SELECT count(*) FROM gc_candidates") != 0 {
		t.Fatal("dry-run persisted a candidate")
	}
	rep, err = f.svc.CollectGarbage(ctx, false)
	if err != nil || !reflect.DeepEqual(rep.Marked, dry.Marked) || len(rep.Swept) != 0 {
		t.Fatalf("mark phase differs from dry-run: %+v %v", rep, err)
	}
	// Marked but too young to sweep: the mark must age for the grace period.
	rep, err = f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Swept) != 0 || !f.hasBytes(info.Digest) {
		t.Fatalf("swept a young candidate: %+v %v", rep, err)
	}
	f.ageCandidates(t, 2*time.Hour)
	dry, err = f.svc.CollectGarbage(ctx, true)
	if err != nil || len(dry.Swept) != 1 || !f.hasBytes(info.Digest) || f.count(t, "SELECT count(*) FROM blobs") != 1 {
		t.Fatalf("dry-run sweep must not delete: %+v %v", dry, err)
	}
	rep, err = f.svc.CollectGarbage(ctx, false)
	if err != nil || !reflect.DeepEqual(rep.Swept, dry.Swept) {
		t.Fatalf("sweep differs from dry-run: %+v vs %+v %v", rep.Swept, dry.Swept, err)
	}
	if f.hasBytes(info.Digest) || f.count(t, "SELECT count(*) FROM blobs") != 0 || f.count(t, "SELECT count(*) FROM gc_candidates") != 0 {
		t.Fatal("sweep left bytes, row or candidate behind")
	}
}

func TestMarkAndSweepNeverShareARun(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Nanosecond})
	ctx := context.Background()
	info := f.orphan(t, "1.0", []byte("same-run bytes"))
	time.Sleep(10 * time.Millisecond)
	rep, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Marked) != 1 || len(rep.Swept) != 0 || !f.hasBytes(info.Digest) {
		t.Fatalf("one run marked and swept: %+v %v", rep, err)
	}
	rep, err = f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Swept) != 1 || f.hasBytes(info.Digest) {
		t.Fatalf("second run must sweep: %+v %v", rep, err)
	}
}

func TestReferencedBlobIsNeverMarkedAndLateReferenceUnmarks(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Nanosecond})
	ctx := context.Background()
	live := []byte("live release")
	f.publish(t, "1.0", live)
	f.ageBlobs(t, 100*time.Hour)
	for range 3 {
		rep, err := f.svc.CollectGarbage(ctx, false)
		if err != nil || len(rep.Marked) != 0 || len(rep.Swept) != 0 {
			t.Fatalf("referenced blob touched: %+v %v", rep, err)
		}
	}
	// Orphan, mark, then re-reference by republishing the same bytes.
	data := []byte("revived bytes")
	info := f.orphan(t, "2.0", data)
	f.ageBlobs(t, 100*time.Hour)
	if _, err := f.svc.CollectGarbage(ctx, false); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM gc_candidates") != 1 {
		t.Fatal("expected one candidate")
	}
	f.publish(t, "3.0", data)
	f.ageCandidates(t, time.Hour)
	rep, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Swept) != 0 || rep.Unmarked != 1 || !f.hasBytes(info.Digest) || f.count(t, "SELECT count(*) FROM gc_candidates") != 0 {
		t.Fatalf("re-referenced blob not rescued: %+v %v", rep, err)
	}
}

func TestStrayObjectsFromFailedCommitsAreCollected(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour, ObjectScanLimit: 1})
	ctx := context.Background()
	var stray []blob.Info
	for _, body := range []string{"stray one", "stray two"} {
		info := sum([]byte(body))
		if _, err := f.disk.Put(ctx, bytes.NewReader([]byte(body)), info); err != nil {
			t.Fatal(err)
		}
		stray = append(stray, info)
	}
	f.publish(t, "1.0", []byte("referenced"))
	// Fresh objects are inside the grace period: an upload may still be committing.
	for range 3 {
		rep, err := f.svc.CollectGarbage(ctx, false)
		if err != nil || len(rep.Marked) != 0 {
			t.Fatalf("fresh object marked: %+v %v", rep, err)
		}
	}
	old := time.Now().Add(-3 * time.Hour)
	entries, _ := os.ReadDir(filepath.Join(f.root, "objects"))
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(f.root, "objects", e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// ObjectScanLimit=1 makes the scan resumable across runs; wrap-around included.
	marked := map[string]string{}
	for range 6 {
		rep, err := f.svc.CollectGarbage(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for d, k := range digests(rep.Marked) {
			marked[d] = k
		}
	}
	if len(marked) != 2 {
		t.Fatalf("marked %v", marked)
	}
	for _, s := range stray {
		if marked[string(s.Digest)] != "object" {
			t.Fatalf("stray %s not marked as object", s.Digest)
		}
	}
	f.ageCandidates(t, 2*time.Hour)
	rep, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Swept) != 2 {
		t.Fatalf("stray sweep: %+v %v", rep, err)
	}
	for _, s := range stray {
		if f.hasBytes(s.Digest) {
			t.Fatal("stray bytes survived")
		}
	}
	if !f.hasBytes(sum([]byte("referenced")).Digest) || f.count(t, "SELECT count(*) FROM blobs") != 1 {
		t.Fatal("referenced content damaged")
	}
	if f.count(t, "SELECT count(*) FROM blobs WHERE size_bytes=0") != 0 {
		t.Fatal("placeholder row leaked")
	}
}

// insertAsset references blobID from a fresh version inside tx, exactly like the
// tail of artifact.commitAsset.
func (f *fixture) insertAsset(ctx context.Context, tx pgx.Tx, info blob.Info, version string) error {
	var blobID, component, versionID string
	if err := tx.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text`, f.instance, f.domain, info.Digest, info.Size).Scan(&blobID); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'race/pkg') ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate RETURNING id::text`, f.instance, f.source).Scan(&component); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) RETURNING id::text`, f.instance, component, version).Scan(&versionID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'a',$3)`, f.instance, versionID, blobID)
	return err
}

// prepareCandidate leaves an old, marked, sweep-eligible orphan.
func (f *fixture) prepareCandidate(t *testing.T, version string, data []byte) blob.Info {
	t.Helper()
	info := f.orphan(t, version, data)
	f.ageBlobs(t, 100*time.Hour)
	if _, err := f.svc.CollectGarbage(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.ageCandidates(t, 100*time.Hour)
	return info
}

func TestSweepWaitsForConcurrentPublishAndRechecksReference(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	info := f.prepareCandidate(t, "1.0", []byte("raced bytes"))

	// A publisher upserts the blob row and asset but has not yet committed.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = f.insertAsset(ctx, tx, info, "9.9"); err != nil {
		t.Fatal(err)
	}
	type result struct {
		rep GCReport
		err error
	}
	done := make(chan result, 1)
	go func() { rep, err := f.svc.CollectGarbage(ctx, false); done <- result{rep, err} }()
	select {
	case r := <-done:
		t.Fatalf("sweep did not wait for the in-flight publish: %+v %v", r.rep, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || len(r.rep.Swept) != 0 || r.rep.SkippedReferenced != 1 {
		t.Fatalf("sweep must abort on the new reference: %+v %v", r.rep, r.err)
	}
	if !f.hasBytes(info.Digest) || f.count(t, "SELECT count(*) FROM assets WHERE version_id IN (SELECT id FROM versions WHERE version='9.9')") != 1 {
		t.Fatal("re-referenced blob lost its bytes")
	}
}

// Publishers that Stat the blob after upserting its row inside their transaction
// (the documented contract) never commit metadata for swept bytes, however the
// publish and the sweep interleave.
func TestConcurrentPublishAndSweepNeverCommitMissingBytes(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	var committed, rejected, swept int
	for i := range 80 {
		data := []byte(fmt.Sprintf("race iteration %d", i))
		info := f.prepareCandidate(t, fmt.Sprintf("o%d", i), data)
		var wg sync.WaitGroup
		var rep GCReport
		var sweepErr error
		var publishErr error
		var ok bool
		// Sweep jitter spans the interval in which publish commits; vary both sides.
		sweepDelay := time.Duration(rand.IntN(6000)) * time.Microsecond
		publishDelay := time.Duration(rand.IntN(3000)) * time.Microsecond
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(sweepDelay)
			rep, sweepErr = f.svc.CollectGarbage(ctx, false)
		}()
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(publishDelay)
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				publishErr = err
				return
			}
			defer tx.Rollback(ctx)
			if err = f.insertAsset(ctx, tx, info, fmt.Sprintf("p%d", i)); err != nil {
				publishErr = err
				return
			}
			if _, err = f.disk.Stat(ctx, info.Digest); err != nil {
				return // bytes already swept: the publish fails and is retried by the client
			}
			if publishErr = tx.Commit(ctx); publishErr == nil {
				ok = true
			}
		}()
		close(start)
		wg.Wait()
		if sweepErr != nil || publishErr != nil {
			t.Fatalf("iteration %d: sweep=%v publish=%v", i, sweepErr, publishErr)
		}
		swept += len(rep.Swept)
		if ok {
			committed++
			if !f.hasBytes(info.Digest) {
				t.Fatalf("iteration %d: committed reference to deleted bytes", i)
			}
		} else {
			rejected++
		}
	}
	t.Logf("publish won %d, sweep won %d (swept %d)", committed, rejected, swept)
}

func TestSweepFailsClosedWithoutLease(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	info := f.prepareCandidate(t, "1.0", []byte("fenced bytes"))
	lost := f.lease
	f.svc.Lease = func() ownership.Lease { lost.Epoch++; return lost }
	if _, err := f.svc.CollectGarbage(context.Background(), false); !errors.Is(err, ownership.ErrLost) {
		t.Fatalf("expected lost lease, got %v", err)
	}
	if !f.hasBytes(info.Digest) {
		t.Fatal("deleted under a lost lease")
	}
	// Dry-run is read-only and needs no lease.
	if _, err := f.svc.CollectGarbage(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

type abortingStore struct {
	blob.Store
	calls []bool
	age   time.Duration
}

func (a *abortingStore) AbortStaleUploads(_ context.Context, olderThan time.Duration, dryRun bool) (int, error) {
	a.calls = append(a.calls, dryRun)
	a.age = olderThan
	return 3, nil
}

func TestGCAbortsStaleMultipartUploads(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour, StaleUploadAge: 6 * time.Hour})
	store := &abortingStore{Store: f.disk}
	f.svc.Stores = func(context.Context, string) (blob.Store, error) { return store, nil }
	ctx := context.Background()
	rep, err := f.svc.CollectGarbage(ctx, true)
	if err != nil || rep.StaleUploadsAborted != 3 || store.age != 6*time.Hour {
		t.Fatalf("dry run: %+v %v age=%v", rep, err, store.age)
	}
	rep, err = f.svc.CollectGarbage(ctx, false)
	if err != nil || rep.StaleUploadsAborted != 3 {
		t.Fatalf("real run: %+v %v", rep, err)
	}
	if len(store.calls) < 2 || !store.calls[0] || store.calls[len(store.calls)-1] {
		t.Fatalf("dry-run flag not propagated: %v", store.calls)
	}
}
