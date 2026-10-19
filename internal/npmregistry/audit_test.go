package npmregistry

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestAuditForwardsToRemoteAndCaches(t *testing.T) {
	f := remotetest.New(t)
	var hits atomic.Int32
	var last atomic.Value
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/-/npm/v1/security/advisories/bulk" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		hits.Add(1)
		last.Store(string(b) + "|" + r.Header.Get("Content-Encoding"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"left-pad":[{"id":1,"title":"x","severity":"high"}]}`))
	}))
	feed := f.AddFeed("npmaudit", "npm")
	f.AddRemote(feed, "npm", "registry", 0, up.URL, remotetest.RemoteOptions{MetadataTTL: 1, StaleSeconds: 60})
	local := f.AddFeed("npmlocal", "npm")
	f.AddLocal(local, "npm", "local", 0)
	registry, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	if err = registry.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	post := func(path string, body []byte, gz bool) (int, string, http.Header) {
		var rd io.Reader = bytes.NewReader(body)
		if gz {
			var b bytes.Buffer
			zw := gzip.NewWriter(&b)
			_, _ = zw.Write(body)
			_ = zw.Close()
			rd = &b
		}
		req, _ := http.NewRequest("POST", srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+f.Token)
		req.Header.Set("Content-Type", "application/json")
		if gz {
			req.Header.Set("Content-Encoding", "gzip")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(out), res.Header
	}
	q := []byte(`{"left-pad":["1.0.0"]}`)
	path := "/feeds/npmaudit/npm/-/npm/v1/security/advisories/bulk"
	status, body, h := post(path, q, true)
	if status != 200 || !strings.Contains(body, "left-pad") || h.Get("X-Kuvryn-Cache") != "miss" {
		t.Fatalf("gzip audit: %d %s %v", status, body, h)
	}
	if got := last.Load().(string); got != string(q)+"|" {
		t.Fatalf("upstream saw %q", got)
	}
	if status, _, h = post(path, q, false); status != 200 || h.Get("X-Kuvryn-Cache") != "hit" || hits.Load() != 1 {
		t.Fatalf("expected cached answer: %d %v hits=%d", status, h, hits.Load())
	}
	if status, _, _ = post("/feeds/npmaudit/npm/-/npm/v1/security/audits/quick", q, false); status != 404 {
		t.Fatalf("upstream without quick endpoint: %d", status)
	}
	if status, _, _ = post("/feeds/npmaudit/npm/-/npm/v1/security/nope", q, false); status != 404 {
		t.Fatalf("unknown audit route: %d", status)
	}
	if status, _, _ = post(path, bytes.Repeat([]byte("a"), 1<<20+10), false); status != 400 && status != 413 {
		t.Fatalf("oversize body: %d", status)
	}
	if status, _, _ = post(path, nil, false); status != 400 {
		t.Fatalf("empty body: %d", status)
	}
	if status, _, _ := f.Get(srv.URL + path); status != 405 {
		t.Fatalf("GET audit: %d", status)
	}
	if status, body, _ := post("/feeds/npmlocal/npm/-/npm/v1/security/advisories/bulk", q, false); status != 404 || !strings.Contains(body, "audit unavailable") {
		t.Fatalf("local-only feed: %d %s", status, body)
	}
}
