package harness

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
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

// rotateRecovery runs the offline rotation flow (template, offline signing,
// signed submission) and checks that the server refuses a private key, a
// statement signed by another key and a statement for another generation.
func rotateRecovery(t *testing.T, authority *instance, current string) federation.RecoveryRotationResult {
	t.Helper()
	var tpl federation.RecoveryTemplate
	if code := authority.call("GET", "/api/v1/federation/recovery-key/rotation", nil, &tpl); code != 200 {
		t.Fatalf("rotation template: %d", code)
	}
	newKey, newPub, err := federation.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	rot, err := federation.SignRecoveryRotation(tpl, current, newPub)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := federation.SignAuthorityCertificate(tpl, newKey)
	if err != nil {
		t.Fatal(err)
	}
	// The private key is never accepted by the API (no recovery_key field).
	req, _ := newJSONRequest("POST", authority.api.URL+"/api/v1/federation/recovery-key/rotate", []byte(`{"recovery_key":"`+current+`"}`))
	req.Header.Set("Authorization", "Bearer "+authority.token)
	if resp, err := httpDo(req); err != nil || resp.status != 400 {
		t.Fatalf("rotation with a private key: %+v %v", resp, err)
	}
	// Signed by a key that is not the current recovery key.
	stranger, _, _ := federation.NewRecoveryKey()
	forgedTpl := tpl
	forgedTpl.PreviousKey, _ = federation.RecoveryPublicKey(stranger)
	forged, _ := federation.SignRecoveryRotation(forgedTpl, stranger, newPub)
	forged.PreviousKey = tpl.PreviousKey
	if code := authority.call("POST", "/api/v1/federation/recovery-key/rotate", federation.SignedRecoveryRotation{Rotation: forged, AuthorityCertificate: cert}, nil); code != 400 {
		t.Fatalf("rotation signed by another key: %d", code)
	}
	// Signed for another generation.
	staleTpl := tpl
	staleTpl.Generation++
	stale, _ := federation.SignRecoveryRotation(staleTpl, current, newPub)
	if code := authority.call("POST", "/api/v1/federation/recovery-key/rotate", federation.SignedRecoveryRotation{Rotation: stale, AuthorityCertificate: cert}, nil); code != 409 {
		t.Fatalf("rotation for another generation: %d", code)
	}
	var out federation.RecoveryRotationResult
	if code := authority.call("POST", "/api/v1/federation/recovery-key/rotate", federation.SignedRecoveryRotation{Rotation: rot, AuthorityCertificate: cert}, &out); code != 200 {
		t.Fatalf("rotate recovery key: %d", code)
	}
	if out.RecoveryPublicKey != newPub {
		t.Fatalf("rotated to %s, want %s", out.RecoveryPublicKey, newPub)
	}
	return out
}

