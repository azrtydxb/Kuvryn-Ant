package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"testing"
)

func inspectBytes(t *testing.T, ctx context.Context, file string, b []byte) error {
	t.Helper()
	f, e := os.CreateTemp(t.TempDir(), "dist")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	f.Write(b)
	_, _, e = inspect(ctx, f, file)
	return e
}
func TestNamesAndFilenames(t *testing.T) {
	if normalize("Demo...__PKG") != "demo-pkg" {
		t.Fatal("normalization")
	}
	for _, s := range []string{"../a-1.0.tar.gz", "demo-1-py3.whl", "demo-1.0-x-py3-none-any.whl", "demo-01x.tar.gz", "a-1.zip", "a-1.0-py3-none-any.whl/"} {
		if _, _, _, e := filename(s); e == nil {
			t.Error("accepted", s)
		}
	}
	for _, s := range []string{"demo_pkg-1.0-py3-none-any.whl", "demo_pkg-1.0-1-py3-none-any.whl", "demo_pkg-1.0.tar.gz", "demo-1!2.0rc1.post2.dev3+local.1.tar.gz"} {
		if _, _, _, e := filename(s); e != nil {
			t.Error(s, e)
		}
	}
}
func TestArchiveValidation(t *testing.T) {
	file := "demo_pkg-1.0-py3-none-any.whl"
	for _, tc := range []struct {
		file  string
		data  []byte
		valid bool
	}{{file, wheel(t, "demo-pkg", "1.0", ""), true}, {"demo_pkg-1.0.tar.gz", sdist(t), true}, {file, wheel(t, "other", "1.0", ""), false}, {file, wheel(t, "demo-pkg", "1.0", "../evil"), false}, {file, wheel(t, "demo-pkg", "1.0", "/absolute"), false}, {file, wheel(t, "demo-pkg", "1.0", "x\\evil"), false}, {file, []byte("bad zip"), false}} {
		e := inspectBytes(t, context.Background(), tc.file, tc.data)
		if (e == nil) != tc.valid {
			t.Errorf("valid=%v error=%v", tc.valid, e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if inspectBytes(t, ctx, file, wheel(t, "demo-pkg", "1.0", "")) == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestExpansionAndDuplicateRejected(t *testing.T) {
	for _, duplicate := range []bool{true, false} {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		w, _ := z.Create("x")
		if duplicate {
			w.Write([]byte("x"))
			z.Create("x")
		} else {
			w.Write(bytes.Repeat([]byte("a"), 4<<20))
		}
		z.Close()
		if inspectBytes(t, context.Background(), "demo-1.0-py3-none-any.whl", b.Bytes()) == nil {
			t.Fatal("accepted unsafe archive")
		}
	}
}
func TestNegotiation(t *testing.T) {
	for _, tc := range []struct{ accept, want string }{{"", "text/html"}, {"application/json", ""}, {"application/vnd.pypi.simple.v1+json", "application/vnd.pypi.simple.v1+json"}, {"application/vnd.pypi.simple.v1+json;q=0,text/html", "text/html"}, {"text/html;q=0,*/*;q=0", ""}, {"application/vnd.pypi.simple.v1+html", "application/vnd.pypi.simple.v1+html"}} {
		if got := negotiate(tc.accept); got != tc.want {
			t.Errorf("%s: %s", tc.accept, got)
		}
	}
}
func TestWheelRecordIntegrity(t *testing.T) {
	data := wheel(t, "demo-pkg", "1.0", "")
	reader, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, entry := range reader.File {
		r, e := entry.Open()
		if e != nil {
			t.Fatal(e)
		}
		content, e := boundedRead(r, maxMetadata)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		if entry.Name == "demo_pkg/__init__.py" {
			content = []byte("changed after RECORD was generated\n")
		}
		target, _ := w.Create(entry.Name)
		target.Write(content)
	}
	w.Close()
	if inspectBytes(t, context.Background(), "demo_pkg-1.0-py3-none-any.whl", b.Bytes()) == nil {
		t.Fatal("accepted mismatched RECORD digest")
	}
}

func TestMetadataWithoutBlankLineTerminator(t *testing.T) {
	for _, in := range []string{"Metadata-Version: 2.4\nName: demo\nVersion: 1.0", "Metadata-Version: 2.4\nName: demo\nVersion: 1.0\n", "Metadata-Version: 2.4\nName: demo\nVersion: 1.0\n\nbody"} {
		h, e := parseHeaders([]byte(in))
		if e != nil || h.Get("Name") != "demo" || h.Get("Version") != "1.0" {
			t.Fatalf("%q: %v %v", in, h, e)
		}
	}
}
