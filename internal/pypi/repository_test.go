package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
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
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	schema := "ant_pypi_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "PyPI")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'python','pypi') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','pypi') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	req.SetBasicAuth("__token__", f.token)
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
func wheel(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	stem := strings.ReplaceAll(name, "-", "_") + "-" + version + ".dist-info/"
	files := map[string]string{stem + "METADATA": "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\nRequires-Python: >=3.8\n\n", stem + "WHEEL": "Wheel-Version: 1.0\nGenerator: ant-fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n", stem + "RECORD": "", "demo_pkg/__init__.py": "value = 42\n"}
	if extra != "" {
		files[extra] = "x"
	}
	var record bytes.Buffer
	cw := csv.NewWriter(&record)
	recordNames := []string{}
	for name := range files {
		if name != stem+"RECORD" {
			recordNames = append(recordNames, name)
		}
	}
	sort.Strings(recordNames)
	for _, name := range recordNames {
		sum := sha256.Sum256([]byte(files[name]))
		cw.Write([]string{name, "sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]), strconv.Itoa(len(files[name]))})
	}
	cw.Write([]string{stem + "RECORD", "", ""})
	cw.Flush()
	files[stem+"RECORD"] = record.String()
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		io.WriteString(w, data)
	}
	z.Close()
	return b.Bytes()
}
func sdist(t *testing.T) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tr := tar.NewWriter(gz)
	for name, data := range map[string]string{"demo_pkg-1.0/PKG-INFO": "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0\n\n", "demo_pkg-1.0/pyproject.toml": "[build-system]\nrequires=[]\n"} {
		tr.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))})
		io.WriteString(tr, data)
	}
	tr.Close()
	gz.Close()
	return b.Bytes()
}
func upload(file string, data []byte) ([]byte, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	name, version, _, _ := filename(file)
	for k, v := range map[string]string{":action": "file_upload", "protocol_version": "1", "name": name, "version": version, "sha256_digest": string(bytesInfo(data).Digest)} {
		w.WriteField(k, v)
	}
	p, _ := w.CreateFormFile("content", file)
	p.Write(data)
	w.Close()
	return b.Bytes(), w.FormDataContentType()
}
func (f fixture) publish(t *testing.T, file string, data []byte, status int) {
	t.Helper()
	body, ctype := upload(file, data)
	assertCode(t, f.request("POST", "/feeds/python/pypi/", body, map[string]string{"Content-Type": ctype}), status)
}
func TestPublicationSimpleAndSidecar(t *testing.T) {
	f := setup(t)
	base := "/feeds/python/pypi/"
	file := "demo_pkg-1.0-py3-none-any.whl"
	data := wheel(t, "demo-pkg", "1.0", "")
	f.publish(t, file, data, 200)
	f.publish(t, file, data, 200)
	f.publish(t, "demo_pkg-1.0.tar.gz", sdist(t), 200)
	response := f.request("GET", base+"simple/demo-pkg/", nil, map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"})
	assertCode(t, response, 200)
	var index struct {
		Files []struct {
			URL, Filename string
			Hashes        map[string]string `json:"hashes"`
		}
	}
	if e := json.Unmarshal(response.Body.Bytes(), &index); e != nil {
		t.Fatal(e)
	}
	if len(index.Files) != 2 {
		t.Fatal(response.Body.String())
	}
	for _, file := range index.Files {
		r := f.request("GET", file.URL, nil, nil)
		assertCode(t, r, 200)
		if string(bytesInfo(r.Body.Bytes()).Digest) != file.Hashes["sha256"] {
			t.Fatal("hash")
		}
		assertCode(t, f.request("GET", file.URL+".metadata", nil, nil), 200)
		bad := strings.Replace(file.URL, f.source, "00000000-0000-4000-8000-000000000000", 1)
		assertCode(t, f.request("GET", bad, nil, nil), 404)
	}
	html := f.request("GET", base+"simple/demo-pkg/", nil, nil)
	assertCode(t, html, 200)
	if !strings.Contains(html.Body.String(), "data-core-metadata") || !strings.Contains(html.Body.String(), "&gt;=3.8") {
		t.Fatal(html.Body.String())
	}
	assertCode(t, f.request("GET", base+"simple/Demo_Pkg/", nil, nil), 301)
	assertCode(t, f.request("GET", base+"simple/", nil, map[string]string{"Authorization": ""}), 401)
	f.publish(t, file, wheel(t, "demo-pkg", "1.0", "extra.py"), 409)
	f.publish(t, "demo_pkg-2.0-py3-none-any.whl", wheel(t, "demo-pkg", "2.0", "../escape"), 400)
	var count int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); e != nil || count != 6 {
		t.Fatalf("atomicity %d %v", count, e)
	}
	token, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	body, ctype := upload(file, data)
	assertCode(t, f.request("POST", base, body, map[string]string{"Content-Type": ctype, "Authorization": "Bearer " + token.Secret}), 403)
}
func TestOwnerDoesNotFallBack(t *testing.T) {
	f := setup(t)
	f.publish(t, "demo_pkg-1.0-py3-none-any.whl", wheel(t, "demo-pkg", "1.0", ""), 200)
	ctx := context.Background()
	var source string
	if e := f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'other','local','pypi') RETURNING id`, f.instance).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if _, e := f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,1)`, f.instance, f.feed, source); e != nil {
		t.Fatal(e)
	}
	f.pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, f.instance, "source:"+source)
	router, e := routing.Compile(routing.Config{Sources: []routing.Source{{ID: source, Kind: routing.Local}}, DefaultTarget: source})
	if e != nil {
		t.Fatal(e)
	}
	f.service.Router = func(context.Context, string) (*routing.Router, error) { return router, nil }
	f.publish(t, "demo_pkg-2.0-py3-none-any.whl", wheel(t, "demo-pkg", "2.0", ""), 200)
	r := f.request("GET", "/feeds/python/pypi/simple/demo-pkg/", nil, nil)
	assertCode(t, r, 200)
	if strings.Contains(r.Body.String(), "2.0") {
		t.Fatal("owner leak")
	}
	// A token scoped to another feed cannot enumerate package metadata.
	var otherFeed string
	if e := f.pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'other','pypi') RETURNING id`, f.instance).Scan(&otherFeed); e != nil {
		t.Fatal(e)
	}
	token, e := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{FeedID: otherFeed, Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	r = f.request("GET", "/feeds/python/pypi/simple/", nil, map[string]string{"Authorization": "Bearer " + token.Secret})
	assertCode(t, r, 200)
	if strings.Contains(r.Body.String(), "demo-pkg") {
		t.Fatal("unauthorized enumeration")
	}
}
func TestInstalledPipDownloadInstall(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("python unavailable")
	}
	version, e := exec.Command(python, "-m", "pip", "--version").CombinedOutput()
	if e != nil {
		t.Skip("pip unavailable")
	}
	t.Log(strings.TrimSpace(string(version)))
	f := setup(t)
	data := wheel(t, "demo-pkg", "1.0", "")
	f.publish(t, "demo_pkg-1.0-py3-none-any.whl", data, 200)
	server := httptest.NewServer(f.registry)
	defer server.Close()
	root := t.TempDir()
	u, _ := url.Parse(server.URL)
	u.User = url.UserPassword("__token__", f.token)
	index := u.String() + "/feeds/python/pypi/simple/"
	requirements := filepath.Join(root, "requirements.txt")
	os.WriteFile(requirements, []byte("demo-pkg==1.0 --hash=sha256:"+string(bytesInfo(data).Digest)+"\n"), 0600)
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, python, append([]string{"-m", "pip", "--isolated", "--disable-pip-version-check", "--no-input"}, args...)...)
		cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE=/dev/null")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("pip failed: %v\n%s", e, strings.ReplaceAll(string(out), f.token, "REDACTED"))
		}
	}
	run("download", "--no-deps", "--no-cache-dir", "--index-url", index, "--require-hashes", "-r", requirements, "-d", filepath.Join(root, "download"))
	run("install", "--no-deps", "--no-cache-dir", "--index-url", index, "--require-hashes", "-r", requirements, "--target", filepath.Join(root, "installed"))
	b, e := os.ReadFile(filepath.Join(root, "installed", "demo_pkg", "__init__.py"))
	if e != nil || !bytes.Contains(b, []byte("42")) {
		t.Fatal("install failed", e)
	}
	cmd := exec.Command(python, "-m", "pip", "--isolated", "--disable-pip-version-check", "--no-input", "download", "--no-deps", "--no-cache-dir", "--index-url", index, "--python-version", "3.7", "demo-pkg==1.0", "-d", filepath.Join(root, "old-python"))
	cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE=/dev/null")
	if out, e := cmd.CombinedOutput(); e == nil || !bytes.Contains(out, []byte("Requires-Python")) || !bytes.Contains(out, []byte("3.8")) {
		t.Fatalf("pip did not demonstrate Requires-Python rejection: %s", strings.ReplaceAll(string(out), f.token, "REDACTED"))
	}
	t.Log(fmt.Sprintf("verified wheel sha256:%s", bytesInfo(data).Digest))
}
func TestCredentialAndIntegrityFailures(t *testing.T) {
	f := setup(t)
	base := "/feeds/python/pypi/"
	file := "demo_pkg-1.0-py3-none-any.whl"
	data := wheel(t, "demo-pkg", "1.0", "")
	body, ctype := upload(file, data)
	body = bytes.Replace(body, []byte(bytesInfo(data).Digest), []byte(strings.Repeat("0", 64)), 1)
	assertCode(t, f.request("POST", base, body, map[string]string{"Content-Type": ctype}), 400)
	r := httptest.NewRequest("GET", base+"simple/", nil)
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{InstanceID: f.instance, PrincipalID: f.principal}))
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, r)
	assertCode(t, w, 401)
	f.registry.instance = "00000000-0000-4000-8000-000000000001"
	assertCode(t, f.request("GET", base+"simple/", nil, nil), 404)
	var count int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM assets`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("invalid upload became visible %d %v", count, e)
	}
}
func TestImportedYankedMetadata(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := "demo_pkg-1.0-py3-none-any.whl"
	data := wheel(t, "demo-pkg", "1.0", "")
	temp, e := os.CreateTemp(t.TempDir(), "dist")
	if e != nil {
		t.Fatal(e)
	}
	defer temp.Close()
	temp.Write(data)
	m, core, e := inspect(ctx, temp, file)
	if e != nil {
		t.Fatal(e)
	}
	m.Yanked = "security <fix>"
	m.Digest = string(bytesInfo(data).Digest)
	index, _ := json.Marshal(m)
	pubs := []artifact.Publication{}
	for i, key := range []string{file, file + ".metadata", "index"} {
		payload := [][]byte{data, core, index}[i]
		pubs = append(pubs, artifact.Publication{FeedID: f.feed, Coordinate: m.Name, Version: versionKey(file), AssetKey: key, MediaType: []string{"application/octet-stream", "text/plain", "application/json"}[i], Info: bytesInfo(payload)})
	}
	p, e := (identity.Tokens{DB: f.pool}).Authenticate(ctx, f.token)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.PublishBundle(ctx, p, pubs, []io.Reader{bytes.NewReader(data), bytes.NewReader(core), bytes.NewReader(index)}); e != nil {
		t.Fatal(e)
	}
	r := f.request("GET", "/feeds/python/pypi/simple/demo-pkg/", nil, nil)
	assertCode(t, r, 200)
	if !strings.Contains(r.Body.String(), `data-yanked="security &lt;fix&gt;"`) {
		t.Fatal(r.Body.String())
	}
	r = f.request("GET", "/feeds/python/pypi/simple/demo-pkg/", nil, map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"})
	assertCode(t, r, 200)
	var result struct{ Files []struct{ Yanked string } }
	if e = json.Unmarshal(r.Body.Bytes(), &result); e != nil || len(result.Files) != 1 || result.Files[0].Yanked != m.Yanked {
		t.Fatal("yanked metadata lost", e)
	}
}
