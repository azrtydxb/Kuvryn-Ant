package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tokenFixture struct {
	pool                                               *pgxpool.Pool
	instance, other, actor, service, feed, foreignFeed string
}

func newTokenFixture(t *testing.T) tokenFixture {
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
	schema := fmt.Sprintf("ant_tokens_%d", time.Now().UnixNano())
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
	f := tokenFixture{pool: pool}
	insert := func(dest *string, query string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, query, args...).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	insert(&f.instance, "INSERT INTO instances(name) VALUES('tokens') RETURNING id")
	insert(&f.other, "INSERT INTO instances(name) VALUES('other') RETURNING id")
	insert(&f.actor, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','actor') RETURNING id", f.instance)
	insert(&f.service, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','machine') RETURNING id", f.instance)
	insert(&f.feed, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'feed','raw') RETURNING id", f.instance)
	insert(&f.foreignFeed, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'feed','raw') RETURNING id", f.other)
	return f
}
func (f tokenFixture) issue(t *testing.T) IssuedToken {
	t.Helper()
	issued, err := (Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.actor, f.service, []Scope{{FeedID: f.feed, Namespace: "team", Actions: []Action{Read, Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return issued
}
func (f tokenFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTokenIssuanceVerifierAndLifecycle(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	tokens := Tokens{DB: f.pool}
	issued := f.issue(t)
	p, err := tokens.Authenticate(ctx, issued.Secret)
	if err != nil || p.InstanceID != f.instance || p.PrincipalID != f.service || p.TokenID != issued.ID || len(p.Scopes) != 1 || p.Scopes[0].FeedID != f.feed {
		t.Fatalf("authentication failed: %+v %v", p, err)
	}
	var verifier []byte
	var row, audit, outbox string
	if err = f.pool.QueryRow(ctx, "SELECT verifier,row_to_json(t)::text FROM service_tokens t WHERE id=$1", issued.ID).Scan(&verifier, &row); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(issued.Secret))
	if !bytes.Equal(verifier, want[:]) {
		t.Fatal("stored credential is not the SHA-256 verifier")
	}
	if err = f.pool.QueryRow(ctx, "SELECT row_to_json(a)::text FROM audit_events a WHERE action='token.issue'").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(ctx, "SELECT row_to_json(o)::text FROM outbox_events o WHERE topic='token.issue'").Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{row, audit, outbox} {
		if strings.Contains(value, issued.Secret) || strings.Contains(value, issued.Secret[4:]) {
			t.Fatal("plaintext credential persisted")
		}
	}
	if strings.Contains(audit, hex.EncodeToString(verifier)) || strings.Contains(outbox, hex.EncodeToString(verifier)) {
		t.Fatal("credential verifier leaked to audit/outbox")
	}
	for _, raw := range []string{"", "bad", "ant_" + strings.Repeat("!", 43), "ant_" + strings.Repeat("A", 43)} {
		if _, err = tokens.Authenticate(ctx, raw); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("invalid token accepted: %v", err)
		}
	}
	if _, err = f.pool.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", f.service); err != nil {
		t.Fatal(err)
	}
	if _, err = tokens.Authenticate(ctx, issued.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled principal accepted")
	}
	if _, err = f.pool.Exec(ctx, "UPDATE principals SET disabled=false WHERE id=$1", f.service); err != nil {
		t.Fatal(err)
	}
	if err = tokens.Revoke(ctx, f.other, f.actor, issued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tokens.Authenticate(ctx, issued.Secret); err != nil {
		t.Fatal("cross-instance revoke changed token")
	}
	if err = tokens.Revoke(ctx, f.instance, f.actor, issued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tokens.Authenticate(ctx, issued.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked token accepted")
	}
	if err = tokens.Revoke(ctx, f.instance, f.actor, issued.ID); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "audit_events") != 2 || f.count(t, "outbox_events") != 2 {
		t.Fatal("revocation was not recorded exactly once")
	}
	expired := f.issue(t)
	if _, err = f.pool.Exec(ctx, "UPDATE service_tokens SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1", expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tokens.Authenticate(ctx, expired.Secret); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("expired token accepted")
	}
}

func TestTokenScopeOwnershipAndIssuanceValidation(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	tokens := Tokens{DB: f.pool}
	for _, scopes := range [][]Scope{nil, {{FeedID: f.foreignFeed, Actions: []Action{Read}}}, {{FeedID: "invalid", Actions: []Action{Read}}}, {{Actions: []Action{Read, Read}}}, {{Actions: []Action{"unknown"}}}, {{Actions: nil}}} {
		issued, err := tokens.Issue(ctx, f.instance, f.actor, f.service, scopes, time.Now().Add(time.Hour))
		if !errors.Is(err, ErrInvalidScope) || issued.Secret != "" {
			t.Fatalf("invalid scope issuance: %v", err)
		}
	}
	for _, expiry := range []time.Time{time.Now().Add(-time.Hour), time.Now().Add(366 * 24 * time.Hour)} {
		if issued, err := tokens.Issue(ctx, f.instance, f.actor, f.service, []Scope{{Actions: []Action{Read}}}, expiry); err == nil || issued.Secret != "" {
			t.Fatal("invalid expiry accepted")
		}
	}
	if _, err := tokens.Issue(ctx, f.instance, f.actor, f.actor, []Scope{{Actions: []Action{Read}}}, time.Now().Add(time.Hour)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("user principal received machine credential")
	}
	if _, err := f.pool.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", f.service); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Issue(ctx, f.instance, f.actor, f.service, []Scope{{Actions: []Action{Read}}}, time.Now().Add(time.Hour)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled service received credential")
	}
	if f.count(t, "service_tokens") != 0 || f.count(t, "audit_events") != 0 || f.count(t, "outbox_events") != 0 {
		t.Fatal("failed issuance left records")
	}
}

func TestTokenAuditOutboxAtomicity(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	tokens := Tokens{DB: f.pool}
	if _, err := f.pool.Exec(ctx, "ALTER TABLE outbox_events ADD CONSTRAINT fail_issue CHECK(topic<>'token.issue')"); err != nil {
		t.Fatal(err)
	}
	issued, err := tokens.Issue(ctx, f.instance, f.actor, f.service, []Scope{{Actions: []Action{Read}}}, time.Now().Add(time.Hour))
	if err == nil || issued.Secret != "" {
		t.Fatal("issuance survived outbox failure")
	}
	if f.count(t, "service_tokens") != 0 || f.count(t, "audit_events") != 0 {
		t.Fatal("issuance was not atomic")
	}
	if _, err = f.pool.Exec(ctx, "ALTER TABLE outbox_events DROP CONSTRAINT fail_issue"); err != nil {
		t.Fatal(err)
	}
	issued = f.issue(t)
	if _, err = f.pool.Exec(ctx, "ALTER TABLE outbox_events ADD CONSTRAINT fail_revoke CHECK(topic<>'token.revoke')"); err != nil {
		t.Fatal(err)
	}
	if err = tokens.Revoke(ctx, f.instance, f.actor, issued.ID); err == nil {
		t.Fatal("revocation survived outbox failure")
	}
	if _, err = tokens.Authenticate(ctx, issued.Secret); err != nil {
		t.Fatal("failed revoke invalidated credential")
	}
	if f.count(t, "audit_events") != 1 || f.count(t, "outbox_events") != 1 {
		t.Fatal("failed revocation left partial event")
	}
}

func TestTokenFeedScopeCanonicalization(t *testing.T) {
	f := newTokenFixture(t)
	tokens := Tokens{DB: f.pool}
	issued, err := tokens.Issue(context.Background(), f.instance, f.actor, f.service, []Scope{{FeedID: strings.ToUpper(f.feed), Actions: []Action{Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := tokens.Authenticate(context.Background(), issued.Secret)
	if err != nil || len(principal.Scopes) != 1 || principal.Scopes[0].FeedID != f.feed {
		t.Fatalf("feed scope not canonical: %+v %v", principal.Scopes, err)
	}
}
