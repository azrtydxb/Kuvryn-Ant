package helmrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"go.yaml.in/yaml/v3"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestHelmPullThrough(t *testing.T) {
	f := remotetest.New(t)
	demo := goodChart(t, "demo", "1.2.3")
	owned := goodChart(t, "owned", "2.0.0")
	abs := goodChart(t, "abs", "0.1.0")
	var hits, outage atomic.Int32
	var cdnHits atomic.Int32
	cdn := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to redirect origin")
		}
		_, _ = w.Write(abs)
	}))
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if outage.Load() == 1 {
			http.Error(w, "down", 503)
			return
		}
		switch r.URL.Path {
		case "/index.yaml":
			fmt.Fprintf(w, `apiVersion: v1
generated: "2024-01-01T00:00:00Z"
entries:
  demo:
  - name: demo
    version: 1.2.3
    description: demo chart
    digest: %s
    urls: [charts/demo-1.2.3.tgz]
  owned:
  - name: owned
    version: 2.0.0
    digest: %s
    urls: [charts/owned-2.0.0.tgz]
  abs:
  - name: abs
    version: 0.1.0
    digest: %s
    urls: ["%s/dl/abs-0.1.0.tgz"]
  bad:
  - name: bad
    version: notsemver
    urls: [charts/bad.tgz]
`, sum(demo), sum(owned), sum(abs), cdn.URL)
		case "/charts/demo-1.2.3.tgz":
			_, _ = w.Write(demo)
		case "/charts/demo-1.2.3.tgz.prov":
			_, _ = w.Write([]byte("PROV"))
		case "/charts/owned-2.0.0.tgz":
			t.Errorf("local-owned chart requested upstream")
			_, _ = w.Write(owned)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("charts", "helm")
	local := f.AddLocal(feed, "helm", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "helm", "upstream", 1, up.URL, remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{cdn.URL}})
	h := New(f.Pool, f.Service)
	srv := httptest.NewServer(h)
	defer srv.Close()
	base := srv.URL + "/feeds/charts/helm/"

	// Publish a local chart named like an upstream one: local owner wins.
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	part, _ := m.CreateFormFile("chart", "owned-1.0.0.tgz")
	_, _ = part.Write(goodChart(t, "owned", "1.0.0"))
	_ = m.Close()
	req, _ := http.NewRequest("POST", base+"charts/owned/1.0.0", &body)
	admin, err := (identity.Tokens{DB: f.Pool}).Issue(context.Background(), f.Boot.InstanceID, f.Boot.PrincipalID, f.Boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+admin.Secret)
	req.Header.Set("Content-Type", m.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode >= 300 {
		t.Fatalf("local publish: %v %v", err, res)
	}
	res.Body.Close()
	before := hits.Load()

	status, text, _ := f.Get(base + "index.yaml")
	if status != 200 {
		t.Fatalf("index %d %s", status, text)
	}
	var idx indexFile
	if err = yaml.Unmarshal([]byte(text), &idx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, up.URL) || strings.Contains(text, cdn.URL) || strings.Contains(text, "notsemver") {
		t.Fatalf("upstream leaked or invalid entry kept: %s", text)
	}
	if len(idx.Entries["owned"]) != 1 || idx.Entries["owned"][0]["version"] != "1.0.0" {
		t.Fatalf("local owner must not be mixed with upstream versions: %s", text)
	}
	wantDemo := "charts/demo/1.2.3/" + remote + "/" + sum(demo) + "/demo-1.2.3.tgz"
	if len(idx.Entries["demo"]) != 1 || idx.Entries["demo"][0]["urls"].([]any)[0] != wantDemo || idx.Entries["demo"][0]["description"] != "demo chart" {
		t.Fatalf("demo entry not rewritten: %s", text)
	}
	if hits.Load() == before {
		t.Fatalf("index never fetched upstream")
	}

	// miss -> cache -> hit
	if status, text, _ = f.Get(base + wantDemo); status != 200 || text != string(demo) {
		t.Fatalf("demo miss: %d", status)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatalf("cached %d", f.CachedAssets(remote))
	}
	n := hits.Load()
	if status, text, _ = f.Get(base + wantDemo); status != 200 || text != string(demo) || hits.Load() != n {
		t.Fatalf("demo hit went upstream: %d", status)
	}
	// provenance is fetched on demand through the same pinned URL
	if status, text, _ = f.Get(base + wantDemo + ".prov"); status != 200 || text != "PROV" {
		t.Fatalf("prov: %d %q", status, text)
	}
	// absolute chart URL on a redirect origin, no credential
	absURL := "charts/abs/0.1.0/" + remote + "/" + sum(abs) + "/abs-0.1.0.tgz"
	if status, text, _ = f.Get(base + absURL); status != 200 || text != string(abs) || cdnHits.Load() != 1 {
		t.Fatalf("abs: %d", status)
	}
	// local owner: upstream version of an owned chart is unreachable, without an upstream request
	n = hits.Load()
	if status, _, _ = f.Get(base + "charts/owned/2.0.0/" + remote + "/" + sum(owned) + "/owned-2.0.0.tgz"); status != 404 || hits.Load() != n {
		t.Fatalf("owned: %d hits+%d", status, hits.Load()-n)
	}
	// digest not advertised upstream
	if status, _, _ = f.Get(base + "charts/demo/1.2.3/" + remote + "/" + sum([]byte("x")) + "/demo-1.2.3.tgz"); status == 200 {
		t.Fatalf("unadvertised digest served")
	}
	// outage: cached charts and the stale index keep working
	outage.Store(1)
	if status, _, _ = f.Get(base + wantDemo); status != 200 {
		t.Fatalf("cached chart in outage: %d", status)
	}
	if status, text, _ = f.Get(base + "index.yaml"); status != 200 || !strings.Contains(text, "demo") {
		t.Fatalf("index in outage: %d %s", status, text)
	}
}
