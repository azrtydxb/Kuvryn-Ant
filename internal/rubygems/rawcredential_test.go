package rubygems

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// `gem push` and `gem yank` send the API key as a bare "Authorization: <key>".
// The shared client-authentication middleware must resolve user PATs, service
// tokens and exchanged CI tokens from that location exactly as it does Bearer.
func TestRawAuthorizationResolvesEveryCredentialType(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	h := (&identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1}).Middleware(f.registry)
	do := func(method, path string, body []byte, auth string, form bool) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if form {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	push := func(name, auth string) int {
		return do("POST", testBase+"api/v1/gems", packageBytes(t, name, "1.0.0", "ruby", ""), auth, false)
	}
	yank := func(name, auth string) int {
		return do("DELETE", testBase+"api/v1/gems/yank", []byte("gem_name="+name+"&version=1.0.0"), auth, true)
	}
	scopes := func(a ...identity.Action) []identity.Scope { return []identity.Scope{{Actions: a}} }
	// PATs belong to user principals: give one the bootstrap owner's roles.
	var user string
	if err := f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','pat-user') RETURNING id`, f.instance).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) SELECT instance_id,$2,role_id FROM principal_roles WHERE instance_id=$1 AND principal_id=$3`, f.instance, user, f.principal); err != nil {
		t.Fatal(err)
	}
	pats := identity.PATs{DB: f.pool}
	rec, pat, err := pats.Create(ctx, f.instance, user, "gem", scopes(identity.Read, identity.Publish, identity.Delete), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{FeedID: f.feed, Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, cred := range map[string]string{"service": f.token, "pat": pat, "exchanged": ci.Secret} {
		gem := "ok" + name
		if code := push(gem, cred); code != 201 {
			t.Fatalf("%s push: %d", name, code)
		}
		if code := do("GET", testBase+"gems/"+gem+"-1.0.0.gem", nil, cred, false); code != 200 {
			t.Fatalf("%s read: %d", name, code)
		}
		if code := yank(gem, cred); code != 200 {
			t.Fatalf("%s yank: %d", name, code)
		}
	}
	// Out of scope: a publish-only PAT can push but not yank; read-only cannot push.
	_, pushOnly, err := pats.Create(ctx, f.instance, user, "push", scopes(identity.Read, identity.Publish), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, ro, err := pats.Create(ctx, f.instance, user, "ro", scopes(identity.Read), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code := push("scoped", pushOnly); code != 201 {
		t.Fatalf("publish-scoped PAT push: %d", code)
	}
	if code := yank("scoped", pushOnly); code != 403 {
		t.Fatalf("publish-scoped PAT yank: %d", code)
	}
	if code := push("denied", ro); code != 403 {
		t.Fatalf("read-only PAT push: %d", code)
	}
	for name, cred := range map[string]string{
		"bad pat":     "antp_" + strings.Repeat("A", 43),
		"bad service": "ant_" + strings.Repeat("x", 43),
		"garbage":     "not-a-key",
	} {
		if code := push("nope", cred); code != 401 && code != 403 {
			t.Fatalf("%s accepted: %d", name, code)
		}
	}
	if code := push("nope", ""); code != 401 {
		t.Fatalf("anonymous push: %d", code)
	}
	if err := pats.Revoke(ctx, f.instance, user, rec.ID); err != nil {
		t.Fatal(err)
	}
	if code := push("revoked", pat); code != 401 && code != 403 {
		t.Fatalf("revoked PAT accepted: %d", code)
	}
}
