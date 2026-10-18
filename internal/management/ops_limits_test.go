package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/limits"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

func newCfgFixture(t *testing.T) *opsFixture {
	t.Helper()
	o := newOpsFixture(t)
	root := bytes.Repeat([]byte{9}, 32)
	keys, err := secrets.New("v1", map[string][]byte{"v1": root})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterRemoteSources(mux, o.db, keys, nil, root)
	RegisterPublishPolicies(mux, o.db)
	mux.Handle("/", o.handler)
	o.handler = WithRequestID(mux)
	return o
}

func (o *opsFixture) feedRevision(id string) string {
	w := o.get("/api/v1/feeds/" + id)
	expectSourceStatus(o.t, w, 200)
	return strconv.Quote(strconv.FormatInt(decodeJSON[Feed](o.t, w).Revision, 10))
}

func (o *opsFixture) attach(feed Feed, source Source, priority int) {
	o.t.Helper()
	expectSourceStatus(o.t, o.request("PUT", "/api/v1/feeds/"+feed.ID+"/sources/"+source.ID, fmt.Sprintf(`{"priority":%d}`, priority), "attach-"+feed.ID+source.ID, o.feedRevision(feed.ID)), 200)
}

func message(w *httptest.ResponseRecorder) string {
	var e struct{ Message string }
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Message
}

func TestLimitsConfigAPIAndRateLimitMiddleware(t *testing.T) {
	o := newCfgFixture(t)
	feed := o.feed("limited-feed", "raw")
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/limits", "", "", "", o.limited), 403)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/limits", "", "", "", ""), 401)
	expectSourceStatus(t, o.requestAs("PUT", "/api/v1/limits", `{"rate_limits":[],"quotas":[]}`, "k", `"1"`, o.limited), 403)
	initial := o.get("/api/v1/limits")
	expectSourceStatus(t, initial, 200)
	if cfg := decodeJSON[LimitsConfig](t, initial); cfg.Revision != 1 || len(cfg.RateLimits) != 0 || len(cfg.Quotas) != 0 {
		t.Fatalf("initial config: %+v", cfg)
	}
	bad := o.request("PUT", "/api/v1/limits", fmt.Sprintf(`{"rate_limits":[{"scope":"feed","subject":%q,"requests_per_minute":0,"burst":1}],"quotas":[]}`, feed.ID), "bad-1", `"1"`)
	expectSourceStatus(t, bad, 400)
	if !strings.Contains(message(bad), "requests_per_minute") {
		t.Fatalf("unclear validation message: %s", message(bad))
	}
	unknown := o.request("PUT", "/api/v1/limits", `{"rate_limits":[{"scope":"feed","subject":"99999999-9999-4999-8999-999999999999","requests_per_minute":5,"burst":1}],"quotas":[]}`, "bad-2", `"1"`)
	expectSourceStatus(t, unknown, 400)
	if !strings.Contains(message(unknown), "unknown feed") {
		t.Fatalf("unknown feed message: %s", message(unknown))
	}
	expectSourceStatus(t, o.request("PUT", "/api/v1/limits", `{"rate_limits":[],"quotas":[]}`, "no-match", ""), 428)
	body := fmt.Sprintf(`{"rate_limits":[{"scope":"principal","subject":%q,"requests_per_minute":60,"burst":2},{"scope":"feed","subject":%q,"requests_per_minute":60,"burst":100},{"scope":"site","subject":"lab","cidrs":["10.0.0.0/8"],"requests_per_minute":600,"burst":50}],"quotas":[{"scope":"feed","feed_id":%q,"max_bytes":1000,"max_packages":5}]}`, o.principal, feed.ID, feed.ID)
	put := o.request("PUT", "/api/v1/limits", body, "limits-1", `"1"`)
	expectSourceStatus(t, put, 200)
	if put.Header().Get("ETag") != `"2"` {
		t.Fatalf("etag %q", put.Header().Get("ETag"))
	}
	expectSourceStatus(t, o.request("PUT", "/api/v1/limits", body, "limits-1", `"1"`), 200) // idempotent replay
	expectSourceStatus(t, o.request("PUT", "/api/v1/limits", body, "limits-2", `"1"`), 412)
	if got := decodeJSON[LimitsConfig](t, o.get("/api/v1/limits")); got.Revision != 2 || len(got.RateLimits) != 3 || got.Quotas[0].MaxBytes == nil || *got.Quotas[0].MaxBytes != 1000 {
		t.Fatalf("stored config: %+v", got)
	}
	if o.count(`SELECT count(*) FROM audit_events WHERE action='limits.put'`) != 1 {
		t.Fatal("limits change not audited exactly once")
	}
	usage := o.get("/api/v1/limits/usage")
	expectSourceStatus(t, usage, 200)
	if u := decodeJSON[LimitsUsage](t, usage); len(u.Quotas) != 1 || u.Quotas[0].BytesUsed != 0 || u.Quotas[0].MaxPackages == nil {
		t.Fatalf("usage: %+v", u)
	}

	// The middleware enforces the stored principal limit with the standard 429.
	limiter := &limits.Limiter{DB: o.db, InstanceID: o.instance, Resolve: limits.TokenResolver(identity.Tokens{DB: o.db}), APIError: Error}
	if err := limiter.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := limiter.Middleware(o.handler)
	call := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://example.test/api/v1/limits", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	expectSourceStatus(t, call(o.credential), 200)
	expectSourceStatus(t, call(o.credential), 200)
	throttled := call(o.credential)
	expectSourceStatus(t, throttled, 429)
	if secs, err := strconv.Atoi(throttled.Header().Get("Retry-After")); err != nil || secs < 1 {
		t.Fatalf("Retry-After=%q", throttled.Header().Get("Retry-After"))
	}
	var e struct{ Code, RequestID string }
	if json.Unmarshal(throttled.Body.Bytes(), &e) != nil || e.Code != "rate_limited" {
		t.Fatalf("429 body: %s", throttled.Body.String())
	}
	if call(o.limited).Code == 429 {
		t.Fatal("another principal was throttled")
	}
}

