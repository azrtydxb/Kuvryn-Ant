package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
)

// found founds federation "lab" on alpha and joins the other instances.
func found(t *testing.T, alpha *instance, members ...*instance) federation.FoundResult {
	t.Helper()
	var founded federation.FoundResult
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "lab", "member_name": alpha.name}, &founded); code != 201 {
		t.Fatalf("found: %d", code)
	}
	for _, in := range members {
		if code := in.join(alpha, alpha.invite(in.name)); code != 200 {
			t.Fatalf("join %s: %d", in.name, code)
		}
	}
	return founded
}

// respell replaces the signature spelling of a stored document (kind
// membership or policy) and moves the applied hash to it, as a member that
// applied a relayed, re-spelled copy before strict decoding would hold.
func (in *instance) respell(kind string, gen int64) string {
	in.t.Helper()
	ctx := context.Background()
	var body []byte
	if err := in.pool.QueryRow(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND kind=$2 AND generation=$3`, in.id, kind, gen).Scan(&body); err != nil {
		in.t.Fatal(err)
	}
	var hash string
	var sig string
	switch kind {
	case "membership":
		var d federation.Membership
		_ = json.Unmarshal(body, &d)
		d.Signature.Value = malleate(d.Signature.Value)
		sig, hash = d.Signature.Value, federation.Hash(&d)
	default:
		var d federation.PolicyDocument
		_ = json.Unmarshal(body, &d)
		d.Signature.Value = malleate(d.Signature.Value)
		sig, hash = d.Signature.Value, federation.PolicyHash(&d)
	}
	tx, err := in.pool.Begin(ctx)
	if err != nil {
		in.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`ALTER TABLE federation_documents DISABLE TRIGGER federation_documents_immutable`,
		`UPDATE federation_documents SET body=jsonb_set(body,'{signature,value}',to_jsonb($4::text)),hash=$5 WHERE instance_id=$1 AND kind=$2 AND generation=$3`,
		`ALTER TABLE federation_documents ENABLE TRIGGER federation_documents_immutable`} {
		var args []any
		if strings.HasPrefix(q, "UPDATE") {
			args = []any{in.id, kind, gen, sig, hash}
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			in.t.Fatal(err)
		}
	}
	column := "applied_hash"
	if kind == "policy" {
		column = "policy_hash"
	}
	if _, err := tx.Exec(ctx, `UPDATE federation_state SET `+column+`=$2 WHERE instance_id=$1`, in.id, hash); err != nil {
		in.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		in.t.Fatal(err)
	}
	return hash
}

func malleate(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, s[len(s)-1])
	return s[:len(s)-1] + string(alphabet[i|1])
}

// A member whose applied chain diverged from the authority's recovers by a
// snapshot fetch from the authority (§6.4 rule 6) instead of staying stuck.
func TestFederationSnapshotResync(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma := newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")
	found(t, alpha, beta)

	// Same generation, another spelling: detected through the heads.
	gen := beta.generation()
	bad := beta.respell("membership", gen)
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("resync at the same generation: %v", err)
	}
	if got := beta.status().Membership.Hash; got == bad || got != alpha.status().Membership.Hash {
		t.Fatalf("beta still holds the divergent document: %s", got)
	}

	// Divergent generation followed by a new one: the chain mismatch on the
	// next generation triggers the resync.
	beta.respell("membership", gen)
	if code := gamma.join(alpha, alpha.invite("gamma")); code != 200 {
		t.Fatalf("join gamma: %d", code)
	}
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("resync after a chain mismatch: %v", err)
	}
	if beta.generation() != alpha.generation() || beta.document(gen).Signature.Value != alpha.document(gen).Signature.Value {
		t.Fatalf("beta did not recover: generation %d, authority %d", beta.generation(), alpha.generation())
	}
	if beta.auditCount("federation.document.resynced", "success") != 2 {
		t.Fatal("resync not audited")
	}

	// The same holds for the policy chain.
	if code := alpha.call("PUT", "/api/v1/federation/policy/draft", map[string]any{"policy": policyBody()}, nil); code != 200 {
		t.Fatalf("draft: %d", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true}, nil); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	eventually(t, "policy on beta", func() bool { return beta.policyGeneration() == 1 })
	beta.respell("policy", 1)
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 1, "confirm": true, "policy": policyBody()}, nil); code != 200 {
		t.Fatalf("publish 2: %d", code)
	}
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("policy resync: %v", err)
	}
	if beta.policyGeneration() != 2 || beta.policyView().Status.Hash != alpha.policyView().Status.Hash {
		t.Fatalf("beta policy did not recover: %+v", beta.policyView().Status)
	}
}

// An invalid invitation is refused without waiting for the federation state
// lock, and a stream of refusals produces throttled audit records.
func TestFederationJoinRejectsBeforeLocking(t *testing.T) {
	ctx := context.Background()
	alpha := newInstance(t, "alpha")
	founded := found(t, alpha)
	bogus := func() error {
		var id string
		_ = alpha.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id)
		var inst string
		_ = alpha.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&inst)
		body, _ := json.Marshal(cloneJoinRequest(t, id+".not-the-secret-not-the-secret", inst))
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err := (peer.Client{}).DoPinned(cctx, alpha.fedURL, founded.CASHA256, "POST", "/federation/v1/join", body)
		return err
	}
	// Hold the federation state lock (as a long federation change would).
	tx, err := alpha.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM federation_state WHERE instance_id=$1 FOR UPDATE`, alpha.id); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = bogus()
	_ = tx.Rollback(ctx)
	if peerCode(err) != "invitation_invalid" || time.Since(start) > 2*time.Second {
		t.Fatalf("bogus join while the state is locked: %v after %v", err, time.Since(start))
	}
	for range 7 {
		if err := bogus(); peerCode(err) != "invitation_invalid" {
			t.Fatalf("bogus join: %v", err)
		}
	}
	if n := alpha.auditCount("federation.invitation.rejected", "denied"); n != 1 {
		t.Fatalf("rejection audit records for 8 refusals within the window: %d, want 1", n)
	}
	// Beyond the per-IP burst the endpoint answers 429 before any work.
	limited := false
	for range 2 * peer.DefaultPublicBurst {
		if peerCode(bogus()) == "rate_limited" {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("join endpoint not rate-limited")
	}
}
