package retention

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
)

// anyDomain makes the fixture's service resolve every dedup domain to the one
// disk store, so tests can use several sources.
func (f *fixture) anyDomain() {
	f.svc.Stores = func(context.Context, string) (blob.Store, error) { return f.disk, nil }
}

// configure edits the stored settings through SaveSettings.
func (f *fixture) configure(t *testing.T, edit func(*Settings)) Settings {
	t.Helper()
	ctx := context.Background()
	cur, err := LoadStored(ctx, f.pool, f.instance)
	if err != nil {
		t.Fatal(err)
	}
	edit(&cur)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	saved, err := SaveSettings(ctx, tx, f.instance, f.svc.Env, cur.Revision, cur)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return saved
}

// pressureOn enables pressure with a byte quota and the given watermarks.
func (f *fixture) pressureOn(t *testing.T, quota int64, high, low int) {
	t.Helper()
	f.anyDomain()
	f.configure(t, func(s *Settings) {
		s.Pressure = PressureConfig{Enabled: true, HighPercent: high, LowPercent: low, CapacityBytes: quota}
	})
}

type sized struct {
	size      int
	idleHours int // last_accessed_at this long ago; negative leaves it unset
	ageHours  int // created_at this long ago
	tag       string
}

// seedSized creates a version with one asset of s.size real bytes, so a sweep
// has something to delete. Versions are created oldest-first by ageHours.
func (f *fixture) seedSized(t *testing.T, source, domain, coordinate, version string, s sized) blob.Info {
	t.Helper()
	ctx := context.Background()
	data := bytes.Repeat([]byte(source+coordinate+version+"|"), s.size/len(source+coordinate+version+"|")+1)[:s.size]
	info := sum(data)
	if _, err := f.disk.Put(ctx, bytes.NewReader(data), info); err != nil {
		t.Fatal(err)
	}
	age := fmt.Sprintf("%d hours", s.ageHours)
	var component, versionID, blobID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,$3) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate RETURNING id::text`, f.instance, source, coordinate).Scan(&component); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version,created_at) VALUES($1,$2,$3,now()-$4::interval) RETURNING id::text`, f.instance, component, version, age).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text`, f.instance, domain, info.Digest, info.Size).Scan(&blobID); err != nil {
		t.Fatal(err)
	}
	var accessed any
	if s.idleHours >= 0 {
		accessed = time.Now().Add(-time.Duration(s.idleHours) * time.Hour)
	}
	f.exec(t, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,created_at,last_accessed_at) VALUES($1,$2,'a',$3,now()-$4::interval,$5)`, f.instance, versionID, blobID, age, accessed)
	if s.tag != "" {
		f.exec(t, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3)`, component, s.tag, versionID)
	}
	return info
}

func evicted(rep PressureReport) []string {
	var out []string
	for _, d := range rep.Evicted {
		out = append(out, d.Coordinate+"@"+d.Version)
	}
	sort.Strings(out)
	return out
}

// cacheFixture builds a remote source with six components, each with an old
// version v1 (100 bytes, idle 100h, 90h, ... 50h for c0..c5) and a newer v2
// (100 bytes, just accessed). 1200 bytes in total.
func cacheFixture(t *testing.T, f *fixture) (source, domain string) {
	t.Helper()
	_, source, domain = f.newSource(t, "cache", "remote")
	for i := range 6 {
		coord := fmt.Sprintf("pkg/c%d", i)
		f.seedSized(t, source, domain, coord, "v1", sized{size: 100, idleHours: 100 - 10*i, ageHours: 500})
		f.seedSized(t, source, domain, coord, "v2", sized{size: 100, idleHours: 0, ageHours: 1})
	}
	return
}

func TestPressureEvictsLeastRecentlyUsedDownToLowWatermark(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour, PressureSweepGrace: time.Minute})
	ctx := context.Background()
	source, _ := cacheFixture(t, f)
	// c1's old version is tagged and therefore protected.
	f.exec(t, `INSERT INTO tags(component_id,name,version_id) SELECT c.id,'keep',v.id FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1 AND c.coordinate='pkg/c1' AND v.version='v1'`, source)
	// 1200 used of 2000 (60%); high 50% = 1000, low 40% = 800: free 400 bytes.
	f.pressureOn(t, 2000, 50, 40)

	dry, err := f.svc.RunPressure(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pkg/c0@v1", "pkg/c2@v1", "pkg/c3@v1", "pkg/c4@v1"}
	if dry.State != StatePressure || dry.TargetBytes != 400 || !slices.Equal(evicted(dry), want) || dry.EvictedBytes != 400 {
		t.Fatalf("dry-run: %+v", dry)
	}
	if f.count(t, "SELECT count(*) FROM versions") != 12 || f.count(t, "SELECT count(*) FROM gc_candidates") != 0 {
		t.Fatal("dry-run modified data")
	}

	rep, err := f.svc.RunPressure(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(evicted(rep), want) || rep.EvictedBytes != 400 || rep.ShortfallBytes != 0 {
		t.Fatalf("live run differs from dry-run: %+v", rep)
	}
	for _, d := range rep.Evicted {
		if d.Reason != "pressure_lru" {
			t.Fatalf("reason %q", d.Reason)
		}
	}
	// The protected tagged version, every newest version and the freshest old one remain.
	left := f.versions(t, source)
	for _, keep := range []string{"pkg/c1@v1", "pkg/c5@v1", "pkg/c0@v2", "pkg/c5@v2"} {
		if !slices.Contains(left, keep) {
			t.Fatalf("%s was evicted: %v", keep, left)
		}
	}
	// Audit and outbox rows exist for each eviction.
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action='retention.pressure.evict'`); n != 4 {
		t.Fatalf("audit rows %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM outbox_events WHERE topic='retention.pressure.evict'`); n != 4 {
		t.Fatalf("outbox rows %d", n)
	}
	// Freed blobs were marked with a short sweep wait, far below the GC grace.
	if n := f.count(t, `SELECT count(*) FROM gc_candidates WHERE sweep_after IS NOT NULL AND sweep_after<now()+interval '2 minutes'`); n != 4 {
		t.Fatalf("expedited candidates %d", n)
	}
	// Bytes stay until the sweep wait is over.
	if st, err := f.svc.PressureStatus(ctx); err != nil || st.State != StatePressure || st.UsedBytes != 1200 || st.ReclaimableBytes != 400 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestPressureHysteresisAndShortSweep(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour, PressureSweepGrace: time.Minute})
	ctx := context.Background()
	source, _ := cacheFixture(t, f)

	// 60% is between low (40%) and high (70%): nothing happens.
	f.pressureOn(t, 2000, 70, 40)
	rep, err := f.svc.RunPressure(ctx, false)
	if err != nil || rep.State != StateOK || rep.EvictedVersions != 0 || f.count(t, "SELECT count(*) FROM versions") != 12 {
		t.Fatalf("below high: %+v %v", rep, err)
	}

	// Cross the high watermark: evict down to the low one (800 bytes = 4 versions).
	f.pressureOn(t, 1600, 70, 40) // 1200/1600 = 75%; low = 640: free 560 -> 6 versions
	rep, err = f.svc.RunPressure(ctx, false)
	if err != nil || rep.State != StatePressure || rep.EvictedBytes < rep.TargetBytes {
		t.Fatalf("eviction: %+v %v", rep, err)
	}
	evictedFirst := rep.EvictedVersions

	// A second check before the sweep must not evict again: the freed bytes are
	// already counted as reclaimable.
	again, err := f.svc.RunPressure(ctx, false)
	if err != nil || again.EvictedVersions != 0 || again.TargetBytes != 0 {
		t.Fatalf("second pass evicted more: %+v %v", again, err)
	}
	// Not due yet: the short wait has not elapsed.
	if again.Swept != 0 || f.count(t, "SELECT count(*) FROM blobs") != 12 {
		t.Fatalf("swept before the wait: %+v", again)
	}

	// After the short wait, the next check sweeps them (far sooner than the 1h grace
	// and without a normal GC run), and usage drops below the high watermark.
	f.exec(t, `UPDATE gc_candidates SET sweep_after=now()-interval '1 second'`)
	f.exec(t, `UPDATE gc_candidates SET first_seen_at=first_seen_at-interval '1 minute'`)
	swept, err := f.svc.RunPressure(ctx, false)
	if err != nil || swept.Swept != evictedFirst {
		t.Fatalf("sweep: %+v %v (want %d)", swept, err, evictedFirst)
	}
	if got := f.count(t, "SELECT count(*) FROM blobs"); got != 12-evictedFirst {
		t.Fatalf("blobs left %d", got)
	}
	after, err := f.svc.RunPressure(ctx, false)
	if err != nil || after.State != StateOK || after.EvictedVersions != 0 {
		t.Fatalf("after relief: %+v %v", after, err)
	}
	// Used is now at or below the low watermark: no flapping.
	if after.UsedBytes > 640 {
		t.Fatalf("used %d above low watermark", after.UsedBytes)
	}
	_ = source
}

