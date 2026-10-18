package pubrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"testing"
)

func TestValidation(t *testing.T) {
	for _, v := range []string{"1.0.0", "1.0.0-alpha.1", "1.2.3+build"} {
		if !validVersion(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"1", "1.0", "01.0.0", "v1.0.0", "1.0.0/evil"} {
		if validVersion(v) {
			t.Fatal(v)
		}
	}
	for _, name := range []string{"../pubspec.yaml", "/pubspec.yaml", "pubspec.yaml"} {
		var b bytes.Buffer
		g := gzip.NewWriter(&b)
		tr := tar.NewWriter(g)
		data := []byte("name: bad\nversion: 1.0.0\nx: &a [1]\ny: *a\n")
		tr.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0600})
		tr.Write(data)
		tr.Close()
		g.Close()
		if _, e := inspect(context.Background(), &b); e == nil {
			t.Fatal("accepted unsafe archive", name)
		}
	}
	b := archive(t, "valid_name", "1.0.0", "")
	if _, e := inspect(context.Background(), bytes.NewReader(b)); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := inspect(ctx, bytes.NewReader(b)); e == nil {
		t.Fatal("ignored cancellation")
	}
	b[len(b)-1] ^= 1
	if _, e := inspect(context.Background(), bytes.NewReader(b)); e == nil {
		t.Fatal("accepted corrupt gzip")
	}
}
func TestPublicURL(t *testing.T) {
	r := &Repository{}
	for _, s := range []string{"https://packages.example", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if e := r.SetPublicURL(s); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []string{"http://packages.example", "https://user:pass@example.com", "https://example.com/path", "https://example.com?q=1", "https://example.com#x"} {
		if r.SetPublicURL(s) == nil {
			t.Fatal(s)
		}
	}
}
