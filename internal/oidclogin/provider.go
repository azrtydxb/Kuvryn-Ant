// Package oidclogin implements OIDC browser login and PostgreSQL BFF sessions.
package oidclogin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var ErrAuthentication = errors.New("browser authentication failed")
var ErrUnavailable = errors.New("browser authentication unavailable")

// ProviderConfig is trusted operator configuration, never populated from requests.
// AllowedEndpoints lists exact authorization, token and JWKS URLs approved by the operator;
// discovery cannot expand the outbound network authority. HTTPS is mandatory.
type ProviderConfig struct {
	Key, Issuer, ClientID, ClientSecret, RedirectURI string
	AllowedEndpoints                                 []string
	// RolesClaim names an ID-token claim; ClaimRoles maps its values to local
	// role IDs, applied at login by Sessions.ApplyClaimRoles. Scopes are extra
	// scopes requested besides openid.
	RolesClaim string
	ClaimRoles map[string][]string
	Scopes     []string
}
type Provider struct {
	key, issuer, redirect string
	oauth                 oauth2.Config
	verifier              *oidc.IDTokenVerifier
	client                *http.Client
	rolesClaim            string
	claimRoles            map[string][]string
}

func NewProvider(ctx context.Context, cfg ProviderConfig) (*Provider, error) {
	return newProvider(ctx, cfg, http.DefaultTransport, false)
}

// NewProviderWithTransport is NewProvider with an explicit outbound transport,
// used by administrator diagnostics and tests to control name resolution and
// trust. HTTPS remains mandatory and the approved-endpoint bound still applies.
func NewProviderWithTransport(ctx context.Context, cfg ProviderConfig, transport http.RoundTripper) (*Provider, error) {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return newProvider(ctx, cfg, transport, false)
}
func newProvider(ctx context.Context, cfg ProviderConfig, transport http.RoundTripper, testHTTP bool) (*Provider, error) {
	valid := func(raw string) bool {
		u, e := url.Parse(raw)
		return e == nil && u.Host != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || testHTTP && u.Scheme == "http")
	}
	if cfg.Key == "" || cfg.ClientID == "" || !valid(cfg.Issuer) || !valid(cfg.RedirectURI) {
		return nil, errors.New("invalid trusted OIDC configuration")
	}
	issuer, _ := url.Parse(cfg.Issuer)
	redirect, _ := url.Parse(cfg.RedirectURI)
	if issuer.RawQuery != "" || redirect.RawQuery != "" {
		return nil, errors.New("OIDC issuer and redirect must not contain queries")
	}
	allowed := map[string]bool{strings.TrimSuffix(cfg.Issuer, "/") + "/.well-known/openid-configuration": true}
	for _, u := range cfg.AllowedEndpoints {
		if !valid(u) {
			return nil, errors.New("invalid approved OIDC endpoint")
		}
		allowed[u] = true
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: boundedTransport{base: transport, allowed: allowed}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OIDC redirects prohibited") }}
	ctx = oidc.ClientContext(ctx, client)
	discovered, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, ErrUnavailable
	}
	var metadata struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if discovered.Claims(&metadata) != nil || metadata.Issuer != cfg.Issuer || !allowed[metadata.JWKS] || !allowed[discovered.Endpoint().TokenURL] || !valid(discovered.Endpoint().AuthURL) || !allowed[discovered.Endpoint().AuthURL] {
		return nil, errors.New("unapproved OIDC discovery endpoints")
	}
	// The library caches keys and refetches on unknown signing keys. Only asymmetric
	// RS256 is enabled; no key/algorithm selection is accepted from browser input.
	p := &Provider{key: cfg.Key, issuer: cfg.Issuer, redirect: cfg.RedirectURI, client: client}
	p.oauth = oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURI, Endpoint: discovered.Endpoint(), Scopes: append([]string{oidc.ScopeOpenID}, cfg.Scopes...)}
	p.rolesClaim, p.claimRoles = cfg.RolesClaim, cfg.ClaimRoles
	p.verifier = discovered.Verifier(&oidc.Config{ClientID: cfg.ClientID, SupportedSigningAlgs: []string{oidc.RS256}})
	return p, nil
}

