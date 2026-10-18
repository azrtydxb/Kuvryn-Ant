package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/google/uuid"
)

type consoleFixture struct {
	*sourceHTTPFixture
	configure, readOnly, domain string
	seq                         int
}

func newConsoleFixture(t *testing.T) *consoleFixture {
	t.Helper()
	f := newSourceHTTPFixture(t)
	ctx := context.Background()
	issue := func(actions ...identity.Action) string {
		tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: actions}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return tok.Secret
	}
	c := &consoleFixture{sourceHTTPFixture: f}
	f.credential = issue(identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin)
	c.configure = issue(identity.Read, identity.Configure)
	c.readOnly = issue(identity.Read)
	mux := http.NewServeMux()
	codec := NewCursorCodec(make([]byte, 32))
	RegisterFeeds(mux, f.db, codec)
	RegisterSources(mux, f.db, codec)
	RegisterPublishPolicies(mux, f.db)
	RegisterIdentities(mux, f.db)
	RegisterConsole(mux, f.db, codec)
	f.handler = WithRequestID(mux)
	if err := f.db.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,'console') RETURNING id::text`, f.instance).Scan(&c.domain); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *consoleFixture) exec(sql string, args ...any) {
	c.t.Helper()
	if _, err := c.db.Exec(context.Background(), sql, args...); err != nil {
		c.t.Fatal(err)
	}
}

// seed creates a component (and, when age>=0, one cached asset created `age` ago).
func (c *consoleFixture) seed(source, coordinate, namespace string, age time.Duration) {
	c.t.Helper()
	ctx := context.Background()
	var comp, version, blob string
	if err := c.db.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4) RETURNING id::text`, c.instance, source, coordinate, namespace).Scan(&comp); err != nil {
		c.t.Fatal(err)
	}
	if age < 0 {
		return
	}
	if err := c.db.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1') RETURNING id::text`, c.instance, comp).Scan(&version); err != nil {
		c.t.Fatal(err)
	}
	digest := strings.Repeat(strings.ReplaceAll(uuid.NewString(), "-", ""), 2)
	if err := c.db.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,10,'k') RETURNING id::text`, c.instance, c.domain, digest).Scan(&blob); err != nil {
		c.t.Fatal(err)
	}
	c.exec(`INSERT INTO assets(instance_id,version_id,asset_key,blob_id,created_at) VALUES($1,$2,'file',$3,now()-make_interval(secs=>$4))`, c.instance, version, blob, age.Seconds())
}

func (c *consoleFixture) key(prefix string) string {
	c.seq++
	return fmt.Sprintf("%s-%d", prefix, c.seq)
}

func (c *consoleFixture) attach(feed Feed, source Source, priority int) Feed {
	c.t.Helper()
	w := c.request("PUT", "/api/v1/feeds/"+feed.ID+"/sources/"+source.ID, fmt.Sprintf(`{"priority":%d}`, priority), c.key("attach"), fmt.Sprintf(`"%d"`, feed.Revision))
	expectSourceStatus(c.t, w, 200)
	var m AttachmentMutation
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	feed.Revision = m.FeedRevision
	return feed
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return v
}

func (c *consoleFixture) auditCount(action string) int {
	return c.count(`SELECT count(*) FROM audit_events WHERE action=$1`, action)
}

func TestConsolePackageSearch(t *testing.T) {
	c := newConsoleFixture(t)
	s1, s2 := c.source("s-local", "local", "raw"), c.source("s-remote", "remote", "raw")
	other := c.source("o-local", "local", "oci")
	feedA, feedB := c.feed("feed-a", "raw"), c.feed("feed-b", "oci")
	feedA = c.attach(feedA, s1, 0)
	c.attach(feedA, s2, 1)
	c.attach(feedB, other, 0)
	c.seed(s1.ID, "team/Widget", "", -1)
	c.seed(s2.ID, "team/Widget", "", -1) // shadowed by s1 in feed-a
	c.seed(s2.ID, "remote/gadget", "", -1)
	c.seed(other.ID, "img/widget", "", -1)

	list := func(query, cred string) (PackageSearchPage, int) {
		w := c.requestAs("GET", "/api/v1/packages"+query, "", "", "", cred)
		if w.Code != 200 {
			t.Logf("search %s: %d %s", query, w.Code, w.Body.String())
			return PackageSearchPage{}, w.Code
		}
		return decode[PackageSearchPage](t, w), 200
	}
	page, code := list("?q=WIDGET", c.credential)
	if code != 200 {
		t.Fatalf("search status %d", code)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("case-insensitive total=%d items=%d", page.Total, len(page.Items))
	}
	for _, item := range page.Items {
		if item.Coordinate == "team/Widget" && (item.SourceID != s1.ID || item.Origin != "local" || item.FeedSlug != "feed-a") {
			t.Fatalf("shadowed owner surfaced: %+v", item)
		}
	}
	if page, _ = list("?q=widget&format=oci", c.credential); page.Total != 1 || page.Items[0].FeedID != feedB.ID {
		t.Fatal("format filter")
	}
	if page, _ = list("?feed="+feedA.ID, c.credential); page.Total != 2 {
		t.Fatalf("feed filter total=%d", page.Total)
	}
	if page, _ = list("?origin=remote", c.credential); page.Total != 1 || page.Items[0].Coordinate != "remote/gadget" {
		t.Fatal("origin filter")
	}
	if page, _ = list("?q=%25", c.credential); page.Total != 0 {
		t.Fatal("q must be literal, not a LIKE pattern")
	}
	// Pagination keeps the total stable and binds the cursor to the filters.
	first, _ := list("?limit=1", c.credential)
	if first.Total != 3 || len(first.Items) != 1 || first.Page.NextCursor == nil {
		t.Fatalf("first page %+v", first)
	}
	seen := map[string]bool{first.Items[0].ID + first.Items[0].FeedID: true}
	next := first
	for next.Page.HasMore {
		next, _ = list("?limit=1&cursor="+url.QueryEscape(*next.Page.NextCursor), c.credential)
		for _, item := range next.Items {
			if seen[item.ID+item.FeedID] {
				t.Fatal("pagination repeated an item")
			}
			seen[item.ID+item.FeedID] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("pagination visited %d", len(seen))
	}
	if _, code := list("?q=x&cursor="+url.QueryEscape(*first.Page.NextCursor), c.credential); code != 400 {
		t.Fatalf("cursor accepted with different filters: %d", code)
	}
	for _, bad := range []string{"?format=nope", "?origin=nope", "?feed=nope", "?limit=0", "?unknown=1", "?q=a&q=b"} {
		if _, code := list(bad, c.credential); code != 400 {
			t.Fatalf("%s accepted: %d", bad, code)
		}
	}
	if _, code := list("", ""); code != 401 {
		t.Fatalf("unauthenticated search: %d", code)
	}
	// Authorization is applied per source: a denied owner disappears from results and the total.
	c.exec(`INSERT INTO role_grants(instance_id,role_id,kind,effect,source_id,actions) SELECT instance_id,role_id,'artifact','deny',$2,ARRAY['read'] FROM principal_roles WHERE principal_id=$1`, c.principal, s2.ID)
	if page, _ = list("?q=gadget", c.credential); page.Total != 0 || len(page.Items) != 0 {
		t.Fatal("denied source leaked into search")
	}
	if page, _ = list("", c.credential); page.Total != 2 {
		t.Fatalf("total counted unauthorized rows: %d", page.Total)
	}
	c.exec(`INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) SELECT instance_id,role_id,'artifact','deny',$2,ARRAY['read'] FROM principal_roles WHERE principal_id=$1`, c.principal, feedB.ID)
	if page, _ = list("?q=widget", c.credential); page.Total != 1 {
		t.Fatalf("denied feed leaked: %d", page.Total)
	}
}

func TestConsoleReorderSources(t *testing.T) {
	c := newConsoleFixture(t)
	feed := c.feed("ordered", "raw")
	var sources []Source
	for i := 0; i < 3; i++ {
		s := c.source(fmt.Sprintf("src-%d", i), "local", "raw")
		sources = append(sources, s)
		feed = c.attach(feed, s, i*10+5)
	}
	path := "/api/v1/feeds/" + feed.ID + "/sources"
	body := func(ids ...string) string {
		b, _ := json.Marshal(map[string]any{"source_ids": ids})
		return string(b)
	}
	rev := fmt.Sprintf(`"%d"`, feed.Revision)
	reversed := body(sources[2].ID, sources[0].ID, sources[1].ID)
	w := c.request("PUT", path, reversed, "reorder-1", rev)
	expectSourceStatus(t, w, 200)
	order := decode[SourceOrder](t, w)
	if order.FeedRevision != feed.Revision+1 || w.Header().Get("ETag") != fmt.Sprintf(`"%d"`, order.FeedRevision) || len(order.Items) != 3 || order.Items[0].SourceID != sources[2].ID || order.Items[2].Priority != 2 {
		t.Fatalf("unexpected order %+v", order)
	}
	list := decode[AttachmentPage](t, c.request("GET", path, "", "", ""))
	if list.Items[0].Source.ID != sources[2].ID || list.Items[1].Source.ID != sources[0].ID || list.Items[2].Source.ID != sources[1].ID || list.FeedRevision != order.FeedRevision {
		t.Fatal("read order not persisted")
	}
	// Idempotent replay: identical body, no second revision bump or audit event.
	replay := c.request("PUT", path, reversed, "reorder-1", rev)
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != w.Body.String() || c.auditCount("feed.source.order") != 1 {
		t.Fatal("replay was not idempotent")
	}
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "reorder-1", rev), 409)
	// Stale revision, missing precondition, partial/unknown/duplicate lists.
	newRev := fmt.Sprintf(`"%d"`, order.FeedRevision)
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "stale", rev), 412)
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "noif", ""), 428)
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID), "partial", newRev), 409)
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID, uuid.NewString()), "unknown", newRev), 409)
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[0].ID, sources[1].ID), "dup", newRev), 400)
	expectSourceStatus(t, c.request("PUT", path, `{"source_ids":[]}`, "empty", newRev), 400)
	expectSourceStatus(t, c.request("PUT", path, `{"source_ids":["`+sources[0].ID+`"],"x":1}`, "extra", newRev), 400)
	expectSourceStatus(t, c.request("PUT", path, reversed, "", newRev), 400)
	if c.auditCount("feed.source.order") != 1 {
		t.Fatal("failed reorder produced audit events")
	}
	// Authorization.
	expectSourceStatus(t, c.requestAs("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "ro", newRev, c.readOnly), 404)
	expectSourceStatus(t, c.requestAs("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "anon", newRev, ""), 401)
	c.deny("source", sources[1].ID, []string{"read"})
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "denied-source", newRev), 404)
	c.deny("feed", feed.ID, []string{"configure"})
	expectSourceStatus(t, c.request("PUT", path, body(sources[0].ID, sources[1].ID, sources[2].ID), "denied-feed", newRev), 404)
}

func TestConsoleBatchCreateFeed(t *testing.T) {
	c := newConsoleFixture(t)
	s1, s2 := c.source("b-one", "local", "raw"), c.source("b-two", "local", "raw")
	oci := c.source("b-oci", "local", "oci")
	batch := func(slug string, sources []string, policy string) string {
		b := fmt.Sprintf(`{"slug":%q,"format":"raw","anonymous_read":true`, slug)
		if sources != nil {
			ids, _ := json.Marshal(sources)
			b += `,"source_ids":` + string(ids)
		}
		if policy != "" {
			b += `,"publish_policy":` + policy
		}
		return b + "}"
	}
	policy := `{"default_source_id":"` + s1.ID + `","rules":[{"id":"docs","priority":10,"deny":false,"selector":{"namespace":"docs"},"target_source_id":"` + s2.ID + `"}]}`
	w := c.request("POST", "/api/v1/feeds/batch", batch("new-feed", []string{s2.ID, s1.ID}, policy), "batch-1", "")
	expectSourceStatus(t, w, 201)
	result := decode[BatchFeedResult](t, w)
	if result.Feed.Revision != 2 || !result.Feed.AnonymousRead || len(result.Sources) != 2 || result.Sources[0].SourceID != s2.ID || result.PublishPolicy == nil || result.PublishPolicy.Revision != 2 || w.Header().Get("Location") != "/api/v1/feeds/"+result.Feed.ID {
		t.Fatalf("unexpected result %+v", result)
	}
	if c.count(`SELECT count(*) FROM feed_sources WHERE feed_id=$1`, result.Feed.ID) != 2 || c.count(`SELECT count(*) FROM publish_policies WHERE feed_id=$1 AND default_source_id=$2`, result.Feed.ID, s1.ID) != 1 {
		t.Fatal("attachments or policy missing")
	}
	got := decode[PublishPolicy](t, c.request("GET", "/api/v1/feeds/"+result.Feed.ID+"/publish-policy", "", "", ""))
	if got.Revision != result.Feed.Revision {
		t.Fatal("policy revision differs from feed revision")
	}
	// Replay returns the stored result; nothing is duplicated.
	replay := c.request("POST", "/api/v1/feeds/batch", batch("new-feed", []string{s2.ID, s1.ID}, policy), "batch-1", "")
	expectSourceStatus(t, replay, 201)
	if replay.Body.String() != w.Body.String() || c.count(`SELECT count(*) FROM feeds WHERE slug='new-feed'`) != 1 || c.auditCount("feed.create") != 1 || c.auditCount("feed.source.attach") != 2 || c.auditCount("feed.publish_policy") != 1 {
		t.Fatal("batch replay not idempotent / audit mismatch")
	}
	expectSourceStatus(t, c.request("POST", "/api/v1/feeds/batch", batch("new-feed", []string{s1.ID}, ""), "batch-1", ""), 409)
	expectSourceStatus(t, c.request("POST", "/api/v1/feeds/batch", batch("new-feed", nil, ""), "batch-dup-slug", ""), 409)
	// Failures roll the whole batch back, including the feed row itself.
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"format-mismatch":  {batch("rb-format", []string{s1.ID, oci.ID}, ""), 409},
		"unknown-source":   {batch("rb-unknown", []string{s1.ID, uuid.NewString()}, ""), 404},
		"unattached-rule":  {batch("rb-policy", []string{s1.ID}, policy), 404},
		"invalid-policy":   {batch("rb-invalid", []string{s1.ID}, `{"rules":[{"id":"x"}]}`), 400},
		"duplicate-source": {batch("rb-dup", []string{s1.ID, s1.ID}, ""), 400},
		"bad-slug":         {`{"slug":"BAD","format":"raw"}`, 400},
	} {
		expectSourceStatus(t, c.request("POST", "/api/v1/feeds/batch", tc.body, "rb-"+name, ""), tc.status)
	}
	if c.count(`SELECT count(*) FROM feeds WHERE slug LIKE 'rb-%'`) != 0 {
		t.Fatal("failed batch left a feed behind")
	}
	// Feed-only batch is valid; revision stays 1.
	minimal := decode[BatchFeedResult](t, c.request("POST", "/api/v1/feeds/batch", `{"slug":"minimal","format":"raw"}`, "batch-min", ""))
	if minimal.Feed.Revision != 1 || len(minimal.Sources) != 0 || minimal.PublishPolicy != nil {
		t.Fatalf("%+v", minimal)
	}
	// Authorization: collection-level configure and source-level configure are required.
	expectSourceStatus(t, c.requestAs("POST", "/api/v1/feeds/batch", batch("ro-feed", nil, ""), "ro", "", c.readOnly), 403)
	expectSourceStatus(t, c.requestAs("POST", "/api/v1/feeds/batch", batch("anon-feed", nil, ""), "anon", "", ""), 401)
	expectSourceStatus(t, c.request("POST", "/api/v1/feeds/batch", batch("no-key", nil, ""), "", ""), 400)
	c.deny("source", s2.ID, []string{"configure"})
	expectSourceStatus(t, c.request("POST", "/api/v1/feeds/batch", batch("denied-src", []string{s2.ID}, ""), "denied-src", ""), 404)
	if c.count(`SELECT count(*) FROM feeds WHERE slug IN ('ro-feed','anon-feed','no-key','denied-src')`) != 0 {
		t.Fatal("denied batch created a feed")
	}
}

func TestConsoleResolveExplain(t *testing.T) {
	c := newConsoleFixture(t)
	remote, local, cold := c.source("r-remote", "remote", "raw"), c.source("r-local", "local", "raw"), c.source("r-cold", "remote", "raw")
	feed := c.feed("resolve", "raw")
	feed = c.attach(feed, remote, 0)
	feed = c.attach(feed, cold, 1)
	feed = c.attach(feed, local, 2)
	c.exec(`INSERT INTO remote_source_configs(instance_id,source_id,config) VALUES($1,$2,'{"cache":{"metadata_ttl_seconds":300,"stale_if_error_seconds":600}}')`, c.instance, remote.ID)
	c.seed(remote.ID, "pkg/cached", "", 400*time.Second) // stale, still servable on error
	c.seed(local.ID, "pkg/cached", "", -1)               // shadowed
	c.seed(local.ID, "pkg/fresh-local", "", -1)
	explain := func(query, cred string) (ResolveExplanation, int) {
		w := c.requestAs("GET", "/api/v1/feeds/"+feed.ID+"/resolve"+query, "", "", "", cred)
		if w.Code != 200 {
			return ResolveExplanation{}, w.Code
		}
		return decode[ResolveExplanation](t, w), 200
	}
	out, _ := explain("?coordinate=pkg/cached", c.credential)
	if out.Status != "served" || out.Serving == nil || out.Serving.SourceID != remote.ID || out.Serving.Role != "serving" {
		t.Fatalf("serving: %+v", out)
	}
	roles := map[string]string{}
	for _, s := range out.Sources {
		roles[s.SourceID] = s.Role
	}
	if roles[local.ID] != "shadowed" || roles[cold.ID] != "absent" || out.Cache.State != "stale" || !out.Cache.ServableStaleOnError || out.Cache.MetadataTTLSeconds != 300 || out.Cache.AgeSeconds == nil || *out.Cache.AgeSeconds < 399 {
		t.Fatalf("shadow/cache info: %+v", out)
	}
	// Fresh cache.
	c.exec(`UPDATE assets SET created_at=now() WHERE id IN (SELECT a.id FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.coordinate='pkg/cached')`)
	if out, _ = explain("?coordinate=pkg/cached", c.credential); out.Cache.State != "fresh" || out.Cache.ServableStaleOnError {
		t.Fatalf("fresh: %+v", out.Cache)
	}
	// Local owner: cache not applicable. Remote owners precede it, but they do not contain the coordinate.
	if out, _ = explain("?coordinate=pkg/fresh-local", c.credential); out.Status != "served" || out.Serving.SourceID != local.ID || out.Cache.State != "not_applicable" {
		t.Fatalf("local owner: %+v", out)
	}
	// Unknown coordinate falls through to remote sources on first read.
	if out, _ = explain("?coordinate=pkg/unknown", c.credential); out.Status != "pull_through" || out.Cache.State != "not_cached" || out.Serving != nil {
		t.Fatalf("pull-through: %+v", out)
	}
	if out, _ = explain("?coordinate=pkg/cached&namespace=other", c.credential); out.Status != "namespace_mismatch" || out.Serving != nil {
		t.Fatalf("namespace mismatch: %+v", out)
	}
	if _, code := explain("", c.credential); code != 400 {
		t.Fatal("coordinate required")
	}
	if _, code := explain("?coordinate=x&bad=1", c.credential); code != 400 {
		t.Fatal("unknown parameter accepted")
	}
	if _, code := explain("?coordinate=x", ""); code != 401 {
		t.Fatal("anonymous explain")
	}
	// A source the caller cannot read is hidden, and an unreadable owner reveals nothing.
	c.exec(`INSERT INTO role_grants(instance_id,role_id,kind,effect,source_id,actions) SELECT instance_id,role_id,'artifact','deny',$2,ARRAY['read'] FROM principal_roles WHERE principal_id=$1`, c.principal, local.ID)
	if out, _ = explain("?coordinate=pkg/cached", c.credential); out.HiddenSources != 1 || len(out.Sources) != 2 || strings.Contains(fmt.Sprint(out), local.ID) {
		t.Fatalf("hidden source leaked: %+v", out)
	}
	if out, _ = explain("?coordinate=pkg/fresh-local", c.credential); out.Status != "not_found" || out.Serving != nil || len(out.Sources) != 0 {
		t.Fatalf("unreadable owner leaked: %+v", out)
	}
	c.deny("feed", feed.ID, []string{"read"})
	if _, code := explain("?coordinate=pkg/cached", c.credential); code != 404 {
		t.Fatalf("denied feed: %d", code)
	}
}

func TestConsolePublishPolicyDryRun(t *testing.T) {
	c := newConsoleFixture(t)
	docs, general := c.source("d-docs", "local", "raw"), c.source("d-general", "local", "raw")
	remote := c.source("d-remote", "remote", "raw")
	feed := c.feed("dryrun", "raw")
	feed = c.attach(feed, docs, 0)
	feed = c.attach(feed, general, 1)
	c.attach(feed, remote, 2)
	path := "/api/v1/feeds/" + feed.ID + "/publish-policy:test"
	policy := `{"default_source_id":"` + general.ID + `","rules":[` +
		`{"id":"docs","priority":10,"deny":false,"selector":{"path_prefix":"/docs"},"target_source_id":"` + docs.ID + `"},` +
		`{"id":"no-secret","priority":1,"deny":true,"selector":{"package":"secret"}}]}`
	test := func(body, cred string) *httptest.ResponseRecorder {
		return c.requestAs("POST", path, body, "", "", cred)
	}
	req := func(coordinate, p string, extra string) string {
		return fmt.Sprintf(`{"policy":%s,"coordinate":%q,"path":%q%s}`, policy, coordinate, p, extra)
	}
	before := c.count(`SELECT count(*) FROM audit_events`)
	res := decode[PolicyTestResult](t, test(req("a", "/docs/a.txt", ""), c.credential))
	if !res.Allowed || res.MatchedRuleID != "docs" || res.TargetSourceID != docs.ID || res.TargetSourceName != "d-docs" || res.PrincipalAuthorized == nil || !*res.PrincipalAuthorized {
		t.Fatalf("matched rule: %+v", res)
	}
	res = decode[PolicyTestResult](t, test(req("a", "/other/a.txt", ""), c.credential))
	if !res.Allowed || res.Reason != "default_destination" || res.TargetSourceID != general.ID || res.MatchedRuleID != "" {
		t.Fatalf("default: %+v", res)
	}
	res = decode[PolicyTestResult](t, test(req("secret", "/docs/x", ""), c.credential))
	if res.Allowed || res.Denial != "denied_by_rule" || res.MatchedRuleID != "no-secret" || len(res.Trace) != 2 {
		t.Fatalf("deny rule: %+v", res)
	}
	// Evaluating the effective permissions of another principal needs admin; an unprivileged principal cannot publish.
	var nobody string
	if err := c.db.QueryRow(context.Background(), `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','nobody') RETURNING id::text`, c.instance).Scan(&nobody); err != nil {
		t.Fatal(err)
	}
	res = decode[PolicyTestResult](t, test(req("a", "/docs/a", `,"principal_id":"`+nobody+`"`), c.credential))
	if !res.Allowed || res.PrincipalAuthorized == nil || *res.PrincipalAuthorized || res.Denial != "principal_not_permitted" || res.PrincipalID != nobody {
		t.Fatalf("principal evaluation: %+v", res)
	}
	expectSourceStatus(t, test(req("a", "/docs/a", `,"principal_id":"`+nobody+`"`), c.configure), 403)
	if w := test(req("a", "/docs/a", ""), c.configure); w.Code != 200 {
		t.Fatalf("configure-only caller evaluating itself: %d", w.Code)
	}
	// Invalid input and policies that saving would reject.
	expectSourceStatus(t, test(fmt.Sprintf(`{"policy":%s,"coordinate":""}`, policy), c.credential), 400)
	expectSourceStatus(t, test(fmt.Sprintf(`{"policy":%s,"coordinate":"a","path":"relative"}`, policy), c.credential), 400)
	expectSourceStatus(t, test(`{"policy":{"rules":[{"id":"x"}]},"coordinate":"a"}`, c.credential), 400)
	expectSourceStatus(t, test(`{"policy":{"default_source_id":"`+remote.ID+`","rules":[]},"coordinate":"a"}`, c.credential), 400)
	expectSourceStatus(t, test(`{"policy":{"default_source_id":"`+uuid.NewString()+`","rules":[]},"coordinate":"a"}`, c.credential), 404)
	expectSourceStatus(t, test(req("a", "/x", `,"principal_id":"bad"`), c.credential), 400)
	expectSourceStatus(t, test(req("a", "/x", ""), ""), 401)
	// Authorization: configure on feed and sources is required.
	expectSourceStatus(t, test(req("a", "/x", ""), c.readOnly), 404)
	c.deny("source", docs.ID, []string{"configure"})
	expectSourceStatus(t, test(req("a", "/docs/x", ""), c.credential), 404)
	c.deny("feed", feed.ID, []string{"configure"})
	expectSourceStatus(t, test(`{"policy":{"rules":[]},"coordinate":"a"}`, c.credential), 404)
	if c.count(`SELECT count(*) FROM audit_events`) != before || c.count(`SELECT count(*) FROM publish_policies WHERE feed_id=$1`, feed.ID) != 0 {
		t.Fatal("dry run persisted state")
	}
}

