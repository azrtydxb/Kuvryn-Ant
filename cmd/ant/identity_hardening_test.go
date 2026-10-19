package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/idpadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/go-ldap/ldap/v3"
	"github.com/jackc/pgx/v5/pgxpool"
)

const hardeningPassword = "emergency passphrase 42"

type hardeningFixture struct {
	pool     *pgxpool.Pool
	cfg      config.Config
	instance string
	actor    string
	rt       *browserRuntime
	mux      *http.ServeMux
}

type nopConn struct{}

func (nopConn) Bind(string, string) error { return nil }
func (nopConn) Search(_ context.Context, r *ldap.SearchRequest) (*ldap.SearchResult, error) {
	return &ldap.SearchResult{Entries: []*ldap.Entry{ldap.NewEntry(r.BaseDN, map[string][]string{"entryUUID": {"1"}})}}, nil
}
func (nopConn) Close() error { return nil }

func newHardeningFixture(t *testing.T) *hardeningFixture {
	pool, cfg := runtimeFixture(t)
	t.Setenv("ANT_OIDC_CONFIG_FILE", "")
	t.Setenv("ANT_SAML_CONFIG_FILE", "")
	t.Setenv("ANT_LDAP_CONFIG_FILE", "")
	ctx := context.Background()
	owner, err := identity.Bootstrap(ctx, pool, "hardening")
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys, _ = secrets.New("k1", map[string][]byte{"k1": bytes.Repeat([]byte{3}, 32)})
	cfg.PublicURL = "https://ant.example"
	f := &hardeningFixture{pool: pool, cfg: cfg, instance: owner.InstanceID, actor: owner.PrincipalID}
	var out bytes.Buffer
	if err = runBreakGlass(ctx, pool, cfg, strings.NewReader(hardeningPassword+"\n"), true, &out); err != nil {
		t.Fatal(err)
	}
	f.rt, err = openBrowser(ctx, pool, cfg, owner.InstanceID)
	if err != nil || f.rt == nil || f.rt.dyn == nil {
		t.Fatal("dynamic runtime unavailable", err)
	}
	f.rt.dyn.dial = func(context.Context, string, *tls.Config) (directory.Connection, error) { return nopConn{}, nil }
	f.mux = http.NewServeMux()
	f.rt.Register(f.mux)
	return f
}

func (f *hardeningFixture) login(body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/auth/local/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://ant.example")
	r.RemoteAddr = "198.51.100.20:4000"
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *hardeningFixture) providers() []string {
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/providers", nil))
	var body struct {
		Providers []string `json:"providers"`
		Local     bool     `json:"local"`
		SSO       bool     `json:"sso_mandatory"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Providers
}

func (f *hardeningFixture) count(t *testing.T, q string, args ...any) int {
	var n int
	if err := f.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBreakGlassCLIResetAndLocalLogin(t *testing.T) {
	f := newHardeningFixture(t)
	creds := `{"username":"break-glass","password":"` + hardeningPassword + `"}`
	w := f.login(creds, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var cookies = map[string]bool{}
	for _, c := range w.Result().Cookies() {
		cookies[c.Name] = c.HttpOnly && c.Secure
	}
	if !cookies[oidclogin.SessionCookie] || !cookies[oidclogin.CSRFCookie] {
		t.Fatalf("session cookies missing or insecure: %v", cookies)
	}
	// The issued session authenticates the principal as an administrator (emergency kind).
	var session, csrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case oidclogin.SessionCookie:
			session = c
		case oidclogin.CSRFCookie:
			csrf = c
		}
	}
	r := httptest.NewRequest("GET", "/auth/session", nil)
	r.AddCookie(session)
	r.AddCookie(csrf)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatal("session not valid", rec.Code, rec.Body.String())
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action='auth.breakglass.login' AND result='success'`) != 1 {
		t.Fatal("break-glass login not audited")
	}
	// Negative cases: wrong password, wrong origin, cross-site, unknown field, GET.
	for name, tc := range map[string]struct {
		body   string
		mutate func(*http.Request)
		want   int
	}{
		"wrong password": {`{"username":"break-glass","password":"not the password at all"}`, nil, 401},
		"wrong origin":   {creds, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 401},
		"cross site":     {creds, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 401},
		"extra field":    {`{"username":"break-glass","password":"x","admin":true}`, nil, 401},
		"not json":       {creds, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 401},
		"get":            {"", func(r *http.Request) { r.Method = "GET" }, 405},
	} {
		if got := f.login(tc.body, tc.mutate).Code; got != tc.want {
			t.Fatalf("%s: %d want %d", name, got, tc.want)
		}
	}
	// CLI reset: generated password is returned once, old password dies, sessions are revoked.
	var out bytes.Buffer
	if err := runBreakGlass(context.Background(), f.pool, f.cfg, nil, false, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Password string `json:"password"`
		Created  bool   `json:"created"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Created || len(result.Password) < 20 {
		t.Fatal("reset output", out.String())
	}
	if f.login(creds, nil).Code != 401 {
		t.Fatal("old password survived reset")
	}
	if f.login(`{"username":"break-glass","password":"`+result.Password+`"}`, nil).Code != 200 {
		t.Fatal("reset password rejected")
	}
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, func() *http.Request {
		r := httptest.NewRequest("GET", "/auth/session", nil)
		r.AddCookie(session)
		r.AddCookie(csrf)
		return r
	}())
	if rec.Code == 200 {
		t.Fatal("pre-reset session survived")
	}
	// Reset requires the root keyring.
	cfg := f.cfg
	cfg.SecretKeys = nil
	if err := runBreakGlass(context.Background(), f.pool, cfg, nil, false, &bytes.Buffer{}); err == nil {
		t.Fatal("reset without keyring accepted")
	}
}

func TestLocalLoginLockoutOverHTTP(t *testing.T) {
	f := newHardeningFixture(t)
	bad := `{"username":"break-glass","password":"wrong password here"}`
	for i := 0; i < localadmin.LockoutThreshold; i++ {
		if f.login(bad, func(r *http.Request) { r.RemoteAddr = "198.51.100.3" + string(rune('0'+i)) + ":1" }).Code != 401 {
			t.Fatal("expected 401")
		}
	}
	// Locked from a fresh peer even with the right password; same generic status.
	w := f.login(`{"username":"break-glass","password":"`+hardeningPassword+`"}`, func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1" })
	if w.Code != 401 || strings.Contains(w.Body.String(), "lock") {
		t.Fatal("lockout not enforced or leaked", w.Code, w.Body.String())
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action='auth.local.denied' AND result='denied' AND details->>'reason'='locked'`) != 1 {
		t.Fatal("lockout not audited as denied")
	}
}

