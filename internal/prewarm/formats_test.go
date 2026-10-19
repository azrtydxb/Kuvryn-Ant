package prewarm

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

func TestValidateItems(t *testing.T) {
	cases := []struct {
		format, kind string
		in           []Item
		ok           bool
	}{
		{"npm", KindPackages, []Item{{Name: "left-pad", Version: "1.3.0"}, {Name: "@scope/pkg", Version: "2.0.0"}}, true},
		{"npm", KindPackages, []Item{{Name: "../etc/passwd", Version: "1.0.0"}}, false},
		{"npm", KindPackages, []Item{{Name: "left-pad"}}, false},
		{"go", KindPackages, []Item{{Name: "github.com/Azure/azure-sdk", Version: "v1.0.0"}}, true},
		{"go", KindPackages, []Item{{Name: "github.com/a/b", Version: "1.0.0"}}, false},
		{"go", KindPackages, []Item{{Name: "a/../b", Version: "v1.0.0"}}, false},
		{"cargo", KindPackages, []Item{{Name: "serde", Version: "1.0.0"}}, true},
		{"cargo", KindPackages, []Item{{Name: "a/b", Version: "1.0.0"}}, false},
		{"pypi", KindPackages, []Item{{Name: "Flask_Login", Version: "0.6.3"}}, true},
		{"maven", KindPackages, []Item{{Name: "org.apache:commons:jar:sources", Version: "1.0"}}, true},
		{"maven", KindPackages, []Item{{Name: "org.apache:commons", Version: "1.0-SNAPSHOT"}}, false},
		{"maven", KindPackages, []Item{{Name: "justone", Version: "1.0"}}, false},
		{"raw", KindPackages, []Item{{Name: "dir/file.bin"}}, true},
		{"raw", KindPackages, []Item{{Name: "/abs"}}, false},
		{"raw", KindPackages, []Item{{Name: "a/../b"}}, false},
		{"oci", KindImages, []Item{{Name: "library/alpine", Version: "3.20"}, {Name: "x/y", Version: "sha256:" + string(make([]byte, 0)) + "0000000000000000000000000000000000000000000000000000000000000000"}}, true},
		{"oci", KindImages, []Item{{Name: "UPPER", Version: "1"}}, false},
		{"oci", KindPackages, []Item{{Name: "x", Version: "1"}}, false},
		{"npm", KindPackages, nil, false},
		{"rpm", KindPackages, []Item{{Name: "x", Version: "1"}}, false},
	}
	for i, tc := range cases {
		_, err := ValidateItems(tc.format, tc.kind, tc.in)
		if tc.kind == KindPackages && tc.format == "oci" {
			err = CheckKind(tc.format, tc.kind, "")
		}
		if (err == nil) != tc.ok {
			t.Errorf("case %d (%s %v): err=%v want ok=%v", i, tc.format, tc.in, err, tc.ok)
		}
	}
	items, err := ValidateItems("npm", KindPackages, []Item{{Name: "a", Version: "1.0.0"}, {Name: "a", Version: "1.0.0"}})
	if err != nil || len(items) != 1 {
		t.Fatalf("duplicates kept: %+v %v", items, err)
	}
}

func TestCheckKind(t *testing.T) {
	if err := CheckKind("npm", KindLockfile, "package-lock.json"); err != nil {
		t.Fatal(err)
	}
	if err := CheckKind("npm", KindLockfile, "go.sum"); err == nil {
		t.Fatal("go.sum accepted for npm")
	}
	if err := CheckKind("oci", KindFeed, ""); err == nil {
		t.Fatal("feed source accepted for oci")
	}
	if err := CheckKind("helm", KindPackages, ""); err == nil {
		t.Fatal("unsupported format accepted")
	}
	if len(Formats()) != len(SupportedFormats()) {
		t.Fatal("format listings disagree")
	}
}

func TestGoHashMatchesDirhash(t *testing.T) {
	content := []byte("module example.com/m\n\ngo 1.21\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return os.Open(filepath.Join(dir, "go.mod")) })
	if err != nil {
		t.Fatal(err)
	}
	if got := goHashOfFile(content); got != want {
		t.Fatalf("%s != %s", got, want)
	}
	if goEscape("github.com/Azure/Go") != "github.com/!azure/!go" {
		t.Fatal("escape")
	}
}

func TestParseIntegrity(t *testing.T) {
	e := parseIntegrity("sha512-" + b64(64) + " sha1-" + b64(20))
	if len(e.sha512) != 1 || len(e.sha1) != 0 {
		t.Fatalf("%+v", e)
	}
	e = parseIntegrity("sha1-" + b64(20))
	if len(e.sha1) != 1 {
		t.Fatalf("%+v", e)
	}
	e = parseIntegrity("sha256:" + string(make([]byte, 0)) + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,sha256:bad")
	if len(e.sha256) != 1 {
		t.Fatalf("%+v", e)
	}
	if !parseIntegrity("garbage").empty() {
		t.Fatal("garbage accepted")
	}
}

func TestPyPIVersionOf(t *testing.T) {
	cases := []struct {
		project, file, version string
		wheel, ok              bool
	}{
		{"flask-login", "Flask_Login-0.6.3-py3-none-any.whl", "0.6.3", true, true},
		{"flask-login", "Flask-Login-0.6.3.tar.gz", "0.6.3", false, true},
		{"numpy", "numpy-1.26.0-cp312-cp312-manylinux_2_17_x86_64.whl", "1.26.0", true, true},
		{"numpy", "scipy-1.0.0.tar.gz", "", false, false},
		{"numpy", "numpy-1.0.egg", "", false, false},
	}
	for _, c := range cases {
		v, wheel, ok := pypiVersionOf(c.project, c.file)
		if v != c.version || wheel != c.wheel || ok != c.ok {
			t.Errorf("%s: %q %v %v", c.file, v, wheel, ok)
		}
	}
}

func TestParseImageRef(t *testing.T) {
	cases := map[string]Item{
		"library/alpine":               {Name: "library/alpine", Version: "latest"},
		"library/alpine:3.20":          {Name: "library/alpine", Version: "3.20"},
		"a/b@sha256:abc":               {Name: "a/b", Version: "sha256:abc"},
		"registry:5000/team/app:1.0.0": {Name: "registry:5000/team/app", Version: "1.0.0"},
	}
	for in, want := range cases {
		if got := ParseImageRef(in); got != want {
			t.Errorf("%s: %+v", in, got)
		}
	}
}

func TestPlatformWanted(t *testing.T) {
	d := func(os, arch, variant string) ociDescriptor {
		x := ociDescriptor{}
		x.Platform = &struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		}{os, arch, variant}
		return x
	}
	if !platformWanted(d("linux", "amd64", ""), []string{"linux/amd64"}) || platformWanted(d("linux", "arm64", ""), []string{"linux/amd64"}) {
		t.Fatal("exact platform")
	}
	if !platformWanted(d("linux", "arm", "v7"), []string{"linux/arm/v7"}) || platformWanted(d("linux", "arm", "v6"), []string{"linux/arm/v7"}) {
		t.Fatal("variant")
	}
	if platformWanted(d("unknown", "unknown", ""), []string{"all"}) || !platformWanted(d("windows", "amd64", ""), []string{"all"}) {
		t.Fatal("all excludes attestations only")
	}
	if platformWanted(ociDescriptor{}, []string{"all"}) {
		t.Fatal("descriptor without platform")
	}
}
