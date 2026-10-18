package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Method is one client authentication method for package-protocol traffic.
// These are distinct from upstream credentials, which are per remote source.
type Method string

const (
	MethodAnonymous      Method = "anonymous"
	MethodToken          Method = "token"
	MethodBasicDirectory Method = "basic-directory"
	MethodPAT            Method = "pat"
	MethodGitHubOIDC     Method = "github-oidc"
)

// AllMethods lists every method in presentation order.
var AllMethods = []Method{MethodAnonymous, MethodToken, MethodBasicDirectory, MethodPAT, MethodGitHubOIDC}

func ValidMethod(m Method) bool {
	for _, k := range AllMethods {
		if m == k {
			return true
		}
	}
	return false
}

// ClientPolicy says which methods a feed accepts. Anonymous access is further
// limited to AnonymousNetworkGroupIDs when that list is non-empty. Anonymous is
// only ever honored for read (GET/HEAD) of feeds that also have anonymous_read.
type ClientPolicy struct {
	Methods                  []Method
	AnonymousNetworkGroupIDs []string
}

// DefaultClientPolicy applies when no instance default has been stored. It keeps
// pre-policy behaviour: every credential type, and anonymous wherever a feed
// already has anonymous_read.
func DefaultClientPolicy() ClientPolicy {
	return ClientPolicy{Methods: append([]Method(nil), AllMethods...)}
}

func (p ClientPolicy) Allows(m Method) bool {
	for _, k := range p.Methods {
		if k == m {
			return true
		}
	}
	return false
}

// DirectoryVerifier checks a username and password against the configured LDAP
// or Active Directory and returns the immutable issuer and subject used by the
// existing identity bindings. It must return ErrUnavailable for infrastructure
// faults and any other error for bad credentials.
type DirectoryVerifier interface {
	VerifyPassword(ctx context.Context, username string, password []byte) (issuer, subject string, err error)
}

// FeedTarget identifies the feed a package request addresses.
type FeedTarget struct {
	FeedID        string
	AnonymousRead bool
}

type enforcedKey struct{}

// PolicyEnforced reports that the client-authentication middleware already
// decided this request, so adapters must not apply their own anonymous fallback.
func PolicyEnforced(ctx context.Context) bool {
	v, _ := ctx.Value(enforcedKey{}).(bool)
	return v
}

// ClientAuth is the shared package-protocol authentication middleware. It
// enforces the per-feed ClientPolicy for every adapter by verifying the
// credential's method, then handing the adapter an already-verified principal
// through its normal Bearer pipeline.
type ClientAuth struct {
	DB         *pgxpool.Pool
	InstanceID string
	Directory  DirectoryVerifier
	GitHub     *GitHubOIDC
	// ResolveOCI maps a /v2/ request to its feed (the mirror and prefix routing
	// belong to the OCI adapter). Unresolvable requests get the instance default.
	ResolveOCI func(*http.Request) (FeedTarget, bool)
	// TrustedProxies are peers whose X-Forwarded-For is honored for CIDR checks.
	TrustedProxies []netip.Prefix
	// CacheTTL bounds policy and feed lookup staleness (default 5s; negative disables).
	CacheTTL time.Duration
	// DirectoryCacheTTL is the positive username+password cache lifetime (default 60s).
	DirectoryCacheTTL time.Duration

	once     sync.Once
	mu       sync.Mutex
	feeds    map[string]feedEntry
	policies map[string]policyEntry
	dirCache map[[32]byte]dirEntry
	salt     [32]byte
	anon     Principal
	users    *failLimiter
	peers    *failLimiter
	now      func() time.Time
}

type feedEntry struct {
	target FeedTarget
	found  bool
	until  time.Time
}
type policyEntry struct {
	policy   ClientPolicy
	prefixes []netip.Prefix
	until    time.Time
}
type dirEntry struct {
	principal string
	until     time.Time
}

func (c *ClientAuth) init() {
	c.once.Do(func() {
		c.feeds, c.policies, c.dirCache = map[string]feedEntry{}, map[string]policyEntry{}, map[[32]byte]dirEntry{}
		_, _ = rand.Read(c.salt[:])
		c.users = &failLimiter{max: 5, window: 10 * time.Minute, m: map[string]*failEntry{}}
		c.peers = &failLimiter{max: 30, window: time.Minute, m: map[string]*failEntry{}}
		c.now = time.Now
		if c.CacheTTL == 0 {
			c.CacheTTL = 5 * time.Second
		}
		if c.DirectoryCacheTTL == 0 {
			c.DirectoryCacheTTL = 60 * time.Second
		}
	})
}

// Invalidate drops cached feed and policy lookups after an administrative change.
func (c *ClientAuth) Invalidate() {
	c.init()
	c.mu.Lock()
	c.feeds, c.policies = map[string]feedEntry{}, map[string]policyEntry{}
	c.mu.Unlock()
}

