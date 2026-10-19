package directory

import (
	"context"
	"net"
	"net/url"
	"sort"

	"github.com/go-ldap/ldap/v3"
)

// Step is one connectivity check. Detail is fixed text (never server output,
// credentials or DNs) so it is safe to show to administrators.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Diagnosis is the outcome of Diagnose. TLSMode is "ldaps" (implicit TLS) or
// "starttls"; plaintext LDAP is never used by this package.
type Diagnosis struct {
	TLSMode string `json:"tls_mode"`
	Steps   []Step `json:"steps"`
	OK      bool   `json:"ok"`
}

// Diagnose runs the same checks as Health, reporting each stage separately so
// an operator can tell a network fault from a certificate or bind problem.
// Later steps are skipped after the first failure.
func (p *Provider) Diagnose(ctx context.Context) Diagnosis {
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	u, _ := url.Parse(p.config.URL)
	d := Diagnosis{TLSMode: "starttls"}
	if u.Scheme == "ldaps" {
		d.TLSMode = "ldaps"
	}
	add := func(name string, ok bool, detail string) bool {
		d.Steps = append(d.Steps, Step{name, ok, detail})
		return ok
	}
	port := u.Port()
	if port == "" {
		port = "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
	}
	// A dialer hook (tests) replaces real networking, so only probe TCP when
	// the default transport is in use.
	if p.defaultDial {
		raw, e := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
		if e != nil {
			add("tcp", false, "cannot open a TCP connection to the directory host and port")
			return d
		}
		_ = raw.Close()
		add("tcp", true, "TCP connection established")
	}
	c, e := p.connect(ctx)
	if e != nil {
		add("tls", false, "TLS negotiation or certificate verification failed ("+d.TLSMode+"); check the CA certificate and that the host name matches the certificate")
		return d
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	add("tls", true, "TLS established and certificate chain verified ("+d.TLSMode+")")
	if !add("service_bind", p.bindService(ctx, c) == nil, "service account bind") {
		d.Steps[len(d.Steps)-1].Detail = "service account bind was rejected or the bind password is unavailable"
		return d
	}
	d.Steps[len(d.Steps)-1].Detail = "service account bind succeeded"
	for _, s := range []struct{ name, base string }{{"user_base", p.config.UserBaseDN}, {"group_base", p.config.GroupBaseDN}} {
		req := ldap.NewSearchRequest(s.base, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 1, false, "(objectClass=*)", []string{"1.1"}, nil)
		r, e := c.Search(ctx, req)
		if e != nil || r == nil || len(r.Entries) != 1 || len(r.Referrals) > 0 {
			add(s.name, false, "search base is missing, unreadable to the service account, or returned a referral")
			return d
		}
		add(s.name, true, "search base readable")
	}
	d.OK = true
	return d
}

// Lookup resolves a login to its immutable subject, group IDs and the roles
// the configured mapping proposes, WITHOUT authenticating the user. It exists
// for administrator previews; it must never gate a login or issue a session.
func (p *Provider) Lookup(ctx context.Context, login string) (ExternalIdentity, error) {
	if login == "" || len(login) > 256 {
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
	subject, e := immutableID(users[0], p.config.UserIDAttribute)
	if e != nil {
		return ExternalIdentity{}, ErrAuthentication
	}
	groups, e := p.groups(ctx, c, users[0].DN)
	if e != nil {
		return ExternalIdentity{}, ErrGroups
	}
	roles := map[string]bool{}
	for _, g := range groups {
		for _, r := range p.config.GroupRoles[g] {
			roles[r] = true
		}
	}
	out := ExternalIdentity{ProviderID: p.config.ProviderID, Subject: subject, GroupIDs: groups, RolesMapped: len(p.config.GroupRoles) > 0}
	for r := range roles {
		out.RoleIDs = append(out.RoleIDs, r)
	}
	sort.Strings(out.RoleIDs)
	return out, nil
}
