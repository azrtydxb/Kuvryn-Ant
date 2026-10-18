package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

type fakeNexus struct {
	t       *testing.T
	mu      sync.Mutex
	files   map[string]string // repo/path -> content
	corrupt map[string]string // repo/path -> bytes actually served
	srv     *httptest.Server
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func newFakeNexus(t *testing.T) *fakeNexus {
	f := &fakeNexus{t: t, files: map[string]string{
		"maven-hosted/com/acme/lib/1.0/lib-1.0.jar":          "jar-bytes",
		"maven-hosted/com/acme/lib/1.0/lib-1.0.pom":          "<project/>",
		"maven-hosted/com/acme/lib/1.0/lib-1.0.jar.sha1":     "da39a3ee",
		"maven-hosted/com/acme/lib/1.0/lib-1.0.jar.asc":      "signature",
		"maven-hosted/com/acme/lib/maven-metadata.xml":       "<metadata/>",
		"maven-hosted/com/acme/lib/2.0-SNAPSHOT/lib-2.0.jar": "snap",
		"raw-hosted/docs/readme.txt":                         "hello",
	}, corrupt: map[string]string{}}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "s3cret" {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	js := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/service/rest/v1/repositories", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]any{
			{"name": "maven-hosted", "format": "maven2", "type": "hosted"},
			{"name": "maven-proxy", "format": "maven2", "type": "proxy", "attributes": map[string]any{"proxy": map[string]any{"remoteUrl": "https://repo1.example.org/maven2/"}}},
			{"name": "maven-public", "format": "maven2", "type": "group"},
			{"name": "raw-hosted", "format": "raw", "type": "hosted"},
			{"name": "Bower.Stuff", "format": "bower", "type": "hosted"},
		})
	}))
	mux.HandleFunc("/service/rest/v1/repositories/maven/hosted/maven-hosted", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"cleanup": map[string]any{"policyNames": []string{"old", "other"}}})
	}))
	mux.HandleFunc("/service/rest/v1/repositories/maven/proxy/maven-proxy", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"proxy": map[string]any{"remoteUrl": "https://repo1.example.org/maven2/", "metadataMaxAge": 1440}, "httpClient": map[string]any{"authentication": map[string]any{"type": "username"}}})
	}))
	mux.HandleFunc("/service/rest/v1/repositories/maven/group/maven-public", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"group": map[string]any{"memberNames": []string{"maven-proxy", "maven-hosted"}, "writableMember": "maven-hosted"}})
	}))
	mux.HandleFunc("/service/rest/v1/cleanup-policies", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]any{
			{"name": "old", "criteriaLastBlobUpdated": 30, "criteriaLastDownloaded": 7, "criteriaAssetRegex": ".*", "retain": 3, "criteriaReleaseType": "RELEASES"},
			{"name": "other", "criteriaLastBlobUpdated": 5},
		})
	}))
	mux.HandleFunc("/service/rest/v1/assets", auth(func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repository")
		var items []map[string]any
		for k, v := range f.files {
			if rp, path, _ := strings.Cut(k, "/"); rp == repo {
				items = append(items, map[string]any{"path": path, "downloadUrl": "http://nexus.internal.invalid/repository/" + k, "contentType": "application/octet-stream", "fileSize": len(v), "checksum": map[string]any{"sha256": sum(v)}})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i]["path"].(string) < items[j]["path"].(string) })
		// Two pages exercise the continuation token.
		if r.URL.Query().Get("continuationToken") == "" && len(items) > 1 {
			js(w, map[string]any{"items": items[:1], "continuationToken": "p2"})
			return
		}
		if r.URL.Query().Get("continuationToken") == "p2" {
			items = items[1:]
		}
		js(w, map[string]any{"items": items, "continuationToken": nil})
	}))
	mux.HandleFunc("/repository/", auth(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/repository/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if c, ok := f.corrupt[key]; ok {
			w.Write([]byte(c))
			return
		}
		c, ok := f.files[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(c))
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNexus) client(t *testing.T, pass string) *Nexus {
	n, err := NewNexus(NexusConfig{BaseURL: f.srv.URL, Username: "admin", Password: pass})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNexusMapping(t *testing.T) {
	ctx := context.Background()
	f := newFakeNexus(t)
	plan, err := BuildPlan(ctx, f.client(t, "s3cret"), MapOptions{CopyContent: true})
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]SourceDef{}
	for _, s := range plan.Document.Sources {
		src[s.Name] = s
	}
	if src["maven-hosted"].Kind != "local" || src["maven-hosted"].Format != "maven" || src["maven-proxy"].Kind != "remote" || src["raw-hosted"].Format != "raw" {
		t.Fatalf("source mapping: %+v", src)
	}
	if _, ok := src["Bower.Stuff"]; ok {
		t.Fatal("unsupported format mapped")
	}
	var origin remoteOrigin
	_ = json.Unmarshal(*src["maven-proxy"].RemoteConfig, &origin)
	if origin.Origin != "https://repo1.example.org/maven2/" || origin.Cache.MetadataTTLSeconds != 86400 || src["maven-proxy"].Credential == nil {
		t.Fatalf("proxy mapping: %+v %+v", origin, src["maven-proxy"].Credential)
	}
	feeds := map[string]FeedDef{}
	for _, fd := range plan.Document.Feeds {
		feeds[fd.Slug] = fd
	}
	pub := feeds["maven-public"]
	if len(pub.Sources) != 2 || pub.Sources[0].Source != "maven-proxy" || pub.Sources[0].Priority != 1 || pub.Sources[1].Source != "maven-hosted" || pub.PublishPolicy == nil || pub.PublishPolicy.DefaultSource != "maven-hosted" {
		t.Fatalf("group mapping: %+v", pub)
	}
	r := src["maven-hosted"].Retention
	if r == nil || r.KeepLast == nil || *r.KeepLast != 3 || *r.MaxAgeSeconds != 30*86400 || *r.MaxIdleSeconds != 7*86400 || !r.AllowReleaseDelete {
		t.Fatalf("retention mapping: %+v", r)
	}
	notes := ""
	for _, u := range plan.Unmapped {
		notes += u.Kind + ":" + u.Name + ":" + u.Reason + "\n"
	}
	for _, want := range []string{"regex criterion", "ignored; only one policy", "format bower has no Ant equivalent", "snapshot", "not supported by Ant's Maven"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("missing unmapped note %q in:\n%s", want, notes)
		}
	}
	// Metadata is regenerated by Ant, so it is not copied.
	n := 0
	for _, c := range plan.Document.Components {
		for _, v := range c.Versions {
			for _, a := range v.Assets {
				n++
				if strings.Contains(a.Key, "maven-metadata") {
					t.Fatal("maven-metadata copied")
				}
			}
		}
	}
	if n != 4 { // jar, pom, jar.sha1, readme
		t.Fatalf("expected 4 mapped assets, got %d", n)
	}
	if _, err = BuildPlan(ctx, f.client(t, "wrong"), MapOptions{}); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad credentials: %v", err)
	}
}