type failEntry struct {
	start time.Time
	count int
}

// failLimiter counts failures per key inside a fixed window.
type failLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*failEntry
}

func (l *failLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil {
		return false
	}
	if now.Sub(e.start) >= l.window {
		delete(l.m, key)
		return false
	}
	return e.count >= l.max
}
func (l *failLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil || now.Sub(e.start) >= l.window {
		if e == nil && len(l.m) >= 8192 {
			for k, v := range l.m {
				if now.Sub(v.start) >= l.window {
					delete(l.m, k)
				}
			}
			if len(l.m) >= 8192 {
				return
			}
		}
		e = &failEntry{start: now}
		l.m[key] = e
	}
	e.count++
}
func (l *failLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

// Middleware wraps the full server mux. Only /feeds/{slug}/... and /v2/...
// are package-protocol routes; everything else passes through untouched.
func (c *ClientAuth) Middleware(next http.Handler) http.Handler {
	c.init()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oci := r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/")
		slug := ""
		if !oci {
			rest, ok := strings.CutPrefix(r.URL.Path, "/feeds/")
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			slug, _, _ = strings.Cut(rest, "/")
			if slug == "" {
				next.ServeHTTP(w, r)
				return
			}
		}
		c.handle(w, r, next, oci, slug)
	})
}

func (c *ClientAuth) target(r *http.Request, oci bool, slug string) (FeedTarget, bool, error) {
	if oci {
		if c.ResolveOCI == nil {
			return FeedTarget{}, false, nil
		}
		t, ok := c.ResolveOCI(r)
		return t, ok, nil
	}
	now := c.now()
	c.mu.Lock()
	if e, ok := c.feeds[slug]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.target, e.found, nil
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var t FeedTarget
	err := c.DB.QueryRow(ctx, "SELECT id::text,anonymous_read FROM feeds WHERE instance_id=$1 AND slug=$2", c.InstanceID, slug).Scan(&t.FeedID, &t.AnonymousRead)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return FeedTarget{}, false, ErrUnavailable
	}
	if c.CacheTTL > 0 {
		c.mu.Lock()
		if len(c.feeds) > 4096 {
			c.feeds = map[string]feedEntry{}
		}
		c.feeds[slug] = feedEntry{t, found, now.Add(c.CacheTTL)}
		c.mu.Unlock()
	}
	return t, found, nil
}

// EffectivePolicy returns the feed's policy (or the instance default when the
// feed has none) and the CIDR prefixes anonymous access is limited to. A
// restriction whose groups cannot be resolved yields an unsatisfiable (empty)
// prefix list with restricted=true, so it fails closed.
func (c *ClientAuth) policy(ctx context.Context, feedID string) (ClientPolicy, []netip.Prefix, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.policies[feedID]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.policy, e.prefixes, nil
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p, prefixes, err := LoadEffectivePolicy(ctx, c.DB, c.InstanceID, feedID)
	if err != nil {
		return ClientPolicy{}, nil, err
	}
	if c.CacheTTL > 0 {
		c.mu.Lock()
		if len(c.policies) > 4096 {
			c.policies = map[string]policyEntry{}
		}
		c.policies[feedID] = policyEntry{p, prefixes, now.Add(c.CacheTTL)}
		c.mu.Unlock()
	}
	return p, prefixes, nil
}

// LoadEffectivePolicy resolves the policy for feedID ("" selects the instance
// default). The returned prefixes are meaningful only when the policy has
// AnonymousNetworkGroupIDs; an empty prefix list there means "match nothing".
func LoadEffectivePolicy(ctx context.Context, db *pgxpool.Pool, instance, feedID string) (ClientPolicy, []netip.Prefix, error) {
	var methods []string
	var groups []string
	var err error
	if feedID != "" {
		err = db.QueryRow(ctx, "SELECT methods,anonymous_network_group_ids::text[] FROM client_auth_policies WHERE instance_id=$1 AND feed_id=$2", instance, feedID).Scan(&methods, &groups)
	} else {
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.QueryRow(ctx, "SELECT methods,anonymous_network_group_ids::text[] FROM client_auth_policies WHERE instance_id=$1 AND feed_id IS NULL", instance).Scan(&methods, &groups)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultClientPolicy(), nil, nil
	}
	if err != nil {
		return ClientPolicy{}, nil, ErrUnavailable
	}
	p := ClientPolicy{AnonymousNetworkGroupIDs: groups}
	for _, m := range methods {
		if !ValidMethod(Method(m)) {
			return ClientPolicy{}, nil, ErrUnavailable
		}
		p.Methods = append(p.Methods, Method(m))
	}
	var prefixes []netip.Prefix
	if len(groups) > 0 {
		rows, err := db.Query(ctx, "SELECT unnest(cidrs) FROM network_groups WHERE instance_id=$1 AND id::text=ANY($2)", instance, groups)
		if err != nil {
			return ClientPolicy{}, nil, ErrUnavailable
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if rows.Scan(&s) != nil {
				return ClientPolicy{}, nil, ErrUnavailable
			}
			if pf, perr := netip.ParsePrefix(s); perr == nil {
				prefixes = append(prefixes, pf.Masked())
			}
		}
		if rows.Err() != nil {
			return ClientPolicy{}, nil, ErrUnavailable
		}
	}
	return p, prefixes, nil
}

