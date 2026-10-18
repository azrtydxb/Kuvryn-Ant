package npmregistry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestNPMPullThroughPackumentRewriteAndTarballCache(t *testing.T) {
	f := remotetest.New(t)
	var down atomic.Bool
	var tarballs, accepts atomic.Int32
	var lastAccept atomic.Value
	packument := func(name string) string {
		base := strings.TrimPrefix(name[strings.LastIndex(name, "/")+1:], "")
		return `{"name":"` + name + `","dist-tags":{"latest":"1.0.0","next":"2.0.0-rc.1"},"time":{"1.0.0":"2020-01-01T00:00:00Z"},"versions":{` +
			`"1.0.0":{"name":"` + name + `","version":"1.0.0","dist":{"tarball":"https://registry.upstream.invalid/` + name + `/-/` + base + `-1.0.0.tgz","shasum":"abc","integrity":"sha512-xyz"}},` +
			`"2.0.0-rc.1":{"name":"` + name + `","version":"2.0.0-rc.1","dist":{"tarball":"https://registry.upstream.invalid/x.tgz","shasum":"def"}}}}`
	}
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/leftpad", "/@acme/tool":
			accepts.Add(1)
			lastAccept.Store(r.Header.Get("Accept"))
			w.Header().Set("Content-Type", r.Header.Get("Accept"))
			_, _ = w.Write([]byte(packument(strings.TrimPrefix(r.URL.Path, "/"))))
		case "/leftpad/-/leftpad-1.0.0.tgz", "/@acme/tool/-/tool-1.0.0.tgz":
			tarballs.Add(1)
			_, _ = w.Write([]byte("TARBALL:" + r.URL.Path))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("npm1", "npm")
	remote := f.AddRemote(feed, "npm", "registry", 0, up.URL, remotetest.RemoteOptions{MetadataTTL: 1, StaleSeconds: 60})
	registry, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	if err = registry.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"leftpad", "@acme/tool"} {
		status, body, _ := f.Get(srv.URL + "/feeds/npm1/npm/" + name)
		if status != 200 {
			t.Fatalf("%s packument: %d %s", name, status, body)
		}
		var doc struct {
			Tags     map[string]string
			Time     map[string]string
			Versions map[string]struct {
				Dist map[string]string
			}
		}
		raw := map[string]json.RawMessage{}
		if err = json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw["dist-tags"], &doc.Tags)
		_ = json.Unmarshal(raw["time"], &doc.Time)
		_ = json.Unmarshal(raw["versions"], &doc.Versions)
		if strings.Contains(body, "upstream.invalid") {
			t.Fatalf("upstream URL leaked into packument: %s", body)
		}
		want := srv.URL + "/feeds/npm1/npm/" + name + "/-/" + name[strings.LastIndex(name, "/")+1:] + "-1.0.0.tgz?source=" + remote
		if doc.Versions["1.0.0"].Dist["tarball"] != want || doc.Versions["1.0.0"].Dist["integrity"] != "sha512-xyz" || doc.Tags["next"] != "2.0.0-rc.1" || doc.Time["1.0.0"] == "" {
			t.Fatalf("rewrite wrong for %s: %s", name, body)
		}
	}
	// Single-version and dist-tags routes are served from the same document.
	if status, body, _ := f.Get(srv.URL + "/feeds/npm1/npm/leftpad/latest"); status != 200 || !strings.Contains(body, `"version":"1.0.0"`) {
		t.Fatalf("version by tag: %d %s", status, body)
	}
	if status, body, _ := f.Get(srv.URL + "/feeds/npm1/npm/-/package/leftpad/dist-tags"); status != 200 || !strings.Contains(body, `"latest":"1.0.0"`) {
		t.Fatalf("dist-tags: %d %s", status, body)
	}
	// Abbreviated documents are requested and answered when the client asks for them.
	status, _, header := f.Get(srv.URL+"/feeds/npm1/npm/leftpad", "Accept", abbreviated)
	if status != 200 || header.Get("Content-Type") != abbreviated || lastAccept.Load() != abbreviated {
		t.Fatalf("abbreviated: %d %v", status, header)
	}

	tarball := srv.URL + "/feeds/npm1/npm/leftpad/-/leftpad-1.0.0.tgz?source=" + remote
	if status, body, h := f.Get(tarball); status != 200 || body != "TARBALL:/leftpad/-/leftpad-1.0.0.tgz" || h.Get("X-Kuvryn-Cache") != "miss" {
		t.Fatalf("tarball miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatal("tarball not cached")
	}
	// A tarball pinned to a source that is not part of the feed is not found.
	if status, _, _ := f.Get(srv.URL + "/feeds/npm1/npm/@acme/tool/-/tool-1.0.0.tgz?source=00000000-0000-4000-8000-000000000009"); status != 404 {
		t.Fatalf("foreign source token accepted: %d", status)
	}

	down.Store(true)
	time.Sleep(1200 * time.Millisecond)
	if status, body, _ := f.Get(tarball); status != 200 || body != "TARBALL:/leftpad/-/leftpad-1.0.0.tgz" || tarballs.Load() != 1 {
		t.Fatalf("cached tarball during outage: %d", status)
	}
	status, body, header := f.Get(srv.URL + "/feeds/npm1/npm/@acme/tool")
	if status != 200 || header.Get("X-Kuvryn-Cache") != "stale" || !strings.Contains(body, "dist-tags") {
		t.Fatalf("stale packument during outage: %d %v", status, header)
	}
	if status, _, _ = f.Get(srv.URL + "/feeds/npm1/npm/never-fetched"); status != 503 {
		t.Fatalf("unfetched package during outage: %d", status)
	}
}
