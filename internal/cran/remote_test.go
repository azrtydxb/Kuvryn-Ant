package cran

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func sum(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(h[:])), Size: int64(len(b))}
}

func TestCRANPullThrough(t *testing.T) {
	f := remotetest.New(t)
	packages := "Package: ggplot2\nVersion: 3.5.0\nDepends: R (>= 3.5)\nImports: rlang,\n        scales\nMD5sum: 0123456789abcdef0123456789abcdef\n\n" +
		"Package: mypkg\nVersion: 9.9.9\nMD5sum: ffffffffffffffffffffffffffffffff\n\n" +
		"Package: bad_name\nVersion: 1.0\n\n"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(packages))
	_ = zw.Close()
	tarball, archived, zipped := []byte("TARBALL"), []byte("OLD"), []byte("ZIP")
	var hits atomic.Int32
	var down atomic.Bool
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/src/contrib/PACKAGES.gz":
			_, _ = w.Write(gz.Bytes())
		case "/src/contrib/ggplot2_3.5.0.tar.gz":
			_, _ = w.Write(tarball)
		case "/src/contrib/Archive/ggplot2/ggplot2_3.4.0.tar.gz":
			_, _ = w.Write(archived)
		case "/bin/windows/contrib/4.4/PACKAGES":
			_, _ = w.Write([]byte("Package: ggplot2\nVersion: 3.5.0\n\n"))
		case "/bin/windows/contrib/4.4/ggplot2_3.5.0.zip":
			_, _ = w.Write(zipped)
		default:
			if strings.Contains(r.URL.Path, "mypkg") {
				t.Errorf("locally owned package requested upstream: %s", r.URL.Path)
			}
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("r", "cran")
	local := f.AddLocal(feed, "cran", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "cran", "cran-mirror", 1, up.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/r/cran/"

	// A locally ingested package owns its name.
	desc := []byte(`{"Package":"mypkg","Version":"1.0.0","MD5sum":"0123456789abcdef0123456789abcdef","License":"MIT"}`)
	for key, data := range map[string][]byte{"archive": []byte("LOCAL"), "DESCRIPTION": desc} {
		if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Namespace: channel, Coordinate: channel, Version: "mypkg_1.0.0", AssetKey: key, MediaType: "application/octet-stream", Info: sum(data)}, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}

	// merged index: local name from local, the rest from upstream, invalid dropped
	status, body, _ := f.Get(base + "src/contrib/PACKAGES")
	if status != 200 || !strings.Contains(body, "Package: ggplot2\nVersion: 3.5.0\nDepends: R (>= 3.5)\nImports: rlang,\n        scales\n") || !strings.Contains(body, "Package: mypkg\nVersion: 1.0.0\n") || strings.Contains(body, "9.9.9") || strings.Contains(body, "bad_name") {
		t.Fatalf("index: %d %q", status, body)
	}
	if status, _, _ = f.Get(base + "src/contrib/PACKAGES.gz"); status != 200 {
		t.Fatalf("index gz: %d", status)
	}

	// source tarball: miss -> cached -> hit with no upstream traffic
	dl := base + "src/contrib/ggplot2_3.5.0.tar.gz"
	if status, body, _ = f.Get(dl); status != 200 || body != string(tarball) {
		t.Fatalf("miss: %d %q", status, body)
	}
	n := hits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(tarball) || hits.Load() != n {
		t.Fatalf("hit: %d", status)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatalf("cached %d assets", f.CachedAssets(remote))
	}
	if status, body, _ = f.Get(base + "src/contrib/Archive/ggplot2/ggplot2_3.4.0.tar.gz"); status != 200 || body != string(archived) {
		t.Fatalf("archive: %d %q", status, body)
	}
	if status, _, _ = f.Get(base + "src/contrib/Archive/ggplot2/ggplot2_1.0.0.tar.gz"); status != 404 {
		t.Fatalf("missing archive: %d", status)
	}

	// binaries
	if status, body, _ = f.Get(base + "bin/windows/contrib/4.4/PACKAGES"); status != 200 || !strings.Contains(body, "ggplot2") {
		t.Fatalf("bin index: %d %q", status, body)
	}
	if status, body, _ = f.Get(base + "bin/windows/contrib/4.4/ggplot2_3.5.0.zip"); status != 200 || body != string(zipped) {
		t.Fatalf("bin zip: %d %q", status, body)
	}

	// local owner precedence: no version of a locally owned name goes upstream
	before := hits.Load()
	for _, path := range []string{"src/contrib/mypkg_9.9.9.tar.gz", "src/contrib/Archive/mypkg/mypkg_0.1.0.tar.gz", "bin/windows/contrib/4.4/mypkg_1.0.0.zip"} {
		if status, _, _ = f.Get(base + path); status != 404 {
			t.Fatalf("%s: %d", path, status)
		}
	}
	if hits.Load() != before {
		t.Fatal("locally owned name reached upstream")
	}

	// outage: cached files still served, uncached ones are 503, index stays up
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(tarball) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, _, _ = f.Get(base + "src/contrib/other_1.0.tar.gz"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "src/contrib/PACKAGES"); status != 200 || !strings.Contains(body, "ggplot2") {
		t.Fatalf("index in outage: %d", status)
	}
}
