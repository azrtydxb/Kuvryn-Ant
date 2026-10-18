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
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
)

func identityHTTPFixture(t *testing.T) *sourceHTTPFixture {
	f := newSourceHTTPFixture(t)
	tok, err := (identity.Tokens{DB: f.db}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterIdentities(mux, f.db)
	f.handler = WithRequestID(mux)
	f.credential = tok.Secret
	spec, err := api.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	f.router, err = legacy.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func createIdentityPrincipal(f *sourceHTTPFixture, name, kind string) PrincipalRecord {
	f.t.Helper()
	w := f.request("POST", "/api/v1/principals", fmt.Sprintf(`{"subject":%q,"kind":%q}`, name, kind), "principal-"+name, "")
	expectSourceStatus(f.t, w, 201)
	var item PrincipalRecord
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		f.t.Fatal(err)
	}
	return item
}
func createIdentityRole(f *sourceHTTPFixture, name string) RoleRecord {
	f.t.Helper()
	w := f.request("POST", "/api/v1/roles", fmt.Sprintf(`{"name":%q}`, name), "role-"+name, "")
	expectSourceStatus(f.t, w, 201)
	var item RoleRecord
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		f.t.Fatal(err)
	}
	return item
}
func TestIdentityAdministrationContract(t *testing.T) {
	f := identityHTTPFixture(t)
	ctx := context.Background()
	user := createIdentityPrincipal(f, "human", "user")
	service := createIdentityPrincipal(f, "robot", "service")
	role := createIdentityRole(f, "reader")
	replay := f.request("POST", "/api/v1/principals", `{"subject":"human","kind":"user"}`, "principal-human", "")
	expectSourceStatus(t, replay, 201)
	var replayed PrincipalRecord
	_ = json.Unmarshal(replay.Body.Bytes(), &replayed)
	if replayed.ID != user.ID {
		t.Fatal("create replay changed identity")
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/principals", `{"subject":"different","kind":"user"}`, "principal-human", ""), 409)
	grants := `{"grants":[{"kind":"artifact","effect":"allow","actions":["read"]},{"kind":"artifact","effect":"deny","namespace":"private","actions":["read"]}]}`
	path := "/api/v1/roles/" + role.ID + "/grants"
	changed := f.request("PUT", path, grants, "grant-update", `"1"`)
	expectSourceStatus(t, changed, 200)
	if changed.Header().Get("ETag") != `"2"` {
		t.Fatal("missing role revision")
	}
	expectSourceStatus(t, f.request("PUT", path, grants, "grant-update", `"1"`), 200)
	expectSourceStatus(t, f.request("PUT", path, grants, "grant-stale", `"1"`), 412)
	expectSourceStatus(t, f.request("PUT", path, grants, "grant-missing", ""), 428)
	membership := fmt.Sprintf(`{"role_ids":[%q]}`, role.ID)
	path = "/api/v1/principals/" + service.ID + "/roles"
	expectSourceStatus(t, f.request("PUT", path, membership, "membership", `"1"`), 200)
	expectSourceStatus(t, f.request("PUT", path, membership, "membership", `"1"`), 200)
	expectSourceStatus(t, f.request("PUT", path, membership, "membership-stale", `"1"`), 412)
	tokenBody := fmt.Sprintf(`{"scopes":[{"actions":["read"]}],"expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	path = "/api/v1/principals/" + service.ID + "/tokens"
	expectSourceStatus(t, f.request("POST", path, tokenBody, "forbidden-cache", ""), 400)
	issued := f.request("POST", path, tokenBody, "", "")
	expectSourceStatus(t, issued, 201)
	if issued.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("secret response cacheable")
	}
	var token struct{ ID, Secret string }
	_ = json.Unmarshal(issued.Body.Bytes(), &token)
	p, err := (identity.Tokens{DB: f.db}).Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := (identity.Roles{DB: f.db}).Resolve(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: f.instance, Kind: identity.ArtifactResource}, identity.Read) || policy.Authorize(p, identity.Resource{InstanceID: f.instance, Kind: identity.ArtifactResource, Namespace: "private"}, identity.Read) {
		t.Fatal("grant/explicit deny ineffective")
	}
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/roles", "", "", "", token.Secret), 403)
	if f.count(`SELECT count(*) FROM mutation_results WHERE response::text LIKE '%'||$1||'%'`, token.Secret) != 0 || f.count(`SELECT count(*) FROM audit_events WHERE row_to_json(audit_events)::text LIKE '%'||$1||'%'`, token.Secret) != 0 || f.count(`SELECT count(*) FROM outbox_events WHERE payload::text LIKE '%'||$1||'%'`, token.Secret) != 0 {
		t.Fatal("secret persisted in replay/audit/outbox")
	}
	expectSourceStatus(t, f.request("DELETE", "/api/v1/tokens/"+token.ID, "", "revoke", ""), 200)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/tokens/"+token.ID, "", "revoke", ""), 200)
	if _, err = (identity.Tokens{DB: f.db}).Authenticate(ctx, token.Secret); err == nil {
		t.Fatal("revoked credential authenticated")
	}
	binding := fmt.Sprintf(`{"issuer":"https://id.example","subject":"external-subject","principal_id":%q}`, user.ID)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-bindings", binding, "bind", ""), 201)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-bindings", binding, "bind", ""), 201)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-bindings", binding, "bind-other", ""), 409)
	expectSourceStatus(t, f.request("POST", "/api/v1/identity-bindings", strings.ReplaceAll(binding, user.ID, service.ID), "bind-service", ""), 404)
	page := f.request("GET", "/api/v1/principals?limit=1", "", "", "")
	expectSourceStatus(t, page, 200)
	var list struct {
		Items []PrincipalRecord `json:"items"`
		Next  string            `json:"next_after"`
	}
	_ = json.Unmarshal(page.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Next == "" {
		t.Fatal("pagination missing")
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/principals?limit=1&after="+list.Next, "", "", ""), 200)
	expectSourceStatus(t, f.request("GET", "/api/v1/roles", "", "", ""), 200)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/principals", "", "", "", ""), 401)
	expectSourceStatus(t, f.request("GET", "/api/v1/principals?limit=201", "", "", ""), 400)
	if n := f.count(`SELECT count(*) FROM audit_events WHERE action IN ('principal.create','role.create','role.grants','principal.roles','token.revoke','identity.bind')`); n != 7 {
		t.Fatalf("metadata audit count %d", n)
	}
	if n := f.count(`SELECT count(*) FROM audit_events a LEFT JOIN outbox_events o ON o.audit_id=a.id WHERE o.id IS NULL`); n != 0 {
		t.Fatal("audit missing outbox")
	}
}
func TestIdentityAdministrationIsolationAndAtomicity(t *testing.T) {
	f := identityHTTPFixture(t)
	ctx := context.Background()
	user := createIdentityPrincipal(f, "local", "user")
	role := createIdentityRole(f, "local")
	var other, foreignRole, foreignPrincipal, foreignFeed string
	if err := f.db.QueryRow(ctx, `INSERT INTO instances(name) VALUES('foreign') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,'foreign') RETURNING id::text`, other).Scan(&foreignRole); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO principals(instance_id,subject,kind) VALUES($1,'foreign','user') RETURNING id::text`, other).Scan(&foreignPrincipal); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'foreign','raw') RETURNING id::text`, other).Scan(&foreignFeed); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("PUT", "/api/v1/principals/"+user.ID+"/roles", fmt.Sprintf(`{"role_ids":[%q]}`, foreignRole), "cross-role", `"1"`), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/principals/"+foreignPrincipal+"/roles", `{"role_ids":[]}`, "cross-principal", `"1"`), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/roles/"+role.ID+"/grants", fmt.Sprintf(`{"grants":[{"kind":"artifact","effect":"allow","feed_id":%q,"actions":["read"]}]}`, foreignFeed), "cross-feed", `"1"`), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/roles/"+foreignRole+"/grants", `{"grants":[]}`, "foreign-role", `"1"`), 404)
	if n := f.count(`SELECT count(*) FROM principals WHERE id=$1 AND revision=1`, user.ID); n != 1 {
		t.Fatal("failed cross-instance mutation advanced revision")
	}
	// An administrator's configure permission alone and feed-scoped admin token
	// never imply instance administration.
	for _, scope := range []identity.Scope{{Actions: []identity.Action{identity.Configure}}, {FeedID: foreignFeed, Actions: []identity.Action{identity.Admin}}} {
		if scope.FeedID != "" {
			var localFeed string
			if err := f.db.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'local','raw') RETURNING id::text`, f.instance).Scan(&localFeed); err != nil {
				t.Fatal(err)
			}
			scope.FeedID = localFeed
		}
		tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{scope}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		expectSourceStatus(t, f.requestAs("POST", "/api/v1/roles", `{"name":"escalate"}`, "deny", "", tok.Secret), 403)
	}
	if _, err := f.db.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT reject_identity_test CHECK(topic<>'principal.create') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/principals", `{"subject":"rollback","kind":"user"}`, "rollback", ""), 503)
	if n := f.count(`SELECT count(*) FROM principals WHERE subject='rollback'`); n != 0 {
		t.Fatal("audit failure leaked principal")
	}
	if _, err := f.db.Exec(ctx, `ALTER TABLE outbox_events DROP CONSTRAINT reject_identity_test`); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/principals", `{"subject":"rollback","kind":"user"}`, "rollback", ""), 201)
	// Explicit instance deny defeats the existing bootstrap allow.
	if _, err := f.db.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) SELECT instance_id,role_id,'instance','deny',ARRAY['admin'] FROM principal_roles WHERE principal_id=$1`, f.principal); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/roles", "", "", ""), 403)
}
func TestIdentityAdministrationConcurrentCAS(t *testing.T) {
	f := identityHTTPFixture(t)
	role := createIdentityRole(f, "concurrent")
	const count = 6
	codes := make(chan int, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest("PUT", "/api/v1/roles/"+role.ID+"/grants", strings.NewReader(`{"grants":[]}`))
			r.Header.Set("Authorization", "Bearer "+f.credential)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("If-Match", `"1"`)
			r.Header.Set("Idempotency-Key", fmt.Sprint("race-", i))
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			codes <- w.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	won := 0
	for code := range codes {
		if code == 200 {
			won++
		} else if code != 412 {
			t.Fatalf("unexpected CAS result %d", code)
		}
	}
	if won != 1 {
		t.Fatalf("CAS winners=%d", won)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action='role.grants' AND resource_id=$1`, role.ID) != 1 {
		t.Fatal("CAS duplicate effects")
	}
}
func TestIdentityAdministrationStrictInput(t *testing.T) {
	f := identityHTTPFixture(t)
	role := createIdentityRole(f, "validation")
	for _, body := range []string{`{"subject":"a","kind":"emergency"}`, `{"subject":"a","kind":"user","disabled":false}`, `{"subject":"a","subject":"b","kind":"user"}`, `{"subject":null,"kind":"user"}`, `{"subject":"a","kind":"USER"}`} {
		expectSourceStatus(t, f.request("POST", "/api/v1/principals", body, "bad", ""), 400)
	}
	for _, body := range []string{`{"grants":null}`, `{"grants":[{"kind":"instance","effect":"allow","actions":["read","read"]}]}`, `{"grants":[{"kind":"instance","effect":"allow","effect":"deny","actions":["admin"]}]}`, `{"grants":[{"kind":"instance","effect":"allow","namespace":"x","actions":["admin"]}]}`, `{"grants":[{"kind":"instance","effect":"allow","actions":["root"]}]}`, `{"grants":[{"Kind":"instance","effect":"allow","actions":["admin"]}]}`} {
		expectSourceStatus(t, f.request("PUT", "/api/v1/roles/"+role.ID+"/grants", body, "bad", `"1"`), 400)
	}
}

