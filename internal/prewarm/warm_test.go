package prewarm

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"golang.org/x/mod/sumdb/dirhash"
)

// recorder is a data plane that serves canned bodies and logs what was asked.
type recorder struct {
	mu     sync.Mutex
	paths  []string
	accept map[string]string
	body   map[string][]byte
	status map[string]int
}

func newRecorder() *recorder {
	return &recorder{accept: map[string]string{}, body: map[string][]byte{}, status: map[string]int{}}
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p := req.URL.Path
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.accept[p] = req.Header.Get("Accept")
	body, ok := r.body[p]
	status := r.status[p]
	r.mu.Unlock()
	if status != 0 {
		http.Error(w, "x", status)
		return
	}
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

func (r *recorder) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.paths...)
	sort.Strings(out)
	return out
}

func target(format string, o Options) Target {
	return Target{FeedID: "f", Slug: "feed", Format: format, Principal: identity.Principal{InstanceID: "i", PrincipalID: "p"}, Options: o}
}

func code(err error) string {
	var ie *ItemError
	if errors.As(err, &ie) {
		return ie.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}

func warm(r *recorder, format string, o Options, it Item) (*Result, error) {
	res := &Result{}
	w := &Warmer{Feeds: r}
	return res, formats[format].warm(context.Background(), w, target(format, o), it, res)
}

func TestWarmNPMAndCargo(t *testing.T) {
	r := newRecorder()
	tgz := []byte("tarball")
	r.body["/feeds/feed/npm/@scope/pkg/-/pkg-2.0.0.tgz"] = tgz
	sum := sha512Hex(tgz)
	res, err := warm(r, "npm", Options{}, Item{Name: "@scope/pkg", Version: "2.0.0", Integrity: "sha512-" + b64FromHex(sum)})
	if err != nil || res.Bytes() != int64(len(tgz)) {
		t.Fatalf("%v %d", err, res.Bytes())
	}
	if _, err = warm(r, "npm", Options{}, Item{Name: "@scope/pkg", Version: "2.0.0", Integrity: "sha512-" + b64(64)}); code(err) != ErrIntegrity {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err = warm(r, "npm", Options{}, Item{Name: "nothing", Version: "1.0.0"}); code(err) != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	crate := []byte("crate-bytes")
	r.body["/feeds/feed/cargo/api/v1/crates/serde/1.0.0/download"] = crate
	h := sha256.Sum256(crate)
	if _, err = warm(r, "cargo", Options{}, Item{Name: "serde", Version: "1.0.0", Integrity: "sha256:" + hex.EncodeToString(h[:])}); err != nil {
		t.Fatal(err)
	}
	if _, err = warm(r, "cargo", Options{}, Item{Name: "serde", Version: "1.0.0", Integrity: "sha256:" + strings.Repeat("0", 64)}); code(err) != ErrIntegrity {
		t.Fatalf("cargo mismatch: %v", err)
	}
}

func TestWarmGoVerifiesGoSumHashes(t *testing.T) {
	r := newRecorder()
	mod := []byte("module example.com/Mod\n\ngo 1.21\n")
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, content := range map[string]string{"example.com/Mod@v1.0.0/go.mod": string(mod), "example.com/Mod@v1.0.0/main.go": "package m\n"} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(content))
	}
	_ = zw.Close()
	zipPath := filepath.Join(t.TempDir(), "m.zip")
	if err := os.WriteFile(zipPath, zb.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	zipHash, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	base := "/feeds/feed/go/example.com/!mod/@v/v1.0.0"
	r.body[base+".info"] = []byte(`{"Version":"v1.0.0"}`)
	r.body[base+".mod"] = mod
	r.body[base+".zip"] = zb.Bytes()
	it := Item{Name: "example.com/Mod", Version: "v1.0.0", Integrity: zipHash, Aux: goHashOfFile(mod)}
	if _, err = warm(r, "go", Options{}, it); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.asked(), " "); got != base+".info "+base+".mod "+base+".zip" {
		t.Fatalf("requests: %s", got)
	}
	bad := it
	bad.Integrity = "h1:" + strings.Repeat("A", 43) + "="
	if _, err = warm(r, "go", Options{}, bad); code(err) != ErrIntegrity {
		t.Fatalf("zip mismatch: %v", err)
	}
	bad = it
	bad.Aux = "h1:" + strings.Repeat("A", 43) + "="
	if _, err = warm(r, "go", Options{}, bad); code(err) != ErrIntegrity {
		t.Fatalf("go.mod mismatch: %v", err)
	}
	// go.sum lines for go.mod only: nothing but go.mod is fetched.
	r2 := newRecorder()
	r2.body[base+".mod"] = mod
	if _, err = warm(r2, "go", Options{}, Item{Name: "example.com/Mod", Version: "v1.0.0", Aux: goHashOfFile(mod)}); err != nil {
		t.Fatal(err)
	}
	if got := r2.asked(); len(got) != 1 || got[0] != base+".mod" {
		t.Fatalf("mod-only: %v", got)
	}
}

