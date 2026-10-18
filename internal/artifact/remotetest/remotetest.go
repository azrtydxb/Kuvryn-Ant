// Package remotetest builds the PostgreSQL-backed pull-through fixture shared by
// the seam and adapter integration tests: schema, principal and token, artifact
// service with a disk store, remote runtime, and TLS httptest upstreams trusted
// through an operator CA map. It is test support only.
package remotetest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CARef is the operator CA identifier every fixture remote source uses.
const CARef = "fixture-ca"

type Fixture struct {
	T         *testing.T
	Pool      *pgxpool.Pool
	Boot      identity.BootstrapIdentity
	Principal identity.Principal
	Token     string
	Service   *artifact.Service
	Manager   *remoteruntime.Manager
	Disk      *blob.Disk
	Keys      *secrets.Keyring
	caPath    string
}

// New skips when no PostgreSQL fixture is configured.
func New(t *testing.T) *Fixture {
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
		t.Fatal(err)
	}
	var salt [6]byte
	_, _ = rand.Read(salt[:])
	schema := "ant_remote_"
	for _, b := range salt {
		schema += string(rune('a' + b%26))
	}
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
	boot, err := identity.Bootstrap(ctx, pool, "Remote")
	if err != nil {
		t.Fatal(err)
	}
	tokens := identity.Tokens{DB: pool}
	token, err := tokens.Issue(ctx, boot.InstanceID, boot.PrincipalID, boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := tokens.Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := ownership.Acquire(ctx, pool, boot.InstanceID, "00000000-0000-4000-8000-000000000001", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := blob.NewDisk(filepath.Join(t.TempDir(), "blobs"), blob.TransferBufferSize*8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{T: t, Pool: pool, Boot: boot, Principal: principal, Token: token.Secret, Disk: disk, Keys: keys}
	// httptest TLS servers share one built-in certificate, so a single operator
	// CA entry trusts every fixture upstream.
	probe := httptest.NewTLSServer(http.NotFoundHandler())
	f.caPath = filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(f.caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: probe.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	probe.Close()
	f.Manager, err = remoteruntime.New(pool, boot.InstanceID, keys, remoteruntime.Options{CAFiles: map[string]string{CARef: f.caPath}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Manager.Close() })
	policies := artifact.Policies{DB: pool, InstanceID: boot.InstanceID}
	f.Service = &artifact.Service{DB: pool, Lease: func() ownership.Lease { return lease }, Router: policies.Load, Remote: f.Manager, Storage: func(context.Context, string) (blob.Store, error) {
		return disk, nil
	}}
	return f
}

// Upstream starts a TLS origin trusted by the fixture CA map.
func (f *Fixture) Upstream(h http.Handler) *httptest.Server {
	f.T.Helper()
	s := httptest.NewTLSServer(h)
	f.T.Cleanup(s.Close)
	return s
}

func (f *Fixture) AddFeed(slug, format string) string {
	f.T.Helper()
	var id string
	if err := f.Pool.QueryRow(context.Background(), "INSERT INTO feeds(instance_id,slug,format) VALUES($1,$2,$3) RETURNING id::text", f.Boot.InstanceID, slug, format).Scan(&id); err != nil {
		f.T.Fatal(err)
	}
	return id
}

// AddLocal attaches a new local source (with its deduplication domain).
func (f *Fixture) AddLocal(feed, format, name string, priority int) string {
	f.T.Helper()
	ctx := context.Background()
	var id string
	if err := f.Pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'local',$3) RETURNING id::text", f.Boot.InstanceID, name, format).Scan(&id); err != nil {
		f.T.Fatal(err)
	}
	if _, err := f.Pool.Exec(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)", f.Boot.InstanceID, "source:"+id); err != nil {
		f.T.Fatal(err)
	}
	f.attach(feed, id, priority)
	// The most recently added local source is the publish destination.
	if _, err := f.Pool.Exec(ctx, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id) VALUES($1,$2,$3) ON CONFLICT(feed_id) DO UPDATE SET default_source_id=excluded.default_source_id`, f.Boot.InstanceID, feed, id); err != nil {
		f.T.Fatal(err)
	}
	return id
}

// RemoteOptions adjusts the stored remote configuration.
type RemoteOptions struct {
	Credential      string   // authorization header value; empty means none
	RedirectOrigins []string // extra origins; credentials are never sent to them
	StaleSeconds    int
	MetadataTTL     int
	NegativeTTL     int
}

// AddRemote creates and attaches a remote source pointing at origin.
func (f *Fixture) AddRemote(feed, format, name string, priority int, origin string, o RemoteOptions) string {
	f.T.Helper()
	ctx := context.Background()
	var id string
	if err := f.Pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'remote',$3) RETURNING id::text", f.Boot.InstanceID, name, format).Scan(&id); err != nil {
		f.T.Fatal(err)
	}
	f.attach(feed, id, priority)
	f.SetRemoteConfig(id, origin, o)
	return id
}

// SetRemoteConfig stores (and bumps the revision of) a source configuration.
func (f *Fixture) SetRemoteConfig(source, origin string, o RemoteOptions) {
	f.T.Helper()
	ctx := context.Background()
	c := remoteconfig.DefaultConfig(origin)
	c.CARef = CARef
	c.AllowPrivate = true
	c.TimeoutSeconds = 5
	c.RedirectOrigins = o.RedirectOrigins
	if o.StaleSeconds > 0 {
		c.Cache.StaleIfErrorSeconds = o.StaleSeconds
	}
	if o.MetadataTTL > 0 {
		c.Cache.MetadataTTLSeconds = o.MetadataTTL
	}
	if o.NegativeTTL > 0 {
		c.Cache.NegativeTTLSeconds = o.NegativeTTL
	}
	if o.Credential != "" {
		plain, _ := json.Marshal(map[string]string{"authorization": o.Credential})
		rec, err := (secrets.Store{DB: f.Pool, Keys: f.Keys}).Create(ctx, f.Boot.InstanceID, f.Boot.PrincipalID, remoteconfig.CredentialPurpose(source), plain)
		if err != nil {
			f.T.Fatal(err)
		}
		c.CredentialID = rec.ID
	}
	c, err := remoteconfig.Normalize(c)
	if err != nil {
		f.T.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	var credential any
	if c.CredentialID != "" {
		credential = c.CredentialID
	}
	if _, err = f.Pool.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,credential_id,config) VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,source_id) DO UPDATE SET credential_id=excluded.credential_id,config=excluded.config,updated_at=clock_timestamp()`, f.Boot.InstanceID, source, credential, raw); err != nil {
		f.T.Fatal(err)
	}
	if _, err = f.Pool.Exec(ctx, `UPDATE sources SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, f.Boot.InstanceID, source); err != nil {
		f.T.Fatal(err)
	}
}

func (f *Fixture) attach(feed, source string, priority int) {
	f.T.Helper()
	if _, err := f.Pool.Exec(context.Background(), "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,$4)", f.Boot.InstanceID, feed, source, priority); err != nil {
		f.T.Fatal(err)
	}
}

// CachedAssets counts assets stored under a source (remote cache population).
func (f *Fixture) CachedAssets(source string) int {
	f.T.Helper()
	var n int
	err := f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=$1`, source).Scan(&n)
	if err != nil {
		f.T.Fatal(err)
	}
	return n
}

// Get performs an authenticated GET against a handler served by an httptest
// server, returning status, body and headers.
func (f *Fixture) Get(url string, headers ...string) (int, string, http.Header) {
	f.T.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		f.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.Token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.T.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		f.T.Fatal(err)
	}
	return res.StatusCode, string(body), res.Header
}
