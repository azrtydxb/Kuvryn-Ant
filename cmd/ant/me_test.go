package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/jackc/pgx/v5/pgxpool"
)

const meOrigin = "https://ant.example"

type meEnv struct {
	t        *testing.T
	pool     *pgxpool.Pool
	instance string
	rt       *browserRuntime
	local    *localadmin.Service
	sessions oidclogin.Sessions
	mux      *http.ServeMux
}

type meSession struct{ raw, csrf string }

func newMeEnv(t *testing.T) *meEnv {
	pool, _ := runtimeFixture(t)
	owner, err := identity.Bootstrap(context.Background(), pool, "Me")
	if err != nil {
		t.Fatal(err)
	}
	sessions := oidclogin.Sessions{DB: pool, InstanceID: owner.InstanceID}
	local := &localadmin.Service{DB: pool, InstanceID: owner.InstanceID}
	rt := &browserRuntime{client: &oidclogin.Client{Sessions: sessions, Origin: meOrigin}}
	rt.dyn = &dynamicProviders{pool: pool, instance: owner.InstanceID, local: local}
	mux := http.NewServeMux()
	registerMe(mux, pool, rt)
	return &meEnv{t: t, pool: pool, instance: owner.InstanceID, rt: rt, local: local, sessions: sessions, mux: mux}
}

// external binds an IdP identity to a fresh user principal and signs it in.
func (e *meEnv) external(issuer, subject string, p oidclogin.Profile) (meSession, string) {
	e.t.Helper()
	ctx := context.Background()
	var principal string
	if err := e.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user',$2) RETURNING id::text`, e.instance, subject).Scan(&principal); err != nil {
		e.t.Fatal(err)
	}
	if err := e.sessions.BindIdentity(ctx, issuer, subject, principal); err != nil {
		e.t.Fatal(err)
	}
	_, raw, csrf, err := e.sessions.IssueExternal(oidclogin.WithProfile(ctx, p), issuer, subject)
	if err != nil {
		e.t.Fatal(err)
	}
	return meSession{raw, csrf}, principal
}

func (e *meEnv) provider(kind, name, config string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO identity_providers(instance_id,kind,name,enabled,config) VALUES($1,$2,$3,true,$4::jsonb)`, e.instance, kind, name, config); err != nil {
		e.t.Fatal(err)
	}
}

func (e *meEnv) do(s meSession, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, meOrigin+path, strings.NewReader(body))
	r.Header.Set("Origin", meOrigin)
	r.Header.Set("X-CSRF-Token", s.csrf)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: oidclogin.SessionCookie, Value: s.raw})
	w := httptest.NewRecorder()
	e.rt.Middleware(e.mux).ServeHTTP(w, r)
	return w
}

type whoami struct {
	AuthMethod, DisplayName, Subject, Email, Picture, IdP string
	Roles                                                 []struct{ Name string }
	PasswordChange                                        struct{ Mode, URL, Reason string } `json:"password_change"`
	ActiveSessions                                        int                                `json:"active_sessions"`
}

func (e *meEnv) whoami(s meSession) whoami {
	e.t.Helper()
	w := e.do(s, "GET", "/api/v1/me", "")
	if w.Code != 200 {
		e.t.Fatal("whoami", w.Code, w.Body.String())
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	var out whoami
	out.AuthMethod, _ = raw["auth_method"].(string)
	out.DisplayName, _ = raw["display_name"].(string)
	out.Subject, _ = raw["subject"].(string)
	out.Email, _ = raw["email"].(string)
	out.Picture, _ = raw["picture"].(string)
	out.IdP, _ = raw["idp"].(string)
	pc, _ := raw["password_change"].(map[string]any)
	out.PasswordChange.Mode, _ = pc["mode"].(string)
	out.PasswordChange.URL, _ = pc["url"].(string)
	out.PasswordChange.Reason, _ = pc["reason"].(string)
	n, _ := raw["active_sessions"].(float64)
	out.ActiveSessions = int(n)
	return out
}

func TestMeLocalPasswordChange(t *testing.T) {
	e := newMeEnv(t)
	ctx := context.Background()
	const pw, next = "another long enough password", "a brand new long password"
	u, err := e.local.CreateUser(ctx, "", "alice", pw, false)
	if err != nil {
		t.Fatal(err)
	}
	_, a, _, err := e.sessions.IssueExternal(ctx, localadmin.Issuer, "alice")
	_, b, bcsrf, err2 := e.sessions.IssueExternal(ctx, localadmin.Issuer, "alice")
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	_ = a
	s := meSession{b, bcsrf}
	me := e.whoami(s)
	if me.AuthMethod != "local" || me.DisplayName != "alice" || me.PasswordChange.Mode != "local" || me.PasswordChange.URL != "" || me.ActiveSessions != 2 {
		t.Fatalf("local whoami: %+v", me)
	}
	body := func(c, n string) string {
		x, _ := json.Marshal(map[string]string{"current_password": c, "new_password": n})
		return string(x)
	}
	if w := e.do(s, "POST", "/api/v1/me/password", body("wrong password value", next)); w.Code != 403 {
		t.Fatal("wrong current password", w.Code)
	}
	if w := e.do(s, "POST", "/api/v1/me/password", body(pw, "short")); w.Code != 400 {
		t.Fatal("policy", w.Code)
	}
	// Missing CSRF: the browser middleware does not authenticate it.
	bad := s
	bad.csrf = strings.Repeat("x", 43)
	if w := e.do(bad, "POST", "/api/v1/me/password", body(pw, next)); w.Code != 401 {
		t.Fatal("csrf not enforced", w.Code)
	}
	w := e.do(s, "POST", "/api/v1/me/password", body(pw, next))
	if w.Code != 204 {
		t.Fatal("change", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), next) {
		t.Fatal("password echoed")
	}
	if _, err = e.sessions.Authenticate(ctx, b, "", false); err != nil {
		t.Fatal("current session must survive", err)
	}
	if _, err = e.sessions.Authenticate(ctx, a, "", false); err == nil {
		t.Fatal("other session must end")
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE instance_id=$1 AND principal_id=$2 AND action='auth.local.password_changed'`, e.instance, u.PrincipalID).Scan(&n)
	if n != 1 {
		t.Fatal("not audited")
	}
	if _, err = e.local.Login(ctx, "alice", next, "", ""); err != nil {
		t.Fatal("new password rejected", err)
	}
}

