package helmrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
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
	"go.yaml.in/yaml/v3"
)

type repoFixture struct {
	pool                 *pgxpool.Pool
	service              *artifact.Service
	principal            identity.Principal
	feed, source, domain string
	disk                 *blob.Disk
	secret               string
}

func newRepoFixture(t *testing.T) repoFixture {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_helm_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Artifacts")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := (identity.Tokens{DB: pool}).Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	f := repoFixture{pool: pool, principal: principal, secret: token.Secret}
	if err = pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'charts','helm') RETURNING id", owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','helm') RETURNING id", owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id", owner.InstanceID, "source:"+f.source).Scan(&f.domain); err != nil {
		t.Fatal(err)
	}
	lease, err := ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000001", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.disk, err = blob.NewDisk(filepath.Join(t.TempDir(), "blobs"), blob.TransferBufferSize*8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.disk.Close(); err != nil {
			t.Error(err)
		}
	})
	compiled, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: f.source, Kind: routing.Local}}, DefaultTarget: f.source})
	if err != nil {
		t.Fatal(err)
	}
	f.service = &artifact.Service{DB: pool, Lease: func() ownership.Lease { return lease }, Router: func(context.Context, string) (*routing.Router, error) { return compiled, nil }, Storage: func(_ context.Context, domain string) (blob.Store, error) {
		if domain != f.domain {
			return nil, errors.New("unexpected domain")
		}
		return f.disk, nil
	}}
	return f
}
func (f repoFixture) request(t *testing.T, h http.Handler, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.SetBasicAuth("helm", f.secret)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func (f repoFixture) upload(t *testing.T, h http.Handler, name, version string, chart, prov []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	part, err := m.CreateFormFile("chart", name+"-"+version+".tgz")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(chart)
	if prov != nil {
		part, err = m.CreateFormFile("provenance", name+"-"+version+".tgz.prov")
		if err != nil {
			t.Fatal(err)
		}
		part.Write(prov)
	}
	m.Close()
	return f.request(t, h, "POST", "/feeds/charts/helm/charts/"+name+"/"+version, &body, m.FormDataContentType())
}
func TestRepositoryPostgres(t *testing.T) {
	f := newRepoFixture(t)
	h := New(f.pool, f.service)
	data := goodChart(t, "demo", "1.2.3")
	prov := provenance(data, "demo", "1.2.3")
	for i := 0; i < 2; i++ {
		w := f.upload(t, h, "demo", "1.2.3", data, prov)
		if w.Code != 201 {
			t.Fatalf("publish %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"signatureVerified":false`) {
			t.Fatal("signature status omitted")
		}
	}
	var n int
	f.pool.QueryRow(context.Background(), "SELECT count(*) FROM assets").Scan(&n)
	if n != 3 {
		t.Fatalf("bundle assets %d", n)
	}
	bad := bytes.Replace(prov, []byte(string(digestInfo(data).Digest)), []byte(strings.Repeat("0", 64)), 1)
	if w := f.upload(t, h, "demo", "1.2.3", data, bad); w.Code != 400 {
		t.Fatalf("provenance mismatch %d", w.Code)
	}
	if w := f.upload(t, h, "demo", "1.2.3", data, nil); w.Code != 409 {
		t.Fatalf("partial retry %d", w.Code)
	}
	w := f.request(t, h, "GET", "/feeds/charts/helm/index.yaml", nil, "")
	if w.Code != 200 {
		t.Fatalf("index %d %s", w.Code, w.Body.String())
	}
	var index indexFile
	if err := yaml.Unmarshal(w.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Entries["demo"]) != 1 {
		t.Fatalf("index %s", w.Body.String())
	}
	urls := index.Entries["demo"][0]["urls"].([]any)
	path := "/feeds/charts/helm/" + urls[0].(string)
	for suffix, expected := range map[string][]byte{"": data, ".prov": prov} {
		w = f.request(t, h, "GET", path+suffix, nil, "")
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), expected) {
			t.Fatalf("download %s %d", suffix, w.Code)
		}
	}
	r := httptest.NewRequest("GET", path, nil)
	r.SetBasicAuth("helm", f.secret)
	r.Header.Set("Range", "bytes=2-9")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), data[2:10]) {
		t.Fatalf("range %d %q", w.Code, w.Body.Bytes())
	}
	etag := w.Header().Get("ETag")
	r.Header.Del("Range")
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
	r.Header.Del("If-None-Match")
	r.Header.Set("Range", "bytes=999999-")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 416 {
		t.Fatal(w.Code)
	}
	w = f.request(t, h, "HEAD", path, nil, "")
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD")
	}
	// Unauthorized access never returns index metadata or chart bytes.
	r = httptest.NewRequest("GET", path, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	// Read-only service tokens cannot invoke administrative ingestion.
	tok, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	reader := f
	reader.secret = tok.Secret
	if w = reader.upload(t, h, "demo", "1.2.3", data, prov); w.Code != 403 {
		t.Fatal(w.Code)
	}
	// A malformed archive adds no visible metadata.
	if w = f.upload(t, h, "bad", "1.0.0", []byte("bad tar"), nil); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if err = f.pool.QueryRow(context.Background(), "SELECT count(*) FROM components WHERE coordinate='bad'").Scan(&n); err != nil || n != 0 {
		t.Fatal("invalid chart published")
	}
}

