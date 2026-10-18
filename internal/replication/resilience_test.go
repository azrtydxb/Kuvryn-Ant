package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
)

// sameJSON compares two JSON documents irrespective of key order (stored
// idempotent responses are normalized by the database).
func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func (s *site) exporter() Exporter {
	return Exporter{DB: s.pool, Storage: s.stores.Resolve, Lease: s.mgr.Lease}
}

// purgeAll trims every committed change from the source's history.
func (s *site) purgeAll(t *testing.T) PurgeResult {
	t.Helper()
	time.Sleep(15 * time.Millisecond)
	res, err := s.exporter().PurgeHistory(context.Background(), s.instance, PurgePolicy{MaxAge: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHistoryPurgeTriggersAutomaticRestartableReseed(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	for i := range 4 {
		src.publish(src.publisher, src.feed, "n", fmt.Sprintf("n/p%d", i), "1", "f.bin", "stable", data(fmt.Sprintf("old-%d", i)))
	}
	src.publish(src.publisher, src.feed, "n", "n/gone", "1", "gone.bin", "", data("will-vanish"))
	srv := src.serve(t)
	svc := dst.service(t, srv, func(c *Config) { c.BlobConcurrency = 1 })
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	seeded := status(t, svc, dst, job.ID)
	if seeded.Phase != "incremental" || dst.replicaAssets(job) != 5 {
		t.Fatalf("initial seed: %+v", seeded)
	}

	// Destination-only content inside the replica source, plus source changes
	// the job has not consumed: new content and one deletion.
	protected := data("destination-only")
	dst.insertAsset(job.ReplicaSourceID, mustDomain(t, dst, job), "n", "dest/only", "1", "x.bin", protected, "{}")
	for i := 4; i < 7; i++ {
		src.publish(src.publisher, src.feed, "n", fmt.Sprintf("n/p%d", i), "1", "f.bin", "", data(fmt.Sprintf("new-%d", i)))
	}
	src.exec(`DELETE FROM assets WHERE asset_key='gone.bin'`)

	res := src.purgeAll(t)
	if !res.Raised || res.LowWater != int64(src.head()) || res.Deleted == 0 {
		t.Fatalf("purge result %+v head=%d", res, src.head())
	}
	if n := src.count("SELECT count(*) FROM replication_changes"); n != 0 {
		t.Fatalf("%d change rows survived the purge", n)
	}
	if again := src.purgeAll(t); again.Raised || again.LowWater != res.LowWater {
		t.Fatalf("purge must be idempotent and monotonic: %+v", again)
	}

	// The source refuses a cursor it can no longer serve and serves one at the mark.
	ex := src.exporter()
	_, err := ex.Changes(ctx, src.instance, src.feed, ChangeQuery{After: seeded.Checkpoint, Limit: 10, IncludeCached: true})
	var purged *HistoryPurgedError
	if !errors.Is(err, ErrHistoryPurged) || !errors.As(err, &purged) || purged.LowWater != res.LowWater {
		t.Fatalf("stale cursor not refused: %v", err)
	}
	if page, err := ex.Changes(ctx, src.instance, src.feed, ChangeQuery{After: res.LowWater, Limit: 10, IncludeCached: true}); err != nil || len(page.Changes) != 0 || page.LowWater != res.LowWater {
		t.Fatalf("cursor at the low-water mark must be served: %+v %v", page, err)
	}
	if h, err := ex.Head(ctx, src.instance, src.feed); err != nil || h.LowWater != res.LowWater {
		t.Fatalf("head must report the low-water mark: %+v %v", h, err)
	}

	// Interrupt the automatic reseed: it must be durable and restartable.
	srv.mu.Lock()
	srv.failAt = srv.total + 1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected the reseed to be interrupted")
	}
	mid := status(t, svc, dst, job.ID)
	if mid.Phase != "seed_assets" || !mid.Reseeding || mid.Reseeds != 1 || mid.Checkpoint != seeded.Checkpoint {
		t.Fatalf("reseed state not durable: %+v", mid)
	}
	srv.mu.Lock()
	srv.failAt = -1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	done := status(t, svc, dst, job.ID)
	if done.Phase != "incremental" || done.Reseeding || done.Reseeds != 1 || done.Checkpoint != res.LowWater || done.LastError != "" {
		t.Fatalf("reseed did not finish: %+v", done)
	}
	if done.SourceLowWater == nil || *done.SourceLowWater != res.LowWater {
		t.Fatalf("low-water mark not tracked: %+v", done)
	}
	for i := range 7 {
		if got, err := dst.read(dst.feed, "n", fmt.Sprintf("n/p%d", i), "1", "f.bin"); err != nil || len(got) == 0 {
			t.Fatalf("n/p%d missing after reseed: %v", i, err)
		}
	}
	// Dedup: no blob was fetched twice across seed, interrupted reseed and restart.
	if srv.maxPerDigest() != 1 {
		t.Fatalf("blobs re-transferred: %v", srv.served)
	}
	// Never deleting destination-only content, and nothing deleted directly.
	if got, err := dst.read(dst.feed, "n", "dest/only", "1", "x.bin"); err != nil || !bytes.Equal(got, protected) {
		t.Fatalf("destination-only content lost: %v", err)
	}
	if _, err := dst.read(dst.feed, "n", "n/gone", "1", "gone.bin"); err != nil {
		t.Fatalf("an item the peer deleted during the purged window was removed without policy: %v", err)
	}
	if done.DeletionsApplied != 0 || done.DeletionsHeld != 1 {
		t.Fatalf("vanished item must be held, not deleted: %+v", done)
	}
	preview, err := svc.PreviewDeletions(ctx, dst.instance, job.ID)
	if err != nil || preview.Counts["would_delete"] != 1 {
		t.Fatalf("dry run must list the vanished item: %+v %v", preview, err)
	}

	// Deletion policy decides: propagate removes only the vanished replicated item.
	if _, err = svc.UpdateJob(ctx, dst.instance, dst.admin.PrincipalID, job.ID, JobPatch{Revision: job.Revision, DeletionPolicy: ptr("propagate")}); err != nil {
		t.Fatal(err)
	}
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = dst.read(dst.feed, "n", "n/gone", "1", "gone.bin"); err == nil {
		t.Fatal("held deletion not applied under propagate")
	}
	if got, err := dst.read(dst.feed, "n", "dest/only", "1", "x.bin"); err != nil || !bytes.Equal(got, protected) {
		t.Fatalf("destination-only content lost on propagation: %v", err)
	}
	// After the reseed the job follows the source incrementally again.
	src.publish(src.publisher, src.feed, "n", "n/after", "1", "f.bin", "", data("after-reseed"))
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = dst.read(dst.feed, "n", "n/after", "1", "f.bin"); err != nil {
		t.Fatalf("incremental after reseed: %v", err)
	}
	if status(t, svc, dst, job.ID).Reseeds != 1 {
		t.Fatal("job reseeded again without a purge")
	}
}

func TestPurgePolicyBounds(t *testing.T) {
	ctx := context.Background()
	src := newSite(t, "src", "artifacts")
	for i := range 10 {
		src.publish(src.publisher, src.feed, "n", fmt.Sprintf("n/p%d", i), "1", "f.bin", "", data(fmt.Sprintf("p%d", i)))
	}
	ex := src.exporter()
	if _, err := (Exporter{DB: src.pool}).PurgeHistory(ctx, src.instance, PurgePolicy{MaxRows: 1}); err == nil {
		t.Fatal("purging without a writer lease must be refused")
	}
	// Row bound: keep the newest 4 of 10.
	res, err := ex.PurgeHistory(ctx, src.instance, PurgePolicy{MaxRows: 4})
	if err != nil || res.LowWater != 6 || res.Deleted != 6 || src.count("SELECT count(*) FROM replication_changes") != 4 {
		t.Fatalf("row bound: %+v %v", res, err)
	}
	// Age bound never removes rows younger than the cutoff.
	res, err = ex.PurgeHistory(ctx, src.instance, PurgePolicy{MaxAge: time.Hour})
	if err != nil || res.Raised || src.count("SELECT count(*) FROM replication_changes") != 4 {
		t.Fatalf("age bound removed fresh rows: %+v %v", res, err)
	}
	// A consumer at or above the mark is still served; below it is not.
	if _, err = ex.Changes(ctx, src.instance, src.feed, ChangeQuery{After: 6, Limit: 100, IncludeCached: true}); err != nil {
		t.Fatalf("cursor at mark: %v", err)
	}
	if _, err = ex.Changes(ctx, src.instance, src.feed, ChangeQuery{After: 5, Limit: 100, IncludeCached: true}); !errors.Is(err, ErrHistoryPurged) {
		t.Fatalf("cursor below mark: %v", err)
	}
	// New changes keep the sequence gapless after a purge.
	src.publish(src.publisher, src.feed, "n", "n/late", "1", "f.bin", "", data("late"))
	if src.head() != 11 {
		t.Fatalf("head after purge: %d", src.head())
	}
}

func TestSourceAttachedAfterSeedIsBackfilled(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "n", "n/base", "1", "f.bin", "", data("base"))
	// Content that exists before the seed in sources not (yet) attached to the feed.
	extra, extraDomain := src.newSource("extra-local", "local")
	src.insertAsset(extra, extraDomain, "n", "n/extra1", "1", "f.bin", data("extra-1"), "{}")
	src.insertAsset(extra, extraDomain, "n", "n/extra2", "1", "f.bin", data("extra-2"), "{}")
	src.exec(`INSERT INTO tags(component_id,name,version_id) SELECT c.id,'stable',v.id FROM components c JOIN versions v ON v.component_id=c.id WHERE c.source_id=$1 AND c.coordinate='n/extra1'`, extra)
	cache, cacheDomain := src.newSource("late-cache", "remote")
	src.insertAsset(cache, cacheDomain, "", "cached/c", "1", "c.bin", data("cached"), "{}")

	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{IncludeCached: ptr(false)})
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if dst.replicaAssets(job) != 1 {
		t.Fatalf("seed replicated %d assets, want 1", dst.replicaAssets(job))
	}

	// Attaching after the seed: the change feed carries it, the job back-fills.
	src.attach(src.feed, extra, 5)
	src.attach(src.feed, cache, 6)
	if dst.replicaAssets(job) != 1 {
		t.Fatal("nothing may arrive before the job runs")
	}
	// The attachment is a change in the source's history, with its origin.
	page, err := src.exporter().Changes(ctx, src.instance, src.feed, ChangeQuery{After: int64(src.head()) - 2, Limit: 10, IncludeCached: true})
	if err != nil {
		t.Fatal(err)
	}
	attachments := 0
	for _, c := range page.Changes {
		if c.Kind == "attachment" && c.Op == "put" && c.Attachment != nil && c.Attachment.Origin.SourceID == c.SourceID {
			attachments++
		}
	}
	if attachments != 2 {
		t.Fatalf("attachment changes not exported: %+v", page.Changes)
	}
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	st := status(t, svc, dst, job.ID)
	if dst.replicaAssets(job) != 3 || st.BackfillsPending != 0 || st.BackfillsCompleted != 1 {
		t.Fatalf("back-fill incomplete: assets=%d %+v", dst.replicaAssets(job), st)
	}
	for _, c := range []string{"n/extra1", "n/extra2"} {
		if got, err := dst.read(dst.feed, "n", c, "1", "f.bin"); err != nil || len(got) == 0 {
			t.Fatalf("%s not served after back-fill: %v", c, err)
		}
	}
	if n := dst.count(`SELECT count(*) FROM tags t JOIN components c ON c.id=t.component_id WHERE c.source_id=$1 AND t.name='stable'`, job.ReplicaSourceID); n != 1 {
		t.Fatalf("tag of the attached source not back-filled: %d", n)
	}
	if dst.count(`SELECT count(*) FROM components WHERE source_id=$1 AND coordinate='cached/c'`, job.ReplicaSourceID) != 0 {
		t.Fatal("cached source back-filled despite include_cached=false")
	}

	// Detaching changes nothing at the destination and nothing is re-fetched.
	before := srv.successes()
	src.exec(`DELETE FROM feed_sources WHERE source_id=$1`, extra)
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if dst.replicaAssets(job) != 3 || srv.successes() != before {
		t.Fatal("detach must not remove or re-transfer replicated content")
	}
}

