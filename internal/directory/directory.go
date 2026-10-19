// Package directory authenticates against explicitly configured LDAP directories.
// It does not provision principals, issue sessions, or change persisted grants.
package directory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

var (
	ErrConfig         = errors.New("invalid directory configuration")
	ErrAuthentication = errors.New("directory authentication failed")
	ErrUnavailable    = errors.New("directory unavailable")
	ErrGroups         = errors.New("directory group resolution failed")
)

// Connection is a trusted, injectable transport boundary. Implementations must
// enforce request size/time limits and concurrent Close must interrupt operations.
type Connection interface {
	Bind(string, string) error
	Search(context.Context, *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}
type Dialer func(context.Context, string, *tls.Config) (Connection, error)

// SecretSource returns a fresh owned buffer, which is erased after binding.
// It must obey ctx. Secrets are fetched per operation and never cached here.
type SecretSource func(context.Context) ([]byte, time.Time, error)

type Config struct {
	ProviderID, URL, BindDN, UserBaseDN, GroupBaseDN string
	// Attribute names and object classes are trusted administrator configuration.
	LoginAttribute, UserIDAttribute, GroupIDAttribute string
	UserClass, GroupClass, MemberAttribute            string
	CAPEM                                             []byte
	Timeout                                           time.Duration
	MaxGroups, MaxSearches, MaxDepth                  int
	// GroupRoles maps base64url-encoded raw immutable group IDs to internal role IDs.
	// No defaults, name-based mappings, or automatic administrator grants exist.
	GroupRoles map[string][]string
	// GroupNesting selects group resolution: "nested" (default) follows groups that
	// are members of groups up to MaxDepth; "direct" uses only groups the user is
	// a direct member of, so a role mapped to an outer group is never inherited.
	GroupNesting string
}

type Provider struct {
	config Config
	tls    *tls.Config
	secret SecretSource
	dial   Dialer
	// defaultDial is true when the real network transport is in use.
	defaultDial bool
}

type ExternalIdentity struct {
	ProviderID string
	Subject    string // base64url raw entryUUID/objectGUID, scoped by ProviderID
	GroupIDs   []string
	RoleIDs    []string // trusted configuration proposals; caller persists/resolves grants
	// RolesMapped reports whether any group-to-role mapping is configured, so the
	// caller reconciles claim-managed roles (including down to none).
	RolesMapped bool
}

// Group nesting policies for Config.GroupNesting.
const (
	NestingNested = "nested"
	NestingDirect = "direct"
)

var attribute = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

func New(c Config, secret SecretSource, dial Dialer) (*Provider, error) {
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxGroups == 0 {
		c.MaxGroups = 200
	}
	if c.MaxSearches == 0 {
		c.MaxSearches = 201
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = 10
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "ldaps" && u.Scheme != "ldap") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, ErrConfig
	}
	if c.ProviderID == "" || len(c.ProviderID) > 256 || secret == nil || c.Timeout < time.Second || c.Timeout > 30*time.Second || c.MaxGroups < 1 || c.MaxGroups > 1000 || c.MaxSearches < 1 || c.MaxSearches > 2001 || c.MaxDepth < 1 || c.MaxDepth > 10 {
		return nil, ErrConfig
	}
	for _, dn := range []string{c.BindDN, c.UserBaseDN, c.GroupBaseDN} {
		if !validDN(dn) {
			return nil, ErrConfig
		}
	}
	for _, a := range []string{c.LoginAttribute, c.UserIDAttribute, c.GroupIDAttribute, c.UserClass, c.GroupClass, c.MemberAttribute} {
		if !attribute.MatchString(a) {
			return nil, ErrConfig
		}
	}
	// Only immutable identity schemas are supported by this baseline.
	for _, a := range []string{c.UserIDAttribute, c.GroupIDAttribute} {
		if a != "entryUUID" && a != "objectGUID" {
			return nil, ErrConfig
		}
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if len(c.CAPEM) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(c.CAPEM) {
			return nil, ErrConfig
		}
	}
	mappings := make(map[string][]string, len(c.GroupRoles))
	for id, roles := range c.GroupRoles {
		if b, e := base64.RawURLEncoding.DecodeString(id); e != nil || len(b) == 0 || len(b) > 128 {
			return nil, ErrConfig
		}
		for _, r := range roles {
			if r == "" || len(r) > 128 || strings.ContainsAny(r, "\x00\r\n") {
				return nil, ErrConfig
			}
		}
		mappings[id] = append([]string(nil), roles...)
	}
	switch c.GroupNesting {
	case "":
		c.GroupNesting = NestingNested
	case NestingNested, NestingDirect:
	default:
		return nil, ErrConfig
	}
	c.GroupRoles = mappings
	c.CAPEM = nil
	isDefault := dial == nil
	if isDefault {
		dial = dialLDAP
	}
	return &Provider{c, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname(), RootCAs: roots}, secret, dial, isDefault}, nil
}
func validDN(s string) bool {
	if len(s) == 0 || len(s) > 4096 {
		return false
	}
	d, e := ldap.ParseDN(s)
	return e == nil && len(d.RDNs) > 0
}

