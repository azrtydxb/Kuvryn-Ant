package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/samllogin"
	"github.com/jackc/pgx/v5"
)

// SAML single logout over the HTTP-POST binding.
//
//   - SP-initiated: the console's POST /auth/logout ends the local session first
//     (always), then, when the session came from the SAML provider and the IdP
//     publishes a POST logout endpoint, stages a signed LogoutRequest bound to
//     the browser and answers {"saml_logout":"/auth/saml/logout"}. Navigating
//     there serves a self-submitting form to the IdP; the IdP's signed
//     LogoutResponse returns to /auth/saml/slo and is correlated to the staged
//     state. The local logout never depends on the IdP outcome.
//   - IdP-initiated: a signed LogoutRequest posted to /auth/saml/slo ends the
//     matching sessions and is answered with a signed LogoutResponse.

const samlLogoutCookie = "__Host-ant-saml-logout"
const samlLogoutPath = "/auth/saml/logout"

// postPageScript submits the self-posting form; the page's content policy allows
// exactly this script by hash and form posts only to https destinations.
const postPageScript = `document.getElementById("f").submit();`

var postPageCSP = func() string {
	sum := sha256.Sum256([]byte(postPageScript))
	return "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; form-action https:; base-uri 'none'; frame-ancestors 'none'"
}()

