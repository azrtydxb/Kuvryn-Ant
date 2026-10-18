package rawrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type publishFixture struct {
	pool                 *pgxpool.Pool
	service              *artifact.Service
	principal            identity.Principal
	feed, source, domain string
	disk                 *blob.Disk
	secret               string
	repo                 *Repository
}

func newPublishFixture(t *testing.T) publishFixture {
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
	schema := fmt.Sprintf("ant_raw_%d", time.Now().UnixNano())
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
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := (identity.Tokens{DB: pool}).Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	f := publishFixture{pool: pool, principal: principal, secret: token.Secret}
	if err = pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'artifacts','raw') RETURNING id", owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','raw') RETURNING id", owner.InstanceID).Scan(&f.source); err != nil {
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
	f.repo, err = New(pool, f.service)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func checksum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (f publishFixture) request(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/feeds/artifacts/raw/"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.secret)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.repo.ServeHTTP(w, req)
	return w
}
func TestRawLifecycleAndHTTPConditions(t *testing.T) {
	f := newPublishFixture(t)
	body := "0123456789"
	w := f.request("PUT", "team/a.bin", body, map[string]string{"X-Checksum-Sha256": checksum(body)})
	if w.Code != 201 {
		t.Fatalf("put %d %s", w.Code, w.Body)
	}
	tag := w.Header().Get("ETag")
	cases := []struct {
		method  string
		headers map[string]string
		status  int
		body    string
	}{
		{"GET", nil, 200, body}, {"HEAD", nil, 200, ""},
		{"GET", map[string]string{"Range": "bytes=2-4"}, 206, "234"},
		{"GET", map[string]string{"Range": "bytes=-3"}, 206, "789"},
		{"GET", map[string]string{"Range": "bytes=7-"}, 206, "789"},
		{"GET", map[string]string{"Range": "bytes=99-"}, 416, ""},
		{"GET", map[string]string{"If-None-Match": "W/" + tag}, 304, ""},
		{"GET", map[string]string{"If-Match": "W/" + tag}, 412, ""},
		{"GET", map[string]string{"If-Match": tag, "Range": "bytes=1-2"}, 206, "12"},
		{"GET", map[string]string{"If-Match": "\"other\"", "If-None-Match": tag}, 412, ""},
		{"GET", map[string]string{"If-Range": "\"other\"", "Range": "bytes=1-2"}, 200, body},
		{"HEAD", map[string]string{"Range": "bytes=1-2"}, 200, ""},
	}
	for _, tc := range cases {
		w = f.request(tc.method, "team/a.bin", "", tc.headers)
		if w.Code != tc.status || (tc.status != 416 && w.Body.String() != tc.body) {
			t.Errorf("%s %v: %d %q", tc.method, tc.headers, w.Code, w.Body.String())
		}
	}
	w = f.request("GET", "team/a.bin", "", map[string]string{"Range": "bytes=0-1,8-9"})
	if w.Code != 206 || !strings.HasPrefix(w.Header().Get("Content-Type"), "multipart/byteranges;") || !strings.Contains(w.Body.String(), "01") || !strings.Contains(w.Body.String(), "89") {
		t.Fatalf("multipart %d %s", w.Code, w.Body)
	}
	if w = f.request("PUT", "team/a.bin", body, nil); w.Code != 204 {
		t.Fatalf("replay %d", w.Code)
	}
	if w = f.request("PUT", "team/a.bin", "changed", nil); w.Code != 409 {
		t.Fatalf("overwrite %d", w.Code)
	}
	if w = f.request("DELETE", "team/a.bin", "", nil); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if w = f.request("PUT", "team/a.bin", body, map[string]string{"If-Match": tag}); w.Code != 501 {
		t.Fatal(w.Code)
	}
	// Each sibling is a separate coordinate, not a package version.
	if w = f.request("PUT", "team/b.bin", "sibling", nil); w.Code != 201 {
		t.Fatal(w.Code)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM components WHERE coordinate IN ('team/a.bin','team/b.bin')").Scan(&count); err != nil || count != 2 {
		t.Fatalf("path coordinates %d %v", count, err)
	}
}
func TestRawIntegrityAndUnknownLength(t *testing.T) {
	f := newPublishFixture(t)
	for _, tc := range []struct {
		name   string
		size   int64
		digest string
		status int
	}{
		{"short", 7, checksum("hello"), 400}, {"long", 3, checksum("hello"), 400}, {"digest", 5, checksum("other"), 400},
		{"unknown", -1, checksum("hello"), 201}, {"unknown-auto", -1, "", 201}, {"spool-short", 7, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", "/feeds/artifacts/raw/"+tc.name, strings.NewReader("hello"))
			req.ContentLength = tc.size
			req.Header.Set("Authorization", "Bearer "+f.secret)
			if tc.digest != "" {
				req.Header.Set("X-Checksum-Sha256", tc.digest)
			}
			w := httptest.NewRecorder()
			f.repo.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if tc.status == 400 {
				w = f.request("GET", tc.name, "", nil)
				if w.Code != 404 {
					t.Fatal("invalid upload visible", w.Code)
				}
			}
		})
	}
}
func TestRawTraversal(t *testing.T) {
	for _, path := range []string{"../x", "a/../x", "a//x", "a/./x", "a/%2e%2e/x", "a%2fx", "a%5cx", "a/%252e%252e/x", "a/%00x", "a/", "", strings.Repeat("x", 257)} {
		req := httptest.NewRequest("GET", "/feeds/artifacts/raw/"+path, nil)
		if _, _, err := parsePath(req); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	req := httptest.NewRequest("GET", "/feeds/artifacts/raw/team/a%20b.bin", nil)
	if slug, key, err := parsePath(req); err != nil || slug != "artifacts" || key != "team/a b.bin" {
		t.Fatal(slug, key, err)
	}
}
func TestRawAuthorizationAndInstanceIsolation(t *testing.T) {
	f := newPublishFixture(t)
	if w := f.request("PUT", "private/a", "secret", nil); w.Code != 201 {
		t.Fatal(w.Code)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		action identity.Action
		method string
		status int
	}{{identity.Read, "PUT", 403}, {identity.Publish, "GET", 404}} {
		token, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Actions: []identity.Action{tc.action}}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		w := f.request(tc.method, "private/a", "blocked", map[string]string{"Authorization": "Bearer " + token.Secret})
		if w.Code != tc.status {
			t.Fatalf("scope %d %s", w.Code, w.Body)
		}
	}
	var owner identity.BootstrapIdentity
	err := f.pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('other') RETURNING id").Scan(&owner.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.pool.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','foreign') RETURNING id", owner.InstanceID).Scan(&owner.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: f.pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PUT"} {
		w := f.request(method, "private/a", "foreign", map[string]string{"Authorization": "Bearer " + token.Secret})
		if w.Code != 404 {
			t.Fatalf("cross instance %d", w.Code)
		}
	}
	if w := f.request("GET", "private/a", "", map[string]string{"Authorization": ""}); w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestRawConcurrentImmutablePUT(t *testing.T) {
	f := newPublishFixture(t)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- f.request("PUT", "race/file", fmt.Sprintf("body-%d", i), nil).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == 201 {
			created++
		} else if code != 409 {
			t.Errorf("unexpected %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("created %d", created)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='artifact.publish'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit %d %v", n, err)
	}
}

