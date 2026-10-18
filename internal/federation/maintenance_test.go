package federation

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

func maintenanceDB(t *testing.T) (*pgxpool.Pool, string, *secrets.Keyring) {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	base, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ant_fed_maint_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg := base.Copy()
	cfg.ConnConfig.Database = name
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "maint")
	if err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	keys, err := secrets.New("v1", map[string][]byte{"v1": k})
	if err != nil {
		t.Fatal(err)
	}
	return pool, owner.InstanceID, keys
}

func TestReIdentifyAndPurge(t *testing.T) {
	ctx := context.Background()
	pool, old, keys := maintenanceDB(t)
	svc, err := New(Config{DB: pool, Keys: keys, InstanceID: old, PublicURL: "https://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	boot, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Found(ctx, boot.PrincipalID, FoundInput{Name: "lab"}); err != nil {
		t.Fatal(err)
	}
	store := secrets.Store{DB: pool, Keys: keys}
	rec, err := store.Create(ctx, old, boot.PrincipalID, "remote-source", []byte("upstream-password"))
	if err != nil {
		t.Fatal(err)
	}
	var feed string
	if err := pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'release','raw') RETURNING id::text`, old).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	// Purge refuses while the instance is an active member.
	if err := Purge(ctx, pool, old); !errors.Is(err, ErrStillFederated) {
		t.Fatalf("purge of an active member: %v", err)
	}
	// Re-identify refuses while a serving process holds the writer lease.
	if _, err := pool.Exec(ctx, `INSERT INTO instance_writer_leases(instance_id,owner_id,epoch,expires_at) VALUES($1,gen_random_uuid(),1,now()+interval '1 minute')`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := ReIdentify(ctx, pool, keys, old); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("re-identify under a live lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE instance_writer_leases SET expires_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	fresh, err := ReIdentify(ctx, pool, keys, old)
	if err != nil {
		t.Fatalf("re-identify: %v", err)
	}
	if fresh == old {
		t.Fatal("instance ID unchanged")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE id=$1`, old).Scan(&n)
	if n != 0 {
		t.Fatal("old instance row still present")
	}
	owner, err := identity.LoadBootstrapIdentity(ctx, pool)
	if err != nil || owner.InstanceID != fresh {
		t.Fatalf("bootstrap identity after re-identify: %+v %v", owner, err)
	}
	if _, plain, err := store.Open(ctx, fresh, rec.ID, "remote-source"); err != nil || string(plain) != "upstream-password" {
		t.Fatalf("secret after re-identify: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM feeds WHERE instance_id=$1 AND id=$2`, fresh, feed).Scan(&n)
	if n != 1 {
		t.Fatal("feed not moved")
	}
	st, err := loadState(ctx, pool, fresh)
	if err != nil || st.State != "none" {
		t.Fatalf("federation state after re-identify: %+v %v", st, err)
	}
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM federation_documents)+(SELECT count(*) FROM encrypted_secrets WHERE purpose LIKE 'federation-%')`).Scan(&n)
	if n != 0 {
		t.Fatalf("federation rows left after re-identify: %d", n)
	}
	// Constraints are back to immediate and triggers active.
	var deferrable int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype='f' AND condeferrable AND connamespace=current_schema()::regnamespace`).Scan(&deferrable)
	if deferrable != 0 {
		t.Fatalf("%d foreign keys left deferrable", deferrable)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'x','raw')`, old); err == nil {
		t.Fatal("foreign key not enforced after re-identify")
	}
	// A re-identified copy can found again; after leaving, purge removes the rows.
	svc2, _ := New(Config{DB: pool, Keys: keys, InstanceID: fresh, PublicURL: "https://127.0.0.1:1"})
	if _, err := svc2.Found(ctx, owner.PrincipalID, FoundInput{Name: "lab2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE federation_state SET state='left'`); err != nil {
		t.Fatal(err)
	}
	if err := Purge(ctx, pool, fresh); err != nil {
		t.Fatalf("purge: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM federation_documents)+(SELECT count(*) FROM federation_state)+(SELECT count(*) FROM federation_members)`).Scan(&n)
	if n != 0 {
		t.Fatalf("federation rows after purge: %d", n)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM federation_documents`); err != nil {
		t.Fatal(err) // no rows; the append-only trigger must be enabled again
	}
	var enabled string
	_ = pool.QueryRow(ctx, `SELECT tgenabled::text FROM pg_trigger WHERE tgname='federation_documents_immutable'`).Scan(&enabled)
	if enabled != "O" {
		t.Fatalf("append-only trigger state %q", enabled)
	}
}
