package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgconn"
)

// Feed metadata visibility must follow data-plane access. The data plane
// (OCI, raw) authorizes artifact resources, so a CI principal that holds only
// artifact grants (optionally namespace-limited) and a scoped token must still
// see the feeds it can push to, and nothing else.
func TestScopedTokenFeedVisibility(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	cache := o.feed("buildcache", "oci")
	sccache := o.feed("sccache", "raw")
	other := o.feed("releases", "oci")
	for _, id := range []string{cache.ID, sccache.ID} {
		mustExec(t, o.db, `UPDATE feeds SET purpose='cache' WHERE id=$1`, id)
	}
	all := []identity.Action{identity.Read, identity.Publish, identity.Delete}
	newCI := func(name string, grants func(roleID string)) string {
		t.Helper()
		p := createIdentityPrincipal(o.sourceHTTPFixture, name, "service")
		var role string
		if err := o.db.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,$2) RETURNING id::text`, o.instance, name).Scan(&role); err != nil {
			t.Fatal(err)
		}
		mustExec(t, o.db, `INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)`, o.instance, p.ID, role)
		grants(role)
		return p.ID
	}
	grant := func(role, feed, ns string) {
		mustExec(t, o.db, `INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,namespace,actions) VALUES($1,$2,'artifact','allow',$3,$4,ARRAY['read','publish','delete'])`, o.instance, role, feed, ns)
	}
	issue := func(principal string, scopes ...identity.Scope) string {
		t.Helper()
		tok, err := (identity.Tokens{DB: o.db}).Issue(ctx, o.instance, o.principal, principal, scopes, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return tok.Secret
	}
	ci := newCIPrincipal(t, newCI, grant, cache.ID, sccache.ID, "")
	nsLimited := newCIPrincipal(t, newCI, grant, cache.ID, sccache.ID, "org")
	none := newCI("noaccess", func(string) {})

	oidc := []identity.Scope{{Actions: []identity.Action{identity.Read}}, {FeedID: cache.ID, Actions: all}, {FeedID: sccache.ID, Actions: all}}
	tokens := map[string]string{
		"scoped":       issue(ci, oidc...),
		"ns grant":     issue(nsLimited, oidc...),
		"ns scope":     issue(ci, identity.Scope{FeedID: cache.ID, Namespace: "org", Actions: all}, identity.Scope{FeedID: sccache.ID, Actions: all}),
		"no access":    issue(none, oidc...),
		"wrong action": issue(ci, identity.Scope{FeedID: cache.ID, Actions: []identity.Action{identity.Delete}}),
	}
	list := func(token, query string) map[string]bool {
		t.Helper()
		w := o.requestAs("GET", "/api/v1/feeds"+query, "", "", "", token)
		expectSourceStatus(t, w, 200)
		got := map[string]bool{}
		for _, f := range decodeJSON[FeedPage](t, w).Items {
			got[f.ID] = true
		}
		return got
	}
	for _, name := range []string{"scoped", "ns grant", "ns scope"} {
		token := tokens[name]
		for _, q := range []string{"", "?prefix=buildcache", "?purpose=cache", "?purpose=cache&prefix=buildcache"} {
			got := list(token, q)
			if !got[cache.ID] || got[other.ID] {
				t.Fatalf("%s list %q: %v", name, q, got)
			}
		}
		if got := list(token, "?purpose=artifacts"); len(got) != 0 {
			t.Fatalf("%s purpose=artifacts leaked %v", name, got)
		}
		expectSourceStatus(t, o.requestAs("GET", "/api/v1/feeds/"+cache.ID, "", "", "", token), 200)
		expectSourceStatus(t, o.requestAs("GET", "/api/v1/feeds/"+other.ID, "", "", "", token), 404)
	}
	for _, name := range []string{"no access", "wrong action"} {
		token := tokens[name]
		for _, q := range []string{"", "?prefix=buildcache", "?purpose=cache", "?purpose=cache&prefix=buildcache"} {
			if got := list(token, q); len(got) != 0 {
				t.Fatalf("%s list %q leaked %v", name, q, got)
			}
		}
		for _, id := range []string{cache.ID, sccache.ID, other.ID} {
			expectSourceStatus(t, o.requestAs("GET", "/api/v1/feeds/"+id, "", "", "", token), http.StatusNotFound)
		}
	}
}

func newCIPrincipal(t *testing.T, newCI func(string, func(string)) string, grant func(role, feed, ns string), cache, sccache, ns string) string {
	t.Helper()
	name := "ci-" + ns
	return newCI(name, func(role string) {
		grant(role, cache, ns)
		grant(role, sccache, ns)
	})
}

func mustExec(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
