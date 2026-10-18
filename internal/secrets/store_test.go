package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type secretFixture struct {
	pool                   *pgxpool.Pool
	instance, actor, other string
	old, new               *Keyring
}

func newSecretFixture(t *testing.T) secretFixture {
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
	schema := fmt.Sprintf("ant_secrets_%d", time.Now().UnixNano())
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
	f := secretFixture{pool: pool}
	for _, dest := range []*string{&f.instance, &f.other} {
		if err = pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('secrets') RETURNING id").Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err = pool.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','operator') RETURNING id", f.instance).Scan(&f.actor); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	f.old = ring(t, "old", map[string][]byte{"old": oldKey})
	f.new = ring(t, "new", map[string][]byte{"old": oldKey, "new": newKey})
	return f
}
func (f secretFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSecretStoreEncryptedCRUDAndScope(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	s := Store{DB: f.pool, Keys: f.old}
	plain := []byte("sensitive-upstream-password")
	record, err := s.Create(ctx, f.instance, f.actor, "upstream-password", plain)
	if err != nil {
		t.Fatal(err)
	}
	got, actual, err := s.Open(ctx, strings.ToUpper(f.instance), strings.ToUpper(record.ID), record.Purpose)
	if err != nil || got.Revision != 1 || !bytes.Equal(actual, plain) {
		t.Fatal("stored secret unreadable", err)
	}
	clear(actual)
	for _, scope := range [][2]string{{f.other, record.Purpose}, {f.instance, "wrong-purpose"}} {
		if _, _, err = s.Open(ctx, scope[0], record.ID, scope[1]); !errors.Is(err, ErrNotFound) {
			t.Fatal("secret scope escaped", err)
		}
	}
	var stored, audit, event string
	if err = f.pool.QueryRow(ctx, "SELECT row_to_json(s)::text FROM encrypted_secrets s WHERE id=$1", record.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, "SELECT row_to_json(a)::text FROM audit_events a WHERE resource_id=$1", record.ID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, "SELECT row_to_json(o)::text FROM outbox_events o").Scan(&event); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{stored, audit, event} {
		if strings.Contains(text, string(plain)) {
			t.Fatal("plaintext persisted")
		}
	}
	for _, text := range []string{audit, event} {
		if strings.Contains(text, "ciphertext") || strings.Contains(text, "wrapped_key") {
			t.Fatal("encrypted secret leaked into event")
		}
	}
	updated, err := s.Update(ctx, f.instance, f.actor, record.ID, record.Purpose, 1, []byte("replacement"))
	if err != nil || updated.Revision != 2 {
		t.Fatal("update failed", err)
	}
	if _, err = s.Update(ctx, f.instance, f.actor, record.ID, record.Purpose, 1, []byte("stale")); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("stale update accepted", err)
	}
	_, actual, err = s.Open(ctx, f.instance, record.ID, record.Purpose)
	if err != nil || string(actual) != "replacement" {
		t.Fatal("stale update changed data", err)
	}
	clear(actual)
	if f.count(t, "SELECT count(*) FROM mutation_results") != 0 || f.count(t, "SELECT count(*) FROM audit_events") != 2 || f.count(t, "SELECT count(*) FROM outbox_events") != 2 {
		t.Fatal("incorrect secret event/cache storage")
	}
}

func TestSecretStoreCASAndAuditRollback(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	s := Store{DB: f.pool, Keys: f.old}
	record, err := s.Create(ctx, f.instance, f.actor, "password", []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Update(ctx, f.instance, f.actor, record.ID, record.Purpose, 1, []byte("winner"))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("CAS did not select one winner")
	}
	if _, err = f.pool.Exec(ctx, "ALTER TABLE outbox_events ADD CONSTRAINT fail_secrets CHECK(topic NOT IN ('secret.update','secret.create')) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Update(ctx, f.instance, f.actor, record.ID, record.Purpose, 2, []byte("failed")); !errors.Is(err, ErrStorage) {
		t.Fatal("update survived event failure")
	}
	if _, err = s.Create(ctx, f.instance, f.actor, "other", []byte("failed")); !errors.Is(err, ErrStorage) {
		t.Fatal("create survived event failure")
	}
	got, actual, err := s.Open(ctx, f.instance, record.ID, record.Purpose)
	if err != nil || got.Revision != 2 || string(actual) != "winner" {
		t.Fatal("failed transaction changed secret", err)
	}
	clear(actual)
	if f.count(t, "SELECT count(*) FROM encrypted_secrets") != 1 || f.count(t, "SELECT count(*) FROM audit_events") != 2 {
		t.Fatal("partial secret transaction")
	}
}

