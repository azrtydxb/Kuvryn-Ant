package harness

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const idpIssuer = "https://idp.example.test"

// policyBody is the scenario's federation policy (generation 1).
func policyBody() map[string]any {
	return map[string]any{
		"identity_domains": []any{map[string]any{"id": "idp", "type": "oidc", "issuer": idpIssuer}},
		"roles": []any{
			map[string]any{"key": "fed:readers", "revision": 1, "grants": []any{
				map[string]any{"kind": "feed", "effect": "allow", "feed_key": "lab/release", "actions": []string{"read"}},
				map[string]any{"kind": "artifact", "effect": "allow", "feed_key": "lab/release", "actions": []string{"read"}},
				map[string]any{"kind": "feed", "effect": "allow", "feed_key": "lab/missing", "actions": []string{"read"}},
			}},
			map[string]any{"key": "fed:admins", "revision": 1, "grants": []any{
				map[string]any{"kind": "instance", "effect": "allow", "actions": []string{"read", "admin"}},
			}},
		},
		"principal_bindings": []any{map[string]any{"domain": "idp", "subject": "alice", "roles": []string{"fed:readers"}}},
		"group_bindings":     []any{map[string]any{"domain": "idp", "group_id": "ops", "roles": []string{"fed:admins"}}},
		"service_accounts":   []any{map[string]any{"name": "ci", "roles": []string{"fed:readers", "fed:admins"}}},
		"revocations":        []any{},
	}
}

func (in *instance) createFeed(slug string) string {
	in.t.Helper()
	var f struct {
		ID string `json:"id"`
	}
	if code := in.call("POST", "/api/v1/feeds", map[string]any{"slug": slug, "format": "raw"}, &f); code != 201 {
		in.t.Fatalf("%s create feed %s: %d", in.name, slug, code)
	}
	return f.ID
}

func (in *instance) bind(key, feed string) {
	in.t.Helper()
	if code := in.call("POST", "/api/v1/federation/feed-bindings", map[string]any{"feed_key": key, "feed_id": feed}, nil); code != 200 {
		in.t.Fatalf("%s bind %s: %d", in.name, key, code)
	}
}

func (in *instance) policyGeneration() int64 {
	in.t.Helper()
	st := in.status()
	if st.Policy == nil {
		return 0
	}
	return st.Policy.Generation
}

func (in *instance) policyView() federation.PolicyView {
	in.t.Helper()
	var v federation.PolicyView
	if code := in.call("GET", "/api/v1/federation/policy", nil, &v); code != 200 {
		in.t.Fatalf("%s policy: %d", in.name, code)
	}
	return v
}

// principal returns the local federated principal with a subject.
func (in *instance) principal(subject string) federation.FederatedPrincipal {
	in.t.Helper()
	var list struct {
		Items []federation.FederatedPrincipal `json:"items"`
	}
	if code := in.call("GET", "/api/v1/federation/principals", nil, &list); code != 200 {
		in.t.Fatalf("%s principals: %d", in.name, code)
	}
	for _, p := range list.Items {
		if p.Subject == subject {
			return p
		}
	}
	in.t.Fatalf("%s: no federated principal %s in %+v", in.name, subject, list.Items)
	return federation.FederatedPrincipal{}
}

// tokenFor issues a local token (all actions) for a principal, as a local
// administrator would for a federated service account.
func (in *instance) tokenFor(principal string) string {
	in.t.Helper()
	all := []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}
	tok, err := (identity.Tokens{DB: in.pool}).Issue(context.Background(), in.id, principal, principal, []identity.Scope{{Actions: all}}, time.Now().Add(time.Hour))
	if err != nil {
		in.t.Fatalf("%s token for %s: %v", in.name, principal, err)
	}
	return tok.Secret
}

func (in *instance) errorCode(method, path string, body any) (int, string) {
	in.t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	code := in.call(method, path, body, &e)
	return code, e.Code
}

