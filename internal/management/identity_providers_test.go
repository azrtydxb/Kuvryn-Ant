package management

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/go-ldap/ldap/v3"
)

type idpFixture struct {
	*opsFixture
	reloads int
	oidc    *httptest.Server
	dialOK  bool
}

func newIDPFixture(t *testing.T) *idpFixture {
	o := newOpsFixture(t)
	f := &idpFixture{opsFixture: o, dialOK: true}
	mux := http.NewServeMux()
	Register(mux)
	mux.Handle("/jwks-host/", http.NotFoundHandler())
	f.oidc = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			base := "https://" + r.Host
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "jwks_uri": base + "/jwks",
				"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "1", "n": "AQAB", "e": "AQAB"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.oidc.Close)
	RegisterIdentityProviders(mux, IdentityAdminDeps{
		Pool: o.db, Keys: o.deps.Keys, Origin: "https://ant.example.test", Transport: f.oidc.Client().Transport,
		Reload:        func(context.Context) error { f.reloads++; return nil },
		Active:        func() []ActiveProvider { return []ActiveProvider{{Kind: "oidc", Source: "file"}} },
		FileProviders: []FileProvider{{Kind: "oidc", Label: "https://file.example", Detail: "client x"}},
		Dial: func(context.Context, string, *tls.Config) (directory.Connection, error) {
			if !f.dialOK {
				return nil, directory.ErrUnavailable
			}
			return idpConn{}, nil
		},
	})
	o.handler = WithRequestID(mux)
	return f
}

type idpConn struct{}

func (idpConn) Bind(string, string) error { return nil }
func (idpConn) Search(_ context.Context, r *ldap.SearchRequest) (*ldap.SearchResult, error) {
	return &ldap.SearchResult{Entries: []*ldap.Entry{ldap.NewEntry(r.BaseDN, map[string][]string{"entryUUID": {"1"}})}}, nil
}
func (idpConn) Close() error { return nil }

func ldapBody(name string, enabled bool, secret string) string {
	return fmt.Sprintf(`{"kind":"ldap","name":%q,"enabled":%v,"secret":%q,"config":{"provider_id":"https://ldap.example","url":"ldaps://directory.example","bind_dn":"cn=reader,dc=example","user_base_dn":"ou=users,dc=example","group_base_dn":"ou=groups,dc=example","login_attribute":"uid","user_id_attribute":"entryUUID","group_id_attribute":"entryUUID","user_class":"person","group_class":"groupOfNames","member_attribute":"member","group_roles":[{"group_id":"ZGV2","role_ids":["reader"]}]}}`, name, enabled, secret)
}

func etag(w *httptest.ResponseRecorder) string { return w.Header().Get("ETag") }

