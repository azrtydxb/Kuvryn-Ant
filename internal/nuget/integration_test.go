package nuget

import (
	"archive/zip"
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
	"mime/multipart"
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
	schema := "ant_nuget_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "NuGet")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','nuget') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','nuget') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	if method == "PUT" {
		req.Header.Set("X-NuGet-ApiKey", f.token)
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

const testBase = "/feeds/packages/nuget/"

func packageBytes(t *testing.T, id, version, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, e := z.Create(id + ".nuspec")
	if e != nil {
		t.Fatal(e)
	}
	fmt.Fprintf(w, `<?xml version="1.0"?><package><metadata><id>%s</id><version>%s</version><authors>Fixture</authors><description>NuGet fixture</description></metadata></package>`, id, version)
	w, e = z.Create("content/readme.txt")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(w, "fixture"+extra)
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func (f fixture) push(t *testing.T, b []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	p, e := m.CreateFormFile("package", "ignored.nupkg")
	if e != nil {
		t.Fatal(e)
	}
	p.Write(b)
	m.Close()
	return f.request("PUT", testBase+"v2/package", body.Bytes(), map[string]string{"Content-Type": m.FormDataContentType()})
}
func TestPostgresRepository(t *testing.T) {
	f := setup(t)
	b := packageBytes(t, "Example.Widget", "01.2.0.0+build", "")
	assertCode(t, f.push(t, b), 201)
	assertCode(t, f.push(t, b), 201)
	assertCode(t, f.push(t, packageBytes(t, "example.widget", "1.2.0", "changed")), 409)
	for _, path := range []string{"v3/flatcontainer/example.widget/index.json", "v3/registration/example.widget/index.json", "v3/registration/example.widget/1.2.0.json", "v3/flatcontainer/example.widget/1.2.0/example.widget.nuspec", "v3/flatcontainer/example.widget/1.2.0/example.widget.1.2.0.nupkg"} {
		response := f.request("GET", testBase+path, nil, nil)
		assertCode(t, response, 200)
		head := f.request("HEAD", testBase+path, nil, nil)
		assertCode(t, head, 200)
		if head.Body.Len() != 0 || head.Header().Get("Content-Length") != response.Header().Get("Content-Length") {
			t.Fatal("HEAD mismatch")
		}
	}
	// Bearer and Basic read authentication share the service-token scopes.

	assertCode(t, f.request("GET", testBase+"v3/index.json", nil, map[string]string{"Authorization": "Bearer " + f.token}), 200)
	response := f.request("GET", testBase+"v3/flatcontainer/example.widget/index.json", nil, nil)
	if response.Body.String() != `{"versions":["1.2.0"]}` {
		t.Fatal(response.Body.String())
	}
	unconfigured, e := New(f.pool, f.service)
	if e != nil {
		t.Fatal(e)
	}
	request := httptest.NewRequest("GET", testBase+"v3/index.json", nil)
	request.SetBasicAuth("fixture", f.token)
	unconfiguredResponse := httptest.NewRecorder()
	unconfigured.ServeHTTP(unconfiguredResponse, request)
	assertCode(t, unconfiguredResponse, 503)
	spoof := httptest.NewRequest("GET", testBase+"v3/index.json", nil)
	spoof.Host = "attacker.invalid"
	spoof.Header.Set("X-Forwarded-Host", "attacker.invalid")
	spoof.SetBasicAuth("fixture", f.token)
	spoofResponse := httptest.NewRecorder()
	f.registry.ServeHTTP(spoofResponse, spoof)
	assertCode(t, spoofResponse, 200)
	if strings.Contains(spoofResponse.Body.String(), "attacker.invalid") {
		t.Fatal("untrusted host used")
	}
	response = f.request("GET", testBase+"v3/index.json", nil, nil)
	assertCode(t, response, 200)
	if !strings.Contains(response.Body.String(), "https://packages.example/") {
		t.Fatal("configured origin absent")
	}
	req := httptest.NewRequest("GET", testBase+"v3/index.json", nil)
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
	for _, path := range []string{"v3/flatcontainer/example.widget/index.json", "v3/registration/example.widget/index.json", "v3/flatcontainer/example.widget/1.2.0/example.widget.1.2.0.nupkg"} {
		assertCode(t, rf.request("GET", testBase+path, nil, nil), 404)
	}
	assertCode(t, rf.push(t, b), 403)
	// A higher-priority owner with no versions must never fall through.
	var source string
	ctx := context.Background()
	if e = f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'higher','local','nuget') RETURNING id`, f.instance).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'example.widget')`, f.instance, source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", testBase+"v3/flatcontainer/example.widget/index.json", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"v3/registration/example.widget/index.json", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"v3/flatcontainer/example.widget/1.2.0/example.widget.1.2.0.nupkg", nil, nil), 404)
}
func TestPostgresDotnetPushRestore(t *testing.T) {
	dotnet, e := exec.LookPath("dotnet")
	if e != nil {
		t.Skip("dotnet unavailable")
	}
	f := setup(t)
	// Loopback credential bridge isolates this client fixture from TLS/certificate
	// setup; direct Basic/Bearer/API-key authentication is covered separately.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			r.SetBasicAuth("fixture", f.token)
		}
		f.registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	if e = f.registry.SetPublicURL(server.URL); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	pkg := filepath.Join(dir, "example.widget.1.2.0.nupkg")
	if e = os.WriteFile(pkg, packageBytes(t, "Example.Widget", "1.2.0", ""), 0600); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, dotnet, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "DOTNET_CLI_HOME="+dir, "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1", "DOTNET_CLI_TELEMETRY_OPTOUT=1", "NUGET_PACKAGES="+filepath.Join(dir, "cache"))
		output, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("dotnet %s: %v\n%s", args[0], e, strings.ReplaceAll(string(output), f.token, "[redacted]"))
		}
	}
	run("nuget", "push", pkg, "--source", server.URL+testBase+"v3/index.json", "--api-key", f.token)
	project := `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net8.0</TargetFramework><NuGetAudit>false</NuGetAudit></PropertyGroup><ItemGroup><PackageReference Include="Example.Widget" Version="1.2.0" /></ItemGroup></Project>`
	if e = os.WriteFile(filepath.Join(dir, "fixture.csproj"), []byte(project), 0600); e != nil {
		t.Fatal(e)
	}
	run("restore", "fixture.csproj", "--source", server.URL+testBase+"v3/index.json", "--no-cache")
	if _, e = os.Stat(filepath.Join(dir, "cache", "example.widget", "1.2.0", "example.widget.1.2.0.nupkg")); e != nil {
		t.Fatal(e)
	}
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
	b := packageBytes(t, "Example.Widget", "1.0.0", "")
	assertCode(t, f.push(t, b), 503)
	assertCode(t, f.request("GET", testBase+"v3/flatcontainer/example.widget/index.json", nil, nil), 404)
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial commit: %d %v", count, err)
	}
	f.service.Storage = original
	assertCode(t, f.push(t, b), 201)
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("bundle commit: %d %v", count, err)
	}
}

func TestPostgresForeignInstance(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var instance, principal string
	if e := f.pool.QueryRow(ctx, `INSERT INTO instances(name) VALUES('foreign') RETURNING id`).Scan(&instance); e != nil {
		t.Fatal(e)
	}
	if e := f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','foreign') RETURNING id`, instance).Scan(&principal); e != nil {
		t.Fatal(e)
	}
	token, e := (identity.Tokens{DB: f.pool}).Issue(ctx, instance, principal, principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	f.token = token.Secret
	assertCode(t, f.request("GET", testBase+"v3/index.json", nil, nil), 404)
}
