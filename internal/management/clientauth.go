package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClientAuthDeps wires the client-authentication management API.
type ClientAuthDeps struct {
	Pool *pgxpool.Pool
	// GitHub validates and exchanges GitHub Actions OIDC tokens. Optional.
	GitHub *identity.GitHubOIDC
	// Invalidate drops the package middleware's policy caches after a change.
	Invalidate func()
}

type authHandlers struct {
	identityHandlers
	d       ClientAuthDeps
	limiter *exchangeLimiter
}

// ClientAuthPolicy is the effective policy of a feed (or the instance default
// when FeedID is empty). Inherited is true when a feed has no policy of its own.
type ClientAuthPolicy struct {
	FeedID                   string   `json:"feed_id,omitempty"`
	Methods                  []string `json:"methods"`
	AnonymousNetworkGroupIDs []string `json:"anonymous_network_group_ids"`
	Inherited                bool     `json:"inherited"`
	Revision                 int64    `json:"revision"`
}
type PutClientAuthPolicy struct {
	Methods                  []string `json:"methods"`
	AnonymousNetworkGroupIDs []string `json:"anonymous_network_group_ids"`
}
type NetworkGroup struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	CIDRs    []string `json:"cidrs"`
	Revision int64    `json:"revision"`
}
type NetworkGroupInput struct {
	Name  string   `json:"name"`
	CIDRs []string `json:"cidrs"`
}
type NetworkGroupList struct {
	Items []NetworkGroup `json:"items"`
}
type GitHubOIDCConfig struct {
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	Revision int64  `json:"revision"`
}
type PutGitHubOIDCConfig struct {
	Audience string `json:"audience"`
}
type TokenScope struct {
	FeedID    string   `json:"feed_id,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Actions   []string `json:"actions"`
}
type GitHubTrustRule struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Org         string       `json:"org"`
	Repo        string       `json:"repo"`
	Ref         string       `json:"ref"`
	Environment string       `json:"environment"`
	Workflow    string       `json:"workflow"`
	PrincipalID string       `json:"principal_id"`
	Scopes      []TokenScope `json:"scopes"`
	TTLSeconds  int          `json:"ttl_seconds"`
	Enabled     bool         `json:"enabled"`
	Revision    int64        `json:"revision"`
}
type GitHubTrustRuleInput struct {
	Name        string       `json:"name"`
	Org         string       `json:"org"`
	Repo        string       `json:"repo"`
	Ref         string       `json:"ref,omitempty"`
	Environment string       `json:"environment,omitempty"`
	Workflow    string       `json:"workflow,omitempty"`
	PrincipalID string       `json:"principal_id"`
	Scopes      []TokenScope `json:"scopes"`
	TTLSeconds  int          `json:"ttl_seconds,omitempty"`
	Enabled     *bool        `json:"enabled,omitempty"`
}
type GitHubTrustRuleList struct {
	Items []GitHubTrustRule `json:"items"`
}
type GitHubExchangeRequest struct {
	Token string `json:"token"`
}
type GitHubExchangeResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
	TrustRuleID string    `json:"trust_rule_id"`
}
type PersonalAccessToken struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Scopes     []TokenScope `json:"scopes"`
	CreatedAt  time.Time    `json:"created_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	LastUsedAt *time.Time   `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time   `json:"revoked_at,omitempty"`
}
type PersonalAccessTokenList struct {
	Items []PersonalAccessToken `json:"items"`
}
type CreatePersonalAccessToken struct {
	Name          string       `json:"name"`
	Scopes        []TokenScope `json:"scopes"`
	ExpiresInDays int          `json:"expires_in_days"`
}

// CreatedPersonalAccessToken carries the secret exactly once.
type CreatedPersonalAccessToken struct {
	PersonalAccessToken
	Token string `json:"token"`
}

// exchangeLimiter bounds failed unauthenticated exchange attempts per peer.
type exchangeLimiter struct {
	mu sync.Mutex
	m  map[string]*struct {
		start time.Time
		n     int
	}
}

