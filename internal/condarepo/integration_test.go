package condarepo

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

type condaFixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Repository
	service                                        *artifact.Service
	root, token, instance, principal, feed, source string
}

func condaSetup(t *testing.T) condaFixture {
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
	schema := "ant_conda_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Conda")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := condaFixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','conda') RETURNING id::text`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','conda') RETURNING id::text`, owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, owner.InstanceID, "source:"+f.source); err != nil {
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

func condaRequest(t *testing.T, f condaFixture, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
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

func condaStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}

// packageBytes builds a minimal conda package (info/index.json plus a bin
// member) compressed with the system bzip2, matching the native wire format.
func packageBytes(t *testing.T, name, version, build, subdir string) ([]byte, string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"name": name, "version": version, "build": build, "build_number": 0, "subdir": subdir, "depends": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	dir := name + "-" + version
	write := func(n string, data []byte) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	_ = dir
	write("info/index.json", meta)
	write("bin/"+"tool", []byte("#!/bin/sh\necho "+name+"\n"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(tarBuf.Bytes())
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Skipf("bzip2 unavailable: %v", err)
	}
	return out.Bytes(), name + "-" + version + "-" + build + ".tar.bz2"
}

const condaBase = "/feeds/packages/conda/linux-64"

func TestCondaPostgresLifecycle(t *testing.T) {
	f := condaSetup(t)
	data, filename := packageBytes(t, "demo", "1.0", "0", "linux-64")
	path := "/feeds/packages/conda/ingest/linux-64/" + filename
	condaStatus(t, condaRequest(t, f, "PUT", path, data, nil), 201)
	condaStatus(t, condaRequest(t, f, "PUT", path, data, nil), 201)
	conflicting, _ := packageBytes(t, "demo", "1.0", "0", "linux-64")
	_ = conflicting[:3]
	truncated := data[:3]
	condaStatus(t, condaRequest(t, f, "PUT", path, truncated, nil), 400)
	// repodata.json includes sha256 and size for native clients.
	response := condaRequest(t, f, "GET", condaBase+"/repodata.json", nil, nil)
	condaStatus(t, response, 200)
	var index map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	packages := index["packages"].(map[string]any)
	item, ok := packages[filename].(map[string]any)
	if !ok || item["sha256"] == "" || item["size"] == nil {
		t.Fatal(index)
	}
	// The immutable archive downloads byte-identically with ETag.
	response = condaRequest(t, f, "GET", condaBase+"/"+filename, nil, nil)
	condaStatus(t, response, 200)
	if !bytes.Equal(response.Body.Bytes(), data) || response.Header().Get("ETag") == "" {
		t.Fatal("archive mismatch")
	}
	condaStatus(t, condaRequest(t, f, "HEAD", condaBase+"/"+filename, nil, nil), 200)
	// An empty channel is served to satisfy native noarch probing.
	response = condaRequest(t, f, "GET", "/feeds/packages/conda/noarch/repodata.json", nil, nil)
	condaStatus(t, response, 200)
	json.Unmarshal(response.Body.Bytes(), &index)
	if len(index["packages"].(map[string]any)) != 0 {
		t.Fatal(index)
	}
	// Namespace mismatch and unknown subdirs fail closed.
	badSubdir, _ := packageBytes(t, "demo", "2.0", "0", "linux-64")
	condaStatus(t, condaRequest(t, f, "PUT", "/feeds/packages/conda/ingest/osx-64/demo-2.0-0.tar.bz2", badSubdir, nil), 400)
	condaStatus(t, condaRequest(t, f, "GET", "/feeds/packages/conda/badsubdir/repodata.json", nil, nil), 404)
}

func TestCondaPostgresIsolationAndBounds(t *testing.T) {
	f := condaSetup(t)
	// Unknown slug, wrong format and missing auth fail closed.
	condaStatus(t, condaRequest(t, f, "GET", "/feeds/other/conda/linux-64/repodata.json", nil, nil), 404)
	req := httptest.NewRequest("GET", condaBase+"/repodata.json", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	// An invalid metadata JSON cannot publish.
	bad, badName := packageBytes(t, "demo", "1.0", "0", "linux-64")
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "info/index.json", Mode: 0600, Size: 3}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("{}\xff"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(tarBuf.Bytes())
	var badOut bytes.Buffer
	cmd.Stdout = &badOut
	if err := cmd.Run(); err == nil {
		condaStatus(t, condaRequest(t, f, "PUT", "/feeds/packages/conda/ingest/linux-64/"+badName, badOut.Bytes(), nil), 400)
	} else {
		_ = bad
	}
	// Cross-validation: a filename that disagrees with its metadata rejects.
	condaStatus(t, condaRequest(t, f, "PUT", "/feeds/packages/conda/ingest/linux-64/other-1.0-0.tar.bz2", bad, nil), 400)
	ctx := context.Background()
	var published int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM versions`).Scan(&published); err != nil || published != 0 {
		t.Fatal("rejected package published", published, err)
	}
	_ = io.Discard
}