func (p *Provider) connect(ctx context.Context) (Connection, error) {
	c, e := p.dial(ctx, p.config.URL, p.tls.Clone())
	if e != nil {
		return nil, ErrUnavailable
	}
	return c, nil
}
func (p *Provider) bindService(ctx context.Context, c Connection) error {
	b, expiry, e := p.secret(ctx)
	defer clear(b)
	if e != nil || len(b) == 0 || len(b) > 4096 || !expiry.After(time.Now()) {
		return ErrUnavailable
	}
	if e = c.Bind(p.config.BindDN, string(b)); e != nil || ctx.Err() != nil || !expiry.After(time.Now()) {
		return ErrUnavailable
	}
	return nil
}

// Authenticate consumes and erases password on every return. Go/LDAP string and
// network buffers cannot guarantee erasure; connections live at most Timeout.
// Caller must not retain/log credentials and must apply ingress rate limiting.
func (p *Provider) Authenticate(ctx context.Context, login string, password []byte) (ExternalIdentity, error) {
	defer clear(password)
	if login == "" || len(login) > 256 || len(password) == 0 || len(password) > 4096 {
		return ExternalIdentity{}, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	c, e := p.connect(ctx)
	if e != nil {
		return ExternalIdentity{}, e
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if e = p.bindService(ctx, c); e != nil {
		return ExternalIdentity{}, e
	}
	filter := "(&(objectClass=" + ldap.EscapeFilter(p.config.UserClass) + ")(" + p.config.LoginAttribute + "=" + ldap.EscapeFilter(login) + "))"
	users, e := p.search(ctx, c, p.config.UserBaseDN, filter, 2, []string{p.config.UserIDAttribute})
	if e != nil || len(users) != 1 {
		return ExternalIdentity{}, ErrAuthentication
	}
	user := users[0]
	subject, e := immutableID(user, p.config.UserIDAttribute)
	if e != nil {
		return ExternalIdentity{}, ErrAuthentication
	}
	// A distinct connection prevents user ACLs or failed binds influencing service searches.
	auth, e := p.connect(ctx)
	if e != nil {
		return ExternalIdentity{}, e
	}
	stopAuth := context.AfterFunc(ctx, func() { _ = auth.Close() })
	e = auth.Bind(user.DN, string(password))
	clear(password)
	stopAuth()
	_ = auth.Close()
	if e != nil || ctx.Err() != nil {
		return ExternalIdentity{}, ErrAuthentication
	}
	groups, e := p.groups(ctx, c, user.DN)
	if e != nil {
		return ExternalIdentity{}, ErrGroups
	}
	roles := map[string]bool{}
	for _, g := range groups {
		for _, r := range p.config.GroupRoles[g] {
			roles[r] = true
		}
	}
	result := ExternalIdentity{ProviderID: p.config.ProviderID, Subject: subject, GroupIDs: groups, RolesMapped: len(p.config.GroupRoles) > 0}
	for r := range roles {
		result.RoleIDs = append(result.RoleIDs, r)
	}
	sort.Strings(result.RoleIDs)
	if ctx.Err() != nil {
		return ExternalIdentity{}, ErrUnavailable
	}
	return result, nil
}

// Health verifies TLS, service bind and bounded read access to both configured bases.
func (p *Provider) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	c, e := p.connect(ctx)
	if e != nil {
		return e
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if e = p.bindService(ctx, c); e != nil {
		return e
	}
	for _, base := range []string{p.config.UserBaseDN, p.config.GroupBaseDN} {
		req := ldap.NewSearchRequest(base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 1, false, "(objectClass=*)", []string{"1.1"}, nil)
		r, e := c.Search(ctx, req)
		if e != nil || r == nil || len(r.Entries) != 1 || len(r.Referrals) > 0 {
			return ErrUnavailable
		}
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	return nil
}
func (p *Provider) search(ctx context.Context, c Connection, base, filter string, limit int, attrs []string) ([]*ldap.Entry, error) {
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	req := ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, limit, int((p.config.Timeout+time.Second-1)/time.Second), false, filter, attrs, nil)
	r, e := c.Search(ctx, req)
	if e != nil || r == nil || len(r.Referrals) > 0 || len(r.Entries) > limit || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	b, _ := ldap.ParseDN(base)
	for _, v := range r.Entries {
		if v == nil || !validDN(v.DN) {
			return nil, ErrUnavailable
		}
		dn, _ := ldap.ParseDN(v.DN)
		if !b.EqualFold(dn) && !b.AncestorOfFold(dn) {
			return nil, ErrUnavailable
		}
	}
	return r.Entries, nil
}
func immutableID(e *ldap.Entry, attr string) (string, error) {
	values := e.GetRawAttributeValues(attr)
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 128 {
		return "", ErrAuthentication
	}
	if attr == "objectGUID" && len(values[0]) != 16 {
		return "", ErrAuthentication
	}
	return base64.RawURLEncoding.EncodeToString(values[0]), nil
}
func (p *Provider) groups(ctx context.Context, c Connection, user string) ([]string, error) {
	ids := map[string]string{}
	searches := 0
	var walk func(string, int, []*ldap.DN) error
	walk = func(member string, depth int, path []*ldap.DN) error {
		searches++
		if searches > p.config.MaxSearches {
			return ErrGroups
		}
		filter := "(&(objectClass=" + ldap.EscapeFilter(p.config.GroupClass) + ")(" + p.config.MemberAttribute + "=" + ldap.EscapeFilter(member) + "))"
		entries, e := p.search(ctx, c, p.config.GroupBaseDN, filter, p.config.MaxGroups+1, []string{p.config.GroupIDAttribute})
		if e != nil || len(entries) > p.config.MaxGroups {
			return ErrGroups
		}
		for _, entry := range entries {
			if depth >= p.config.MaxDepth {
				return ErrGroups
			}
			dn, _ := ldap.ParseDN(entry.DN)
			for _, ancestor := range path {
				if dn.EqualFold(ancestor) {
					return ErrGroups
				}
			}
			id, e := immutableID(entry, p.config.GroupIDAttribute)
			if e != nil {
				return ErrGroups
			}
			if old, ok := ids[id]; ok {
				oldDN, _ := ldap.ParseDN(old)
				if !oldDN.EqualFold(dn) {
					return ErrGroups
				}
			}
			ids[id] = entry.DN
			if len(ids) > p.config.MaxGroups {
				return ErrGroups
			}
			if p.config.GroupNesting == NestingDirect {
				continue
			}
			if e = walk(entry.DN, depth+1, append(append([]*ldap.DN(nil), path...), dn)); e != nil {
				return e
			}
		}
		return nil
	}
	dn, _ := ldap.ParseDN(user)
	if e := walk(user, 0, []*ldap.DN{dn}); e != nil {
		return nil, e
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}
