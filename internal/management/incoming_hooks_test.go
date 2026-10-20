package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/hooks"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

type storeResolver struct{ store secrets.Store }

func (r storeResolver) Resolve(ctx context.Context, instance, record string) ([]byte, error) {
	_, plain, err := r.store.Open(ctx, instance, record, "webhook")
	return plain, err
}

type hookFixture struct {
	*sourceHTTPFixture
	svc     *hooks.Service
	receive *http.ServeMux
	keys    *secrets.Keyring
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	f, keys := webhookHTTPFixture(t)
	svc, err := hooks.New(hooks.Config{DB: f.db, InstanceID: f.instance, Secrets: storeResolver{secrets.Store{DB: f.db, Keys: keys}}, PublicURL: "https://ant.example.com/", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	mux := http.NewServeMux()
	RegisterWebhooks(mux, f.db, keys)
	RegisterIncomingHooks(mux, f.db, svc)
	f.handler = WithRequestID(mux)
	recv := http.NewServeMux()
	svc.Register(recv)
	return &hookFixture{sourceHTTPFixture: f, svc: svc, receive: recv, keys: keys}
}

func (f *hookFixture) post(path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://ant.example.com"+path, bytes.NewReader(body))
	r.RemoteAddr = "198.51.100.7:5000"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.receive.ServeHTTP(w, r)
	return w
}

func (f *hookFixture) createHook(body, key string) (IncomingHook, *httptest.ResponseRecorder) {
	f.t.Helper()
	w := f.request("POST", "/api/v1/incoming-hooks", body, key, "")
	var item IncomingHook
	_ = json.Unmarshal(w.Body.Bytes(), &item)
	return item, w
}

const upstreamAction = `{"type":"upstream_notify","provider":"dockerhub"}`

func TestIncomingHookConfigAndCRUD(t *testing.T) {
	f := newHookFixture(t)
	cfg := f.request("GET", "/api/v1/incoming-hook-config", "", "", "")
	expectSourceStatus(t, cfg, 200)
	var c IncomingHookConfig
	_ = json.Unmarshal(cfg.Body.Bytes(), &c)
	if c.HookBaseURL != "https://ant.example.com/hook/" || c.MaxBodyBytes != hooks.DefaultMaxBody || len(c.HMACFormats) != 6 {
		t.Fatalf("config %+v", c)
	}
	secret := createWebhookSecret(f.sourceHTTPFixture, `{"name":"gh"}`, "s1")

	// HMAC hook: no token, URL built from the public URL.
	h, w := f.createHook(`{"slug":"release-notify","description":"GitHub releases","auth_mode":"hmac","hmac_format":"github","signing_secret_id":"`+secret.ID+`","action":`+upstreamAction+`}`, "c1")
	expectSourceStatus(t, w, 201)
	if h.Token != "" || h.URL != "https://ant.example.com/hook/release-notify" || h.Path != "/hook/release-notify" || h.SigningSecretName != "gh" || h.ActionSummary == "" || h.RatePerMinute != 60 || !h.Enabled {
		t.Fatalf("hmac hook %+v", h)
	}

	// URL token: shown once, then only a hint.
	body := `{"slug":"hub-push","auth_mode":"url_token","action":` + upstreamAction + `}`
	tok, w := f.createHook(body, "c2")
	expectSourceStatus(t, w, 201)
	if !strings.HasPrefix(tok.Token, "hk_") || len(tok.Token) < 40 || tok.TokenHint != tok.Token[len(tok.Token)-4:] {
		t.Fatalf("token %+v", tok)
	}
	replay := f.request("POST", "/api/v1/incoming-hooks", body, "c2", "")
	expectSourceStatus(t, replay, 201)
	if strings.Contains(replay.Body.String(), tok.Token) {
		t.Fatal("an idempotent replay returned the token again")
	}
	got := f.request("GET", "/api/v1/incoming-hooks/"+tok.ID, "", "", "")
	expectSourceStatus(t, got, 200)
	if strings.Contains(got.Body.String(), tok.Token) {
		t.Fatal("GET returned the token")
	}
	var stored int
	_ = f.db.QueryRow(context.Background(), `SELECT count(*) FROM incoming_hooks WHERE token_hash IS NOT NULL AND position($1 in action::text||description)>0`, tok.Token).Scan(&stored)
	if stored != 0 {
		t.Fatal("token stored in plaintext")
	}

	// Validation.
	for name, b := range map[string]string{
		"duplicate slug": `{"slug":"hub-push","auth_mode":"url_token","action":` + upstreamAction + `}`,
	} {
		expectSourceStatus(t, f.request("POST", "/api/v1/incoming-hooks", b, "dup-"+name, ""), 409)
	}
	for name, b := range map[string]string{
		"bad slug":         `{"slug":"Bad_Slug","auth_mode":"url_token","action":` + upstreamAction + `}`,
		"unknown action":   `{"slug":"x1","auth_mode":"url_token","action":{"type":"shell"}}`,
		"missing secret":   `{"slug":"x2","auth_mode":"hmac","hmac_format":"github","signing_secret_id":"11111111-1111-1111-1111-111111111111","action":` + upstreamAction + `}`,
		"hmac no format":   `{"slug":"x3","auth_mode":"hmac","signing_secret_id":"` + secret.ID + `","action":` + upstreamAction + `}`,
		"relay private":    `{"slug":"x4","auth_mode":"url_token","action":{"type":"relay","target_url":"https://127.0.0.1/hook"}}`,
		"unknown property": `{"slug":"x5","auth_mode":"url_token","action":` + upstreamAction + `,"extra":1}`,
		"artifactory bare": `{"slug":"x6","auth_mode":"url_token","action":{"type":"upstream_notify","provider":"artifactory"}}`,
		"rate too high":    `{"slug":"x7","auth_mode":"url_token","rate_per_minute":999999,"action":` + upstreamAction + `}`,
	} {
		expectSourceStatus(t, f.request("POST", "/api/v1/incoming-hooks", b, "bad-"+name, ""), 400)
	}

	// List with statistics, ordered by slug.
	list := f.request("GET", "/api/v1/incoming-hooks", "", "", "")
	expectSourceStatus(t, list, 200)
	var page struct{ Items []IncomingHook }
	_ = json.Unmarshal(list.Body.Bytes(), &page)
	if len(page.Items) != 2 || page.Items[0].Slug != "hub-push" || page.Items[1].Slug != "release-notify" {
		t.Fatalf("list %s", list.Body)
	}
	one := f.request("GET", "/api/v1/incoming-hooks?limit=1", "", "", "")
	expectSourceStatus(t, one, 200)
	if !strings.Contains(one.Body.String(), `"next_after":"hub-push"`) {
		t.Fatalf("paging %s", one.Body)
	}

	// Patch with optimistic concurrency; omitted fields stay.
	patched := f.request("PATCH", "/api/v1/incoming-hooks/"+h.ID, `{"enabled":false,"description":"paused"}`, "p1", `"1"`)
	expectSourceStatus(t, patched, 200)
	var after IncomingHook
	_ = json.Unmarshal(patched.Body.Bytes(), &after)
	if after.Enabled || after.Description != "paused" || after.Slug != "release-notify" || after.AuthMode != hooks.AuthHMAC || after.SigningSecretID != secret.ID || after.Revision != 2 {
		t.Fatalf("patched %+v", after)
	}
	expectSourceStatus(t, f.request("PATCH", "/api/v1/incoming-hooks/"+h.ID, `{"enabled":true}`, "p2", `"1"`), 412)
	// A disabled hook answers like an unknown one.
	if w := f.post("/hook/release-notify", []byte(`{}`), nil); w.Code != 404 {
		t.Fatalf("disabled hook served: %d", w.Code)
	}

	// Switching a hook to url_token generates a token once; regenerating invalidates the old one.
	sw := f.request("PATCH", "/api/v1/incoming-hooks/"+h.ID, `{"enabled":true,"auth_mode":"url_token"}`, "p3", `"2"`)
	expectSourceStatus(t, sw, 200)
	var switched IncomingHook
	_ = json.Unmarshal(sw.Body.Bytes(), &switched)
	if !strings.HasPrefix(switched.Token, "hk_") || switched.SigningSecretID != "" || switched.HMACFormat != "" {
		t.Fatalf("switched %+v", switched)
	}
	if w := f.post("/hook/release-notify/"+switched.Token, []byte(`{"push_data":{"tag":"1"},"repository":{"repo_name":"a/b"}}`), nil); w.Code != 202 {
		t.Fatalf("new token refused: %d", w.Code)
	}
	regen := f.request("POST", "/api/v1/incoming-hooks/"+h.ID+"/token", "", "r1", `"3"`)
	expectSourceStatus(t, regen, 200)
	var fresh IncomingHook
	_ = json.Unmarshal(regen.Body.Bytes(), &fresh)
	if fresh.Token == "" || fresh.Token == switched.Token {
		t.Fatal("token not regenerated")
	}
	if w := f.post("/hook/release-notify/"+switched.Token, []byte(`{}`), nil); w.Code != 404 {
		t.Fatalf("old token still works: %d", w.Code)
	}
	if w := f.post("/hook/release-notify/"+fresh.Token, []byte(`{}`), nil); w.Code != 202 {
		t.Fatalf("regenerated token refused: %d", w.Code)
	}
	// Regenerating is only meaningful for url_token hooks.
	hm, w := f.createHook(`{"slug":"gh-sig","auth_mode":"hmac","hmac_format":"github","signing_secret_id":"`+secret.ID+`","action":`+upstreamAction+`}`, "c3")
	expectSourceStatus(t, w, 201)
	expectSourceStatus(t, f.request("POST", "/api/v1/incoming-hooks/"+hm.ID+"/token", "", "r2", `"1"`), 400)

	// The signing secret cannot be deleted while a hook uses it.
	expectSourceStatus(t, f.request("DELETE", "/api/v1/webhook-secrets/"+secret.ID, "", "ds", `"`+itoa(secret.Revision)+`"`), 409)

	// Delete removes the hook and its log.
	expectSourceStatus(t, f.request("DELETE", "/api/v1/incoming-hooks/"+hm.ID, "", "del", `"1"`), 200)
	expectSourceStatus(t, f.request("GET", "/api/v1/incoming-hooks/"+hm.ID, "", "", ""), 404)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/webhook-secrets/"+secret.ID, "", "ds2", `"`+itoa(secret.Revision)+`"`), 200)

	// Configuration changes are audited.
	for _, action := range []string{"incoming_hook.create", "incoming_hook.patch", "incoming_hook.token", "incoming_hook.delete"} {
		if f.count(`SELECT count(*) FROM audit_events WHERE action=$1`, action) == 0 {
			t.Fatalf("%s not audited", action)
		}
	}
}

func TestIncomingHookAdminOnly(t *testing.T) {
	f := newHookFixture(t)
	tok, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/incoming-hooks", "", "", "", tok.Secret), 403)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/incoming-hook-config", "", "", "", tok.Secret), 403)
	expectSourceStatus(t, f.requestAs("POST", "/api/v1/incoming-hooks", `{"slug":"a","auth_mode":"url_token","action":`+upstreamAction+`}`, "k", "", tok.Secret), 403)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/incoming-hooks", "", "", "", ""), 401)
}

