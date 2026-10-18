package rpmrepo

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func gz(b []byte) []byte {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, _ = w.Write(b)
	_ = w.Close()
	return z.Bytes()
}

func TestRPMPullThrough(t *testing.T) {
	f := remotetest.New(t)
	rpm := []byte("RPM-BYTES")
	owned2 := []byte("OWNED-2")
	pkg := func(name, ver string, data []byte) string {
		return fmt.Sprintf(`<package type="rpm"><name>%s</name><arch>x86_64</arch><version epoch="0" ver="%s" rel="1.el9"/><checksum type="sha256" pkgid="YES">%s</checksum><size package="%d" installed="1" archive="1"/><location href="Packages/%s/%s-%s-1.el9.x86_64.rpm"/></package>`, name, ver, hexSum(data), len(data), name[:1], name, ver)
	}
	primary := gz([]byte(`<?xml version="1.0"?><metadata xmlns="http://linux.duke.edu/metadata/common" packages="2">` + pkg("hello", "1.0", rpm) + pkg("owned", "2.0", owned2) + `</metadata>`))
	primaryHref := "repodata/" + hexSum(primary) + "-primary.xml.gz"
	repomd := fmt.Sprintf(`<?xml version="1.0"?><repomd xmlns="http://linux.duke.edu/metadata/repo"><revision>1</revision><data type="primary"><checksum type="sha256">%s</checksum><open-checksum type="sha256">x</open-checksum><location href="%s"/><size>%d</size></data><data type="other"><checksum type="sha512">abc</checksum><location href="repodata/other.xml.gz"/></data></repomd>`, hexSum(primary), primaryHref, len(primary))
	var hits, pkgHits atomic.Int32
	var down, tamper atomic.Bool
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/os/repodata/repomd.xml":
			_, _ = w.Write([]byte(repomd))
		case "/os/repodata/repomd.xml.asc":
			_, _ = w.Write([]byte("ASC"))
		case "/os/" + primaryHref:
			_, _ = w.Write(primary)
		case "/os/Packages/h/hello-1.0-1.el9.x86_64.rpm":
			pkgHits.Add(1)
			if tamper.Load() {
				_, _ = w.Write([]byte("EVIL!!!!"))
				return
			}
			_, _ = w.Write(rpm)
		case "/os/Packages/o/owned-2.0-1.el9.x86_64.rpm":
			t.Errorf("locally owned package requested upstream")
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("rocky", "rpm")
	local := f.AddLocal(feed, "rpm", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "rpm", "rocky", 1, up.URL+"/os", remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/rocky/rpm/"

	// A package name owned locally is never completed from upstream.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ap, err := (identity.Tokens{DB: f.Pool}).Authenticate(context.Background(), admin.Secret)
	if err != nil {
		t.Fatal(err)
	}
	owned1 := []byte("OWNED-1")
	if _, err = f.Service.Publish(context.Background(), ap, artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: "owned", Version: "1.0-1.el9.x86_64", AssetKey: packageAsset, MediaType: "application/x-rpm", Info: blob.Info{Size: int64(len(owned1)), Digest: blob.Digest(hexSum(owned1))}}, bytes.NewReader(owned1)); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if status, body, _ := f.Get(base + "Packages/owned-1.0-1.el9.x86_64.rpm"); status != 200 || body != string(owned1) {
		t.Fatalf("local package: %d %q", status, body)
	}
	if status, _, _ := f.Get(base + "Packages/o/owned-2.0-1.el9.x86_64.rpm"); status != 404 || hits.Load() != before {
		t.Fatalf("owned name reached upstream: %d", status)
	}

	// repomd verbatim; repomd.xml.asc verbatim
	if status, body, _ := f.Get(base + "repodata/repomd.xml"); status != 200 || body != repomd {
		t.Fatalf("repomd not verbatim: %d", status)
	}
	if status, body, _ := f.Get(base + "repodata/repomd.xml.asc"); status != 200 || body != "ASC" {
		t.Fatalf("asc: %d", status)
	}
	// listed data file: verified and cached; unlisted and non-sha256 entries refused
	if status, body, _ := f.Get(base + primaryHref); status != 200 || body != string(primary) {
		t.Fatalf("primary: %d", status)
	}
	if status, _, _ := f.Get(base + "repodata/other.xml.gz"); status != 404 {
		t.Fatalf("unverifiable (sha512) entry served: %d", status)
	}
	if status, _, _ := f.Get(base + "repodata/unlisted.xml.gz"); status != 404 {
		t.Fatalf("unlisted: %d", status)
	}

	// package in a subdirectory: tampered bytes rejected and not cached
	tamper.Store(true)
	n := f.CachedAssets(remote)
	if status, _, _ := f.Get(base + "Packages/h/hello-1.0-1.el9.x86_64.rpm"); status == 200 || f.CachedAssets(remote) != n {
		t.Fatalf("tampered package served or cached: %d", status)
	}
	tamper.Store(false)
	// miss -> verify -> cache -> hit
	url := base + "Packages/h/hello-1.0-1.el9.x86_64.rpm"
	if status, body, _ := f.Get(url); status != 200 || body != string(rpm) {
		t.Fatalf("package miss: %d %q", status, body)
	}
	pn := pkgHits.Load()
	if status, body, _ := f.Get(url); status != 200 || body != string(rpm) || pkgHits.Load() != pn {
		t.Fatalf("package hit went upstream: %d", status)
	}
	// not listed in primary
	if status, _, _ := f.Get(base + "Packages/h/hello-9.9-1.el9.x86_64.rpm"); status != 404 {
		t.Fatalf("unlisted package: %d", status)
	}

	// outage: cached package and data keep working, repomd served stale
	down.Store(true)
	if status, _, _ := f.Get(url); status != 200 {
		t.Fatalf("cached package in outage: %d", status)
	}
	if status, body, _ := f.Get(base + primaryHref); status != 200 || body != string(primary) {
		t.Fatalf("cached primary in outage: %d", status)
	}
	if status, body, _ := f.Get(base + "repodata/repomd.xml"); status != 200 || body != repomd {
		t.Fatalf("stale repomd: %d", status)
	}
}
