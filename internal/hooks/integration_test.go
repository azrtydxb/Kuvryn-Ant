package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/testdb"
)

type storeResolver struct{ store secrets.Store }

func (r storeResolver) Resolve(ctx context.Context, instance, record string) ([]byte, error) {
	_, plain, err := r.store.Open(ctx, instance, record, "webhook")
	return plain, err
}

type fakeBackend struct {
	mu          sync.Mutex
	replication []string
	cleaned     []string
	cleanErr    error
	invalidated map[string][]string
	prewarms    int
}

func (b *fakeBackend) backend() Backend {
	return Backend{
		TriggerReplication: func(_ context.Context, _, actor, job string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.replication = append(b.replication, actor+"/"+job)
			return nil
		},
		CleanCache: func(_ context.Context, p identity.Principal, feed, prefix string) (CacheCleaned, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.cleaned = append(b.cleaned, p.PrincipalID+"|"+feed+"|"+prefix)
			return CacheCleaned{Matched: 3, Bytes: 99}, b.cleanErr
		},
		InvalidateMetadata: func(source string, names []string) int {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.invalidated == nil {
				b.invalidated = map[string][]string{}
			}
			b.invalidated[source] = names
			return 2
		},
		NotifyPrewarm: func() { b.mu.Lock(); b.prewarms++; b.mu.Unlock() },
	}
}

