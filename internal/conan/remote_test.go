package conan

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func TestConanPullThrough(t *testing.T) {
	f := remotetest.New(t)
	conanfile := "from conan import ConanFile\nclass Zlib(ConanFile):\n    pass\n"
	rrev, rmanifest := conanRecipeLock(t, conanfile)
	pid := strings.Repeat("ab", 20)
	prev, pmanifest, tgz := conanPackageLock(t, "[settings]\nos=Linux\n", "lib/libz.a", "ARCHIVE")
	// A second recipe revision whose served conanfile does not match its manifest.
	_, badManifest := conanRecipeLock(t, "genuine recipe\n")
	badRev := md5h([]byte("conanfile.py: " + md5h([]byte("genuine recipe\n")) + "\n"))
	// A package whose tarball member differs from the manifest.
	_, _, evilTgz := conanPackageLock(t, "[settings]\nos=Linux\n", "lib/libz.a", "TAMPERED")

	var fileHits atomic.Int32
	var down atomic.Bool
	root := "/v2/conans/zlib/1.3/_/_"
	pkgRoot := root + "/revisions/" + rrev + "/packages/" + pid
	pkgRoot2 := root + "/revisions/" + rrev + "/packages/" + strings.Repeat("cd", 20)
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if strings.Contains(r.URL.Path, "/owned/") {
			t.Errorf("locally owned reference requested upstream: %s", r.URL.Path)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		if strings.Contains(r.URL.Path, "/files/") {
			fileHits.Add(1)
		}
		switch r.URL.Path {
		case root + "/latest":
			fmt.Fprintf(w, `{"revision":"%s","time":"2024-01-01T00:00:00Z"}`, rrev)
		case root + "/revisions":
			fmt.Fprintf(w, `{"revisions":[{"revision":"%s","time":"2024-01-01T00:00:00Z"},{"revision":"zz","time":"x"}]}`, rrev)
		case root + "/revisions/" + rrev + "/files":
			_, _ = w.Write([]byte(`{"files":{"conanfile.py":{},"conanmanifest.txt":{},"../evil":{}}}`))
		case root + "/revisions/" + rrev + "/search":
			fmt.Fprintf(w, `{"%s":{"settings":{"os":"Linux"}},"nothex":{}}`, pid)
		case root + "/revisions/" + rrev + "/files/conanmanifest.txt":
			_, _ = w.Write(rmanifest)
		case root + "/revisions/" + rrev + "/files/conanfile.py":
			_, _ = w.Write([]byte(conanfile))
		case root + "/revisions/" + badRev + "/files/conanmanifest.txt":
			_, _ = w.Write(badManifest)
		case root + "/revisions/" + badRev + "/files/conanfile.py":
			_, _ = w.Write([]byte("tampered recipe\n"))
		case pkgRoot + "/revisions/" + prev + "/files/conanmanifest.txt":
			_, _ = w.Write(pmanifest)
		case pkgRoot + "/revisions/" + prev + "/files/conan_package.tgz":
			_, _ = w.Write(tgz)
		case pkgRoot + "/revisions/" + badRev + "/files/conanmanifest.txt":
			_, _ = w.Write(pmanifest)
		case pkgRoot2 + "/revisions/" + prev + "/files/conanmanifest.txt":
			_, _ = w.Write(pmanifest)
		case pkgRoot2 + "/revisions/" + prev + "/files/conan_package.tgz":
			_, _ = w.Write(evilTgz)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("packages", "conan")
	local := f.AddLocal(feed, "conan", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "conan", "conancenter", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/packages/conan"

	// Local owner: publish "owned"; the upstream handler fails the test if asked about it.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	put := func(path string, body []byte) {
		req, _ := http.NewRequest("PUT", base+path, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+admin.Secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("PUT %s: %v %v", path, err, res)
		}
		res.Body.Close()
	}
	lrev, lmanifest := conanRecipeLock(t, "local recipe\n")
	put("/v2/conans/owned/1.0/_/_/revisions/"+lrev+"/files/conanfile.py", []byte("local recipe\n"))
	put("/v2/conans/owned/1.0/_/_/revisions/"+lrev+"/files/conanmanifest.txt", lmanifest)
	if status, body, _ := f.Get(base + "/v2/conans/owned/1.0/_/_/latest"); status != 200 || !strings.Contains(body, lrev) {
		t.Fatalf("local latest %d %s", status, body)
	}

	// listings come from upstream, validated
	status, body, _ := f.Get(base + root + "/latest")
	if status != 200 || !strings.Contains(body, rrev) {
		t.Fatalf("latest: %d %s", status, body)
	}
	if status, body, _ = f.Get(base + root + "/revisions"); status != 200 || !strings.Contains(body, rrev) || strings.Contains(body, `"zz"`) {
		t.Fatalf("revisions: %d %s", status, body)
	}
	if status, body, _ = f.Get(base + root + "/revisions/" + rrev + "/files"); status != 200 || !strings.Contains(body, "conanfile.py") || strings.Contains(body, "evil") {
		t.Fatalf("files: %d %s", status, body)
	}
	if status, body, _ = f.Get(base + root + "/revisions/" + rrev + "/search"); status != 200 || !strings.Contains(body, pid) || strings.Contains(body, "nothex") {
		t.Fatalf("search: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "/v2/conans/nosuch/1.0/_/_/latest"); status != 404 {
		t.Fatalf("unknown ref: %d", status)
	}

	// recipe file: miss -> verified against manifest -> cached -> hit
	dl := base + root + "/revisions/" + rrev + "/files/conanfile.py"
	if status, body, _ = f.Get(dl); status != 200 || body != conanfile {
		t.Fatalf("recipe miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d assets, want recipe+manifest", f.CachedAssets(remote))
	}
	n := fileHits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != conanfile || fileHits.Load() != n {
		t.Fatalf("recipe hit: %d", status)
	}
	// package tarball is verified member by member
	pdl := base + pkgRoot + "/revisions/" + prev + "/files/conan_package.tgz"
	if status, body, _ = f.Get(pdl); status != 200 || body != string(tgz) {
		t.Fatalf("package miss: %d", status)
	}
	if f.CachedAssets(remote) != 4 {
		t.Fatalf("cached %d assets, want 4", f.CachedAssets(remote))
	}
	// a tarball whose member differs from the manifest is rejected
	if status, body, _ = f.Get(base + pkgRoot2 + "/revisions/" + prev + "/files/conan_package.tgz"); status == 200 || strings.Contains(body, "TAMPERED") {
		t.Fatalf("tampered tarball served: %d", status)
	}
	if f.CachedAssets(remote) != 5 { // plus the verified manifest of that package
		t.Fatalf("cached %d assets after tarball reject, want 5", f.CachedAssets(remote))
	}
	// manifest whose digest does not match the revision in the URL is rejected
	cached := f.CachedAssets(remote)
	if status, _, _ = f.Get(base + pkgRoot + "/revisions/" + badRev + "/files/conanmanifest.txt"); status == 200 {
		t.Fatalf("manifest/revision mismatch served")
	}
	// a file that does not match its manifest is rejected and not cached
	if status, body, _ = f.Get(base + root + "/revisions/" + badRev + "/files/conanfile.py"); status == 200 || strings.Contains(body, "tampered") {
		t.Fatalf("tampered file served: %d", status)
	}
	if f.CachedAssets(remote) != cached+1 { // only the verified bad-revision manifest is cached
		t.Fatalf("cached %d assets after rejects, want %d", f.CachedAssets(remote), cached+1)
	}
	// local precedence: the owned reference never reaches upstream (handler asserts)
	if status, _, _ = f.Get(base + "/v2/conans/owned/1.0/_/_/revisions/" + rrev + "/files/conanfile.py"); status != 404 {
		t.Fatalf("owned reference reachable upstream: %d", status)
	}

	// outage: cached files download; uncached references report 503
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != conanfile {
		t.Fatalf("recipe in outage: %d", status)
	}
	if status, _, _ = f.Get(base + "/v2/conans/other/1.0/_/_/latest"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