func TestPackageOwnerPostgres(t *testing.T) {
	f := newRepoFixture(t)
	h := New(f.pool, f.service)
	data := goodChart(t, "demo", "1.0.0")
	if w := f.upload(t, h, "demo", "1.0.0", data, nil); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx := context.Background()
	var other, domain string
	if err := f.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'other','local','helm') RETURNING id", f.principal.InstanceID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,1)", f.principal.InstanceID, f.feed, other); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id", f.principal.InstanceID, "source:"+other).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	disk, err := blob.NewDisk(filepath.Join(t.TempDir(), "other"), blob.TransferBufferSize*8)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	storage := f.service.Storage
	f.service.Storage = func(ctx context.Context, d string) (blob.Store, error) {
		if d == domain {
			return disk, nil
		}
		return storage(ctx, d)
	}
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: other, Kind: routing.Local}}, DefaultTarget: other})
	if err != nil {
		t.Fatal(err)
	}
	f.service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	data = goodChart(t, "demo", "2.0.0")
	if w := f.upload(t, h, "demo", "2.0.0", data, nil); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := f.request(t, h, "GET", "/feeds/charts/helm/index.yaml", nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "2.0.0") {
		t.Fatalf("cross-source index %d %s", w.Code, w.Body.String())
	}
	if w = f.request(t, h, "GET", "/feeds/charts/helm/charts/demo/2.0.0/demo-2.0.0.tgz", nil, ""); w.Code != 404 {
		t.Fatalf("version fallback %d", w.Code)
	}
	// A previously generated source-bound URL fails closed after owner changes.
	var index indexFile
	yaml.Unmarshal(f.request(t, h, "GET", "/feeds/charts/helm/index.yaml", nil, "").Body.Bytes(), &index)
	pinned := "/feeds/charts/helm/" + index.Entries["demo"][0]["urls"].([]any)[0].(string)
	if _, err = f.pool.Exec(ctx, "DELETE FROM feed_sources WHERE feed_id=$1 AND source_id=$2", f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	w = f.request(t, h, "GET", pinned, nil, "")
	if w.Code != 404 && w.Code != 409 {
		t.Fatalf("stale owner download %d", w.Code)
	}
}

func TestRealHelmClientPostgres(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm executable not installed")
	}
	f := newRepoFixture(t)
	h := New(f.pool, f.service)
	server := httptest.NewServer(h)
	defer server.Close()
	root := t.TempDir()
	chart := filepath.Join(root, "demo")
	if err = os.MkdirAll(filepath.Join(chart, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"Chart.yaml": "apiVersion: v2\nname: demo\nversion: 1.2.3\ndescription: Helm client fixture\n", "templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n"} {
		if err = os.WriteFile(filepath.Join(chart, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(helm, args...)
		cmd.Env = append(os.Environ(), "HELM_CONFIG_HOME="+filepath.Join(root, "config"), "HELM_CACHE_HOME="+filepath.Join(root, "cache"), "HELM_DATA_HOME="+filepath.Join(root, "data"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helm %s: %v %s", args[0], err, out)
		}
		return out
	}
	t.Logf("Helm client: %s", run("version", "--short"))
	run("package", chart, "--destination", root)
	data, err := os.ReadFile(filepath.Join(root, "demo-1.2.3.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if w := f.upload(t, h, "demo", "1.2.3", data, nil); w.Code != 201 {
		t.Fatalf("ingest %d %s", w.Code, w.Body.String())
	}
	run("repo", "add", "fixture", server.URL+"/feeds/charts/helm", "--username", "helm", "--password", f.secret)
	run("repo", "update", "fixture")
	dest := filepath.Join(root, "download")
	os.Mkdir(dest, 0700)
	run("pull", "fixture/demo", "--version", "1.2.3", "--destination", dest)
	pulled, err := os.ReadFile(filepath.Join(dest, "demo-1.2.3.tgz"))
	if err != nil || !bytes.Equal(pulled, data) {
		t.Fatal("Helm pull changed chart bytes", err)
	}

	parent := filepath.Join(root, "parent")
	if err = os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	parentMetadata := "apiVersion: v2\nname: parent\nversion: 1.0.0\ndependencies:\n  - name: demo\n    version: 1.2.3\n    repository: '@fixture'\n"
	if err = os.WriteFile(filepath.Join(parent, "Chart.yaml"), []byte(parentMetadata), 0600); err != nil {
		t.Fatal(err)
	}
	run("dependency", "update", parent)
	if _, err = os.Stat(filepath.Join(parent, "charts", "demo-1.2.3.tgz")); err != nil {
		t.Fatal("dependency download", err)
	}
	run("template", "parent-release", parent)
	out := run("template", "fixture-release", filepath.Join(dest, "demo-1.2.3.tgz"))
	if !bytes.Contains(out, []byte("name: fixture-release")) {
		t.Fatal("Helm template output missing fixture")
	}
}

func TestInstanceAndIndexByteBound(t *testing.T) {
	f := newRepoFixture(t)
	h := New(f.pool, f.service)
	if w := f.upload(t, h, "demo", "1.2.3", goodChart(t, "demo", "1.2.3"), nil); w.Code != 201 {
		t.Fatal(w.Code)
	}
	h.Limits.IndexBytes = 1
	if w := f.request(t, h, "GET", "/feeds/charts/helm/index.yaml", nil, ""); w.Code != 503 {
		t.Fatal("index byte bound", w.Code)
	}
	h.instance = "another-instance"
	if w := f.request(t, h, "GET", "/feeds/charts/helm/index.yaml", nil, ""); w.Code != 404 {
		t.Fatal("instance isolation", w.Code)
	}
}
