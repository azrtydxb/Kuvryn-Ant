package cargo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
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
	schema := "ant_cargo_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Cargo")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'crates','cargo') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','cargo') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	req.Header.Set("Authorization", f.token)
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
func crateArchive(t *testing.T, name, version string, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	files := map[string]string{name + "-" + version + "/Cargo.toml": "[package]\nname = \"" + name + "\"\nversion = \"" + version + "\"\nedition = \"2021\"\n", name + "-" + version + "/src/lib.rs": "pub fn hello() {}\n"}
	if extra != "" {
		files[extra] = "x"
	}
	for name, data := range files {
		if e := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); e != nil {
			t.Fatal(e)
		}
		tw.Write([]byte(data))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}
func wire(m metadata, crate []byte) []byte {
	var b bytes.Buffer
	data, _ := json.Marshal(m)
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	binary.Write(&b, binary.LittleEndian, uint32(len(crate)))
	b.Write(crate)
	return b.Bytes()
}
func TestPublishIndexDownload(t *testing.T) {
	f := setup(t)
	base := "/feeds/crates/cargo/"
	m := metadata{Name: "demo", Version: "1.0.0"}
	crate := crateArchive(t, m.Name, m.Version, "")
	body := wire(m, crate)
	assertCode(t, f.request("GET", base+"config.json", nil, map[string]string{"Authorization": ""}), 401)
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", body, map[string]string{"Authorization": "invalid"}), 403)
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", body, nil), 200)
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", body, nil), 200)
	response := f.request("GET", base+indexPath("demo"), nil, nil)
	assertCode(t, response, 200)
	var row map[string]any
	if e := json.Unmarshal(bytes.TrimSpace(response.Body.Bytes()), &row); e != nil {
		t.Fatal(e)
	}
	if row["cksum"] != string(bytesInfo(crate).Digest) {
		t.Fatal("checksum mismatch")
	}
	assertCode(t, f.request("GET", base+indexPath("demo"), nil, map[string]string{"If-None-Match": response.Header().Get("ETag")}), 304)
	downloaded := f.request("GET", base+"api/v1/crates/demo/1.0.0/download", nil, nil)
	assertCode(t, downloaded, 200)
	if !bytes.Equal(downloaded.Body.Bytes(), crate) {
		t.Fatal("download changed")
	}
	// Yank is the core's unlist: policy-gated, audited and visible in the index.
	assertCode(t, f.request("DELETE", base+"api/v1/crates/demo/1.0.0/yank", nil, nil), 200)
	if yanked := f.request("GET", base+indexPath("demo"), nil, nil); !bytes.Contains(yanked.Body.Bytes(), []byte(`"yanked":true`)) || yanked.Header().Get("ETag") == response.Header().Get("ETag") {
		t.Fatal("yank not reflected in index")
	}
	assertCode(t, f.request("GET", base+"api/v1/crates/demo/1.0.0/download", nil, nil), 200)
	assertCode(t, f.request("PUT", base+"api/v1/crates/demo/1.0.0/unyank", nil, nil), 200)
	if unyanked := f.request("GET", base+indexPath("demo"), nil, nil); !bytes.Contains(unyanked.Body.Bytes(), []byte(`"yanked":false`)) {
		t.Fatal("unyank not reflected in index")
	}
	if _, e := f.pool.Exec(context.Background(), `INSERT INTO delete_policies(instance_id,feed_id,mode) VALUES($1,$2,'denied')`, f.instance, f.feed); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", base+"api/v1/crates/demo/1.0.0/yank", nil, nil), 403)
	if _, e := f.pool.Exec(context.Background(), `DELETE FROM delete_policies`); e != nil {
		t.Fatal(e)
	}
	m.Version = "1.0.0+other"
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", wire(m, crateArchive(t, m.Name, m.Version, "")), nil), 409)
	m.Version = "2.0.0"
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", wire(m, crateArchive(t, m.Name, m.Version, "../escape")), nil), 400)
	var n int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&n); e != nil || n != 2 {
		t.Fatalf("partial publication: %d %v", n, e)
	}
	// A read-only token cannot accept archive bytes or publish metadata.
	token, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", body, map[string]string{"Authorization": token.Secret}), 403)
	// A publish-only token (no delete) may yank and unyank: reversible, publisher-level.
	pub, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", base+"api/v1/crates/demo/1.0.0/yank", nil, map[string]string{"Authorization": pub.Secret}), 200)
	assertCode(t, f.request("PUT", base+"api/v1/crates/demo/1.0.0/unyank", nil, map[string]string{"Authorization": pub.Secret}), 200)
	assertCode(t, f.request("DELETE", base+"api/v1/crates/demo/1.0.0/yank", nil, map[string]string{"Authorization": token.Secret}), 403)
}
func TestRealCargoPublishFetch(t *testing.T) {
	if _, e := exec.LookPath("cargo"); e != nil {
		t.Skip("cargo unavailable")
	}
	f := setup(t)
	server := httptest.NewServer(f.registry)
	if err := f.registry.SetPublicURL(server.URL); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0700)
	config := "[registries.ant]\nindex = \"sparse+" + server.URL + "/feeds/crates/cargo/\"\ncredential-provider = \"cargo:token\"\n"
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("cargo", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CARGO_HOME="+home, "CARGO_REGISTRIES_ANT_TOKEN="+f.token)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("cargo %v: %v\n%s", args, e, out)
		}
	}
	pkg := filepath.Join(root, "pkg")
	os.MkdirAll(filepath.Join(pkg, "src"), 0700)
	os.WriteFile(filepath.Join(pkg, "Cargo.toml"), []byte("[package]\nname=\"cargo-fixture\"\nversion=\"1.0.0\"\nedition=\"2021\"\n"), 0600)
	os.WriteFile(filepath.Join(pkg, "src", "lib.rs"), []byte("pub fn hello() {}"), 0600)
	run(pkg, "publish", "--registry", "ant", "--allow-dirty", "--no-verify")
	consumer := filepath.Join(root, "consumer")
	os.MkdirAll(filepath.Join(consumer, "src"), 0700)
	os.WriteFile(filepath.Join(consumer, "Cargo.toml"), []byte("[package]\nname=\"consumer\"\nversion=\"1.0.0\"\nedition=\"2021\"\n[dependencies]\ncargo-fixture={version=\"1.0.0\",registry=\"ant\"}\n"), 0600)
	os.WriteFile(filepath.Join(consumer, "src", "lib.rs"), []byte("pub fn run(){cargo_fixture::hello();}"), 0600)
	run(consumer, "fetch")
	run(consumer, "check", "--offline")
}
func TestPackageOwnerNeverMixesVersions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	base := "/feeds/crates/cargo/"
	m := metadata{Name: "demo", Version: "1.0.0"}
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", wire(m, crateArchive(t, m.Name, m.Version, "")), nil), 200)
	var second string
	if e := f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'second','local','cargo') RETURNING id`, f.instance).Scan(&second); e != nil {
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
	m.Version = "2.0.0"
	assertCode(t, f.request("PUT", base+"api/v1/crates/new", wire(m, crateArchive(t, m.Name, m.Version, "")), nil), 200)
	response := f.request("GET", base+indexPath(m.Name), nil, nil)
	assertCode(t, response, 200)
	if strings.Contains(response.Body.String(), "2.0.0") {
		t.Fatal("mixed owners")
	}
	assertCode(t, f.request("GET", base+"api/v1/crates/demo/2.0.0/download", nil, nil), 404)
}

func TestConfiguredOriginIgnoresRequestHost(t *testing.T) {
	f := setup(t)
	path := "/feeds/crates/cargo/config.json"
	assertCode(t, f.request("GET", path, nil, nil), 503)
	if err := f.registry.SetPublicURL("https://registry.example"); err != nil {
		t.Fatal(err)
	}
	response := f.request("GET", path, nil, map[string]string{"Forwarded": "proto=http;host=attacker.example", "X-Forwarded-Host": "attacker.example"})
	assertCode(t, response, 200)
	var config map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if config["api"] != "https://registry.example/feeds/crates/cargo" {
		t.Fatal("untrusted request influenced origin", config)
	}
}