func TestIdentityProviderAPIAuthorizationAndSecrets(t *testing.T) {
	f := newIDPFixture(t)
	for _, path := range []string{"/api/v1/identity-providers", "/api/v1/identity-policy", "/api/v1/local-account"} {
		expectSourceStatus(t, f.get(path), 200)
		expectSourceStatus(t, f.requestAs("GET", path, "", "", "", f.limited), 403)
		expectSourceStatus(t, f.requestAs("GET", path, "", "", "", ""), 401)
	}
	expectSourceStatus(t, f.requestAs("POST", "/api/v1/identity-providers", ldapBody("x", false, "pw-pw-pw-pw"), "", "", f.limited), 403)

	w := f.request("POST", "/api/v1/identity-providers", ldapBody("corp", false, "bind-password-xyz"), "", "")
	expectSourceStatus(t, w, 201)
	if strings.Contains(w.Body.String(), "bind-password-xyz") {
		t.Fatal("secret echoed on create")
	}
	created := decodeJSON[IdentityProvider](t, w)
	if !created.HasSecret || created.Enabled || created.Revision != 1 {
		t.Fatalf("%+v", created)
	}
	list := f.get("/api/v1/identity-providers")
	expectSourceStatus(t, list, 200)
	if strings.Contains(list.Body.String(), "bind-password-xyz") {
		t.Fatal("secret in list")
	}
	l := decodeJSON[IdentityProviderList](t, list)
	if len(l.Items) != 1 || len(l.FileItems) != 1 || l.FileItems[0].Label != "https://file.example" || l.Origin != "https://ant.example.test" || l.LocalAccount.Exists {
		t.Fatalf("%+v", l)
	}
	if got := l.Items[0].Config.GroupRoles; len(got) != 1 || got[0].GroupID != "ZGV2" {
		t.Fatal("group mapping not round-tripped")
	}
	// Duplicate names conflict; unknown properties and bad kinds are refused.
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-providers", ldapBody("corp", false, "pw-pw-pw-pw"), "", ""), 409)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-providers", `{"kind":"ldap","name":"z","enabled":false,"config":{},"extra":1}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-providers", `{"kind":"radius","name":"z","enabled":false,"config":{}}`, "", ""), 400)
	// Stale and missing If-Match.
	path := "/api/v1/identity-providers/" + created.ID
	expectSourceStatus(t, f.request("PUT", path, ldapBody("corp", false, ""), "", ""), 428)
	expectSourceStatus(t, f.request("PUT", path, `{"name":"corp","enabled":false,"config":{"provider_id":"https://ldap.example","url":"ldaps://directory.example","bind_dn":"cn=reader,dc=example","user_base_dn":"ou=users,dc=example","group_base_dn":"ou=groups,dc=example","login_attribute":"uid","user_id_attribute":"entryUUID","group_id_attribute":"entryUUID","user_class":"person","group_class":"groupOfNames","member_attribute":"member"}}`, "", `"7"`), 412)

	// Enable swaps and triggers hot reload.
	before := f.reloads
	w = f.request("POST", path+"/enable", "", "", `"1"`)
	expectSourceStatus(t, w, 200)
	if !decodeJSON[IdentityProvider](t, w).Enabled || f.reloads != before+1 {
		t.Fatal("enable did not reload")
	}
	// SSO mandatory needs the break-glass account first.
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":true}`, "", ""), 409)
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":false}`, "", ""), 200)
	expectSourceStatus(t, f.request("DELETE", path, "", "", etag(w)), 204)
	expectSourceStatus(t, f.request("GET", path, "", "", ""), 404)
	if f.reloads != before+2 {
		t.Fatal("delete did not reload")
	}
}

func TestIdentityProviderLDAPDiagnostics(t *testing.T) {
	f := newIDPFixture(t)
	body := func(secret string) string {
		return strings.Replace(ldapBody("t", false, secret), `"enabled":false,`, "", 1)
	}
	req := `{"kind":"ldap","secret":"pw-pw-pw-pw","config":` + strings.SplitN(body("pw-pw-pw-pw"), `"config":`, 2)[1]
	w := f.request("POST", "/api/v1/identity-providers/test", req, "", "")
	expectSourceStatus(t, w, 200)
	res := decodeJSON[IdentityProviderTest](t, w)
	if !res.OK || res.TLSMode != "ldaps" || len(res.Checks) != 4 {
		t.Fatalf("%+v", res)
	}
	f.dialOK = false
	res = decodeJSON[IdentityProviderTest](t, f.request("POST", "/api/v1/identity-providers/test", req, "", ""))
	if res.OK || res.Checks[len(res.Checks)-1].OK {
		t.Fatalf("TLS failure not reported: %+v", res)
	}
	// Diagnostics are audited without the secret.
	var n int
	_ = f.db.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='identity_provider.test' AND details::text NOT LIKE '%pw-pw-pw-pw%'`).Scan(&n)
	if n != 2 {
		t.Fatal("test runs not audited", n)
	}
}

func idpMetadataXML(t *testing.T, entityID string, notAfter time.Time) string {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "idp"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/></IDPSSODescriptor></EntityDescriptor>`, entityID, base64.StdEncoding.EncodeToString(der))
}

