package pypi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func digestOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestPyPIPullThroughJSONIndexCDNAndCaching(t *testing.T) {
	f := remotetest.New(t)
	const wheel, sdist, core = "demo-1.0-py3-none-any.whl", "demo-1.0.tar.gz", "Metadata-Version: 2.1\nName: demo\n"
	wheelBytes, sdistBytes := "WHEEL-BYTES", "SDIST-BYTES"
	var cdnAuth atomic.Value
	cdnAuth.Store("")
	var cdnCount, originCount atomic.Int32
	var tamper atomic.Bool
	_, _ = &cdnCount, &originCount
	cdn := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnCount.Add(1)
		cdnAuth.Store(r.Header.Get("Authorization") + r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/packages/" + wheel:
			if tamper.Load() {
				_, _ = w.Write([]byte("TAMPERED!!!"))
				return
			}
			_, _ = w.Write([]byte(wheelBytes))
		case "/packages/" + wheel + ".metadata":
			_, _ = w.Write([]byte(core))
		default:
			http.NotFound(w, r)
		}
	}))
	var down atomic.Bool
	origin := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCount.Add(1)
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/simple/demo/":
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
				http.Error(w, "json required", 406)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
			doc := map[string]any{"meta": map[string]string{"api-version": "1.0"}, "name": "demo", "files": []map[string]any{
				{"filename": wheel, "url": cdn.URL + "/packages/" + wheel, "hashes": map[string]string{"sha256": digestOf(wheelBytes)}, "requires-python": ">=3.8", "core-metadata": map[string]string{"sha256": digestOf(core)}},
				{"filename": sdist, "url": "../../packages/" + sdist, "hashes": map[string]string{"sha256": digestOf(sdistBytes)}, "yanked": "broken"},
				{"filename": "demo-1.0.zip", "url": "../../packages/demo-1.0.zip", "hashes": map[string]string{"sha256": digestOf("zip")}},
				{"filename": "demo-0.9.tar.gz", "url": "../../packages/demo-0.9.tar.gz", "hashes": map[string]string{"md5": "nope"}},
			}}
			_ = json.NewEncoder(w).Encode(doc)
		case "/packages/" + sdist:
			_, _ = w.Write([]byte(sdistBytes))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("py1", "pypi")
	remote := f.AddRemote(feed, "pypi", "pypi-org", 0, origin.URL, remotetest.RemoteOptions{Credential: "Bearer pypi-secret", RedirectOrigins: []string{cdn.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/py1/pypi/"

	status, body, header := f.Get(base+"simple/demo/", "Accept", "application/vnd.pypi.simple.v1+json")
	if status != 200 || header.Get("Content-Type") != "application/vnd.pypi.simple.v1+json" {
		t.Fatalf("index: %d %v %s", status, header, body)
	}
	var page struct {
		Files []struct {
			Filename     string
			URL          string
			Hashes       map[string]string
			Yanked       any
			CoreMetadata map[string]string `json:"core-metadata"`
		}
	}
	if err = json.Unmarshal([]byte(body), &page); err != nil || len(page.Files) != 2 {
		t.Fatalf("expected the wheel and sdist only (unsupported/unhashed skipped): %s", body)
	}
	if strings.Contains(body, cdn.URL) || strings.Contains(body, origin.URL) {
		t.Fatalf("upstream origin leaked into index: %s", body)
	}
	for _, file := range page.Files {
		want := "/feeds/py1/pypi/files/demo/" + remote + "/" + file.Hashes["sha256"] + "/" + file.Filename
		if !strings.HasSuffix(file.URL, want) {
			t.Fatalf("link %s does not bind source+digest", file.URL)
		}
	}
	// HTML negotiation renders the same files.
	if status, body, _ = f.Get(base + "simple/demo/"); status != 200 || !strings.Contains(body, "#sha256="+digestOf(sdistBytes)) || !strings.Contains(body, `data-yanked="broken"`) || !strings.Contains(body, `data-core-metadata="sha256=`+digestOf(core)) {
		t.Fatalf("html: %d %s", status, body)
	}

	wheelURL := srv.URL + page.Files[0].URL
	// Pip requests the PEP 658 sidecar before the wheel: a cold cache must pull
	// it through on the first request (regression: first request returned 404).
	if status, body, _ = f.Get(wheelURL + ".metadata"); status != 200 || body != core {
		t.Fatalf("cold PEP 658 sidecar: %d %q", status, body)
	}
	if status, body, _ = f.Get(wheelURL); status != 200 || body != wheelBytes {
		t.Fatalf("wheel via cdn: %d %q", status, body)
	}
	if status, body, _ = f.Get(wheelURL + ".metadata"); status != 200 || body != core {
		t.Fatalf("PEP 658 sidecar: %d %q", status, body)
	}
	sdistURL := srv.URL + page.Files[1].URL
	if status, body, _ = f.Get(sdistURL); status != 200 || body != sdistBytes {
		t.Fatalf("sdist via origin: %d %q", status, body)
	}
	if cdnAuth.Load() != "" {
		t.Fatalf("source credential reached the CDN origin: %v", cdnAuth.Load())
	}
	if f.CachedAssets(remote) != 3 {
		t.Fatalf("cached %d assets, want wheel+metadata+sdist", f.CachedAssets(remote))
	}
	// Everything cached is served with both upstreams down.
	down.Store(true)
	cdn.Close()
	for _, u := range []string{wheelURL, sdistURL} {
		if status, _, _ = f.Get(u); status != 200 {
			t.Fatalf("cached %s during outage: %d", u, status)
		}
	}
	if status, _, _ = f.Get(base + "simple/other/"); status != 503 {
		t.Fatalf("uncached project during outage: %d", status)
	}
}

func TestPyPIPullThroughRejectsTamperedFileAndForeignDigest(t *testing.T) {
	f := remotetest.New(t)
	const wheel = "demo-2.0-py3-none-any.whl"
	good := "GOOD"
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/demo/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body><a href="/files/%s#sha256=%s" data-requires-python="&gt;=3.9">%s</a></body></html>`, wheel, digestOf(good), wheel)
		case "/files/" + wheel:
			_, _ = w.Write([]byte("EVIL"))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("py2", "pypi")
	remote := f.AddRemote(feed, "pypi", "html-only", 0, up.URL, remotetest.RemoteOptions{})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/py2/pypi/"
	status, body, _ := f.Get(base + "simple/demo/")
	if status != 200 || !strings.Contains(body, wheel) || !strings.Contains(body, `data-requires-python="&gt;=3.9"`) {
		t.Fatalf("HTML-only upstream: %d %s", status, body)
	}
	link := base + "files/demo/" + remote + "/" + digestOf(good) + "/" + wheel
	if status, _, _ = f.Get(link); status == 200 || f.CachedAssets(remote) != 0 {
		t.Fatalf("tampered file served or cached: %d", status)
	}
	// A digest that is not the one the upstream advertised is not resolvable.
	if status, _, _ = f.Get(base + "files/demo/" + remote + "/" + digestOf("other") + "/" + wheel); status != 404 {
		t.Fatalf("unadvertised digest: %d", status)
	}
}
