package retention

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// cacheFeed creates a cache feed with one local source (feed purpose cache).
func (f *fixture) cacheFeed(t *testing.T, slug string, maxIdle time.Duration, maxBytes int64) (feed, source, domain string) {
	t.Helper()
	feed, source, domain = f.newSource(t, slug, "local")
	var limit any
	if maxBytes > 0 {
		limit = maxBytes
	}
	f.exec(t, `UPDATE feeds SET purpose='cache',cache_max_idle_seconds=$2,cache_max_bytes=$3 WHERE id=$1`, feed, int64(maxIdle/time.Second), limit)
	return
}

func deleted(r RetentionReport) []string {
	var out []string
	for _, d := range r.Deleted {
		out = append(out, d.Coordinate+"@"+d.Version+":"+d.Reason)
	}
	slices.Sort(out)
	return out
}

// Cache content is evictable by retention without allow_release_delete, tagged
// versions and the newest version are not exempt, retention policies are
// ignored for it, and the same data in an ordinary feed stays protected.
func TestCacheContentIsEvictableWithoutReleaseDelete(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour, PressureSweepGrace: time.Minute})
	ctx := context.Background()
	f.anyDomain()
	_, cacheSource, cacheDomain := f.cacheFeed(t, "buildcache", time.Hour, 0)
	f.seedSized(t, cacheSource, cacheDomain, "repo", "old", sized{size: 100, idleHours: 5, ageHours: 6, tag: "main"})
	f.seedSized(t, cacheSource, cacheDomain, "repo", "fresh", sized{size: 100, idleHours: 0, ageHours: 6})
	// An ordinary release feed with an equivalent max-idle policy and the same data.
	relFeed, relSource, relDomain := f.newSource(t, "releases", "local")
	f.seedSized(t, relSource, relDomain, "repo", "old", sized{size: 100, idleHours: 5, ageHours: 6})
	f.seedSized(t, relSource, relDomain, "repo", "newer", sized{size: 100, idleHours: 5, ageHours: 5})
	if _, err := f.svc.SetPolicy(ctx, Policy{FeedID: relFeed, MaxIdle: time.Hour, MinKeep: 0, IncludeTagged: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// A policy on the cache feed is ignored: the cache policy decides.
	var cacheFeedID string
	f.exec(t, `UPDATE feeds SET cache_max_idle_seconds=3600 WHERE slug='buildcache'`)
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM feeds WHERE slug='buildcache'`).Scan(&cacheFeedID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetPolicy(ctx, Policy{FeedID: cacheFeedID, KeepLast: 100, MinKeep: 5, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	dry, err := f.svc.ApplyRetention(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := deleted(dry), []string{"repo@old:cache_idle"}; !slices.Equal(got, want) {
		t.Fatalf("dry-run deleted %v, want %v (release content must stay protected: %+v)", got, want, dry)
	}
	if f.count(t, "SELECT count(*) FROM versions") != 4 {
		t.Fatal("dry-run modified data")
	}
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleted(rep); len(got) != 1 || got[0] != "repo@old:cache_idle" {
		t.Fatalf("deleted %v", got)
	}
	if f.count(t, "SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1", cacheSource) != 1 ||
		f.count(t, "SELECT count(*) FROM tags") != 0 {
		t.Fatal("the idle tagged version was not evicted")
	}
	if f.count(t, "SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1", relSource) != 2 {
		t.Fatal("release content was evicted without allow_release_delete")
	}
	// Freed blobs wait only the short pressure grace, not the full one.
	var shortWait float64
	if err = f.pool.QueryRow(ctx, `SELECT extract(epoch FROM (sweep_after-first_seen_at)) FROM gc_candidates`).Scan(&shortWait); err != nil {
		t.Fatal(err)
	}
	if shortWait < 30 || shortWait > 120 {
		t.Fatalf("freed blob waits %.0fs, want about the 60s pressure grace", shortWait)
	}
}

// Idle is the latest of created, last accessed and last downloaded.
func TestCacheIdleIsLatestOfCreatedAccessedDownloaded(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	f.anyDomain()
	_, source, domain := f.cacheFeed(t, "sccache", 2*time.Hour, 0)
	f.seedSized(t, source, domain, "idle", "1", sized{size: 50, idleHours: -1, ageHours: 5})       // created 5h ago, never used
	f.seedSized(t, source, domain, "accessed", "1", sized{size: 50, idleHours: 0, ageHours: 5})    // read just now
	f.seedSized(t, source, domain, "downloaded", "1", sized{size: 50, idleHours: -1, ageHours: 5}) // downloaded just now
	f.seedSized(t, source, domain, "young", "1", sized{size: 50, idleHours: -1, ageHours: 1})      // created within max_idle
	f.exec(t, `INSERT INTO asset_downloads(asset_id,instance_id,downloads,last_downloaded_at) SELECT a.id,a.instance_id,3,now() FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.coordinate='downloaded'`)
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleted(rep); len(got) != 1 || got[0] != "idle@1:cache_idle" {
		t.Fatalf("deleted %v", got)
	}
	// The emptied component is removed with its last version.
	if f.count(t, "SELECT count(*) FROM components WHERE coordinate='idle'") != 0 || f.count(t, "SELECT count(*) FROM components") != 3 {
		t.Fatal("component of an evicted raw file remained")
	}
}

// max_bytes evicts least recently used content until the feed is under it.
func TestCacheMaxBytesEvictsLeastRecentlyUsed(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	f.anyDomain()
	_, source, domain := f.cacheFeed(t, "sccache", 30*24*time.Hour, 250)
	// Five 100-byte files, used 50h, 40h, 30h, 20h and 10h ago: 500 bytes, limit 250.
	for i := range 5 {
		f.seedSized(t, source, domain, fmt.Sprintf("k%d", i), "1", sized{size: 100, idleHours: 50 - 10*i, ageHours: 60})
	}
	dry, err := f.svc.ApplyRetention(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := deleted(dry); len(got) != 3 {
		t.Fatalf("dry-run estimate: %v", got)
	}
	if f.count(t, "SELECT count(*) FROM versions") != 5 {
		t.Fatal("dry-run modified data")
	}
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"k0@1:cache_max_bytes", "k1@1:cache_max_bytes", "k2@1:cache_max_bytes"}
	if got := deleted(rep); !slices.Equal(got, want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	var left int64
	if err = f.pool.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0) FROM assets a JOIN blobs b ON b.id=a.blob_id`).Scan(&left); err != nil || left != 200 {
		t.Fatalf("feed holds %d bytes (%v), want 200", left, err)
	}
	// Under the limit: nothing more happens.
	again, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || len(again.Deleted) != 0 {
		t.Fatalf("second pass %v %v", again.Deleted, err)
	}
}

