package management

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestPackageDeleteUnlistAndPolicyAPI(t *testing.T) {
	f := packageHTTPFixture(t)
	ctx := context.Background()
	one := f.seed(f.high, "team/pkg", "team", "1", "a", "")
	two := f.seed(f.high, "team/pkg", "team", "2", "b", "")
	versionPath := func(v string) string { return f.packages("/" + one.Component + "/versions/" + v) }

	// Read-only credential: not authorized to delete.
	if w := f.request("DELETE", versionPath("1"), "", idk(), ""); w.Code != 403 {
		t.Fatalf("read-only delete %d %s", w.Code, w.Body)
	}
	tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Delete, identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.credential = tok.Secret

	// Policy API round trip, then enforcement.
	put := f.request("PUT", "/api/v1/feeds/"+f.feed+"/delete-policy", `{"mode":"allowed","immutable_releases":true}`, idk(), "")
	if put.Code != 200 {
		t.Fatalf("set policy %d %s", put.Code, put.Body)
	}
	if w := f.request("DELETE", versionPath("1"), "", idk(), ""); w.Code != 409 {
		t.Fatalf("immutable %d %s", w.Code, w.Body)
	}
	if w := f.request("DELETE", versionPath("1")+"?action=unlist", "", idk(), ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"unchanged":false`) {
		t.Fatalf("unlist %d %s", w.Code, w.Body)
	}
	if w := f.request("PUT", "/api/v1/feeds/"+f.feed+"/delete-policy", `{"mode":"denied","immutable_releases":false}`, idk(), ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := f.request("DELETE", versionPath("1"), "", idk(), ""); w.Code != 403 {
		t.Fatalf("denied %d", w.Code)
	}
	if w := f.request("PUT", "/api/v1/feeds/"+f.feed+"/delete-policy", `{"mode":"bogus","immutable_releases":false}`, idk(), ""); w.Code != 400 {
		t.Fatalf("bad mode %d", w.Code)
	}
	if w := f.request("PUT", "/api/v1/feeds/"+f.feed+"/delete-policy", `{"mode":"allowed","immutable_releases":false}`, idk(), ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := f.request("DELETE", "/api/v1/feeds/"+f.feed+"/assets/"+two.Asset, "", idk(), ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"version_removed":true`) {
		t.Fatalf("asset delete %d %s", w.Code, w.Body)
	}
	if w := f.request("DELETE", versionPath("1"), "", idk(), ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"component_removed":true`) {
		t.Fatalf("version delete %d %s", w.Code, w.Body)
	}
	if w := f.request("DELETE", versionPath("1"), "", idk(), ""); w.Code != 404 {
		t.Fatalf("repeat %d", w.Code)
	}
	var audits, tombstones int
	if err = f.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_events WHERE action IN ('artifact.delete','artifact.unlist')),(SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='asset')`).Scan(&audits, &tombstones); err != nil || audits != 3 || tombstones != 2 {
		t.Fatal(audits, tombstones, err)
	}
}
