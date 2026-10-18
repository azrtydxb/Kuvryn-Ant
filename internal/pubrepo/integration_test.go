package pubrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	schema := "ant_pub_" + strings.ToLower(rand.Text())
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
	config.MaxConns = 1
	pool, err := database.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Pub")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','pub') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','pub') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	if err = f.registry.SetPublicURL("https://packages.example"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.registry.Close)
	return f
}
func (f fixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+f.token)
	if method == "PUT" {
		req.Header.Set("X-Pub-ApiKey", f.token)
	}
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

const testBase = "/feeds/packages/pub/"

func archive(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	tr := tar.NewWriter(g)
	data := []byte(fmt.Sprintf("name: %s\nversion: %s\ndescription: Fixture %s\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\n", name, version, extra))
	if e := tr.WriteHeader(&tar.Header{Name: "pubspec.yaml", Mode: 0600, Size: int64(len(data))}); e != nil {
		t.Fatal(e)
	}
	tr.Write(data)
	tr.Close()
	g.Close()
	return b.Bytes()
}
func (f fixture) stage(t *testing.T, b []byte) string {
	t.Helper()
	res := f.request("GET", testBase+"api/packages/versions/new", nil, nil)
	assertCode(t, res, 200)
	var start struct {
		URL string `json:"url"`
	}
	json.Unmarshal(res.Body.Bytes(), &start)
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	part, _ := m.CreateFormFile("file", "package.tar.gz")
	part.Write(b)
	m.Close()
	req := httptest.NewRequest("POST", start.URL, &body)
	req.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 204)
	return w.Header().Get("Location")
}
func TestPostgresPublish(t *testing.T) {
	f := setup(t)
	b := archive(t, "fixture_pkg", "1.0.0", "")
	loc := f.stage(t, b)
	assertCode(t, f.request("GET", testBase+"api/packages/fixture_pkg", nil, nil), 404)
	other, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Publish}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", loc, nil, map[string]string{"Authorization": "Bearer " + other.Secret}), 403)
	assertCode(t, f.request("GET", loc, nil, nil), 200)
	assertCode(t, f.request("GET", loc, nil, nil), 200)
	res := f.request("GET", testBase+"api/packages/fixture_pkg", nil, nil)
	assertCode(t, res, 200)
	if !strings.Contains(res.Body.String(), string(info(b).Digest)) {
		t.Fatal("missing digest")
	}
	res = f.request("GET", testBase+"packages/fixture_pkg/versions/1.0.0.tar.gz", nil, nil)
	assertCode(t, res, 200)
	if !bytes.Equal(res.Body.Bytes(), b) {
		t.Fatal("archive changed")
	}
	loc = f.stage(t, archive(t, "fixture_pkg", "1.0.0", "changed"))
	assertCode(t, f.request("GET", loc, nil, nil), 409)
	loc = f.stage(t, []byte("invalid"))
	assertCode(t, f.request("GET", loc, nil, nil), 400)
	var source string
	ctx := context.Background()
	e = f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'higher','local','pub') RETURNING id`, f.instance).Scan(&source)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'fixture_pkg')`, f.instance, source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", testBase+"api/packages/fixture_pkg", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"packages/fixture_pkg/versions/1.0.0.tar.gz", nil, nil), 404)
}
func TestPostgresDart(t *testing.T) {
	dart, e := exec.LookPath("dart")
	if e != nil {
		t.Skip("dart unavailable")
	}
	f := setup(t)
	server := httptest.NewServer(f.registry)
	defer server.Close()
	if e = f.registry.SetPublicURL(server.URL); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache")
	config := filepath.Join(dir, "config")
	os.MkdirAll(config, 0700)
	run := func(cwd string, input string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, dart, args...)
		cmd.Dir = cwd
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = append(os.Environ(), "PUB_CACHE="+cache, "XDG_CONFIG_HOME="+config, "DART_SUPPRESS_ANALYTICS=true")
		output, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("dart %s: %v\n%s", args[0], e, strings.ReplaceAll(string(output), f.token, "[redacted]"))
		}
	}
	hosted := server.URL + strings.TrimSuffix(testBase, "/")
	run(dir, f.token+"\n", "pub", "token", "add", hosted)
	pkg := filepath.Join(dir, "pkg")
	os.MkdirAll(filepath.Join(pkg, "lib"), 0700)
	os.WriteFile(filepath.Join(pkg, "pubspec.yaml"), []byte("name: fixture_pkg\nversion: 1.0.0\ndescription: A local fixture for testing the Pub hosted protocol.\npublish_to: "+hosted+"\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\n"), 0600)
	for _, file := range []string{"LICENSE", "README.md", "CHANGELOG.md", "lib/fixture_pkg.dart"} {
		os.WriteFile(filepath.Join(pkg, file), []byte("// fixture\n"), 0600)
	}
	run(pkg, "", "pub", "get")
	run(pkg, "", "pub", "publish", "--force")
	app := filepath.Join(dir, "app")
	os.Mkdir(app, 0700)
	os.WriteFile(filepath.Join(app, "pubspec.yaml"), []byte("name: fixture_app\nenvironment:\n  sdk: '>=3.0.0 <4.0.0'\ndependencies:\n  fixture_pkg:\n    hosted: "+hosted+"\n    version: 1.0.0\n"), 0600)
	run(app, "", "pub", "get")
	lock, e := os.ReadFile(filepath.Join(app, "pubspec.lock"))
	if e != nil || !bytes.Contains(lock, []byte("sha256:")) {
		t.Fatalf("lockfile hash absent: %v", e)
	}
}
func TestPostgresFailedFinalizeAndConcurrent(t *testing.T) {
	f := setup(t)
	loc := f.stage(t, archive(t, "atomic_pkg", "1.0.0", ""))
	original := f.service.Storage
	f.service.Storage = func(context.Context, string) (blob.Store, error) { return nil, artifact.ErrUnavailable }
	assertCode(t, f.request("GET", loc, nil, nil), 503)
	var count int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("partial visibility: %d %v", count, e)
	}
	f.service.Storage = original
	assertCode(t, f.request("GET", loc, nil, nil), 200)
	locations := []string{f.stage(t, archive(t, "race_pkg", "1.0.0", "a")), f.stage(t, archive(t, "race_pkg", "1.0.0", "b"))}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, loc := range locations {
		wg.Add(1)
		go func(loc string) { defer wg.Done(); statuses <- f.request("GET", loc, nil, nil).Code }(loc)
	}
	wg.Wait()
	close(statuses)
	got := map[int]int{}
	for status := range statuses {
		got[status]++
	}
	if got[200] != 1 || got[409] != 1 {
		t.Fatal(got)
	}
}
func TestPostgresAuthAndOrigin(t *testing.T) {
	f := setup(t)
	assertCode(t, f.request("GET", testBase+"api/packages/versions/new", nil, map[string]string{"Authorization": ""}), 401)
	token, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", testBase+"api/packages/versions/new", nil, map[string]string{"Authorization": "Bearer " + token.Secret}), 403)
	req := httptest.NewRequest("GET", testBase+"api/packages/versions/new", nil)
	req.Host = "attacker.invalid"
	req.Header.Set("X-Forwarded-Host", "attacker.invalid")
	req.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), "attacker.invalid") {
		t.Fatal("untrusted host used")
	}
}
