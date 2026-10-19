package maven

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

func sum1(b []byte) []byte { s := sha1.Sum(b); return []byte(hex.EncodeToString(s[:])) }

// A native `mvn deploy` PUTs every file separately: artifacts, their checksum
// files, then the artifact level maven-metadata.xml, which completes the version.
func TestNativeMavenDeployPUTPG(t *testing.T) {
	f := setup(t)
	j := jar(t, "META-INF/MANIFEST.MF")
	p := pom("org.example", "demo", "4.0", "jar")
	dir := basePath + "4.0/"
	put := func(path string, body []byte, want int) {
		t.Helper()
		assertCode(t, f.request("PUT", path, body, nil), want)
	}
	deploy := func(j []byte, want int) {
		t.Helper()
		put(dir+"demo-4.0.jar", j, 201)
		put(dir+"demo-4.0.jar.sha1", sum1(j), 201)
		put(dir+"demo-4.0.jar.md5", []byte("ignored-by-design"), 400)
		put(dir+"demo-4.0.pom", p, 201)
		put(dir+"demo-4.0.pom.sha1", sum1(p), 201)
		// Nothing is visible until the metadata document completes the deploy.
		if want != 204 {
			assertCode(t, f.request("GET", dir+"demo-4.0.jar", nil, nil), 404)
		}
		put(basePath+"maven-metadata.xml", []byte("<metadata/>"), want)
		put(basePath+"maven-metadata.xml.sha1", sum1([]byte("<metadata/>")), 201)
	}
	deploy(j, 201)
	r := f.request("GET", dir+"demo-4.0.jar", nil, nil)
	assertCode(t, r, 200)
	if !bytes.Equal(r.Body.Bytes(), j) {
		t.Fatal("jar changed")
	}
	assertCode(t, f.request("GET", dir+"demo-4.0.pom.sha1", nil, nil), 200)
	if m := f.request("GET", basePath+"maven-metadata.xml", nil, nil); m.Code != 200 || !strings.Contains(m.Body.String(), "<version>4.0</version>") {
		t.Fatalf("%d %s", m.Code, m.Body.String())
	}
	// Redeploying identical bytes is a replay; different bytes conflict.
	deploy(j, 204)
	put(dir+"demo-4.0.jar", jar(t, "changed"), 201)
	put(dir+"demo-4.0.pom", p, 201)
	put(basePath+"maven-metadata.xml", []byte("<metadata/>"), 409)
	// A deploy missing its main jar never publishes, and an invalid POM is refused at upload.
	put(basePath+"5.0/demo-5.0.pom", pom("org.example", "demo", "5.0", "jar"), 201)
	put(basePath+"maven-metadata.xml", []byte("<metadata/>"), 400)
	assertCode(t, f.request("GET", basePath+"5.0/demo-5.0.pom", nil, nil), 404)
	put(basePath+"6.0/demo-6.0.pom", pom("org.example", "other", "6.0", "jar"), 400)
	put(basePath+"6.0/demo-6.0.jar", []byte("not a jar"), 400)
}