func TestConsoleTokenList(t *testing.T) {
	c := newConsoleFixture(t)
	svc := createIdentityPrincipalFor(c, "robot")
	other := createIdentityPrincipalFor(c, "robot-2")
	feed := c.feed("tok-feed", "raw")
	issue := func(principal string, expires time.Duration) string {
		w := c.request("POST", "/api/v1/principals/"+principal+"/tokens", fmt.Sprintf(`{"scopes":[{"feed_id":%q,"actions":["read","publish"]}],"expires_at":%q}`, feed.ID, time.Now().Add(expires).UTC().Format(time.RFC3339)), "", "")
		expectSourceStatus(t, w, 201)
		return decode[struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		}](t, w).ID
	}
	active, revoked := issue(svc, time.Hour), issue(svc, 2*time.Hour)
	issue(other, time.Hour)
	expectSourceStatus(t, c.request("DELETE", "/api/v1/tokens/"+revoked, "", "revoke-1", ""), 200)
	w := c.request("GET", "/api/v1/tokens?principal="+svc, "", "", "")
	expectSourceStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "verifier") || strings.Contains(w.Body.String(), "ant_") {
		t.Fatalf("token list leaks credential material: %s", w.Body.String())
	}
	page := decode[TokenPage](t, w)
	if len(page.Items) != 2 {
		t.Fatalf("principal filter: %d", len(page.Items))
	}
	byID := map[string]TokenRecord{}
	for _, item := range page.Items {
		byID[item.ID] = item
	}
	if byID[active].Status != "active" || byID[revoked].Status != "revoked" || byID[revoked].RevokedAt == nil || byID[active].Scopes[0].FeedID != feed.ID || byID[active].LastUsedAt != nil {
		t.Fatalf("records: %+v", byID)
	}
	// The caller's own token has authenticated, so it reports last use.
	all := decode[TokenPage](t, c.request("GET", "/api/v1/tokens", "", "", ""))
	used := 0
	for _, item := range all.Items {
		if item.LastUsedAt != nil {
			used++
		}
	}
	if len(all.Items) < 6 || used < 1 {
		t.Fatalf("all=%d used=%d", len(all.Items), used)
	}
	if got := decode[TokenPage](t, c.request("GET", "/api/v1/tokens?status=revoked", "", "", "")); len(got.Items) != 1 || got.Items[0].ID != revoked {
		t.Fatal("status filter")
	}
	c.exec(`UPDATE service_tokens SET expires_at=now()+interval '1 second',created_at=now()-interval '1 hour' WHERE id=$1`, active)
	time.Sleep(1200 * time.Millisecond)
	if got := decode[TokenPage](t, c.request("GET", "/api/v1/tokens?status=expired", "", "", "")); len(got.Items) != 1 || got.Items[0].ID != active {
		t.Fatalf("expired filter: %+v", got.Items)
	}
	first := decode[TokenPage](t, c.request("GET", "/api/v1/tokens?limit=2", "", "", ""))
	if len(first.Items) != 2 || first.Page.NextCursor == nil {
		t.Fatal("pagination")
	}
	second := decode[TokenPage](t, c.request("GET", "/api/v1/tokens?limit=2&cursor="+url.QueryEscape(*first.Page.NextCursor), "", "", ""))
	if len(second.Items) == 0 || second.Items[0].ID == first.Items[0].ID || second.Items[0].ID == first.Items[1].ID {
		t.Fatal("pagination repeated")
	}
	expectSourceStatus(t, c.request("GET", "/api/v1/tokens?principal=bad", "", "", ""), 400)
	expectSourceStatus(t, c.request("GET", "/api/v1/tokens?status=bad", "", "", ""), 400)
	expectSourceStatus(t, c.requestAs("GET", "/api/v1/tokens", "", "", "", c.configure), 403)
	expectSourceStatus(t, c.requestAs("GET", "/api/v1/tokens", "", "", "", ""), 401)
}

