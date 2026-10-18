package identity

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GitHubIssuer is the issuer of GitHub Actions OIDC tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

var (
	ErrGitHubDisabled = errors.New("github oidc audience is not configured")
	ErrNoTrust        = errors.New("no github oidc trust rule matches")
)

// GitHubClaims are the validated claims trust rules are evaluated against.
type GitHubClaims struct {
	Subject, Repository, Owner, Ref, Environment, Workflow string
}

// TrustRule maps a class of GitHub workflows to a scoped, short-lived Ant
// token. Authority is the intersection of the service principal's role grants
// and Scopes, so a rule can never exceed what its principal already holds.
type TrustRule struct {
	ID, Name, Org, Repo, Ref, Environment, Workflow, PrincipalID string
	Scopes                                                       []Scope
	TTLSeconds                                                   int
	Enabled                                                      bool
	Revision                                                     int64
	CreatedAt, UpdatedAt                                         time.Time
}

// GitHubOIDC validates GitHub Actions OIDC JWTs and exchanges them for Ant
// service tokens. It is safe for concurrent use.
type GitHubOIDC struct {
	DB         *pgxpool.Pool
	InstanceID string
	// Issuer defaults to GitHubIssuer. AllowInsecure permits http issuers and is
	// for tests only.
	Issuer        string
	AllowInsecure bool
	HTTP          *http.Client
	Now           func() time.Time

	mu        sync.Mutex
	keys      jose.JSONWebKeySet
	keysAt    time.Time
	lastFetch time.Time
	audience  string
	audienceT time.Time
	exchanged map[[32]byte]cachedExchange
}

type cachedExchange struct {
	raw     string
	expires time.Time
}

const (
	jwksMaxAge     = time.Hour
	jwksMinRefetch = 30 * time.Second
	githubLeeway   = 60 * time.Second
)

func (g *GitHubOIDC) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}
func (g *GitHubOIDC) issuer() string {
	if g.Issuer != "" {
		return g.Issuer
	}
	return GitHubIssuer
}
func (g *GitHubOIDC) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// LooksLikeJWT reports whether a credential has compact-JWS shape.
func LooksLikeJWT(s string) bool {
	return len(s) > 20 && len(s) <= 8192 && strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2
}

func (g *GitHubOIDC) fetchJSON(ctx context.Context, target string, out any) error {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(g.AllowInsecure && u.Scheme == "http")) {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ErrUnavailable
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(body, out) != nil {
		return ErrUnavailable
	}
	return nil
}

// refreshKeys fetches discovery and JWKS. Callers hold g.mu.
func (g *GitHubOIDC) refreshKeys(ctx context.Context) error {
	issuer := g.issuer()
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := g.fetchJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &discovery); err != nil {
		return err
	}
	base, e1 := url.Parse(issuer)
	jwks, e2 := url.Parse(discovery.JWKSURI)
	if e1 != nil || e2 != nil || discovery.Issuer != issuer || jwks.Host != base.Host {
		return ErrUnavailable
	}
	var set jose.JSONWebKeySet
	if err := g.fetchJSON(ctx, discovery.JWKSURI, &set); err != nil {
		return err
	}
	g.keys, g.keysAt, g.lastFetch = set, g.now(), g.now()
	return nil
}

// key returns the RSA verification key for kid. An unknown kid triggers at most
// one refetch per jwksMinRefetch so key rotation works without enabling abuse.
func (g *GitHubOIDC) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lookup := func() *rsa.PublicKey {
		for _, k := range g.keys.Key(kid) {
			if pub, ok := k.Key.(*rsa.PublicKey); ok && k.Valid() && (k.Use == "" || k.Use == "sig") && (k.Algorithm == "" || k.Algorithm == string(jose.RS256)) {
				return pub
			}
		}
		return nil
	}
	stale := g.keysAt.IsZero() || g.now().Sub(g.keysAt) > jwksMaxAge
	if !stale {
		if pub := lookup(); pub != nil {
			return pub, nil
		}
	}
	if stale || g.now().Sub(g.lastFetch) >= jwksMinRefetch {
		g.lastFetch = g.now()
		if err := g.refreshKeys(ctx); err != nil {
			return nil, err
		}
		if pub := lookup(); pub != nil {
			return pub, nil
		}
	}
	return nil, ErrUnauthenticated
}

func (g *GitHubOIDC) configuredAudience(ctx context.Context) (string, error) {
	g.mu.Lock()
	if g.audience != "" && g.now().Sub(g.audienceT) < 5*time.Second {
		a := g.audience
		g.mu.Unlock()
		return a, nil
	}
	g.mu.Unlock()
	var aud string
	err := g.DB.QueryRow(ctx, "SELECT audience FROM github_oidc_config WHERE instance_id=$1", g.InstanceID).Scan(&aud)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrGitHubDisabled
	}
	if err != nil {
		return "", ErrUnavailable
	}
	if aud == "" {
		return "", ErrGitHubDisabled
	}
	g.mu.Lock()
	g.audience, g.audienceT = aud, g.now()
	g.mu.Unlock()
	return aud, nil
}

// InvalidateConfig drops cached audience (call after changing it).
func (g *GitHubOIDC) InvalidateConfig() {
	g.mu.Lock()
	g.audience = ""
	g.mu.Unlock()
}