type boundedTransport struct {
	base    http.RoundTripper
	allowed map[string]bool
}

func (t boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.allowed[r.URL.String()] {
		return nil, errors.New("unapproved OIDC request")
	}
	resp, e := t.base.RoundTrip(r)
	if e != nil {
		return nil, e
	}
	// Read the bounded response before returning: library readers cannot allocate
	// unbounded discovery, JWKS, or token bodies, including chunked responses.
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if e != nil || len(b) > 1<<20 {
		return nil, errors.New("OIDC response exceeds bound")
	}
	resp.Body = io.NopCloser(strings.NewReader(string(b)))
	return resp, nil
}
func (p *Provider) authorization(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}
func (p *Provider) exchange(ctx context.Context, code, verifier string, nonceHash []byte) (string, error) {
	id, err := p.exchangeIdentity(ctx, code, verifier, nonceHash)
	return id.Subject, err
}

// Identity is a verified ID-token identity. Roles are the local role IDs the
// provider's claim mapping yields for this login (RolesMapped reports whether
// a mapping is configured at all).
type Identity struct {
	Subject     string
	Groups      []string
	Roles       []string
	RolesMapped bool
}

// claimValues reads a claim as a set of strings: an array of strings, a single
// string, or an object whose keys are the values (Zitadel project roles).
func claimValues(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k := range obj {
			list = append(list, k)
		}
		sort.Strings(list)
		return list
	}
	return nil
}

func (p *Provider) mapRoles(all map[string]json.RawMessage) []string {
	if p.rolesClaim == "" {
		return nil
	}
	seen := map[string]bool{}
	var roles []string
	for i, v := range claimValues(all[p.rolesClaim]) {
		if i >= 500 {
			break
		}
		for _, r := range p.claimRoles[v] {
			if !seen[r] {
				seen[r] = true
				roles = append(roles, r)
			}
		}
	}
	sort.Strings(roles)
	return roles
}

// exchangeIdentity also returns the ID token "groups" claim (an array of
// strings; anything else is ignored). Groups are used only for federated
// group bindings (P2-M2) and never grant anything locally by themselves.
func (p *Provider) exchangeIdentity(ctx context.Context, code, verifier string, nonceHash []byte) (Identity, error) {
	ctx = oidc.ClientContext(ctx, p.client)
	token, e := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if e != nil {
		return Identity{}, ErrAuthentication
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return Identity{}, ErrAuthentication
	}
	id, e := p.verifier.Verify(ctx, raw)
	if e != nil {
		return Identity{}, ErrAuthentication
	}
	var claims struct {
		AuthorizedParty string          `json:"azp"`
		NotBefore       int64           `json:"nbf"`
		Groups          json.RawMessage `json:"groups"`
	}
	now := time.Now()
	if id.Claims(&claims) != nil || id.Subject == "" || id.Issuer != p.issuer || id.IssuedAt.IsZero() || id.IssuedAt.After(now.Add(30*time.Second)) || !id.Expiry.After(now) || claims.NotBefore > now.Add(30*time.Second).Unix() || !matches(id.Nonce, nonceHash) {
		return Identity{}, ErrAuthentication
	}
	if (len(id.Audience) > 1 && claims.AuthorizedParty == "") || (claims.AuthorizedParty != "" && claims.AuthorizedParty != p.oauth.ClientID) {
		return Identity{}, ErrAuthentication
	}
	if id.AccessTokenHash != "" && id.VerifyAccessToken(token.AccessToken) != nil {
		return Identity{}, ErrAuthentication
	}
	var groups []string
	if len(claims.Groups) > 0 && json.Unmarshal(claims.Groups, &groups) != nil {
		groups = nil
	}
	if len(groups) > 500 {
		groups = groups[:500]
	}
	var all map[string]json.RawMessage
	if p.rolesClaim != "" && id.Claims(&all) != nil {
		return Identity{}, ErrAuthentication
	}
	return Identity{Subject: id.Subject, Groups: groups, Roles: p.mapRoles(all), RolesMapped: p.rolesClaim != ""}, nil
}
