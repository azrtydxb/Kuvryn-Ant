package maven

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestMavenPullThroughArtifactsAndMetadata(t *testing.T) {
	f := remotetest.New(t)
	var down atomic.Bool
	var calls atomic.Int32
	files := map[string]string{
		"/com/acme/lib/1.0/lib-1.0.pom":         "<project/>",
		"/com/acme/lib/1.0/lib-1.0.jar":         "JARBYTES",
		"/com/acme/lib/1.0/lib-1.0.jar.sha1":    "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		"/com/acme/lib/maven-metadata.xml":      `<metadata><groupId>com.acme</groupId><artifactId>lib</artifactId><versioning><latest>2.0</latest><release>2.0</release><versions><version>1.0</version><version>2.0</version></versions></versioning></metadata>`,
		"/com/acme/lib/maven-metadata.xml.sha1": "abc123",
	}
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(r.URL.Path) > 4 && r.URL.Path[len(r.URL.Path)-4:] == ".xml" {
			w.Header().Set("Content-Type", "application/xml")
		}
		_, _ = w.Write([]byte(body))
	}))
	feed := f.AddFeed("mvn", "maven")
	remote := f.AddRemote(feed, "maven", "central", 0, up.URL, remotetest.RemoteOptions{MetadataTTL: 1, StaleSeconds: 60})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/mvn/maven/com/acme/lib/"

	for path, want := range map[string]string{"1.0/lib-1.0.pom": "<project/>", "1.0/lib-1.0.jar": "JARBYTES", "1.0/lib-1.0.jar.sha1": files["/com/acme/lib/1.0/lib-1.0.jar.sha1"]} {
		if status, body, _ := f.Get(base + path); status != 200 || body != want {
			t.Fatalf("%s: %d %q", path, status, body)
		}
	}
	if f.CachedAssets(remote) != 3 {
		t.Fatalf("cached %d assets, want 3", f.CachedAssets(remote))
	}
	// Metadata is proxied verbatim (upstream version list, not just cached versions).
	status, body, _ := f.Get(base + "maven-metadata.xml")
	if status != 200 || body != files["/com/acme/lib/maven-metadata.xml"] {
		t.Fatalf("metadata: %d %q", status, body)
	}
	if status, body, _ = f.Get(base + "maven-metadata.xml.sha1"); status != 200 || body != "abc123" {
		t.Fatalf("metadata checksum: %d %q", status, body)
	}

	// Outage: cached artifacts still resolve; expired metadata is served stale and marked.
	down.Store(true)
	time.Sleep(1200 * time.Millisecond)
	if status, body, _ = f.Get(base + "1.0/lib-1.0.jar"); status != 200 || body != "JARBYTES" {
		t.Fatalf("cached artifact during outage: %d", status)
	}
	status, body, header := f.Get(base + "maven-metadata.xml")
	if status != 200 || body != files["/com/acme/lib/maven-metadata.xml"] || header.Get("X-Kuvryn-Cache") != "stale" {
		t.Fatalf("stale metadata: %d %v", status, header)
	}
	// Never cached and the upstream is down: a visible 503, not a 404.
	if status, _, _ = f.Get(base + "9.9/lib-9.9.jar"); status != 503 {
		t.Fatalf("uncached during outage: %d", status)
	}
	down.Store(false)
	// A genuinely missing artifact is a 404 and is negatively cached.
	before := calls.Load()
	for i := 0; i < 3; i++ {
		if status, _, _ = f.Get(base + "3.0/lib-3.0.jar"); status != 404 {
			t.Fatalf("missing: %d", status)
		}
	}
	if calls.Load()-before != 1 {
		t.Fatalf("negative cache: %d upstream requests for repeated misses", calls.Load()-before)
	}
}
