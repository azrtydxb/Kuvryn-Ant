package goproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestGoProxySumdbMirror(t *testing.T) {
	f := remotetest.New(t)
	files := map[string]string{
		"/sumdb/sum.golang.org/latest":                         "go.sum database tree\n1\nabc\n",
		"/sumdb/sum.golang.org/lookup/example.com/!mod@v1.0.0": "42\nexample.com/Mod v1.0.0 h1:x=\n",
		"/sumdb/sum.golang.org/tile/8/0/x001/234.p/12":         "tilebytes",
		"/sumdb/sum.golang.org/tile/8/data/000":                "datatile",
	}
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	feed := f.AddFeed("gofeed", "go")
	f.AddRemote(feed, "go", "proxy", 0, up.URL, remotetest.RemoteOptions{})
	proxy, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	proxy.sumdbOrigin = up.URL // keep the test off the real checksum database
	srv := httptest.NewServer(proxy)
	defer srv.Close()
	root := srv.URL + "/feeds/gofeed/go/sumdb/"
	if status, _, _ := f.Get(root + "sum.golang.org/supported"); status != 200 {
		t.Fatalf("supported: %d", status)
	}
	if status, _, _ := f.Get(root + "sum.example.org/supported"); status != 404 {
		t.Fatalf("foreign db supported: %d", status)
	}
	for p, want := range files {
		rel := strings.TrimPrefix(p, "/sumdb/")
		if status, body, _ := f.Get(root + rel); status != 200 || body != want {
			t.Fatalf("%s: %d %q", rel, status, body)
		}
	}
	if status, _, _ := f.Get(root + "sum.golang.org/lookup/example.com/!mod@v9.9.9"); status != 404 {
		t.Fatalf("missing lookup: %d", status)
	}
	if status, _, _ := f.Get(root + "sum.golang.org/tile/8/0/..%2f..%2fx"); status != 404 && status != 400 {
		t.Fatalf("traversal: %d", status)
	}
}

// A GOPROXY upstream such as proxy.golang.org does not serve /sumdb/; the
// mirror then fetches the public checksum database itself.
func TestGoProxySumdbFallsBackToDatabase(t *testing.T) {
	f := remotetest.New(t)
	proxyUp := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	files := map[string]string{
		"/latest":                      "go.sum database tree\n1\nabc\n",
		"/lookup/example.com/m@v1.0.0": "7\nexample.com/m v1.0.0 h1:x=\n",
		"/tile/8/0/000":                "tile",
	}
	db := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	feed := f.AddFeed("gofeed", "go")
	f.AddRemote(feed, "go", "proxy", 0, proxyUp.URL, remotetest.RemoteOptions{RedirectOrigins: []string{db.URL}})
	proxy, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	proxy.sumdbOrigin = db.URL
	srv := httptest.NewServer(proxy)
	defer srv.Close()
	root := srv.URL + "/feeds/gofeed/go/sumdb/sum.golang.org/"
	for p, want := range files {
		if status, body, _ := f.Get(root + strings.TrimPrefix(p, "/")); status != 200 || body != want {
			t.Fatalf("%s: %d %q", p, status, body)
		}
	}
	if status, _, _ := f.Get(root + "lookup/example.com/m@v9.9.9"); status != 404 {
		t.Fatalf("missing lookup: %d", status)
	}
}

// The go command probes path prefixes and branch names; anything that is not a
// module version must answer 404 so it moves on (a 400 aborts the lookup).
func TestGoProxyNonModuleReadsAre404(t *testing.T) {
	f := remotetest.New(t)
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	feed := f.AddFeed("gofeed", "go")
	f.AddRemote(feed, "go", "proxy", 0, up.URL, remotetest.RemoteOptions{})
	proxy, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(proxy)
	defer srv.Close()
	for _, p := range []string{
		"github.com/org/@v/list", "golang.org/x/@v/list", "example/@v/list", "x/y/z/@v/list",
		"example.com/Acme/@v/list", "example.com/a/@v/master.info", "example.com/a/@v/v1.2.info", "example.com/a/@latest/..", "example.com/a/@v/v2.0.0.zip",
	} {
		if status, _, _ := f.Get(srv.URL + "/feeds/gofeed/go/" + p); status != 404 {
			t.Errorf("%s: %d, want 404", p, status)
		}
	}
}
