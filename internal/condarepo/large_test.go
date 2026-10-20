package condarepo

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/klauspost/compress/zstd"
)

// writeRepodata streams a synthetic upstream repodata: the given real packages
// plus filler valid entries, an entry the validator must drop, and an info
// block whose base_url must not survive.
func writeRepodata(w io.Writer, filler int, real map[string]string) {
	fmt.Fprint(w, `{"info":{"subdir":"linux-64","base_url":"https://evil.example/"},"packages":{`)
	first := true
	sep := func() {
		if !first {
			fmt.Fprint(w, ",")
		}
		first = false
	}
	for name, sum := range real {
		if strings.HasSuffix(name, ".tar.bz2") {
			base := strings.TrimSuffix(name, ".tar.bz2")
			parts := strings.Split(base, "-")
			sep()
			fmt.Fprintf(w, `%q:{"name":%q,"version":%q,"build":%q,"build_number":0,"subdir":"linux-64","depends":[],"sha256":%q,"size":9}`, name, parts[0], parts[1], parts[2], sum)
		}
	}
	for i := 0; i < filler; i++ {
		sep()
		fmt.Fprintf(w, `"filler%06d-1.0-0.tar.bz2":{"name":"filler%06d","version":"1.0","build":"0","build_number":0,"subdir":"linux-64","depends":["libgcc-ng >=12","libstdcxx-ng >=12"],"sha256":%q,"md5":"0123456789abcdef0123456789abcdef","size":1048576,"timestamp":1700000000000}`, i, i, hexSum([]byte(fmt.Sprint(i))))
	}
	sep()
	fmt.Fprint(w, `"nosha-1.0-0.tar.bz2":{"name":"nosha","version":"1.0","build":"0","subdir":"linux-64"}`)
	fmt.Fprint(w, `},"packages.conda":{`)
	first = true
	for name, sum := range real {
		if strings.HasSuffix(name, ".conda") {
			base := strings.TrimSuffix(name, ".conda")
			parts := strings.Split(base, "-")
			sep()
			fmt.Fprintf(w, `%q:{"name":%q,"version":%q,"build":%q,"subdir":"linux-64","depends":[],"sha256":%q,"size":11}`, name, parts[0], parts[1], parts[2], sum)
		}
	}
	fmt.Fprint(w, `},"removed":["gone-1.0-0.tar.bz2"],"repodata_version":1}`)
}

type largeFixture struct {
	f                 *remotetest.Fixture
	base              string
	remote            string
	repodataHits      atomic.Int32
	zstdHits          atomic.Int32
	pkgHits           atomic.Int32
	bz, cd            []byte
	serveZstd, outage atomic.Bool
	repo              *Repository
}

func newLargeFixture(t *testing.T, filler int, metadataLimit int64, serveZstd bool) *largeFixture {
	t.Helper()
	f := remotetest.New(t)
	lf := &largeFixture{f: f, bz: []byte("BZ2-BYTES"), cd: []byte("CONDA-BYTES")}
	lf.serveZstd.Store(serveZstd)
	real := map[string]string{
		"zlib-1.2.13-h0_0.tar.bz2":    hexSum(lf.bz),
		"numpy-1.26.0-py311_0.conda":  hexSum(lf.cd),
		"bad-1.0-0.tar.bz2":           hexSum([]byte("not what is served")),
		"filler000007-1.0-0.tar.bz2x": "", // not a package name: ignored by the writer
	}
	delete(real, "filler000007-1.0-0.tar.bz2x")
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lf.outage.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/linux-64/repodata.json.zst":
			lf.zstdHits.Add(1)
			if !lf.serveZstd.Load() {
				http.NotFound(w, r)
				return
			}
			z, _ := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
			writeRepodata(z, filler, real)
			_ = z.Close()
		case "/linux-64/repodata.json":
			lf.repodataHits.Add(1)
			writeRepodata(w, filler, real)
		case "/linux-64/zlib-1.2.13-h0_0.tar.bz2":
			lf.pkgHits.Add(1)
			_, _ = w.Write(lf.bz)
		case "/linux-64/numpy-1.26.0-py311_0.conda":
			_, _ = w.Write(lf.cd)
		case "/linux-64/bad-1.0-0.tar.bz2":
			_, _ = w.Write([]byte("BAD-BYTES"))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("packages", "conda")
	lf.remote = f.AddRemote(feed, "conda", "conda-forge", 1, upstream.URL, remotetest.RemoteOptions{MaxMetadataBytes: metadataLimit})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	lf.repo = repo
	srv := httptest.NewServer(repo)
	t.Cleanup(srv.Close)
	lf.base = srv.URL + "/feeds/packages/conda/"
	return lf
}