type env struct {
	t         *testing.T
	f         testdb.Fixture
	svc       *Service
	mux       *http.ServeMux
	be        *fakeBackend
	principal string
	secretID  string
	secretKey []byte
	feedSeq   int
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	f := testdb.New(t)
	ctx := context.Background()
	e := &env{t: t, f: f, be: &fakeBackend{}, secretKey: []byte(strings.Repeat("s", 40))}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.Pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service','hook-admin') RETURNING id::text`, f.InstanceID).Scan(&e.principal))
	var role string
	must(f.Pool.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,'hook-admin') RETURNING id::text`, f.InstanceID).Scan(&role))
	_, err := f.Pool.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)`, f.InstanceID, e.principal, role)
	must(err)
	for _, kind := range []string{"artifact", "feed", "source", "instance"} {
		_, err = f.Pool.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,$3,'allow',$4)`, f.InstanceID, role, kind, []string{"read", "publish", "delete", "configure", "replicate", "admin"})
		must(err)
	}
	store := secrets.Store{DB: f.Pool, Keys: f.Keys}
	tx, err := f.Pool.Begin(ctx)
	must(err)
	rec, err := store.CreateTx(ctx, tx, f.InstanceID, e.principal, "webhook", append([]byte(nil), e.secretKey...))
	must(err)
	must(tx.QueryRow(ctx, `INSERT INTO webhook_signing_secrets(instance_id,name,secret_id,created_by) VALUES($1,'ci',$2,$3) RETURNING id::text`, f.InstanceID, rec.ID, e.principal).Scan(&e.secretID))
	must(tx.Commit(ctx))
	cfg := Config{DB: f.Pool, InstanceID: f.InstanceID, Secrets: storeResolver{store}, Backend: e.be.backend(), PublicURL: "https://ant.example.com/", AllowHTTP: true, AllowPrivate: true}
	for _, m := range mutate {
		m(&cfg)
	}
	e.svc, err = New(cfg)
	must(err)
	t.Cleanup(e.svc.Close)
	e.mux = http.NewServeMux()
	e.svc.Register(e.mux)
	return e
}

// hook stores a hook (bypassing Validate, so tests may target loopback).
func (e *env) hook(h Hook, token string) Hook {
	e.t.Helper()
	ctx := context.Background()
	if h.Slug == "" {
		h.Slug = "h" + strings.ToLower(rand8())
	}
	h.Enabled = true
	h.CreatedBy = e.principal
	if h.RatePerMinute == 0 {
		h.RatePerMinute = 600
	}
	if h.AuthMode == "" {
		h.AuthMode, h.HMACFormat, h.SigningSecretID = AuthHMAC, FormatGitHub, e.secretID
	}
	if h.AuthMode == AuthHMAC && h.SigningSecretID == "" {
		h.SigningSecretID = e.secretID
	}
	tx, err := e.f.Pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := InsertHook(ctx, tx, e.f.InstanceID, h, token)
	if err != nil {
		e.t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	e.svc.Invalidate()
	got, err := GetHook(ctx, e.f.Pool, e.f.InstanceID, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

var seq atomic.Int64

func rand8() string { return fmt.Sprintf("%08x", seq.Add(1)) }

func (e *env) post(path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://ant.example.com"+path, bytes.NewReader(body))
	r.RemoteAddr = "192.0.2.10:4000"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

func (e *env) signed(h Hook, body []byte, extra map[string]string) map[string]string {
	name, value := SignBody(h.HMACFormat, body, e.secretKey)
	m := map[string]string{name: value, "Content-Type": "application/json"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// drain runs the worker until no delivery is left that can run now.
func (e *env) drain() int {
	e.t.Helper()
	n := 0
	for {
		did, err := e.svc.processOne(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if !did {
			return n
		}
		n++
	}
}

type row struct {
	state, auth, result, errText string
	attempts                     int
	detail                       map[string]any
}

func (e *env) last(hook Hook) row {
	e.t.Helper()
	var r row
	var detail []byte
	var errText *string
	var result *string
	err := e.f.Pool.QueryRow(context.Background(), `SELECT state,auth_result,action_result,error,attempts,detail FROM incoming_hook_deliveries WHERE hook_id=$1 ORDER BY received_at DESC,id DESC LIMIT 1`, hook.ID).Scan(&r.state, &r.auth, &result, &errText, &r.attempts, &detail)
	if err != nil {
		e.t.Fatal(err)
	}
	if result != nil {
		r.result = *result
	}
	if errText != nil {
		r.errText = *errText
	}
	_ = json.Unmarshal(detail, &r.detail)
	return r
}

func (e *env) feed(format, purpose string) string {
	e.t.Helper()
	e.feedSeq++
	var id string
	if err := e.f.Pool.QueryRow(context.Background(), `INSERT INTO feeds(instance_id,slug,format,purpose) VALUES($1,$2,$3,$4) RETURNING id::text`, e.f.InstanceID, format+"-"+rand8(), format, purpose).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) source(name, format, kind, origin string) string {
	e.t.Helper()
	ctx := context.Background()
	var id string
	if err := e.f.Pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,$3,$4) RETURNING id::text`, e.f.InstanceID, name, kind, format).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	if kind == "remote" {
		cfg, _ := json.Marshal(map[string]any{"origin": origin})
		if _, err := e.f.Pool.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,config) VALUES($1,$2,$3)`, e.f.InstanceID, id, cfg); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func (e *env) count(q string, args ...any) int { return e.f.Count(e.t, q, args...) }

func TestAcceptedCallIsQueuedAndAnswered202(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{"zen":"x","token":"super-secret-value"}`)
	w := e.post("/hook/"+h.Slug, body, e.signed(h, body, map[string]string{"X-GitHub-Event": "ping"}))
	if w.Code != 202 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var resp struct{ ID, Status string }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID == "" || resp.Status != "accepted" {
		t.Fatalf("response %s", w.Body)
	}
	var preview, ip, state string
	var payload []byte
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT preview,source_ip,state,payload FROM incoming_hook_deliveries WHERE id=$1`, resp.ID).Scan(&preview, &ip, &state, &payload); err != nil {
		t.Fatal(err)
	}
	if state != StatePending || ip != "192.0.2.10" || !bytes.Equal(payload, body) {
		t.Fatalf("stored row %s %s", state, ip)
	}
	if strings.Contains(preview, "super-secret-value") || !strings.Contains(preview, redacted) {
		t.Fatalf("preview not redacted: %s", preview)
	}
	if e.drain() != 1 {
		t.Fatal("worker did not process the delivery")
	}
	if r := e.last(h); r.state != StateSucceeded || r.result != "replication_requested" || r.auth != AuthOK {
		t.Fatalf("%+v", r)
	}
	if len(e.be.replication) != 1 || e.be.replication[0] != e.principal+"/11111111-1111-1111-1111-111111111111" {
		t.Fatalf("replication calls %v", e.be.replication)
	}
}

func TestHMACModesValidInvalidMissing(t *testing.T) {
	e := newEnv(t)
	for format := range HMACFormats {
		h := e.hook(Hook{AuthMode: AuthHMAC, HMACFormat: format, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
		path := "/hook/" + h.Slug
		body := []byte(`{"n":1}`)
		if w := e.post(path, body, e.signed(h, body, nil)); w.Code != 202 {
			t.Fatalf("%s valid: %d %s", format, w.Code, w.Body)
		}
		bad := e.signed(h, []byte(`{"n":2}`), nil) // signature of another body
		if format == FormatGitLab {
			bad[HMACFormats[format]] = strings.Repeat("x", 40)
		}
		if w := e.post(path, body, bad); w.Code != 404 {
			t.Fatalf("%s invalid: %d", format, w.Code)
		}
		if w := e.post(path, body, map[string]string{"Content-Type": "application/json"}); w.Code != 404 {
			t.Fatalf("%s missing: %d", format, w.Code)
		}
		if w := e.post(path+"/sometoken", body, e.signed(h, body, nil)); w.Code != 404 {
			t.Fatalf("%s with a path token: %d", format, w.Code)
		}
		want := map[string]int{AuthOK: 1, AuthInvalid: 1, AuthMissing: 1}
		rows, err := e.f.Pool.Query(context.Background(), `SELECT auth_result,count(*) FROM incoming_hook_deliveries WHERE hook_id=$1 GROUP BY 1`, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			_ = rows.Scan(&k, &n)
			got[k] = n
		}
		rows.Close()
		if got[AuthOK] != 1 || got[AuthMissing] != 1 || got[AuthInvalid] != 2 {
			t.Fatalf("%s log %v want ok=1 missing=1 invalid=2 (%v)", format, got, want)
		}
	}
}

func TestRotationGraceAcceptsPreviousSecret(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	ctx := context.Background()
	store := secrets.Store{DB: e.f.Pool, Keys: e.f.Keys}
	newKey := []byte(strings.Repeat("N", 40))
	tx, _ := e.f.Pool.Begin(ctx)
	rec, err := store.CreateTx(ctx, tx, e.f.InstanceID, e.principal, "webhook", append([]byte(nil), newKey...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE webhook_signing_secrets SET previous_secret_id=secret_id,previous_purpose='webhook',previous_expires_at=now()+interval '1 hour',secret_id=$2 WHERE id=$1`, e.secretID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{}`)
	if w := e.post("/hook/"+h.Slug, body, e.signed(h, body, nil)); w.Code != 202 { // old key
		t.Fatalf("previous secret refused: %d", w.Code)
	}
	name, value := SignBody(h.HMACFormat, body, newKey)
	if w := e.post("/hook/"+h.Slug, body, map[string]string{name: value}); w.Code != 202 {
		t.Fatalf("current secret refused: %d", w.Code)
	}
}

func TestURLTokenMode(t *testing.T) {
	e := newEnv(t)
	token := "tok_" + strings.Repeat("a1b2", 10)
	h := e.hook(Hook{AuthMode: AuthURLToken, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, token)
	if h.TokenHint != token[len(token)-4:] || bytes.Contains(h.TokenHash, []byte(token)) {
		t.Fatal("token hint or hash")
	}
	var plain int
	_ = e.f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM incoming_hooks WHERE position($1 in action::text)>0 OR position($1 in description)>0`, token).Scan(&plain)
	if plain != 0 {
		t.Fatal("token stored in plaintext")
	}
	body := []byte(`{"push_data":{"tag":"x"}}`)
	if w := e.post("/hook/"+h.Slug+"/"+token, body, nil); w.Code != 202 {
		t.Fatalf("valid token: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/hook/" + h.Slug + "/" + token + "x", "/hook/" + h.Slug + "/tok", "/hook/" + h.Slug} {
		if w := e.post(p, body, nil); w.Code != 404 {
			t.Fatalf("%s -> %d", p, w.Code)
		}
	}
	// The failure body is identical to an unknown slug's.
	a := e.post("/hook/"+h.Slug+"/wrong", body, nil)
	b := e.post("/hook/nosuchhook/wrong", body, nil)
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("bad token and unknown slug differ: %q vs %q", a.Body, b.Body)
	}
}

