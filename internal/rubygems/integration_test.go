package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"sync"
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
	schema := "ant_rubygems_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "RubyGems")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','rubygems') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','rubygems') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	if method == "POST" {
		req.Header.Set("Authorization", f.token)
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

const testBase = "/feeds/packages/rubygems/"

func packageBytes(t *testing.T, name, version, platform, extra string) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	payload := []byte("# fixture\n" + extra)
	tw.WriteHeader(&tar.Header{Name: "lib/" + name + ".rb", Mode: 0644, Size: int64(len(payload))})
	tw.Write(payload)
	tw.Close()
	gz.Close()
	meta := fmt.Sprintf(`--- !ruby/object:Gem::Specification
name: %s
version: !ruby/object:Gem::Version
  version: %s
platform: %s
summary: fixture
`, name, version, platform)
	var mb bytes.Buffer
	mg := gzip.NewWriter(&mb)
	mg.Write([]byte(meta))
	mg.Close()
	var out bytes.Buffer
	outer := tar.NewWriter(&out)
	for _, item := range []struct {
		name string
		data []byte
	}{{"metadata.gz", mb.Bytes()}, {"data.tar.gz", data.Bytes()}} {
		outer.WriteHeader(&tar.Header{Name: item.name, Mode: 0644, Size: int64(len(item.data))})
		outer.Write(item.data)
	}
	outer.Close()
	return out.Bytes()
}
func (f fixture) push(t *testing.T, b []byte) *httptest.ResponseRecorder {
	t.Helper()
	return f.request("POST", testBase+"api/v1/gems", b, nil)
}
func TestPostgresRepository(t *testing.T) {
	f := setup(t)
	b := packageBytes(t, "example", "1.2.0", "ruby", "")
	assertCode(t, f.push(t, b), 201)
	assertCode(t, f.push(t, b), 201)
	assertCode(t, f.push(t, packageBytes(t, "example", "1.2.0", "ruby", "changed")), 409)
	assertCode(t, f.push(t, packageBytes(t, "example", "1.2.0", "arm64-darwin", "")), 201)
	for _, path := range []string{"names", "versions", "info/example", "specs.4.8.gz", "prerelease_specs.4.8.gz", "quick/Marshal.4.8/example-1.2.0.gemspec.rz", "gems/example-1.2.0.gem", "gems/example-1.2.0-arm64-darwin.gem"} {
		res := f.request("GET", testBase+path, nil, nil)
		assertCode(t, res, 200)
		h := f.request("HEAD", testBase+path, nil, nil)
		assertCode(t, h, 200)
		if h.Body.Len() != 0 || h.Header().Get("Content-Length") != res.Header().Get("Content-Length") {
			t.Fatal("HEAD mismatch")
		}
	}
	res := f.request("GET", testBase+"gems/example-1.2.0.gem", nil, nil)
	if !bytes.Equal(res.Body.Bytes(), b) {
		t.Fatal("gem changed")
	}
	partial := f.request("GET", testBase+"gems/example-1.2.0.gem", nil, map[string]string{"Range": "bytes=0-63"})
	assertCode(t, partial, 206)
	if !bytes.Equal(partial.Body.Bytes(), b[:64]) {
		t.Fatal("range bytes mismatch")
	}
	assertCode(t, f.request("GET", testBase+"gems/example-1.2.0.gem", nil, map[string]string{"Range": "bytes=9999999-"}), 416)
	etag := f.request("GET", testBase+"versions", nil, nil).Header().Get("ETag")
	assertCode(t, f.request("GET", testBase+"versions", nil, map[string]string{"If-None-Match": etag}), 304)
	assertCode(t, f.request("GET", testBase+"info/example", nil, map[string]string{"Range": "bytes=0-2"}), 206)
	assertCode(t, f.request("GET", testBase+"versions", nil, map[string]string{"Authorization": ""}), 401)
	denied, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Namespace: "other", Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	rf := f
	rf.token = denied.Secret
	assertCode(t, rf.request("GET", testBase+"versions", nil, nil), 404)
	assertCode(t, rf.push(t, b), 403)
	assertCode(t, f.push(t, []byte("bad")), 400)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- f.push(t, packageBytes(t, "race", "1.0", "ruby", fmt.Sprint(i))).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	sum := 0
	for c := range codes {
		sum += c
	}
	if sum != 610 {
		t.Fatalf("race statuses sum %d", sum)
	}
	var source string
	ctx := context.Background()
	if e = f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'higher','local','rubygems') RETURNING id`, f.instance).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'example')`, f.instance, source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", testBase+"info/example", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"gems/example-1.2.0.gem", nil, nil), 404)
}
func TestPostgresNativeClient(t *testing.T) {
	ruby, e := exec.LookPath("ruby")
	if e != nil {
		t.Skip("Ruby unavailable")
	}
	f := setup(t)
	var requestsMu sync.Mutex
	requests := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests[r.Method+" "+r.URL.Path] = true
		requestsMu.Unlock()
		t.Log(r.Method, r.URL.Path)
		f.registry.ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, ruby, args...)
		cmd.Dir = dir
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "LANG=en_US.UTF-8", "TMPDIR=" + dir}, "HOME="+dir, "GEM_HOME="+filepath.Join(dir, "installed"), "GEM_PATH="+filepath.Join(dir, "installed"), "GEM_HOST_API_KEY="+f.token)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("native Ruby: %v\n%s", e, strings.ReplaceAll(string(out), f.token, "[redacted]"))
		}
		t.Log(strings.ReplaceAll(string(out), f.token, "[redacted]"))
	}
	// Build trusted fixture gemspecs with the actual client. The adapter never
	// runs Ruby. Native authentication is tested without a credential bridge.
	if e = os.WriteFile(filepath.Join(dir, "example.gemspec"), []byte(`Gem::Specification.new do |s|
s.name="ant-example";s.version="1.2.0";s.summary="fixture";s.authors=["Fixture"];s.files=["lib/ant-example.rb"];s.add_dependency "ant-dep", ">= 1", "< 2"
end`), 0600); e != nil {
		t.Fatal(e)
	}
	os.Mkdir(filepath.Join(dir, "lib"), 0700)
	os.WriteFile(filepath.Join(dir, "lib", "ant-example.rb"), []byte("# fixture\n"), 0600)
	os.WriteFile(filepath.Join(dir, "dep.gemspec"), []byte(`Gem::Specification.new do |s|;s.name="ant-dep";s.version="1.0.0";s.summary="fixture";s.authors=["Fixture"];s.files=["lib/ant-dep.rb"];end`), 0600)
	os.WriteFile(filepath.Join(dir, "lib", "ant-dep.rb"), []byte("# dep\n"), 0600)
	// Invoke the installed gem executable through Ruby, preserving that runtime.
	gem, e := exec.LookPath("gem")
	if e != nil {
		t.Fatal(e)
	}
	run(gem, "--version")
	run(gem, "build", "dep.gemspec")
	run(gem, "build", "example.gemspec")
	run("-rrubygems/package", "-e", `s=Gem::Specification.load("dep.gemspec");s.platform=Gem::Platform.local;Gem::Package.build(s)`)
	variants, e := filepath.Glob(filepath.Join(dir, "ant-dep-1.0.0-*.gem"))
	if e != nil || len(variants) != 1 {
		t.Fatalf("native variant missing: %v", e)
	}
	run(gem, "push", variants[0], "--host", server.URL+testBase)
	run(gem, "push", "ant-dep-1.0.0.gem", "--host", server.URL+testBase)
	run(gem, "push", "ant-example-1.2.0.gem", "--host", server.URL+testBase)
	// Remove locally built gems so gem install must download both releases.
	built, e := filepath.Glob(filepath.Join(dir, "*.gem"))
	if e != nil {
		t.Fatal(e)
	}
	for _, file := range built {
		if e = os.Remove(file); e != nil {
			t.Fatal(e)
		}
	}
	// Modern gem install prefers compact metadata. Explicitly decode the legacy
	// index using the native client's safe Marshal reader as additional evidence.
	run("-rnet/http", "-rzlib", "-rstringio", "-rrubygems/safe_marshal", "-e", `u=URI(ARGV.fetch(0));q=Net::HTTP::Get.new(u);q.basic_auth("fixture",ENV.fetch("GEM_HOST_API_KEY"));r=Net::HTTP.start(u.host,u.port){|h|h.request(q)};abort "index status" unless r.code=="200";a=Gem::SafeMarshal.safe_load(Zlib::GzipReader.new(StringIO.new(r.body)).read);abort "legacy tuples" unless a.any?{|n,v,p|n=="ant-example" && v.to_s=="1.2.0" && p=="ruby"}`, server.URL+testBase+"specs.4.8.gz")
	source := strings.Replace(server.URL, "http://", "http://fixture:"+f.token+"@", 1) + testBase
	run(gem, "install", "ant-example", "--version", "1.2.0", "--clear-sources", "--source", source, "--no-document")
	if _, e = os.Stat(filepath.Join(dir, "installed", "gems", "ant-example-1.2.0", "lib", "ant-example.rb")); e != nil {
		t.Fatal(e)
	}
	bundle, e := exec.LookPath("bundle")
	if e != nil {
		t.Skip("Bundler unavailable")
	}
	os.WriteFile(filepath.Join(dir, "Gemfile"), []byte("source '"+source+"'\ngem 'ant-example', '1.2.0'\n"), 0600)
	run(bundle, "--version")
	run(bundle, "config", "set", "--local", "path", filepath.Join(dir, "bundle"))
	run(bundle, "install")
	run(bundle, "check")
	requestsMu.Lock()
	for _, route := range []string{"specs.4.8.gz", "quick/Marshal.4.8/ant-example-1.2.0.gemspec.rz", "versions", "info/ant-example", "gems/ant-example-1.2.0.gem"} {
		if !requests["GET "+testBase+route] {
			t.Errorf("native clients did not request %s", route)
		}
	}
	requestsMu.Unlock()
	run(bundle, "config", "set", "--local", "frozen", "true")
	run(bundle, "install")
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
	b := packageBytes(t, "example", "1.0.0", "ruby", "")
	assertCode(t, f.push(t, b), 503)
	assertCode(t, f.request("GET", testBase+"info/example", nil, nil), 404)
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
	assertCode(t, f.request("GET", testBase+"versions", nil, nil), 404)
}