func TestIncomingHookEndToEndLogTestAndRedeliver(t *testing.T) {
	f := newHookFixture(t)
	secret := createWebhookSecret(f.sourceHTTPFixture, `{"name":"gh","value":"`+strings.Repeat("g", 40)+`"}`, "s1")
	h, w := f.createHook(`{"slug":"hub","auth_mode":"hmac","hmac_format":"github","signing_secret_id":"`+secret.ID+`","action":`+upstreamAction+`}`, "c1")
	expectSourceStatus(t, w, 201)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.svc.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	payload := []byte(`{"push_data":{"tag":"1.4.0"},"repository":{"repo_name":"acme/api"},"callback_url":"https://registry.hub.docker.com/u/acme/api/hook/abc/","token":"must-not-leak"}`)
	name, value := hooks.SignBody(hooks.FormatGitHub, payload, []byte(strings.Repeat("g", 40)))
	resp := f.post("/hook/hub", payload, map[string]string{name: value, "X-GitHub-Delivery": "g-1"})
	if resp.Code != 202 {
		t.Fatalf("receive: %d %s", resp.Code, resp.Body)
	}
	if replay := f.post("/hook/hub", payload, map[string]string{name: value, "X-GitHub-Delivery": "g-1"}); replay.Code != 409 {
		t.Fatalf("replay: %d", replay.Code)
	}
	f.post("/hook/hub", payload, map[string]string{name: "sha256=00"})

	type page struct {
		Items []IncomingHookDelivery
		Next  string `json:"next_before"`
	}
	waitFor := func(want int, state string) page {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			rec := f.request("GET", "/api/v1/incoming-hooks/"+h.ID+"/deliveries?state="+state, "", "", "")
			expectSourceStatus(t, rec, 200)
			var p page
			_ = json.Unmarshal(rec.Body.Bytes(), &p)
			if len(p.Items) >= want {
				return p
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %d %s deliveries: %s", want, state, rec.Body)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	done1 := waitFor(1, "succeeded")
	d := done1.Items[0]
	if d.ActionResult != "no_matching_source" || d.AuthResult != "ok" || d.SourceIP != "198.51.100.7" || d.DeliveryRef != "X-GitHub-Delivery:g-1" || d.Detail["provider"] != "dockerhub" {
		t.Fatalf("delivery %+v", d)
	}
	if strings.Contains(d.Preview, "must-not-leak") || !strings.Contains(d.Preview, "[redacted]") || !strings.Contains(d.Preview, "acme/api") {
		t.Fatalf("preview %q", d.Preview)
	}
	rejected := waitFor(2, "rejected")
	results := map[string]bool{}
	for _, r := range rejected.Items {
		results[r.AuthResult] = true
	}
	if !results["duplicate"] || !results["invalid"] {
		t.Fatalf("rejected log %+v", rejected.Items)
	}

	// Send test: queued, run, logged as a test.
	test := f.request("POST", "/api/v1/incoming-hooks/"+h.ID+"/test", "", "t1", "")
	expectSourceStatus(t, test, 202)
	var q IncomingHookQueued
	_ = json.Unmarshal(test.Body.Bytes(), &q)
	if q.DeliveryID == "" {
		t.Fatal("no delivery id")
	}
	// Redeliver the first delivery.
	re := f.request("POST", "/api/v1/incoming-hooks/"+h.ID+"/deliveries/"+d.ID+"/redeliver", "", "t2", "")
	expectSourceStatus(t, re, 202)
	all := waitFor(3, "succeeded")
	kinds := map[string]bool{}
	for _, it := range all.Items {
		kinds[it.AuthResult] = true
		if it.AuthResult == "redelivery" && it.RedeliveryOf != d.ID {
			t.Fatalf("redelivery link %+v", it)
		}
	}
	if !kinds["test"] || !kinds["redelivery"] || !kinds["ok"] {
		t.Fatalf("kinds %v", kinds)
	}
	// A rejected call has no payload to redeliver.
	expectSourceStatus(t, f.request("POST", "/api/v1/incoming-hooks/"+h.ID+"/deliveries/"+rejected.Items[0].ID+"/redeliver", "", "t3", ""), 404)
	// Paging by `before`.
	pg := f.request("GET", "/api/v1/incoming-hooks/"+h.ID+"/deliveries?limit=2", "", "", "")
	expectSourceStatus(t, pg, 200)
	var first page
	_ = json.Unmarshal(pg.Body.Bytes(), &first)
	if len(first.Items) != 2 || first.Next == "" {
		t.Fatalf("page %s", pg.Body)
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/incoming-hooks/"+h.ID+"/deliveries?limit=2&before="+first.Next, "", "", ""), 200)

	// The list carries last-received time and the success rate.
	list := f.request("GET", "/api/v1/incoming-hooks", "", "", "")
	var lp struct{ Items []IncomingHook }
	_ = json.Unmarshal(list.Body.Bytes(), &lp)
	got := lp.Items[0]
	if got.LastReceivedAt == nil || got.Succeeded7d != 3 || got.SuccessRate == nil || *got.SuccessRate != 1 {
		t.Fatalf("stats %+v", got)
	}
}