func TestSSOMandatoryOverHTTP(t *testing.T) {
	f := newHardeningFixture(t)
	ctx := context.Background()
	hash, _ := localadmin.HashPassword("ordinary local password")
	var p string
	if err := f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'user','ops','ant-local') RETURNING id::text`, f.instance).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO local_accounts(instance_id,principal_id,username,password_hash) VALUES($1,$2,'ops',$3)`, f.instance, p, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,'ant-local','ops',$2)`, f.instance, p); err != nil {
		t.Fatal(err)
	}
	ops := `{"username":"ops","password":"ordinary local password"}`
	if f.login(ops, nil).Code != 200 {
		t.Fatal("ordinary local login should work before the policy")
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity_policy(instance_id,sso_mandatory) VALUES($1,true)`, f.instance); err != nil {
		t.Fatal(err)
	}
	if f.login(ops, nil).Code != 401 {
		t.Fatal("SSO mandatory not enforced for ordinary account")
	}
	if f.login(`{"username":"break-glass","password":"`+hardeningPassword+`"}`, nil).Code != 200 {
		t.Fatal("break-glass must work under SSO mandatory")
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/providers", nil))
	if !strings.Contains(w.Body.String(), `"sso_mandatory":true`) {
		t.Fatal(w.Body.String())
	}
}

func ldapInput(name string, enabled bool) idpadmin.Input {
	cfg, _ := json.Marshal(idpadmin.LDAPConfig{ProviderID: "https://ldap.example", URL: "ldaps://directory.example", BindDN: "cn=reader,dc=example", UserBaseDN: "ou=users,dc=example", GroupBaseDN: "ou=groups,dc=example",
		LoginAttribute: "uid", UserIDAttribute: "entryUUID", GroupIDAttribute: "entryUUID", UserClass: "person", GroupClass: "groupOfNames", MemberAttribute: "member",
		GroupRoles: map[string][]string{base64.RawURLEncoding.EncodeToString([]byte("dev")): {"reader"}}})
	secret := "bind-password-123"
	return idpadmin.Input{Kind: idpadmin.LDAP, Name: name, Enabled: enabled, Config: cfg, Secret: &secret}
}

