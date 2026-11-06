package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestManifestAcceptFitsRuntimeBound(t *testing.T) {
	if len(manifestAccept) > 256 {
		t.Fatalf("manifestAccept is %d bytes; the runtime rejects representations over 256", len(manifestAccept))
	}
}

func TestOCIPullThroughTagsManifestsAndBlobs(t *testing.T) {
	f := remotetest.New(t)
	const manifestType = "application/vnd.oci.image.manifest.v1+json"
	blobBytes := strings.Repeat("layer-bytes-", 2000)
	manifest := `{"schemaVersion":2,"mediaType":"` + manifestType + `","config":{"digest":"sha256:` + sha("cfg") + `"},"layers":[{"digest":"sha256:` + sha(blobBytes) + `","size":24000}]}`
	var down atomic.Bool
	var blobFetches atomic.Int32
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/v2/library/app/manifests/latest", "/v2/library/app/manifests/sha256:" + sha(manifest):
			w.Header().Set("Content-Type", manifestType)
			_, _ = w.Write([]byte(manifest))
		case "/v2/library/app/blobs/sha256:" + sha(blobBytes):
			blobFetches.Add(1)
			_, _ = w.Write([]byte(blobBytes))
		case "/v2/library/app/tags/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"library/app","tags":["latest","1.0","bad tag"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("oci1", "oci")
	remote := f.AddRemote(feed, "oci", "hub", 0, up.URL, remotetest.RemoteOptions{MetadataTTL: 1, StaleSeconds: 60})
	registry, err := New(f.Pool, f.Service, filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	base := srv.URL + "/v2/oci1/library/app/"

	status, body, header := f.Get(base+"manifests/latest", "Accept", manifestType)
	if status != 200 || body != manifest || header.Get("Docker-Content-Digest") != "sha256:"+sha(manifest) || header.Get("Content-Type") != manifestType {
		t.Fatalf("tag: %d %v %s", status, header, body)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatal("manifest resolved from a tag was not cached by digest")
	}
	// The same manifest by digest is now a local hit, even with the upstream down.
	down.Store(true)
	if status, body, _ = f.Get(base+"manifests/sha256:"+sha(manifest), "Accept", manifestType); status != 200 || body != manifest {
		t.Fatalf("manifest by digest from cache: %d", status)
	}
	// Client media type negotiation applies to proxied tags too.
	if status, _, _ = f.Get(base+"manifests/latest", "Accept", "application/vnd.docker.distribution.manifest.v2+json"); status != 406 {
		t.Fatalf("unacceptable manifest type: %d", status)
	}
	down.Store(false)

	blobURL := base + "blobs/sha256:" + sha(blobBytes)
	if status, body, _ = f.Get(blobURL); status != 200 || body != blobBytes {
		t.Fatalf("blob miss: %d", status)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached assets %d", f.CachedAssets(remote))
	}
	down.Store(true)
	status, body, header = f.Get(blobURL, "Range", "bytes=0-5")
	if status != 206 || body != "layer-" || blobFetches.Load() != 1 {
		t.Fatalf("cached blob range: %d %q fetches=%d", status, body, blobFetches.Load())
	}

	// A tag that was never resolved while the upstream is down is a 503.
	if status, _, _ = f.Get(base + "manifests/never"); status != 503 {
		t.Fatalf("outage on unknown tag: %d", status)
	}
	// Expired tag metadata is served stale and marked.
	time.Sleep(1200 * time.Millisecond)
	status, _, header = f.Get(base+"manifests/latest", "Accept", manifestType)
	if status != 200 || header.Get("X-Kuvryn-Cache") != "stale" {
		t.Fatalf("stale tag: %d %v", status, header)
	}
	down.Store(false)

	// Tag list is proxied under the feed-qualified name; unsafe tag names dropped.
	status, body, _ = f.Get(base + "tags/list")
	if status != 200 || !strings.Contains(body, `"name":"oci1/library/app"`) || strings.Contains(body, "bad tag") || !strings.Contains(body, `"1.0"`) {
		t.Fatalf("tags: %d %s", status, body)
	}
}

func TestOCIPullThroughNeverServesBytesThatMissDigest(t *testing.T) {
	f := remotetest.New(t)
	want := strings.Repeat("good", 100)
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("evil", 100)))
	}))
	feed := f.AddFeed("oci2", "oci")
	remote := f.AddRemote(feed, "oci", "hub", 0, up.URL, remotetest.RemoteOptions{})
	registry, err := New(f.Pool, f.Service, filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	// The blob is streamed while it is verified, so a mismatch shows up as an
	// aborted response: never a complete 200, never cached.
	status, body, rerr := f.TryGet(srv.URL + "/v2/oci2/lib/app/blobs/sha256:" + sha(want))
	if (status == 200 && rerr == nil) || body == strings.Repeat("evil", 100) || f.CachedAssets(remote) != 0 {
		t.Fatalf("digest mismatch must fail closed: %d %v %q cached=%d", status, rerr, body, f.CachedAssets(remote))
	}
}

// Regression: a Kubernetes fsGroup makes the uploads volume group-writable
// (setgid, the pod's own group). That crash-looped startup with "invalid OCI
// registry configuration"; world access must still be rejected.
func TestNewAcceptsFsGroupWritableUploadsDir(t *testing.T) {
	f := remotetest.New(t)
	dir := filepath.Join(t.TempDir(), "uploads")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f.Pool, f.Service, dir); err != nil {
		t.Fatalf("group-writable uploads dir rejected: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f.Pool, f.Service, dir); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("world-writable uploads dir accepted: %v", err)
	}
}

// A cold blob streams to the client while the upstream is still sending it: the
// response headers and first bytes arrive before the upstream finishes, and the
// blob is cached once the verified body completed.
func TestOCIColdBlobStreamsBeforeUpstreamFinishes(t *testing.T) {
	f := remotetest.New(t)
	blobBytes := strings.Repeat("layer-bytes-", 20000)
	first := 32 << 10
	release := make(chan struct{})
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(blobBytes)))
		_, _ = io.WriteString(w, blobBytes[:first])
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, blobBytes[first:])
	}))
	feed := f.AddFeed("oci3", "oci")
	remote := f.AddRemote(feed, "oci", "hub", 0, up.URL, remotetest.RemoteOptions{})
	registry, err := New(f.Pool, f.Service, filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/oci3/lib/app/blobs/sha256:"+sha(blobBytes), nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	type result struct {
		res *http.Response
		err error
	}
	got := make(chan result, 1)
	go func() { res, err := http.DefaultClient.Do(req); got <- result{res, err} }()
	var res *http.Response
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatal(r.err)
		}
		res = r.res
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("no response headers before the upstream finished: cold blob is buffered")
	}
	defer res.Body.Close()
	head := make([]byte, 1024)
	if _, err := io.ReadFull(res.Body, head); err != nil || string(head) != blobBytes[:1024] {
		close(release)
		t.Fatalf("first bytes: %v", err)
	}
	close(release)
	rest, err := io.ReadAll(res.Body)
	if err != nil || string(head)+string(rest) != blobBytes || res.Header.Get("X-Kuvryn-Cache") != "miss" {
		t.Fatalf("body: %v cache=%q", err, res.Header.Get("X-Kuvryn-Cache"))
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatalf("blob not cached: %d", f.CachedAssets(remote))
	}
}
