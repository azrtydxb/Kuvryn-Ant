package swiftrepo

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func (f fixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Accept", "application/vnd.swift.registry.v1+json")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func assertCode(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}

const base = "/feeds/packages/swift/"
const meta = `{"description":"demo","repositoryURLs":["https://github.com/Acme/Demo.git","git@github.com:acme/demo.git"]}`

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(c))
	}
	_ = zw.Close()
	return b.Bytes()
}
func goodZip(t *testing.T, extra string) []byte {
	return zipBytes(t, map[string]string{
		"demo/Package.swift":           "// swift-tools-version:5.9\nimport PackageDescription\n// " + extra + "\n",
		"demo/Package@swift-5.7.swift": "// swift-tools-version:5.7\nimport PackageDescription\n",
		"demo/Sources/demo/a.swift":    "print(1)",
	})
}

func form(parts [][2]string) ([]byte, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		w, _ := mw.CreateFormField(p[0])
		_, _ = w.Write([]byte(p[1]))
	}
	_ = mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}
func publish(f fixture, path string, archive []byte, metadata string, extra map[string]string) *httptest.ResponseRecorder {
	body, ct := form([][2]string{{"source-archive", string(archive)}, {"metadata", metadata}})
	h := map[string]string{"Content-Type": ct}
	for k, v := range extra {
		h[k] = v
	}
	return f.request("PUT", path, body, h)
}

func TestSemver(t *testing.T) {
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.10.0", "2.0.0"}
	for i := 1; i < len(order); i++ {
		if !semverLess(order[i-1], order[i]) || semverLess(order[i], order[i-1]) {
			t.Fatal(order[i-1], order[i])
		}
	}
	if normalizeRepoURL("git@github.com:Acme/Demo.git") != normalizeRepoURL("https://github.com/acme/demo/") {
		t.Fatal("url normalization")
	}
}

