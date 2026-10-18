package terraform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestParseGet(t *testing.T) {
	cases := []struct {
		in, target, subdir string
		ok                 bool
	}{
		{"https://api.github.com/repos/o/r/tarball/abc//*?archive=tar.gz", "https://api.github.com/repos/o/r/tarball/abc", "*", true},
		{"https://example.com/mod.tar.gz", "https://example.com/mod.tar.gz", "", true},
		{"git::https://github.com/o/r.git?ref=v1.2.3", githubArchives + "/o/r/tar.gz/v1.2.3", "*", true},
		{"git::https://github.com/o/r?ref=release/1.x", githubArchives + "/o/r/tar.gz/release/1.x", "*", true},
		{"git::https://gitlab.com/o/r?ref=v1", "", "", false},
		{"git::https://github.com/o/r?ref=../x", "", "", false},
		{"git::https://github.com/o/r", "", "", false},
		{"http://example.com/mod.tar.gz", "", "", false},
		{"https://example.com/mod.zip?archive=zip", "", "", false},
		{"https://example.com/m.tar.gz//../x", "", "", false},
	}
	for _, c := range cases {
		target, subdir, ok := parseGet(c.in)
		if ok != c.ok || target != c.target || subdir != c.subdir {
			t.Errorf("parseGet(%q) = %q %q %v", c.in, target, subdir, ok)
		}
	}
}

