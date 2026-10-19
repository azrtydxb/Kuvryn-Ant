package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/auditsink"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

type opsFixture struct {
	*sourceHTTPFixture
	deps    OpsDeps
	prewarm *notifyCounter
	limited string // token of a principal without any role
	limitID string
}

func newOpsFixture(t *testing.T) *opsFixture {
	f := newSourceHTTPFixture(t)
	all := []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}
	tok, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: all}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.credential = tok.Secret
	keys, err := secrets.New("v1", map[string][]byte{"v1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	codec := NewCursorCodec(make([]byte, 32))
	counter := &notifyCounter{}
	deps := OpsDeps{Prewarm: counter, Pool: f.db, Codec: codec, Keys: keys, Background: context.Background(), Health: remoteruntime.NewHealth(), StorageBackend: "disk",
		Env: []EnvSetting{{Key: "public_url", Value: "https://ant.example.test", Description: "env"}},
		Readiness: func(context.Context) ReadinessDetail {
			return ReadinessDetail{Ready: true, Database: ComponentStatus{Status: "ok"}, BlobStorage: ComponentStatus{Status: "ok"}, WriterLease: WriterLeaseStatus{State: "held"}, CheckedAt: time.Now().UTC()}
		}}
	mux := http.NewServeMux()
	Register(mux)
	RegisterFeeds(mux, f.db, codec)
	RegisterSources(mux, f.db, codec)
	RegisterIdentities(mux, f.db)
	RegisterOperations(mux, deps)
	RegisterTransfer(mux, TransferDeps{Pool: f.db})
	EnableStatusDetail(f.db, deps.Readiness)
	t.Cleanup(func() { statusDetail.Store(nil) })
	f.handler = WithRequestID(mux)
	o := &opsFixture{sourceHTTPFixture: f, deps: deps, prewarm: counter}
	p := createIdentityPrincipal(f, "limited", "service")
	lt, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, p.ID, []identity.Scope{{Actions: all}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	o.limited, o.limitID = lt.Secret, p.ID
	return o
}

// rawRequest skips OpenAPI response validation: the validating router cannot
// match verb-suffixed paths such as /sources/{id}:test.
func (o *opsFixture) rawRequest(method, path, key, credential string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://example.test"+path, nil)
	r.Header.Set("Authorization", "Bearer "+credential)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	o.handler.ServeHTTP(w, r)
	return w
}
func (o *opsFixture) get(path string) *httptest.ResponseRecorder {
	return o.request("GET", path, "", "", "")
}
func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOperationsAuthorization(t *testing.T) {
	o := newOpsFixture(t)
	admin := []string{"/api/v1/storage/usage", "/api/v1/settings", "/api/v1/retention/policies", "/api/v1/retention/runs", "/api/v1/retention/integrity-findings", "/api/v1/audit-sink", "/api/v1/webhooks/stats", "/api/v1/metrics/traffic"}
	for _, path := range admin {
		expectSourceStatus(t, o.get(path), 200)
		expectSourceStatus(t, o.requestAs("GET", path, "", "", "", o.limited), 403)
		expectSourceStatus(t, o.requestAs("GET", path, "", "", "", ""), 401)
	}
	expectSourceStatus(t, o.requestAs("PUT", "/api/v1/settings", `{}`, "k", `"1"`, o.limited), 403)
	expectSourceStatus(t, o.requestAs("POST", "/api/v1/retention/runs", `{"dry_run":true}`, idk(), "", o.limited), 403)
	expectSourceStatus(t, o.requestAs("PUT", "/api/v1/audit-sink", `{"kind":"http","endpoint":"https://siem.example.test/x"}`, "k", `"1"`, o.limited), 403)
	// Unprivileged callers see no feeds or sources in stats/health, and cannot inspect others.
	feed := o.feed("authz-feed", "raw")
	src := o.source("authz-src", "local", "raw")
	page := decodeJSON[FeedStatsPage](t, o.requestAs("GET", "/api/v1/stats/feeds", "", "", "", o.limited))
	if len(page.Items) != 0 {
		t.Fatal("stats leaked feeds")
	}
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/sources/"+src.ID+"/health", "", "", "", o.limited), 404)
	expectSourceStatus(t, o.rawRequest("POST", "/api/v1/sources/"+src.ID+":test", "", o.limited), 404)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/metrics/traffic?scope=feed&ref="+feed.ID, "", "", "", o.limited), 404)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/permissions/effective?principal="+o.principal+"&resource=instance", "", "", "", o.limited), 403)
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/permissions/effective?principal="+o.limitID+"&resource=instance", "", "", "", o.limited), 200)
}

