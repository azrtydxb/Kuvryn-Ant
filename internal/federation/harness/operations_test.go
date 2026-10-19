package harness

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
)

// P2-M6 scenarios: planned authority handover, forced recovery and fencing,
// the recovery-key drill, federated audit, the two-phase join and revocation
// pruning.

func (in *instance) secretCount(purpose string) int {
	in.t.Helper()
	var n int
	if err := in.pool.QueryRow(context.Background(), `SELECT count(*) FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2`, in.id, purpose).Scan(&n); err != nil {
		in.t.Fatal(err)
	}
	return n
}

func (in *instance) authorityStatus() federation.AuthorityStatus {
	in.t.Helper()
	var out federation.AuthorityStatus
	if code := in.call("GET", "/api/v1/federation/authority", nil, &out); code != 200 {
		in.t.Fatalf("%s authority status: %d", in.name, code)
	}
	return out
}

func (in *instance) alertCodes() map[string]bool {
	in.t.Helper()
	out := map[string]bool{}
	for _, a := range in.status().Alerts {
		out[a.Code] = true
	}
	return out
}

// threeMembers founds a federation on alpha and joins beta and gamma.
func threeMembers(t *testing.T) (alpha, beta, gamma *instance, recovery string) {
	t.Helper()
	ctx := context.Background()
	alpha, beta, gamma = newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")
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
	return alpha, beta, gamma, founded.RecoveryKey
}