func TestWarmPyPISelectsFiles(t *testing.T) {
	r := newRecorder()
	wheel, sdist, native := []byte("wheel"), []byte("sdist"), []byte("native")
	hx := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	prefix := "/feeds/feed/pypi/files/flask-login/"
	page, _ := json.Marshal(map[string]any{"files": []map[string]any{
		{"filename": "Flask_Login-0.6.3-py3-none-any.whl", "url": "https://ant.example" + prefix + "a/Flask_Login-0.6.3-py3-none-any.whl", "hashes": map[string]string{"sha256": hx(wheel)}},
		{"filename": "Flask-Login-0.6.3.tar.gz", "url": "https://ant.example" + prefix + "b/Flask-Login-0.6.3.tar.gz", "hashes": map[string]string{"sha256": hx(sdist)}},
		{"filename": "Flask_Login-0.6.3-cp312-cp312-manylinux_2_17_x86_64.whl", "url": "https://ant.example" + prefix + "c/native.whl", "hashes": map[string]string{"sha256": hx(native)}},
		{"filename": "Flask_Login-0.6.2-py3-none-any.whl", "url": "https://ant.example" + prefix + "d/old.whl", "hashes": map[string]string{"sha256": hx(wheel)}},
	}})
	r.body["/feeds/feed/pypi/simple/flask-login/"] = page
	r.body[prefix+"a/Flask_Login-0.6.3-py3-none-any.whl"] = wheel
	r.body[prefix+"b/Flask-Login-0.6.3.tar.gz"] = sdist
	r.body[prefix+"c/native.whl"] = native
	it := Item{Name: "flask-login", Version: "0.6.3"}
	asked := func() string {
		got := r.asked()
		var files []string
		for _, p := range got {
			if !strings.Contains(p, "/simple/") {
				files = append(files, p[strings.LastIndex(p, "/")+1:])
			}
		}
		r.paths = nil
		sort.Strings(files)
		return strings.Join(files, " ")
	}
	// default: universal wheels and sdists
	if _, err := warm(r, "pypi", Options{}, it); err != nil {
		t.Fatal(err)
	}
	if got := asked(); got != "Flask-Login-0.6.3.tar.gz Flask_Login-0.6.3-py3-none-any.whl" {
		t.Fatalf("default selection: %s", got)
	}
	if r.accept["/feeds/feed/pypi/simple/flask-login/"] != pypiJSON {
		t.Fatalf("accept %q", r.accept["/feeds/feed/pypi/simple/flask-login/"])
	}
	// file filters
	if _, err := warm(r, "pypi", Options{FileFilters: []string{"manylinux"}}, it); err != nil {
		t.Fatal(err)
	}
	if got := asked(); got != "native.whl" {
		t.Fatalf("filter selection: %s", got)
	}
	// pinned hashes win over filters
	pinned := it
	pinned.Integrity = "sha256:" + hx(native)
	if _, err := warm(r, "pypi", Options{FileFilters: []string{"none-any"}}, pinned); err != nil {
		t.Fatal(err)
	}
	if got := asked(); got != "native.whl" {
		t.Fatalf("pinned selection: %s", got)
	}
	// pinned hash that the page does not list, and a version without files
	pinned.Integrity = "sha256:" + strings.Repeat("1", 64)
	_, err := warm(r, "pypi", Options{}, pinned)
	var ie *ItemError
	if !errors.As(err, &ie) || ie.Code != ErrNoMatching || !ie.Skip {
		t.Fatalf("unpinned file: %v", err)
	}
	if _, err = warm(r, "pypi", Options{}, Item{Name: "flask-login", Version: "9.9.9"}); !errors.As(err, &ie) || !ie.Skip {
		t.Fatalf("missing version: %v", err)
	}
	// corrupt bytes against the listed hash
	r.body[prefix+"a/Flask_Login-0.6.3-py3-none-any.whl"] = []byte("corrupt")
	if _, err = warm(r, "pypi", Options{}, it); code(err) != ErrIntegrity {
		t.Fatalf("corrupt file: %v", err)
	}
	// a file location outside the feed is refused
	r.body["/feeds/feed/pypi/simple/evil/"] = []byte(`{"files":[{"filename":"evil-1.0.0.tar.gz","url":"https://elsewhere.example/evil-1.0.0.tar.gz","hashes":{}}]}`)
	if _, err = warm(r, "pypi", Options{}, Item{Name: "evil", Version: "1.0.0"}); code(err) != ErrUnavail {
		t.Fatalf("foreign location: %v", err)
	}
}

