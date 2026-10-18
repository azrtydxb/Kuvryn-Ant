package oci

import (
	"context"
	"encoding/json"
	"testing"
)

func TestManifestAndTagDelete(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	layer := []byte("delete-layer")
	layerDigest := f.pushBlob(t, "team/app", layer)
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := f.pushBlob(t, "team/app", config)
	doc := map[string]any{"schemaVersion": 2, "mediaType": imageManifest, "config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": len(config)}, "layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layerDigest, "size": len(layer)}}}
	manifest, _ := json.Marshal(doc)
	hdr := map[string]string{"Content-Type": imageManifest}
	assertCode(t, f.request("PUT", "/v2/images/team/app/manifests/v1", manifest, hdr), 201)
	assertCode(t, f.request("PUT", "/v2/images/team/app/manifests/v2", manifest, hdr), 201)

	// Tag delete drops only the pointer; the manifest stays pullable by digest.
	assertCode(t, f.request("DELETE", "/v2/images/team/app/manifests/v1", nil, nil), 202)
	assertCode(t, f.request("GET", "/v2/images/team/app/manifests/v1", nil, nil), 404)
	assertCode(t, f.request("GET", "/v2/images/team/app/manifests/v2", nil, nil), 200)
	assertCode(t, f.request("GET", "/v2/images/team/app/manifests/"+digest(manifest), nil, nil), 200)
	assertCode(t, f.request("DELETE", "/v2/images/team/app/manifests/v1", nil, nil), 404)

	// An immutable-release policy refuses digest deletion (tags remain mutable).
	if _, e := f.pool.Exec(ctx, `INSERT INTO delete_policies(instance_id,feed_id,mode,immutable_releases) VALUES($1,$2,'allowed',true)`, f.instance, f.feed); e != nil {
		t.Fatal(e)
	}
	assertCode(t, f.request("DELETE", "/v2/images/team/app/manifests/"+digest(manifest), nil, nil), 405)
	if _, e := f.pool.Exec(ctx, `UPDATE delete_policies SET immutable_releases=false`); e != nil {
		t.Fatal(e)
	}

	// Digest delete removes the manifest and the tags pointing at it.
	assertCode(t, f.request("DELETE", "/v2/images/team/app/manifests/"+digest(manifest), nil, nil), 202)
	assertCode(t, f.request("GET", "/v2/images/team/app/manifests/v2", nil, nil), 404)
	assertCode(t, f.request("GET", "/v2/images/team/app/manifests/"+digest(manifest), nil, nil), 404)
	// Layer blobs are untouched: they await garbage collection.
	assertCode(t, f.request("GET", "/v2/images/team/app/blobs/"+layerDigest, nil, nil), 200)
	var audits int
	if e := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='artifact.delete'`).Scan(&audits); e != nil || audits != 2 {
		t.Fatal(audits, e)
	}
}