func TestTerraformPullThrough(t *testing.T) {
	f := remotetest.New(t)
	prov := newProvider(t, "random", "3.5.0", "linux/amd64", "darwin/arm64")
	evil := derivedProvider(t, prov, "evil", "1.0.0")
	badsig := derivedProvider(t, prov, "badsig", "1.0.0")
	otherKey, _ := newEntity(t)
	badsig.sign(t, otherKey) // signed by a different key than the one advertised
	modTar := moduleTarball(t, map[string]string{"repo-abc/main.tf": "variable \"x\" {}\n"})
	var releaseHits, archiveHits atomic.Int32
	var down atomic.Bool

	release := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the release origin")
		}
		releaseHits.Add(1)
		name := strings.TrimPrefix(r.URL.Path, "/")
		for _, p := range []providerFixture{prov, evil, badsig} {
			if b, ok := p.zips[name]; ok {
				if strings.HasPrefix(name, "terraform-provider-evil") {
					b = []byte("not the released zip")
				}
				_, _ = w.Write(b)
				return
			}
			if strings.HasSuffix(name, "_SHA256SUMS") && strings.Contains(name, providerType(p)) {
				_, _ = w.Write(p.sums)
				return
			}
			if strings.HasSuffix(name, "_SHA256SUMS.sig") && strings.Contains(name, providerType(p)) {
				_, _ = w.Write(p.sig)
				return
			}
		}
		http.NotFound(w, r)
	}))
	archives := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveHits.Add(1)
		switch r.URL.Path {
		case "/acme/terraform-aws-vpc/tar.gz/v1.0.0":
			_, _ = w.Write(modTar)
		case "/acme/bad/tar.gz/v1.0.1":
			_, _ = w.Write([]byte("this is not a gzip stream"))
		default:
			http.NotFound(w, r)
		}
	}))
	downloadDoc := func(p providerFixture, typ, ver, os, arch string) string {
		name := zipName(typ, ver, os, arch)
		return fmt.Sprintf(`{"protocols":["5.0"],"os":%q,"arch":%q,"filename":%q,"download_url":"%s/%s","shasums_url":"%s/%s","shasums_signature_url":"%s/%s","shasum":%q,"signing_keys":{"gpg_public_keys":[{"key_id":"X","ascii_armor":%q}]}}`,
			os, arch, name, release.URL, name, release.URL, sumsName(typ, ver), release.URL, sumsName(typ, ver)+".sig", sha(p.zips[name]), p.pub)
	}
	upstream := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if strings.Contains(r.URL.Path, "/owned/") {
			t.Errorf("locally owned package requested upstream: %s", r.URL.Path)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/v1/providers/hashicorp/random/versions":
			_, _ = w.Write([]byte(`{"id":"hashicorp/random","versions":[{"version":"3.5.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"},{"os":"darwin","arch":"arm64"}]},{"version":"bad version","protocols":["5.0"],"platforms":[]}]}`))
		case "/v1/providers/hashicorp/random/3.5.0/download/linux/amd64":
			_, _ = w.Write([]byte(downloadDoc(prov, "random", "3.5.0", "linux", "amd64")))
		case "/v1/providers/hashicorp/random/3.5.0/download/darwin/arm64":
			_, _ = w.Write([]byte(downloadDoc(prov, "random", "3.5.0", "darwin", "arm64")))
		case "/v1/providers/hashicorp/evil/versions":
			_, _ = w.Write([]byte(`{"versions":[{"version":"1.0.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"}]}]}`))
		case "/v1/providers/hashicorp/evil/1.0.0/download/linux/amd64":
			_, _ = w.Write([]byte(downloadDoc(evil, "evil", "1.0.0", "linux", "amd64")))
		case "/v1/providers/hashicorp/badsig/versions":
			_, _ = w.Write([]byte(`{"versions":[{"version":"1.0.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"}]}]}`))
		case "/v1/providers/hashicorp/badsig/1.0.0/download/linux/amd64":
			_, _ = w.Write([]byte(downloadDoc(badsig, "badsig", "1.0.0", "linux", "amd64")))
		case "/v1/modules/acme/vpc/aws/versions":
			_, _ = w.Write([]byte(`{"modules":[{"source":"acme/vpc/aws","versions":[{"version":"1.0.0","root":{}},{"version":"nope"}]}]}`))
		case "/v1/modules/acme/vpc/aws/1.0.0":
			_, _ = w.Write([]byte(`{"id":"acme/vpc/aws/1.0.0","source":"https://github.com/acme/terraform-aws-vpc","tag":"v1.0.0"}`))
		case "/v1/modules/acme/vpc/aws/1.0.1":
			_, _ = w.Write([]byte(`{"id":"acme/vpc/aws/1.0.1","source":"https://github.com/acme/bad","tag":"v1.0.1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("tf", "terraform")
	local := f.AddLocal(feed, "terraform", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	githubArchives = archives.URL
	t.Cleanup(func() { githubArchives = "https://codeload.github.com" })
	remote := f.AddRemote(feed, "terraform", "registry", 1, upstream.URL, remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{release.URL, archives.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/tf/terraform/v1/"

	// Local owner: publish a module "owned/mod/aws"; upstream is never asked about it.
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", base+"modules/owned/mod/aws/1.0.0", bytes.NewReader(moduleTarball(t, map[string]string{"main.tf": "x"})))
	req.Header.Set("Authorization", "Bearer "+admin.Secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("publish: %v %v", err, res)
	}
	res.Body.Close()
	if status, body, _ := f.Get(base + "modules/owned/mod/aws/versions"); status != 200 || !strings.Contains(body, "1.0.0") {
		t.Fatalf("local versions %d %s", status, body)
	}
	if status, _, _ := f.Get(base + "modules/owned/mod/aws/2.0.0/download"); status != 404 {
		t.Fatalf("owned remote version reachable: %d", status)
	}

	// provider versions and download document
	status, body, _ := f.Get(base + "providers/hashicorp/random/versions")
	if status != 200 || !strings.Contains(body, `"3.5.0"`) || strings.Contains(body, "bad version") {
		t.Fatalf("provider versions: %d %s", status, body)
	}
	status, body, _ = f.Get(base + "providers/hashicorp/random/3.5.0/download/linux/amd64")
	files := base + "providers/hashicorp/random/3.5.0/files/"
	zip := zipName("random", "3.5.0", "linux", "amd64")
	if status != 200 || !strings.Contains(body, `"download_url":"`+files+zip+`"`) || !strings.Contains(body, `"shasums_url":"`+files+sumsName("random", "3.5.0")+`"`) || strings.Contains(body, release.URL) || !strings.Contains(body, `"shasum":"`+sha(prov.zips[zip])+`"`) || !strings.Contains(body, "BEGIN PGP PUBLIC KEY BLOCK") {
		t.Fatalf("provider download: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "providers/hashicorp/nonexistent/versions"); status != 404 {
		t.Fatalf("unknown provider: %d", status)
	}

	// files: zip, SHA256SUMS and signature pass through verified, then are cache hits
	if status, body, _ = f.Get(files + zip); status != 200 || body != string(prov.zips[zip]) {
		t.Fatalf("zip miss: %d", status)
	}
	if status, body, _ = f.Get(files + sumsName("random", "3.5.0")); status != 200 || body != string(prov.sums) {
		t.Fatalf("sums: %d", status)
	}
	if status, body, _ = f.Get(files + sumsName("random", "3.5.0") + ".sig"); status != 200 || body != string(prov.sig) {
		t.Fatalf("sig: %d", status)
	}
	if f.CachedAssets(remote) != 3 {
		t.Fatalf("cached %d assets, want 3", f.CachedAssets(remote))
	}
	n := releaseHits.Load()
	if status, body, _ = f.Get(files + zip); status != 200 || body != string(prov.zips[zip]) || releaseHits.Load() != n {
		t.Fatalf("zip hit: %d", status)
	}
	// digest mismatch and wrong-key signature are rejected, nothing cached
	cached := f.CachedAssets(remote)
	if status, body, _ = f.Get(base + "providers/hashicorp/evil/1.0.0/files/" + zipName("evil", "1.0.0", "linux", "amd64")); status == 200 || strings.Contains(body, "not the released") {
		t.Fatalf("digest mismatch served: %d", status)
	}
	if status, _, _ = f.Get(base + "providers/hashicorp/badsig/1.0.0/files/" + sumsName("badsig", "1.0.0") + ".sig"); status == 200 {
		t.Fatalf("bad signature served")
	}
	if f.CachedAssets(remote) != cached+1 { // the verified SHA256SUMS fetched on the way
		t.Fatalf("cached %d assets after rejects, want %d", f.CachedAssets(remote), cached+1)
	}

	// modules: versions, download resolves the upstream header once and caches the archive
	if status, body, _ = f.Get(base + "modules/acme/vpc/aws/versions"); status != 200 || !strings.Contains(body, `"1.0.0"`) || strings.Contains(body, "nope") {
		t.Fatalf("module versions: %d %s", status, body)
	}
	req, _ = http.NewRequest("GET", base+"modules/acme/vpc/aws/1.0.0/download", nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 204 {
		t.Fatalf("module download: %v %v", err, res)
	}
	res.Body.Close()
	get := base + "modules/acme/vpc/aws/1.0.0/archive.tar.gz"
	if res.Header.Get("X-Terraform-Get") != get+"//*" {
		t.Fatalf("X-Terraform-Get %q", res.Header.Get("X-Terraform-Get"))
	}
	if status, body, _ = f.Get(get); status != 200 || body != string(modTar) {
		t.Fatalf("module archive: %d", status)
	}
	hits := archiveHits.Load()
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 204 || res.Header.Get("X-Terraform-Get") != get+"//*" || archiveHits.Load() != hits {
		t.Fatalf("module download hit: %v %v", err, res)
	}
	res.Body.Close()
	// a body that is not a tar.gz is rejected
	if status, _, _ = f.Get(base + "modules/acme/vpc/aws/1.0.1/archive.tar.gz"); status == 200 {
		t.Fatalf("invalid archive served")
	}

	// outage: cached files and module downloads keep working, cached versions list from the store
	down.Store(true)
	if status, body, _ = f.Get(files + zip); status != 200 || body != string(prov.zips[zip]) {
		t.Fatalf("zip in outage: %d", status)
	}
	if status, body, _ = f.Get(get); status != 200 || body != string(modTar) {
		t.Fatalf("archive in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "modules/acme/vpc/aws/versions"); status != 200 || !strings.Contains(body, "1.0.0") {
		t.Fatalf("module versions in outage: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "providers/hashicorp/uncached/versions"); status != 503 {
		t.Fatalf("uncached provider in outage: %d", status)
	}
}

func providerType(p providerFixture) string {
	for n := range p.zips {
		return strings.SplitN(strings.TrimPrefix(n, "terraform-provider-"), "_", 2)[0]
	}
	return ""
}

// derivedProvider builds a provider that reuses base's signing key, so tests
// do not pay for key generation repeatedly.
func derivedProvider(t *testing.T, base providerFixture, typ, ver string) providerFixture {
	t.Helper()
	p := providerFixture{entity: base.entity, pub: base.pub, zips: map[string][]byte{}}
	name := zipName(typ, ver, "linux", "amd64")
	p.zips[name] = providerZip(t, "terraform-provider-"+typ+"_v"+ver, "binary for "+typ)
	p.sums = sumLine(name, p.zips[name])
	p.sign(t, base.entity)
	return p
}
