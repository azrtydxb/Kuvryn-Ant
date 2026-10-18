package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
)

const validMetadata = `--- !ruby/object:Gem::Specification
name: example
version: !ruby/object:Gem::Version
  version: 1.2.0
platform: ruby
summary: fixture
`

func TestMetadataSafety(t *testing.T) {
	m, e := parseMetadata([]byte(validMetadata))
	if e != nil || m.Name != "example" || m.key() != "1.2.0" {
		t.Fatalf("%+v %v", m, e)
	}
	for _, b := range []string{
		validMetadata + "name: duplicate\n",
		strings.Replace(validMetadata, "!ruby/object:Gem::Version", "!!map", 1),
		strings.Replace(validMetadata, "name: example", "name: ../evil", 1),
		strings.Replace(validMetadata, "platform: ruby", "platform: \"ruby\\nforged\"", 1),
		strings.Replace(validMetadata, "version: 1.2.0", "version: 1.2.0,2.0", 1),
		validMetadata + "metadata: !ruby/object:ERB {}\n",
		validMetadata + "metadata: &x [*x]\n",
		validMetadata + "---\nname: extra\n",
		validMetadata + "dependencies: [bad]\n",
		validMetadata + "required_ruby_version: bad\n",
	} {
		if _, e := parseMetadata([]byte(b)); e == nil {
			t.Fatalf("accepted malformed metadata: %s", b)
		}
	}
}
func TestArchiveBounds(t *testing.T) {
	good := packageBytes(t, "example", "1.0", "ruby", "")
	if _, e := inspectPackage(context.Background(), bytes.NewReader(good)); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := inspectPackage(ctx, bytes.NewReader(good)); e == nil {
		t.Fatal("ignored cancellation")
	}
	// Rebuild the outer tar, replacing the data archive with traversal, symlink,
	// duplicate member, or a corrupt gzip checksum. No extraction ever occurs.
	for _, kind := range []string{"traversal", "symlink", "duplicate", "crc", "checksum"} {
		t.Run(kind, func(t *testing.T) {
			var out bytes.Buffer
			tw := tar.NewWriter(&out)
			tr := tar.NewReader(bytes.NewReader(good))
			for {
				h, e := tr.Next()
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Fatal(e)
				}
				data, _ := io.ReadAll(tr)
				if h.Name == "data.tar.gz" && kind != "checksum" {
					var compressed bytes.Buffer
					g := gzip.NewWriter(&compressed)
					inner := tar.NewWriter(g)
					ih := &tar.Header{Name: "lib/file", Mode: 0644, Size: 1}
					if kind == "traversal" {
						ih.Name = "../escape"
					}
					if kind == "symlink" {
						ih.Typeflag = tar.TypeSymlink
						ih.Linkname = "../../escape"
						ih.Size = 0
					}
					inner.WriteHeader(ih)
					if ih.Size > 0 {
						inner.Write([]byte("x"))
					}
					if kind == "duplicate" {
						inner.WriteHeader(ih)
						inner.Write([]byte("x"))
					}
					inner.Close()
					g.Close()
					data = compressed.Bytes()
					if kind == "crc" {
						data[len(data)-5] ^= 1
					}
				}
				h.Size = int64(len(data))
				tw.WriteHeader(h)
				tw.Write(data)
			}
			if kind == "checksum" {
				var b bytes.Buffer
				g := gzip.NewWriter(&b)
				g.Write([]byte("SHA256:\n  metadata.gz: wrong\n  data.tar.gz: wrong\n"))
				g.Close()
				tw.WriteHeader(&tar.Header{Name: "checksums.yaml.gz", Mode: 0644, Size: int64(b.Len())})
				tw.Write(b.Bytes())
			}
			tw.Close()
			if _, e := inspectPackage(context.Background(), bytes.NewReader(out.Bytes())); e == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
	var bomb bytes.Buffer
	g := gzip.NewWriter(&bomb)
	g.Write(bytes.Repeat([]byte("a"), maxMetadata+1))
	g.Close()
	if _, e := inflate(context.Background(), bomb.Bytes(), maxMetadata); e == nil {
		t.Fatal("accepted expansion")
	}
}
func TestPublicURL(t *testing.T) {
	s := &Repository{}
	for _, u := range []string{"http://packages.example", "https://user:pass@example.org", "https://example.org/path", "https://example.org?", "https://example.org#", "//example.org"} {
		if s.SetPublicURL(u) == nil {
			t.Fatal(u)
		}
	}
	for _, u := range []string{"https://example.org", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if e := s.SetPublicURL(u); e != nil {
			t.Fatal(e)
		}
	}
}
func FuzzMetadata(f *testing.F) {
	f.Add([]byte(validMetadata))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > maxMetadata {
			t.Skip()
		}
		_, _ = parseMetadata(b)
	})
}
