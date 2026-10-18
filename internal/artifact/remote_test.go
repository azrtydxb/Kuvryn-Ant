package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

func sum(b []byte) blob.Digest { h := sha256.Sum256(b); return blob.Digest(hex.EncodeToString(h[:])) }

func publication(feed, key string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Coordinate: key, Version: "first", AssetKey: key}
}

func drain(t *testing.T, m *artifact.Materialized) []byte {
	t.Helper()
	defer m.Body.Close()
	data, err := io.ReadAll(m.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Hit, miss, cache population and the local hit afterwards, with the upstream
// taken away to prove the second read is served from the artifact store.
func TestMaterializeMissCachesAndLaterReadsHitLocally(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("kuvryn"), 50000)
	var calls atomic.Int32
	var auth atomic.Value
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/files/a.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-fixture")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	feed := f.AddFeed("raw1", "raw")
	f.AddLocal(feed, "raw", "local", 0)
	remote := f.AddRemote(feed, "raw", "up", 1, up.URL, remotetest.RemoteOptions{Credential: "Bearer secret-one"})
	ctx := context.Background()
	pub := publication(feed, "files/a.bin")
	if _, _, err := f.Service.Open(ctx, f.Principal, pub, 0, -1); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("expected local miss, got %v", err)
	}
	m, err := f.Service.Materialize(ctx, f.Principal, pub, artifact.Origin{Path: "files/a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Committed || m.Size != int64(len(payload)) || m.SourceID != remote || m.MediaType != "application/x-fixture" {
		t.Fatalf("unexpected stream descriptor %+v", m)
	}
	if !bytes.Equal(drain(t, m), payload) {
		t.Fatal("streamed bytes differ")
	}
	if auth.Load() != "Bearer secret-one" || f.CachedAssets(remote) != 1 {
		t.Fatalf("credential/cache state: %v %d", auth.Load(), f.CachedAssets(remote))
	}
	up.Close() // outage: only the cache can answer now
	rec, body, err := f.Service.Open(ctx, f.Principal, pub, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, payload) || rec.SourceID != remote || rec.Info.Digest != sum(payload) || calls.Load() != 1 {
		t.Fatalf("local hit failed: calls=%d", calls.Load())
	}
}

func TestMaterializeCoalescesConcurrentMisses(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("x"), 300000)
	var calls atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write(payload)
	}))
	feed := f.AddFeed("raw2", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	pub := publication(feed, "blob.bin")
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := f.Service.Materialize(context.Background(), f.Principal, pub, artifact.Origin{Path: "blob.bin"})
			if err != nil {
				bad.Add(1)
				return
			}
			data, err := io.ReadAll(m.Body)
			m.Body.Close()
			if err != nil || !bytes.Equal(data, payload) {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || calls.Load() != 1 || f.CachedAssets(remote) != 1 {
		t.Fatalf("coalescing: bad=%d upstream=%d cached=%d", bad.Load(), calls.Load(), f.CachedAssets(remote))
	}
}

func TestMaterializeNegativeCacheAndNotFoundFallthrough(t *testing.T) {
	f := remotetest.New(t)
	var missing, present atomic.Int32
	var secondGone atomic.Bool
	first := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		missing.Add(1)
		http.NotFound(w, r)
	}))
	second := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		present.Add(1)
		if secondGone.Load() {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "from-second")
	}))
	feed := f.AddFeed("raw3", "raw")
	f.AddRemote(feed, "raw", "first", 0, first.URL, remotetest.RemoteOptions{NegativeTTL: 60})
	second2 := f.AddRemote(feed, "raw", "second", 1, second.URL, remotetest.RemoteOptions{})
	ctx := context.Background()
	m, err := f.Service.Materialize(ctx, f.Principal, publication(feed, "x/y.bin"), artifact.Origin{Path: "x/y.bin"})
	if err != nil || m.SourceID != second2 || string(drain(t, m)) != "from-second" {
		t.Fatalf("priority fallthrough on 404 failed: %v", err)
	}
	// A different, never-cached path that nobody has: the first source's 404 is
	// negatively cached, so a repeat does not reach it again.
	pub := publication(feed, "x/none.bin")
	secondGone.Store(true)
	for i := 0; i < 3; i++ {
		if _, err = f.Service.Materialize(ctx, f.Principal, pub, artifact.Origin{Path: "x/none.bin"}); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("want not found, got %v", err)
		}
	}
	if missing.Load() != 2 { // one for y.bin, one for none.bin; repeats are negative-cache hits
		t.Fatalf("negative cache not applied: first source saw %d requests", missing.Load())
	}
}

