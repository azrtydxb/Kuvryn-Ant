package management

import (
	"context"
	"testing"
)

func TestSecurityResultIngestStoresAndEmitsEvent(t *testing.T) {
	o := newOpsFixture(t)
	ctx := context.Background()
	feed := o.feed("sfeed", "raw")
	src := o.source("ssrc", "local", "raw")
	expectSourceStatus(t, o.request("PUT", "/api/v1/feeds/"+feed.ID+"/sources/"+src.ID, `{"priority":10}`, "att-s", `"1"`), 200)
	var domain, blobID, component, version, asset string
	_ = o.db.QueryRow(ctx, `SELECT id::text FROM dedup_domains WHERE name='source:'||$1`, src.ID).Scan(&domain)
	_ = o.db.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,repeat('c',64),100,'k') RETURNING id::text`, o.instance, domain).Scan(&blobID)
	_ = o.db.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'pkg') RETURNING id::text`, o.instance, src.ID).Scan(&component)
	_ = o.db.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1.0') RETURNING id::text`, o.instance, component).Scan(&version)
	_ = o.db.QueryRow(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,provenance) VALUES($1,$2,'f.bin',$3,jsonb_build_object('source_id',$4::text,'feed_id',$5::text)) RETURNING id::text`, o.instance, version, blobID, src.ID, feed.ID).Scan(&asset)

	path := "/api/v1/assets/" + asset + "/security-results"
	body := `{"scanner":"trivy","status":"failed","summary":{"critical":2}}`
	expectSourceStatus(t, o.request("POST", path, body, "", ""), 400)
	expectSourceStatus(t, o.requestAs("POST", path, body, "sec-1", "", ""), 401)
	expectSourceStatus(t, o.requestAs("POST", path, body, "sec-1", "", o.limited), 404)
	expectSourceStatus(t, o.request("POST", path, `{"scanner":"trivy","status":"bogus"}`, "sec-bad", ""), 400)
	first := o.request("POST", path, body, "sec-1", "")
	expectSourceStatus(t, first, 201)
	replay := o.request("POST", path, body, "sec-1", "")
	expectSourceStatus(t, replay, 201)
	if a, b := decodeJSON[SecurityResult](t, first), decodeJSON[SecurityResult](t, replay); a.ID != b.ID || a.Status != "failed" {
		t.Fatalf("replay created a second result: %+v %+v", a, b)
	}
	if n := o.count(`SELECT count(*) FROM asset_security_results`); n != 1 {
		t.Fatalf("results %d", n)
	}
	if n := o.count(`SELECT count(*) FROM outbox_events WHERE topic='asset.security_result'`); n != 1 {
		t.Fatalf("events %d", n)
	}
}