func TestMePasswordRateLimit(t *testing.T) {
	e := newMeEnv(t)
	const pw = "another long enough password"
	if _, err := e.local.CreateUser(context.Background(), "", "bob", pw, false); err != nil {
		t.Fatal(err)
	}
	_, raw, csrf, err := e.sessions.IssueExternal(context.Background(), localadmin.Issuer, "bob")
	if err != nil {
		t.Fatal(err)
	}
	s := meSession{raw, csrf}
	seen429 := false
	for i := 0; i < 12; i++ {
		if w := e.do(s, "POST", "/api/v1/me/password", `{"current_password":"wrong password value","new_password":"a brand new long password"}`); w.Code == 429 {
			seen429 = true
		}
	}
	if !seen429 {
		t.Fatal("no rate limit")
	}
}

func TestMeOIDCKnownProvidersAndOverride(t *testing.T) {
	e := newMeEnv(t)
	const issuer = "https://auth.example.com"
	e.provider("oidc", "Zitadel", `{"issuer":"`+issuer+`","client_id":"c","allowed_endpoints":["`+issuer+`/oauth/v2/authorize","`+issuer+`/oauth/v2/token","`+issuer+`/oauth/v2/keys"]}`)
	s, _ := e.external(issuer, "sub-1", oidclogin.Profile{Name: "Ada Lovelace", Email: "ada@example.com", Picture: "https://img.example.com/a.png", IDToken: "tok"})
	me := e.whoami(s)
	if me.AuthMethod != "oidc" || me.IdP != "Zitadel" || me.DisplayName != "Ada Lovelace" || me.Email != "ada@example.com" || me.Picture != "https://img.example.com/a.png" {
		t.Fatalf("oidc whoami: %+v", me)
	}
	if me.PasswordChange.Mode != "external" || me.PasswordChange.URL != issuer+"/ui/console/users/me/password" {
		t.Fatalf("zitadel url: %+v", me.PasswordChange)
	}
	if w := e.do(s, "POST", "/api/v1/me/password", `{"current_password":"x","new_password":"a brand new long password"}`); w.Code != 403 {
		t.Fatal("oidc user must not change a password locally", w.Code)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE identity_providers SET config=jsonb_set(config,'{password_change_url}','"https://account.example.com/pw"') WHERE instance_id=$1`, e.instance); err != nil {
		t.Fatal(err)
	}
	if me = e.whoami(s); me.PasswordChange.URL != "https://account.example.com/pw" {
		t.Fatalf("explicit override: %+v", me.PasswordChange)
	}
	// A non-https configured value is ignored, never offered.
	if _, err := e.pool.Exec(context.Background(), `UPDATE identity_providers SET config=jsonb_set(config,'{password_change_url}','"http://x.example/pw"') WHERE instance_id=$1`, e.instance); err != nil {
		t.Fatal(err)
	}
	if me = e.whoami(s); strings.HasPrefix(me.PasswordChange.URL, "http://") {
		t.Fatalf("http url offered: %+v", me.PasswordChange)
	}
}

func TestMeUnknownOIDCSAMLLDAPAndPicture(t *testing.T) {
	e := newMeEnv(t)
	e.provider("oidc", "Corp SSO", `{"issuer":"https://idp.example.com","client_id":"c","allowed_endpoints":["https://idp.example.com/a"]}`)
	s, _ := e.external("https://idp.example.com", "s1", oidclogin.Profile{Name: "Eve", Picture: "http://insecure.example/p.png"})
	me := e.whoami(s)
	if me.PasswordChange.Mode != "unavailable" || me.PasswordChange.Reason != "Your password is managed by Corp SSO." || me.PasswordChange.URL != "" {
		t.Fatalf("unknown oidc: %+v", me.PasswordChange)
	}
	if me.Picture != "" {
		t.Fatal("non-https picture stored")
	}
	e.provider("saml", "Corp SAML", `{"idp_entity_id":"https://saml.example.com/meta"}`)
	s, _ = e.external("https://saml.example.com/meta", "s2", oidclogin.Profile{Name: "Sam", Email: "sam@example.com"})
	if me = e.whoami(s); me.AuthMethod != "saml" || me.IdP != "Corp SAML" || me.PasswordChange.Mode != "unavailable" {
		t.Fatalf("saml: %+v", me)
	}
	e.provider("ldap", "Directory", `{"provider_id":"https://ldap.example.com/dir","password_change_url":"https://pw.example.com/"}`)
	s, _ = e.external("https://ldap.example.com/dir", "s3", oidclogin.Profile{Name: "lee"})
	if me = e.whoami(s); me.AuthMethod != "ldap" || me.PasswordChange.Mode != "external" || me.PasswordChange.URL != "https://pw.example.com/" {
		t.Fatalf("ldap configured: %+v", me)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE identity_providers SET config=config-'password_change_url' WHERE instance_id=$1 AND kind='ldap'`, e.instance); err != nil {
		t.Fatal(err)
	}
	if me = e.whoami(s); me.PasswordChange.Mode != "unavailable" || me.PasswordChange.Reason != "Your password is managed by your directory." {
		t.Fatalf("ldap default: %+v", me)
	}
}

