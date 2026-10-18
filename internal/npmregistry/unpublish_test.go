package npmregistry

import (
	"context"
	"strings"
	"testing"
)

func TestUnpublishThroughPolicyGatedCore(t *testing.T) {
	f := setup(t)
	base := "/feeds/crates/npm/"
	name := "@scope/demo"
	target := base + "@scope%2fdemo"
	assertCode(t, f.request("PUT", target, wire(name, "1.0.0", "latest", archive(t, name, "1.0.0", "")), nil), 201)
	assertCode(t, f.request("PUT", target, wire(name, "2.0.0", "next", archive(t, name, "2.0.0", "")), nil), 201)
	if r := f.request("GET", target, nil, nil); !strings.Contains(r.Body.String(), `"_rev"`) {
		t.Fatal("packument must carry _rev for npm unpublish")
	}

	// A policy that denies deletion blocks unpublish and changes nothing.
	ctx := context.Background()
	if _, e := f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode) VALUES($1,$2,'denied')`, f.instance, f.feed); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("PUT", target+"/-rev/1-ant", []byte(`{"versions":{"2.0.0":{}}}`), nil), 403)
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET mode='allowed',immutable_releases=true`); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", target+"/-rev/1-ant", nil, nil), 409)
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET immutable_releases=false`); e != nil {
		t.Fatal(e)
	}

	// `npm unpublish pkg@1.0.0`: packument PUT without the version, then the tarball DELETE.
	assertCode(t, f.request("PUT", target+"/-rev/1-ant", []byte(`{"versions":{"2.0.0":{}}}`), nil), 200)
	assertCode(t, f.request("DELETE", base+name+"/-/demo-1.0.0.tgz/-rev/1-ant", nil, nil), 200)
	assertCode(t, f.request("GET", base+name+"/-/demo-1.0.0.tgz", nil, nil), 404)
	assertCode(t, f.request("GET", base+name+"/-/demo-2.0.0.tgz", nil, nil), 200)

	// `npm unpublish pkg --force` removes everything; audit and tombstones follow.
	assertCode(t, f.request("DELETE", target+"/-rev/1-ant", nil, nil), 200)
	assertCode(t, f.request("GET", target, nil, nil), 404)
	assertCode(t, f.request("DELETE", target+"/-rev/1-ant", nil, nil), 404)
	var audits, tombs int
	if e := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_events WHERE action='artifact.delete'),(SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='asset')`).Scan(&audits, &tombs); e != nil || audits != 2 || tombs != 4 {
		t.Fatal(audits, tombs, e)
	}
}
