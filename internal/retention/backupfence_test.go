package retention

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backup"
	"github.com/azrtydxb/Kuvryn-Ant/internal/backupfence"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// expedite queues the unreferenced blob for an immediate sweep, as a cache
// supersede plus the short sweep grace would after the wait.
func (f *fixture) expedite(t *testing.T, digest blob.Digest) {
	t.Helper()
	f.exec(t, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind,sweep_after,first_seen_at) SELECT b.instance_id,b.dedup_domain_id,b.digest,'row',now()-interval '1 second',now()-interval '1 minute' FROM blobs b WHERE b.digest=$1 ON CONFLICT(dedup_domain_id,digest) DO UPDATE SET sweep_after=excluded.sweep_after,first_seen_at=excluded.first_seen_at`, string(digest))
}

func swept(rep GCReport, d blob.Digest) bool {
	for _, s := range rep.Swept {
		if s.Digest == string(d) {
			return true
		}
	}
	return false
}

type hookStores struct {
	f    *fixture
	once sync.Once
	on   func()
}

func (h *hookStores) Resolve(context.Context, string) (blob.Store, error) {
	h.once.Do(h.on)
	return h.f.disk, nil
}

// The incident: a backup snapshot lists a cache blob, the cache tag is then
// overwritten and the expedited sweep runs while the backup is still copying.
// The fence keeps the bytes; once the backup is done the next sweep frees them.
func TestBackupFenceKeepsSnapshotBlobsFromExpeditedSweep(t *testing.T) {
	f := newFixture(t, Options{Grace: 24 * time.Hour, PressureSweepGrace: time.Minute})
	ctx := context.Background()
	f.anyDomain()
	_, cacheSource, cacheDomain := f.cacheFeed(t, "buildcache", time.Hour, 0)
	cached := f.seedSized(t, cacheSource, cacheDomain, "repo", "v1", sized{size: 100, idleHours: 0, ageHours: 1, tag: "main"})
	f.publish(t, "1.0", []byte("a live release artifact"))
	live := sum([]byte("a live release artifact"))

	var during GCReport
	var duringErr error
	var cachedBytesDuring, liveBytesDuring bool
	hook := &hookStores{f: f}
	hook.on = func() {
		// Overwrite the cache tag: the old version and its blob reference go away,
		// and the blob becomes an expedited sweep candidate.
		f.exec(t, `DELETE FROM tags WHERE version_id IN (SELECT id FROM versions WHERE version='v1')`)
		f.exec(t, `DELETE FROM assets WHERE version_id IN (SELECT id FROM versions WHERE version='v1')`)
		f.exec(t, `DELETE FROM versions WHERE version='v1'`)
		f.expedite(t, cached.Digest)
		during, duringErr = f.svc.CollectGarbage(ctx, false)
		cachedBytesDuring, liveBytesDuring = f.hasBytes(cached.Digest), f.hasBytes(live.Digest)
	}
	dir := filepath.Join(t.TempDir(), "bk")
	res, err := backup.Create(ctx, f.pool, hook, dir, backup.Options{})
	if err != nil {
		t.Fatalf("backup must succeed: %v", err)
	}
	if duringErr != nil || during.Fenced != 1 || swept(during, cached.Digest) || !cachedBytesDuring || !liveBytesDuring {
		t.Fatalf("sweep ran under the fence: %+v %v cached=%v live=%v", during, duringErr, cachedBytesDuring, liveBytesDuring)
	}
	if res.Manifest.MissingCount() != 0 || len(res.Warnings) != 0 {
		t.Fatalf("nothing may be missing: %+v", res.Warnings)
	}
	if _, err = backup.Verify(ctx, dir, res.ManifestSHA256, 2); err != nil {
		t.Fatalf("backup incomplete: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM gc_candidates`); n != 1 {
		t.Fatalf("fenced candidate must stay marked: %d", n)
	}

	after, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || after.Fenced != 0 || !swept(after, cached.Digest) || f.hasBytes(cached.Digest) {
		t.Fatalf("the next sweep after release must free the blob: %+v %v", after, err)
	}
	if !f.hasBytes(live.Digest) {
		t.Fatal("live blob was swept")
	}
}

func TestExpeditedSweepPausedByFenceAndResumesWhenItExpires(t *testing.T) {
	f := newFixture(t, Options{Grace: 24 * time.Hour, PressureSweepGrace: time.Minute})
	ctx := context.Background()
	orphan := f.orphan(t, "1.0", []byte("freed blob"))
	f.expedite(t, orphan.Digest)

	_, fence, err := backupfence.Acquire(ctx, f.pool, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Release()
	var pr PressureReport
	if err = f.svc.sweepExpedited(ctx, &pr); err != nil || pr.Swept != 0 || !f.hasBytes(orphan.Digest) {
		t.Fatalf("expedited sweep must pause under a fence: %+v %v", pr, err)
	}
	// The holder crashed: no heartbeat, so the fence runs out.
	f.exec(t, `UPDATE backup_fences SET expires_at=now()-interval '1 second'`)
	if err = f.svc.sweepExpedited(ctx, &pr); err != nil || pr.Swept != 1 || f.hasBytes(orphan.Digest) {
		t.Fatalf("an expired fence must not block sweeping: %+v %v", pr, err)
	}
}

// Release removes the fence at once, and an in-flight fence does not stop
// marking, only byte deletion.
func TestFenceDoesNotStopMarking(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	orphan := f.orphan(t, "1.0", []byte("to be marked"))
	f.ageBlobs(t, 2*time.Hour)
	_, fence, err := backupfence.Acquire(ctx, f.pool, "test")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Marked) != 1 || rep.Marked[0].Digest != string(orphan.Digest) {
		t.Fatalf("marking must continue under a fence: %+v %v", rep, err)
	}
	fence.Release()
	if live, err := backupfence.Live(ctx, f.pool); err != nil || live {
		t.Fatalf("release must clear the fence: %v %v", live, err)
	}
}
