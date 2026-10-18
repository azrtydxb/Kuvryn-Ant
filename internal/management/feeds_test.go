package management

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
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFeedJSONBoundary(t *testing.T) {
	for _, body := range []string{`{"SLUG":"a","format":"raw"}`, `{"slug":"a","Slug":"b","format":"raw"}`, `{"anonymous_Read":true}`, `{"slug":"a","format":"raw","unknown":true}`, `{"slug":"a"} {}`, `{"slug":"a","slug":"b"}`, `{"anonymous_read":null}`, `null`, `[]`, `{"slug":"` + strings.Repeat("a", 65536) + `"}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var v CreateFeed
		if strictJSON(w, r, &v) {
			t.Fatalf("accepted invalid body %.80s", body)
		}
		if w.Code < 400 {
			t.Fatalf("invalid error status %d", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"slug":"a","format":"raw"}`))
	r.Header.Set("Content-Type", "application/json")
	var v CreateFeed
	if !strictJSON(httptest.NewRecorder(), r, &v) || v.Slug != "a" {
		t.Fatal("valid body rejected")
	}
	for _, header := range []string{"", "*", "W/\"1\"", "1", "\"0\"", "\"01\"", "\"1\", \"2\""} {
		r.Header.Set("If-Match", header)
		if _, ok := revisionHeader(r); ok {
			t.Fatalf("accepted revision %q", header)
		}
	}
	r.Header.Set("If-Match", `"12"`)
	if n, ok := revisionHeader(r); !ok || n != 12 {
		t.Fatal("valid revision rejected")
	}
}
func feedFixture(t *testing.T) *pgxpool.Pool {
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
	schema := "ant_feeds_" + strings.ToLower(rand.Text())
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
func TestFeedManagementAtomicityPermissionsAndPagination(t *testing.T) {
	pool := feedFixture(t)
	ctx := context.Background()
	boot, err := identity.Bootstrap(ctx, pool, "feed-test")
	if err != nil {
		t.Fatal(err)
	}
	tokens := identity.Tokens{DB: pool}
	token, err := tokens.Issue(ctx, boot.InstanceID, boot.PrincipalID, boot.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterFeeds(mux, pool, NewCursorCodec(make([]byte, 32)))
	handler := WithRequestID(mux)
	request := func(method, path, body, key, revision, credential string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
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
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	create := request("POST", "/api/v1/feeds", `{"slug":"alpha","format":"raw"}`, "create-a", "", token.Secret)
	if create.Code != 201 {
		t.Fatalf("create %d %s", create.Code, create.Body.String())
	}
	var a Feed
	if err = json.Unmarshal(create.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.Revision != 1 || create.Header().Get("ETag") != `"1"` {
		t.Fatal("missing create revision")
	}
	replay := request("POST", "/api/v1/feeds", `{"slug":"alpha","format":"raw"}`, "create-a", "", token.Secret)
	if replay.Code != 201 || replay.Body.String() != create.Body.String() {
		t.Fatalf("replay %d %s", replay.Code, replay.Body.String())
	}
	conflict := request("POST", "/api/v1/feeds", `{"slug":"changed","format":"raw"}`, "create-a", "", token.Secret)
	if conflict.Code != 409 {
		t.Fatalf("fingerprint conflict %d", conflict.Code)
	}
	duplicate := request("POST", "/api/v1/feeds", `{"slug":"alpha","format":"raw"}`, "create-duplicate", "", token.Secret)
	if duplicate.Code != 409 {
		t.Fatalf("slug conflict %d %s", duplicate.Code, duplicate.Body.String())
	}
	patch := request("PATCH", "/api/v1/feeds/"+a.ID, `{"anonymous_read":true}`, "patch-a", `"1"`, token.Secret)
	if patch.Code != 200 || patch.Header().Get("ETag") != `"2"` {
		t.Fatalf("patch %d %s", patch.Code, patch.Body.String())
	}
	replay = request("PATCH", "/api/v1/feeds/"+a.ID, `{"anonymous_read":true}`, "patch-a", `"1"`, token.Secret)
	if replay.Code != 200 || replay.Body.String() != patch.Body.String() {
		t.Fatal("patch replay changed result")
	}
	stale := request("PATCH", "/api/v1/feeds/"+a.ID, `{"slug":"stale"}`, "patch-stale", `"1"`, token.Secret)
	if stale.Code != 412 {
		t.Fatalf("stale revision %d", stale.Code)
	}
	var events, outbox int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action IN ('feed.create','feed.patch')").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE topic IN ('feed.create','feed.patch')").Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if events != 2 || outbox != 2 {
		t.Fatalf("non-atomic or duplicate events audit=%d outbox=%d", events, outbox)
	}
	for _, slug := range []string{"beta", "gamma"} {
		w := request("POST", "/api/v1/feeds", fmt.Sprintf(`{"slug":%q,"format":"raw"}`, slug), "create-"+slug, "", token.Secret)
		if w.Code != 201 {
			t.Fatalf("create %s %d %s", slug, w.Code, w.Body.String())
		}
	}
	page1 := request("GET", "/api/v1/feeds?limit=1", "", "", "", token.Secret)
	if page1.Code != 200 {
		t.Fatalf("list %d %s", page1.Code, page1.Body.String())
	}
	var page FeedPage
	if err = json.Unmarshal(page1.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !page.Page.HasMore || page.Page.NextCursor == nil {
		t.Fatalf("pagination %+v", page)
	}
	page2 := request("GET", "/api/v1/feeds?limit=1&cursor="+*page.Page.NextCursor, "", "", "", token.Secret)
	var second FeedPage
	json.Unmarshal(page2.Body.Bytes(), &second)
	if page2.Code != 200 || len(second.Items) != 1 || second.Items[0].ID == page.Items[0].ID {
		t.Fatal("unstable keyset page")
	}
	tampered := request("GET", "/api/v1/feeds?prefix=other&cursor="+*page.Page.NextCursor, "", "", "", token.Secret)
	if tampered.Code != 400 {
		t.Fatal("cross-filter cursor accepted")
	}
	restricted, err := tokens.Issue(ctx, boot.InstanceID, boot.PrincipalID, boot.PrincipalID, []identity.Scope{{FeedID: a.ID, Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	limited := request("GET", "/api/v1/feeds", "", "", "", restricted.Secret)
	var visible FeedPage
	json.Unmarshal(limited.Body.Bytes(), &visible)
	if limited.Code != 200 || len(visible.Items) != 1 || visible.Items[0].ID != a.ID || visible.Page.HasMore {
		t.Fatalf("permission-filter leak: %d %+v", limited.Code, visible)
	}
	denied := request("POST", "/api/v1/feeds", `{"slug":"denied","format":"raw"}`, "denied", "", restricted.Secret)
	if denied.Code != 403 {
		t.Fatalf("scope escalation: %d", denied.Code)
	}
	noauth := request("GET", "/api/v1/feeds/"+a.ID, "", "", "", "")
	if noauth.Code != 401 {
		t.Fatalf("anonymous metadata leak %d", noauth.Code)
	}
	// Deny beats bootstrap allow, including replay of an earlier mutation.
	_, err = pool.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) SELECT instance_id,role_id,'feed','deny',$2,ARRAY['read','configure'] FROM principal_roles WHERE principal_id=$1`, boot.PrincipalID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	hidden := request("GET", "/api/v1/feeds/"+a.ID, "", "", "", token.Secret)
	if hidden.Code != 404 {
		t.Fatalf("denied feed visible %d", hidden.Code)
	}
	deniedCreateReplay := request("POST", "/api/v1/feeds", `{"slug":"alpha","format":"raw"}`, "create-a", "", token.Secret)
	if deniedCreateReplay.Code != 404 {
		t.Fatalf("create replay bypassed resource deny: %d %s", deniedCreateReplay.Code, deniedCreateReplay.Body.String())
	}
	deniedReplay := request("PATCH", "/api/v1/feeds/"+a.ID, `{"anonymous_read":true}`, "patch-a", `"1"`, token.Secret)
	if deniedReplay.Code != 404 {
		t.Fatalf("replay bypassed current policy %d", deniedReplay.Code)
	}
}
