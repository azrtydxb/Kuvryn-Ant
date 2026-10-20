package transfer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const artToken = "art-access-token-0123456789"

// fakeArtifactory serves the subset of the Artifactory REST API, AQL and the
// Docker v2 API that the migration uses.
type fakeArtifactory struct {
	t       *testing.T
	mu      sync.Mutex
	files   map[string]string // repo/path -> content
	corrupt map[string]string // repo/path -> bytes actually served
	blobs   map[string]string // digest hex -> content (docker blobs, per fake shared)
	// docker: repo -> image -> tag -> manifest digest
	tags      map[string]map[string]map[string]string
	manifests map[string]string // digest hex -> manifest JSON
	aqlCalls  int
	srv       *httptest.Server
}

func newFakeArtifactory(t *testing.T) *fakeArtifactory {
	f := &fakeArtifactory{t: t, corrupt: map[string]string{}, blobs: map[string]string{}, manifests: map[string]string{},
		tags: map[string]map[string]map[string]string{},
		files: map[string]string{
			"generic-local/docs/readme.txt":                     "hello",
			"generic-local/top.bin":                             "top-level",
			"generic-local/.artifactory/internal":               "ignored",
			"maven-local/com/acme/lib/1.0/lib-1.0.jar":          "jar-bytes",
			"maven-local/com/acme/lib/1.0/lib-1.0.pom":          "<project/>",
			"maven-local/com/acme/lib/maven-metadata.xml":       "<metadata/>",
			"maven-local/com/acme/lib/2.0-SNAPSHOT/lib-2.0.jar": "snap",
			"npm-local/left-pad/-/left-pad-1.0.0.tgz":           "tarball",
			"gradle-local/org/x/y/1.0/y-1.0.jar":                "gradle-jar",
		}}
	f.addImage("docker-local", "team/app", map[string]string{"latest": "v1", "1.0": "v1", "2.0": "v2"})
	f.addIndex("docker-local", "team/multi", "stable")
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if r.Header.Get("Authorization") != "Bearer "+artToken && !(ok && u == "admin" && p == "api-key-value") {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/artifactory/api/repositories", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]any{
			{"key": "generic-local", "type": "LOCAL", "packageType": "Generic"},
			{"key": "maven-local", "type": "LOCAL", "packageType": "Maven"},
			{"key": "gradle-local", "type": "LOCAL", "packageType": "Gradle"},
			{"key": "npm-local", "type": "LOCAL", "packageType": "Npm"},
			{"key": "docker-local", "type": "LOCAL", "packageType": "Docker"},
			{"key": "bower-local", "type": "LOCAL", "packageType": "Bower"},
			{"key": "maven-central", "type": "REMOTE", "packageType": "Maven"},
			{"key": "locked-remote", "type": "REMOTE", "packageType": "Npm"},
			{"key": "maven-all", "type": "VIRTUAL", "packageType": "Maven"},
			{"key": "maven-outer", "type": "VIRTUAL", "packageType": "Maven"},
			{"key": "fed-local", "type": "FEDERATED", "packageType": "Generic"},
		})
	}))
	detail := func(v map[string]any) http.HandlerFunc {
		return auth(func(w http.ResponseWriter, r *http.Request) { js(w, v) })
	}
	mux.HandleFunc("/artifactory/api/repositories/generic-local", detail(map[string]any{"rclass": "local", "packageType": "generic"}))
	mux.HandleFunc("/artifactory/api/repositories/maven-local", detail(map[string]any{"rclass": "local", "packageType": "maven", "maxUniqueSnapshots": 5}))
	mux.HandleFunc("/artifactory/api/repositories/maven-central", detail(map[string]any{"rclass": "remote", "packageType": "maven", "url": "https://repo1.maven.org/maven2", "username": "svc", "retrievalCachePeriodSecs": 7200, "proxy": "corp-proxy"}))
	mux.HandleFunc("/artifactory/api/repositories/locked-remote", auth(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", 403) }))
	mux.HandleFunc("/artifactory/api/repositories/maven-all", detail(map[string]any{"rclass": "virtual", "packageType": "maven", "repositories": []string{"maven-central", "maven-local", "maven-outer"}, "defaultDeploymentRepo": "maven-local"}))
	mux.HandleFunc("/artifactory/api/repositories/maven-outer", detail(map[string]any{"rclass": "virtual", "packageType": "maven", "repositories": []string{"gradle-local"}}))
	mux.HandleFunc("/artifactory/api/repositories/fed-local", detail(map[string]any{"rclass": "federated", "packageType": "generic"}))
	mux.HandleFunc("/artifactory/api/storageinfo", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"repositoriesSummaryList": []map[string]any{
			{"repoKey": "generic-local", "filesCount": 2, "usedSpace": "14 bytes"},
			{"repoKey": "maven-local", "filesCount": 3, "usedSpace": "1.5 KB"},
			{"repoKey": "maven-central-cache", "filesCount": 9, "usedSpaceInBytes": 4096},
			{"repoKey": "TOTAL", "filesCount": 99, "usedSpace": "1 GB"},
		}})
	}))
	mux.HandleFunc("/artifactory/api/security/users", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]any{{"name": "alice"}, {"name": "bob"}})
	}))
	mux.HandleFunc("/artifactory/api/security/groups", auth(func(w http.ResponseWriter, r *http.Request) { js(w, []map[string]any{{"name": "devs"}}) }))
	mux.HandleFunc("/artifactory/api/v2/security/permissions", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]any{{"name": "release-managers"}})
	}))
	aql := regexp.MustCompile(`^items\.find\((\{.*\})\)\.include\(.*\)\.sort\(.*\)\.offset\((\d+)\)\.limit\((\d+)\)$`)
	mux.HandleFunc("/artifactory/api/search/aql", auth(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
			http.Error(w, "bad", 400)
			return
		}
		m := aql.FindStringSubmatch(string(body))
		if m == nil {
			http.Error(w, "bad aql", 400)
			return
		}
		var crit struct{ Repo, Type string }
		_ = json.Unmarshal([]byte(m[1]), &crit)
		off, _ := strconv.Atoi(m[2])
		lim, _ := strconv.Atoi(m[3])
		f.mu.Lock()
		f.aqlCalls++
		var keys []string
		for k := range f.files {
			if strings.HasPrefix(k, crit.Repo+"/") {
				keys = append(keys, k)
			}
		}
		f.mu.Unlock()
		sort.Strings(keys)
		if off > len(keys) {
			off = len(keys)
		}
		keys = keys[off:]
		if len(keys) > lim {
			keys = keys[:lim]
		}
		var res []map[string]any
		for _, k := range keys {
			rest := strings.TrimPrefix(k, crit.Repo+"/")
			dir, name := ".", rest
			if i := strings.LastIndexByte(rest, '/'); i >= 0 {
				dir, name = rest[:i], rest[i+1:]
			}
			content := f.files[k]
			res = append(res, map[string]any{"repo": crit.Repo, "path": dir, "name": name, "size": len(content), "sha256": sum(content)})
		}
		js(w, map[string]any{"results": res, "range": map[string]any{"start_pos": off, "end_pos": off + len(res), "total": len(res), "limit": lim}})
	}))
	dockerRe := regexp.MustCompile(`^/artifactory/api/docker/([^/]+)/v2/(.*)$`)
	mux.HandleFunc("/artifactory/api/docker/", auth(func(w http.ResponseWriter, r *http.Request) {
		m := dockerRe.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		repo, rest := m[1], m[2]
		f.mu.Lock()
		defer f.mu.Unlock()
		images := f.tags[repo]
		switch {
		case rest == "_catalog":
			var names []string
			for n := range images {
				names = append(names, n)
			}
			sort.Strings(names)
			last := r.URL.Query().Get("last")
			if last == "" && len(names) > 1 { // two pages exercise the Link header
				w.Header().Set("Link", `</artifactory/api/docker/`+repo+`/v2/_catalog?n=1000&last=`+names[0]+`>; rel="next"`)
				names = names[:1]
			} else if last != "" {
				names = names[1:]
			}
			js(w, map[string]any{"repositories": names})
		case strings.HasSuffix(rest, "/tags/list"):
			image := strings.TrimSuffix(rest, "/tags/list")
			var tags []string
			for tg := range images[image] {
				tags = append(tags, tg)
			}
			sort.Strings(tags)
			js(w, map[string]any{"name": image, "tags": tags})
		case strings.Contains(rest, "/manifests/"):
			image, ref, _ := strings.Cut(rest, "/manifests/")
			d := ref
			if tg, ok := images[image][ref]; ok {
				d = tg
			}
			d = strings.TrimPrefix(d, "sha256:")
			body, ok := f.manifests[d]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if c, bad := f.corrupt["manifest/"+d]; bad {
				body = c
			}
			var mm struct{ MediaType string }
			_ = json.Unmarshal([]byte(body), &mm)
			w.Header().Set("Content-Type", mm.MediaType)
			_, _ = w.Write([]byte(body))
		case strings.Contains(rest, "/blobs/"):
			_, d, _ := strings.Cut(rest, "/blobs/")
			d = strings.TrimPrefix(d, "sha256:")
			body, ok := f.blobs[d]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if c, bad := f.corrupt["blob/"+d]; bad {
				body = c
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	mux.HandleFunc("/artifactory/", auth(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/artifactory/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.corrupt[key]; ok {
			_, _ = w.Write([]byte(c))
			return
		}
		c, ok := f.files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(c))
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeArtifactory) putManifest(m map[string]any) string {
	b, _ := json.Marshal(m)
	d := sum(string(b))
	f.manifests[d] = string(b)
	return d
}

func (f *fakeArtifactory) putBlob(content string) (string, int) {
	d := sum(content)
	f.blobs[d] = content
	return "sha256:" + d, len(content)
}

// addImage adds an image manifest per distinct version label; tags map to labels.
func (f *fakeArtifactory) addImage(repo, image string, tags map[string]string) {
	if f.tags[repo] == nil {
		f.tags[repo] = map[string]map[string]string{}
	}
	f.tags[repo][image] = map[string]string{}
	byLabel := map[string]string{}
	for tag, label := range tags {
		if _, ok := byLabel[label]; !ok {
			cd, cs := f.putBlob("config-" + image + label)
			ld, ls := f.putBlob("layer-" + image + label)
			byLabel[label] = f.putManifest(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
				"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": cs},
				"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": ld, "size": ls}}})
		}
		f.tags[repo][image][tag] = byLabel[label]
	}
}

func (f *fakeArtifactory) addIndex(repo, image, tag string) {
	if f.tags[repo] == nil {
		f.tags[repo] = map[string]map[string]string{}
	}
	var children []map[string]any
	for _, arch := range []string{"amd64", "arm64"} {
		cd, cs := f.putBlob("cfg-" + arch)
		ld, ls := f.putBlob("layer-" + arch)
		md := f.putManifest(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": cs},
			"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": ld, "size": ls}}})
		children = append(children, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + md, "size": len(f.manifests[md])})
	}
	idx := f.putManifest(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": children})
	f.tags[repo][image] = map[string]string{tag: idx}
}