func TestAuthorityHandover(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, _ := threeMembers(t)
	oldKey := alpha.authorityKey(1)
	if alpha.secretCount("federation-ca") != 1 || beta.secretCount("federation-ca") != 0 {
		t.Fatal("CA key should live on the authority only")
	}

	// Refusals.
	if code := alpha.call("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": beta.id, "confirm": false}, nil); code != 400 {
		t.Fatalf("handover without confirm: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": alpha.id, "confirm": true}, nil); code != 409 {
		t.Fatalf("handover to itself: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": "00000000-0000-4000-8000-000000000000", "confirm": true}, nil); code != 404 {
		t.Fatalf("handover to a stranger: %d", code)
	}
	if code, c := beta.errorCode("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": gamma.id, "confirm": true}); code != 409 || c != "not_authority" {
		t.Fatalf("handover on a member: %d %s", code, c)
	}
	// An unreachable target fails cleanly and leaves publishing enabled.
	gamma.proxy.set(faultDrop)
	if code := alpha.call("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": gamma.id, "confirm": true}, nil); code != 503 {
		t.Fatalf("handover to an unreachable member: %d", code)
	}
	gamma.proxy.set(faultPass)
	if as := alpha.authorityStatus(); as.Handover != nil || as.Role != "authority" || as.Epoch != 1 {
		t.Fatalf("status after a failed handover: %+v", as)
	}
	// A handover in progress pauses publishing; abort resumes it.
	if _, err := alpha.pool.Exec(ctx, `UPDATE federation_state SET handover_to=$2,handover_base_generation=1,handover_started_at=clock_timestamp() WHERE instance_id=$1`, alpha.id, beta.id); err != nil {
		t.Fatal(err)
	}
	if code := alpha.call("POST", "/api/v1/federation/invitations", map[string]any{"name": "paused"}, nil); code != 409 {
		t.Fatalf("invitation during a handover: %d", code)
	}
	if !alpha.alertCodes()["handover_pending"] || alpha.authorityStatus().Handover == nil {
		t.Fatal("pending handover not reported")
	}
	if code := alpha.call("POST", "/api/v1/federation/authority/handover/abort", nil, nil); code != 204 {
		t.Fatalf("abort: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/authority/handover/abort", nil, nil); code != 404 {
		t.Fatalf("second abort: %d", code)
	}
	paused := alpha.invite("paused")
	if code := alpha.call("DELETE", "/api/v1/federation/invitations/"+paused.ID, nil, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}

	// --- the handover ---
	var res federation.HandoverResult
	if code := alpha.call("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": beta.id, "confirm": true}, &res); code != 200 || res.Epoch != 2 || !res.Applied {
		t.Fatalf("handover: %d %+v", code, res)
	}
	if st := alpha.status(); st.Role != "member" || st.AuthorityEpoch != 2 || st.AuthorityInstanceID != beta.id {
		t.Fatalf("old authority after handover: %+v", st)
	}
	if st := beta.status(); st.Role != "authority" || st.AuthorityEpoch != 2 || st.Membership.Generation != alpha.generation() {
		t.Fatalf("new authority: %+v", st)
	}
	if alpha.alertCodes()["authority_fenced"] {
		t.Fatal("a planned handover must not be reported as fencing")
	}
	// Retired keys are destroyed on the old authority; the new one holds both.
	if alpha.secretCount("federation-ca") != 0 || alpha.secretCount("federation-authority:1") != 0 {
		t.Fatal("old authority kept its authority or CA key")
	}
	if beta.secretCount("federation-ca") != 1 || beta.secretCount("federation-authority:2") != 1 {
		t.Fatal("new authority is missing its keys")
	}
	as := alpha.authorityStatus()
	if len(as.Epochs) != 2 || as.Epochs[1].Epoch != 2 || as.Epochs[1].CertifiedBy != "previous_epoch" || as.Epochs[1].InstanceID != beta.id || as.Fenced != nil {
		t.Fatalf("epoch history: %+v", as)
	}
	doc := beta.document(beta.generation())
	if doc.Authority.Acceptance == "" || doc.IssuedBy != "handover:"+alpha.id {
		t.Fatalf("epoch document: %+v", doc.Authority)
	}
	// Members follow the new epoch (nudge, or poll).
	eventually(t, "gamma follows epoch 2", func() bool {
		_ = gamma.svc.SyncOnce(ctx)
		return gamma.status().AuthorityEpoch == 2
	})
	// The retired key cannot extend the chain any more.
	head := beta.document(beta.generation())
	forged := head
	forged.Generation, forged.PrevHash, forged.AuthorityEpoch = head.Generation+1, federation.Hash(&head), 1
	forged.Authority = alpha.document(1).Authority
	forged.Signature = nil
	_ = federation.Sign(&forged, oldKey)
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{forged}, "test"); rejectReason(err) != "stale_epoch" {
		t.Fatalf("document of the retired epoch: %v", err)
	}

	// The new authority administers: invitations and joins (certificates come
	// from the transferred CA), policy publication; the old one cannot.
	if code, c := alpha.errorCode("POST", "/api/v1/federation/invitations", map[string]any{"name": "late"}); code != 409 || c != "not_authority" {
		t.Fatalf("invitation on the old authority: %d %s", code, c)
	}
	delta := newInstance(t, "delta")
	if code := delta.join(beta, beta.invite("delta")); code != 200 {
		t.Fatalf("join through the new authority: %d", code)
	}
	if err := delta.svc.Probe(ctx, gamma.id); err != nil {
		t.Fatalf("new member probing an old one: %v", err)
	}
	if code := beta.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}, nil); code != 200 {
		t.Fatalf("draft on the new authority: %d", code)
	}
	if code := beta.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true}, nil); code != 200 {
		t.Fatalf("publish on the new authority: %d", code)
	}
	eventually(t, "policy reaches the former authority", func() bool { return alpha.policyGeneration() == 1 && gamma.policyGeneration() == 1 })
	if code := alpha.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}, nil); code != 409 {
		t.Fatalf("draft on the old authority: %d", code)
	}

	// --- and back: the role can return, with the CA key travelling again ---
	if code := beta.call("POST", "/api/v1/federation/authority/handover", map[string]any{"to_member_id": alpha.id, "confirm": true}, &res); code != 200 || res.Epoch != 3 {
		t.Fatalf("handover back: %d %+v", code, res)
	}
	if alpha.status().Role != "authority" || beta.status().Role != "member" || alpha.secretCount("federation-ca") != 1 || beta.secretCount("federation-ca") != 0 {
		t.Fatalf("roles after handing back: %s %s", alpha.status().Role, beta.status().Role)
	}
	if code := alpha.call("POST", "/api/v1/federation/members/"+delta.id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("suspend on the returned authority: %d", code)
	}
	if n := alpha.auditCount("federation.authority.epoch_changed", "success"); n < 2 {
		t.Fatalf("epoch changes audited: %d", n)
	}
}

