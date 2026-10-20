package identity

import "testing"

func TestPolicyIntersectionAndDenyWins(t *testing.T) {
	r := Resource{InstanceID: "instance", FeedID: "feed", Namespace: "team", Kind: ArtifactResource}
	principal := Principal{InstanceID: "instance", PrincipalID: "user", TokenID: "token", Scopes: []Scope{{FeedID: "feed", Namespace: "team", Actions: []Action{Read}}}}
	grant := Grant{InstanceID: "instance", Kind: ArtifactResource, Actions: []Action{Read, Publish}, Effect: Allow}
	policy := Policy{Roles: []Role{{Name: "reader", PrincipalIDs: []string{"user"}, Grants: []Grant{grant}}}}
	if !policy.Authorize(principal, r, Read) {
		t.Fatal("matching role and scope denied")
	}
	if policy.Authorize(principal, r, Publish) {
		t.Fatal("role bypassed token scope")
	}
	if (Policy{}).Authorize(principal, r, Read) {
		t.Fatal("token scope became a grant")
	}
	other := principal
	other.InstanceID = "other"
	if policy.Authorize(other, r, Read) {
		t.Fatal("cross-instance access")
	}
	child := r
	child.Namespace = "team/child"
	if policy.Authorize(principal, child, Read) {
		t.Fatal("exact namespace matched descendant")
	}
	deny := grant
	deny.Effect = Deny
	policy.Roles = append(policy.Roles, Role{Name: "denied", PrincipalIDs: []string{"user"}, Grants: []Grant{deny}})
	if policy.Authorize(principal, r, Read) {
		t.Fatal("role allow bypassed deny")
	}
}
func TestAdministrationAndAnonymousAreExplicit(t *testing.T) {
	r := Resource{InstanceID: "instance", FeedID: "feed", Kind: FeedResource}
	p := Principal{InstanceID: "instance", PrincipalID: "user"}
	grant := Grant{PrincipalID: "user", InstanceID: "instance", Kind: FeedResource, Actions: []Action{Configure}, Effect: Allow}
	policy := Policy{Grants: []Grant{grant}}
	if !policy.Authorize(p, r, Configure) {
		t.Fatal("explicit administration denied")
	}
	r.Kind = SourceResource
	if policy.Authorize(p, r, Configure) {
		t.Fatal("feed grant conferred source administration")
	}
	if policy.Authorize(Principal{}, r, Read) {
		t.Fatal("implicit anonymous access")
	}
	grant.PrincipalID = ""
	grant.Anonymous = true
	grant.Kind = SourceResource
	grant.Actions = []Action{Read, Publish}
	policy.Grants = append(policy.Grants, grant)
	if !policy.Authorize(Principal{}, r, Read) {
		t.Fatal("explicit anonymous read denied")
	}
	if policy.Authorize(Principal{}, r, Publish) {
		t.Fatal("anonymous write allowed")
	}
	grant.Effect = Deny
	policy.Grants = append(policy.Grants, grant)
	if policy.Authorize(Principal{}, r, Read) {
		t.Fatal("anonymous deny ignored")
	}
}

func TestSourceAdministrationSelectorsAndTokenIntersection(t *testing.T) {
	p := Principal{InstanceID: "instance", PrincipalID: "user", TokenID: "token", Scopes: []Scope{{Actions: []Action{Configure}}}}
	policy := Policy{Grants: []Grant{{PrincipalID: "user", InstanceID: "instance", Kind: SourceResource, SourceID: "source-a", Actions: []Action{Configure}, Effect: Allow}}}
	r := Resource{InstanceID: "instance", Kind: SourceResource, SourceID: "source-a"}
	if !policy.Authorize(p, r, Configure) {
		t.Fatal("scoped source administration denied")
	}
	r.SourceID = "source-b"
	if policy.Authorize(p, r, Configure) {
		t.Fatal("source grant escaped exact selector")
	}
	r.SourceID = "source-a"
	p.Scopes[0].FeedID = "feed"
	if policy.Authorize(p, r, Configure) {
		t.Fatal("feed-scoped token authorized independent source")
	}
}

func TestCanSeeFeedMatchesDataPlane(t *testing.T) {
	const inst, feed, other = "instance", "feed", "other"
	art := func(feedID, ns string, effect Effect, actions ...Action) Grant {
		return Grant{InstanceID: inst, Kind: ArtifactResource, FeedID: feedID, Namespace: ns, Effect: effect, Actions: actions}
	}
	policy := func(grants ...Grant) Policy {
		return Policy{Roles: []Role{{Name: "r", PrincipalIDs: []string{"user"}, Grants: grants}}}
	}
	tok := func(scopes ...Scope) Principal {
		return Principal{InstanceID: inst, PrincipalID: "user", TokenID: "t", Scopes: scopes}
	}
	cases := []struct {
		name   string
		policy Policy
		p      Principal
		feedID string
		want   bool
	}{
		{"artifact grant with feed scope", policy(art(feed, "", Allow, Read, Publish, Delete)), tok(Scope{Actions: []Action{Read}}, Scope{FeedID: feed, Actions: []Action{Read, Publish}}), feed, true},
		{"global read scope only", policy(art(feed, "", Allow, Read)), tok(Scope{Actions: []Action{Read}}), feed, true},
		{"namespace limited grant", policy(art(feed, "org", Allow, Read, Publish)), tok(Scope{FeedID: feed, Actions: []Action{Read}}), feed, true},
		{"namespace limited scope", policy(art(feed, "", Allow, Read)), tok(Scope{FeedID: feed, Namespace: "org", Actions: []Action{Read}}), feed, true},
		{"publish only", policy(art(feed, "", Allow, Publish)), tok(Scope{FeedID: feed, Actions: []Action{Publish}}), feed, true},
		{"other feed", policy(art(feed, "", Allow, Read)), tok(Scope{Actions: []Action{Read}}), other, false},
		{"scope for other feed", policy(art(feed, "", Allow, Read)), tok(Scope{FeedID: other, Actions: []Action{Read}}), feed, false},
		{"no grants", Policy{}, tok(Scope{Actions: []Action{Read}}), feed, false},
		{"scope without matching action", policy(art(feed, "", Allow, Read)), tok(Scope{FeedID: feed, Actions: []Action{Delete}}), feed, false},
		{"delete grant only", policy(art(feed, "", Allow, Delete)), tok(Scope{Actions: []Action{Delete}}), feed, false},
		{"feed deny wins", policy(art(feed, "", Allow, Read), Grant{InstanceID: inst, Kind: FeedResource, FeedID: feed, Effect: Deny, Actions: []Action{Read}}), tok(Scope{Actions: []Action{Read}}), feed, false},
		{"artifact deny wins", policy(art(feed, "", Allow, Read), art(feed, "", Deny, Read)), tok(Scope{Actions: []Action{Read}}), feed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.policy.CanSeeFeed(c.p, inst, c.feedID); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
