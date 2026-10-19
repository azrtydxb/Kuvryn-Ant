package apkrepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func indexEntryOf(t *testing.T, pkg []byte, stem, arch string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "pkg")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(pkg); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	entry, err := inspect(f, int64(len(pkg)), stem, arch)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestParseIndexReadsSignedArchive(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkg := makeApk(t, "hello", "1.0-r0", "x86_64", "a")
	entry := indexEntryOf(t, pkg, "hello-1.0-r0", "x86_64")
	idx, err := buildIndex([]string{entry}, "up", key, "up.rsa.pub")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseIndex(idx)
	if err != nil || len(got) != 1 || got["hello-1.0-r0"].checksum != entryFields(entry)["C"] || got["hello-1.0-r0"].size != int64(len(pkg)) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = parseIndex(bytes.Repeat([]byte("x"), 64)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestAPKPullThrough(t *testing.T) {
	f := remotetest.New(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	good := makeApk(t, "hello", "1.0-r0", "x86_64", "good")
	forged := makeApk(t, "bad", "1.0-r0", "x86_64", "forged")
	other := makeApk(t, "bad", "1.0-r0", "x86_64", "listed")
	noarch := makeApk(t, "docs", "2.0-r1", "noarch", "docs")
	entries := []string{
		indexEntryOf(t, good, "hello-1.0-r0", "x86_64"),
		indexEntryOf(t, other, "bad-1.0-r0", "x86_64"), // index lists "other"; upstream serves "forged"
		indexEntryOf(t, noarch, "docs-2.0-r1", "x86_64"),
	}
	index, err := buildIndex(entries, "upstream", key, "upstream.rsa.pub")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	var down atomic.Bool
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/v3.24/main/x86_64/APKINDEX.tar.gz":
			_, _ = w.Write(index)
		case "/v3.24/main/x86_64/hello-1.0-r0.apk":
			_, _ = w.Write(good)
		case "/v3.24/main/x86_64/bad-1.0-r0.apk":
			_, _ = w.Write(forged)
		case "/v3.24/main/x86_64/docs-2.0-r1.apk":
			_, _ = w.Write(noarch)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("alpine", "apk")
	local := f.AddLocal(feed, "apk", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "apk", "cdn", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/alpine/apk/v3.24/main/x86_64/"

	// the signed index is preserved byte for byte
	status, body, h := f.Get(base + "APKINDEX.tar.gz")
	if status != 200 || body != string(index) || h.Get("X-Kuvryn-Cache") != "" {
		t.Fatalf("index: %d", status)
	}
	if status, _, _ = f.Get(srv.URL + "/feeds/alpine/apk/v3.24/main/aarch64/APKINDEX.tar.gz"); status != 404 {
		t.Fatalf("unknown channel: %d", status)
	}
	// package: miss -> control checksum verified -> cached -> hit with no upstream traffic
	dl := base + "hello-1.0-r0.apk"
	if status, body, _ = f.Get(dl); status != 200 || body != string(good) {
		t.Fatalf("miss: %d", status)
	}
	if f.CachedAssets(remote) < 1 {
		t.Fatal("package not cached")
	}
	n := hits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(good) || hits.Load() != n {
		t.Fatalf("hit: %d hits=%d/%d", status, hits.Load(), n)
	}
	// noarch package listed in the arch index
	if status, body, _ = f.Get(base + "docs-2.0-r1.apk"); status != 200 || body != string(noarch) {
		t.Fatalf("noarch: %d", status)
	}
	// a package whose control segment does not match the index is rejected and not cached
	cached := f.CachedAssets(remote)
	if status, body, _ = f.Get(base + "bad-1.0-r0.apk"); status == 200 || strings.Contains(body, "forged") {
		t.Fatalf("forged package served: %d", status)
	}
	if f.CachedAssets(remote) != cached {
		t.Fatal("forged package cached")
	}
	// not listed in the upstream index -> never fetched
	if status, _, _ = f.Get(base + "ghost-1.0-r0.apk"); status != 404 {
		t.Fatalf("unlisted: %d", status)
	}
	// outage: cached package still downloads, uncached listed package is unavailable
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(good) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, _, _ = f.Get(base + "bad-1.0-r0.apk"); status == 200 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
