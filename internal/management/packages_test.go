package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/google/uuid"
)

func TestPackagePermissionPartitions(t *testing.T) {
	p := identity.Principal{InstanceID: "instance", PrincipalID: "person", TokenID: "token", Scopes: []identity.Scope{{Actions: []identity.Action{identity.Read}, Namespace: "team"}}}
	policy := identity.Policy{Roles: []identity.Role{{Name: "role", PrincipalIDs: []string{"person"}, Grants: []identity.Grant{{InstanceID: "instance", Kind: identity.ArtifactResource, Effect: identity.Allow, Actions: []identity.Action{identity.Read}}, {InstanceID: "instance", Kind: identity.ArtifactResource, SourceID: "denied", Namespace: "team", Effect: identity.Deny, Actions: []identity.Action{identity.Read}}}}}}
	for _, scope := range []string{"team", "", "private"} {
		p.Scopes[0].Namespace = scope
		parts, err := packagePermissions(p, policy, "feed", []string{"allowed", "denied"})
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"allowed", "denied"} {
			for _, namespace := range []string{"", "team", "private", "another"} {
				actual := false
				for _, part := range parts {
					if part.SourceID == source {
						actual = part.Base
						for _, name := range part.Namespaces {
							if name == namespace {
								actual = !actual
							}
						}
					}
				}
				expected := policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: "feed", SourceID: source, Namespace: namespace, Kind: identity.ArtifactResource}, identity.Read)
				if actual != expected {
					t.Fatalf("partition changed authorization for %q/%q", source, namespace)
				}
			}
		}
	}
	if _, err := packagePermissions(p, policy, "feed", make([]string, 1025)); err == nil {
		t.Fatal("unbounded source set accepted")
	}
}

type packageFixture struct {
	*sourceHTTPFixture
	feed, high, low, domain string
}

func packageHTTPFixture(t *testing.T) *packageFixture {
	f := identityHTTPFixture(t)
	ctx := context.Background()
	tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.credential = tok.Secret
	mux := http.NewServeMux()
	RegisterPackages(mux, f.db, NewCursorCodec(make([]byte, 32)))
	f.handler = WithRequestID(mux)
	out := &packageFixture{sourceHTTPFixture: f}
	for query, target := range map[string]*string{`INSERT INTO feeds(instance_id,slug,format) VALUES($1,'browse','raw') RETURNING id::text`: &out.feed, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'high','local','raw') RETURNING id::text`: &out.high, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'low','local','raw') RETURNING id::text`: &out.low, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,'browse') RETURNING id::text`: &out.domain} {
		if err := f.db.QueryRow(ctx, query, f.instance).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,1),($1,$2,$4,2)`, f.instance, out.feed, out.high, out.low); err != nil {
		t.Fatal(err)
	}
	return out
}

type packageSeed struct{ Component, Version, Asset, Digest string }

