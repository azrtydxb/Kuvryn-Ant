package nuget

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// `dotnet nuget push` and `delete` send the credential only in X-NuGet-ApiKey.
// The shared client-authentication middleware must resolve user PATs, service
// tokens and exchanged CI tokens from that header exactly as it does Bearer.
func TestApiKeyHeaderResolvesEveryCredentialType(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	h := (&identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1}).Middleware(f.registry)
	do := func(method, path string, body []byte, key string, ctype string) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		if key != "" {
			req.Header.Set("X-NuGet-ApiKey", key)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	push := func(id, key string) int {
		var body bytes.Buffer
		m := multipart.NewWriter(&body)
		p, _ := m.CreateFormFile("package", "ignored.nupkg")
		p.Write(packageBytes(t, id, "1.0.0", ""))
		m.Close()
		return do("PUT", testBase+"v2/package", body.Bytes(), key, m.FormDataContentType())
	}
	del := func(id, key string) int { return do("DELETE", testBase+"v2/package/"+id+"/1.0.0", nil, key, "") }
	read := func(id, user, password string) int {
		req := httptest.NewRequest("GET", testBase+"v3/flatcontainer/"+strings.ToLower(id)+"/index.json", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.SetBasicAuth(user, password)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
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
	rec, pat, err := pats.Create(ctx, f.instance, user, "nuget", scopes(identity.Read, identity.Publish, identity.Delete), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{FeedID: f.feed, Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, cred := range map[string]string{"service": f.token, "pat": pat, "exchanged": ci.Secret} {
		id := "Ok." + name
		if code := push(id, cred); code != 201 {
			t.Fatalf("%s push: %d", name, code)
		}
		if code := read(id, "u", cred); code != 200 {
			t.Fatalf("%s read: %d", name, code)
		}
		if code := del(id, cred); code != 204 {
			t.Fatalf("%s delete: %d", name, code)
		}
	}
	_, pushOnly, err := pats.Create(ctx, f.instance, user, "push", scopes(identity.Read, identity.Publish), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, ro, err := pats.Create(ctx, f.instance, user, "ro", scopes(identity.Read), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code := push("Scoped.Pkg", pushOnly); code != 201 {
		t.Fatalf("publish-scoped PAT push: %d", code)
	}
	if code := del("Scoped.Pkg", pushOnly); code != 403 {
		t.Fatalf("publish-scoped PAT delete: %d", code)
	}
	if code := push("Denied.Pkg", ro); code != 403 {
		t.Fatalf("read-only PAT push: %d", code)
	}
	for name, cred := range map[string]string{
		"bad pat":     "antp_" + strings.Repeat("A", 43),
		"bad service": "ant_" + strings.Repeat("x", 43),
		"garbage":     "not-a-key",
	} {
		if code := push("Nope.Pkg", cred); code != 401 && code != 403 {
			t.Fatalf("%s accepted: %d", name, code)
		}
	}
	if code := push("Nope.Pkg", ""); code != 401 {
		t.Fatalf("missing key: %d", code)
	}
	if err := pats.Revoke(ctx, f.instance, user, rec.ID); err != nil {
		t.Fatal(err)
	}
	if code := push("Revoked.Pkg", pat); code != 401 && code != 403 {
		t.Fatalf("revoked PAT accepted: %d", code)
	}
}
