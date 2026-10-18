package oci

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"
)

// pushImage pushes a manifest (config + layers) and returns its digest and body.
func (f fixture) pushImage(t *testing.T, repo, tag string, config []byte, layers [][]byte, subject map[string]any) (string, []byte) {
	t.Helper()
	cfg := f.pushBlob(t, repo, config)
	ls := []map[string]any{}
	for _, l := range layers {
		ls = append(ls, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": f.pushBlob(t, repo, l), "size": len(l)})
	}
	m := map[string]any{"schemaVersion": 2, "mediaType": imageManifest, "config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfg, "size": len(config)}, "layers": ls}
	if subject != nil {
		m["subject"] = subject
		m["annotations"] = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
	}
	body, _ := json.Marshal(m)
	t.Log(string(body))
	assertCode(t, f.request("PUT", "/v2/images/"+repo+"/manifests/"+tag, body, map[string]string{"Content-Type": imageManifest}), 201)
	return digest(body), body
}

// TestIndexWithAttestationsAndSharedBlobs mirrors a buildx multi-platform
// image: per-platform manifests plus attestation manifests whose subject is the
// platform manifest, sharing one empty config blob, uploaded concurrently, then
// an index referencing all of them, and cross-repository mounts (including from
// a name outside the feed namespace, which must fall back to a normal upload).
func TestIndexWithAttestationsAndSharedBlobs(t *testing.T) {
	f := setup(t)
	empty := []byte("{}")
	var wg sync.WaitGroup
	type child struct {
		d    string
		body []byte
	}
	results := make([]child, 6)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, body := f.pushImage(t, "multi/app", fmt.Sprintf("p%d", i), []byte(fmt.Sprintf(`{"os":"linux","n":%d}`, i)), [][]byte{[]byte(fmt.Sprintf("layer-%d", i)), []byte("shared-layer")}, nil)
			results[i] = child{d, body}
		}()
	}
	wg.Wait()
	for i := range 3 {
		sz := len(results[i].body)
		d, body := f.pushImage(t, "multi/app", fmt.Sprintf("att%d", i), empty, [][]byte{[]byte(fmt.Sprintf("in-toto-%d", i))}, map[string]any{"mediaType": imageManifest, "digest": results[i].d, "size": sz})
		results[3+i] = child{d, body}
	}
	var entries []map[string]any
	for i, c := range results {
		e := map[string]any{"mediaType": imageManifest, "digest": c.d, "size": len(c.body), "platform": map[string]string{"architecture": "amd64", "os": "linux"}}
		if i >= 3 {
			e["platform"] = map[string]string{"architecture": "unknown", "os": "unknown"}
			e["annotations"] = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
		}
		entries = append(entries, e)
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": imageIndex, "manifests": entries})
	done := make(chan struct{})
	go func() {
		defer close(done)
		assertCode(t, f.request("PUT", "/v2/images/multi/app/manifests/latest", index, map[string]string{"Content-Type": imageIndex}), 201)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("index PUT hung")
	}
	assertCode(t, f.request("GET", "/v2/images/multi/app/manifests/latest", nil, map[string]string{"Accept": imageIndex}), 200)

	// Cross-repository mounts: known source, unknown source repo, and a source
	// outside any feed. Unmountable sources fall back to a regular session.
	assertCode(t, f.request("POST", "/v2/images/other/deep/er/app/blobs/uploads/?mount="+url.QueryEscape(digest([]byte("shared-layer")))+"&from="+url.QueryEscape("images/multi/app"), nil, nil), 201)
	assertCode(t, f.request("POST", "/v2/images/other/app/blobs/uploads/?mount="+url.QueryEscape(digest([]byte("shared-layer")))+"&from="+url.QueryEscape("library/alpine"), nil, nil), 202)
	assertCode(t, f.request("POST", "/v2/images/other/app/blobs/uploads/?mount="+url.QueryEscape(digest([]byte("absent")))+"&from="+url.QueryEscape("images/multi/app"), nil, nil), 202)
	assertCode(t, f.request("POST", "/v2/images/other/app/blobs/uploads?mount="+url.QueryEscape(digest([]byte("absent")))+"&from=nonsense", nil, nil), 202)
}
