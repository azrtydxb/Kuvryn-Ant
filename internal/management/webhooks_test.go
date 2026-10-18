package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

func webhookHTTPFixture(t *testing.T) (*sourceHTTPFixture, *secrets.Keyring) {
	f := identityHTTPFixture(t)
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterWebhooks(mux, f.db, keys)
	f.handler = WithRequestID(mux)
	return f, keys
}
func createWebhook(f *sourceHTTPFixture, endpoint, secret, key string) Webhook {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"endpoint": endpoint, "secret": secret})
	w := f.request("POST", "/api/v1/webhooks", string(body), key, "")
	expectSourceStatus(f.t, w, 201)
	var item Webhook
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		f.t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "secret_ref") {
		f.t.Fatal("secret response exposure")
	}
	return item
}
func TestWebhookAdministrationContract(t *testing.T) {
	f, keys := webhookHTTPFixture(t)
	ctx := context.Background()
	oldSecret := strings.Repeat("old-private-key-", 3)
	newSecret := strings.Repeat("new-private-key-", 3)
	item := createWebhook(f, "https://hooks.example.invalid/events", oldSecret, "create")
	again := createWebhook(f, "https://hooks.example.invalid/events", oldSecret, "create")
	if item.ID != again.ID {
		t.Fatal("replay changed subscription")
	}
	body, _ := json.Marshal(map[string]string{"endpoint": item.Endpoint, "secret": newSecret})
	expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", string(body), "create", ""), 409)
	if f.count(`SELECT count(*) FROM encrypted_secrets`) != 1 || f.count(`SELECT count(*) FROM event_subscriptions`) != 1 {
		t.Fatal("replay allocated secrets or subscriptions")
	}
	var ref string
	if err := f.db.QueryRow(ctx, `SELECT secret_ref FROM event_subscriptions WHERE id=$1`, item.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	store := secrets.Store{DB: f.db, Keys: keys}
	record, plain, err := store.Open(ctx, f.instance, ref, "webhook")
	if err != nil || string(plain) != oldSecret || record.Revision != 1 {
		t.Fatal("encrypted secret cannot be resolved")
	}
	clear(plain)
	path := "/api/v1/webhooks/" + item.ID
	updated := f.request("PATCH", path, `{"enabled":false}`, "disable", `"1"`)
	expectSourceStatus(t, updated, 200)
	if updated.Header().Get("ETag") != `"2"` {
		t.Fatal("missing revision")
	}
	expectSourceStatus(t, f.request("PATCH", path, `{"enabled":false}`, "disable", `"1"`), 200)
	expectSourceStatus(t, f.request("PATCH", path, `{"enabled":true}`, "stale", `"1"`), 412)
	expectSourceStatus(t, f.request("PATCH", path, `{"enabled":true}`, "missing", ""), 428)
	rotate, _ := json.Marshal(map[string]string{"secret": newSecret})
	expectSourceStatus(t, f.request("PUT", path+"/secret", string(rotate), "rotate", `"2"`), 200)
	expectSourceStatus(t, f.request("PUT", path+"/secret", string(rotate), "rotate", `"2"`), 200)
	expectSourceStatus(t, f.request("PUT", path+"/secret", string(rotate), "rotate-stale", `"2"`), 412)
	record, plain, err = store.Open(ctx, f.instance, ref, "webhook")
	if err != nil || string(plain) != newSecret || record.Revision != 2 {
		t.Fatal("rotation did not replace encrypted secret once")
	}
	clear(plain)
	expectSourceStatus(t, f.request("PATCH", path, `{"enabled":true}`, "enable", `"3"`), 200)
	createWebhook(f, "https://hooks.example.invalid/other", oldSecret, "other")
	page := f.request("GET", "/api/v1/webhooks?limit=1", "", "", "")
	expectSourceStatus(t, page, 200)
	var list struct {
		Items []Webhook `json:"items"`
		Next  string    `json:"next_after"`
	}
	_ = json.Unmarshal(page.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Next == "" {
		t.Fatal("subscription page missing")
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/webhooks?after="+list.Next, "", "", ""), 200)
	for _, secret := range []string{oldSecret, newSecret} {
		for _, query := range []string{`SELECT count(*) FROM encrypted_secrets WHERE envelope::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM mutation_results WHERE response::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM audit_events WHERE row_to_json(audit_events)::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM outbox_events WHERE payload::text LIKE '%'||$1||'%'`} {
			if f.count(query, secret) != 0 {
				t.Fatal("plaintext persisted outside encrypted payload")
			}
		}
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action='webhook.secret'`) != 1 || f.count(`SELECT count(*) FROM audit_events WHERE action='secret.update'`) != 1 {
		t.Fatal("rotation replay duplicated audit")
	}
	if f.count(`SELECT count(*) FROM audit_events a LEFT JOIN outbox_events o ON o.audit_id=a.id WHERE o.id IS NULL`) != 0 {
		t.Fatal("audit missing transactional outbox")
	}
}
func TestWebhookAdministrationRollbackAndIsolation(t *testing.T) {
	f, keys := webhookHTTPFixture(t)
	ctx := context.Background()
	secret := strings.Repeat("sensitive-", 4)
	if _, err := f.db.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT webhook_fault CHECK(topic<>'webhook.create') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"endpoint": "https://hooks.example.invalid/events", "secret": secret})
	expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", string(body), "rollback", ""), 503)
	if f.count(`SELECT count(*) FROM encrypted_secrets`) != 0 || f.count(`SELECT count(*) FROM event_subscriptions`) != 0 || f.count(`SELECT count(*) FROM audit_events WHERE action='secret.create'`) != 0 {
		t.Fatal("create rollback leaked partial state")
	}
	if _, err := f.db.Exec(ctx, `ALTER TABLE outbox_events DROP CONSTRAINT webhook_fault`); err != nil {
		t.Fatal(err)
	}
	item := createWebhook(f, "https://hooks.example.invalid/events", secret, "rollback")
	if _, err := f.db.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT webhook_fault CHECK(topic<>'webhook.secret') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	next := strings.Repeat("next-sensitive-", 3)
	rotate, _ := json.Marshal(map[string]string{"secret": next})
	expectSourceStatus(t, f.request("PUT", "/api/v1/webhooks/"+item.ID+"/secret", string(rotate), "rotate-fail", `"1"`), 503)
	var ref string
	if err := f.db.QueryRow(ctx, `SELECT secret_ref FROM event_subscriptions WHERE id=$1`, item.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	record, plain, err := (secrets.Store{DB: f.db, Keys: keys}).Open(ctx, f.instance, ref, "webhook")
	if err != nil || record.Revision != 1 || string(plain) != secret {
		t.Fatal("failed rotate changed secret")
	}
	clear(plain)
	if f.count(`SELECT count(*) FROM event_subscriptions WHERE id=$1 AND revision=1`, item.ID) != 1 {
		t.Fatal("failed rotate advanced subscription")
	}
	var other, foreign string
	if err := f.db.QueryRow(ctx, `INSERT INTO instances(name) VALUES('foreign') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) VALUES($1,'https://foreign.example.invalid/','opaque') RETURNING id::text`, other).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("PATCH", "/api/v1/webhooks/"+foreign, `{"enabled":false}`, "cross", `"1"`), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/webhooks/"+foreign+"/secret", string(rotate), "cross", `"1"`), 404)
	expectSourceStatus(t, f.request("GET", "/api/v1/webhooks/"+foreign+"/deliveries", "", "", ""), 404)
	page := f.request("GET", "/api/v1/webhooks", "", "", "")
	expectSourceStatus(t, page, 200)
	if strings.Contains(page.Body.String(), foreign) {
		t.Fatal("foreign subscription visible")
	}
	tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("POST", "/api/v1/webhooks", string(body), "configure-only", "", tok.Secret), 403)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/webhooks", "", "", "", ""), 401)
}
func TestWebhookAdministrationDeliveries(t *testing.T) {
	f, _ := webhookHTTPFixture(t)
	ctx := context.Background()
	item := createWebhook(f, "https://hooks.example.invalid/", strings.Repeat("x", 32), "create")
	// Seed retained delivery rows, including an arbitrary historical diagnostic.
	rows, err := f.db.Query(ctx, `SELECT id FROM outbox_events ORDER BY id LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatal("missing fixture events")
	}
	for i, id := range ids {
		state := "pending"
		var failure any
		attempts := 0
		if i == 1 {
			failure = "http_failure"
			attempts = 1
		}
		if i == 2 {
			state = "dead"
			failure = "raw-sensitive-error"
			attempts = 8
		}
		if _, err := f.db.Exec(ctx, `INSERT INTO event_deliveries(subscription_id,event_id,state,attempts,last_failure) VALUES($1,$2,$3,$4,$5)`, item.ID, id, state, attempts, failure); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/v1/webhooks/" + item.ID
	page := f.request("GET", path+"/deliveries?limit=1", "", "", "")
	expectSourceStatus(t, page, 200)
	var list struct {
		Items []WebhookDelivery `json:"items"`
		Next  int64             `json:"next_after"`
	}
	_ = json.Unmarshal(page.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Next != ids[0] {
		t.Fatal("delivery pagination broken")
	}
	next := f.request("GET", path+fmt.Sprintf("/deliveries?after=%d", list.Next), "", "", "")
	expectSourceStatus(t, next, 200)
	failure := f.request("GET", path+"/failures", "", "", "")
	expectSourceStatus(t, failure, 200)
	_ = json.Unmarshal(failure.Body.Bytes(), &list)
	if len(list.Items) != 2 || strings.Contains(failure.Body.String(), "raw-sensitive-error") || !strings.Contains(failure.Body.String(), "delivery_failure") {
		t.Fatal("failures filter or sanitization broken")
	}
	dead := f.request("GET", path+"/failures?state=dead", "", "", "")
	expectSourceStatus(t, dead, 200)
	_ = json.Unmarshal(dead.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].State != "dead" {
		t.Fatal("state filter broken")
	}
	for _, query := range []string{"?limit=201", "?after=0", "?after=no", "?state=bad", "?state=", "?limit=1&limit=2", "?unknown=1"} {
		expectSourceStatus(t, f.request("GET", path+"/deliveries"+query, "", "", ""), 400)
	}
}
func TestWebhookAdministrationStrictAndConcurrent(t *testing.T) {
	f, _ := webhookHTTPFixture(t)
	secret := strings.Repeat("x", 32)
	for _, endpoint := range []string{"http://public.example/", "https://localhost/", "https://127.0.0.1/", "https://10.1.2.3/", "https://user:password@example.com/", "https://example.com/?token=x", "https://example.com/#fragment"} {
		body, _ := json.Marshal(map[string]string{"endpoint": endpoint, "secret": secret})
		expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", string(body), "bad", ""), 400)
	}
	for _, body := range []string{`{"endpoint":"https://example.com/","secret":"short"}`, `{"endpoint":"https://example.com/","secret":null}`, `{"endpoint":"https://example.com/","secret":"` + secret + `","Secret":"` + secret + `"}`, `{"endpoint":"https://example.com/","secret":"` + secret + `","secret":"` + secret + `"}`, `{"endpoint":"https://example.com/","secret":"` + strings.Repeat("s", 4097) + `"}`} {
		expectSourceStatus(t, f.request("POST", "/api/v1/webhooks", body, "bad", ""), 400)
	}
	item := createWebhook(f, "https://example.com/", secret, "create")
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for _, method := range []string{"PATCH", "PUT"} {
		wg.Add(1)
		go func(method string) {
			defer wg.Done()
			path := "/api/v1/webhooks/" + item.ID
			body := `{"enabled":false}`
			if method == "PUT" {
				path += "/secret"
				body = `{"secret":"` + strings.Repeat("y", 32) + `"}`
			}
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+f.credential)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("If-Match", `"1"`)
			r.Header.Set("Idempotency-Key", method)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			codes <- w.Code
		}(method)
	}
	wg.Wait()
	close(codes)
	wins := 0
	for code := range codes {
		if code == 200 {
			wins++
		} else if code != 412 {
			t.Fatalf("unexpected concurrent result %d", code)
		}
	}
	if wins != 1 {
		t.Fatal("concurrent config updates did not CAS")
	}
}
func TestWebhookAdministrationGeneratedClient(t *testing.T) {
	f, _ := webhookHTTPFixture(t)
	client, err := api.NewClientWithResponses("http://example.test", api.WithHTTPClient(&http.Client{Transport: identityContractTransport{f}}), api.WithRequestEditorFn(func(ctx context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+f.credential)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("🔐", 8)
	response, err := client.CreateWebhookWithResponse(context.Background(), &api.CreateWebhookParams{IdempotencyKey: "client"}, api.CreateWebhookJSONRequestBody{Endpoint: "https://example.com/events", Secret: &secret})
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON201 == nil {
		t.Fatalf("generated create %s", response.Body)
	}
	list, err := client.ListWebhooksWithResponse(context.Background(), &api.ListWebhooksParams{})
	if err != nil {
		t.Fatal(err)
	}
	if list.JSON200 == nil || len(list.JSON200.Items) != 1 {
		t.Fatalf("generated list %s", list.Body)
	}
}
