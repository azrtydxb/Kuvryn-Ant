package goproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Proxy
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
	schema := "ant_go_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Go")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'modules','go') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','go') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	return f
}
func (f fixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("service", f.token)
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

const testBase = "/feeds/modules/go/example.com/!acme/!widget/@v/"

func (f fixture) ingest(t *testing.T, b *bundle, version, omit string) *httptest.ResponseRecorder {
	t.Helper()
	data, typ := multipartBundle(t, b, omit)
	return f.request("PUT", testBase+version, data, map[string]string{"Content-Type": typ})
}
func TestPostgresProxy(t *testing.T) {
	f := setup(t)
	b := fixtureBundle(t, "example.com/Acme/Widget", "v1.0.0", nil)
	assertCode(t, f.ingest(t, b, "v1.0.0", "mod"), 400)
	assertCode(t, f.request("GET", testBase+"v1.0.0.info", nil, nil), 404)
	assertCode(t, f.ingest(t, b, "v1.0.0", ""), 201)
	assertCode(t, f.ingest(t, b, "v1.0.0", ""), 200)
	for _, kind := range []string{"info", "mod", "zip"} {
		response := f.request("GET", testBase+"v1.0.0."+kind, nil, nil)
		assertCode(t, response, 200)
		head := f.request("HEAD", testBase+"v1.0.0."+kind, nil, nil)
		assertCode(t, head, 200)
		if head.Body.Len() != 0 || head.Header().Get("Content-Length") != response.Header().Get("Content-Length") {
			t.Fatal("HEAD differs")
		}
	}
	response := f.request("GET", testBase+"list", nil, nil)
	assertCode(t, response, 200)
	if response.Body.String() != "v1.0.0\n" {
		t.Fatal(response.Body.String())
	}
	response = f.request("GET", strings.TrimSuffix(testBase, "@v/")+"@latest", nil, nil)
	assertCode(t, response, 200)
	if !bytes.Equal(response.Body.Bytes(), b.info) {
		t.Fatal("latest info differs")
	}
	changed := fixtureBundle(t, "example.com/Acme/Widget", "v1.0.0", map[string]string{"example.com/Acme/Widget@v1.0.0/change.go": "package changed"})
	assertCode(t, f.ingest(t, changed, "v1.0.0", ""), 409)
	// Missing auth and read-only scope never expose ingestion.
	req := httptest.NewRequest("GET", testBase+"list", nil)
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 401)
	readonly, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rf := f
	rf.token = readonly.Secret
	assertCode(t, rf.ingest(t, b, "v1.0.0", ""), 403)
	// A token scoped to another namespace cannot list or download this package.
	denied, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Namespace: "example.com/else", Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rf.token = denied.Secret
	assertCode(t, rf.request("GET", testBase+"list", nil, nil), 404)
	assertCode(t, rf.request("GET", testBase+"v1.0.0.zip", nil, nil), 404)
}

type observingStore struct {
	blob.Store
	before func() error
}
type observingUpload struct {
	blob.Upload
	before func() error
}

func (s observingStore) Stage(ctx context.Context) (blob.Upload, error) {
	u, e := s.Store.Stage(ctx)
	return observingUpload{u, s.before}, e
}
func (u observingUpload) Finalize(ctx context.Context, info blob.Info) (blob.Info, error) {
	if err := u.before(); err != nil {
		return blob.Info{}, err
	}
	return u.Upload.Finalize(ctx, info)
}
func TestPostgresAtomicBundle(t *testing.T) {
	f := setup(t)
	original := f.service.Storage
	var attempts atomic.Int32
	f.service.Storage = func(ctx context.Context, domain string) (blob.Store, error) {
		store, err := original(ctx, domain)
		return observingStore{store, func() error {
			var count int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM assets`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("partial assets visible: %d", count)
			}
			if attempts.Add(1) == 2 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}}, err
	}
	b := fixtureBundle(t, "example.com/Acme/Widget", "v1.0.0", nil)
	assertCode(t, f.ingest(t, b, "v1.0.0", ""), 503)
	for _, kind := range []string{"info", "mod", "zip"} {
		assertCode(t, f.request("GET", testBase+"v1.0.0."+kind, nil, nil), 404)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial commit: %d %v", count, err)
	}
	f.service.Storage = original
	assertCode(t, f.ingest(t, b, "v1.0.0", ""), 201)
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("bundle commit: %d %v", count, err)
	}
}
func TestPostgresPackageOwner(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := fixtureBundle(t, "example.com/Acme/Widget", "v1.0.0", nil)
	assertCode(t, f.ingest(t, b, "v1.0.0", ""), 201)
	var other, component string
	if err := f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'first','local','go') RETURNING id`, f.instance).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, other); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'example.com/Acme/Widget') RETURNING id`, f.instance, other).Scan(&component); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("GET", testBase+"v1.0.0.zip", nil, nil), 404)
	response := f.request("GET", testBase+"list", nil, nil)
	assertCode(t, response, 200)
	if response.Body.Len() != 0 {
		t.Fatal("list fell through package owner")
	}
}
func TestPostgresGoModDownload(t *testing.T) {
	f := setup(t)
	b := fixtureBundle(t, "example.com/Acme/Widget", "v1.0.0", nil)
	assertCode(t, f.ingest(t, b, "v1.0.0", ""), 201)
	// The isolated loopback bridge supplies a service token because go refuses
	// credentials over HTTP. Real authenticated requests are tested above.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetBasicAuth("fixture", f.token)
		f.registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	cmd := exec.Command("go", "mod", "download", "-json", "example.com/Acme/Widget@v1.0.0")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPROXY="+server.URL+"/feeds/modules/go", "GONOSUMDB=example.com/Acme/*", "GOPRIVATE=example.com/Acme/*", "GONOPROXY=none", "GOWORK=off", "GOFLAGS=-modcacherw", "GOMODCACHE="+filepath.Join(dir, "modcache"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte(`"Sum": "h1:`)) {
		t.Fatal(string(output))
	}
}
