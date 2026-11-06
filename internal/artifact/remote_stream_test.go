package artifact_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

// A digest-pinned miss streams to the client while upstream is still sending:
// the first bytes arrive before the upstream has finished, and the object is
// committed only once the whole body verified.
func TestMaterializeDigestMissStreamsBeforeUpstreamFinishes(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("stream"), 100000)
	first := 64 << 10
	release := make(chan struct{})
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:first])
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write(payload[first:])
	}))
	feed := f.AddFeed("raw-s1", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	type result struct {
		m   *artifact.Materialized
		err error
	}
	got := make(chan result, 1)
	go func() {
		m, err := f.Service.Materialize(context.Background(), f.Principal, publication(feed, "big.bin"), artifact.Origin{Path: "big.bin", Digest: sum(payload), StreamVerified: true})
		got <- result{m, err}
	}()
	var m *artifact.Materialized
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatal(r.err)
		}
		m = r.m
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("Materialize did not return before the upstream body finished (buffered miss)")
	}
	defer m.Body.Close()
	buf := make([]byte, 1024)
	if _, err := io.ReadFull(m.Body, buf); err != nil || !bytes.Equal(buf, payload[:1024]) {
		close(release)
		t.Fatalf("first bytes: %v", err)
	}
	if f.CachedAssets(remote) != 0 {
		t.Fatal("committed before the body completed")
	}
	close(release)
	rest, err := io.ReadAll(m.Body)
	if err != nil || !bytes.Equal(append(buf, rest...), payload) {
		t.Fatalf("streamed body wrong: %v", err)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatal("verified object not committed")
	}
}

// A digest mismatch is detected at the end of the body. The client must see an
// error and must not receive the declared length in full (so it cannot take
// the response for a complete one); nothing is committed.
func TestMaterializeDigestMismatchAbortsStreamAndCommitsNothing(t *testing.T) {
	f := remotetest.New(t)
	want := bytes.Repeat([]byte("a"), 200000)
	bad := bytes.Repeat([]byte("b"), 200000)
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(bad)))
		_, _ = w.Write(bad)
	}))
	feed := f.AddFeed("raw-s2", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	m, err := f.Service.Materialize(context.Background(), f.Principal, publication(feed, "bad.bin"), artifact.Origin{Path: "bad.bin", Digest: sum(want), StreamVerified: true})
	if err != nil {
		return // verifying before serving is also a refusal; only the buffered path does this
	}
	defer m.Body.Close()
	data, err := io.ReadAll(m.Body)
	if err == nil || len(data) >= len(bad) {
		t.Fatalf("mismatch not signalled: err=%v delivered=%d of %d", err, len(data), len(bad))
	}
	if f.CachedAssets(remote) != 0 {
		t.Fatal("mismatched object committed")
	}
}

// An upstream that drops the connection mid-body fails the client read and
// leaves nothing in the cache, for a digest-pinned object too.
func TestMaterializeDigestUpstreamCutMidBodyCommitsNothing(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("c"), 200000)
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:50000])
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	feed := f.AddFeed("raw-s3", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	m, err := f.Service.Materialize(context.Background(), f.Principal, publication(feed, "cut.bin"), artifact.Origin{Path: "cut.bin", Digest: sum(payload), StreamVerified: true})
	if err == nil {
		defer m.Body.Close()
		if _, err = io.ReadAll(m.Body); err == nil {
			t.Fatal("cut body read cleanly")
		}
	}
	if f.CachedAssets(remote) != 0 {
		t.Fatal("partial object committed")
	}
}

// Concurrent digest-pinned misses make one upstream request; every client
// gets the verified bytes and exactly one object is committed.
func TestMaterializeDigestConcurrentMissesShareOneUpstreamFetch(t *testing.T) {
	f := remotetest.New(t)
	payload := bytes.Repeat([]byte("d"), 400000)
	var calls atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:1000])
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write(payload[1000:])
	}))
	feed := f.AddFeed("raw-s4", "raw")
	remote := f.AddRemote(feed, "raw", "up", 0, up.URL, remotetest.RemoteOptions{})
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := f.Service.Materialize(context.Background(), f.Principal, publication(feed, "c.bin"), artifact.Origin{Path: "c.bin", Digest: sum(payload), StreamVerified: true})
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
		t.Fatalf("bad=%d upstream=%d cached=%d", bad.Load(), calls.Load(), f.CachedAssets(remote))
	}
}
