package maven

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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
	schema := "ant_maven_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Maven")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'releases','maven') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','maven') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	req.Header.Set("Authorization", "Bearer "+f.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func bundle(t *testing.T, assets map[string][]byte) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	for name, data := range assets {
		part, e := m.CreateFormFile("asset", name)
		if e != nil {
			t.Fatal(e)
		}
		part.Write(data)
	}
	if e := m.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes(), m.FormDataContentType()
}

const basePath = "/feeds/releases/maven/org/example/demo/"

func publishFixture(t *testing.T, f fixture, version string) []byte {
	t.Helper()
	j := jar(t, "META-INF/MANIFEST.MF")
	body, ct := bundle(t, map[string][]byte{"demo-" + version + ".pom": pom("org.example", "demo", version, "jar"), "demo-" + version + ".jar": j})
	assertCode(t, f.request("POST", basePath+version+"/_bundle", body, map[string]string{"Content-Type": ct}), 201)
	assertCode(t, f.request("POST", basePath+version+"/_bundle", body, map[string]string{"Content-Type": ct}), 204)
	return j
}
func assertCode(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status %d want %d: %s", r.Code, want, r.Body.String())
	}
}
func TestReleaseBundlePG(t *testing.T) {
	f := setup(t)
	j := publishFixture(t, f, "1.0")
	response := f.request("GET", basePath+"1.0/demo-1.0.jar", nil, nil)
	assertCode(t, response, 200)
	if !bytes.Equal(response.Body.Bytes(), j) {
		t.Fatal("jar changed")
	}
	response = f.request("GET", basePath+"1.0/demo-1.0.jar", nil, map[string]string{"Range": "bytes=2-8"})
	assertCode(t, response, 206)
	if !bytes.Equal(response.Body.Bytes(), j[2:9]) {
		t.Fatal("range")
	}
	response = f.request("GET", basePath+"1.0/demo-1.0.jar.sha1", nil, nil)
	assertCode(t, response, 200)
	sum := sha1.Sum(j)
	if response.Body.String() != hex.EncodeToString(sum[:]) {
		t.Fatal("checksum")
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("service:"+f.token))
	assertCode(t, f.request("HEAD", basePath+"1.0/demo-1.0.pom", nil, map[string]string{"Authorization": basic}), 200)
	publishFixture(t, f, "2.0")
	response = f.request("GET", basePath+"maven-metadata.xml", nil, nil)
	assertCode(t, response, 200)
	if !strings.Contains(response.Body.String(), "<version>1.0</version>") || !strings.Contains(response.Body.String(), "<release>2.0</release>") {
		t.Fatal(response.Body.String())
	}
	body, ct := bundle(t, map[string][]byte{"demo-1.0.pom": pom("org.example", "demo", "1.0", "jar"), "demo-1.0.jar": jar(t, "changed")})
	assertCode(t, f.request("POST", basePath+"1.0/_bundle", body, map[string]string{"Content-Type": ct}), 409)
	body, ct = bundle(t, map[string][]byte{"demo-3.0.pom": pom("org.example", "demo", "3.0", "jar")})
	assertCode(t, f.request("POST", basePath+"3.0/_bundle", body, map[string]string{"Content-Type": ct}), 400)
	assertCode(t, f.request("GET", basePath+"3.0/demo-3.0.pom", nil, nil), 404)
	assertCode(t, f.request("PUT", basePath+"3.0/_bundle", nil, nil), 405)
	assertCode(t, f.request("GET", basePath+"1.0-SNAPSHOT/maven-metadata.xml", nil, nil), 501)
	assertCode(t, f.request("GET", basePath+"maven-metadata.xml", nil, map[string]string{"Authorization": ""}), 401)
}
func TestOwnerAndScopePG(t *testing.T) {
	f := setup(t)
	publishFixture(t, f, "1.0")
	ctx := context.Background()
	denied, e := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Namespace: "other", Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"maven-metadata.xml", "maven-metadata.xml.sha1", "1.0/demo-1.0.jar"} {
		assertCode(t, f.request("GET", basePath+key, nil, map[string]string{"Authorization": "Bearer " + denied.Secret}), 404)
	}
	var source string
	if e = f.pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'earlier','local','maven') RETURNING id`, f.instance).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `UPDATE feed_sources SET priority=1 WHERE feed_id=$1`, f.feed); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, f.instance, f.feed, source); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pool.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'org.example:demo')`, f.instance, source); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("GET", basePath+"1.0/demo-1.0.jar", nil, nil), 404)
	assertCode(t, f.request("GET", basePath+"maven-metadata.xml", nil, nil), 404)
}
func TestNativeMavenParentPG(t *testing.T) {
	mvn, e := exec.LookPath("mvn")
	if e != nil {
		t.Skip("Maven not installed")
	}
	f := setup(t)
	body, ct := bundle(t, map[string][]byte{"demo-1.0.pom": pom("org.example", "demo", "1.0", "pom")})
	assertCode(t, f.request("POST", basePath+"1.0/_bundle", body, map[string]string{"Content-Type": ct}), 201)
	server := httptest.NewServer(http.HandlerFunc(f.registry.ServeHTTP))
	defer server.Close()
	dir := t.TempDir()
	settings := `<settings><mirrors><mirror><id>fixture</id><mirrorOf>*</mirrorOf><url>` + server.URL + `/feeds/releases/maven/</url></mirror></mirrors><servers><server><id>fixture</id><username>service</username><password>` + f.token + `</password></server></servers></settings>`
	project := `<project><modelVersion>4.0.0</modelVersion><parent><groupId>org.example</groupId><artifactId>demo</artifactId><version>1.0</version><relativePath/></parent><artifactId>child</artifactId><packaging>pom</packaging></project>`
	if e = os.WriteFile(filepath.Join(dir, "settings.xml"), []byte(settings), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "pom.xml"), []byte(project), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, mvn, "-B", "-s", filepath.Join(dir, "settings.xml"), "-Dmaven.repo.local="+filepath.Join(dir, "repository"), "-f", filepath.Join(dir, "pom.xml"), "validate")
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("Maven: %v\n%s", e, out)
	}
	data, e := os.ReadFile(filepath.Join(dir, "repository/org/example/demo/1.0/demo-1.0.pom"))
	if e != nil || !bytes.Equal(data, pom("org.example", "demo", "1.0", "pom")) {
		t.Fatalf("native parent resolution: %v", e)
	}
}

