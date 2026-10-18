package huggingface

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

func TestHuggingFacePullThrough(t *testing.T) {
	f := remotetest.New(t)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	weights := []byte("0123456789-WEIGHTS-0123456789")
	weights2 := []byte("SECOND-LFS-FILE-CONTENT")
	config := []byte(`{"model_type":"test"}`)
	good, bad := []byte("EXPECTED"), []byte("TAMPERED")
	var hubHits, cdnHits atomic.Int32
	var down atomic.Bool
	cdn := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the CDN origin")
		}
		switch r.URL.Path {
		case "/lfs/w1":
			_, _ = w.Write(weights)
		case "/lfs/w2":
			_, _ = w.Write(weights2)
		case "/lfs/x":
			_, _ = w.Write(bad)
		default:
			http.NotFound(w, r)
		}
	}))
	hub := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubHits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer hf_secret" {
			t.Errorf("source credential missing on hub request %s: %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		switch r.URL.Path {
		case "/api/models/org/model/revision/main", "/api/models/org/model/revision/" + commit:
			fmt.Fprintf(w, `{"id":"org/model","sha":"%s","siblings":[{"rfilename":"config.json"},{"rfilename":"w.bin"}]}`, commit)
		case "/api/models/org/model/tree/" + commit:
			fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d,"oid":"aaaa"},{"type":"file","path":"w.bin","size":%d,"oid":"bbbb","lfs":{"oid":"%s","size":%d}},{"type":"file","path":"w2.bin","size":%d,"oid":"cccc","lfs":{"oid":"%s","size":%d}},{"type":"directory","path":"sub","oid":"dddd"}]`,
				len(config), len(weights), sumOf(weights), len(weights), len(weights2), sumOf(weights2), len(weights2))
		case "/api/models/org/model/tree/" + commit + "/sub":
			fmt.Fprintf(w, `[{"type":"file","path":"sub/x.bin","size":%d,"oid":"eeee","lfs":{"oid":"%s","size":%d}}]`, len(good), sumOf(good), len(good))
		case "/org/model/resolve/" + commit + "/config.json":
			_, _ = w.Write(config)
		case "/org/model/resolve/" + commit + "/w.bin":
			http.Redirect(w, r, cdn.URL+"/lfs/w1", http.StatusFound)
		case "/org/model/resolve/" + commit + "/w2.bin":
			http.Redirect(w, r, cdn.URL+"/lfs/w2", http.StatusFound)
		case "/org/model/resolve/" + commit + "/sub/x.bin":
			http.Redirect(w, r, cdn.URL+"/lfs/x", http.StatusFound)
		case "/api/models/org/gated/revision/main":
			http.Error(w, "gated", 403)
		default:
			if strings.Contains(r.URL.Path, "/mine") {
				t.Errorf("locally owned repository requested upstream: %s", r.URL.Path)
			}
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("hf", "huggingface")
	local := f.AddLocal(feed, "huggingface", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "huggingface", "hf", 1, hub.URL, remotetest.RemoteOptions{Credential: "Bearer hf_secret", RedirectOrigins: []string{cdn.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/hf/huggingface/"

	// local owner of org/mine
	if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: "models/org/mine", Version: commit, AssetKey: "x", MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(sumOf([]byte("x"))), Size: 1}}, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}

	status, body, _ := f.Get(base + "api/models/org/model")
	if status != 404 && status != 200 {
		t.Fatalf("plain info: %d", status)
	}
	status, body, _ = f.Get(base + "api/models/org/model/revision/main")
	if status != 200 || !strings.Contains(body, commit) || !strings.Contains(body, "w.bin") {
		t.Fatalf("info: %d %s", status, body)
	}
	status, body, _ = f.Get(base + "api/models/org/model/tree/main")
	if status != 200 || !strings.Contains(body, `"path":"w.bin"`) || strings.Contains(body, "sub/x.bin") {
		t.Fatalf("tree: %d %s", status, body)
	}
	if status, body, _ = f.Get(base + "api/models/org/model/tree/main?recursive=true"); status != 200 || !strings.Contains(body, "sub/x.bin") {
		t.Fatalf("recursive tree: %d %s", status, body)
	}

	// HEAD of an LFS file is answered from metadata without touching the CDN
	req, _ := http.NewRequest("HEAD", base+"org/model/resolve/main/w.bin", nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 || res.Header.Get("X-Repo-Commit") != commit || res.Header.Get("X-Linked-Etag") != `"`+sumOf(weights)+`"` || res.Header.Get("Content-Length") != fmt.Sprint(len(weights)) {
		t.Fatalf("head: %v %v", err, res)
	}
	res.Body.Close()
	if cdnHits.Load() != 0 {
		t.Fatal("HEAD downloaded the file")
	}

	// GET miss streams through the redirect, caches, and the next read is local
	if status, body, _ = f.Get(base + "org/model/resolve/main/w.bin"); status != 200 || body != string(weights) {
		t.Fatalf("miss: %d %q", status, body)
	}
	hits := cdnHits.Load()
	if status, body, _ = f.Get(base + "org/model/resolve/" + commit + "/w.bin"); status != 200 || body != string(weights) || cdnHits.Load() != hits {
		t.Fatalf("hit: %d", status)
	}
	if status, body, _ = f.Get(base+"org/model/resolve/"+commit+"/w.bin", "Range", "bytes=2-5"); status != 206 || body != string(weights[2:6]) {
		t.Fatalf("range hit: %d %q", status, body)
	}

	// Range on a miss: verified fill, then the range is answered from the store
	if status, body, _ = f.Get(base+"org/model/resolve/"+commit+"/w2.bin", "Range", "bytes=3-"); status != 206 || body != string(weights2[3:]) {
		t.Fatalf("range miss: %d %q", status, body)
	}
	hits = cdnHits.Load()
	if status, body, _ = f.Get(base + "org/model/resolve/" + commit + "/w2.bin"); status != 200 || body != string(weights2) || cdnHits.Load() != hits {
		t.Fatalf("range fill not cached: %d", status)
	}

	// non-LFS file: length checked only, HEAD fills the (small) file
	if status, body, _ = f.Get(base + "org/model/resolve/" + commit + "/config.json"); status != 200 || body != string(config) {
		t.Fatalf("config: %d %q", status, body)
	}

	// digest mismatch: the connection is aborted and nothing is cached
	assets := f.CachedAssets(remote)
	req, _ = http.NewRequest("GET", base+"org/model/resolve/"+commit+"/sub/x.bin", nil)
	req.Header.Set("Authorization", "Bearer "+f.Token)
	if res, err = http.DefaultClient.Do(req); err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("tampered download completed")
	}
	if f.CachedAssets(remote) != assets {
		t.Fatal("tampered file cached")
	}

	// gated repository: forbidden upstream is reported as unavailable, never as content
	if status, _, _ = f.Get(base + "org/gated/resolve/main/x"); status != 503 {
		t.Fatalf("gated: %d", status)
	}

	// local precedence
	before := hubHits.Load()
	if status, _, _ = f.Get(base + "org/mine/resolve/main/x"); status != 404 {
		t.Fatalf("local owner: %d", status)
	}
	if status, _, _ = f.Get(base + "api/models/org/mine/revision/main"); status != 404 {
		t.Fatalf("local owner info: %d", status)
	}
	if hubHits.Load() != before {
		t.Fatal("locally owned repository reached upstream")
	}

	// outage: cached files by commit and by branch still resolve, uncached ones are 503
	down.Store(true)
	if status, body, _ = f.Get(base + "org/model/resolve/" + commit + "/w.bin"); status != 200 || body != string(weights) {
		t.Fatalf("commit in outage: %d", status)
	}
	if status, body, _ = f.Get(base + "org/model/resolve/main/w.bin"); status != 200 || body != string(weights) {
		t.Fatalf("branch in outage: %d", status)
	}
	if status, _, _ = f.Get(base + "org/other/resolve/main/w.bin"); status != 503 {
		t.Fatalf("uncached in outage: %d", status)
	}
}
