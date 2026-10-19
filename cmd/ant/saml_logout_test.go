package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
)

type sessionCookies struct{ session, csrf *http.Cookie }

// samlSignIn performs a full SAML browser login through the handlers.
func samlSignIn(t *testing.T, e *enterpriseLogin) sessionCookies {
	t.Helper()
	start := httptest.NewRecorder()
	e.samlLogin(start, httptest.NewRequest("GET", "https://ant.example/auth/saml/login", nil))
	loginCookie := findCookie(t, start, "__Host-ant-saml-login")
	location, _ := url.Parse(start.Header().Get("Location"))
	body := url.Values{"RelayState": {location.Query().Get("RelayState")}, "SAMLResponse": {base64.StdEncoding.EncodeToString([]byte("verified-assertion"))}}
	r := httptest.NewRequest("POST", "https://ant.example/auth/saml/callback", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(loginCookie)
	w := httptest.NewRecorder()
	e.samlCallback(w, r)
	if w.Code != 200 {
		t.Fatal("sign-in failed", w.Code, w.Body.String())
	}
	return sessionCookies{findCookie(t, w, oidclogin.SessionCookie), findCookie(t, w, oidclogin.CSRFCookie)}
}

func logoutPost(rt *browserRuntime, c sessionCookies, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://ant.example/auth/logout", nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("X-CSRF-Token", c.csrf.Value)
	r.AddCookie(c.session)
	r.AddCookie(c.csrf)
	w := httptest.NewRecorder()
	rt.logout(w, r)
	return w
}

func sloPost(e *enterpriseLogin, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://ant.example/auth/saml/slo", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	e.samlSLO(w, r)
	return w
}

func sessionAlive(t *testing.T, e *enterpriseLogin, c sessionCookies) bool {
	t.Helper()
	_, err := e.client.Sessions.Authenticate(context.Background(), c.session.Value, "", false)
	return err == nil
}

func TestSAMLSPInitiatedLogoutEndToEnd(t *testing.T) {
	e, saml, _, _ := enterpriseFixture(t)
	saml.logout, saml.sessionIndex = true, "idx-1"
	rt := &browserRuntime{client: e.client, enterprise: e}
	c := samlSignIn(t, e)

	// A cross-origin logout never reaches the IdP leg and does not end the session.
	if w := logoutPost(rt, c, "https://evil.example"); w.Code != 401 || !sessionAlive(t, e, c) {
		t.Fatalf("cross-origin logout: %d", w.Code)
	}
	w := logoutPost(rt, c, e.client.Origin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"saml_logout":"/auth/saml/logout"`) {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if sessionAlive(t, e, c) {
		t.Fatal("local session survived logout")
	}
	if saml.lastIdentity.Issuer != "https://id.example/saml" || saml.lastIdentity.Subject != "immutable-subject" || saml.lastIdentity.SessionIndex != "idx-1" || saml.lastIdentity.NameIDFormat != "urn:test:persistent" {
		t.Fatalf("request not built from the session's IdP identity: %+v", saml.lastIdentity)
	}
	if cleared := findCookie(t, w, oidclogin.SessionCookie); cleared.MaxAge >= 0 {
		t.Fatal("session cookie not cleared")
	}
	stage := findCookie(t, w, samlLogoutCookie)
	if !stage.Secure || !stage.HttpOnly || stage.SameSite != http.SameSiteNoneMode {
		t.Fatal("unsafe logout correlation cookie")
	}

	// The browser navigates to the staged form once; it posts only to the IdP.
	page := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://ant.example/auth/saml/logout", nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		e.samlLogoutPage(w, r)
		return w
	}
	if w = page(nil); w.Code != 303 {
		t.Fatalf("unbound page: %d", w.Code)
	}
	w = page(stage)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `action="https://id.example/slo"`) || !strings.Contains(body, `value="signed-logout-request"`) || strings.Contains(body, "SAMLResponse") {
		t.Fatalf("page: %d %s", w.Code, body)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'sha256-") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("weak content policy: %s", csp)
	}
	if w = page(stage); w.Code != 303 {
		t.Fatalf("staged request served twice: %d", w.Code)
	}

	// The IdP's response must carry this browser's state and a valid response.
	relay := saml.lastRelay
	good := url.Values{"SAMLResponse": {base64.StdEncoding.EncodeToString([]byte("idp-logout-response"))}, "RelayState": {relay}}
	if w = sloPost(e, nil, good); w.Code != 401 {
		t.Fatalf("unbound response accepted: %d", w.Code)
	}
	forged := url.Values{"SAMLResponse": {base64.StdEncoding.EncodeToString([]byte("forged"))}, "RelayState": {relay}}
	if w = sloPost(e, stage, forged); w.Code != 401 {
		t.Fatalf("forged response accepted: %d", w.Code)
	}
	// The failed attempt consumed the single-use state: a retry with the good response fails too.
	if w = sloPost(e, stage, good); w.Code != 401 {
		t.Fatalf("state reused: %d", w.Code)
	}

	// A fresh logout completes.
	saml.sessionIndex = "idx-2"
	c = samlSignIn(t, e)
	w = logoutPost(rt, c, e.client.Origin)
	stage = findCookie(t, w, samlLogoutCookie)
	good = url.Values{"SAMLResponse": {base64.StdEncoding.EncodeToString([]byte("idp-logout-response"))}, "RelayState": {saml.lastRelay}}
	w = sloPost(e, stage, good)
	if w.Code != 303 || w.Header().Get("Location") != "/console/" {
		t.Fatalf("response not accepted: %d %s", w.Code, w.Body.String())
	}
	if w = sloPost(e, stage, good); w.Code != 401 {
		t.Fatalf("response replay: %d", w.Code)
	}
	var ok, bad int
	if err := e.client.Sessions.DB.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE action='auth.saml_logout.success'),count(*) FILTER(WHERE action='auth.saml_logout.failure') FROM audit_events`).Scan(&ok, &bad); err != nil || ok != 1 || bad < 3 {
		t.Fatal("logout audit", ok, bad, err)
	}
}

func TestLogoutWithoutIdPSingleLogoutStaysLocal(t *testing.T) {
	e, saml, _, _ := enterpriseFixture(t)
	rt := &browserRuntime{client: e.client, enterprise: e}
	// The IdP publishes no single logout endpoint: plain local logout.
	c := samlSignIn(t, e)
	if w := logoutPost(rt, c, e.client.Origin); w.Code != 204 || sessionAlive(t, e, c) {
		t.Fatalf("local logout: %d", w.Code)
	}
	// A session from another provider never starts a SAML leg.
	saml.logout = true
	cookie, csrf := ldapPrelogin(t, e)
	w := ldapPost(e, cookie, csrf, e.client.Origin, `{"username":"alice","password":"private-password"}`)
	ldapSession := sessionCookies{findCookie(t, w, oidclogin.SessionCookie), findCookie(t, w, oidclogin.CSRFCookie)}
	if w = logoutPost(rt, ldapSession, e.client.Origin); w.Code != 204 || sessionAlive(t, e, ldapSession) {
		t.Fatalf("ldap logout: %d", w.Code)
	}
	if saml.lastIdentity.Subject != "" {
		t.Fatal("SAML logout started for an LDAP session")
	}
}

func TestSAMLIdPInitiatedLogout(t *testing.T) {
	e, saml, _, _ := enterpriseFixture(t)
	saml.logout, saml.sessionIndex = true, "idx-1"
	request := func(nameID, index, relay string) url.Values {
		v := url.Values{"SAMLRequest": {base64.StdEncoding.EncodeToString([]byte("idp-logout-request|" + nameID + "|" + index))}}
		if relay != "" {
			v.Set("RelayState", relay)
		}
		return v
	}
	one, two := samlSignIn(t, e), samlSignIn(t, e)
	// A request for a different IdP session index leaves these sessions alone.
	w := sloPost(e, nil, request("immutable-subject", "idx-other", ""))
	if w.Code != 200 || !sessionAlive(t, e, one) || !sessionAlive(t, e, two) {
		t.Fatalf("request: %d", w.Code)
	}
	// Garbage and non-requests are rejected without any response to the IdP.
	for name, form := range map[string]url.Values{
		"garbage":     {"SAMLRequest": {base64.StdEncoding.EncodeToString([]byte("garbage"))}},
		"not base64":  {"SAMLRequest": {"!!!"}},
		"both":        {"SAMLRequest": {request("immutable-subject", "idx-1", "").Get("SAMLRequest")}, "SAMLResponse": {"AAAA"}},
		"extra field": {"SAMLRequest": {request("immutable-subject", "idx-1", "").Get("SAMLRequest")}, "other": {"x"}},
		"empty":       {},
	} {
		if w := sloPost(e, nil, form); w.Code != 401 || strings.Contains(w.Body.String(), "SAMLResponse") {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	if !sessionAlive(t, e, one) || !sessionAlive(t, e, two) {
		t.Fatal("rejected requests ended sessions")
	}
	// A matching request ends every session of the subject and answers with a signed response.
	w = sloPost(e, nil, request("immutable-subject", "idx-1", "relay-1"))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `action="https://id.example/slo"`) || !strings.Contains(body, `name="SAMLResponse" value="signed-logout-response-for-idp-request-one"`) || !strings.Contains(body, `name="RelayState" value="relay-1"`) {
		t.Fatalf("response page: %d %s", w.Code, body)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'sha256-") {
		t.Fatal("missing content policy")
	}
	if sessionAlive(t, e, one) || sessionAlive(t, e, two) {
		t.Fatal("sessions survived IdP logout")
	}
	if cleared := findCookie(t, w, oidclogin.SessionCookie); cleared.MaxAge >= 0 {
		t.Fatal("session cookie not cleared")
	}
	// Logging out an absent session is still a success.
	if w = sloPost(e, nil, request("immutable-subject", "idx-1", "")); w.Code != 200 {
		t.Fatalf("absent session: %d", w.Code)
	}
	var revoked int
	if err := e.client.Sessions.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='browser.session_revoked_by_idp'`).Scan(&revoked); err != nil || revoked != 2 {
		t.Fatal("audit", revoked, err)
	}
	// Wrong method, content type and host are refused.
	r := httptest.NewRequest("GET", "https://ant.example/auth/saml/slo?SAMLRequest=x", nil)
	rec := httptest.NewRecorder()
	e.samlSLO(rec, r)
	if rec.Code != 401 {
		t.Fatalf("GET accepted: %d", rec.Code)
	}
	r = httptest.NewRequest("POST", "https://evil.example/auth/saml/slo", strings.NewReader(request("immutable-subject", "idx-1", "").Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	e.samlSLO(rec, r)
	if rec.Code != 401 {
		t.Fatalf("foreign host accepted: %d", rec.Code)
	}
}

func TestSLORoutesAreRegistered(t *testing.T) {
	e, _, _, _ := enterpriseFixture(t)
	mux := http.NewServeMux()
	e.Register(mux)
	for _, p := range []struct{ method, path string }{{"POST", "/auth/saml/slo"}, {"GET", "/auth/saml/logout"}} {
		if _, pattern := mux.Handler(httptest.NewRequest(p.method, "https://ant.example"+p.path, nil)); pattern == "" {
			t.Fatalf("%s %s is not routed", p.method, p.path)
		}
	}
}