func createIdentityPrincipalFor(c *consoleFixture, name string) string {
	c.t.Helper()
	var id string
	if err := c.db.QueryRow(context.Background(), `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service',$2) RETURNING id::text`, c.instance, name).Scan(&id); err != nil {
		c.t.Fatal(err)
	}
	return id
}

func TestConsoleIdentityBindings(t *testing.T) {
	c := newConsoleFixture(t)
	var alice, bob string
	for name, dst := range map[string]*string{"alice": &alice, "bob": &bob} {
		if err := c.db.QueryRow(context.Background(), `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user',$2) RETURNING id::text`, c.instance, name).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(issuer, subject, principal string) {
		expectSourceStatus(t, c.request("POST", "/api/v1/identity-bindings", fmt.Sprintf(`{"issuer":%q,"subject":%q,"principal_id":%q}`, issuer, subject, principal), c.key("bind"), ""), 201)
	}
	bind("https://idp.example/a", "alice@example.com", alice)
	bind("https://idp.example/a", "alice-alt", alice)
	bind("https://idp.example/b", "bob", bob)
	list := decode[IdentityBindingPage](t, c.request("GET", "/api/v1/identity-bindings", "", "", ""))
	if len(list.Items) != 3 {
		t.Fatalf("bindings: %d", len(list.Items))
	}
	if got := decode[IdentityBindingPage](t, c.request("GET", "/api/v1/identity-bindings?principal="+alice, "", "", "")); len(got.Items) != 2 {
		t.Fatal("principal filter")
	}
	first := decode[IdentityBindingPage](t, c.request("GET", "/api/v1/identity-bindings?limit=2", "", "", ""))
	if len(first.Items) != 2 || first.Page.NextCursor == nil {
		t.Fatal("pagination")
	}
	second := decode[IdentityBindingPage](t, c.request("GET", "/api/v1/identity-bindings?limit=2&cursor="+url.QueryEscape(*first.Page.NextCursor), "", "", ""))
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID || second.Items[0].ID == first.Items[1].ID {
		t.Fatal("pagination repeated")
	}
	target := list.Items[0]
	// A browser session depending on the binding is removed with it.
	c.exec(`INSERT INTO browser_sessions(verifier,csrf_hash,instance_id,issuer,subject,principal_id,expires_at,idle_seconds) VALUES(decode(repeat('ab',32),'hex'),decode(repeat('cd',32),'hex'),$1,$2,$3,$4,now()+interval '1 hour',600)`, c.instance, target.Issuer, target.Subject, target.PrincipalID)
	path := "/api/v1/identity-bindings/" + target.ID
	w := c.request("DELETE", path, "", "unbind-1", "")
	expectSourceStatus(t, w, 200)
	if got := decode[IdentityBindingDeleted](t, w); got.ID != target.ID || !got.Deleted {
		t.Fatal("delete result")
	}
	if c.count(`SELECT count(*) FROM oidc_identity_bindings`) != 2 || c.count(`SELECT count(*) FROM browser_sessions`) != 0 || c.auditCount("identity.unbind") != 1 {
		t.Fatal("binding/session/audit state")
	}
	replay := c.request("DELETE", path, "", "unbind-1", "")
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != w.Body.String() || c.auditCount("identity.unbind") != 1 {
		t.Fatal("replay not idempotent")
	}
	expectSourceStatus(t, c.request("DELETE", path, "", "unbind-2", ""), 404)
	expectSourceStatus(t, c.request("DELETE", "/api/v1/identity-bindings/short", "", "unbind-3", ""), 400)
	expectSourceStatus(t, c.request("DELETE", "/api/v1/identity-bindings/"+list.Items[1].ID, "", "", ""), 400)
	expectSourceStatus(t, c.requestAs("DELETE", "/api/v1/identity-bindings/"+list.Items[1].ID, "", "x", "", c.configure), 403)
	expectSourceStatus(t, c.requestAs("GET", "/api/v1/identity-bindings", "", "", "", c.configure), 403)
	expectSourceStatus(t, c.requestAs("DELETE", "/api/v1/identity-bindings/"+list.Items[1].ID, "", "x", "", ""), 401)
	if c.count(`SELECT count(*) FROM oidc_identity_bindings`) != 2 {
		t.Fatal("denied delete removed a binding")
	}
}

func TestConsoleAuditReadAndExport(t *testing.T) {
	c := newConsoleFixture(t)
	feed := c.feed("audited", "raw")
	src := c.source("audited-src", "local", "raw")
	c.attach(feed, src, 0)
	c.exec(`INSERT INTO audit_events(instance_id,principal_id,action,resource_id,result,details) VALUES($1,$2,'feed.delete',$3,'denied','{"why":"test"}'),($1,$2,'=HYPERLINK("x")',$3,'failure','{}')`, c.instance, c.principal, feed.ID)
	get := func(query string) AuditPage {
		w := c.request("GET", "/api/v1/audit"+query, "", "", "")
		expectSourceStatus(t, w, 200)
		return decode[AuditPage](t, w)
	}
	all := get("")
	if len(all.Items) < 5 || all.Items[0].ID < all.Items[1].ID {
		t.Fatalf("not newest-first: %d", len(all.Items))
	}
	if got := get("?action=feed.create"); len(got.Items) != 1 || got.Items[0].Action != "feed.create" || got.Items[0].Result != "success" {
		t.Fatalf("action filter: %+v", got.Items)
	}
	if got := get("?result=denied"); len(got.Items) != 1 || string(got.Items[0].Details) != `{"why": "test"}` && string(got.Items[0].Details) != `{"why":"test"}` {
		t.Fatalf("result filter: %+v", got.Items)
	}
	if got := get("?principal=" + c.principal + "&action=source.create"); len(got.Items) != 1 {
		t.Fatal("principal filter")
	}
	if got := get("?principal=" + uuid.NewString()); len(got.Items) != 0 {
		t.Fatal("unknown principal matched")
	}
	now := time.Now().UTC()
	if got := get("?from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339))); len(got.Items) != len(all.Items) {
		t.Fatal("time window")
	}
	if got := get("?to=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339))); len(got.Items) != 0 {
		t.Fatal("to bound")
	}
	if got := get("?from=" + url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339))); len(got.Items) != 0 {
		t.Fatal("from bound")
	}
	// Cursor pagination is stable and bound to the filter.
	seen := map[int64]bool{}
	page := get("?limit=2")
	for {
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("pagination repeated")
			}
			seen[item.ID] = true
		}
		if !page.Page.HasMore {
			break
		}
		page = get("?limit=2&cursor=" + url.QueryEscape(*page.Page.NextCursor))
	}
	if len(seen) != len(all.Items) {
		t.Fatalf("visited %d of %d", len(seen), len(all.Items))
	}
	first := get("?limit=1")
	expectSourceStatus(t, c.request("GET", "/api/v1/audit?limit=1&action=x&cursor="+url.QueryEscape(*first.Page.NextCursor), "", "", ""), 400)
	for _, bad := range []string{"?result=bogus", "?principal=zzz", "?from=yesterday", "?limit=500", "?x=1", "?from=2030-01-02T00:00:00Z&to=2030-01-01T00:00:00Z"} {
		expectSourceStatus(t, c.request("GET", "/api/v1/audit"+bad, "", "", ""), 400)
	}
	expectSourceStatus(t, c.requestAs("GET", "/api/v1/audit", "", "", "", c.configure), 403)
	expectSourceStatus(t, c.requestAs("GET", "/api/v1/audit", "", "", "", ""), 401)

	export := func(query, cred string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://example.test/api/v1/audit/export"+query, nil)
		if cred != "" {
			r.Header.Set("Authorization", "Bearer "+cred)
		}
		w := httptest.NewRecorder()
		c.handler.ServeHTTP(w, r)
		return w
	}
	before := c.auditCount("audit.export")
	w := export("?format=ndjson", c.credential)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" || !strings.Contains(w.Header().Get("Content-Disposition"), ".ndjson") {
		t.Fatalf("ndjson export: %d %v", w.Code, w.Header())
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) < len(all.Items) {
		t.Fatalf("ndjson lines=%d", len(lines))
	}
	var event AuditEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &event); err != nil || event.Action != "instance.bootstrap" {
		t.Fatalf("oldest line: %v %s", err, lines[len(lines)-1])
	}
	csvOut := export("?format=csv&result=failure", c.credential)
	body := csvOut.Body.String()
	if csvOut.Code != 200 || !strings.HasPrefix(csvOut.Header().Get("Content-Type"), "text/csv") || !strings.HasPrefix(body, "id,created_at,principal_id,action,resource_id,result,details\n") || strings.Count(body, "\n") != 2 {
		t.Fatalf("csv export: %d %q", csvOut.Code, body)
	}
	if !strings.Contains(body, `'=HYPERLINK`) {
		t.Fatalf("csv formula injection not neutralized: %q", body)
	}
	if c.auditCount("audit.export") != before+2 {
		t.Fatal("exports were not audited")
	}
	// Large exports stream across batches without loss.
	c.exec(`INSERT INTO audit_events(instance_id,principal_id,action) SELECT $1,$2,'bulk' FROM generate_series(1,1203)`, c.instance, c.principal)
	if got := export("?action=bulk&format=ndjson", c.credential); strings.Count(got.Body.String(), "\n") != 1203 {
		t.Fatalf("bulk export rows=%d", strings.Count(got.Body.String(), "\n"))
	}
	if export("?format=xml", c.credential).Code != 400 || export("", c.configure).Code != 403 || export("", "").Code != 401 {
		t.Fatal("export validation/authorization")
	}
	if c.auditCount("audit.export") != before+3 {
		t.Fatal("rejected exports must not be audited")
	}
}

