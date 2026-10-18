package nuget

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
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

func TestNuGetPullThrough(t *testing.T) {
	f := remotetest.New(t)
	nupkg := []byte("NUPKG-BYTES")
	nuspec := "<package/>"
	var origin string
	var hits, nupkgHits atomic.Int32
	var down atomic.Bool
	item := func(id, v string) map[string]any {
		return map[string]any{"@id": origin + "/v3/registration5/" + id + "/" + v + ".json", "packageContent": origin + "/v3-flatcontainer/" + id + "/" + v + "/" + id + "." + v + ".nupkg", "registration": origin + "/v3/registration5/" + id + "/index.json",
			"catalogEntry": map[string]any{"@id": origin + "/v3/catalog0/data/x.json", "id": id, "version": v, "authors": "A", "description": "D", "projectUrl": "https://project.example/", "listed": true}}
	}
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/v3/index.json":
			_ = enc.Encode(map[string]any{"version": "3.0.0", "resources": []map[string]any{
				{"@id": origin + "/v3-flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
				{"@id": origin + "/v3/registration5/", "@type": []string{"RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/Versioned"}},
			}})
		case "/v3-flatcontainer/demo.pkg/index.json":
			_ = enc.Encode(map[string]any{"versions": []string{"1.0.0", "1.1.0", "bad version"}})
		case "/v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg":
			nupkgHits.Add(1)
			_, _ = w.Write(nupkg)
		case "/v3-flatcontainer/demo.pkg/1.0.0/demo.pkg.nuspec":
			_, _ = w.Write([]byte(nuspec))
		case "/v3/registration5/demo.pkg/index.json":
			// first page inline, second page referenced and fetched on demand
			_ = enc.Encode(map[string]any{"@id": origin + "/v3/registration5/demo.pkg/index.json", "count": 2, "items": []any{
				map[string]any{"@id": origin + "/v3/registration5/demo.pkg/index.json#page/1.0.0/1.0.0", "count": 1, "lower": "1.0.0", "upper": "1.0.0", "items": []any{item("demo.pkg", "1.0.0")}},
				map[string]any{"@id": origin + "/v3/registration5/demo.pkg/page/1.1.0/1.1.0.json", "count": 1, "lower": "1.1.0", "upper": "1.1.0"},
			}})
		case "/v3/registration5/demo.pkg/page/1.1.0/1.1.0.json":
			_ = enc.Encode(map[string]any{"items": []any{item("demo.pkg", "1.1.0"), item("other", "1.0.0")}})
		default:
			http.NotFound(w, r)
		}
	}))
	origin = up.URL
	feed := f.AddFeed("packages", "nuget")
	local := f.AddLocal(feed, "nuget", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "nuget", "nuget-org", 1, up.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/packages/nuget/"

	// local owner
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	part, _ := m.CreateFormFile("package", "x.nupkg")
	_, _ = part.Write(packageBytes(t, "owned.pkg", "1.0.0", ""))
	_ = m.Close()
	req, _ := http.NewRequest("PUT", base+"v2/package", &body)
	req.Header.Set("X-NuGet-ApiKey", admin.Secret)
	req.Header.Set("Content-Type", m.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("publish: %v %v", err, res)
	}
	res.Body.Close()

	status, text, _ := f.Get(base + "v3/flatcontainer/demo.pkg/index.json")
	if status != 200 || !strings.Contains(text, `"1.1.0"`) || strings.Contains(text, "bad version") {
		t.Fatalf("versions: %d %s", status, text)
	}
	status, text, _ = f.Get(base + "v3/registration/demo.pkg/index.json")
	if status != 200 || strings.Contains(text, up.URL) || !strings.Contains(text, `"projectUrl":"https://project.example/"`) || strings.Contains(text, `"other"`) {
		t.Fatalf("registration leaks upstream or lost fields: %d %s", status, text)
	}
	var reg struct {
		Items []struct {
			Count int
			Items []struct {
				ID             string `json:"@id"`
				PackageContent string
			}
		}
	}
	if err = json.Unmarshal([]byte(text), &reg); err != nil || len(reg.Items) != 1 || reg.Items[0].Count != 2 {
		t.Fatalf("registration pages not inlined: %s", text)
	}
	if reg.Items[0].Items[1].PackageContent != base+"v3/flatcontainer/demo.pkg/1.1.0/demo.pkg.1.1.0.nupkg" || reg.Items[0].Items[1].ID != base+"v3/registration/demo.pkg/1.1.0.json" {
		t.Fatalf("urls not rewritten to the feed: %+v", reg.Items[0].Items[1])
	}
	status, text, _ = f.Get(base + "v3/registration/demo.pkg/1.0.0.json")
	if status != 200 || strings.Contains(text, up.URL) || !strings.Contains(text, base+"v3/flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg") {
		t.Fatalf("leaf: %d %s", status, text)
	}
	if status, _, _ = f.Get(base + "v3/registration/demo.pkg/9.0.0.json"); status != 404 {
		t.Fatalf("missing leaf: %d", status)
	}

	// miss -> cache -> hit
	dl := base + "v3/flatcontainer/demo.pkg/1.0.0/demo.pkg.1.0.0.nupkg"
	if status, text, _ = f.Get(dl); status != 200 || text != string(nupkg) {
		t.Fatalf("nupkg miss: %d %q", status, text)
	}
	if status, text, _ = f.Get(base + "v3/flatcontainer/demo.pkg/1.0.0/demo.pkg.nuspec"); status != 200 || text != nuspec {
		t.Fatalf("nuspec: %d %q", status, text)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d", f.CachedAssets(remote))
	}
	if status, text, _ = f.Get(dl); status != 200 || text != string(nupkg) || nupkgHits.Load() != 1 {
		t.Fatalf("nupkg hit went upstream: %d hits=%d", status, nupkgHits.Load())
	}

	// local owner precedence: no upstream request for an owned id
	before := hits.Load()
	if status, _, _ = f.Get(base + "v3/flatcontainer/owned.pkg/2.0.0/owned.pkg.2.0.0.nupkg"); status != 404 || hits.Load() != before {
		t.Fatalf("owned id reached upstream: %d", status)
	}
	if status, _, _ = f.Get(base + "v3/registration/owned.pkg/index.json"); status != 200 || hits.Load() != before {
		t.Fatalf("owned registration: %d", status)
	}

	// outage: cached nupkg and the stale registration still served, uncached 503
	down.Store(true)
	if status, _, _ = f.Get(dl); status != 200 {
		t.Fatalf("cached nupkg in outage: %d", status)
	}
	if status, text, _ = f.Get(base + "v3/registration/demo.pkg/index.json"); status != 200 || !strings.Contains(text, "1.1.0") {
		t.Fatalf("stale registration: %d", status)
	}
	if status, _, _ = f.Get(base + "v3/flatcontainer/uncached.pkg/index.json"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