type remoteOrigin struct {
	Origin string `json:"origin"`
	Cache  struct {
		MetadataTTLSeconds int `json:"metadata_ttl_seconds"`
	} `json:"cache"`
}

func TestNexusMigrationDryRunApplyAndCompare(t *testing.T) {
	ctx := context.Background()
	f := newFakeNexus(t)
	dst := newEnv(t)
	nx := f.client(t, "s3cret")
	opts := MapOptions{CopyContent: true}
	dry, err := Migrate(ctx, dst.pool, dst.stores, dst.instance, nx, opts, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dst.scalar(t, `SELECT count(*) FROM sources`) != 0 {
		t.Fatal("dry run wrote")
	}
	if dry.Count("source", ActionCreated) != 3 || dry.Count("asset", ActionCreated) != 4 || dry.Count("feed", ActionCreated) != 4 {
		t.Fatalf("dry run: %+v", dry.Summary)
	}
	var cred bool
	for _, u := range dry.Unmapped {
		cred = cred || u.Kind == "credential" && u.Name == "maven-proxy"
	}
	if !cred {
		t.Fatal("proxy upstream credential not reported")
	}
	// A digest mismatch during download is an integrity failure, not an import.
	f.corrupt["raw-hosted/docs/readme.txt"] = "tampered"
	applied, err := Migrate(ctx, dst.pool, dst.stores, dst.instance, nx, opts, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.IntegrityFailures) != 1 || applied.Count("asset", ActionSkipped) != 1 || applied.Count("asset", ActionCreated) != 3 {
		t.Fatalf("apply: %+v %+v", applied.Summary, applied.IntegrityFailures)
	}
	if dst.scalar(t, `SELECT count(*) FROM assets`) != 3 {
		t.Fatal("tampered asset was imported")
	}
	// Retry once the source serves correct bytes; earlier work is unchanged.
	delete(f.corrupt, "raw-hosted/docs/readme.txt")
	retry, err := Migrate(ctx, dst.pool, dst.stores, dst.instance, nx, opts, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Count("asset", ActionCreated) != 1 || retry.Count("asset", ActionUnchanged) != 3 || len(retry.IntegrityFailures) != 0 {
		t.Fatalf("retry: %+v", retry.Summary)
	}
	again, _ := Migrate(ctx, dst.pool, dst.stores, dst.instance, nx, opts, ImportOptions{})
	if len(again.Items) != 0 {
		t.Fatalf("not idempotent: %+v", again.Items)
	}
	if dst.scalar(t, `SELECT count(*) FROM feed_sources fs JOIN feeds f ON f.id=fs.feed_id WHERE f.slug='maven-public'`) != 2 {
		t.Fatal("group feed sources missing")
	}
	// Content preserved: path, namespace, format metadata and checksum.
	if dst.scalar(t, `SELECT count(*) FROM assets a JOIN blobs b ON b.id=a.blob_id JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE a.asset_key='com/acme/lib/1.0/lib-1.0.jar' AND b.digest=$1 AND c.coordinate='com.acme:lib' AND c.namespace='com.acme' AND v.version='1.0'`, sum("jar-bytes")) != 1 {
		t.Fatal("maven asset not mapped with its checksum and coordinates")
	}

	// Staged adoption: compare what a client gets from each system.
	f.files["raw-hosted/docs/only-nexus.txt"] = "x"
	rep, err := Compare(ctx, dst.pool, dst.instance, nx, []CompareItem{
		{Repository: "raw-hosted", Path: "docs/readme.txt"},
		{Repository: "raw-hosted", Path: "docs/only-nexus.txt"},
		{Repository: "maven-hosted", Path: "com/acme/lib/1.0/lib-1.0.pom"},
		{Repository: "raw-hosted", Path: "docs/none.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range rep.Results {
		got = append(got, r.Status)
	}
	if strings.Join(got, ",") != "identical,missing_in_ant,identical,missing_in_both" {
		t.Fatalf("compare statuses: %v", got)
	}
	dst.exec(t, `UPDATE blobs SET digest=$1 WHERE digest=$2`, sum("changed"), sum("hello"))
	rep, _ = Compare(ctx, dst.pool, dst.instance, nx, []CompareItem{{Repository: "raw-hosted", Path: "docs/readme.txt"}})
	if rep.Results[0].Status != "differs" {
		t.Fatalf("expected differs, got %+v", rep.Results[0])
	}
}

func TestNexusRejectsBadBaseURLAndForeignOrigin(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "http://user:pw@x", "http://x?q=1"} {
		if _, err := NewNexus(NexusConfig{BaseURL: u}); err == nil {
			t.Fatalf("accepted %q", u)
		}
	}
	n, _ := NewNexus(NexusConfig{BaseURL: "http://127.0.0.1:1", Username: "a", Password: "b"})
	if _, err := n.do(context.Background(), "http://other.example/x"); err == nil {
		t.Fatal("credentials would be sent to a foreign origin")
	}
}
