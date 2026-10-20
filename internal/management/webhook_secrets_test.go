package management

import (
	"context"
	"encoding/json"
	"net/http"
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

func TestWebhookSlugAndBaseURL(t *testing.T) {
	f, _ := webhookHTTPFixture(t)
	ctx := context.Background()
	secret := createWebhookSecret(f, `{"name":"shared"}`, "s")
	body := `{"name":"deploys","secret_id":"` + secret.ID + `"}`
	// No base URL yet: slug mode is refused with guidance, custom mode works.
	expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", body, "nobase", ""), 409)
	custom := f.request("POST", "/api/v1/webhooks", `{"endpoint":"https://legacy.example.invalid/hook","secret_id":"`+secret.ID+`"}`, "custom", "")
	expectSourceStatus(t, custom, 201)
	var c Webhook
	_ = json.Unmarshal(custom.Body.Bytes(), &c)
	if !c.CustomEndpoint || c.SecretName != "shared" {
		t.Fatal("custom endpoint not marked")
	}

	settingsRevision := func() string {
		return `"` + itoa(int64(f.count(`SELECT COALESCE((SELECT revision FROM instance_settings WHERE instance_id=$1),1)`, f.instance))) + `"`
	}
	// The base URL setting lives on its own route; invalid values are rejected.
	ops := http.NewServeMux()
	RegisterOperations(ops, OpsDeps{Pool: f.db, Codec: NewCursorCodec(make([]byte, 32))})
	RegisterWebhooks(ops, f.db, mustKeys(t))
	f.handler = WithRequestID(ops)
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings/webhook-base-url", `{"value":"http://hooks.example.com/ant/"}`, "http", settingsRevision()), 400)
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings/webhook-base-url", `{"value":"https://user:pw@hooks.example.com/"}`, "cred", settingsRevision()), 400)
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings/webhook-base-url", `{"value":"https://hooks.example.com/ant"}`, "set", settingsRevision()), 200)
	if got := f.count(`SELECT count(*) FROM instance_settings WHERE webhook_base_url='https://hooks.example.com/ant/'`); got != 1 {
		t.Fatal("base URL not normalized with one trailing slash")
	}
	// PUT /settings does not reset the base URL.
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings", `{}`, "put", settingsRevision()), 200)
	if f.count(`SELECT count(*) FROM instance_settings WHERE webhook_base_url IS NOT NULL`) != 1 {
		t.Fatal("PUT /settings reset the webhook base URL")
	}

	// Slug validation and URL joining.
	for _, bad := range []string{"Deploys", "dep_loys", "", strings.Repeat("a", 64), "a/b"} {
		b, _ := json.Marshal(map[string]string{"name": bad, "secret_id": secret.ID})
		expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", string(b), "bad-"+bad, ""), 400)
	}
	created := f.request("POST", "/api/v1/webhooks", body, "good", "")
	expectSourceStatus(t, created, 201)
	var wh Webhook
	_ = json.Unmarshal(created.Body.Bytes(), &wh)
	if wh.Endpoint != "https://hooks.example.com/ant/deploys" || wh.CustomEndpoint || wh.Name != "deploys" {
		t.Fatalf("resolved %+v", wh)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", body, "dup-name", ""), 409)
	// Slug change re-resolves; custom stays untouched on base changes.
	patched := f.request("PATCH", "/api/v1/webhooks/"+wh.ID, `{"name":"releases"}`, "rename", `"1"`)
	expectSourceStatus(t, patched, 200)
	if !strings.Contains(patched.Body.String(), "https://hooks.example.com/ant/releases") {
		t.Fatal("rename did not re-resolve")
	}
	// Base URL change re-resolves every slug webhook and audits it.
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings/webhook-base-url", `{"value":"https://new.example.com/h/"}`, "move-base", settingsRevision()), 200)
	var endpoint string
	_ = f.db.QueryRow(ctx, `SELECT endpoint FROM event_subscriptions WHERE id=$1`, wh.ID).Scan(&endpoint)
	if endpoint != "https://new.example.com/h/releases" {
		t.Fatalf("endpoint %s", endpoint)
	}
	var customEndpoint string
	_ = f.db.QueryRow(ctx, `SELECT endpoint FROM event_subscriptions WHERE id=$1`, c.ID).Scan(&customEndpoint)
	if customEndpoint != "https://legacy.example.invalid/hook" || f.count(`SELECT count(*) FROM audit_events WHERE action='webhook.reresolve' AND resource_id=$1`, wh.ID) != 1 {
		t.Fatal("custom endpoint changed or re-resolution unaudited")
	}
	// Clearing leaves resolved endpoints in place.
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings/webhook-base-url", `{"value":""}`, "clear", settingsRevision()), 200)
	_ = f.db.QueryRow(ctx, `SELECT endpoint FROM event_subscriptions WHERE id=$1`, wh.ID).Scan(&endpoint)
	if endpoint != "https://new.example.com/h/releases" {
		t.Fatal("clearing the base URL rewrote endpoints")
	}

	// Env lock: the environment value wins and the setting is read-only.
	locked := http.NewServeMux()
	RegisterOperations(locked, OpsDeps{Pool: f.db, Codec: NewCursorCodec(make([]byte, 32)), Env: []EnvSetting{{Key: "webhook_base_url", Value: "https://env.example.com/e/"}}})
	RegisterWebhooks(locked, f.db, mustKeys(t), WebhookOptions{BaseURL: "https://env.example.com/e/"})
	f.handler = WithRequestID(locked)
	expectSourceStatus(t, f.request("PUT", "/api/v1/settings/webhook-base-url", `{"value":"https://other.example.com/"}`, "locked", settingsRevision()), 409)
	envHook := f.request("POST", "/api/v1/webhooks", `{"name":"fromenv","secret_id":"`+secret.ID+`"}`, "env", "")
	expectSourceStatus(t, envHook, 201)
	if !strings.Contains(envHook.Body.String(), "https://env.example.com/e/fromenv") {
		t.Fatal("env base URL not used")
	}
	got := f.request("GET", "/api/v1/settings", "", "", "")
	expectSourceStatus(t, got, 200)
	if !strings.Contains(got.Body.String(), `"webhook_base_url"`) || !strings.Contains(got.Body.String(), `"source":"env"`) {
		t.Fatal("env-locked setting not reported")
	}
	n, err := ReconcileWebhookEndpoints(ctx, f.db, f.instance, "https://env.example.com/e/", false)
	if err != nil || n != 1 {
		t.Fatalf("reconcile moved %d (%v)", n, err)
	}
}

func mustKeys(t *testing.T) *secrets.Keyring {
	k, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}