func TestBackfillIsRestartable(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "n", "n/base", "1", "f.bin", "", data("base"))
	extra, extraDomain := src.newSource("extra", "local")
	for i := range 5 {
		src.insertAsset(extra, extraDomain, "n", fmt.Sprintf("n/e%d", i), "1", "f.bin", data(fmt.Sprintf("e%d", i)), "{}")
	}
	srv := src.serve(t)
	svc := dst.service(t, srv, func(c *Config) { c.BlobConcurrency = 1 })
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	src.attach(src.feed, extra, 5)
	srv.mu.Lock()
	srv.failAt = srv.total + 2
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected an interrupted back-fill")
	}
	st := status(t, svc, dst, job.ID)
	if st.BackfillsPending != 1 || st.BackfillsCompleted != 0 {
		t.Fatalf("back-fill queue not durable: %+v", st)
	}
	srv.mu.Lock()
	srv.failAt = -1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	st = status(t, svc, dst, job.ID)
	if st.BackfillsPending != 0 || st.BackfillsCompleted != 1 || dst.replicaAssets(job) != 6 || srv.maxPerDigest() != 1 {
		t.Fatalf("restarted back-fill: assets=%d %+v served=%v", dst.replicaAssets(job), st, srv.served)
	}
}

func (s *site) partialPath(domain string, digest blob.Digest) string {
	return filepath.Join(s.dir, "blobs", domain, "staging", "resume-"+string(digest))
}

