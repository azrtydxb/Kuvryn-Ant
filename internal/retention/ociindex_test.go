package retention

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// ociRepo is an OCI repository with real stored manifests, blobs and index links.
type ociRepo struct {
	t         *testing.T
	f         *fixture
	dom       string
	src       string
	component string
}

func newOCIRepo(t *testing.T, kind string) *ociRepo {
	t.Helper()
	f := newFixture(t, Options{Grace: time.Hour, PressureSweepGrace: time.Minute})
	_, src, dom := f.newSource(t, "images", kind)
	f.svc.Stores = func(context.Context, string) (blob.Store, error) { return f.disk, nil }
	f.exec(t, `UPDATE sources SET format='oci' WHERE id=$1`, src)
	f.exec(t, `UPDATE feeds SET format='oci'`)
	r := &ociRepo{t: t, f: f, dom: dom, src: src}
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'team/app') RETURNING id::text`, f.instance, src).Scan(&r.component); err != nil {
		t.Fatal(err)
	}
	return r
}

// put stores data as an asset of version, created ageHours ago and (when
// idleHours >= 0) last accessed idleHours ago.
func (r *ociRepo) put(version, key string, data []byte, ageHours, idleHours int) {
	r.t.Helper()
	ctx := context.Background()
	info := sum(data)
	if _, err := r.f.disk.Put(ctx, bytes.NewReader(data), info); err != nil {
		r.t.Fatal(err)
	}
	var vid, bid string
	age := fmt.Sprintf("%d hours", ageHours)
	if err := r.f.pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version,created_at) VALUES($1,$2,$3,now()-$4::interval) ON CONFLICT(component_id,version) DO UPDATE SET version=excluded.version RETURNING id::text`, r.f.instance, r.component, version, age).Scan(&vid); err != nil {
		r.t.Fatal(err)
	}
	if err := r.f.pool.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text`, r.f.instance, r.dom, info.Digest, info.Size).Scan(&bid); err != nil {
		r.t.Fatal(err)
	}
	var accessed any
	if idleHours >= 0 {
		accessed = time.Now().Add(-time.Duration(idleHours) * time.Hour)
	}
	r.f.exec(r.t, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,created_at,last_accessed_at) VALUES($1,$2,$3,$4,now()-$5::interval,$6)`, r.f.instance, vid, key, bid, age, accessed)
}

