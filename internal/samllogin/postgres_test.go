package samllogin

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func replayFixture(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_saml_replay_%d", time.Now().UnixNano())
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
	var own, other string
	for _, dest := range []*string{&own, &other} {
		if err = pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('SAML') RETURNING id").Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	return pool, own, other
}
func replayStore(t *testing.T, pool *pgxpool.Pool, instance, provider string) *PostgresReplay {
	t.Helper()
	store, err := NewPostgresReplay(pool, instance, provider)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
func TestPostgresReplayAtomicConcurrentClaimsAndRestart(t *testing.T) {
	pool, instance, _ := replayFixture(t)
	ctx := context.Background()
	store := replayStore(t, pool, instance, "provider-a")
	expires := time.Now().Add(time.Hour)
	var wg sync.WaitGroup
	wins := make(chan bool, 12)
	failures := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys := []string{"private-request-id", "private-assertion-id"}
			if i%2 == 1 {
				keys[0], keys[1] = keys[1], keys[0]
			}
			ok, err := store.Claim(ctx, keys, expires)
			wins <- ok
			failures <- err
		}(i)
	}
	wg.Wait()
	close(wins)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for ok := range wins {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent successful claims=%d", count)
	}
	restarted := replayStore(t, pool, strings.ToUpper(instance), "provider-a")
	if ok, err := restarted.Claim(ctx, []string{"new-request-id", "private-assertion-id"}, expires); err != nil || ok {
		t.Fatal("restart replay accepted", err)
	}
	if ok, err := restarted.Claim(ctx, []string{"new-request-id"}, expires); err != nil || !ok {
		t.Fatal("failed multi-claim partially persisted", err)
	}
	if ok, err := store.Claim(ctx, []string{"duplicate-id", "duplicate-id"}, expires); err != nil || !ok {
		t.Fatal("duplicate input set not canonicalized", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM saml_replay_claims").Scan(&rows); err != nil || rows != 4 {
		t.Fatal("unexpected claim rows", err)
	}
	var plaintext bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM saml_replay_claims r WHERE row_to_json(r)::text LIKE '%private-%')").Scan(&plaintext); err != nil || plaintext {
		t.Fatal("raw replay ID persisted", err)
	}
}
func TestPostgresReplayExpiryAndProviderInstanceIsolation(t *testing.T) {
	pool, instance, other := replayFixture(t)
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	a := replayStore(t, pool, instance, "provider-a")
	for _, store := range []*PostgresReplay{a, replayStore(t, pool, instance, "provider-b"), replayStore(t, pool, other, "provider-a")} {
		if ok, err := store.Claim(ctx, []string{"same-opaque-id"}, expires); err != nil || !ok {
			t.Fatal("scope collision", err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE saml_replay_claims SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE instance_id=$1 AND provider_hash=$2", instance, a.provider[:]); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Claim(ctx, []string{"same-opaque-id"}, expires); err != nil || !ok {
		t.Fatal("expired key not reusable", err)
	}
	if ok, err := replayStore(t, pool, other, "provider-a").Claim(ctx, []string{"same-opaque-id"}, expires); err != nil || ok {
		t.Fatal("expiry crossed scope", err)
	}
}
func TestPostgresReplayInvalidAndStorageFailureClosed(t *testing.T) {
	pool, instance, _ := replayFixture(t)
	ctx := context.Background()
	s := replayStore(t, pool, instance, "provider")
	for _, expires := range []time.Time{time.Time{}, time.Now().Add(-time.Second), time.Now().Add(25 * time.Hour)} {
		if ok, err := s.Claim(ctx, []string{"secret-opaque-id"}, expires); ok || !errors.Is(err, ErrReplayConfiguration) || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid expiry accepted or leaked")
		}
	}
	for _, keys := range [][]string{nil, {""}, {strings.Repeat("x", maxReplayKeyBytes+1)}, make([]string, maxReplayKeys+1)} {
		if ok, err := s.Claim(ctx, keys, time.Now().Add(time.Hour)); ok || !errors.Is(err, ErrReplayConfiguration) {
			t.Fatal("invalid keys accepted")
		}
	}
	for _, provider := range []string{"", " padded ", "line\nbreak", strings.Repeat("x", 2049)} {
		if _, err := NewPostgresReplay(pool, instance, provider); !errors.Is(err, ErrReplayConfiguration) {
			t.Fatal("invalid provider accepted")
		}
	}
	if _, err := NewPostgresReplay(pool, "invalid", "provider"); !errors.Is(err, ErrReplayConfiguration) {
		t.Fatal("invalid instance accepted")
	}
	if _, err := pool.Exec(ctx, "DROP TABLE saml_replay_claims"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(ctx, []string{"private-id"}, time.Now().Add(time.Hour)); ok || !errors.Is(err, ErrReplayStorage) || strings.Contains(err.Error(), "private-id") {
		t.Fatal("missing storage did not fail closed")
	}
}
func TestPostgresReplayRollsBackEarlierKeyOnInsertFailure(t *testing.T) {
	pool, instance, _ := replayFixture(t)
	ctx := context.Background()
	s := replayStore(t, pool, instance, "provider")
	digest := sha256.Sum256([]byte("blocked"))
	// A constraint injected in this isolated schema forces the second key insert
	// to fail, verifying the first key is rolled back rather than consumed.
	query := fmt.Sprintf("ALTER TABLE saml_replay_claims ADD CONSTRAINT reject_key CHECK(key_hash <> decode('%x','hex'))", digest)
	if _, err := pool.Exec(ctx, query); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(ctx, []string{"first", "blocked"}, time.Now().Add(time.Hour)); ok || !errors.Is(err, ErrReplayStorage) {
		t.Fatal("injected failure accepted")
	}
	if ok, err := s.Claim(ctx, []string{"first"}, time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatal("earlier key remained consumed", err)
	}
}