func (lf *largeFixture) get(t *testing.T, path string, headers ...string) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", lf.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+lf.f.Token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, body, res.Header
}

// A repodata above the per-source metadata bound is streamed through the
// validator (zstd upstream preferred), downloads verify against a spooled
// digest index instead of re-reading the document, and the configured bound is
// what decides between the cached and the streaming path.
func TestCondaLargeRepodataStreamsAndVerifiesDownloads(t *testing.T) {
	lf := newLargeFixture(t, 3000, 64<<10, true)
	status, body, header := lf.get(t, "linux-64/repodata.json", "Accept-Encoding", "identity")
	if status != 200 || header.Get("Content-Encoding") != "" {
		t.Fatalf("repodata: %d %v", status, header)
	}
	if lf.zstdHits.Load() != 1 || lf.repodataHits.Load() != 1 {
		t.Fatalf("expected one probe of the plain document and one zstd stream: zst=%d plain=%d", lf.zstdHits.Load(), lf.repodataHits.Load())
	}
	var doc struct {
		Info     map[string]string          `json:"info"`
		Packages map[string]json.RawMessage `json:"packages"`
		Conda    map[string]json.RawMessage `json:"packages.conda"`
		Removed  []string                   `json:"removed"`
		Version  int                        `json:"repodata_version"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(doc.Packages) != 3000+2 || len(doc.Conda) != 1 || doc.Info["subdir"] != "linux-64" || len(doc.Info) != 1 || doc.Version != 1 || len(doc.Removed) != 0 {
		t.Fatalf("shape: packages=%d conda=%d info=%v", len(doc.Packages), len(doc.Conda), doc.Info)
	}
	if _, ok := doc.Packages["nosha-1.0-0.tar.bz2"]; ok || bytes.Contains(body, []byte("evil.example")) {
		t.Fatal("invalid entry or upstream base_url survived")
	}

	// gzip negotiation keeps the same document.
	req, _ := http.NewRequest("GET", lf.base+"linux-64/repodata.json", nil)
	req.Header.Set("Authorization", "Bearer "+lf.f.Token)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil || res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response: %v %v", err, res.Header)
	}
	plain, err := io.ReadAll(zr)
	res.Body.Close()
	var again struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err != nil || json.Unmarshal(plain, &again) != nil || len(again.Packages) != len(doc.Packages) {
		t.Fatalf("gzip body differs: %v", err)
	}

	// The stream built the digest index: downloads verify without a new read
	// of the repodata.
	reads := lf.zstdHits.Load()
	if status, body, _ := lf.get(t, "linux-64/zlib-1.2.13-h0_0.tar.bz2"); status != 200 || string(body) != string(lf.bz) {
		t.Fatalf("download: %d %q", status, body)
	}
	if status, body, _ := lf.get(t, "linux-64/numpy-1.26.0-py311_0.conda"); status != 200 || string(body) != string(lf.cd) {
		t.Fatalf(".conda download: %d %q", status, body)
	}
	if lf.zstdHits.Load() != reads {
		t.Fatalf("downloads re-read the repodata (%d -> %d)", reads, lf.zstdHits.Load())
	}
	// A digest the upstream does not honour is refused and not cached.
	cached := lf.f.CachedAssets(lf.remote)
	if status, body, _ := lf.get(t, "linux-64/bad-1.0-0.tar.bz2"); status == 200 || strings.Contains(string(body), "BAD-BYTES") {
		t.Fatalf("digest mismatch served: %d", status)
	}
	if lf.f.CachedAssets(lf.remote) != cached {
		t.Fatal("mismatching package was cached")
	}
	if status, _, _ := lf.get(t, "linux-64/missing-1.0-0.tar.bz2"); status != 404 {
		t.Fatalf("unknown package: %d", status)
	}

	// A process that never served the index (restart) builds it once on demand,
	// then answers further lookups from it.
	lf.repo.indexes = indexSet{}
	if status, _, _ := lf.get(t, "linux-64/numpy-1.26.0-py311_0.conda"); status != 200 {
		// already cached locally: served without any lookup
		t.Fatalf("cached download: %d", status)
	}
	reads = lf.zstdHits.Load()
	if status, _, _ := lf.get(t, "linux-64/bad-1.0-0.tar.bz2"); status == 200 {
		t.Fatal("mismatch served")
	}
	if status, _, _ := lf.get(t, "linux-64/bad-1.0-0.tar.bz2"); status == 200 {
		t.Fatal("mismatch served")
	}
	if lf.zstdHits.Load() != reads+1 {
		t.Fatalf("index should be built once on demand: %d -> %d", reads, lf.zstdHits.Load())
	}
}

// Without a .zst upstream the plain document is streamed; a repodata within the
// bound stays on the cached path and is not streamed at all.
func TestCondaLargeRepodataFallsBackToPlainAndRespectsTheBound(t *testing.T) {
	lf := newLargeFixture(t, 500, 64<<10, false)
	if status, body, _ := lf.get(t, "linux-64/repodata.json"); status != 200 || !json.Valid(body) {
		t.Fatalf("plain stream: %d", status)
	}
	if lf.zstdHits.Load() != 1 || lf.repodataHits.Load() != 2 {
		t.Fatalf("probe, missing zst, plain stream: zst=%d plain=%d", lf.zstdHits.Load(), lf.repodataHits.Load())
	}

	// Raising the bound above the document size puts it back on the cached
	// path: one plain read serves repeated requests.
	small := newLargeFixture(t, 500, 8<<20, true)
	for i := 0; i < 2; i++ {
		if status, body, _ := small.get(t, "linux-64/repodata.json"); status != 200 || !json.Valid(body) {
			t.Fatalf("cached path: %d", status)
		}
	}
	if small.zstdHits.Load() != 0 || small.repodataHits.Load() != 1 {
		t.Fatalf("cached path: zst=%d plain=%d", small.zstdHits.Load(), small.repodataHits.Load())
	}
}

// An upstream failure mid-stream must not look like a complete document.
func TestCondaLargeRepodataTruncationIsNotServedAsComplete(t *testing.T) {
	f := remotetest.New(t)
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/linux-64/repodata.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "1000000")
		fmt.Fprint(w, `{"packages":{"zlib-1.2.13-h0_0.tar.bz2":{"name":"zlib","version":"1.2.13","build":"h0_0","subdir":"linux-64","sha256":"`+hexSum([]byte("x"))+`"},`)
	}))
	feed := f.AddFeed("packages", "conda")
	f.AddRemote(feed, "conda", "conda-forge", 1, upstream.URL, remotetest.RemoteOptions{MaxMetadataBytes: 1024})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/feeds/packages/conda/linux-64/repodata.json", nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
		if err == nil && res.StatusCode == 200 {
			t.Fatal("a truncated upstream document was served as complete")
		}
	}
}

// A large synthetic repodata (tens of MiB, far above the instance bound) is
// served with memory that does not grow with the document.
func TestCondaLargeRepodataMemoryIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("large synthetic document")
	}
	const filler = 100000 // about 30 MiB of JSON
	lf := newLargeFixture(t, filler, 0, true)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", lf.base+"linux-64/repodata.json", nil)
	req.Header.Set("Authorization", "Bearer "+lf.f.Token)
	req.Header.Set("Accept-Encoding", "identity")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, res.Body) // never held in memory by the test either
	res.Body.Close()
	close(stop)
	wg.Wait()
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("stream: %d %v", res.StatusCode, err)
	}
	if n < 24<<20 {
		t.Fatalf("document too small to prove anything: %d bytes", n)
	}
	if grown := int64(peak.Load()) - int64(base.HeapAlloc); grown > 48<<20 {
		t.Fatalf("heap grew by %d MiB while streaming %d MiB", grown>>20, n>>20)
	}
	// The spooled index answers a download without reading the document again.
	reads := lf.zstdHits.Load()
	if status, body, _ := lf.get(t, "linux-64/zlib-1.2.13-h0_0.tar.bz2"); status != 200 || string(body) != string(lf.bz) {
		t.Fatalf("download: %d", status)
	}
	if lf.zstdHits.Load() != reads {
		t.Fatal("download re-read the document")
	}
}

// Once a subdir is known to be large, further requests stream directly instead
// of probing the cached path (which reads up to the bound) every time.
func TestCondaLargeRepodataIsNotReprobed(t *testing.T) {
	lf := newLargeFixture(t, 500, 64<<10, true)
	for i := 0; i < 3; i++ {
		if status, body, _ := lf.get(t, "linux-64/repodata.json"); status != 200 || !json.Valid(body) {
			t.Fatalf("request %d: %d", i, status)
		}
	}
	if lf.repodataHits.Load() != 1 || lf.zstdHits.Load() != 3 {
		t.Fatalf("plain probes=%d zst streams=%d, want 1 and 3", lf.repodataHits.Load(), lf.zstdHits.Load())
	}
}
