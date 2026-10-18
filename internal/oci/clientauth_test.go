package oci

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

type ldapStub struct{}

func (ldapStub) VerifyPassword(_ context.Context, user string, pw []byte) (string, string, error) {
	if user == "alice" && string(pw) == "directory password" {
		return "ldap", "uid-alice", nil
	}
	return "", "", identity.ErrUnauthenticated
}

// TestDockerLoginFlowUnderClientAuthPolicy drives the registry the way docker
// does: an unauthenticated /v2/ probe that must challenge, then login and
// operations with a directory password, a personal access token or a service
// token, each gated by the feed's client authentication policy.
func TestDockerLoginFlowUnderClientAuthPolicy(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var user, role string
	if err := f.pool.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','alice') RETURNING id", f.instance).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,'images-rw') RETURNING id", f.instance).Scan(&role); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,'" + user + "','" + role + "')",
		"INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,'" + role + "','artifact','allow',ARRAY['read','publish'])",
		"INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,'ldap','uid-alice','" + user + "')",
	} {
		if _, err := f.pool.Exec(ctx, q, f.instance); err != nil {
			t.Fatal(err)
		}
	}
	ca := &identity.ClientAuth{DB: f.pool, InstanceID: f.instance, Directory: ldapStub{}, ResolveOCI: f.registry.ClientAuthTarget, CacheTTL: -1}
	srv := httptest.NewServer(ca.Middleware(f.registry))
	defer srv.Close()
	do := func(method, path, auth string) (int, string, http.Header) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 512)
		for {
			n, e := resp.Body.Read(buf)
			b.Write(buf[:n])
			if e != nil {
				break
			}
		}
		return resp.StatusCode, b.String(), resp.Header
	}
	basic := func(u, p string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)) }
	setPolicy := func(feed string, methods ...string) {
		t.Helper()
		var err error
		if feed == "" {
			_, err = f.pool.Exec(ctx, "DELETE FROM client_auth_policies WHERE feed_id=$1", f.feed)
		} else {
			_, err = f.pool.Exec(ctx, "INSERT INTO client_auth_policies(instance_id,feed_id,methods) VALUES($1,$2,$3) ON CONFLICT(instance_id,feed_id) WHERE feed_id IS NOT NULL DO UPDATE SET methods=excluded.methods", f.instance, feed, methods)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// 1. The credential-free probe challenges so docker knows to log in.
	status, body, header := do("GET", "/v2/", "")
	if status != 401 || !strings.HasPrefix(header.Get("WWW-Authenticate"), "Basic") || !strings.Contains(body, "UNAUTHORIZED") {
		t.Fatalf("probe: %d %v %s", status, header, body)
	}
	// 2. docker login with a directory user and password.
	if status, _, _ = do("GET", "/v2/", basic("alice", "directory password")); status != 200 {
		t.Fatalf("directory login: %d", status)
	}
	if status, _, _ = do("GET", "/v2/", basic("alice", "wrong")); status != 401 {
		t.Fatalf("bad password: %d", status)
	}
	// 3. docker login with a service token, and with a PAT under any username.
	if status, _, _ = do("GET", "/v2/", basic("service", f.token)); status != 200 {
		t.Fatalf("token login: %d", status)
	}
	pats := identity.PATs{DB: f.pool}
	_, readPAT, err := pats.Create(ctx, f.instance, user, "ro", []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, pushPAT, err := pats.Create(ctx, f.instance, user, "rw", []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ = do("GET", "/v2/", basic("anything", pushPAT)); status != 200 {
		t.Fatalf("PAT login: %d", status)
	}
	// Scopes still bind: a read-only PAT cannot start an upload, a push PAT can.
	upload := "/v2/images/app/blobs/uploads/"
	if status, _, _ = do("POST", upload, basic("u", readPAT)); status != 403 {
		t.Fatalf("read-only PAT upload: %d", status)
	}
	if status, _, _ = do("POST", upload, basic("u", pushPAT)); status != 202 {
		t.Fatalf("push PAT upload: %d", status)
	}
	if status, _, _ = do("POST", upload, basic("alice", "directory password")); status != 202 {
		t.Fatalf("directory user upload: %d", status)
	}
	// 4. Per-feed policy: only the listed methods log in to this feed.
	setPolicy(f.feed, "token")
	status, body, _ = do("GET", "/v2/images/app/manifests/latest", basic("alice", "directory password"))
	if status != 403 || !strings.Contains(body, "DENIED") {
		t.Fatalf("directory login to token-only feed: %d %s", status, body)
	}
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", basic("u", pushPAT)); status != 403 {
		t.Fatalf("PAT to token-only feed: %d", status)
	}
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", basic("service", f.token)); status != 404 {
		t.Fatalf("token to token-only feed should reach the registry: %d", status)
	}
	setPolicy(f.feed, "basic-directory", "pat")
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", basic("service", f.token)); status != 403 {
		t.Fatalf("service token to directory/pat-only feed: %d", status)
	}
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", basic("alice", "directory password")); status != 404 {
		t.Fatalf("directory user to directory feed: %d", status)
	}
	// 5. Anonymous reads follow the policy, and never apply to pushes.
	if _, err = f.pool.Exec(ctx, "UPDATE feeds SET anonymous_read=true WHERE id=$1", f.feed); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", ""); status != 401 {
		t.Fatalf("policy without anonymous must not serve anonymous reads: %d", status)
	}
	setPolicy(f.feed, "anonymous", "token")
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", ""); status != 404 {
		t.Fatalf("anonymous read of anonymous feed: %d", status)
	}
	if status, _, _ = do("POST", upload, ""); status != 401 {
		t.Fatalf("anonymous publish: %d", status)
	}
	if status, _, _ = do("GET", "/v2/images/app/manifests/latest", basic("alice", "directory password")); status != 403 {
		t.Fatalf("directory login when not allowed: %d", status)
	}
}
