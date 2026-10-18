package goproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// hangupKey carries, through the request context's values (which survive
// context.WithoutCancel), a channel that closes when the HTTP server cancels
// the request because the client hung up.
type hangupKey struct{}

// hangupStore delays every staged upload's Finalize until the client has hung
// up and the server has cancelled the request. That is the ordering the go tool
// produces: it closes the connection as soon as it has Content-Length bytes,
// which can cancel the request context before the cache entry is committed. A
// staged upload bound to the request context then fails its commit.
type hangupStore struct {
	blob.Store
	waited *atomic.Int32
}

func (s hangupStore) Stage(ctx context.Context) (blob.Upload, error) {
	up, err := s.Store.Stage(ctx)
	if err != nil {
		return nil, err
	}
	ch, _ := ctx.Value(hangupKey{}).(chan struct{})
	return hangupUpload{Upload: up, hangup: ch, waited: s.waited}, nil
}

type hangupUpload struct {
	blob.Upload
	hangup <-chan struct{}
	waited *atomic.Int32
}

func (u hangupUpload) Finalize(ctx context.Context, info blob.Info) (blob.Info, error) {
	select {
	case <-u.hangup:
		u.waited.Add(1)
	case <-time.After(5 * time.Second):
	}
	return u.Upload.Finalize(ctx, info)
}

// A module zip must be cached, and served with the upstream gone, when the
// client hangs up as soon as it has read Content-Length bytes. This fails if
// the staged upload is bound to the request context (the cache entry is lost
// and the next request needs the upstream again).
func TestGoProxyZipCachedWhenClientHangsUpAtLastByte(t *testing.T) {
	f := remotetest.New(t)
	var waited atomic.Int32
	f.Service.Storage = func(context.Context, string) (blob.Store, error) {
		return hangupStore{Store: f.Disk, waited: &waited}, nil
	}
	zip := bytes.Repeat([]byte("PK-zip"), 20000)
	// Over HTTP/2 the end of the body is its own event, after the last data, so
	// the proxy reads EOF separately from the final bytes (as with
	// proxy.golang.org). HTTP/1.1 couples them in one read and the entry would
	// be committed before the client could hang up.
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("upstream request used %s, want HTTP/2", r.Proto)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(zip)))
		_, _ = w.Write(zip)
	}))
	up.EnableHTTP2 = true
	up.StartTLS()
	t.Cleanup(up.Close)
	feed := f.AddFeed("gohang", "go")
	remote := f.AddRemote(feed, "go", "proxy", 0, up.URL, remotetest.RemoteOptions{})
	proxy, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hangup := make(chan struct{})
		go func() { <-r.Context().Done(); close(hangup) }()
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hangupKey{}, hangup)))
	}))
	defer srv.Close()
	url := func(i int) string {
		return fmt.Sprintf("%s/feeds/gohang/go/example.com/mod/@v/v1.0.%d.zip", srv.URL, i)
	}
	const rounds = 5
	for i := 0; i < rounds; i++ {
		req, _ := http.NewRequest(http.MethodGet, url(i), nil)
		req.Header.Set("Authorization", "Bearer "+f.Token)
		res, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.ContentLength != int64(len(zip)) {
			t.Fatalf("round %d: Content-Length %d", i, res.ContentLength)
		}
		got := make([]byte, len(zip))
		if _, err = io.ReadFull(res.Body, got); err != nil || !bytes.Equal(got, zip) {
			t.Fatalf("round %d: read %v", i, err)
		}
		res.Body.Close() // hang up without waiting for EOF
	}
	// Commits finish asynchronously relative to the hang-up; wait for them.
	deadline := time.Now().Add(15 * time.Second)
	for f.CachedAssets(remote) < rounds && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	// Every commit was held until the server had cancelled its request; if not,
	// this test is not reproducing the hang-up it guards.
	if n := waited.Load(); n != rounds {
		t.Fatalf("request cancelled before the commit in %d of %d rounds", n, rounds)
	}
	if n := f.CachedAssets(remote); n != rounds {
		t.Fatalf("cached %d of %d zips after client hang-ups", n, rounds)
	}

	// With the upstream gone, every zip is served from the cache, byte for byte.
	up.Close()
	for i := 0; i < rounds; i++ {
		status, body, _ := f.Get(url(i))
		if status != http.StatusOK || body != string(zip) {
			t.Fatalf("round %d offline: status %d, %d bytes", i, status, len(body))
		}
	}
}
