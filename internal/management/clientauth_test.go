package management

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type authFixture struct {
	*opsFixture
	invalidated int
	gh          *identity.GitHubOIDC
	issuerURL   string
	key         *rsa.PrivateKey
}

const authAudience = "https://ant.example.test"

func newAuthFixture(t *testing.T) *authFixture {
	o := newOpsFixture(t)
	f := &authFixture{opsFixture: o}
	f.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.issuerURL, "jwks_uri": f.issuerURL + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "k", Use: "sig", Algorithm: "RS256"}}})
		}
	}))
	t.Cleanup(issuer.Close)
	f.issuerURL = issuer.URL
	f.gh = &identity.GitHubOIDC{DB: o.db, InstanceID: o.instance, Issuer: issuer.URL, AllowInsecure: true}
	mux := http.NewServeMux()
	Register(mux)
	RegisterFeeds(mux, o.db, o.deps.Codec)
	RegisterSources(mux, o.db, o.deps.Codec)
	RegisterIdentities(mux, o.db)
	RegisterClientAuth(mux, ClientAuthDeps{Pool: o.db, GitHub: f.gh, Invalidate: func() { f.invalidated++ }})
	// Stand-in for the browser-session middleware: a trusted in-process principal.
	session := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Test-Session"); id != "" {
			r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{InstanceID: o.instance, PrincipalID: id}))
		}
		mux.ServeHTTP(w, r)
	})
	o.handler = WithRequestID(session)
	return f
}

// as sends a request, optionally as a signed-in user, and validates the response contract.
func (f *authFixture) as(session, method, path, body, credential string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.asKeyed(session, "", method, path, body, credential)
}

// asKeyed is as with an Idempotency-Key header when key is not empty.
func (f *authFixture) asKeyed(session, key, method, path, body, credential string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	if session != "" {
		r.Header.Set("X-Test-Session", session)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	route, params, err := f.router.FindRoute(r)
	if err != nil {
		f.t.Fatal(err)
	}
	in := (&openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route}, Status: w.Code, Header: w.Header()}).SetBodyBytes(w.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		f.t.Fatalf("%s %s response violates OpenAPI: %v; %s", method, path, err, w.Body.String())
	}
	return w
}
func (f *authFixture) admin(method, path, body string) *httptest.ResponseRecorder {
	return f.as("", method, path, body, f.credential)
}