func TestForcedRecoveryAndFencing(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, recovery := threeMembers(t)
	if err := gamma.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	baseGen := beta.generation()

	// The old authority is partitioned and keeps publishing on its own:
	// suspend and resume gamma (two generations that will be voided).
	beta.proxy.set(faultDrop)
	gamma.proxy.set(faultDrop)
	if code := alpha.call("POST", "/api/v1/federation/members/"+gamma.id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("suspend: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/members/"+gamma.id+"/resume", nil, nil); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	if alpha.generation() != baseGen+2 || beta.generation() != baseGen {
		t.Fatalf("partition setup: alpha %d beta %d base %d", alpha.generation(), beta.generation(), baseGen)
	}

	// Recovery on beta. A template, a signature by the wrong key and a
	// request without confirm are all refused.
	if code, c := alpha.errorCode("POST", "/api/v1/federation/authority/recovery-template", nil); code != 409 {
		t.Fatalf("template on the authority: %d %s", code, c)
	}
	var tpl federation.RecoveryTemplate
	if code := beta.call("POST", "/api/v1/federation/authority/recovery-template", nil, &tpl); code != 200 || tpl.Authority.Epoch != 2 || tpl.Authority.InstanceID != beta.id || tpl.Generation != baseGen+1 {
		t.Fatalf("template: %d %+v", code, tpl)
	}
	var again federation.RecoveryTemplate
	beta.call("POST", "/api/v1/federation/authority/recovery-template", nil, &again)
	if again.Authority.PublicKey != tpl.Authority.PublicKey {
		t.Fatal("the pending authority key changed between template requests")
	}
	stranger, _, _ := federation.NewRecoveryKey()
	bad, _ := federation.SignAuthorityCertificate(tpl, stranger)
	if code := beta.call("POST", "/api/v1/federation/authority/recover", map[string]any{"authority_certificate": bad, "confirm": true}, nil); code != 400 {
		t.Fatalf("recovery signed by a stranger: %d", code)
	}
	cert, err := federation.SignAuthorityCertificate(tpl, recovery)
	if err != nil {
		t.Fatal(err)
	}
	if code := beta.call("POST", "/api/v1/federation/authority/recover", map[string]any{"authority_certificate": cert, "confirm": false}, nil); code != 400 {
		t.Fatalf("recovery without confirm: %d", code)
	}
	var rec federation.RecoverResult
	if code := beta.call("POST", "/api/v1/federation/authority/recover", map[string]any{"authority_certificate": cert, "confirm": true}, &rec); code != 200 || rec.Epoch != 2 || len(rec.CASHA256) != 64 {
		t.Fatalf("recover: %d %+v", code, rec)
	}
	oldCA := alpha.status().CASHA256 // pin of the listener certificate beta keeps presenting until it renews
	if st := beta.status(); st.Role != "authority" || st.AuthorityEpoch != 2 || st.Membership.Generation != baseGen+1 || st.CASHA256 != oldCA || rec.CASHA256 == oldCA {
		t.Fatalf("recovered authority: %+v", st)
	}
	head := beta.document(baseGen + 1)
	if newCA, _ := parseCA(head.CACertificates[len(head.CACertificates)-1]); len(head.CACertificates) != 2 || newCA != rec.CASHA256 || head.Authority.CertifiedBy != "recovery" || head.Authority.Acceptance == "" {
		t.Fatalf("recovery document: CAs %d %+v", len(head.CACertificates), head.Authority)
	}
	if code := beta.call("POST", "/api/v1/federation/authority/recover", map[string]any{"authority_certificate": cert, "confirm": true}, nil); code != 409 {
		t.Fatalf("second recovery: %d", code)
	}

	// The partition heals. The old authority learns of epoch 2, replaces its
	// voided generations, destroys its keys and reports being fenced.
	beta.proxy.set(faultPass)
	gamma.proxy.set(faultPass)
	if err := alpha.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("old authority sync: %v", err)
	}
	if st := alpha.status(); st.Role != "member" || st.AuthorityEpoch != 2 || st.Membership.Generation != baseGen+1 || st.AuthorityInstanceID != beta.id {
		t.Fatalf("old authority after healing: %+v", st)
	}
	if h := alpha.document(baseGen + 1); federation.Hash(&h) != federation.Hash(&head) {
		t.Fatal("the voided generation was not replaced by the recovery document")
	}
	if alpha.secretCount("federation-ca") != 0 || alpha.secretCount("federation-authority:1") != 0 {
		t.Fatal("fenced authority kept its keys")
	}
	as := alpha.authorityStatus()
	if as.Fenced == nil || as.Fenced.ByEpoch != 2 || !alpha.alertCodes()["authority_fenced"] {
		t.Fatalf("fence not reported: %+v", as.Fenced)
	}
	if alpha.auditCount("federation.authority.fenced", "success") != 1 {
		t.Fatal("fence not audited")
	}
	if code, c := alpha.errorCode("POST", "/api/v1/federation/invitations", map[string]any{"name": "x"}); code != 409 || c != "not_authority" {
		t.Fatalf("administration on the fenced authority: %d %s", code, c)
	}
	// Members: gamma follows even though the old authority answered first.
	eventually(t, "gamma follows the recovered epoch", func() bool {
		_ = gamma.svc.SyncOnce(ctx)
		return gamma.status().AuthorityEpoch == 2
	})
	// Old certificates stay valid through the CA overlap; new joins use the new CA.
	if err := gamma.svc.Probe(ctx, beta.id); err != nil {
		t.Fatalf("member probing the recovered authority: %v", err)
	}
	if err := alpha.svc.Probe(ctx, gamma.id); err != nil {
		t.Fatalf("fenced authority probing a member: %v", err)
	}
	delta := newInstance(t, "delta")
	inv := beta.invite("delta")
	if inv.CASHA256 != oldCA {
		t.Fatalf("invitation pins %s, want the CA of the listener certificate %s", inv.CASHA256, oldCA)
	}
	if code := delta.join(beta, inv); code != 200 {
		t.Fatalf("join after recovery: %d", code)
	}
	// The retired epoch cannot extend the chain.
	forged := head
	forged.Generation, forged.PrevHash = head.Generation+1, federation.Hash(&head)
	forged.AuthorityEpoch, forged.Authority = 1, alpha.document(1).Authority
	forged.Signature = nil
	_ = federation.Sign(&forged, ed25519.NewKeyFromSeed(make([]byte, 32)))
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{forged}, "test"); rejectReason(err) != "stale_epoch" {
		t.Fatalf("forged retired-epoch document: %v", err)
	}
	// Policy publication continues on the new authority at epoch 2.
	if code := beta.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}, nil); code != 200 {
		t.Fatalf("draft: %d", code)
	}
	if code := beta.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true}, nil); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	eventually(t, "policy at epoch 2 everywhere", func() bool { return alpha.policyGeneration() == 1 && gamma.policyGeneration() == 1 })
}

