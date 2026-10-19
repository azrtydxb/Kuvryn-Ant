package federation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrEpochUnknown reports a policy document signed for an authority epoch the
// applied membership does not certify yet (membership must sync first).
var ErrEpochUnknown = errors.New("federation: policy document epoch is not certified by the applied membership")

// IdentityDomain is a trusted identity issuer. Members keep their own IdP
// client configuration (secrets stay local) but must use the same issuer
// identifier for federated bindings to apply.
type IdentityDomain struct {
	ID     string `json:"id"`
	Type   string `json:"type"` // oidc | saml | ldap
	Issuer string `json:"issuer"`
}

// PolicyGrant is a grant on a portable feed key. An empty feed key on an
// artifact or feed grant selects every feed of the member, like a local grant
// without a feed: such an allow is inert unless the member accepts admin
// grants (rule 4). A key that is not bound locally is inert, never a wildcard.
type PolicyGrant struct {
	Kind      string   `json:"kind"`   // artifact | feed | instance
	Effect    string   `json:"effect"` // allow | deny
	FeedKey   string   `json:"feed_key,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Actions   []string `json:"actions"`
}

// PolicyRole is a federated role. Keys always start with "fed:".
type PolicyRole struct {
	Key      string        `json:"key"`
	Revision int64         `json:"revision"`
	Grants   []PolicyGrant `json:"grants"`
}

// PrincipalBinding grants roles to one federated principal.
type PrincipalBinding struct {
	Domain  string   `json:"domain"`
	Subject string   `json:"subject"`
	Roles   []string `json:"roles"`
}

// GroupBinding grants roles to members of an IdP group, evaluated at sign-in.
type GroupBinding struct {
	Domain  string   `json:"domain"`
	GroupID string   `json:"group_id"`
	Roles   []string `json:"roles"`
}

// ServiceAccount is a federated service principal. Tokens stay instance-local.
type ServiceAccount struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// Revocation disables a federated principal (domain + subject) or a federated
// service account (whose local tokens are revoked).
type Revocation struct {
	Domain         string `json:"domain,omitempty"`
	Subject        string `json:"subject,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
	EffectiveAt    string `json:"effective_at,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// MemberOverride records what a member reported about its local settings.
// It is informational: the authority can never change local settings.
type MemberOverride struct {
	AcceptAdminGrants bool `json:"accept_admin_grants"`
}

// PolicyBody is the editable content of a policy document.
type PolicyBody struct {
	IdentityDomains   []IdentityDomain          `json:"identity_domains"`
	Roles             []PolicyRole              `json:"roles"`
	PrincipalBindings []PrincipalBinding        `json:"principal_bindings"`
	GroupBindings     []GroupBinding            `json:"group_bindings"`
	ServiceAccounts   []ServiceAccount          `json:"service_accounts"`
	Revocations       []Revocation              `json:"revocations"`
	MemberOverrides   map[string]MemberOverride `json:"member_overrides"`
}

// PolicyDocument is the signed, versioned policy document (§6.2). It uses the
// same envelope as membership: JCS canonical form, Ed25519 signature by the
// authority key of its epoch, hash chain over generations.
type PolicyDocument struct {
	Kind           string `json:"kind"`
	FederationID   string `json:"federation_id"`
	FederationName string `json:"federation_name"`
	Generation     int64  `json:"generation"`
	AuthorityEpoch int64  `json:"authority_epoch"`
	PrevHash       string `json:"prev_hash"`
	PolicyBody
	IssuedAt  string     `json:"issued_at"`
	IssuedBy  string     `json:"issued_by"`
	Signature *Signature `json:"signature,omitempty"`
}

// Normalize replaces nil collections with empty ones so the signed form is
// stable however the body was produced.
func (b *PolicyBody) Normalize() {
	if b.IdentityDomains == nil {
		b.IdentityDomains = []IdentityDomain{}
	}
	if b.Roles == nil {
		b.Roles = []PolicyRole{}
	}
	for i := range b.Roles {
		if b.Roles[i].Grants == nil {
			b.Roles[i].Grants = []PolicyGrant{}
		}
		if b.Roles[i].Revision == 0 {
			b.Roles[i].Revision = 1
		}
	}
	if b.PrincipalBindings == nil {
		b.PrincipalBindings = []PrincipalBinding{}
	}
	if b.GroupBindings == nil {
		b.GroupBindings = []GroupBinding{}
	}
	if b.ServiceAccounts == nil {
		b.ServiceAccounts = []ServiceAccount{}
	}
	for i := range b.ServiceAccounts {
		if b.ServiceAccounts[i].Roles == nil {
			b.ServiceAccounts[i].Roles = []string{}
		}
	}
	if b.Revocations == nil {
		b.Revocations = []Revocation{}
	}
	if b.MemberOverrides == nil {
		b.MemberOverrides = map[string]MemberOverride{}
	}
}

// Domain returns an identity domain by ID.
func (b *PolicyBody) Domain(id string) (IdentityDomain, bool) {
	for _, d := range b.IdentityDomains {
		if d.ID == id {
			return d, true
		}
	}
	return IdentityDomain{}, false
}

// DomainByIssuer returns the identity domain of an issuer.
func (b *PolicyBody) DomainByIssuer(issuer string) (IdentityDomain, bool) {
	for _, d := range b.IdentityDomains {
		if d.Issuer == issuer {
			return d, true
		}
	}
	return IdentityDomain{}, false
}

var (
	domainPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	roleKeyPattern = regexp.MustCompile(`^fed:[a-z0-9][a-z0-9._-]{0,62}$`)
	slugPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	saPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
)

// ServiceAccountDomain is the reserved assertion domain of federated service
// accounts (issuer federation:<federation_id>).
const ServiceAccountDomain = "federation"

// ServiceAccountIssuer is the principals.issuer of federated service accounts.
func ServiceAccountIssuer(federationID string) string { return "federation:" + federationID }

// ValidFeedKey reports whether key is <federation>/<feed-slug> for this federation.
func ValidFeedKey(federationName, key string) bool {
	fed, slug, ok := strings.Cut(key, "/")
	return ok && fed == federationName && slugPattern.MatchString(slug)
}

func cleanText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

var validActions = map[string]bool{"read": true, "publish": true, "delete": true, "configure": true, "replicate": true, "admin": true}

// ValidatePolicy checks a policy body for the named federation and returns a
// client-safe message for the first problem.
func ValidatePolicy(federationName string, b *PolicyBody) error {
	bad := func(format string, args ...any) error { return invalid(fmt.Sprintf(format, args...)) }
	if len(b.IdentityDomains) > 32 || len(b.Roles) > 512 || len(b.PrincipalBindings) > 10000 || len(b.GroupBindings) > 4096 ||
		len(b.ServiceAccounts) > 1024 || len(b.Revocations) > 10000 || len(b.MemberOverrides) > 256 {
		return bad("policy exceeds size limits")
	}
	domains, issuers := map[string]bool{}, map[string]bool{}
	for _, d := range b.IdentityDomains {
		if !domainPattern.MatchString(d.ID) || d.ID == ServiceAccountDomain {
			return bad("identity domain id %q is invalid or reserved", d.ID)
		}
		if d.Type != "oidc" && d.Type != "saml" && d.Type != "ldap" {
			return bad("identity domain %s: type must be oidc, saml or ldap", d.ID)
		}
		if !cleanText(d.Issuer, 2048) || strings.HasPrefix(d.Issuer, "federation:") || d.Issuer == "local" || d.Issuer == "ant-internal" {
			return bad("identity domain %s: issuer is invalid or reserved", d.ID)
		}
		if domains[d.ID] || issuers[d.Issuer] {
			return bad("identity domain %s: duplicate id or issuer", d.ID)
		}
		domains[d.ID], issuers[d.Issuer] = true, true
	}
	roles := map[string]bool{}
	for _, r := range b.Roles {
		if !roleKeyPattern.MatchString(r.Key) || roles[r.Key] {
			return bad("role key %q must match ^fed:[a-z0-9][a-z0-9._-]{0,62}$ and be unique", r.Key)
		}
		roles[r.Key] = true
		if r.Revision < 1 || len(r.Grants) > 256 {
			return bad("role %s: revision must be positive and grants at most 256", r.Key)
		}
		for _, g := range r.Grants {
			if g.Kind != "artifact" && g.Kind != "feed" && g.Kind != "instance" {
				return bad("role %s: grant kind must be artifact, feed or instance (source grants are instance-local)", r.Key)
			}
			if g.Effect != "allow" && g.Effect != "deny" {
				return bad("role %s: grant effect must be allow or deny", r.Key)
			}
			if g.FeedKey != "" && (g.Kind == "instance" || !ValidFeedKey(federationName, g.FeedKey)) {
				return bad("role %s: feed_key must be %s/<feed-slug> on artifact or feed grants", r.Key, federationName)
			}
			if g.Namespace != "" && (g.Kind != "artifact" || !cleanText(g.Namespace, 200)) {
				return bad("role %s: namespace is only valid on artifact grants (at most 200 bytes)", r.Key)
			}
			if len(g.Actions) < 1 || len(g.Actions) > 6 {
				return bad("role %s: grants need 1 to 6 actions", r.Key)
			}
			seen := map[string]bool{}
			for _, a := range g.Actions {
				if !validActions[a] || seen[a] {
					return bad("role %s: invalid or repeated action %q", r.Key, a)
				}
				seen[a] = true
			}
		}
	}
	checkRoles := func(where string, list []string) error {
		if len(list) == 0 || len(list) > 64 {
			return bad("%s: 1 to 64 roles required", where)
		}
		seen := map[string]bool{}
		for _, k := range list {
			if !roles[k] || seen[k] {
				return bad("%s: unknown or repeated role %q", where, k)
			}
			seen[k] = true
		}
		return nil
	}
	pairs := map[string]bool{}
	for _, p := range b.PrincipalBindings {
		if !domains[p.Domain] || !cleanText(p.Subject, 512) {
			return bad("principal binding: unknown domain %q or invalid subject", p.Domain)
		}
		if pairs[p.Domain+"\x00"+p.Subject] {
			return bad("principal binding %s/%s is duplicated", p.Domain, p.Subject)
		}
		pairs[p.Domain+"\x00"+p.Subject] = true
		if err := checkRoles("principal binding "+p.Domain+"/"+p.Subject, p.Roles); err != nil {
			return err
		}
	}
	groups := map[string]bool{}
	for _, g := range b.GroupBindings {
		if !domains[g.Domain] || !cleanText(g.GroupID, 512) {
			return bad("group binding: unknown domain %q or invalid group_id", g.Domain)
		}
		if groups[g.Domain+"\x00"+g.GroupID] {
			return bad("group binding %s/%s is duplicated", g.Domain, g.GroupID)
		}
		groups[g.Domain+"\x00"+g.GroupID] = true
		if err := checkRoles("group binding "+g.Domain+"/"+g.GroupID, g.Roles); err != nil {
			return err
		}
	}
	accounts := map[string]bool{}
	for _, s := range b.ServiceAccounts {
		if !saPattern.MatchString(s.Name) || accounts[s.Name] {
			return bad("service account name %q is invalid or duplicated", s.Name)
		}
		accounts[s.Name] = true
		if len(s.Roles) > 0 {
			if err := checkRoles("service account "+s.Name, s.Roles); err != nil {
				return err
			}
		}
	}
	for _, r := range b.Revocations {
		switch {
		case r.ServiceAccount != "" && r.Domain == "" && r.Subject == "":
			if !saPattern.MatchString(r.ServiceAccount) {
				return bad("revocation: invalid service account %q", r.ServiceAccount)
			}
		case r.ServiceAccount == "" && domains[r.Domain] && cleanText(r.Subject, 512):
		default:
			return bad("revocation: set domain and subject, or service_account")
		}
		if r.EffectiveAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, r.EffectiveAt); err != nil {
				return bad("revocation: effective_at must be RFC 3339")
			}
		}
		if len(r.Reason) > 512 || (r.Reason != "" && !utf8.ValidString(r.Reason)) {
			return bad("revocation: reason too long")
		}
	}
	for id := range b.MemberOverrides {
		if !uuidPattern.MatchString(id) {
			return bad("member_overrides keys must be member instance IDs")
		}
	}
	return nil
}

// --- envelope ---

func policySigningPayload(d *PolicyDocument) ([]byte, error) {
	c := *d
	c.Signature = nil
	return CanonicalJSON(&c)
}

// PolicyHash is the chain hash of a signed policy document.
func PolicyHash(d *PolicyDocument) string {
	b, err := CanonicalJSON(d)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SignPolicy sets the authority signature on a policy document.
func SignPolicy(d *PolicyDocument, authority ed25519.PrivateKey) error {
	payload, err := policySigningPayload(d)
	if err != nil {
		return err
	}
	d.Signature = &Signature{KeyID: fmt.Sprintf("authority-epoch-%d", d.AuthorityEpoch), Alg: "Ed25519", Value: sign(authority, payload)}
	return nil
}

// VerifyPolicy checks a policy document against the applied policy (nil when
// none is applied yet) and the applied membership, which certifies the
// authority key of each epoch. It enforces structure, federation identity,
// epoch and generation ordering, the authority signature and the hash chain.
func VerifyPolicy(doc, applied *PolicyDocument, membership *Membership) error {
	if membership == nil {
		return ErrEpochUnknown
	}
	if doc.Kind != "policy" || !uuidPattern.MatchString(doc.FederationID) || doc.Generation < 1 || doc.AuthorityEpoch < 1 ||
		doc.Signature == nil || doc.Signature.Alg != "Ed25519" || doc.Signature.KeyID != fmt.Sprintf("authority-epoch-%d", doc.AuthorityEpoch) ||
		(doc.Generation == 1) != (doc.PrevHash == "") {
		return ErrMalformed
	}
	if _, err := time.Parse(time.RFC3339Nano, doc.IssuedAt); err != nil {
		return ErrMalformed
	}
	if doc.FederationID != membership.FederationID || doc.FederationName != membership.FederationName {
		return ErrWrongFed
	}
	if ValidatePolicy(doc.FederationName, &doc.PolicyBody) != nil {
		return ErrMalformed
	}
	if applied != nil {
		if doc.AuthorityEpoch < applied.AuthorityEpoch {
			return ErrStaleEpoch
		}
		if doc.Generation <= applied.Generation {
			return ErrStaleGen
		}
	} else if doc.Generation != 1 {
		return ErrGap
	}
	switch {
	case doc.AuthorityEpoch < membership.AuthorityEpoch:
		return ErrStaleEpoch
	case doc.AuthorityEpoch > membership.AuthorityEpoch:
		return ErrEpochUnknown
	}
	payload, err := policySigningPayload(doc)
	if err != nil {
		return ErrMalformed
	}
	if !verify(membership.Authority.PublicKey, doc.Signature.Value, payload) {
		return ErrSignature
	}
	if applied != nil {
		if doc.Generation != applied.Generation+1 {
			return ErrGap
		}
		if doc.PrevHash != PolicyHash(applied) {
			return ErrChain
		}
	}
	return nil
}

// --- diff ---

// SectionDiff lists keys added, removed and changed in one policy section.
type SectionDiff struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

// PolicyDiff is a structured comparison of two policy bodies.
type PolicyDiff struct {
	FromGeneration    int64       `json:"from_generation"`
	ToGeneration      int64       `json:"to_generation"`
	IdentityDomains   SectionDiff `json:"identity_domains"`
	Roles             SectionDiff `json:"roles"`
	PrincipalBindings SectionDiff `json:"principal_bindings"`
	GroupBindings     SectionDiff `json:"group_bindings"`
	ServiceAccounts   SectionDiff `json:"service_accounts"`
	Revocations       SectionDiff `json:"revocations"`
	Changes           int         `json:"changes"`
}

func sectionDiff[T any](from, to []T, key func(T) string) SectionDiff {
	a, b := map[string]string{}, map[string]string{}
	for _, x := range from {
		c, _ := CanonicalJSON(x)
		a[key(x)] = string(c)
	}
	for _, x := range to {
		c, _ := CanonicalJSON(x)
		b[key(x)] = string(c)
	}
	out := SectionDiff{Added: []string{}, Removed: []string{}, Changed: []string{}}
	for k, v := range b {
		if old, ok := a[k]; !ok {
			out.Added = append(out.Added, k)
		} else if old != v {
			out.Changed = append(out.Changed, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out.Removed = append(out.Removed, k)
		}
	}
	sort.Strings(out.Added)
	sort.Strings(out.Removed)
	sort.Strings(out.Changed)
	return out
}

// DiffPolicy compares two policy bodies section by section.
func DiffPolicy(from, to *PolicyBody) PolicyDiff {
	if from == nil {
		from = &PolicyBody{}
	}
	d := PolicyDiff{
		IdentityDomains:   sectionDiff(from.IdentityDomains, to.IdentityDomains, func(x IdentityDomain) string { return x.ID }),
		Roles:             sectionDiff(from.Roles, to.Roles, func(x PolicyRole) string { return x.Key }),
		PrincipalBindings: sectionDiff(from.PrincipalBindings, to.PrincipalBindings, func(x PrincipalBinding) string { return x.Domain + "/" + x.Subject }),
		GroupBindings:     sectionDiff(from.GroupBindings, to.GroupBindings, func(x GroupBinding) string { return x.Domain + "/" + x.GroupID }),
		ServiceAccounts:   sectionDiff(from.ServiceAccounts, to.ServiceAccounts, func(x ServiceAccount) string { return x.Name }),
		Revocations: sectionDiff(from.Revocations, to.Revocations, func(x Revocation) string {
			if x.ServiceAccount != "" {
				return "service_account/" + x.ServiceAccount
			}
			return x.Domain + "/" + x.Subject
		}),
	}
	for _, s := range []SectionDiff{d.IdentityDomains, d.Roles, d.PrincipalBindings, d.GroupBindings, d.ServiceAccounts, d.Revocations} {
		d.Changes += len(s.Added) + len(s.Removed) + len(s.Changed)
	}
	return d
}

// FederatedID is the stable federation-wide ID of a principal: derived from
// the federation, issuer and subject, so every member computes the same value
// and audit records correlate across members without a registry.
func FederatedID(federationID, issuer, subject string) string {
	sum := sha256.Sum256([]byte("kuvryn-ant/federated-principal/v1\n" + federationID + "\n" + issuer + "\n" + subject))
	sum[6] = (sum[6] & 0x0f) | 0x50 // version 5-style (name-based, SHA)
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