func TestClientAuthPolicyAndNetworkGroupAPI(t *testing.T) {
	f := newAuthFixture(t)
	feed := f.feed("pol-feed", "raw")
	// Defaults: every method, inherited by every feed.
	def := decodeJSON[ClientAuthPolicy](t, f.admin("GET", "/api/v1/auth/policy", ""))
	if len(def.Methods) != 5 || len(def.AnonymousNetworkGroupIDs) != 0 {
		t.Fatalf("default policy: %+v", def)
	}
	if p := decodeJSON[ClientAuthPolicy](t, f.admin("GET", "/api/v1/feeds/"+feed.ID+"/auth-policy", "")); !p.Inherited || p.FeedID != feed.ID {
		t.Fatalf("feed should inherit: %+v", p)
	}
	group := decodeJSON[NetworkGroup](t, mustStatus(t, f.admin("POST", "/api/v1/auth/network-groups", `{"name":"cluster","cidrs":["10.0.0.5/8","fd00::/8","10.0.0.0/8"]}`), 201))
	if len(group.CIDRs) != 2 || group.CIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("cidrs must be masked and deduplicated: %v", group.CIDRs)
	}
	for _, body := range []string{`{"name":"Bad Name","cidrs":["10.0.0.0/8"]}`, `{"name":"x","cidrs":[]}`, `{"name":"x","cidrs":["not-a-cidr"]}`, `{"name":"x","cidrs":["10.0.0.0/8"],"extra":1}`} {
		mustStatus(t, f.admin("POST", "/api/v1/auth/network-groups", body), 400)
	}
	mustStatus(t, f.admin("POST", "/api/v1/auth/network-groups", `{"name":"cluster","cidrs":["10.0.0.0/8"]}`), 409)
	mustStatus(t, f.admin("PUT", "/api/v1/auth/network-groups/"+group.ID, `{"name":"cluster","cidrs":["10.0.0.0/8","192.168.0.0/16"]}`), 200)

	// Feed policy.
	body := fmt.Sprintf(`{"methods":["anonymous","pat"],"anonymous_network_group_ids":[%q]}`, group.ID)
	put := decodeJSON[ClientAuthPolicy](t, mustStatus(t, f.admin("PUT", "/api/v1/feeds/"+feed.ID+"/auth-policy", body), 200))
	if put.Inherited || len(put.Methods) != 2 || put.Revision != 1 {
		t.Fatalf("put policy: %+v", put)
	}
	if f.invalidated == 0 {
		t.Fatal("middleware cache not invalidated")
	}
	got := decodeJSON[ClientAuthPolicy](t, f.admin("GET", "/api/v1/feeds/"+feed.ID+"/auth-policy", ""))
	if got.Inherited || got.AnonymousNetworkGroupIDs[0] != group.ID {
		t.Fatalf("get policy: %+v", got)
	}
	if again := decodeJSON[ClientAuthPolicy](t, f.admin("PUT", "/api/v1/feeds/"+feed.ID+"/auth-policy", body)); again.Revision != 2 {
		t.Fatalf("revision: %d", again.Revision)
	}
	for _, bad := range []string{
		`{"methods":[],"anonymous_network_group_ids":[]}`,
		`{"methods":["ldap"],"anonymous_network_group_ids":[]}`,
		`{"methods":["pat","pat"],"anonymous_network_group_ids":[]}`,
		fmt.Sprintf(`{"methods":["pat"],"anonymous_network_group_ids":[%q]}`, group.ID),
		`{"methods":["anonymous"],"anonymous_network_group_ids":["3f8b8d58-8f0f-4a0f-9e55-4f6f1b0f6d11"]}`,
	} {
		mustStatus(t, f.admin("PUT", "/api/v1/feeds/"+feed.ID+"/auth-policy", bad), 400)
	}
	// A group in use cannot be deleted (it would silently widen anonymous access).
	mustStatus(t, f.admin("DELETE", "/api/v1/auth/network-groups/"+group.ID, ""), 409)
	mustStatus(t, f.admin("DELETE", "/api/v1/feeds/"+feed.ID+"/auth-policy", ""), 204)
	if p := decodeJSON[ClientAuthPolicy](t, f.admin("GET", "/api/v1/feeds/"+feed.ID+"/auth-policy", "")); !p.Inherited {
		t.Fatal("delete should restore inheritance")
	}
	mustStatus(t, f.admin("DELETE", "/api/v1/auth/network-groups/"+group.ID, ""), 204)
	mustStatus(t, f.admin("DELETE", "/api/v1/auth/network-groups/"+group.ID, ""), 404)

	// Instance default.
	mustStatus(t, f.admin("PUT", "/api/v1/auth/policy", `{"methods":["token","github-oidc"],"anonymous_network_group_ids":[]}`), 200)
	if p := decodeJSON[ClientAuthPolicy](t, f.admin("GET", "/api/v1/feeds/"+feed.ID+"/auth-policy", "")); !p.Inherited || len(p.Methods) != 2 {
		t.Fatalf("feed should see the new default: %+v", p)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action LIKE 'auth.%'") < 6 {
		t.Fatal("policy changes were not audited")
	}

	// Authorization: instance admin for global settings, configure on the feed for feed policy.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/auth/policy", ""}, {"PUT", "/api/v1/auth/policy", `{"methods":["token"],"anonymous_network_group_ids":[]}`},
		{"GET", "/api/v1/auth/network-groups", ""}, {"POST", "/api/v1/auth/network-groups", `{"name":"x","cidrs":["10.0.0.0/8"]}`},
		{"GET", "/api/v1/auth/github-oidc/config", ""}, {"GET", "/api/v1/auth/github-oidc/trust-rules", ""},
	} {
		mustStatus(t, f.as("", c.method, c.path, c.body, f.limited), 403)
		mustStatus(t, f.as("", c.method, c.path, c.body, ""), 401)
	}
	mustStatus(t, f.as("", "GET", "/api/v1/feeds/"+feed.ID+"/auth-policy", "", f.limited), 404)
	mustStatus(t, f.as("", "PUT", "/api/v1/feeds/"+feed.ID+"/auth-policy", `{"methods":["anonymous"],"anonymous_network_group_ids":[]}`, f.limited), 404)
}

