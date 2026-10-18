package oidclogin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const SessionCookie = "__Host-ant-session"
const CSRFCookie = "__Host-ant-csrf"
const loginCookie = "__Host-ant-login"

// Client is bound to one trusted provider and one instance. Mount Callback at
// exactly ProviderConfig.RedirectURI's path. Do not derive that URI from Host or
// forwarding headers. Origin must be the externally configured console origin.
type Client struct {
	Provider *Provider
	Sessions Sessions
	Origin   string
	// SuccessRedirect is a trusted application path; only the console route is accepted.
	SuccessRedirect string
	limiter         peerLimiter
}

func cookie(w http.ResponseWriter, name, value string, expiry time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expiry, MaxAge: maxAge})
}
func getCookie(r *http.Request, name string) string {
	c, e := r.Cookie(name)
	if e != nil {
		return ""
	}
	return c.Value
}
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
func failure(w http.ResponseWriter) {
	http.Error(w, "browser authentication failed", http.StatusUnauthorized)
}
func (c *Client) Login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !c.allowLogin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Reject cross-site subresource login initiation. Top-level navigations remain
	// possible; callback requires browser-bound state and one-time PKCE verifier.
	if v := r.Header.Get("Sec-Fetch-Dest"); v != "" && v != "document" {
		c.loginFailure(w, r)
		return
	}
	state, browser, nonce := secret(), secret(), secret()
	if e := c.Sessions.begin(r.Context(), c.Provider, state, browser, nonce); e != nil {
		c.loginFailure(w, r)
		return
	}
	cookie(w, loginCookie, browser, time.Now().Add(5*time.Minute), 300)
	http.Redirect(w, r, c.Provider.authorization(state, nonce, browser), http.StatusFound)
}
func (c *Client) Callback(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !c.allowLogin(w, r) {
		return
	}
	expected, _ := url.Parse(c.Provider.redirect)
	if r.Method != http.MethodGet || r.URL.EscapedPath() != expected.EscapedPath() || r.Host != expected.Host {
		c.loginFailure(w, r)
		return
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || len(q["state"]) != 1 || len(q["code"]) != 1 || q.Get("error") != "" || len(q.Get("code")) > 8192 || q.Get("code") == "" || len(q["iss"]) > 1 || q.Get("iss") != "" && q.Get("iss") != c.Provider.issuer {
		c.loginFailure(w, r)
		return
	}
	browser := getCookie(r, loginCookie)
	nonce, e := c.Sessions.consume(r.Context(), c.Provider, q.Get("state"), browser)
	cookie(w, loginCookie, "", time.Unix(1, 0), -1)
	if e != nil {
		c.loginFailure(w, r)
		return
	}
	subject, groups, e := c.Provider.exchangeIdentity(r.Context(), q.Get("code"), browser, nonce)
	if e != nil {
		c.loginFailure(w, r)
		return
	}
	session, raw, csrf, e := c.Sessions.issueGroups(r.Context(), c.Provider.issuer, subject, groups)
	if e != nil {
		c.loginFailure(w, r)
		return
	}
	// Rotate the local session on every successful login.
	if old := getCookie(r, SessionCookie); old != "" {
		if c.Sessions.Revoke(r.Context(), old) != nil {
			_ = c.Sessions.Revoke(r.Context(), raw)
			c.loginFailure(w, r)
			return
		}
	}
	age := int(time.Until(session.ExpiresAt).Seconds())
	cookie(w, SessionCookie, raw, session.ExpiresAt, age)
	cookie(w, CSRFCookie, csrf, session.ExpiresAt, age)
	if c.SuccessRedirect == "/console/" {
		http.Redirect(w, r, c.SuccessRedirect, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Session Session `json:"session"`
		CSRF    string  `json:"csrf_token"`
	}{session, csrf})
}

// AuthenticateRequest is for management APIs only; package clients retain their
// machine-credential pipeline. The caller must still authorize the principal.
func (c *Client) AuthenticateRequest(r *http.Request) (Session, error) {
	mutation := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	if mutation && (c.Origin == "" || r.Header.Get("Origin") != c.Origin) {
		return Session{}, ErrAuthentication
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return Session{}, ErrAuthentication
	}
	return c.Sessions.Authenticate(r.Context(), getCookie(r, SessionCookie), r.Header.Get("X-CSRF-Token"), mutation)
}

// SessionInfo supplies the CSRF token to same-origin JS after page reload. The
// parent must not enable credentialed cross-origin CORS for these routes.
func (c *Client) SessionInfo(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != c.Origin {
		failure(w)
		return
	}
	session, e := c.AuthenticateRequest(r)
	if e != nil {
		failure(w)
		return
	}
	csrf := getCookie(r, CSRFCookie)
	if _, e = c.Sessions.Authenticate(r.Context(), getCookie(r, SessionCookie), csrf, true); e != nil {
		failure(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Session Session `json:"session"`
		CSRF    string  `json:"csrf_token"`
	}{session, csrf})
}
func (c *Client) Logout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, e := c.AuthenticateRequest(r); e != nil {
		failure(w)
		return
	}
	if e := c.Sessions.Revoke(r.Context(), getCookie(r, SessionCookie)); e != nil {
		failure(w)
		return
	}
	cookie(w, SessionCookie, "", time.Unix(1, 0), -1)
	cookie(w, CSRFCookie, "", time.Unix(1, 0), -1)
	w.WriteHeader(http.StatusNoContent)
}