func TestBearerMode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	issue := func(actions ...identity.Action) string {
		tok, err := (identity.Tokens{DB: e.f.Pool}).Issue(ctx, e.f.InstanceID, e.principal, e.principal, []identity.Scope{{Actions: actions}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return tok.Secret
	}
	h := e.hook(Hook{AuthMode: AuthBearer, BearerScope: ScopeConfigure, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{}`)
	path := "/hook/" + h.Slug
	if w := e.post(path, body, map[string]string{"Authorization": "Bearer " + issue(identity.Configure)}); w.Code != 202 {
		t.Fatalf("token with the scope: %d %s", w.Code, w.Body)
	}
	for name, hdr := range map[string]map[string]string{
		"wrong scope": {"Authorization": "Bearer " + issue(identity.Read)},
		"garbage":     {"Authorization": "Bearer ant_" + strings.Repeat("x", 43)},
		"not bearer":  {"Authorization": "Basic abc"},
		"missing":     {},
	} {
		if w := e.post(path, body, hdr); w.Code != 404 {
			t.Fatalf("%s -> %d", name, w.Code)
		}
	}
	admin := e.hook(Hook{AuthMode: AuthBearer, BearerScope: ScopeAdmin, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	if w := e.post("/hook/"+admin.Slug, body, map[string]string{"Authorization": "Bearer " + issue(identity.Configure)}); w.Code != 404 {
		t.Fatalf("configure token on an admin hook: %d", w.Code)
	}
	if w := e.post("/hook/"+admin.Slug, body, map[string]string{"Authorization": "Bearer " + issue(identity.Admin)}); w.Code != 202 {
		t.Fatalf("admin token on an admin hook: %d", w.Code)
	}
}

func TestUnknownDisabledAndWrongMethod(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{}`)
	unknown := e.post("/hook/does-not-exist", body, nil)
	badAuth := e.post("/hook/"+h.Slug, body, nil)
	if unknown.Code != 404 || badAuth.Code != 404 || unknown.Body.String() != badAuth.Body.String() {
		t.Fatalf("unknown %d %q vs bad auth %d %q", unknown.Code, unknown.Body, badAuth.Code, badAuth.Body)
	}
	if _, err := e.f.Pool.Exec(context.Background(), `UPDATE incoming_hooks SET enabled=false WHERE id=$1`, h.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.Invalidate()
	if w := e.post("/hook/"+h.Slug, body, e.signed(h, body, nil)); w.Code != 404 || w.Body.String() != unknown.Body.String() {
		t.Fatalf("disabled hook: %d %q", w.Code, w.Body)
	}
	for _, p := range []string{"/hook/Bad_Slug", "/hook/" + strings.Repeat("a", 64)} {
		if w := e.post(p, body, nil); w.Code != 404 {
			t.Fatalf("%s -> %d", p, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "https://ant.example.com/hook/"+h.Slug, nil)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("GET -> %d", w.Code)
	}
}

func TestBodySizeCapAppliesBeforeAuthentication(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxBody = 1024 })
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	big := bytes.Repeat([]byte("a"), 2048)
	// Declared length: uniform 413, no matter whether the slug exists or the signature is valid.
	for _, slug := range []string{h.Slug, "nosuchhook"} {
		if w := e.post("/hook/"+slug, big, nil); w.Code != 413 {
			t.Fatalf("%s declared: %d", slug, w.Code)
		}
	}
	// Undeclared length (chunked) is cut off while reading.
	r := httptest.NewRequest(http.MethodPost, "https://ant.example.com/hook/"+h.Slug, io.NopCloser(bytes.NewReader(big)))
	r.ContentLength = -1
	r.RemoteAddr = "192.0.2.10:1"
	for k, v := range e.signed(h, big, nil) {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("chunked: %d", w.Code)
	}
	// Exactly at the cap is accepted.
	atCap := bytes.Repeat([]byte("b"), 1024)
	if w := e.post("/hook/"+h.Slug, atCap, e.signed(h, atCap, nil)); w.Code != 202 {
		t.Fatalf("at cap: %d", w.Code)
	}
	if e.count(`SELECT count(*) FROM incoming_hook_deliveries WHERE hook_id=$1 AND payload IS NOT NULL AND payload_size>1024`, h.ID) != 0 {
		t.Fatal("oversized payload stored")
	}
}

func TestReplayProtectionRemembersDeliveryIDsFor24Hours(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{"a":1}`)
	send := func(hdr, id string) int {
		return e.post("/hook/"+h.Slug, body, e.signed(h, body, map[string]string{hdr: id})).Code
	}
	if c := send("X-GitHub-Delivery", "guid-1"); c != 202 {
		t.Fatalf("first: %d", c)
	}
	if c := send("X-GitHub-Delivery", "guid-1"); c != 409 {
		t.Fatalf("replay: %d", c)
	}
	if c := send("X-GitHub-Delivery", "guid-2"); c != 202 {
		t.Fatalf("new id: %d", c)
	}
	if c := send("X-Gitlab-Event-UUID", "guid-1"); c != 202 { // another header is another namespace
		t.Fatalf("gitlab uuid: %d", c)
	}
	if c := send("X-Gitlab-Event-UUID", "guid-1"); c != 409 {
		t.Fatalf("gitlab replay: %d", c)
	}
	// A bad signature never reserves an id, so an attacker cannot burn a real delivery's id.
	other := e.post("/hook/"+h.Slug, body, map[string]string{"X-Hub-Signature-256": "sha256=00", "X-GitHub-Delivery": "guid-9"})
	if other.Code != 404 {
		t.Fatal("bad signature accepted")
	}
	if c := send("X-GitHub-Delivery", "guid-9"); c != 202 {
		t.Fatalf("id burned by an unauthenticated call: %d", c)
	}
	if e.count(`SELECT count(*) FROM incoming_hook_deliveries WHERE hook_id=$1 AND auth_result='duplicate'`, h.ID) != 2 {
		t.Fatal("replays not logged")
	}
	// Remembered ids expire after 24 hours.
	if _, err := e.f.Pool.Exec(context.Background(), `UPDATE incoming_hook_replay SET seen_at=now()-interval '25 hours' WHERE delivery_ref='X-GitHub-Delivery:guid-1'`); err != nil {
		t.Fatal(err)
	}
	e.svc.Purge(context.Background())
	if c := send("X-GitHub-Delivery", "guid-1"); c != 202 {
		t.Fatalf("id not forgotten after 24h: %d", c)
	}
}

func TestPerHookRateLimit(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{RatePerMinute: 1, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{}`)
	codes := map[int]int{}
	for range 8 {
		codes[e.post("/hook/"+h.Slug, body, e.signed(h, body, nil)).Code]++
	}
	if codes[202] != 5 || codes[429] != 3 {
		t.Fatalf("codes %v (burst of 5 then limited)", codes)
	}
	// Another hook has its own bucket; unauthenticated calls never consume a hook's tokens.
	h2 := e.hook(Hook{RatePerMinute: 1, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	for range 20 {
		e.post("/hook/"+h2.Slug, body, nil)
	}
	if w := e.post("/hook/"+h2.Slug, body, e.signed(h2, body, nil)); w.Code != 202 {
		t.Fatalf("second hook limited by failed calls: %d", w.Code)
	}
}

func TestRejectedCallsAreLoggedWithoutPayload(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	e.post("/hook/"+h.Slug, []byte(`{"password":"hunter2"}`), map[string]string{"X-Hub-Signature-256": "sha256=ab"})
	var state, preview string
	var payload []byte
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT state,preview,payload FROM incoming_hook_deliveries WHERE hook_id=$1`, h.ID).Scan(&state, &preview, &payload); err != nil {
		t.Fatal(err)
	}
	if state != StateRejected || payload != nil || strings.Contains(preview, "hunter2") {
		t.Fatalf("rejected row: %s %q %v", state, preview, payload)
	}
}

func TestMetricsByHookAndResult(t *testing.T) {
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{}`)
	e.post("/hook/"+h.Slug, body, e.signed(h, body, nil))
	e.post("/hook/"+h.Slug, body, nil)
	e.post("/hook/unknownhook", body, nil)
	e.drain()
	var out bytes.Buffer
	e.svc.WriteMetrics(&out)
	text := out.String()
	for _, want := range []string{
		`ant_incoming_hooks_total{hook="` + h.Slug + `",result="accepted"} 1`,
		`ant_incoming_hooks_total{hook="` + h.Slug + `",result="unauthorized"} 1`,
		`ant_incoming_hooks_total{hook="` + h.Slug + `",result="succeeded"} 1`,
		`ant_incoming_hooks_total{hook="_unknown",result="unauthorized"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in\n%s", want, text)
		}
	}
	if strings.Contains(text, "unknownhook") {
		t.Fatal("unknown slug became a label")
	}
}

func TestRetentionPurgesOldDeliveries(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LogRetentionDays = 7 })
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	body := []byte(`{}`)
	for range 2 {
		e.post("/hook/"+h.Slug, body, e.signed(h, body, nil))
	}
	e.drain()
	if _, err := e.f.Pool.Exec(context.Background(), `UPDATE incoming_hook_deliveries SET received_at=now()-interval '8 days' WHERE id=(SELECT id FROM incoming_hook_deliveries WHERE hook_id=$1 ORDER BY id LIMIT 1)`, h.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.Purge(context.Background())
	if n := e.count(`SELECT count(*) FROM incoming_hook_deliveries WHERE hook_id=$1`, h.ID); n != 1 {
		t.Fatalf("%d deliveries left", n)
	}
}

func TestSendTestAndRedeliver(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h := e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	id, err := e.svc.SendTest(ctx, e.f.Pool, h)
	if err != nil || id == "" {
		t.Fatal(err)
	}
	e.drain()
	if r := e.last(h); r.auth != AuthTest || r.state != StateSucceeded {
		t.Fatalf("%+v", r)
	}
	if _, err = e.svc.Redeliver(ctx, e.f.Pool, h, id); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if r := e.last(h); r.auth != AuthRedelivery || r.state != StateSucceeded {
		t.Fatalf("%+v", r)
	}
	if len(e.be.replication) != 2 {
		t.Fatalf("expected two runs, got %d", len(e.be.replication))
	}
	// A rejected row has no payload and cannot be redelivered.
	e.post("/hook/"+h.Slug, []byte(`{}`), nil)
	var rejected string
	_ = e.f.Pool.QueryRow(ctx, `SELECT id::text FROM incoming_hook_deliveries WHERE hook_id=$1 AND state='rejected'`, h.ID).Scan(&rejected)
	if _, err = e.svc.Redeliver(ctx, e.f.Pool, h, rejected); err == nil {
		t.Fatal("rejected delivery redelivered")
	}
	// Tests of a token-less mode work too.
	tok := e.hook(Hook{AuthMode: AuthURLToken, Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "tkn_"+strings.Repeat("z", 30))
	if _, err = e.svc.SendTest(ctx, e.f.Pool, tok); err != nil {
		t.Fatal(err)
	}
}

func TestPrewarmActionUsesTemplatedValues(t *testing.T) {
	e := newEnv(t)
	feed := e.feed("npm", "artifacts")
	h := e.hook(Hook{Action: Action{Type: ActionPrewarm, FeedID: feed, Kind: "package", Name: "{{ .package.name | lower }}", Version: "{{ .release.tag_name | trimv }}"}}, "")
	body := []byte(`{"package":{"name":"Left-Pad"},"release":{"tag_name":"v1.3.0"}}`)
	if w := e.post("/hook/"+h.Slug, body, e.signed(h, body, nil)); w.Code != 202 {
		t.Fatal(w.Code)
	}
	e.drain()
	r := e.last(h)
	if r.state != StateSucceeded || r.result != "prewarm_started" || r.detail["name"] != "left-pad" || r.detail["version"] != "1.3.0" {
		t.Fatalf("%+v", r)
	}
	var name, version, jobName, state string
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT i.name,i.version,j.name,r.state FROM prewarm_run_items i JOIN prewarm_runs r ON r.id=i.run_id JOIN prewarm_jobs j ON j.id=r.job_id WHERE j.feed_id=$1`, feed).Scan(&name, &version, &jobName, &state); err != nil {
		t.Fatal(err)
	}
	if name != "left-pad" || version != "1.3.0" || state != "queued" || !strings.HasPrefix(jobName, "hook:"+h.Slug+":") {
		t.Fatalf("prewarm item %q %q %q %q", name, version, jobName, state)
	}
	if e.be.prewarms != 1 {
		t.Fatal("prewarm scheduler not woken")
	}
	if e.count(`SELECT count(*) FROM audit_events WHERE action='incoming_hook.prewarm'`) != 1 {
		t.Fatal("prewarm not audited")
	}
	// A payload the template cannot render fails permanently with a clear error.
	bad := []byte(`{"package":{"name":"x"}}`)
	e.post("/hook/"+h.Slug, bad, e.signed(h, bad, nil))
	e.drain()
	if r := e.last(h); r.state != StateFailed || r.result != "template_error" || r.attempts != 1 {
		t.Fatalf("%+v", r)
	}
	// A rendered value that is not a valid item fails permanently too.
	weird := []byte(`{"package":{"name":"../../etc"},"release":{"tag_name":"1"}}`)
	e.post("/hook/"+h.Slug, weird, e.signed(h, weird, nil))
	e.drain()
	if r := e.last(h); r.state != StateFailed || r.result != "invalid_item" {
		t.Fatalf("%+v", r)
	}
}

func TestPrewarmImageAction(t *testing.T) {
	e := newEnv(t)
	feed := e.feed("oci", "artifacts")
	h := e.hook(Hook{AuthMode: AuthURLToken, Action: Action{Type: ActionPrewarm, FeedID: feed, Kind: "image", Name: "{{ .repository.repo_name }}", Version: "{{ .push_data.tag }}"}}, "tkn_"+strings.Repeat("q", 30))
	body := []byte(`{"push_data":{"tag":"2.1"},"repository":{"repo_name":"acme/api"}}`)
	if w := e.post("/hook/"+h.Slug+"/tkn_"+strings.Repeat("q", 30), body, nil); w.Code != 202 {
		t.Fatal(w.Code)
	}
	e.drain()
	if r := e.last(h); r.state != StateSucceeded {
		t.Fatalf("%+v", r)
	}
	var name, version string
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT i.name,i.version FROM prewarm_run_items i JOIN prewarm_runs r ON r.id=i.run_id WHERE r.feed_id=$1`, feed).Scan(&name, &version); err != nil || name != "acme/api" || version != "2.1" {
		t.Fatalf("%q %q %v", name, version, err)
	}
}

func TestPurgeSourceCacheAction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := e.source("npmjs", "npm", "remote", "https://registry.npmjs.org")
	other := e.source("pypi", "pypi", "remote", "https://pypi.org")
	add := func(source, coordinate string) {
		var comp string
		if err := e.f.Pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,$3) RETURNING id::text`, e.f.InstanceID, source, coordinate).Scan(&comp); err != nil {
			t.Fatal(err)
		}
		if _, err := e.f.Pool.Exec(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1.0.0')`, e.f.InstanceID, comp); err != nil {
			t.Fatal(err)
		}
	}
	add(src, "left-pad")
	add(src, "right-pad")
	add(other, "left-pad")
	byCoordinate := e.hook(Hook{Action: Action{Type: ActionPurgeSource, SourceID: src, Coordinate: "{{ .name }}"}}, "")
	body := []byte(`{"name":"left-pad"}`)
	e.post("/hook/"+byCoordinate.Slug, body, e.signed(byCoordinate, body, nil))
	e.drain()
	r := e.last(byCoordinate)
	if r.state != StateSucceeded || r.result != "cache_purged" || r.detail["components"] != float64(1) {
		t.Fatalf("%+v", r)
	}
	if e.count(`SELECT count(*) FROM components WHERE source_id=$1`, src) != 1 || e.count(`SELECT count(*) FROM components WHERE source_id=$1`, other) != 1 {
		t.Fatal("purge removed the wrong components")
	}
	if e.count(`SELECT count(*) FROM audit_events WHERE action='source.cache.purge' AND resource_id=$1`, src) != 1 {
		t.Fatal("purge not audited")
	}
	all := e.hook(Hook{Action: Action{Type: ActionPurgeSource, SourceID: src}}, "")
	e.post("/hook/"+all.Slug, body, e.signed(all, body, nil))
	e.drain()
	if e.count(`SELECT count(*) FROM components WHERE source_id=$1`, src) != 0 || e.count(`SELECT count(*) FROM components WHERE source_id=$1`, other) != 1 {
		t.Fatal("purging all removed the wrong components")
	}
	// A local source is refused.
	local := e.source("local", "npm", "local", "")
	bad := e.hook(Hook{Action: Action{Type: ActionPurgeSource, SourceID: local}}, "")
	e.post("/hook/"+bad.Slug, body, e.signed(bad, body, nil))
	e.drain()
	if r := e.last(bad); r.state != StateFailed || r.result != "not_remote" {
		t.Fatalf("%+v", r)
	}
}