func (f *packageFixture) seed(source, coordinate, namespace, version, digest, componentID string) packageSeed {
	f.t.Helper()
	ctx := context.Background()
	out := packageSeed{Digest: "sha256:" + strings.Repeat(digest, 64)}
	if err := f.db.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace,metadata,id) VALUES($1,$2,$3,$4,'{"adapter":"retained"}',COALESCE(NULLIF($5,'')::uuid,gen_random_uuid())) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate RETURNING id::text`, f.instance, source, coordinate, namespace, componentID).Scan(&out.Component); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) RETURNING id::text`, f.instance, out.Component, version).Scan(&out.Version); err != nil {
		f.t.Fatal(err)
	}
	var blob string
	if err := f.db.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,7,'PRIVATE_STORAGE_KEY') ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text`, f.instance, f.domain, strings.Repeat(digest, 64)).Scan(&blob); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,media_type,provenance) VALUES($1,$2,'file.bin',$3,'application/octet-stream',jsonb_build_object('principal_id',$4::text,'origin','retained')) RETURNING id::text`, f.instance, out.Version, blob, f.principal).Scan(&out.Asset); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Exec(ctx, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3)`, out.Component, "tag-"+version, out.Version); err != nil {
		f.t.Fatal(err)
	}
	return out
}
func (f *packageFixture) packages(query string) string {
	return "/api/v1/feeds/" + f.feed + "/packages" + query
}
func TestPackageDiscoveryOwnersProvenanceAndChecksums(t *testing.T) {
	f := packageHTTPFixture(t)
	high := f.seed(f.high, "team/pkg", "team", "1", "a", "")
	low := f.seed(f.low, "team/pkg", "team", "2", "b", "")
	other := f.seed(f.low, "other/pkg", "other", "1", "c", "")
	page := f.request("GET", f.packages(""), "", "", "")
	expectSourceStatus(t, page, 200)
	var packages struct {
		Items []PackageRecord `json:"items"`
		Page  api.PageInfo    `json:"page"`
	}
	_ = json.Unmarshal(page.Body.Bytes(), &packages)
	if len(packages.Items) != 2 || strings.Contains(page.Body.String(), low.Component) {
		t.Fatal("shadowed package surfaced")
	}
	for _, tc := range []struct {
		query string
		count int
	}{{"?namespace=team", 1}, {"?prefix=team/", 1}, {"?prefix=%25", 0}, {"?digest=" + high.Digest, 1}, {"?digest=" + low.Digest, 0}, {"?namespace=", 0}} {
		w := f.request("GET", f.packages(tc.query), "", "", "")
		expectSourceStatus(t, w, 200)
		_ = json.Unmarshal(w.Body.Bytes(), &packages)
		if len(packages.Items) != tc.count {
			t.Fatalf("query %s count=%d", tc.query, len(packages.Items))
		}
	}
	versions := f.request("GET", f.packages("/"+high.Component+"/versions"), "", "", "")
	expectSourceStatus(t, versions, 200)
	var vp struct {
		Items []PackageVersion `json:"items"`
	}
	_ = json.Unmarshal(versions.Body.Bytes(), &vp)
	if len(vp.Items) != 1 || vp.Items[0].Version != "1" {
		t.Fatal("versions merged across sources")
	}
	expectSourceStatus(t, f.request("GET", f.packages("/"+low.Component+"/versions"), "", "", ""), 404)
	assets := f.request("GET", f.packages("/"+high.Component+"/versions/"+high.Version+"/assets"), "", "", "")
	expectSourceStatus(t, assets, 200)
	var ap struct {
		Items []PackageAsset `json:"items"`
	}
	_ = json.Unmarshal(assets.Body.Bytes(), &ap)
	if len(ap.Items) != 1 || ap.Items[0].Digest != high.Digest || ap.Items[0].SourceID != f.high || ap.Items[0].UploaderID == nil || *ap.Items[0].UploaderID != f.principal || ap.Items[0].CreatedAt.IsZero() || !strings.Contains(string(ap.Items[0].Provenance), "retained") {
		t.Fatal("asset provenance incomplete")
	}
	if strings.Contains(assets.Body.String(), "PRIVATE_STORAGE_KEY") || strings.Contains(assets.Body.String(), f.domain) {
		t.Fatal("internal storage locator leaked")
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/feeds/"+f.feed+"/assets/"+high.Asset, "", "", ""), 200)
	expectSourceStatus(t, f.request("GET", "/api/v1/feeds/"+f.feed+"/assets/"+low.Asset, "", "", ""), 404)
	expectSourceStatus(t, f.request("GET", f.packages("/"+high.Component+"/versions/"+other.Version+"/assets"), "", "", ""), 404)
	expectSourceStatus(t, f.request("GET", f.packages("/"+high.Component+"/tags"), "", "", ""), 200)
	// Denying the owner must not reveal an allowed lower source's version.
	if _, err := f.db.Exec(context.Background(), `INSERT INTO role_grants(instance_id,role_id,kind,effect,source_id,actions) SELECT instance_id,role_id,'artifact','deny',$2,ARRAY['read'] FROM principal_roles WHERE principal_id=$1`, f.principal, f.high); err != nil {
		t.Fatal(err)
	}
	denied := f.request("GET", f.packages("?limit=1"), "", "", "")
	expectSourceStatus(t, denied, 200)
	_ = json.Unmarshal(denied.Body.Bytes(), &packages)
	if len(packages.Items) != 1 || packages.Items[0].ID != other.Component || packages.Page.HasMore {
		t.Fatal("unauthorized owner affected records or continuation")
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/feeds/"+f.feed+"/assets/"+high.Asset, "", "", ""), 404)
}
func TestPackageDiscoveryCursorAndNamespaceIsolation(t *testing.T) {
	f := packageHTTPFixture(t)
	first := f.seed(f.high, "team/first", "team", "1", "a", "00000000-0000-4000-8000-000000000011")
	last := f.seed(f.high, "team/last", "team", "1", "b", "00000000-0000-4000-8000-000000000033")
	f.seed(f.high, "private/hidden", "private", "1", "c", "00000000-0000-4000-8000-000000000001")
	ctx := context.Background()
	tok, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}, Namespace: "team"}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.credential = tok.Secret
	page := f.request("GET", f.packages("?limit=1"), "", "", "")
	expectSourceStatus(t, page, 200)
	var list struct {
		Items []PackageRecord `json:"items"`
		Page  api.PageInfo    `json:"page"`
	}
	_ = json.Unmarshal(page.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0].ID != first.Component || list.Page.NextCursor == nil {
		t.Fatal("scoped first page incorrect")
	}
	cursor := *list.Page.NextCursor
	middle := f.seed(f.low, "team/middle", "team", "1", "d", "00000000-0000-4000-8000-000000000022")
	next := f.request("GET", f.packages("?limit=2&cursor="+cursor), "", "", "")
	expectSourceStatus(t, next, 200)
	_ = json.Unmarshal(next.Body.Bytes(), &list)
	if len(list.Items) != 2 || list.Items[0].ID != middle.Component || list.Items[1].ID != last.Component || list.Page.HasMore {
		t.Fatal("keyset pagination skipped concurrent insertion")
	}
	expectSourceStatus(t, f.request("GET", f.packages("?cursor="+cursor+"&namespace=private"), "", "", ""), 400)
	other, err := (identity.Tokens{DB: f.db}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}, Namespace: "team"}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.requestAs("GET", f.packages("?cursor="+cursor), "", "", "", other.Secret), 400)
	expectSourceStatus(t, f.request("GET", f.packages("?cursor="+cursor+"x"), "", "", ""), 400)
	expectSourceStatus(t, f.request("GET", f.packages("?digest=bad"), "", "", ""), 400)
	expectSourceStatus(t, f.requestAs("GET", f.packages(""), "", "", "", ""), 401)
	// Unreconciled legacy namespace cannot be guessed from the coordinate.
	if _, err := f.db.Exec(ctx, `UPDATE components SET namespace=NULL WHERE id=$1`, first.Component); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("GET", f.packages("/"+first.Component+"/versions"), "", "", ""), 404)
}
func TestPackageDiscoveryGeneratedClient(t *testing.T) {
	f := packageHTTPFixture(t)
	f.seed(f.high, "team/pkg", "team", "1", "a", "")
	client, err := api.NewClientWithResponses("http://example.test", api.WithHTTPClient(&http.Client{Transport: identityContractTransport{f.sourceHTTPFixture}}), api.WithRequestEditorFn(func(ctx context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+f.credential)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(f.feed)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ListPackagesWithResponse(context.Background(), id, &api.ListPackagesParams{})
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON200 == nil || len(response.JSON200.Items) != 1 {
		t.Fatalf("generated package list: %s", response.Body)
	}
}
func TestPackageDiscoveryBoundedMetadata(t *testing.T) {
	f := packageHTTPFixture(t)
	item := f.seed(f.high, "team/pkg", "team", "1", "a", "")
	if _, err := f.db.Exec(context.Background(), `UPDATE components SET metadata=jsonb_build_object('large',$2::text) WHERE id=$1`, item.Component, strings.Repeat("x", 65537)); err != nil {
		t.Fatal(err)
	}
	expectSourceStatus(t, f.request("GET", f.packages(""), "", "", ""), 503)
}

func TestPackageDiscoveryMigrationBackfill(t *testing.T) {
	f := packageHTTPFixture(t)
	ctx := context.Background()
	cases := []struct {
		format, coordinate, expected string
		known                        bool
	}{
		{"oci", "org/image", "org", true}, {"oci", "image", "", true}, {"raw", "dir/file", "dir", true}, {"go", "example.org/team/module", "example.org/team", true}, {"npm", "@team/package", "@team", true}, {"npm", "package", "", true}, {"maven", "org.example:library", "org.example", true}, {"helm", "chart", "", true}, {"cargo", "crate", "", true}, {"pypi", "wheel", "", true}, {"nuget", "package", "", true}, {"ansible", "unknown", "", false},
	}
	var seeded []packageSeed
	for i, c := range cases {
		var source string
		if err := f.db.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'local',$3) RETURNING id::text`, f.instance, fmt.Sprint("legacy-", i), c.format).Scan(&source); err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, f.seed(source, c.coordinate, "legacy-untrusted", "1", "a", ""))
	}
	published := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := f.db.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,created_at) VALUES($1,$2,'artifact.publish',$3,$4)`, f.instance, f.principal, seeded[0].Asset, published); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `ALTER TABLE components DROP COLUMN namespace; ALTER TABLE assets DROP COLUMN created_at; DROP INDEX components_discovery_idx,versions_component_page_idx,assets_version_page_idx,blobs_checksum_idx`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../database/migrations/016_package_discovery.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		var namespace *string
		if err = tx.QueryRow(ctx, `SELECT namespace FROM components WHERE id=$1`, seeded[i].Component).Scan(&namespace); err != nil {
			t.Fatal(err)
		}
		if c.known && (namespace == nil || *namespace != c.expected) {
			t.Fatalf("%s namespace not preserved", c.format)
		}
		if !c.known && namespace != nil {
			t.Fatal("unsupported namespace guessed")
		}
	}
	var timestamp time.Time
	if err = tx.QueryRow(ctx, `SELECT created_at FROM assets WHERE id=$1`, seeded[0].Asset).Scan(&timestamp); err != nil || !timestamp.Equal(published) {
		t.Fatal("publication timestamp not retained")
	}
}
func TestPackageDiscoveryUnknownNamespaceFailsClosed(t *testing.T) {
	f := packageHTTPFixture(t)
	item := f.seed(f.high, "unknown/pkg", "", "1", "a", "")
	if _, err := f.db.Exec(context.Background(), `UPDATE components SET namespace=NULL WHERE id=$1`, item.Component); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", f.packages(""), "", "", "")
	expectSourceStatus(t, w, 200)
	var page struct {
		Items []PackageRecord `json:"items"`
		Page  api.PageInfo    `json:"page"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 || page.Page.HasMore {
		t.Fatal("unknown namespace exposed to global token")
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/feeds/"+f.feed+"/assets/"+item.Asset, "", "", ""), 404)
}