// image stores a manifest with its own config and layer (blobs are as old as
// blobAge and just used, so only the manifests decide) and returns its digest.
func (r *ociRepo) image(name string, ageHours, idleHours, blobAge int, extraLayers ...[]byte) string {
	cfg, layer := []byte("config of "+name), []byte("layer of "+name)
	r.put(string(sum(cfg).Digest), "blob", cfg, blobAge, 0)
	r.put(string(sum(layer).Digest), "blob", layer, blobAge, 0)
	layers := fmt.Sprintf(`{"digest":"sha256:%s"}`, sum(layer).Digest)
	for _, l := range extraLayers {
		r.put(string(sum(l).Digest), "blob", l, blobAge, 0)
		layers += fmt.Sprintf(`,{"digest":"sha256:%s"}`, sum(l).Digest)
	}
	m := []byte(fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"sha256:%s"},"layers":[%s]}`, sum(cfg).Digest, layers))
	r.put(string(sum(m).Digest), "manifest", m, ageHours, idleHours)
	return string(sum(m).Digest)
}

// index stores an index naming the given children and links them.
func (r *ociRepo) index(name string, ageHours, idleHours int, children ...string) string {
	list := ""
	for i, c := range children {
		if i > 0 {
			list += ","
		}
		list += fmt.Sprintf(`{"digest":"sha256:%s"}`, c)
	}
	m := []byte(fmt.Sprintf(`{"schemaVersion":2,"name":%q,"manifests":[%s]}`, name, list))
	d := string(sum(m).Digest)
	r.put(d, "manifest", m, ageHours, idleHours)
	for _, c := range children {
		r.f.exec(r.t, `INSERT INTO version_children(component_id,parent_version_id,child_version,platform) SELECT $1,v.id,$3,'' FROM versions v WHERE v.component_id=$1 AND v.version=$2`, r.component, d, c)
	}
	return d
}

// multiArch builds an index with two platform images and their attestations.
func (r *ociRepo) multiArch(name string, ageHours, idleHours int) (index string, children []string) {
	for _, p := range []string{"amd64", "attest-amd64", "arm64", "attest-arm64"} {
		children = append(children, r.image(name+"/"+p, ageHours, idleHours, 1))
	}
	return r.index(name, ageHours, idleHours, children...), children
}

func (r *ociRepo) visible() []string {
	r.t.Helper()
	rows, err := r.f.pool.Query(context.Background(), `SELECT version FROM versions WHERE NOT blob_only ORDER BY version`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func (r *ociRepo) wholeOrNothing() {
	r.t.Helper()
	if n := r.f.count(r.t, `SELECT count(*) FROM version_children vc WHERE NOT EXISTS(SELECT 1 FROM versions v WHERE v.component_id=vc.component_id AND v.version=vc.child_version)`); n != 0 {
		r.t.Fatalf("%d index children are missing", n)
	}
}

func sorted(in ...string) []string { s := slices.Clone(in); slices.Sort(s); return s }

func TestKeepLastRanksTopLevelAndImagesStayWhole(t *testing.T) {
	r := newOCIRepo(t, "local")
	ctx := context.Background()
	oldIdx, oldKids := r.multiArch("old", 100, -1)
	newIdx, newKids := r.multiArch("new", 50, -1)
	if _, err := r.f.svc.SetPolicy(ctx, Policy{SourceID: r.src, KeepLast: 1, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	dry, err := r.f.svc.ApplyRetention(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	wantGone := sorted(append(slices.Clone(oldKids), oldIdx)...)
	var gotGone []string
	for _, d := range dry.Deleted {
		if d.Reason != "unreferenced_blob" {
			gotGone = append(gotGone, d.Version)
		}
	}
	if !slices.Equal(sorted(gotGone...), wantGone) {
		t.Fatalf("dry run manifests %v, want %v", sorted(gotGone...), wantGone)
	}
	rep, err := r.f.svc.ApplyRetention(ctx, false)
	if err != nil || len(rep.Deleted) != len(dry.Deleted) {
		t.Fatalf("real run %d vs dry %d: %v", len(rep.Deleted), len(dry.Deleted), err)
	}
	if got, want := r.visible(), sorted(append(slices.Clone(newKids), newIdx)...); !slices.Equal(got, want) {
		t.Fatalf("visible after: %v want %v", got, want)
	}
	r.wholeOrNothing()
	// The older image's config and layer blobs were released with it, the newer image's were not.
	if n := r.f.count(t, `SELECT count(*) FROM versions WHERE blob_only`); n != 8 {
		t.Fatalf("blob rows left: %d (want the newer image's 4 configs + 4 layers)", n)
	}
}

func TestSharedChildSurvivesUntilBothIndexesAreGone(t *testing.T) {
	r := newOCIRepo(t, "local")
	ctx := context.Background()
	shared := r.image("shared", 100, -1, 1)
	onlyA := r.image("only-a", 100, -1, 1)
	onlyB := r.image("only-b", 80, -1, 1)
	idxA := r.index("a", 100, -1, shared, onlyA)
	idxB := r.index("b", 80, -1, shared, onlyB)
	idxC := r.index("c", 10, -1, r.image("only-c", 10, -1, 1))
	if _, err := r.f.svc.SetPolicy(ctx, Policy{SourceID: r.src, KeepLast: 2, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.svc.ApplyRetention(ctx, false); err != nil {
		t.Fatal(err)
	}
	vis := r.visible()
	if slices.Contains(vis, idxA) || slices.Contains(vis, onlyA) {
		t.Fatalf("index A and its own child should be gone: %v", vis)
	}
	if !slices.Contains(vis, shared) || !slices.Contains(vis, idxB) || !slices.Contains(vis, onlyB) {
		t.Fatalf("shared child or B lost while B is kept: %v", vis)
	}
	r.wholeOrNothing()
	// Shrink to one: B goes, and with it the shared child.
	if _, err := r.f.svc.SetPolicy(ctx, Policy{SourceID: r.src, KeepLast: 1, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.svc.ApplyRetention(ctx, false); err != nil {
		t.Fatal(err)
	}
	vis = r.visible()
	if slices.Contains(vis, shared) || slices.Contains(vis, idxB) || slices.Contains(vis, onlyB) || !slices.Contains(vis, idxC) {
		t.Fatalf("after both indexes are gone: %v", vis)
	}
	r.wholeOrNothing()
}

func TestIdleEvictionFollowsTheIndex(t *testing.T) {
	r := newOCIRepo(t, "local")
	ctx := context.Background()
	// Image "busy": index and three children are old, but one platform manifest was just pulled.
	var busyKids []string
	for i, p := range []string{"amd64", "arm64", "s390x"} {
		idle := 500
		if i == 0 {
			idle = 1
		}
		busyKids = append(busyKids, r.image("busy/"+p, 500, idle, 1))
	}
	busy := r.index("busy", 500, 500, busyKids...)
	idleIdx, idleKids := r.multiArch("idle", 500, 500)
	if _, err := r.f.svc.SetPolicy(ctx, Policy{SourceID: r.src, MaxIdle: 48 * time.Hour, MinKeep: 0, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.svc.ApplyRetention(ctx, false); err != nil {
		t.Fatal(err)
	}
	vis := r.visible()
	for _, keep := range append(slices.Clone(busyKids), busy) {
		if !slices.Contains(vis, keep) {
			t.Fatalf("busy image lost %s: %v", keep, vis)
		}
	}
	for _, gone := range append(slices.Clone(idleKids), idleIdx) {
		if slices.Contains(vis, gone) {
			t.Fatalf("idle image kept %s", gone)
		}
	}
	r.wholeOrNothing()
}

func TestPressureNeverSplitsAnImage(t *testing.T) {
	r := newOCIRepo(t, "remote")
	ctx := context.Background()
	r.multiArch("a", 300, 300)
	r.multiArch("b", 200, 200)
	r.multiArch("c", 100, 100)
	r.multiArch("d", 5, 0)
	var used int64
	if err := r.f.pool.QueryRow(ctx, `SELECT sum(size_bytes) FROM blobs`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	r.f.pressureOn(t, used*2, 40, 30)
	rep, err := r.f.svc.RunPressure(ctx, false)
	if err != nil || rep.EvictedVersions == 0 {
		t.Fatalf("expected evictions: %+v %v", rep, err)
	}
	r.wholeOrNothing()
	// Every remaining index still has all of its platform manifests, and every
	// remaining platform manifest has an index or is top-level on its own.
	if n := r.f.count(t, `SELECT count(*) FROM version_children vc WHERE NOT EXISTS(SELECT 1 FROM versions v WHERE v.id=vc.parent_version_id)`); n != 0 {
		t.Fatalf("orphan child links: %d", n)
	}
	if n := r.f.count(t, `SELECT count(*) FROM versions v WHERE NOT v.blob_only AND NOT EXISTS(SELECT 1 FROM version_children vc WHERE vc.component_id=v.component_id AND vc.child_version=v.version) AND NOT EXISTS(SELECT 1 FROM version_children vc WHERE vc.parent_version_id=v.id)`); n != 0 {
		t.Fatalf("%d platform manifests were evicted from under their index but linger on their own", n)
	}
}
