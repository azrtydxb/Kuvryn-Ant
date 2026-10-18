package rubygems

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

func TestRubygemsPullThrough(t *testing.T) {
	f := remotetest.New(t)
	gem := []byte("GEM-BYTES")
	bad := []byte("GEM-BAD")
	spec := []byte("GEMSPEC-RZ")
	var gemHits atomic.Int32
	var down atomic.Bool
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/info/my-gem":
			fmt.Fprintf(w, "---\n1.2.3 dep-a:>= 1&< 3,dep-b:>= 0|checksum:%s,ruby:>= 2.7.0,rubygems:>= 3.0\n1.2.4-java |checksum:nothex\n", hexSum(gem))
		case "/info/bad-gem":
			fmt.Fprintf(w, "---\n1.0.0 |checksum:%s\n", hexSum([]byte("something else")))
		case "/gems/my-gem-1.2.3.gem":
			gemHits.Add(1)
			_, _ = w.Write(gem)
		case "/gems/bad-gem-1.0.0.gem":
			_, _ = w.Write(bad)
		case "/quick/Marshal.4.8/my-gem-1.2.3.gemspec.rz":
			_, _ = w.Write(spec)
		case "/info/owned":
			t.Errorf("locally owned gem requested upstream")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("gems", "rubygems")
	local := f.AddLocal(feed, "rubygems", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "rubygems", "rubygems-org", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/gems/rubygems/"

	// Local owner: push "owned"; upstream is never consulted for it.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", base+"api/v1/gems", strings.NewReader(string(packageBytes(t, "owned", "1.0.0", "ruby", ""))))
	req.Header.Set("Authorization", admin.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("push: %v %v", err, res)
	}
	res.Body.Close()
	if status, body, _ := f.Get(base + "info/owned"); status != 200 || !strings.Contains(body, "1.0.0") {
		t.Fatalf("local info %d %s", status, body)
	}

	// info: invalid line dropped, checksum and deps kept verbatim
	status, body, _ := f.Get(base + "info/my-gem")
	if status != 200 || !strings.Contains(body, "1.2.3 dep-a:>= 1&< 3,dep-b:>= 0|checksum:"+hexSum(gem)) || strings.Contains(body, "nothex") {
		t.Fatalf("info: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "info/nonexistent"); status != 404 {
		t.Fatalf("unknown gem: %d", status)
	}

	// download: miss -> verify -> cache -> hit without upstream traffic
	dl := base + "gems/my-gem-1.2.3.gem"
	if status, body, _ = f.Get(dl); status != 200 || body != string(gem) {
		t.Fatalf("download miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d assets, want gem+metadata", f.CachedAssets(remote))
	}
	n := gemHits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(gem) || gemHits.Load() != n {
		t.Fatalf("download hit: %d", status)
	}
	if status, _, _ = f.Get(base + "gems/my-gem-9.9.9.gem"); status != 404 {
		t.Fatalf("version missing upstream: %d", status)
	}
	// legacy quick gemspec of a cached gem is pulled through too
	if status, body, _ = f.Get(base + "quick/Marshal.4.8/my-gem-1.2.3.gemspec.rz"); status != 200 || body != string(spec) {
		t.Fatalf("gemspec: %d %q", status, body)
	}
	// digest mismatch is rejected and nothing is cached
	cached := f.CachedAssets(remote)
	if status, body, _ = f.Get(base + "gems/bad-gem-1.0.0.gem"); status == 200 || strings.Contains(body, "GEM-BAD") {
		t.Fatalf("digest mismatch served: %d", status)
	}
	if f.CachedAssets(remote) != cached {
		t.Fatalf("mismatching gem was cached")
	}
	// local precedence: an owned name is never requested upstream (handler fails the test)
	if status, _, _ = f.Get(base + "gems/owned-2.0.0.gem"); status != 404 {
		t.Fatalf("owned remote version reachable: %d", status)
	}

	// outage: cached gem downloads, cached versions listed offline, uncached 503
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(gem) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "info/my-gem"); status != 200 || !strings.Contains(body, "1.2.3") {
		t.Fatalf("info in outage: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "info/uncached"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