var postPage = template.Must(template.New("post").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Signing out</title></head><body>` +
	`<form id="f" method="post" action="{{.Destination}}">` +
	`{{if .SAMLRequest}}<input type="hidden" name="SAMLRequest" value="{{.SAMLRequest}}">{{end}}` +
	`{{if .SAMLResponse}}<input type="hidden" name="SAMLResponse" value="{{.SAMLResponse}}">{{end}}` +
	`{{if .RelayState}}<input type="hidden" name="RelayState" value="{{.RelayState}}">{{end}}` +
	`<noscript><button type="submit">Continue</button></noscript></form><script>` + postPageScript + `</script></body></html>`))

func writePostPage(w http.ResponseWriter, p samllogin.LogoutPost) {
	browserNoStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", postPageCSP)
	_ = postPage.Execute(w, p)
}

func (e *enterpriseLogin) clearSession(w http.ResponseWriter) {
	enterpriseCookie(w, oidclogin.SessionCookie, "", time.Unix(1, 0), http.SameSiteLaxMode)
	enterpriseCookie(w, oidclogin.CSRFCookie, "", time.Unix(1, 0), http.SameSiteLaxMode)
}

func (e *enterpriseLogin) auditAction(ctx context.Context, action string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := e.client.Sessions.DB.Exec(ctx, `INSERT INTO audit_events(instance_id,action) VALUES($1,$2)`, e.client.Sessions.InstanceID, action)
	return err
}

// beginSAMLLogout handles POST /auth/logout for sessions issued by the SAML
// provider. It reports whether it wrote the response; when it did not, the
// caller performs the ordinary local logout.
func (e *enterpriseLogin) beginSAMLLogout(w http.ResponseWriter, r *http.Request) bool {
	if e == nil || e.saml == nil || e.client == nil || r.Method != http.MethodPost || !e.saml.SupportsLogout() {
		return false
	}
	if _, err := e.client.AuthenticateRequest(r); err != nil {
		return false // the ordinary path rejects it
	}
	raw := singleCookie(r, oidclogin.SessionCookie)
	ext, err := e.client.Sessions.ExternalSession(r.Context(), raw)
	if err != nil || ext.Issuer != e.samlIssuer {
		return false
	}
	state, browser := browserNonce(), browserNonce()
	post, pending, err := e.saml.BeginLogoutPost(samllogin.Identity{Issuer: ext.Issuer, Subject: ext.Subject, NameIDFormat: ext.NameIDFormat, SessionIndex: ext.SessionIndex}, state)
	var payload []byte
	if err == nil {
		payload, err = json.Marshal(post)
	}
	if err == nil {
		err = e.saveStatePayload(r.Context(), "saml-logout", e.samlIssuer, state, browser, string(payload), pending)
	}
	if err != nil {
		_ = e.auditAction(r.Context(), "auth.saml_logout.failure")
		return false // local logout proceeds without the IdP leg
	}
	// The local session ends regardless of what the IdP does next.
	if err = e.client.Sessions.Revoke(r.Context(), raw); err != nil {
		e.fail(w, r, "saml_logout")
		return true
	}
	e.clearSession(w)
	enterpriseCookie(w, samlLogoutCookie, browser, pending.Expires, http.SameSiteNoneMode)
	browserNoStore(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"saml_logout": samlLogoutPath})
	return true
}

// samlLogoutPage serves the staged LogoutRequest once, to the browser that
// staged it. Anything else is sent back to the console.
func (e *enterpriseLogin) samlLogoutPage(w http.ResponseWriter, r *http.Request) {
	browserNoStore(w)
	if !e.throttle.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	browser := singleCookie(r, samlLogoutCookie)
	if !validBrowserNonce(browser) {
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var payload string
	err := e.client.Sessions.DB.QueryRow(ctx, `WITH staged AS (SELECT state_hash,payload FROM enterprise_login_states WHERE browser_hash=$1 AND instance_id=$2 AND provider=$3 AND kind='saml-logout' AND payload IS NOT NULL AND expires_at>clock_timestamp() FOR UPDATE)
 UPDATE enterprise_login_states s SET payload=NULL FROM staged WHERE s.state_hash=staged.state_hash RETURNING staged.payload`, browserHash(browser), e.client.Sessions.InstanceID, e.samlIssuer).Scan(&payload)
	var post samllogin.LogoutPost
	if err != nil || json.Unmarshal([]byte(payload), &post) != nil || post.SAMLRequest == "" {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			_ = e.auditAction(r.Context(), "auth.saml_logout.failure")
		}
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return
	}
	writePostPage(w, post)
}

// samlSLO is the SP single logout endpoint: it receives the IdP's LogoutResponse
// (SP-initiated flow) or LogoutRequest (IdP-initiated flow) by HTTP-POST.
func (e *enterpriseLogin) samlSLO(w http.ResponseWriter, r *http.Request) {
	browserNoStore(w)
	if !e.throttle.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	origin, _ := url.Parse(e.client.Origin)
	if r.Method != http.MethodPost || r.Host != origin.Host || r.URL.EscapedPath() != "/auth/saml/slo" || r.URL.RawQuery != "" {
		e.fail(w, r, "saml_logout")
		return
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media != "application/x-www-form-urlencoded" {
		e.fail(w, r, "saml_logout")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if r.ParseForm() != nil || len(r.PostForm["SAMLRequest"])+len(r.PostForm["SAMLResponse"]) != 1 || len(r.PostForm["RelayState"]) > 1 {
		e.fail(w, r, "saml_logout")
		return
	}
	for k := range r.PostForm {
		if k != "SAMLRequest" && k != "SAMLResponse" && k != "RelayState" {
			e.fail(w, r, "saml_logout")
			return
		}
	}
	if len(r.PostForm["SAMLResponse"]) == 1 {
		e.samlLogoutResponse(w, r)
		return
	}
	e.samlLogoutRequest(w, r)
}

func (e *enterpriseLogin) samlLogoutResponse(w http.ResponseWriter, r *http.Request) {
	pending, err := e.consumeState(r.Context(), "saml-logout", e.samlIssuer, r.PostForm.Get("RelayState"), singleCookie(r, samlLogoutCookie))
	enterpriseCookie(w, samlLogoutCookie, "", time.Unix(1, 0), http.SameSiteNoneMode)
	if err != nil {
		e.fail(w, r, "saml_logout")
		return
	}
	pending.Logout = true
	raw, err := base64.StdEncoding.DecodeString(r.PostForm.Get("SAMLResponse"))
	if err != nil || len(raw) > 1<<20 || e.saml.ConsumeLogout(r.Context(), raw, pending) != nil {
		e.fail(w, r, "saml_logout")
		return
	}
	_ = e.auditAction(r.Context(), "auth.saml_logout.success")
	http.Redirect(w, r, "/console/", http.StatusSeeOther)
}

func (e *enterpriseLogin) samlLogoutRequest(w http.ResponseWriter, r *http.Request) {
	raw, err := base64.StdEncoding.DecodeString(r.PostForm.Get("SAMLRequest"))
	if err != nil || len(raw) > 1<<20 {
		e.fail(w, r, "saml_logout")
		return
	}
	req, err := e.saml.ConsumeLogoutRequest(r.Context(), raw)
	if err != nil || req.Issuer != e.samlIssuer {
		e.fail(w, r, "saml_logout")
		return
	}
	// Ending zero sessions is still success: the user is logged out either way.
	if _, err = e.client.Sessions.RevokeExternal(r.Context(), req.Issuer, req.NameID, req.SessionIndex); err != nil {
		http.Error(w, "browser authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	relay := r.PostForm.Get("RelayState")
	if len(relay) > 80 {
		relay = ""
	}
	post, err := e.saml.LogoutResponsePost(req.ID, relay)
	if err != nil {
		e.fail(w, r, "saml_logout")
		return
	}
	_ = e.auditAction(r.Context(), "auth.saml_logout.requested_by_idp")
	e.clearSession(w)
	writePostPage(w, post)
}