func TestInvalidBundleInvisiblePG(t *testing.T) {
	f := setup(t)
	// A checksum mismatch after spooling must not expose even the valid POM.
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	part, e := m.CreateFormFile("asset", "demo-1.0.pom")
	if e != nil {
		t.Fatal(e)
	}
	part.Write(pom("org.example", "demo", "1.0", "jar"))
	h := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="asset"; filename="demo-1.0.jar"`}, "X-Checksum-Sha256": {strings.Repeat("0", 64)}}
	part, e = m.CreatePart(h)
	if e != nil {
		t.Fatal(e)
	}
	part.Write(jar(t, "fixture"))
	m.Close()
	assertCode(t, f.request("POST", basePath+"1.0/_bundle", b.Bytes(), map[string]string{"Content-Type": m.FormDataContentType()}), 400)
	for _, key := range []string{"maven-metadata.xml", "1.0/demo-1.0.pom", "1.0/demo-1.0.jar"} {
		assertCode(t, f.request("GET", basePath+key, nil, nil), 404)
	}
	var count int
	if e = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM versions`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("partial publication: %d %v", count, e)
	}
}
func TestConcurrentVersionsPG(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	for _, version := range []string{"1.0", "2.0"} {
		body, ct := bundle(t, map[string][]byte{"demo-" + version + ".pom": pom("org.example", "demo", version, "pom")})
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.request("POST", basePath+version+"/_bundle", body, map[string]string{"Content-Type": ct})
			if r.Code != 201 {
				t.Errorf("publish %s: %d %s", version, r.Code, r.Body.String())
			}
		}()
	}
	wg.Wait()
	response := f.request("GET", basePath+"maven-metadata.xml", nil, nil)
	assertCode(t, response, 200)
	for _, version := range []string{"1.0", "2.0"} {
		if !strings.Contains(response.Body.String(), "<version>"+version+"</version>") {
			t.Fatal("lost version", response.Body.String())
		}
	}
}
func TestForeignTokenAndBrowserContextPG(t *testing.T) {
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
	assertCode(t, f.request("GET", basePath+"maven-metadata.xml", nil, map[string]string{"Authorization": "Bearer " + token.Secret}), 404)
	req := httptest.NewRequest("GET", basePath+"maven-metadata.xml", nil)
	req = req.WithContext(identity.WithAuthenticatedPrincipal(ctx, identity.Principal{InstanceID: f.instance, PrincipalID: f.principal}))
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	assertCode(t, response, 401)
}
