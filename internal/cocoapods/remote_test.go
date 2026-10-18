package cocoapods

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestCocoaPodsPullThrough(t *testing.T) {
	f := remotetest.New(t)
	spec := `{"name":"AFNetworking","version":"4.0.1","source":{"git":"https://github.com/AFNetworking/AFNetworking.git","tag":"4.0.1"}}`
	// Pick a second pod that falls into the same shard as AFNetworking.
	want := shard("AFNetworking")
	owned := ""
	for i := 0; owned == ""; i++ {
		n := "Local" + string(rune('A'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
		if shard(n) == want {
			owned = n
		}
	}
	var hits atomic.Int32
	var down atomic.Bool
	index := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" {
			t.Errorf("non-GET upstream request %s", r.Method)
		}
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		shardFile := "/all_pods_versions_" + want[0] + "_" + want[1] + "_" + want[2] + ".txt"
		switch {
		case r.URL.Path == shardFile:
			_, _ = w.Write([]byte("AFNetworking/3.0.0/4.0.1\n" + owned + "/9.9.9\nBad Name/1.0\n"))
		case r.URL.Path == "/Specs/"+want[0]+"/"+want[1]+"/"+want[2]+"/AFNetworking/4.0.1/AFNetworking.podspec.json":
			_, _ = w.Write([]byte(spec))
		case strings.Contains(r.URL.Path, "/"+owned+"/"):
			t.Errorf("locally owned pod requested upstream: %s", r.URL.Path)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("pods", "cocoapods")
	local := f.AddLocal(feed, "cocoapods", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "cocoapods", "cdn", 1, index.URL, remotetest.RemoteOptions{Credential: "Bearer secret"})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	base := srv.URL + "/feeds/pods/cocoapods/"

	sum := sha256.Sum256([]byte("{}"))
	if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Coordinate: owned, Version: "1.0.0", AssetKey: podspecKey, MediaType: "application/json", Info: blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: 2}}, strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}

	shardURL := base + "all_pods_versions_" + want[0] + "_" + want[1] + "_" + want[2] + ".txt"
	status, body, _ := f.Get(shardURL)
	if status != 200 || !strings.Contains(body, "AFNetworking/3.0.0/4.0.1\n") || !strings.Contains(body, owned+"/1.0.0\n") || strings.Contains(body, "9.9.9") || strings.Contains(body, "Bad Name") {
		t.Fatalf("shard: %d %q", status, body)
	}

	specURL := base + "Specs/" + want[0] + "/" + want[1] + "/" + want[2] + "/AFNetworking/4.0.1/AFNetworking.podspec.json"
	if status, body, _ = f.Get(specURL); status != 200 || body != spec {
		t.Fatalf("spec miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatalf("cached %d", f.CachedAssets(remote))
	}
	n := hits.Load()
	if status, body, _ = f.Get(specURL); status != 200 || body != spec || hits.Load() != n {
		t.Fatalf("spec hit: %d", status)
	}
	if status, _, _ = f.Get(strings.Replace(specURL, "4.0.1", "9.0.0", 2)); status != 404 {
		t.Fatalf("missing spec: %d", status)
	}
	// local owner: an unpublished version of a locally owned pod never goes upstream
	osh := shard(owned)
	before := hits.Load()
	if status, _, _ = f.Get(base + "Specs/" + osh[0] + "/" + osh[1] + "/" + osh[2] + "/" + owned + "/2.0.0/" + owned + ".podspec.json"); status != 404 || hits.Load() != before {
		t.Fatalf("owned pod reached upstream: %d", status)
	}
	// archives are never proxied
	if status, _, _ = f.Get(base + "archives/AFNetworking/4.0.1/AFNetworking-4.0.1.tar.gz"); status != 404 {
		t.Fatalf("archive: %d", status)
	}

	// outage: cached spec is served, cached versions stay listed
	down.Store(true)
	if status, body, _ = f.Get(specURL); status != 200 || body != spec {
		t.Fatalf("spec in outage: %d", status)
	}
	if status, body, _ = f.Get(shardURL); status != 200 || !strings.Contains(body, "AFNetworking/3.0.0/4.0.1\n") {
		t.Fatalf("cached shard in outage: %d %q", status, body)
	}
	other := "0_0_0"
	if want == [3]string{"0", "0", "0"} {
		other = "f_f_f"
	}
	if status, _, _ = f.Get(base + "all_pods_versions_" + other + ".txt"); status != 503 {
		t.Fatalf("uncached shard in outage: %d", status)
	}
}
