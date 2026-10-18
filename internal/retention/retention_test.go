package retention

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

type seeded struct{ ageHours, idleHours int }

// seedVersion inserts a component version with one asset whose timestamps are
// ageHours old. idleHours >= 0 sets last_accessed_at that long ago.
func (f *fixture) seedVersion(t *testing.T, source, domain, coordinate, version string, s seeded, tag string) {
	t.Helper()
	ctx := context.Background()
	var component, versionID, blobID string
	age := fmt.Sprintf("%d hours", s.ageHours)
	if err := f.pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,$3) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate RETURNING id::text`, f.instance, source, coordinate).Scan(&component); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version,created_at) VALUES($1,$2,$3,now()-$4::interval) RETURNING id::text`, f.instance, component, version, age).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	info := sum([]byte(source + coordinate + version))
	if err := f.pool.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) RETURNING id::text`, f.instance, domain, info.Digest, info.Size).Scan(&blobID); err != nil {
		t.Fatal(err)
	}
	var accessed any
	if s.idleHours >= 0 {
		accessed = time.Now().Add(-time.Duration(s.idleHours) * time.Hour)
	}
	f.exec(t, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,created_at,last_accessed_at) VALUES($1,$2,'a',$3,now()-$4::interval,$5)`, f.instance, versionID, blobID, age, accessed)
	if tag != "" {
		f.exec(t, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3)`, component, tag, versionID)
	}
}

func (f *fixture) versions(t *testing.T, source string) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT c.coordinate||'@'||v.version FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1 ORDER BY 1`, source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func names(d []Deletion) []string {
	var out []string
	for _, x := range d {
		out = append(out, x.Coordinate+"@"+x.Version)
	}
	sort.Strings(out)
	return out
}

func TestKeepLastNWithReleaseProtectionAndDryRunAccuracy(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	for i := range 5 { // v0 is newest
		f.seedVersion(t, f.source, f.domain, "lib/a", fmt.Sprintf("v%d", i), seeded{ageHours: i + 1, idleHours: -1}, "")
	}
	all := f.versions(t, f.source)

	// No policy: nothing is ever deleted.
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || len(rep.Deleted) != 0 || !reflect.DeepEqual(f.versions(t, f.source), all) {
		t.Fatalf("deleted without policy: %+v %v", rep, err)
	}
	p, err := f.svc.SetPolicy(ctx, Policy{SourceID: f.source, KeepLast: 2, MinKeep: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Immutable release content is protected unless the policy allows deletion.
	rep, err = f.svc.ApplyRetention(ctx, false)
	if err != nil || len(rep.Deleted) != 0 || rep.ProtectedRelease != 3 || !reflect.DeepEqual(f.versions(t, f.source), all) {
		t.Fatalf("release content deleted without permission: %+v %v", rep, err)
	}
	p.AllowReleaseDelete = true
	if _, err = f.svc.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	dry, err := f.svc.ApplyRetention(ctx, true)
	want := []string{"lib/a@v2", "lib/a@v3", "lib/a@v4"}
	if err != nil || !reflect.DeepEqual(names(dry.Deleted), want) || !reflect.DeepEqual(f.versions(t, f.source), all) {
		t.Fatalf("dry-run: %v %+v %v", names(dry.Deleted), dry, err)
	}
	beforeBlobs := f.count(t, "SELECT count(*) FROM blobs")
	rep, err = f.svc.ApplyRetention(ctx, false)
	if err != nil || !reflect.DeepEqual(rep.Deleted, dry.Deleted) {
		t.Fatalf("real run differs from dry-run: %+v vs %+v %v", rep.Deleted, dry.Deleted, err)
	}
	if got := f.versions(t, f.source); !reflect.DeepEqual(got, []string{"lib/a@v0", "lib/a@v1"}) {
		t.Fatalf("remaining %v", got)
	}
	if f.count(t, "SELECT count(*) FROM blobs") != beforeBlobs {
		t.Fatal("retention must leave blob bytes to garbage collection")
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action='retention.version.delete'") != 3 {
		t.Fatal("deletions not audited")
	}
	// Idempotent.
	if rep, err = f.svc.ApplyRetention(ctx, false); err != nil || len(rep.Deleted) != 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}
}

func TestMaxAgeKeepsMinimumAndProtectsTags(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	f.seedVersion(t, f.source, f.domain, "lib/b", "old1", seeded{ageHours: 500, idleHours: -1}, "")
	f.seedVersion(t, f.source, f.domain, "lib/b", "old2", seeded{ageHours: 400, idleHours: -1}, "stable")
	f.seedVersion(t, f.source, f.domain, "lib/b", "old3", seeded{ageHours: 300, idleHours: -1}, "")
	f.seedVersion(t, f.source, f.domain, "lib/c", "only", seeded{ageHours: 900, idleHours: -1}, "")
	p, err := f.svc.SetPolicy(ctx, Policy{SourceID: f.source, MaxAge: 200 * time.Hour, MinKeep: 1, AllowReleaseDelete: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !reflect.DeepEqual(names(rep.Deleted), []string{"lib/b@old1"}) || rep.ProtectedTagged != 1 {
		t.Fatalf("max_age: %v %+v %v", names(rep.Deleted), rep, err)
	}
	// lib/c's only version is the newest and exempt through min_keep; old3 too.
	if got := f.versions(t, f.source); !reflect.DeepEqual(got, []string{"lib/b@old2", "lib/b@old3", "lib/c@only"}) {
		t.Fatalf("remaining %v", got)
	}
	p.IncludeTagged, p.MinKeep = true, 0
	if _, err = f.svc.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rep, err = f.svc.ApplyRetention(ctx, false); err != nil || len(rep.Deleted) != 3 || len(f.versions(t, f.source)) != 0 {
		t.Fatalf("include_tagged/min_keep=0: %+v %v", rep, err)
	}
	if f.count(t, "SELECT count(*) FROM tags") != 0 {
		t.Fatal("tags of deleted versions remain")
	}
}

func TestRemoteCacheEvictionByLastAccess(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	feed, remote, domain := f.newSource(t, "mirror", "remote")
	_ = feed
	f.seedVersion(t, remote, domain, "lib/r", "hot", seeded{ageHours: 1000, idleHours: 1}, "")
	f.seedVersion(t, remote, domain, "lib/r", "cold", seeded{ageHours: 1000, idleHours: 500}, "")
	f.seedVersion(t, remote, domain, "lib/r", "never-read", seeded{ageHours: 600, idleHours: -1}, "")
	f.seedVersion(t, remote, domain, "lib/r", "fresh", seeded{ageHours: 2, idleHours: -1}, "")
	// A local source with the same shape is untouched by a remote-only policy.
	f.seedVersion(t, f.source, f.domain, "lib/r", "cold", seeded{ageHours: 1000, idleHours: 500}, "")
	if _, err := f.svc.SetPolicy(ctx, Policy{SourceID: remote, MaxIdle: 100 * time.Hour, MinKeep: 0, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	dry, err := f.svc.ApplyRetention(ctx, true)
	if err != nil || !reflect.DeepEqual(names(dry.Deleted), []string{"lib/r@cold", "lib/r@never-read"}) {
		t.Fatalf("dry-run: %v %v", names(dry.Deleted), err)
	}
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !reflect.DeepEqual(rep.Deleted, dry.Deleted) {
		t.Fatalf("real: %+v %v", rep, err)
	}
	if got := f.versions(t, remote); !reflect.DeepEqual(got, []string{"lib/r@fresh", "lib/r@hot"}) {
		t.Fatalf("remote remaining %v", got)
	}
	if len(f.versions(t, f.source)) != 1 {
		t.Fatal("local content touched by remote policy")
	}
}

func TestFeedPolicyInheritanceAndSourceOverride(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	for i := range 4 {
		f.seedVersion(t, f.source, f.domain, "lib/d", fmt.Sprintf("v%d", i), seeded{ageHours: i + 1, idleHours: -1}, "")
	}
	// Second feed shares the source: both feeds must agree before deleting.
	var feed2 string
	if err := f.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'second','raw') RETURNING id::text", f.instance).Scan(&feed2); err != nil {
		t.Fatal(err)
	}
	f.exec(t, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", f.instance, feed2, f.source)
	if _, err := f.svc.SetPolicy(ctx, Policy{FeedID: f.feed, KeepLast: 1, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.svc.ApplyRetention(ctx, false); err != nil || len(rep.Deleted) != 0 {
		t.Fatalf("feed without policy must keep everything: %+v %v", rep, err)
	}
	if _, err := f.svc.SetPolicy(ctx, Policy{FeedID: feed2, KeepLast: 3, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !reflect.DeepEqual(names(rep.Deleted), []string{"lib/d@v3"}) {
		t.Fatalf("intersection of feed policies: %v %v", names(rep.Deleted), err)
	}
	// A source policy overrides both feed policies.
	if _, err = f.svc.SetPolicy(ctx, Policy{SourceID: f.source, KeepLast: 2, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rep, err = f.svc.ApplyRetention(ctx, false); err != nil || !reflect.DeepEqual(names(rep.Deleted), []string{"lib/d@v2"}) {
		t.Fatalf("source override: %v %v", names(rep.Deleted), err)
	}
}

func TestPolicyValidationAndRoundTrip(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	for name, p := range map[string]Policy{
		"no target":   {KeepLast: 1},
		"both":        {FeedID: f.feed, SourceID: f.source, KeepLast: 1},
		"no rule":     {SourceID: f.source},
		"negative":    {SourceID: f.source, KeepLast: -1},
		"bad id":      {SourceID: "x", KeepLast: 1},
		"tiny window": {SourceID: f.source, MaxAge: time.Millisecond},
	} {
		if _, err := f.svc.SetPolicy(ctx, p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	want := Policy{SourceID: f.source, KeepLast: 3, MaxAge: 48 * time.Hour, MaxIdle: time.Hour, MinKeep: 2, IncludeTagged: true, Enabled: true}
	saved, err := f.svc.SetPolicy(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.SetPolicy(ctx, want)
	if err != nil || again.ID != saved.ID {
		t.Fatalf("upsert created a duplicate: %v %v", again, err)
	}
	list, err := f.svc.ListPolicies(ctx)
	want.ID = saved.ID
	if err != nil || len(list) != 1 || list[0] != want {
		t.Fatalf("round trip %+v %v", list, err)
	}
	if err = f.svc.DeletePolicy(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ = f.svc.ListPolicies(ctx); len(list) != 0 {
		t.Fatal("policy not deleted")
	}
}

func TestRetentionThenGarbageCollectionReclaimsBytes(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	for i := range 3 {
		f.publish(t, fmt.Sprintf("1.%d", i), []byte(fmt.Sprintf("release %d", i)))
		f.exec(t, "UPDATE versions SET created_at=now()-$1::interval WHERE version=$2", fmt.Sprintf("%d hours", 10-i), fmt.Sprintf("1.%d", i))
	}
	if _, err := f.svc.SetPolicy(ctx, Policy{SourceID: f.source, KeepLast: 1, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.svc.RunOnce(ctx, false); err != nil || len(rep.Retention.Deleted) != 2 {
		t.Fatalf("%+v %v", rep, err)
	}
	f.ageBlobs(t, 5*time.Hour)
	for range 2 {
		if _, err := f.svc.RunOnce(ctx, false); err != nil {
			t.Fatal(err)
		}
		f.ageCandidates(t, 5*time.Hour)
	}
	if f.hasBytes(sum([]byte("release 0")).Digest) || f.hasBytes(sum([]byte("release 1")).Digest) || !f.hasBytes(sum([]byte("release 2")).Digest) {
		t.Fatal("expected only the newest release to survive")
	}
}
