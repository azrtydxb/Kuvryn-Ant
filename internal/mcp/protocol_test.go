package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

type fakeAPI struct {
	mu    sync.Mutex
	calls []*http.Request
	reply func(r *http.Request) (int, string)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.mu.Unlock()
	code, body := 200, `{}`
	if f.reply != nil {
		code, body = f.reply(r)
	}
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

type harness struct {
	api    *fakeAPI
	events []AuditEvent
	audErr error
	srv    *Server
}

func newHarness() *harness {
	h := &harness{api: &fakeAPI{}}
	h.srv = New(h.api, func(_ context.Context, _ identity.Principal, e AuditEvent) error {
		h.events = append(h.events, e)
		return h.audErr
	}, nil)
	return h
}

func (h *harness) post(t *testing.T, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{InstanceID: "i", PrincipalID: "p"}))
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	return w
}

type rpcOut struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcError       `json:"error"`
}

func decode(t *testing.T, w *httptest.ResponseRecorder) rpcOut {
	t.Helper()
	var o rpcOut
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatalf("bad json %q: %v", w.Body.String(), err)
	}
	return o
}

func TestInitializeAndListing(t *testing.T) {
	h := newHarness()
	o := decode(t, h.post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	if o.Error != nil || o.Result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("%+v", o)
	}
	if decode(t, h.post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999"}}`)).Result["protocolVersion"] != latestProtocol {
		t.Fatal("version negotiation")
	}
	if w := h.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); w.Code != 202 || w.Body.Len() != 0 {
		t.Fatalf("notification %d", w.Code)
	}
	list := decode(t, h.post(t, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`))
	tools := list.Result["tools"].([]any)
	names := map[string]map[string]any{}
	for _, x := range tools {
		m := x.(map[string]any)
		names[m["name"].(string)] = m
	}
	for _, n := range []string{"list_feeds", "get_feed", "list_sources", "get_source", "search_packages", "list_package_versions", "list_package_assets", "lookup_checksum", "get_health", "get_cache_insight", "create_feed", "attach_source", "set_publish_policy", "rotate_webhook_secret", "detach_source", "disable_webhook", "delete_package_version", "get_federation_status", "list_federation_members", "get_federation_policy", "publish_federation_policy", "check_federation_access", "list_federated_principals", "federated_search", "federated_lookup_checksum", "get_federation_topology", "list_replication_rules", "get_replication_rule_compliance", "get_federation_replication_status", "set_replication_rule", "adopt_replication_job", "start_feed_transfer", "explain_route", "get_federation_authority", "federated_audit", "handover_federation_authority"} {
		if names[n] == nil {
			t.Fatalf("missing tool %s", n)
		}
	}
	for n, m := range names {
		ann := m["annotations"].(map[string]any)
		destructive := map[string]bool{"detach_source": true, "disable_webhook": true, "delete_package_version": true, "start_retention_run": true, "start_storage_job": true, "revoke_service_token": true, "purge_source_cache": true, "delete_source": true, "publish_federation_policy": true, "start_feed_transfer": true, "handover_federation_authority": true}[n]
		if got, _ := ann["destructiveHint"].(bool); got != destructive && !ann["readOnlyHint"].(bool) {
			t.Fatalf("%s destructiveHint=%v", n, got)
		}
		if destructive {
			req := m["inputSchema"].(map[string]any)["required"].([]any)
			found := false
			for _, r := range req {
				found = found || r == "confirm"
			}
			if !found {
				t.Fatalf("%s schema lacks required confirm", n)
			}
		}
	}
	res := decode(t, h.post(t, `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`))
	if len(res.Result["resources"].([]any)) == 0 {
		t.Fatal("no resources")
	}
	if decode(t, h.post(t, `{"jsonrpc":"2.0","id":3,"method":"resources/templates/list"}`)).Result["resourceTemplates"] == nil {
		t.Fatal("no templates")
	}
	if decode(t, h.post(t, `{"jsonrpc":"2.0","id":4,"method":"ping"}`)).Error != nil {
		t.Fatal("ping")
	}
}

func TestTransportErrors(t *testing.T) {
	h := newHarness()
	for body, code := range map[string]int{
		`{not json`: codeParse,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`:                                                                   codeInvalidRequest,
		`{"jsonrpc":"2.0","id":{},"method":"ping"}`:                                                                  codeInvalidRequest,
		`{"jsonrpc":"2.0","id":1,"method":"nope"}`:                                                                   codeMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`:                                    codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_feed","arguments":{}}}`:                 codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_feed","arguments":{"feed_id":"../x"}}}`: codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feeds","arguments":{"bogus":1}}}`:      codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"file:///etc/passwd"}}`:                   codeInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"ant://feeds/../../x"}}`:                  codeInvalidParams,
	} {
		w := h.post(t, body)
		if o := decode(t, w); o.Error == nil || o.Error.Code != code {
			t.Fatalf("%s: got %s", body, w.Body.String())
		}
	}
	if len(h.api.calls) != 0 {
		t.Fatal("invalid requests reached the API")
	}
	if w := h.post(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example"); w.Code != 403 {
		t.Fatalf("origin %d", w.Code)
	}
	if w := h.post(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "1"); w.Code != 400 {
		t.Fatalf("version %d", w.Code)
	}
	if w := h.post(t, `{}`, "Content-Type", "text/plain"); w.Code != 415 {
		t.Fatalf("content type %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/mcp", nil)
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("GET %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthenticated %d", w.Code)
	}
}

func TestBatch(t *testing.T) {
	h := newHarness()
	w := h.post(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`)
	var out []rpcOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("%s", w.Body.String())
	}
}