// ClientIP returns the TCP peer, or the right-most X-Forwarded-For hop that is
// not a trusted proxy when the peer itself is one.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer, err := netip.ParseAddr(peerIP(r.RemoteAddr))
	if err != nil {
		return netip.Addr{}
	}
	in := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	if !in(peer) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer
		}
		a = a.Unmap()
		if !in(a) {
			return a
		}
	}
	return peer
}

func networkAllowed(p ClientPolicy, prefixes []netip.Prefix, ip netip.Addr) bool {
	if len(p.AnonymousNetworkGroupIDs) == 0 {
		return true
	}
	if !ip.IsValid() {
		return false
	}
	for _, pf := range prefixes {
		if pf.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *ClientAuth) anonymousPrincipal(ctx context.Context) (Principal, error) {
	c.mu.Lock()
	if c.anon.PrincipalID != "" {
		p := c.anon
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	p, err := (Roles{DB: c.DB}).EnsureAnonymous(ctx, c.InstanceID)
	if err != nil {
		return Principal{}, err
	}
	c.mu.Lock()
	c.anon = p
	c.mu.Unlock()
	return p, nil
}

func (c *ClientAuth) deny(w http.ResponseWriter, r *http.Request, status int, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kuvryn Ant"`)
	}
	if strings.HasPrefix(r.URL.Path, "/v2") {
		code := "UNAUTHORIZED"
		switch status {
		case http.StatusForbidden:
			code = "DENIED"
		case http.StatusTooManyRequests:
			code = "TOOMANYREQUESTS"
		case http.StatusServiceUnavailable:
			code = "UNKNOWN"
		}
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
		return
	}
	code := api.ErrorCodeUnauthenticated
	switch status {
	case http.StatusForbidden:
		code = api.ErrorCodeForbidden
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		code = api.ErrorCodeUnavailable
	}
	authError(w, r, status, code, message)
}

func parseCredential(header string) (user, secret string, basic, ok bool) {
	if len(header) > 8192 {
		return "", "", false, false
	}
	scheme, value, found := strings.Cut(strings.TrimSpace(header), " ")
	value = strings.TrimSpace(value)
	if !found || value == "" {
		return "", "", false, false
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		if strings.ContainsAny(value, " \t") {
			return "", "", false, false
		}
		return "", value, false, true
	case strings.EqualFold(scheme, "Basic"):
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return "", "", false, false
		}
		u, p, found := strings.Cut(string(raw), ":")
		if !found || u == "" || p == "" {
			return "", "", false, false
		}
		return u, p, true, true
	}
	return "", "", false, false
}