func TestInterruptedBlobResumesWithoutRetransferringCompletedBytes(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "n", "n/seed", "1", "f.bin", "", data("seed"))
	srv := src.serve(t)
	svc := dst.service(t, srv, func(c *Config) { c.BlobConcurrency = 1 })
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	domain := mustDomain(t, dst, job)
	big := bytes.Repeat([]byte("0123456789abcdef"), 40000) // 640 KB
	bigInfo := digestOf(big)
	src.publish(src.publisher, src.feed, "n", "n/big", "1", "big.bin", "", big)

	// Attempt 1: the connection is cut after 200000 bytes.
	srv.mu.Lock()
	srv.cutAt = 200000
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected an interrupted transfer")
	}
	partial, err := os.Stat(dst.partialPath(domain, bigInfo.Digest))
	if err != nil || partial.Size() == 0 || partial.Size() > 200000 {
		t.Fatalf("partial transfer not staged: %v", err)
	}
	store, _ := dst.stores.Resolve(ctx, domain)
	if _, err = store.Stat(ctx, bigInfo.Digest); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("partial bytes must not be an object: %v", err)
	}
	if n := dst.count(`SELECT count(*) FROM components WHERE source_id=$1 AND coordinate='n/big'`, job.ReplicaSourceID); n != 0 {
		t.Fatal("metadata committed for an incomplete blob")
	}
	staged := partial.Size()

	// Attempt 2: resumes with a Range request from the staged offset.
	srv.mu.Lock()
	srv.cutAt = 0
	firstAttemptBytes := srv.bytesServed[string(bigInfo.Digest)]
	srv.mu.Unlock()
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	ranges := append([]string(nil), srv.ranges...)
	secondAttemptBytes := srv.bytesServed[string(bigInfo.Digest)] - firstAttemptBytes
	srv.mu.Unlock()
	if len(ranges) != 1 || ranges[0] != fmt.Sprintf("bytes=%d-", staged) {
		t.Fatalf("resume did not use a Range request from the staged offset %d: %v", staged, ranges)
	}
	if int64(secondAttemptBytes) != bigInfo.Size-staged {
		t.Fatalf("completed bytes were transferred again: second attempt sent %d, want %d", secondAttemptBytes, bigInfo.Size-staged)
	}
	if got, err := dst.read(dst.feed, "n", "n/big", "1", "big.bin"); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("resumed blob wrong or missing: %v", err)
	}
	if _, err = os.Stat(dst.partialPath(domain, bigInfo.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial left behind after publication: %v", err)
	}
	st := status(t, svc, dst, job.ID)
	if st.ResumedBytes != staged || st.BytesTransferred < bigInfo.Size || st.BytesTransferred > bigInfo.Size+int64(len(data("seed")))+int64(len(data("seed"))) {
		t.Fatalf("resume accounting: resumed=%d bytes=%d staged=%d", st.ResumedBytes, st.BytesTransferred, staged)
	}
}

func TestResumeRejectsCorruptPartialAndHandlesPeersIgnoringRange(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "n", "n/seed", "1", "f.bin", "", data("seed"))
	srv := src.serve(t)
	svc := dst.service(t, srv, func(c *Config) { c.BlobConcurrency = 1 })
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	domain := mustDomain(t, dst, job)

	cut := func(n int) {
		srv.mu.Lock()
		srv.cutAt = n
		srv.mu.Unlock()
	}
	// A staged partial that was corrupted on disk: the final digest check fails,
	// the partial is discarded and nothing becomes visible; the next run starts over.
	one := bytes.Repeat([]byte("corrupt-me-"), 30000)
	oneInfo := digestOf(one)
	src.publish(src.publisher, src.feed, "n", "n/one", "1", "one.bin", "", one)
	cut(100000)
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected an interrupted transfer")
	}
	cut(0)
	path := dst.partialPath(domain, oneInfo.Digest)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatalf("no partial to corrupt: %v", err)
	}
	raw[0] ^= 0xff
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = svc.RunNow(ctx, job.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt partial not rejected: %v", err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt partial kept")
	}
	if dst.count(`SELECT count(*) FROM components WHERE source_id=$1 AND coordinate='n/one'`, job.ReplicaSourceID) != 0 {
		t.Fatal("metadata visible for a corrupt blob")
	}
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatalf("recovery after a discarded partial: %v", err)
	}
	if got, err := dst.read(dst.feed, "n", "n/one", "1", "one.bin"); err != nil || !bytes.Equal(got, one) {
		t.Fatalf("recovered blob wrong: %v", err)
	}

	// A peer that ignores Range sends the whole blob: the partial is discarded
	// and the transfer restarts from zero; the digest still verifies.
	two := bytes.Repeat([]byte("ignore-range-"), 30000)
	twoInfo := digestOf(two)
	src.publish(src.publisher, src.feed, "n", "n/two", "1", "two.bin", "", two)
	cut(100000)
	if err = svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected an interrupted transfer")
	}
	cut(0)
	srv.mu.Lock()
	srv.ignoreRange = true
	srv.mu.Unlock()
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := dst.read(dst.feed, "n", "n/two", "1", "two.bin"); err != nil || !bytes.Equal(got, two) {
		t.Fatalf("blob from a Range-ignoring peer wrong: %v", err)
	}
	if st := status(t, svc, dst, job.ID); st.ResumedBytes != 0 {
		t.Fatalf("nothing was resumed, got %d", st.ResumedBytes)
	}
	_ = twoInfo
}

