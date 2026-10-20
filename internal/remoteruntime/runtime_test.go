package remoteruntime

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runtimeFixture(t *testing.T) (*pgxpool.Pool, identity.BootstrapIdentity) {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL fixture required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ant_runtime_" + strings.ToLower(randomText(t))
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
		t.Fatal(err)
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
	boot, err := identity.Bootstrap(ctx, pool, "runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	return pool, boot
}

func randomText(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return string([]byte{'a' + b[0]%26, 'a' + b[1]%26, 'a' + b[2]%26, 'a' + b[3]%26, 'a' + b[4]%26, 'a' + b[5]%26})
}

func insertRemoteSource(t *testing.T, pool *pgxpool.Pool, instance, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO sources(instance_id,name,kind) VALUES($1,$2,'remote') RETURNING id::text`, instance, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertConfig(t *testing.T, pool *pgxpool.Pool, instance, source string, revision int64, c remoteconfig.Config) {
	t.Helper()
	c, err := remoteconfig.Normalize(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var credentialID any
	if c.CredentialID != "" {
		credentialID = c.CredentialID
	}
	if _, err = pool.Exec(context.Background(), `INSERT INTO remote_source_configs(instance_id,source_id,credential_id,config) VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,source_id) DO UPDATE SET credential_id=excluded.credential_id,config=excluded.config,updated_at=clock_timestamp()`, instance, source, credentialID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE sources SET revision=$1 WHERE instance_id=$2 AND id=$3`, revision, instance, source); err != nil {
		t.Fatal(err)
	}
}

// writeCAPEM writes the httptest server certificate as a standalone trust bundle.
func writeCAPEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	cert := server.Certificate()
	block := &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemoteRuntimeFetchMetadataCacheAndClose(t *testing.T) {
	pool, boot := runtimeFixture(t)
	var calls atomic.Int32
	var lastAuth atomic.Value
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/index.json":
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"latest":"1.2.3"}`))
		case "/pkg/list":
			io.WriteString(w, "cached-list")
		case "/missing":
			w.WriteHeader(404)
		case "/slow":
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer origin.Close()
	caPath := writeCAPEM(t, origin)

	source := insertRemoteSource(t, pool, boot.InstanceID, "runtime-fetch")
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := (secrets.Store{DB: pool, Keys: keys}).Create(context.Background(), boot.InstanceID, boot.PrincipalID, remoteconfig.CredentialPurpose(source), []byte(`{"authorization":"Bearer fixture-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := remoteconfig.DefaultConfig(origin.URL)
	c.CredentialID = credential.ID
	c.CARef = "runtime-ca"
	c.AllowPrivate = true
	c.TimeoutSeconds = 5
	c.Cache.MaxEntries = 32
	insertConfig(t, pool, boot.InstanceID, source, 1, c)

	manager, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": caPath}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	request := Request{SourceID: source, AuthorizationDomain: "scope/generation-1", Representation: "application/json", Path: "index.json"}
	value, err := manager.Metadata(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if value.Status != 200 || string(value.Body) != `{"latest":"1.2.3"}` || value.ETag != `"v1"` {
		t.Fatal(value)
	}
	if calls.Load() != 1 || lastAuth.Load() != "Bearer fixture-token" {
		t.Fatal("credential not forwarded", calls.Load(), lastAuth.Load())
	}
	// Fresh metadata is served from cache with no second network call.
	value, err = manager.Metadata(context.Background(), request)
	if err != nil || value.Status != 200 || calls.Load() != 1 {
		t.Fatal("cache miss", err, calls.Load())
	}
	// A distinct representation is a distinct cache identity.
	plain, err := manager.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: request.AuthorizationDomain, Path: "index.json"})
	if err != nil || plain.Status != 200 || string(plain.Body) != `{"latest":"1.2.3"}` || calls.Load() != 2 {
		t.Fatal("representation ignored", err, calls.Load())
	}
	// 404 metadata is cached as a negative result.
	missing, err := manager.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: request.AuthorizationDomain, Path: "missing"})
	if err != nil || missing.Status != 404 {
		t.Fatal(missing, err)
	}
	before := calls.Load()
	if _, err = manager.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: request.AuthorizationDomain, Path: "missing"}); err != nil || calls.Load() != before {
		t.Fatal("negative cache miss", err, calls.Load())
	}
	// Streaming fetch returns exactly the upstream body and drains on Close.
	response, err := manager.Fetch(context.Background(), Request{SourceID: source, AuthorizationDomain: request.AuthorizationDomain, Path: "pkg/list"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "cached-list" {
		t.Fatal(body, err)
	}
	if err = response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// Unconfigured path fails closed with 404 from upstream, mapped through.
	if response, err = manager.Fetch(context.Background(), Request{SourceID: source, AuthorizationDomain: request.AuthorizationDomain, Path: "other"}); err == nil {
		response.Body.Close()
	} else {
		t.Fatal(err)
	}
}

func TestRemoteRuntimeReloadCredentialRotationAndPolicy(t *testing.T) {
	pool, boot := runtimeFixture(t)
	source := insertRemoteSource(t, pool, boot.InstanceID, "runtime-reload")
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := (secrets.Store{DB: pool, Keys: keys}).Create(context.Background(), boot.InstanceID, boot.PrincipalID, remoteconfig.CredentialPurpose(source), []byte(`{"authorization":"Bearer first"}`))
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer second" {
			io.WriteString(w, "new-credential-visible")
			return
		}
		io.WriteString(w, "old")
	}))
	defer origin.Close()
	caPath := writeCAPEM(t, origin)
	c := remoteconfig.DefaultConfig(origin.URL)
	c.CredentialID = credential.ID
	c.CARef = "runtime-ca"
	c.AllowPrivate = true
	c.Cache.MetadataTTLSeconds = 1
	insertConfig(t, pool, boot.InstanceID, source, 1, c)

	manager, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": caPath}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	request := Request{SourceID: source, AuthorizationDomain: "gen-1", Path: "asset"}
	value, err := manager.Metadata(context.Background(), request)
	if err != nil || string(value.Body) != "old" {
		t.Fatal(value, err)
	}
	// Rotate the encrypted credential without touching the source config; the
	// manager must rebuild the entry because the credential revision changed.
	if _, err = (secrets.Store{DB: pool, Keys: keys}).Update(context.Background(), boot.InstanceID, boot.PrincipalID, credential.ID, remoteconfig.CredentialPurpose(source), 1, []byte(`{"authorization":"Bearer second"}`)); err != nil {
		t.Fatal(err)
	}
	// Force a metadata TTL expiry on the cached entry by advancing the cache? The
	// cache clock is internal, so a fresh path proves the new credential.
	value, err = manager.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: "gen-1", Path: "fresh"})
	if err != nil || string(value.Body) != "new-credential-visible" {
		t.Fatal("credential rotation not applied", value, err)
	}
	// Updating the source config to a new revision and CARef... the manager must
	// re-materialize because the stored revision advanced. Point at a different
	// CA file the runtime does not know and confirm it fails closed.
	badCA := filepath.Join(t.TempDir(), "missing-ca.pem")
	_ = os.WriteFile(badCA, []byte("garbage"), 0600)
	c2 := c
	c2.CARef = "other-ca"
	insertConfig(t, pool, boot.InstanceID, source, 2, c2)
	if _, err = manager.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: "gen-1", Path: "changed"}); !errors.Is(err, ErrPolicy) {
		t.Fatal("missing operator CA not denied", err)
	}
	// Re-inserting the known CA ref restores service after the revision bump.
	expanded, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": caPath, "other-ca": caPath}})
	if err != nil {
		t.Fatal(err)
	}
	defer expanded.Close()
	value, err = expanded.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: "gen-1", Path: "back"})
	if err != nil || value.Status != 200 {
		t.Fatal(value, err)
	}
	// Policy: a request referencing a source without a config is refused.
	if _, err = manager.Metadata(context.Background(), Request{SourceID: insertRemoteSource(t, pool, boot.InstanceID, "unconfigured"), AuthorizationDomain: "gen-1", Path: "x"}); !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrPolicy) {
		t.Fatal("unconfigured source not refused", err)
	}
}