func mustStatus(t *testing.T, w *httptest.ResponseRecorder, want int) *httptest.ResponseRecorder {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status=%d want=%d: %s", w.Code, want, w.Body.String())
	}
	return w
}

func TestPersonalAccessTokenAPI(t *testing.T) {
	f := newAuthFixture(t)
	alice := createIdentityPrincipal(f.sourceHTTPFixture, "alice", "user")
	bob := createIdentityPrincipal(f.sourceHTTPFixture, "bob", "user")
	create := `{"name":"laptop","scopes":[{"actions":["read","publish"]}],"expires_in_days":30}`
	// Only a signed-in session can manage PATs; machine tokens (service or PAT) cannot.
	mustStatus(t, f.as("", "POST", "/api/v1/auth/personal-access-tokens", create, f.credential), 403)
	mustStatus(t, f.as("", "POST", "/api/v1/auth/personal-access-tokens", create, ""), 401)
	// Creation requires an Idempotency-Key.
	mustStatus(t, f.as(alice.ID, "POST", "/api/v1/auth/personal-access-tokens", create, ""), 400)
	created := decodeJSON[CreatedPersonalAccessToken](t, mustStatus(t, f.asKeyed(alice.ID, "pat-laptop-1", "POST", "/api/v1/auth/personal-access-tokens", create, ""), 201))
	if !strings.HasPrefix(created.Token, "antp_") || created.Name != "laptop" || time.Until(created.ExpiresAt) < 29*24*time.Hour {
		t.Fatalf("created: %+v", created)
	}
	// A replay with the same key never returns the secret again: 409, no token.
	replay := f.asKeyed(alice.ID, "pat-laptop-1", "POST", "/api/v1/auth/personal-access-tokens", create, "")
	mustStatus(t, replay, 409)
	if strings.Contains(replay.Body.String(), "antp_") || strings.Contains(replay.Body.String(), created.Token) {
		t.Fatal("replay leaked a token secret")
	}
	// The same key with a different body is a key conflict, not a second token.
	mustStatus(t, f.asKeyed(alice.ID, "pat-laptop-1", "POST", "/api/v1/auth/personal-access-tokens", `{"name":"other","scopes":[{"actions":["read"]}],"expires_in_days":30}`, ""), 409)
	list := f.as(alice.ID, "GET", "/api/v1/auth/personal-access-tokens", "", "")
	mustStatus(t, list, 200)
	if strings.Contains(list.Body.String(), created.Token) || strings.Contains(list.Body.String(), `"token"`) {
		t.Fatal("secret shown again after creation")
	}
	if page := decodeJSON[PersonalAccessTokenList](t, list); len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("list: %+v", page)
	}
	// Tokens are private to their owner.
	if page := decodeJSON[PersonalAccessTokenList](t, f.as(bob.ID, "GET", "/api/v1/auth/personal-access-tokens", "", "")); len(page.Items) != 0 {
		t.Fatal("tokens leaked across users")
	}
	mustStatus(t, f.as(bob.ID, "DELETE", "/api/v1/auth/personal-access-tokens/"+created.ID, "", ""), 404)
	for _, bad := range []string{
		`{"name":"","scopes":[{"actions":["read"]}],"expires_in_days":30}`,
		`{"name":"x","scopes":[],"expires_in_days":30}`,
		`{"name":"x","scopes":[{"actions":["admin"]}],"expires_in_days":30}`,
		`{"name":"x","scopes":[{"actions":["read"]}],"expires_in_days":0}`,
		`{"name":"x","scopes":[{"actions":["read"]}],"expires_in_days":366}`,
		`{"name":"x","scopes":[{"feed_id":"3f8b8d58-8f0f-4a0f-9e55-4f6f1b0f6d11","actions":["read"]}],"expires_in_days":30}`,
	} {
		mustStatus(t, f.as(alice.ID, "POST", "/api/v1/auth/personal-access-tokens", bad, ""), 400)
	}
	mustStatus(t, f.as(alice.ID, "DELETE", "/api/v1/auth/personal-access-tokens/"+created.ID, "", ""), 204)
	if _, err := (identity.PATs{DB: f.db}).Authenticate(context.Background(), created.Token); err == nil {
		t.Fatal("revoked token still authenticates")
	}
	page := decodeJSON[PersonalAccessTokenList](t, f.as(alice.ID, "GET", "/api/v1/auth/personal-access-tokens", "", ""))
	if page.Items[0].RevokedAt == nil {
		t.Fatal("revocation not listed")
	}
	// A new key creates a distinct token.
	second := decodeJSON[CreatedPersonalAccessToken](t, mustStatus(t, f.asKeyed(alice.ID, "pat-laptop-2", "POST", "/api/v1/auth/personal-access-tokens", create, ""), 201))
	if second.ID == created.ID || second.Token == created.Token {
		t.Fatal("distinct key reused a token")
	}
}

