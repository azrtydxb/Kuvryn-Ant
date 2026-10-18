package condarepo

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

func TestCondaPullThrough(t *testing.T) {
	f := remotetest.New(t)
	bz := []byte("BZ2-BYTES")
	cd := []byte("CONDA-BYTES")
	bad := []byte("BAD-BYTES")
	var pkgHits atomic.Int32
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
		case "/linux-64/repodata.json":
			fmt.Fprintf(w, `{"info":{"subdir":"linux-64","base_url":"https://evil.example/"},"packages":{`+
				`"zlib-1.2.13-h0_0.tar.bz2":{"name":"zlib","version":"1.2.13","build":"h0_0","build_number":0,"subdir":"linux-64","depends":["libgcc-ng >=12"],"sha256":"%s","md5":"x","size":9},`+
				`"bad-1.0-0.tar.bz2":{"name":"bad","version":"1.0","build":"0","build_number":0,"subdir":"linux-64","depends":[],"sha256":"%s","size":9},`+
				`"nosha-1.0-0.tar.bz2":{"name":"nosha","version":"1.0","build":"0","subdir":"linux-64"},`+
				`"mismatch-1.0-0.tar.bz2":{"name":"other","version":"1.0","build":"0","subdir":"linux-64","sha256":"%s"}},`+
				`"packages.conda":{"numpy-1.26.0-py311_0.conda":{"name":"numpy","version":"1.26.0","build":"py311_0","subdir":"linux-64","depends":[],"sha256":"%s","size":11}}}`,
				hexSum(bz), hexSum([]byte("not what is served")), hexSum(bz), hexSum(cd))
		case "/linux-64/zlib-1.2.13-h0_0.tar.bz2":
			pkgHits.Add(1)
			_, _ = w.Write(bz)
		case "/linux-64/numpy-1.26.0-py311_0.conda":
			_, _ = w.Write(cd)
		case "/linux-64/bad-1.0-0.tar.bz2":
			_, _ = w.Write(bad)
		case "/osx-64/repodata.json":
			t.Errorf("locally owned subdir requested upstream")
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("packages", "conda")
	local := f.AddLocal(feed, "conda", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "conda", "conda-forge", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/packages/conda/"

	// Local owner of osx-64: publish there; upstream is never consulted for it.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pkg, filename := packageBytes(t, "demo", "2.0", "0", "osx-64")
	req, _ := http.NewRequest("PUT", base+"ingest/osx-64/"+filename, strings.NewReader(string(pkg)))
	req.Header.Set("Authorization", "Bearer "+admin.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("publish: %v %v", err, res)
	}
	res.Body.Close()
	if status, body, _ := f.Get(base + "osx-64/repodata.json"); status != 200 || !strings.Contains(body, filename) {
		t.Fatalf("local repodata %d %s", status, body)
	}

	// repodata: invalid entries dropped, base_url removed, conda packages listed
	status, body, _ := f.Get(base + "linux-64/repodata.json")
	if status != 200 || !strings.Contains(body, "zlib-1.2.13-h0_0.tar.bz2") || !strings.Contains(body, "numpy-1.26.0-py311_0.conda") || strings.Contains(body, "nosha") || strings.Contains(body, "mismatch") || strings.Contains(body, "base_url") || strings.Contains(body, "evil.example") {
		t.Fatalf("repodata: %d %s", status, body)
	}

	// download: miss -> verify -> cache -> hit with no upstream traffic
	dl := base + "linux-64/zlib-1.2.13-h0_0.tar.bz2"
	if status, body, _ = f.Get(dl); status != 200 || body != string(bz) {
		t.Fatalf("download miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d assets, want archive+metadata", f.CachedAssets(remote))
	}
	n := pkgHits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(bz) || pkgHits.Load() != n {
		t.Fatalf("download hit: %d", status)
	}
	if status, body, _ = f.Get(base + "linux-64/numpy-1.26.0-py311_0.conda"); status != 200 || body != string(cd) {
		t.Fatalf(".conda download: %d %q", status, body)
	}
	if status, _, _ = f.Get(base + "linux-64/missing-1.0-0.tar.bz2"); status != 404 {
		t.Fatalf("missing upstream: %d", status)
	}
	// digest mismatch rejected, nothing cached
	cached := f.CachedAssets(remote)
	if status, body, _ = f.Get(base + "linux-64/bad-1.0-0.tar.bz2"); status == 200 || strings.Contains(body, "BAD-BYTES") {
		t.Fatalf("digest mismatch served: %d", status)
	}
	if f.CachedAssets(remote) != cached {
		t.Fatalf("mismatching package was cached")
	}
	// local precedence: a package of the locally owned subdir is not fetched upstream
	if status, _, _ = f.Get(base + "osx-64/zlib-1.2.13-h0_0.tar.bz2"); status != 404 {
		t.Fatalf("owned subdir reachable upstream: %d", status)
	}

	// outage: cached packages still download; cached entry is listed offline
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(bz) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, body, _ := f.Get(base + "linux-64/repodata.json"); status != 200 || !strings.Contains(body, "zlib-1.2.13-h0_0.tar.bz2") {
		t.Fatalf("repodata in outage: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "noarch/repodata.json"); status != 503 {
		t.Fatalf("uncached subdir in outage: %d", status)
	}
}