func TestForcedRecoveryRemovesCompromisedAuthority(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, recovery := threeMembers(t)
	_ = gamma.svc.SyncOnce(ctx)
	alpha.stop()
	var tpl federation.RecoveryTemplate
	beta.call("POST", "/api/v1/federation/authority/recovery-template", nil, &tpl)
	cert, _ := federation.SignAuthorityCertificate(tpl, recovery)
	var rec federation.RecoverResult
	if code := beta.call("POST", "/api/v1/federation/authority/recover", map[string]any{"authority_certificate": cert, "remove_previous_authority": true, "confirm": true}, &rec); code != 200 || !rec.PreviousRemoved {
		t.Fatalf("recover: %d %+v", code, rec)
	}
	if m := beta.members()["alpha"]; m.Status != "removed" {
		t.Fatalf("previous authority after recovery: %+v", m)
	}
	head := beta.document(beta.generation())
	if len(head.RevokedCertSerials) == 0 {
		t.Fatal("the previous authority's certificate was not revoked")
	}
	eventually(t, "gamma follows", func() bool {
		_ = gamma.svc.SyncOnce(ctx)
		return gamma.status().AuthorityEpoch == 2
	})
	// The removed authority is refused everywhere once it returns.
	alpha.restart()
	if code := peerCode(alpha.svc.Probe(ctx, beta.id)); code != "certificate_revoked" {
		t.Fatalf("removed authority at the new one: %q", code)
	}
}

