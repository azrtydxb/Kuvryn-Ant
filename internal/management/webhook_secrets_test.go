package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

func createWebhookSecret(f *sourceHTTPFixture, body, key string) WebhookSecret {
	f.t.Helper()
	w := f.request("POST", "/api/v1/webhook-secrets", body, key, "")
	expectSourceStatus(f.t, w, 201)
	var s WebhookSecret
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func rev(s WebhookSecret) string { return `"` + itoa(s.Revision) + `"` }
func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestWebhookSigningSecretLifecycle(t *testing.T) {
	f, keys := webhookHTTPFixture(t)
	ctx := context.Background()
	supplied := strings.Repeat("supplied-secret-", 3)

	generated := createWebhookSecret(f, `{"name":"prod","description":"Production receivers"}`, "gen")
	if !strings.HasPrefix(generated.Value, "whsec_") || len(generated.Value) < 40 {
		t.Fatal("server did not generate a value")
	}
	// Replay returns the metadata but never the value.
	w := f.request("POST", "/api/v1/webhook-secrets", `{"name":"prod","description":"Production receivers"}`, "gen", "")
	expectSourceStatus(t, w, 201)
	if strings.Contains(w.Body.String(), generated.Value) || strings.Contains(w.Body.String(), `"value"`) {
		t.Fatal("replay exposed the generated value")
	}
	pasted := createWebhookSecret(f, `{"name":"staging","value":"`+supplied+`"}`, "paste")
	if pasted.Value != supplied {
		t.Fatal("supplied value not echoed once")
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/webhook-secrets", `{"name":"prod"}`, "dup", ""), 409)
	expectSourceStatus(t, f.request("POST", "/api/v1/webhook-secrets", `{"name":"short","value":"tooshort"}`, "short", ""), 400)

	// Never readable again; encrypted at rest; absent from caches, audit and outbox.
	for _, path := range []string{"/api/v1/webhook-secrets", "/api/v1/webhook-secrets/" + generated.ID} {
		body := f.request("GET", path, "", "", "")
		expectSourceStatus(t, body, 200)
		if strings.Contains(body.Body.String(), generated.Value) || strings.Contains(body.Body.String(), supplied) || strings.Contains(body.Body.String(), `"value"`) {
			t.Fatal("value readable after creation")
		}
	}
	for _, secret := range []string{generated.Value, supplied} {
		for _, query := range []string{`SELECT count(*) FROM encrypted_secrets WHERE envelope::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM mutation_results WHERE response::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM audit_events WHERE row_to_json(audit_events)::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM outbox_events WHERE payload::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM webhook_signing_secrets WHERE row_to_json(webhook_signing_secrets)::text LIKE '%'||$1||'%'`} {
			if f.count(query, secret) != 0 {
				t.Fatal("plaintext persisted")
			}
		}
	}
	var ref string
	if err := f.db.QueryRow(ctx, `SELECT secret_id::text FROM webhook_signing_secrets WHERE id=$1`, generated.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	_, plain, err := (secrets.Store{DB: f.db, Keys: keys}).Open(ctx, f.instance, ref, "webhook")
	if err != nil || string(plain) != generated.Value {
		t.Fatal("stored value does not decrypt to the returned value")
	}

	// Rotate without grace replaces in place; stale revision is refused.
	rotated := f.request("POST", "/api/v1/webhook-secrets/"+generated.ID+"/rotate", `{}`, "rot1", rev(generated))
	expectSourceStatus(t, rotated, 200)
	var r1 WebhookSecret
	_ = json.Unmarshal(rotated.Body.Bytes(), &r1)
	if r1.Value == "" || r1.Value == generated.Value || r1.RotatedAt == nil || r1.PreviousValidUntil != nil || r1.Revision != 2 {
		t.Fatalf("rotation result %+v", r1)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/webhook-secrets/"+generated.ID+"/rotate", `{}`, "rot-stale", rev(generated)), 412)

	// Rotate with a grace window keeps the previous value in a separate record.
	graceBody := `{"grace_seconds":3600}`
	rotated = f.request("POST", "/api/v1/webhook-secrets/"+generated.ID+"/rotate", graceBody, "rot2", rev(r1))
	expectSourceStatus(t, rotated, 200)
	var r2 WebhookSecret
	_ = json.Unmarshal(rotated.Body.Bytes(), &r2)
	if r2.PreviousValidUntil == nil || r2.Value == "" {
		t.Fatal("grace window missing")
	}
	var current, previous string
	if err := f.db.QueryRow(ctx, `SELECT secret_id::text,previous_secret_id::text FROM webhook_signing_secrets WHERE id=$1`, generated.ID).Scan(&current, &previous); err != nil {
		t.Fatal(err)
	}
	_, now, _ := (secrets.Store{DB: f.db, Keys: keys}).Open(ctx, f.instance, current, "webhook")
	_, old, _ := (secrets.Store{DB: f.db, Keys: keys}).Open(ctx, f.instance, previous, "webhook")
	if string(now) != r2.Value || string(old) != r1.Value {
		t.Fatal("current and previous values wrong")
	}
	// Once the window ends the previous ciphertext is purged by the next mutation.
	if _, err := f.db.Exec(ctx, `UPDATE webhook_signing_secrets SET previous_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, generated.ID); err != nil {
		t.Fatal(err)
	}
	createWebhookSecret(f, `{"name":"another"}`, "another")
	if f.count(`SELECT count(*) FROM encrypted_secrets WHERE id=$1`, previous) != 0 || f.count(`SELECT count(*) FROM webhook_signing_secrets WHERE id=$1 AND previous_secret_id IS NULL`, generated.ID) != 1 {
		t.Fatal("expired previous value not purged")
	}

	// Delete is refused while a webhook uses the secret.
	hook := f.request("POST", "/api/v1/webhooks", `{"endpoint":"https://hooks.example.invalid/x","secret_id":"`+generated.ID+`"}`, "hook", "")
	expectSourceStatus(t, hook, 201)
	var wh Webhook
	_ = json.Unmarshal(hook.Body.Bytes(), &wh)
	cur := f.request("GET", "/api/v1/webhook-secrets/"+generated.ID, "", "", "")
	var s WebhookSecret
	_ = json.Unmarshal(cur.Body.Bytes(), &s)
	if s.UsedByCount != 1 || s.UsedBy[0].ID != wh.ID {
		t.Fatal("used_by missing")
	}
	expectSourceStatus(t, f.request("DELETE", "/api/v1/webhook-secrets/"+generated.ID, "", "del", rev(s)), 409)
	// Free the secret, then delete.
	expectSourceStatus(t, f.request("PATCH", "/api/v1/webhooks/"+wh.ID, `{"secret_id":"`+pasted.ID+`"}`, "move", `"1"`), 200)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/webhook-secrets/"+generated.ID, "", "del2", rev(s)), 200)
	expectSourceStatus(t, f.request("GET", "/api/v1/webhook-secrets/"+generated.ID, "", "", ""), 404)
	if f.count(`SELECT count(*) FROM encrypted_secrets WHERE id=$1`, current) != 0 {
		t.Fatal("deleted secret ciphertext retained")
	}
	for _, action := range []string{"webhook_secret.create", "webhook_secret.rotate", "webhook_secret.delete"} {
		if f.count(`SELECT count(*) FROM audit_events a JOIN outbox_events o ON o.audit_id=a.id WHERE a.action=$1`, action) < 1 {
			t.Fatalf("%s audit/outbox missing", action)
		}
	}

	// Authz: admin only.
	tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/webhook-secrets", "", "", "", tok.Secret), 403)
	expectSourceStatus(t, f.requestAs("POST", "/api/v1/webhook-secrets", `{"name":"x"}`, "nope", "", tok.Secret), 403)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/webhook-secrets", "", "", "", ""), 401)
}

func TestWebhookNameAndEndpoint(t *testing.T) {
	f, _ := webhookHTTPFixture(t)
	secret := createWebhookSecret(f, `{"name":"shared"}`, "s")
	body := `{"name":"Deploys (prod)","endpoint":"https://hooks.example.com/ant/deploys","secret_id":"` + secret.ID + `"}`
	created := f.request("POST", "/api/v1/webhooks", body, "good", "")
	expectSourceStatus(t, created, 201)
	var wh Webhook
	_ = json.Unmarshal(created.Body.Bytes(), &wh)
	if wh.Endpoint != "https://hooks.example.com/ant/deploys" || wh.Name != "Deploys (prod)" || wh.SecretName != "shared" {
		t.Fatalf("created %+v", wh)
	}
	// The name is a display label, unique per instance regardless of case.
	expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", strings.Replace(body, "Deploys (prod)", "deploys (PROD)", 1), "dup-name", ""), 409)
	// The endpoint is required, https only and validated with the egress rules.
	expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", `{"name":"slug-only","secret_id":"`+secret.ID+`"}`, "no-endpoint", ""), 400)
	for _, bad := range []string{"http://hooks.example.com/x", "https://localhost/x", "https://10.0.0.1/x", "https://user:pw@hooks.example.com/x", "https://hooks.example.com/x?q=1"} {
		b, _ := json.Marshal(map[string]string{"name": "bad", "endpoint": bad, "secret_id": secret.ID})
		expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", string(b), "bad-"+bad, ""), 400)
	}
	for _, bad := range []string{" padded ", strings.Repeat("a", 101), "line\nbreak"} {
		b, _ := json.Marshal(map[string]string{"name": bad, "endpoint": "https://hooks.example.com/x", "secret_id": secret.ID})
		expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", string(b), "badname-"+bad, ""), 400)
	}
	// Legacy shape: endpoint plus secret_id and no name gets a generated label.
	legacy := f.request("POST", "/api/v1/webhooks", `{"endpoint":"https://legacy.example.invalid/hook","secret_id":"`+secret.ID+`"}`, "legacy", "")
	expectSourceStatus(t, legacy, 201)
	var l Webhook
	_ = json.Unmarshal(legacy.Body.Bytes(), &l)
	if !strings.HasPrefix(l.Name, "webhook-") {
		t.Fatalf("generated name %q", l.Name)
	}
	// Rename and retarget in place.
	patched := f.request("PATCH", "/api/v1/webhooks/"+wh.ID, `{"name":"Releases","endpoint":"https://hooks.example.com/ant/releases"}`, "edit", `"1"`)
	expectSourceStatus(t, patched, 200)
	if !strings.Contains(patched.Body.String(), `"name":"Releases"`) || !strings.Contains(patched.Body.String(), "ant/releases") {
		t.Fatalf("patch result %s", patched.Body.String())
	}
	expectSourceStatus(t, f.request("PATCH", "/api/v1/webhooks/"+wh.ID, `{"endpoint":"http://hooks.example.com/x"}`, "edit-bad", `"2"`), 400)
	// slug-mode leftovers are no longer fields.
	expectSourceStatus(t, f.request("PATCH", "/api/v1/webhooks/"+wh.ID, `{"custom_endpoint":false}`, "edit-old", `"2"`), 400)
}

// The webhook base URL setting and its route are gone.
func TestWebhookBaseURLRemoved(t *testing.T) {
	f, _ := webhookHTTPFixture(t)
	ops := http.NewServeMux()
	RegisterOperations(ops, OpsDeps{Pool: f.db, Codec: NewCursorCodec(make([]byte, 32))})
	RegisterWebhooks(ops, f.db, mustKeys(t))
	f.handler = WithRequestID(ops)
	// Not in the API contract any more, so bypass the response validator.
	raw := httptest.NewRequest("PUT", "http://example.test/api/v1/settings/webhook-base-url", strings.NewReader(`{"value":"https://hooks.example.com/"}`))
	raw.Header.Set("Authorization", "Bearer "+f.credential)
	raw.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, raw)
	if rec.Code != 404 && rec.Code != 405 {
		t.Fatalf("base URL route still served: %d", rec.Code)
	}
	got := f.request("GET", "/api/v1/settings", "", "", "")
	expectSourceStatus(t, got, 200)
	if strings.Contains(got.Body.String(), "webhook_base_url") {
		t.Fatal("settings still report the webhook base URL")
	}
}

func mustKeys(t *testing.T) *secrets.Keyring {
	k, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}
