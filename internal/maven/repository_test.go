package maven

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func pom(group, name, version, packaging string) []byte {
	return []byte(`<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>` + group + `</groupId><artifactId>` + name + `</artifactId><version>` + version + `</version><packaging>` + packaging + `</packaging></project>`)
}
func jar(t *testing.T, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, e := z.Create(name)
	if e != nil {
		t.Fatal(e)
	}
	f.Write([]byte("fixture content"))
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestPaths(t *testing.T) {
	for _, path := range []string{"/feeds/releases/maven/org/example/demo/1.0/demo-1.0.pom", "/feeds/releases/maven/org/example/demo/1.0/demo-1.0-sources.jar.sha1", "/feeds/releases/maven/org/example/demo/maven-metadata.xml.md5", "/feeds/releases/maven/org/example/demo/1.0/_bundle"} {
		l, e := parsePath(httptest.NewRequest("GET", path, nil))
		if e != nil || l.group != "org.example" || l.name != "demo" {
			t.Fatalf("%s: %+v %v", path, l, e)
		}
	}
	for _, path := range []string{"/feeds/releases/maven/org/example/demo/1.0/other-1.0.jar", "/feeds/releases/maven/org/example/demo/../demo-1.0.jar", "/feeds/releases/maven/org%2fexample/demo/1.0/demo-1.0.jar", "/feeds/releases/maven/org.example/demo/1.0/demo-1.0.jar", "/feeds/releases/maven/org/example/demo/1.0/demo-1.0.jar/", "/feeds/releases/maven/org/example/demo/1.0-SNAPSHOT/demo-1.0-SNAPSHOT.jar"} {
		if _, e := parsePath(httptest.NewRequest("GET", path, nil)); e == nil {
			t.Errorf("accepted %s", path)
		}
	}
}
func TestPOMValidation(t *testing.T) {
	l := location{group: "org.example", name: "demo", version: "1.0"}
	good := pom(l.group, l.name, l.version, "jar")
	if p, e := validatePOM(good, l); e != nil || p != "jar" {
		t.Fatal(p, e)
	}
	for _, bad := range [][]byte{pom("other", l.name, l.version, "jar"), pom(l.group, l.name, l.version, "war"), []byte(`<!DOCTYPE project [<!ENTITY x SYSTEM "file:///etc/passwd">]>` + string(good)), []byte(strings.Replace(string(good), "</project>", "<version>1.0</version></project>", 1)), append(good, good...), []byte(strings.Replace(string(good), "org.example", "&x;", 1)), bytes.Repeat([]byte("x"), maxPOM+1)} {
		if _, e := validatePOM(bad, l); e == nil {
			t.Fatal("accepted invalid POM")
		}
	}
}
func TestJarValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{{"META-INF/MANIFEST.MF", true}, {"../bad", false}, {"/absolute", false}, {"a/../bad", false}} {
		f, e := os.CreateTemp(t.TempDir(), "jar")
		if e != nil {
			t.Fatal(e)
		}
		b := jar(t, tc.name)
		f.Write(b)
		e = validateJAR(context.Background(), f, int64(len(b)))
		if (e == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.name, e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if e = validateJAR(ctx, f, int64(len(b))); !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
		f.Close()
	}
}
func TestRangeReader(t *testing.T) {
	data := "abcdefgh"
	r := rangeReader{size: 8, open: func(n int64) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data[n:])), nil }}
	defer r.Close()
	if _, e := r.Seek(-3, io.SeekEnd); e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(&r)
	if e != nil || string(b) != "fgh" {
		t.Fatal(string(b), e)
	}
	if _, e = r.Seek(-1, io.SeekStart); e == nil {
		t.Fatal("negative seek")
	}
}
