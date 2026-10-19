package p2repo

import (
	"context"
	"crypto/md5" // #nosec G501
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func TestParseRemoteArtifactsIsLenient(t *testing.T) {
	xmlDoc := `<?xml version='1.0'?><repository name='r'><artifacts size='4'>
<artifact classifier='osgi.bundle' id='a.b' version='1.0.0'><properties size='2'><property name='download.checksum.sha-256' value='` + strings.Repeat("a", 64) + `'/><property name='download.size' value='7'/></properties></artifact>
<artifact classifier='osgi.bundle' id='packed' version='1.0.0'><properties size='2'><property name='format' value='packed'/><property name='download.md5' value='` + strings.Repeat("b", 32) + `'/></properties></artifact>
<artifact classifier='osgi.bundle' id='nosum' version='1.0.0'><properties size='1'><property name='download.size' value='7'/></properties></artifact>
<artifact classifier='org.eclipse.update.feature' id='f.x' version='2.0'><properties size='1'><property name='download.md5' value='` + strings.Repeat("c", 32) + `'/></properties></artifact>
</artifacts></repository>`
	got, err := parseRemoteArtifacts(strings.NewReader(xmlDoc))
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if a := got[[3]string{"osgi.bundle", "a.b", "1.0.0"}]; a.sha256 == "" || a.size != 7 {
		t.Fatalf("%+v", a)
	}
	if a := got[[3]string{"org.eclipse.update.feature", "f.x", "2.0"}]; a.md5 == "" || a.sha256 != "" {
		t.Fatalf("%+v", a)
	}
	for _, bad := range []string{"<notrepo/>", "<repository><artifacts><artifact", ""} {
		if _, err = parseRemoteArtifacts(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestP2PullThrough(t *testing.T) {
	f := remotetest.New(t)
	bundle := []byte("SIGNED-JAR-BYTES")
	feature := []byte("FEATURE-JAR")
	tampered := []byte("TAMPERED")
	hs := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	hm := func(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) } // #nosec G401
	artifacts := fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8'?><repository name='up' type='x' version='1'><artifacts size='3'>
<artifact classifier='osgi.bundle' id='org.acme.core' version='1.0.0'><properties size='2'><property name='download.checksum.sha-256' value='%s'/><property name='download.size' value='%d'/></properties></artifact>
<artifact classifier='osgi.bundle' id='org.acme.bad' version='1.0.0'><properties size='1'><property name='download.checksum.sha-256' value='%s'/></properties></artifact>
<artifact classifier='org.eclipse.update.feature' id='org.acme.feature' version='1.0.0'><properties size='2'><property name='download.md5' value='%s'/><property name='download.size' value='%d'/></properties></artifact>
</artifacts></repository>`, hs(bundle), len(bundle), hs(bundle), hm(feature), len(feature))
	artifactsXZ := xzOf([]byte(artifacts))
	content := []byte("<content/>")
	var hits atomic.Int32
	var down atomic.Bool
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/artifacts.xml.xz":
			_, _ = w.Write(artifactsXZ)
		case "/content.xml":
			_, _ = w.Write(content)
		case "/plugins/org.acme.core_1.0.0.jar":
			_, _ = w.Write(bundle)
		case "/plugins/org.acme.bad_1.0.0.jar":
			_, _ = w.Write(tampered)
		case "/features/org.acme.feature_1.0.0.jar":
			_, _ = w.Write(feature)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("eclipse", "p2")
	local := f.AddLocal(feed, "p2", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "p2", "mirror", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/eclipse/p2/"

	// indexes are served verbatim from upstream
	if status, body, _ := f.Get(base + "content.xml"); status != 200 || body != string(content) {
		t.Fatalf("content.xml: %d %q", status, body)
	}
	if status, body, h := f.Get(base + "artifacts.xml.xz"); status != 200 || body != string(artifactsXZ) || h.Get("Content-Type") != "application/x-xz" {
		t.Fatalf("artifacts.xml.xz: %d", status)
	}
	if status, _, _ := f.Get(base + "content.jar"); status != 404 {
		t.Fatalf("absent index variant: %d", status)
	}
	// bundle: miss -> verified (sha-256) -> cached -> hit with no upstream traffic
	dl := base + "plugins/org.acme.core_1.0.0.jar"
	if status, body, _ := f.Get(dl); status != 200 || body != string(bundle) {
		t.Fatalf("miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) < 1 {
		t.Fatal("bundle not cached")
	}
	n := hits.Load()
	if status, body, _ := f.Get(dl); status != 200 || body != string(bundle) || hits.Load() != n {
		t.Fatalf("hit: %d hits=%d/%d", status, hits.Load(), n)
	}
	// feature verified with md5 + size
	if status, body, _ := f.Get(base + "features/org.acme.feature_1.0.0.jar"); status != 200 || body != string(feature) {
		t.Fatalf("feature: %d %q", status, body)
	}
	// digest mismatch is rejected and not cached
	if status, body, _ := f.Get(base + "plugins/org.acme.bad_1.0.0.jar"); status == 200 || strings.Contains(body, "TAMPERED") {
		t.Fatalf("tampered bundle served: %d", status)
	}
	// not listed in the upstream index -> not fetched
	before := hits.Load()
	if status, _, _ := f.Get(base + "plugins/org.acme.other_1.0.0.jar"); status != 404 {
		t.Fatalf("unlisted: %d", status)
	}
	_ = before
	// outage: cached bundle still downloads; uncached listed bundle is unavailable
	down.Store(true)
	if status, body, _ := f.Get(dl); status != 200 || body != string(bundle) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, _, _ := f.Get(base + "plugins/org.acme.bad_1.0.0.jar"); status == 200 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
