package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/denyaudit"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	handler http.Handler
	admin   string // full-scope token
	reader  string // read-only token
	secret  []byte
	deny    *denyaudit.Recorder
	clock   *time.Time // the denial recorder's clock; advance it to leave a rate-limit window
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL fixture required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ant_mcp_" + strings.ToLower(rand.Text())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func newFixture(t *testing.T) *fixture {
	pool := newPool(t)
	ctx := context.Background()
	boot, err := identity.Bootstrap(ctx, pool, "mcp-test")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(actions ...identity.Action) string {
		tok, err := (identity.Tokens{DB: pool}).Issue(ctx, boot.InstanceID, boot.PrincipalID, boot.PrincipalID, []identity.Scope{{Actions: actions}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return tok.Secret
	}
	mux := http.NewServeMux()
	codec := management.NewCursorCodec(make([]byte, 32))
	management.RegisterFeeds(mux, pool, codec)
	management.RegisterSources(mux, pool, codec)
	management.RegisterPublishPolicies(mux, pool)
	management.RegisterPackages(mux, pool, codec)
	management.RegisterIdentities(mux, pool)
	management.RegisterWebhooks(mux, pool, nil)
	management.RegisterOperations(mux, management.OpsDeps{Pool: pool, Codec: codec, Background: context.Background()})
	management.RegisterConsole(mux, pool, codec)
	management.RegisterFederation(mux, pool, nil)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	Register(mux, pool, nil)
	// Same outermost wrapper the server installs, so API-side denial audit rows
	// are produced exactly as in production.
	clock := time.Now()
	deny := &denyaudit.Recorder{DB: pool, InstanceID: boot.InstanceID, Now: func() time.Time { return clock }}
	return &fixture{t: t, pool: pool, handler: deny.Middleware(management.WithRequestID(mux)), deny: deny, clock: &clock,
		admin:  issue(identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin),
		reader: issue(identity.Read)}
}

func (f *fixture) do(method, path, body, token string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

var rpcID int

func (f *fixture) tool(token, name string, args map[string]any) rpcOut {
	f.t.Helper()
	rpcID++
	a, _ := json.Marshal(args)
	w := f.do("POST", "/mcp", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, rpcID, name, a), token)
	if w.Code != 200 {
		f.t.Fatalf("%s: HTTP %d %s", name, w.Code, w.Body.String())
	}
	return decode(f.t, w)
}

func text(o rpcOut) string {
	c := o.Result["content"].([]any)
	return c[0].(map[string]any)["text"].(string)
}

func (f *fixture) count(action string) int {
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action=$1`, action).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestMCPAuthentication(t *testing.T) {
	f := newFixture(t)
	for _, token := range []string{"", "bogus"} {
		w := f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, token)
		if w.Code != 401 {
			t.Fatalf("token %q: %d", token, w.Code)
		}
	}
	w := f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, f.reader)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestMCPAdminLifecycleAuditAndDestructiveConfirmation(t *testing.T) {
	f := newFixture(t)
	o := f.tool(f.admin, "create_feed", map[string]any{"slug": "alpha", "format": "raw", "idempotency_key": "k1"})
	if o.Error != nil || o.Result["isError"] != nil {
		t.Fatalf("%+v", o)
	}
	var feed management.Feed
	if err := json.Unmarshal([]byte(text(o)), &feed); err != nil || feed.ID == "" {
		t.Fatalf("%s", text(o))
	}
	// Replay with the same idempotency key is the API's replay, not a second feed.
	f.tool(f.admin, "create_feed", map[string]any{"slug": "alpha", "format": "raw", "idempotency_key": "k1"})
	if n := f.count("feed.create"); n != 1 {
		t.Fatalf("feed.create audit events=%d, want 1", n)
	}
	src := f.do("POST", "/api/v1/sources", `{"name":"local-a","kind":"local","format":"raw"}`, f.admin, "Idempotency-Key", "s1")
	var source management.Source
	if err := json.Unmarshal(src.Body.Bytes(), &source); err != nil || src.Code != 201 {
		t.Fatalf("%d %s", src.Code, src.Body.String())
	}

	o = f.tool(f.admin, "attach_source", map[string]any{"feed_id": feed.ID, "source_id": source.ID, "priority": 10})
	if o.Error != nil || o.Result["isError"] != nil {
		t.Fatalf("attach: %+v %v", o, o.Result)
	}
	o = f.tool(f.admin, "set_publish_policy", map[string]any{"feed_id": feed.ID, "default_source_id": source.ID, "rules": []any{}})
	if o.Error != nil || o.Result["isError"] != nil {
		t.Fatalf("policy: %v", o.Result)
	}
	if !strings.Contains(text(f.tool(f.admin, "get_publish_policy", map[string]any{"feed_id": feed.ID})), source.ID) {
		t.Fatal("policy not stored")
	}
	if !strings.Contains(text(f.tool(f.admin, "list_feed_sources", map[string]any{"feed_id": feed.ID})), source.ID) {
		t.Fatal("not attached")
	}
	if !strings.Contains(text(f.tool(f.admin, "list_feeds", nil)), "alpha") || !strings.Contains(text(f.tool(f.admin, "get_source", map[string]any{"source_id": source.ID})), "local-a") {
		t.Fatal("discovery")
	}
	if out := text(f.tool(f.admin, "get_federation_status", nil)); !strings.Contains(out, `"state":"none"`) || !strings.Contains(out, `"health":"disabled"`) {
		t.Fatalf("federation status: %s", out)
	}
	if out := text(f.tool(f.admin, "list_federation_members", nil)); !strings.Contains(out, `"items":[]`) {
		t.Fatalf("federation members: %s", out)
	}
	if !strings.Contains(text(f.tool(f.admin, "search_packages", map[string]any{"feed_id": feed.ID})), `"items"`) {
		t.Fatal("package search")
	}
	digest := "sha256:" + strings.Repeat("0", 64)
	if !strings.Contains(text(f.tool(f.admin, "lookup_checksum", map[string]any{"feed_id": feed.ID, "digest": digest})), `"items"`) {
		t.Fatal("checksum lookup")
	}
	if !strings.Contains(text(f.tool(f.admin, "get_health", nil)), `"ready":{"ok":true`) {
		t.Fatal("health")
	}

	src2 := f.do("POST", "/api/v1/sources", `{"name":"local-b","kind":"local","format":"raw"}`, f.admin, "Idempotency-Key", "s2")
	var second management.Source
	if err := json.Unmarshal(src2.Body.Bytes(), &second); err != nil || src2.Code != 201 {
		t.Fatalf("%d %s", src2.Code, src2.Body.String())
	}
	if o = f.tool(f.admin, "attach_source", map[string]any{"feed_id": feed.ID, "source_id": second.ID, "priority": 20}); o.Result["isError"] != nil {
		t.Fatalf("attach second: %v", o.Result)
	}
	// The default publish target cannot be detached; the API's conflict is surfaced.
	if o = f.tool(f.admin, "detach_source", map[string]any{"feed_id": feed.ID, "source_id": source.ID, "confirm": true}); o.Result["isError"] != true {
		t.Fatalf("detached the default publish source: %v", o.Result)
	}
	source = second
	// Destructive: refused without confirm, nothing changes, still audited.
	before := f.count("feed.source.detach")
	o = f.tool(f.admin, "detach_source", map[string]any{"feed_id": feed.ID, "source_id": source.ID})
	if o.Error == nil || o.Error.Code != codeInvalidParams {
		t.Fatalf("detach without confirm: %+v", o)
	}
	if !strings.Contains(text(f.tool(f.admin, "list_feed_sources", map[string]any{"feed_id": feed.ID})), source.ID) || f.count("feed.source.detach") != before {
		t.Fatal("unconfirmed detach changed state")
	}
	o = f.tool(f.admin, "detach_source", map[string]any{"feed_id": feed.ID, "source_id": source.ID, "confirm": true})
	if o.Error != nil || o.Result["isError"] != nil {
		t.Fatalf("detach: %v", o.Result)
	}
	if f.count("feed.source.detach") != before+1 {
		t.Fatal("detach not audited by API path")
	}

	// MCP-level audit rows: one per call, with outcome, no arguments.
	var calls, confirmations int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE details->>'outcome'='confirmation_required') FROM audit_events WHERE action='mcp.tool.call'`).Scan(&calls, &confirmations)
	if calls < 12 || confirmations != 1 {
		t.Fatalf("mcp audit calls=%d confirmations=%d", calls, confirmations)
	}
	var leaked bool
	_ = f.pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='mcp.tool.call' AND details::text LIKE '%'||$1||'%')`, f.admin).Scan(&leaked)
	if leaked {
		t.Fatal("token in audit details")
	}
}

func TestMCPAuthorizationParityWithAPI(t *testing.T) {
	f := newFixture(t)
	// Read-only token: the API denies feed creation; MCP must surface the same denial and create nothing.
	api := f.do("POST", "/api/v1/feeds", `{"slug":"nope","format":"raw"}`, f.reader, "Idempotency-Key", "p1")
	if api.Code < 400 {
		t.Fatalf("API unexpectedly allowed: %d", api.Code)
	}
	var apiErr struct{ Code string }
	_ = json.Unmarshal(api.Body.Bytes(), &apiErr)
	o := f.tool(f.reader, "create_feed", map[string]any{"slug": "nope", "format": "raw"})
	if o.Result["isError"] != true || !strings.Contains(text(o), apiErr.Code) || !strings.Contains(text(o), fmt.Sprintf("status %d", api.Code)) {
		t.Fatalf("parity: api=%d %s mcp=%v", api.Code, api.Body.String(), o.Result)
	}
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM feeds WHERE slug='nope'`).Scan(&n)
	if n != 0 {
		t.Fatal("denied call created feed")
	}
	// Admin tools denied the same way for destructive confirmed calls.
	o = f.tool(f.reader, "disable_webhook", map[string]any{"webhook_id": "33333333-3333-3333-3333-333333333333", "confirm": true})
	if o.Result["isError"] != true {
		t.Fatalf("reader disabled webhook: %v", o.Result)
	}
	// Denied call is audited at the MCP level with outcome=error.
	var denied int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='mcp.tool.call' AND details->>'name'='create_feed' AND details->>'outcome'='error'`).Scan(&denied)
	if denied != 1 {
		t.Fatalf("denied audit=%d", denied)
	}
	// Reads work for the reader and match the API response body.
	apiList := f.do("GET", "/api/v1/feeds", "", f.reader)
	mcpList := f.tool(f.reader, "list_feeds", nil)
	if strings.TrimSpace(apiList.Body.String()) != text(mcpList) {
		t.Fatalf("list parity %q vs %q", apiList.Body.String(), text(mcpList))
	}
}

func TestMCPResources(t *testing.T) {
	f := newFixture(t)
	c := f.tool(f.admin, "create_feed", map[string]any{"slug": "res", "format": "raw"})
	var feed management.Feed
	_ = json.Unmarshal([]byte(text(c)), &feed)
	w := f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"ant://feeds/`+feed.ID+`"}}`, f.reader)
	o := decode(t, w)
	if o.Error != nil || !strings.Contains(w.Body.String(), "res") {
		t.Fatalf("%s", w.Body.String())
	}
	w = f.do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"ant://feeds"}}`, f.reader)
	if decode(t, w).Error != nil {
		t.Fatalf("%s", w.Body.String())
	}
	if f.count("mcp.resource.read") != 2 {
		t.Fatal("resource reads not audited")
	}
}
