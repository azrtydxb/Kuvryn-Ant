package npmregistry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact/remotetest"
)

func TestRewritePackumentStreams(t *testing.T) {
	// dist-tags after versions, a dangling tag, an invalid version, an
	// unknown member and _rev.
	src := `{"_rev":"9-x","versions":{"1.0.0":{"version":"1.0.0","dist":{"tarball":"https://up/x.tgz","shasum":"s"}},"bad version":{"dist":{}},"1.1.0":{"version":"1.1.0"}},"time":{"1.0.0":"t"},"dist-tags":{"latest":"1.1.0","gone":"9.9.9"},"readme":"<b>&</b>"}`
	var out bytes.Buffer
	if err := rewritePackument(&out, strings.NewReader(src), "pkg", func(v string) string { return "https://ant/" + v }); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Name, ID string `json:"name"`
		Tags     map[string]string
		Versions map[string]struct{ Dist map[string]string }
		Time     map[string]string
		Rev      *string `json:"_rev"`
		Readme   string
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	_ = json.Unmarshal(raw["versions"], &doc.Versions)
	_ = json.Unmarshal(raw["dist-tags"], &doc.Tags)
	_ = json.Unmarshal(raw["readme"], &doc.Readme)
	if len(doc.Versions) != 2 || doc.Versions["1.0.0"].Dist["tarball"] != "https://ant/1.0.0" || doc.Versions["1.0.0"].Dist["shasum"] != "s" || doc.Versions["1.1.0"].Dist["tarball"] != "https://ant/1.1.0" {
		t.Fatalf("versions: %s", out.String())
	}
	if len(doc.Tags) != 1 || doc.Tags["latest"] != "1.1.0" || raw["_rev"] != nil || string(raw["_id"]) != `"pkg"` || string(raw["name"]) != `"pkg"` || doc.Readme != "<b>&</b>" || raw["time"] == nil {
		t.Fatalf("members: %s", out.String())
	}
	for _, bad := range []string{`[]`, `{"versions":[]}`, `{"versions":{"1.0.0":"x"}}`, `{"versions":{"1.0.0":{}}`} {
		if err := rewritePackument(&bytes.Buffer{}, strings.NewReader(bad), "pkg", func(string) string { return "" }); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// A packument larger than both the metadata cache bound and the 16 MiB
// rewrite limit is streamed through, in either representation.
func TestNPMLargeRemotePackumentStreams(t *testing.T) {
	f := remotetest.New(t)
	const count = 20000
	pad := strings.Repeat("x", 1000)
	var big strings.Builder
	big.WriteString(`{"name":"huge","dist-tags":{"latest":"1.0.19999"},"versions":{`)
	for i := 0; i < count; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		fmt.Fprintf(&big, `"1.0.%d":{"name":"huge","version":"1.0.%d","description":"%s","dist":{"tarball":"https://registry.upstream.invalid/huge/-/huge-1.0.%d.tgz","integrity":"sha512-q"}}`, i, i, pad, i)
	}
	big.WriteString(`}}`)
	if big.Len() < 17<<20 {
		t.Fatalf("fixture too small: %d", big.Len())
	}
	up := f.Upstream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/huge" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"up1"`)
		_, _ = w.Write([]byte(big.String()))
	}))
	feed := f.AddFeed("npmbig", "npm")
	remote := f.AddRemote(feed, "npm", "registry", 0, up.URL, remotetest.RemoteOptions{})
	registry, err := New(f.Pool, f.Service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(registry)
	defer srv.Close()
	if err = registry.SetPublicURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	for _, accept := range []string{"application/json", abbreviated} {
		status, body, header := f.Get(srv.URL+"/feeds/npmbig/npm/huge", "Accept", accept)
		if status != 200 || len(body) < 17<<20 {
			t.Fatalf("%s: %d len=%d", accept, status, len(body))
		}
		if strings.Contains(body, "upstream.invalid") || !strings.Contains(body, srv.URL+"/feeds/npmbig/npm/huge/-/huge-1.0.19999.tgz?source="+remote) {
			t.Fatalf("%s: tarball not rewritten", accept)
		}
		var doc struct {
			Versions map[string]json.RawMessage
			Tags     map[string]string `json:"dist-tags"`
		}
		if err = json.Unmarshal([]byte(body), &doc); err != nil || len(doc.Versions) != count || doc.Tags["latest"] != "1.0.19999" {
			t.Fatalf("%s: invalid document: %v", accept, err)
		}
		etag := header.Get("ETag")
		if etag == "" {
			t.Fatal("no etag")
		}
		if status, _, _ = f.Get(srv.URL+"/feeds/npmbig/npm/huge", "Accept", accept, "If-None-Match", etag); status != 304 {
			t.Fatalf("conditional: %d", status)
		}
	}
}
