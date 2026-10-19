package harness

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgxpool"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	return dsn
}

var dbCounter atomic.Int64

// instance is one simulated Kuvryn Ant instance.
type instance struct {
	t        *testing.T
	name     string
	pool     *pgxpool.Pool
	id       string
	token    string
	keys     *secrets.Keyring
	svc      *federation.Service
	addr     string // federation listener address
	fedURL   string
	api      *httptest.Server
	stopPeer context.CancelFunc
	router   routers.Router
	clock    *fakeClock
	proxy    *faultProxy
	mux      *http.ServeMux
	repl     *replRuntime
}

func newInstance(t *testing.T, name string) *instance {
	t.Helper()
	ctx := context.Background()
	base, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	db := fmt.Sprintf("ant_fed_%s_%d_%d", name, time.Now().UnixNano(), dbCounter.Add(1))
	if _, err = adminPool.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg := base.Copy()
	cfg.ConnConfig.Database = db
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	in := &instance{t: t, name: name, pool: pool}
	t.Cleanup(func() {
		in.stop()
		if in.api != nil {
			in.api.Close()
		}
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		adminPool.Close()
	})
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, name)
	if err != nil {
		t.Fatal(err)
	}
	in.id = owner.InstanceID
	all := []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}
	tok, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: all}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	in.token = tok.Secret
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	if in.keys, err = secrets.New("v1", map[string][]byte{"v1": key}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	in.addr = ln.Addr().String()
	// Peers reach this instance only through its fault proxy.
	in.proxy = newFaultProxy(t, in.addr)
	in.clock = &fakeClock{}
	in.fedURL = "https://" + in.proxy.addr()
	// The management API listens before the service is built, so membership
	// carries the real API URL (token-mode replication peers point at it).
	apiServer := httptest.NewUnstartedServer(nil)
	in.svc, err = federation.New(federation.Config{DB: pool, Keys: in.keys, InstanceID: in.id, PublicURL: in.fedURL, ListenAddress: in.addr,
		APIURL: "http://" + apiServer.Listener.Addr().String(), SoftwareVersion: "harness", PeerTimeout: 5 * time.Second, Logger: quiet, Now: in.clock.Now,
		SearchMemberTimeout: 700 * time.Millisecond, SearchTimeout: 2 * time.Second, SearchPeerRate: 30, SearchPrincipalRate: 15})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	in.serve(ln)
	mux := http.NewServeMux()
	management.Register(mux)
	management.RegisterFeeds(mux, pool, management.NewCursorCodec(key))
	management.RegisterFederation(mux, pool, in.svc)
	management.RegisterFederationSearch(mux, pool, in.svc, management.NewCursorCodec(key))
	management.RegisterIdentities(mux, pool)
	in.mux = mux
	apiServer.Config.Handler = management.WithRequestID(mux)
	apiServer.Start()
	in.api = apiServer
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	doc.Servers = nil
	if in.router, err = legacy.NewRouter(doc); err != nil {
		t.Fatal(err)
	}
	return in
}

// serve starts the federation listener; stop closes it (simulated outage).
func (in *instance) serve(ln net.Listener) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = in.svc.Serve(ctx, ln); close(done) }()
	in.stopPeer = func() { cancel(); <-done }
}

func (in *instance) stop() {
	if in.stopPeer != nil {
		in.stopPeer()
		in.stopPeer = nil
	}
}

func (in *instance) restart() {
	in.t.Helper()
	var ln net.Listener
	var err error
	for range 50 {
		if ln, err = net.Listen("tcp", in.addr); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		in.t.Fatalf("%s: cannot rebind federation listener: %v", in.name, err)
	}
	in.serve(ln)
}

// call performs a management request, validating request and response
// against the OpenAPI contract, and decodes the JSON response into out.
func (in *instance) call(method, path string, body any, out any) int {
	in.t.Helper()
	return in.callAs(in.token, method, path, body, out)
}