func TestRecoveryKeyDrill(t *testing.T) {
	alpha, beta, _, recovery := threeMembers(t)
	if as := alpha.authorityStatus(); !as.Recovery.Due || as.Recovery.VerifiedAt != nil {
		t.Fatalf("drill before running: %+v", as.Recovery)
	}
	var ch federation.RecoveryChallenge
	if code := alpha.call("POST", "/api/v1/federation/recovery-key/challenge", nil, &ch); code != 200 || ch.Challenge == "" {
		t.Fatalf("challenge: %d", code)
	}
	if code := beta.call("POST", "/api/v1/federation/recovery-key/challenge", nil, nil); code != 409 {
		t.Fatalf("challenge on a member: %d", code)
	}
	stranger, _, _ := federation.NewRecoveryKey()
	if _, err := federation.SignRecoveryChallenge(ch, stranger); err == nil {
		t.Fatal("a stranger's key signed the challenge")
	}
	// A wrong signature is refused and consumes the challenge.
	other, _, _ := federation.NewRecoveryKey()
	pubOther, _ := federation.RecoveryPublicKey(other)
	forged, err := federation.SignRecoveryChallenge(federation.RecoveryChallenge{FederationID: ch.FederationID, Challenge: ch.Challenge, PublicKey: pubOther}, other)
	if err != nil {
		t.Fatal(err)
	}
	if code := alpha.call("POST", "/api/v1/federation/recovery-key/verify", map[string]any{"challenge": ch.Challenge, "signature": forged}, nil); code != 400 {
		t.Fatalf("forged proof: %d", code)
	}
	if alpha.auditCount("federation.recovery_key.drill", "denied") != 1 {
		t.Fatal("failed drill not audited")
	}
	alpha.call("POST", "/api/v1/federation/recovery-key/challenge", nil, &ch)
	sig, err := federation.SignRecoveryChallenge(ch, recovery)
	if err != nil {
		t.Fatal(err)
	}
	var as federation.AuthorityStatus
	if code := alpha.call("POST", "/api/v1/federation/recovery-key/verify", map[string]any{"challenge": ch.Challenge, "signature": sig}, &as); code != 200 || as.Recovery.VerifiedAt == nil || as.Recovery.Due {
		t.Fatalf("verify: %d %+v", code, as.Recovery)
	}
	if code := alpha.call("POST", "/api/v1/federation/recovery-key/verify", map[string]any{"challenge": ch.Challenge, "signature": sig}, nil); code != 400 {
		t.Fatalf("challenge reuse: %d", code)
	}
	if alpha.auditCount("federation.recovery_key.drill", "success") != 1 {
		t.Fatal("drill not audited")
	}
}

func TestInterceptedJoinDoesNotConsumeInvitation(t *testing.T) {
	ctx := context.Background()
	alpha, beta, _, _ := threeMembers(t)
	_ = beta
	inv := alpha.invite("delta")
	delta := newInstance(t, "delta")

	// A relay (another member that obtained the secret on the way) completes
	// the join toward the authority but can never confirm it for the joiner.
	relayed := cloneJoinRequest(t, inv.Secret, delta.id)
	relayed.FederationURL = "https://127.0.0.1:2"
	body, _ := json.Marshal(relayed)
	if _, err := (peer.Client{}).DoPinned(ctx, alpha.fedURL, inv.CASHA256, "POST", "/federation/v1/join", body); err != nil {
		t.Fatalf("relayed join: %v", err)
	}
	var invs struct {
		Items []federation.Invitation `json:"items"`
	}
	alpha.call("GET", "/api/v1/federation/invitations", nil, &invs)
	state := func() string {
		alpha.call("GET", "/api/v1/federation/invitations", nil, &invs)
		for _, i := range invs.Items {
			if i.ID == inv.ID {
				return i.State
			}
		}
		return ""
	}
	if state() != "claimed" {
		t.Fatalf("invitation after a relayed join: %q", state())
	}
	// Another instance cannot take a claimed invitation.
	if code := newInstance(t, "eta").join(alpha, inv); code != 403 {
		t.Fatalf("claimed invitation used by another instance: %d", code)
	}
	// The real joiner retries with the same invitation: it replaces the
	// provisional member and the join completes (and confirms).
	if code := delta.join(alpha, inv); code != 200 {
		t.Fatalf("legitimate join after a relayed attempt: %d", code)
	}
	if state() != "consumed" {
		t.Fatalf("invitation after the confirmed join: %q", state())
	}
	if m := alpha.members()["delta"]; m.Status != "active" || m.FederationURL != delta.fedURL {
		t.Fatalf("member entry after the retry: %+v", m)
	}
	if err := delta.svc.Probe(ctx, alpha.id); err != nil {
		t.Fatalf("joined member: %v", err)
	}
	if code := newInstance(t, "epsilon").join(alpha, inv); code != 403 {
		t.Fatalf("consumed invitation reused: %d", code)
	}

	// A join nobody confirms is released after the claim timeout: the
	// provisional member is removed and its certificate revoked.
	inv2 := alpha.invite("zeta")
	zeta := newInstance(t, "zeta")
	abandoned := cloneJoinRequest(t, inv2.Secret, zeta.id)
	abandoned.FederationURL = "https://127.0.0.1:3"
	body, _ = json.Marshal(abandoned)
	if _, err := (peer.Client{}).DoPinned(ctx, alpha.fedURL, inv2.CASHA256, "POST", "/federation/v1/join", body); err != nil {
		t.Fatalf("abandoned join: %v", err)
	}
	if m := alpha.members()["zeta"]; m.Status != "active" {
		t.Fatalf("provisional member: %+v", m)
	}
	alpha.clock.Advance(11 * time.Minute)
	if err := alpha.svc.ReapUnconfirmedJoins(ctx); err != nil {
		t.Fatalf("reaper: %v", err)
	}
	if m := alpha.members()["zeta"]; m.Status != "removed" {
		t.Fatalf("provisional member after the timeout: %+v", m)
	}
	if alpha.auditCount("federation.join.abandoned", "success") != 1 {
		t.Fatal("abandoned join not audited")
	}
	if code := zeta.join(alpha, inv2); code != 200 {
		t.Fatalf("join with a released invitation: %d", code)
	}
}