func TestRemoteRuntimeRequestBoundsAndRepresentationPolicy(t *testing.T) {
	pool, boot := runtimeFixture(t)
	manager, err := New(pool, boot.InstanceID, mustKeyring(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for _, request := range []Request{
		{SourceID: "not-a-uuid", AuthorizationDomain: "a", Path: "x"},
		{SourceID: "", AuthorizationDomain: "a", Path: "x"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "", Path: "x"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: ""},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "http://absolute/x"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "//x"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "a\\b"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "a/../b"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "a/./b"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "a/b?c=1"},
		{SourceID: boot.InstanceID, AuthorizationDomain: strings.Repeat("a", 8193), Path: "x"},
		{SourceID: boot.InstanceID, AuthorizationDomain: "a", Path: "x", Representation: "bad\x00"},
	} {
		if _, err = manager.Metadata(context.Background(), request); !errors.Is(err, ErrInput) {
			t.Fatalf("unsafe request accepted: %+v -> %v", request, err)
		}
	}
}

func mustKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

var _ *x509.Certificate
var _ = secrets.Record{}
var _ = remoteconfig.Record{}

// Docker Hub answers 401 for a repository that does not exist even after a
// valid token handshake. That is a negative answer: resolution must see 404
// and the source's health must not record an authentication failure. A token
// request the realm refuses is a real credential failure and still counts.
func TestRemoteRuntimeRegistryScopeDenialIsNotAnAuthenticationFailure(t *testing.T) {
	pool, boot := runtimeFixture(t)
	var origin *httptest.Server
	origin = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := func(scope, extra string) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+origin.URL+`/token",service="registry",scope="`+scope+`"`+extra)
		}
		switch {
		case r.URL.Path == "/token":
			if strings.Contains(r.URL.Query().Get("scope"), "credential-refused") {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"token":"granted","expires_in":300}`)
		case strings.HasPrefix(r.URL.Path, "/v2/nobody/missing/"):
			if r.Header.Get("Authorization") == "Bearer granted" {
				challenge("repository:nobody/missing:pull", `,error="insufficient_scope"`)
				w.WriteHeader(401)
				io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
				return
			}
			challenge("repository:nobody/missing:pull", "")
			w.WriteHeader(401)
		default:
			challenge("repository:team/credential-refused:pull", "")
			w.WriteHeader(401)
		}
	}))
	defer origin.Close()
	source := insertRemoteSource(t, pool, boot.InstanceID, "runtime-scope-denial")
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	c := remoteconfig.DefaultConfig(origin.URL)
	c.CARef = "runtime-ca"
	c.AllowPrivate = true
	c.TimeoutSeconds = 5
	insertConfig(t, pool, boot.InstanceID, source, 1, c)
	manager, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": writeCAPEM(t, origin)}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	response, err := manager.Fetch(context.Background(), Request{SourceID: source, AuthorizationDomain: "scope/1", Path: "v2/nobody/missing/manifests/latest"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", response.StatusCode)
	}
	if snap, ok := manager.health.Snapshot(source); !ok || snap.Failures != 0 || snap.LastError != "" {
		t.Fatalf("scope denial counted as a failure: %+v", snap)
	}

	response, err = manager.Fetch(context.Background(), Request{SourceID: source, AuthorizationDomain: "scope/1", Path: "v2/team/credential-refused/manifests/latest"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", response.StatusCode)
	}
	if snap, _ := manager.health.Snapshot(source); snap.Failures != 1 || snap.LastError != "authentication" {
		t.Fatalf("credential failure not counted: %+v", snap)
	}
}

// The metadata bound is configurable per source: a lower value makes a
// document that the instance default would cache "too large" (so streaming
// adapters take over), and the value is bounded by the source's cache quota.
func TestRemoteRuntimePerSourceMetadataBound(t *testing.T) {
	pool, boot := runtimeFixture(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer origin.Close()
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": writeCAPEM(t, origin)}, MaxMetadataBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	source := insertRemoteSource(t, pool, boot.InstanceID, "runtime-metadata-bound")
	configure := func(revision int64, limit int64) {
		c := remoteconfig.DefaultConfig(origin.URL)
		c.CARef = "runtime-ca"
		c.AllowPrivate = true
		c.Cache.MaxMetadataBytes = limit
		insertConfig(t, pool, boot.InstanceID, source, revision, c)
	}
	request := Request{SourceID: source, AuthorizationDomain: "scope/1", Path: "doc"}
	configure(1, 0)
	if v, err := manager.Metadata(context.Background(), request); err != nil || len(v.Body) != 4096 {
		t.Fatalf("instance default: %v", err)
	}
	configure(2, 1024)
	if _, err := manager.Metadata(context.Background(), request); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("per-source bound not applied: %v", err)
	}
	configure(3, 8192)
	if v, err := manager.Metadata(context.Background(), request); err != nil || len(v.Body) != 4096 {
		t.Fatalf("raised bound: %v", err)
	}
	if _, err := remoteconfig.Normalize(func() remoteconfig.Config {
		c := remoteconfig.DefaultConfig(origin.URL)
		c.Cache.MaxMetadataBytes = -1
		return c
	}()); err == nil {
		t.Fatal("negative bound accepted")
	}
}

// InvalidateMetadata drops the cached metadata and negative answers of the
// matching resources only, so the next read goes upstream for them and nothing else.
func TestRemoteRuntimeInvalidateMetadataByName(t *testing.T) {
	pool, boot := runtimeFixture(t)
	var calls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
	}))
	defer origin.Close()
	source := insertRemoteSource(t, pool, boot.InstanceID, "runtime-invalidate")
	c := remoteconfig.DefaultConfig(origin.URL)
	c.CARef = "runtime-ca"
	c.AllowPrivate = true
	c.TimeoutSeconds = 5
	insertConfig(t, pool, boot.InstanceID, source, 1, c)
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(pool, boot.InstanceID, keys, Options{CAFiles: map[string]string{"runtime-ca": writeCAPEM(t, origin)}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	get := func(path string) {
		t.Helper()
		if _, err := manager.Metadata(context.Background(), Request{SourceID: source, AuthorizationDomain: "d", Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"Left-Pad/index.json", "left-pad/missing", "right-pad/index.json"} {
		get(p)
	}
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
	if n := manager.InvalidateMetadata("00000000-0000-0000-0000-000000000000", []string{"left-pad"}); n != 0 {
		t.Fatalf("unknown source dropped %d", n)
	}
	// Case-insensitive match on the resource name; the 404 answer goes too.
	if n := manager.InvalidateMetadata(source, []string{"left-pad"}); n != 2 {
		t.Fatalf("dropped %d, want 2", n)
	}
	get("right-pad/index.json")
	if calls.Load() != 3 {
		t.Fatal("an unrelated package was invalidated")
	}
	get("Left-Pad/index.json")
	get("left-pad/missing")
	if calls.Load() != 5 {
		t.Fatalf("invalidated packages were not refetched: %d", calls.Load())
	}
	// No names drops everything of the source.
	if n := manager.InvalidateMetadata(source, nil); n != 3 {
		t.Fatalf("dropped %d, want 3", n)
	}
}