func (f *fakeArtifactory) client(t *testing.T) *Artifactory {
	t.Helper()
	a, err := NewArtifactory(ArtifactoryConfig{BaseURL: f.srv.URL, Token: artToken})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func unmappedText(u []Unmapped) string {
	var b strings.Builder
	for _, x := range u {
		b.WriteString(x.Kind + ":" + x.Name + ":" + x.Reason + "\n")
	}
	return b.String()
}

func TestArtifactoryDiscoveryAndMapping(t *testing.T) {
	ctx := context.Background()
	f := newFakeArtifactory(t)
	plan, err := BuildPlan(ctx, f.client(t), MapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]SourceDef{}
	for _, s := range plan.Document.Sources {
		src[s.Name] = s
	}
	want := map[string][2]string{
		"generic-local": {"local", "raw"}, "maven-local": {"local", "maven"}, "gradle-local": {"local", "maven"},
		"npm-local": {"local", "npm"}, "docker-local": {"local", "oci"}, "maven-central": {"remote", "maven"}, "fed-local": {"local", "raw"},
	}
	for name, kf := range want {
		if s, ok := src[name]; !ok || s.Kind != kf[0] || s.Format != kf[1] {
			t.Fatalf("source %s: %+v", name, s)
		}
	}
	for _, name := range []string{"bower-local", "locked-remote", "maven-all", "maven-outer"} {
		if _, ok := src[name]; ok {
			t.Fatalf("%s must not be a source", name)
		}
	}
	var origin remoteOrigin
	_ = json.Unmarshal(*src["maven-central"].RemoteConfig, &origin)
	if origin.Origin != "https://repo1.maven.org/maven2" || origin.Cache.MetadataTTLSeconds != 7200 || src["maven-central"].Credential == nil {
		t.Fatalf("remote mapping: %+v %+v", origin, src["maven-central"].Credential)
	}
	feeds := map[string]FeedDef{}
	for _, fd := range plan.Document.Feeds {
		feeds[fd.Slug] = fd
	}
	all := feeds["maven-all"]
	// maven-outer flattens to gradle-local; its position is kept.
	order := []string{}
	for _, s := range all.Sources {
		order = append(order, s.Source)
	}
	if strings.Join(order, ",") != "maven-central,maven-local,gradle-local" || all.Format != "maven" ||
		all.PublishPolicy == nil || all.PublishPolicy.DefaultSource != "maven-local" {
		t.Fatalf("virtual mapping: %+v", all)
	}
	for i, s := range all.Sources {
		if s.Priority != i+1 {
			t.Fatalf("priority: %+v", all.Sources)
		}
	}
	if feeds["maven-local"].Sources[0].Source != "maven-local" || feeds["docker-local"].Format != "oci" {
		t.Fatalf("single-source feeds: %+v", feeds)
	}
	notes := unmappedText(plan.Unmapped)
	for _, w := range []string{
		"repository:bower-local:format bower has no Ant equivalent",
		"repository:locked-remote:configuration could not be read (HTTP 403",
		"repository:fed-local:federated repository mapped as a local source",
		"repository:maven-local:maxUniqueSnapshots=5",
		"repository:maven-central:outbound proxy corp-proxy",
		"nested group maven-outer was flattened",
		"user:2 users:",
		"group:1 groups:",
		"permission:release-managers:",
	} {
		if !strings.Contains(notes, w) {
			t.Fatalf("missing %q in:\n%s", w, notes)
		}
	}
	inv := map[string]RepoInventory{}
	for _, r := range plan.Repositories {
		inv[r.Name] = r
	}
	if inv["maven-local"].Assets != 3 || inv["maven-local"].Bytes != 1536 || inv["generic-local"].Bytes != 14 || inv["maven-central"].Bytes != 4096 || inv["maven-central"].Assets != 9 {
		t.Fatalf("storage summary not applied: %+v", inv)
	}
	if inv["bower-local"].MappedTo != "unmapped" || inv["locked-remote"].MappedTo != "unmapped" {
		t.Fatalf("inventory: %+v", inv)
	}
	if f.aqlCalls != 0 {
		t.Fatal("discovery enumerated assets")
	}
}

func TestArtifactorySelectionPullsInVirtualMembers(t *testing.T) {
	f := newFakeArtifactory(t)
	plan, err := BuildPlan(context.Background(), f.client(t), MapOptions{Repositories: []string{"maven-all"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Document.Feeds) != 1 || plan.Document.Feeds[0].Slug != "maven-all" || len(plan.Document.Sources) != 3 {
		t.Fatalf("selection: %d feeds %d sources", len(plan.Document.Feeds), len(plan.Document.Sources))
	}
	if _, err = BuildPlan(context.Background(), f.client(t), MapOptions{Repositories: []string{"nope"}}); err == nil {
		t.Fatal("unknown repository accepted")
	}
}

func TestArtifactoryAQLPagingAndDownload(t *testing.T) {
	old := aqlPage
	aqlPage = 2
	defer func() { aqlPage = old }()
	ctx := context.Background()
	f := newFakeArtifactory(t)
	a := f.client(t)
	var got []SourceAsset
	if err := a.Assets(ctx, "generic-local", func(s SourceAsset) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	// The fake lists 3 files; the internal .artifactory path is skipped.
	paths := []string{}
	for _, g := range got {
		paths = append(paths, g.Path)
	}
	if strings.Join(paths, ",") != "docs/readme.txt,top.bin" || f.aqlCalls < 2 {
		t.Fatalf("assets %v after %d AQL calls", paths, f.aqlCalls)
	}
	if got[0].SHA256 != sum("hello") || got[0].Size != 5 || got[1].Path != "top.bin" {
		t.Fatalf("asset fields: %+v", got)
	}
	rc, err := a.Open(ctx, got[0])
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("download: %q", b)
	}
	if _, err = a.Open(ctx, SourceAsset{Repository: "generic-local", Path: "missing"}); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestArtifactoryDockerEnumeration(t *testing.T) {
	ctx := context.Background()
	f := newFakeArtifactory(t)
	a := f.client(t)
	var got []SourceAsset
	if err := a.Assets(ctx, "docker-local", func(s SourceAsset) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	var tagged []string
	for _, s := range got {
		if s.Note != "" {
			t.Fatalf("unexpected note: %+v", s)
		}
		count[s.Image+"/"+s.Kind]++
		if len(s.Tags) > 0 {
			sort.Strings(s.Tags)
			tagged = append(tagged, s.Image+"="+strings.Join(s.Tags, "+"))
		}
	}
	sort.Strings(tagged)
	// team/app: 2 manifests (v1 shared by two tags, v2), 2 configs + 2 layers.
	// team/multi: index + 2 child manifests, 2 configs + 2 layers. Link pagination reached both images.
	if count["team/app/manifest"] != 2 || count["team/app/blob"] != 4 || count["team/multi/manifest"] != 3 || count["team/multi/blob"] != 4 {
		t.Fatalf("counts: %v", count)
	}
	if strings.Join(tagged, ";") != "team/app=1.0+latest;team/app=2.0;team/multi=stable" {
		t.Fatalf("tags: %v", tagged)
	}
	for _, s := range got {
		if s.Kind == "manifest" {
			rc, err := a.Open(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if sum(string(b)) != s.SHA256 {
				t.Fatal("manifest bytes differ from digest")
			}
		}
	}
	// Schema 1 / unsupported manifests are reported, never dropped silently.
	f.mu.Lock()
	f.addImage("docker-local", "old/one", map[string]string{"x": "a"})
	d := f.tags["docker-local"]["old/one"]["x"]
	f.manifests[d] = strings.Replace(f.manifests[d], "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v1+json", 1)
	f.mu.Unlock()
	var notes []string
	_ = a.Assets(ctx, "docker-local", func(s SourceAsset) error {
		if s.Note != "" {
			notes = append(notes, s.Path+": "+s.Note)
		}
		return nil
	})
	if len(notes) != 1 || !strings.Contains(notes[0], "old/one:x") {
		t.Fatalf("notes: %v", notes)
	}
}

func TestArtifactoryAuthModesAndOriginChecks(t *testing.T) {
	ctx := context.Background()
	f := newFakeArtifactory(t)
	basic, err := NewArtifactory(ArtifactoryConfig{BaseURL: f.srv.URL, Username: "admin", Password: "api-key-value"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = basic.Repositories(ctx); err != nil {
		t.Fatalf("basic auth: %v", err)
	}
	bad, _ := NewArtifactory(ArtifactoryConfig{BaseURL: f.srv.URL, Username: "admin", Password: "wrong-secret"})
	if _, err = bad.Repositories(ctx); err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad credentials: %v", err)
	}
	for _, u := range []string{"", "ftp://x", "http://user:pw@x", "http://x?q=1"} {
		if _, err := NewArtifactory(ArtifactoryConfig{BaseURL: u}); err == nil {
			t.Fatalf("accepted %q", u)
		}
	}
	a, _ := NewArtifactory(ArtifactoryConfig{BaseURL: "http://127.0.0.1:1", Token: "t"})
	if a.base.Path != "/artifactory" {
		t.Fatalf("context path: %q", a.base.Path)
	}
	if _, err := a.do(ctx, "GET", "http://other.example/x", nil, nil); err == nil {
		t.Fatal("credentials would be sent to a foreign origin")
	}
	// A redirect to another host is not followed.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect was followed; credentials leaked: " + r.Header.Get("Authorization"))
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redirect.Close()
	r, _ := NewArtifactory(ArtifactoryConfig{BaseURL: redirect.URL, Token: artToken})
	if _, err = r.Repositories(ctx); err == nil {
		t.Fatal("redirect treated as success")
	}
}

func TestArtifactoryCABundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer srv.Close()
	if _, err := mustArt(t, ArtifactoryConfig{BaseURL: srv.URL, Token: "t"}).Repositories(context.Background()); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := LoadCAPool(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = mustArt(t, ArtifactoryConfig{BaseURL: srv.URL, Token: "t", RootCAs: pool}).Repositories(context.Background()); err != nil {
		t.Fatalf("trusted bundle rejected: %v", err)
	}
	if err = os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadCAPool(path); err == nil {
		t.Fatal("garbage bundle accepted")
	}
	_ = x509.NewCertPool
	_ = tls.VersionTLS12
}

func mustArt(t *testing.T, cfg ArtifactoryConfig) *Artifactory {
	t.Helper()
	a, err := NewArtifactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestArtifactoryMigrationCopyResumeAndIntegrity(t *testing.T) {
	ctx := context.Background()
	f := newFakeArtifactory(t)
	dst := newEnv(t)
	a := f.client(t)
	opts := MapOptions{CopyContent: true, Repositories: []string{"generic-local", "maven-local", "docker-local", "npm-local"}}
	dry, err := Migrate(ctx, dst.pool, dst.stores, dst.instance, a, opts, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dst.scalar(t, `SELECT count(*) FROM sources`) != 0 {
		t.Fatal("dry run wrote")
	}
	// generic: 2, maven: jar+pom, docker: 2+3 manifests and 4+4 blobs (shared digests count once per image).
	if dry.Count("asset", ActionCreated) < 4+5+8 {
		t.Fatalf("dry run: %+v", dry.Summary)
	}
	text := unmappedText(dry.Unmapped)
	for _, w := range []string{"snapshot versions are not supported", "content copy is not supported for format npm"} {
		if !strings.Contains(text, w) {
			t.Fatalf("missing %q in\n%s", w, text)
		}
	}
	// A tampered download is an integrity failure; nothing from it is stored.
	f.corrupt["generic-local/docs/readme.txt"] = "tampered"
	badLayer := sum("layer-team/appv2")
	f.corrupt["blob/"+badLayer] = "tampered-layer"
	applied, err := Migrate(ctx, dst.pool, dst.stores, dst.instance, a, opts, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.IntegrityFailures) != 2 || applied.Count("asset", ActionSkipped) != 2 {
		t.Fatalf("apply: %+v %+v", applied.Summary, applied.IntegrityFailures)
	}
	if dst.scalar(t, `SELECT count(*) FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE b.digest IN ($1,$2)`, sum("tampered"), sum("tampered-layer")) != 0 {
		t.Fatal("tampered content was imported")
	}
	before := dst.scalar(t, `SELECT count(*) FROM assets`)
	// Resume once the source is healthy: only the missing assets are created.
	delete(f.corrupt, "generic-local/docs/readme.txt")
	delete(f.corrupt, "blob/"+badLayer)
	retry, err := Migrate(ctx, dst.pool, dst.stores, dst.instance, a, opts, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Count("asset", ActionCreated) != 2 || len(retry.IntegrityFailures) != 0 || dst.scalar(t, `SELECT count(*) FROM assets`) != before+2 {
		t.Fatalf("retry: %+v", retry.Summary)
	}
	again, _ := Migrate(ctx, dst.pool, dst.stores, dst.instance, a, opts, ImportOptions{})
	if len(again.Items) != 0 {
		t.Fatalf("not idempotent: %+v", again.Items)
	}
	// Registry layout: image components with the tags, manifests and blobs.
	if dst.scalar(t, `SELECT count(*) FROM tags t JOIN components c ON c.id=t.component_id WHERE c.coordinate='team/app' AND c.namespace='team' AND t.name IN ('latest','1.0','2.0')`) != 3 {
		t.Fatal("tags not created")
	}
	if dst.scalar(t, `SELECT count(*) FROM tags t JOIN versions v ON v.id=t.version_id WHERE t.name='latest' AND v.version=(SELECT version FROM tags t2 JOIN versions v2 ON v2.id=t2.version_id WHERE t2.name='1.0' AND t2.component_id=t.component_id)`) != 1 {
		t.Fatal("latest and 1.0 should share one manifest")
	}
	if dst.scalar(t, `SELECT count(*) FROM assets WHERE asset_key='manifest'`) != 5 {
		t.Fatal("manifests missing")
	}
	// Feed layout for the registry and generic content.
	if dst.scalar(t, `SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN sources s ON s.id=c.source_id WHERE s.name='generic-local' AND a.asset_key='docs/readme.txt' AND c.namespace='docs'`) != 1 {
		t.Fatal("generic asset not stored with its path")
	}

	// Staged adoption compare through the same manager.
	rep, err := Compare(ctx, dst.pool, dst.instance, a, []CompareItem{
		{Repository: "generic-local", Path: "docs/readme.txt"},
		{Repository: "generic-local", Path: "top.bin", Feed: "no-such-feed"},
		{Repository: "generic-local", Path: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range rep.Results {
		got = append(got, r.Status)
	}
	if strings.Join(got, ",") != "identical,missing_in_ant,missing_in_both" || rep.Manager != "artifactory" {
		t.Fatalf("compare: %v %s", got, rep.Manager)
	}
	_ = fmt.Sprint
}