func TestRevocationPruning(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, _ := threeMembers(t)
	if code := alpha.call("DELETE", "/api/v1/federation/members/"+beta.id+"?confirm=true", nil, nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	revoked := func() int { return len(alpha.document(alpha.generation()).RevokedCertSerials) }
	if revoked() != 1 {
		t.Fatalf("revoked serials: %d", revoked())
	}
	// Not expired yet: nothing to prune.
	gen := alpha.generation()
	if err := alpha.svc.PruneRevocations(ctx); err != nil || alpha.generation() != gen {
		t.Fatalf("early prune: %v (gen %d -> %d)", err, gen, alpha.generation())
	}
	// A certificate lives 30 days: after that margin the serial is dropped,
	// so the 4096 limit is never reached by an old federation.
	alpha.clock.Advance(33 * 24 * time.Hour)
	if err := alpha.svc.PruneRevocations(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	alpha.clock.Reset() // signed requests carry the clock: peers refuse a skew beyond 60 s
	if revoked() != 0 || alpha.generation() != gen+1 {
		t.Fatalf("after pruning: %d revoked, generation %d -> %d", revoked(), gen, alpha.generation())
	}
	if alpha.auditCount("federation.certificate.revocations_pruned", "success") != 1 {
		t.Fatal("pruning not audited")
	}
	// Peers accept the pruned list (it is a normal chain extension).
	eventually(t, "gamma applies the pruned generation", func() bool {
		_ = gamma.svc.SyncOnce(ctx)
		return gamma.generation() == gen+1
	})
}

func TestFederatedAudit(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, _, ci := searchFederation(t)
	_ = ctx
	accept := func(in *instance) {
		if code := in.call("PUT", "/api/v1/federation/settings", map[string]any{"accept_admin_grants": true, "policy_max_age_seconds": nil}, nil); code != 200 {
			t.Fatalf("%s settings: %d", in.name, code)
		}
	}
	// ci is a federated administrator, but each member decides: gamma keeps the
	// federated instance-admin grant inert.
	accept(alpha)
	accept(beta)
	query := func(in *instance, token string, params url.Values) (int, auditPage) {
		var out auditPage
		code := in.callAs(token, "GET", "/api/v1/federation/audit?"+params.Encode(), nil, &out)
		return code, out
	}
	code, page := query(alpha, ci["alpha"], url.Values{"action": {"federation.member.added"}})
	if code != 200 {
		t.Fatalf("federated audit: %d", code)
	}
	if got := page.member("gamma"); got.Status != "forbidden" || got.Code != "audit_read_denied" || !page.Partial {
		t.Fatalf("gamma should deny the caller: %+v partial=%v", got, page.Partial)
	}
	if page.member("alpha").Status != "ok" || page.member("beta").Status != "ok" {
		t.Fatalf("members: %+v", page.Members)
	}
	// Only the authority recorded member.added events (beta and gamma joined).
	if len(page.Items) != 2 {
		t.Fatalf("member.added events: %d (%+v)", len(page.Items), page.Items)
	}
	for _, e := range page.Items {
		if e.MemberName != "alpha" || e.Action != "federation.member.added" {
			t.Fatalf("event: %+v", e)
		}
	}
	// Local principals are not federated principals.
	if code, c := alpha.errorCode("GET", "/api/v1/federation/audit?limit=1", nil); code != 403 || c != "not_federated" {
		t.Fatalf("local administrator: %d %s", code, c)
	}
	// Merging: newest first across members, one page at a time, no duplicates.
	seen := map[string]bool{}
	var last time.Time
	total := 0
	cursor := ""
	for range 200 {
		params := url.Values{"prefix": {"federation."}, "limit": {"7"}, "members": {"alpha,beta"}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		code, pg := query(beta, ci["beta"], params)
		if code != 200 {
			t.Fatalf("page: %d", code)
		}
		if len(pg.Items) > 7 {
			t.Fatalf("page of %d items for limit 7", len(pg.Items))
		}
		for _, e := range pg.Items {
			key := fmt.Sprintf("%s/%d", e.Member, e.ID)
			if seen[key] {
				t.Fatalf("duplicate event %s", key)
			}
			seen[key] = true
			if total > 0 && e.CreatedAt.After(last) {
				t.Fatalf("not newest first: %s after %s", e.CreatedAt, last)
			}
			last = e.CreatedAt
			total++
		}
		if !pg.Page.HasMore {
			break
		}
		cursor = pg.Page.NextCursor
	}
	if total < 10 {
		t.Fatalf("only %d federation events across the members", total)
	}
	// A tampered or foreign cursor is rejected.
	_, pg := query(beta, ci["beta"], url.Values{"prefix": {"federation."}, "limit": {"2"}, "members": {"alpha,beta"}})
	if pg.Page.NextCursor == "" {
		t.Fatal("no cursor")
	}
	if code, _ := query(beta, ci["beta"], url.Values{"prefix": {"federation."}, "limit": {"2"}, "members": {"alpha,beta"}, "cursor": {pg.Page.NextCursor + "x"}}); code != 400 {
		t.Fatalf("tampered cursor: %d", code)
	}
	if code, _ := query(beta, ci["beta"], url.Values{"prefix": {"federation."}, "limit": {"3"}, "members": {"alpha,beta"}, "cursor": {pg.Page.NextCursor}}); code != 400 {
		t.Fatalf("cursor of another query: %d", code)
	}
	// A member that is down is reported, not fatal.
	alpha.proxy.set(faultDrop)
	code, page = query(beta, ci["beta"], url.Values{"action": {"federation.member.added"}, "members": {"alpha,beta"}})
	if code != 200 || page.member("alpha").Status == "ok" || !page.Partial {
		t.Fatalf("audit with alpha down: %d %+v", code, page.Members)
	}
	alpha.proxy.set(faultPass)
	// Once gamma accepts federated administrators it answers too.
	accept(gamma)
	code, page = query(alpha, ci["alpha"], url.Values{"prefix": {"federation."}, "limit": {"5"}})
	if code != 200 || page.member("gamma").Status != "ok" || page.Partial {
		t.Fatalf("after gamma accepts: %d %+v partial=%v", code, page.Members, page.Partial)
	}
}

type auditPage struct {
	Items   []federation.MergedAuditEntry  `json:"items"`
	Members []federation.MemberAuditStatus `json:"members"`
	Partial bool                           `json:"partial"`
	Page    struct {
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	} `json:"page"`
}

func (p auditPage) member(name string) federation.MemberAuditStatus {
	for _, m := range p.Members {
		if m.Name == name {
			return m
		}
	}
	return federation.MemberAuditStatus{Name: name, Status: "absent"}
}

var _ = strings.TrimSpace

// parseCA returns the SHA-256 fingerprint of a PEM CA certificate.
func parseCA(pemCert string) (string, error) {
	block, _ := pem.Decode([]byte(pemCert))
	if block == nil {
		return "", fmt.Errorf("not a PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}
