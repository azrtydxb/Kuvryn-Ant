package huggingface

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

type hfFixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Repository
	service                                        *artifact.Service
	blobs                                          string
	root, token, instance, principal, feed, source string
}

func hfSetup(t *testing.T) hfFixture {
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
	schema := "ant_hf_" + strings.ToLower(rand.Text())
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
	owner, err := identity.Bootstrap(ctx, pool, "HF")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := hfFixture{blobs: filepath.Join(t.TempDir(), "blobs"), pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'hub','huggingface') RETURNING id::text`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','huggingface') RETURNING id::text`, owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, owner.InstanceID, "source:"+f.source); err != nil {
		t.Fatal(err)
	}
	disk, err := blob.NewDisk(f.blobs, blob.TransferBufferSize*4)
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
func (f hfFixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
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
func status(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status %d expected %d: %s", r.Code, want, r.Body.String())
	}
}
func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

const base = "/feeds/hub/huggingface"

// publish uploads files into a session and commits, returning the commit sha.
func (f hfFixture) publish(t *testing.T, typ, repo, session string, files map[string]string, refs ...string) string {
	t.Helper()
	for p, c := range files {
		status(t, f.request("PUT", base+"/api/"+typ+"/"+repo+"/ant-upload/"+session+"/"+p, []byte(c), map[string]string{"X-Checksum-Sha256": sum([]byte(c))}), 200)
	}
	body, _ := json.Marshal(map[string]any{"refs": refs})
	r := f.request("POST", base+"/api/"+typ+"/"+repo+"/ant-commit/"+session, body, nil)
	status(t, r, 200)
	var out struct{ SHA string }
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || len(out.SHA) != 40 {
		t.Fatal(r.Body.String(), err)
	}
	return out.SHA
}

func TestHuggingFaceModelAndDatasetLifecycle(t *testing.T) {
	f := hfSetup(t)
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	model := map[string]string{"config.json": `{"a":1}`, "weights/model.bin": big, "README.md": "# hi"}
	sha := f.publish(t, "models", "acme/tiny", "session-00000001", model, "main", "v1")

	// Repo info via default, branch, tag, and commit.
	for _, rev := range []string{"", "/revision/main", "/revision/v1", "/revision/" + sha} {
		r := f.request("GET", base+"/api/models/acme/tiny"+rev, nil, nil)
		status(t, r, 200)
		var info struct {
			SHA      string
			Siblings []struct{ Rfilename string }
		}
		_ = json.Unmarshal(r.Body.Bytes(), &info)
		if info.SHA != sha || len(info.Siblings) != 3 {
			t.Fatal(rev, r.Body.String())
		}
	}
	status(t, f.request("GET", base+"/api/models/acme/tiny/revision/nope", nil, nil), 404)
	status(t, f.request("GET", base+"/api/models/acme/other", nil, nil), 404)
	r := f.request("GET", base+"/api/models/acme/tiny/tree/main?recursive=true", nil, nil)
	status(t, r, 200)
	if !strings.Contains(r.Body.String(), "weights/model.bin") {
		t.Fatal(r.Body.String())
	}

	// HEAD and GET resolve with HF headers.
	url := base + "/acme/tiny/resolve/main/weights/model.bin"
	r = f.request("HEAD", url, nil, nil)
	status(t, r, 200)
	if r.Header().Get("X-Repo-Commit") != sha || r.Header().Get("ETag") != `"`+sum([]byte(big))+`"` || r.Header().Get("X-Linked-Etag") == "" || r.Header().Get("X-Linked-Size") != fmt.Sprint(len(big)) || r.Header().Get("Content-Length") != fmt.Sprint(len(big)) || r.Body.Len() != 0 {
		t.Fatal(r.Header(), r.Body.Len())
	}
	r = f.request("GET", url, nil, nil)
	status(t, r, 200)
	if r.Body.String() != big {
		t.Fatal("body mismatch")
	}
	// Range and resume.
	r = f.request("GET", url, nil, map[string]string{"Range": "bytes=100-199"})
	status(t, r, 206)
	if r.Body.String() != big[100:200] || r.Header().Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(big)) {
		t.Fatal(r.Header(), r.Body.Len())
	}
	half := len(big) / 2
	r = f.request("GET", url, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-", half)})
	status(t, r, 206)
	if big[:half]+r.Body.String() != big {
		t.Fatal("resume mismatch")
	}
	r = f.request("GET", url, nil, map[string]string{"Range": "bytes=-10"})
	status(t, r, 206)
	if r.Body.String() != big[len(big)-10:] {
		t.Fatal("suffix mismatch")
	}
	status(t, f.request("GET", url, nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(big))}), 416)
	// Stale If-Range returns the full body.
	r = f.request("GET", url, nil, map[string]string{"Range": "bytes=0-9", "If-Range": `"stale"`})
	status(t, r, 200)
	// By commit id; missing file; traversal.
	status(t, f.request("GET", base+"/acme/tiny/resolve/"+sha+"/config.json", nil, nil), 200)
	status(t, f.request("GET", base+"/acme/tiny/resolve/main/missing.txt", nil, nil), 404)
	status(t, f.request("GET", base+"/acme/tiny/resolve/main/../x", nil, nil), 404)

	// Datasets use /datasets/ prefix; single-segment repo ids work.
	dsha := f.publish(t, "datasets", "corpus", "session-00000002", map[string]string{"data/train.csv": "a,b\n1,2\n"}, "main")
	r = f.request("GET", base+"/datasets/corpus/resolve/main/data/train.csv", nil, nil)
	status(t, r, 200)
	if r.Body.String() != "a,b\n1,2\n" || r.Header().Get("X-Repo-Commit") != dsha {
		t.Fatal(r.Body.String())
	}
	status(t, f.request("GET", base+"/api/datasets/corpus", nil, nil), 200)
	// A model path never serves dataset content.
	status(t, f.request("GET", base+"/corpus/resolve/main/data/train.csv", nil, nil), 404)

	// Idempotent republish, then a moving branch.
	if again := f.publish(t, "models", "acme/tiny", "session-00000003", model, "main"); again != sha {
		t.Fatal("not idempotent")
	}
	sha2 := f.publish(t, "models", "acme/tiny", "session-00000004", map[string]string{"config.json": `{"a":2}`}, "main")
	if sha2 == sha {
		t.Fatal("same commit")
	}
	r = f.request("GET", base+"/acme/tiny/resolve/main/config.json", nil, nil)
	if r.Body.String() != `{"a":2}` || r.Header().Get("X-Repo-Commit") != sha2 {
		t.Fatal(r.Body.String())
	}
	r = f.request("GET", base+"/acme/tiny/resolve/v1/config.json", nil, nil)
	if r.Body.String() != `{"a":1}` {
		t.Fatal("tag moved", r.Body.String())
	}
	status(t, f.request("GET", base+"/acme/tiny/resolve/main/README.md", nil, nil), 404)

	// Delete a ref; the commit stays immutable and addressable.
	status(t, f.request("DELETE", base+"/api/models/acme/tiny/ant-ref/v1", nil, nil), 204)
	status(t, f.request("GET", base+"/acme/tiny/resolve/v1/config.json", nil, nil), 404)
	status(t, f.request("GET", base+"/acme/tiny/resolve/"+sha+"/config.json", nil, nil), 200)
	status(t, f.request("DELETE", base+"/api/models/acme/tiny/ant-ref/v1", nil, nil), 404)
	status(t, f.request("GET", base+"/api/whoami-v2", nil, nil), 200)
}