// Under storage pressure cache feeds go first, least recently used first,
// before any remote-cached content, even when the remote content is older.
func TestPressureEvictsCacheFeedsBeforeRemoteContent(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour, PressureSweepGrace: time.Minute})
	ctx := context.Background()
	_, remote, remoteDomain := f.newSource(t, "mirror", "remote")
	_, cache, cacheDomain := f.cacheFeed(t, "buildcache", 30*24*time.Hour, 0)
	// Remote cache content: six 100-byte versions, all very old.
	for i := range 3 {
		f.seedSized(t, remote, remoteDomain, fmt.Sprintf("pkg/r%d", i), "v1", sized{size: 100, idleHours: 500 - i, ageHours: 600})
		f.seedSized(t, remote, remoteDomain, fmt.Sprintf("pkg/r%d", i), "v2", sized{size: 100, idleHours: 400, ageHours: 600})
	}
	// Cache feed content: six 100-byte files used much more recently than the remote content.
	for i := range 6 {
		f.seedSized(t, cache, cacheDomain, fmt.Sprintf("layer%d", i), "1", sized{size: 100, idleHours: 10 + i, ageHours: 20})
	}
	// 1200 used of 2000; high 50% = 1000, low 40% = 800: 400 bytes must go.
	f.pressureOn(t, 2000, 50, 40)
	dry, err := f.svc.RunPressure(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"layer2@1", "layer3@1", "layer4@1", "layer5@1"} // the four least recently used cache files
	if got := evicted(dry); !slices.Equal(got, want) {
		t.Fatalf("dry-run evicts %v, want %v", got, want)
	}
	rep, err := f.svc.RunPressure(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := evicted(rep); !slices.Equal(got, want) || rep.EvictedBytes != 400 || rep.ShortfallBytes != 0 {
		t.Fatalf("evicted %v bytes=%d shortfall=%d (%s)", got, rep.EvictedBytes, rep.ShortfallBytes, rep.Detail)
	}
	if f.count(t, "SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1", remote) != 6 {
		t.Fatal("remote content was evicted before the cache feed was exhausted")
	}
	// A heavier overshoot spills over to remote content after the cache is gone.
	f.pressureOn(t, 1000, 50, 20)
	rep, err = f.svc.RunPressure(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1", cache) != 0 {
		t.Fatalf("cache feed not exhausted first: %v", evicted(rep))
	}
	if f.count(t, "SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1", remote) >= 6 {
		t.Fatal("remote content was not evicted once the cache feed was empty")
	}
}

