package cargo

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Cargo sends the registry token as a bare "Authorization: <token>". The shared
// client-authentication middleware must resolve user PATs, service tokens and
// exchanged CI tokens from that location exactly as it does for Bearer.
func TestRawAuthorizationResolvesEveryCredentialType(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	h := (&identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1}).Middleware(f.registry)
	base := "/feeds/crates/cargo/"
	do := func(method, path string, body []byte, auth string) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	publish := func(name, auth string) int {
		m := metadata{Name: name, Version: "1.0.0"}
		return do("PUT", base+"api/v1/crates/new", wire(m, crateArchive(t, name, m.Version, "")), auth)
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
	rec, pat, err := pats.Create(ctx, f.instance, user, "cargo", scopes(identity.Read, identity.Publish), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// An exchanged CI token is a short-lived scoped service token.
	ci, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{FeedID: f.feed, Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, cred := range map[string]string{"service": f.token, "pat": pat, "exchanged": ci.Secret} {
		if code := publish("ok-"+name, cred); code != 200 {
			t.Fatalf("%s publish: %d", name, code)
		}
		if code := do("GET", base+indexPath("ok-"+name), nil, cred); code != 200 {
			t.Fatalf("%s read: %d", name, code)
		}
	}
	// Out of scope: read-only PAT cannot publish; a token for another feed cannot read.
	_, ro, err := pats.Create(ctx, f.instance, user, "ro", scopes(identity.Read), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code := publish("denied", ro); code != 403 {
		t.Fatalf("read-only PAT publish: %d", code)
	}
	if code := do("GET", base+indexPath("ok-pat"), nil, ro); code != 200 {
		t.Fatalf("read-only PAT read: %d", code)
	}
	// Invalid values.
	for name, cred := range map[string]string{
		"bad pat":     "antp_" + strings.Repeat("A", 43),
		"bad service": "ant_" + strings.Repeat("x", 43),
		"garbage":     "not-a-token",
	} {
		if code := publish("nope", cred); code != 401 && code != 403 {
			t.Fatalf("%s accepted: %d", name, code)
		}
	}
	if code := publish("nope", ""); code != 401 {
		t.Fatalf("anonymous publish: %d", code)
	}
	// Revoked PAT stops working.
	if err := pats.Revoke(ctx, f.instance, user, rec.ID); err != nil {
		t.Fatal(err)
	}
	if code := publish("revoked", pat); code != 401 && code != 403 {
		t.Fatalf("revoked PAT accepted: %d", code)
	}
}
