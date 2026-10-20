package ansiblerepo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func galaxyThroughClientAuth(t *testing.T, h http.Handler, authorization string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/feeds/collections/ansible/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", authorization)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

// ansible-galaxy sends `Authorization: Token <secret>`. Every credential the
// client-auth policy knows must work with that scheme, not only service tokens.
func TestGalaxyTokenSchemeAcceptsPersonalAccessTokens(t *testing.T) {
	f := galaxySetup(t)
	ctx := context.Background()
	var user string
	if err := f.pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','galaxy-user') RETURNING id`, f.instance).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) SELECT instance_id,$2,role_id FROM principal_roles WHERE instance_id=$1 AND principal_id=$3`, f.instance, user, f.principal); err != nil {
		t.Fatal(err)
	}
	_, pat, err := (identity.PATs{DB: f.pool}).Create(ctx, f.instance, user, "galaxy", []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ca := &identity.ClientAuth{DB: f.pool, InstanceID: f.instance, CacheTTL: -1}
	h := ca.Middleware(f.registry)
	if got := galaxyThroughClientAuth(t, h, "Token "+f.token); got != 200 {
		t.Fatalf("service token: %d", got)
	}
	if got := galaxyThroughClientAuth(t, h, "Token "+pat); got != 200 {
		t.Fatalf("personal access token: %d", got)
	}
	if got := galaxyThroughClientAuth(t, h, "Bearer "+pat); got != 200 {
		t.Fatalf("personal access token as Bearer: %d", got)
	}
	if got := galaxyThroughClientAuth(t, h, "Token antp_"+pat[5:len(pat)-1]+"x"); got != 401 {
		t.Fatalf("bad personal access token: %d", got)
	}
}
