package oci

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestSubjectsReferrersAndAtomicReplay(t *testing.T) {
	f := setup(t)
	repo := "/v2/images/team/app"
	config := []byte("{}")
	f.pushBlob(t, "team/app", config)
	size := int64(len(config))
	cfg := descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: digest(config), Size: &size}
	base, _ := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: imageManifest, Config: &cfg, Layers: []descriptor{}})
	assertCode(t, f.request("PUT", repo+"/manifests/base", base, map[string]string{"Content-Type": imageManifest}), 201)
	baseSize := int64(len(base))
	subject := descriptor{MediaType: imageManifest, Digest: digest(base), Size: &baseSize}
	var first []byte
	for _, kind := range []string{"example/sbom", "example/signature"} {
		data, _ := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: imageManifest, Config: &cfg, Layers: []descriptor{}, Subject: &subject, ArtifactType: kind, Annotations: map[string]string{"test": "preserved"}})
		response := f.request("PUT", repo+"/manifests/"+digest(data), data, map[string]string{"Content-Type": imageManifest})
		assertCode(t, response, 201)
		if response.Header().Get("OCI-Subject") != digest(base) {
			t.Fatal("missing subject acknowledgment")
		}
		if first == nil {
			first = data
		}
	}
	// Retry leaves a single association and does not emit another publication.
	var before, after int
	f.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events").Scan(&before)
	assertCode(t, f.request("PUT", repo+"/manifests/"+digest(first), first, map[string]string{"Content-Type": imageManifest}), 201)
	f.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_events").Scan(&after)
	if before != after {
		t.Fatal("replay emitted mutation")
	}
	response := f.request("GET", repo+"/referrers/"+digest(base)+"?n=1", nil, nil)
	assertCode(t, response, 200)
	if response.Header().Get("Link") == "" {
		t.Fatal("missing pagination")
	}
	filtered := f.request("GET", repo+"/referrers/"+digest(base)+"?artifactType=example%2Fsbom", nil, nil)
	assertCode(t, filtered, 200)
	var page struct {
		Manifests []referrerDescriptor `json:"manifests"`
	}
	if json.Unmarshal(filtered.Body.Bytes(), &page) != nil || len(page.Manifests) != 1 || page.Manifests[0].Annotations["test"] != "preserved" || filtered.Header().Get("OCI-Filters-Applied") != "artifactType" {
		t.Fatal(filtered.Body.String())
	}
	// A missing subject is an association, not a failed dependency closure.
	missing := subject
	missing.Digest = "sha256:" + strings.Repeat("a", 64)
	orphan, _ := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: imageManifest, Config: &cfg, Layers: []descriptor{}, Subject: &missing})
	assertCode(t, f.request("PUT", repo+"/manifests/"+digest(orphan), orphan, map[string]string{"Content-Type": imageManifest}), 201)
	assertCode(t, f.request("GET", repo+"/referrers/"+missing.Digest, nil, nil), 200)
	// Existing subjects must match their descriptor; failure publishes no edge.
	bad := subject
	wrong := baseSize + 1
	bad.Size = &wrong
	invalid, _ := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: imageManifest, Config: &cfg, Layers: []descriptor{}, Subject: &bad})
	assertCode(t, f.request("PUT", repo+"/manifests/"+digest(invalid), invalid, map[string]string{"Content-Type": imageManifest}), 400)
	assertCode(t, f.request("GET", repo+"/manifests/"+digest(invalid), nil, nil), 404)
	token, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("PUT", repo+"/manifests/"+digest(first), first, map[string]string{"Content-Type": imageManifest, "Authorization": "Bearer " + token.Secret}), 403)
	assertCode(t, f.request("GET", repo+"/referrers/"+digest(base), nil, map[string]string{"Authorization": "Bearer " + token.Secret}), 404)
	// An earlier source owns the entire repository; referrers never fall through.
	var newSource string
	if err = f.pool.QueryRow(context.Background(), `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'replacement','local','oci') RETURNING id`, f.instance).Scan(&newSource); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(context.Background(), `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)`, f.instance, f.feed, newSource); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(context.Background(), `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,'team/app','team')`, f.instance, newSource); err != nil {
		t.Fatal(err)
	}
	hidden := f.request("GET", repo+"/referrers/"+digest(base), nil, nil)
	assertCode(t, hidden, 200)
	if json.Unmarshal(hidden.Body.Bytes(), &page) != nil || len(page.Manifests) != 0 {
		t.Fatal("referrers leaked from non-owning source")
	}
	link := response.Header().Get("Link")
	next := strings.SplitN(strings.TrimPrefix(link, "<"), ">", 2)[0]
	assertCode(t, f.request("GET", next, nil, nil), 409)

}

