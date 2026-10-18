package cocoapods

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func status(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status %d expected %d: %s", r.Code, want, r.Body.String())
	}
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for n, c := range files {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0644, Size: int64(len(c)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

func spec(name, version string) string {
	return `{"name":"` + name + `","version":"` + version + `","summary":"s","homepage":"https://example.com","license":"MIT","authors":{"a":"a@example.com"},"source":{"git":"https://example.com/x.git","tag":"1"}}`
}

func (f fixture) publish(name, version, podspec string, archive []byte, extra map[string]string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormField("podspec")
	_, _ = w.Write([]byte(podspec))
	for k, v := range extra {
		w, _ = mw.CreateFormField(k)
		_, _ = w.Write([]byte(v))
	}
	w, _ = mw.CreateFormFile("source", "src")
	_, _ = w.Write(archive)
	_ = mw.Close()
	return f.request("PUT", "/feeds/packages/cocoapods/pods/"+name+"/"+version, body.Bytes(), map[string]string{"Content-Type": mw.FormDataContentType()})
}

func TestCocoaPodsPostgresLifecycle(t *testing.T) {
	f := setup(t)
	a := tgz(t, map[string]string{"Foo/Foo.h": "x", "Foo/Foo.m": "y"})
	sum := sha256.Sum256(a)
	digest := hex.EncodeToString(sum[:])
	status(t, f.publish("Foo", "1.0.0", spec("Foo", "1.0.0"), a, map[string]string{"sha256": digest}), 201)
	// Identical retry is idempotent.
	status(t, f.publish("Foo", "1.0.0", spec("Foo", "1.0.0"), a, nil), 201)
	status(t, f.publish("Foo", "1.1.0", spec("Foo", "1.1.0"), tgz(t, map[string]string{"a": "b"}), nil), 201)

	r := f.request("GET", "/feeds/packages/cocoapods/CocoaPods-version.yml", nil, nil)
	status(t, r, 200)
	if !strings.Contains(r.Body.String(), "prefix_lengths") {
		t.Fatal(r.Body.String())
	}
	h := md5.Sum([]byte("Foo"))
	x := hex.EncodeToString(h[:])
	s := [3]string{x[:1], x[1:2], x[2:3]}
	idx := "/feeds/packages/cocoapods/all_pods_versions_" + s[0] + "_" + s[1] + "_" + s[2] + ".txt"
	r = f.request("GET", idx, nil, nil)
	status(t, r, 200)
	if r.Body.String() != "Foo/1.0.0/1.1.0\n" {
		t.Fatal(r.Body.String())
	}
	other := "0_0_0"
	if s == [3]string{"0", "0", "0"} {
		other = "1_1_1"
	}
	r = f.request("GET", "/feeds/packages/cocoapods/all_pods_versions_"+other+".txt", nil, nil)
	status(t, r, 200)
	if r.Body.String() != "" {
		t.Fatal(r.Body.String())
	}
	specPath := func(v string) string {
		return "/feeds/packages/cocoapods/Specs/" + s[0] + "/" + s[1] + "/" + s[2] + "/Foo/" + v + "/Foo.podspec.json"
	}
	r = f.request("GET", specPath("1.0.0"), nil, nil)
	status(t, r, 200)
	var m map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	src := m["source"].(map[string]any)
	if src["sha256"] != digest || src["type"] != "tgz" || src["http"] != "https://packages.example/feeds/packages/cocoapods/archives/Foo/1.0.0/Foo-1.0.0.tar.gz" || src["git"] != nil {
		t.Fatal(src)
	}
	status(t, f.request("GET", "/feeds/packages/cocoapods/Specs/z/z/z/Foo/1.0.0/Foo.podspec.json", nil, nil), 404) // wrong shard
	r = f.request("GET", "/feeds/packages/cocoapods/archives/Foo/1.0.0/Foo-1.0.0.tar.gz", nil, nil)
	status(t, r, 200)
	if !bytes.Equal(r.Body.Bytes(), a) {
		t.Fatal("archive bytes differ")
	}

	// Conflicting bytes for an existing version.
	status(t, f.publish("Foo", "1.0.0", spec("Foo", "1.0.0"), tgz(t, map[string]string{"z": "z"}), nil), 409)
	// Invalid podspec, name/version mismatch, tampered hash, bad archive, traversal.
	status(t, f.publish("Bar", "1.0.0", "{not json", a, nil), 400)
	status(t, f.publish("Bar", "1.0.0", spec("Baz", "1.0.0"), a, nil), 400)
	status(t, f.publish("Bar", "1.0.0", spec("Bar", "2.0.0"), a, nil), 400)
	status(t, f.publish("Bar", "1.0.0", `{"name":"Bar","version":"1.0.0"}`, a, nil), 400)
	status(t, f.publish("Bar", "1.0.0", spec("Bar", "1.0.0"), a, map[string]string{"sha256": strings.Repeat("0", 64)}), 400)
	status(t, f.publish("Bar", "1.0.0", spec("Bar", "1.0.0"), []byte("not an archive"), nil), 400)
	status(t, f.publish("Bar", "1.0.0", spec("Bar", "1.0.0"), tgz(t, map[string]string{"../evil": "x"}), nil), 400)
	status(t, f.request("PUT", "/feeds/packages/cocoapods/pods/Bar/1.0.0", []byte("raw"), map[string]string{"Content-Type": "text/plain"}), 400)
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='Bar'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected publish leaked", count, err)
	}

	// Delete (unlist) hides the version from index, podspec and archive.
	status(t, f.request("DELETE", "/feeds/packages/cocoapods/pods/Foo/1.1.0", nil, nil), 204)
	status(t, f.request("DELETE", "/feeds/packages/cocoapods/pods/Foo/1.1.0", nil, nil), 404)
	r = f.request("GET", idx, nil, nil)
	if r.Body.String() != "Foo/1.0.0\n" {
		t.Fatal(r.Body.String())
	}
	status(t, f.request("GET", "/feeds/packages/cocoapods/archives/Foo/1.1.0/Foo-1.1.0.tar.gz", nil, nil), 404)
	status(t, f.request("GET", specPath("1.1.0"), nil, nil), 404)
	status(t, f.publish("Foo", "1.1.0", spec("Foo", "1.1.0"), tgz(t, map[string]string{"a": "b"}), nil), 409)
}

func TestCocoaPodsPostgresAuthAndIsolation(t *testing.T) {
	f := setup(t)
	req := httptest.NewRequest("GET", "/feeds/packages/cocoapods/CocoaPods-version.yml", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	status(t, f.request("GET", "/feeds/other/cocoapods/CocoaPods-version.yml", nil, nil), 404)
	status(t, f.request("GET", "/feeds/packages/raw/CocoaPods-version.yml", nil, nil), 404)
	status(t, f.request("GET", "/feeds/packages/cocoapods/Specs/a/b/c/..%2Ffoo/1/x.podspec.json", nil, nil), 404)
	// Read-only token cannot publish.
	ro, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.Close()
	req = httptest.NewRequest("PUT", "/feeds/packages/cocoapods/pods/Foo/1.0.0", &body)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth("u", ro.Secret)
	rec = httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	status(t, f.request("PATCH", "/feeds/packages/cocoapods/pods/Foo/1.0.0", nil, nil), 405)
}