func TestServerSideChangeFiltering(t *testing.T) {
	ctx := context.Background()
	src := newSite(t, "src", "artifacts")
	src.publish(src.publisher, src.feed, "team", "team/a", "1.0", "a.bin", "", data("a10"))
	src.publish(src.publisher, src.feed, "team", "team/a", "2.0", "a.bin", "", data("a20"))
	src.publish(src.publisher, src.feed, "team", "team/b", "1.1", "b.bin", "", data("b11"))
	src.publish(src.publisher, src.feed, "other", "other/c", "1.0", "c.bin", "", data("c10"))
	src.exec(`DELETE FROM assets WHERE asset_key='b.bin'`)
	ex := src.exporter()
	all := func(f Filters) []Change {
		var out []Change
		var after int64
		for {
			page, err := ex.Changes(ctx, src.instance, src.feed, ChangeQuery{After: after, Limit: 2, IncludeCached: true, Filters: f})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, page.Changes...)
			if !page.More {
				return out
			}
			after = page.Next
		}
	}
	coords := func(cs []Change) map[string]int {
		m := map[string]int{}
		for _, c := range cs {
			m[c.Coordinate+"/"+c.Version+"/"+c.Op]++
		}
		return m
	}
	if n := len(all(Filters{})); n != 5 {
		t.Fatalf("unfiltered: %d changes", n)
	}
	got := all(Filters{Packages: []string{"team/a"}})
	for _, c := range got {
		if c.Coordinate != "team/a" {
			t.Fatalf("package filter leaked %+v", c)
		}
	}
	if len(got) != 2 {
		t.Fatalf("package filter: %v", coords(got))
	}
	got = all(Filters{VersionPrefixes: []string{"1."}})
	for _, c := range got {
		if !strings.HasPrefix(c.Version, "1.") {
			t.Fatalf("version prefix filter leaked %+v", c)
		}
	}
	if len(got) != 4 { // a 1.0 put, b 1.1 put, b 1.1 tombstone, c 1.0 put
		t.Fatalf("version filter: %v", coords(got))
	}
	got = all(Filters{Namespaces: []string{"other"}})
	if len(got) != 1 || got[0].Coordinate != "other/c" || got[0].Asset == nil || got[0].Asset.Namespace == nil || *got[0].Asset.Namespace != "other" {
		t.Fatalf("namespace filter: %+v", got)
	}
	// Tombstones of a filtered-in package pass; of a filtered-out one do not.
	got = all(Filters{Packages: []string{"team/b"}})
	if c := coords(got); c["team/b/1.1/delete"] != 1 || len(got) != 2 {
		t.Fatalf("tombstone filtering: %v", c)
	}
	if got = all(Filters{Packages: []string{"nope"}}); len(got) != 0 {
		t.Fatalf("filter with no match returned %d changes", len(got))
	}
	// The cursor still advances past unmatched changes.
	page, err := ex.Changes(ctx, src.instance, src.feed, ChangeQuery{After: 0, Limit: 2, IncludeCached: true, Filters: Filters{Packages: []string{"nope"}}})
	if err != nil || page.More || page.Next != int64(src.head()) {
		t.Fatalf("cursor stalled: %+v %v", page, err)
	}
	if _, err = ex.Changes(ctx, src.instance, src.feed, ChangeQuery{Filters: Filters{Packages: []string{""}}}); !errors.Is(err, ErrInput) {
		t.Fatalf("invalid filter accepted: %v", err)
	}

	// Over HTTP the peer applies the same filters and the destination follows.
	dst := newSite(t, "dst", "mirror")
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{Filters: &Filters{Namespaces: []string{"team"}, VersionPrefixes: []string{"1."}}})
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	src.publish(src.publisher, src.feed, "team", "team/a", "1.5", "a.bin", "", data("a15"))
	src.publish(src.publisher, src.feed, "team", "team/a", "3.0", "a.bin", "", data("a30"))
	src.publish(src.publisher, src.feed, "other", "other/z", "1.0", "z.bin", "", data("z10"))
	before := srv.successes()
	if err = svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if n := dst.replicaAssets(job); n != 2 || srv.successes()-before != 1 {
		t.Fatalf("filtered incremental: assets=%d blobs fetched=%d", n, srv.successes()-before)
	}
}

// ---- management API: idempotency, rotation, job edit, templates, metrics ----

type apiEnv struct {
	src, dst *site
	srv      *peerServer
	svc      *Service
	url      string
	c        *http.Client
}

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	mux := http.NewServeMux()
	svc.Register(mux, identity.Tokens{DB: dst.pool})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	return &apiEnv{src: src, dst: dst, srv: srv, svc: svc, url: api.URL, c: api.Client()}
}

func (e *apiEnv) do(t *testing.T, method, path, token, key string, body any) (int, []byte) {
	t.Helper()
	return call(t, e.c, method, e.url+path, token, body, "Idempotency-Key", key)
}

