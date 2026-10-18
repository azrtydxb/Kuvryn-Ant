package oidclogin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dbFixture(t *testing.T) Sessions {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL required")
		}
		t.Skip("set ANT_TEST_DATABASE_URL for PostgreSQL")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := pgx.Identifier{fmt.Sprintf("oidc_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	if e = database.Migrate(ctx, pool); e != nil {
		t.Fatal(e)
	}
	s := Sessions{DB: pool}
	if e = pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('oidc-test') RETURNING id").Scan(&s.InstanceID); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestPostgresStateSessionAndHTTPFlow(t *testing.T) {
	s := dbFixture(t)
	ctx := context.Background()
	f, p := fixtureProvider(t)
	var principal string
	if e := s.DB.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','internal') RETURNING id", s.InstanceID).Scan(&principal); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e := s.issue(ctx, p.issuer, "immutable-user"); e == nil {
		t.Fatal("unmapped login accepted")
	}
	if e := s.BindIdentity(ctx, p.issuer, "immutable-user", principal); e != nil {
		t.Fatal(e)
	}
	state, browser, nonce := secret(), secret(), secret()
	if e := s.begin(ctx, p, state, browser, nonce); e != nil {
		t.Fatal(e)
	}
	if _, e := s.consume(ctx, p, state, secret()); e == nil {
		t.Fatal("wrong browser accepted")
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, e := s.consume(ctx, p, state, browser); e == nil {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("state consumed %d times", winners.Load())
	}
	expiredState := secret()
	if e := s.begin(ctx, p, expiredState, browser, nonce); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "UPDATE oidc_login_states SET expires_at=clock_timestamp()-interval '1 second' WHERE state_hash=$1", hash(expiredState)); e != nil {
		t.Fatal(e)
	}
	if _, e := s.consume(ctx, p, expiredState, browser); e == nil {
		t.Fatal("expired state accepted")
	}
	c := Client{Provider: p, Sessions: s, Origin: "https://console.example"}
	w := httptest.NewRecorder()
	c.Login(w, httptest.NewRequest("GET", "https://console.example/login", nil))
	if w.Code != 302 {
		t.Fatal("login", w.Code, w.Body.String())
	}
	auth, _ := url.Parse(w.Header().Get("Location"))
	login := w.Result().Cookies()[0]
	f.mu.Lock()
	f.verifier = login.Value
	f.claims["nonce"] = auth.Query().Get("nonce")
	f.mu.Unlock()
	callback := httptest.NewRequest("GET", "https://console.example/callback?code=fixture&state="+auth.Query().Get("state"), nil)
	callback.AddCookie(login)
	wrongTarget := callback.Clone(ctx)
	wrongTarget.Host = "attacker.example"
	rejected := httptest.NewRecorder()
	c.Callback(rejected, wrongTarget)
	if rejected.Code != 401 {
		t.Fatal("wrong callback host accepted")
	}
	w = httptest.NewRecorder()
	c.Callback(w, callback)
	if w.Code != 200 {
		t.Fatal("callback", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	raw, csrf := "", ""
	for _, cookie := range cookies {
		if cookie.Name == SessionCookie {
			raw = cookie.Value
		}
		if cookie.Name == CSRFCookie {
			csrf = cookie.Value
		}
	}
	w = httptest.NewRecorder()
	c.Callback(w, callback)
	if w.Code != 401 {
		t.Fatal("callback replay accepted")
	}
	if _, e := s.Authenticate(ctx, raw, secret(), true); e == nil {
		t.Fatal("wrong CSRF accepted")
	}
	if session, e := s.Authenticate(ctx, raw, csrf, true); e != nil || session.PrincipalID != principal {
		t.Fatal("session", session, e)
	}
	var stored []byte
	if e := s.DB.QueryRow(ctx, "SELECT verifier FROM browser_sessions WHERE instance_id=$1", s.InstanceID).Scan(&stored); e != nil || string(stored) == raw || !matches(raw, stored) {
		t.Fatal("session verifier not hashed")
	}
	logout := httptest.NewRequest("POST", "https://console.example/logout", nil)
	logout.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
	logout.Header.Set("Origin", c.Origin)
	logout.Header.Set("X-CSRF-Token", csrf)
	w = httptest.NewRecorder()
	c.Logout(w, logout)
	if w.Code != 204 {
		t.Fatal("logout", w.Code)
	}
	if _, e := s.Authenticate(ctx, raw, "", false); e == nil {
		t.Fatal("revoked session accepted")
	}
	for _, tc := range []struct{ name, sql string }{{"idle", "UPDATE browser_sessions SET last_seen_at=clock_timestamp()-interval '31 minutes'"}, {"expiry", "UPDATE browser_sessions SET created_at=statement_timestamp()-interval '9 hours',expires_at=statement_timestamp()-interval '1 hour'"}, {"disabled", "UPDATE principals SET disabled=true"}, {"unbound", "DELETE FROM oidc_identity_bindings"}} {
		t.Run(tc.name, func(t *testing.T) {
			_, token, _, e := s.issue(ctx, p.issuer, "immutable-user")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.DB.Exec(ctx, tc.sql); e != nil {
				t.Fatal(e)
			}
			if _, e = s.Authenticate(ctx, token, "", false); e == nil {
				t.Fatal("inactive session accepted")
			}
			s.DB.Exec(ctx, "UPDATE principals SET disabled=false")
		})
	}
}
