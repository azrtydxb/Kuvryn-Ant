package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// contractHTTP validates schema only. The real server still authenticates every
// bearer credential and resolves database roles; this stub grants no access.
type contractHTTP struct {
	t      *testing.T
	router routers.Router
	client *http.Client
}

func (c contractHTTP) Do(req *http.Request) (*http.Response, error) {
	c.t.Helper()
	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Fatal(err)
	}
	copyReq := req.Clone(req.Context())
	if req.GetBody != nil {
		copyReq.Body, err = req.GetBody()
		if err != nil {
			c.t.Fatal(err)
		}
		defer copyReq.Body.Close()
	}
	input := &openapi3filter.RequestValidationInput{Request: copyReq, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: func(context.Context, *openapi3filter.AuthenticationInput) error { return nil }}}
	if err = openapi3filter.ValidateRequest(req.Context(), input); err != nil {
		c.t.Fatalf("request contract %s %s: %v", req.Method, req.URL.Path, err)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	output := &openapi3filter.ResponseValidationInput{RequestValidationInput: input, Status: response.StatusCode, Header: response.Header, Options: &openapi3filter.Options{IncludeResponseStatus: true}}
	output.SetBodyBytes(body)
	if err = openapi3filter.ValidateResponse(req.Context(), output); err != nil {
		c.t.Fatalf("response contract %s %s status %d: %v", req.Method, req.URL.Path, response.StatusCode, err)
	}
	if len(response.Header.Get("X-Request-ID")) < 16 {
		c.t.Fatal("missing request correlation")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

func feedContractFixture(t *testing.T) (*pgxpool.Pool, identity.BootstrapIdentity, string) {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_feed_contract_%d", time.Now().UnixNano())
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
		t.Fatal("invalid fixture DSN")
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
	owner, err := identity.Bootstrap(ctx, pool, "API contract")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return pool, owner, token.Secret
}

func TestFeedGeneratedClientPostgresContract(t *testing.T) {
	pool, owner, secret := feedContractFixture(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	management.Register(mux)
	management.RegisterFeeds(mux, pool, management.NewCursorCodec(bytes.Repeat([]byte{7}, 32)))
	server := httptest.NewServer(management.WithRequestID(mux))
	defer server.Close()
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClientWithResponses(server.URL, api.WithHTTPClient(contractHTTP{t, router, server.Client()}))
	if err != nil {
		t.Fatal(err)
	}
	auth := func(raw string) api.RequestEditorFn {
		return func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+raw)
			return nil
		}
	}
	admin := auth(secret)
	body := api.CreateFeedJSONRequestBody{Slug: "test-one", Format: api.CreateFeedFormat("raw")}
	created, err := client.CreateFeedWithResponse(ctx, &api.CreateFeedParams{IdempotencyKey: "create-one"}, body, admin)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode() != 201 || created.JSON201 == nil {
		t.Fatalf("create: %d %s", created.StatusCode(), created.Body)
	}
	feed := *created.JSON201
	if feed.Revision != 1 || created.HTTPResponse.Header.Get("ETag") != `"1"` {
		t.Fatal("initial revision/etag mismatch")
	}
	replay, err := client.CreateFeedWithResponse(ctx, &api.CreateFeedParams{IdempotencyKey: "create-one"}, body, admin)
	if err != nil || replay.StatusCode() != 201 || replay.JSON201 == nil || replay.JSON201.Id != feed.Id {
		t.Fatalf("create replay: %v %+v", err, replay)
	}
	conflictBody := body
	conflictBody.Slug = "different"
	conflict, err := client.CreateFeedWithResponse(ctx, &api.CreateFeedParams{IdempotencyKey: "create-one"}, conflictBody, admin)
	if err != nil || conflict.StatusCode() != 409 || conflict.JSON409 == nil {
		t.Fatalf("fingerprint conflict: %v %+v", err, conflict)
	}
	get, err := client.GetFeedWithResponse(ctx, feed.Id, admin)
	if err != nil || get.StatusCode() != 200 || get.JSON200 == nil || get.JSON200.Id != feed.Id {
		t.Fatalf("get: %v %+v", err, get)
	}
	slug := "test-renamed"
	patch := api.UpdateFeedJSONRequestBody{Slug: &slug}
	params := &api.UpdateFeedParams{IdempotencyKey: "patch-one", IfMatch: `"1"`}
	updated, err := client.UpdateFeedWithResponse(ctx, feed.Id, params, patch, admin)
	if err != nil || updated.StatusCode() != 200 || updated.JSON200 == nil || updated.JSON200.Revision != 2 || updated.HTTPResponse.Header.Get("ETag") != `"2"` {
		t.Fatalf("patch: %v %+v", err, updated)
	}
	patchReplay, err := client.UpdateFeedWithResponse(ctx, feed.Id, params, patch, admin)
	if err != nil || patchReplay.StatusCode() != 200 || patchReplay.JSON200 == nil || patchReplay.JSON200.Revision != 2 {
		t.Fatalf("patch replay: %v %+v", err, patchReplay)
	}
	stale, err := client.UpdateFeedWithResponse(ctx, feed.Id, &api.UpdateFeedParams{IdempotencyKey: "patch-stale", IfMatch: `"1"`}, patch, admin)
	if err != nil || stale.StatusCode() != 412 || stale.JSON412 == nil {
		t.Fatalf("stale revision: %v %+v", err, stale)
	}
	second, err := client.CreateFeedWithResponse(ctx, &api.CreateFeedParams{IdempotencyKey: "create-two"}, api.CreateFeedJSONRequestBody{Slug: "test-two", Format: api.CreateFeedFormat("oci")}, admin)
	if err != nil || second.StatusCode() != 201 || second.JSON201 == nil {
		t.Fatalf("second create: %v %+v", err, second)
	}
	limit, prefix := 1, "test-"
	first, err := client.ListFeedsWithResponse(ctx, &api.ListFeedsParams{Limit: &limit, Prefix: &prefix}, admin)
	if err != nil || first.JSON200 == nil || len(first.JSON200.Items) != 1 || !first.JSON200.Page.HasMore || first.JSON200.Page.NextCursor == nil {
		t.Fatalf("first page: %v %+v", err, first)
	}
	next, err := client.ListFeedsWithResponse(ctx, &api.ListFeedsParams{Limit: &limit, Prefix: &prefix, Cursor: first.JSON200.Page.NextCursor}, admin)
	if err != nil || next.JSON200 == nil || len(next.JSON200.Items) != 1 || next.JSON200.Page.HasMore || next.JSON200.Items[0].Id == first.JSON200.Items[0].Id {
		t.Fatalf("next page: %v %+v", err, next)
	}
	literal := "test_%"
	empty, err := client.ListFeedsWithResponse(ctx, &api.ListFeedsParams{Prefix: &literal}, admin)
	if err != nil || empty.JSON200 == nil || len(empty.JSON200.Items) != 0 {
		t.Fatalf("literal prefix: %v %+v", err, empty)
	}
	scoped, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{FeedID: feed.Id.String(), Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	foreignCursor, err := client.ListFeedsWithResponse(ctx, &api.ListFeedsParams{Limit: &limit, Prefix: &prefix, Cursor: first.JSON200.Page.NextCursor}, auth(scoped.Secret))
	if err != nil || foreignCursor.StatusCode() != 400 {
		t.Fatalf("cursor crossed credential scope: %v %+v", err, foreignCursor)
	}
	visible, err := client.ListFeedsWithResponse(ctx, nil, auth(scoped.Secret))
	if err != nil || visible.JSON200 == nil || len(visible.JSON200.Items) != 1 || visible.JSON200.Items[0].Id != feed.Id || visible.JSON200.Page.HasMore {
		t.Fatalf("permission filtering: %v %+v", err, visible)
	}
	denied, err := client.CreateFeedWithResponse(ctx, &api.CreateFeedParams{IdempotencyKey: "forbidden"}, conflictBody, auth(scoped.Secret))
	if err != nil || denied.StatusCode() != 403 {
		t.Fatalf("readonly create: %v %+v", err, denied)
	}
	hidden, err := client.GetFeedWithResponse(ctx, second.JSON201.Id, auth(scoped.Secret))
	if err != nil || hidden.StatusCode() != 404 {
		t.Fatalf("hidden feed: %v %+v", err, hidden)
	}
	for _, editors := range [][]api.RequestEditorFn{nil, {auth("ant_" + strings.Repeat("A", 43))}} {
		unauthorized, err := client.ListFeedsWithResponse(ctx, nil, editors...)
		if err != nil || unauthorized.StatusCode() != 401 || unauthorized.JSON401 == nil {
			t.Fatalf("unauthorized listing: %v %+v", err, unauthorized)
		}
	}
	var mutations, audits int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM mutation_results").Scan(&mutations); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action IN ('feed.create','feed.patch')").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if mutations != 3 || audits != 3 {
		t.Fatalf("replay/stale request duplicated effects: mutations=%d audits=%d", mutations, audits)
	}
}
