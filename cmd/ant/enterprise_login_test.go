package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/samllogin"
)

type testSAMLBrowser struct {
	subject string
	calls   int
}

func (p *testSAMLBrowser) Begin(state string) (*url.URL, samllogin.Pending, error) {
	u, _ := url.Parse("https://id.example/login?RelayState=" + state)
	return u, samllogin.Pending{ID: "request-one", Expires: time.Now().Add(4 * time.Minute)}, nil
}
func (p *testSAMLBrowser) Consume(_ context.Context, raw []byte, pending samllogin.Pending) (samllogin.Identity, error) {
	p.calls++
	if string(raw) != "verified-assertion" || pending.ID != "request-one" {
		return samllogin.Identity{}, errors.New("invalid")
	}
	return samllogin.Identity{Issuer: "https://id.example/saml", Subject: p.subject}, nil
}
func (*testSAMLBrowser) Metadata() ([]byte, error) { return []byte("metadata"), nil }

type testLDAPBrowser struct {
	subject string
	calls   int
}

func (p *testLDAPBrowser) Authenticate(_ context.Context, user string, password []byte) (directory.ExternalIdentity, error) {
	p.calls++
	if user != "alice" || string(password) != "private-password" {
		return directory.ExternalIdentity{}, errors.New("private-directory-response")
	}
	return directory.ExternalIdentity{ProviderID: "https://id.example/ldap", Subject: p.subject, RoleIDs: []string{"must-not-grant-admin"}}, nil
}
func enterpriseFixture(t *testing.T) (*enterpriseLogin, *testSAMLBrowser, *testLDAPBrowser, string) {
	t.Helper()
	pool, _ := runtimeFixture(t)
	ctx := context.Background()
	owner, err := identity.Bootstrap(ctx, pool, "Enterprise")
	if err != nil {
		t.Fatal(err)
	}
	var principal string
	if err = pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','enterprise-user') RETURNING id::text`, owner.InstanceID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	sessions := oidclogin.Sessions{DB: pool, InstanceID: owner.InstanceID}
	for _, issuer := range []string{"https://id.example/saml", "https://id.example/ldap"} {
		if err = sessions.BindIdentity(ctx, issuer, "immutable-subject", principal); err != nil {
			t.Fatal(err)
		}
	}
	saml := &testSAMLBrowser{subject: "immutable-subject"}
	ldap := &testLDAPBrowser{subject: "immutable-subject"}
	e := &enterpriseLogin{client: &oidclogin.Client{Sessions: sessions, Origin: "https://ant.example"}, saml: saml, ldap: ldap, samlIssuer: "https://id.example/saml", ldapIssuer: "https://id.example/ldap"}
	return e, saml, ldap, principal
}
func findCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatal("missing cookie", name)
	return nil
}
func TestSAMLBrowserCorrelationAndSession(t *testing.T) {
	e, saml, _, principal := enterpriseFixture(t)
	start := httptest.NewRecorder()
	e.samlLogin(start, httptest.NewRequest("GET", "https://ant.example/auth/saml/login", nil))
	if start.Code != 302 {
		t.Fatal(start.Code)
	}
	cookie := findCookie(t, start, "__Host-ant-saml-login")
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteNoneMode {
		t.Fatal("unsafe POST correlation cookie")
	}
	location, _ := url.Parse(start.Header().Get("Location"))
	state := location.Query().Get("RelayState")
	post := func(cookie *http.Cookie, state, assertion string) *httptest.ResponseRecorder {
		body := url.Values{"RelayState": {state}, "SAMLResponse": {base64.StdEncoding.EncodeToString([]byte(assertion))}}
		r := httptest.NewRequest("POST", "https://ant.example/auth/saml/callback", strings.NewReader(body.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		e.samlCallback(w, r)
		return w
	}
	if w := post(nil, state, "verified-assertion"); w.Code != 401 || saml.calls != 0 {
		t.Fatal("unbound callback")
	}
	w := post(cookie, state, "verified-assertion")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	sessionCookie := findCookie(t, w, oidclogin.SessionCookie)
	if !sessionCookie.Secure || !sessionCookie.HttpOnly {
		t.Fatal("unsafe session")
	}
	session, err := e.client.Sessions.Authenticate(context.Background(), sessionCookie.Value, "", false)
	if err != nil || session.PrincipalID != principal {
		t.Fatal("session not issued", err)
	}
	if w = post(cookie, state, "verified-assertion"); w.Code != 401 || saml.calls != 1 {
		t.Fatal("callback replay")
	}
}
func ldapPrelogin(t *testing.T, e *enterpriseLogin) (*http.Cookie, string) {
	t.Helper()
	w := httptest.NewRecorder()
	e.ldapLogin(w, httptest.NewRequest("GET", "https://ant.example/auth/ldap/login", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var body map[string]string
	if json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal("invalid prelogin JSON")
	}
	return findCookie(t, w, "__Host-ant-ldap-login"), body["csrf_token"]
}
func ldapPost(e *enterpriseLogin, cookie *http.Cookie, csrf, origin, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://ant.example/auth/ldap/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	r.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	e.ldapLogin(w, r)
	return w
}
func TestLDAPBrowserCSRFExplicitBindingAuditAndSession(t *testing.T) {
	e, _, ldap, principal := enterpriseFixture(t)
	cookie, csrf := ldapPrelogin(t, e)
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe LDAP login cookie")
	}
	credentials := `{"username":"alice","password":"private-password"}`
	if w := ldapPost(e, cookie, csrf, "https://evil.example", credentials); w.Code != 401 || ldap.calls != 0 {
		t.Fatal("cross-site credentials reached LDAP")
	}
	if w := ldapPost(e, nil, csrf, e.client.Origin, credentials); w.Code != 401 || ldap.calls != 0 {
		t.Fatal("unbound credentials reached LDAP")
	}
	w := ldapPost(e, cookie, csrf, e.client.Origin, credentials)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	raw := findCookie(t, w, oidclogin.SessionCookie).Value
	s, err := e.client.Sessions.Authenticate(context.Background(), raw, "", false)
	if err != nil || s.PrincipalID != principal {
		t.Fatal("missing bound session", err)
	}
	if w = ldapPost(e, cookie, csrf, e.client.Origin, credentials); w.Code != 401 || ldap.calls != 1 {
		t.Fatal("pre-login nonce replay")
	}
	cookie, csrf = ldapPrelogin(t, e)
	ldap.subject = "unbound-subject"
	if w = ldapPost(e, cookie, csrf, e.client.Origin, credentials); w.Code != 401 {
		t.Fatal("unbound user granted access")
	}
	cookie, csrf = ldapPrelogin(t, e)
	w = ldapPost(e, cookie, csrf, e.client.Origin, `{"username":"alice","password":"bad"}`)
	if w.Code != 401 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("failure disclosed credentials")
	}
	var success, failed, principals int
	pool := e.client.Sessions.DB
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE action='auth.ldap.success'),count(*) FILTER(WHERE action='auth.ldap.failure') FROM audit_events`).Scan(&success, &failed); err != nil || success != 1 || failed < 4 {
		t.Fatal(success, failed, err)
	}
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM principals`).Scan(&principals); err != nil || principals != 2 {
		t.Fatal("implicit account provisioning", principals, err)
	}
	var leaked bool
	if err = pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE details::text LIKE '%private-password%' OR details::text LIKE '%alice%')`).Scan(&leaked); err != nil || leaked {
		t.Fatal("credentials in audit", err)
	}
}
func TestEnterpriseConfigurationAndThrottle(t *testing.T) {
	var c ldapBrowserConfig
	for _, raw := range []string{`{"origin":"https://ant.example","origin":"https://evil.example"}`, `{"Origin":"https://ant.example"}`, `{"origin":null}`, `{"bind_password":"secret"}`, `{} {}`} {
		if exactObject([]byte(raw), &c) == nil {
			t.Fatal("ambiguous config accepted")
		}
	}
	for _, origin := range []string{"http://ant.example", "https://ant.example/", "https://user@ant.example", "https://ant.example?", "https://ant.example/#x"} {
		if validBrowserOrigin(origin) {
			t.Fatal("unsafe origin", origin)
		}
	}
	var throttle loginThrottle
	for i := 0; i < 20; i++ {
		if !throttle.allow("192.0.2.1:1234") {
			t.Fatal("early limit")
		}
	}
	if throttle.allow("192.0.2.1:9999") {
		t.Fatal("source port bypass")
	}
	if !throttle.allow("192.0.2.2:1234") {
		t.Fatal("unrelated peer blocked")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ldap.json")
	os.WriteFile(path, []byte(`{"origin":"https://different.example"}`), 0600)
	if _, _, err := openEnterprise(nil, "", "https://ant.example", "", path); err == nil {
		t.Fatal("cross-provider origin mismatch accepted")
	}
}

func TestLDAPMountedConfigurationWithoutOIDC(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "bind-password")
	if err := os.WriteFile(password, []byte("service-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := ldapBrowserConfig{Origin: "https://ant.example", ProviderID: "https://identity.example/directory/main", URL: "ldaps://directory.example:636", BindDN: "cn=service,dc=example", BindPasswordFile: password, UserBaseDN: "ou=users,dc=example", GroupBaseDN: "ou=groups,dc=example", LoginAttribute: "uid", UserIDAttribute: "entryUUID", GroupIDAttribute: "entryUUID", UserClass: "inetOrgPerson", GroupClass: "groupOfNames", MemberAttribute: "member"}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "ldap.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	enterprise, origin, err := openEnterprise(nil, "", "", "", path)
	if err != nil || enterprise.ldap == nil || enterprise.saml != nil || origin != cfg.Origin {
		t.Fatal("standalone LDAP config", err)
	}
	cfg.URL = "http://directory.example"
	data, _ = json.Marshal(cfg)
	os.WriteFile(path, data, 0600)
	if _, _, err = openEnterprise(nil, "", "", "", path); err == nil {
		t.Fatal("plaintext directory URL accepted")
	}
}

func TestBrowserProviderDiscoveryAndUnknownAuth(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var b *browserRuntime
		if enabled {
			b = &browserRuntime{client: &oidclogin.Client{}, enterprise: &enterpriseLogin{saml: &testSAMLBrowser{}, ldap: &testLDAPBrowser{}}}
		}
		mux := http.NewServeMux()
		b.Register(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/providers", nil))
		expected := `{"providers":[]}`
		if enabled {
			expected = `{"providers":["saml","ldap"]}`
		}
		if strings.TrimSpace(w.Body.String()) != expected || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal(w.Body.String())
		}
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/unknown", nil))
		if w.Code != 404 || !strings.Contains(w.Body.String(), `"code":"not_found"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestSAMLBrowserRedirectIsTrustedAndKeepsCookies(t *testing.T) {
	e, _, _, _ := enterpriseFixture(t)
	e.client.SuccessRedirect = "/console/"
	w := httptest.NewRecorder()
	e.issue(w, httptest.NewRequest("POST", "https://ant.example/auth/saml/callback?redirect=https://evil.example", nil), "saml", e.samlIssuer, "immutable-subject")
	if w.Code != 303 || w.Header().Get("Location") != "/console/" {
		t.Fatal(w.Code, w.Header())
	}
	findCookie(t, w, oidclogin.SessionCookie)
}