// A federated all-feed allow is inert on a member until it accepts admin
// grants locally.
func TestFederationWildcardGrantsGated(t *testing.T) {
	alpha, beta := newInstance(t, "alpha"), newInstance(t, "beta")
	found(t, alpha, beta)
	body := policyBody()
	body["roles"] = append(body["roles"].([]any), map[string]any{"key": "fed:everything", "revision": 1, "grants": []any{
		map[string]any{"kind": "feed", "effect": "allow", "actions": []string{"read", "publish"}},
	}})
	if code := alpha.call("POST", "/api/v1/federation/policy/publish", map[string]any{"base_generation": 0, "confirm": true, "policy": body}, nil); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	eventually(t, "policy on beta", func() bool { return beta.policyGeneration() == 1 })
	inert := func() string {
		var v string
		if err := beta.pool.QueryRow(context.Background(), `SELECT coalesce(g.inert,'') FROM role_grants g JOIN roles r ON (r.instance_id,r.id)=(g.instance_id,g.role_id)
WHERE g.instance_id=$1 AND r.name='fed:everything'`, beta.id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := inert(); got != "wildcard_not_accepted" {
		t.Fatalf("all-feed grant before acceptance: %q", got)
	}
	if r := beta.policyView().Status.Report; r.InertAdminGrants != 2 {
		t.Fatalf("inert grants reported: %d, want 2 (instance admin and all-feed)", r.InertAdminGrants)
	}
	if code := beta.call("PUT", "/api/v1/federation/settings", map[string]any{"accept_admin_grants": true, "policy_max_age_seconds": nil}, nil); code != 200 {
		t.Fatalf("accept admin grants: %d", code)
	}
	if got := inert(); got != "" {
		t.Fatalf("all-feed grant after acceptance: %q", got)
	}
}

// memberIdentity rebuilds an instance's peer identity from its database,
// presenting certPEM (its current certificate when empty).
func (in *instance) memberIdentity(certPEM string) peer.Identity {
	in.t.Helper()
	ctx := context.Background()
	var secretID, current string
	if err := in.pool.QueryRow(ctx, `SELECT member_secret_id::text,member_cert FROM federation_state WHERE instance_id=$1`, in.id).Scan(&secretID, &current); err != nil {
		in.t.Fatal(err)
	}
	if certPEM == "" {
		certPEM = current
	}
	_, plain, err := (secrets.Store{DB: in.pool, Keys: in.keys}).Open(ctx, in.id, secretID, "federation-member-key")
	if err != nil {
		in.t.Fatal(err)
	}
	var mat struct {
		TLSKeyDER  []byte `json:"tls_key_der"`
		SigningKey []byte `json:"signing_key"`
	}
	if err := json.Unmarshal(plain, &mat); err != nil {
		in.t.Fatal(err)
	}
	key, err := x509.ParseECPrivateKey(mat.TLSKeyDER)
	if err != nil {
		in.t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	doc := in.document(in.generation())
	caBlock, _ := pem.Decode([]byte(doc.CACertificates[0]))
	ca, _ := x509.ParseCertificate(caBlock.Bytes)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return peer.Identity{InstanceID: in.id, SigningKey: ed25519.NewKeyFromSeed(mat.SigningKey), Roots: roots,
		Certificate: &tls.Certificate{Certificate: [][]byte{block.Bytes, ca.Raw}, PrivateKey: key}}
}

func (in *instance) memberCert() string {
	in.t.Helper()
	var c string
	if err := in.pool.QueryRow(context.Background(), `SELECT member_cert FROM federation_state WHERE instance_id=$1`, in.id).Scan(&c); err != nil {
		in.t.Fatal(err)
	}
	return c
}

// Peers accept only a member's current certificate, and its previous one
// during the renewal grace period; the previous serial is revoked after the
// grace period, and a renewal cannot move the member to a key it does not hold.
func TestFederationCertificatePinning(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma := newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")
	found(t, alpha, beta, gamma)
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	status := func(id peer.Identity, to *instance) string {
		_, err := (peer.Client{Now: beta.clock.Now}).Do(ctx, id, to.fedURL, "GET", "/federation/v1/status", nil, to.id)
		return peerCode(err)
	}
	old := beta.memberCert()
	if code := status(beta.memberIdentity(""), alpha); code != "" {
		t.Fatalf("current certificate: %s", code)
	}
	// A certificate the CA issued for beta's instance ID that membership does
	// not publish (here: issued directly with the CA key) is refused.
	rogue := issueWithCA(t, alpha, beta)
	if code := status(beta.memberIdentity(rogue), alpha); code != "certificate_not_current" {
		t.Fatalf("unpublished certificate for the same instance: %q", code)
	}

	// Renewal: the old certificate stays accepted during the grace period.
	if err := beta.svc.RenewCertificate(ctx); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if beta.memberCert() == old {
		t.Fatal("certificate not renewed")
	}
	if err := gamma.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, peerInst := range []*instance{alpha, gamma} {
		if code := status(beta.memberIdentity(""), peerInst); code != "" {
			t.Fatalf("renewed certificate at %s: %s", peerInst.name, code)
		}
		if code := status(beta.memberIdentity(old), peerInst); code != "" {
			t.Fatalf("previous certificate during the grace period at %s: %s", peerInst.name, code)
		}
	}
	// After the grace period the authority revokes the previous serial.
	for _, in := range []*instance{alpha, beta, gamma} {
		in.clock.Advance(25 * time.Hour)
	}
	if err := alpha.svc.RetireCertificates(ctx); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := gamma.svc.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	head := alpha.document(alpha.generation())
	if m, _ := head.Member(beta.id); m.PreviousCertSerial != "" || !head.Revoked(serialOf(t, old)) {
		t.Fatalf("previous certificate not retired: %+v", m)
	}
	for _, peerInst := range []*instance{alpha, gamma} {
		if code := status(beta.memberIdentity(old), peerInst); code != "certificate_revoked" {
			t.Fatalf("previous certificate after the grace period at %s: %q", peerInst.name, code)
		}
		if code := status(beta.memberIdentity(""), peerInst); code != "" {
			t.Fatalf("current certificate after retirement at %s: %s", peerInst.name, code)
		}
	}

	// A renewal CSR for another key needs a signature by the current key.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: beta.id}}, other)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	body, _ := json.Marshal(map[string]string{"csr": csr})
	if _, err := (peer.Client{Now: beta.clock.Now}).Do(ctx, beta.memberIdentity(""), alpha.fedURL, "POST", "/federation/v1/csr/renew", body, alpha.id); peerCode(err) != "csr_key_mismatch" {
		t.Fatalf("renewal onto a foreign key: %v", err)
	}
}

// issueWithCA signs a certificate for member's instance ID and key directly
// with the authority's CA key, bypassing membership.
func issueWithCA(t *testing.T, authority, member *instance) string {
	t.Helper()
	ctx := context.Background()
	var caID string
	if err := authority.pool.QueryRow(ctx, `SELECT ca_secret_id::text FROM federation_state WHERE instance_id=$1`, authority.id).Scan(&caID); err != nil {
		t.Fatal(err)
	}
	_, plain, err := (secrets.Store{DB: authority.pool, Keys: authority.keys}).Open(ctx, authority.id, caID, "federation-ca")
	if err != nil {
		t.Fatal(err)
	}
	var ca struct {
		KeyDER  []byte `json:"key_der"`
		CertPEM string `json:"cert_pem"`
	}
	_ = json.Unmarshal(plain, &ca)
	caKey, _ := x509.ParseECPrivateKey(ca.KeyDER)
	caBlock, _ := pem.Decode([]byte(ca.CertPEM))
	caCert, _ := x509.ParseCertificate(caBlock.Bytes)
	curBlock, _ := pem.Decode([]byte(member.memberCert()))
	cur, _ := x509.ParseCertificate(curBlock.Bytes)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(424242), Subject: pkix.Name{CommonName: member.id}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, IPAddresses: cur.IPAddresses, DNSNames: cur.DNSNames}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, cur.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func serialOf(t *testing.T, certPEM string) string {
	t.Helper()
	b, _ := pem.Decode([]byte(certPEM))
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.SerialNumber.Text(16)
}

// A member's CA-issued certificate cannot be used to intercept another
// instance's join: the joiner checks that the answering server presents the
// authority's published certificate (and can pin it up front from the
// invitation), and the authority refuses a second member on the same host.
func TestFederationJoinInterception(t *testing.T) {
	ctx := context.Background()
	alpha, gamma, delta, epsilon := newInstance(t, "alpha"), newInstance(t, "gamma"), newInstance(t, "delta"), newInstance(t, "epsilon")
	founded := found(t, alpha, gamma)

	// gamma relays joins to the authority with its own member certificate,
	// which chains to the pinned CA and is valid for the shared lab host.
	upstream, _ := url.Parse(alpha.fedURL)
	relay := httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(upstream) },
		Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}}, //nolint:gosec // test relay
	})
	relay.TLS = &tls.Config{Certificates: []tls.Certificate{*gamma.memberIdentity("").Certificate}, MinVersion: tls.VersionTLS13}
	relay.StartTLS()
	defer relay.Close()
	inv := alpha.invite("delta")
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	code := delta.call("POST", "/api/v1/federation/join", map[string]any{"authority_url": relay.URL, "ca_sha256": inv.CASHA256, "invitation": inv.Secret}, &e)
	if code != 409 || !strings.Contains(e.Message, "authority_certificate_mismatch") {
		t.Fatalf("join answered through another member's certificate: %d %+v", code, e)
	}
	if st := delta.status(); st.State != "none" {
		t.Fatalf("delta applied an intercepted join: %+v", st)
	}

	// With the authority certificate from the invitation the relay is refused
	// before the invitation is sent.
	inv2 := alpha.invite("epsilon")
	if inv2.AuthorityCertSHA256 == "" || inv2.AuthorityCertSHA256 != alpha.members()["alpha"].CertSHA256 {
		t.Fatalf("invitation authority certificate: %q", inv2.AuthorityCertSHA256)
	}
	pinned := map[string]any{"authority_url": relay.URL, "ca_sha256": inv2.CASHA256, "authority_cert_sha256": inv2.AuthorityCertSHA256, "invitation": inv2.Secret}
	if code := epsilon.call("POST", "/api/v1/federation/join", pinned, nil); code != 503 {
		t.Fatalf("pinned join through the relay: %d", code)
	}
	pinned["authority_url"] = alpha.fedURL
	if code := epsilon.call("POST", "/api/v1/federation/join", pinned, nil); code != 200 {
		t.Fatalf("pinned join (the invitation must still be unused): %d", code)
	}

	// A second member on an existing member's host is refused and audited.
	var id string
	_ = alpha.pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id)
	req := cloneJoinRequest(t, alpha.invite("zeta").Secret, id)
	req.FederationURL = gamma.fedURL
	body, _ := json.Marshal(req)
	if _, err := (peer.Client{}).DoPinned(ctx, alpha.fedURL, founded.CASHA256, "POST", "/federation/v1/join", body); peerCode(err) != "duplicate_instance" {
		t.Fatalf("join on another member's host: %v", err)
	}
	var n int
	_ = alpha.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='federation.invitation.rejected' AND details->>'reason'='duplicate_host'`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate host audits: %d", n)
	}
}