func TestDestructiveRequiresConfirm(t *testing.T) {
	h := newHarness()
	const feed, src = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	args := `{"feed_id":"` + feed + `","source_id":"` + src + `","revision":3`
	w := h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"detach_source","arguments":`+args+`}}}`)
	if o := decode(t, w); o.Error == nil || o.Error.Code != codeInvalidParams {
		t.Fatalf("missing confirm accepted: %s", w.Body.String())
	}
	w = h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"detach_source","arguments":`+args+`,"confirm":"yes"}}}`)
	if o := decode(t, w); o.Error == nil {
		t.Fatal("non-boolean confirm accepted")
	}
	w = h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"detach_source","arguments":`+args+`,"confirm":false}}}`)
	if o := decode(t, w); o.Error == nil {
		t.Fatal("confirm=false accepted")
	}
	if len(h.api.calls) != 0 {
		t.Fatal("API reached without confirmation")
	}
	if got := h.events[len(h.events)-1]; got.Outcome != "confirmation_required" || !got.Destructive {
		t.Fatalf("audit %+v", got)
	}
	w = h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"detach_source","arguments":`+args+`,"confirm":true}}}`)
	if o := decode(t, w); o.Error != nil || o.Result["isError"] != nil {
		t.Fatalf("confirmed call failed: %s", w.Body.String())
	}
	req := h.api.calls[0]
	if req.Method != "DELETE" || req.URL.Path != "/api/v1/feeds/"+feed+"/sources/"+src || req.Header.Get("If-Match") != `"3"` || req.Header.Get("Idempotency-Key") == "" {
		t.Fatalf("bad dispatch %s %s %v", req.Method, req.URL.Path, req.Header)
	}
}

func TestAPIErrorsAreSanitisedAndAuditFailsClosed(t *testing.T) {
	h := newHarness()
	h.api.reply = func(r *http.Request) (int, string) {
		return 500, `{"message":"pq: password authentication failed for user ant at 10.0.0.1","stack":"secret"}`
	}
	w := h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feeds"}}`)
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "10.0.0.1") {
		t.Fatalf("leak: %s", w.Body.String())
	}
	o := decode(t, w)
	if o.Result["isError"] != true {
		t.Fatalf("expected isError: %s", w.Body.String())
	}
	h.api.reply = func(r *http.Request) (int, string) {
		return 403, `{"code":"forbidden","message":"permission denied","request_id":"abc"}`
	}
	w = h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feeds"}}`)
	if !strings.Contains(w.Body.String(), "forbidden: permission denied") {
		t.Fatalf("denial lost: %s", w.Body.String())
	}
	if e := h.events[len(h.events)-1]; e.Outcome != "error" || e.Status != 403 {
		t.Fatalf("audit %+v", e)
	}
	h.audErr = errors.New("db down: secret-dsn")
	h.api.reply = nil
	w = h.post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feeds"}}`)
	if o := decode(t, w); o.Error == nil || o.Error.Code != codeInternal || strings.Contains(w.Body.String(), "secret-dsn") {
		t.Fatalf("audit failure not closed: %s", w.Body.String())
	}
}

func TestAuthorizationForwarded(t *testing.T) {
	h := newHarness()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_feeds","arguments":{"limit":5,"prefix":"a b"}}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer tok")
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{InstanceID: "i", PrincipalID: "p"}))
	h.srv.ServeHTTP(httptest.NewRecorder(), r)
	got := h.api.calls[0]
	if got.Header.Get("Authorization") != "Bearer tok" || got.URL.Query().Get("limit") != "5" || got.URL.Query().Get("prefix") != "a b" {
		t.Fatalf("%v %s", got.Header, got.URL)
	}
}
