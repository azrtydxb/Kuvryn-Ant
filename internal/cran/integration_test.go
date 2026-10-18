package cran

import (
	"bytes"
	"context"
	"crypto/rand"

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
	schema := "ant_cran_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "CRAN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','cran') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','cran') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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

const testBase = "/feeds/packages/cran/src/contrib/"

func TestPostgres(t *testing.T) {
	f := setup(t)
	data := packageBytes(t, "demo", "1.0", "")
	assertCode(t, f.request("PUT", testBase+"demo_1.0.tar.gz", data, nil), 201)
	assertCode(t, f.request("PUT", testBase+"demo_1.0.tar.gz", data, nil), 201)
	assertCode(t, f.request("PUT", testBase+"demo_1.0.tar.gz", packageBytes(t, "demo", "1.0", "changed"), nil), 409)
	w := f.request("GET", testBase+"PACKAGES", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "Package: demo\nVersion: 1.0\n") || !strings.Contains(w.Body.String(), "MD5sum:") {
		t.Fatal(w.Body.String())
	}
	w = f.request("GET", testBase+"demo_1.0.tar.gz", nil, nil)
	assertCode(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("archive changed")
	}
	assertCode(t, f.request("HEAD", testBase+"demo_1.0.tar.gz", nil, nil), 200)
	assertCode(t, f.request("PUT", testBase+"wrong_1.0.tar.gz", data, nil), 400)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, suffix := range []string{"one", "two"} {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			codes <- f.request("PUT", testBase+"race_1.0.tar.gz", packageBytes(t, "race", "1.0", s), nil).Code
		}(suffix)
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
	// A channel owner prevents a different source supplying even a different package.
	ctx := context.Background()
	var other string
	if e := f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'other','local','cran') RETURNING id`, f.instance).Scan(&other); e != nil {
		t.Fatal(e)
	}
	if _, e := f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, other); e != nil {
		t.Fatal(e)
	}
	// Persisted namespace mismatch fails closed, not a second-source fallback.
	if _, e := f.pool.Exec(ctx, `UPDATE components SET namespace='wrong' WHERE source_id=$1`, f.source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", testBase+"demo_1.0.tar.gz", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"PACKAGES", nil, nil), 503)
}