func TestFederationPolicyScenarios(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma := newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")
	var founded federation.FoundResult
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "lab", "member_name": "alpha"}, &founded); code != 201 {
		t.Fatalf("found: %d", code)
	}
	for _, in := range []*instance{beta, gamma} {
		if code := in.join(alpha, alpha.invite(in.name)); code != 200 {
			t.Fatalf("join %s: %d", in.name, code)
		}
	}
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Each member holds its own "release" feed and binds it to the feed key.
	feeds := map[string]string{}
	for _, in := range []*instance{alpha, beta, gamma} {
		feeds[in.name] = in.createFeed("release")
		in.bind("lab/release", feeds[in.name])
	}

	// --- draft, diff, publish on the authority ---
	if code, c := beta.errorCode("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}); code != 409 || c != "not_authority" {
		t.Fatalf("draft on a member: %d %s", code, c)
	}
	bad := policyBody()
	bad["principal_bindings"] = []any{map[string]any{"domain": "idp", "subject": "alice", "roles": []string{"fed:unknown"}}}
	if code := alpha.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": bad}, nil); code != 400 {
		t.Fatalf("invalid draft: %d", code)
	}
	var draft federation.PolicyDraft
	if code := alpha.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}, &draft); code != 200 {
		t.Fatalf("save draft: %d", code)
	}
	if draft.Revision != 1 || draft.Diff.Roles.Added[0] != "fed:admins" || draft.Diff.Changes != 6 {
		t.Fatalf("draft diff: %+v", draft.Diff)
	}
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": false}, nil); code != 400 {
		t.Fatalf("publish without confirm: %d", code)
	}
	if code, c := alpha.errorCode("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 3, "confirm": true}); code != 409 || c != "stale_generation" {
		t.Fatalf("publish with a stale base: %d %s", code, c)
	}
	var published struct {
		Generation int64                   `json:"generation"`
		Report     federation.PolicyReport `json:"report"`
	}
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true}, &published); code != 200 || published.Generation != 1 {
		t.Fatalf("publish: %d %+v", code, published)
	}

	// --- policy propagates to every member (nudged by the authority) ---
	start := time.Now()
	eventually(t, "policy generation 1 on every member", func() bool { return beta.policyGeneration() == 1 && gamma.policyGeneration() == 1 })
	t.Logf("policy propagated in %v", time.Since(start))
	pv := beta.policyView()
	if pv.Document == nil || pv.Document.Generation != 1 || len(pv.Document.Roles) != 2 || pv.Status.Staleness != "fresh" {
		t.Fatalf("beta policy view: %+v", pv.Status)
	}
	// Unbound feed key stays inert; the federated instance-admin grant is inert by default.
	if r := pv.Status.Report; len(r.UnresolvedFeedKeys) != 1 || r.UnresolvedFeedKeys[0] != "lab/missing" || r.InertAdminGrants != 1 || r.Principals != 2 {
		t.Fatalf("beta materialization report: %+v", r)
	}
	var roles struct {
		Items []struct {
			Name     string `json:"name"`
			Origin   string `json:"origin"`
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
			Grants   []struct {
				FeedID  string `json:"feed_id"`
				FeedKey string `json:"feed_key"`
				Inert   string `json:"inert"`
			} `json:"grants"`
		} `json:"items"`
	}
	beta.call("GET", "/api/v1/roles", nil, &roles)
	fedRoles := map[string]string{}
	for _, r := range roles.Items {
		if r.Origin == "federation" {
			fedRoles[r.Name] = r.ID
			for _, g := range r.Grants {
				if g.FeedKey == "lab/missing" && (g.FeedID != "" || g.Inert != "unresolved_feed") {
					t.Fatalf("unresolved grant must be inert, never a wildcard: %+v", g)
				}
				if g.FeedKey == "lab/release" && g.FeedID != feeds["beta"] {
					t.Fatalf("grant not resolved to the local feed: %+v", g)
				}
			}
		}
	}
	if len(fedRoles) != 2 {
		t.Fatalf("federated roles on beta: %v", fedRoles)
	}

	// --- enforcement with a local token of the federated service account ---
	ciBeta := beta.principal("ci")
	ciTokenBeta := beta.tokenFor(ciBeta.ID)
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 200 {
		t.Fatalf("federated read on beta: %d", code)
	}
	other := beta.createFeed("other")
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+other, nil, nil); code != 404 {
		t.Fatalf("federated read on an unbound feed: %d", code)
	}
	// Rule 4: federated instance admin is inert until the member accepts it locally.
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/federation/principals", nil, nil); code != 403 {
		t.Fatalf("inert federated admin: %d", code)
	}
	if code := beta.call("PUT", "/api/v1/federation/settings", map[string]any{"accept_admin_grants": true, "policy_max_age_seconds": nil}, nil); code != 200 {
		t.Fatalf("accept admin grants: %d", code)
	}
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/federation/principals", nil, nil); code != 200 {
		t.Fatalf("accepted federated admin: %d", code)
	}
	// Binding the missing key activates its grant.
	beta.bind("lab/missing", other)
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+other, nil, nil); code != 200 {
		t.Fatalf("grant after binding the key: %d", code)
	}
	if r := beta.policyView().Status.Report; len(r.UnresolvedFeedKeys) != 0 || r.InertAdminGrants != 0 {
		t.Fatalf("report after binding and accepting: %+v", r)
	}

	// --- federation-managed rows are read-only locally ---
	var ciRevision int64
	_ = beta.pool.QueryRow(ctx, `SELECT revision FROM roles WHERE id=$1`, fedRoles["fed:readers"]).Scan(&ciRevision)
	if code, c := beta.ifMatch("PUT", "/api/v1/roles/"+fedRoles["fed:readers"]+"/grants", ciRevision, map[string]any{"grants": []any{}}); code != 409 || c != "federation_managed" {
		t.Fatalf("edit federated role: %d %s", code, c)
	}
	if code := beta.call("POST", "/api/v1/roles", map[string]any{"name": "fed:mine"}, nil); code != 400 {
		t.Fatalf("local role with the fed: prefix: %d", code)
	}
	alice := beta.principal("alice")
	if alice.Kind != "user" || len(alice.Roles) != 1 || alice.Roles[0] != "fed:readers" || alice.FederatedID == "" || alice.FederatedID != gamma.principal("alice").FederatedID {
		t.Fatalf("alice on beta: %+v", alice)
	}

	// --- local override on gamma: a local deny beats the federated allow ---
	ciGamma := gamma.principal("ci")
	var block struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if code := gamma.call("POST", "/api/v1/roles", map[string]any{"name": "block-release"}, &block); code != 201 {
		t.Fatalf("create local role: %d", code)
	}
	gamma.mustIfMatch("PUT", "/api/v1/roles/"+block.ID+"/grants", block.Revision, map[string]any{"grants": []any{map[string]any{"kind": "feed", "effect": "deny", "feed_id": feeds["gamma"], "actions": []string{"read"}}}})
	var princ struct {
		Items []struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
			Origin   string `json:"origin"`
		} `json:"items"`
	}
	gamma.call("GET", "/api/v1/principals?limit=200", nil, &princ)
	var ciRev int64
	for _, p := range princ.Items {
		if p.ID == ciGamma.ID {
			ciRev = p.Revision
			if p.Origin != "federation" {
				t.Fatalf("ci origin on gamma: %s", p.Origin)
			}
		}
	}
	gamma.mustIfMatch("PUT", "/api/v1/principals/"+ciGamma.ID+"/roles", ciRev, map[string]any{"role_ids": []string{block.ID}})
	ciTokenGamma := gamma.tokenFor(ciGamma.ID)
	if code := gamma.callAs(ciTokenGamma, "GET", "/api/v1/feeds/"+feeds["gamma"], nil, nil); code != 404 {
		t.Fatalf("local deny must win on gamma: %d", code)
	}
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 200 {
		t.Fatalf("gamma's local deny leaked to beta: %d", code)
	}
	// A local membership may not reference a federated role.
	var gRoles struct {
		Items []struct {
			ID, Name, Origin string
		} `json:"items"`
	}
	gamma.call("GET", "/api/v1/roles", nil, &gRoles)
	for _, r := range gRoles.Items {
		if r.Name == "fed:readers" {
			gamma.call("GET", "/api/v1/principals?limit=200", nil, &princ)
			for _, p := range princ.Items {
				if p.ID == ciGamma.ID {
					ciRev = p.Revision
				}
			}
			code, c := gamma.ifMatch("PUT", "/api/v1/principals/"+ciGamma.ID+"/roles", ciRev, map[string]any{"role_ids": []string{r.ID}})
			if code != 409 || c != "federation_managed" {
				t.Fatalf("local membership in a federated role: %d %s", code, c)
			}
		}
	}

	// --- signed cross-member assertions on the peer listener ---
	var res federation.AccessCheckResult
	if code := beta.callAs(ciTokenBeta, "POST", "/api/v1/federation/access-check", map[string]any{"member_id": gamma.id, "kind": "feed", "feed_key": "lab/release", "action": "read"}, &res); code != 200 || res.Allowed || res.Reason != "denied" {
		t.Fatalf("gamma must apply its own local deny to the asserted principal: %d %+v", code, res)
	}
	if code := beta.callAs(ciTokenBeta, "POST", "/api/v1/federation/access-check", map[string]any{"member_id": alpha.id, "kind": "feed", "feed_key": "lab/release", "action": "read"}, &res); code != 200 || !res.Allowed || res.PolicyGeneration != 1 {
		t.Fatalf("alpha decision for the asserted principal: %d %+v", code, res)
	}
	if code, c := beta.errorCode("POST", "/api/v1/federation/access-check", map[string]any{"member_id": alpha.id, "kind": "feed", "feed_key": "lab/release", "action": "read"}); code != 403 || c != "not_federated" {
		t.Fatalf("local principal asserted: %d %s", code, c)
	}
	body, _ := json.Marshal(map[string]string{"kind": "feed", "feed_key": "lab/release", "action": "read"})
	session := identity.Principal{InstanceID: beta.id, PrincipalID: ciBeta.ID}
	req, err := beta.svc.Assert(ctx, session, alpha.id, "POST", "/federation/v1/access-check", body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beta.svc.SendAsserted(ctx, req); err != nil {
		t.Fatalf("first use of an assertion: %v", err)
	}
	if code := peerCode(sendAsserted(beta, req)); code != "assertion_replayed" {
		t.Fatalf("replayed assertion: %q", code)
	}
	if alpha.auditCount("federation.assertion.rejected", "denied") == 0 {
		t.Fatal("assertion replay not audited")
	}
	tampered, _ := beta.svc.Assert(ctx, session, alpha.id, "POST", "/federation/v1/access-check", body)
	tampered.Body = []byte(`{"kind":"instance","action":"admin"}`)
	if code := peerCode(sendAsserted(beta, tampered)); code != "assertion_request_mismatch" {
		t.Fatalf("assertion bound to another request: %q", code)
	}
	misdirected, _ := beta.svc.Assert(ctx, session, gamma.id, "POST", "/federation/v1/access-check", body)
	misdirected.Member = alpha.id
	if code := peerCode(sendAsserted(beta, misdirected)); code != "assertion_audience_mismatch" {
		t.Fatalf("assertion for another member: %q", code)
	}
	forged, _ := beta.svc.Assert(ctx, session, alpha.id, "POST", "/federation/v1/access-check", body)
	payload, _, _ := strings.Cut(forged.Header, ".")
	_, rogue, _ := ed25519.GenerateKey(rand.Reader)
	forged.Header = payload + "." + b64(ed25519.Sign(rogue, mustB64(payload)))
	if code := peerCode(sendAsserted(beta, forged)); code != "assertion_signature_invalid" {
		t.Fatalf("forged assertion: %q", code)
	}
	gamma.clock.Advance(3 * time.Minute)
	skewed, _ := beta.svc.Assert(ctx, session, gamma.id, "POST", "/federation/v1/access-check", body)
	if code := peerCode(sendAsserted(beta, skewed)); code != "clock_skew" {
		t.Fatalf("skewed member clock: %q", code)
	}
	gamma.clock.Reset()

	// --- stale, forged and replayed policy documents are rejected; last good stays ---
	g1 := policyDocument(t, gamma, 1)
	key := alpha.authorityKey(1)
	forgedDoc := g1
	forgedDoc.Generation, forgedDoc.PrevHash = 2, federation.PolicyHash(&g1)
	forgedDoc.Roles = append([]federation.PolicyRole(nil), g1.Roles...)
	forgedDoc.Roles[0].Grants = []federation.PolicyGrant{{Kind: "instance", Effect: "allow", Actions: []string{"admin"}}}
	_ = federation.SignPolicy(&forgedDoc, rogue)
	if _, err := gamma.svc.ApplyPolicyDocuments(ctx, []federation.PolicyDocument{forgedDoc}, "test"); rejectReason(err) != "bad_signature" {
		t.Fatalf("forged policy: %v", err)
	}
	stale := g1
	stale.IssuedBy = "replayed"
	_ = federation.SignPolicy(&stale, key)
	if _, err := gamma.svc.ApplyPolicyDocuments(ctx, []federation.PolicyDocument{stale}, "test"); rejectReason(err) != "stale_generation" {
		t.Fatalf("stale policy: %v", err)
	}
	tamperedDoc := g1
	tamperedDoc.Generation, tamperedDoc.PrevHash = 2, federation.PolicyHash(&g1)
	_ = federation.SignPolicy(&tamperedDoc, key)
	tamperedDoc.ServiceAccounts = nil
	if _, err := gamma.svc.ApplyPolicyDocuments(ctx, []federation.PolicyDocument{tamperedDoc}, "test"); rejectReason(err) != "bad_signature" {
		t.Fatalf("tampered policy: %v", err)
	}
	gap := g1
	gap.Generation, gap.PrevHash = 3, federation.PolicyHash(&g1)
	_ = federation.SignPolicy(&gap, key)
	if _, err := gamma.svc.ApplyPolicyDocuments(ctx, []federation.PolicyDocument{gap}, "test"); rejectReason(err) != "generation_gap" {
		t.Fatalf("policy gap: %v", err)
	}
	st := gamma.status()
	if st.Policy.Generation != 1 || st.Policy.Error == "" || !hasAlert(st, "policy_rejected") || st.Health != "degraded" {
		t.Fatalf("rejections not reported: %+v %+v", st.Policy, st.Alerts)
	}
	var policyRejections int
	_ = gamma.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='federation.document.rejected' AND details->>'kind'='policy'`).Scan(&policyRejections)
	if policyRejections != 4 {
		t.Fatalf("policy rejections audited: %d", policyRejections)
	}
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 200 {
		t.Fatalf("last good policy not enforced: %d", code)
	}
	if err := gamma.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st := gamma.status(); st.Policy.Error != "" || hasAlert(st, "policy_rejected") {
		t.Fatalf("confirmation by the authority did not clear the rejection: %+v", st.Policy)
	}

	// --- authority down: last good policy enforced, staleness alert raised ---
	alpha.proxy.set(faultDrop)
	gamma.proxy.set(faultDrop)
	beta.clock.Advance(16 * time.Minute)
	if err := beta.svc.SyncOnce(ctx); err == nil {
		t.Fatal("sync succeeded with the authority and the relay down")
	}
	alerts, err := beta.svc.EvaluateAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st = beta.status()
	if st.Policy.Staleness != "warning" || !hasAlert(st, "policy_stale") || !hasAlert(st, "membership_stale") || st.Health != "degraded" || len(alerts) < 2 {
		t.Fatalf("authority outage not alerted: %+v %+v", st.Policy, st.Alerts)
	}
	if beta.auditCount("federation.alert.raised", "success") < 2 {
		t.Fatal("alert transition not recorded as an event")
	}
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 200 {
		t.Fatalf("last good policy must stay enforced during an outage: %d", code)
	}
	// Optional hard limit (D4): beyond it federated principals are denied, local ones are not.
	if code := beta.call("PUT", "/api/v1/federation/settings", map[string]any{"accept_admin_grants": true, "policy_max_age_seconds": 300}, nil); code != 200 {
		t.Fatalf("set hard limit: %d", code)
	}
	if _, err := beta.pool.Exec(ctx, `UPDATE federation_state SET policy_confirmed_at=now()-interval '1 hour' WHERE instance_id=$1`, beta.id); err != nil {
		t.Fatal(err)
	}
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 404 {
		t.Fatalf("hard staleness limit must deny federated principals: %d", code)
	}
	if code := beta.call("GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 200 {
		t.Fatalf("hard staleness limit must not affect local principals: %d", code)
	}
	if code := beta.call("PUT", "/api/v1/federation/settings", map[string]any{"accept_admin_grants": true, "policy_max_age_seconds": nil}, nil); code != 200 {
		t.Fatalf("clear hard limit: %d", code)
	}
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 200 {
		t.Fatalf("after clearing the hard limit: %d", code)
	}
	alpha.proxy.set(faultPass)
	gamma.proxy.set(faultPass)
	beta.clock.Reset()
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := beta.svc.EvaluateAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	if st := beta.status(); hasAlert(st, "policy_stale") || st.Health != "ok" || beta.auditCount("federation.alert.cleared", "success") == 0 {
		t.Fatalf("alerts not cleared after recovery: %+v", st.Alerts)
	}

	// --- conflict with a local principal (rule 5) and adoption ---
	// dave already exists on alpha as a local user bound to the same identity.
	var local string
	if err := alpha.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','dave-local') RETURNING id::text`, alpha.id).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if _, err := alpha.pool.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,$2,'dave',$3)`, alpha.id, idpIssuer, local); err != nil {
		t.Fatal(err)
	}

	// --- relay: a member that cannot reach the authority gets the policy from a peer ---
	gen2 := policyBody()
	gen2["principal_bindings"] = []any{
		map[string]any{"domain": "idp", "subject": "alice", "roles": []string{"fed:readers"}},
		map[string]any{"domain": "idp", "subject": "dave", "roles": []string{"fed:readers"}},
	}
	gamma.proxy.setDelay(50 * time.Millisecond) // slow but reachable
	beta.proxy.set(faultDrop)                   // beta misses the nudge
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 1, "confirm": true, "policy": gen2}, nil); code != 200 {
		t.Fatalf("publish generation 2: %d", code)
	}
	eventually(t, "gamma at generation 2", func() bool { return gamma.policyGeneration() == 2 })
	beta.proxy.set(faultPass)
	alpha.proxy.set(faultDrop)
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("relay sync: %v", err)
	}
	if st := beta.status(); st.Policy.Generation != 2 || st.Sync.Source != "gamma" {
		t.Fatalf("beta did not get the policy through gamma: %+v %+v", st.Policy, st.Sync)
	}
	alpha.proxy.set(faultPass)
	gamma.proxy.set(faultPass)

	r := alpha.policyView().Status.Report
	if len(r.Conflicts) != 1 || r.Conflicts[0].PrincipalID != local || !hasAlert(alpha.status(), "policy_conflicts") {
		t.Fatalf("conflict not reported: %+v", r)
	}
	if code := alpha.call("POST", "/api/v1/federation/principals/"+local+"/adopt", map[string]any{"confirm": true}, nil); code != 200 {
		t.Fatalf("adopt: %d", code)
	}
	if d := alpha.principal("dave"); d.ID != local || d.Origin != "federation" || len(d.Roles) != 1 {
		t.Fatalf("adopted principal: %+v", d)
	}
	gen3 := policyBody()
	gen3["principal_bindings"] = gen2["principal_bindings"]
	gen3["revocations"] = []any{map[string]any{"service_account": "ci", "reason": "rotated"}}
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 2, "confirm": true, "policy": gen3}, nil); code != 200 {
		t.Fatalf("publish generation 3: %d", code)
	}

	// --- revocation reaches every reachable member within 60 seconds ---
	start = time.Now()
	deadline := time.Now().Add(60 * time.Second)
	for beta.policyGeneration() != 3 || gamma.policyGeneration() != 3 {
		if time.Now().After(deadline) {
			t.Fatal("revocation did not reach all members within 60s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("revocation propagated in %v", time.Since(start))
	if code := beta.callAs(ciTokenBeta, "GET", "/api/v1/feeds/"+feeds["beta"], nil, nil); code != 401 {
		t.Fatalf("revoked service account token on beta: %d", code)
	}
	if p := gamma.principal("ci"); !p.Disabled {
		t.Fatalf("revoked principal not disabled on gamma: %+v", p)
	}

	// --- group bindings at sign-in ---
	if err := beta.svc.ExternalLogin(ctx, idpIssuer, "bob", []string{"ops", "unrelated"}); err != nil {
		t.Fatal(err)
	}
	if b := beta.principal("bob"); len(b.Roles) != 1 || b.Roles[0] != "fed:admins" {
		t.Fatalf("group binding at sign-in: %+v", b)
	}
	if err := beta.svc.ExternalLogin(ctx, idpIssuer, "carol", nil); err != nil {
		t.Fatal(err)
	}
	var carol int
	_ = beta.pool.QueryRow(ctx, `SELECT count(*) FROM principals WHERE subject='carol'`).Scan(&carol)
	if carol != 0 {
		t.Fatal("an unbound identity was materialized")
	}
	if err := beta.svc.ExternalLogin(ctx, idpIssuer, "bob", nil); err != nil {
		t.Fatal(err)
	}
	if b := beta.principal("bob"); len(b.Roles) != 0 {
		t.Fatalf("group roles must follow the groups of the last sign-in: %+v", b)
	}

	// --- history and diff ---
	var diff federation.PolicyDiff
	if code := beta.call("GET", "/api/v1/federation/policy/diff?from=1&to=3", nil, &diff); code != 200 || len(diff.Revocations.Added) != 1 || diff.Revocations.Added[0] != "service_account/ci" {
		t.Fatalf("diff 1..3: %d %+v", code, diff)
	}
	if pv := beta.policyView(); len(pv.History) != 3 {
		t.Fatalf("history: %+v", pv.History)
	}
	var doc struct {
		Kind     string                    `json:"kind"`
		Document federation.PolicyDocument `json:"document"`
	}
	if code := beta.call("GET", "/api/v1/federation/documents/policy/2", nil, &doc); code != 200 || doc.Kind != "policy" || doc.Document.Generation != 2 {
		t.Fatalf("policy document 2: %d %+v", code, doc.Kind)
	}

	// --- leaving drops federation-managed identity; local rows stay ---
	if code := gamma.call("POST", "/api/v1/federation/leave", map[string]any{"confirm": true}, nil); code != 200 {
		t.Fatalf("gamma leave: %d", code)
	}
	var fedRows, localRole int
	_ = gamma.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM roles WHERE origin='federation')+(SELECT count(*) FROM role_grants WHERE origin='federation')+(SELECT count(*) FROM principals WHERE origin='federation')`).Scan(&fedRows)
	_ = gamma.pool.QueryRow(ctx, `SELECT count(*) FROM roles WHERE name='block-release'`).Scan(&localRole)
	if fedRows != 0 || localRole != 1 {
		t.Fatalf("after leave: federation rows %d, local role %d", fedRows, localRole)
	}
}