// The cache tick evicts idle content and sweeps the freed blobs after the
// short wait only; the sweep protocol (row lock, reference re-check) is the
// ordinary one, so a blob that regained a reference is kept.
func TestCacheTickSweepsFreedBlobsAfterShortWait(t *testing.T) {
	// A long short-sweep wait: the first tick evicts and queues, but must not sweep yet.
	f := newFixture(t, Options{Grace: 24 * time.Hour, PressureSweepGrace: time.Hour})
	ctx := context.Background()
	f.anyDomain()
	_, source, domain := f.cacheFeed(t, "sccache", time.Hour, 0)
	gone := f.seedSized(t, source, domain, "gone", "1", sized{size: 80, idleHours: 3, ageHours: 4})
	kept := f.seedSized(t, source, domain, "kept", "1", sized{size: 90, idleHours: 3, ageHours: 4})
	rep, err := f.svc.CacheTick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Evicted) != 2 || rep.Swept != 0 {
		t.Fatalf("first tick: evicted %v swept %d", rep.Evicted, rep.Swept)
	}
	if !f.hasBytes(gone.Digest) || !f.hasBytes(kept.Digest) {
		t.Fatal("bytes were deleted inline")
	}
	if f.count(t, "SELECT count(*) FROM gc_candidates WHERE sweep_after IS NOT NULL") != 2 {
		t.Fatal("freed blobs were not queued for the short sweep")
	}
	// Before the wait is over, a tick sweeps nothing.
	if rep, err = f.svc.CacheTick(ctx); err != nil || rep.Swept != 0 {
		t.Fatalf("tick before the short wait: swept %d (%v)", rep.Swept, err)
	}
	// The wait elapses; meanwhile a republish re-references the "kept" blob.
	f.exec(t, `UPDATE gc_candidates SET first_seen_at=first_seen_at-interval '2 hours',sweep_after=sweep_after-interval '2 hours'`)
	f.seedSized(t, source, domain, "kept", "again", sized{size: 90, idleHours: 0, ageHours: 0})
	f.exec(t, `UPDATE assets SET blob_id=(SELECT id FROM blobs WHERE digest=$1) WHERE version_id IN (SELECT v.id FROM versions v WHERE v.version='again')`, string(kept.Digest))
	if rep, err = f.svc.CacheTick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.hasBytes(gone.Digest) {
		t.Fatal("freed blob not swept after the short wait")
	}
	if !f.hasBytes(kept.Digest) {
		t.Fatal("re-referenced blob was deleted: the sweep must re-check references")
	}
	if rep.Swept != 1 {
		t.Fatalf("swept %d, want 1", rep.Swept)
	}
}
