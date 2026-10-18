package goproxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestGoProxyPullThrough(t *testing.T) {
	f := remotetest.New(t)
	var down atomic.Bool
	var calls atomic.Int32
	const base = "/example.com/!mod/@v/"
	files := map[string]string{
		base + "list":               "v1.0.0\n",
		"/example.com/!mod/@latest": `{"Version":"v1.0.0","Time":"2024-01-02T03:04:05Z"}`,
		base + "v1.0.0.info":        `{"Version":"v1.0.0","Time":"2024-01-02T03:04:05Z"}`,
		base + "v1.0.0.mod":         "module example.com/Mod\n",
		base + "v1.0.0.zip":         "PK-zip-bytes",
	}
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		if body, ok := files[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	feed := f.AddFeed("gofeed", "go")
	remote := f.AddRemote(feed, "go", "proxy", 0, up.URL, remotetest.RemoteOptions{})
	proxy, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(proxy)
	defer srv.Close()
	root := srv.URL + "/feeds/gofeed/go/example.com/!mod/"

	if status, body, _ := f.Get(root + "@v/list"); status != 200 || body != "v1.0.0\n" {
		t.Fatalf("list: %d %q", status, body)
	}
	if status, body, _ := f.Get(root + "@latest"); status != 200 || body != files["/example.com/!mod/@latest"] {
		t.Fatalf("latest: %d %q", status, body)
	}
	for _, kind := range []string{"info", "mod", "zip"} {
		if status, body, h := f.Get(root + "@v/v1.0.0." + kind); status != 200 || body != files[base+"v1.0.0."+kind] || h.Get("X-Kuvryn-Cache") != "miss" {
			t.Fatalf("%s: %d %q", kind, status, body)
		}
	}
	if f.CachedAssets(remote) != 3 {
		t.Fatalf("cached %d, want 3", f.CachedAssets(remote))
	}
	down.Store(true)
	for _, kind := range []string{"info", "mod", "zip"} {
		if status, body, _ := f.Get(root + "@v/v1.0.0." + kind); status != 200 || body != files[base+"v1.0.0."+kind] {
			t.Fatalf("cached %s during outage: %d", kind, status)
		}
	}
	// With the upstream down and the list entry expired/absent from cache the
	// degraded offline view lists the fully cached versions.
	status, body, _ := f.Get(root + "@v/list")
	if status != 200 || body != "v1.0.0\n" {
		t.Fatalf("list during outage: %d %q", status, body)
	}
	if status, _, _ = f.Get(root + "@v/v1.9.9.info"); status != 503 {
		t.Fatalf("uncached version during outage: %d", status)
	}
}
