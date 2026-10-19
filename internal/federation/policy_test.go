package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func testBody() PolicyBody {
	b := PolicyBody{
		IdentityDomains: []IdentityDomain{{ID: "idp", Type: "oidc", Issuer: "https://idp.example"}},
		Roles: []PolicyRole{
			{Key: "fed:readers", Revision: 1, Grants: []PolicyGrant{{Kind: "feed", Effect: "allow", FeedKey: "lab/release", Actions: []string{"read"}},
				{Kind: "artifact", Effect: "deny", FeedKey: "lab/release", Namespace: "internal", Actions: []string{"read"}}}},
			{Key: "fed:admins", Revision: 1, Grants: []PolicyGrant{{Kind: "instance", Effect: "allow", Actions: []string{"read", "admin"}}}},
		},
		PrincipalBindings: []PrincipalBinding{{Domain: "idp", Subject: "alice", Roles: []string{"fed:readers"}}},
		GroupBindings:     []GroupBinding{{Domain: "idp", GroupID: "ops", Roles: []string{"fed:admins"}}},
		ServiceAccounts:   []ServiceAccount{{Name: "ci", Roles: []string{"fed:readers"}}},
		Revocations:       []Revocation{{Domain: "idp", Subject: "mallory"}},
	}
	b.Normalize()
	return b
}

// signedChain builds a membership (epoch 1) and n signed policy generations.
func signedChain(t *testing.T, n int) (*Membership, ed25519.PrivateKey, []PolicyDocument) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	m := &Membership{Kind: "membership", FederationID: "11111111-1111-4111-8111-111111111111", FederationName: "lab", Generation: 1, AuthorityEpoch: 1,
		Authority: AuthorityCert{Epoch: 1, PublicKey: encodeKey(pub)}}
	var docs []PolicyDocument
	var prev *PolicyDocument
	for i := 1; i <= n; i++ {
		d := PolicyDocument{Kind: "policy", FederationID: m.FederationID, FederationName: "lab", Generation: int64(i), AuthorityEpoch: 1, PolicyBody: testBody(),
			IssuedAt: time.Now().UTC().Format(time.RFC3339Nano), IssuedBy: "test"}
		d.Roles[0].Revision = int64(i)
		if prev != nil {
			d.PrevHash = PolicyHash(prev)
		}
		if err := SignPolicy(&d, key); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
		prev = &docs[len(docs)-1]
	}
	return m, key, docs
}

