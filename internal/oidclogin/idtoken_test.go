package oidclogin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"golang.org/x/oauth2"
)

const testIDToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhZGEifQ.c2lnbmF0dXJl"

func idTokenFixture(t *testing.T) (Sessions, *Provider, string) {
	t.Helper()
	s := dbFixture(t)
	ring, err := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	s.Keys = ring
	p := &Provider{issuer: "https://idp.example", endSession: "https://idp.example/end", oauth: oauth2.Config{ClientID: "client-1"}}
	var principal string
	if err = s.DB.QueryRow(context.Background(), "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','ada') RETURNING id", s.InstanceID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	return s, p, principal
}

func issueOIDC(t *testing.T, s Sessions, issuer, subject, principal, token string) (string, string) {
	t.Helper()
	ctx := context.Background()
	if err := s.BindIdentity(ctx, issuer, subject, principal); err != nil {
		t.Fatal(err)
	}
	_, raw, csrf, err := s.IssueExternal(WithProfile(ctx, Profile{Name: "Ada", IDToken: token}), issuer, subject)
	if err != nil {
		t.Fatal(err)
	}
	return raw, csrf
}

func storedToken(t *testing.T, s Sessions, raw string) string {
	t.Helper()
	var v string
	if err := s.DB.QueryRow(context.Background(), `SELECT COALESCE(idp_id_token,'') FROM browser_sessions WHERE verifier=$1`, hash(raw)).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIDTokenEncryptedAtRestAndUsedForLogout(t *testing.T) {
	s, p, principal := idTokenFixture(t)
	ctx := context.Background()
	raw, csrf := issueOIDC(t, s, p.issuer, "sub-1", principal, testIDToken)

	stored := storedToken(t, s, raw)
	if stored == "" || strings.Contains(stored, testIDToken) || strings.Contains(stored, "eyJ") {
		t.Fatal("ID token must not be stored in plaintext")
	}
	var env secrets.Envelope
	if err := json.Unmarshal([]byte(stored), &env); err != nil || len(env.Ciphertext) == 0 {
		t.Fatal("stored value is not an encrypted envelope", err)
	}
	var plainRows int
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE idp_id_token LIKE '%'||$1||'%'`, testIDToken).Scan(&plainRows)
	if plainRows != 0 {
		t.Fatal("plaintext found in database")
	}
	if got := s.IDTokenHint(ctx, raw); got != testIDToken {
		t.Fatal("hint did not round trip", got)
	}

	c := &Client{Provider: p, Sessions: s, Origin: "https://ant.example"}
	r := httptest.NewRequest("POST", "https://ant.example/auth/logout", nil)
	r.Header.Set("Origin", c.Origin)
	r.Header.Set("X-CSRF-Token", csrf)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
	w := httptest.NewRecorder()
	c.Logout(w, r)
	var body struct {
		URL string `json:"oidc_logout"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal("logout", w.Code, w.Body.String())
	}
	u, _ := url.Parse(body.URL)
	if u.Query().Get("id_token_hint") != testIDToken || u.Query().Get("client_id") != "client-1" {
		t.Fatal("logout URL lacks the hint", body.URL)
	}
	var left int
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE idp_id_token IS NOT NULL`).Scan(&left)
	if left != 0 {
		t.Fatal("token must go with the session")
	}
}

func TestIDTokenTamperedOrWrongSessionRejected(t *testing.T) {
	s, p, principal := idTokenFixture(t)
	ctx := context.Background()
	a, _ := issueOIDC(t, s, p.issuer, "sub-a", principal, testIDToken)
	var other string
	if err := s.DB.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','bob') RETURNING id", s.InstanceID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	b, _ := issueOIDC(t, s, p.issuer, "sub-b", other, "other.token.value")

	// Ciphertext of session A copied onto session B must not open there.
	if _, err := s.DB.Exec(ctx, `UPDATE browser_sessions SET idp_id_token=$2 WHERE verifier=$1`, hash(b), storedToken(t, s, a)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.openIDToken(ctx, b); err == nil || got != "" {
		t.Fatal("another session's ciphertext was accepted")
	}
	if s.IDTokenHint(ctx, a) != testIDToken {
		t.Fatal("original session must still open")
	}

	// Flipped ciphertext byte.
	var env secrets.Envelope
	_ = json.Unmarshal([]byte(storedToken(t, s, a)), &env)
	env.Ciphertext[0] ^= 1
	bad, _ := json.Marshal(env)
	if _, err := s.DB.Exec(ctx, `UPDATE browser_sessions SET idp_id_token=$2 WHERE verifier=$1`, hash(a), string(bad)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.openIDToken(ctx, a); err == nil || got != "" {
		t.Fatal("tampered ciphertext accepted")
	}
	// Garbage, and a plaintext token left behind, are rejected too.
	for _, v := range []string{"not json", testIDToken} {
		_, _ = s.DB.Exec(ctx, `UPDATE browser_sessions SET idp_id_token=$2 WHERE verifier=$1`, hash(a), v)
		if s.IDTokenHint(ctx, a) != "" {
			t.Fatal("non-envelope value accepted", v)
		}
	}
	// A different instance keyring cannot open it either.
	wrong, _ := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)})
	s2 := s
	s2.Keys = wrong
	c, _ := issueOIDC2(t, s, p.issuer)
	if s2.IDTokenHint(ctx, c) != "" {
		t.Fatal("wrong key opened the token")
	}
}

func issueOIDC2(t *testing.T, s Sessions, issuer string) (string, string) {
	t.Helper()
	var principal string
	if err := s.DB.QueryRow(context.Background(), "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','carl') RETURNING id", s.InstanceID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	return issueOIDC(t, s, issuer, "sub-c", principal, testIDToken)
}

func TestIDTokenNotStoredWithoutKeyringOrToken(t *testing.T) {
	s, p, principal := idTokenFixture(t)
	s.Keys = nil
	raw, _ := issueOIDC(t, s, p.issuer, "sub-1", principal, testIDToken)
	if storedToken(t, s, raw) != "" {
		t.Fatal("stored without a keyring")
	}
	// Non-OIDC logins (no token in the profile) store nothing either.
	s.Keys, _ = secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{9}, 32)})
	_, raw2, _, err := s.IssueExternal(context.Background(), p.issuer, "sub-1")
	if err != nil || storedToken(t, s, raw2) != "" {
		t.Fatal("token stored for a login without one", err)
	}
}