func TestIdentityProviderSAMLImportExportAndPreview(t *testing.T) {
	f := newIDPFixture(t)
	xmlDoc := idpMetadataXML(t, "https://idp.example/", time.Now().AddDate(0, 0, 10))
	enc, _ := json.Marshal(map[string]string{"xml": xmlDoc})
	imp := decodeJSON[idpImport](t, f.request("POST", "/api/v1/identity-providers/saml/import-metadata", string(enc), "", ""))
	if !imp.OK || imp.EntityID != "https://idp.example/" || imp.Certificates[0].Status != "expiring" || len(imp.Warnings) == 0 {
		t.Fatalf("%+v", imp)
	}
	cfg, _ := json.Marshal(map[string]any{"kind": "saml", "name": "okta", "enabled": true, "config": map[string]any{"entity_id": "https://ant.example.test/auth/saml/metadata", "idp_entity_id": "https://idp.example/", "idp_metadata_xml": xmlDoc, "groups_attribute": "groups"}})
	w := f.request("POST", "/api/v1/identity-providers", string(cfg), "", "")
	expectSourceStatus(t, w, 201)
	p := decodeJSON[IdentityProvider](t, w)
	if len(p.Certificates) != 2 || p.Config.SPCertificatePEM == nil || strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatalf("SP key material or certificates: %s", w.Body.String())
	}
	meta := f.rawRequest("GET", "/api/v1/identity-providers/"+p.ID+"/metadata", "", f.credential)
	expectSourceStatus(t, meta, 200)
	if !strings.Contains(meta.Body.String(), "https://ant.example.test/auth/saml/callback") || meta.Header().Get("Content-Type") != "application/samlmetadata+xml" {
		t.Fatal("SP metadata export")
	}
	// Preview an assertion whose subject has no binding.
	now := time.Now().UTC()
	assertion := fmt.Sprintf(`<Response xmlns="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion"><saml:Assertion><saml:Issuer>https://idp.example/</saml:Issuer><saml:Subject><saml:NameID>nobody</saml:NameID></saml:Subject><saml:Conditions NotBefore="%s" NotOnOrAfter="%s"><saml:AudienceRestriction><saml:Audience>https://ant.example.test/auth/saml/metadata</saml:Audience></saml:AudienceRestriction></saml:Conditions><saml:AttributeStatement><saml:Attribute Name="groups"><saml:AttributeValue>eng</saml:AttributeValue></saml:Attribute></saml:AttributeStatement></saml:Assertion></Response>`,
		now.Add(-time.Minute).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	enc, _ = json.Marshal(map[string]string{"sample": assertion})
	prev := decodeJSON[MappingPreview](t, f.request("POST", "/api/v1/identity-providers/"+p.ID+"/preview", string(enc), "", ""))
	if prev.WouldSignIn || prev.Binding.Found || len(prev.Identity.Groups) != 1 || !strings.Contains(strings.Join(prev.Problems, "|"), "no identity binding") {
		t.Fatalf("%+v", prev)
	}
	// Rotation changes the SP certificate; expired IdP metadata is rejected (422) and nothing is stored.
	bad, _ := json.Marshal(map[string]any{"name": "okta", "enabled": true, "config": map[string]any{"entity_id": "https://ant.example.test/auth/saml/metadata", "idp_entity_id": "https://idp.example/", "idp_metadata_xml": idpMetadataXML(t, "https://idp.example/", time.Now().Add(-time.Hour))}})
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-providers/"+p.ID, string(bad), "", etag(w)), 422)
}

type idpImport struct {
	OK           bool   `json:"ok"`
	EntityID     string `json:"entity_id"`
	Certificates []struct {
		Status string `json:"status"`
	} `json:"certificates"`
	Warnings []string `json:"warnings"`
}