func TestHuggingFaceTamperAndAtomicity(t *testing.T) {
	f := hfSetup(t)
	content := strings.Repeat("x", 4096)
	// Checksum mismatch is rejected before staging.
	status(t, f.request("PUT", base+"/api/models/acme/m/ant-upload/session-00000001/a.bin", []byte(content), map[string]string{"X-Checksum-Sha256": strings.Repeat("0", 64)}), 400)
	// Bad paths and sessions.
	status(t, f.request("PUT", base+"/api/models/acme/m/ant-upload/session-00000001/.ant-manifest.json", []byte("x"), nil), 400)
	status(t, f.request("PUT", base+"/api/models/acme/m/ant-upload/s/a.bin", []byte("x"), nil), 400)
	// Uploaded but uncommitted files are invisible; empty/unknown sessions fail.
	status(t, f.request("PUT", base+"/api/models/acme/m/ant-upload/session-00000002/a.bin", []byte(content), nil), 200)
	status(t, f.request("GET", base+"/acme/m/resolve/main/a.bin", nil, nil), 404)
	status(t, f.request("POST", base+"/api/models/acme/m/ant-commit/session-00000009", nil, nil), 404)
	status(t, f.request("POST", base+"/api/models/acme/m/ant-commit/session-00000002", []byte(`{"refs":["a/b"]}`), nil), 400)
	var versions int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM versions`).Scan(&versions)
	if versions != 0 {
		t.Fatal("partial publish visible", versions)
	}
	sha := f.publish(t, "models", "acme/m", "session-00000003", map[string]string{"a.bin": content}, "main")
	status(t, f.request("GET", base+"/acme/m/resolve/main/a.bin", nil, nil), 200)

	// Tamper: truncate the stored blob; resolve must fail, not serve bad bytes.
	blobPath := ""
	_ = filepath.Walk(f.blobs, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() && i.Size() == int64(len(content)) {
			blobPath = p
		}
		return nil
	})
	if blobPath == "" {
		t.Fatal("blob not found")
	}
	_ = os.Chmod(blobPath, 0o600)
	if err := os.Truncate(blobPath, 100); err != nil {
		t.Fatal(err)
	}
	r := f.request("GET", base+"/acme/m/resolve/"+sha+"/a.bin", nil, nil)
	if r.Code == 200 || r.Code == 206 {
		t.Fatal("tampered blob served", r.Code)
	}
	r = f.request("HEAD", base+"/acme/m/resolve/"+sha+"/a.bin", nil, nil)
	if r.Code == 200 {
		t.Fatal("tampered blob advertised")
	}
}

func TestHuggingFaceIsolationAndAuth(t *testing.T) {
	f := hfSetup(t)
	status(t, f.request("GET", "/feeds/other/huggingface/api/models/a/b", nil, nil), 404)
	status(t, f.request("GET", "/feeds/hub/raw/x", nil, nil), 404)
	status(t, f.request("GET", base+"/api/models/a/b%2Fc", nil, nil), 404)
	status(t, f.request("PATCH", base+"/api/models/acme/m", nil, nil), 405)
	req := httptest.NewRequest("GET", base+"/api/models/acme/m", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	if err := f.registry.SetPublicURL("http://example.com"); err == nil {
		t.Fatal("insecure URL accepted")
	}
	if err := f.registry.SetPublicURL("https://ant.example.com"); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, _, _, _ = artifact.ErrInput, rand.Text, time.Now, identity.Read, blob.ErrIntegrity, ownership.Lease{}, routing.Local, pgx.ErrNoRows
	_ = database.Open
}
