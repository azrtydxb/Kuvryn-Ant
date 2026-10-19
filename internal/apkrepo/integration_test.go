package apkrepo

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- APK v2 is SHA-1 by specification.
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
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
)

type fixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Repository
	service                                        *artifact.Service
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
	schema := "ant_apk_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "APK")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'alpine','apk') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','apk') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	var domain string
	if err = pool.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id`, owner.InstanceID, "source:"+f.source).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	disk, err := blob.NewDisk(filepath.Join(t.TempDir(), "blobs"), blob.TransferBufferSize*4)
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
	ring, err := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f.registry.SetKeyring(ring)
	if err = f.registry.SetPublicURL("https://packages.example"); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) request(method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("service", f.token)
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func assertCode(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}

// makeApk builds a v2 package: signature segment, control segment, data segment.
func makeApk(t testing.TB, name, ver, arch, salt string) []byte {
	t.Helper()
	pkginfo := "pkgname = " + name + "\npkgver = " + ver + "\npkgdesc = demo " + salt + "\narch = " + arch + "\nsize = 12\ndepend = busybox\nlicense = MIT\n"
	sig, err := segment(map[string][]byte{".SIGN.RSA.upstream.rsa.pub": []byte("not-verified-by-ant")}, []string{".SIGN.RSA.upstream.rsa.pub"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := segment(map[string][]byte{".PKGINFO": []byte(pkginfo)}, []string{".PKGINFO"}, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := segment(map[string][]byte{"usr/share/" + name: []byte("hello " + salt)}, []string{"usr/share/" + name}, true)
	if err != nil {
		t.Fatal(err)
	}
	return append(append(sig, ctl...), data...)
}

// verifyIndex acts as the client: checks the RSA signature over the compressed
// index segment and returns the APKINDEX text.
func verifyIndex(t testing.TB, index, pubPEM []byte) (string, error) {
	t.Helper()
	blk, _ := pem.Decode(pubPEM)
	if blk == nil {
		t.Fatal("no pem")
	}
	pk, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	c := &counter{br: bufio.NewReader(bytes.NewReader(index))}
	gz, err := gzip.NewReader(c)
	if err != nil {
		return "", err
	}
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || !strings.HasPrefix(h.Name, ".SIGN.RSA.ant-") || !strings.HasSuffix(h.Name, ".rsa.pub") {
		return "", errInvalid
	}
	sig, _ := io.ReadAll(tr)
	_, _ = io.Copy(io.Discard, gz)
	start := c.n
	if err = gz.Reset(c); err != nil {
		return "", err
	}
	gz.Multistream(false)
	sum := sha1.Sum(index[start:]) // #nosec G401
	if err = rsa.VerifyPKCS1v15(pk.(*rsa.PublicKey), crypto.SHA1, sum[:], sig); err != nil {
		return "", err
	}
	tr = tar.NewReader(gz)
	for {
		h, err = tr.Next()
		if err != nil {
			return "", err
		}
		if h.Name == "APKINDEX" {
			b, _ := io.ReadAll(tr)
			return string(b), nil
		}
	}
}

const base = "/feeds/alpine/apk/v3.24/main/"

func TestPostgres(t *testing.T) {
	f := setup(t)
	x86 := makeApk(t, "demo", "1.0-r0", "x86_64", "a")
	arm := makeApk(t, "demo", "1.0-r0", "aarch64", "b")
	assertCode(t, f.request("PUT", base+"x86_64/demo-1.0-r0.apk", x86), 201)
	assertCode(t, f.request("PUT", base+"x86_64/demo-1.0-r0.apk", x86), 201) // idempotent
	assertCode(t, f.request("PUT", base+"aarch64/demo-1.0-r0.apk", arm), 201)
	assertCode(t, f.request("PUT", base+"x86_64/demo-1.0-r0.apk", makeApk(t, "demo", "1.0-r0", "x86_64", "changed")), 409)
	assertCode(t, f.request("PUT", base+"x86_64/noarch-1.0-r0.apk", makeApk(t, "noarch", "1.0-r0", "noarch", "n")), 201)

	// Validation / tamper cases on upload.
	assertCode(t, f.request("PUT", base+"x86_64/demo-2.0-r0.apk", x86), 400) // name/version mismatch
	assertCode(t, f.request("PUT", base+"x86_64/demo-1.0-r0.apk", arm), 400)
	assertCode(t, f.request("PUT", base+"x86_64/other-1.0-r0.apk", arm), 400)                                         // wrong arch for channel
	assertCode(t, f.request("PUT", base+"x86_64/trunc-1.0-r0.apk", x86[:len(x86)-20]), 400)                           // truncated data segment
	assertCode(t, f.request("PUT", base+"x86_64/junk-1.0-r0.apk", []byte("not an apk at all")), 400)                  // garbage
	assertCode(t, f.request("PUT", base+"x86_64/ctl-1.0-r0.apk", x86[:bytes.Index(x86, []byte{0x1f, 0x8b})+10]), 400) // header only
	assertCode(t, f.request("PUT", base+"x86_64/bad_name-1.0.apk", x86), 404)

	// Download preserves bytes.
	w := f.request("GET", base+"x86_64/demo-1.0-r0.apk", nil)
	assertCode(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), x86) {
		t.Fatal("package bytes changed")
	}
	assertCode(t, f.request("HEAD", base+"aarch64/demo-1.0-r0.apk", nil), 200)
	// A noarch package is served from every arch path and from /noarch/ (apk asks for
	// the package architecture); an arch-specific package is not.
	for _, arch := range []string{"aarch64", "noarch", "x86_64"} {
		w := f.request("GET", base+arch+"/noarch-1.0-r0.apk", nil)
		assertCode(t, w, 200)
		if !bytes.Equal(w.Body.Bytes(), makeApk(t, "noarch", "1.0-r0", "noarch", "n")) {
			t.Fatal("noarch bytes differ via ", arch)
		}
	}
	assertCode(t, f.request("GET", base+"noarch/demo-1.0-r0.apk", nil), 404)

	// Public key + signed index per architecture.
	var feedKey string
	if e := f.pool.QueryRow(context.Background(), `SELECT replace($1::text,'-','')`, f.feed).Scan(&feedKey); e != nil {
		t.Fatal(e)
	}
	keyPath := "/feeds/alpine/apk/keys/ant-" + feedKey[:8] + ".rsa.pub"
	pub := f.request("GET", keyPath, nil)
	assertCode(t, pub, 200)
	assertCode(t, f.request("GET", "/feeds/alpine/apk/keys/other.rsa.pub", nil), 404)
	w = f.request("GET", base+"x86_64/APKINDEX.tar.gz", nil)
	assertCode(t, w, 200)
	text, err := verifyIndex(t, w.Body.Bytes(), pub.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "P:demo\nV:1.0-r0\nA:x86_64\nS:") || !strings.Contains(text, "P:noarch\n") || strings.Contains(text, "aarch64") {
		t.Fatal(text)
	}
	f2 := f.request("GET", base+"aarch64/APKINDEX.tar.gz", nil)
	assertCode(t, f2, 200)
	text, err = verifyIndex(t, f2.Body.Bytes(), pub.Body.Bytes())
	if err != nil || !strings.Contains(text, "A:aarch64") || strings.Contains(text, "x86_64") {
		t.Fatal(err, text)
	}
	// Control checksum matches SHA-1 of the control segment.
	ctlOnly, _ := segment(map[string][]byte{".PKGINFO": []byte("pkgname = demo\npkgver = 1.0-r0\npkgdesc = demo a\narch = x86_64\nsize = 12\ndepend = busybox\nlicense = MIT\n")}, []string{".PKGINFO"}, false)
	sum := sha1.Sum(ctlOnly) // #nosec G401
	_, _ = verifyIndex(t, w.Body.Bytes(), pub.Body.Bytes())
	if !strings.Contains(string(mustIndex(t, f, base+"x86_64/APKINDEX.tar.gz", pub.Body.Bytes())), "C:Q1"+base64.StdEncoding.EncodeToString(sum[:])) {
		t.Fatal("control checksum mismatch")
	}

	// Tampering with the index or signature is rejected by a verifying client.
	idx := append([]byte(nil), w.Body.Bytes()...)
	idx[len(idx)-30] ^= 0xff
	if _, err = verifyIndex(t, idx, pub.Body.Bytes()); err == nil {
		t.Fatal("tampered index accepted")
	}
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherPEM, _ := publicPEM(otherKey)
	if _, err = verifyIndex(t, w.Body.Bytes(), otherPEM); err == nil {
		t.Fatal("index verified with foreign key")
	}
	// The key is stable across restarts (reloaded from the secret store).
	restarted, _ := New(f.pool, f.service)
	ring, _ := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{7}, 32)})
	restarted.SetKeyring(ring)
	f.registry = restarted
	again := f.request("GET", keyPath, nil)
	if again.Body.String() != pub.Body.String() {
		t.Fatal("signing key changed")
	}
	// Blob corruption in the index source is not served as valid metadata.
	if _, e := f.pool.Exec(context.Background(), `UPDATE components SET namespace='wrong' WHERE source_id=$1 AND coordinate='v3.24/main/aarch64'`, f.source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", base+"aarch64/APKINDEX.tar.gz", nil), 503)

	// Concurrent immutable conflicts: exactly one wins.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, s := range []string{"one", "two"} {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			codes <- f.request("PUT", base+"x86_64/race-1.0-r0.apk", makeApk(t, "race", "1.0-r0", "x86_64", s)).Code
		}(s)
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}

	// Delete removes the package and its index entry atomically.
	assertCode(t, f.request("DELETE", base+"x86_64/demo-1.0-r0.apk", nil), 204)
	assertCode(t, f.request("DELETE", base+"x86_64/demo-1.0-r0.apk", nil), 404)
	assertCode(t, f.request("GET", base+"x86_64/demo-1.0-r0.apk", nil), 404)
	w = f.request("GET", base+"x86_64/APKINDEX.tar.gz", nil)
	assertCode(t, w, 200)
	text, err = verifyIndex(t, w.Body.Bytes(), pub.Body.Bytes())
	if err != nil || strings.Contains(text, "P:demo\n") || !strings.Contains(text, "P:race\n") {
		t.Fatal(err, text)
	}
	// Unauthenticated requests are refused.
	req := httptest.NewRequest("GET", base+"x86_64/APKINDEX.tar.gz", nil)
	r := httptest.NewRecorder()
	f.registry.ServeHTTP(r, req)
	assertCode(t, r, 401)
}

func mustIndex(t testing.TB, f fixture, path string, pub []byte) string {
	t.Helper()
	w := f.request("GET", path, nil)
	text, err := verifyIndex(t, w.Body.Bytes(), pub)
	if err != nil {
		t.Fatal(err)
	}
	return text
}
