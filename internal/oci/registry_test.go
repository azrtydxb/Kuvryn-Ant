package oci

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func TestManifestAndRangeValidation(t *testing.T) {
	zero := int64(0)
	desc := descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digest(nil), Size: &zero}
	data, _ := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: imageManifest, Config: &desc, Layers: []descriptor{}})
	if refs, err := validateManifest(data, imageManifest); err != nil || len(refs) != 1 {
		t.Fatalf("valid manifest %v", err)
	}
	for _, bad := range [][]byte{[]byte(`{"schemaVersion":1}`), []byte(`{"schemaVersion":2,"config":{},"layers":[]}`)} {
		if _, err := validateManifest(bad, imageManifest); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
	for _, header := range []string{"bytes=8-2", "bytes=10-", "bytes=-0", "bytes=0-1,3-4", "bytes=999999999999999999999-"} {
		if _, _, ok := byteRange(header, 10); ok {
			t.Fatal("invalid range accepted", header)
		}
	}
	for _, example := range []struct {
		header         string
		offset, length int64
	}{{"", 0, 10}, {"bytes=2-5", 2, 4}, {"bytes=-3", 7, 3}, {"bytes=8-99", 8, 2}} {
		offset, length, ok := byteRange(example.header, 10)
		if !ok || offset != example.offset || length != example.length {
			t.Fatal("valid range failed", example.header)
		}
	}
}

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
	schema := "ant_oci_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "OCI")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'images','oci') RETURNING id`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','oci') RETURNING id`, owner.InstanceID).Scan(&f.source); err != nil {
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
	f.registry, err = New(pool, f.service, f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.registry.Close() })
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
func (f fixture) pushBlob(t *testing.T, repo string, data []byte) string {
	t.Helper()
	response := f.request("POST", "/v2/images/"+repo+"/blobs/uploads/", nil, nil)
	assertCode(t, response, 202)
	location := response.Header().Get("Location")
	response = f.request("PUT", location+"?digest="+digest(data), data, nil)
	assertCode(t, response, 201)
	return digest(data)
}
func TestPushResumeManifestPullAndTags(t *testing.T) {
	f := setup(t)
	assertCode(t, f.request("GET", "/v2/", nil, nil), 200)
	data := []byte("layer-content")
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	first := f.request("PATCH", location, data[:5], map[string]string{"Content-Range": "0-4"})
	assertCode(t, first, 202)
	if first.Header().Get("Range") != "0-4" {
		t.Fatal("wrong upload offset")
	}
	// Uncommitted tail simulates a process dying during the next chunk.
	file, err := os.OpenFile(filepath.Join(f.root, id), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Write([]byte("uncommitted-tail"))
	file.Close()
	restarted, err := New(f.pool, f.service, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	f.registry = restarted
	status := f.request("GET", location, nil, nil)
	assertCode(t, status, 204)
	if status.Header().Get("Range") != "0-4" {
		t.Fatal("restart lost durable offset")
	}
	bad := f.request("PATCH", location, []byte("x"), map[string]string{"Content-Range": "0-0"})
	assertCode(t, bad, 416)
	final := f.request("PUT", location+"?digest="+digest(data), data[5:], map[string]string{"Content-Range": fmt.Sprintf("5-%d", len(data)-1)})
	assertCode(t, final, 201)
	repeat := f.request("PUT", location+"?digest="+digest(data), nil, nil)
	assertCode(t, repeat, 201)
	read := f.request("GET", "/v2/images/team/app/blobs/"+digest(data), nil, map[string]string{"Range": "bytes=2-6"})
	assertCode(t, read, 206)
	if read.Body.String() != string(data[2:7]) {
		t.Fatal("wrong range")
	}
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := f.pushBlob(t, "team/app", config)
	doc := map[string]any{"schemaVersion": 2, "mediaType": imageManifest, "config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)}, "layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": digest(data), "size": len(data)}}}
	manifest, _ := json.Marshal(doc)
	pushed := f.request("PUT", "/v2/images/team/app/manifests/latest", manifest, map[string]string{"Content-Type": imageManifest})
	assertCode(t, pushed, 201)
	pulled := f.request("GET", "/v2/images/team/app/manifests/latest", nil, map[string]string{"Accept": imageManifest})
	assertCode(t, pulled, 200)
	if !bytes.Equal(pulled.Body.Bytes(), manifest) || pulled.Header().Get("Docker-Content-Digest") != digest(manifest) {
		t.Fatal("manifest bytes changed")
	}
	assertCode(t, f.request("HEAD", "/v2/images/team/app/manifests/"+digest(manifest), nil, nil), 200)
	assertCode(t, f.request("GET", "/v2/images/team/app/manifests/latest", nil, map[string]string{"Accept": dockerManifest}), 406)
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": imageIndex, "manifests": []map[string]any{{"mediaType": imageManifest, "digest": digest(manifest), "size": len(manifest), "platform": map[string]string{"architecture": "amd64", "os": "linux"}}}})
	assertCode(t, f.request("PUT", "/v2/images/team/app/manifests/multi", index, map[string]string{"Content-Type": imageIndex}), 201)
	tags := f.request("GET", "/v2/images/team/app/tags/list?n=1", nil, nil)
	assertCode(t, tags, 200)
	if !strings.Contains(tags.Body.String(), "latest") || tags.Header().Get("Link") == "" {
		t.Fatal("tag pagination missing")
	}
	// Immutable tag conflict and missing closure do not publish partial metadata.
	changed := append([]byte(" "), manifest...)
	assertCode(t, f.request("PUT", "/v2/images/team/app/manifests/latest", changed, map[string]string{"Content-Type": imageManifest}), 409)
	doc["config"] = map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digest([]byte("missing")), "size": 7}
	missing, _ := json.Marshal(doc)
	badManifest := f.request("PUT", "/v2/images/team/app/manifests/missing", missing, map[string]string{"Content-Type": imageManifest})
	assertCode(t, badManifest, 400)
	if !strings.Contains(badManifest.Body.String(), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatal("missing closure wrong error")
	}
}
func TestUploadAuthorizationIsolationAndCancellation(t *testing.T) {
	f := setup(t)
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	location := start.Header().Get("Location")
	assertCode(t, f.request("PATCH", strings.Replace(location, "team/app", "team/other", 1), []byte("hidden"), nil), 404)
	assertCode(t, f.request("DELETE", location, nil, nil), 204)
	assertCode(t, f.request("GET", location, nil, nil), 404)
	narrow, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{FeedID: f.feed, Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	original := f.token
	f.token = narrow.Secret
	assertCode(t, f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil), 403)
	f.token = original
	req := httptest.NewRequest("GET", "/v2/", nil)
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	assertCode(t, response, 401)
	if !strings.Contains(response.Body.String(), "UNAUTHORIZED") || !strings.HasPrefix(response.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatal("missing registry auth challenge")
	}
	blobDigest := f.pushBlob(t, "team/source", []byte("mount"))
	mount := f.request("POST", "/v2/images/team/destination/blobs/uploads/?mount="+blobDigest+"&from=images/team/source", nil, nil)
	assertCode(t, mount, 201)
	// Mount requires source read even when the caller may publish to destination.
	onlyPublish, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.token = onlyPublish.Secret
	assertCode(t, f.request("POST", "/v2/images/team/denied/blobs/uploads/?mount="+blobDigest+"&from=images/team/source", nil, nil), 202) // unreadable source: fall back to a regular upload session
}

func TestDuplicateManifestMembersAndAcceptQuality(t *testing.T) {
	for _, body := range []string{`{"schemaVersion":2,"schemaVersion":1}`, `{"config":{"digest":"first","digest":"second"}}`, `{"layers":[{"size":1,"size":2}]}`, `{} {}`} {
		if uniqueJSON([]byte(body)) == nil {
			t.Fatal("ambiguous JSON accepted", body)
		}
	}
	if err := uniqueJSON([]byte(`{"extension":{"a":[1,2]},"layers":[]}`)); err != nil {
		t.Fatal(err)
	}
	for _, accept := range []string{imageManifest + ";q=0.0", imageManifest + ";q=0, */*;q=1", imageManifest + ";q=2"} {
		if accepts(accept, imageManifest) {
			t.Fatal("unacceptable media type selected", accept)
		}
	}
	if !accepts(imageManifest+";q=0.5", imageManifest) {
		t.Fatal("acceptable media type rejected")
	}
}
func TestExpiredUploadCleanupIsRootScoped(t *testing.T) {
	f := setup(t)
	own := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, own, 202)
	for _, state := range []string{"complete", "cancelled"} {
		stale := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
		assertCode(t, stale, 202)
		if _, err := f.pool.Exec(context.Background(), `UPDATE oci_uploads SET state=$2 WHERE id=$1`, stale.Header().Get("Docker-Upload-UUID"), state); err != nil {
			t.Fatal(err)
		}
	}
	otherRoot := filepath.Join(t.TempDir(), "other-uploads")
	other, err := New(f.pool, f.service, otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second := f
	second.registry = other
	foreign := second.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, foreign, 202)
	if _, err = f.pool.Exec(context.Background(), `UPDATE oci_uploads SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := f.registry.ReapExpired(context.Background(), 200); err != nil || n != 3 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if _, err = os.Stat(filepath.Join(otherRoot, foreign.Header().Get("Docker-Upload-UUID"))); err != nil {
		t.Fatal("cleanup removed foreign-root staging", err)
	}
	var state string
	if err = f.pool.QueryRow(context.Background(), `SELECT state FROM oci_uploads WHERE id=$1`, foreign.Header().Get("Docker-Upload-UUID")).Scan(&state); err != nil || state != "active" {
		t.Fatal("cleanup cancelled foreign-root upload", state, err)
	}
}

type loseLeaseReader struct {
	reader *bytes.Reader
	lose   func()
	once   bool
}

func (r *loseLeaseReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if !r.once {
		r.once = true
		r.lose()
	}
	return n, err
}
func TestChunkDoesNotCommitAfterWriterLeaseLoss(t *testing.T) {
	f := setup(t)
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	previous := f.service.Lease
	body := &loseLeaseReader{reader: bytes.NewReader([]byte("uncommitted")), lose: func() { f.service.Lease = func() ownership.Lease { return ownership.Lease{} } }}
	req := httptest.NewRequest("PATCH", start.Header().Get("Location"), body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("service", f.token)
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	f.service.Lease = previous
	assertCode(t, response, 403)
	var offset int64
	if err := f.pool.QueryRow(context.Background(), `SELECT offset_bytes FROM oci_uploads WHERE id=$1`, start.Header().Get("Docker-Upload-UUID")).Scan(&offset); err != nil || offset != 0 {
		t.Fatal("stale writer committed upload offset", offset, err)
	}
	info, err := os.Stat(filepath.Join(f.root, start.Header().Get("Docker-Upload-UUID")))
	if err != nil || info.Size() != 0 {
		t.Fatal("stale writer retained uncommitted tail", err)
	}
}

func TestDiscoveryChallengesDoNotExhaustCredentialBudget(t *testing.T) {
	f := setup(t)
	for i := 0; i < 20; i++ {
		request := httptest.NewRequest("GET", "/v2/", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		f.registry.ServeHTTP(response, request)
		assertCode(t, response, http.StatusUnauthorized)
	}
	assertCode(t, f.request("GET", "/v2/", nil, nil), http.StatusOK)
	for i := 0; i < 10; i++ {
		assertCode(t, f.request("GET", "/v2/", nil, map[string]string{"Authorization": "Bearer invalid"}), http.StatusUnauthorized)
	}
	assertCode(t, f.request("GET", "/v2/", nil, map[string]string{"Authorization": "Bearer invalid"}), http.StatusTooManyRequests)
}
