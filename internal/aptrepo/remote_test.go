package aptrepo

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func TestAPTPullThrough(t *testing.T) {
	f := remotetest.New(t)
	debBytes := []byte("DEB-BYTES")
	const pool = "pool/main/h/hello/hello_1.0-1_amd64.deb"
	packages := []byte(fmt.Sprintf("Package: hello\nVersion: 1.0-1\nArchitecture: amd64\nFilename: %s\nSize: %d\nSHA256: %s\nDescription: x\n", pool, len(debBytes), hexSum(debBytes)))
	gz := deterministicGzip(packages)
	release := func(sha string, n int) string {
		return fmt.Sprintf("Origin: Debian\nSuite: stable\nAcquire-By-Hash: yes\nArchitectures: amd64\nComponents: main\nSHA256:\n %s %d main/binary-amd64/Packages.gz\n", sha, n)
	}
	var hits atomic.Int32
	var down atomic.Bool
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/debian/dists/stable/Release":
			_, _ = w.Write([]byte(release(hexSum(gz), len(gz))))
		case "/debian/dists/stable/InRelease":
			_, _ = w.Write([]byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\n" + release(hexSum(gz), len(gz)) + "-----BEGIN PGP SIGNATURE-----\n\nxx\n-----END PGP SIGNATURE-----\n"))
		case "/debian/dists/stable/Release.gpg":
			_, _ = w.Write([]byte("SIG"))
		case "/debian/dists/stable/main/binary-amd64/by-hash/SHA256/" + hexSum(gz):
			_, _ = w.Write(gz)
		case "/debian/dists/evil/Release":
			_, _ = w.Write([]byte(release(hexSum(gz), len(gz))))
		case "/debian/dists/evil/main/binary-amd64/by-hash/SHA256/" + hexSum(gz):
			_, _ = w.Write([]byte("TAMPERED"))
		case "/debian/" + pool:
			_, _ = w.Write(debBytes)
		case "/debian/dists/local/Release":
			t.Errorf("locally owned distribution requested upstream")
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("debian", "apt")
	local := f.AddLocal(feed, "apt", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "apt", "debian", 1, up.URL+"/debian", remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/debian/apt/"

	// A distribution owned locally is served locally; upstream is never asked.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ap, err := (identity.Tokens{DB: f.Pool}).Authenticate(context.Background(), admin.Secret)
	if err != nil {
		t.Fatal(err)
	}
	localRelease := []byte("Suite: local\n")
	for _, pub := range []artifact.Publication{{FeedID: feed, Namespace: nsDists, Coordinate: distCoord("local"), Version: "000000000001", AssetKey: "Release", MediaType: "text/plain", Info: info(localRelease)}, {FeedID: feed, Namespace: nsDists, Coordinate: hashCoord("local"), Version: sum(localRelease), AssetKey: "blob", MediaType: "text/plain", Info: info(localRelease)}} {
		if _, err = f.Service.Publish(context.Background(), ap, pub, bytes.NewReader(localRelease)); err != nil {
			t.Fatal(err)
		}
	}
	before := hits.Load()
	if status, body, _ := f.Get(base + "dists/local/Release"); status != 200 || body != string(localRelease) || hits.Load() != before {
		t.Fatalf("local dist: %d %q hits+%d", status, body, hits.Load()-before)
	}

	// deb before any index was served: unknown to this feed
	if status, _, _ := f.Get(base + pool); status != 404 {
		t.Fatalf("unlisted deb: %d", status)
	}
	// Release / InRelease / Release.gpg verbatim
	status, body, _ := f.Get(base + "dists/stable/Release")
	if status != 200 || body != release(hexSum(gz), len(gz)) {
		t.Fatalf("Release not verbatim: %d %q", status, body)
	}
	if status, body, _ = f.Get(base + "dists/stable/InRelease"); status != 200 || !strings.Contains(body, "BEGIN PGP SIGNED MESSAGE") {
		t.Fatalf("InRelease: %d", status)
	}
	if status, body, _ = f.Get(base + "dists/stable/Release.gpg"); status != 200 || body != "SIG" {
		t.Fatalf("Release.gpg: %d", status)
	}
	// index fetched via the listed path is served from upstream by-hash, verified
	idx := base + "dists/stable/main/binary-amd64/Packages.gz"
	if status, body, _ = f.Get(idx); status != 200 || body != string(gz) {
		t.Fatalf("Packages.gz: %d", status)
	}
	if status, body, _ = f.Get(base + "dists/stable/main/binary-amd64/by-hash/SHA256/" + hexSum(gz)); status != 200 || body != string(gz) {
		t.Fatalf("by-hash: %d", status)
	}
	// unlisted file under dists
	if status, _, _ = f.Get(base + "dists/stable/main/binary-amd64/Packages.xz"); status != 404 {
		t.Fatalf("unlisted index: %d", status)
	}
	// tampered upstream index is rejected and never cached
	f.Get(base + "dists/evil/Release")
	n := f.CachedAssets(remote)
	if status, _, _ = f.Get(base + "dists/evil/main/binary-amd64/Packages.gz"); status == 200 {
		t.Fatalf("tampered index served")
	}
	if f.CachedAssets(remote) != n {
		t.Fatalf("tampered index cached")
	}

	// deb: found via the cached index, verified, cached, then a local hit
	if status, body, _ = f.Get(base + pool); status != 200 || body != string(debBytes) {
		t.Fatalf("deb miss: %d %q", status, body)
	}
	h := hits.Load()
	if status, body, _ = f.Get(base + pool); status != 200 || body != string(debBytes) || hits.Load() != h {
		t.Fatalf("deb hit went upstream: %d", status)
	}
	// a pool path not listed by any index is not fetched
	if status, _, _ = f.Get(base + "pool/main/h/hello/hello_9.9-1_amd64.deb"); status != 404 {
		t.Fatalf("unlisted deb: %d", status)
	}

	// outage: cached deb and index continue; Release is stale
	down.Store(true)
	if status, _, _ = f.Get(base + pool); status != 200 {
		t.Fatalf("cached deb in outage: %d", status)
	}
	if status, _, _ = f.Get(idx); status != 200 {
		t.Fatalf("cached index in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "dists/stable/Release"); status != 200 || body == "" {
		t.Fatalf("stale Release: %d", status)
	}
	if status, _, _ = f.Get(base + "dists/other/Release"); status != 503 {
		t.Fatalf("uncached dist in outage: %d", status)
	}
}