func TestSecretRotationResumesSkipsLocksAndRetiresKey(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	old := Store{DB: f.pool, Keys: f.old}
	rotating := Store{DB: f.pool, Keys: f.new}
	var records []Record
	for range 3 {
		r, err := old.Create(ctx, f.instance, f.actor, "password", []byte("secret"))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	lock, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err = lock.Exec(ctx, "SELECT id FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 FOR UPDATE", f.instance, records[0].ID); err != nil {
		t.Fatal(err)
	}
	batch, err := rotating.RotateBatch(ctx, f.instance, f.actor, 100)
	if err != nil || batch.Rotated != 2 {
		t.Fatalf("skip locked: %+v %v", batch, err)
	}
	if err = lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// New Store value models restart: persisted key versions are the work queue.
	batch, err = (Store{DB: f.pool, Keys: f.new}).RotateBatch(ctx, f.instance, f.actor, 1)
	if err != nil || batch.Rotated != 1 {
		t.Fatalf("resume: %+v %v", batch, err)
	}
	batch, err = rotating.RotateBatch(ctx, f.instance, f.actor, 100)
	if err != nil || batch.Rotated != 0 {
		t.Fatalf("completed queue: %+v %v", batch, err)
	}
	retired := Store{DB: f.pool, Keys: ring(t, "new", map[string][]byte{"new": bytes.Repeat([]byte{2}, 32)})}
	for _, record := range records {
		got, plain, err := retired.Open(ctx, f.instance, record.ID, record.Purpose)
		if err != nil || got.Revision != 2 || string(plain) != "secret" {
			t.Fatal("rotation failed after old key retirement", err)
		}
		clear(plain)
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action='secret.rewrap'") != 3 {
		t.Fatal("rotation audit count mismatch")
	}
	for _, limit := range []int{0, 101} {
		if _, err = rotating.RotateBatch(ctx, f.instance, f.actor, limit); !errors.Is(err, ErrStoreInput) {
			t.Fatal("unbounded batch accepted")
		}
	}
}

func TestSecretRotationFailsClosedAndRollsBackBatch(t *testing.T) {
	f := newSecretFixture(t)
	ctx := context.Background()
	old := Store{DB: f.pool, Keys: f.old}
	rotating := Store{DB: f.pool, Keys: f.new}
	for range 2 {
		if _, err := old.Create(ctx, f.instance, f.actor, "password", []byte("secret")); err != nil {
			t.Fatal(err)
		}
	}
	missing := Store{DB: f.pool, Keys: ring(t, "new", map[string][]byte{"new": bytes.Repeat([]byte{2}, 32)})}
	if _, err := missing.RotateBatch(ctx, f.instance, f.actor, 100); !errors.Is(err, ErrUnknownVersion) {
		t.Fatal("rotation accepted missing old key", err)
	}
	// Corrupt the second row so the first is rewritten before failure; rollback
	// must undo that already-executed update and its audit/outbox insertion.
	if _, err := f.pool.Exec(ctx, `UPDATE encrypted_secrets SET envelope=jsonb_set(envelope,'{ciphertext}','"AAAA"'::jsonb) WHERE id=(SELECT id FROM encrypted_secrets ORDER BY id DESC LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rotating.RotateBatch(ctx, f.instance, f.actor, 100); err == nil {
		t.Fatal("tampered batch rotated")
	}
	if f.count(t, "SELECT count(*) FROM encrypted_secrets WHERE key_version='old' AND revision=1") != 2 || f.count(t, "SELECT count(*) FROM audit_events WHERE action='secret.rewrap'") != 0 {
		t.Fatal("failed batch partially committed")
	}
}

func TestStoredEnvelopeBoundAndVersionValidation(t *testing.T) {
	keyring := ring(t, "old", map[string][]byte{"old": bytes.Repeat([]byte{1}, 32)})
	envelope, err := keyring.Seal(Context{RecordID: "record", Purpose: "test"}, bytes.Repeat([]byte("x"), maxSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeStoredEnvelope(encoded, "old"); err != nil {
		t.Fatal("maximum valid secret envelope rejected", err)
	}
	for _, test := range []struct {
		body    []byte
		version string
	}{
		{nil, "old"},
		{[]byte(`{"key_version":null}`), "old"},
		{[]byte(`{"key_version":"old"}`), "new"},
		{[]byte(`{"key_version":""}`), ""},
		{[]byte(`{"key_version":"old","padding":"` + strings.Repeat("x", maxStoredEnvelopeBytes) + `"}`), "old"},
	} {
		if _, err := decodeStoredEnvelope(test.body, test.version); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatal("invalid persisted envelope accepted", err)
		}
	}
}

func TestSecretSchemaRejectsNullVersionAndOversizedEnvelope(t *testing.T) {
	f := newSecretFixture(t)
	for _, encoded := range []string{
		`{"key_version":null}`,
		`{}`,
		`{"key_version":123}`,
		`{"key_version":"new"}`,
		`{"key_version":"old","padding":"` + strings.Repeat("x", maxStoredEnvelopeBytes) + `"}`,
	} {
		_, err := f.pool.Exec(context.Background(), "INSERT INTO encrypted_secrets(instance_id,purpose,envelope,key_version) VALUES($1,'test',$2,'old')", f.instance, encoded)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
			t.Fatalf("invalid envelope bypassed schema check: %v", err)
		}
	}
	if f.count(t, "SELECT count(*) FROM encrypted_secrets") != 0 {
		t.Fatal("invalid envelope persisted")
	}
}
