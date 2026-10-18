package pubrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestPubPullThrough(t *testing.T) {
	f := remotetest.New(t)
	archive := []byte("ARCHIVE-BYTES")
	bad := []byte("TAMPERED")
	var cdnHits, apiHits atomic.Int32
	var down atomic.Bool
	cdn := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the CDN origin")
		}
		switch r.URL.Path {
		case "/foo-1.0.0.tar.gz":
			_, _ = w.Write(archive)
		case "/foo-1.1.0.tar.gz":
			_, _ = w.Write(bad)
		default:
			http.NotFound(w, r)
		}
	}))
	api := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/api/packages/foo":
			v := func(ver, sha, url string) string {
				return fmt.Sprintf(`{"version":"%s","pubspec":{"name":"foo","version":"%s"},"archive_url":"%s","archive_sha256":"%s","published":"2024-01-01T00:00:00Z"}`, ver, ver, url, sha)
			}
			fmt.Fprintf(w, `{"name":"foo","isDiscontinued":false,"latest":%s,"versions":[%s,%s,%s]}`,
				v("1.1.0", hexSum(archive), cdn.URL+"/foo-1.1.0.tar.gz"),
				v("1.0.0", hexSum(archive), cdn.URL+"/foo-1.0.0.tar.gz"),
				v("1.1.0", hexSum(archive), cdn.URL+"/foo-1.1.0.tar.gz"),
				v("bad version", "zz", "https://x/"))
		case "/api/packages/owned":
			t.Errorf("locally owned package requested upstream")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("pubs", "pub")
	local := f.AddLocal(feed, "pub", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "pub", "pub-dev", 1, api.URL, remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{cdn.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/pubs/pub/"

	// metadata: URLs rewritten, invalid and duplicate versions dropped
	status, body, _ := f.Get(base + "api/packages/foo")
	if status != 200 || strings.Contains(body, cdn.URL) || !strings.Contains(body, base+"packages/foo/versions/1.0.0.tar.gz") || strings.Contains(body, "bad version") {
		t.Fatalf("index: %d %s", status, body)
	}
	if status, body, _ = f.Get(base + "api/packages/foo/versions/1.0.0"); status != 200 || !strings.Contains(body, base+"packages/foo/versions/1.0.0.tar.gz") {
		t.Fatalf("version: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "api/packages/missing"); status != 404 {
		t.Fatalf("unknown: %d", status)
	}

	// download: miss -> verified -> cached -> hit with no upstream traffic
	dl := base + "packages/foo/versions/1.0.0.tar.gz"
	if status, body, _ = f.Get(dl); status != 200 || body != string(archive) {
		t.Fatalf("miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) < 1 {
		t.Fatal("archive not cached")
	}
	n := cdnHits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(archive) || cdnHits.Load() != n {
		t.Fatalf("hit: %d", status)
	}
	// digest mismatch is rejected and not cached
	if status, body, _ = f.Get(base + "packages/foo/versions/1.1.0.tar.gz"); status == 200 || strings.Contains(body, "TAMPERED") {
		t.Fatalf("tampered archive served: %d", status)
	}
	if status, _, _ = f.Get(base + "packages/foo/versions/9.9.9.tar.gz"); status != 404 {
		t.Fatalf("missing version: %d", status)
	}

	// local owner precedence: nothing goes upstream for a locally owned name
	if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Coordinate: "owned", Version: "1.0.0", AssetKey: "archive", MediaType: "application/gzip", Info: blob.Info{Digest: blob.Digest(hexSum([]byte("x"))), Size: 1}}, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	before := apiHits.Load()
	if status, _, _ = f.Get(base + "packages/owned/versions/2.0.0.tar.gz"); status != 404 || apiHits.Load() != before {
		t.Fatalf("owned name reached upstream: %d", status)
	}

	// outage: cached archive still downloads and the cached version is listed offline
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(archive) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "api/packages/foo/versions/1.0.0"); status != 200 || !strings.Contains(body, `"version":"1.0.0"`) {
		t.Fatalf("index in outage: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "api/packages/uncached"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