func TestPressureNeverEvictsLocalOrReplicaContent(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	f.anyDomain()
	// The cache holds a single version per package (all protected by min_keep).
	_, cache, cacheDomain := f.newSource(t, "cache", "remote")
	for i := range 3 {
		f.seedSized(t, cache, cacheDomain, fmt.Sprintf("pkg/c%d", i), "v1", sized{size: 100, idleHours: 100, ageHours: 500})
	}
	// Local and replica sources are full of old, idle versions.
	_, replica, replicaDomain := f.newSource(t, "mirror", "replica")
	for i := range 3 {
		f.seedSized(t, f.source, f.domain, "rel/app", fmt.Sprintf("1.%d", i), sized{size: 100, idleHours: 1000 + i, ageHours: 2000 + i})
		f.seedSized(t, replica, replicaDomain, "rel/lib", fmt.Sprintf("2.%d", i), sized{size: 100, idleHours: 1000 + i, ageHours: 2000 + i})
	}
	before := f.count(t, "SELECT count(*) FROM versions")
	f.pressureOn(t, 1000, 50, 30) // 900/1000 = 90%

	rep, err := f.svc.RunPressure(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != StateUnevictable || rep.EvictedVersions != 0 || rep.ShortfallBytes == 0 || f.count(t, "SELECT count(*) FROM versions") != before {
		t.Fatalf("release content was evicted or the shortfall is not reported: %+v", rep)
	}
	if w, ok := PressureWarning(); !ok || w == "" {
		t.Fatal("no health warning for unevictable pressure")
	}
	st, err := f.svc.PressureStatus(ctx)
	if err != nil || st.State != StateUnevictable {
		t.Fatalf("status %+v %v", st, err)
	}

	// A default policy alone never makes release content evictable.
	f.configure(t, func(s *Settings) { s.Default = DefaultPolicy{Enabled: true, MaxIdle: time.Hour, MinKeep: 1} })
	if rep, err = f.svc.RunPressure(ctx, false); err != nil || rep.EvictedVersions != 0 {
		t.Fatalf("default policy opened release content: %+v %v", rep, err)
	}

	// Explicit opt-in: the local source's own policy allows release deletion.
	if _, err = f.svc.SetPolicy(ctx, Policy{SourceID: f.source, KeepLast: 100, MinKeep: 1, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rep, err = f.svc.RunPressure(ctx, false); err != nil {
		t.Fatal(err)
	}
	if rep.EvictedVersions == 0 {
		t.Fatalf("opted-in release content was not evicted: %+v", rep)
	}
	for _, d := range rep.Evicted {
		if d.SourceID != f.source {
			t.Fatalf("evicted from %s which did not opt in", d.SourceID)
		}
	}
	if got := f.versions(t, replica); len(got) != 3 {
		t.Fatalf("replica content touched: %v", got)
	}
	// The newest local version is always kept.
	if !slices.Contains(f.versions(t, f.source), "rel/app@1.0") {
		t.Fatalf("min_keep violated: %v", f.versions(t, f.source))
	}
}

func TestPressureCapacitySourcesAndDisabledStates(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.anyDomain()

	rep, err := f.svc.RunPressure(ctx, false)
	if err != nil || rep.State != StateDisabled {
		t.Fatalf("default must be disabled: %+v %v", rep, err)
	}

	// Object storage has no native capacity: without a quota the trigger is
	// disabled and says why.
	f.svc.Backend = func(context.Context) (string, error) { return "s3", nil }
	f.configure(t, func(s *Settings) { s.Pressure.Enabled = true })
	rep, err = f.svc.RunPressure(ctx, false)
	if err != nil || rep.State != StateNoCapacity || rep.CapacitySource != "none" || rep.Detail == "" {
		t.Fatalf("s3 without quota: %+v %v", rep, err)
	}
	st, err := f.svc.PressureStatus(ctx)
	if err != nil || st.State != StateNoCapacity || !st.Enabled {
		t.Fatalf("status %+v %v", st, err)
	}

	// A quota turns it on, measured from committed blob bytes.
	_, cache, domain := f.newSource(t, "cache", "remote")
	f.seedSized(t, cache, domain, "pkg/a", "v1", sized{size: 100, idleHours: 50, ageHours: 100})
	f.seedSized(t, cache, domain, "pkg/a", "v2", sized{size: 100, idleHours: 0, ageHours: 1})
	f.configure(t, func(s *Settings) { s.Pressure.CapacityBytes = 400 })
	rep, err = f.svc.RunPressure(ctx, true)
	if err != nil || rep.CapacitySource != "quota" || rep.UsedBytes != 200 || rep.CapacityBytes != 400 || rep.UsageRatio != 0.5 || rep.State != StateOK {
		t.Fatalf("quota: %+v %v", rep, err)
	}

	// Disk without a quota uses the filesystem of the blob root.
	f.svc.Backend = func(context.Context) (string, error) { return "disk", nil }
	f.svc.DataDir = "/data"
	f.svc.FSUsage = func(path string) (uint64, uint64, error) {
		if path != "/data" {
			t.Fatalf("path %q", path)
		}
		return 1000, 150, nil // 85% used
	}
	f.configure(t, func(s *Settings) { s.Pressure.CapacityBytes = 0 })
	rep, err = f.svc.RunPressure(ctx, true)
	if err != nil || rep.CapacitySource != "filesystem" || rep.UsedBytes != 850 || rep.CapacityBytes != 1000 || rep.State != StatePressure {
		t.Fatalf("filesystem: %+v %v", rep, err)
	}
	if rep.EvictedVersions != 1 || rep.Evicted[0].Version != "v1" {
		t.Fatalf("expected the old version to be selected: %+v", rep)
	}

	// Disk capacity errors are surfaced, not turned into silent success.
	f.svc.FSUsage = func(string) (uint64, uint64, error) { return 0, 0, errors.New("statfs failed") }
	if _, err = f.svc.RunPressure(ctx, true); err == nil {
		t.Fatal("filesystem error swallowed")
	}

	// Inconsistent watermarks cannot be stored.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cur, _ := LoadStored(ctx, f.pool, f.instance)
	cur.Pressure.LowPercent = cur.Pressure.HighPercent
	if _, err = SaveSettings(ctx, tx, f.instance, Overrides{}, cur.Revision, cur); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("got %v", err)
	}
}

func TestSettingsRevisionAndEnvironmentLocks(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	idle := 48 * time.Hour
	quota := int64(5 << 30)
	f.svc.Env = Overrides{DefaultMaxIdle: &idle, CapacityBytes: &quota}

	s, err := f.svc.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Default.MaxIdle != idle || !s.Default.Enabled || s.Pressure.CapacityBytes != quota || !s.IsLocked(LockDefaultMaxIdle) || !s.IsLocked(LockCapacityBytes) || !s.IsLocked(LockDefaultEnabled) || s.IsLocked(LockHighPercent) {
		t.Fatalf("env not applied: %+v", s)
	}
	stored := f.configure(t, func(cur *Settings) {
		cur.Default.Enabled, cur.Default.MaxIdle = s.Default.Enabled, s.Default.MaxIdle // equal to the effective value: accepted
		cur.Pressure.CapacityBytes = quota
		cur.Pressure.HighPercent, cur.Pressure.LowPercent = 90, 80
	})
	if stored.Revision != 2 {
		t.Fatalf("revision %d", stored.Revision)
	}
	raw, _ := LoadStored(ctx, f.pool, f.instance)
	if raw.Default.MaxIdle != 0 || raw.Pressure.CapacityBytes != 0 || raw.Pressure.HighPercent != 90 {
		t.Fatalf("locked values must not be written to the database: %+v", raw)
	}

	// Changing a locked value is refused; a stale revision conflicts.
	tx, _ := f.pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	bad := raw
	bad.Pressure.CapacityBytes = quota * 2
	if _, err = SaveSettings(ctx, tx, f.instance, f.svc.Env, raw.Revision, bad); !errors.Is(err, ErrSettingLocked) {
		t.Fatalf("got %v", err)
	}
	_ = tx.Rollback(ctx)
	tx2, _ := f.pool.Begin(ctx)
	defer func() { _ = tx2.Rollback(ctx) }()
	if _, err = SaveSettings(ctx, tx2, f.instance, f.svc.Env, 1, raw); !errors.Is(err, database.ErrRevisionConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestDefaultPolicyAppliesOnlyToRemoteSourcesWithoutOwnPolicy(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.anyDomain()
	feedA, srcA, domA := f.newSource(t, "cache-a", "remote")
	feedB, srcB, domB := f.newSource(t, "cache-b", "remote")
	_ = feedA
	for _, x := range []struct{ src, dom string }{{srcA, domA}, {srcB, domB}} {
		f.seedSized(t, x.src, x.dom, "pkg/x", "old-idle", sized{size: 10, idleHours: 24 * 40, ageHours: 24 * 60})
		f.seedSized(t, x.src, x.dom, "pkg/x", "old-used", sized{size: 11, idleHours: 1, ageHours: 24 * 59})
		f.seedSized(t, x.src, x.dom, "pkg/x", "newest", sized{size: 12, idleHours: 0, ageHours: 1})
	}
	f.seedSized(t, f.source, f.domain, "rel/app", "1.0", sized{size: 13, idleHours: 24 * 400, ageHours: 24 * 500})
	f.seedSized(t, f.source, f.domain, "rel/app", "2.0", sized{size: 14, idleHours: 24 * 300, ageHours: 24 * 400})

	// No default: nothing is deleted.
	if rep, err := f.svc.ApplyRetention(ctx, false); err != nil || len(rep.Deleted) != 0 {
		t.Fatalf("no policy must delete nothing: %+v %v", rep, err)
	}
	f.configure(t, func(s *Settings) { s.Default = DefaultPolicy{Enabled: true, MaxIdle: 30 * 24 * time.Hour, MinKeep: 1} })
	// Source B has its own, disabled policy; feed policies override the default as well.
	if _, err := f.svc.SetPolicy(ctx, Policy{SourceID: srcB, KeepLast: 50, MinKeep: 1, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	dry, err := f.svc.ApplyRetention(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(dry.Deleted); !slices.Equal(got, []string{"pkg/x@old-idle"}) {
		t.Fatalf("dry-run: %v", got)
	}
	live, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !slices.Equal(names(live.Deleted), names(dry.Deleted)) || live.Deleted[0].SourceID != srcA {
		t.Fatalf("live differs from dry-run: %+v %v", live, err)
	}
	if got := f.versions(t, srcA); !slices.Equal(got, []string{"pkg/x@newest", "pkg/x@old-used"}) {
		t.Fatalf("source A: %v", got)
	}
	if len(f.versions(t, srcB)) != 3 {
		t.Fatalf("source B has its own policy and must be untouched: %v", f.versions(t, srcB))
	}
	if len(f.versions(t, f.source)) != 2 {
		t.Fatal("the default policy deleted local release content")
	}

	// A feed-level policy also takes precedence over the default for that feed.
	feedC, srcC, domC := f.newSource(t, "cache-c", "remote")
	f.seedSized(t, srcC, domC, "pkg/y", "old", sized{size: 10, idleHours: 24 * 40, ageHours: 24 * 60})
	f.seedSized(t, srcC, domC, "pkg/y", "new", sized{size: 10, idleHours: 0, ageHours: 1})
	if _, err = f.svc.SetPolicy(ctx, Policy{FeedID: feedC, KeepLast: 5, MinKeep: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.svc.ApplyRetention(ctx, false); err != nil || len(rep.Deleted) != 0 || len(f.versions(t, srcC)) != 2 {
		t.Fatalf("feed policy must override the default: %+v %v", rep, err)
	}
	_ = feedB
}

// A download recorded by the traffic flusher (asset_downloads) counts as an
// access for idle-based eviction, and a young GC candidate with an explicit
// short sweep_after is swept without waiting out the grace period.
func TestIdleUsesDownloadsAndExpeditedCandidatesSweep(t *testing.T) {
	f := newFixture(t, Options{Grace: 24 * time.Hour})
	ctx := context.Background()
	f.anyDomain()
	_, src, dom := f.newSource(t, "cache", "remote")
	f.seedSized(t, src, dom, "pkg/z", "stale", sized{size: 10, idleHours: -1, ageHours: 24 * 90})
	f.seedSized(t, src, dom, "pkg/z", "downloaded", sized{size: 11, idleHours: -1, ageHours: 24 * 91})
	f.seedSized(t, src, dom, "pkg/z", "newest", sized{size: 12, idleHours: 0, ageHours: 1})
	f.exec(t, `INSERT INTO asset_downloads(asset_id,instance_id,downloads,last_downloaded_at) SELECT a.id,a.instance_id,5,now() FROM assets a JOIN versions v ON v.id=a.version_id WHERE v.version='downloaded'`)
	f.configure(t, func(s *Settings) { s.Default = DefaultPolicy{Enabled: true, MaxIdle: 30 * 24 * time.Hour, MinKeep: 1} })
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !slices.Equal(names(rep.Deleted), []string{"pkg/z@stale"}) {
		t.Fatalf("idle must follow downloads: %+v %v", rep, err)
	}

	// The deleted version's blob is unreferenced; mark it as a pressure eviction would.
	f.exec(t, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind,sweep_after) SELECT b.instance_id,b.dedup_domain_id,b.digest,'row',now()-interval '1 second' FROM blobs b WHERE NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id)`)
	f.exec(t, `UPDATE gc_candidates SET first_seen_at=now()-interval '1 minute'`)
	// A normal candidate waits out the grace period; an expedited one does not.
	normal := f.orphan(t, "1.0", []byte("orphan bytes of an ordinary candidate"))
	f.ageBlobs(t, 25*time.Hour)
	gc, err := f.svc.CollectGarbage(ctx, false)
	if err != nil || len(gc.Swept) != 1 || gc.Swept[0].Kind != "row" || len(gc.Marked) != 1 || gc.Marked[0].Digest != string(normal.Digest) {
		t.Fatalf("exactly the expedited candidate is swept: %+v %v", gc, err)
	}
	if !f.hasBytes(normal.Digest) {
		t.Fatal("ordinary candidate swept before its grace period")
	}
}

// The sweep keeps the reference re-check under the row lock even when the wait
// was shortened: a blob that regained a reference is not deleted.
func TestExpeditedSweepStillRechecksReferences(t *testing.T) {
	f := newFixture(t, Options{Grace: 24 * time.Hour})
	ctx := context.Background()
	f.anyDomain()
	_, src, dom := f.newSource(t, "cache", "remote")
	info := f.seedSized(t, src, dom, "pkg/r", "v1", sized{size: 20, idleHours: 0, ageHours: 1})
	f.exec(t, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind,sweep_after,first_seen_at) SELECT instance_id,dedup_domain_id,digest,'row',now()-interval '1 second',now()-interval '1 minute' FROM blobs`)
	var rep GCReport
	if err := f.svc.sweep(ctx, time.Now(), false, true, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Swept) != 0 || rep.SkippedReferenced != 1 || !f.hasBytes(info.Digest) {
		t.Fatalf("a referenced blob was swept: %+v", rep)
	}
}
