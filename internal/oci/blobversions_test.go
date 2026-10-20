package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

const (
	bvIndex = "application/vnd.oci.image.index.v1+json"
	bvImage = "application/vnd.oci.image.manifest.v1+json"
)

// bvUpstream serves manifests (by tag and digest) and blobs of repository team/app.
type bvUpstream struct {
	manifests map[string]string // path suffix -> body
	blobs     map[string]string // hex digest -> body
	blobHits  atomic.Int32
	srv       *httptest.Server
}

func newBVUpstream(f *remotetest.Fixture) *bvUpstream {
	u := &bvUpstream{manifests: map[string]string{}, blobs: map[string]string{}}
	u.srv = f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/v2/team/app/"
		p := r.URL.Path
		if len(p) > len(base+"manifests/") && p[:len(base+"manifests/")] == base+"manifests/" {
			ref := p[len(base+"manifests/"):]
			if body, ok := u.manifests[ref]; ok {
				if len(body) > 0 && contains(body, bvIndex) {
					w.Header().Set("Content-Type", bvIndex)
				} else {
					w.Header().Set("Content-Type", bvImage)
				}
				_, _ = w.Write([]byte(body))
				return
			}
		}
		if len(p) > len(base+"blobs/sha256:") && p[:len(base+"blobs/sha256:")] == base+"blobs/sha256:" {
			if body, ok := u.blobs[p[len(base+"blobs/sha256:"):]]; ok {
				u.blobHits.Add(1)
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	return u
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// image adds an image manifest with its own config and layer, returning its
// digest and the blob digests it names.
func (u *bvUpstream) image(seed string) (manifest string, blobs []string) {
	cfg, layer := "config-"+seed, "layer-"+seed+"-bytes-bytes-bytes"
	u.blobs[sha(cfg)], u.blobs[sha(layer)] = cfg, layer
	body := `{"schemaVersion":2,"mediaType":"` + bvImage + `","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` + sha(cfg) + `","size":` + itoa(len(cfg)) + `},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:` + sha(layer) + `","size":` + itoa(len(layer)) + `}]}`
	u.manifests["sha256:"+sha(body)] = body
	return sha(body), []string{sha(cfg), sha(layer)}
}

func bvRegistry(t *testing.T, f *remotetest.Fixture, up *bvUpstream) *httptest.Server {
	t.Helper()
	feed := f.AddFeed("bv", "oci")
	f.AddRemote(feed, "oci", "bv-remote", 0, up.srv.URL, remotetest.RemoteOptions{})
	srv := httptest.NewServer(mirrorRegistry(t, f))
	t.Cleanup(srv.Close)
	return srv
}

func bvCount(t *testing.T, f *remotetest.Fixture, where string) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM versions v WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPullThroughMultiArchYieldsOnlyManifestVersions(t *testing.T) {
	f := remotetest.New(t)
	up := newBVUpstream(f)
	var all []string
	var children []string
	for _, seed := range []string{"amd64", "attest-amd64", "arm64", "attest-arm64"} {
		m, blobs := up.image(seed)
		children = append(children, m)
		all = append(all, blobs...)
	}
	entry := func(d, platform string) string {
		return `{"mediaType":"` + bvImage + `","digest":"sha256:` + d + `","size":10,"platform":{"os":"linux","architecture":"` + platform + `"}}`
	}
	index := `{"schemaVersion":2,"mediaType":"` + bvIndex + `","manifests":[` + entry(children[0], "amd64") + `,` + entry(children[1], "unknown") + `,` + entry(children[2], "arm64") + `,` + entry(children[3], "unknown") + `]}`
	up.manifests["latest"] = index
	up.manifests["sha256:"+sha(index)] = index
	srv := bvRegistry(t, f, up)
	base := srv.URL + "/v2/bv/team/app/"
	if status, _, _ := f.Get(base+"manifests/latest", "Accept", bvIndex); status != 200 {
		t.Fatalf("index: %d", status)
	}
	for _, c := range children {
		if status, _, _ := f.Get(base+"manifests/sha256:"+c, "Accept", bvImage); status != 200 {
			t.Fatalf("child: %d", status)
		}
	}
	for _, b := range all {
		if status, _, _ := f.Get(base + "blobs/sha256:" + b); status != 200 {
			t.Fatalf("blob: %d", status)
		}
	}
	if n := bvCount(t, f, "NOT v.blob_only"); n != 5 {
		t.Fatalf("visible versions = %d, want 1 index + 4 children", n)
	}
	if n := bvCount(t, f, "v.blob_only"); n != len(all) {
		t.Fatalf("blob versions = %d, want %d", n, len(all))
	}
	for _, b := range all {
		if bvCount(t, f, "NOT v.blob_only AND v.version='"+b+"'") != 0 {
			t.Fatal("a blob is a visible version")
		}
	}
	var children2 int
	if err := f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM version_children`).Scan(&children2); err != nil || children2 != 4 {
		t.Fatalf("children linked: %d %v", children2, err)
	}

	// Blob GET/HEAD/range are served from cache without another upstream fetch.
	layer := all[1]
	hits := up.blobHits.Load()
	status, body, header := f.Get(base+"blobs/sha256:"+layer, "Range", "bytes=2-6")
	if status != 206 || body != up.blobs[layer][2:7] || header.Get("X-Kuvryn-Cache") != "hit" {
		t.Fatalf("range: %d %q %v", status, body, header)
	}
	req, _ := http.NewRequest(http.MethodHead, base+"blobs/sha256:"+layer, nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 || res.ContentLength != int64(len(up.blobs[layer])) {
		t.Fatalf("head: %d %d", res.StatusCode, res.ContentLength)
	}
	if up.blobHits.Load() != hits {
		t.Fatal("cached blob refetched from upstream")
	}
}

func TestPullThroughSingleArchAndDigestPull(t *testing.T) {
	f := remotetest.New(t)
	up := newBVUpstream(f)
	m, blobs := up.image("solo")
	up.manifests["v1"] = up.manifests["sha256:"+m]
	srv := bvRegistry(t, f, up)
	base := srv.URL + "/v2/bv/team/app/"
	if status, _, _ := f.Get(base+"manifests/v1", "Accept", bvImage); status != 200 {
		t.Fatalf("tag: %d", status)
	}
	for _, b := range blobs {
		f.Get(base + "blobs/sha256:" + b)
	}
	if n := bvCount(t, f, "NOT v.blob_only"); n != 1 {
		t.Fatalf("single-arch tag pull: %d versions", n)
	}

	// A platform manifest pulled by digest alone is one tag-less version.
	f2 := remotetest.New(t)
	up2 := newBVUpstream(f2)
	m2, blobs2 := up2.image("bydigest")
	srv2 := bvRegistry(t, f2, up2)
	base2 := srv2.URL + "/v2/bv/team/app/"
	if status, _, _ := f2.Get(base2+"manifests/sha256:"+m2, "Accept", bvImage); status != 200 {
		t.Fatalf("digest: %d", status)
	}
	for _, b := range blobs2 {
		f2.Get(base2 + "blobs/sha256:" + b)
	}
	if n := bvCount(t, f2, "NOT v.blob_only AND v.version='"+m2+"' AND NOT EXISTS(SELECT 1 FROM tags t WHERE t.version_id=v.id)"); n != 1 || bvCount(t, f2, "NOT v.blob_only") != 1 {
		t.Fatalf("digest pull: %d", n)
	}
}

func TestLocalPushMarksBlobsOnly(t *testing.T) {
	f := setup(t)
	cfg, layer := []byte("{}"), []byte("local-layer-bytes")
	cd, ld := f.pushBlob(t, "team/app", cfg), f.pushBlob(t, "team/app", layer)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"` + imageManifest + `","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cd + `","size":2},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + ld + `","size":17}]}`)
	assertCode(t, f.request("PUT", "/v2/images/team/app/manifests/v1", manifest, map[string]string{"Content-Type": imageManifest}), 201)
	var visible, hidden int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE NOT blob_only),count(*) FILTER (WHERE blob_only) FROM versions`).Scan(&visible, &hidden); err != nil || visible != 1 || hidden != 2 {
		t.Fatalf("local push visible=%d hidden=%d %v", visible, hidden, err)
	}
}

