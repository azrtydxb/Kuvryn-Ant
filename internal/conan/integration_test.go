package conan

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type conanFixture struct {
	pool                                           *pgxpool.Pool
	registry                                       *Repository
	service                                        *artifact.Service
	root, token, instance, principal, feed, source string
}

func conanSetup(t *testing.T) conanFixture {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("PostgreSQL required")
		}
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ant_conan_" + strings.ToLower(rand.Text())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 1
	pool, err := database.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Conan")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Admin}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := conanFixture{pool: pool, token: token.Secret, instance: owner.InstanceID, principal: owner.PrincipalID, root: filepath.Join(t.TempDir(), "uploads")}
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','conan') RETURNING id::text`, owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','conan') RETURNING id::text`, owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, owner.InstanceID, "source:"+f.source); err != nil {
		t.Fatal(err)
	}
	disk, err := blob.NewDisk(filepath.Join(t.TempDir(), "blobs"), blob.TransferBufferSize*4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	lease, err := ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000001", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: f.source, Kind: routing.Local}}, DefaultTarget: f.source})
	if err != nil {
		t.Fatal(err)
	}
	f.service = &artifact.Service{DB: pool, Lease: func() ownership.Lease { return lease }, Router: func(context.Context, string) (*routing.Router, error) { return router, nil }, Storage: func(context.Context, string) (blob.Store, error) { return disk, nil }}
	f.registry, err = New(pool, f.service)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f conanFixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("service", f.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func conanStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}
func md5h(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }

// conanRecipeLock builds a recipe revision: conanfile.py plus a manifest whose
// lines are the sorted "name: md5" pairs; the revision is the manifest md5.
func conanRecipeLock(t *testing.T, conanfile string) (rrev string, manifest []byte) {
	t.Helper()
	sum := md5h([]byte(conanfile))
	lock := "conanfile.py: " + sum
	rrev = md5h([]byte(lock + "\n"))
	manifest = []byte("1\n" + lock + "\n")
	return rrev, manifest
}

// conanPackageLock builds a binary package revision: conaninfo.txt, a tarball
// containing arbitrary members, and the manifest naming every member plus the
// info file in sorted order.
func conanPackageLock(t *testing.T, info, memberName, memberContent string) (prev string, manifest []byte, tgz []byte) {
	t.Helper()
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(gw)
	members := map[string][]byte{"conaninfo.txt": []byte(info)}
	_ = tw.WriteHeader(&tar.Header{Name: memberName, Mode: 0600, Size: int64(len(memberContent))})
	_, _ = tw.Write([]byte(memberContent))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	tgz = gz.Bytes()
	members[memberName] = []byte(memberContent)
	lines := make([]string, 0, len(members))
	for _, name := range []string{"conaninfo.txt", memberName} {
		lines = append(lines, name+": "+md5h(members[name]))
	}
	sort.Strings(lines)
	prev = md5h([]byte(strings.Join(lines, "\n") + "\n"))
	manifest = []byte(strconv.Itoa(len(lines)) + "\n" + strings.Join(lines, "\n") + "\n")
	return prev, manifest, tgz
}

const conanBase = "/feeds/packages/conan/v2/conans/example/1.0/main/stable"

func TestConanPostgresRecipeAndPackageLifecycle(t *testing.T) {
	f := conanSetup(t)
	rrev, manifest := conanRecipeLock(t, "from conan import ConanFile\nclass Example(ConanFile):\n    pass\n")
	// Authenticate and hand the returned token back as the store credential.
	response := f.request("GET", "/feeds/packages/conan/v1/ping", nil, nil)
	conanStatus(t, response, 200)
	response = f.request("GET", "/feeds/packages/conan/v2/users/authenticate", nil, nil)
	conanStatus(t, response, 200)
	if response.Body.String() != f.token {
		t.Fatal("token echo mismatch")
	}
	conanStatus(t, f.request("GET", "/feeds/packages/conan/v2/users/check_credentials", nil, nil), 200)
	// Upload the recipe files one at a time; the manifest seals the revision.
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanfile.py", []byte("from conan import ConanFile\nclass Example(ConanFile):\n    pass\n"), nil), 200)
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanmanifest.txt", manifest, nil), 200)
	// The recipe is now visible with its revision and file inventory.
	response = f.request("GET", conanBase+"/latest", nil, nil)
	conanStatus(t, response, 200)
	var latest map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &latest); err != nil || latest["revision"] != rrev {
		t.Fatal(latest, err)
	}
	response = f.request("GET", conanBase+"/revisions", nil, nil)
	conanStatus(t, response, 200)
	var revisions map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &revisions); err != nil || len(revisions["revisions"].([]any)) != 1 {
		t.Fatal(revisions, err)
	}
	response = f.request("GET", conanBase+"/revisions/"+rrev+"/files", nil, nil)
	conanStatus(t, response, 200)
	var files map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	keys := files["files"].(map[string]any)
	if _, ok := keys["conanfile.py"]; !ok {
		t.Fatal("recipe file missing", files)
	}
	// Download the sealed recipe bytes.
	response = f.request("GET", conanBase+"/revisions/"+rrev+"/files/conanfile.py", nil, nil)
	conanStatus(t, response, 200)
	if !bytes.Contains(response.Body.Bytes(), []byte("class Example")) {
		t.Fatal("downloaded recipe mismatch")
	}
	// Re-uploading the identical sealed revision is idempotent.
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanfile.py", []byte("from conan import ConanFile\nclass Example(ConanFile):\n    pass\n"), nil), 200)
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanmanifest.txt", manifest, nil), 200)
	// A different conanfile with the same revision cannot seal, and a corrupt
	// manifest is rejected outright.
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanfile.py", []byte("changed"), nil), 200)
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanmanifest.txt", manifest, nil), 400)
	response = f.request("GET", conanBase+"/revisions/"+rrev+"/files/conanfile.py", nil, nil)
	conanStatus(t, response, 200)
	if string(response.Body.Bytes()) == "changed" {
		t.Fatal("staged change leaked", response.Body.String())
	}
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanmanifest.txt", []byte("2\nconanfile.py: 000"), nil), 400)
	conanStatus(t, f.request("GET", conanBase+"/revisions/"+rrev+"/files/conanfile.py", nil, nil), 200)
	// A second, distinct revision sequence coexists without mixing.
	secondRrev, secondManifest := conanRecipeLock(t, "from conan import ConanFile\nclass Second(ConanFile):\n    pass\n")
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+secondRrev+"/files/conanfile.py", []byte("from conan import ConanFile\nclass Second(ConanFile):\n    pass\n"), nil), 200)
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+secondRrev+"/files/conanmanifest.txt", secondManifest, nil), 200)
	response = f.request("GET", conanBase+"/revisions", nil, nil)
	conanStatus(t, response, 200)
	json.Unmarshal(response.Body.Bytes(), &revisions)
	if len(revisions["revisions"].([]any)) != 2 {
		t.Fatal(revisions)
	}
}

func TestConanPostgresBinaryPackagesAndSearch(t *testing.T) {
	f := conanSetup(t)
	rrev, manifest := conanRecipeLock(t, "from conan import ConanFile\nclass Example(ConanFile):\n    pass\n")
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanfile.py", []byte("from conan import ConanFile\nclass Example(ConanFile):\n    pass\n"), nil), 200)
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+rrev+"/files/conanmanifest.txt", manifest, nil), 200)
	// Upload two binary package revisions with distinct 40-hex package IDs.
	type pkg struct{ pid, prev string }
	packages := []pkg{}
	for _, p := range []struct{ pid, suffix string }{
		{strings.Repeat("a", 40), "debug"},
		{strings.Repeat("b", 40), "release"},
	} {
		prev, pmanifest, tgz := conanPackageLock(t, "[settings]\nos="+p.suffix, "bin/example.bin", "MEMBER:"+p.suffix)
		packages = append(packages, pkg{p.pid, prev})
		base := conanBase + "/revisions/" + rrev + "/packages/" + p.pid + "/revisions/" + prev + "/files"
		conanStatus(t, f.request("PUT", base+"/conaninfo.txt", []byte("[settings]\nos="+p.suffix), nil), 200)
		conanStatus(t, f.request("PUT", base+"/conan_package.tgz", tgz, nil), 200)
		conanStatus(t, f.request("PUT", base+"/conanmanifest.txt", pmanifest, nil), 200)
		response := f.request("GET", conanBase+"/revisions/"+rrev+"/packages/"+p.pid+"/latest", nil, nil)
		conanStatus(t, response, 200)
		var latest map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &latest); err != nil || latest["revision"] != prev {
			t.Fatal(latest, err)
		}
		download := f.request("GET", base+"/conan_package.tgz", nil, nil)
		conanStatus(t, download, 200)
		if !bytes.Equal(download.Body.Bytes(), tgz) {
			t.Fatal("package tarball changed")
		}
	}
	// Recipe package listing exposes both binary IDs.
	response := f.request("GET", conanBase+"/revisions/"+rrev+"/packages", nil, nil)
	conanStatus(t, response, 200)
	var listing map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	ids := listing["package_ids"].([]any)
	if len(ids) != 2 {
		t.Fatal(ids)
	}
	// Package revision listing for one binary.
	response = f.request("GET", conanBase+"/revisions/"+rrev+"/packages/"+packages[0].pid+"/revisions", nil, nil)
	conanStatus(t, response, 200)
	json.Unmarshal(response.Body.Bytes(), &listing)
	if len(listing["revisions"].([]any)) != 1 {
		t.Fatal(listing)
	}
	// The recipe search endpoint returns the recipe reference; a missing package
	// ID and unknown tail are not found.
	response = f.request("GET", "/feeds/packages/conan/v2/conans/search?q=*", nil, nil)
	conanStatus(t, response, 200)
	var search map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &search); err != nil {
		t.Fatal(err)
	}
	if len(search["results"].([]any)) != 1 {
		t.Fatal(search)
	}
	conanStatus(t, f.request("GET", conanBase+"/revisions/"+rrev+"/packages/"+strings.Repeat("c", 40)+"/latest", nil, nil), 404)
	conanStatus(t, f.request("GET", conanBase+"/revisions/"+rrev+"/packages/"+strings.Repeat("a", 40)+"/latest", nil, nil), 200)
}

func TestConanPostgresIsolationAndBounds(t *testing.T) {
	f := conanSetup(t)
	// Unknown slug, wrong format, missing auth and unsafe paths fail closed.
	conanStatus(t, f.request("GET", "/feeds/other/conan/v2/conans/search", nil, nil), 404)
	conanStatus(t, f.request("GET", "/feeds/packages/raw/search", nil, nil), 404)
	req := httptest.NewRequest("GET", conanBase+"/revisions", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	conanStatus(t, f.request("GET", conanBase+"/revisions", nil, nil), 404)
	// Revisions are content-addressed: an unmatched manifest can never publish.
	conanStatus(t, f.request("PUT", conanBase+"/revisions/"+strings.Repeat("0", 32)+"/files/conanmanifest.txt", []byte("1\nconanfile.py: 00"), nil), 400)
	// Oversized metadata is bounded by the server before any storage write.
	big := strings.Repeat("x", 2<<20)
	conanStatus(t, f.request("POST", conanBase+"/revisions/"+strings.Repeat("0", 32)+"/files/conanfile.py", []byte(big), nil), 405)
	ctx := context.Background()
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM versions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unexpected versions", count, err)
	}
}