func TestConsoleDeleteFeed(t *testing.T) {
	c := newConsoleFixture(t)
	src, replica := c.source("del-src", "local", "raw"), c.source("del-replica", "replica", "raw")
	feed := c.feed("doomed", "raw")
	feed = c.attach(feed, src, 0)
	feed = c.attach(feed, replica, 1)
	c.seed(src.ID, "keep/me", "", -1)
	expectSourceStatus(t, c.request("PUT", "/api/v1/feeds/"+feed.ID+"/publish-policy", `{"default_source_id":"`+src.ID+`","rules":[]}`, "policy", fmt.Sprintf(`"%d"`, feed.Revision)), 200)
	feed.Revision++
	c.exec(`INSERT INTO retention_policies(instance_id,feed_id,keep_last) VALUES($1,$2,3)`, c.instance, feed.ID)
	c.exec(`INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) SELECT instance_id,role_id,'feed','allow',$2,ARRAY['read'] FROM principal_roles WHERE principal_id=$1`, c.principal, feed.ID)
	peer := uuid.NewString()
	c.exec(`INSERT INTO encrypted_secrets(instance_id,purpose,envelope,key_version) VALUES($1,$2,'{"key_version":"v1"}','v1')`, c.instance, "replication-peer:"+peer)
	c.exec(`INSERT INTO replication_peers(id,instance_id,name,url,token_secret_id) SELECT $2,$1,'peer','https://peer.example',id FROM encrypted_secrets WHERE purpose=$3`, c.instance, peer, "replication-peer:"+peer)
	c.exec(`INSERT INTO replication_jobs(instance_id,name,peer_id,remote_feed_id,dest_feed_id,replica_source_id) VALUES($1,'job',$2,gen_random_uuid(),$3,$4)`, c.instance, peer, feed.ID, replica.ID)
	path := "/api/v1/feeds/" + feed.ID
	rev := fmt.Sprintf(`"%d"`, feed.Revision)
	expectSourceStatus(t, c.request("DELETE", path, "", "d1", ""), 428)
	expectSourceStatus(t, c.request("DELETE", path, "", "d1", `"1"`), 412)
	expectSourceStatus(t, c.request("DELETE", path, "", "", rev), 400)
	expectSourceStatus(t, c.request("DELETE", path+"?force=maybe", "", "d1", rev), 400)
	expectSourceStatus(t, c.requestAs("DELETE", path, "", "ro", rev, c.readOnly), 404)
	expectSourceStatus(t, c.requestAs("DELETE", path, "", "anon", rev, ""), 401)
	// Protected while a replication job targets the feed.
	conflict := c.request("DELETE", path, "", "d-noforce", rev)
	expectSourceStatus(t, conflict, 409)
	if !strings.Contains(conflict.Body.String(), "replication") || c.count(`SELECT count(*) FROM feeds WHERE id=$1`, feed.ID) != 1 || c.count(`SELECT count(*) FROM replication_jobs`) != 1 {
		t.Fatal("protected feed was modified")
	}
	// Force needs instance administration even with configure permission.
	expectSourceStatus(t, c.requestAs("DELETE", path+"?force=true", "", "cfg-force", rev, c.configure), 403)
	if c.auditCount("feed.delete") != 0 {
		t.Fatal("failed deletes were audited")
	}
	w := c.request("DELETE", path+"?force=true", "", "d-force", rev)
	expectSourceStatus(t, w, 200)
	out := decode[FeedDeleted](t, w)
	if !out.Deleted || out.SourcesDetached != 2 || out.ReplicationJobs != 1 || out.RoleGrantsRemoved != 1 || out.RetentionPolicies != 1 {
		t.Fatalf("result: %+v", out)
	}
	if c.count(`SELECT count(*) FROM feeds WHERE id=$1`, feed.ID) != 0 || c.count(`SELECT count(*) FROM feed_sources WHERE feed_id=$1`, feed.ID) != 0 || c.count(`SELECT count(*) FROM publish_policies WHERE feed_id=$1`, feed.ID) != 0 || c.count(`SELECT count(*) FROM replication_jobs`) != 0 {
		t.Fatal("feed dependents remain")
	}
	// Sources and their content are kept.
	if c.count(`SELECT count(*) FROM sources WHERE id IN ($1,$2)`, src.ID, replica.ID) != 2 || c.count(`SELECT count(*) FROM components WHERE coordinate='keep/me'`) != 1 {
		t.Fatal("sources or content were deleted")
	}
	if c.auditCount("feed.delete") != 1 {
		t.Fatal("missing audit event")
	}
	replay := c.request("DELETE", path+"?force=true", "", "d-force", rev)
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != w.Body.String() || c.auditCount("feed.delete") != 1 {
		t.Fatal("replay not idempotent")
	}
	expectSourceStatus(t, c.request("GET", path, "", "", ""), 404)
	expectSourceStatus(t, c.request("DELETE", path, "", "again", rev), 404)
	// An ordinary feed (no replication) deletes without force and with configure only.
	plain := c.feed("plain", "raw")
	expectSourceStatus(t, c.requestAs("DELETE", "/api/v1/feeds/"+plain.ID, "", "plain", fmt.Sprintf(`"%d"`, plain.Revision), c.configure), 200)
}

