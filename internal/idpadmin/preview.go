package idpadmin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Preview answers "if this person signed in, who would they be and what could
// they do?" without creating sessions. Pasted tokens and assertions are decoded
// but their signatures are NOT verified; the result says so. Only an existing
// identity binding grants access, exactly as at real sign-in; group claims are
// shown for information and for LDAP group-to-role proposals.
type Preview struct {
	DB         *pgxpool.Pool
	InstanceID string
	Now        func() time.Time
}

func (p Preview) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

type Subject struct {
	Issuer   string   `json:"issuer"`
	Subject  string   `json:"subject"`
	Email    string   `json:"email,omitempty"`
	Groups   []string `json:"groups"`
	Audience []string `json:"audience,omitempty"`
}

type Binding struct {
	Found       bool     `json:"found"`
	PrincipalID string   `json:"principal_id,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
	Roles       []string `json:"roles"`
}

type PreviewResult struct {
	Subject  Subject `json:"identity"`
	Verified bool    `json:"signature_verified"`
	Binding  Binding `json:"binding"`
	// ProposedRoles are LDAP group-to-role mappings from provider config. The
	// runtime does not apply these automatically; roles come from the binding.
	ProposedRoles    []string `json:"proposed_roles,omitempty"`
	WouldSignIn      bool     `json:"would_sign_in"`
	Problems         []string `json:"problems"`
	Warnings         []string `json:"warnings"`
	ClockSkewSeconds *int64   `json:"clock_skew_seconds,omitempty"`
}

func (p Preview) binding(ctx context.Context, issuer, subject string) (Binding, error) {
	b := Binding{Roles: []string{}}
	var disabled bool
	err := p.DB.QueryRow(ctx, `SELECT pr.id::text,pr.disabled FROM oidc_identity_bindings b JOIN principals pr ON (pr.instance_id,pr.id)=(b.instance_id,b.principal_id) WHERE b.instance_id=$1 AND b.issuer=$2 AND b.subject=$3`, p.InstanceID, issuer, subject).Scan(&b.PrincipalID, &disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	b.Found, b.Disabled = true, disabled
	policy, err := (identity.Roles{DB: p.DB}).Resolve(ctx, identity.Principal{InstanceID: p.InstanceID, PrincipalID: b.PrincipalID})
	if err != nil && !errors.Is(err, identity.ErrUnauthenticated) {
		return b, err
	}
	for _, r := range policy.Roles {
		b.Roles = append(b.Roles, r.Name)
	}
	sort.Strings(b.Roles)
	return b, nil
}

func (p Preview) finish(ctx context.Context, r *PreviewResult) (PreviewResult, error) {
	if r.Subject.Subject == "" {
		r.Problems = append(r.Problems, "no subject (sub / NameID) found; sign-in would be refused")
		return *r, nil
	}
	b, err := p.binding(ctx, r.Subject.Issuer, r.Subject.Subject)
	if err != nil {
		return *r, err
	}
	r.Binding = b
	switch {
	case !b.Found:
		r.Problems = append(r.Problems, "no identity binding for this issuer and subject; sign-in would be refused until an administrator binds it to a user")
	case b.Disabled:
		r.Problems = append(r.Problems, "bound user is disabled")
	case len(b.Roles) == 0:
		r.Warnings = append(r.Warnings, "bound user has no roles and could sign in but access nothing")
	}
	r.WouldSignIn = b.Found && !b.Disabled && len(r.Problems) == 0
	return *r, nil
}

func newResult() PreviewResult {
	return PreviewResult{Subject: Subject{Groups: []string{}}, Binding: Binding{Roles: []string{}}, Problems: []string{}, Warnings: []string{}}
}

func strList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func num(v any) (int64, bool) {
	f, ok := v.(float64)
	return int64(f), ok
}

// OIDCToken decodes a pasted ID token (unverified) and evaluates it against the
// provider configuration.
func (p Preview) OIDCToken(ctx context.Context, cfg OIDCConfig, token string) (PreviewResult, error) {
	r := newResult()
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || len(token) > 32<<10 {
		r.Problems = append(r.Problems, "not a JWT (expected three dot-separated parts)")
		return r, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	var claims map[string]any
	if err != nil || json.Unmarshal(raw, &claims) != nil {
		r.Problems = append(r.Problems, "token payload is not valid base64url JSON")
		return r, nil
	}
	r.Warnings = append(r.Warnings, "signature was not verified; this is a mapping preview, not a login check")
	r.Subject.Issuer, _ = claims["iss"].(string)
	r.Subject.Subject, _ = claims["sub"].(string)
	r.Subject.Email, _ = claims["email"].(string)
	r.Subject.Audience = strList(claims["aud"])
	claim := cfg.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	if g := strList(claims[claim]); g != nil {
		r.Subject.Groups = g
	} else {
		r.Warnings = append(r.Warnings, fmt.Sprintf("claim %q not present in the token; groups are informational only", claim))
	}
	if r.Subject.Issuer != cfg.Issuer {
		r.Problems = append(r.Problems, "iss does not match the configured issuer")
	}
	aud := false
	for _, a := range r.Subject.Audience {
		aud = aud || a == cfg.ClientID
	}
	if !aud {
		r.Problems = append(r.Problems, "aud does not include the configured client ID")
	}
	now := p.now()
	if exp, ok := num(claims["exp"]); !ok || !time.Unix(exp, 0).After(now) {
		r.Problems = append(r.Problems, "token is expired or has no exp (paste a fresh token)")
	}
	if nbf, ok := num(claims["nbf"]); ok && time.Unix(nbf, 0).After(now.Add(SkewWarning)) {
		r.Problems = append(r.Problems, "token is not yet valid (nbf in the future)")
	}
	if iat, ok := num(claims["iat"]); ok {
		d := int64(time.Unix(iat, 0).Sub(now) / time.Second)
		r.ClockSkewSeconds = &d
		if time.Unix(iat, 0).After(now.Add(SkewWarning)) {
			r.Problems = append(r.Problems, fmt.Sprintf("iat is %ds in the future: the provider clock is ahead of this server's; real sign-ins would be rejected", d))
		}
	}
	return p.finish(ctx, &r)
}

type samlAttr struct {
	Name   string   `xml:"Name,attr"`
	Values []string `xml:"AttributeValue"`
}
type samlDoc struct {
	Issuer    string `xml:"Issuer"`
	Assertion struct {
		Issuer  string `xml:"Issuer"`
		Subject struct {
			NameID string `xml:"NameID"`
		} `xml:"Subject"`
		Conditions struct {
			NotBefore    string   `xml:"NotBefore,attr"`
			NotOnOrAfter string   `xml:"NotOnOrAfter,attr"`
			Audience     []string `xml:"AudienceRestriction>Audience"`
		} `xml:"Conditions"`
		Attributes []samlAttr `xml:"AttributeStatement>Attribute"`
	} `xml:"Assertion"`
}

// SAMLAssertion decodes a pasted SAMLResponse (base64) or raw XML (unverified).
func (p Preview) SAMLAssertion(ctx context.Context, cfg SAMLConfig, payload string) (PreviewResult, error) {
	r := newResult()
	payload = strings.TrimSpace(payload)
	if payload == "" || len(payload) > 2<<20 {
		r.Problems = append(r.Problems, "paste a SAMLResponse (base64) or the XML")
		return r, nil
	}
	data := []byte(payload)
	if !strings.HasPrefix(payload, "<") {
		dec, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(payload), ""))
		if err != nil {
			r.Problems = append(r.Problems, "payload is neither XML nor base64")
			return r, nil
		}
		data = dec
	}
	lower := bytes.ToLower(data)
	if bytes.Contains(lower, []byte("<!doctype")) || bytes.Contains(lower, []byte("<!entity")) {
		r.Problems = append(r.Problems, "DOCTYPE and entity declarations are not allowed")
		return r, nil
	}
	var doc samlDoc
	if err := xml.Unmarshal(data, &doc); err != nil || doc.Assertion.Subject.NameID == "" {
		r.Problems = append(r.Problems, "no Assertion/Subject/NameID found")
		return r, nil
	}
	r.Warnings = append(r.Warnings, "signature was not verified; this is a mapping preview, not a login check")
	issuer := strings.TrimSpace(doc.Assertion.Issuer)
	if issuer == "" {
		issuer = strings.TrimSpace(doc.Issuer)
	}
	r.Subject.Issuer = issuer
	r.Subject.Subject = strings.TrimSpace(doc.Assertion.Subject.NameID)
	r.Subject.Audience = doc.Assertion.Conditions.Audience
	if issuer != cfg.IDPEntityID {
		r.Problems = append(r.Problems, "Issuer does not match the configured IdP entity ID")
	}
	aud := false
	for _, a := range r.Subject.Audience {
		aud = aud || strings.TrimSpace(a) == cfg.EntityID
	}
	if !aud {
		r.Problems = append(r.Problems, "Audience does not include this service provider's entity ID")
	}
	groupsAttr := cfg.GroupsAttribute
	for _, a := range doc.Assertion.Attributes {
		switch {
		case groupsAttr != "" && a.Name == groupsAttr:
			r.Subject.Groups = append(r.Subject.Groups, a.Values...)
		case cfg.EmailAttribute != "" && a.Name == cfg.EmailAttribute && len(a.Values) > 0:
			r.Subject.Email = a.Values[0]
		}
	}
	if groupsAttr == "" {
		r.Warnings = append(r.Warnings, "no groups_attribute configured; groups are not extracted")
	}
	now := p.now()
	if t, err := time.Parse(time.RFC3339, doc.Assertion.Conditions.NotOnOrAfter); err != nil || !now.Before(t) {
		r.Problems = append(r.Problems, "assertion is expired or has no valid NotOnOrAfter (paste a fresh response)")
	}
	if t, err := time.Parse(time.RFC3339, doc.Assertion.Conditions.NotBefore); err == nil {
		d := int64(t.Sub(now) / time.Second)
		r.ClockSkewSeconds = &d
		if t.After(now.Add(SkewWarning)) {
			r.Problems = append(r.Problems, fmt.Sprintf("NotBefore is %ds in the future: the IdP clock is ahead of this server's; real sign-ins would be rejected", d))
		}
	}
	return p.finish(ctx, &r)
}

// LDAPUser looks a login up in the directory (no password) and resolves the
// immutable subject, groups and proposed roles.
func (p Preview) LDAPUser(ctx context.Context, dir *directory.Provider, providerID, login string) (PreviewResult, error) {
	r := newResult()
	ext, err := dir.Lookup(ctx, login)
	switch {
	case errors.Is(err, directory.ErrAuthentication):
		r.Problems = append(r.Problems, "no unique user matched that login, or the user has no immutable ID attribute")
		return r, nil
	case errors.Is(err, directory.ErrGroups):
		r.Problems = append(r.Problems, "group resolution failed (cycle, depth or size limit); sign-in would be refused")
		return r, nil
	case err != nil:
		r.Problems = append(r.Problems, "directory unreachable; run the connectivity test")
		return r, nil
	}
	r.Subject = Subject{Issuer: providerID, Subject: ext.Subject, Groups: ext.GroupIDs}
	if r.Subject.Groups == nil {
		r.Subject.Groups = []string{}
	}
	r.ProposedRoles = ext.RoleIDs
	r.Verified = true
	r.Warnings = append(r.Warnings, "LDAP mapping proposals are informational; effective roles come from the identity binding")
	return p.finish(ctx, &r)
}