func TestWarmMaven(t *testing.T) {
	r := newRecorder()
	dir := "/feeds/feed/maven/org/apache/commons/commons-lang3/3.14.0/"
	r.body[dir+"commons-lang3-3.14.0.pom"] = []byte("pom")
	r.body[dir+"commons-lang3-3.14.0.jar"] = []byte("jar")
	r.body[dir+"commons-lang3-3.14.0-sources.jar"] = []byte("src")
	if _, err := warm(r, "maven", Options{}, Item{Name: "org.apache.commons:commons-lang3", Version: "3.14.0"}); err != nil {
		t.Fatal(err)
	}
	if got := r.asked(); strings.Join(got, " ") != dir+"commons-lang3-3.14.0.jar "+dir+"commons-lang3-3.14.0.pom" {
		t.Fatalf("%v", got)
	}
	if _, err := warm(r, "maven", Options{}, Item{Name: "org.apache.commons:commons-lang3:jar:sources", Version: "3.14.0"}); err != nil {
		t.Fatal(err)
	}
	// a bom has a pom and no main file
	bom := "/feeds/feed/maven/org/x/bom/1.0/"
	r.body[bom+"bom-1.0.pom"] = []byte("pom")
	if _, err := warm(r, "maven", Options{}, Item{Name: "org.x:bom:pom", Version: "1.0"}); err != nil {
		t.Fatal(err)
	}
	r.body[bom+"gone-1.0.pom"] = nil
	if _, err := warm(r, "maven", Options{}, Item{Name: "org.x:gone", Version: "1.0"}); code(err) != ErrNotFound {
		t.Fatalf("missing pom: %v", err)
	}
}

func TestWarmRaw(t *testing.T) {
	r := newRecorder()
	r.body["/feeds/feed/raw/dir/file one.bin"] = []byte("data")
	if _, err := warm(r, "raw", Options{}, Item{Name: "dir/file one.bin"}); err != nil {
		t.Fatal(err)
	}
}