func (l *exchangeLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil {
		return false
	}
	if time.Since(e.start) >= time.Minute {
		delete(l.m, key)
		return false
	}
	return e.n >= 20
}
func (l *exchangeLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil || len(l.m) > 8192 {
		l.m = map[string]*struct {
			start time.Time
			n     int
		}{}
	}
	e := l.m[key]
	if e == nil || time.Since(e.start) >= time.Minute {
		e = &struct {
			start time.Time
			n     int
		}{start: time.Now()}
		l.m[key] = e
	}
	e.n++
}

// RegisterClientAuth mounts the client-authentication policy, network group,
// personal access token and GitHub OIDC APIs. Only the GitHub exchange route is
// unauthenticated: its credential is the OIDC JWT itself.
func RegisterClientAuth(mux *http.ServeMux, d ClientAuthDeps) {
	h := authHandlers{identityHandlers{feedHandlers{db: d.Pool}}, d, &exchangeLimiter{}}
	bearer := identity.NewBearer(identity.Tokens{DB: d.Pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/auth/policy":                          h.getInstancePolicy,
		"PUT /api/v1/auth/policy":                          h.putInstancePolicy,
		"GET /api/v1/feeds/{id}/auth-policy":               h.getFeedPolicy,
		"PUT /api/v1/feeds/{id}/auth-policy":               h.putFeedPolicy,
		"DELETE /api/v1/feeds/{id}/auth-policy":            h.deleteFeedPolicy,
		"GET /api/v1/auth/network-groups":                  h.listNetworkGroups,
		"POST /api/v1/auth/network-groups":                 h.createNetworkGroup,
		"PUT /api/v1/auth/network-groups/{id}":             h.putNetworkGroup,
		"DELETE /api/v1/auth/network-groups/{id}":          h.deleteNetworkGroup,
		"GET /api/v1/auth/github-oidc/config":              h.getGitHubConfig,
		"PUT /api/v1/auth/github-oidc/config":              h.putGitHubConfig,
		"GET /api/v1/auth/github-oidc/trust-rules":         h.listTrustRules,
		"POST /api/v1/auth/github-oidc/trust-rules":        h.createTrustRule,
		"PUT /api/v1/auth/github-oidc/trust-rules/{id}":    h.putTrustRule,
		"DELETE /api/v1/auth/github-oidc/trust-rules/{id}": h.deleteTrustRule,
		"GET /api/v1/auth/personal-access-tokens":          h.listPATs,
		"POST /api/v1/auth/personal-access-tokens":         h.createPAT,
		"DELETE /api/v1/auth/personal-access-tokens/{id}":  h.revokePAT,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
	mux.HandleFunc("POST /api/v1/auth/github-oidc/exchange", h.exchange)
}

func (h authHandlers) changed() {
	if h.d.Invalidate != nil {
		h.d.Invalidate()
	}
}
func authRollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func (h authHandlers) tx(w http.ResponseWriter, r *http.Request) (pgx.Tx, context.Context, context.CancelFunc, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	tx, err := h.db.Begin(ctx)
	if err != nil {
		cancel()
		identityError(w, r, err)
		return nil, nil, nil, false
	}
	return tx, ctx, cancel, true
}
func bad(w http.ResponseWriter, r *http.Request, what string) {
	Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid "+what)
}
func uuidText(raw string) (string, bool) {
	var id pgtype.UUID
	if id.Scan(raw) != nil || !id.Valid {
		return "", false
	}
	return id.String(), true
}

// --- policies ---

func validateMethods(in PutClientAuthPolicy) (PutClientAuthPolicy, bool) {
	if len(in.Methods) == 0 || len(in.Methods) > len(identity.AllMethods) || len(in.AnonymousNetworkGroupIDs) > 32 {
		return in, false
	}
	seen := map[string]bool{}
	for _, m := range in.Methods {
		if !identity.ValidMethod(identity.Method(m)) || seen[m] {
			return in, false
		}
		seen[m] = true
	}
	groups := make([]string, 0, len(in.AnonymousNetworkGroupIDs))
	seenGroup := map[string]bool{}
	for _, g := range in.AnonymousNetworkGroupIDs {
		id, ok := uuidText(g)
		if !ok || seenGroup[id] {
			return in, false
		}
		seenGroup[id] = true
		groups = append(groups, id)
	}
	if len(groups) > 0 && !seen[string(identity.MethodAnonymous)] {
		return in, false
	}
	in.AnonymousNetworkGroupIDs = groups
	return in, true
}

func (h authHandlers) loadPolicy(ctx context.Context, instance, feed string) (ClientAuthPolicy, error) {
	out := ClientAuthPolicy{FeedID: feed, Revision: 1}
	var methods, groups []string
	scan := func(feedClause string, args ...any) error {
		return h.db.QueryRow(ctx, "SELECT methods,anonymous_network_group_ids::text[],revision FROM client_auth_policies WHERE instance_id=$1 AND "+feedClause, args...).Scan(&methods, &groups, &out.Revision)
	}
	var err error
	if feed != "" {
		err = scan("feed_id=$2", instance, feed)
		if errors.Is(err, pgx.ErrNoRows) {
			out.Inherited = true
			err = scan("feed_id IS NULL", instance)
		}
	} else {
		err = scan("feed_id IS NULL", instance)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		for _, m := range identity.DefaultClientPolicy().Methods {
			methods = append(methods, string(m))
		}
		groups, out.Revision, err = nil, 1, nil
	}
	if err != nil {
		return out, err
	}
	out.Methods, out.AnonymousNetworkGroupIDs = methods, groups
	if out.AnonymousNetworkGroupIDs == nil {
		out.AnonymousNetworkGroupIDs = []string{}
	}
	return out, nil
}

func (h authHandlers) getInstancePolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	out, err := h.loadPolicy(r.Context(), p.InstanceID, "")
	if err != nil {
		identityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}
func (h authHandlers) putInstancePolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	h.writePolicy(w, r, p, "")
}

// feedConfigure authorizes feed policy access (Configure on the feed).
func (h authHandlers) feedConfigure(w http.ResponseWriter, r *http.Request) (identity.Principal, string, bool) {
	return h.opsFeed(w, r, identity.Configure)
}
func (h authHandlers) opsFeed(w http.ResponseWriter, r *http.Request, action identity.Action) (identity.Principal, string, bool) {
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		bad(w, r, "feed ID")
		return identity.Principal{}, "", false
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return p, "", false
	}
	if !feedAllowed(policy, p, id, action) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return p, "", false
	}
	return p, id, true
}
func (h authHandlers) getFeedPolicy(w http.ResponseWriter, r *http.Request) {
	p, feed, ok := h.feedConfigure(w, r)
	if !ok {
		return
	}
	out, err := h.loadPolicy(r.Context(), p.InstanceID, feed)
	if err != nil {
		identityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}
func (h authHandlers) putFeedPolicy(w http.ResponseWriter, r *http.Request) {
	p, feed, ok := h.feedConfigure(w, r)
	if !ok {
		return
	}
	h.writePolicy(w, r, p, feed)
}
func (h authHandlers) writePolicy(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	var in PutClientAuthPolicy
	if !sourceJSON(w, r, &in, "methods", "anonymous_network_group_ids") {
		return
	}
	in, valid := validateMethods(in)
	if !valid {
		bad(w, r, "client authentication policy")
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	var found int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM network_groups WHERE instance_id=$1 AND id::text=ANY($2)", p.InstanceID, in.AnonymousNetworkGroupIDs).Scan(&found); err != nil {
		identityError(w, r, err)
		return
	}
	if found != len(in.AnonymousNetworkGroupIDs) {
		bad(w, r, "network group")
		return
	}
	var revision int64
	var id string
	var err error
	if feed != "" {
		err = tx.QueryRow(ctx, `INSERT INTO client_auth_policies(instance_id,feed_id,methods,anonymous_network_group_ids) VALUES($1,$2,$3,$4::uuid[])
 ON CONFLICT(instance_id,feed_id) WHERE feed_id IS NOT NULL DO UPDATE SET methods=excluded.methods,anonymous_network_group_ids=excluded.anonymous_network_group_ids,revision=client_auth_policies.revision+1,updated_at=now() RETURNING revision,id::text`, p.InstanceID, feed, in.Methods, in.AnonymousNetworkGroupIDs).Scan(&revision, &id)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO client_auth_policies(instance_id,methods,anonymous_network_group_ids) VALUES($1,$2,$3::uuid[])
 ON CONFLICT(instance_id) WHERE feed_id IS NULL DO UPDATE SET methods=excluded.methods,anonymous_network_group_ids=excluded.anonymous_network_group_ids,revision=client_auth_policies.revision+1,updated_at=now() RETURNING revision,id::text`, p.InstanceID, in.Methods, in.AnonymousNetworkGroupIDs).Scan(&revision, &id)
	}
	if err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	if err = identityEvent(ctx, tx, p, id, "auth.policy.put", revision); err != nil {
		identityError(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	h.changed()
	JSON(w, 200, ClientAuthPolicy{FeedID: feed, Methods: in.Methods, AnonymousNetworkGroupIDs: in.AnonymousNetworkGroupIDs, Revision: revision})
}
func (h authHandlers) deleteFeedPolicy(w http.ResponseWriter, r *http.Request) {
	p, feed, ok := h.feedConfigure(w, r)
	if !ok {
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	var id string
	err := tx.QueryRow(ctx, "DELETE FROM client_auth_policies WHERE instance_id=$1 AND feed_id=$2 RETURNING id::text", p.InstanceID, feed).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		identityError(w, r, err)
		return
	}
	if err == nil {
		if err = identityEvent(ctx, tx, p, id, "auth.policy.delete", 0); err != nil {
			identityError(w, r, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	h.changed()
	w.WriteHeader(204)
}

// --- network groups ---

var groupName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func validateGroup(in NetworkGroupInput) (NetworkGroupInput, bool) {
	if !groupName.MatchString(in.Name) || len(in.CIDRs) == 0 || len(in.CIDRs) > 64 {
		return in, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in.CIDRs))
	for _, c := range in.CIDRs {
		pf, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return in, false
		}
		s := pf.Masked().String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	in.CIDRs = out
	return in, true
}
func (h authHandlers) listNetworkGroups(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	rows, err := h.db.Query(r.Context(), "SELECT id::text,name,cidrs,revision FROM network_groups WHERE instance_id=$1 ORDER BY name LIMIT 500", p.InstanceID)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := NetworkGroupList{Items: []NetworkGroup{}}
	for rows.Next() {
		var g NetworkGroup
		if err = rows.Scan(&g.ID, &g.Name, &g.CIDRs, &g.Revision); err != nil {
			identityError(w, r, err)
			return
		}
		out.Items = append(out.Items, g)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	JSON(w, 200, out)
}
func (h authHandlers) createNetworkGroup(w http.ResponseWriter, r *http.Request) {
	h.saveNetworkGroup(w, r, "")
}
func (h authHandlers) putNetworkGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := uuidText(r.PathValue("id"))
	if !ok {
		bad(w, r, "network group ID")
		return
	}
	h.saveNetworkGroup(w, r, id)
}
func (h authHandlers) saveNetworkGroup(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in NetworkGroupInput
	if !sourceJSON(w, r, &in, "name", "cidrs") {
		return
	}
	in, valid := validateGroup(in)
	if !valid {
		bad(w, r, "network group")
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	g := NetworkGroup{Name: in.Name, CIDRs: in.CIDRs}
	var err error
	status := 201
	action := "auth.network_group.create"
	if id == "" {
		err = tx.QueryRow(ctx, "INSERT INTO network_groups(instance_id,name,cidrs) VALUES($1,$2,$3) RETURNING id::text,revision", p.InstanceID, in.Name, in.CIDRs).Scan(&g.ID, &g.Revision)
	} else {
		status, action = 200, "auth.network_group.put"
		err = tx.QueryRow(ctx, "UPDATE network_groups SET name=$3,cidrs=$4,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2 RETURNING id::text,revision", p.InstanceID, id, in.Name, in.CIDRs).Scan(&g.ID, &g.Revision)
	}
	if err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	if err = identityEvent(ctx, tx, p, g.ID, action, g.Revision); err != nil {
		identityError(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	h.changed()
	JSON(w, status, g)
}
func (h authHandlers) deleteNetworkGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, valid := uuidText(r.PathValue("id"))
	if !valid {
		bad(w, r, "network group ID")
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	// A policy that references a deleted group would silently widen anonymous
	// access to every network, so referenced groups cannot be deleted.
	var used bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM client_auth_policies WHERE instance_id=$1 AND $2::uuid=ANY(anonymous_network_group_ids))", p.InstanceID, id).Scan(&used); err != nil {
		identityError(w, r, err)
		return
	}
	if used {
		Error(w, r, 409, api.ErrorCodeConflict, "network group is referenced by a policy")
		return
	}
	tag, err := tx.Exec(ctx, "DELETE FROM network_groups WHERE instance_id=$1 AND id=$2", p.InstanceID, id)
	if err != nil {
		identityError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		identityError(w, r, identityDBError(pgx.ErrNoRows))
		return
	}
	if err = identityEvent(ctx, tx, p, id, "auth.network_group.delete", 0); err != nil {
		identityError(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	h.changed()
	w.WriteHeader(204)
}

// --- GitHub OIDC ---

func (h authHandlers) getGitHubConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	out := GitHubOIDCConfig{Issuer: identity.GitHubIssuer, Revision: 1}
	err := h.db.QueryRow(r.Context(), "SELECT audience,revision FROM github_oidc_config WHERE instance_id=$1", p.InstanceID).Scan(&out.Audience, &out.Revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		identityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}
func (h authHandlers) putGitHubConfig(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in PutGitHubOIDCConfig
	if !sourceJSON(w, r, &in, "audience") {
		return
	}
	if len(in.Audience) > 256 || strings.TrimSpace(in.Audience) != in.Audience || strings.ContainsFunc(in.Audience, func(c rune) bool { return c < 32 || c == 127 }) || !utf8.ValidString(in.Audience) {
		bad(w, r, "audience")
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	out := GitHubOIDCConfig{Issuer: identity.GitHubIssuer, Audience: in.Audience}
	if err := tx.QueryRow(ctx, `INSERT INTO github_oidc_config(instance_id,audience) VALUES($1,$2)
 ON CONFLICT(instance_id) DO UPDATE SET audience=excluded.audience,revision=github_oidc_config.revision+1,updated_at=now() RETURNING revision`, p.InstanceID, in.Audience).Scan(&out.Revision); err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	if err := identityEvent(ctx, tx, p, p.InstanceID, "auth.github_oidc.config", out.Revision); err != nil {
		identityError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	if h.d.GitHub != nil {
		h.d.GitHub.InvalidateConfig()
	}
	JSON(w, 200, out)
}

var orgPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func toIdentityScopes(in []TokenScope) []identity.Scope {
	out := make([]identity.Scope, 0, len(in))
	for _, s := range in {
		sc := identity.Scope{FeedID: s.FeedID, Namespace: s.Namespace}
		for _, a := range s.Actions {
			sc.Actions = append(sc.Actions, identity.Action(a))
		}
		out = append(out, sc)
	}
	return out
}
func fromIdentityScopes(in []identity.Scope) []TokenScope {
	out := make([]TokenScope, 0, len(in))
	for _, s := range in {
		ts := TokenScope{FeedID: s.FeedID, Namespace: s.Namespace, Actions: []string{}}
		for _, a := range s.Actions {
			ts.Actions = append(ts.Actions, string(a))
		}
		out = append(out, ts)
	}
	return out
}
func globText(v string) bool {
	return len(v) > 0 && len(v) <= 256 && utf8.ValidString(v) && !strings.ContainsFunc(v, func(c rune) bool { return c < 32 || c == 127 })
}

func normalizeRule(in GitHubTrustRuleInput) (GitHubTrustRuleInput, []identity.Scope, bool) {
	if in.Ref == "" {
		in.Ref = "*"
	}
	if in.Environment == "" {
		in.Environment = "*"
	}
	if in.Workflow == "" {
		in.Workflow = "*"
	}
	if in.TTLSeconds == 0 {
		in.TTLSeconds = 900
	}
	if in.Enabled == nil {
		t := true
		in.Enabled = &t
	}
	scopes := toIdentityScopes(in.Scopes)
	_, pid := uuidText(in.PrincipalID)
	ok := identityText(in.Name, 128) && orgPattern.MatchString(in.Org) && globText(in.Repo) && globText(in.Ref) && globText(in.Environment) && globText(in.Workflow) && pid &&
		in.TTLSeconds >= 60 && in.TTLSeconds <= 3600 && identity.ValidPATScopes(scopes)
	if ok {
		in.PrincipalID, _ = uuidText(in.PrincipalID)
	}
	return in, scopes, ok
}
func (h authHandlers) listTrustRules(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	rows, err := h.db.Query(r.Context(), "SELECT id::text,name,org,repo,ref,environment,workflow,principal_id::text,scopes,ttl_seconds,enabled,revision FROM github_oidc_trust_rules WHERE instance_id=$1 ORDER BY created_at,id LIMIT 500", p.InstanceID)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := GitHubTrustRuleList{Items: []GitHubTrustRule{}}
	for rows.Next() {
		var g GitHubTrustRule
		var raw []byte
		if err = rows.Scan(&g.ID, &g.Name, &g.Org, &g.Repo, &g.Ref, &g.Environment, &g.Workflow, &g.PrincipalID, &raw, &g.TTLSeconds, &g.Enabled, &g.Revision); err != nil {
			identityError(w, r, err)
			return
		}
		g.Scopes = decodeScopes(raw)
		out.Items = append(out.Items, g)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	JSON(w, 200, out)
}
func (h authHandlers) createTrustRule(w http.ResponseWriter, r *http.Request) { h.saveRule(w, r, "") }
func (h authHandlers) putTrustRule(w http.ResponseWriter, r *http.Request) {
	id, ok := uuidText(r.PathValue("id"))
	if !ok {
		bad(w, r, "trust rule ID")
		return
	}
	h.saveRule(w, r, id)
}
func (h authHandlers) saveRule(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in GitHubTrustRuleInput
	if !sourceJSON(w, r, &in, "name", "org", "repo", "ref", "environment", "workflow", "principal_id", "scopes", "ttl_seconds", "enabled") {
		return
	}
	in, scopes, valid := normalizeRule(in)
	if !valid {
		bad(w, r, "trust rule")
		return
	}
	scopes, err := identityNormalized(scopes)
	if err != nil {
		bad(w, r, "trust rule")
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	var service bool
	if err = tx.QueryRow(ctx, "SELECT kind='service' AND NOT disabled FROM principals WHERE instance_id=$1 AND id=$2", p.InstanceID, in.PrincipalID).Scan(&service); err != nil || !service {
		bad(w, r, "principal (must be an enabled service principal)")
		return
	}
	for _, s := range scopes {
		if s.FeedID == "" {
			continue
		}
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)", p.InstanceID, s.FeedID).Scan(&exists); err != nil || !exists {
			bad(w, r, "scope feed")
			return
		}
	}
	encoded := encodeScopes(scopes)
	g := GitHubTrustRule{Name: in.Name, Org: in.Org, Repo: in.Repo, Ref: in.Ref, Environment: in.Environment, Workflow: in.Workflow, PrincipalID: in.PrincipalID, Scopes: fromIdentityScopes(scopes), TTLSeconds: in.TTLSeconds, Enabled: *in.Enabled}
	status, action := 201, "auth.github_oidc.rule.create"
	if id == "" {
		err = tx.QueryRow(ctx, `INSERT INTO github_oidc_trust_rules(instance_id,name,org,repo,ref,environment,workflow,principal_id,scopes,ttl_seconds,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id::text,revision`,
			p.InstanceID, g.Name, g.Org, g.Repo, g.Ref, g.Environment, g.Workflow, g.PrincipalID, encoded, g.TTLSeconds, g.Enabled).Scan(&g.ID, &g.Revision)
	} else {
		status, action = 200, "auth.github_oidc.rule.put"
		err = tx.QueryRow(ctx, `UPDATE github_oidc_trust_rules SET name=$3,org=$4,repo=$5,ref=$6,environment=$7,workflow=$8,principal_id=$9,scopes=$10,ttl_seconds=$11,enabled=$12,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2 RETURNING id::text,revision`,
			p.InstanceID, id, g.Name, g.Org, g.Repo, g.Ref, g.Environment, g.Workflow, g.PrincipalID, encoded, g.TTLSeconds, g.Enabled).Scan(&g.ID, &g.Revision)
	}
	if err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	if err = identityEvent(ctx, tx, p, g.ID, action, g.Revision); err != nil {
		identityError(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	JSON(w, status, g)
}
func (h authHandlers) deleteTrustRule(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, valid := uuidText(r.PathValue("id"))
	if !valid {
		bad(w, r, "trust rule ID")
		return
	}
	tx, ctx, cancel, ok := h.tx(w, r)
	if !ok {
		return
	}
	defer cancel()
	defer authRollback(tx)
	tag, err := tx.Exec(ctx, "DELETE FROM github_oidc_trust_rules WHERE instance_id=$1 AND id=$2", p.InstanceID, id)
	if err != nil {
		identityError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		identityError(w, r, identityDBError(pgx.ErrNoRows))
		return
	}
	if err = identityEvent(ctx, tx, p, id, "auth.github_oidc.rule.delete", 0); err != nil {
		identityError(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		identityError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

// exchange trades a GitHub Actions OIDC JWT for a short-lived scoped Ant token.
// The JWT is the credential: it may arrive as {"token":...} or as a Bearer.
func (h authHandlers) exchange(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.d.GitHub == nil {
		Error(w, r, 404, api.ErrorCodeNotFound, "github oidc exchange is not enabled")
		return
	}
	peer := identity.ClientIP(r, nil).String()
	if h.limiter.blocked(peer) {
		w.Header().Set("Retry-After", "60")
		Error(w, r, 429, api.ErrorCodeUnavailable, "exchange attempts limited")
		return
	}
	raw := ""
	if v := r.Header.Values("Authorization"); len(v) == 1 {
		if scheme, tok, ok := strings.Cut(v[0], " "); ok && strings.EqualFold(scheme, "Bearer") {
			raw = strings.TrimSpace(tok)
		}
	}
	if raw == "" {
		var in GitHubExchangeRequest
		if !sourceJSON(w, r, &in, "token") {
			return
		}
		raw = in.Token
	}
	issued, rule, err := h.d.GitHub.Exchange(r.Context(), raw)
	switch {
	case err == nil:
		JSON(w, 200, GitHubExchangeResponse{AccessToken: issued.Secret, TokenType: "Bearer", ExpiresAt: issued.ExpiresAt, TrustRuleID: rule.ID})
	case errors.Is(err, identity.ErrNoTrust):
		h.limiter.fail(peer)
		Error(w, r, 403, api.ErrorCodeForbidden, "no trust rule matches this workflow")
	case errors.Is(err, identity.ErrGitHubDisabled):
		Error(w, r, 403, api.ErrorCodeForbidden, "github oidc exchange is not configured")
	case errors.Is(err, identity.ErrUnauthenticated):
		h.limiter.fail(peer)
		Error(w, r, 401, api.ErrorCodeUnauthenticated, "invalid token")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "token exchange unavailable")
	}
}

// --- personal access tokens ---

func patDTO(rec identity.PATRecord) PersonalAccessToken {
	return PersonalAccessToken{ID: rec.ID, Name: rec.Name, Scopes: fromIdentityScopes(rec.Scopes), CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt, LastUsedAt: rec.LastUsedAt, RevokedAt: rec.RevokedAt}
}

// self authorizes a signed-in user acting on their own tokens. Machine tokens
// (including PATs) cannot manage PATs.
func (h authHandlers) self(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok {
		Error(w, r, 401, api.ErrorCodeUnauthenticated, "authentication required")
		return p, false
	}
	if p.TokenID != "" {
		Error(w, r, 403, api.ErrorCodeForbidden, "personal access tokens are managed from a signed-in session")
		return p, false
	}
	return p, true
}
func patError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrPATInvalid):
		bad(w, r, "personal access token")
	case errors.Is(err, identity.ErrPATLimit):
		Error(w, r, 409, api.ErrorCodeConflict, "too many active personal access tokens")
	case errors.Is(err, identity.ErrPATNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "personal access token not found")
	case errors.Is(err, identity.ErrPATNotUser):
		Error(w, r, 403, api.ErrorCodeForbidden, "personal access tokens are only available to user accounts")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "token management unavailable")
	}
}
func (h authHandlers) listPATs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.self(w, r)
	if !ok {
		return
	}
	recs, err := (identity.PATs{DB: h.db}).List(r.Context(), p.InstanceID, p.PrincipalID)
	if err != nil {
		patError(w, r, err)
		return
	}
	out := PersonalAccessTokenList{Items: []PersonalAccessToken{}}
	for _, rec := range recs {
		out.Items = append(out.Items, patDTO(rec))
	}
	JSON(w, 200, out)
}
func (h authHandlers) createPAT(w http.ResponseWriter, r *http.Request) {
	p, ok := h.self(w, r)
	if !ok {
		return
	}
	var in CreatePersonalAccessToken
	if !sourceJSON(w, r, &in, "name", "scopes", "expires_in_days") {
		return
	}
	if in.ExpiresInDays < 1 || in.ExpiresInDays > 365 {
		bad(w, r, "expiry (1 to 365 days)")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	// The secret is returned to the first caller only. The replay store keeps
	// the non-secret metadata, so a retry with the same key gets a 409 instead
	// of a token it can never see.
	var rec identity.PATRecord
	var secret string
	expires := time.Now().Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour)
	apply := func(ctx context.Context, tx pgx.Tx) (any, error) {
		var err error
		rec, secret, err = identity.CreatePATTx(ctx, tx, p.InstanceID, p.PrincipalID, in.Name, toIdentityScopes(in.Scopes), expires)
		if err != nil {
			return nil, err
		}
		return patDTO(rec), nil
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "auth.pat.create", "principal:"+p.PrincipalID, in, apply)
	if callErr != nil {
		patError(w, r, callErr)
		return
	}
	if storeErr != nil {
		storeError(w, r, storeErr)
		return
	}
	if out.Replayed {
		Error(w, r, 409, api.ErrorCodeConflict, "already created, secret not retrievable")
		return
	}
	JSON(w, 201, CreatedPersonalAccessToken{PersonalAccessToken: patDTO(rec), Token: secret})
}
func (h authHandlers) revokePAT(w http.ResponseWriter, r *http.Request) {
	p, ok := h.self(w, r)
	if !ok {
		return
	}
	id, valid := uuidText(r.PathValue("id"))
	if !valid {
		bad(w, r, "token ID")
		return
	}
	if err := (identity.PATs{DB: h.db}).Revoke(r.Context(), p.InstanceID, p.PrincipalID, id); err != nil {
		patError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func identityNormalized(in []identity.Scope) ([]identity.Scope, error) {
	return identity.NormalizeScopes(in)
}
func encodeScopes(in []identity.Scope) []byte {
	b, _ := json.Marshal(in)
	return b
}
func decodeScopes(raw []byte) []TokenScope {
	var scopes []identity.Scope
	if json.Unmarshal(raw, &scopes) != nil {
		return []TokenScope{}
	}
	return fromIdentityScopes(scopes)
}
