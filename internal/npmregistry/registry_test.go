package npmregistry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Registry
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
	schema := "ant_npm_" + strings.ToLower(rand.Text())
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
	pool, err := database.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "npm")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'crates','npm') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','npm') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	if err = f.registry.SetPublicURL("http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+f.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func assertCode(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected%d: %s", response.Code, status, response.Body.String())
	}
}
func archive(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tr := tar.NewWriter(gz)
	manifest, _ := json.Marshal(map[string]string{"name": name, "version": version, "main": "index.js"})
	files := map[string][]byte{"package/package.json": manifest, "package/index.js": []byte("module.exports=42;\n")}
	if extra != "" {
		files[extra] = []byte("x")
	}
	for name, data := range files {
		if e := tr.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); e != nil {
			t.Fatal(e)
		}
		tr.Write(data)
	}
	tr.Close()
	gz.Close()
	return b.Bytes()
}
func wire(name, version, tag string, b []byte) []byte {
	h1 := sha1.Sum(b)
	h512 := sha512.Sum512(b)
	m := map[string]any{"name": name, "version": version, "dist": map[string]string{"shasum": hex.EncodeToString(h1[:]), "integrity": "sha512-" + base64.StdEncoding.EncodeToString(h512[:]), "tarball": "https://untrusted.invalid/a"}}
	doc := map[string]any{"name": name, "versions": map[string]any{version: m}, "dist-tags": map[string]string{tag: version}, "_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{"length": len(b), "content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(b)}}}
	data, _ := json.Marshal(doc)
	return data
}
func TestValidation(t *testing.T) {
	for _, n := range []string{"@scope/pkg", "demo", "a.b"} {
		if !validName(n) {
			t.Fatal(n)
		}
	}
	for _, n := range []string{"@scope", "@scope/../x", "../foo", "UPPER", "a%2fb", "@/x"} {
		if validName(n) {
			t.Fatal(n)
		}
	}
	for _, v := range []string{"1.2.3", "1.0.0-beta.1+build"} {
		if !validVersion(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"v1.0.0", "01.2.3", "1.2.3-01", "1.2", "1.2.3/evil", "9007199254740992.0.0"} {
		if validVersion(v) {
			t.Fatal(v)
		}
	}
	b := archive(t, "@scope/demo", "1.0.0", "")
	body := wire("@scope/demo", "1.0.0", "latest", b)
	if _, e := parsePublish(context.Background(), bytes.NewReader(body), "@scope/demo"); e != nil {
		t.Fatal(e)
	}
	for _, bad := range [][]byte{append(append([]byte{}, body...), []byte("{}")...), wire("@scope/demo", "1.0.0", "latest", archive(t, "@scope/demo", "1.0.0", "package/../../evil")), bytes.Replace(body, []byte(`"shasum":"`), []byte(`"shasum":"0`), 1)} {
		if _, e := parsePublish(context.Background(), bytes.NewReader(bad), "@scope/demo"); e == nil {
			t.Fatal("accepted malformed publish")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if validateArchive(ctx, b, "@scope/demo", "1.0.0") == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestPublicURL(t *testing.T) {
	r := &Registry{}
	for _, u := range []string{"https://registry.example", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if e := r.SetPublicURL(u); e != nil {
			t.Fatal(u, e)
		}
	}
	for _, u := range []string{"http://localhost:1234", "http://example.com", "https://user@host", "https://host/path", "https://host?x", "https://host#x", "/relative"} {
		if r.SetPublicURL(u) == nil {
			t.Fatal(u)
		}
	}
}
func TestPublishReadAndIsolation(t *testing.T) {
	f := setup(t)
	base := "/feeds/crates/npm/"
	name := "@scope/demo"
	target := base + "@scope%2fdemo"
	b := archive(t, name, "1.0.0", "")
	body := wire(name, "1.0.0", "latest", b)
	assertCode(t, f.request("GET", target, nil, map[string]string{"Authorization": ""}), 401)
	assertCode(t, f.request("PUT", target, body, nil), 201)
	assertCode(t, f.request("PUT", target, body, nil), 201)
	response := f.request("GET", target, nil, nil)
	assertCode(t, response, 200)
	if strings.Contains(response.Body.String(), "untrusted.invalid") || !strings.Contains(response.Body.String(), "http://127.0.0.1:1234") {
		t.Fatal(response.Body.String())
	}
	assertCode(t, f.request("GET", target, nil, map[string]string{"If-None-Match": response.Header().Get("ETag")}), 304)
	assertCode(t, f.request("GET", base+"-/package/@scope%2fdemo/dist-tags", nil, nil), 200)
	assertCode(t, f.request("PUT", base+"-/package/@scope%2fdemo/dist-tags/latest", []byte(`"2.0.0"`), nil), 404)
	assertCode(t, f.request("DELETE", target, nil, nil), 405)
	d := f.request("GET", base+name+"/-/demo-1.0.0.tgz", nil, nil)
	assertCode(t, d, 200)
	if !bytes.Equal(d.Body.Bytes(), b) {
		t.Fatal("tarball changed")
	}
	assertCode(t, f.request("GET", base+name+"/-/demo-1.0.0.tgz?source=wrong", nil, nil), 409)
	// Moving an existing tag fails atomically: neither asset of v2 becomes visible.
	assertCode(t, f.request("PUT", target, wire(name, "2.0.0", "latest", archive(t, name, "2.0.0", "")), nil), 409)
	assertCode(t, f.request("GET", target+"/2.0.0", nil, nil), 404)
	var n int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&n); e != nil || n != 2 {
		t.Fatal(n, e)
	}
	token, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}, Namespace: "@other"}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", target, nil, map[string]string{"Authorization": "Bearer " + token.Secret}), 404)
	assertCode(t, f.request("PUT", target, body, map[string]string{"Authorization": "Bearer " + token.Secret}), 403)
}
func TestRealNPMPublishInstall(t *testing.T) {
	if _, e := exec.LookPath("npm"); e != nil {
		t.Skip("npm unavailable")
	}
	f := setup(t)
	server := httptest.NewUnstartedServer(f.registry)
	server.Start()
	defer server.Close()
	if e := f.registry.SetPublicURL(server.URL); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	config := filepath.Join(root, "npmrc")
	cache := filepath.Join(root, "cache")
	registry := server.URL + "/feeds/crates/npm/"
	os.WriteFile(config, []byte("registry="+registry+"\n"+strings.TrimPrefix(registry, "http:")+":_authToken="+f.token+"\naudit=false\nfund=false\n"), 0600)
	run := func(dir string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "npm", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "NPM_CONFIG_USERCONFIG="+config, "NPM_CONFIG_CACHE="+cache, "NPM_CONFIG_FETCH_RETRIES=0")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("npm %v: %v\n%s", args, e, out)
		}
		t.Logf("npm %v: %s", args, out)
	}
	pkg := filepath.Join(root, "pkg")
	consumer := filepath.Join(root, "consumer")
	os.MkdirAll(pkg, 0700)
	os.MkdirAll(consumer, 0700)
	os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@fixture/demo","version":"1.0.0","main":"index.js"}`), 0600)
	os.WriteFile(filepath.Join(pkg, "index.js"), []byte("module.exports=42;\n"), 0600)
	os.WriteFile(filepath.Join(consumer, "package.json"), []byte(`{"name":"consumer","version":"1.0.0","private":true}`), 0600)
	run(pkg, "--version")
	run(pkg, "publish", "--access=public", "--ignore-scripts")
	run(consumer, "view", "@fixture/demo", "version")
	run(consumer, "dist-tag", "ls", "@fixture/demo")
	run(consumer, "install", "@fixture/demo@1.0.0", "--ignore-scripts", "--no-audit", "--no-fund")
	run(consumer, "ci", "--offline", "--ignore-scripts", "--no-audit", "--no-fund")
	if data, e := os.ReadFile(filepath.Join(consumer, "node_modules/@fixture/demo/index.js")); e != nil || string(data) != "module.exports=42;\n" {
		t.Fatal(string(data), e)
	}
}
func TestPackageOwnerAndInstance(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := "/feeds/crates/npm/demo"
	assertCode(t, f.request("PUT", base, wire("demo", "1.0.0", "latest", archive(t, "demo", "1.0.0", "")), nil), 201)
	var second string
	if e := f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'second','local','npm') RETURNING id`, f.instance).Scan(&second); e != nil {
		t.Fatal(e)
	}
	if _, e := f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,1)`, f.instance, f.feed, second); e != nil {
		t.Fatal(e)
	}
	if _, e := f.pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, f.instance, "source:"+second); e != nil {
		t.Fatal(e)
	}
	router, e := routing.Compile(routing.Config{Sources: []routing.Source{{ID: second, Kind: routing.Local}}, DefaultTarget: second})
	if e != nil {
		t.Fatal(e)
	}
	f.service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	assertCode(t, f.request("PUT", base, wire("demo", "2.0.0", "next", archive(t, "demo", "2.0.0", "")), nil), 201)
	response := f.request("GET", base, nil, nil)
	assertCode(t, response, 200)
	if strings.Contains(response.Body.String(), "2.0.0") || strings.Contains(response.Body.String(), "next") {
		t.Fatal("mixed owners", response.Body.String())
	}
	assertCode(t, f.request("GET", base+"/-/demo-2.0.0.tgz", nil, nil), 404)
	// One database connection proves rows are closed before authorization/storage reads.
	config := f.pool.Config()
	config.MaxConns = 1
	single, e := pgxpool.NewWithConfig(ctx, config)
	if e != nil {
		t.Fatal(e)
	}
	defer single.Close()
	f.registry.db = single
	f.service.DB = single
	assertCode(t, f.request("GET", base, nil, nil), 200)
	// A valid token from another instance cannot use this handler.
	var foreign, principal string
	if e = f.pool.QueryRow(ctx, `INSERT INTO instances(name) VALUES('foreign') RETURNING id`).Scan(&foreign); e != nil {
		t.Fatal(e)
	}
	if e = f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','foreign') RETURNING id`, foreign).Scan(&principal); e != nil {
		t.Fatal(e)
	}
	token, e := (identity.Tokens{DB: f.pool}).Issue(ctx, foreign, principal, principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", base, nil, map[string]string{"Authorization": "Bearer " + token.Secret}), 404)
}
func TestBrowserContextCannotAuthenticate(t *testing.T) {
	registry := &Registry{}
	req := httptest.NewRequest("GET", "/feeds/crates/npm/demo", nil)
	req = req.WithContext(identity.WithAuthenticatedPrincipal(req.Context(), identity.Principal{InstanceID: "browser", PrincipalID: "browser"}))
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, req)
	assertCode(t, response, 401)
}
func TestConcurrentImmutableConflict(t *testing.T) {
	f := setup(t)
	bodies := [][]byte{wire("demo", "1.0.0", "latest", archive(t, "demo", "1.0.0", "")), wire("demo", "1.0.0", "latest", archive(t, "demo", "1.0.0", "package/extra"))}
	results := make(chan int, 2)
	for _, body := range bodies {
		go func(body []byte) { results <- f.request("PUT", "/feeds/crates/npm/demo", body, nil).Code }(body)
	}
	a, b := <-results, <-results
	if !((a == 201 && b == 409) || (a == 409 && b == 201)) {
		t.Fatal(a, b)
	}
	var count int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); e != nil || count != 2 {
		t.Fatal(count, e)
	}
}
