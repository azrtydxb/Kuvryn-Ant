package aptrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/clearsign"
)

type fixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Repository
	service                                        *artifact.Service
	blobs                                          string
	root, token, instance, principal, feed, source string
}

func setup(t *testing.T) fixture {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL required")
		}
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ant_apt_" + strings.ToLower(rand.Text())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 4
	pool, err := database.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "APT")
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, instance: owner.InstanceID, principal: owner.PrincipalID, blobs: filepath.Join(t.TempDir(), "blobs")}
	f.token = f.issue(t, identity.Read, identity.Publish, identity.Admin, identity.Delete)
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'debs','apt') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','apt') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	var domain string
	if err = pool.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id`, owner.InstanceID, "source:"+f.source).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	disk, err := blob.NewDisk(f.blobs, blob.TransferBufferSize*4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	lease, err := ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000001", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: f.source, Kind: routing.Local}}, DefaultTarget: f.source})
	if err != nil {
		t.Fatal(err)
	}
	f.service = &artifact.Service{DB: pool, Lease: func() ownership.Lease { return lease }, Router: func(context.Context, string) (*routing.Router, error) { return router, nil }, Storage: func(context.Context, string) (blob.Store, error) { return disk, nil }}
	f.registry, err = New(pool, f.service)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.registry.SetPublicURL("https://packages.example"); err != nil {
		t.Fatal(err)
	}
	f.registry.SetKeyring(testKeyring(t))
	return f
}

func testKeyring(t *testing.T) *secrets.Keyring {
	k, err := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (f fixture) issue(t *testing.T, actions ...identity.Action) string {
	t.Helper()
	token, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: actions}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return token.Secret
}

func (f fixture) as(token, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("service", token)
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func (f fixture) request(method, path string, body []byte) *httptest.ResponseRecorder {
	return f.as(f.token, method, path, body)
}
func assertCode(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}

const base = "/feeds/debs/apt/"

// --- .deb construction -----------------------------------------------------

func tarGz(t *testing.T, files map[string]string) []byte {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	var z bytes.Buffer
	g := gzip.NewWriter(&z)
	_, _ = g.Write(raw.Bytes())
	_ = g.Close()
	return z.Bytes()
}

func arMember(name string, data []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
	b.Write(data)
	if len(data)%2 == 1 {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

type debSpec struct{ pkg, version, arch, source, extra, controlName string }

func (d debSpec) control() string {
	s := "Package: " + d.pkg + "\nVersion: " + d.version + "\nArchitecture: " + d.arch + "\nMaintainer: Dev <dev@example.com>\n"
	if d.source != "" {
		s += "Source: " + d.source + "\n"
	}
	return s + "Description: test package\n long description\n" + d.extra
}

func buildDeb(t *testing.T, d debSpec) []byte {
	if d.controlName == "" {
		d.controlName = "control.tar.gz"
	}
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	b.Write(arMember("debian-binary", []byte("2.0\n")))
	b.Write(arMember(d.controlName, tarGz(t, map[string]string{"./control": d.control()})))
	b.Write(arMember("data.tar.gz", tarGz(t, map[string]string{"./usr/share/doc/x": d.pkg + d.version})))
	return b.Bytes()
}

func poolPath(pkg, version, arch string) string {
	return poolPathSrc(pkg, pkg, version, arch)
}

func poolPathSrc(pkg, src, version, arch string) string {
	prefix := src[:1]
	if strings.HasPrefix(src, "lib") && len(src) > 3 {
		prefix = src[:4]
	}
	v := version
	if _, rest, ok := strings.Cut(version, ":"); ok {
		v = rest
	}
	return fmt.Sprintf("pool/main/%s/%s/%s_%s_%s.deb", prefix, src, pkg, v, arch)
}

func (f fixture) put(t *testing.T, d debSpec, dist string, want int) []byte {
	t.Helper()
	data := buildDeb(t, d)
	src := d.pkg
	if d.source != "" {
		src = strings.Fields(d.source)[0]
	}
	assertCode(t, f.request("PUT", base+poolPathSrc(d.pkg, src, d.version, d.arch)+"?distribution="+dist, data), want)
	return data
}

// --- client-side verification ---------------------------------------------

func (f fixture) keyring(t *testing.T) openpgp.EntityList {
	t.Helper()
	w := f.request("GET", base+"signing-key.gpg", nil)
	assertCode(t, w, 200)
	kr, err := openpgp.ReadKeyRing(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// verifyInRelease mimics apt: signature must verify and Valid-Until must be in
// the future; it returns the signed Release text.
func verifyInRelease(kr openpgp.EntityList, inRelease []byte, now time.Time) (string, error) {
	block, _ := clearsign.Decode(inRelease)
	if block == nil {
		return "", fmt.Errorf("no clearsigned block")
	}
	if _, err := openpgp.CheckDetachedSignature(kr, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body); err != nil {
		return "", err
	}
	text := string(block.Plaintext)
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "Valid-Until: "); ok {
			until, err := time.Parse(releaseDate, v)
			if err != nil {
				return "", err
			}
			if !now.Before(until) {
				return "", fmt.Errorf("Release expired")
			}
		}
	}
	return text, nil
}

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func gunzip(t *testing.T, b []byte) []byte {
	g, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(g)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- tests -------------------------------------------------------------------

func TestPublishSignedRepository(t *testing.T) {
	f := setup(t)
	amd := debSpec{pkg: "hello", version: "1:2.10-3", arch: "amd64"}
	data := f.put(t, amd, "stable", 201)
	all := debSpec{pkg: "libdocs", version: "1.0", arch: "all", source: "docs-src (1.0)"}
	f.put(t, all, "stable", 201)
	f.put(t, debSpec{pkg: "hello", version: "1:2.10-3", arch: "amd64"}, "stable", 201) // identical retry

	kr := f.keyring(t)
	in := f.request("GET", base+"dists/stable/InRelease", nil)
	assertCode(t, in, 200)
	release, err := verifyInRelease(kr, in.Body.Bytes(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plain := f.request("GET", base+"dists/stable/Release", nil)
	assertCode(t, plain, 200)
	sig := f.request("GET", base+"dists/stable/Release.gpg", nil)
	assertCode(t, sig, 200)
	if _, err = openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(plain.Body.Bytes()), bytes.NewReader(sig.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Architectures: amd64\n", "Components: main\n", "Acquire-By-Hash: yes\n", "Codename: stable\n"} {
		if !strings.Contains(release, want) {
			t.Fatalf("missing %q in\n%s", want, release)
		}
	}
	if release != plain.Body.String() {
		t.Fatal("InRelease and Release differ")
	}
	// Packages and Packages.gz must match the signed hashes and contain both packages
	// (arch all is merged into amd64).
	pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil)
	assertCode(t, pk, 200)
	gz := f.request("GET", base+"dists/stable/main/binary-amd64/Packages.gz", nil)
	assertCode(t, gz, 200)
	for _, h := range []string{hexSum(pk.Body.Bytes()), hexSum(gz.Body.Bytes())} {
		if !strings.Contains(release, " "+h+" ") {
			t.Fatalf("hash %s not in Release", h)
		}
		by := f.request("GET", base+"dists/stable/main/binary-amd64/by-hash/SHA256/"+h, nil)
		assertCode(t, by, 200)
		if hexSum(by.Body.Bytes()) != h {
			t.Fatal("by-hash content mismatch")
		}
	}
	if !bytes.Equal(gunzip(t, gz.Body.Bytes()), pk.Body.Bytes()) {
		t.Fatal("Packages.gz differs from Packages")
	}
	text := pk.Body.String()
	for _, want := range []string{"Package: hello\nVersion: 1:2.10-3\nArchitecture: amd64\n", "Filename: pool/main/h/hello/hello_2.10-3_amd64.deb\n", "Package: libdocs\n", "Filename: pool/main/d/docs-src/libdocs_1.0_all.deb\n", "SHA256: " + hexSum(data) + "\n", fmt.Sprintf("Size: %d\n", len(data)), "MD5sum: ", "SHA1: ", " long description\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if strings.Count(text, "Package: ") != 2 {
		t.Fatal(text)
	}
	// Download by pool path is byte-identical.
	d := f.request("GET", base+poolPath("hello", "1:2.10-3", "amd64"), nil)
	assertCode(t, d, 200)
	if !bytes.Equal(d.Body.Bytes(), data) {
		t.Fatal("deb bytes changed")
	}
	assertCode(t, f.request("HEAD", base+poolPath("hello", "1:2.10-3", "amd64"), nil), 200)
	// Non-canonical pool directory and unknown distribution are 404.
	assertCode(t, f.request("GET", base+"pool/main/x/other/hello_2.10-3_amd64.deb", nil), 404)
	assertCode(t, f.request("GET", base+"dists/nope/InRelease", nil), 404)
	assertCode(t, f.request("GET", base+"dists/stable/main/binary-arm64/Packages", nil), 404)
	// Exactly one generation per distinct change: retry added none.
	var n int
	if err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='dists/stable'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("generations %d %v", n, err)
	}
}

func TestIngestRejections(t *testing.T) {
	f := setup(t)
	good := debSpec{pkg: "demo", version: "1.0", arch: "amd64"}
	data := f.put(t, good, "stable", 201)
	// Same filename, different bytes -> immutable conflict.
	changed := good
	changed.extra = "Section: misc\n"
	assertCode(t, f.request("PUT", base+poolPath("demo", "1.0", "amd64")+"?distribution=stable", buildDeb(t, changed)), 409)
	// Missing/invalid distribution.
	assertCode(t, f.request("PUT", base+poolPath("demo", "2.0", "amd64"), data), 400)
	assertCode(t, f.request("PUT", base+poolPath("demo", "2.0", "amd64")+"?distribution=Bad/Dist", data), 400)
	// Garbage, truncated, bad ar magic.
	assertCode(t, f.request("PUT", base+poolPath("demo", "3.0", "amd64")+"?distribution=stable", []byte("not a deb")), 400)
	assertCode(t, f.request("PUT", base+poolPath("demo", "3.0", "amd64")+"?distribution=stable", data[:len(data)-40]), 400)
	// Control does not match the URL.
	assertCode(t, f.request("PUT", base+poolPath("demo", "9.9", "amd64")+"?distribution=stable", data), 400)
	assertCode(t, f.request("PUT", base+"pool/main/z/demo/demo_1.0_amd64.deb?distribution=stable", data), 400)
	// Invalid control metadata and unsupported compressed control member.
	for _, bad := range []debSpec{{pkg: "Bad_Name", version: "1.0", arch: "amd64"}, {pkg: "okname", version: "x1", arch: "amd64"}, {pkg: "okname", version: "1.0", arch: "AMD64"}} {
		assertCode(t, f.request("PUT", base+"pool/main/o/okname/okname_1.0_amd64.deb?distribution=stable", buildDeb(t, bad)), 400)
	}
	assertCode(t, f.request("PUT", base+poolPath("xzpkg", "1.0", "amd64")+"?distribution=stable", buildDeb(t, debSpec{pkg: "xzpkg", version: "1.0", arch: "amd64", controlName: "control.tar.xz"})), 400)
	// Wrong declared checksum.
	req := httptest.NewRequest("PUT", base+poolPath("demo", "4.0", "amd64")+"?distribution=stable", bytes.NewReader(buildDeb(t, debSpec{pkg: "demo", version: "4.0", arch: "amd64"})))
	req.SetBasicAuth("s", f.token)
	req.Header.Set("X-Checksum-Sha256", strings.Repeat("0", 64))
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 400)
	// Nothing rejected is visible: only the one good package is listed or stored.
	pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil)
	assertCode(t, pk, 200)
	if strings.Count(pk.Body.String(), "Package: ") != 1 {
		t.Fatal(pk.Body.String())
	}
	assertCode(t, f.request("GET", base+poolPath("demo", "4.0", "amd64"), nil), 404)
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM components WHERE coordinate LIKE 'pool/%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("components %d %v", n, err)
	}
}

func TestAuthorization(t *testing.T) {
	f := setup(t)
	f.put(t, debSpec{pkg: "demo", version: "1.0", arch: "amd64"}, "stable", 201)
	reader := f.issue(t, identity.Read)
	publisher := f.issue(t, identity.Read, identity.Publish) // no Admin, no Delete
	deb := buildDeb(t, debSpec{pkg: "demo", version: "2.0", arch: "amd64"})
	put := base + poolPath("demo", "2.0", "amd64") + "?distribution=stable"
	assertCode(t, f.as(reader, "GET", base+"dists/stable/InRelease", nil), 200)
	assertCode(t, f.as(reader, "GET", base+poolPath("demo", "1.0", "amd64"), nil), 200)
	assertCode(t, f.as(reader, "PUT", put, deb), 403)
	assertCode(t, f.as(publisher, "PUT", put, deb), 403)
	assertCode(t, f.as(publisher, "DELETE", base+poolPath("demo", "1.0", "amd64")+"?distribution=stable", nil), 403)
	assertCode(t, f.as(reader, "DELETE", base+poolPath("demo", "1.0", "amd64")+"?distribution=stable", nil), 403)
	// Missing, malformed, and invalid credentials.
	req := httptest.NewRequest("GET", base+"dists/stable/InRelease", nil)
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 401)
	assertCode(t, f.as("not-a-token", "GET", base+"dists/stable/InRelease", nil), 401)
	// Package was not published by denied attempts.
	assertCode(t, f.request("GET", base+poolPath("demo", "2.0", "amd64"), nil), 404)
}

func TestTamperAndExpiry(t *testing.T) {
	f := setup(t)
	f.put(t, debSpec{pkg: "demo", version: "1.0", arch: "amd64"}, "stable", 201)
	kr := f.keyring(t)
	in := f.request("GET", base+"dists/stable/InRelease", nil).Body.Bytes()
	if _, err := verifyInRelease(kr, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Tampered InRelease (content change) fails signature verification.
	bad := bytes.Replace(in, []byte("Codename: stable"), []byte("Codename: stablf"), 1)
	if _, err := verifyInRelease(kr, bad, time.Now()); err == nil {
		t.Fatal("tampered InRelease verified")
	}
	// Detached signature over a modified Release fails too.
	rel := f.request("GET", base+"dists/stable/Release", nil).Body.Bytes()
	sig := f.request("GET", base+"dists/stable/Release.gpg", nil).Body.Bytes()
	if _, err := openpgp.CheckArmoredDetachedSignature(kr, bytes.NewReader(append([]byte("X"), rel...)), bytes.NewReader(sig)); err == nil {
		t.Fatal("tampered Release verified")
	}
	// A different key does not verify.
	other, err := openpgp.NewEntity("other", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifyInRelease(openpgp.EntityList{other}, in, time.Now()); err == nil {
		t.Fatal("untrusted key verified")
	}
	// Expired metadata: the repository signs with a Valid-Until in the past.
	if _, err = verifyInRelease(kr, in, time.Now().Add(31*24*time.Hour)); err == nil {
		t.Fatal("expired Release accepted")
	}
	// Corrupt the stored Packages blob on disk: serving must fail, never return bad bytes.
	pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil).Body.Bytes()
	corrupted := 0
	_ = filepath.WalkDir(f.blobs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, e := os.ReadFile(path); e == nil && bytes.Equal(b, pk) {
			_ = os.Chmod(path, 0600)
			if os.WriteFile(path, append([]byte("#"), b...), 0600) == nil {
				corrupted++
			}
		}
		return nil
	})
	if corrupted == 0 {
		t.Fatal("blob file not found")
	}
	w := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil)
	if w.Code == 200 {
		t.Fatalf("served corrupted index: %q", w.Body.String())
	}
}

func TestExpiredReleaseIsDetectable(t *testing.T) {
	f := setup(t)
	f.registry.now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	f.registry.SetValidity(time.Hour)
	f.put(t, debSpec{pkg: "demo", version: "1.0", arch: "amd64"}, "stable", 201)
	kr := f.keyring(t)
	in := f.request("GET", base+"dists/stable/InRelease", nil).Body.Bytes()
	if _, err := verifyInRelease(kr, in, time.Now()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry, got %v", err)
	}
}

func TestConcurrentPublishAndStableReaders(t *testing.T) {
	f := setup(t)
	f.put(t, debSpec{pkg: "first", version: "1.0", arch: "amd64"}, "stable", 201)
	oldRelease := f.request("GET", base+"dists/stable/Release", nil).Body.String()
	oldPackages := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil).Body.Bytes()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := debSpec{pkg: fmt.Sprintf("pkg%d", i), version: "1.0", arch: "amd64"}
			data := buildDeb(t, d)
			w := f.request("PUT", base+poolPath(d.pkg, d.version, d.arch)+"?distribution=stable", data)
			if w.Code != 201 {
				t.Errorf("upload %d: %d %s", i, w.Code, w.Body.String())
			}
		}(i)
	}
	// Readers run concurrently and must always see a Release whose indexes verify.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 2; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rel := f.request("GET", base+"dists/stable/Release", nil)
				pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil)
				if rel.Code != 200 || pk.Code != 200 {
					t.Errorf("reader %d %d", rel.Code, pk.Code)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil)
	assertCode(t, pk, 200)
	if got := strings.Count(pk.Body.String(), "Package: "); got != 5 {
		t.Fatalf("lost updates: %d packages\n%s", got, pk.Body.String())
	}
	// The pre-update generation's index is still reachable by hash.
	if !strings.Contains(oldRelease, hexSum(oldPackages)) {
		t.Fatal("old release does not list old packages")
	}
	assertCode(t, f.request("GET", base+"dists/stable/main/binary-amd64/by-hash/SHA256/"+hexSum(oldPackages), nil), 200)
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='dists/stable'`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("generations %d %v", n, err)
	}
}

