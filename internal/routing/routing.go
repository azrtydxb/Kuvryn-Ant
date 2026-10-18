// Package routing resolves publish destinations before any content is accepted.
// Authorization against the selected source remains a separate service concern.
package routing

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxRules = 256
const MaxSources = 1024
const MaxPathBytes = 2048
const MaxSelectorBytes = 256

type SourceKind string

const (
	Local   SourceKind = "local"
	Remote  SourceKind = "remote"
	Replica SourceKind = "replica"
)

type Source struct {
	ID   string
	Kind SourceKind
}

// Selector dimensions are ANDed. Empty fields are unconstrained, including an
// empty PathPrefix; an entirely empty Selector matches every valid request.
// Nonempty namespace/package/principal selectors are exact, case-sensitive
// identities, already canonicalized by the relevant domain or protocol adapter.
// PathPrefix uses segment boundaries: /team matches /team and /team/a, not
// /teammate. A / prefix matches any present path, but never an absent path.
type Selector struct{ Namespace, Package, PathPrefix, Principal string }
type Rule struct {
	ID       string
	Priority int
	Selector Selector
	Target   string
	Deny     bool
}
type Config struct {
	Sources       []Source
	Rules         []Rule
	DefaultTarget string
}

// Request intentionally has no credential fields. Empty namespace/package/
// principal means absent and cannot match a nonempty exact selector. Empty Path
// means absent; nonempty paths use NormalizePath's canonicalization rules.
type Request struct{ Namespace, Package, Path, Principal string }

type Reason string

const (
	SelectedRule       Reason = "selected_rule"
	DefaultDestination Reason = "default_destination"
	DeniedByRule       Reason = "denied_by_rule"
	NoDestination      Reason = "no_destination"
	InvalidInput       Reason = "invalid_input"
	NamespaceMismatch  Reason = "namespace_mismatch"
	PackageMismatch    Reason = "package_mismatch"
	PrincipalMismatch  Reason = "principal_mismatch"
	PathMissing        Reason = "path_missing"
	PathMismatch       Reason = "path_mismatch"
	LowerPriority      Reason = "lower_priority"
	StableTieBreak     Reason = "stable_tie_break"
	DenyOverride       Reason = "deny_override"
)

// Trace contains only configured identifiers and fixed reason codes, never
// request selectors or credentials. Callers still apply permission-aware
// redaction before exposing configured rule/source identifiers to clients.
type Trace struct {
	RuleID            string
	Priority          int
	Matched, Selected bool
	Reason            Reason
}
type Decision struct {
	Allowed          bool
	TargetID, RuleID string
	Reason           Reason
	Trace            []Trace
}

var ErrInvalidRequest = errors.New("invalid publish routing request")
var ErrInvalidPath = errors.New("invalid publish path")

type ConfigError struct{ Code, RuleID, OtherRuleID string }

func (e *ConfigError) Error() string {
	return fmt.Sprintf("invalid publish routing configuration: %s", e.Code)
}

// Router is an immutable compiled policy, safe for concurrent Resolve calls.
// Compilation rejects non-local write destinations, including defaults, even
// when a rule would otherwise be unreachable. Overlapping allow rules at equal
// priority may only name the same destination. Denies have no target and always
// override allows regardless of priority, so deny/allow overlap is intentional.
type Router struct {
	rules         []Rule
	defaultTarget string
}