func TestManagementMutationsAreIdempotent(t *testing.T) {
	e := newAPIEnv(t)
	adm := e.dst.adminTok
	peerToken := e.src.grantReplicate("peer", e.src.feed)
	in := PeerInput{Name: "site-a", URL: e.srv.URL, CARef: "peer-ca", Token: peerToken}

	if code, _ := call(t, e.c, "POST", e.url+"/api/v1/replication/peers", adm, in, "Idempotency-Key", ""); code != 400 {
		t.Fatalf("mutation without an Idempotency-Key: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/v1/replication/peers", adm, strings.Repeat("k", 257), in); code != 400 {
		t.Fatalf("oversized Idempotency-Key: %d", code)
	}
	code, first := e.do(t, "POST", "/api/v1/replication/peers", adm, "peer-1", in)
	if code != 201 {
		t.Fatalf("create peer: %d %s", code, first)
	}
	code, replay := e.do(t, "POST", "/api/v1/replication/peers", adm, "peer-1", in)
	if code != 201 || !sameJSON(first, replay) {
		t.Fatalf("replay differs: %d %s vs %s", code, replay, first)
	}
	if n := e.dst.count("SELECT count(*) FROM replication_peers"); n != 1 {
		t.Fatalf("replay created %d peers", n)
	}
	other := in
	other.Token = strings.Repeat("z", 24)
	if code, _ = e.do(t, "POST", "/api/v1/replication/peers", adm, "peer-1", other); code != 409 {
		t.Fatalf("same key with a different token: %d", code)
	}
	// Neither the response, the stored result nor its fingerprint reveals the token.
	for _, tok := range []string{peerToken, other.Token} {
		if strings.Contains(string(first), tok) || e.dst.count("SELECT count(*) FROM mutation_results WHERE response::text LIKE '%'||$1||'%'", tok) != 0 {
			t.Fatal("peer token leaked into the idempotency record")
		}
	}
	var peer Peer
	if err := json.Unmarshal(first, &peer); err != nil {
		t.Fatal(err)
	}

	// Job create, patch (revision CAS makes a naive retry fail; the key makes it safe), trigger, delete.
	jin := JobInput{Name: "mirror", PeerID: peer.ID, RemoteFeedID: e.src.feed, DestFeedID: e.dst.feed, Mode: "manual"}
	code, jb := e.do(t, "POST", "/api/v1/replication/jobs", adm, "job-1", jin)
	if code != 201 {
		t.Fatalf("create job: %d %s", code, jb)
	}
	if code, again := e.do(t, "POST", "/api/v1/replication/jobs", adm, "job-1", jin); code != 201 || !sameJSON(again, jb) {
		t.Fatalf("job replay: %d", code)
	}
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs", adm, "job-other", jin); code != 409 {
		t.Fatalf("duplicate job with a new key: %d", code)
	}
	var job Job
	_ = json.Unmarshal(jb, &job)
	patch := JobPatch{Revision: job.Revision, IntervalSeconds: ptr(60)}
	code, p1 := e.do(t, "PATCH", "/api/v1/replication/jobs/"+job.ID, adm, "patch-1", patch)
	if code != 200 {
		t.Fatalf("patch: %d %s", code, p1)
	}
	if code, p2 := e.do(t, "PATCH", "/api/v1/replication/jobs/"+job.ID, adm, "patch-1", patch); code != 200 || !sameJSON(p1, p2) {
		t.Fatalf("patch replay: %d", code)
	}
	if code, _ = e.do(t, "PATCH", "/api/v1/replication/jobs/"+job.ID, adm, "patch-2", patch); code != 409 {
		t.Fatalf("stale revision with a new key must conflict: %d", code)
	}
	if n := e.dst.count("SELECT revision::int FROM replication_jobs WHERE id=$1", job.ID); n != int(job.Revision)+1 {
		t.Fatalf("replayed patch applied twice: revision %d", n)
	}
	for i := range 2 {
		if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/runs", adm, "run-1", nil); code != 202 {
			t.Fatalf("trigger %d: %d", i, code)
		}
	}
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/runs", adm, "", nil); code != 400 {
		t.Fatalf("trigger without key: %d", code)
	}
	for i := range 2 {
		if code, _ = e.do(t, "DELETE", "/api/v1/replication/jobs/"+job.ID, adm, "del-1", nil); code != 204 {
			t.Fatalf("delete job replay %d: %d", i, code)
		}
	}
	if code, _ = e.do(t, "DELETE", "/api/v1/replication/jobs/"+job.ID, adm, "del-2", nil); code != 404 {
		t.Fatalf("delete of a missing job with a new key: %d", code)
	}
	for i := range 2 {
		if code, _ = e.do(t, "DELETE", "/api/v1/replication/peers/"+peer.ID, adm, "del-peer", nil); code != 204 {
			t.Fatalf("delete peer replay %d: %d", i, code)
		}
	}
	// Reads need no key; authorization still applies on replay.
	limited := e.dst.grantReplicate("limited", e.dst.feed)
	if code, _ = e.do(t, "POST", "/api/v1/replication/peers", limited, "peer-1", in); code != 403 {
		t.Fatalf("replay must still authorize: %d", code)
	}
}

