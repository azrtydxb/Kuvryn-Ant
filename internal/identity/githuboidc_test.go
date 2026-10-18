package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const testAudience = "https://ant.example.com"

// fakeIssuer is a GitHub-like OIDC issuer with a rotatable JWKS.
type fakeIssuer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	keys    map[string]*rsa.PrivateKey
	retired map[string]*rsa.PrivateKey
	fetches int
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	i := &fakeIssuer{keys: map[string]*rsa.PrivateKey{}, retired: map[string]*rsa.PrivateKey{}}
	i.addKey(t, "k1")
	i.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.mu.Lock()
		defer i.mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": i.srv.URL, "jwks_uri": i.srv.URL + "/jwks"})
		case "/jwks":
			i.fetches++
			set := jose.JSONWebKeySet{}
			for kid, k := range i.keys {
				set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Use: "sig", Algorithm: "RS256"})
			}
			_ = json.NewEncoder(w).Encode(set)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(i.srv.Close)
	return i
}
func (i *fakeIssuer) addKey(t *testing.T, kid string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i.mu.Lock()
	i.keys[kid] = k
	i.retired[kid] = k
	i.mu.Unlock()
}
func (i *fakeIssuer) dropKey(kid string) {
	i.mu.Lock()
	delete(i.keys, kid)
	i.mu.Unlock()
}

type tokenOpts struct {
	kid, aud, iss string
	exp           time.Duration
	claims        map[string]any
}

func (i *fakeIssuer) sign(t *testing.T, o tokenOpts) string {
	t.Helper()
	if o.kid == "" {
		o.kid = "k1"
	}
	if o.aud == "" {
		o.aud = testAudience
	}
	if o.iss == "" {
		o.iss = i.srv.URL
	}
	if o.exp == 0 {
		o.exp = 10 * time.Minute
	}
	i.mu.Lock()
	key := i.retired[o.kid]
	i.mu.Unlock()
	if key == nil {
		key = i.retired["k1"]
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", o.kid))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{
		"repository": "acme/widgets", "repository_owner": "acme", "ref": "refs/heads/main", "environment": "prod", "workflow": "release",
	}
	for k, v := range o.claims {
		claims[k] = v
	}
	std := jwt.Claims{Issuer: o.iss, Subject: "repo:acme/widgets:ref:refs/heads/main", Audience: jwt.Audience{o.aud}, IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), Expiry: jwt.NewNumericDate(now.Add(o.exp))}
	raw, err := jwt.Signed(signer).Claims(std).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type githubEnv struct {
	*authEnv
	issuer *fakeIssuer
	gh     *GitHubOIDC
	clock  *time.Duration
	rule   string
}

