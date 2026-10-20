package conan

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// conanThroughClientAuth mounts the Conan adapter behind the shared client
// authentication middleware exactly as cmd/ant does, and mints a user PAT.
func conanThroughClientAuth(t *testing.T) (http.Handler, string) {
	t.Helper()
	f := conanSetup(t)
	ctx := context.Background()
	var user string
	if err := f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','conan-user') RETURNING id`, f.instance).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) SELECT instance_id,$2,role_id FROM principal_roles WHERE instance_id=$1 AND principal_id=$3`, f.instance, user, f.principal); err != nil {
		t.Fatal(err)
	}
	_, pat, err := (identity.PATs{DB: f.pool}).Create(ctx, f.instance, user, "conan", []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ca := &identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1}
	return ca.Middleware(f.registry), pat
}

func conanDo(h http.Handler, path, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", authorization)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A user PAT presented the way `conan remote login` does (Basic) must yield a
// token the Conan client can keep using, not a single-use in-process ticket.
func TestConanRemoteLoginWithUserPAT(t *testing.T) {
	h, pat := conanThroughClientAuth(t)
	basic := "Basic " + base64Std("alice:"+pat)
	login := conanDo(h, "/feeds/packages/conan/v2/users/authenticate", basic)
	conanStatus(t, login, 200)
	token := login.Body.String()
	if token == "" {
		t.Fatal("no token returned")
	}
	for i := 0; i < 2; i++ { // reusable, not single use
		conanStatus(t, conanDo(h, "/feeds/packages/conan/v2/users/check_credentials", "Bearer "+token), 200)
	}
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

type conanDirectory struct{}

func (conanDirectory) VerifyPassword(_ context.Context, user string, pw []byte) (string, string, error) {
	if user == "alice" && string(pw) == "correct horse" {
		return "ldap-main", "uid-alice", nil
	}
	return "", "", identity.ErrUnauthenticated
}

// A directory user logging in with a password gets a reusable token, and a
// second login replaces the first so tokens do not accumulate.
func TestConanRemoteLoginWithDirectoryPassword(t *testing.T) {
	f := conanSetup(t)
	ctx := context.Background()
	var user string
	if err := f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','alice') RETURNING id`, f.instance).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) SELECT instance_id,$2,role_id FROM principal_roles WHERE instance_id=$1 AND principal_id=$3`, f.instance, user, f.principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,'ldap-main','uid-alice',$2)`, f.instance, user); err != nil {
		t.Fatal(err)
	}
	ca := &identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1, Directory: conanDirectory{}}
	h := ca.Middleware(f.registry)
	var first string
	for i := 0; i < 2; i++ {
		login := conanDo(h, "/feeds/packages/conan/v2/users/authenticate", "Basic "+base64Std("alice:correct horse"))
		conanStatus(t, login, 200)
		token := login.Body.String()
		conanStatus(t, conanDo(h, "/feeds/packages/conan/v2/users/check_credentials", "Bearer "+token), 200)
		if i == 0 {
			first = token
		}
	}
	conanStatus(t, conanDo(h, "/feeds/packages/conan/v2/users/check_credentials", "Bearer "+first), 401)
	conanStatus(t, conanDo(h, "/feeds/packages/conan/v2/users/authenticate", "Basic "+base64Std("alice:wrong")), 401)
}
