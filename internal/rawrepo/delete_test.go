package rawrepo

import (
	"context"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestRawDeletePolicyGated(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	tok, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	as := map[string]string{"Authorization": "Bearer " + tok.Secret}
	if w := f.request("PUT", "team/a.bin", "payload", nil); w.Code != 201 {
		t.Fatal(w.Code)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.pool.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO delete_policies(instance_id,feed_id,namespace,mode,immutable_releases) VALUES($1,$2,'team','allowed',true)`, f.principal.InstanceID, f.feed)
	if w := f.request("DELETE", "team/a.bin", "", as); w.Code != 409 {
		t.Fatalf("immutable %d", w.Code)
	}
	exec(`UPDATE delete_policies SET mode='denied'`)
	if w := f.request("DELETE", "team/a.bin", "", as); w.Code != 403 {
		t.Fatalf("denied %d", w.Code)
	}
	exec(`DELETE FROM delete_policies`)
	if w := f.request("DELETE", "team/a.bin", "", as); w.Code != 204 {
		t.Fatalf("delete %d", w.Code)
	}
	if w := f.request("GET", "team/a.bin", "", nil); w.Code != 404 {
		t.Fatalf("deleted object still readable %d", w.Code)
	}
	if w := f.request("DELETE", "team/a.bin", "", as); w.Code != 404 {
		t.Fatalf("repeat %d", w.Code)
	}
	// The key can be republished after the delete.
	if w := f.request("PUT", "team/a.bin", "payload2", nil); w.Code != 201 {
		t.Fatalf("republish %d", w.Code)
	}
	var n int
	if e := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='artifact.delete'`).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
