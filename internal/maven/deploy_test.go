package maven

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
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

// Staging lives in PostgreSQL: the PUTs of one deploy may reach different
// replicas, and a restart between them loses nothing.
func TestMavenDeployAcrossReplicasPG(t *testing.T) {
	f := setup(t)
	// A multi-chunk jar (stored, incompressible) so chunk reassembly is covered.
	var jb bytes.Buffer
	z := zip.NewWriter(&jb)
	w, err := z.CreateHeader(&zip.FileHeader{Name: "blob.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 2*chunkSize+12345)
	if _, err = rand.Read(big); err != nil {
		t.Fatal(err)
	}
	w.Write(big)
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	j := jb.Bytes()
	p := pom("org.example", "demo", "7.0", "jar")
	dir := basePath + "7.0/"
	replica := func() fixture { // a "restarted" or second replica sharing only the database
		r, err := New(f.pool, f.service)
		if err != nil {
			t.Fatal(err)
		}
		g := f
		g.registry = r
		return g
	}
	a, b, c := replica(), replica(), replica()
	assertCode(t, a.request("PUT", dir+"demo-7.0.jar", j, nil), 201)
	assertCode(t, b.request("PUT", dir+"demo-7.0.pom", p, nil), 201)
	assertCode(t, c.request("PUT", dir+"demo-7.0.jar.sha1", sum1(j), nil), 201)
	assertCode(t, replica().request("PUT", dir+"demo-7.0.jar.sha1", []byte("0000000000000000000000000000000000000000"), nil), 400)
	assertCode(t, a.request("GET", dir+"demo-7.0.jar", nil, nil), 404)
	assertCode(t, c.request("PUT", basePath+"maven-metadata.xml", []byte("<metadata/>"), nil), 201)
	got := b.request("GET", dir+"demo-7.0.jar", nil, nil)
	assertCode(t, got, 200)
	if !bytes.Equal(got.Body.Bytes(), j) {
		t.Fatal("jar changed across replicas")
	}
	var left int
	if err = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM maven_deploy_sets)+(SELECT count(*) FROM maven_deploy_files)+(SELECT count(*) FROM maven_deploy_chunks)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("staging left behind: %d %v", left, err)
	}

	// An abandoned deploy expires and is purged by the next deploy request.
	assertCode(t, a.request("PUT", basePath+"8.0/demo-8.0.pom", pom("org.example", "demo", "8.0", "jar"), nil), 201)
	if _, err = f.pool.Exec(context.Background(), `UPDATE maven_deploy_sets SET updated_at=now()-interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	assertCode(t, b.request("PUT", basePath+"maven-metadata.xml", []byte("<metadata/>"), nil), 201)
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM maven_deploy_sets`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("expired staging kept: %d %v", left, err)
	}
	assertCode(t, a.request("GET", basePath+"8.0/demo-8.0.pom", nil, nil), 404)
}