// Blob-only versions never appear in the versions listing, and a tag-less
// top-level OCI manifest is flagged as pulled by digest.
func TestPackageVersionsHideBlobsAndFlagDigestPulls(t *testing.T) {
	f := packageHTTPFixture(t)
	ctx := context.Background()
	var feed, source string
	if err := f.db.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'images','oci') RETURNING id::text`, f.instance).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'imgsrc','local','oci') RETURNING id::text`, f.instance).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,1)`, f.instance, feed, source); err != nil {
		t.Fatal(err)
	}
	manifest := f.seed(source, "team/app", "team", strings.Repeat("a", 64), "a", "")
	if _, err := f.db.Exec(ctx, `DELETE FROM tags WHERE component_id=$1`, manifest.Component); err != nil {
		t.Fatal(err)
	}
	blobVersion := f.seed(source, "team/app", "team", strings.Repeat("b", 64), "b", "")
	if _, err := f.db.Exec(ctx, `UPDATE versions SET blob_only=true WHERE id=$1`, blobVersion.Version); err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", "/api/v1/feeds/"+feed+"/packages/"+manifest.Component+"/versions", "", "", "")
	expectSourceStatus(t, w, 200)
	var page struct {
		Items []PackageVersion `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].Version != strings.Repeat("a", 64) || !page.Items[0].PulledByDigest {
		t.Fatalf("versions: %s", w.Body.String())
	}
}