func TestPolicyTemplatesEffectiveViewValidationAndAudit(t *testing.T) {
	o := newCfgFixture(t)
	feed := o.feed("tmpl-feed", "raw")
	local := o.source("tmpl-local", "local", "raw")
	stray := o.source("tmpl-stray", "local", "raw")
	o.attach(feed, local, 1)
	create := func(key, body string) *httptest.ResponseRecorder {
		return o.request("POST", "/api/v1/policy-templates", body, key, "")
	}
	publishBody := func(name, extends, def, rules string) string {
		ext := ""
		if extends != "" {
			ext = fmt.Sprintf(`"extends_id":%q,`, extends)
		}
		return fmt.Sprintf(`{"name":%q,"kind":"publish",%s"description":"shared","publish":{"default_source_id":%q,"rules":[%s]}}`, name, ext, def, rules)
	}
	rule := func(id string, priority int, ns, target string, deny bool) string {
		t := ""
		if target != "" {
			t = fmt.Sprintf(`"target_source_id":%q,`, target)
		}
		return fmt.Sprintf(`{"id":%q,"priority":%d,"selector":{"namespace":%q},%s"deny":%t}`, id, priority, ns, t, deny)
	}
	baseResp := create("t-base", publishBody("base", "", local.ID, rule("deny-secret", 10, "secret", "", true)+","+rule("main", 1, "", local.ID, false)))
	expectSourceStatus(t, baseResp, 201)
	base := decodeJSON[PolicyTemplate](t, baseResp)
	childResp := create("t-child", publishBody("child", base.ID, "", rule("main", 5, "", local.ID, false)))
	expectSourceStatus(t, childResp, 201)
	child := decodeJSON[PolicyTemplate](t, childResp)
	if child.ExtendsID != base.ID {
		t.Fatal("extends not stored")
	}
	readResp := create("t-read", `{"name":"rd","kind":"read","read":{"local_first":true,"fallback_on_not_found":false}}`)
	expectSourceStatus(t, readResp, 201)
	read := decodeJSON[PolicyTemplate](t, readResp)
	list := o.get("/api/v1/policy-templates")
	expectSourceStatus(t, list, 200)
	if items := decodeJSON[struct{ Items []PolicyTemplate }](t, list).Items; len(items) != 3 {
		t.Fatalf("templates listed: %d", len(items))
	}
	dup := create("t-dup", publishBody("base", "", "", ""))
	expectSourceStatus(t, dup, 409)
	if !strings.Contains(message(dup), "already exists") {
		t.Fatalf("duplicate message: %s", message(dup))
	}
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/policy-templates", publishBody("denied", "", "", ""), "t-denied", "", o.limited), 403)

	// Binding applies the templates to the feed and bumps its revision.
	bindPath := "/api/v1/feeds/" + feed.ID + "/policy-templates"
	before := o.feedRevision(feed.ID)
	bind := o.request("PUT", bindPath, fmt.Sprintf(`{"publish_template_id":%q,"read_template_id":%q}`, child.ID, read.ID), "bind-1", before)
	expectSourceStatus(t, bind, 200)
	if bind.Header().Get("ETag") == before {
		t.Fatal("binding did not bump the feed revision")
	}
	effective := func() EffectivePolicy {
		w := o.get("/api/v1/feeds/" + feed.ID + "/effective-policy")
		expectSourceStatus(t, w, 200)
		return decodeJSON[EffectivePolicy](t, w)
	}
	byID := func(e EffectivePolicy, id string) EffectivePublishRule {
		for _, r := range e.Rules {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("rule %s missing from effective policy", id)
		return EffectivePublishRule{}
	}
	e := effective()
	if strings.Join(e.PublishTemplateChain, ",") != "base,child" || e.DefaultSourceID != local.ID || e.DefaultOrigin != "template:base" ||
		byID(e, "deny-secret").Origin != "template:base" || byID(e, "main").Origin != "template:child" || byID(e, "main").Priority != 5 ||
		e.Read.LocalFirst == nil || !*e.Read.LocalFirst || e.ReadOrigins["local_first"] != "template:rd" {
		t.Fatalf("effective view: %+v", e)
	}

	// The feed's own policy is applied last and is attributed to the feed.
	own := o.request("PUT", "/api/v1/feeds/"+feed.ID+"/publish-policy", `{"default_source_id":"","rules":[`+rule("main", 7, "", local.ID, false)+`]}`, "own-1", o.feedRevision(feed.ID))
	expectSourceStatus(t, own, 200)
	if r := byID(effective(), "main"); r.Origin != "feed" || r.Priority != 7 {
		t.Fatalf("own policy not layered over template: %+v", r)
	}

	// The runtime router uses the same effective configuration.
	router, err := (artifact.Policies{DB: o.db, InstanceID: o.instance}).Load(context.Background(), feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	denied, _ := router.Resolve(routing.Request{Namespace: "secret", Package: "x", Path: "/a", Principal: o.principal})
	allowed, _ := router.Resolve(routing.Request{Namespace: "team", Package: "x", Path: "/a", Principal: o.principal})
	if denied.Allowed || denied.RuleID != "deny-secret" || !allowed.Allowed || allowed.TargetID != local.ID || allowed.RuleID != "main" {
		t.Fatalf("runtime router ignores templates: denied=%+v allowed=%+v", denied, allowed)
	}

	// Rejections: cycles, self-extension, kind, unknown parent.
	etag := func(id string) string {
		w := o.get("/api/v1/policy-templates/" + id)
		expectSourceStatus(t, w, 200)
		return w.Header().Get("ETag")
	}
	baseETag := etag(base.ID)
	cycle := o.request("PUT", "/api/v1/policy-templates/"+base.ID, publishBody("base", child.ID, local.ID, rule("main", 1, "", local.ID, false)), "cycle-1", baseETag)
	expectSourceStatus(t, cycle, 400)
	if !strings.Contains(message(cycle), "policy cycle") {
		t.Fatalf("cycle message: %s", message(cycle))
	}
	self := o.request("PUT", "/api/v1/policy-templates/"+base.ID, publishBody("base", base.ID, local.ID, ""), "cycle-2", baseETag)
	expectSourceStatus(t, self, 400)
	if !strings.Contains(message(self), "cannot extend itself") {
		t.Fatalf("self message: %s", message(self))
	}
	expectSourceStatus(t, create("t-kind", fmt.Sprintf(`{"name":"wrong-kind","kind":"read","extends_id":%q,"read":{}}`, base.ID)), 400)
	expectSourceStatus(t, create("t-unknown", publishBody("orphan", "99999999-9999-4999-8999-999999999999", "", "")), 400)
	if etag(base.ID) != baseETag {
		t.Fatal("a rejected template edit changed the template")
	}

	// A template that targets a source the feed does not have cannot be bound,
	// and cannot be edited into a bound feed.
	strayResp := create("t-stray", publishBody("stray", "", stray.ID, ""))
	expectSourceStatus(t, strayResp, 201)
	badBind := o.request("PUT", bindPath, fmt.Sprintf(`{"publish_template_id":%q}`, decodeJSON[PolicyTemplate](t, strayResp).ID), "bind-2", o.feedRevision(feed.ID))
	expectSourceStatus(t, badBind, 400)
	if !strings.Contains(message(badBind), "effective publish policy would be invalid") {
		t.Fatalf("bind message: %s", message(badBind))
	}
	if got := decodeJSON[FeedPolicyBinding](t, o.get(bindPath)); got.PublishTemplateID != child.ID {
		t.Fatal("rejected binding changed the feed")
	}
	edit := o.request("PUT", "/api/v1/policy-templates/"+base.ID, publishBody("base", "", stray.ID, rule("main", 1, "", local.ID, false)), "edit-bad", baseETag)
	expectSourceStatus(t, edit, 400)
	if !strings.Contains(message(edit), `feed "tmpl-feed"`) || etag(base.ID) != baseETag {
		t.Fatalf("edit that invalidates a bound feed: %s", message(edit))
	}
	// Detaching a source that bound templates still target is refused.
	expectSourceStatus(t, o.request("DELETE", "/api/v1/feeds/"+feed.ID+"/sources/"+local.ID, "", "detach-1", o.feedRevision(feed.ID)), 409)

	// Referenced templates cannot be deleted; unreferenced ones can.
	del := o.request("DELETE", "/api/v1/policy-templates/"+base.ID, "", "del-1", baseETag)
	expectSourceStatus(t, del, 409)
	if !strings.Contains(message(del), "still referenced") {
		t.Fatalf("delete message: %s", message(del))
	}
	unbind := o.request("PUT", bindPath, `{}`, "unbind", o.feedRevision(feed.ID))
	expectSourceStatus(t, unbind, 200)
	if e := effective(); len(e.PublishTemplateChain) != 0 || byID(e, "main").Origin != "feed" {
		t.Fatalf("unbound feed still resolves templates: %+v", e)
	}
	expectSourceStatus(t, o.request("DELETE", "/api/v1/policy-templates/"+base.ID, "", "del-2", baseETag), 409) // child extends it
	expectSourceStatus(t, o.request("DELETE", "/api/v1/policy-templates/"+child.ID, "", "del-3", etag(child.ID)), 204)
	expectSourceStatus(t, o.request("DELETE", "/api/v1/policy-templates/"+base.ID, "", "del-4", baseETag), 204)

	// Every change is audited, and the binding event records what was bound.
	for _, action := range []string{"policy_template.create", "feed.policy_templates", "policy_template.delete"} {
		if o.count(`SELECT count(*) FROM audit_events WHERE action=$1`, action) < 1 {
			t.Fatalf("%s not audited", action)
		}
	}
	if o.count(`SELECT count(*) FROM outbox_events WHERE topic='feed.policy_templates' AND payload->>'publish_template_id'=$1`, child.ID) != 1 {
		t.Fatal("binding audit does not record the bound template")
	}
}

func TestFeedResolutionLoopRejectedAtConfigurationTime(t *testing.T) {
	o := newCfgFixture(t)
	feedgraph.SetSelfOrigins("https://ant.example")
	t.Cleanup(func() { feedgraph.SetSelfOrigins() })
	a := o.feed("loop-a", "raw")
	b := o.feed("loop-b", "raw")
	remoteSource := func(name, origin string) Source {
		s := o.source(name, "remote", "raw")
		expectSourceStatus(t, o.request("PUT", "/api/v1/sources/"+s.ID+"/remote-config", remoteBody(t, remoteconfig.DefaultConfig(origin)), "cfg-"+name, `"1"`), 200)
		return s
	}
	toB := remoteSource("to-b", "https://ant.example/feeds/loop-b")
	toA := remoteSource("to-a", "https://ant.example/feeds/loop-a")
	o.attach(a, toB, 1) // a resolves through b: fine on its own
	closing := o.request("PUT", "/api/v1/feeds/"+b.ID+"/sources/"+toA.ID, `{"priority":1}`, "loop-attach", o.feedRevision(b.ID))
	expectSourceStatus(t, closing, 400)
	if m := message(closing); !strings.Contains(m, "feed resolution loop") || !strings.Contains(m, "loop-a") || !strings.Contains(m, "loop-b") {
		t.Fatalf("loop message: %s", m)
	}
	if o.count(`SELECT count(*) FROM feed_sources WHERE feed_id=$1`, b.ID) != 0 {
		t.Fatal("rejected attachment was kept")
	}
	// A feed resolving to itself.
	self := remoteSource("to-self", "https://ant.example/feeds/loop-b")
	selfLoop := o.request("PUT", "/api/v1/feeds/"+b.ID+"/sources/"+self.ID, `{"priority":1}`, "self-attach", o.feedRevision(b.ID))
	expectSourceStatus(t, selfLoop, 400)
	if !strings.Contains(message(selfLoop), "loop-b -> loop-b") {
		t.Fatalf("self-loop message: %s", message(selfLoop))
	}
	// Re-pointing an already attached source so that it closes a loop is rejected too.
	external := remoteSource("external", "https://upstream.example/x")
	o.attach(b, external, 1)
	repoint := o.request("PUT", "/api/v1/sources/"+external.ID+"/remote-config", remoteBody(t, remoteconfig.DefaultConfig("https://ant.example/feeds/loop-a")), "repoint", `"2"`)
	expectSourceStatus(t, repoint, 400)
	if !strings.Contains(message(repoint), "feed resolution loop") {
		t.Fatalf("repoint message: %s", message(repoint))
	}
	got := o.request("GET", "/api/v1/sources/"+external.ID+"/remote-config", "", "", "")
	expectSourceStatus(t, got, 200)
	if !strings.Contains(got.Body.String(), "upstream.example") {
		t.Fatal("rejected configuration was kept")
	}
}