type identityContractTransport struct{ f *sourceHTTPFixture }

func (v identityContractTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	route, params, err := v.f.router.FindRoute(r)
	if err != nil {
		return nil, err
	}
	input := &openapi3filter.RequestValidationInput{Request: r, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	if err = openapi3filter.ValidateRequest(r.Context(), input); err != nil {
		return nil, err
	}
	w := httptest.NewRecorder()
	v.f.handler.ServeHTTP(w, r)
	output := (&openapi3filter.ResponseValidationInput{RequestValidationInput: input, Status: w.Code, Header: w.Header()}).SetBodyBytes(w.Body.Bytes())
	if err = openapi3filter.ValidateResponse(r.Context(), output); err != nil {
		return nil, err
	}
	return w.Result(), nil
}
func TestIdentityAdministrationGeneratedClient(t *testing.T) {
	f := identityHTTPFixture(t)
	client, err := api.NewClientWithResponses("http://example.test", api.WithHTTPClient(&http.Client{Transport: identityContractTransport{f}}), api.WithRequestEditorFn(func(ctx context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+f.credential)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreatePrincipalWithResponse(context.Background(), &api.CreatePrincipalParams{IdempotencyKey: "client"}, api.CreatePrincipalJSONRequestBody{Subject: "client-user", Kind: api.CreatePrincipalKindUser})
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON201 == nil || response.JSON201.Subject != "client-user" {
		t.Fatalf("generated create: %s", response.Body)
	}
	list, err := client.ListPrincipalsWithResponse(context.Background(), &api.ListPrincipalsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if list.JSON200 == nil || len(list.JSON200.Items) != 2 {
		t.Fatalf("generated list: %s", list.Body)
	}
}
