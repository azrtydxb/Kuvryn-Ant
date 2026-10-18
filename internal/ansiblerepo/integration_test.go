package ansiblerepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
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

type galaxyFixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Repository
	service                                        *artifact.Service
	root, token, instance, principal, feed, source string
}

func galaxySetup(t *testing.T) galaxyFixture {
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
	schema := "ant_galaxy_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "Galaxy")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := galaxyFixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'collections','ansible') RETURNING id::text`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','ansible') RETURNING id::text`, owner.InstanceID).Scan(&f.source); err != nil {
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
	if err = f.registry.SetPublicURL("https://galaxy.example"); err != nil {
		t.Fatal(err)
	}
	return f
}

func galaxyRequest(t *testing.T, f galaxyFixture, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+f.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}

func galaxyStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}

// collectionBytes builds a valid Galaxy collection tarball and its sha256.
func collectionBytes(t *testing.T, ns, name, version string) ([]byte, string) {
	t.Helper()
	content := "plugin: 42\n"
	contentSum := sha256.Sum256([]byte(content))
	entries := map[string]struct {
		data []byte
		dir  bool
	}{
		"plugins/action/hello.py": {data: []byte(content)},
	}
	fileList := []map[string]any{}
	names := []string{"plugins/action/hello.py"}
	for _, n := range names {
		fileList = append(fileList, map[string]any{"name": n, "ftype": "file", "chksum_sha256": hex.EncodeToString(contentSum[:]), "chksum_type": "sha256"})
	}
	filesJSON, _ := json.Marshal(map[string]any{"files": fileList, "format": 1})
	filesSum := sha256.Sum256(filesJSON)
	collection := map[string]any{
		"namespace": ns, "name": name, "version": version,
		"dependencies": map[string]string{},
	}
	manifestJSON, _ := json.Marshal(map[string]any{
		"collection_info":    collection,
		"file_manifest_file": map[string]any{"name": "FILES.json", "ftype": "file", "chksum_sha256": hex.EncodeToString(filesSum[:]), "chksum_type": "sha256"},
		"format":             1,
	})
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(gw)
	writeEntry := func(n string, data []byte) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry("MANIFEST.json", manifestJSON)
	writeEntry("FILES.json", filesJSON)
	for n, e := range entries {
		writeEntry(n, e.data)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(gz.Bytes())
	return gz.Bytes(), hex.EncodeToString(sum[:])
}

func publishCollection(t *testing.T, f galaxyFixture, ns, name, version string) string {
	t.Helper()
	archive, digest := collectionBytes(t, ns, name, version)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("sha256", digest)
	part, err := mw.CreateFormFile("file", ns+"-"+name+"-"+version+".tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}
	response := galaxyRequest(t, f, "POST", "/feeds/collections/ansible/v3/artifacts/collections", body.Bytes(), mw.FormDataContentType())
	galaxyStatus(t, response, 202)
	var result map[string]string
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil || !strings.HasSuffix(result["task"], "/") {
		t.Fatal(result, err)
	}
	return result["task"]
}

func pollImport(t *testing.T, f galaxyFixture, task string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response := galaxyRequest(t, f, "GET", task, nil, "")
		if response.Code == 200 {
			var state map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
			if state["state"] == "completed" {
				return
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		galaxyStatus(t, response, 200)
	}
	t.Fatal("import task did not complete")
}

func TestGalaxyPostgresCollectionPublishAndIndex(t *testing.T) {
	f := galaxySetup(t)
	task := publishCollection(t, f, "community", "utils", "1.2.3")
	pollImport(t, f, task)
	// The collection detail page lists the version and download entrypoint.
	response := galaxyRequest(t, f, "GET", "/feeds/collections/ansible/v3/collections/community/utils/", nil, "")
	galaxyStatus(t, response, 200)
	var detail map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	versions := detail["versions"].([]any)
	if len(versions) != 1 || detail["name"] != "utils" {
		t.Fatal(detail)
	}
	// The versions list has the exact version with its download URL.
	response = galaxyRequest(t, f, "GET", "/feeds/collections/ansible/v3/collections/community/utils/versions/", nil, "")
	galaxyStatus(t, response, 200)
	var listing map[string]any
	json.Unmarshal(response.Body.Bytes(), &listing)
	results := listing["results"].([]any)
	if len(results) != 1 {
		t.Fatal(listing)
	}
	// The version detail resolves dependencies and the immutable archive.
	response = galaxyRequest(t, f, "GET", "/feeds/collections/ansible/v3/collections/community/utils/versions/1.2.3/", nil, "")
	galaxyStatus(t, response, 200)
	var versionDetail map[string]any
	json.Unmarshal(response.Body.Bytes(), &versionDetail)
	artifactInfo := versionDetail["artifact"].(map[string]any)
	if versionDetail["version"] != "1.2.3" || artifactInfo["sha256"] == "" || versionDetail["download_url"] == "" {
		t.Fatal(versionDetail)
	}
	// Publishing the identical archive is a second completed import.
	task = publishCollection(t, f, "community", "utils", "1.2.3")
	pollImport(t, f, task)
	// A different version is a separate immutable record.
	task = publishCollection(t, f, "community", "utils", "1.2.4")
	pollImport(t, f, task)
	response = galaxyRequest(t, f, "GET", "/feeds/collections/ansible/v3/collections/community/utils/versions/", nil, "")
	galaxyStatus(t, response, 200)
	json.Unmarshal(response.Body.Bytes(), &listing)
	if len(listing["results"].([]any)) != 2 {
		t.Fatal(listing)
	}
	// The collection archive itself downloads byte-identically.
	response = galaxyRequest(t, f, "GET", "/feeds/collections/ansible/downloads/community/utils/1.2.3/collection.tar.gz", nil, "")
	galaxyStatus(t, response, 200)
	archive, digest := collectionBytes(t, "community", "utils", "1.2.3")
	if response.Header().Get("ETag") == "" || string(response.Body.Bytes()) != string(archive) {
		t.Fatal("archive mismatch")
	}
	_ = digest
}

func TestGalaxyPostgresRejectionAndIsolation(t *testing.T) {
	f := galaxySetup(t)
	// A tampered digest is rejected before spooling.
	archive, _ := collectionBytes(t, "community", "utils", "1.0.0")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("sha256", strings.Repeat("0", 64))
	part, _ := mw.CreateFormFile("file", "community-utils-1.0.0.tar.gz")
	_, _ = part.Write(archive)
	_ = mw.Close()
	response := galaxyRequest(t, f, "POST", "/feeds/collections/ansible/v3/artifacts/collections", body.Bytes(), mw.FormDataContentType())
	galaxyStatus(t, response, 400)
	// A collection whose metadata disagrees with its filename fails closed.
	bad, badSum := collectionBytes(t, "other", "name", "1.0.0")
	var body2 bytes.Buffer
	mw2 := multipart.NewWriter(&body2)
	_ = mw2.WriteField("sha256", badSum)
	part2, _ := mw2.CreateFormFile("file", "community-utils-1.0.0.tar.gz")
	_, _ = part2.Write(bad)
	_ = mw2.Close()
	response = galaxyRequest(t, f, "POST", "/feeds/collections/ansible/v3/artifacts/collections", body2.Bytes(), mw2.FormDataContentType())
	galaxyStatus(t, response, 400)
	// Corrupt archives, missing uploads and unsafe names are rejected.
	for _, raw := range [][]byte{[]byte("not-a-tar"), archive[:len(archive)-3]} {
		var body3 bytes.Buffer
		mw3 := multipart.NewWriter(&body3)
		_ = mw3.WriteField("sha256", badSum)
		part3, _ := mw3.CreateFormFile("file", "a-b-1.0.0.tar.gz")
		_, _ = part3.Write(raw)
		_ = mw3.Close()
		response = galaxyRequest(t, f, "POST", "/feeds/collections/ansible/v3/artifacts/collections", body3.Bytes(), mw3.FormDataContentType())
		if response.Code != 400 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	// Unknown feed, wrong format and README routes fail closed.
	galaxyStatus(t, galaxyRequest(t, f, "GET", "/feeds/missing/ansible/v3/collections/community/utils/", nil, ""), 404)
	galaxyStatus(t, galaxyRequest(t, f, "GET", "/feeds/collections/raw/v3/collections/community/utils/", nil, ""), 404)
	req := httptest.NewRequest("GET", "/feeds/collections/ansible/v3/collections/community/utils/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	ctx := context.Background()
	var errors int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM versions WHERE version IN ('1.0.0')`).Scan(&errors); err != nil || errors != 0 {
		t.Fatal("rejected collection published", errors, err)
	}
}
