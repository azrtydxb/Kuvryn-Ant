package events

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type resolverFunc func(context.Context, string, string) ([]byte, error)

func (f resolverFunc) Resolve(ctx context.Context, instance, ref string) ([]byte, error) {
	return f(ctx, instance, ref)
}
func signingKeys() SecretResolver {
	return resolverFunc(func(context.Context, string, string) ([]byte, error) { return []byte(strings.Repeat("k", 32)), nil })
}
func TestSafeEndpointPolicyAndSignature(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "169.254.169.254", "10.1.2.3", "100.64.0.1", "192.0.2.1", "2001:db8::1", "fe80::1", "fc00::1"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Fatalf("unsafe public address: %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(ip)) {
			t.Fatalf("public address denied %s", ip)
		}
	}
	for _, url := range []string{"http://example.com", "https://user:password@example.com", "https://example.com?token=secret", "https://example.com/#secret"} {
		if endpointAllowed(url, Options{}) {
			t.Fatalf("unsafe endpoint accepted %s", url)
		}
	}
	var received atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
		mac.Write([]byte(r.Header.Get("X-Ant-Timestamp") + "\n" + r.Header.Get("X-Ant-Event-ID") + "\n"))
		mac.Write(body)
		if r.Header.Get("X-Ant-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("X-Ant-Event-ID") != "42" {
			t.Error("invalid signature")
		}
		w.WriteHeader(204)
	}))
	defer endpoint.Close()
	client, err := safeClient(nil, Options{AllowPrivate: true, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	d := Dispatcher{client: client, secrets: signingKeys(), options: Options{AllowPrivate: true, AllowHTTP: true}}
	item := delivery{endpoint: endpoint.URL, eventID: 42, instance: "instance", topic: "feed.create", payload: []byte(`{"value":"safe"}`), created: time.Now()}
	if failure := d.send(context.Background(), item); failure != "" {
		t.Fatal(failure)
	}
	denied, err := safeClient(nil, Options{AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer denied.CloseIdleConnections()
	d.client = denied
	d.options = Options{AllowHTTP: true}
	if failure := d.send(context.Background(), item); failure != "transport_failure" {
		t.Fatalf("private destination not denied: %s", failure)
	}
	if received.Load() != 1 {
		t.Fatal("private endpoint reached under default policy")
	}
}
func TestRedirectAndSecretErrorAreSanitized(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	client, err := safeClient(nil, Options{AllowPrivate: true, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	d := Dispatcher{client: client, secrets: signingKeys(), options: Options{AllowPrivate: true, AllowHTTP: true}}
	item := delivery{endpoint: redirect.URL, eventID: 1, payload: []byte(`{}`)}
	if failure := d.send(context.Background(), item); failure != "http_failure" {
		t.Fatal(failure)
	}
	if forwarded.Load() != 0 {
		t.Fatal("redirect forwarded signed credentials")
	}
	d.secrets = resolverFunc(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("private-secret-provider-detail")
	})
	if failure := d.send(context.Background(), item); failure != "secret_unavailable" {
		t.Fatal("secret error leaked", failure)
	}
	for _, attempt := range []int{1, 8, 99} {
		if delay := backoff(attempt); delay < time.Second || delay > 5*time.Minute {
			t.Fatal("unbounded retry delay")
		}
	}
}
func fixture(t *testing.T) *pgxpool.Pool {
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
	schema := "ant_events_" + strings.ToLower(rand.Text())
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
	return pool
}
func instance(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO instances(name) VALUES('events') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func event(t *testing.T, tx pgx.Tx, instance string) int64 {
	t.Helper()
	var audit, id int64
	ctx := context.Background()
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,action) VALUES($1,'feed.create') RETURNING id`, instance).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'feed.create','{"feed_id":"value"}') RETURNING id`, audit).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestDurableDeliveryRetriesClaimsAndLateCommit(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	owner := instance(t, pool)
	other := instance(t, pool)
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer endpoint.Close()
	d, err := New(pool, nil, signingKeys(), Options{AllowHTTP: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	sub, err := d.Subscribe(ctx, owner, endpoint.URL, "owned-secret")
	if err != nil {
		t.Fatal(err)
	}
	early, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(early)
	first := event(t, early, owner)
	late, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	event(t, late, owner)
	event(t, late, other)
	if err = late.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Materialize(ctx, sub, 100); err != nil || n != 1 {
		t.Fatalf("discovery %d %v", n, err)
	}
	if err = early.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Materialize(ctx, sub, 100); err != nil || n != 1 {
		t.Fatalf("lost late committing lower ID %d %v", n, err)
	}
	if n, err := d.Materialize(ctx, sub, 100); err != nil || n != 0 {
		t.Fatalf("duplicate discovery %d %v", n, err)
	}
	if worked, err := d.DeliverOne(ctx); !worked || err != nil {
		t.Fatalf("attempt %v %v", worked, err)
	}
	var attempts int
	var state, failure string
	if err = pool.QueryRow(ctx, `SELECT attempts,state,last_failure FROM event_deliveries WHERE subscription_id=$1 AND event_id=$2`, sub, first).Scan(&attempts, &state, &failure); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || state != "pending" || failure != "http_failure" {
		t.Fatalf("failure persistence %d %s %s", attempts, state, failure)
	}
	fail.Store(false)
	if _, err = pool.Exec(ctx, `UPDATE event_deliveries SET available_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := d.DeliverOne(ctx); errorsCh <- err }()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("concurrent delivery duplicates: %d", calls.Load())
	}
	var delivered int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM event_deliveries WHERE state='delivered'`).Scan(&delivered); err != nil || delivered != 2 {
		t.Fatalf("delivery count %d %v", delivered, err)
	}
	// Simulate an interrupted claim: an expired lease is claimed again, while the
	// previous fencing token cannot acknowledge the new owner's work.
	if _, err = pool.Exec(ctx, `UPDATE event_deliveries SET state='pending',attempts=0,available_at=clock_timestamp(),lease_token=NULL,lease_until=NULL WHERE event_id=$1`, first); err != nil {
		t.Fatal(err)
	}
	old, ok, err := d.claim(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err = d.claim(ctx); err != nil || ok {
		t.Fatal("live lease reclaimed", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE event_deliveries SET lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1`, first); err != nil {
		t.Fatal(err)
	}
	replacement, ok, err := d.claim(ctx)
	if err != nil || !ok || replacement.token == old.token || replacement.attempt != 2 {
		t.Fatal("crash reclaim/fence failed", err)
	}
	tag, err := pool.Exec(ctx, `UPDATE event_deliveries SET state='delivered' WHERE subscription_id=$1 AND event_id=$2 AND lease_token=$3`, sub, first, old.token)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatal("stale owner acknowledged delivery", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE event_deliveries SET attempts=$2,lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1`, first, maxAttempts); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = d.claim(ctx); err != nil || ok {
		t.Fatal("exhausted attempt reclaimed", err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM event_deliveries WHERE event_id=$1`, first).Scan(&state); err != nil || state != "dead" {
		t.Fatal("exhausted claim not terminal", state, err)
	}
}

// A webhook with a signing secret signs with that secret's current value and,
// inside the rotation grace window, also with the previous one.
func TestDeliverySignsWithReferencedSecretAndGraceWindow(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	owner := instance(t, pool)
	values := map[string]string{"cur": strings.Repeat("c", 32), "prev": strings.Repeat("p", 32), "legacy": strings.Repeat("l", 32)}
	refs := map[string]string{}
	for label := range values {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO encrypted_secrets(instance_id,purpose,envelope,key_version) VALUES($1,'webhook','{"key_version":"v1"}','v1') RETURNING id::text`, owner).Scan(&id); err != nil {
			t.Fatal(err)
		}
		refs[label] = id
	}
	resolver := resolverFunc(func(_ context.Context, _, ref string) ([]byte, error) {
		for label, id := range refs {
			if id == ref {
				return []byte(values[label]), nil
			}
		}
		return nil, errors.New("unknown")
	})
	sign := func(secret, ts, id string, body []byte) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "\n" + id + "\n"))
		mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	var header atomic.Value
	var bodies atomic.Value
	var ts, eventID atomic.Value
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		header.Store(r.Header.Get("X-Ant-Signature"))
		bodies.Store(b)
		ts.Store(r.Header.Get("X-Ant-Timestamp"))
		eventID.Store(r.Header.Get("X-Ant-Event-ID"))
		w.WriteHeader(204)
	}))
	defer endpoint.Close()
	d, err := New(pool, nil, resolver, Options{AllowHTTP: true, AllowPrivate: true, InstanceID: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var signing string
	if err = pool.QueryRow(ctx, `INSERT INTO webhook_signing_secrets(instance_id,name,secret_id) VALUES($1,'shared',$2) RETURNING id::text`, owner, refs["cur"]).Scan(&signing); err != nil {
		t.Fatal(err)
	}
	var sub string
	if err = pool.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,name,endpoint,signing_secret_id) VALUES($1,'hook',$2,$3) RETURNING id::text`, owner, endpoint.URL, signing).Scan(&sub); err != nil {
		t.Fatal(err)
	}
	deliver := func() string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		event(t, tx, owner)
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = d.Materialize(ctx, sub, 200); err != nil {
			t.Fatal(err)
		}
		if attempted, err := d.DeliverOne(ctx); err != nil || !attempted {
			t.Fatal("not delivered", err)
		}
		return header.Load().(string)
	}
	got := deliver()
	want := func(secrets ...string) string {
		var parts []string
		for _, s := range secrets {
			parts = append(parts, sign(s, ts.Load().(string), eventID.Load().(string), bodies.Load().([]byte)))
		}
		return strings.Join(parts, ",")
	}
	if got != want(values["cur"]) {
		t.Fatal("not signed with the referenced secret alone")
	}
	// Rotate with a grace window: new current, previous still signs.
	if _, err = pool.Exec(ctx, `UPDATE webhook_signing_secrets SET secret_id=$2,previous_secret_id=$3,previous_purpose='webhook',previous_expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, signing, refs["legacy"], refs["prev"]); err != nil {
		t.Fatal(err)
	}
	got = deliver()
	if got != want(values["legacy"], values["prev"]) {
		t.Fatalf("grace window not double-signed: %q", got)
	}
	// After the window only the current value signs.
	if _, err = pool.Exec(ctx, `UPDATE webhook_signing_secrets SET previous_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, signing); err != nil {
		t.Fatal(err)
	}
	got = deliver()
	if got != want(values["legacy"]) {
		t.Fatalf("expired previous value still signs: %q", got)
	}
}
