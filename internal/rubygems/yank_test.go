package rubygems

import (
	"context"
	"testing"
)

func TestPostgresGemYank(t *testing.T) {
	f := setup(t)
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	assertCode(t, f.push(t, packageBytes(t, "example", "1.2.0", "ruby", "")), 201)
	assertCode(t, f.push(t, packageBytes(t, "example", "1.2.0", "arm64-darwin", "")), 201)
	ctx := context.Background()

	// Immutable releases refuse the yank outright and change nothing.
	if _, e := f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode,immutable_releases) VALUES($1,$2,'allowed',true)`, f.instance, f.feed); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", testBase+"api/v1/gems/yank", []byte("gem_name=example&version=1.2.0"), form), 409)
	assertCode(t, f.request("GET", testBase+"gems/example-1.2.0.gem", nil, nil), 200)
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET immutable_releases=false`); e != nil {
		t.Fatal(e)
	}

	assertCode(t, f.request("DELETE", testBase+"api/v1/gems/yank", []byte("gem_name=example&version=1.2.0&platform=arm64-darwin"), form), 200)
	assertCode(t, f.request("GET", testBase+"gems/example-1.2.0-arm64-darwin.gem", nil, nil), 404)
	assertCode(t, f.request("GET", testBase+"gems/example-1.2.0.gem", nil, nil), 200)
	assertCode(t, f.request("DELETE", testBase+"api/v1/gems/yank", []byte("gem_name=example&version=1.2.0&platform=arm64-darwin"), form), 404)
	assertCode(t, f.request("DELETE", testBase+"api/v1/gems/yank", []byte("gem_name=example&version=1.2.0"), form), 200)
	assertCode(t, f.request("GET", testBase+"info/example", nil, nil), 404)
	assertCode(t, f.request("DELETE", testBase+"api/v1/gems/yank", []byte("gem_name=bad/name&version=1"), form), 400)
	var audits int
	if e := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='artifact.delete'`).Scan(&audits); e != nil || audits != 2 {
		t.Fatal(audits, e)
	}
}
