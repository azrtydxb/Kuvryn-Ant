package swiftrepo

import (
	"context"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestReleaseDeletePolicyGated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	assertCode(t, publish(f, base+"Acme/Demo/1.0.0", goodZip(t, ""), meta, nil), 201)
	// The fixture token has no delete action.
	assertCode(t, f.request("DELETE", base+"acme/demo/1.0.0", nil, nil), 403)
	tok, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	as := map[string]string{"Authorization": "Bearer " + tok.Secret}
	if _, err = f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode,immutable_releases) VALUES($1,$2,'allowed',true)`, f.instance, f.feed); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("DELETE", base+"acme/demo/1.0.0", nil, as), 409)
	if _, err = f.pool.Exec(ctx, `UPDATE delete_policies SET immutable_releases=false`); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("DELETE", base+"acme/demo/1.0.0", nil, as), 204)
	assertCode(t, f.request("GET", base+"acme/demo/1.0.0", nil, nil), 404)
	assertCode(t, f.request("DELETE", base+"acme/demo/1.0.0", nil, as), 404)
	assertCode(t, f.request("DELETE", base+"acme/demo", nil, as), 405)
}