func TestPostgres(t *testing.T) {
	f := setup(t)
	z := goodZip(t, "")
	w := publish(f, base+"Acme/Demo/1.0.0", z, meta, nil)
	assertCode(t, w, 201)
	if !strings.HasSuffix(w.Header().Get("Location"), "/feeds/packages/swift/Acme/Demo/1.0.0") {
		t.Fatal(w.Header())
	}
	assertCode(t, publish(f, base+"Acme/Demo/1.0.0", z, meta, nil), 201) // idempotent
	assertCode(t, publish(f, base+"Acme/Demo/1.0.0", goodZip(t, "changed"), meta, nil), 409)
	assertCode(t, publish(f, base+"Acme/Demo/1.1.0", goodZip(t, "x"), meta, nil), 201)
	assertCode(t, publish(f, base+"Acme/Demo/1.2.0-beta.1", goodZip(t, "y"), meta, nil), 201)

	w = f.request("GET", base+"acme/demo", nil, nil)
	assertCode(t, w, 200)
	var list struct {
		Releases map[string]map[string]string `json:"releases"`
	}
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Releases) != 3 || list.Releases["1.1.0"]["url"] != "https://packages.example/feeds/packages/swift/acme/demo/1.1.0" {
		t.Fatal(w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Link"), `demo/1.2.0-beta.1>; rel="latest-version"`) {
		t.Fatal(w.Header().Get("Link"))
	}
	w = f.request("GET", base+"Acme/Demo/1.0.0", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), `"id":"acme.demo"`) || !strings.Contains(w.Body.String(), `"name":"source-archive"`) || !strings.Contains(w.Body.String(), `"checksum"`) {
		t.Fatal(w.Body.String())
	}
	w = f.request("GET", base+"acme/demo/1.0.0/Package.swift", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "tools-version:5.9") || !strings.Contains(w.Header().Get("Link"), `swift-tools-version="5.7"`) || !strings.Contains(w.Header().Get("Link"), `filename="Package@swift-5.7.swift"`) {
		t.Fatal(w.Body.String(), w.Header())
	}
	w = f.request("GET", base+"acme/demo/1.0.0/Package.swift?swift-version=5.7", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "tools-version:5.7") {
		t.Fatal(w.Body.String())
	}
	assertCode(t, f.request("GET", base+"acme/demo/1.0.0/Package.swift?swift-version=x", nil, nil), 400)
	w = f.request("GET", base+"acme/demo/1.0.0.zip", nil, nil)
	assertCode(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), z) || !strings.HasPrefix(w.Header().Get("Digest"), "sha-256=") || !strings.Contains(w.Header().Get("Content-Disposition"), `filename="demo-1.0.0.zip"`) || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(w.Header())
	}
	assertCode(t, f.request("HEAD", base+"acme/demo/1.0.0.zip", nil, nil), 200)
	w = f.request("GET", base+"identifiers?url=https://github.com/acme/demo", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), `["acme.demo"]`) {
		t.Fatal(w.Body.String())
	}
	assertCode(t, f.request("GET", base+"identifiers?url=ssh://git@github.com/other/x.git", nil, nil), 404)
	assertCode(t, f.request("GET", base+"identifiers?url=bogus", nil, nil), 400)

	w = f.request("GET", base+"acme/demo", nil, map[string]string{"Accept": ""})
	assertCode(t, w, 400)
	if w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal(w.Header())
	}
	assertCode(t, f.request("GET", base+"acme/demo", nil, map[string]string{"Accept": "application/vnd.swift.registry.v2+json"}), 415)
	assertCode(t, f.request("GET", base+"acme/nope", nil, nil), 404)
	assertCode(t, f.request("GET", base+"acme/demo/9.9.9", nil, nil), 404)
	assertCode(t, f.request("GET", base+"acme/demo", nil, map[string]string{"Authorization": "Bearer bad"}), 401)
	assertCode(t, f.request("DELETE", base+"acme/demo/1.0.0", nil, nil), 403) // fixture token has no delete action

	assertCode(t, publish(f, base+"acme/bad/1.0.0", []byte("not a zip"), meta, nil), 422)
	assertCode(t, publish(f, base+"acme/bad/1.0.0", zipBytes(t, map[string]string{"x/readme": "no manifest"}), meta, nil), 422)
	assertCode(t, publish(f, base+"acme/bad/1.0.0", zipBytes(t, map[string]string{"../Package.swift": "x"}), meta, nil), 422)
	assertCode(t, publish(f, base+"acme/bad/1.0.0", z, `{"repositoryURLs":["ftp://x/y"]}`, nil), 422)
	assertCode(t, publish(f, base+"acme/bad/1.0.0", z, `not json`, nil), 422)
	assertCode(t, publish(f, base+"acme/bad/1.0.0", z, meta, map[string]string{"X-Checksum-Sha256": strings.Repeat("0", 64)}), 422)
	assertCode(t, publish(f, base+"acme/bad/notaversion", z, meta, nil), 400)
	assertCode(t, publish(f, base+"-bad/bad/1.0.0", z, meta, nil), 404)
	assertCode(t, f.request("PUT", base+"acme/bad/1.0.0", z, map[string]string{"Content-Type": "application/zip"}), 415)
	assertCode(t, f.request("GET", base+"acme/bad/1.0.0", nil, nil), 404)
	body, ct := form([][2]string{{"metadata", "{}"}})
	assertCode(t, f.request("PUT", base+"acme/bad/1.0.0", body, map[string]string{"Content-Type": ct}), 422)
	body, ct = form([][2]string{{"evil", "x"}})
	assertCode(t, f.request("PUT", base+"acme/bad/1.0.0", body, map[string]string{"Content-Type": ct}), 400)
	assertCode(t, f.request("GET", base+"acme/bad", nil, nil), 404)

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, s := range []string{"one", "two"} {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			codes <- publish(f, base+"acme/race/1.0.0", goodZip(t, s), "{}", nil).Code
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

	body, ct = form([][2]string{{"source-archive", string(goodZip(t, "sig"))}, {"source-archive-signature", "SIGBYTES"}})
	assertCode(t, f.request("PUT", base+"acme/signed/1.0.0", body, map[string]string{"Content-Type": ct, "X-Swift-Package-Signature-Format": "cms-1.0.0"}), 201)
	assertCode(t, f.request("PUT", base+"acme/signed/2.0.0", body, map[string]string{"Content-Type": ct}), 422) // signature without format
	w = f.request("GET", base+"acme/signed/1.0.0", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), `"signatureFormat":"cms-1.0.0"`) {
		t.Fatal(w.Body.String())
	}
	w = f.request("GET", base+"acme/signed/1.0.0.zip", nil, nil)
	if w.Header().Get("X-Swift-Package-Signature-Format") != "cms-1.0.0" {
		t.Fatal(w.Header())
	}

	// Tamper with stored archive bytes: download must fail, never serve bad bytes.
	stored := ""
	_ = filepath.WalkDir(f.blobRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, e := os.ReadFile(p); e == nil && bytes.Equal(b, z) {
				stored = p
			}
		}
		return nil
	})
	if stored == "" {
		t.Fatal("stored archive not found")
	}
	_ = os.Chmod(stored, 0o600)
	if err := os.WriteFile(stored, append(append([]byte{}, z...), 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if w = f.request("GET", base+"acme/demo/1.0.0.zip", nil, nil); w.Code == 200 {
		t.Fatal("tampered archive returned 200")
	}

	// Persisted namespace mismatch fails closed.
	if _, e := f.pool.Exec(context.Background(), `UPDATE components SET namespace='wrong' WHERE source_id=$1`, f.source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", base+"acme/demo/1.1.0", nil, nil), 404)
}