func hasAlert(st federation.Status, code string) bool {
	for _, a := range st.Alerts {
		if a.Code == code {
			return true
		}
	}
	return false
}

func sendAsserted(in *instance, req federation.AssertedRequest) error {
	_, err := in.svc.SendAsserted(context.Background(), req)
	return err
}

func policyDocument(t *testing.T, in *instance, gen int64) federation.PolicyDocument {
	t.Helper()
	var d struct {
		Document federation.PolicyDocument `json:"document"`
	}
	if code := in.call("GET", fmt.Sprintf("/api/v1/federation/documents/policy/%d", gen), nil, &d); code != 200 {
		t.Fatalf("%s policy document %d: %d", in.name, gen, code)
	}
	return d.Document
}

// ifMatch sends a mutation with If-Match and returns the status and error code.
func (in *instance) ifMatch(method, path string, revision int64, body any) (int, string) {
	in.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := newJSONRequest(method, in.api.URL+path, raw)
	req.Header.Set("Authorization", "Bearer "+in.token)
	req.Header.Set("If-Match", fmt.Sprintf("%q", fmt.Sprint(revision)))
	req.Header.Set("Idempotency-Key", fmt.Sprintf("harness-%d", dbCounter.Add(1)))
	resp, err := httpDo(req)
	if err != nil {
		in.t.Fatal(err)
	}
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(resp.body, &e)
	return resp.status, e.Code
}

func (in *instance) mustIfMatch(method, path string, revision int64, body any) {
	in.t.Helper()
	if code, c := in.ifMatch(method, path, revision, body); code != 200 {
		in.t.Fatalf("%s %s %s: %d %s", in.name, method, path, code, c)
	}
}
