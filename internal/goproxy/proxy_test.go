package goproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
)

func TestRouteEscaping(t *testing.T) {
	r, err := parseRoute("/feeds/private/go/example.com/!acme/!widget/@v/v1.2.3.info")
	if err != nil || r.module != "example.com/Acme/Widget" || r.version != "v1.2.3" || r.kind != "info" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, p := range []string{
		"/feeds/x/go/example.com/Acme/@v/list",
		"/feeds/x/go/example.com/!/@v/list",
		"/feeds/x/go/example.com/a/../b/@v/list",
		"/feeds/x/go/example.com/a/@v/v2.0.0.zip",
		"/feeds/x/go/example.com/a/@v/v1.2.info",
		"/feeds/x/go/example.com/a/@v/master.info",
		"/feeds/x/go/example.com/a/@v/v1.2.3+build.info",
	} {
		if _, err := parseRoute(p); err == nil {
			t.Error("accepted invalid path", p)
		}
	}
}
func TestLatest(t *testing.T) {
	cases := []struct {
		versions []string
		want     string
	}{
		{[]string{"v1.0.0", "v1.9.0-rc.1", "v1.8.0"}, "v1.8.0"},
		{[]string{"v1.0.0-beta.1", "v1.0.0-rc.1"}, "v1.0.0-rc.1"},
		{[]string{"v1.9.1-0.20240101000000-abcdefabcdef", "v1.0.1-0.20250101000000-abcdefabcdef"}, "v1.0.1-0.20250101000000-abcdefabcdef"},
	}
	for _, c := range cases {
		if got := latest(c.versions); got != c.want {
			t.Fatalf("latest %q want %q", got, c.want)
		}
	}
}
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for name, data := range files {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(w, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func fixtureBundle(t *testing.T, name, version string, files map[string]string) *bundle {
	t.Helper()
	mod := []byte("module " + name + "\n\ngo 1.23.0\n")
	if files == nil {
		files = map[string]string{name + "@" + version + "/go.mod": string(mod), name + "@" + version + "/hello.go": "package hello\nconst Message = \"hello\"\n"}
	}
	f, err := os.CreateTemp(t.TempDir(), "bundle-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(makeZip(t, files)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	info, _ := json.Marshal(versionInfo{Version: version, Time: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)})
	return &bundle{info: info, mod: mod, zip: f}
}
func TestValidateBundle(t *testing.T) {
	const name = "example.com/Acme/Widget"
	const version = "v1.0.0"
	m := module.Version{Path: name, Version: version}
	b := fixtureBundle(t, name, version, nil)
	if err := validateBundle(context.Background(), m, b); err != nil {
		t.Fatal(err)
	}
	prefix := name + "@" + version + "/"
	for _, files := range []map[string]string{
		{prefix + "../escape": "bad"},
		{"example.com/wrong@v1.0.0/go.mod": string(b.mod)},
		{prefix + "go.mod": "module example.com/wrong\n"},
		{prefix + "dir/go.mod": string(b.mod)},
		{prefix + "a.go": "package a", prefix + "A.go": "package a"},
	} {
		bad := fixtureBundle(t, name, version, files)
		if err := validateBundle(context.Background(), m, bad); err == nil {
			t.Error("accepted invalid zip", files)
		}
	}
	b.mod = []byte("module example.com/wrong\n")
	if err := validateBundle(context.Background(), m, b); err == nil {
		t.Fatal("accepted wrong module path")
	}
}
func multipartBundle(t *testing.T, b *bundle, omit string) ([]byte, string) {
	t.Helper()
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	for _, kind := range []string{"info", "mod", "zip"} {
		if kind == omit {
			continue
		}
		part, err := w.CreateFormFile(kind, "module."+kind)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "info":
			_, err = part.Write(b.info)
		case "mod":
			_, err = part.Write(b.mod)
		case "zip":
			_, err = b.zip.Seek(0, io.SeekStart)
			if err == nil {
				_, err = io.Copy(part, b.zip)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), w.FormDataContentType()
}
func TestMissingPart(t *testing.T) {
	b := fixtureBundle(t, "example.com/test", "v1.0.0", nil)
	body, contentType := multipartBundle(t, b, "mod")
	r := httptest.NewRequest("PUT", "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	if b, err := readBundle(httptest.NewRecorder(), r); err == nil {
		b.close()
		t.Fatal("accepted incomplete bundle")
	}
}
func TestBoundedError(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, io.ErrUnexpectedEOF)
	if w.Code != 503 || strings.Contains(w.Body.String(), "EOF") || w.Body.Len() > 100 {
		t.Fatal(w.Body.String())
	}
}

func TestZipCRCValidation(t *testing.T) {
	const name = "example.com/test"
	b := fixtureBundle(t, name, "v1.0.0", nil)
	raw, err := os.ReadFile(b.zip.Name())
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the central-directory CRC, not compressed padding bits that may
	// legitimately leave the decoded content unchanged.
	central := bytes.Index(raw, []byte{'P', 'K', 1, 2})
	if central < 0 {
		t.Fatal("missing central directory")
	}
	raw[central+16] ^= 0x80
	if err = os.WriteFile(b.zip.Name(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = validateBundle(context.Background(), module.Version{Path: name, Version: "v1.0.0"}, b); err == nil {
		t.Fatal("corrupt zip accepted")
	}
}
