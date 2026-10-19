package oci

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

// A Docker Hub pull-through feed: both name forms address one library/ component,
// the tag a client resolved is recorded (and follows the tag when it moves),
// the index groups its platform manifests, and an observed tag never serves a
// request nor overwrites an authoritative tag.
func TestOCIPullThroughRecordsTagsAndGroupsIndexChildren(t *testing.T) {
	f := remotetest.New(t)
	const indexType = "application/vnd.oci.image.index.v1+json"
	const imageType = "application/vnd.oci.image.manifest.v1+json"
	child := func(seed string) string {
		return `{"schemaVersion":2,"mediaType":"` + imageType + `","config":{"digest":"sha256:` + sha("cfg"+seed) + `"},"layers":[]}`
	}
	c1, c2 := child("amd64"), child("arm64")
	index := func(extra string) string {
		return `{"schemaVersion":2,"mediaType":"` + indexType + `","manifests":[` +
			`{"mediaType":"` + imageType + `","digest":"sha256:` + sha(c1) + `","size":` + itoa(len(c1)) + `,"platform":{"os":"linux","architecture":"amd64"}},` +
			`{"mediaType":"` + imageType + `","digest":"sha256:` + sha(c2) + `","size":` + itoa(len(c2)) + `,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}` + extra + `]}`
	}
	var current atomic.Value
	current.Store(index(""))
	var paths atomic.Value
	paths.Store("")
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/library/alpine/manifests/latest" {
			paths.Store(r.URL.Path)
			w.Header().Set("Content-Type", indexType)
			_, _ = w.Write([]byte(current.Load().(string)))
			return
		}
		http.NotFound(w, r)
	}))
	feed := f.AddFeed("dockerhub", "oci")
	remote := f.AddRemote(feed, "oci", "hub-test", 0, up.URL, remotetest.RemoteOptions{MetadataTTL: 1})
	f.AddRemote(feed, "oci", "hub-origin", 1, "https://registry-1.docker.io", remotetest.RemoteOptions{})
	registry, err := New(f.Pool, f.Service, filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()

	// Short name: the official image is library/alpine upstream and locally.
	first := index("")
	status, _, header := f.Get(srv.URL+"/v2/dockerhub/alpine/manifests/latest", "Accept", indexType)
	if status != 200 || header.Get("Docker-Content-Digest") != "sha256:"+sha(first) || paths.Load() != "/v2/library/alpine/manifests/latest" {
		t.Fatalf("short name: %d %v upstream=%v", status, header, paths.Load())
	}
	// The long form resolves to the same component: no duplicate.
	if status, _, _ = f.Get(srv.URL+"/v2/dockerhub/library/alpine/manifests/latest", "Accept", indexType); status != 200 {
		t.Fatalf("long name: %d", status)
	}
	ctx := context.Background()
	var components int
	if err = f.Pool.QueryRow(ctx, `SELECT count(*) FROM components WHERE source_id=$1`, remote).Scan(&components); err != nil || components != 1 {
		t.Fatalf("components %d err=%v", components, err)
	}
	var coordinate string
	_ = f.Pool.QueryRow(ctx, `SELECT coordinate FROM components WHERE source_id=$1`, remote).Scan(&coordinate)
	if coordinate != "library/alpine" {
		t.Fatalf("coordinate %q", coordinate)
	}

	tagVersion := func() (string, bool) {
		var version string
		var observed bool
		err := f.Pool.QueryRow(ctx, `SELECT v.version,t.observed_at IS NOT NULL FROM tags t JOIN versions v ON v.id=t.version_id JOIN components c ON c.id=t.component_id WHERE c.source_id=$1 AND t.name='latest'`, remote).Scan(&version, &observed)
		if err != nil {
			t.Fatalf("tag row: %v", err)
		}
		return version, observed
	}
	if v, observed := tagVersion(); v != sha(first) || !observed {
		t.Fatalf("recorded tag %s observed=%v", v, observed)
	}
	var children int
	if err = f.Pool.QueryRow(ctx, `SELECT count(*) FROM version_children vc JOIN versions v ON v.id=vc.parent_version_id WHERE v.version=$1 AND vc.platform IN ('linux/amd64','linux/arm64/v8')`, sha(first)).Scan(&children); err != nil || children != 2 {
		t.Fatalf("children %d err=%v", children, err)
	}

	// An observed tag never serves a request.
	pr := f.Principal
	if _, err = f.Service.ResolveTag(ctx, pr, artifact.Publication{FeedID: feed, Namespace: "library", Coordinate: "library/alpine"}, "latest"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("observed tag resolved locally: %v", err)
	}

	// Re-resolving a moved tag moves the recorded mapping.
	moved := index(`,{"mediaType":"` + imageType + `","digest":"sha256:` + sha(child("s390x")) + `","size":10,"platform":{"os":"linux","architecture":"s390x"}}`)
	current.Store(moved)
	time.Sleep(1200 * time.Millisecond)
	if status, _, _ = f.Get(srv.URL+"/v2/dockerhub/alpine/manifests/latest", "Accept", indexType); status != 200 {
		t.Fatalf("moved: %d", status)
	}
	if v, _ := tagVersion(); v != sha(moved) {
		t.Fatalf("tag did not move: %s", v)
	}

	// An authoritative tag (no observed_at) is never overwritten by upstream.
	if _, err = f.Pool.Exec(ctx, `UPDATE tags SET observed_at=NULL WHERE name='latest' AND component_id IN (SELECT id FROM components WHERE source_id=$1)`, remote); err != nil {
		t.Fatal(err)
	}
	current.Store(first)
	time.Sleep(1200 * time.Millisecond)
	if status, _, _ = f.Get(srv.URL+"/v2/dockerhub/alpine/manifests/latest", "Accept", indexType); status != 200 {
		t.Fatalf("authoritative: %d", status)
	}
	if v, observed := tagVersion(); v != sha(moved) || observed {
		t.Fatalf("authoritative tag changed: %s observed=%v", v, observed)
	}
}