func (f *authFixture) jwt(t *testing.T, claims map[string]any, aud string, exp time.Duration) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k"))
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]any{"repository": "acme/widgets", "repository_owner": "acme", "ref": "refs/heads/main", "workflow": "release"}
	for k, v := range claims {
		all[k] = v
	}
	now := time.Now()
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: f.issuerURL, Subject: "repo:acme/widgets", Audience: jwt.Audience{aud}, NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), Expiry: jwt.NewNumericDate(now.Add(exp))}).Claims(all).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGitHubOIDCTrustRulesAndExchangeAPI(t *testing.T) {
	f := newAuthFixture(t)
	ci := createIdentityPrincipal(f.sourceHTTPFixture, "ci", "service")
	user := createIdentityPrincipal(f.sourceHTTPFixture, "human", "user")
	feed := f.feed("ci-feed", "raw")
	exchange := func(token string) *httptest.ResponseRecorder {
		return f.as("", "POST", "/api/v1/auth/github-oidc/exchange", fmt.Sprintf(`{"token":%q}`, token), "")
	}
	// Disabled until an audience is configured.
	mustStatus(t, exchange(f.jwt(t, nil, authAudience, time.Hour)), 403)
	cfg := decodeJSON[GitHubOIDCConfig](t, mustStatus(t, f.admin("PUT", "/api/v1/auth/github-oidc/config", fmt.Sprintf(`{"audience":%q}`, authAudience)), 200))
	if cfg.Issuer != identity.GitHubIssuer || cfg.Audience != authAudience {
		t.Fatalf("config: %+v", cfg)
	}
	mustStatus(t, f.admin("GET", "/api/v1/auth/github-oidc/config", ""), 200)
	rule := fmt.Sprintf(`{"name":"main-publish","org":"Acme","repo":"widg*","ref":"main","principal_id":%q,"scopes":[{"feed_id":%q,"actions":["read","publish"]}],"ttl_seconds":600}`, ci.ID, feed.ID)
	created := decodeJSON[GitHubTrustRule](t, mustStatus(t, f.admin("POST", "/api/v1/auth/github-oidc/trust-rules", rule), 201))
	if created.Environment != "*" || created.Workflow != "*" || !created.Enabled || created.TTLSeconds != 600 {
		t.Fatalf("rule defaults: %+v", created)
	}
	for _, bad := range []string{
		fmt.Sprintf(`{"name":"x","org":"acme","repo":"*","principal_id":%q,"scopes":[{"actions":["read"]}]}`, user.ID),
		fmt.Sprintf(`{"name":"x","org":"acme","repo":"*","principal_id":%q,"scopes":[{"actions":["admin"]}]}`, ci.ID),
		fmt.Sprintf(`{"name":"x","org":"ac me","repo":"*","principal_id":%q,"scopes":[{"actions":["read"]}]}`, ci.ID),
		fmt.Sprintf(`{"name":"x","org":"acme","repo":"*","principal_id":%q,"scopes":[{"actions":["read"]}],"ttl_seconds":5}`, ci.ID),
		fmt.Sprintf(`{"name":"x","org":"acme","repo":"*","principal_id":%q,"scopes":[{"feed_id":"3f8b8d58-8f0f-4a0f-9e55-4f6f1b0f6d11","actions":["read"]}]}`, ci.ID),
		`{"name":"x","org":"acme","repo":"*","principal_id":"3f8b8d58-8f0f-4a0f-9e55-4f6f1b0f6d11","scopes":[{"actions":["read"]}]}`,
	} {
		mustStatus(t, f.admin("POST", "/api/v1/auth/github-oidc/trust-rules", bad), 400)
	}
	mustStatus(t, f.admin("POST", "/api/v1/auth/github-oidc/trust-rules", rule), 409) // duplicate name
	if list := decodeJSON[GitHubTrustRuleList](t, f.admin("GET", "/api/v1/auth/github-oidc/trust-rules", "")); len(list.Items) != 1 {
		t.Fatalf("rules: %+v", list)
	}

	// Valid exchange, via the body and via Bearer.
	good := f.jwt(t, nil, authAudience, time.Hour)
	out := decodeJSON[GitHubExchangeResponse](t, mustStatus(t, exchange(good), 200))
	if out.TokenType != "Bearer" || !strings.HasPrefix(out.AccessToken, "ant_") || out.TrustRuleID != created.ID || time.Until(out.ExpiresAt) > 10*time.Minute+5*time.Second {
		t.Fatalf("exchange: %+v", out)
	}
	p, err := (identity.Tokens{DB: f.db}).Authenticate(context.Background(), out.AccessToken)
	if err != nil || p.PrincipalID != ci.ID || len(p.Scopes) != 1 || p.Scopes[0].FeedID != feed.ID {
		t.Fatalf("issued token: %+v %v", p, err)
	}
	mustStatus(t, f.as("", "POST", "/api/v1/auth/github-oidc/exchange", "", good), 200) // JWT as Bearer

	// Rejections: wrong audience / expiry / issuer signature => 401; untrusted workflow => 403.
	mustStatus(t, exchange(f.jwt(t, nil, "https://elsewhere", time.Hour)), 401)
	mustStatus(t, exchange(f.jwt(t, nil, authAudience, -time.Hour)), 401)
	mustStatus(t, exchange("eyJhbGciOiJSUzI1NiIsImtpZCI6ImsifQ.e30.AAAA"), 401)
	mustStatus(t, exchange(f.jwt(t, map[string]any{"repository": "acme/other"}, authAudience, time.Hour)), 403)
	mustStatus(t, exchange(f.jwt(t, map[string]any{"ref": "refs/heads/dev"}, authAudience, time.Hour)), 403)
	mustStatus(t, f.as("", "POST", "/api/v1/auth/github-oidc/exchange", `{"token":"x","extra":1}`, ""), 400)

	// Rule edit and delete; a deleted rule stops exchanges immediately.
	mustStatus(t, f.admin("PUT", "/api/v1/auth/github-oidc/trust-rules/"+created.ID, strings.Replace(rule, `"ref":"main"`, `"ref":"release/*"`, 1)), 200)
	mustStatus(t, exchange(good), 403)
	mustStatus(t, f.admin("DELETE", "/api/v1/auth/github-oidc/trust-rules/"+created.ID, ""), 204)
	mustStatus(t, f.admin("DELETE", "/api/v1/auth/github-oidc/trust-rules/"+created.ID, ""), 404)
	mustStatus(t, f.as("", "POST", "/api/v1/auth/github-oidc/trust-rules", rule, f.limited), 403)

	// Failed attempts are rate limited per peer.
	var last int
	for i := 0; i < 25; i++ {
		last = exchange(f.jwt(t, nil, "https://elsewhere", time.Hour)).Code
	}
	if last != 429 {
		t.Fatalf("exchange failures not limited: %d", last)
	}
}
