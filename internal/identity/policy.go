package identity

type ResourceKind string

const (
	ArtifactResource ResourceKind = "artifact"
	FeedResource     ResourceKind = "feed"
	SourceResource   ResourceKind = "source"
	InstanceResource ResourceKind = "instance"
)

type Resource struct {
	InstanceID, FeedID, SourceID, Namespace string
	Kind                                    ResourceKind
}
type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

// Grant selectors are exact, except an empty feed/namespace selects all values.
// Namespaces do not imply descendants: "team" never matches "team/child".
// Kind and instance are always required and exact. Admin is a distinct action,
// not a wildcard; feed administration does not confer source administration.
type Grant struct {
	PrincipalID                             string
	Anonymous                               bool
	InstanceID, FeedID, SourceID, Namespace string
	Kind                                    ResourceKind
	Actions                                 []Action
	Effect                                  Effect
}

// Role memberships come from trusted identity/group resolution, never tokens.
// A role grant's PrincipalID and Anonymous selectors must be empty/false.
type Role struct {
	Name string
	// Origin is local or federation (federation-managed roles, P2-M2).
	Origin       string
	PrincipalIDs []string
	Grants       []Grant
}
type Policy struct {
	Grants []Grant
	Roles  []Role
}

// Authorize intersects effective grants with machine-token scopes. Every matching
// deny wins, including one from another role. A zero principal is anonymous and
// may only read resources explicitly granted Anonymous read access.
func (p Policy) Authorize(principal Principal, resource Resource, action Action) bool {
	if resource.InstanceID == "" || !validAction(action) || !validKind(resource.Kind) {
		return false
	}
	anonymous := principal.PrincipalID == ""
	if anonymous {
		if principal.InstanceID != "" || principal.TokenID != "" || len(principal.Scopes) != 0 || action != Read {
			return false
		}
	} else if principal.InstanceID != resource.InstanceID {
		return false
	}
	allowed := false
	consider := func(g Grant) bool {
		if !matchesGrant(g, resource, action) {
			return true
		}
		if g.Effect == Deny {
			return false
		}
		if g.Effect == Allow {
			allowed = true
		}
		return true
	}
	for _, g := range p.Grants {
		applies := g.Anonymous && anonymous && g.PrincipalID == ""
		if !anonymous {
			applies = !g.Anonymous && g.PrincipalID == principal.PrincipalID
		}
		if applies && !consider(g) {
			return false
		}
	}
	if !anonymous {
		for _, role := range p.Roles {
			if !contains(role.PrincipalIDs, principal.PrincipalID) {
				continue
			}
			for _, g := range role.Grants {
				if g.PrincipalID == "" && !g.Anonymous && !consider(g) {
					return false
				}
			}
		}
	}
	if !allowed {
		return false
	}
	if principal.Delegated && principal.Scopes == nil {
		return true // asserted browser session: no token narrowing
	}
	if principal.TokenID == "" && !principal.Delegated {
		return len(principal.Scopes) == 0
	}
	for _, scope := range principal.Scopes {
		if (scope.FeedID == "" || scope.FeedID == resource.FeedID) && (scope.Namespace == "" || scope.Namespace == resource.Namespace) && hasAction(scope.Actions, action) {
			return true
		}
	}
	return false
}
func matchesGrant(g Grant, r Resource, a Action) bool {
	return g.InstanceID == r.InstanceID && g.Kind == r.Kind && (g.SourceID == "" || g.SourceID == r.SourceID) && (g.FeedID == "" || g.FeedID == r.FeedID) && (g.Namespace == "" || g.Namespace == r.Namespace) && hasAction(g.Actions, a)
}
func validAction(a Action) bool {
	return a == Read || a == Publish || a == Delete || a == Configure || a == Replicate || a == Admin
}
func validKind(k ResourceKind) bool {
	return k == ArtifactResource || k == FeedResource || k == SourceResource || k == InstanceResource
}
func hasAction(actions []Action, a Action) bool {
	for _, candidate := range actions {
		if candidate == a {
			return true
		}
	}
	return false
}
func contains(values []string, v string) bool {
	for _, candidate := range values {
		if candidate == v {
			return true
		}
	}
	return false
}

// CanSeeFeed reports whether the principal may see a feed's metadata: it holds
// feed-level read, or it may read or publish artifacts in the feed within at
// least one namespace. The data plane (for example OCI) authorizes artifact
// resources, whose grants and token scopes may be namespace-limited, so a
// feed-kind check alone would hide a feed the caller can already push to.
// Nothing is widened: every candidate goes through Authorize, so denies, the
// instance check and token-scope intersection all still apply, and a matching
// feed-level deny hides the feed regardless of artifact grants.
func (p Policy) CanSeeFeed(principal Principal, instanceID, feedID string) bool {
	feed := Resource{InstanceID: instanceID, FeedID: feedID, Kind: FeedResource}
	if p.Authorize(principal, feed, Read) {
		return true
	}
	if p.denied(principal, feed, Read) {
		return false
	}
	namespaces := map[string]struct{}{"": {}}
	add := func(g Grant) {
		if g.Effect == Allow && g.Kind == ArtifactResource && g.InstanceID == instanceID && (g.FeedID == "" || g.FeedID == feedID) {
			namespaces[g.Namespace] = struct{}{}
		}
	}
	for _, g := range p.Grants {
		add(g)
	}
	for _, role := range p.Roles {
		for _, g := range role.Grants {
			add(g)
		}
	}
	for _, s := range principal.Scopes {
		namespaces[s.Namespace] = struct{}{}
	}
	for ns := range namespaces {
		artifact := Resource{InstanceID: instanceID, FeedID: feedID, Namespace: ns, Kind: ArtifactResource}
		if p.Authorize(principal, artifact, Read) || p.Authorize(principal, artifact, Publish) {
			return true
		}
	}
	return false
}

// denied reports whether a deny grant applicable to the principal matches.
func (p Policy) denied(principal Principal, r Resource, a Action) bool {
	anonymous := principal.PrincipalID == ""
	for _, g := range p.Grants {
		applies := g.Anonymous && anonymous && g.PrincipalID == ""
		if !anonymous {
			applies = !g.Anonymous && g.PrincipalID == principal.PrincipalID
		}
		if applies && g.Effect == Deny && matchesGrant(g, r, a) {
			return true
		}
	}
	if !anonymous {
		for _, role := range p.Roles {
			if !contains(role.PrincipalIDs, principal.PrincipalID) {
				continue
			}
			for _, g := range role.Grants {
				if g.PrincipalID == "" && !g.Anonymous && g.Effect == Deny && matchesGrant(g, r, a) {
					return true
				}
			}
		}
	}
	return false
}