func (c *ClientAuth) handle(w http.ResponseWriter, r *http.Request, next http.Handler, oci bool, slug string) {
	enforced := r.WithContext(context.WithValue(r.Context(), enforcedKey{}, true))
	target, found, err := c.target(r, oci, slug)
	if err != nil {
		c.deny(w, r, http.StatusServiceUnavailable, "authentication unavailable")
		return
	}
	if !oci && !found {
		next.ServeHTTP(w, enforced) // the adapter answers unknown feeds
		return
	}
	feedID := ""
	if found {
		feedID = target.FeedID
	}
	policy, prefixes, err := c.policy(r.Context(), feedID)
	if err != nil {
		c.deny(w, r, http.StatusServiceUnavailable, "authentication unavailable")
		return
	}
	headers := r.Header.Values("Authorization")
	if len(headers) > 1 {
		next.ServeHTTP(w, enforced)
		return
	}
	if len(headers) == 0 {
		// Publish and every non-read request is never anonymous.
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		if !read || feedID == "" || !target.AnonymousRead || !policy.Allows(MethodAnonymous) || !networkAllowed(policy, prefixes, ClientIP(r, c.TrustedProxies)) {
			next.ServeHTTP(w, enforced)
			return
		}
		anon, err := c.anonymousPrincipal(r.Context())
		if err != nil {
			c.deny(w, r, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		c.forward(w, enforced, next, anon)
		return
	}
	user, secret, basic, ok := parseCredential(headers[0])
	if !ok {
		next.ServeHTTP(w, enforced)
		return
	}
	var method Method
	switch {
	case strings.HasPrefix(secret, patPrefix) && len(secret) == patLength:
		method = MethodPAT
	case strings.HasPrefix(secret, "ant_") && len(secret) == 47:
		method = MethodToken
	case LooksLikeJWT(secret):
		method = MethodGitHubOIDC
	case basic:
		method = MethodBasicDirectory
	default:
		next.ServeHTTP(w, enforced)
		return
	}
	if !policy.Allows(method) {
		c.deny(w, r, http.StatusForbidden, "authentication method not permitted for this feed")
		return
	}
	if method == MethodToken {
		next.ServeHTTP(w, enforced) // the adapter's Bearer pipeline verifies it
		return
	}
	now := c.now()
	peer := "p:" + peerIP(r.RemoteAddr)
	if c.peers.blocked(peer, now) {
		w.Header().Set("Retry-After", "60")
		c.deny(w, r, http.StatusTooManyRequests, "authentication attempts limited")
		return
	}
	fail := func() {
		c.peers.fail(peer, c.now())
		c.deny(w, r, http.StatusUnauthorized, "authentication required")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	switch method {
	case MethodPAT:
		p, err := (PATs{DB: c.DB}).Authenticate(ctx, secret)
		if errors.Is(err, ErrUnavailable) {
			c.deny(w, r, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		if err != nil || p.InstanceID != c.InstanceID {
			fail()
			return
		}
		c.forward(w, enforced, next, p)
	case MethodGitHubOIDC:
		if c.GitHub == nil {
			fail()
			return
		}
		raw, err := c.GitHub.tokenForJWT(ctx, secret)
		switch {
		case errors.Is(err, ErrUnavailable):
			c.deny(w, r, http.StatusServiceUnavailable, "authentication unavailable")
			return
		case errors.Is(err, ErrNoTrust):
			c.peers.fail(peer, c.now())
			c.deny(w, r, http.StatusForbidden, "no trust rule matches this workflow")
			return
		case err != nil:
			fail()
			return
		}
		out := enforced.Clone(enforced.Context())
		out.Header.Set("Authorization", "Bearer "+raw)
		next.ServeHTTP(w, out)
	case MethodBasicDirectory:
		if c.Directory == nil || len(user) > 256 || len(secret) > 4096 {
			fail()
			return
		}
		ukey := "u:" + strings.ToLower(user)
		if c.users.blocked(ukey, now) {
			w.Header().Set("Retry-After", "600")
			c.deny(w, r, http.StatusTooManyRequests, "authentication attempts limited")
			return
		}
		principal, err := c.verifyDirectory(ctx, user, secret)
		if errors.Is(err, ErrUnavailable) {
			c.deny(w, r, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		if err != nil {
			c.users.fail(ukey, c.now())
			fail()
			return
		}
		c.users.reset(ukey)
		c.forward(w, enforced, next, Principal{InstanceID: c.InstanceID, PrincipalID: principal})
	}
}

func (c *ClientAuth) verifyDirectory(ctx context.Context, user, password string) (string, error) {
	h := sha256.New()
	h.Write(c.salt[:])
	h.Write([]byte(strconv.Itoa(len(user)) + ":" + user))
	h.Write([]byte(password))
	var key [32]byte
	h.Sum(key[:0])
	now := c.now()
	c.mu.Lock()
	if e, ok := c.dirCache[key]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.principal, nil
	}
	c.mu.Unlock()
	pw := []byte(password)
	defer clear(pw)
	issuer, subject, err := c.Directory.VerifyPassword(ctx, user, pw)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return "", ErrUnavailable
		}
		return "", ErrUnauthenticated
	}
	var principal string
	err = c.DB.QueryRow(ctx, `SELECT b.principal_id::text FROM oidc_identity_bindings b JOIN principals p ON (p.instance_id,p.id)=(b.instance_id,b.principal_id)
 WHERE b.instance_id=$1 AND b.issuer=$2 AND b.subject=$3 AND p.kind='user' AND NOT p.disabled`, c.InstanceID, issuer, subject).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthenticated
	}
	if err != nil {
		return "", ErrUnavailable
	}
	c.mu.Lock()
	if len(c.dirCache) > 2048 {
		c.dirCache = map[[32]byte]dirEntry{}
	}
	c.dirCache[key] = dirEntry{principal, now.Add(c.DirectoryCacheTTL)}
	c.mu.Unlock()
	return principal, nil
}

// forward runs the adapter with the verified principal as a single-use ticket
// presented through the adapter's own Authorization handling.
func (c *ClientAuth) forward(w http.ResponseWriter, r *http.Request, next http.Handler, p Principal) {
	ticket := mintTicket(p)
	defer dropTicket(ticket)
	out := r.Clone(r.Context())
	out.Header.Set("Authorization", "Bearer "+ticket)
	next.ServeHTTP(w, out)
}
