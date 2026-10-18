package cargo

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
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestCargoPullThrough(t *testing.T) {
	f := remotetest.New(t)
	crate := []byte("CRATE-BYTES")
	var staticHits, indexHits atomic.Int32
	var down atomic.Bool
	static := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		staticHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the download origin")
		}
		if r.URL.Path == "/crates/my_crate/my_crate-1.2.3.crate" {
			_, _ = w.Write(crate)
			return
		}
		http.NotFound(w, r)
	}))
	index := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		indexHits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/config.json":
			fmt.Fprintf(w, `{"dl":"%s/crates/{crate}/{crate}-{version}.crate","api":"https://crates.example"}`, static.URL)
		case "/my/_c/my_crate":
			fmt.Fprintf(w, `{"name":"my_crate","vers":"1.2.3","deps":[],"cksum":"%s","features":{},"yanked":false}`+"\n"+`{"name":"my_crate","vers":"1.2.4","deps":[],"cksum":"nothex","features":{},"yanked":false}`+"\n"+`{"name":"other","vers":"1.0.0","deps":[],"cksum":"%s","features":{},"yanked":false}`+"\n", hexSum(crate), hexSum(crate))
		case "/ow/ne/owned":
			t.Errorf("locally owned crate requested upstream")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("crates", "cargo")
	local := f.AddLocal(feed, "cargo", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "cargo", "crates-io", 1, index.URL, remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{static.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/crates/cargo/"

	// Local owner: publish "owned" locally; the upstream is never consulted for it.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m := metadata{Name: "owned", Version: "1.0.0"}
	req, _ := http.NewRequest("PUT", base+"api/v1/crates/new", strings.NewReader(string(wire(m, crateArchive(t, "owned", "1.0.0", "")))))
	req.Header.Set("Authorization", admin.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("publish: %v %v", err, res)
	}
	res.Body.Close()
	if status, _, _ := f.Get(base + indexPath("owned")); status != 200 {
		t.Fatalf("local index %d", status)
	}

	// sparse index: invalid lines dropped, mixed-case/underscore names accepted
	status, body, header := f.Get(base + indexPath("my_crate"))
	if status != 200 || !strings.Contains(body, `"vers":"1.2.3"`) || strings.Contains(body, "nothex") || strings.Contains(body, `"other"`) || header.Get("ETag") == "" {
		t.Fatalf("index: %d %s", status, body)
	}
	if status, _, _ = f.Get(base+indexPath("my_crate"), "If-None-Match", header.Get("ETag")); status != 304 {
		t.Fatalf("conditional: %d", status)
	}
	if status, _, _ = f.Get(base + indexPath("nonexistent")); status != 404 {
		t.Fatalf("unknown crate: %d", status)
	}

	// download: miss -> verify against cksum -> cache -> hit with no upstream traffic
	dl := base + "api/v1/crates/my_crate/1.2.3/download"
	if status, body, _ = f.Get(dl); status != 200 || body != string(crate) {
		t.Fatalf("download miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d assets, want crate+index", f.CachedAssets(remote))
	}
	n := staticHits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(crate) || staticHits.Load() != n {
		t.Fatalf("download hit: %d", status)
	}
	if status, _, _ = f.Get(base + "api/v1/crates/my_crate/9.9.9/download"); status != 404 {
		t.Fatalf("version missing upstream: %d", status)
	}
	// local owner precedence: nothing upstream, owned versions only
	before := indexHits.Load()
	if status, _, _ = f.Get(base + "api/v1/crates/owned/2.0.0/download"); status != 404 || indexHits.Load() != before {
		t.Fatalf("owned remote version reachable: %d", status)
	}

	// outage: cached crate still downloads; cached index line lists the cached version offline
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(crate) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, body, header = f.Get(base + indexPath("my_crate")); status != 200 || !strings.Contains(body, `"vers":"1.2.3"`) {
		t.Fatalf("index in outage: %d %s", status, body)
	}
	_ = header
	if status, _, _ = f.Get(base + indexPath("uncached")); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