func Compile(config Config) (*Router, error) {
	fail := func(code, id, other string) (*Router, error) { return nil, &ConfigError{code, id, other} }
	if len(config.Rules) > MaxRules || len(config.Sources) > MaxSources {
		return fail("policy_limit", "", "")
	}
	sources := make(map[string]SourceKind, len(config.Sources))
	for _, source := range config.Sources {
		if !validID(source.ID) {
			return fail("invalid_source_id", "", "")
		}
		if _, exists := sources[source.ID]; exists {
			return fail("duplicate_source_id", "", "")
		}
		if source.Kind != Local && source.Kind != Remote && source.Kind != Replica {
			return fail("invalid_source_kind", "", "")
		}
		sources[source.ID] = source.Kind
	}
	if config.DefaultTarget != "" && sources[config.DefaultTarget] != Local {
		return fail("invalid_default_target", "", "")
	}
	router := &Router{defaultTarget: config.DefaultTarget, rules: append([]Rule(nil), config.Rules...)}
	ids := map[string]bool{}
	for i := range router.rules {
		rule := &router.rules[i]
		if !validID(rule.ID) {
			return fail("invalid_rule_id", "", "")
		}
		if ids[rule.ID] {
			return fail("duplicate_rule_id", rule.ID, "")
		}
		ids[rule.ID] = true
		if !validExact(rule.Selector.Namespace) || !validExact(rule.Selector.Package) || !validExact(rule.Selector.Principal) {
			return fail("invalid_selector", rule.ID, "")
		}
		if rule.Selector.PathPrefix != "" {
			normalized, err := NormalizePath(rule.Selector.PathPrefix)
			if err != nil {
				return fail("invalid_path_prefix", rule.ID, "")
			}
			rule.Selector.PathPrefix = normalized
		}
		if rule.Deny {
			if rule.Target != "" {
				return fail("deny_has_target", rule.ID, "")
			}
		} else if sources[rule.Target] != Local {
			return fail("invalid_rule_target", rule.ID, "")
		}
	}
	// Canonical ordering makes explanations and ties independent of config order.
	sort.Slice(router.rules, func(i, j int) bool {
		a, b := router.rules[i], router.rules[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		return a.ID < b.ID
	})
	for i, a := range router.rules {
		if a.Deny {
			continue
		}
		for _, b := range router.rules[i+1:] {
			if b.Priority != a.Priority {
				break
			}
			if !b.Deny && a.Target != b.Target && intersects(a.Selector, b.Selector) {
				return fail("ambiguous_equal_priority", a.ID, b.ID)
			}
		}
	}
	return router, nil
}

func (r *Router) Resolve(request Request) (Decision, error) {
	rejected := Decision{Reason: InvalidInput}
	if r == nil || !validExact(request.Namespace) || !validExact(request.Package) || !validExact(request.Principal) {
		return rejected, ErrInvalidRequest
	}
	if request.Path != "" {
		normalized, err := NormalizePath(request.Path)
		if err != nil {
			return rejected, errors.Join(ErrInvalidRequest, err)
		}
		request.Path = normalized
	}
	result := Decision{Trace: make([]Trace, len(r.rules))}
	winner := -1
	denier := -1
	for i, rule := range r.rules {
		reason := mismatch(rule.Selector, request)
		matched := reason == ""
		result.Trace[i] = Trace{RuleID: rule.ID, Priority: rule.Priority, Matched: matched, Reason: reason}
		if matched {
			if rule.Deny {
				if denier < 0 {
					denier = i
				}
			} else if winner < 0 {
				winner = i
			}
		}
	}
	if denier >= 0 {
		winner = denier
		result.Reason = DeniedByRule
	} else if winner >= 0 {
		result.Allowed = true
		result.TargetID = r.rules[winner].Target
		result.Reason = SelectedRule
	} else if r.defaultTarget != "" {
		result.Allowed = true
		result.TargetID = r.defaultTarget
		result.Reason = DefaultDestination
	} else {
		result.Reason = NoDestination
	}
	if winner >= 0 {
		selected := r.rules[winner]
		result.RuleID = selected.ID
		for i := range result.Trace {
			trace := &result.Trace[i]
			if !trace.Matched {
				continue
			}
			if i == winner {
				trace.Selected = true
				trace.Reason = result.Reason
			} else if denier >= 0 && !r.rules[i].Deny {
				trace.Reason = DenyOverride
			} else if trace.Priority == selected.Priority {
				trace.Reason = StableTieBreak
			} else {
				trace.Reason = LowerPriority
			}
		}
	}
	return result, nil
}

func mismatch(selector Selector, request Request) Reason {
	if selector.Namespace != "" && selector.Namespace != request.Namespace {
		return NamespaceMismatch
	}
	if selector.Package != "" && selector.Package != request.Package {
		return PackageMismatch
	}
	if selector.Principal != "" && selector.Principal != request.Principal {
		return PrincipalMismatch
	}
	if selector.PathPrefix != "" {
		if request.Path == "" {
			return PathMissing
		}
		if !containsPath(selector.PathPrefix, request.Path) {
			return PathMismatch
		}
	}
	return ""
}
func intersects(a, b Selector) bool {
	if !exactIntersects(a.Namespace, b.Namespace) || !exactIntersects(a.Package, b.Package) || !exactIntersects(a.Principal, b.Principal) {
		return false
	}
	return a.PathPrefix == "" || b.PathPrefix == "" || containsPath(a.PathPrefix, b.PathPrefix) || containsPath(b.PathPrefix, a.PathPrefix)
}
func exactIntersects(a, b string) bool { return a == "" || b == "" || a == b }
func containsPath(prefix, path string) bool {
	return prefix == "/" || path == prefix || strings.HasPrefix(path, prefix+"/")
}
func validExact(value string) bool {
	return len(value) <= MaxSelectorBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}
func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

// NormalizePath accepts an absolute slash path, not a URL or filesystem path.
// It decodes each percent-encoded segment once, rejects escaped separators,
// residual percent signs (ambiguous double encoding), dot/empty segments,
// backslashes, controls and invalid UTF-8. One trailing slash is normalized away;
// / remains root. Empty is invalid here; Request/Selector separately use empty
// to mean absent/unconstrained. Query/fragment delimiters are not path syntax.
func NormalizePath(raw string) (string, error) {
	if raw == "" || len(raw) > MaxPathBytes || !strings.HasPrefix(raw, "/") || strings.ContainsAny(raw, "\\?#") || !utf8.ValidString(raw) {
		return "", ErrInvalidPath
	}
	if raw == "/" {
		return raw, nil
	}
	path := strings.TrimSuffix(raw, "/")
	segments := strings.Split(path[1:], "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\%?#") || !utf8.ValidString(decoded) || strings.ContainsFunc(decoded, unicode.IsControl) {
			return "", ErrInvalidPath
		}
		segments[i] = decoded
	}
	return "/" + strings.Join(segments, "/"), nil
}