// Indexes stored before grouping was recorded are linked by the backfill, which
// reads manifest bytes from blob storage and is idempotent.
func TestBackfillIndexChildren(t *testing.T) {
	f := remotetest.New(t)
	const indexType = "application/vnd.oci.image.index.v1+json"
	const imageType = "application/vnd.oci.image.manifest.v1+json"
	body := `{"schemaVersion":2,"mediaType":"` + indexType + `","manifests":[{"mediaType":"` + imageType + `","digest":"sha256:` + sha("a") + `","size":5,"platform":{"os":"linux","architecture":"amd64"}}]}`
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/team/app/manifests/v1" {
			w.Header().Set("Content-Type", indexType)
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	feed := f.AddFeed("bf", "oci")
	f.AddRemote(feed, "oci", "bf-remote", 0, up.URL, remotetest.RemoteOptions{})
	registry, err := New(f.Pool, f.Service, filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	if status, _, _ := f.Get(srv.URL+"/v2/bf/team/app/manifests/v1", "Accept", indexType); status != 200 {
		t.Fatalf("pull: %d", status)
	}
	ctx := context.Background()
	count := func() int {
		var n int
		if err := f.Pool.QueryRow(ctx, `SELECT count(*) FROM version_children`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("children after pull: %d", count())
	}
	if _, err = f.Pool.Exec(ctx, `DELETE FROM version_children`); err != nil {
		t.Fatal(err)
	}
	report, err := BackfillIndexChildren(ctx, f.Pool, f.Service.Storage, true)
	if err != nil || report.Indexes != 1 || count() != 0 {
		t.Fatalf("dry run: %+v %v count=%d", report, err, count())
	}
	for i := 0; i < 2; i++ {
		if report, err = BackfillIndexChildren(ctx, f.Pool, f.Service.Storage, false); err != nil {
			t.Fatal(err)
		}
		if count() != 1 {
			t.Fatalf("pass %d: %+v count=%d", i, report, count())
		}
	}
}