func TestPeerTokenRotationAndJobEdit(t *testing.T) {
	e := newAPIEnv(t)
	adm := e.dst.adminTok
	e.src.publish(e.src.publisher, e.src.feed, "n", "n/a", "1", "f.bin", "", data("a"))
	good := e.src.grantReplicate("peer", e.src.feed)
	bad := strings.Repeat("x", 30)
	code, body := e.do(t, "POST", "/api/v1/replication/peers", adm, "p", PeerInput{Name: "site", URL: e.srv.URL, CARef: "peer-ca", Token: bad})
	var peer Peer
	if code != 201 || json.Unmarshal(body, &peer) != nil {
		t.Fatalf("create peer: %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/api/v1/replication/jobs", adm, "j", JobInput{Name: "mirror", PeerID: peer.ID, RemoteFeedID: e.src.feed, DestFeedID: e.dst.feed, Mode: "manual"})
	var job Job
	if code != 201 || json.Unmarshal(body, &job) != nil {
		t.Fatalf("create job: %d %s", code, body)
	}
	if err := e.svc.RunNow(context.Background(), job.ID); err == nil {
		t.Fatal("run with a rejected token must fail")
	}
	if st := status(t, e.svc, e.dst, job.ID); st.FailuresTotal != 1 || st.ConsecutiveFailures != 1 {
		t.Fatalf("failure not counted: %+v", st)
	}
	// Rotation: stale revision conflicts; the new token is stored encrypted and never returned.
	if code, _ = e.do(t, "PATCH", "/api/v1/replication/peers/"+peer.ID, adm, "r0", PeerPatch{Revision: peer.Revision + 3, Token: &good}); code != 409 {
		t.Fatalf("stale rotation: %d", code)
	}
	short := "short"
	if code, _ = e.do(t, "PATCH", "/api/v1/replication/peers/"+peer.ID, adm, "r1", PeerPatch{Revision: peer.Revision, Token: &short}); code != 400 {
		t.Fatalf("weak token accepted: %d", code)
	}
	code, body = e.do(t, "PATCH", "/api/v1/replication/peers/"+peer.ID, adm, "r2", PeerPatch{Revision: peer.Revision, Token: &good})
	var rotated Peer
	if code != 200 || json.Unmarshal(body, &rotated) != nil || rotated.Revision != peer.Revision+1 || strings.Contains(string(body), good) {
		t.Fatalf("rotate: %d %s", code, body)
	}
	if n := e.dst.count("SELECT count(*) FROM encrypted_secrets WHERE envelope::text LIKE '%' || $1 || '%'", good); n != 0 {
		t.Fatal("rotated token stored in plaintext")
	}
	if err := e.svc.RunNow(context.Background(), job.ID); err != nil {
		t.Fatalf("run with the rotated token: %v", err)
	}
	if e.dst.replicaAssets(job) != 1 {
		t.Fatal("rotated token did not replicate")
	}
	if st := status(t, e.svc, e.dst, job.ID); st.ConsecutiveFailures != 0 || st.FailuresTotal != 1 {
		t.Fatalf("status after recovery: %+v", st)
	}

	// Job edit through the API: mode, interval, filters, deletion policy.
	patch := JobPatch{Revision: job.Revision, Mode: ptr("scheduled"), IntervalSeconds: ptr(120), IncludeCached: ptr(false),
		Filters: &Filters{Namespaces: []string{"n"}, VersionPrefixes: []string{"1."}}, DeletionPolicy: ptr("propagate")}
	code, body = e.do(t, "PATCH", "/api/v1/replication/jobs/"+job.ID, adm, "e1", patch)
	var edited Job
	if code != 200 || json.Unmarshal(body, &edited) != nil {
		t.Fatalf("edit job: %d %s", code, body)
	}
	if edited.Mode != "scheduled" || edited.IntervalSeconds != 120 || edited.IncludeCached || edited.DeletionPolicy != "propagate" ||
		len(edited.Filters.Namespaces) != 1 || edited.Filters.VersionPrefixes[0] != "1." || edited.Revision != job.Revision+1 {
		t.Fatalf("edit not applied: %+v", edited)
	}
	if code, _ = e.do(t, "PATCH", "/api/v1/replication/jobs/"+job.ID, adm, "e2", JobPatch{Revision: job.Revision, Mode: ptr("bogus")}); code != 400 && code != 409 {
		t.Fatalf("invalid edit accepted: %d", code)
	}
	if code, _ = e.do(t, "PATCH", "/api/v1/replication/jobs/"+job.ID, adm, "e3", JobPatch{Revision: edited.Revision, IntervalSeconds: ptr(0)}); code != 400 {
		t.Fatalf("invalid interval accepted: %d", code)
	}
}

func TestReplicationMetricsAndStatus(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	for i := range 3 {
		src.publish(src.publisher, src.feed, "n", fmt.Sprintf("n/p%d", i), "1", "f.bin", "", data(fmt.Sprintf("m%d", i)))
	}
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	unregister := svc.RegisterMetrics()
	defer unregister()
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{Name: "metrics-job"})
	srv.mu.Lock()
	srv.failAt = 1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected failure")
	}
	srv.mu.Lock()
	srv.failAt = -1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	src.publish(src.publisher, src.feed, "n", "n/late", "1", "f.bin", "", data("late"))
	time.Sleep(20 * time.Millisecond)
	// Refresh the head without applying: the job is now behind.
	dst.exec(`UPDATE replication_jobs SET remote_head=remote_head+1,remote_head_at=clock_timestamp() WHERE id=$1`, job.ID)

	st := status(t, svc, dst, job.ID)
	if st.FailuresTotal != 1 || st.BytesTransferred == 0 || st.ChangesBehind == nil || *st.ChangesBehind != 1 || st.LagSeconds == nil {
		t.Fatalf("status endpoint values: %+v", st)
	}
	w := httptest.NewRecorder()
	observability.NewHTTP().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	text := w.Body.String()
	for _, want := range []string{
		`ant_replication_failures_total{job="metrics-job"} 1`,
		fmt.Sprintf(`ant_replication_bytes_transferred_total{job="metrics-job"} %d`, st.BytesTransferred),
		`ant_replication_changes_behind{job="metrics-job"} 1`,
		`ant_replication_lag_seconds{job="metrics-job"}`,
		`ant_replication_consecutive_failures{job="metrics-job"} 0`,
		`ant_replication_reseeds_total{job="metrics-job"} 0`,
		"ant_replication_transfer_bytes_total ",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metric %q missing from:\n%s", want, text)
		}
	}
	for _, secret := range []string{srv.URL, src.feed, dst.feed, job.ID} {
		if strings.Contains(text, secret) {
			t.Fatalf("metrics leaked %q", secret)
		}
	}
	unregister()
	w = httptest.NewRecorder()
	observability.NewHTTP().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "ant_replication_") {
		t.Fatal("collector not removed")
	}
}