func TestIdentityProviderOIDCTestCreateAndRejection(t *testing.T) {
	f := newIDPFixture(t)
	issuer := f.oidc.URL
	allowed := fmt.Sprintf(`["%s/authorize","%s/token","%s/jwks"]`, issuer, issuer, issuer)
	// Test endpoint discovers and suggests the endpoints to approve.
	req := fmt.Sprintf(`{"kind":"oidc","config":{"issuer":%q,"client_id":"ant"}}`, issuer)
	res := decodeJSON[IdentityProviderTest](t, f.request("POST", "/api/v1/identity-providers/test", req, "", ""))
	if res.OK || len(res.SuggestedAllowed) != 3 || res.ClockSkewSeconds == nil {
		t.Fatalf("%+v", res)
	}
	req = fmt.Sprintf(`{"kind":"oidc","config":{"issuer":%q,"client_id":"ant","allowed_endpoints":%s}}`, issuer, allowed)
	if res = decodeJSON[IdentityProviderTest](t, f.request("POST", "/api/v1/identity-providers/test", req, "", "")); !res.OK {
		t.Fatalf("%+v", res)
	}
	// Create enabled OIDC; unapproved endpoints are refused with 422 and nothing stored.
	badBody := fmt.Sprintf(`{"kind":"oidc","name":"sso","enabled":true,"secret":"client-secret-xyz","config":{"issuer":%q,"client_id":"ant","allowed_endpoints":["%s/authorize"]}}`, issuer, issuer)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-providers", badBody, "", ""), 422)
	var n int
	_ = f.db.QueryRow(context.Background(), `SELECT count(*) FROM identity_providers`).Scan(&n)
	if n != 0 {
		t.Fatal("rejected provider stored")
	}
	body := fmt.Sprintf(`{"kind":"oidc","name":"sso","enabled":true,"secret":"client-secret-xyz","config":{"issuer":%q,"client_id":"ant","allowed_endpoints":%s}}`, issuer, allowed)
	w := f.request("POST", "/api/v1/identity-providers", body, "", "")
	expectSourceStatus(t, w, 201)
	p := decodeJSON[IdentityProvider](t, w)
	// File config of the same kind shadows the database provider.
	if !p.Enabled || !p.ShadowedByFile {
		t.Fatalf("%+v", p)
	}
	// Preview an ID token: future iat is flagged as clock skew.
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "u1", "aud": "ant", "exp": now.Add(time.Hour).Unix(), "iat": now.Add(10 * time.Minute).Unix(), "groups": []string{"eng"}})
	tok := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
	enc, _ := json.Marshal(map[string]string{"sample": tok})
	prev := decodeJSON[MappingPreview](t, f.request("POST", "/api/v1/identity-providers/"+p.ID+"/preview", string(enc), "", ""))
	if prev.WouldSignIn || prev.ClockSkewSeconds == nil || *prev.ClockSkewSeconds < 500 || prev.SignatureVerified || len(prev.Identity.Groups) != 1 {
		t.Fatalf("%+v", prev)
	}
	// Previews never audit the pasted token.
	_ = f.db.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE details::text LIKE '%e30.%'`).Scan(&n)
	if n != 0 {
		t.Fatal("token material audited")
	}
}

func TestLocalAccountTOTPOnlyByBreakGlassItself(t *testing.T) {
	f := newIDPFixture(t)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-account/totp", "", "", ""), 403)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-account/totp", "", "", ""), 403)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-account/unlock", "", "", ""), 204)
}

var idpKeySeq atomic.Int64

// requestAs adds a fresh Idempotency-Key to mutations that do not name one; key
// "-" sends none so the missing-key contract stays testable.
func (f *idpFixture) requestAs(method, path, body, key, revision, credential string) *httptest.ResponseRecorder {
	f.t.Helper()
	switch {
	case key == "-":
		key = ""
	case key == "" && method != http.MethodGet:
		key = fmt.Sprintf("idp-test-%d", idpKeySeq.Add(1))
	}
	return f.sourceHTTPFixture.requestAs(method, path, body, key, revision, credential)
}
func (f *idpFixture) request(method, path, body, key, revision string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.requestAs(method, path, body, key, revision, f.credential)
}

func TestIdentityAdminMutationsAreIdempotent(t *testing.T) {
	f := newIDPFixture(t)
	count := func(query string) int {
		var n int
		if err := f.db.QueryRow(context.Background(), query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Missing key.
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-providers", ldapBody("nokey", false, "pw-pw-pw-pw"), "-", ""), 400)
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":false}`, "-", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-account/unlock", "", "-", ""), 400)

	// Create replays to the same provider without a second row or audit event.
	body := ldapBody("idem", false, "bind-password-xyz")
	a := f.request("POST", "/api/v1/identity-providers", body, "idp-create", "")
	b := f.request("POST", "/api/v1/identity-providers", body, "idp-create", "")
	expectSourceStatus(t, a, 201)
	expectSourceStatus(t, b, 201)
	if strings.Contains(b.Body.String(), "bind-password-xyz") {
		t.Fatal("secret in replay")
	}
	p := decodeJSON[IdentityProvider](t, a)
	if q := decodeJSON[IdentityProvider](t, b); q.ID != p.ID || etag(a) != etag(b) {
		t.Fatalf("replay differs: %+v", q)
	}
	if n := count(`SELECT count(*) FROM identity_providers`); n != 1 {
		t.Fatalf("providers=%d", n)
	}
	if n := count(`SELECT count(*) FROM mutation_results WHERE response::text LIKE '%bind-password-xyz%'`); n != 0 {
		t.Fatal("secret persisted in replay store")
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-providers", ldapBody("other", false, "pw-pw-pw-pw"), "idp-create", ""), 409)
	reloads := f.reloads

	// Update with If-Match: the replay does not 412 on the now-stale revision.
	upd := ldapBody("idem", true, "")
	u1 := f.request("PUT", "/api/v1/identity-providers/"+p.ID, strings.Replace(upd, `"secret":"",`, "", 1), "idp-upd", etag(a))
	u2 := f.request("PUT", "/api/v1/identity-providers/"+p.ID, strings.Replace(upd, `"secret":"",`, "", 1), "idp-upd", etag(a))
	expectSourceStatus(t, u1, 200)
	expectSourceStatus(t, u2, 200)
	if etag(u1) != etag(u2) || etag(u1) == etag(a) {
		t.Fatalf("etags %s %s %s", etag(a), etag(u1), etag(u2))
	}
	if f.reloads != reloads+1 {
		t.Fatalf("reloads %d -> %d: replay must not reload", reloads, f.reloads)
	}
	// Toggle replays.
	d1 := f.request("POST", "/api/v1/identity-providers/"+p.ID+"/disable", "", "idp-dis", etag(u1))
	d2 := f.request("POST", "/api/v1/identity-providers/"+p.ID+"/disable", "", "idp-dis", etag(u1))
	expectSourceStatus(t, d1, 200)
	expectSourceStatus(t, d2, 200)

	// Policy and local account.
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":false}`, "pol-1", ""), 200)
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":false}`, "pol-1", ""), 200)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-account/unlock", "", "unlock-1", ""), 204)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-account/unlock", "", "unlock-1", ""), 204)
	if n := count(`SELECT count(*) FROM audit_events WHERE action='auth.local.unlocked'`); n != 1 {
		t.Fatalf("unlock audited %d times", n)
	}

	// Delete replays to 204 rather than 404.
	cur := etag(d1)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/identity-providers/"+p.ID, "", "idp-del", cur), 204)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/identity-providers/"+p.ID, "", "idp-del", cur), 204)
	// Authorization is rechecked on replay.
	expectSourceStatus(t, f.requestAs("DELETE", "/api/v1/identity-providers/"+p.ID, "", "idp-del", cur, f.limited), 403)
}