func TestRepairBlobVersions(t *testing.T) {
	f := remotetest.New(t)
	up := newBVUpstream(f)
	m, blobs := up.image("repair")
	srv := bvRegistry(t, f, up)
	base := srv.URL + "/v2/bv/team/app/"
	f.Get(base+"manifests/sha256:"+m, "Accept", bvImage)
	for _, b := range blobs {
		f.Get(base + "blobs/sha256:" + b)
	}
	// A manifest that reached the store through the blob path stays a version.
	odd, _ := up.image("odd")
	up.blobs[odd] = up.manifests["sha256:"+odd]
	f.Get(base + "blobs/sha256:" + odd)
	ctx := context.Background()
	// Reproduce pre-fix rows: nothing is flagged.
	if _, err := f.Pool.Exec(ctx, `UPDATE versions SET blob_only=false`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(ctx, `UPDATE assets SET last_accessed_at=now()-interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	var accessBefore string
	_ = f.Pool.QueryRow(ctx, `SELECT string_agg(a.id::text||coalesce(a.last_accessed_at::text,''),',' ORDER BY a.id) FROM assets a`).Scan(&accessBefore)

	dry, err := RepairBlobVersions(ctx, f.Pool, f.Service.Storage, BlobRepairOptions{DryRun: true})
	if err != nil || dry.Converted != 2 || dry.ManifestsKept != 1 || bvCount(t, f, "v.blob_only") != 0 {
		t.Fatalf("dry run %+v %v", dry, err)
	}
	if len(dry.Feeds) != 1 || dry.Feeds[0].Feed != "bv" || dry.Feeds[0].Converted != 2 || dry.Feeds[0].Components[0].Coordinate != "team/app" {
		t.Fatalf("per feed report %+v", dry.Feeds)
	}
	if other, err := RepairBlobVersions(ctx, f.Pool, f.Service.Storage, BlobRepairOptions{DryRun: true, Feed: "nope"}); err != nil || other.Converted != 0 {
		t.Fatalf("feed filter %+v %v", other, err)
	}
	real, err := RepairBlobVersions(ctx, f.Pool, f.Service.Storage, BlobRepairOptions{})
	if err != nil || real.Converted != dry.Converted {
		t.Fatalf("real %+v %v", real, err)
	}
	again, err := RepairBlobVersions(ctx, f.Pool, f.Service.Storage, BlobRepairOptions{})
	if err != nil || again.Converted != 0 {
		t.Fatalf("not idempotent %+v %v", again, err)
	}
	if n := bvCount(t, f, "NOT v.blob_only"); n != 2 { // the image manifest and the misfiled manifest
		t.Fatalf("visible after repair: %d", n)
	}
	var accessAfter string
	_ = f.Pool.QueryRow(ctx, `SELECT string_agg(a.id::text||coalesce(a.last_accessed_at::text,''),',' ORDER BY a.id) FROM assets a`).Scan(&accessAfter)
	if accessAfter != accessBefore {
		t.Fatal("repair changed access timestamps")
	}
	for _, b := range blobs {
		if status, _, _ := f.Get(base + "blobs/sha256:" + b); status != 200 {
			t.Fatalf("referenced blob lost: %d", status)
		}
	}
	var audits int
	_ = f.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='repair.oci_blob_versions'`).Scan(&audits)
	if audits != 1 {
		t.Fatalf("summary audit events: %d", audits)
	}
}

// Whatever path inserts the asset (publish, pull-through, replication, import),
// the database marks OCI blob carriers; other formats and manifests are never
// marked, and a manifest stored under a blob's digest clears the mark.
func TestBlobCarrierMarkIsDerivedFromTheAsset(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var rawSource, domain, blobID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'rawsrc','local','raw') RETURNING id::text`, f.instance).Scan(&rawSource); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,'mark') RETURNING id::text`, f.instance).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,1,'k') RETURNING id::text`, f.instance, domain, sha("x")).Scan(&blobID); err != nil {
		t.Fatal(err)
	}
	add := func(source, coordinate, version, key string) bool {
		var component, vid string
		if err := f.pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,'') ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate RETURNING id::text`, f.instance, source, coordinate).Scan(&component); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) ON CONFLICT(component_id,version) DO UPDATE SET version=excluded.version RETURNING id::text`, f.instance, component, version).Scan(&vid); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,$3,$4)`, f.instance, vid, key, blobID); err != nil {
			t.Fatal(err)
		}
		var only bool
		if err := f.pool.QueryRow(ctx, `SELECT blob_only FROM versions WHERE id=$1`, vid).Scan(&only); err != nil {
			t.Fatal(err)
		}
		return only
	}
	if !add(f.source, "a/b", "v-blob", "blob") {
		t.Fatal("oci blob asset not marked")
	}
	if add(f.source, "a/b", "v-manifest", "manifest") {
		t.Fatal("manifest marked")
	}
	if add(rawSource, "a/b", "v-raw", "blob") {
		t.Fatal("non-oci blob key marked")
	}
	if add(f.source, "a/b", "v-blob", "manifest") {
		t.Fatal("mark not cleared by a manifest asset")
	}
}