// An upstream error on a higher priority source must not resolve the name from a
// lower priority one (dependency confusion), and must not be cached.
func TestMaterializeUpstreamErrorFailsClosed(t *testing.T) {
	f := remotetest.New(t)
	var lower atomic.Int32
	broken := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	public := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lower.Add(1)
		_, _ = io.WriteString(w, "public")
	}))
	feed := f.AddFeed("raw4", "raw")
	a := f.AddRemote(feed, "raw", "private", 0, broken.URL, remotetest.RemoteOptions{})
	b := f.AddRemote(feed, "raw", "public", 1, public.URL, remotetest.RemoteOptions{})
	_, err := f.Service.Materialize(context.Background(), f.Principal, publication(feed, "n.bin"), artifact.Origin{Path: "n.bin"})
	if !errors.Is(err, artifact.ErrUnavailable) || lower.Load() != 0 || f.CachedAssets(a)+f.CachedAssets(b) != 0 {
		t.Fatalf("failed open expected: err=%v lower=%d", err, lower.Load())
	}
}

func TestMaterializeSkipsUpstreamWhenLocalSourceOwnsPackage(t *testing.T) {
	f := remotetest.New(t)
	var calls atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "upstream")
	}))
	feed := f.AddFeed("raw5", "raw")
	f.AddLocal(feed, "raw", "local", 0)
	f.AddRemote(feed, "raw", "up", 1, up.URL, remotetest.RemoteOptions{})
	data := []byte("private")
	pub := artifact.Publication{FeedID: feed, Coordinate: "pkg", Version: "v1", AssetKey: "pkg", MediaType: "application/octet-stream", Info: blob.Info{Digest: sum(data), Size: int64(len(data))}}
	if _, err := f.Service.Publish(context.Background(), f.Principal, pub, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	missing := artifact.Publication{FeedID: feed, Coordinate: "pkg", Version: "v2", AssetKey: "pkg"}
	if _, err := f.Service.Materialize(context.Background(), f.Principal, missing, artifact.Origin{Path: "pkg"}); !errors.Is(err, artifact.ErrNotFound) || calls.Load() != 0 {
		t.Fatalf("local owner must block upstream: err=%v calls=%d", err, calls.Load())
	}
}

func TestMaterializeVerifiesDigestBeforeServingAndNeverCachesMismatch(t *testing.T) {
	f := remotetest.New(t)
	good := []byte("good bytes")
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			_, _ = io.WriteString(w, "tampered!!")
			return
		}
		_, _ = w.Write(good)
	}))
	feed := f.AddFeed("raw6", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	ctx := context.Background()
	if _, err := f.Service.Materialize(ctx, f.Principal, publication(feed, "bad"), artifact.Origin{Path: "bad", Digest: sum(good)}); err == nil || f.CachedAssets(remote) != 0 {
		t.Fatalf("mismatched digest accepted: %v", err)
	}
	m, err := f.Service.Materialize(ctx, f.Principal, publication(feed, "ok"), artifact.Origin{Path: "ok", Digest: sum(good)})
	if err != nil || !m.Committed || !bytes.Equal(drain(t, m), good) || f.CachedAssets(remote) != 1 {
		t.Fatalf("verified fetch failed: %v", err)
	}
}

func TestMaterializeTruncatedUpstreamIsNeverCached(t *testing.T) {
	f := remotetest.New(t)
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "short")
		if h, ok := w.(http.Hijacker); ok {
			conn, _, _ := h.Hijack()
			conn.Close()
		}
	}))
	feed := f.AddFeed("raw7", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	m, err := f.Service.Materialize(context.Background(), f.Principal, publication(feed, "t.bin"), artifact.Origin{Path: "t.bin"})
	if err == nil {
		defer m.Body.Close()
		if _, err = io.ReadAll(m.Body); err == nil {
			t.Fatal("truncated body read cleanly")
		}
	}
	if f.CachedAssets(remote) != 0 {
		t.Fatal("truncated artifact was cached")
	}
}