// callAs is call with another bearer token.
func (in *instance) callAs(token, method, path string, body any, out any) int {
	in.t.Helper()
	var reader io.Reader
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, in.api.URL+path, reader)
	if err != nil {
		in.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if replaySafe(method, path) {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("harness-%d", dbCounter.Add(1)))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		in.t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// Contract validation on a copy of the request with the original body.
	vreq, _ := http.NewRequest(method, "http://contract.test"+path, bytes.NewReader(raw))
	vreq.Header = req.Header.Clone()
	route, params, err := in.router.FindRoute(vreq)
	if err != nil {
		in.t.Fatalf("%s %s: no contract route: %v", method, path, err)
	}
	input := &openapi3filter.RequestValidationInput{Request: vreq, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	if err := openapi3filter.ValidateRequest(context.Background(), input); err != nil {
		in.t.Fatalf("%s %s: request contract: %v", method, path, err)
	}
	output := (&openapi3filter.ResponseValidationInput{RequestValidationInput: input, Status: resp.StatusCode, Header: resp.Header}).SetBodyBytes(respBody)
	if err := openapi3filter.ValidateResponse(context.Background(), output); err != nil {
		in.t.Fatalf("%s %s: response contract (%d %s): %v", method, path, resp.StatusCode, respBody, err)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			in.t.Fatalf("%s %s: decode: %v (%s)", method, path, err, respBody)
		}
	}
	return resp.StatusCode
}

// replaySafe reports the federation mutations that require Idempotency-Key.
func replaySafe(method, path string) bool {
	if method == "GET" {
		return false
	}
	for _, once := range []string{"/api/v1/federation", "/api/v1/federation/invitations", "/api/v1/federation/join", "/api/v1/federation/leave",
		"/api/v1/federation/recovery-key/rotate", "/api/v1/federation/access-check",
		"/api/v1/federation/authority/handover", "/api/v1/federation/authority/handover/abort", "/api/v1/federation/authority/recovery-template",
		"/api/v1/federation/authority/recover", "/api/v1/federation/recovery-key/challenge", "/api/v1/federation/recovery-key/verify"} {
		if path == once {
			return false
		}
	}
	return true
}

func (in *instance) status() federation.Status {
	in.t.Helper()
	var st federation.Status
	if code := in.call("GET", "/api/v1/federation/status", nil, &st); code != 200 {
		in.t.Fatalf("%s status: %d", in.name, code)
	}
	return st
}

func (in *instance) generation() int64 {
	in.t.Helper()
	st := in.status()
	if st.Membership == nil {
		return 0
	}
	return st.Membership.Generation
}

func (in *instance) members() map[string]federation.MemberView {
	in.t.Helper()
	var list struct {
		Items []federation.MemberView `json:"items"`
	}
	if code := in.call("GET", "/api/v1/federation/members", nil, &list); code != 200 {
		in.t.Fatalf("%s members: %d", in.name, code)
	}
	out := map[string]federation.MemberView{}
	for _, m := range list.Items {
		out[m.Name] = m
	}
	return out
}

