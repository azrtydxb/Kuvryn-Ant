package gitlfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"net/url"
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
	schema := "ant_gitlfs_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Git LFS")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','git-lfs') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','git-lfs') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	req.Header.Set("Content-Type", mediaType)
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

const testBase = "/feeds/packages/git-lfs/"

func oid(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (f fixture) batch(t *testing.T, op, id string, size int64) objectResult {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"operation": op, "objects": []map[string]any{{"oid": id, "size": size}}})
	w := f.request("POST", testBase+"objects/batch", b, nil)
	assertCode(t, w, 200)
	var result struct{ Objects []objectResult }
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil || len(result.Objects) != 1 {
		t.Fatalf("bad batch: %v %s", e, w.Body.String())
	}
	return result.Objects[0]
}
func (f fixture) action(t *testing.T, a action, method string, body []byte, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	u, e := url.Parse(a.Href)
	if e != nil {
		t.Fatal(e)
	}
	headers := map[string]string{}
	for k, v := range a.Header {
		headers[k] = v
	}
	for k, v := range extra {
		headers[k] = v
	}
	return f.request(method, u.RequestURI(), body, headers)
}
func TestPostgresTransfers(t *testing.T) {
	f := setup(t)
	data := bytes.Repeat([]byte("large object\n"), 100000)
	id := oid(data)
	size := int64(len(data))
	upload := f.batch(t, "upload", id, size)
	if upload.Error != nil {
		t.Fatal(upload.Error)
	}
	verify, _ := json.Marshal(map[string]any{"oid": id, "size": size})
	assertCode(t, f.action(t, upload.Actions["verify"], "POST", verify, nil), 404)
	assertCode(t, f.action(t, upload.Actions["upload"], "PUT", data[:len(data)-1], nil), 422)
	wrong := bytes.Clone(data)
	wrong[0] ^= 1
	assertCode(t, f.action(t, upload.Actions["upload"], "PUT", wrong, nil), 422)
	if got := f.batch(t, "download", id, size); got.Error == nil || got.Error.Code != 404 {
		t.Fatal("unverified object visible")
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); assertCode(t, f.action(t, upload.Actions["upload"], "PUT", data, nil), 200) }()
	}
	wg.Wait()
	assertCode(t, f.action(t, upload.Actions["verify"], "POST", verify, nil), 200)
	if got := f.batch(t, "upload", id, size); got.Error != nil || len(got.Actions) != 0 {
		t.Fatal("existing upload was not idempotent")
	}
	download := f.batch(t, "download", id, size).Actions["download"]
	w := f.action(t, download, "GET", nil, map[string]string{"Range": "bytes=65536-"})
	assertCode(t, w, 206)
	if !bytes.Equal(w.Body.Bytes(), data[65536:]) {
		t.Fatal("range mismatch")
	}
	assertCode(t, f.action(t, download, "GET", nil, map[string]string{"Range": "bytes=0-2,5-8"}), 416)
	assertCode(t, f.action(t, download, "HEAD", nil, nil), 200)
	assertCode(t, f.action(t, download, "GET", nil, map[string]string{"X-LFS-Action": upload.Actions["upload"].Header["X-LFS-Action"]}), 403)
	f.registry.mu.Lock()
	key := download.Header["X-LFS-Action"]
	g := f.registry.grants[key]
	g.expires = time.Now().Add(-time.Second)
	f.registry.grants[key] = g
	f.registry.mu.Unlock()
	assertCode(t, f.action(t, download, "GET", nil, nil), 403)
	download = f.batch(t, "download", id, size).Actions["download"]
	assertCode(t, f.action(t, download, "GET", nil, nil), 200)
	// Namespace-denied readers cannot enumerate an object, even when bytes exist.
	token, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Namespace: "other", Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	denied := f
	denied.token = token.Secret
	if got := denied.batch(t, "download", id, size); got.Error == nil || got.Error.Code != 404 {
		t.Fatal("object disclosed")
	}
	assertCode(t, denied.action(t, download, "GET", nil, map[string]string{"Authorization": "Bearer " + denied.token}), 403)
	// Higher-priority owner without an asset must not fall through to old bytes.
	var higher string
	if e = f.pool.QueryRow(context.Background(), `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'higher','local','git-lfs') RETURNING id`, f.instance).Scan(&higher); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(context.Background(), `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, higher); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(context.Background(), `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,$3)`, f.instance, higher, id); e != nil {
		t.Fatal(e)
	}
	if got := f.batch(t, "download", id, size); got.Error == nil || got.Error.Code != 404 {
		t.Fatal("owner fallback")
	}
	assertCode(t, f.action(t, download, "GET", nil, nil), 404)
}
func TestPostgresNativeClient(t *testing.T) {
	if _, e := exec.LookPath("git-lfs"); e != nil {
		t.Skip("git-lfs not installed")
	}
	f := setup(t)
	srv := httptest.NewServer(f.registry)
	defer srv.Close()
	if e := f.registry.SetPublicURL(srv.URL); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	home := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + os.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
		out, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %s: %v %s", args[0], e, strings.ReplaceAll(string(out), f.token, "[redacted]"))
		}
		return string(out)
	}
	t.Log(strings.TrimSpace(run("lfs", "version")))
	binary, _ := exec.LookPath("git-lfs")
	b, e := os.ReadFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("git-lfs binary SHA256 %s", oid(b))
	run("init")
	run("config", "user.name", "Fixture")
	run("config", "user.email", "fixture@example.invalid")
	run("lfs", "install", "--local")
	run("config", "lfs.url", srv.URL+strings.TrimSuffix(testBase, "/"))
	run("config", "http.extraHeader", "Authorization: Bearer "+f.token)
	run("config", "lfs.locksverify", "false")
	run("remote", "add", "origin", filepath.Join(t.TempDir(), "unused.git"))
	run("lfs", "track", "*.bin")
	data := bytes.Repeat([]byte("native git-lfs fixture\n"), 100000)
	if e = os.WriteFile(filepath.Join(dir, "large.bin"), data, 0600); e != nil {
		t.Fatal(e)
	}
	run("add", ".")
	run("commit", "-m", "fixture")
	run("lfs", "push", "--all", "origin")
	if e = os.RemoveAll(filepath.Join(dir, ".git", "lfs", "objects")); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(dir, "large.bin")); e != nil {
		t.Fatal(e)
	}
	run("lfs", "fetch", "origin", "HEAD")
	run("lfs", "checkout")
	got, e := os.ReadFile(filepath.Join(dir, "large.bin"))
	if e != nil || !bytes.Equal(got, data) {
		t.Fatalf("native fetch mismatch %v", e)
	}
}
func TestPostgresRoutingChange(t *testing.T) {
	f := setup(t)
	data := []byte("pinned source")
	up := f.batch(t, "upload", oid(data), int64(len(data)))
	router, _ := routing.Compile(routing.Config{Sources: []routing.Source{{ID: "00000000-0000-4000-8000-000000000099", Kind: routing.Local}}, DefaultTarget: "00000000-0000-4000-8000-000000000099"})
	f.service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	assertCode(t, f.action(t, up.Actions["upload"], "PUT", data, nil), 409)
}

func TestPostgresActionSecurity(t *testing.T) {
	f := setup(t)
	data := []byte("action security")
	id := oid(data)
	size := int64(len(data))
	up := f.batch(t, "upload", id, size)
	assertCode(t, f.action(t, up.Actions["upload"], "PUT", data, nil), 200)
	down := f.batch(t, "download", id, size).Actions["download"]
	// Untrusted request origins never change the destination receiving credentials.
	body, _ := json.Marshal(map[string]any{"operation": "download", "objects": []map[string]any{{"oid": id, "size": size}}})
	req := httptest.NewRequest("POST", testBase+"objects/batch", bytes.NewReader(body))
	req.SetBasicAuth("fixture", f.token)
	req.Host = "attacker.invalid"
	req.Header.Set("X-Forwarded-Host", "attacker.invalid")
	req.Header.Set("Content-Type", mediaType)
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), "attacker.invalid") || !strings.Contains(w.Body.String(), "https://packages.example/") {
		t.Fatal("untrusted action origin")
	}
	// A browser principal does not replace machine credentials.
	req = httptest.NewRequest("POST", testBase+"objects/batch", bytes.NewReader(body))
	req = req.WithContext(identity.WithAuthenticatedPrincipal(req.Context(), identity.Principal{InstanceID: f.instance, PrincipalID: f.principal}))
	w = httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	assertCode(t, w, 401)
	// Neither the URL nor the normal service token alone is sufficient.
	assertCode(t, f.action(t, down, "GET", nil, map[string]string{"X-LFS-Action": ""}), 403)
	other := down
	other.Href = strings.Replace(other.Href, id, oid([]byte("other")), 1)
	assertCode(t, f.action(t, other, "GET", nil, nil), 403)
	// Reconstructing the adapter loses grants, requiring a fresh batch request.
	restarted, e := New(f.pool, f.service)
	if e != nil {
		t.Fatal(e)
	}
	if e = restarted.SetPublicURL("https://packages.example"); e != nil {
		t.Fatal(e)
	}
	fresh := f
	fresh.registry = restarted
	assertCode(t, fresh.action(t, down, "GET", nil, nil), 403)
	assertCode(t, fresh.action(t, fresh.batch(t, "download", id, size).Actions["download"], "GET", nil, nil), 200)
	p, e := (identity.Tokens{DB: f.pool}).Authenticate(context.Background(), f.token)
	if e != nil {
		t.Fatal(e)
	}
	if e = (identity.Tokens{DB: f.pool}).Revoke(context.Background(), f.instance, f.principal, p.TokenID); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.action(t, down, "GET", nil, nil), 401)
}
func TestPostgresCancelledUpload(t *testing.T) {
	f := setup(t)
	data := bytes.Repeat([]byte("cancel"), 1000)
	id := oid(data)
	up := f.batch(t, "upload", id, int64(len(data))).Actions["upload"]
	u, _ := url.Parse(up.Href)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("PUT", u.RequestURI(), nil).WithContext(ctx)
	req.Body = &cancelBody{data: data, cancel: cancel}
	req.ContentLength = -1
	for k, v := range up.Header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatal("cancelled upload succeeded")
	}
	if got := f.batch(t, "download", id, int64(len(data))); got.Error == nil || got.Error.Code != 404 {
		t.Fatal("cancelled upload became visible")
	}
}

type cancelBody struct {
	data   []byte
	cancel context.CancelFunc
}

func (r *cancelBody) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	r.cancel()
	return n, nil
}
func (r *cancelBody) Close() error { return nil }
