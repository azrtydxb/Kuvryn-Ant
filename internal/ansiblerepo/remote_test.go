package ansiblerepo

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

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func sumOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestGalaxyPullThrough(t *testing.T) {
	f := remotetest.New(t)
	good, bad := []byte("COLLECTION-1"), []byte("TAMPERED")
	var apiHits, cdnHits atomic.Int32
	var down atomic.Bool
	cdn := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the download origin")
		}
		switch r.URL.Path {
		case "/community-general-1.0.0.tar.gz":
			_, _ = w.Write(good)
		case "/community-general-2.0.0.tar.gz":
			_, _ = w.Write(bad)
		default:
			http.NotFound(w, r)
		}
	}))
	var api *httptest.Server
	api = f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		detail := func(v, sha string) string {
			return fmt.Sprintf(`{"version":"%s","href":"%s/v3/collections/community/general/versions/%s/","namespace":{"name":"community"},"collection":{"name":"general","href":"%s/v3/collections/community/general/"},"download_url":"%s/community-general-%s.tar.gz","artifact":{"filename":"community-general-%s.tar.gz","sha256":"%s","size":10},"metadata":{"dependencies":{"ansible.utils":">=2.0.0"}}}`, v, api.URL, v, api.URL, cdn.URL, v, v, sha)
		}
		switch r.URL.Path {
		case "/v3/collections/community/general/":
			fmt.Fprintf(w, `{"href":"%s/v3/collections/community/general/","namespace":"community","name":"general","versions_url":"%s/v3/collections/community/general/versions/","highest_version":{"version":"2.0.0","href":"%s/x/"},"deprecated":false}`, api.URL, api.URL, api.URL)
		case "/v3/collections/community/general/versions/":
			if r.URL.Query().Get("offset") == "1" {
				fmt.Fprintf(w, `{"meta":{"count":3},"links":{"next":null},"data":[{"version":"2.0.0","href":"%s/old/"},{"version":"not a version","href":"x"}]}`, api.URL)
				return
			}
			fmt.Fprintf(w, `{"meta":{"count":3},"links":{"next":"%s/v3/collections/community/general/versions/?limit=1&offset=1"},"data":[{"version":"1.0.0","href":"%s/old/","created_at":"2024-01-01T00:00:00Z"}]}`, api.URL, api.URL)
		case "/v3/collections/community/general/versions/1.0.0/":
			fmt.Fprint(w, detail("1.0.0", sumOf(good)))
		case "/v3/collections/community/general/versions/2.0.0/":
			fmt.Fprint(w, detail("2.0.0", sumOf(good)))
		default:
			if strings.Contains(r.URL.Path, "/mine/") {
				t.Errorf("locally owned collection requested upstream: %s", r.URL.Path)
			}
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("galaxy", "ansible")
	local := f.AddLocal(feed, "ansible", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "ansible", "galaxy", 1, api.URL, remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{cdn.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/galaxy/ansible/"

	// local owner
	for key, data := range map[string][]byte{"archive": []byte("LOCAL"), "metadata": []byte(`{"namespace":"mine","name":"coll","version":"1.0.0","dependencies":{}}`)} {
		if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Namespace: "mine", Coordinate: "mine.coll", Version: "1.0.0", AssetKey: key, MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(sumOf(data)), Size: int64(len(data))}}, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}

	// metadata: hrefs and download URLs are rewritten, pagination is consumed
	status, body, _ := f.Get(base + "v3/collections/community/general/")
	if status != 200 || strings.Contains(body, api.URL) || !strings.Contains(body, base+"v3/collections/community/general/versions/") {
		t.Fatalf("detail: %d %s", status, body)
	}
	status, body, _ = f.Get(base + "v3/collections/community/general/versions/")
	if status != 200 || strings.Contains(body, api.URL) || strings.Contains(body, "not a version") || !strings.Contains(body, `"version":"1.0.0"`) || !strings.Contains(body, `"version":"2.0.0"`) || !strings.Contains(body, base+"downloads/community/general/2.0.0/collection.tar.gz") {
		t.Fatalf("versions: %d %s", status, body)
	}
	status, body, _ = f.Get(base + "v3/collections/community/general/versions/1.0.0/")
	if status != 200 || strings.Contains(body, cdn.URL) || strings.Contains(body, api.URL) || !strings.Contains(body, base+"downloads/community/general/1.0.0/collection.tar.gz") || !strings.Contains(body, sumOf(good)) {
		t.Fatalf("version detail: %d %s", status, body)
	}
	if status, _, _ = f.Get(base + "v3/collections/community/missing/"); status != 404 {
		t.Fatalf("unknown: %d", status)
	}

	// download: miss -> verified -> cached -> hit with no upstream traffic
	dl := base + "downloads/community/general/1.0.0/collection.tar.gz"
	if status, body, _ = f.Get(dl); status != 200 || body != string(good) {
		t.Fatalf("miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 2 {
		t.Fatalf("cached %d assets, want archive+metadata", f.CachedAssets(remote))
	}
	n := cdnHits.Load()
	if status, body, _ = f.Get(dl); status != 200 || body != string(good) || cdnHits.Load() != n {
		t.Fatalf("hit: %d", status)
	}
	// digest mismatch rejected
	if status, body, _ = f.Get(base + "downloads/community/general/2.0.0/collection.tar.gz"); status == 200 || strings.Contains(body, "TAMPERED") {
		t.Fatalf("tampered archive served: %d", status)
	}

	// local precedence: neither metadata nor archives of a local collection go upstream
	before := apiHits.Load()
	if status, _, _ = f.Get(base + "downloads/mine/coll/2.0.0/collection.tar.gz"); status != 404 {
		t.Fatalf("local owner download: %d", status)
	}
	if status, body, _ = f.Get(base + "v3/collections/mine/coll/versions/1.0.0/"); status != 200 || !strings.Contains(body, "1.0.0") {
		t.Fatalf("local detail: %d %s", status, body)
	}
	if apiHits.Load() != before {
		t.Fatal("locally owned collection reached upstream")
	}

	// outage: cached archive downloads; uncached collection is 503
	down.Store(true)
	if status, body, _ = f.Get(dl); status != 200 || body != string(good) {
		t.Fatalf("download in outage: %d", status)
	}
	if status, _, _ = f.Get(base + "v3/collections/other/thing/versions/"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
