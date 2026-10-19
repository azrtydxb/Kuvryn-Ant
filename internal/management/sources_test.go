package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSourceJSONBoundary(t *testing.T) {
	for _, body := range []string{`{"NAME":"x"}`, `{"name":"x","name":"y"}`, `{"name":null}`, `{"name":"x","credentials":"secret"}`, `{"name":"x"} {}`, `[]`, `null`, `{"name":"` + strings.Repeat("x", 65536) + `"}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var input CreateSource
		if sourceJSON(httptest.NewRecorder(), r, &input, "name", "kind", "format") {
			t.Fatalf("invalid JSON accepted: %.80s", body)
		}
	}
	for _, name := range []string{"", " padded", "padded ", "nul\x00", strings.Repeat("x", 201), "\xff"} {
		if validSourceName(name) {
			t.Fatal("invalid source name accepted")
		}
	}
}

type sourceHTTPFixture struct {
	t                               *testing.T
	db                              *pgxpool.Pool
	instance, principal, credential string
	handler                         http.Handler
	router                          routers.Router
}

func newSourceHTTPFixture(t *testing.T) *sourceHTTPFixture {
	t.Helper()
	pool := feedFixture(t)
	boot, err := identity.Bootstrap(context.Background(), pool, "sources-test")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(context.Background(), boot.InstanceID, boot.PrincipalID, boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	codec := NewCursorCodec(make([]byte, 32))
	RegisterFeeds(mux, pool, codec)
	RegisterSources(mux, pool, codec)
	RegisterConsole(mux, pool, codec)
	RegisterPublishPolicies(mux, pool)
	doc, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &sourceHTTPFixture{t: t, db: pool, instance: boot.InstanceID, principal: boot.PrincipalID, credential: token.Secret, handler: WithRequestID(mux), router: router}
}
func (f *sourceHTTPFixture) request(method, path, body, key, revision string) *httptest.ResponseRecorder {
	return f.requestAs(method, path, body, key, revision, f.credential)
}
func (f *sourceHTTPFixture) requestAs(method, path, body, key, revision, credential string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if revision != "" {
		r.Header.Set("If-Match", revision)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, r)
	route, params, err := f.router.FindRoute(r)
	if err != nil {
		f.t.Fatal(err)
	}
	validation := (&openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route}, Status: response.Code, Header: response.Header()}).SetBodyBytes(response.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), validation); err != nil {
		f.t.Fatalf("%s %s response violates OpenAPI: %v; %s", method, path, err, response.Body.String())
	}
	return response
}
func expectSourceStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d expected=%d: %s", response.Code, status, response.Body.String())
	}
}
func (f *sourceHTTPFixture) source(name, kind, format string) Source {
	f.t.Helper()
	w := f.request("POST", "/api/v1/sources", fmt.Sprintf(`{"name":%q,"kind":%q,"format":%q}`, name, kind, format), "source-"+name, "")
	expectSourceStatus(f.t, w, 201)
	var source Source
	if err := json.Unmarshal(w.Body.Bytes(), &source); err != nil {
		f.t.Fatal(err)
	}
	return source
}
func (f *sourceHTTPFixture) feed(slug, format string) Feed {
	f.t.Helper()
	w := f.request("POST", "/api/v1/feeds", fmt.Sprintf(`{"slug":%q,"format":%q}`, slug, format), "feed-"+slug, "")
	expectSourceStatus(f.t, w, 201)
	var feed Feed
	if err := json.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		f.t.Fatal(err)
	}
	return feed
}
func (f *sourceHTTPFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func (f *sourceHTTPFixture) deny(kind, id string, actions []string) {
	f.t.Helper()
	column := "source_id"
	if kind == "feed" {
		column = "feed_id"
	}
	_, err := f.db.Exec(context.Background(), `INSERT INTO role_grants(instance_id,role_id,kind,effect,`+column+`,actions) SELECT instance_id,role_id,$2,'deny',$3,$4 FROM principal_roles WHERE principal_id=$1`, f.principal, kind, id, actions)
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestSourceManagementLifecycleAndContract(t *testing.T) {
	f := newSourceHTTPFixture(t)
	source := f.source("local-one", "local", "oci")
	if source.Revision != 1 || source.Format != "oci" {
		t.Fatal(source)
	}
	if f.count("SELECT count(*) FROM dedup_domains WHERE instance_id=$1 AND name=$2", f.instance, "source:"+source.ID) != 1 {
		t.Fatal("local source missing dedup domain")
	}
	replay := f.request("POST", "/api/v1/sources", `{"format":"oci","kind":"local","name":"local-one"}`, "source-local-one", "")
	expectSourceStatus(t, replay, 201)
	if f.count("SELECT count(*) FROM sources") != 1 || f.count("SELECT count(*) FROM audit_events WHERE action='source.create'") != 1 {
		t.Fatal("create replay duplicated mutation")
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/sources", `{"format":"oci","kind":"local","name":"different"}`, "source-local-one", ""), 409)
	expectSourceStatus(t, f.request("POST", "/api/v1/sources", `{"format":"oci","kind":"local","name":"local-one"}`, "duplicate-name", ""), 409)
	path := "/api/v1/sources/" + source.ID
	expectSourceStatus(t, f.request("GET", path, "", "", ""), 200)
	patch := f.request("PATCH", path, `{"name":"renamed"}`, "rename", `"1"`)
	expectSourceStatus(t, patch, 200)
	if patch.Header().Get("ETag") != `"2"` {
		t.Fatal("revision did not increment")
	}
	expectSourceStatus(t, f.request("PATCH", path, `{"name":"renamed"}`, "rename", `"1"`), 200)
	expectSourceStatus(t, f.request("PATCH", path, `{"name":"stale"}`, "stale", `"1"`), 412)
	expectSourceStatus(t, f.request("PATCH", path, `{"kind":"remote"}`, "mutate-kind", `"2"`), 400)
	expectSourceStatus(t, f.request("PATCH", path, `{"format":"raw"}`, "mutate-format", `"2"`), 400)
	f.source("remote-two", "remote", "oci")
	f.source("replica-three", "replica", "oci")
	if f.count("SELECT count(*) FROM dedup_domains WHERE name LIKE 'source:%'") != 1 {
		t.Fatal("non-local source created a dedup domain")
	}
	first := f.request("GET", "/api/v1/sources?limit=1", "", "", "")
	expectSourceStatus(t, first, 200)
	var page SourcePage
	json.Unmarshal(first.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Page.NextCursor == nil {
		t.Fatal("missing page")
	}
	second := f.request("GET", "/api/v1/sources?limit=1&cursor="+url.QueryEscape(*page.Page.NextCursor), "", "", "")
	expectSourceStatus(t, second, 200)
	var next SourcePage
	json.Unmarshal(second.Body.Bytes(), &next)
	if len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatal("pagination duplicated result")
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/sources?prefix=changed&cursor="+url.QueryEscape(*page.Page.NextCursor), "", "", ""), 400)
	expectSourceStatus(t, f.requestAs("GET", path, "", "", "", ""), 401)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	client, err := api.NewClientWithResponses(server.URL, api.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+f.credential)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ListSourcesWithResponse(context.Background(), nil)
	if err != nil || response.JSON200 == nil || len(response.JSON200.Items) != 3 {
		t.Fatalf("generated client: %v %+v", err, response)
	}
	f.deny("source", source.ID, []string{"read", "configure"})
	expectSourceStatus(t, f.request("GET", path, "", "", ""), 404)
	expectSourceStatus(t, f.request("POST", "/api/v1/sources", `{"format":"oci","kind":"local","name":"local-one"}`, "source-local-one", ""), 404)
	expectSourceStatus(t, f.request("PATCH", path, `{"name":"renamed"}`, "rename", `"1"`), 404)
}

func TestSourceAttachmentsOrderingRevisionAndPermissions(t *testing.T) {
	f := newSourceHTTPFixture(t)
	feed := f.feed("registry", "oci")
	a := f.source("a", "local", "oci")
	b := f.source("b", "remote", "oci")
	wrong := f.source("wrong", "local", "raw")
	base := "/api/v1/feeds/" + feed.ID + "/sources"
	path := base + "/" + a.ID
	scoped, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{FeedID: feed.ID, Actions: []identity.Action{identity.Read, identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("PUT", path, `{"priority":20}`, "feed-scope-only", `"1"`, scoped.Secret), 404)
	invisible := f.requestAs("GET", "/api/v1/sources", "", "", "", scoped.Secret)
	expectSourceStatus(t, invisible, 200)
	var invisiblePage SourcePage
	if err := json.Unmarshal(invisible.Body.Bytes(), &invisiblePage); err != nil {
		t.Fatal(err)
	}
	if len(invisiblePage.Items) != 0 || invisiblePage.Page.HasMore {
		t.Fatal("feed permission exposed sources")
	}

	attached := f.request("PUT", path, `{"priority":20}`, "attach-a", `"1"`)
	expectSourceStatus(t, attached, 200)
	if attached.Header().Get("ETag") != `"2"` {
		t.Fatal("feed revision missing")
	}
	replay := f.request("PUT", path, `{"priority":20}`, "attach-a", `"1"`)
	expectSourceStatus(t, replay, 200)
	if replay.Body.String() != attached.Body.String() {
		t.Fatal("attach replay changed result")
	}
	expectSourceStatus(t, f.request("PUT", base+"/"+b.ID, `{"priority":20}`, "duplicate-priority", `"2"`), 409)
	expectSourceStatus(t, f.request("PUT", base+"/"+wrong.ID, `{"priority":30}`, "wrong-format", `"2"`), 409)
	expectSourceStatus(t, f.request("PUT", base+"/"+b.ID, `{"priority":10}`, "attach-b", `"2"`), 200)
	list := f.request("GET", base+"?limit=1", "", "", "")
	expectSourceStatus(t, list, 200)
	var page AttachmentPage
	json.Unmarshal(list.Body.Bytes(), &page)
	if page.FeedRevision != 3 || len(page.Items) != 1 || page.Items[0].Source.ID != b.ID || page.Page.NextCursor == nil {
		t.Fatal(page)
	}
	cursor := *page.Page.NextCursor
	next := f.request("GET", base+"?limit=1&cursor="+url.QueryEscape(cursor), "", "", "")
	expectSourceStatus(t, next, 200)
	json.Unmarshal(next.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0].Source.ID != a.ID {
		t.Fatal(page)
	}
	expectSourceStatus(t, f.request("PUT", path, `{"priority":5}`, "reorder-a", `"3"`), 200)
	expectSourceStatus(t, f.request("GET", base+"?cursor="+url.QueryEscape(cursor), "", "", ""), 400)
	expectSourceStatus(t, f.request("DELETE", path, "", "stale-detach", `"3"`), 412)
	f.deny("source", b.ID, []string{"read", "configure"})
	hidden := f.request("GET", base+"?limit=1", "", "", "")
	expectSourceStatus(t, hidden, 200)
	json.Unmarshal(hidden.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0].Source.ID != a.ID || page.Page.HasMore {
		t.Fatal("hidden source leaked pagination", page)
	}
	expectSourceStatus(t, f.request("DELETE", base+"/"+b.ID, "", "deny-detach", `"4"`), 404)
	expectSourceStatus(t, f.request("PUT", base+"/"+b.ID, `{"priority":10}`, "attach-b", `"2"`), 404)
	expectSourceStatus(t, f.request("DELETE", path, "", "detach-a", `"4"`), 200)
	expectSourceStatus(t, f.request("DELETE", path, "", "detach-a", `"4"`), 200)
	if f.count("SELECT revision FROM feeds WHERE id=$1", feed.ID) != 5 {
		t.Fatal("wrong feed revision")
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action IN ('feed.source.attach','feed.source.detach')") != 4 || f.count("SELECT count(*) FROM outbox_events WHERE topic IN ('feed.source.attach','feed.source.detach')") != 4 {
		t.Fatal("attachment audit/event mismatch")
	}
	f.deny("feed", feed.ID, []string{"read", "configure"})
	expectSourceStatus(t, f.request("GET", base, "", "", ""), 404)
	expectSourceStatus(t, f.request("PUT", path, `{"priority":30}`, "feed-denied", `"5"`), 404)
}

func TestSourceMutationRollbackConcurrentCASAndInstanceIsolation(t *testing.T) {
	f := newSourceHTTPFixture(t)
	feed := f.feed("cas", "oci")
	source := f.source("cas", "local", "oci")
	base := "/api/v1/feeds/" + feed.ID + "/sources/" + source.ID
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			response := f.request("PUT", base, fmt.Sprintf(`{"priority":%d}`, i), "race-"+strconv.Itoa(i), `"1"`)
			statuses <- response.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	wins, stale := 0, 0
	for status := range statuses {
		if status == 200 {
			wins++
		} else if status == 412 {
			stale++
		} else {
			t.Fatal(status)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatal("CAS did not choose one winner")
	}
	var otherInstance, otherSource string
	ctx := context.Background()
	if err := f.db.QueryRow(ctx, "INSERT INTO instances(name) VALUES('other') RETURNING id").Scan(&otherInstance); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'outside','local','oci') RETURNING id", otherInstance).Scan(&otherSource); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/sources/"+otherSource, "", "", ""), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/feeds/"+feed.ID+"/sources/"+otherSource, `{"priority":9}`, "cross-instance", `"2"`), 404)
	if _, err := f.db.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT reject_source_events CHECK(topic NOT IN ('source.create','source.patch','feed.source.detach')) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/sources", `{"name":"rolled-back","kind":"local","format":"oci"}`, "rollback-create", ""), 503)
	expectSourceStatus(t, f.request("PATCH", "/api/v1/sources/"+source.ID, `{"name":"rolled-back"}`, "rollback-patch", `"1"`), 503)
	expectSourceStatus(t, f.request("DELETE", base, "", "rollback-detach", `"2"`), 503)
	if f.count("SELECT count(*) FROM sources WHERE instance_id=$1", f.instance) != 1 || f.count("SELECT count(*) FROM dedup_domains WHERE instance_id=$1 AND name LIKE 'source:%'", f.instance) != 1 || f.count("SELECT revision FROM feeds WHERE id=$1", feed.ID) != 2 || f.count("SELECT count(*) FROM feed_sources WHERE feed_id=$1", feed.ID) != 1 {
		t.Fatal("failed mutation partially committed")
	}
}