func TestStatusReadinessDetailIsAdminOnly(t *testing.T) {
	o := newOpsFixture(t)
	anon := httptest.NewRecorder()
	o.handler.ServeHTTP(anon, httptest.NewRequest("GET", "/api/v1/status", nil))
	if anon.Code != 200 || strings.Contains(anon.Body.String(), "readiness") {
		t.Fatalf("anonymous status leaked detail: %s", anon.Body.String())
	}
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/status", "", "", "", o.limited), 200)
	w := o.get("/api/v1/status")
	expectSourceStatus(t, w, 200)
	var st struct {
		Readiness *ReadinessDetail `json:"readiness"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Readiness == nil || st.Readiness.WriterLease.State != "held" {
		t.Fatal("admin status lacks readiness")
	}
	if strings.Contains(o.requestAs("GET", "/api/v1/status", "", "", "", o.limited).Body.String(), "readiness") {
		t.Fatal("non-admin saw readiness")
	}
}

func TestSettingsLifecycle(t *testing.T) {
	o := newOpsFixture(t)
	w := o.get("/api/v1/settings")
	expectSourceStatus(t, w, 200)
	s := decodeJSON[InstanceSettings](t, w)
	var env, def bool
	for _, it := range s.Items {
		env = env || it.Key == "public_url" && it.ReadOnly && it.Source == "env"
		def = def || it.Key == "session_idle_timeout_seconds" && it.Source == "default"
	}
	if !env || !def || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("unexpected settings %+v", s)
	}
	put := o.request("PUT", "/api/v1/settings", `{"session_idle_timeout_seconds":600}`, "s1", `"1"`)
	expectSourceStatus(t, put, 200)
	expectSourceStatus(t, o.request("PUT", "/api/v1/settings", `{"session_idle_timeout_seconds":600}`, "s1", `"1"`), 200)
	if o.count(`SELECT count(*) FROM audit_events WHERE action='settings.put'`) != 1 {
		t.Fatal("replay duplicated audit or audit missing")
	}
	expectSourceStatus(t, o.request("PUT", "/api/v1/settings", `{}`, "s2", `"1"`), 412)
	expectSourceStatus(t, o.request("PUT", "/api/v1/settings", `{}`, "s3", ""), 428)
	expectSourceStatus(t, o.request("PUT", "/api/v1/settings", `{"session_idle_timeout_seconds":99999}`, "s4", `"2"`), 400)
	expectSourceStatus(t, o.request("PUT", "/api/v1/settings", `{"public_url":"x"}`, "s5", `"2"`), 400)
	s = decodeJSON[InstanceSettings](t, o.get("/api/v1/settings"))
	if s.Revision != 2 {
		t.Fatalf("revision %d", s.Revision)
	}
}

func TestRetentionPolicyAndRuns(t *testing.T) {
	o := newOpsFixture(t)
	feed := o.feed("ret-feed", "raw")
	body := fmt.Sprintf(`{"feed_id":%q,"keep_last":3}`, feed.ID)
	w := o.request("POST", "/api/v1/retention/policies", body, "p1", "")
	expectSourceStatus(t, w, 201)
	pol := decodeJSON[RetentionPolicy](t, w)
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", body, "p1", ""), 201)
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", body, "p2", ""), 409)
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", `{"keep_last":3}`, "p3", ""), 400)
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/policies", fmt.Sprintf(`{"feed_id":%q}`, feed.ID), "p4", ""), 400)
	if o.count(`SELECT count(*) FROM retention_policies`) != 1 || o.count(`SELECT count(*) FROM audit_events WHERE action='retention.policy.create'`) != 1 {
		t.Fatal("create not atomic or not audited")
	}
	path := "/api/v1/retention/policies/" + pol.ID
	expectSourceStatus(t, o.request("PUT", path, `{"max_age_seconds":86400,"enabled":false}`, "u1", `"1"`), 200)
	expectSourceStatus(t, o.request("PUT", path, `{"max_age_seconds":86400}`, "u2", `"1"`), 412)
	got := decodeJSON[RetentionPolicyPage](t, o.get("/api/v1/retention/policies"))
	if len(got.Items) != 1 || got.Items[0].Enabled || got.Items[0].KeepLast != nil || got.Items[0].Revision != 2 {
		t.Fatalf("unexpected policy %+v", got.Items)
	}
	expectSourceStatus(t, o.request("DELETE", path, "", "d1", `"2"`), 200)
	expectSourceStatus(t, o.request("DELETE", path, "", "d2", `"2"`), 404)
	if o.count(`SELECT count(*) FROM audit_events WHERE action='retention.policy.delete'`) != 1 {
		t.Fatal("delete not audited")
	}
	// Maintenance unavailable without a service.
	expectSourceStatus(t, o.request("POST", "/api/v1/retention/runs", `{"dry_run":true}`, idk(), ""), 503)
	// Scheduled history is listed, newest first, reports only on the single-run endpoint.
	rep := retention.Report{DryRun: true, Retention: retention.RetentionReport{Deleted: []retention.Deletion{{SourceID: o.principal, Coordinate: "a", Version: "1", Reason: "keep_last"}}}}
	RecordRetentionRun(context.Background(), o.db, o.instance, "scheduled", rep, nil, time.Now().Add(-time.Minute))
	runs := decodeJSON[RetentionRunPage](t, o.get("/api/v1/retention/runs"))
	if len(runs.Items) != 1 || runs.Items[0].Report != nil || runs.Items[0].Trigger != "scheduled" {
		t.Fatalf("unexpected runs %+v", runs)
	}
	one := decodeJSON[RetentionRun](t, o.get("/api/v1/retention/runs/"+runs.Items[0].ID))
	if one.Report == nil || one.Report.VersionsDeleted != 1 || len(one.Report.Deleted) != 1 {
		t.Fatal("report missing")
	}
	// Integrity findings.
	var domain string
	_ = o.db.QueryRow(context.Background(), `INSERT INTO dedup_domains(instance_id,name) VALUES($1,'d') RETURNING id::text`, o.instance).Scan(&domain)
	_, _ = o.db.Exec(context.Background(), `INSERT INTO blob_integrity_findings(instance_id,dedup_domain_id,digest,kind) VALUES($1,$2,repeat('a',64),'missing')`, o.instance, domain)
	f := decodeJSON[IntegrityFindingPage](t, o.get("/api/v1/retention/integrity-findings"))
	if len(f.Items) != 1 || f.Items[0].Kind != "missing" {
		t.Fatal("finding missing")
	}
}

func TestWebhookRedeliverAndStats(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	var sub string
	if err := o.db.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) VALUES($1,'https://h.example.test/x','ref') RETURNING id::text`, o.instance).Scan(&sub); err != nil {
		t.Fatal(err)
	}
	var events [3]int64
	states := []string{"delivered", "dead", "pending"}
	for i, state := range states {
		var audit int64
		_ = o.db.QueryRow(ctx, `INSERT INTO audit_events(instance_id,action) VALUES($1,'x') RETURNING id`, o.instance).Scan(&audit)
		_ = o.db.QueryRow(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'x','{}') RETURNING id`, audit).Scan(&events[i])
		_, err := o.db.Exec(ctx, `INSERT INTO event_deliveries(subscription_id,event_id,state,attempts,last_failure) VALUES($1,$2,$3,5,'boom')`, sub, events[i], state)
		if err != nil {
			t.Fatal(err)
		}
	}
	stats := decodeJSON[WebhookStatsPage](t, o.get("/api/v1/webhooks/stats"))
	if stats.Overall.Delivered != 1 || stats.Overall.Failed != 1 || stats.Overall.Pending != 1 || stats.Overall.SuccessRate == nil || *stats.Overall.SuccessRate != 0.5 || len(stats.Items) != 1 {
		t.Fatalf("unexpected stats %+v", stats)
	}
	base := "/api/v1/webhooks/" + sub + "/deliveries/"
	w := o.rawRequest("POST", base+fmt.Sprint(events[1])+":redeliver", "r1", o.credential)
	expectSourceStatus(t, w, 200)
	if d := decodeJSON[WebhookDelivery](t, w); d.State != "pending" || d.Attempts != 0 {
		t.Fatalf("not reset: %+v", d)
	}
	expectSourceStatus(t, o.rawRequest("POST", base+fmt.Sprint(events[1])+":redeliver", "r1", o.credential), 200)
	if o.count(`SELECT count(*) FROM audit_events WHERE action='webhook.redeliver'`) != 1 {
		t.Fatal("redeliver audit missing or duplicated")
	}
	expectSourceStatus(t, o.rawRequest("POST", base+fmt.Sprint(events[2])+":redeliver", "r2", o.credential), 409)
	expectSourceStatus(t, o.rawRequest("POST", base+"99999:redeliver", "r3", o.credential), 404)
	expectSourceStatus(t, o.rawRequest("POST", base+fmt.Sprint(events[0])+":redeliver", "", o.credential), 400)
	expectSourceStatus(t, o.rawRequest("POST", base+fmt.Sprint(events[0])+":redeliver", "r4", o.limited), 403)
}

func TestEffectivePermissions(t *testing.T) {
	o := newOpsFixture(t)
	feed := o.feed("eff-feed", "raw")
	ctx := context.Background()
	var role string
	_ = o.db.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,'readers') RETURNING id::text`, o.instance).Scan(&role)
	_, _ = o.db.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)`, o.instance, o.limitID, role)
	_, _ = o.db.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$2,'feed','allow',$3,'{read,publish}')`, o.instance, role, feed.ID)
	query := func(resource string) []EffectiveAction {
		w := o.get("/api/v1/permissions/effective?principal=" + o.limitID + "&resource=" + resource)
		expectSourceStatus(t, w, 200)
		return decodeJSON[EffectivePermissions](t, w).Actions
	}
	allowed := func(actions []EffectiveAction, a identity.Action) EffectiveAction {
		for _, x := range actions {
			if x.Action == a {
				return x
			}
		}
		t.Fatal("missing action")
		return EffectiveAction{}
	}
	actions := query("feed:" + feed.ID)
	if r := allowed(actions, identity.Read); !r.Allowed || len(r.ViaRoles) != 1 || r.ViaRoles[0] != "readers" {
		t.Fatalf("read: %+v", r)
	}
	if allowed(actions, identity.Delete).Allowed || allowed(query("instance"), identity.Read).Allowed {
		t.Fatal("overgrant")
	}
	_, _ = o.db.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$2,'feed','deny',$3,'{publish}')`, o.instance, role, feed.ID)
	if r := allowed(query("feed:"+feed.ID), identity.Publish); r.Allowed || len(r.DeniedByRoles) != 1 {
		t.Fatalf("deny not explained: %+v", r)
	}
	expectSourceStatus(t, o.get("/api/v1/permissions/effective?principal="+o.limitID), 400)
	expectSourceStatus(t, o.get("/api/v1/permissions/effective?principal=00000000-0000-0000-0000-000000000001&resource=instance"), 404)
}

func TestAuditSinkConfigAndForwarding(t *testing.T) {
	o := newOpsFixture(t)
	w := o.get("/api/v1/audit-sink")
	expectSourceStatus(t, w, 200)
	if s := decodeJSON[AuditSink](t, w); s.Configured || s.Revision != 1 {
		t.Fatal("expected virtual unconfigured sink")
	}
	expectSourceStatus(t, o.request("PUT", "/api/v1/audit-sink", `{"kind":"http","endpoint":"http://siem.example.test/x"}`, "a0", `"1"`), 400)
	expectSourceStatus(t, o.request("PUT", "/api/v1/audit-sink", `{"kind":"syslog","endpoint":"tcp://127.0.0.1:514"}`, "a0b", `"1"`), 400)
	expectSourceStatus(t, o.request("PUT", "/api/v1/audit-sink", `{"kind":"syslog","endpoint":"tcp://siem.example.test:514","credential":"x"}`, "a0c", `"1"`), 400)
	secret := "siem-bearer-token-plaintext"
	put := o.request("PUT", "/api/v1/audit-sink", fmt.Sprintf(`{"kind":"http","endpoint":"https://siem.example.test/ingest","credential":%q}`, secret), "a1", `"1"`)
	expectSourceStatus(t, put, 200)
	sink := decodeJSON[AuditSink](t, put)
	if !sink.HasCredential || sink.Revision != 2 || strings.Contains(put.Body.String(), secret) {
		t.Fatalf("unexpected sink %+v", sink)
	}
	expectSourceStatus(t, o.request("PUT", "/api/v1/audit-sink", fmt.Sprintf(`{"kind":"http","endpoint":"https://siem.example.test/ingest","credential":%q}`, secret), "a1", `"1"`), 200)
	for _, q := range []string{`SELECT count(*) FROM encrypted_secrets WHERE envelope::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM audit_events WHERE row_to_json(audit_events)::text LIKE '%'||$1||'%'`, `SELECT count(*) FROM audit_sinks WHERE row_to_json(audit_sinks)::text LIKE '%'||$1||'%'`} {
		if o.count(q, secret) != 0 {
			t.Fatal("credential persisted in plaintext")
		}
	}
	// Forwarder: delivers events after the checkpoint, in order, with the decrypted credential.
	var seen []Event2
	var gotCredential string
	f := &auditsink.Forwarder{DB: o.db, InstanceID: o.instance, Keys: o.deps.Keys, Batch: 2,
		Send: func(_ context.Context, kind, endpoint, credential string, events []auditsink.Event) error {
			gotCredential = credential
			for _, e := range events {
				seen = append(seen, Event2{e.ID, e.Action})
			}
			return nil
		}}
	for i := 0; i < 3; i++ {
		_, _ = o.db.Exec(context.Background(), `INSERT INTO audit_events(instance_id,action) VALUES($1,$2)`, o.instance, fmt.Sprintf("test.event.%d", i))
	}
	total := 0
	for {
		n, err := f.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if gotCredential != secret || total < 3 || seen[len(seen)-1].Action != "test.event.2" {
		t.Fatalf("forwarding wrong: total=%d cred=%q", total, gotCredential)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].ID <= seen[i-1].ID {
			t.Fatal("out of order")
		}
	}
	// Failure keeps the checkpoint and records a coarse error class.
	_, _ = o.db.Exec(context.Background(), `INSERT INTO audit_events(instance_id,action) VALUES($1,'later')`, o.instance)
	f.Send = func(context.Context, string, string, string, []auditsink.Event) error {
		return fmt.Errorf("status 502")
	}
	if _, err := f.RunOnce(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	cur := decodeJSON[AuditSink](t, o.get("/api/v1/audit-sink"))
	if cur.LastError != "status 502" || cur.Checkpoint == 0 {
		t.Fatalf("unexpected state %+v", cur)
	}
	expectSourceStatus(t, o.request("DELETE", "/api/v1/audit-sink", "", "a2", `"3"`), 412)
	expectSourceStatus(t, o.request("DELETE", "/api/v1/audit-sink", "", "a3", fmt.Sprintf(`"%d"`, cur.Revision)), 200)
	if !strings.Contains(auditsink.FormatSyslog("h", auditsink.Event{ID: 1, Action: "a b", Time: time.Unix(0, 0)}), "<110>1 1970-01-01T00:00:00Z h kuvryn-ant - a_b") {
		t.Fatal("syslog format")
	}
}

type Event2 struct {
	ID     int64
	Action string
}

func TestTrafficProvenanceAndStats(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	feed := o.feed("tfeed", "raw")
	src := o.source("tsrc", "local", "raw")
	expectSourceStatus(t, o.request("PUT", "/api/v1/feeds/"+feed.ID+"/sources/"+src.ID, `{"priority":10}`, "att", `"1"`), 200)
	// One asset with provenance written the way the publish path does.
	var domain, blobID, component, version, asset string
	_ = o.db.QueryRow(ctx, `SELECT id::text FROM dedup_domains WHERE name='source:'||$1`, src.ID).Scan(&domain)
	_ = o.db.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,repeat('b',64),100,'k') RETURNING id::text`, o.instance, domain).Scan(&blobID)
	_ = o.db.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'pkg') RETURNING id::text`, o.instance, src.ID).Scan(&component)
	_ = o.db.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1.0') RETURNING id::text`, o.instance, component).Scan(&version)
	_ = o.db.QueryRow(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,provenance) VALUES($1,$2,'f.bin',$3,jsonb_build_object('source_id',$4::text,'principal_id',$5::text,'feed_id',$6::text,'token_id','11111111-1111-1111-1111-111111111111','matched_rule','rule-1')) RETURNING id::text`, o.instance, version, blobID, src.ID, o.principal, feed.ID).Scan(&asset)
	// Hot path only counts in memory; Flush persists in batch.
	tr := observability.NewTraffic()
	h := tr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("0123456789")) }))
	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/feeds/tfeed/raw/pkg/f.bin", nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", "/feeds/tfeed/raw/pkg/g.bin", strings.NewReader("abc")))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/feeds/unknown/raw/x", nil))
	tr.Cache(src.ID, true)
	tr.Cache(src.ID, true)
	tr.Cache(src.ID, false)
	tr.Download(asset)
	tr.Download(asset)
	if o.count(`SELECT count(*) FROM traffic_buckets`) != 0 {
		t.Fatal("request path wrote to the database")
	}
	if err := tr.Flush(ctx, o.db, o.instance); err != nil {
		t.Fatal(err)
	}
	series := decodeJSON[TrafficSeries](t, o.get("/api/v1/metrics/traffic?scope=feed&ref="+feed.ID+"&step=5m&from="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)))
	if series.Totals.Pulls != 3 || series.Totals.Publishes != 1 || series.Totals.BytesOut != 30 || series.Totals.BytesIn != 3 || len(series.Points) == 0 {
		t.Fatalf("feed traffic: %+v", series.Totals)
	}
	inst := decodeJSON[TrafficSeries](t, o.get("/api/v1/metrics/traffic"))
	if inst.Totals.Pulls < 4 || inst.Totals.CacheHits != 2 || inst.Totals.CacheMisses != 1 || inst.Totals.CacheHitRatio == nil {
		t.Fatalf("instance traffic: %+v", inst.Totals)
	}
	expectSourceStatus(t, o.get("/api/v1/metrics/traffic?scope=feed"), 400)
	expectSourceStatus(t, o.get("/api/v1/metrics/traffic?step=2m"), 400)
	// Stats.
	fs := decodeJSON[FeedStatsPage](t, o.get("/api/v1/stats/feeds"))
	if len(fs.Items) != 1 || fs.Items[0].Packages != 1 || fs.Items[0].Assets != 1 || fs.Items[0].Bytes != 100 || fs.Items[0].Pulls != 3 {
		t.Fatalf("feed stats %+v", fs.Items)
	}
	ss := decodeJSON[SourceStatsPage](t, o.get("/api/v1/stats/sources"))
	if len(ss.Items) != 1 || ss.Items[0].AttachedFeeds != 1 || ss.Items[0].Bytes != 100 || ss.Items[0].CacheHits != 2 {
		t.Fatalf("source stats %+v", ss.Items)
	}
	// Storage usage and dedup savings: a second asset sharing the blob.
	var v2 string
	_ = o.db.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'2.0') RETURNING id::text`, o.instance, component).Scan(&v2)
	_, _ = o.db.Exec(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'f.bin',$3)`, o.instance, v2, blobID)
	su := decodeJSON[StorageUsage](t, o.get("/api/v1/storage/usage"))
	if su.BlobBytes != 100 || su.LogicalBytes != 200 || su.DedupSavedBytes != 100 || su.UnreferencedBytes != 0 {
		t.Fatalf("usage %+v", su)
	}
	// Provenance with downloads.
	pv := decodeJSON[Provenance](t, o.get("/api/v1/assets/"+asset+"/provenance"))
	if pv.Downloads != 2 || pv.TokenID == "" || pv.MatchedRule != "rule-1" || pv.UploaderSubject == "" || pv.Verification != "verified" || pv.Origin != "local" {
		t.Fatalf("provenance %+v", pv)
	}
	_, _ = o.db.Exec(ctx, `INSERT INTO blob_integrity_findings(instance_id,dedup_domain_id,digest,kind) VALUES($1,$2,repeat('b',64),'digest')`, o.instance, domain)
	if pv = decodeJSON[Provenance](t, o.get("/api/v1/assets/"+asset+"/provenance")); pv.Verification != "integrity_failure" {
		t.Fatal("finding not reflected")
	}
	expectSourceStatus(t, o.requestAs("GET", "/api/v1/assets/"+asset+"/provenance", "", "", "", o.limited), 404)
}

func TestSourceHealthAndConnectionTest(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	remote := o.source("up", "remote", "raw")
	local := o.source("loc", "local", "raw")
	o.deps.Health.Record(remote.ID, 200, 40*time.Millisecond, nil)
	o.deps.Health.Record(remote.ID, 503, 900*time.Millisecond, nil)
	h := decodeJSON[SourceHealth](t, o.get("/api/v1/sources/"+remote.ID+"/health"))
	if h.Status != "degraded" || h.LastStatus == nil || *h.LastStatus != 503 || h.P95LatencyMS == nil || *h.P95LatencyMS != 900 || h.Requests != 2 || h.Failures != 1 || h.LastError != "upstream_error" {
		t.Fatalf("health %+v", h)
	}
	if decodeJSON[SourceHealth](t, o.get("/api/v1/sources/"+local.ID+"/health")).Status != "not_applicable" {
		t.Fatal("local source health")
	}
	if err := o.deps.Health.Flush(ctx, o.db, o.instance); err != nil {
		t.Fatal(err)
	}
	if o.count(`SELECT count(*) FROM remote_source_health WHERE requests=2 AND failures=1`) != 1 {
		t.Fatal("not persisted")
	}
	list := decodeJSON[SourceHealthPage](t, o.get("/api/v1/source-health"))
	if len(list.Items) != 1 || list.Items[0].SourceID != remote.ID {
		t.Fatalf("list %+v", list)
	}
	// Connection test: unconfigured -> 409; local -> 409; configured -> prober result and audit.
	expectSourceStatus(t, o.rawRequest("POST", "/api/v1/sources/"+remote.ID+":test", "", o.credential), 503) // no prober installed yet
	o.deps.Prober = func(_ context.Context, id, format string) (remoteruntime.ProbeResult, error) {
		return remoteruntime.ProbeResult{Reachable: true, Authenticated: true, StatusCode: 200, LatencyMS: 12}, nil
	}
	mux := http.NewServeMux()
	RegisterSources(mux, o.db, o.deps.Codec)
	RegisterIdentities(mux, o.db)
	RegisterOperations(mux, o.deps)
	o.handler = WithRequestID(mux)
	expectSourceStatus(t, o.rawRequest("POST", "/api/v1/sources/"+remote.ID+":test", "", o.credential), 409)
	expectSourceStatus(t, o.rawRequest("POST", "/api/v1/sources/"+local.ID+":test", "", o.credential), 409)
	_, err := o.db.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,config) VALUES($1,$2,'{}')`, o.instance, remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := o.rawRequest("POST", "/api/v1/sources/"+remote.ID+":test", "", o.credential)
	expectSourceStatus(t, w, 200)
	if r := decodeJSON[SourceTestResult](t, w); !r.Reachable || r.StatusCode == nil || *r.StatusCode != 200 {
		t.Fatalf("result %+v", r)
	}
	if o.count(`SELECT count(*) FROM audit_events WHERE action='source.test'`) != 1 {
		t.Fatal("test not audited")
	}
	expectSourceStatus(t, o.rawRequest("POST", "/api/v1/sources/"+remote.ID+":bogus", "", o.credential), 404)
}

// All route families must coexist in one ServeMux (registration panics on conflicts).
func TestAllRoutesRegisterTogether(t *testing.T) {
	o := newOpsFixture(t)
	mux := http.NewServeMux()
	codec := o.deps.Codec
	Register(mux)
	RegisterFeeds(mux, o.db, codec)
	RegisterSources(mux, o.db, codec)
	RegisterPublishPolicies(mux, o.db)
	RegisterPackages(mux, o.db, codec)
	RegisterRemoteSources(mux, o.db, o.deps.Keys, nil, make([]byte, 32))
	RegisterIdentities(mux, o.db)
	RegisterWebhooks(mux, o.db, o.deps.Keys)
	RegisterConsole(mux, o.db, codec)
	RegisterOperations(mux, o.deps)
}