func TestConsoleSourceCachePurge(t *testing.T) {
	c := newConsoleFixture(t)
	remote, local := c.source("p-remote", "remote", "raw"), c.source("p-local", "local", "raw")
	c.seed(remote.ID, "a/one", "", time.Minute)
	c.seed(remote.ID, "a/two", "", time.Minute)
	c.seed(local.ID, "a/one", "", time.Minute)
	c.exec(`INSERT INTO tags(component_id,name,version_id) SELECT c.id,'latest',v.id FROM components c JOIN versions v ON v.component_id=c.id WHERE c.source_id=$1`, remote.ID)
	path := "/api/v1/sources/" + remote.ID + "/cache"
	w := c.request("DELETE", path+"?coordinate=a/one", "", "purge-one", "")
	expectSourceStatus(t, w, 200)
	out := decode[CachePurge](t, w)
	if out.ComponentsRemoved != 1 || out.VersionsRemoved != 1 || out.AssetsRemoved != 1 || out.BytesReleased != 10 {
		t.Fatalf("single purge: %+v", out)
	}
	replay := c.request("DELETE", path+"?coordinate=a/one", "", "purge-one", "")
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != w.Body.String() || c.auditCount("source.cache.purge") != 1 {
		t.Fatal("replay not idempotent")
	}
	expectSourceStatus(t, c.request("DELETE", path+"?coordinate=a/two", "", "purge-one", ""), 409)
	if c.count(`SELECT count(*) FROM components WHERE source_id=$1`, remote.ID) != 1 {
		t.Fatal("wrong component purged")
	}
	out = decode[CachePurge](t, c.request("DELETE", path, "", "purge-all", ""))
	if out.ComponentsRemoved != 1 || c.count(`SELECT count(*) FROM components WHERE source_id=$1`, remote.ID) != 0 || c.count(`SELECT count(*) FROM tags`) != 0 {
		t.Fatalf("full purge: %+v", out)
	}
	// Only the remote source's cache is touched; blobs are left to garbage collection.
	if c.count(`SELECT count(*) FROM components WHERE source_id=$1`, local.ID) != 1 || c.count(`SELECT count(*) FROM blobs`) != 3 {
		t.Fatal("purge reached beyond the remote cache")
	}
	if c.auditCount("source.cache.purge") != 2 {
		t.Fatal("audit events")
	}
	expectSourceStatus(t, c.request("DELETE", "/api/v1/sources/"+local.ID+"/cache", "", "local", ""), 409)
	expectSourceStatus(t, c.request("DELETE", "/api/v1/sources/"+uuid.NewString()+"/cache", "", "missing", ""), 404)
	expectSourceStatus(t, c.request("DELETE", path, "", "", ""), 400)
	expectSourceStatus(t, c.request("DELETE", path+"?coordinate=", "", "empty", ""), 400)
	expectSourceStatus(t, c.requestAs("DELETE", path, "", "ro", "", c.readOnly), 404)
	expectSourceStatus(t, c.requestAs("DELETE", path, "", "anon", "", ""), 401)
	c.deny("source", remote.ID, []string{"configure"})
	expectSourceStatus(t, c.request("DELETE", path, "", "denied", ""), 404)
}