func newGitHubEnv(t *testing.T) *githubEnv {
	t.Helper()
	e := newAuthEnv(t)
	ctx := context.Background()
	issuer := newFakeIssuer(t)
	offset := new(time.Duration)
	gh := &GitHubOIDC{DB: e.f.pool, InstanceID: e.f.instance, Issuer: issuer.srv.URL, AllowInsecure: true, Now: func() time.Time { return time.Now().Add(*offset) }}
	e.ca.GitHub = gh
	if _, err := e.f.pool.Exec(ctx, "INSERT INTO github_oidc_config(instance_id,audience) VALUES($1,$2)", e.f.instance, testAudience); err != nil {
		t.Fatal(err)
	}
	g := &githubEnv{authEnv: e, issuer: issuer, gh: gh, clock: offset}
	if err := e.f.pool.QueryRow(ctx, `INSERT INTO github_oidc_trust_rules(instance_id,name,org,repo,ref,environment,workflow,principal_id,scopes,ttl_seconds)
 VALUES($1,'releases','Acme','widg*','main','prod','rel*',$2,'[{"actions":["read","publish"]}]',900) RETURNING id`, e.f.instance, e.f.service).Scan(&g.rule); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGitHubOIDCExchangeValidatesEveryClaim(t *testing.T) {
	g := newGitHubEnv(t)
	ctx := context.Background()
	issued, rule, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{}))
	if err != nil || rule.ID != g.rule || !strings.HasPrefix(issued.Secret, "ant_") {
		t.Fatalf("valid token rejected: %v", err)
	}
	if ttl := time.Until(issued.ExpiresAt); ttl > 15*time.Minute+time.Second || ttl < 14*time.Minute {
		t.Fatalf("not short lived: %v", ttl)
	}
	p, err := (Tokens{DB: g.f.pool}).Authenticate(ctx, issued.Secret)
	if err != nil || p.PrincipalID != g.f.service || len(p.Scopes) != 1 || len(p.Scopes[0].Actions) != 2 {
		t.Fatalf("issued token scopes: %+v %v", p, err)
	}
	for name, tc := range map[string]struct {
		tok  string
		want error
	}{
		"wrong repo":    {g.issuer.sign(t, tokenOpts{claims: map[string]any{"repository": "acme/other"}}), ErrNoTrust},
		"wrong org":     {g.issuer.sign(t, tokenOpts{claims: map[string]any{"repository": "evil/widgets", "repository_owner": "evil"}}), ErrNoTrust},
		"wrong branch":  {g.issuer.sign(t, tokenOpts{claims: map[string]any{"ref": "refs/heads/dev"}}), ErrNoTrust},
		"tag not main":  {g.issuer.sign(t, tokenOpts{claims: map[string]any{"ref": "refs/tags/v1"}}), ErrNoTrust},
		"wrong env":     {g.issuer.sign(t, tokenOpts{claims: map[string]any{"environment": "staging"}}), ErrNoTrust},
		"no env":        {g.issuer.sign(t, tokenOpts{claims: map[string]any{"environment": ""}}), ErrNoTrust},
		"wrong flow":    {g.issuer.sign(t, tokenOpts{claims: map[string]any{"workflow": "ci"}}), ErrNoTrust},
		"expired":       {g.issuer.sign(t, tokenOpts{exp: -10 * time.Minute}), ErrUnauthenticated},
		"wrong aud":     {g.issuer.sign(t, tokenOpts{aud: "https://other.example"}), ErrUnauthenticated},
		"wrong issuer":  {g.issuer.sign(t, tokenOpts{iss: "https://evil.example"}), ErrUnauthenticated},
		"owner/repo":    {g.issuer.sign(t, tokenOpts{claims: map[string]any{"repository_owner": "someone"}}), ErrUnauthenticated},
		"missing owner": {g.issuer.sign(t, tokenOpts{claims: map[string]any{"repository_owner": ""}}), ErrUnauthenticated},
		"garbage":       {"eyJhbGciOiJSUzI1NiJ9.e30.AAAA", ErrUnauthenticated},
		"not a jwt":     {"hello", ErrUnauthenticated},
	} {
		if _, _, err := g.gh.Exchange(ctx, tc.tok); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v want %v", name, err, tc.want)
		}
	}
	// Tampered payload: signature no longer matches.
	parts := strings.Split(g.issuer.sign(t, tokenOpts{}), ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), "acme/widgets", "acme/widgetz", 1))
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	if _, _, err := g.gh.Exchange(ctx, strings.Join(parts, ".")); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("tampered payload: %v", err)
	}
	// Algorithm confusion: HS256 signed with the public key bytes, and alg none.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"k1","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + g.issuer.srv.URL + `","aud":"` + testAudience + `","sub":"x","exp":` + itoa(time.Now().Add(time.Hour).Unix()) + `,"repository":"acme/widgets","repository_owner":"acme"}`))
	mac := hmac.New(sha256.New, []byte("public key bytes"))
	mac.Write([]byte(header + "." + body))
	if _, _, err := g.gh.Exchange(ctx, header+"."+body+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil))); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("HS256 accepted: %v", err)
	}
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`))
	if _, _, err := g.gh.Exchange(ctx, none+"."+body+"."); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("alg none accepted: %v", err)
	}
	// A disabled rule or disabled principal grants nothing.
	if _, err := g.f.pool.Exec(ctx, "UPDATE github_oidc_trust_rules SET enabled=false"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{})); !errors.Is(err, ErrNoTrust) {
		t.Errorf("disabled rule matched: %v", err)
	}
	if _, err := g.f.pool.Exec(ctx, "UPDATE github_oidc_trust_rules SET enabled=true"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.pool.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", g.f.service); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{})); !errors.Is(err, ErrNoTrust) {
		t.Errorf("disabled principal minted a token: %v", err)
	}
	// With no audience configured the feature is off.
	if _, err := g.f.pool.Exec(ctx, "UPDATE github_oidc_config SET audience=''"); err != nil {
		t.Fatal(err)
	}
	g.gh.InvalidateConfig()
	if _, _, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{})); !errors.Is(err, ErrGitHubDisabled) {
		t.Errorf("exchange without audience: %v", err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestGitHubOIDCKeyRotation(t *testing.T) {
	g := newGitHubEnv(t)
	ctx := context.Background()
	if _, _, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{})); err != nil {
		t.Fatal(err)
	}
	g.issuer.addKey(t, "k2")
	tok := g.issuer.sign(t, tokenOpts{kid: "k2"})
	// Unknown kid inside the refetch interval is rejected without hammering the issuer.
	before := g.issuer.fetches
	for i := 0; i < 5; i++ {
		if _, _, err := g.gh.Exchange(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("early unknown kid: %v", err)
		}
	}
	if g.issuer.fetches != before {
		t.Fatalf("issuer hammered: %d fetches", g.issuer.fetches-before)
	}
	*g.clock = 31 * time.Second
	if _, _, err := g.gh.Exchange(ctx, tok); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
	// A retired key stops verifying once the JWKS is refreshed.
	g.issuer.dropKey("k1")
	*g.clock = 2 * time.Hour
	if _, _, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{kid: "k1", exp: 5 * time.Hour})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("retired key accepted: %v", err)
	}
	if _, _, err := g.gh.Exchange(ctx, g.issuer.sign(t, tokenOpts{kid: "k2", exp: 5 * time.Hour})); err != nil {
		t.Fatalf("current key rejected: %v", err)
	}
}

func TestGitHubOIDCDirectJWTOnPackageProtocols(t *testing.T) {
	g := newGitHubEnv(t)
	good := g.issuer.sign(t, tokenOpts{})
	path := "/feeds/feed/raw/x"
	g.setPolicy(t, g.f.feed, []string{"github-oidc"})
	for name, h := range map[string]map[string]string{"bearer": bearerHeader(good), "basic": basic("token", good)} {
		w := g.do("PUT", path, lan, h)
		if w.Code != 204 || w.Header().Get("X-Principal") != g.f.service {
			t.Fatalf("%s JWT: %d", name, w.Code)
		}
	}
	// One stored token per distinct JWT, not one per request.
	var n int
	if err := g.f.pool.QueryRow(context.Background(), "SELECT count(*) FROM service_tokens").Scan(&n); err != nil || n != 1 {
		t.Fatalf("tokens minted per request: %d %v", n, err)
	}
	if w := g.do("GET", path, lan, bearerHeader(g.issuer.sign(t, tokenOpts{claims: map[string]any{"repository": "acme/other"}}))); w.Code != 403 {
		t.Fatalf("untrusted repo: %d", w.Code)
	}
	if w := g.do("GET", path, lan, bearerHeader(g.issuer.sign(t, tokenOpts{aud: "x"}))); w.Code != 401 {
		t.Fatalf("wrong audience: %d", w.Code)
	}
	if w := g.do("GET", path, lan, bearerHeader(g.issuer.sign(t, tokenOpts{exp: -time.Hour}))); w.Code != 401 {
		t.Fatalf("expired: %d", w.Code)
	}
	// The method must be enabled for the feed.
	g.setPolicy(t, g.f.feed, []string{"token", "pat"})
	if w := g.do("GET", path, lan, bearerHeader(good)); w.Code != 403 {
		t.Fatalf("policy bypass: %d", w.Code)
	}
	if w := g.do("GET", path, lan, basic("u", good)); w.Code != 403 {
		t.Fatalf("policy bypass via basic: %d", w.Code)
	}
}

func TestGlobMatchAndRuleMatching(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true}, {"*", "anything/at/all", true}, {"a*", "abc", true}, {"a*c", "abc", true}, {"a*c", "abd", false},
		{"?bc", "abc", true}, {"?bc", "bc", false}, {"a*b*c", "aXXbYYc", true}, {"a*b*c", "aXXbYY", false}, {"exact", "exact", true}, {"exact", "exactly", false}, {"", "", true}, {"", "x", false},
		{"release/*", "release/1.2", true}, {"**", "x/y", true},
	} {
		if GlobMatch(c.pattern, c.s) != c.want {
			t.Errorf("GlobMatch(%q,%q)=%v", c.pattern, c.s, !c.want)
		}
	}
	rule := TrustRule{Enabled: true, Org: "Acme", Repo: "widg*", Ref: "release/*", Environment: "*", Workflow: "*"}
	claims := GitHubClaims{Owner: "acme", Repository: "acme/widgets", Ref: "refs/heads/release/1.0"}
	if !rule.Matches(claims) {
		t.Error("branch glob should match")
	}
	claims.Ref = "refs/heads/release"
	if rule.Matches(claims) {
		t.Error("glob must not match bare prefix")
	}
	claims.Ref = "refs/tags/release/1"
	if rule.Matches(claims) {
		t.Error("branch glob matched a tag")
	}
	rule.Ref = "refs/tags/v*"
	claims.Ref = "refs/tags/v1"
	if !rule.Matches(claims) {
		t.Error("full ref glob should match")
	}
	rule.Enabled = false
	if rule.Matches(claims) {
		t.Error("disabled rule matched")
	}
}
