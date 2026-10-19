package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/denyaudit"
)

const (
	someID  = "44444444-4444-4444-4444-444444444444"
	otherID = "55555555-5555-5555-5555-555555555555"
)

// parityCase pairs an MCP tool call with the management API request it maps to.
type parityCase struct {
	tool   string
	args   map[string]any
	method string
	path   string
	body   string
}

func parityCases() []parityCase {
	return []parityCase{
		{"create_feed", map[string]any{"slug": "denied", "format": "raw"}, "POST", "/api/v1/feeds", `{"slug":"denied","format":"raw"}`},
		{"revoke_service_token", map[string]any{"token_id": someID, "confirm": true}, "DELETE", "/api/v1/tokens/" + someID, ""},
		{"start_retention_run", map[string]any{"dry_run": true, "confirm": true}, "POST", "/api/v1/retention/runs", `{"dry_run":true}`},
		{"start_storage_job", map[string]any{"action": "cleanup", "dry_run": true, "confirm": true}, "POST", "/api/v1/storage/migrations", `{"action":"cleanup","dry_run":true}`},
		{"storage_job_action", map[string]any{"storage_job_id": someID, "action": "pause"}, "POST", "/api/v1/storage/migrations/" + someID + "/pause", ""},
		{"purge_source_cache", map[string]any{"source_id": someID, "confirm": true}, "DELETE", "/api/v1/sources/" + someID + "/cache", ""},
		{"delete_package_version", map[string]any{"feed_id": someID, "package_id": otherID, "version": "1", "confirm": true}, "DELETE", "/api/v1/feeds/" + someID + "/packages/" + otherID + "/versions/1", ""},
		{"submit_security_result", map[string]any{"asset_id": someID, "scanner": "trivy", "status": "passed"}, "POST", "/api/v1/assets/" + someID + "/security-results", `{"scanner":"trivy","status":"passed"}`},
		{"list_service_tokens", nil, "GET", "/api/v1/tokens", ""},
		{"query_audit", nil, "GET", "/api/v1/audit", ""},
		{"get_settings", nil, "GET", "/api/v1/settings", ""},
		{"list_retention_runs", nil, "GET", "/api/v1/retention/runs", ""},
		{"list_integrity_findings", nil, "GET", "/api/v1/retention/integrity-findings", ""},
		{"get_storage_usage", nil, "GET", "/api/v1/storage/usage", ""},
		{"get_diagnostics", nil, "GET", "/api/v1/diagnostics", ""},
		{"list_identity_bindings", nil, "GET", "/api/v1/identity-bindings", ""},
		{"list_feed_stats", nil, "GET", "/api/v1/stats/feeds", ""},
		{"publish_federation_policy", map[string]any{"base_generation": 0, "confirm": true}, "POST", "/api/v1/federation/policy/publish", `{"base_generation":0,"confirm":true}`},
		{"list_federated_principals", nil, "GET", "/api/v1/federation/principals", ""},
		{"start_feed_transfer", map[string]any{"feed_key": "lab/x", "to_member_id": someID, "confirm": true}, "POST", "/api/v1/federation/feeds/lab/x/transfer", `{"confirm":true,"to_member_id":"` + someID + `"}`},
		{"set_replication_rule", map[string]any{"feed_key": "lab/x", "targets": map[string]any{"members": []any{someID}}}, "POST", "/api/v1/federation/replication/rules", `{"feed_key":"lab/x","targets":{"members":["` + someID + `"]}}`},
		{"adopt_replication_job", map[string]any{"job_id": someID, "rule_id": otherID}, "POST", "/api/v1/federation/replication/adopt", `{"job_id":"` + someID + `","rule_id":"` + otherID + `"}`},
	}
}

// TestMCPAndAPIDenyAndAuditIdentically asserts, for a representative set of
// management operations, that a read-only token gets the same denial through
// the API and through MCP, that nothing is changed, and that the MCP call is
// audited with its failing outcome.
func TestMCPAndAPIDenyAndAuditIdentically(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i, tc := range parityCases() {
		// Leave the denial recorder's per-principal window so each case is admitted.
		*f.clock = f.clock.Add(2 * time.Minute)
		hdr := []string{}
		if tc.method != "GET" {
			hdr = []string{"Idempotency-Key", fmt.Sprintf("parity-%d", i)}
		}
		api := f.do(tc.method, tc.path, tc.body, f.reader, hdr...)
		o := f.tool(f.reader, tc.tool, tc.args)
		var apiErr struct{ Code string }
		_ = json.Unmarshal(api.Body.Bytes(), &apiErr)
		readOnly := tc.method == "GET"
		if api.Code < 400 {
			// Read-only operations the reader may legitimately perform must match bytes.
			if !readOnly || strings.TrimSpace(api.Body.String()) != text(o) {
				t.Errorf("%s: API %d, mcp %v", tc.tool, api.Code, o.Result)
			}
			continue
		}
		if o.Result["isError"] != true || !strings.Contains(text(o), apiErr.Code+":") || !strings.Contains(text(o), fmt.Sprintf("status %d", api.Code)) {
			t.Errorf("%s: parity broken: api=%d %s mcp=%v", tc.tool, api.Code, api.Body.String(), o.Result)
		}
		// The API request itself leaves a denied audit row (the in-process MCP
		// dispatch does not pass through the HTTP layer, so it adds none).
		f.deny.Wait()
		// Only 401/403 are authorization denials; a 404 for an id that does not
		// exist (or is hidden) is deliberately indistinguishable and not audited.
		want := 0
		if api.Code == 401 || api.Code == 403 {
			want = 1
		}
		if n := f.apiDenials(tc.method, tc.path, api.Code); n != want {
			t.Errorf("%s: API denial audit rows=%d (status %d), want %d", tc.tool, n, api.Code, want)
		}
		var errs int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='mcp.tool.call' AND details->>'name'=$1 AND details->>'outcome'='error' AND (details->>'status')::int=$2`, tc.tool, api.Code).Scan(&errs)
		if errs != 1 {
			t.Errorf("%s: MCP denial audit events=%d, want 1", tc.tool, errs)
		}
	}
	var feeds int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM feeds`).Scan(&feeds)
	if feeds != 0 {
		t.Fatalf("denied calls created %d feeds", feeds)
	}
	var mcpSurface int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action=$1 AND details->>'surface'='mcp'`, denyaudit.Action).Scan(&mcpSurface)
	if mcpSurface != 0 {
		t.Fatalf("MCP tool denials produced %d HTTP-layer denial rows; MCP audits them itself", mcpSurface)
	}
}

// apiDenials counts denied audit rows the HTTP layer wrote for one request shape.
func (f *fixture) apiDenials(method, path string, status int) int {
	f.t.Helper()
	var n int
	err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action=$1 AND result='denied' AND details->>'surface'='management' AND details->>'method'=$2 AND details->>'path'=$3 AND (details->>'status')::int=$4 AND principal_id IS NOT NULL`, denyaudit.Action, method, path, status).Scan(&n)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// TestAPIDenialAuditIsRateLimited: a principal hammering a forbidden endpoint