func (in *instance) auditCount(action, result string) int {
	in.t.Helper()
	var n int
	if err := in.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE instance_id=$1 AND action=$2 AND result=$3`, in.id, action, result).Scan(&n); err != nil {
		in.t.Fatal(err)
	}
	return n
}

func (in *instance) invite(name string) federation.IssuedInvitation {
	in.t.Helper()
	var inv federation.IssuedInvitation
	if code := in.call("POST", "/api/v1/federation/invitations", map[string]any{"name": name, "labels": map[string]string{"site": name}}, &inv); code != 201 {
		in.t.Fatalf("invite %s: %d", name, code)
	}
	return inv
}

func (in *instance) join(auth *instance, inv federation.IssuedInvitation) int {
	in.t.Helper()
	return in.call("POST", "/api/v1/federation/join", map[string]any{"authority_url": auth.fedURL, "ca_sha256": inv.CASHA256, "invitation": inv.Secret}, nil)
}

// document returns a generation from the instance's local log.
func (in *instance) document(gen int64) federation.Membership {
	in.t.Helper()
	var d struct {
		Document federation.Membership `json:"document"`
	}
	if code := in.call("GET", fmt.Sprintf("/api/v1/federation/documents/membership/%d", gen), nil, &d); code != 200 {
		in.t.Fatalf("%s document %d: %d", in.name, gen, code)
	}
	return d.Document
}

// authorityKey opens the authority signing key, so scenarios can forge
// correctly signed but otherwise invalid documents (stale, gap, fork).
func (in *instance) authorityKey(epoch int64) ed25519.PrivateKey {
	in.t.Helper()
	var id string
	if err := in.pool.QueryRow(context.Background(), `SELECT authority_secret_id::text FROM federation_state WHERE instance_id=$1`, in.id).Scan(&id); err != nil {
		in.t.Fatal(err)
	}
	_, seed, err := (secrets.Store{DB: in.pool, Keys: in.keys}).Open(context.Background(), in.id, id, fmt.Sprintf("federation-authority:%d", epoch))
	if err != nil {
		in.t.Fatal(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func peerCode(err error) string {
	var pe *peer.ErrPeer
	if errors.As(err, &pe) {
		return pe.Code
	}
	if err == nil {
		return ""
	}
	return "error: " + err.Error()
}

func TestFederationMembershipScenarios(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma := newInstance(t, "alpha"), newInstance(t, "beta"), newInstance(t, "gamma")

	// Federation off: the instance reports it disabled and serves normally.
	if st := beta.status(); st.State != "none" || st.Health != "disabled" || !st.ListenerConfigured {
		t.Fatalf("unfederated status: %+v", st)
	}

	// --- found ---
	var founded federation.FoundResult
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "lab", "member_name": "alpha", "labels": map[string]string{"site": "alpha"}}, &founded); code != 201 {
		t.Fatalf("found: %d", code)
	}
	if !strings.HasPrefix(founded.RecoveryKey, "ant-recovery-v1:") || founded.Status.Role != "authority" || founded.Status.Membership.Generation != 1 || len(founded.CASHA256) != 64 {
		t.Fatalf("found result: %+v", founded.Status)
	}
	// Secret-returning operations are not replayable: Idempotency-Key is refused.
	keyed, _ := http.NewRequest("POST", alpha.api.URL+"/api/v1/federation/invitations", strings.NewReader(`{"name":"keyed"}`))
	keyed.Header.Set("Authorization", "Bearer "+alpha.token)
	keyed.Header.Set("Content-Type", "application/json")
	keyed.Header.Set("Idempotency-Key", "k")
	if resp, err := http.DefaultClient.Do(keyed); err != nil || resp.StatusCode != 400 {
		t.Fatalf("invitation with Idempotency-Key: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	if code := alpha.call("POST", "/api/v1/federation", map[string]any{"name": "again"}, nil); code != 409 {
		t.Fatalf("second found: %d", code)
	}
	// The recovery private key is never stored.
	var stored int
	_ = alpha.pool.QueryRow(ctx, `SELECT count(*) FROM encrypted_secrets WHERE purpose LIKE 'federation-recovery%'`).Scan(&stored)
	if stored != 0 {
		t.Fatal("recovery key persisted")
	}

	// --- join ---
	invB := alpha.invite("beta")
	if invB.AuthorityURL != alpha.fedURL || invB.CASHA256 != founded.CASHA256 {
		t.Fatalf("invitation: %+v", invB.Invitation)
	}
	// A wrong CA pin never reaches the authority's join handler.
	wrong := invB
	wrong.CASHA256 = strings.Repeat("0", 64)
	if code := beta.join(alpha, wrong); code != 503 {
		t.Fatalf("join with wrong CA pin: %d", code)
	}
	if code := beta.join(alpha, invB); code != 200 {
		t.Fatalf("join beta: %d", code)
	}
	if st := beta.status(); st.State != "active" || st.Role != "member" || st.Membership.Generation != 2 || st.FederationName != "lab" {
		t.Fatalf("beta after join: %+v", st)
	}
	// The invitation is one-time: reuse is rejected and audited on the authority.
	if code := gamma.join(alpha, invB); code != 403 {
		t.Fatalf("invitation reuse: %d", code)
	}
	if alpha.auditCount("federation.invitation.rejected", "denied") == 0 {
		t.Fatal("rejected invitation not audited")
	}
	invG := alpha.invite("gamma")
	if code := gamma.join(alpha, invG); code != 200 {
		t.Fatalf("join gamma: %d", code)
	}

	// --- duplicate instance ID (a restored clone) is rejected and audited ---
	invDup := alpha.invite("clone")
	dupBody, _ := json.Marshal(cloneJoinRequest(t, invDup.Secret, gamma.id))
	_, err := (peer.Client{}).DoPinned(ctx, alpha.fedURL, invDup.CASHA256, "POST", "/federation/v1/join", dupBody)
	if peerCode(err) != "duplicate_instance" {
		t.Fatalf("duplicate instance join: %v", err)
	}
	var dupAudits int
	_ = alpha.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='federation.invitation.rejected' AND details->>'reason'='duplicate_instance'`).Scan(&dupAudits)
	if dupAudits != 1 {
		t.Fatalf("duplicate instance audits: %d", dupAudits)
	}

	// --- sync ---
	if err := beta.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("beta sync: %v", err)
	}
	for _, in := range []*instance{alpha, beta, gamma} {
		if g := in.generation(); g != 3 {
			t.Fatalf("%s generation %d, want 3", in.name, g)
		}
		ms := in.members()
		if len(ms) != 3 || ms["beta"].Status != "active" || ms["gamma"].Labels["site"] != "gamma" || !ms["alpha"].Authority || !ms[in.name].Self {
			t.Fatalf("%s members: %+v", in.name, ms)
		}
	}
	// Peers answer each other over mTLS with signed requests.
	if err := beta.svc.Probe(ctx, gamma.id); err != nil {
		t.Fatalf("beta -> gamma probe: %v", err)
	}
	beta.svc.PingMembers(ctx)
	if m := beta.members()["gamma"]; m.LastSeenAt == nil || m.RTTMS == nil {
		t.Fatalf("ping did not record RTT: %+v", m)
	}

	// --- recovery-key rotation, relay through a non-authority member ---
	gamma.stop()
	rotated := rotateRecovery(t, alpha, founded.RecoveryKey)
	if rotated.Status.Membership.Generation != 4 || rotated.Status.RecoveryKeyFingerprint != federation.KeyFingerprint(rotated.RecoveryPublicKey) {
		t.Fatalf("rotation result: %+v", rotated.Status)
	}
	if err := beta.svc.SyncOnce(ctx); err != nil || beta.generation() != 4 {
		t.Fatalf("beta sync to 4: %v (gen %d)", err, beta.generation())
	}
	alpha.stop() // authority outage
	gamma.restart()
	if err := gamma.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("gamma relay sync: %v", err)
	}
	if st := gamma.status(); st.Membership.Generation != 4 || st.Sync.Source != "beta" {
		t.Fatalf("gamma did not relay through beta: %+v", st.Sync)
	}
	// Serving does not depend on federation: with the authority and the relay
	// down, beta reports degraded federation and keeps serving.
	gamma.stop()
	if err := beta.svc.SyncOnce(ctx); err == nil {
		t.Fatal("sync succeeded with no reachable member")
	}
	if st := beta.status(); st.Health != "degraded" || st.Sync.LastError == "" || st.State != "active" {
		t.Fatalf("beta status during outage: %+v", st)
	}
	if code := beta.call("GET", "/api/v1/feeds", nil, nil); code != 200 {
		t.Fatalf("feed management during federation outage: %d", code)
	}
	alpha.restart()
	gamma.restart()

	// --- tampered, stale, forged and gap documents are rejected ---
	gamma.stop()
	if err := alpha.svc.RenewCertificate(ctx); err != nil { // authority self-renewal publishes generation 5
		t.Fatalf("authority renewal: %v", err)
	}
	g5 := alpha.document(5)
	// The authority nudges asynchronously: keep gamma unreachable inbound so a
	// late nudge cannot deliver generation 5 before the rejection checks.
	gamma.proxy.set(faultDrop)
	gamma.restart()
	tampered := g5
	tampered.Members = append([]federation.Member(nil), g5.Members...)
	tampered.Members[0].Labels = map[string]string{"site": "evil"}
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{tampered}, "test"); rejectReason(err) != "bad_signature" {
		t.Fatalf("tampered document: %v", err)
	}
	key := alpha.authorityKey(1)
	stale := alpha.document(3)
	stale.IssuedBy = "replayed"
	if err := federation.Sign(&stale, key); err != nil {
		t.Fatal(err)
	}
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{stale}, "test"); rejectReason(err) != "stale_generation" {
		t.Fatalf("stale document: %v", err)
	}
	gap := g5
	gap.Generation = 6
	gap.PrevHash = federation.Hash(&g5)
	_ = federation.Sign(&gap, key)
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{gap}, "test"); rejectReason(err) != "generation_gap" {
		t.Fatalf("gap document: %v", err)
	}
	fork := g5
	fork.PrevHash = "sha256:" + strings.Repeat("ab", 32)
	_ = federation.Sign(&fork, key)
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{fork}, "test"); rejectReason(err) != "chain_mismatch" {
		t.Fatalf("forked document: %v", err)
	}
	rogue := g5
	_, rk, _ := ed25519.GenerateKey(rand.Reader)
	_ = federation.Sign(&rogue, rk)
	if _, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{rogue}, "test"); rejectReason(err) != "bad_signature" {
		t.Fatalf("document signed by an uncertified key: %v", err)
	}
	if g := gamma.generation(); g != 4 {
		t.Fatalf("rejected documents changed state: generation %d", g)
	}
	if n := gamma.auditCount("federation.document.rejected", "denied"); n != 5 {
		t.Fatalf("rejections audited: %d, want 5", n)
	}
	if st := gamma.status(); !strings.Contains(st.Sync.LastError, "rejected") {
		t.Fatalf("rejection not reported in status: %+v", st.Sync)
	}
	// Re-delivery of an already applied document is a harmless no-op.
	if n, err := gamma.svc.ApplyDocuments(ctx, []federation.Membership{gamma.document(4)}, "test"); err != nil || n != 0 {
		t.Fatalf("re-delivery: %d %v", n, err)
	}
	if err := gamma.svc.SyncOnce(ctx); err != nil || gamma.generation() != 5 {
		t.Fatalf("gamma sync after rejections: %v (gen %d)", err, gamma.generation())
	}
	gamma.proxy.set(faultPass)

	// --- remove: the revoked member is refused everywhere ---
	if code := alpha.call("DELETE", "/api/v1/federation/members/"+beta.id+"?confirm=true", nil, nil); code != 200 {
		t.Fatalf("remove beta: %d", code)
	}
	eventually(t, "beta learns its removal", func() bool { return beta.status().State == "removed" })
	if err := gamma.svc.SyncOnce(ctx); err != nil {
		t.Fatalf("gamma sync after removal: %v", err)
	}
	if code := peerCode(beta.svc.Probe(ctx, alpha.id)); code != "certificate_revoked" {
		t.Fatalf("removed member at authority: %q", code)
	}
	if code := peerCode(beta.svc.Probe(ctx, gamma.id)); code != "certificate_revoked" {
		t.Fatalf("removed member at peer: %q", code)
	}
	if alpha.auditCount("federation.peer.rejected", "denied") == 0 {
		t.Fatal("peer rejection not audited")
	}
	if code := alpha.call("DELETE", "/api/v1/federation/members/"+beta.id+"?confirm=true", nil, nil); code != 200 {
		t.Fatalf("idempotent remove: %d", code)
	}

	// --- suspend and resume ---
	if code := alpha.call("POST", "/api/v1/federation/members/"+gamma.id+"/suspend", nil, nil); code != 200 {
		t.Fatalf("suspend gamma: %d", code)
	}
	if code := peerCode(gamma.svc.Probe(ctx, alpha.id)); code != "member_suspended" {
		t.Fatalf("suspended member at authority: %q", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/members/"+gamma.id+"/resume", nil, nil); code != 200 {
		t.Fatalf("resume gamma: %d", code)
	}
	if err := gamma.svc.Probe(ctx, alpha.id); err != nil {
		t.Fatalf("resumed member: %v", err)
	}
	if code := alpha.call("POST", "/api/v1/federation/members/"+alpha.id+"/suspend", nil, nil); code != 409 {
		t.Fatalf("authority suspending itself: %d", code)
	}

	// --- leave ---
	if code := gamma.call("POST", "/api/v1/federation/leave", map[string]any{"confirm": false}, nil); code != 400 {
		t.Fatalf("leave without confirm: %d", code)
	}
	var left federation.Status
	if code := gamma.call("POST", "/api/v1/federation/leave", map[string]any{"confirm": true}, &left); code != 200 || left.State != "left" {
		t.Fatalf("leave: %d %+v", code, left)
	}
	ms := alpha.members()
	if ms["gamma"].Status != "removed" || ms["beta"].Status != "removed" || ms["alpha"].Status != "active" {
		t.Fatalf("membership after leave: %+v", ms)
	}
	head := alpha.document(alpha.generation())
	if len(head.RevokedCertSerials) < 2 {
		t.Fatalf("revoked serials: %v", head.RevokedCertSerials)
	}
	if code := peerCode(gamma.svc.Probe(ctx, alpha.id)); code != "certificate_revoked" {
		t.Fatalf("departed member at authority: %q", code)
	}
	if code := alpha.call("POST", "/api/v1/federation/leave", map[string]any{"confirm": true}, nil); code != 409 {
		t.Fatalf("authority leave: %d", code)
	}

	// --- a removed member can rejoin with a new invitation ---
	invB2 := alpha.invite("beta")
	if code := beta.join(alpha, invB2); code != 200 {
		t.Fatalf("rejoin beta: %d", code)
	}
	if m := alpha.members()["beta"]; m.Status != "active" {
		t.Fatalf("rejoined beta: %+v", m)
	}
	if err := beta.svc.Probe(ctx, alpha.id); err != nil {
		t.Fatalf("rejoined member probe: %v", err)
	}

	// --- the federation listener is not reachable without a client certificate ---
	_, err = (peer.Client{}).DoPinned(ctx, alpha.fedURL, founded.CASHA256, "GET", "/federation/v1/heads", nil)
	if peerCode(err) != "client_certificate_required" {
		t.Fatalf("anonymous heads: %v", err)
	}
	// Invitations: list (no secrets) and revoke.
	pending := alpha.invite("delta")
	if code := alpha.call("DELETE", "/api/v1/federation/invitations/"+pending.ID, nil, nil); code != 204 {
		t.Fatalf("revoke invitation: %d", code)
	}
	var invs struct {
		Items []federation.Invitation `json:"items"`
	}
	alpha.call("GET", "/api/v1/federation/invitations", nil, &invs)
	states := map[string]int{}
	for _, i := range invs.Items {
		states[i.State]++
	}
	if states["revoked"] != 1 || states["consumed"] != 3 {
		t.Fatalf("invitation states: %v", states)
	}
	// Non-authority members cannot administer membership.
	if code := beta.call("POST", "/api/v1/federation/invitations", map[string]any{"name": "nope"}, nil); code != 409 {
		t.Fatalf("invitation on a member: %d", code)
	}
}

func rejectReason(err error) string {
	var rej *federation.RejectedError
	if errors.As(err, &rej) {
		return rej.Reason
	}
	if err == nil {
		return "accepted"
	}
	return "error: " + err.Error()
}

// cloneJoinRequest builds a join request for an instance ID that is already a
// member, as a restored backup would send.
func cloneJoinRequest(t *testing.T, invitation, instanceID string) federation.JoinRequest {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: instanceID}}, key)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return federation.JoinRequest{Invitation: invitation, InstanceID: instanceID, FederationURL: "https://127.0.0.1:1", APIURL: "http://clone.example.test",
		CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), SigningKey: "ed25519:" + base64.RawURLEncoding.EncodeToString(pub),
		SoftwareVersion: "harness", Protocol: federation.ProtocolRange{Min: 1, Max: 1}}
}
