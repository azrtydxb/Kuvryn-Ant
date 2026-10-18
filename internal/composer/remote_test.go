package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func zipOf(t *testing.T, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	fw, err := zw.Create("lib/README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(content))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestComposerPullThrough(t *testing.T) {
	f := remotetest.New(t)
	archive := zipOf(t, "hello")
	other := zipOf(t, "other")
	sum := sha1.Sum(archive)
	var distHits atomic.Int32
	var down atomic.Bool
	dist := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the dist origin")
		}
		distHits.Add(1)
		switch r.URL.Path {
		case "/acme/lib/1.1.0.zip":
			_, _ = w.Write(archive)
		case "/acme/lib/dev-main.zip":
			_, _ = w.Write(other)
		case "/acme/html/1.0.0.zip":
			_, _ = w.Write([]byte("<html>rate limited</html>"))
		case "/acme/sum/1.0.0.zip":
			_, _ = w.Write(other)
		default:
			http.NotFound(w, r)
		}
	}))
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if strings.HasPrefix(r.URL.Path, "/p2/owned/") {
			t.Errorf("locally owned package requested upstream")
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/packages.json":
			_, _ = w.Write([]byte(`{"metadata-url":"/p2/%package%.json","available-packages":["acme/lib"]}`))
		case "/p2/acme/lib.json":
			fmt.Fprintf(w, `{"minified":"composer/2.0","packages":{"acme/lib":[`+
				`{"name":"acme/lib","version":"1.1.0","description":"Lib","type":"library","require":{"php":">=8.0"},"source":{"type":"git","url":"https://github.com/acme/lib.git","reference":"abc"},"dist":{"type":"zip","url":"%s/acme/lib/1.1.0.zip","reference":"abc","shasum":"%s"}},`+
				`{"version":"1.0.0","description":"__unset","dist":{"type":"zip","url":"%s/acme/lib/1.0.0.zip","reference":"def","shasum":""}},`+
				`{"version":"bad/ver"}]}}`, dist.URL, hex.EncodeToString(sum[:]), dist.URL)
		case "/p2/acme/lib~dev.json":
			fmt.Fprintf(w, `{"minified":"composer/2.0","packages":{"acme/lib":[{"name":"acme/lib","version":"dev-main","dist":{"type":"zip","url":"%s/acme/lib/dev-main.zip","reference":"zzz","shasum":""}}]}}`, dist.URL)
		case "/p2/acme/html.json":
			fmt.Fprintf(w, `{"packages":{"acme/html":[{"name":"acme/html","version":"1.0.0","dist":{"type":"zip","url":"%s/acme/html/1.0.0.zip","reference":"a","shasum":""}}]}}`, dist.URL)
		case "/p2/acme/sum.json":
			fmt.Fprintf(w, `{"packages":{"acme/sum":[{"name":"acme/sum","version":"1.0.0","dist":{"type":"zip","url":"%s/acme/sum/1.0.0.zip","reference":"a","shasum":"%s"}}]}}`, dist.URL, hex.EncodeToString(sum[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("packages", "composer")
	local := f.AddLocal(feed, "composer", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "composer", "packagist", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{dist.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/packages/composer/"

	// Local owner: publish "owned/pkg"; upstream is never asked about it.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", base+"ingest/owned/pkg/1.0.0", strings.NewReader(string(packageBytes(t, "owned/pkg", "1.0.0", ""))))
	req.Header.Set("Authorization", "Bearer "+admin.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("publish: %v %v", err, res)
	}
	res.Body.Close()
	if status, body, _ := f.Get(base + "p2/owned/pkg.json"); status != 200 || !strings.Contains(body, "1.0.0") {
		t.Fatalf("local index %d %s", status, body)
	}

	// metadata: expanded, dist rewritten to the feed, source dropped, bad version dropped
	status, body, _ := f.Get(base + "p2/acme/lib.json")
	feedDist := base + "dist/acme/lib/1.1.0/archive.zip"
	if status != 200 || !strings.Contains(body, feedDist) || !strings.Contains(body, base+"dist/acme/lib/1.0.0/archive.zip") || strings.Contains(body, dist.URL) || strings.Contains(body, "github.com") || strings.Contains(body, "bad/ver") || strings.Contains(body, "minified") {
		t.Fatalf("p2: %d %s", status, body)
	}
	// the second entry inherited "type" and lost "description" through __unset
	if strings.Count(body, `"type":"library"`) != 2 || strings.Count(body, `"description":"Lib"`) != 1 {
		t.Fatalf("minified expansion: %s", body)
	}
	if status, body, _ = f.Get(base + "p2/acme/lib~dev.json"); status != 200 || !strings.Contains(body, "dist/acme/lib/dev-main/archive.zip") {
		t.Fatalf("p2 dev: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "p2/acme/nonexistent.json"); status != 404 {
		t.Fatalf("unknown package: %d", status)
	}

	// dist: miss -> verified (zip + sha1) -> cached -> hit with no upstream traffic
	if status, body, _ = f.Get(feedDist); status != 200 || body != string(archive) {
		t.Fatalf("dist miss: %d", status)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d assets, want dist+entry", f.CachedAssets(remote))
	}
	n := distHits.Load()
	if status, body, _ = f.Get(feedDist); status != 200 || body != string(archive) || distHits.Load() != n {
		t.Fatalf("dist hit: %d", status)
	}
	if status, body, _ = f.Get(base + "dist/acme/lib/dev-main/archive.zip"); status != 200 || body != string(other) {
		t.Fatalf("dev dist: %d", status)
	}
	if status, _, _ = f.Get(base + "dist/acme/lib/9.9.9/archive.zip"); status != 404 {
		t.Fatalf("version missing upstream: %d", status)
	}
	// a non-zip body and a SHA-1 mismatch are rejected, nothing is cached
	cached := f.CachedAssets(remote)
	for _, p := range []string{"dist/acme/html/1.0.0/archive.zip", "dist/acme/sum/1.0.0/archive.zip"} {
		if status, body, _ = f.Get(base + p); status == 200 {
			t.Fatalf("%s served unverified: %q", p, body)
		}
	}
	if f.CachedAssets(remote) != cached {
		t.Fatalf("rejected archives were cached")
	}
	// local precedence: an owned package is never fetched from upstream
	if status, _, _ = f.Get(base + "dist/owned/pkg/2.0.0/archive.zip"); status != 404 {
		t.Fatalf("owned remote version reachable: %d", status)
	}

	// outage: cached archive downloads; cached versions are listed offline; uncached 503
	down.Store(true)
	if status, body, _ = f.Get(feedDist); status != 200 || body != string(archive) {
		t.Fatalf("dist in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "p2/acme/lib.json"); status != 200 || !strings.Contains(body, feedDist) {
		t.Fatalf("p2 in outage: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "p2/acme/uncached.json"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
