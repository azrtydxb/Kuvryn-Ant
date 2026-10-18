package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
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
	schema := "ant_composer_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Composer")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','composer') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','composer') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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

const testBase = "/feeds/packages/composer/"

func packageBytes(t *testing.T, id, version, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, e := z.Create("composer.json")
	if e != nil {
		t.Fatal(e)
	}
	fmt.Fprintf(w, `{"name":%q,"version":%q,"type":"library","require":{"php":">=8.0"},"autoload":{"psr-4":{"Acme\\":"src/"}}}`, id, version)
	w, e = z.Create("src/Fixture.php")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(w, "<?php namespace Acme; class Fixture {} //"+extra)
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func (f fixture) push(t *testing.T, b []byte) *httptest.ResponseRecorder {
	return f.request("PUT", testBase+"ingest/acme/widget/1.2.0", b, nil)
}
func TestPostgresRepository(t *testing.T) {
	f := setup(t)
	b := packageBytes(t, "acme/widget", "1.2.0", "")
	assertCode(t, f.push(t, b), 201)
	assertCode(t, f.push(t, b), 201)
	assertCode(t, f.push(t, packageBytes(t, "acme/widget", "1.2.0", "different")), 409)
	assertCode(t, f.push(t, packageBytes(t, "acme/wrong", "1.2.0", "")), 400)
	for _, path := range []string{"packages.json", "p2/acme/widget.json", "p2/acme/widget~dev.json", "dist/acme/widget/1.2.0/archive.zip"} {
		r := f.request("GET", testBase+path, nil, nil)
		assertCode(t, r, 200)
		h := f.request("HEAD", testBase+path, nil, nil)
		assertCode(t, h, 200)
		if h.Body.Len() != 0 || h.Header().Get("Content-Length") != r.Header().Get("Content-Length") {
			t.Fatal("HEAD mismatch")
		}
	}
	assertCode(t, f.request("GET", testBase+"packages.json", nil, map[string]string{"Authorization": "Bearer " + f.token}), 200)
	r := f.request("GET", testBase+"p2/acme/widget.json", nil, nil)
	if !strings.Contains(r.Body.String(), "https://packages.example/feeds/packages/composer/dist/acme/widget/1.2.0/archive.zip") {
		t.Fatal(r.Body.String())
	}
	req := httptest.NewRequest("GET", testBase+"packages.json", nil)
	req = req.WithContext(identity.WithAuthenticatedPrincipal(req.Context(), identity.Principal{InstanceID: f.instance, PrincipalID: f.principal}))
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 401)
	denied, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Namespace: "other", Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	rf := f
	rf.token = denied.Secret
	assertCode(t, rf.request("GET", testBase+"p2/acme/widget.json", nil, nil), 404)
	assertCode(t, rf.push(t, b), 403)
	var source string
	ctx := context.Background()
	if e = f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'higher','local','composer') RETURNING id`, f.instance).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,'acme/widget','acme')`, f.instance, source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", testBase+"p2/acme/widget.json", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"dist/acme/widget/1.2.0/archive.zip", nil, nil), 404)
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
	b := packageBytes(t, "acme/widget", "1.2.0", "")
	assertCode(t, f.push(t, b), 503)
	assertCode(t, f.request("GET", testBase+"p2/acme/widget.json", nil, nil), 404)
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial commit: %d %v", count, err)
	}
	f.service.Storage = original
	assertCode(t, f.push(t, b), 201)
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("bundle commit: %d %v", count, err)
	}
}

func TestPostgresConcurrentImmutable(t *testing.T) {
	f := setup(t)
	bodies := [][]byte{packageBytes(t, "acme/widget", "1.2.0", "one"), packageBytes(t, "acme/widget", "1.2.0", "two")}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, b := range bodies {
		wg.Add(1)
		go func(b []byte) {
			defer wg.Done()
			codes <- f.request("PUT", testBase+"ingest/acme/widget/1.2.0", b, nil).Code
		}(b)
	}
	wg.Wait()
	close(codes)
	created, conflict := 0, 0
	for code := range codes {
		if code == 201 {
			created++
		}
		if code == 409 {
			conflict++
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("created=%d conflict=%d", created, conflict)
	}
}

func TestPostgresBoundaries(t *testing.T) {
	f := setup(t)
	b := packageBytes(t, "acme/widget", "1.2.0", "")
	assertCode(t, f.request("PUT", testBase+"ingest/acme/widget/1.2.0", b, map[string]string{"X-Checksum-Sha256": strings.Repeat("0", 64)}), 400)
	assertCode(t, f.request("GET", testBase+"p2/acme/widget.json", nil, nil), 404)
	assertCode(t, f.push(t, b), 201)
	req := httptest.NewRequest("GET", testBase+"packages.json", nil)
	req.Host = "attacker.invalid"
	req.Header.Set("X-Forwarded-Host", "attacker.invalid")
	req.SetBasicAuth("fixture", f.token)
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), "attacker.invalid") {
		t.Fatal("untrusted public origin")
	}
	r, e := New(f.pool, f.service)
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assertCode(t, w, 503)
	// A different leased instance cannot reuse this adapter even with a valid token.
	ctx := context.Background()
	var instance, principal string
	if e = f.pool.QueryRow(ctx, `INSERT INTO instances(name) VALUES('foreign') RETURNING id`).Scan(&instance); e != nil {
		t.Fatal(e)
	}
	if e = f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','foreign') RETURNING id`, instance).Scan(&principal); e != nil {
		t.Fatal(e)
	}
	token, e := (identity.Tokens{DB: f.pool}).Issue(ctx, instance, principal, principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	rf := f
	rf.token = token.Secret
	assertCode(t, rf.request("GET", testBase+"packages.json", nil, nil), 404)
}