func TestCleanCacheAction(t *testing.T) {
	e := newEnv(t)
	feed := e.feed("oci", "cache")
	h := e.hook(Hook{Action: Action{Type: ActionCleanCache, FeedID: feed, Prefix: "{{ .repository }}:pr-{{ .number }}"}}, "")
	body := []byte(`{"repository":"acme/app","number":42}`)
	e.post("/hook/"+h.Slug, body, e.signed(h, body, nil))
	e.drain()
	r := e.last(h)
	if r.state != StateSucceeded || r.result != "cache_cleaned" || r.detail["matched"] != float64(3) {
		t.Fatalf("%+v", r)
	}
	if len(e.be.cleaned) != 1 || e.be.cleaned[0] != e.principal+"|"+feed+"|acme/app:pr-42" {
		t.Fatalf("clean calls %v", e.be.cleaned)
	}
	e.be.cleanErr = ErrTargetDenied
	e.post("/hook/"+h.Slug, body, e.signed(h, body, nil))
	e.drain()
	if r := e.last(h); r.state != StateFailed || r.result != "denied" || r.attempts != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestUpstreamNotificationInvalidatesMatchingSourcesOnly(t *testing.T) {
	e := newEnv(t)
	hub := e.source("hub", "oci", "remote", "https://registry-1.docker.io")
	ghcr := e.source("ghcr", "oci", "remote", "https://ghcr.io")
	npmjs := e.source("npmjs", "npm", "remote", "https://registry.npmjs.org")
	feed := e.feed("oci", "artifacts")
	if _, err := e.f.Pool.Exec(context.Background(), `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,1)`, e.f.InstanceID, feed, hub); err != nil {
		t.Fatal(err)
	}
	h := e.hook(Hook{AuthMode: AuthURLToken, Action: Action{Type: ActionUpstream, Provider: ProviderAuto, Prewarm: true}}, "tkn_"+strings.Repeat("u", 30))
	path := "/hook/" + h.Slug + "/tkn_" + strings.Repeat("u", 30)
	body := []byte(`{"push_data":{"tag":"1.4.0"},"repository":{"repo_name":"acme/api"}}`)
	if w := e.post(path, body, nil); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	e.drain()
	r := e.last(h)
	if r.state != StateSucceeded || r.result != "cache_invalidated" || r.detail["sources_matched"] != float64(1) || r.detail["entries_invalidated"] != float64(2) {
		t.Fatalf("%+v", r)
	}
	if len(e.be.invalidated) != 1 || !reflectEqual(e.be.invalidated[hub], []string{"acme/api"}) {
		t.Fatalf("invalidated %v (ghcr=%s npmjs=%s must not appear)", e.be.invalidated, ghcr, npmjs)
	}
	if r.detail["prewarm_runs"] != float64(1) {
		t.Fatalf("prewarm %+v", r.detail)
	}
	var name, version string
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT i.name,i.version FROM prewarm_run_items i JOIN prewarm_runs r ON r.id=i.run_id WHERE r.feed_id=$1`, feed).Scan(&name, &version); err != nil || name != "acme/api" || version != "1.4.0" {
		t.Fatalf("prewarm item %q %q %v", name, version, err)
	}
	// A payload no configured source maps to is a quiet success, not an error.
	e.be.invalidated = nil
	npm := []byte(`{"event":"package:publish","name":"x","version":"1.0.0"}`)
	e.post(path, npm, nil)
	e.drain()
	r = e.last(h)
	if r.state != StateSucceeded || r.result != "cache_invalidated" || len(e.be.invalidated) != 1 {
		t.Fatalf("npm: %+v %v", r, e.be.invalidated)
	}
	ping := []byte(`{"zen":"hi"}`)
	e.post(path, ping, map[string]string{"X-GitHub-Event": "ping"})
	e.drain()
	if r = e.last(h); r.result != "ignored" || r.state != StateSucceeded {
		t.Fatalf("ping: %+v", r)
	}
	foreign := []byte(`{"unrelated":true}`)
	e.post(path, foreign, nil)
	e.drain()
	if r = e.last(h); r.state != StateFailed || r.result != "unrecognized_payload" {
		t.Fatalf("foreign: %+v", r)
	}
}

func reflectEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRelayRetriesFiltersHeadersAndResigns(t *testing.T) {
	var mu sync.Mutex
	var calls []*http.Request
	var bodies [][]byte
	var failures atomic.Int32
	failures.Store(2)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r)
		bodies = append(bodies, b)
		mu.Unlock()
		if failures.Add(-1) >= 0 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer target.Close()
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionRelay, TargetURL: target.URL + "/internal/hook", ResignSecretID: e.secretID}}, "")
	body := []byte(`{"ref":"refs/tags/v1","password":"keep-me-out-of-logs"}`)
	hdr := e.signed(h, body, map[string]string{"X-GitHub-Event": "push", "X-GitHub-Delivery": "d-1", "Cookie": "session=1", "X-Gitlab-Token": "tok", "Authorization": "Bearer nope"})
	if w := e.post("/hook/"+h.Slug, body, hdr); w.Code != 202 {
		t.Fatalf("%d", w.Code)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if e.drain() != 1 {
			t.Fatalf("attempt %d: nothing to run", attempt)
		}
		r := e.last(h)
		if attempt < 3 {
			if r.state != StatePending || r.attempts != attempt || r.result != "http_failure" {
				t.Fatalf("attempt %d: %+v", attempt, r)
			}
			// Skip the backoff wait.
			if _, err := e.f.Pool.Exec(context.Background(), `UPDATE incoming_hook_deliveries SET available_at=now() WHERE hook_id=$1`, h.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := e.last(h)
	if r.state != StateSucceeded || r.result != "relayed" || r.attempts != 3 {
		t.Fatalf("final: %+v", r)
	}
	last := calls[len(calls)-1]
	if string(bodies[len(bodies)-1]) != string(body) {
		t.Fatal("body altered")
	}
	for _, forbidden := range []string{"Authorization", "Cookie", "X-Gitlab-Token", "X-Hub-Signature-256"} {
		if last.Header.Get(forbidden) != "" {
			t.Fatalf("%s forwarded", forbidden)
		}
	}
	if last.Header.Get("X-GitHub-Event") != "push" || last.Header.Get("X-Ant-Hook") != h.Slug || last.Header.Get("X-Ant-Delivery") == "" {
		t.Fatalf("headers %v", last.Header)
	}
	ts, id := last.Header.Get("X-Ant-Timestamp"), last.Header.Get("X-Ant-Event-ID")
	if want := events.SignatureHeader(ts, id, body, e.secretKey); last.Header.Get("X-Ant-Signature") != want {
		t.Fatalf("re-signature %q want %q", last.Header.Get("X-Ant-Signature"), want)
	}
	// Pass-through (no re-signing) forwards the original body signature instead.
	failures.Store(0)
	p := e.hook(Hook{Action: Action{Type: ActionRelay, TargetURL: target.URL + "/pass"}}, "")
	e.post("/hook/"+p.Slug, body, e.signed(p, body, nil))
	e.drain()
	last = calls[len(calls)-1]
	if last.Header.Get("X-Hub-Signature-256") == "" || last.Header.Get("X-Ant-Signature") != "" {
		t.Fatalf("pass-through headers %v", last.Header)
	}
}

func TestRelayGivesUpAfterMaxAttemptsAndRefusesPrivateTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer target.Close()
	e := newEnv(t)
	h := e.hook(Hook{Action: Action{Type: ActionRelay, TargetURL: target.URL}}, "")
	body := []byte(`{}`)
	e.post("/hook/"+h.Slug, body, e.signed(h, body, nil))
	for range MaxAttempts {
		e.drain()
		_, _ = e.f.Pool.Exec(context.Background(), `UPDATE incoming_hook_deliveries SET available_at=now() WHERE hook_id=$1 AND state='pending'`, h.ID)
	}
	if r := e.last(h); r.state != StateFailed || r.attempts != MaxAttempts {
		t.Fatalf("%+v", r)
	}
	// Without AllowPrivate the same loopback target is refused at connect time.
	strict := newEnv(t, func(c *Config) { c.AllowPrivate = false })
	s := strict.hook(Hook{Action: Action{Type: ActionRelay, TargetURL: target.URL}}, "")
	strict.post("/hook/"+s.Slug, body, strict.signed(s, body, nil))
	strict.drain()
	if r := strict.last(s); r.result != "transport_failure" || r.state != StatePending {
		t.Fatalf("%+v", r)
	}
}

func TestValidateRejectsBadConfiguration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	npmFeed := e.feed("npm", "artifacts")
	cacheFeed := e.feed("oci", "cache")
	remote := e.source("r", "npm", "remote", "https://registry.npmjs.org")
	local := e.source("l", "npm", "local", "")
	check := func(h Hook, wantErr string) {
		t.Helper()
		h.Slug = "valid-slug"
		if h.AuthMode == "" {
			h.AuthMode = AuthURLToken
		}
		err := Validate(ctx, e.f.Pool, e.f.InstanceID, &h, false)
		if wantErr == "" {
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			return
		}
		var ve ValidationError
		if err == nil || !asValidation(err, &ve) || !strings.Contains(string(ve), wantErr) {
			t.Fatalf("want %q, got %v", wantErr, err)
		}
	}
	check(Hook{Action: Action{Type: ActionPrewarm, FeedID: npmFeed, Kind: "package", Name: "{{ .name }}", Version: "{{ .version }}"}}, "")
	check(Hook{Action: Action{Type: ActionPrewarm, FeedID: npmFeed, Kind: "package", Name: "{{ .name }}"}}, "a valid version is required")
	check(Hook{Action: Action{Type: ActionPrewarm, FeedID: npmFeed, Kind: "package", Name: "{{ .name | exec }}", Version: "1"}}, "unknown filter")
	check(Hook{Action: Action{Type: ActionPrewarm, FeedID: npmFeed, Kind: "image", Name: "{{ .name }}"}}, "image list is not supported")
	check(Hook{Action: Action{Type: ActionPrewarm, FeedID: "nope", Kind: "package", Name: "x"}}, "choose a feed")
	check(Hook{Action: Action{Type: ActionCleanCache, FeedID: npmFeed, Prefix: "x"}}, "not a cache feed")
	check(Hook{Action: Action{Type: ActionCleanCache, FeedID: cacheFeed, Prefix: "{{ .repo }}"}}, "")
	check(Hook{Action: Action{Type: ActionCleanCache, FeedID: cacheFeed}}, "prefix is required")
	check(Hook{Action: Action{Type: ActionPurgeSource, SourceID: local}}, "remote source does not exist")
	check(Hook{Action: Action{Type: ActionPurgeSource, SourceID: remote}}, "")
	check(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "replication job does not exist")
	check(Hook{Action: Action{Type: ActionUpstream, Provider: "bogus"}}, "provider must be")
	check(Hook{Action: Action{Type: ActionUpstream, Provider: ProviderArtifactory}}, "choose the remote sources or origins")
	check(Hook{Action: Action{Type: ActionUpstream, Provider: ProviderNexus, Origins: []string{"nexus.example.com"}}}, "")
	check(Hook{Action: Action{Type: ActionUpstream, Provider: ProviderGitHub, Origins: []string{"bad host!"}}}, "not a host name")
	check(Hook{Action: Action{Type: ActionRelay, TargetURL: "http://example.com/x"}}, "public https URL")
	check(Hook{Action: Action{Type: ActionRelay, TargetURL: "https://127.0.0.1/x"}}, "public https URL")
	check(Hook{Action: Action{Type: ActionRelay, TargetURL: "https://hooks.example.com/x", ResignSecretID: "11111111-1111-1111-1111-111111111111"}}, "re-signing secret does not exist")
	check(Hook{Action: Action{Type: ActionRelay, TargetURL: "https://hooks.example.com/x", ResignSecretID: e.secretID}}, "")
	check(Hook{AuthMode: AuthHMAC, HMACFormat: "bogus", SigningSecretID: e.secretID, Action: Action{Type: ActionReplicationRun}}, "signature format")
	check(Hook{AuthMode: AuthHMAC, HMACFormat: FormatGitHub, Action: Action{Type: ActionReplicationRun}}, "choose a signing secret")
	check(Hook{AuthMode: AuthBearer, BearerScope: "root", Action: Action{Type: ActionReplicationRun}}, "scope")
	check(Hook{AuthMode: "magic", Action: Action{Type: ActionReplicationRun}}, "auth mode")
	check(Hook{Action: Action{Type: "shell"}}, "action type")
	h := Hook{Slug: "UPPER", AuthMode: AuthURLToken, Action: Action{Type: ActionReplicationRun}}
	if err := Validate(ctx, e.f.Pool, e.f.InstanceID, &h, false); err == nil {
		t.Fatal("bad slug accepted")
	}
	// Fields foreign to the action type are dropped.
	keep := Hook{Slug: "ok", AuthMode: AuthURLToken, Action: Action{Type: ActionPurgeSource, SourceID: remote, Prefix: "junk", TargetURL: "https://x.example.com"}}
	if err := Validate(ctx, e.f.Pool, e.f.InstanceID, &keep, false); err != nil || keep.Action.Prefix != "" || keep.Action.TargetURL != "" {
		t.Fatalf("%v %+v", err, keep.Action)
	}
}

func asValidation(err error, out *ValidationError) bool {
	ve, ok := err.(ValidationError)
	if ok {
		*out = ve
	}
	return ok
}

func TestSecretUsersCountsVerificationAndResigning(t *testing.T) {
	e := newEnv(t)
	e.hook(Hook{Action: Action{Type: ActionReplicationRun, JobID: "11111111-1111-1111-1111-111111111111"}}, "")
	e.hook(Hook{AuthMode: AuthURLToken, Action: Action{Type: ActionRelay, TargetURL: "https://x.example.com", ResignSecretID: e.secretID}}, "tkn_"+strings.Repeat("r", 30))
	n, err := SecretUsers(context.Background(), e.f.Pool, e.f.InstanceID, e.secretID)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
}
