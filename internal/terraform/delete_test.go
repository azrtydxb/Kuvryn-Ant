package terraform

import (
	"context"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestModuleAndProviderDeletePolicyGated(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tok, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	as := map[string]string{"Authorization": "Bearer " + tok.Secret}
	const base = testBase + "v1/modules/acme/vpc/aws/"
	good := moduleTarball(t, map[string]string{"main.tf": `resource "null_resource" "a" {}`})
	assertCode(t, f.request("PUT", base+"1.0.0", good, nil), 201)
	assertCode(t, f.request("PUT", base+"2.0.0", good, nil), 201)

	var feed string
	if err = f.pool.QueryRow(ctx, `SELECT id::text FROM feeds WHERE slug='packages'`).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode) VALUES($1,$2,'unlist_only')`, f.instance, feed); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("DELETE", base+"1.0.0", nil, as), 409)
	if _, err = f.pool.Exec(ctx, `UPDATE delete_policies SET mode='allowed'`); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("DELETE", base+"1.0.0", nil, as), 204)
	assertCode(t, f.request("GET", base+"1.0.0/download", nil, nil), 404)
	assertCode(t, f.request("GET", base+"2.0.0/download", nil, nil), 204)
	assertCode(t, f.request("DELETE", base+"1.0.0", nil, as), 404)
	assertCode(t, f.request("DELETE", testBase+"v1/providers/acme/demo/9.9.9", nil, as), 404)
}