// ---- configuration templates ------------------------------------------------

func (s *site) remoteSource(feed, name string, priority int, origin string, timeout int, cache remoteconfig.CachePolicy, allowPrivate bool) string {
	s.t.Helper()
	id, _ := s.newSource(name, "remote")
	s.attach(feed, id, priority)
	cfg := remoteconfig.Config{Origin: origin, RedirectOrigins: []string{}, TimeoutSeconds: timeout, Cache: cache, AllowPrivate: allowPrivate}
	b, _ := json.Marshal(cfg)
	s.exec(`INSERT INTO remote_source_configs(instance_id,source_id,config) VALUES($1,$2,$3)`, s.instance, id, b)
	return id
}

func TestConfigurationTemplateExportPreviewApply(t *testing.T) {
	e := newAPIEnv(t)
	src, dst, adm := e.src, e.dst, e.dst.adminTok
	cacheA := remoteconfig.CachePolicy{MetadataTTLSeconds: 600, NegativeTTLSeconds: 60, StaleIfErrorSeconds: 3600, MaxEntries: 5000, MaxBytes: 1 << 26}
	cacheB := remoteconfig.CachePolicy{MetadataTTLSeconds: 300, NegativeTTLSeconds: 30, MaxEntries: 1024, MaxBytes: 16 << 20}

	// Source: retention (with release delete enabled), a remote source, anonymous read.
	src.exec(`UPDATE feeds SET anonymous_read=true WHERE id=$1`, src.feed)
	src.exec(`INSERT INTO retention_policies(instance_id,feed_id,keep_last,max_age_seconds,min_keep,include_tagged,allow_release_delete,enabled) VALUES($1,$2,5,86400,2,true,true,true)`, src.instance, src.feed)
	src.remoteSource(src.feed, "upstream", 5, "https://origin.source.example", 45, cacheA, false)

	// Destination: its own endpoint, trust settings, retention and access settings.
	dst.exec(`UPDATE feeds SET anonymous_read=false WHERE id=$1`, dst.feed)
	dst.exec(`INSERT INTO retention_policies(instance_id,feed_id,keep_last,min_keep,include_tagged,allow_release_delete,enabled) VALUES($1,$2,10,1,false,true,true)`, dst.instance, dst.feed)
	destUp := dst.remoteSource(dst.feed, "upstream", 5, "https://origin.dest.example", 30, cacheB, true)

	peerToken := src.grantReplicate("peer", src.feed)
	code, body := e.do(t, "POST", "/api/v1/replication/peers", adm, "p", PeerInput{Name: "site", URL: e.srv.URL, CARef: "peer-ca", Token: peerToken})
	var peer Peer
	if code != 201 || json.Unmarshal(body, &peer) != nil {
		t.Fatalf("peer: %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/api/v1/replication/jobs", adm, "j", JobInput{Name: "mirror", PeerID: peer.ID, RemoteFeedID: src.feed, DestFeedID: dst.feed, Mode: "manual"})
	var job Job
	if code != 201 || json.Unmarshal(body, &job) != nil {
		t.Fatalf("job: %d %s", code, body)
	}

	var feedRevision int64 // after job creation, which bumps the feed revision itself
	if err := dst.pool.QueryRow(context.Background(), `SELECT revision FROM feeds WHERE id=$1`, dst.feed).Scan(&feedRevision); err != nil {
		t.Fatal(err)
	}

	// Export (source side, peer token): only non-secret, non-destination-specific settings.
	tpl, err := src.exporter().Template(context.Background(), src.instance, src.feed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tpl)
	for _, forbidden := range []string{"origin.source.example", "anonymous_read\":", "allow_release_delete\":", "credential_id", peerToken} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("template leaks %q: %s", forbidden, raw)
		}
	}
	if tpl.Retention == nil || tpl.Retention.KeepLast != 5 || len(tpl.RemoteSources) != 1 || tpl.RemoteSources[0].TimeoutSeconds != 45 || len(tpl.Sources) != 2 {
		t.Fatalf("template content: %s", raw)
	}
	if code, body = call(t, e.srv.Client(), "GET", e.srv.URL+"/api/v1/replication/export/feeds/"+src.feed+"/template", peerToken, nil); code != 200 || !strings.Contains(string(body), `"keep_last":5`) {
		t.Fatalf("template over the export API: %d %s", code, body)
	}
	if code, _ = call(t, e.srv.Client(), "GET", e.srv.URL+"/api/v1/replication/export/feeds/"+src.feed+"/template", "", nil); code != 401 {
		t.Fatalf("unauthenticated template export: %d", code)
	}

	// Preview (destination side), fetching the template from the peer. Read-only.
	var preview TemplatePreview
	code, body = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/preview", adm, "", TemplateRequest{})
	if code != 200 || json.Unmarshal(body, &preview) != nil {
		t.Fatalf("preview: %d %s", code, body)
	}
	find := func(section, field string) *TemplateDiff {
		for i := range preview.Diffs {
			if preview.Diffs[i].Section == section && preview.Diffs[i].Field == field {
				return &preview.Diffs[i]
			}
		}
		return nil
	}
	for section, field := range map[string]string{"retention": "keep_last", "remote_source:upstream": "timeout_seconds"} {
		if d := find(section, field); d == nil || d.Action != TemplateChange {
			t.Fatalf("%s/%s should be a change: %+v", section, field, d)
		}
	}
	for _, sf := range [][2]string{{"retention", "allow_release_delete"}, {"remote_source:upstream", "origin"}, {"remote_source:upstream", "credential"}, {"remote_source:upstream", "ca_ref"},
		{"remote_source:upstream", "allow_private"}, {"feed", "anonymous_read"}, {"feed", "slug"}, {"access", "grants_and_publish_policy"}} {
		if d := find(sf[0], sf[1]); d == nil || d.Action != TemplatePreserved {
			t.Fatalf("%v must be preserved: %+v", sf, d)
		}
	}
	if preview.Blocked != 0 || preview.Applicable == 0 || len(preview.Digest) != 64 || strings.Contains(string(body), "origin.dest.example") || strings.Contains(string(body), "origin.source.example") {
		t.Fatalf("preview summary: %s", body)
	}
	if dst.count(`SELECT keep_last FROM retention_policies WHERE feed_id=$1`, dst.feed) != 10 {
		t.Fatal("preview changed the destination")
	}
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/preview", dst.grantReplicate("limited", dst.feed), "", TemplateRequest{}); code != 404 {
		t.Fatalf("preview without configure: %d", code)
	}

	// Apply: needs the reviewed digest; a wrong or stale digest changes nothing.
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/apply", adm, "a0", TemplateApplyRequest{Digest: strings.Repeat("0", 64)}); code != 409 {
		t.Fatalf("wrong digest: %d", code)
	}
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/apply", adm, "", TemplateApplyRequest{Digest: preview.Digest}); code != 400 {
		t.Fatalf("apply without an Idempotency-Key: %d", code)
	}
	if dst.count(`SELECT keep_last FROM retention_policies WHERE feed_id=$1`, dst.feed) != 10 {
		t.Fatal("a rejected apply changed the destination")
	}
	code, applied := e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/apply", adm, "a1", TemplateApplyRequest{Digest: preview.Digest})
	var result TemplateApplyResult
	if code != 200 || json.Unmarshal(applied, &result) != nil || result.Applied != preview.Applicable || result.Digest != preview.Digest {
		t.Fatalf("apply: %d %s", code, applied)
	}
	var keep, minKeep, timeout int
	var allowDelete, tagged, allowPrivate bool
	var origin string
	if err = dst.pool.QueryRow(context.Background(), `SELECT keep_last,min_keep,include_tagged,allow_release_delete,max_age_seconds::int FROM retention_policies WHERE feed_id=$1`, dst.feed).Scan(&keep, &minKeep, &tagged, &allowDelete, &timeout); err != nil {
		t.Fatal(err)
	}
	if keep != 5 || minKeep != 2 || !tagged || timeout != 86400 {
		t.Fatalf("retention not applied: %d %d %v %d", keep, minKeep, tagged, timeout)
	}
	if !allowDelete {
		t.Fatal("the destination's release-delete switch was overwritten")
	}
	rec, err := remoteconfig.Store{DB: dst.pool, InstanceID: dst.instance}.Load(context.Background(), destUp)
	if err != nil {
		t.Fatal(err)
	}
	origin, allowPrivate = rec.Config.Origin, rec.Config.AllowPrivate
	if rec.Config.TimeoutSeconds != 45 || rec.Config.Cache != cacheA || origin != "https://origin.dest.example" || !allowPrivate {
		t.Fatalf("remote tunables not applied or destination endpoint overwritten: %+v", rec.Config)
	}
	var anon bool
	var slug string
	var rev int64
	if err = dst.pool.QueryRow(context.Background(), `SELECT anonymous_read,slug,revision FROM feeds WHERE id=$1`, dst.feed).Scan(&anon, &slug, &rev); err != nil {
		t.Fatal(err)
	}
	if anon || slug != "mirror" || rev != feedRevision {
		t.Fatalf("destination feed settings changed: anon=%v slug=%s rev=%d/%d", anon, slug, rev, feedRevision)
	}
	if dst.count(`SELECT count(*) FROM audit_events WHERE action='replication.template.apply'`) != 1 {
		t.Fatal("apply not audited")
	}

	// Replay with the same key returns the stored result without applying again.
	dst.exec(`UPDATE retention_policies SET keep_last=99 WHERE feed_id=$1`, dst.feed)
	if code, again := e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/apply", adm, "a1", TemplateApplyRequest{Digest: preview.Digest}); code != 200 || !sameJSON(again, applied) {
		t.Fatalf("replay: %d %s", code, again)
	}
	if dst.count(`SELECT keep_last FROM retention_policies WHERE feed_id=$1`, dst.feed) != 99 {
		t.Fatal("replay applied the template again")
	}
	// A new request with the old digest is stale: the destination changed since the review.
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/apply", adm, "a2", TemplateApplyRequest{Digest: preview.Digest}); code != 409 {
		t.Fatalf("stale review applied: %d", code)
	}

	// A template that cannot be applied completely is refused, not partially adopted.
	missing := tpl
	missing.RemoteSources = append([]TemplateRemote{{Name: "no-such-source", TimeoutSeconds: 10, Cache: cacheA}}, tpl.RemoteSources...)
	code, body = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/preview", adm, "", TemplateRequest{Template: &missing})
	var blocked TemplatePreview
	if code != 200 || json.Unmarshal(body, &blocked) != nil || blocked.Blocked != 1 {
		t.Fatalf("blocked preview: %d %s", code, body)
	}
	dst.exec(`UPDATE retention_policies SET keep_last=7 WHERE feed_id=$1`, dst.feed)
	code, body = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/preview", adm, "", TemplateRequest{Template: &missing})
	_ = json.Unmarshal(body, &blocked)
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/apply", adm, "a3", TemplateApplyRequest{Digest: blocked.Digest, Template: &missing}); code != 409 {
		t.Fatalf("blocked template applied: %d", code)
	}
	if dst.count(`SELECT keep_last FROM retention_policies WHERE feed_id=$1`, dst.feed) != 7 {
		t.Fatal("a blocked template partially applied")
	}
	// Format mismatch blocks everything.
	wrong := tpl
	wrong.Format = "maven"
	code, body = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/preview", adm, "", TemplateRequest{Template: &wrong})
	_ = json.Unmarshal(body, &blocked)
	if code != 200 || blocked.Blocked != 1 || blocked.Applicable != 0 {
		t.Fatalf("format mismatch: %d %s", code, body)
	}
	if code, _ = e.do(t, "POST", "/api/v1/replication/jobs/"+job.ID+"/template/preview", adm, "", TemplateRequest{Template: &Template{Version: 9}}); code != 400 {
		t.Fatalf("invalid template accepted: %d", code)
	}
}