func TestValidatePolicy(t *testing.T) {
	b := testBody()
	if err := ValidatePolicy("lab", &b); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	for name, mutate := range map[string]func(*PolicyBody){
		"role prefix":      func(b *PolicyBody) { b.Roles[0].Key = "readers" },
		"source grant":     func(b *PolicyBody) { b.Roles[0].Grants[0].Kind = "source" },
		"foreign feed key": func(b *PolicyBody) { b.Roles[0].Grants[0].FeedKey = "other/release" },
		"feed key on instance": func(b *PolicyBody) {
			b.Roles[1].Grants[0].FeedKey = "lab/release"
		},
		"namespace on feed": func(b *PolicyBody) { b.Roles[0].Grants[0].Namespace = "x" },
		"bad action":        func(b *PolicyBody) { b.Roles[0].Grants[0].Actions = []string{"own"} },
		"repeated action":   func(b *PolicyBody) { b.Roles[0].Grants[0].Actions = []string{"read", "read"} },
		"unknown role":      func(b *PolicyBody) { b.PrincipalBindings[0].Roles = []string{"fed:nope"} },
		"unknown domain":    func(b *PolicyBody) { b.GroupBindings[0].Domain = "other" },
		"reserved domain":   func(b *PolicyBody) { b.IdentityDomains[0].ID = "federation" },
		"reserved issuer":   func(b *PolicyBody) { b.IdentityDomains[0].Issuer = "federation:x" },
		"duplicate binding": func(b *PolicyBody) { b.PrincipalBindings = append(b.PrincipalBindings, b.PrincipalBindings[0]) },
		"empty revocation":  func(b *PolicyBody) { b.Revocations = []Revocation{{}} },
		"override key":      func(b *PolicyBody) { b.MemberOverrides = map[string]MemberOverride{"x": {}} },
	} {
		c := testBody()
		mutate(&c)
		var in *InputError
		if err := ValidatePolicy("lab", &c); !errors.As(err, &in) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

func TestVerifyPolicyChain(t *testing.T) {
	m, key, docs := signedChain(t, 3)
	var applied *PolicyDocument
	for i := range docs {
		if err := VerifyPolicy(&docs[i], applied, m); err != nil {
			t.Fatalf("generation %d: %v", i+1, err)
		}
		applied = &docs[i]
	}
	_, rogue, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]struct {
		doc     func() PolicyDocument
		applied *PolicyDocument
		want    error
	}{
		"forged": {func() PolicyDocument { d := docs[1]; _ = SignPolicy(&d, rogue); return d }, &docs[0], ErrSignature},
		"tampered": {func() PolicyDocument {
			d := docs[1]
			d.ServiceAccounts = nil
			d.Normalize()
			return d
		}, &docs[0], ErrSignature},
		"stale": {func() PolicyDocument { return docs[0] }, &docs[1], ErrStaleGen},
		"gap":   {func() PolicyDocument { return docs[2] }, &docs[0], ErrGap},
		"first": {func() PolicyDocument { return docs[1] }, nil, ErrGap},
		"fork": {func() PolicyDocument {
			d := docs[1]
			d.PrevHash = "sha256:" + strings.Repeat("0", 64)
			_ = SignPolicy(&d, key)
			return d
		}, &docs[0], ErrChain},
		"future epoch": {func() PolicyDocument { d := docs[1]; d.AuthorityEpoch = 2; _ = SignPolicy(&d, key); return d }, &docs[0], ErrEpochUnknown},
		"other federation": {func() PolicyDocument {
			d := docs[1]
			d.FederationID = "22222222-2222-4222-8222-222222222222"
			_ = SignPolicy(&d, key)
			return d
		}, &docs[0], ErrWrongFed},
	}
	for name, c := range cases {
		d := c.doc()
		if err := VerifyPolicy(&d, c.applied, m); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	// A document round-trips through JSON (the stored form) and still verifies.
	raw, _ := json.Marshal(docs[1])
	var back PolicyDocument
	if err := json.Unmarshal(raw, &back); err != nil || VerifyPolicy(&back, &docs[0], m) != nil || PolicyHash(&back) != PolicyHash(&docs[1]) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestDiffPolicy(t *testing.T) {
	a := testBody()
	b := testBody()
	b.Roles[0].Revision = 2
	b.ServiceAccounts = nil
	b.PrincipalBindings = append(b.PrincipalBindings, PrincipalBinding{Domain: "idp", Subject: "bob", Roles: []string{"fed:admins"}})
	d := DiffPolicy(&a, &b)
	if !slices.Equal(d.Roles.Changed, []string{"fed:readers"}) || !slices.Equal(d.ServiceAccounts.Removed, []string{"ci"}) || !slices.Equal(d.PrincipalBindings.Added, []string{"idp/bob"}) || d.Changes != 3 {
		t.Fatalf("diff: %+v", d)
	}
	if d := DiffPolicy(nil, &a); d.Changes != 7 {
		t.Fatalf("diff from empty: %+v", d)
	}
}

func TestFederatedIDStable(t *testing.T) {
	a := FederatedID("f", "https://idp", "alice")
	if a != FederatedID("f", "https://idp", "alice") || a == FederatedID("f", "https://idp", "bob") || !uuidPattern.MatchString(a) {
		t.Fatalf("federated id: %s", a)
	}
}

// Property: materialized grants never widen access. An unresolved feed key
// is always inert, a federated instance-admin allow is inert unless accepted,
// and evaluating the plan together with random local grants never allows more
// than the union of local and federated policy, and a local deny always holds.
func TestPlanGrantsProperty(t *testing.T) {
	r := mrand.New(mrand.NewPCG(1, 2))
	actions := []string{"read", "publish", "delete", "configure", "replicate", "admin"}
	kinds := []string{"artifact", "feed", "instance"}
	feeds := []string{"lab/a", "lab/b", "lab/c", ""}
	ids := map[string]string{"lab/a": "aaaaaaaa-0000-4000-8000-000000000001", "lab/b": "aaaaaaaa-0000-4000-8000-000000000002"}
	for iter := 0; iter < 500; iter++ {
		body := PolicyBody{}
		for i := 0; i < 1+r.IntN(4); i++ {
			role := PolicyRole{Key: fmt.Sprintf("fed:r%d", i), Revision: 1}
			for j := 0; j < 1+r.IntN(4); j++ {
				g := PolicyGrant{Kind: kinds[r.IntN(3)], Effect: []string{"allow", "deny"}[r.IntN(2)], Actions: []string{actions[r.IntN(6)]}}
				if g.Kind != "instance" {
					g.FeedKey = feeds[r.IntN(4)]
				}
				if r.IntN(3) == 0 && g.Actions[0] != "admin" {
					g.Actions = append(g.Actions, "admin")
				}
				role.Grants = append(role.Grants, g)
			}
			body.Roles = append(body.Roles, role)
		}
		accept := r.IntN(2) == 0
		rows, unresolved, _ := planGrants(&body, ids, accept)
		// Planning is deterministic and idempotent.
		again, _, _ := planGrants(&body, ids, accept)
		if fmt.Sprint(rows) != fmt.Sprint(again) {
			t.Fatal("planGrants not deterministic")
		}
		fed := identity.Role{Name: "fed", PrincipalIDs: []string{"p"}}
		for _, g := range rows {
			if g.FeedKey != "" && ids[g.FeedKey] == "" && g.Inert != "unresolved_feed" {
				t.Fatalf("unresolved key active: %+v", g)
			}
			if g.Inert == "unresolved_feed" && !slices.Contains(unresolved, g.FeedKey) {
				t.Fatalf("unresolved key not reported: %+v", g)
			}
			if g.Kind == "instance" && g.Effect == "allow" && slices.Contains(g.Actions, "admin") && !accept && g.Inert == "" {
				t.Fatalf("admin grant active without acceptance: %+v", g)
			}
			if g.Kind != "instance" && g.FeedKey == "" && g.Effect == "allow" && !accept && g.Inert == "" {
				t.Fatalf("all-feed grant active without acceptance: %+v", g)
			}
			if g.Inert != "" {
				continue
			}
			ig := identity.Grant{InstanceID: "i", Kind: identity.ResourceKind(g.Kind), Effect: identity.Effect(g.Effect), FeedID: g.FeedID, Namespace: g.Namespace}
			for _, a := range g.Actions {
				ig.Actions = append(ig.Actions, identity.Action(a))
			}
			fed.Grants = append(fed.Grants, ig)
		}
		local := identity.Role{Name: "local", PrincipalIDs: []string{"p"}}
		for j := 0; j < r.IntN(3); j++ {
			local.Grants = append(local.Grants, identity.Grant{InstanceID: "i", Kind: identity.ResourceKind(kinds[r.IntN(3)]), Effect: identity.Effect([]string{"allow", "deny"}[r.IntN(2)]),
				Actions: []identity.Action{identity.Action(actions[r.IntN(6)])}})
		}
		p := identity.Principal{InstanceID: "i", PrincipalID: "p"}
		both := identity.Policy{Roles: []identity.Role{fed, local}}
		for _, kind := range kinds {
			for _, feed := range []string{"", ids["lab/a"], ids["lab/b"], "aaaaaaaa-0000-4000-8000-00000000000c"} {
				res := identity.Resource{InstanceID: "i", Kind: identity.ResourceKind(kind), FeedID: feed}
				if kind == "instance" {
					res.FeedID = ""
				}
				for _, a := range actions {
					act := identity.Action(a)
					got := both.Authorize(p, res, act)
					fedOnly := identity.Policy{Roles: []identity.Role{fed}}.Authorize(p, res, act)
					localOnly := identity.Policy{Roles: []identity.Role{local}}.Authorize(p, res, act)
					if got && !fedOnly && !localOnly {
						t.Fatalf("combined policy wider than the union: %+v %s", res, a)
					}
					for _, g := range local.Grants {
						if g.Effect == identity.Deny && g.Kind == res.Kind && slices.Contains(g.Actions, act) && got {
							t.Fatalf("local deny did not hold: %+v %s", res, a)
						}
					}
				}
			}
		}
	}
}

// Property: materialization depends only on the last applied document, so
// applying any ordered prefix of documents ends in the plan of its last one.
func TestPlanDependsOnlyOnLastDocument(t *testing.T) {
	_, _, docs := signedChain(t, 4)
	bindings := map[string]string{"lab/release": "aaaaaaaa-0000-4000-8000-000000000001"}
	for n := 1; n <= len(docs); n++ {
		var last []grantRow
		for i := 0; i < n; i++ {
			last, _, _ = planGrants(&docs[i].PolicyBody, bindings, false)
		}
		direct, _, _ := planGrants(&docs[n-1].PolicyBody, bindings, false)
		if fmt.Sprint(last) != fmt.Sprint(direct) {
			t.Fatalf("prefix %d differs from snapshot", n)
		}
	}
}

func TestDesiredRoles(t *testing.T) {
	b := testBody()
	if got := desiredRoles(&b, "idp", "alice", "", map[string]bool{"ops": true}); !slices.Equal(got, []string{"fed:admins", "fed:readers"}) {
		t.Fatalf("alice: %v", got)
	}
	if got := desiredRoles(&b, "idp", "bob", "", nil); len(got) != 0 {
		t.Fatalf("bob: %v", got)
	}
	if got := desiredRoles(&b, "", "", "ci", nil); !slices.Equal(got, []string{"fed:readers"}) {
		t.Fatalf("ci: %v", got)
	}
}

func TestComputeAlerts(t *testing.T) {
	now := time.Now()
	old := now.Add(-20 * time.Minute)
	st := State{State: "active", Role: "member", AppliedGeneration: 3, AppliedAt: &old, LastSyncSuccessAt: &old}
	ps := PolicyState{Generation: 2, AppliedAt: &old, ConfirmedAt: &old, Error: "policy document rejected (bad_signature) from x"}
	s := &Service{cfg: Config{Now: func() time.Time { return now }}}
	pol := s.policyStatus(st, ps)
	if pol.Staleness != "warning" {
		t.Fatalf("staleness: %+v", pol)
	}
	codes := []string{}
	for _, a := range computeAlerts(st, ps, pol, now) {
		codes = append(codes, a.Code+"/"+a.Severity)
	}
	if !slices.Equal(codes, []string{"membership_stale/warning", "policy_rejected/warning", "policy_stale/warning"}) {
		t.Fatalf("alerts: %v", codes)
	}
	st.Role = "authority"
	ps.Error = ""
	if a := computeAlerts(st, ps, s.policyStatus(st, ps), now); len(a) != 0 {
		t.Fatalf("authority alerts: %+v", a)
	}
}

// A federated artifact or feed allow without a feed key would match every
// local feed (federated or not): it is gated like instance admin.
func TestWildcardFeedGrantsNeedAcceptance(t *testing.T) {
	body := PolicyBody{Roles: []PolicyRole{{Key: "fed:all", Revision: 1, Grants: []PolicyGrant{
		{Kind: "feed", Effect: "allow", Actions: []string{"read", "publish"}},
		{Kind: "artifact", Effect: "allow", Actions: []string{"delete"}},
		{Kind: "artifact", Effect: "deny", Actions: []string{"read"}},
	}}}}
	rows, _, inert := planGrants(&body, nil, false)
	if inert != 2 || rows[0].Inert != "wildcard_not_accepted" || rows[1].Inert != "wildcard_not_accepted" || rows[2].Inert != "" {
		t.Fatalf("wildcard allows must be inert and denies active: %+v (inert %d)", rows, inert)
	}
	rows, _, inert = planGrants(&body, nil, true)
	if inert != 0 || rows[0].Inert != "" || rows[1].Inert != "" {
		t.Fatalf("accepted wildcard allows must be active: %+v", rows)
	}
}
