package helmrepo

import (
	"context"
	"strings"
	"testing"
)

func TestChartDeletePostgres(t *testing.T) {
	f := newRepoFixture(t)
	h := New(f.pool, f.service)
	data := goodChart(t, "demo", "1.2.3")
	prov := provenance(data, "demo", "1.2.3")
	if w := f.upload(t, h, "demo", "1.2.3", data, prov); w.Code != 201 {
		t.Fatalf("publish %d", w.Code)
	}
	const path = "/feeds/charts/helm/charts/demo/1.2.3"
	ctx := context.Background()
	if _, e := f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode) VALUES($1,$2,'unlist_only')`, f.principal.InstanceID, f.feed); e != nil {
		t.Fatal(e)
	}
	if w := f.request(t, h, "DELETE", path, nil, ""); w.Code != 409 {
		t.Fatalf("unlist-only delete %d", w.Code)
	}
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET mode='allowed'`); e != nil {
		t.Fatal(e)
	}
	if w := f.request(t, h, "DELETE", path, nil, ""); w.Code != 200 {
		t.Fatalf("delete %d %s", w.Code, w.Body.String())
	}
	if w := f.request(t, h, "DELETE", path, nil, ""); w.Code != 404 {
		t.Fatalf("repeat delete %d", w.Code)
	}
	if w := f.request(t, h, "GET", "/feeds/charts/helm/index.yaml", nil, ""); strings.Contains(w.Body.String(), "1.2.3") {
		t.Fatalf("deleted chart still indexed: %s", w.Body.String())
	}
	var tombstones int
	if e := f.pool.QueryRow(ctx, `SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='asset' AND seq IS NOT NULL`).Scan(&tombstones); e != nil || tombstones != 2 {
		t.Fatal(tombstones, e)
	}
}
