package swiftrepo

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

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestSwiftPullThrough(t *testing.T) {
	f := remotetest.New(t)
	archive := []byte("PK-fake-zip-bytes")
	manifest := "// swift-tools-version: 5.9\nimport PackageDescription\n"
	var hits atomic.Int32
	var down atomic.Bool
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if !strings.HasPrefix(r.Header.Get("Accept"), "application/vnd.swift.registry.v1+") {
			t.Errorf("accept header %q", r.Header.Get("Accept"))
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/acme/widget":
			fmt.Fprint(w, `{"releases":{"1.0.0":{"url":"https://upstream.example/acme/widget/1.0.0"},"1.1.0":{},"bad":{}}}`)
		case "/acme/widget/1.0.0":
			fmt.Fprintf(w, `{"id":"acme.widget","version":"1.0.0","resources":[{"name":"source-archive","type":"application/zip","checksum":"%s","signing":{"signatureBase64Encoded":"c2ln","signatureFormat":"cms-1.0.0"}}],"metadata":{"description":"w"}}`, sum(archive))
		case "/acme/widget/1.1.0":
			fmt.Fprintf(w, `{"id":"acme.widget","version":"1.1.0","resources":[{"name":"source-archive","type":"application/zip","checksum":"%s"}],"metadata":{}}`, sum(archive))
		case "/acme/widget/1.0.0.zip":
			_, _ = w.Write(archive)
		case "/acme/widget/1.1.0.zip":
			_, _ = w.Write([]byte("TAMPERED"))
		case "/acme/widget/1.0.0/Package.swift":
			_, _ = w.Write([]byte(manifest))
		case "/acme/owned", "/acme/owned/2.0.0":
			t.Errorf("locally owned package requested upstream")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("swifts", "swift")
	local := f.AddLocal(feed, "swift", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "swift", "registry", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/swifts/swift/"
	accept := []string{"Accept", "application/vnd.swift.registry.v1+json"}
	get := func(path string) (int, string, http.Header) { return f.Get(base+path, accept...) }

	// list: URLs point at the feed, invalid versions dropped
	status, body, h := get("acme/widget")
	if status != 200 || strings.Contains(body, "upstream.example") || !strings.Contains(body, base+"acme/widget/1.1.0") || strings.Contains(body, "bad") || !strings.Contains(h.Get("Link"), "latest-version") {
		t.Fatalf("list: %d %s %v", status, body, h)
	}
	if status, _, _ = get("acme/missing"); status != 404 {
		t.Fatalf("unknown: %d", status)
	}
	// release resource keeps checksum and signing
	if status, body, _ = get("acme/widget/1.0.0"); status != 200 || !strings.Contains(body, sum(archive)) || !strings.Contains(body, "cms-1.0.0") {
		t.Fatalf("release: %d %s", status, body)
	}
	// archive: miss -> verified -> cached -> hit without upstream traffic
	dl := "acme/widget/1.0.0.zip"
	status, body, h = get(dl)
	if status != 200 || body != string(archive) {
		t.Fatalf("miss: %d %q", status, body)
	}
	if h.Get("Digest") == "" || h.Get("X-Swift-Package-Signature") != "c2ln" {
		t.Fatalf("headers lost: %v", h)
	}
	if f.CachedAssets(remote) < 2 {
		t.Fatal("archive and release record not cached")
	}
	n := hits.Load()
	if status, body, _ = get(dl); status != 200 || body != string(archive) || hits.Load() != n {
		t.Fatalf("hit: %d hits=%d/%d", status, hits.Load(), n)
	}
	// manifest
	if status, body, _ = get("acme/widget/1.0.0/Package.swift"); status != 200 || body != manifest {
		t.Fatalf("manifest: %d %q", status, body)
	}
	// checksum mismatch is rejected and not cached
	if status, body, _ = get("acme/widget/1.1.0.zip"); status == 200 || strings.Contains(body, "TAMPERED") {
		t.Fatalf("tampered archive served: %d", status)
	}
	if status, _, _ = get("acme/widget/9.9.9.zip"); status != 404 {
		t.Fatalf("missing version: %d", status)
	}
	// a locally owned package is never completed from upstream
	if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Namespace: "acme", Coordinate: "acme.owned", Version: "1.0.0", AssetKey: "archive", MediaType: "application/zip", Info: blob.Info{Digest: blob.Digest(sum([]byte("x"))), Size: 1}}, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = get("acme/owned/2.0.0.zip"); status != 404 {
		t.Fatalf("owned name: %d", status)
	}
	// outage: cached version still listed and downloaded; uncached is 503
	down.Store(true)
	if status, body, h = get("acme/widget"); status != 200 || !strings.Contains(body, "1.0.0") {
		t.Fatalf("list in outage: %d %s", status, body)
	}
	if status, body, _ = get(dl); status != 200 || body != string(archive) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, _, _ = get("acme/uncached"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