func TestMeTokenSession(t *testing.T) {
	e := newMeEnv(t)
	var principal string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','robot') RETURNING id::text`, e.instance).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	h := &meHandlers{pool: e.pool, b: e.rt, hits: map[string]loginAttempt{}}
	ctx := identity.WithAuthenticatedPrincipal(context.Background(), identity.Principal{InstanceID: e.instance, PrincipalID: principal, TokenID: "t1"})
	w := httptest.NewRecorder()
	h.me(w, httptest.NewRequest("GET", "/api/v1/me", nil).WithContext(ctx))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	pc, _ := out["password_change"].(map[string]any)
	if w.Code != 200 || out["auth_method"] != "token" || pc["mode"] != "unavailable" || pc["url"] != nil {
		t.Fatal("token whoami", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.password(w, httptest.NewRequest("POST", "/api/v1/me/password", strings.NewReader(`{}`)).WithContext(ctx))
	if w.Code != 403 {
		t.Fatal("token password change", w.Code)
	}
}

func TestMeRevokeOthers(t *testing.T) {
	e := newMeEnv(t)
	ctx := context.Background()
	if _, err := e.local.CreateUser(ctx, "", "carl", "another long enough password", false); err != nil {
		t.Fatal(err)
	}
	_, a, _, _ := e.sessions.IssueExternal(ctx, localadmin.Issuer, "carl")
	_, b, bc, _ := e.sessions.IssueExternal(ctx, localadmin.Issuer, "carl")
	s := meSession{b, bc}
	w := e.do(s, "DELETE", "/api/v1/me/sessions/others", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"revoked":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := e.sessions.Authenticate(ctx, a, "", false); err == nil {
		t.Fatal("other session alive")
	}
	if e.whoami(s).ActiveSessions != 1 {
		t.Fatal("count")
	}
}