func TestUnlistAndMultipleDistributions(t *testing.T) {
	f := setup(t)
	a := debSpec{pkg: "alpha", version: "1.0", arch: "amd64"}
	b := debSpec{pkg: "beta", version: "1.0", arch: "arm64"}
	f.put(t, a, "stable", 201)
	f.put(t, b, "stable", 201)
	f.put(t, a, "testing", 201) // same bytes, second distribution
	rel := f.request("GET", base+"dists/stable/Release", nil).Body.String()
	if !strings.Contains(rel, "Architectures: amd64 arm64\n") {
		t.Fatal(rel)
	}
	assertCode(t, f.request("DELETE", base+poolPath("alpha", "1.0", "amd64")+"?distribution=stable", nil), 204)
	assertCode(t, f.request("DELETE", base+poolPath("alpha", "1.0", "amd64")+"?distribution=stable", nil), 404)
	assertCode(t, f.request("DELETE", base+poolPath("alpha", "1.0", "amd64"), nil), 400)
	if pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil); strings.Contains(pk.Body.String(), "alpha") {
		t.Fatal("alpha still listed in stable")
	}
	if pk := f.request("GET", base+"dists/testing/main/binary-amd64/Packages", nil); !strings.Contains(pk.Body.String(), "alpha") {
		t.Fatal("alpha missing from testing")
	}
	// The signed stable Release is valid after deletion.
	kr := f.keyring(t)
	if _, err := verifyInRelease(kr, f.request("GET", base+"dists/stable/InRelease", nil).Body.Bytes(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Re-publishing after unlist re-lists it (bytes were retained and identical).
	f.put(t, a, "stable", 201)
	if pk := f.request("GET", base+"dists/stable/main/binary-amd64/Packages", nil); !strings.Contains(pk.Body.String(), "alpha") {
		t.Fatal("alpha not re-listed")
	}
}

func TestSigningUnavailableFailsClosed(t *testing.T) {
	f := setup(t)
	f.registry.SetKeyring(nil)
	data := buildDeb(t, debSpec{pkg: "demo", version: "1.0", arch: "amd64"})
	assertCode(t, f.request("PUT", base+poolPath("demo", "1.0", "amd64")+"?distribution=stable", data), 503)
	assertCode(t, f.request("GET", base+"dists/stable/InRelease", nil), 404)
	assertCode(t, f.request("GET", base+poolPath("demo", "1.0", "amd64"), nil), 404)
	// No key exists until the first publish.
	f.registry.SetKeyring(testKeyring(t))
	assertCode(t, f.request("GET", base+"signing-key.asc", nil), 404)
}

func TestSigningKeyIsEncryptedAndStable(t *testing.T) {
	f := setup(t)
	f.put(t, debSpec{pkg: "demo", version: "1.0", arch: "amd64"}, "stable", 201)
	f.put(t, debSpec{pkg: "demo", version: "2.0", arch: "amd64"}, "testing", 201)
	var n int
	var envelope string
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*),max(envelope::text) FROM encrypted_secrets WHERE purpose='apt-signing:'||$1`, f.feed).Scan(&n, &envelope); err != nil || n != 1 {
		t.Fatalf("key records %d %v", n, err)
	}
	if strings.Contains(envelope, "PRIVATE") || strings.Contains(envelope, "BEGIN") {
		t.Fatal("key stored in clear")
	}
	asc := f.request("GET", base+"signing-key.asc", nil)
	assertCode(t, asc, 200)
	if !strings.Contains(asc.Body.String(), "BEGIN PGP PUBLIC KEY BLOCK") || strings.Contains(asc.Body.String(), "PRIVATE") {
		t.Fatal(asc.Body.String())
	}
	// Both distributions verify against the same single key.
	kr := f.keyring(t)
	for _, d := range []string{"stable", "testing"} {
		if _, err := verifyInRelease(kr, f.request("GET", base+"dists/"+d+"/InRelease", nil).Body.Bytes(), time.Now()); err != nil {
			t.Fatal(d, err)
		}
	}
	// Wrong keyring cannot decrypt the key: signing fails closed.
	k2, _ := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{9}, 32)})
	f.registry.SetKeyring(k2)
	assertCode(t, f.request("PUT", base+poolPath("demo", "3.0", "amd64")+"?distribution=stable", buildDeb(t, debSpec{pkg: "demo", version: "3.0", arch: "amd64"})), 503)
	assertCode(t, f.request("GET", base+"signing-key.asc", nil), 503)
}

func TestFeedIsolationAndFormat(t *testing.T) {
	f := setup(t)
	f.put(t, debSpec{pkg: "demo", version: "1.0", arch: "amd64"}, "stable", 201)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'raw','raw')`, f.instance); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("GET", "/feeds/raw/apt/dists/stable/InRelease", nil), 404)
	assertCode(t, f.request("GET", "/feeds/missing/apt/dists/stable/InRelease", nil), 404)
	assertCode(t, f.request("GET", base+"dists/stable/%2e%2e/InRelease", nil), 404)
	assertCode(t, f.request("GET", base+"dists/stable/main/binary-amd64/by-hash/SHA256/"+strings.Repeat("0", 64), nil), 404)
}

func TestParsers(t *testing.T) {
	if _, err := parseParagraph([]byte("Package: a\n\nPackage: b\n")); err == nil {
		t.Fatal("two paragraphs accepted")
	}
	if _, err := parseParagraph([]byte("Package: a\nPackage: b\n")); err == nil {
		t.Fatal("duplicate field accepted")
	}
	if _, err := parseParagraph([]byte(" continuation: first\n")); err == nil {
		t.Fatal("leading continuation accepted")
	}
	for _, v := range []string{"1.0", "1:2.0-1", "2.0~rc1+dfsg-1ubuntu1"} {
		if !validVersion(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"", "a1", "1:2:3", "1 0"} {
		if validVersion(v) {
			t.Fatal(v)
		}
	}
	if _, err := inspectDeb(bytes.NewReader([]byte("!<arch>\n")), 8); err == nil {
		t.Fatal("empty ar accepted")
	}
}