// Official ORAS1.3.4 darwin_arm64 release SHA256:
// 217761a9500242ff473de8656b5aca21136ff39e17e9e61fd8936bbfd902704c.
// ORAS_BINARY may point to an independently verified platform fixture.
func TestRealORASArtifacts(t *testing.T) {
	binary := os.Getenv("ORAS_BINARY")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("oras")
		if err != nil {
			t.Skip("ORAS fixture unavailable")
		}
	}
	f := setup(t)
	if _, err := f.pool.Exec(context.Background(), "UPDATE feeds SET format='oci-artifacts' WHERE id=$1", f.feed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE sources SET format='oci-artifacts' WHERE id=$1", f.source); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.registry)
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	root := t.TempDir()
	credentials := filepath.Join(root, "credentials.json")
	run := func(input string, args ...string) []byte {
		t.Helper()
		args = append(args, "--registry-config", credentials)
		cmd := exec.Command(binary, args...)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("oras %s failed: %v %s", args[0], err, strings.ReplaceAll(string(out), f.token, "[redacted]"))
		}
		return out
	}
	run(f.token, "login", host, "--plain-http", "--username", "service", "--password-stdin")
	if err := os.WriteFile(filepath.Join(root, "artifact.txt"), []byte("local OCI artifact\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sbom.txt"), []byte("attached metadata\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ref := host + "/images/demo:base"
	run("", "push", ref, "artifact.txt:text/plain", "--artifact-type", "application/vnd.ant.fixture", "--plain-http")
	run("", "attach", ref, "sbom.txt:text/plain", "--artifact-type", "application/vnd.ant.sbom", "--annotation", "ant.test=preserved", "--distribution-spec", "v1.1-referrers-api", "--plain-http")
	output := run("", "discover", ref, "--format", "json", "--depth", "1", "--distribution-spec", "v1.1-referrers-api", "--plain-http")
	var discovery struct {
		Referrers []referrerDescriptor `json:"referrers"`
	}
	if err := json.Unmarshal(output, &discovery); err != nil || len(discovery.Referrers) != 1 {
		t.Fatalf("discovery %s %v", output, err)
	}
	d := discovery.Referrers[0]
	if d.ArtifactType != "application/vnd.ant.sbom" || d.Annotations["ant.test"] != "preserved" {
		t.Fatal(d)
	}
	run("", "pull", host+"/images/demo@"+d.Digest, "--output", filepath.Join(root, "pull"), "--plain-http")
	data, err := os.ReadFile(filepath.Join(root, "pull", "sbom.txt"))
	if err != nil || string(data) != "attached metadata\n" {
		t.Fatal("attachment bytes", err)
	}
}

func TestRepositoryReservedSegmentsRemainValid(t *testing.T) {
	f := setup(t)
	data := []byte("reserved segment fixture")
	for _, repository := range []string{"team/referrers/app", "team/manifests/app", "team/blobs/app"} {
		d := f.pushBlob(t, repository, data)
		assertCode(t, f.request("GET", "/v2/images/"+repository+"/blobs/"+d, nil, nil), 200)
	}
}