func TestRemoteMetadataStaleIfErrorAndStaleMarker(t *testing.T) {
	f := remotetest.New(t)
	var down atomic.Bool
	var calls atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"v":1}`)
	}))
	feed := f.AddFeed("raw8", "raw")
	f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{MetadataTTL: 1, StaleSeconds: 60})
	ctx := context.Background()
	pub := publication(feed, "doc")
	o := artifact.Origin{Path: "doc.json", Representation: "application/json"}
	doc, err := f.Service.RemoteMetadata(ctx, f.Principal, pub, o)
	if err != nil || doc.Stale || string(doc.Body) != `{"v":1}` {
		t.Fatalf("fresh fetch: %v %+v", err, doc)
	}
	again, _ := f.Service.RemoteMetadata(ctx, f.Principal, pub, o)
	if again.Stale || calls.Load() != 1 {
		t.Fatalf("fresh cache hit expected without network: calls=%d", calls.Load())
	}
	time.Sleep(1200 * time.Millisecond)
	down.Store(true)
	doc, err = f.Service.RemoteMetadata(ctx, f.Principal, pub, o)
	if err != nil || !doc.Stale || string(doc.Body) != `{"v":1}` {
		t.Fatalf("outage must serve stale: %v %+v", err, doc)
	}
	if artifact.StaleHeaders()["X-Kuvryn-Cache"] != "stale" {
		t.Fatal("stale marker missing")
	}
}

// Credentials are bound to each source origin. A redirect from one source to
// another configured origin must never carry the first source's credential, and
// each source sees only its own.
func TestCredentialsNeverCrossOrigins(t *testing.T) {
	f := remotetest.New(t)
	var seenB atomic.Value
	seenB.Store("")
	b := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenB.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Key"))
		_, _ = io.WriteString(w, "from-b")
	}))
	var seenA atomic.Value
	a := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenA.Store(r.Header.Get("Authorization"))
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, b.URL+"/landed", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "from-a")
	}))
	feed := f.AddFeed("raw9", "raw")
	f.AddRemote(feed, "raw", "a", 0, a.URL, remotetest.RemoteOptions{Credential: "Bearer token-a", RedirectOrigins: []string{b.URL}})
	ctx := context.Background()
	m, err := f.Service.Materialize(ctx, f.Principal, publication(feed, "redir"), artifact.Origin{Path: "redir"})
	if err != nil || string(drain(t, m)) != "from-b" {
		t.Fatalf("redirect to allowed origin failed: %v", err)
	}
	if got := seenA.Load(); got != "Bearer token-a" {
		t.Fatalf("source origin must receive its credential, got %v", got)
	}
	if got := seenB.Load().(string); strings.Contains(got, "token-a") || got != "|" {
		t.Fatalf("credential leaked cross-origin: %q", got)
	}
	// An absolute URL on an origin that is not configured is refused outright.
	other := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unconfigured origin contacted") }))
	if _, err = f.Service.Materialize(ctx, f.Principal, publication(feed, "abs"), artifact.Origin{URL: other.URL + "/x"}); err == nil {
		t.Fatal("absolute URL to an unconfigured origin accepted")
	}
}

func TestResolveReference(t *testing.T) {
	cases := []struct{ doc, ref, path, abs string }{
		{"simple/pkg/", "../../files/pkg-1.0.tar.gz", "files/pkg-1.0.tar.gz", ""},
		{"simple/pkg/", "https://files.example/pkg-1.0.tar.gz", "", "https://files.example/pkg-1.0.tar.gz"},
		{"simple/pkg/", "//files.example/x", "", "https://files.example/x"},
		{"simple/pkg/", "pkg-1.0.whl#sha256=abc", "simple/pkg/pkg-1.0.whl", ""},
	}
	for _, c := range cases {
		path, abs, err := artifact.ResolveReference(c.doc, c.ref)
		if err != nil || path != c.path || abs != c.abs {
			t.Errorf("%q: got %q %q %v", c.ref, path, abs, err)
		}
	}
	for _, bad := range []string{"http://insecure/x", "x?token=1", "../../../../"} {
		if _, _, err := artifact.ResolveReference("simple/pkg/", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
