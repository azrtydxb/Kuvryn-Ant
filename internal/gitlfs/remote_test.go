package gitlfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func oidOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type batchObject struct {
	OID     string            `json:"oid"`
	Size    int64             `json:"size"`
	Actions map[string]action `json:"actions"`
	Error   *objectError      `json:"error"`
}

func TestLFSPullThrough(t *testing.T) {
	f := remotetest.New(t)
	small := []byte("hello large file storage")
	big := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	cdnObj := []byte("served from a signing CDN")
	tampered := []byte("what the CDN really sends")
	listed := []byte("what the batch API promised")
	sized := []byte("really eleven")
	var batches, gets atomic.Int32
	var down atomic.Bool
	cdn := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the CDN origin")
		}
		switch r.URL.Path {
		case "/signed/cdn":
			_, _ = w.Write(cdnObj)
		case "/signed/tampered":
			_, _ = w.Write(tampered)
		default:
			http.NotFound(w, r)
		}
	}))
	var upstream *httptest.Server
	upstream = f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "outage", 503)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing source credential on %s %s", r.Method, r.URL.Path)
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/org/repo.git/info/lfs/objects/batch":
			batches.Add(1)
			if r.Header.Get("Content-Type") != mediaType || r.Header.Get("Accept") != mediaType {
				t.Errorf("batch headers %q %q", r.Header.Get("Content-Type"), r.Header.Get("Accept"))
			}
			var req struct {
				Operation string   `json:"operation"`
				Transfers []string `json:"transfers"`
				HashAlgo  string   `json:"hash_algo"`
				Objects   []object `json:"objects"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Operation != "download" || len(req.Objects) != 1 {
				t.Errorf("bad upstream batch %+v %v", req, err)
				http.Error(w, "bad", 400)
				return
			}
			o := req.Objects[0]
			res := map[string]any{"oid": o.OID, "size": *o.Size}
			switch o.OID {
			case oidOf(small), oidOf(big):
				res["actions"] = map[string]any{"download": map[string]any{"href": upstream.URL + "/org/repo.git/info/lfs/objects/" + o.OID + "?sig=abc", "header": map[string]string{"X-Ignored": "1"}}}
			case oidOf(cdnObj):
				res["actions"] = map[string]any{"download": map[string]any{"href": cdn.URL + "/signed/cdn?X-Amz-Signature=1"}}
			case oidOf(listed):
				res["actions"] = map[string]any{"download": map[string]any{"href": cdn.URL + "/signed/tampered"}}
			case oidOf(sized):
				res["size"] = len(sized)
				res["actions"] = map[string]any{"download": map[string]any{"href": upstream.URL + "/org/repo.git/info/lfs/objects/" + o.OID}}
			case oidOf([]byte("owned")):
				t.Errorf("locally owned oid requested upstream")
			default:
				res["error"] = map[string]any{"code": 404, "message": "not found"}
			}
			w.Header().Set("Content-Type", mediaType)
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": []any{res}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/org/repo.git/info/lfs/objects/"):
			gets.Add(1)
			switch strings.TrimPrefix(r.URL.Path, "/org/repo.git/info/lfs/objects/") {
			case oidOf(small):
				_, _ = w.Write(small)
			case oidOf(big):
				_, _ = w.Write(big)
			default:
				http.NotFound(w, r)
			}
		default:
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	feed := f.AddFeed("lfs", "git-lfs")
	local := f.AddLocal(feed, "git-lfs", "local", 0)
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: local, Kind: routing.Local}}, DefaultTarget: local})
	if err != nil {
		t.Fatal(err)
	}
	f.Service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	remote := f.AddRemote(feed, "git-lfs", "upstream", 1, upstream.URL+"/org/repo.git/info/lfs", remotetest.RemoteOptions{Credential: "Bearer secret", RedirectOrigins: []string{cdn.URL}})
	repo, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(repo)
	defer srv.Close()
	if err = repo.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/feeds/lfs/git-lfs/objects/"

	batch := func(data []byte, declared int64) batchObject {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"operation": "download", "transfers": []string{"basic"}, "objects": []map[string]any{{"oid": oidOf(data), "size": declared}}})
		req, _ := http.NewRequest("POST", base+"batch", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.Token)
		req.Header.Set("Content-Type", mediaType)
		req.Header.Set("Accept", mediaType)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			Objects []batchObject `json:"objects"`
		}
		if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&out) != nil || len(out.Objects) != 1 {
			t.Fatalf("batch status %d", res.StatusCode)
		}
		return out.Objects[0]
	}
	fetch := func(o batchObject, method string, headers ...string) (int, []byte, http.Header) {
		t.Helper()
		a, ok := o.Actions["download"]
		if !ok {
			t.Fatalf("no download action: %+v", o)
		}
		req, _ := http.NewRequest(method, a.Href, nil)
		for k, v := range a.Header {
			req.Header.Set(k, v)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b, res.Header
	}

	// found upstream: batch answers with an Ant action, nothing is cached yet
	o := batch(small, int64(len(small)))
	if o.Error != nil || !strings.HasPrefix(o.Actions["download"].Href, srv.URL+"/feeds/lfs/git-lfs/objects/"+oidOf(small)+"/download") {
		t.Fatalf("batch: %+v", o)
	}
	if f.CachedAssets(remote) != 0 {
		t.Fatal("batch alone must not cache")
	}
	// download: miss -> streamed + verified -> cached
	if status, body, _ := fetch(o, "GET"); status != 200 || !bytes.Equal(body, small) {
		t.Fatalf("miss: %d %q", status, body)
	}
	if f.CachedAssets(remote) != 1 {
		t.Fatalf("object not cached: %d", f.CachedAssets(remote))
	}
	// second batch is a local hit: no more upstream traffic, ranges work
	before := batches.Load() + gets.Load()
	o = batch(small, int64(len(small)))
	if status, body, _ := fetch(o, "GET", "Range", "bytes=6-10"); status != 206 || string(body) != "large" {
		t.Fatalf("range hit: %d %q", status, body)
	}
	if batches.Load()+gets.Load() != before {
		t.Fatal("cached object reached upstream")
	}
	// object served from a CDN host (redirect origin) without the source credential
	o = batch(cdnObj, int64(len(cdnObj)))
	if status, body, _ := fetch(o, "GET"); status != 200 || !bytes.Equal(body, cdnObj) {
		t.Fatalf("cdn: %d %q", status, body)
	}
	// a range request on a miss fills the cache completely and answers from the store
	o = batch(big, int64(len(big)))
	if status, body, _ := fetch(o, "GET", "Range", "bytes=16-31"); status != 206 || string(body) != "0123456789abcdef" {
		t.Fatalf("range miss: %d %q", status, body)
	}
	if status, body, _ := fetch(batch(big, int64(len(big))), "GET"); status != 200 || !bytes.Equal(body, big) {
		t.Fatalf("full after range: %d", status)
	}
	// HEAD on a miss answers from the grant without fetching
	// content that does not hash to the oid is rejected and not cached
	cached := f.CachedAssets(remote)
	o = batch(listed, int64(len(listed)))
	{
		a := o.Actions["download"]
		req, _ := http.NewRequest("GET", a.Href, nil)
		for k, v := range a.Header {
			req.Header.Set(k, v)
		}
		if res, err := http.DefaultClient.Do(req); err == nil {
			// The bytes stream while they are verified: the transfer must end in an error.
			if b, rerr := io.ReadAll(res.Body); rerr == nil {
				t.Fatalf("tampered object completed: %q", b)
			}
			res.Body.Close()
		}
	}
	if f.CachedAssets(remote) != cached {
		t.Fatal("tampered object cached")
	}
	// unknown upstream object and wrong declared size
	if o = batch([]byte("nope"), 4); o.Error == nil || o.Error.Code != 404 {
		t.Fatalf("unknown: %+v", o)
	}
	if o = batch(sized, 3); o.Error == nil || o.Error.Code != 422 {
		t.Fatalf("size mismatch: %+v", o)
	}
	// local ownership: an oid held by a local source never goes upstream
	owned := []byte("owned")
	if _, err = f.Service.Publish(context.Background(), f.Principal, artifact.Publication{FeedID: feed, Namespace: "git-lfs", Coordinate: oidOf(owned), Version: "sha256", AssetKey: "object", MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(oidOf(owned)), Size: int64(len(owned))}}, bytes.NewReader(owned)); err != nil {
		t.Fatal(err)
	}
	if o = batch(owned, int64(len(owned))); o.Error != nil {
		t.Fatalf("owned: %+v", o)
	}
	// outage: cached still served, uncached is unavailable
	down.Store(true)
	if status, body, _ := fetch(batch(small, int64(len(small))), "GET"); status != 200 || !bytes.Equal(body, small) {
		t.Fatalf("cached in outage: %d", status)
	}
	if o = batch([]byte(fmt.Sprint("fresh")), 5); o.Error == nil || o.Error.Code == 200 {
		t.Fatalf("uncached in outage: %+v", o)
	}
}
