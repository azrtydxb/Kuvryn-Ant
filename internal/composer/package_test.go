package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestManifestValidation(t *testing.T) {
	good := `{"name":"acme/widget","version":"1.2.0","require":{"php":">=8.0","acme/other":"^1.0 || ~2.1"},"source":{"type":"git","url":"https://example.test/repo.git","reference":"abcdef"}}`
	if _, e := parseManifest([]byte(good)); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{
		`{"name":"Acme/widget","version":"1.2.0"}`,
		`{"name":"acme/widget","version":"dev-main"}`,
		`{"name":"acme/widget","name":"acme/other","version":"1.2.0"}`,
		`{"name":"acme/widget","version":"1.2.0","require":{"php":"garbage"}}`,
		`{"name":"acme/widget","version":"1.2.0","require":{"ext-":"*"}}`,
		`{"name":"acme/widget","version":"1.2.0","autoload":{"psr-4":{"Acme\\":"../escape"}}}`,
		`{"name":"acme/widget","version":"1.2.0","scripts":{"install":"curl evil"}}`,
		`{"name":"acme/widget","version":"1.2.0","source":{"type":"git","url":"file:///tmp/repo","reference":"HEAD"}}`,
		good + ` {}`,
	} {
		if _, e := parseManifest([]byte(raw)); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
func inspectBytes(t *testing.T, ctx context.Context, b []byte) error {
	t.Helper()
	f, e := os.CreateTemp(t.TempDir(), "zip")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		t.Fatal(e)
	}
	_, _, e = inspectPackage(ctx, f, int64(len(b)))
	return e
}
func TestArchiveValidation(t *testing.T) {
	if e := inspectBytes(t, context.Background(), packageBytes(t, "acme/widget", "1.2.0", "")); e != nil {
		t.Fatal(e)
	}
	manifest := `{"name":"acme/widget","version":"1.2.0"}`
	cases := []struct {
		name    string
		paths   []string
		symlink bool
	}{
		{"traversal", []string{"composer.json", "../escape"}, false},
		{"absolute", []string{"composer.json", "/etc/file"}, false},
		{"backslash", []string{"composer.json", "a\\b"}, false},
		{"duplicate", []string{"composer.json", "composer.json"}, false},
		{"missing", []string{"readme"}, false},
		{"split-root", []string{"pkg/composer.json", "outside"}, false},
		{"symlink", []string{"composer.json", "link"}, true},
		{"dotdot", []string{"composer.json", ".."}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			for i, p := range tc.paths {
				h := &zip.FileHeader{Name: p}
				if tc.symlink && i == 1 {
					h.SetMode(os.ModeSymlink | 0777)
				}
				w, e := z.CreateHeader(h)
				if e != nil {
					t.Fatal(e)
				}
				w.Write([]byte(manifest))
			}
			z.Close()
			if e := inspectBytes(t, context.Background(), b.Bytes()); e == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := inspectBytes(t, ctx, packageBytes(t, "acme/widget", "1.2.0", "")); e == nil {
		t.Fatal("cancelled archive accepted")
	}
}
func TestPublicURL(t *testing.T) {
	r := &Repository{}
	for _, u := range []string{"http://evil.test", "https://u:p@host", "https://host/path", "https://host?", "https://host#", "https://host\\evil", "http://localhost"} {
		if r.SetPublicURL(u) == nil {
			t.Errorf("accepted %s", u)
		}
	}
	for _, u := range []string{"https://packages.example", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if e := r.SetPublicURL(u); e != nil {
			t.Fatal(e)
		}
	}
}
func FuzzManifest(f *testing.F) {
	f.Add([]byte(`{"name":"acme/widget","version":"1.2.0"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > maxMetadata {
			return
		}
		m, e := parseManifest(b)
		if e == nil {
			encoded, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = parseManifest(encoded); e != nil {
				t.Fatal(e)
			}
		}
	})
}
