package goproxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

// The go tool stops reading and closes the connection as soon as it has
// Content-Length bytes, which cancels the request context before the proxy's
// final EOF read commits the cache entry. The module zip must still be cached
// and served with the upstream gone.
func TestGoProxyZipCachedWhenClientHangsUpAtLastByte(t *testing.T) {
	f := remotetest.New(t)
	zip := bytes.Repeat([]byte("PK-zip"), 20000)
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(zip)))
		_, _ = w.Write(zip)
	}))
	feed := f.AddFeed("gohang", "go")
	remote := f.AddRemote(feed, "go", "proxy", 0, up.URL, remotetest.RemoteOptions{})
	proxy, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(proxy)
	defer srv.Close()
	const rounds = 40
	for i := 0; i < rounds; i++ {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/feeds/gohang/go/example.com/mod/@v/v1.0.%d.zip", srv.URL, i), nil)
		req.Header.Set("Authorization", "Bearer "+f.Token)
		res, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(zip))
		if _, err = io.ReadFull(res.Body, got); err != nil || !bytes.Equal(got, zip) {
			t.Fatalf("round %d: read %v", i, err)
		}
		res.Body.Close() // hang up without waiting for EOF
	}
	// Commits finish asynchronously relative to the hang-up; wait for them.
	deadline := time.Now().Add(10 * time.Second)
	for f.CachedAssets(remote) < rounds && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := f.CachedAssets(remote); n != rounds {
		t.Fatalf("cached %d of %d zips after client hang-ups", n, rounds)
	}
}
