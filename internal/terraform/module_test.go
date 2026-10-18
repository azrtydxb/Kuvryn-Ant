package terraform

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func moduleTarball(t *testing.T, files map[string]string, extra ...tar.Header) []byte {
	t.Helper()
	var raw bytes.Buffer
	g := gzip.NewWriter(&raw)
	tw := tar.NewWriter(g)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(files[n]))
	}
	for i := range extra {
		h := extra[i]
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = g.Close()
	return raw.Bytes()
}

func TestDiscovery(t *testing.T) {
	f := setup(t)
	req := httptest.NewRequest("GET", "/feeds/packages/terraform/.well-known/terraform.json", nil)
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req) // no credentials: Terraform sends none to discovery
	assertCode(t, w, 200)
	var doc map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["modules.v1"] != "https://packages.example/feeds/packages/terraform/v1/modules/" || doc["providers.v1"] != "https://packages.example/feeds/packages/terraform/v1/providers/" {
		t.Fatal(doc)
	}
	req = httptest.NewRequest("POST", "/feeds/packages/terraform/.well-known/terraform.json", nil)
	w = httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 405)
	// Everything else requires machine credentials.
	req = httptest.NewRequest("GET", testBase+"v1/modules/a/b/c/versions", nil)
	w = httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 401)
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatal("missing challenge")
	}
	for _, bad := range []string{"http://example.com", "https://example.com/path", "ftp://x", "https://u:p@example.com"} {
		if f.registry.SetPublicURL(bad) == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestModule(t *testing.T) {
	f := setup(t)
	const base = testBase + "v1/modules/acme/vpc/aws/"
	good := moduleTarball(t, map[string]string{"main.tf": `resource "null_resource" "a" {}`, "modules/x/main.tf": ""})
	assertCode(t, f.request("PUT", base+"1.0.0", good, nil), 201)
	assertCode(t, f.request("PUT", base+"1.0.0", good, nil), 201)
	assertCode(t, f.request("PUT", base+"1.0.0", moduleTarball(t, map[string]string{"main.tf": "# changed"}), nil), 409)
	assertCode(t, f.request("PUT", base+"1.1.0-rc.1", good, map[string]string{"X-Checksum-Sha256": fmt.Sprintf("%x", sha256.Sum256(good))}), 201)
	assertCode(t, f.request("PUT", base+"1.2.0", good, map[string]string{"X-Checksum-Sha256": strings.Repeat("0", 64)}), 502)
	w := f.request("GET", base+"versions", nil, nil)
	assertCode(t, w, 200)
	var versions struct {
		Modules []struct {
			Source   string
			Versions []struct{ Version string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &versions); err != nil || len(versions.Modules) != 1 || versions.Modules[0].Source != "acme/vpc/aws" || len(versions.Modules[0].Versions) != 2 {
		t.Fatal(err, w.Body.String())
	}
	w = f.request("GET", base+"1.0.0/download", nil, nil)
	assertCode(t, w, 204)
	if got := w.Header().Get("X-Terraform-Get"); got != "https://packages.example/feeds/packages/terraform/v1/modules/acme/vpc/aws/1.0.0/archive.tar.gz" {
		t.Fatal(got)
	}
	w = f.request("GET", base+"1.0.0/archive.tar.gz?terraform-get=1", nil, nil)
	assertCode(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), good) {
		t.Fatal("archive changed")
	}
	assertCode(t, f.request("HEAD", base+"1.0.0/archive.tar.gz", nil, nil), 200)
	assertCode(t, f.request("GET", base+"9.9.9/download", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"v1/modules/acme/none/aws/versions", nil, nil), 404)
	assertCode(t, f.request("DELETE", base+"1.0.0", nil, nil), 405)
	assertCode(t, f.request("GET", testBase+"v1/modules/acme/vpc/aws/..%2f/versions", nil, nil), 404)

	// Malformed and unsafe archives are rejected before publication.
	assertCode(t, f.request("PUT", base+"2.0.0", []byte("not gzip"), nil), 400)
	assertCode(t, f.request("PUT", base+"2.0.0", moduleTarball(t, map[string]string{"README.md": "no config"}), nil), 400)
	assertCode(t, f.request("PUT", base+"2.0.0", moduleTarball(t, map[string]string{"main.tf": ""}, tar.Header{Name: "../evil", Mode: 0o644, Typeflag: tar.TypeReg}), nil), 400)
	assertCode(t, f.request("PUT", base+"2.0.0", moduleTarball(t, map[string]string{"main.tf": ""}, tar.Header{Name: "link", Linkname: "../../etc/passwd", Typeflag: tar.TypeSymlink}), nil), 400)
	assertCode(t, f.request("PUT", base+"2.0.0", moduleTarball(t, map[string]string{"main.tf": ""}, tar.Header{Name: "hard", Linkname: "main.tf", Typeflag: tar.TypeLink}), nil), 400)
	assertCode(t, f.request("PUT", base+"not-a-version", good, nil), 400)
	assertCode(t, f.request("PUT", testBase+"v1/modules/Acme/vpc/aws/1.0.0", good, nil), 404)
	assertCode(t, f.request("GET", base+"2.0.0/download", nil, nil), 404)

	// Concurrent different bytes: exactly one winner.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, s := range []string{"one", "two"} {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			codes <- f.request("PUT", base+"3.0.0", moduleTarball(t, map[string]string{"main.tf": "# " + s}), nil).Code
		}(s)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}

	// A read-only token may consume but not ingest.
	reader, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", base+"4.0.0", bytes.NewReader(good))
	req.Header.Set("Authorization", "Bearer "+reader.Secret)
	w = httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("read-only upload: %d", w.Code)
	}
	req = httptest.NewRequest("GET", base+"versions", nil)
	req.Header.Set("Authorization", "Bearer "+reader.Secret)
	w = httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 200)
}
