package maven

import (
	"context"
	"strings"
	"testing"
)

func TestDeleteVersionAndAssetPG(t *testing.T) {
	f := setup(t)
	publishFixture(t, f, "1.0")
	publishFixture(t, f, "2.0")
	ctx := context.Background()

	// Immutable releases block deletion of released versions.
	if _, e := f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode,immutable_releases) VALUES($1,$2,'allowed',true)`, f.instance, f.feed); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", basePath+"1.0/", nil, nil), 409)
	assertCode(t, f.request("GET", basePath+"1.0/demo-1.0.jar", nil, nil), 200)
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET immutable_releases=false,mode='unlist_only'`); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", basePath+"1.0/", nil, nil), 409)
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET mode='allowed'`); e != nil {
		t.Fatal(e)
	}

	// Asset scope: removing one file keeps the version until its last asset goes.
	assertCode(t, f.request("DELETE", basePath+"1.0/demo-1.0.jar", nil, nil), 204)
	assertCode(t, f.request("GET", basePath+"1.0/demo-1.0.jar", nil, nil), 404)
	assertCode(t, f.request("GET", basePath+"1.0/demo-1.0.pom", nil, nil), 200)
	assertCode(t, f.request("DELETE", basePath+"1.0/maven-metadata.xml", nil, nil), 405)
	// Version scope.
	assertCode(t, f.request("DELETE", basePath+"2.0/", nil, nil), 204)
	assertCode(t, f.request("GET", basePath+"2.0/demo-2.0.pom", nil, nil), 404)
	assertCode(t, f.request("DELETE", basePath+"2.0/", nil, nil), 404)
	meta := f.request("GET", basePath+"maven-metadata.xml", nil, nil)
	assertCode(t, meta, 200)
	if strings.Contains(meta.Body.String(), "<version>2.0</version>") {
		t.Fatal(meta.Body.String())
	}
	var tombstones int
	if e := f.pool.QueryRow(ctx, `SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='asset' AND seq IS NOT NULL`).Scan(&tombstones); e != nil || tombstones != 3 {
		t.Fatal(tombstones, e)
	}
}
