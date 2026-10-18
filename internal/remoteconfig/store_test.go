package remoteconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	store                                  Store
	p                                      identity.Principal
	source, local, otherSource, credential string
	secrets                                secrets.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("ant_remote_%d", time.Now().UnixNano())
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
	owner, err := identity.Bootstrap(ctx, pool, "Remote")
	if err != nil {
		t.Fatal(err)
	}
	scopes := []identity.Scope{{Actions: []identity.Action{identity.Configure}}}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, scopes, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{store: Store{DB: pool, InstanceID: owner.InstanceID, AllowedCARefs: []string{"private-ca"}}, p: identity.Principal{InstanceID: owner.InstanceID, PrincipalID: owner.PrincipalID, TokenID: token.ID, Scopes: scopes}}
	for i, dst := range []*string{&f.source, &f.local, &f.otherSource} {
		kind := "remote"
		if i == 1 {
			kind = "local"
		}
		if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind) VALUES($1,$2,$3) RETURNING id::text`, owner.InstanceID, fmt.Sprintf("source-%d", i), kind).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	ring, err := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f.secrets = secrets.Store{DB: pool, Keys: ring}
	record, err := f.secrets.Create(ctx, owner.InstanceID, owner.PrincipalID, CredentialPurpose(f.source), []byte("private-upstream-token"))
	if err != nil {
		t.Fatal(err)
	}
	f.credential = record.ID
	return f
}
func (f fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.store.DB.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestRemoteConfigPersistCASAndCredentialIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := DefaultConfig("https://EXAMPLE.org:443/repo")
	c.CredentialID = f.credential
	c.CARef = "private-ca"
	c.AllowPrivate = true
	r, err := f.store.Set(ctx, f.p, f.source, 1, c)
	if err != nil || r.Revision != 2 || r.Config.Origin != "https://example.org/repo" {
		t.Fatal(r, err)
	}
	loaded, err := f.store.Get(ctx, f.p, f.source)
	if err != nil || loaded.Config.CredentialID != f.credential || loaded.Revision != 2 {
		t.Fatal(loaded, err)
	}
	raw, _ := json.Marshal(loaded)
	if bytes.Contains(raw, []byte("private-upstream-token")) {
		t.Fatal("plaintext credentials returned")
	}
	if _, err = f.store.Set(ctx, f.p, f.source, 1, c); !errors.Is(err, ErrConflict) {
		t.Fatal("stale CAS", err)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action='source.remote.configure'`) != 1 || f.count(t, `SELECT count(*) FROM outbox_events WHERE topic='source.remote.configure'`) != 1 {
		t.Fatal("incorrect events")
	}
	if _, err = f.store.Set(ctx, f.p, f.otherSource, 1, c); !errors.Is(err, ErrInput) {
		t.Fatal("another source secret referenced", err)
	}
	if _, err = f.store.Set(ctx, f.p, f.local, 1, DefaultConfig("https://example.org")); !errors.Is(err, ErrNotFound) {
		t.Fatal("local source configured", err)
	}
	c.CredentialID = ""
	c.CARef = "unapproved"
	if _, err = f.store.Set(ctx, f.p, f.source, 2, c); !errors.Is(err, ErrInput) {
		t.Fatal("unknown CA accepted", err)
	}
	denied := f.p
	denied.Scopes = []identity.Scope{{Actions: []identity.Action{identity.Read}}}
	if _, err = f.store.Get(ctx, denied, f.source); !errors.Is(err, ErrDenied) {
		t.Fatal("read scope disclosed config", err)
	}
	denied = f.p
	denied.Scopes = []identity.Scope{{FeedID: f.source, Actions: []identity.Action{identity.Configure}}}
	if _, err = f.store.Set(ctx, denied, f.source, 2, c); !errors.Is(err, ErrDenied) {
		t.Fatal("feed scope configured source", err)
	}
	c = DefaultConfig("https://example.org")
	if _, err = f.store.Set(ctx, f.p, f.source, 2, c); err != nil {
		t.Fatal("remove credential", err)
	}
	loaded, err = f.store.Load(ctx, f.source)
	if err != nil || loaded.Config.CredentialID != "" || loaded.Revision != 3 {
		t.Fatal(loaded, err)
	}
}
func TestRemoteConfigConcurrentCASAndRollback(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := DefaultConfig("https://example.org")
	var success atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.store.Set(ctx, f.p, f.source, 1, c)
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 || f.count(t, `SELECT count(*) FROM outbox_events WHERE topic='source.remote.configure'`) != 1 {
		t.Fatal("concurrent CAS")
	}
	if _, err := f.store.DB.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT reject_remote CHECK(topic<>'source.remote.configure') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	c.Origin = "https://changed.example"
	if _, err := f.store.Set(ctx, f.p, f.source, 2, c); !errors.Is(err, ErrUnavailable) {
		t.Fatal("outbox failure", err)
	}
	r, err := f.store.Load(ctx, f.source)
	if err != nil || r.Revision != 2 || r.Config.Origin != "https://example.org" {
		t.Fatal("partial mutation", r, err)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action='source.remote.configure'`) != 1 {
		t.Fatal("orphan audit")
	}
}
func TestRemoteConfigDatabaseGuardsAndInstanceIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := DefaultConfig("https://example.org")
	c.CredentialID = f.credential
	raw, _ := json.Marshal(c)
	if _, err := f.store.DB.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,credential_id,config) VALUES($1,$2,$3,$4)`, f.p.InstanceID, f.otherSource, f.credential, raw); err == nil {
		t.Fatal("DB accepted another source credential")
	}
	c.CredentialID = ""
	raw, _ = json.Marshal(c)
	if _, err := f.store.DB.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,config) VALUES($1,$2,$3)`, f.p.InstanceID, f.local, raw); err == nil {
		t.Fatal("DB accepted local source")
	}
	var otherInstance, foreignSource string
	if err := f.store.DB.QueryRow(ctx, `INSERT INTO instances(name) VALUES('other') RETURNING id::text`).Scan(&otherInstance); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind) VALUES($1,'foreign','remote') RETURNING id::text`, otherInstance).Scan(&foreignSource); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Set(ctx, f.p, foreignSource, 1, c); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross instance source", err)
	}
	policy, err := (identity.Roles{DB: f.store.DB}).Resolve(ctx, f.p)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.SetTx(ctx, tx, f.p, policy, f.source, 1, c); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Load(ctx, f.source); !errors.Is(err, ErrNotFound) {
		t.Fatal("SetTx committed independently", err)
	}
	if f.count(t, `SELECT revision FROM sources WHERE id=$1`, f.source) != 1 {
		t.Fatal("rollback changed revision")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = f.store.Set(canceled, f.p, f.source, 1, c); err == nil {
		t.Fatal("canceled mutation accepted")
	}
}
