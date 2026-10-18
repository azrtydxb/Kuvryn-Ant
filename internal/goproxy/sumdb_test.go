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
