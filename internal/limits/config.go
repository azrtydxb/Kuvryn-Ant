// Package limits implements request rate limiting and storage quotas
// (spec 6.3/6.5). Rate limits are in-memory token buckets applied by shared
// HTTP middleware; quotas are enforced inside the publish commit transaction.
package limits

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
)

// Rule limits and scopes.
const (
	MaxRules = 1000
	MaxCIDRs = 64

	ScopePrincipal = "principal"
	ScopeFeed      = "feed"
	ScopeNamespace = "namespace"
	ScopeSite      = "site"
)

var (
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

// RateRule limits requests for one principal, feed, namespace or site.
//
//   - principal: Subject is the principal ID.
//   - feed: Subject is the feed ID.
//   - namespace: Subject is the feed ID and Namespace the package namespace
//     (the first path segment below the feed in package-protocol URLs).
//   - site: Subject is a site label. CIDRs are the client networks (TCP peer
//     address) that belong to the site; an empty list matches every client,
//     which makes the rule an instance-wide limit labelled by Subject.
type RateRule struct {
	Scope             string   `json:"scope"`
	Subject           string   `json:"subject"`
	Namespace         string   `json:"namespace,omitempty"`
	CIDRs             []string `json:"cidrs,omitempty"`
	RequestsPerMinute int      `json:"requests_per_minute"`
	Burst             int      `json:"burst"`
}

// QuotaRule bounds the content of a feed (Scope feed) or of one namespace in a
// feed (Scope namespace). A nil bound is unlimited.
type QuotaRule struct {
	Scope       string `json:"scope"`
	FeedID      string `json:"feed_id"`
	Namespace   string `json:"namespace,omitempty"`
	MaxBytes    *int64 `json:"max_bytes,omitempty"`
	MaxPackages *int64 `json:"max_packages,omitempty"`
}

// Config is the persisted limits document of an instance.
type Config struct {
	RateLimits []RateRule  `json:"rate_limits"`
	Quotas     []QuotaRule `json:"quotas"`
}

func (r RateRule) key() string  { return r.Scope + "\x00" + r.Subject + "\x00" + r.Namespace }
func (q QuotaRule) key() string { return q.Scope + "\x00" + q.FeedID + "\x00" + q.Namespace }

func validText(v string, max int) bool {
	return len(v) <= max && strings.TrimSpace(v) == v && !strings.ContainsFunc(v, unicode.IsControl)
}

// Normalize validates the document and canonicalizes CIDRs. It never returns
// a partially valid configuration.
func Normalize(c Config) (Config, error) {
	if len(c.RateLimits) > MaxRules || len(c.Quotas) > MaxRules {
		return Config{}, fmt.Errorf("at most %d rate limits and %d quotas", MaxRules, MaxRules)
	}
	out := Config{RateLimits: make([]RateRule, 0, len(c.RateLimits)), Quotas: make([]QuotaRule, 0, len(c.Quotas))}
	seen := map[string]bool{}
	for i, r := range c.RateLimits {
		bad := func(msg string) (Config, error) { return Config{}, fmt.Errorf("rate_limits[%d]: %s", i, msg) }
		switch r.Scope {
		case ScopePrincipal, ScopeFeed:
			if !uuidPattern.MatchString(r.Subject) {
				return bad("subject must be a canonical UUID")
			}
			if r.Namespace != "" || len(r.CIDRs) > 0 {
				return bad("namespace and cidrs do not apply to this scope")
			}
		case ScopeNamespace:
			if !uuidPattern.MatchString(r.Subject) {
				return bad("subject must be the feed UUID")
			}
			if r.Namespace == "" || !validText(r.Namespace, 256) {
				return bad("namespace required (at most 256 bytes)")
			}
			if len(r.CIDRs) > 0 {
				return bad("cidrs do not apply to this scope")
			}
		case ScopeSite:
			if !labelPattern.MatchString(r.Subject) {
				return bad("site label must match [A-Za-z0-9][A-Za-z0-9_.-]{0,62}")
			}
			if r.Namespace != "" {
				return bad("namespace does not apply to this scope")
			}
			if len(r.CIDRs) > MaxCIDRs {
				return bad("too many cidrs")
			}
			cidrs := make([]string, 0, len(r.CIDRs))
			for _, raw := range r.CIDRs {
				p, err := netip.ParsePrefix(raw)
				if err != nil {
					return bad(fmt.Sprintf("invalid cidr %q", raw))
				}
				cidrs = append(cidrs, p.Masked().String())
			}
			r.CIDRs = cidrs
		default:
			return bad("scope must be principal, feed, namespace or site")
		}
		if r.RequestsPerMinute < 1 || r.RequestsPerMinute > 6_000_000 {
			return bad("requests_per_minute must be between 1 and 6000000")
		}
		if r.Burst < 1 || r.Burst > 1_000_000 {
			return bad("burst must be between 1 and 1000000")
		}
		if seen[r.key()] {
			return bad("duplicate rule for the same subject")
		}
		seen[r.key()] = true
		out.RateLimits = append(out.RateLimits, r)
	}
	seen = map[string]bool{}
	for i, q := range c.Quotas {
		bad := func(msg string) (Config, error) { return Config{}, fmt.Errorf("quotas[%d]: %s", i, msg) }
		switch q.Scope {
		case ScopeFeed:
			if q.Namespace != "" {
				return bad("namespace does not apply to feed quotas")
			}
		case ScopeNamespace:
			if q.Namespace == "" || !validText(q.Namespace, 256) {
				return bad("namespace required (at most 256 bytes)")
			}
		default:
			return bad("scope must be feed or namespace")
		}
		if !uuidPattern.MatchString(q.FeedID) {
			return bad("feed_id must be a canonical UUID")
		}
		if q.MaxBytes == nil && q.MaxPackages == nil {
			return bad("max_bytes or max_packages required")
		}
		if q.MaxBytes != nil && *q.MaxBytes < 0 || q.MaxPackages != nil && *q.MaxPackages < 0 {
			return bad("limits must not be negative")
		}
		if seen[q.key()] {
			return bad("duplicate quota for the same scope")
		}
		seen[q.key()] = true
		out.Quotas = append(out.Quotas, q)
	}
	return out, nil
}
