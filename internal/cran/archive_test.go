package cran

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"
)

func packageBytes(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	tr := tar.NewWriter(g)
	files := map[string]string{"DESCRIPTION": "Package: " + name + "\nVersion: " + version + "\nTitle: Fixture\nDescription: Test package " + extra + "\nLicense: MIT\nAuthor: Test Author\nMaintainer: Test Author <test@example.org>\nNeedsCompilation: no\n", "NAMESPACE": "export(hello)\n", "R/hello.R": "hello <- function() 42\n"}
	for _, p := range []string{"DESCRIPTION", "NAMESPACE", "R/hello.R"} {
		s := files[p]
		if e := tr.WriteHeader(&tar.Header{Name: name + "/" + p, Mode: 0600, Size: int64(len(s))}); e != nil {
			t.Fatal(e)
		}
		_, _ = tr.Write([]byte(s))
	}
	_ = tr.Close()
	_ = g.Close()
	return b.Bytes()
}
func TestArchive(t *testing.T) {
	b := packageBytes(t, "demo", "1.0", "")
	if _, e := inspect(context.Background(), bytes.NewReader(b), "demo", "1.0"); e != nil {
		t.Fatal(e)
	}
	for _, bad := range [][]byte{b[:len(b)-3], []byte("bad")} {
		if _, e := inspect(context.Background(), bytes.NewReader(bad), "demo", "1.0"); e == nil {
			t.Fatal("corrupt accepted")
		}
	}
	if _, e := inspect(context.Background(), bytes.NewReader(b), "other", "1.0"); e == nil {
		t.Fatal("identity accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := inspect(ctx, bytes.NewReader(b), "demo", "1.0"); e == nil {
		t.Fatal("canceled")
	}
}
func TestDCF(t *testing.T) {
	base := "Package: demo\nVersion: 1.0\nTitle: Demo\nDescription: test\nLicense: MIT\n"
	for _, tail := range []string{"Built: R 4.0; unix\n", "Package: other\n", "\nInjected: field\n", "NeedsCompilation: perhaps\n"} {
		if _, e := parseDCF([]byte(base + tail)); e == nil {
			t.Fatal(tail)
		}
	}
	if !newer("1.10", "1.9") || newer("1.0", "2.0") {
		t.Fatal("R version ordering")
	}
}
func TestPublicURL(t *testing.T) {
	s := &Repository{}
	for _, u := range []string{"http://example.org", "http://localhost", "https://u:p@example.org", "https://example.org/path"} {
		if s.SetPublicURL(u) == nil {
			t.Fatal(u)
		}
	}
	for _, u := range []string{"https://example.org", "http://127.0.0.1:8080"} {
		if e := s.SetPublicURL(u); e != nil {
			t.Fatal(e)
		}
	}
}
