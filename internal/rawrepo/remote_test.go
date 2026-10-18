package rawrepo

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestRawPullThroughCachesAndPublishNeverGoesUpstream(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("0123456789"), 1000)
	var gets, other atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			other.Add(1)
			http.Error(w, "unexpected method", 405)
			return
		}
		gets.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			http.Error(w, "denied", 401)
			return
		}
		if r.URL.Path == "/team/a b.bin" {
			w.Header().Set("Content-Type", "application/x-raw")
			w.Header().Set("Content-Length", "10000")
			_, _ = w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	feed := f.AddFeed("rawfeed", "raw")
	f.AddLocal(feed, "raw", "local", 0)
	remote := f.AddRemote(feed, "raw", "up", 1, up.URL, remotetest.RemoteOptions{Credential: "Bearer upstream-secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/rawfeed/raw/"

	status, body, header := f.Get(base + "team/a%20b.bin")
	if status != 200 || body != string(payload) || header.Get("X-Kuvryn-Cache") != "miss" || header.Get("Content-Type") != "application/x-raw" {
		t.Fatalf("miss: %d %v", status, header)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatal("artifact not cached under the remote source")
	}
	up.Close()
	status, body, header = f.Get(base+"team/a%20b.bin", "Range", "bytes=2-4")
	if status != 206 || body != "234" || header.Get("X-Kuvryn-Cache") != "" {
		t.Fatalf("cached hit with range: %d %q", status, body)
	}
	if gets.Load() != 1 {
		t.Fatalf("upstream contacted %d times", gets.Load())
	}
	// Unknown path while the upstream is down is a 503, never a silent 404.
	if status, _, _ = f.Get(base + "nope.bin"); status != 503 {
		t.Fatalf("outage status %d", status)
	}

	// Publishing routes to the local source; upstream sees no non-GET traffic.
	req, _ := http.NewRequest(http.MethodPut, base+"mine/x.bin", strings.NewReader("local"))
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 201 || other.Load() != 0 {
		t.Fatalf("publish %d, upstream non-GET %d", res.StatusCode, other.Load())
	}
	if status, body, _ = f.Get(base + "mine/x.bin"); status != 200 || body != "local" {
		t.Fatalf("local read %d", status)
	}
}

// A package owned by a local source is never completed from upstream.
func TestRawLocalOwnerBlocksUpstream(t *testing.T) {
	f := remotetest.New(t)
	var calls atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "upstream")
	}))
	feed := f.AddFeed("rawfeed2", "raw")
	f.AddLocal(feed, "raw", "local", 0)
	f.AddRemote(feed, "raw", "up", 1, up.URL, remotetest.RemoteOptions{})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/feeds/rawfeed2/raw/x/y.bin", strings.NewReader("mine"))
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatal(err, res)
	}
	res.Body.Close()
	// Different path = different ownership unit and may come from upstream, but
	// the owned path must not be re-fetched when it is simply requested again.
	if status, body, _ := f.Get(srv.URL + "/feeds/rawfeed2/raw/x/y.bin"); status != 200 || body != "mine" || calls.Load() != 0 {
		t.Fatalf("owned path: %d %q upstream=%d", status, body, calls.Load())
	}
}