func TestIdentityProviderOIDCClaimRoleMapping(t *testing.T) {
	f := newIDPFixture(t)
	issuer := f.oidc.URL
	allowed := fmt.Sprintf(`["%s/authorize","%s/token","%s/jwks"]`, issuer, issuer, issuer)
	var role string
	if err := f.db.QueryRow(context.Background(), `INSERT INTO roles(instance_id,name) VALUES($1,'ant-reader') RETURNING id::text`, f.instance).Scan(&role); err != nil {
		t.Fatal(err)
	}
	cfg := func(claim, mapping string) string {
		return fmt.Sprintf(`{"kind":"oidc","name":"sso","enabled":false,"secret":"client-secret-xyz","config":{"issuer":%q,"client_id":"ant","allowed_endpoints":%s%s%s}}`, issuer, allowed, claim, mapping)
	}
	claim := `,"roles_claim":"urn:zitadel:iam:org:project:1:roles","scopes":["urn:zitadel:iam:org:projects:roles"]`
	// Unknown role, malformed id, mapping without a claim, and duplicate values are refused.
	for _, bad := range []string{
		cfg(claim, `,"claim_roles":[{"value":"ant-user","role_ids":["00000000-0000-4000-8000-000000000000"]}]`),
		cfg(claim, `,"claim_roles":[{"value":"ant-user","role_ids":["reader"]}]`),
		cfg("", `,"claim_roles":[{"value":"ant-user","role_ids":["`+role+`"]}]`),
		cfg(claim, `,"claim_roles":[{"value":"a","role_ids":["`+role+`"]},{"value":"a","role_ids":["`+role+`"]}]`),
		cfg(claim+`,"scopes":["openid"]`, ""),
	} {
		if w := f.request("POST", "/api/v1/identity-providers", bad, "", ""); w.Code < 400 || w.Code >= 500 {
			t.Fatalf("accepted %s: %d %s", bad, w.Code, w.Body.String())
		}
	}
	w := f.request("POST", "/api/v1/identity-providers", cfg(claim, `,"claim_roles":[{"value":"ant-user","role_ids":["`+role+`"]}]`), "", "")
	expectSourceStatus(t, w, 201)
	got := decodeJSON[IdentityProvider](t, w).Config
	if str(got.RolesClaim) != "urn:zitadel:iam:org:project:1:roles" || len(got.ClaimRoles) != 1 || got.ClaimRoles[0].Value != "ant-user" || got.ClaimRoles[0].RoleIDs[0] != role || len(got.Scopes) != 1 {
		t.Fatalf("mapping not round-tripped: %+v", got)
	}
}
