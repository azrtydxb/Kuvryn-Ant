package npmregistry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestDistTagAddRemoveList(t *testing.T) {
	f := setup(t)
	base := "/feeds/crates/npm/"
	name := "@scope/demo"
	target := base + "@scope%2fdemo"
	assertCode(t, f.request("PUT", target, wire(name, "1.0.0", "latest", archive(t, name, "1.0.0", "")), nil), 201)
	assertCode(t, f.request("PUT", target, wire(name, "2.0.0", "next", archive(t, name, "2.0.0", "")), nil), 201)
	tags := base + "-/package/@scope%2fdemo/dist-tags"

	assertCode(t, f.request("PUT", tags+"/beta", []byte(`"1.0.0"`), nil), 200)
	if r := f.request("GET", tags, nil, nil); !strings.Contains(r.Body.String(), `"beta":"1.0.0"`) {
		t.Fatal(r.Body.String())
	}
	// Moving an existing tag, including latest, is allowed.
	assertCode(t, f.request("PUT", tags+"/latest", []byte(`"2.0.0"`), nil), 200)
	if r := f.request("GET", tags, nil, nil); !strings.Contains(r.Body.String(), `"latest":"2.0.0"`) || !strings.Contains(r.Body.String(), `"beta":"1.0.0"`) {
		t.Fatal(r.Body.String())
	}
	assertCode(t, f.request("PUT", tags+"/beta", []byte(`"9.9.9"`), nil), 404)
	assertCode(t, f.request("PUT", tags+"/beta", []byte(`not a version`), nil), 400)
	assertCode(t, f.request("PUT", tags+"/1.2.3", []byte(`"1.0.0"`), nil), 400)
	assertCode(t, f.request("PUT", base+"-/package/missing/dist-tags/x", []byte(`"1.0.0"`), nil), 404)

	assertCode(t, f.request("DELETE", tags+"/beta", nil, nil), 200)
	assertCode(t, f.request("DELETE", tags+"/beta", nil, nil), 404)
	if r := f.request("GET", tags, nil, nil); strings.Contains(r.Body.String(), "beta") {
		t.Fatal(r.Body.String())
	}
	var audits int
	if e := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action IN ('artifact.tag','artifact.untag')`).Scan(&audits); e != nil || audits != 3 {
		t.Fatal(audits, e)
	}

	// Publish-only tokens may manage tags; read-only tokens may not.
	issue := func(a ...identity.Action) string {
		tok, e := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: a}}, time.Now().Add(time.Hour))
		if e != nil {
			t.Fatal(e)
		}
		return tok.Secret
	}
	assertCode(t, f.request("PUT", tags+"/rc", []byte(`"1.0.0"`), map[string]string{"Authorization": "Bearer " + issue(identity.Read)}), 403)
	assertCode(t, f.request("PUT", tags+"/rc", []byte(`"1.0.0"`), map[string]string{"Authorization": "Bearer " + issue(identity.Read, identity.Publish)}), 200)
}