// leaves one row per window, with the suppressed count carried on the next one.
func TestAPIDenialAuditIsRateLimited(t *testing.T) {
	f := newFixture(t)
	for range 25 {
		if w := f.do("GET", "/api/v1/tokens", "", f.reader); w.Code != 403 {
			t.Fatalf("status %d", w.Code)
		}
	}
	f.deny.Wait()
	if n := f.apiDenials("GET", "/api/v1/tokens", 403); n != 1 {
		t.Fatalf("rows within one window=%d, want 1", n)
	}
	*f.clock = f.clock.Add(2 * time.Minute)
	f.do("GET", "/api/v1/tokens", "", f.reader)
	f.deny.Wait()
	var suppressed int
	_ = f.pool.QueryRow(context.Background(), `SELECT COALESCE(max((details->>'suppressed_since_last')::int),0) FROM audit_events WHERE action=$1`, denyaudit.Action).Scan(&suppressed)
	if f.apiDenials("GET", "/api/v1/tokens", 403) != 2 || suppressed != 24 {
		t.Fatalf("after window: rows=%d suppressed=%d", f.apiDenials("GET", "/api/v1/tokens", 403), suppressed)
	}
}

// TestMCPAndAPIAuditAllowedMutation: both transports emit the domain audit
// event for a permitted mutation; MCP adds its own call event.
func TestMCPAndAPIAuditAllowedMutation(t *testing.T) {
	f := newFixture(t)
	if w := f.do("POST", "/api/v1/feeds", `{"slug":"via-api","format":"raw"}`, f.admin, "Idempotency-Key", "a1"); w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if o := f.tool(f.admin, "create_feed", map[string]any{"slug": "via-mcp", "format": "raw"}); o.Result["isError"] != nil {
		t.Fatalf("%v", o.Result)
	}
	if n := f.count("feed.create"); n != 2 {
		t.Fatalf("feed.create events=%d, want one per transport", n)
	}
	if n := f.count("mcp.tool.call"); n != 1 {
		t.Fatalf("mcp.tool.call events=%d", n)
	}
	// Destructive tools without confirm are refused and audited, never executed.
	o := f.tool(f.admin, "revoke_service_token", map[string]any{"token_id": someID})
	if o.Error == nil {
		t.Fatalf("revoke without confirm accepted: %+v", o)
	}
	// Mutating MCP calls reach the API with an Idempotency-Key and replay once.
	args := map[string]any{"slug": "replayed", "format": "raw", "idempotency_key": "same"}
	f.tool(f.admin, "create_feed", args)
	f.tool(f.admin, "create_feed", args)
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM feeds WHERE slug='replayed'`).Scan(&n)
	if n != 1 {
		t.Fatalf("replayed feeds=%d", n)
	}
}

func TestOpsToolsAreDescribedAndDestructiveOnesConfirm(t *testing.T) {
	for _, tl := range catalog() {
		d := tl.descriptor()
		ann := d["annotations"].(map[string]any)
		if !tl.readOnly {
			if _, ok := ann["destructiveHint"]; !ok {
				t.Errorf("%s missing destructiveHint", tl.name)
			}
		}
		if tl.destructive {
			found := false
			for _, p := range tl.allParams() {
				found = found || (p.name == "confirm" && p.required)
			}
			if !found {
				t.Errorf("%s destructive without required confirm", tl.name)
			}
		}
	}
	names := map[string]bool{}
	for _, tl := range catalog() {
		if names[tl.name] {
			t.Errorf("duplicate tool %s", tl.name)
		}
		names[tl.name] = true
	}
	for _, want := range []string{"list_replication_jobs", "get_replication_status", "run_replication_job", "preview_replication_deletions", "list_retention_policies", "list_integrity_findings", "get_storage_usage", "get_storage_migration", "list_service_tokens", "revoke_service_token", "list_identity_bindings", "query_audit", "purge_source_cache", "resolve_coordinate", "test_publish_policy", "list_feed_stats", "list_source_health", "get_traffic_metrics", "get_settings", "delete_package_version", "submit_security_result"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}