func digestOf(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

func TestWarmImagePlatformsAndDigests(t *testing.T) {
	r := newRecorder()
	config, layerA, layerB := []byte(`{"os":"linux"}`), []byte("layer-a"), []byte("layer-b")
	amd := mustJSON(map[string]any{"schemaVersion": 2, "config": desc(config), "layers": []any{desc(layerA), desc(layerB)}})
	arm := mustJSON(map[string]any{"schemaVersion": 2, "config": desc(config), "layers": []any{desc([]byte("arm-layer"))}})
	index := mustJSON(map[string]any{"schemaVersion": 2, "manifests": []any{
		map[string]any{"mediaType": "m", "digest": digestOf(amd), "size": len(amd), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
		map[string]any{"mediaType": "m", "digest": digestOf(arm), "size": len(arm), "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
		map[string]any{"mediaType": "m", "digest": digestOf([]byte("att")), "size": 3, "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
	}})
	base := "/v2/feed/library/alpine"
	r.body[base+"/manifests/3.20"] = index
	r.body[base+"/manifests/"+digestOf(amd)] = amd
	r.body[base+"/manifests/"+digestOf(arm)] = arm
	r.body[base+"/blobs/"+digestOf(config)] = config
	r.body[base+"/blobs/"+digestOf(layerA)] = layerA
	r.body[base+"/blobs/"+digestOf(layerB)] = layerB
	r.body[base+"/blobs/"+digestOf([]byte("arm-layer"))] = []byte("arm-layer")
	res, err := warm(r, "oci", Options{}, Item{Name: "library/alpine", Version: "3.20"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.asked(), "\n")
	if strings.Contains(got, digestOf(arm)) || strings.Contains(got, "arm-layer") || strings.Contains(got, digestOf([]byte("att"))) || !strings.Contains(got, digestOf(layerB)) {
		t.Fatalf("amd64 only expected:\n%s", got)
	}
	if res.Bytes() != int64(len(index)+len(amd)+len(config)+len(layerA)+len(layerB)) {
		t.Fatalf("bytes %d", res.Bytes())
	}
	if !strings.Contains(r.accept[base+"/manifests/3.20"], "image.index") {
		t.Fatalf("manifest accept %q", r.accept[base+"/manifests/3.20"])
	}
	r.paths = nil
	if _, err = warm(r, "oci", Options{Platforms: []string{"all"}}, Item{Name: "library/alpine", Version: "3.20"}); err != nil {
		t.Fatal(err)
	}
	if got = strings.Join(r.asked(), "\n"); !strings.Contains(got, digestOf(arm)) || strings.Contains(got, digestOf([]byte("att"))) {
		t.Fatalf("all platforms except attestations:\n%s", got)
	}
	// a blob that does not hash to its descriptor fails
	r.body[base+"/blobs/"+digestOf(layerB)] = []byte("tampered")
	if _, err = warm(r, "oci", Options{}, Item{Name: "library/alpine", Version: "3.20"}); code(err) != ErrIntegrity {
		t.Fatalf("tampered layer: %v", err)
	}
	// pulling by digest verifies the manifest itself
	r.body[base+"/manifests/"+digestOf(amd)] = []byte(`{"layers":[]}`)
	if _, err = warm(r, "oci", Options{}, Item{Name: "library/alpine", Version: digestOf(amd)}); code(err) != ErrIntegrity {
		t.Fatalf("tampered manifest: %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	for status, want := range map[int]string{404: ErrNotFound, 410: ErrNotFound, 401: ErrDenied, 403: ErrDenied, 503: ErrUnavail, 500: ErrUnavail, 504: ErrTimeout} {
		if got := code(statusError(status)); got != want {
			t.Errorf("%d: %s want %s", status, got, want)
		}
	}
	if !Retryable(statusError(503)) || Retryable(statusError(404)) || Retryable(itemErr(ErrIntegrity, "")) || Retryable(errors.New("x")) {
		t.Fatal("retry classification")
	}
}

func TestAbortedResponseIsAFailureNotACrash(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	})
	_, err := (&Warmer{Feeds: h}).getBody(context.Background(), target("npm", Options{}), "/x", "", expectation{}, &Result{}, 0)
	if code(err) != ErrUnavail {
		t.Fatalf("%v", err)
	}
}

func sha512Hex(b []byte) string { return fmt.Sprintf("%x", sha512Sum(b)) }

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func desc(b []byte) map[string]any {
	return map[string]any{"mediaType": "application/octet-stream", "digest": digestOf(b), "size": len(b)}
}