func TestRawIndependentPathOwnership(t *testing.T) {
	f := newPublishFixture(t)
	for _, path := range []string{"team/a", "team/b"} {
		if w := f.request("PUT", path, path, nil); w.Code != 201 {
			t.Fatal(w.Code)
		}
	}
	ctx := context.Background()
	var preferred string
	if err := f.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'preferred','local','raw') RETURNING id", f.principal.InstanceID).Scan(&preferred); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)", f.principal.InstanceID, f.feed, preferred); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'team/a')", f.principal.InstanceID, preferred); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "team/a", "", nil); w.Code != 404 {
		t.Fatal("owner miss fell through", w.Code)
	}
	if w := f.request("GET", "team/b", "", nil); w.Code != 200 || w.Body.String() != "team/b" {
		t.Fatal("sibling ownership crossed paths", w.Code)
	}
	token, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Namespace: "other", Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PUT"} {
		w := f.request(method, "team/b", "forbidden", map[string]string{"Authorization": "Bearer " + token.Secret})
		if w.Code != 404 && w.Code != 403 {
			t.Fatal("namespace escaped", w.Code)
		}
	}
}

type cancelReader struct {
	reader *strings.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}
func TestRawCancelledUploadInvisible(t *testing.T) {
	f := newPublishFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("PUT", "/feeds/artifacts/raw/cancelled", cancelReader{strings.NewReader("partial"), cancel}).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.secret)
	w := httptest.NewRecorder()
	f.repo.ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatal("cancelled upload succeeded", w.Code)
	}
	if w = f.request("GET", "cancelled", "", nil); w.Code != 404 {
		t.Fatal("cancelled upload visible", w.Code)
	}
}
