package nuget

import (
	"context"
	"strings"
	"testing"
)

func TestPostgresNativeDeleteUnlistRelist(t *testing.T) {
	f := setup(t)
	key := map[string]string{"X-NuGet-ApiKey": f.token}
	ctx := context.Background()
	assertCode(t, f.push(t, packageBytes(t, "Example.Widget", "1.0.0", "")), 201)
	assertCode(t, f.push(t, packageBytes(t, "Example.Widget", "2.0.0", "")), 201)

	// Immutable release policy turns `dotnet nuget delete` into NuGet's own unlist.
	if _, e := f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode,immutable_releases) VALUES($1,$2,'allowed',true)`, f.instance, f.feed); e != nil {
		t.Fatal(e)
	}
	r := f.request("DELETE", testBase+"v2/package/Example.Widget/1.0.0", nil, key)
	assertCode(t, r, 204)
	if r.Header().Get("X-Ant-Action") != "unlist" {
		t.Fatal("expected unlist fallback")
	}
	leaf := f.request("GET", testBase+"v3/registration/example.widget/1.0.0.json", nil, nil)
	if !strings.Contains(leaf.Body.String(), `"listed":false`) {
		t.Fatal(leaf.Body.String())
	}
	reg := f.request("GET", testBase+"v3/registration/example.widget/index.json", nil, nil)
	if !strings.Contains(reg.Body.String(), `"listed":false`) || !strings.Contains(reg.Body.String(), `"listed":true`) {
		t.Fatal(reg.Body.String())
	}
	// Unlisted packages stay restorable by exact version.
	assertCode(t, f.request("GET", testBase+"v3/flatcontainer/example.widget/1.0.0/example.widget.1.0.0.nupkg", nil, nil), 200)
	assertCode(t, f.request("POST", testBase+"v2/package/Example.Widget/1.0.0", nil, key), 200)
	leaf = f.request("GET", testBase+"v3/registration/example.widget/1.0.0.json", nil, nil)
	if !strings.Contains(leaf.Body.String(), `"listed":true`) {
		t.Fatal(leaf.Body.String())
	}

	// With the protection lifted the same call really deletes.
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET immutable_releases=false`); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", testBase+"v2/package/Example.Widget/1.0.0", nil, key), 204)
	assertCode(t, f.request("GET", testBase+"v3/flatcontainer/example.widget/1.0.0/example.widget.1.0.0.nupkg", nil, nil), 404)
	assertCode(t, f.request("DELETE", testBase+"v2/package/Example.Widget/1.0.0", nil, key), 404)
	if r := f.request("GET", testBase+"v3/flatcontainer/example.widget/index.json", nil, nil); r.Body.String() != `{"versions":["2.0.0"]}` {
		t.Fatal(r.Body.String())
	}
	// Denied policy blocks every route.
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET mode='denied'`); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", testBase+"v2/package/Example.Widget/2.0.0", nil, key), 403)
}