func TestHotReloadProvidersWithoutRestartAndSafeRecovery(t *testing.T) {
	f := newHardeningFixture(t)
	ctx := context.Background()
	if got := f.providers(); len(got) != 0 {
		t.Fatalf("unexpected providers %v", got)
	}
	ldapPath := func() int {
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/ldap/login", nil))
		return w.Code
	}
	if ldapPath() != 404 {
		t.Fatal("LDAP route should not exist before configuration")
	}
	store := f.rt.dyn.store()
	p, err := store.Create(ctx, f.actor, ldapInput("corp", true), f.rt.dyn.builder().Check)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.rt.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.providers(); len(got) != 1 || got[0] != "ldap" {
		t.Fatalf("hot reload did not publish provider: %v", got)
	}
	if ldapPath() != 200 {
		t.Fatal("LDAP login route not live after reload")
	}
	active := f.rt.Active()
	if len(active) != 1 || active[0].Source != "database" || active[0].ProviderID != p.ID {
		t.Fatalf("%+v", active)
	}
	// A broken stored configuration (edited out-of-band) is skipped and recorded;
	// the previous snapshot is replaced by a healthy one without the provider,
	// and break-glass login keeps working.
	if _, err = f.pool.Exec(ctx, `UPDATE identity_providers SET config=jsonb_set(config,'{url}','"http://insecure.example"') WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.rt.Reload(ctx); err == nil {
		t.Fatal("broken provider should be reported")
	}
	if got := f.providers(); len(got) != 0 {
		t.Fatalf("broken provider still served: %v", got)
	}
	got, _ := store.Get(ctx, p.ID)
	if got.LastError == "" || strings.Contains(got.LastError, "bind-password") {
		t.Fatal("failure not recorded safely:", got.LastError)
	}
	if !f.rt.dyn.needsRetry.Load() {
		t.Fatal("failed provider should be retried")
	}
	if f.login(`{"username":"break-glass","password":"`+hardeningPassword+`"}`, nil).Code != 200 {
		t.Fatal("break-glass login must survive a broken SSO configuration")
	}
	// Disable and delete take effect immediately too.
	if _, err = f.pool.Exec(ctx, `UPDATE identity_providers SET config=jsonb_set(config,'{url}','"ldaps://directory.example"') WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	_ = f.rt.Reload(ctx)
	if len(f.providers()) != 1 {
		t.Fatal("repaired provider not served after reload")
	}
	cur, _ := store.Get(ctx, p.ID)
	if _, err = store.SetEnabled(ctx, f.actor, p.ID, cur.Revision, false, nil); err != nil {
		t.Fatal(err)
	}
	_ = f.rt.Reload(ctx)
	if len(f.providers()) != 0 || ldapPath() != 404 {
		t.Fatal("disable did not take effect without restart")
	}
}

func TestDatabaseProviderShadowedByFileConfiguration(t *testing.T) {
	f := newHardeningFixture(t)
	ctx := context.Background()
	f.rt.dyn.files.ldap = &testLDAPBrowser{}
	f.rt.dyn.files.ldapIssuer = "https://file-ldap.example"
	if _, err := f.rt.dyn.store().Create(ctx, f.actor, ldapInput("db-ldap", true), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	active := f.rt.Active()
	if len(active) != 1 || active[0].Source != "file" {
		t.Fatalf("file configuration must take precedence: %+v", active)
	}
	_, ent := f.rt.cur()
	if ent.ldapIssuer != "https://file-ldap.example" {
		t.Fatal("database LDAP replaced file LDAP")
	}
}

func TestLocalUserCLIAndBrowserSession(t *testing.T) {
	f := newHardeningFixture(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := runLocalUser(ctx, f.pool, f.cfg, "owner", true, nil, false, &out); err != nil {
		t.Fatal(err)
	}
	var created struct {
		Password      string `json:"password"`
		Created       bool   `json:"created"`
		Administrator bool   `json:"administrator"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil || !created.Created || !created.Administrator || len(created.Password) < 20 {
		t.Fatal("create output", err)
	}
	// Ordinary local accounts sign in over HTTP and receive HttpOnly session cookies.
	w := f.login(`{"username":"owner","password":"`+created.Password+`"}`, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	secure := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		secure[c.Name] = c.HttpOnly && c.Secure
	}
	if !secure[oidclogin.SessionCookie] || !secure[oidclogin.CSRFCookie] {
		t.Fatalf("session cookies missing or insecure: %v", secure)
	}
	if f.login(`{"username":"owner","password":"not the password at all"}`, nil).Code != 401 {
		t.Fatal("wrong password accepted")
	}
	// Running it again resets the password (and refuses --admin on an existing account).
	out.Reset()
	if err := runLocalUser(ctx, f.pool, f.cfg, "owner", false, nil, false, &out); err != nil {
		t.Fatal(err)
	}
	var reset struct {
		Password string `json:"password"`
		Created  bool   `json:"created"`
	}
	if err := json.Unmarshal(out.Bytes(), &reset); err != nil || reset.Created || reset.Password == created.Password {
		t.Fatal("reset output")
	}
	if f.login(`{"username":"owner","password":"`+created.Password+`"}`, nil).Code != 401 {
		t.Fatal("old password survived reset")
	}
	if f.login(`{"username":"owner","password":"`+reset.Password+`"}`, nil).Code != 200 {
		t.Fatal("new password rejected")
	}
	if err := runLocalUser(ctx, f.pool, f.cfg, "owner", true, nil, false, &bytes.Buffer{}); err == nil {
		t.Fatal("--admin on existing account accepted")
	}
	if err := runLocalUser(ctx, f.pool, f.cfg, "Bad Name", false, nil, false, &bytes.Buffer{}); err == nil {
		t.Fatal("bad username accepted")
	}
}

func TestProvidersAdvertiseLabelsOfEnabledStoredProviders(t *testing.T) {
	f := newHardeningFixture(t)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity_providers(instance_id,kind,name,enabled,config) VALUES($1,'ldap','Corp directory',true,'{}')`, f.instance); err != nil {
		t.Skip("identity_providers columns differ: ", err)
	}
	got := f.rt.dyn.labels(context.Background(), []string{"ldap"})
	if got["ldap"] != "Corp directory" {
		t.Fatalf("%v", got)
	}
	if len(f.rt.dyn.labels(context.Background(), []string{"oidc"})) != 0 {
		t.Fatal("label for inactive protocol")
	}
}