// Verify validates signature (RS256 only), issuer, audience, expiry and
// not-before of a GitHub Actions token. Every failure other than infrastructure
// faults is ErrUnauthenticated.
func (g *GitHubOIDC) Verify(ctx context.Context, raw string) (GitHubClaims, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if !LooksLikeJWT(raw) {
		return GitHubClaims{}, ErrUnauthenticated
	}
	audience, err := g.configuredAudience(ctx)
	if err != nil {
		return GitHubClaims{}, err
	}
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(tok.Headers) != 1 || tok.Headers[0].KeyID == "" {
		return GitHubClaims{}, ErrUnauthenticated
	}
	pub, err := g.key(ctx, tok.Headers[0].KeyID)
	if err != nil {
		return GitHubClaims{}, err
	}
	var std jwt.Claims
	var extra struct {
		Repository  string `json:"repository"`
		Owner       string `json:"repository_owner"`
		Ref         string `json:"ref"`
		Environment string `json:"environment"`
		Workflow    string `json:"workflow"`
	}
	if tok.Claims(pub, &std, &extra) != nil {
		return GitHubClaims{}, ErrUnauthenticated
	}
	if std.Expiry == nil || std.Subject == "" {
		return GitHubClaims{}, ErrUnauthenticated
	}
	if std.ValidateWithLeeway(jwt.Expected{Issuer: g.issuer(), AnyAudience: jwt.Audience{audience}, Time: g.now()}, githubLeeway) != nil {
		return GitHubClaims{}, ErrUnauthenticated
	}
	// Both owner and repository must be present and consistent.
	if extra.Owner == "" || extra.Repository == "" || !strings.HasPrefix(strings.ToLower(extra.Repository), strings.ToLower(extra.Owner)+"/") {
		return GitHubClaims{}, ErrUnauthenticated
	}
	return GitHubClaims{Subject: std.Subject, Repository: extra.Repository, Owner: extra.Owner, Ref: extra.Ref, Environment: extra.Environment, Workflow: extra.Workflow}, nil
}

// GlobMatch matches * (any run, including /) and ? (one character).
func GlobMatch(pattern, s string) bool {
	p, i, star, mark := 0, 0, -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// Matches evaluates the rule against validated claims. Fields are conjunctive.
func (r TrustRule) Matches(c GitHubClaims) bool {
	if !r.Enabled || !strings.EqualFold(r.Org, c.Owner) {
		return false
	}
	repo := c.Repository[len(c.Owner)+1:]
	ref := r.Ref
	if ref != "*" && !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	return GlobMatch(r.Repo, repo) && GlobMatch(ref, c.Ref) && GlobMatch(r.Environment, c.Environment) && GlobMatch(r.Workflow, c.Workflow)
}

func (g *GitHubOIDC) rules(ctx context.Context) ([]TrustRule, error) {
	rows, err := g.DB.Query(ctx, "SELECT id,name,org,repo,ref,environment,workflow,principal_id,scopes,ttl_seconds,enabled FROM github_oidc_trust_rules WHERE instance_id=$1 AND enabled ORDER BY created_at,id", g.InstanceID)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	var out []TrustRule
	for rows.Next() {
		var r TrustRule
		var scopes []byte
		if rows.Scan(&r.ID, &r.Name, &r.Org, &r.Repo, &r.Ref, &r.Environment, &r.Workflow, &r.PrincipalID, &scopes, &r.TTLSeconds, &r.Enabled) != nil || json.Unmarshal(scopes, &r.Scopes) != nil {
			return nil, ErrUnavailable
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return out, nil
}

// Exchange validates the JWT, selects the first enabled trust rule (oldest
// first) that matches, and issues a short-lived scoped service token.
func (g *GitHubOIDC) Exchange(ctx context.Context, raw string) (IssuedToken, TrustRule, error) {
	claims, err := g.Verify(ctx, raw)
	if err != nil {
		return IssuedToken{}, TrustRule{}, err
	}
	rules, err := g.rules(ctx)
	if err != nil {
		return IssuedToken{}, TrustRule{}, err
	}
	for _, rule := range rules {
		if !rule.Matches(claims) {
			continue
		}
		issued, err := (Tokens{DB: g.DB}).Issue(ctx, g.InstanceID, rule.PrincipalID, rule.PrincipalID, rule.Scopes, g.now().Add(time.Duration(rule.TTLSeconds)*time.Second))
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrInvalidScope) {
				return IssuedToken{}, TrustRule{}, ErrNoTrust
			}
			return IssuedToken{}, TrustRule{}, err
		}
		return issued, rule, nil
	}
	return IssuedToken{}, TrustRule{}, ErrNoTrust
}

// tokenForJWT backs direct use of the OIDC JWT on package protocols: it
// exchanges once per distinct JWT and reuses the issued token until shortly
// before it expires, bounding stored tokens to one per CI job.
func (g *GitHubOIDC) tokenForJWT(ctx context.Context, raw string) (string, error) {
	key := sha256.Sum256([]byte(raw))
	g.mu.Lock()
	if e, ok := g.exchanged[key]; ok && g.now().Add(30*time.Second).Before(e.expires) {
		g.mu.Unlock()
		return e.raw, nil
	}
	g.mu.Unlock()
	issued, _, err := g.Exchange(ctx, raw)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	if g.exchanged == nil || len(g.exchanged) > 2048 {
		g.exchanged = map[[32]byte]cachedExchange{}
	}
	g.exchanged[key] = cachedExchange{issued.Secret, issued.ExpiresAt}
	g.mu.Unlock()
	return issued.Secret, nil
}

// NormalizeScopes canonicalizes feed UUID spelling in scopes.
func NormalizeScopes(in []Scope) ([]Scope, error) { return normalizeScopes(in) }
