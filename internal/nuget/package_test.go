package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"testing"
)

func TestNormalization(t *testing.T) {
	for in, want := range map[string]string{"1": "1.0.0", "01.02.003.0+abc": "1.2.3", "1.2.3.4": "1.2.3.4", "1.2.3-RC.12+meta": "1.2.3-rc.12"} {
		got, e := normalizeVersion(in)
		if e != nil || got != want {
			t.Fatalf("%s = %s %v", in, got, e)
		}
	}
	for _, in := range []string{"", "v1.0.0", "1..2", "1.2.3-01", "1.2.3+", "1.2.3.4.5", "2147483648"} {
		if _, e := normalizeVersion(in); e == nil {
			t.Fatal(in)
		}
	}
	if !versionLess("1.0.0-rc.2", "1.0.0-rc.10") || !versionLess("1.0.0-rc.10", "1.0.0") || !versionLess("1.2.9", "1.2.10") {
		t.Fatal("ordering")
	}
}
func inspectBytes(t *testing.T, ctx context.Context, b []byte) error {
	t.Helper()
	f, e := os.CreateTemp(t.TempDir(), "package")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	f.Write(b)
	_, _, e = inspectPackage(ctx, f, int64(len(b)))
	return e
}
func TestArchiveValidation(t *testing.T) {
	b := packageBytes(t, "Example.Widget", "1.0.0", "")
	if e := inspectBytes(t, context.Background(), b); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := inspectBytes(t, ctx, b); e == nil {
		t.Fatal("cancel accepted")
	}
	for _, name := range []string{"../escape", "/absolute", "a\\b", "x:stream", "a/../b"} {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		w, _ := z.Create(name)
		io.WriteString(w, "x")
		z.Close()
		if inspectBytes(t, context.Background(), b.Bytes()) == nil {
			t.Fatal(name)
		}
	}
	var bomb bytes.Buffer
	z := zip.NewWriter(&bomb)
	w, _ := z.Create("bomb")
	w.Write(make([]byte, 2<<20))
	z.Close()
	if inspectBytes(t, context.Background(), bomb.Bytes()) == nil {
		t.Fatal("zip bomb")
	}
	for _, xml := range []string{`<!DOCTYPE package [<!ENTITY x SYSTEM "file:///etc/passwd">]><package><metadata><id>x</id><version>1</version></metadata></package>`, `<package><metadata><id>x</id><id>y</id><version>1</version></metadata></package>`, `<package><metadata><id>x</id><version>1</version></metadata></package><package/>`} {
		if _, e := parseManifest([]byte(xml)); e == nil {
			t.Fatal("invalid XML accepted", xml)
		}
	}
}
func TestPublicURL(t *testing.T) {
	s := &Repository{}
	for _, u := range []string{"https://packages.example", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if e := s.SetPublicURL(u); e != nil {
			t.Fatal(e)
		}
	}
	for _, u := range []string{"http://localhost:1234", "http://packages.example", "https://u:p@host", "https://host/path", "https://host?x", "https://host#x", "//host"} {
		if e := s.SetPublicURL(u); e == nil {
			t.Fatal(u)
		}
	}
	w := httptest.NewRecorder()
	writeJSON(w, httptest.NewRequest("HEAD", "/", nil), map[string]string{"x": "y"})
	if w.Body.Len() != 0 {
		t.Fatal("HEAD body")
	}
}

func TestManifestWithUTF8BOM(t *testing.T) {
	spec := "\xEF\xBB\xBF<?xml version=\"1.0\" encoding=\"utf-8\"?><package><metadata><id>Kc.Demo</id><version>1.0.0</version><authors>a</authors><description>d</description></metadata></package>"
	m, e := parseManifest([]byte(spec))
	if e != nil || m.ID != "kc.demo" || m.Version != "1.0.0" {
		t.Fatalf("%+v %v", m, e)
	}
}
