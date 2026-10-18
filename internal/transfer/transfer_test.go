package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const secretPlaintext = "super-secret-upstream-token-0123456789"

func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ant_transfer_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.Database = name
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close(ctx)
	})
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

type env struct {
	pool     *pgxpool.Pool
	stores   *storage.Registry
	instance string
	owner    identity.BootstrapIdentity
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := freshDatabase(t)
	stores, err := storage.New(storage.Config{Backend: "disk", Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stores.Close)
	owner, err := identity.Bootstrap(context.Background(), pool, "transfer-test")
	if err != nil {
		t.Fatal(err)
	}
	return env{pool, stores, owner.InstanceID, owner}
}

func (e env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (e env) scalar(t *testing.T, q string, args ...any) (n int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e env) put(t *testing.T, domain string, data []byte) (string, int64) {
	t.Helper()
	ctx := context.Background()
	store, err := e.stores.Resolve(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	up, err := store.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	up.Write(data)
	sum := sha256.Sum256(data)
	d := hex.EncodeToString(sum[:])
	if _, err = up.Finalize(ctx, blob.Info{Digest: blob.Digest(d), Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	return d, int64(len(data))
}

func (e env) id(t *testing.T, q string, args ...any) (id string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seed builds feed "libs" (local "hosted" + remote "proxy"), a publish policy,
// retention, two components and a tag.
func seed(t *testing.T) env {
	e := newEnv(t)
	ctx := context.Background()
	i := e.instance
	hosted := e.id(t, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'hosted','local','raw') RETURNING id::text`, i)
	proxy := e.id(t, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'proxy','remote','raw') RETURNING id::text`, i)
	domain := e.id(t, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id::text`, i, "source:"+hosted)
	feed := e.id(t, `INSERT INTO feeds(instance_id,slug,format,anonymous_read) VALUES($1,'libs','raw',true) RETURNING id::text`, i)
	e.exec(t, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,10),($1,$2,$4,20)`, i, feed, hosted, proxy)
	rules, _ := json.Marshal([]routing.Rule{{ID: "r1", Priority: 1, Selector: routing.Selector{Namespace: "team"}, Target: hosted}})
	e.exec(t, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id,rules) VALUES($1,$2,$3,$4)`, i, feed, hosted, rules)
	e.exec(t, `INSERT INTO retention_policies(instance_id,feed_id,keep_last,min_keep) VALUES($1,$2,5,2)`, i, feed)
	cred, err := (secrets.Store{DB: e.pool, Keys: testKeys(t)}).Create(ctx, i, e.owner.PrincipalID, "remote-source:"+proxy, []byte(secretPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	cfg := remoteconfig.DefaultConfig("https://upstream.example.com/base")
	cfg.CredentialID = cred.ID
	enc, _ := json.Marshal(cfg)
	e.exec(t, `INSERT INTO remote_source_configs(instance_id,source_id,credential_id,config) VALUES($1,$2,$3,$4)`, i, proxy, cred.ID, enc)
	for _, f := range []struct{ ns, key, body string }{{"team", "team/a.txt", "alpha"}, {"other", "other/b.txt", "beta"}, {"team", "team/c.txt", "alpha"}} {
		d, size := e.put(t, domain, []byte(f.body))
		comp := e.id(t, `INSERT INTO components(instance_id,source_id,coordinate,namespace,metadata) VALUES($1,$2,$3,$4,'{"k":"v"}') RETURNING id::text`, i, hosted, f.key, f.ns)
		ver := e.id(t, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'first') RETURNING id::text`, i, comp)
		blobID := e.id(t, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=EXCLUDED.digest RETURNING id::text`, i, domain, d, size)
		e.exec(t, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,media_type) VALUES($1,$2,$3,$4,'text/plain')`, i, ver, f.key, blobID)
		if f.key == "team/a.txt" {
			e.exec(t, `INSERT INTO tags(component_id,name,version_id) VALUES($1,'latest',$2)`, comp, ver)
		}
	}
	return e
}

func testKeys(t *testing.T) *secrets.Keyring {
	k, err := secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{0x5a}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := seed(t)
	dir := filepath.Join(t.TempDir(), "bundle")
	sum, err := ExportBundle(ctx, src.pool, src.stores, src.instance, dir, ExportOptions{Content: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Feeds != 1 || sum.Sources != 2 || sum.Assets != 3 || sum.Blobs != 2 {
		t.Fatalf("summary %+v", sum)
	}
	// Secrets and credential identifiers never reach the bundle.
	var secretID string
	_ = src.pool.QueryRow(ctx, `SELECT id::text FROM encrypted_secrets LIMIT 1`).Scan(&secretID)
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.Contains(p, "blobs") {
			data, _ := os.ReadFile(p)
			if bytes.Contains(data, []byte(secretPlaintext)) || bytes.Contains(data, []byte(secretID)) {
				t.Fatalf("%s leaks credential material", p)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, dir, sum.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, dir, strings.Repeat("0", 64)); err == nil {
		t.Fatal("pinned manifest digest accepted a mismatch")
	}

	dst := newEnv(t)
	b, err := OpenBundle(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	dry, err := Import(ctx, dst.pool, dst.stores, dst.instance, b.Document, b, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dst.scalar(t, `SELECT count(*) FROM feeds`) != 0 || dst.scalar(t, `SELECT count(*) FROM sources`) != 0 {
		t.Fatal("dry run modified the database")
	}
	if dry.Count("asset", ActionCreated) != 3 || dry.Count("source", ActionCreated) != 2 || dry.Count("feed", ActionCreated) != 1 {
		t.Fatalf("dry-run summary %+v", dry.Summary)
	}
	if len(dry.Unmapped) != 1 || dry.Unmapped[0].Kind != "credential" {
		t.Fatalf("expected the credential reference as unmapped: %+v", dry.Unmapped)
	}
	applied, err := Import(ctx, dst.pool, dst.stores, dst.instance, b.Document, b, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dry.Summary, applied.Summary) || !reflect.DeepEqual(dry.Items, applied.Items) {
		t.Fatalf("dry run differs from apply:\n%+v\n%+v", dry.Summary, applied.Summary)
	}
	if dst.scalar(t, `SELECT count(*) FROM assets`) != 3 || dst.scalar(t, `SELECT count(*) FROM tags`) != 1 || dst.scalar(t, `SELECT count(*) FROM retention_policies`) != 1 {
		t.Fatal("content or policies missing after import")
	}
	if dst.scalar(t, `SELECT count(*) FROM remote_source_configs WHERE credential_id IS NOT NULL`) != 0 || dst.scalar(t, `SELECT count(*) FROM encrypted_secrets`) != 0 {
		t.Fatal("credentials must not be imported")
	}
	// Blobs are present, verified and byte identical in the target store.
	domain := dst.id(t, `SELECT id::text FROM dedup_domains LIMIT 1`)
	store, _ := dst.stores.Resolve(ctx, domain)
	d := sha256.Sum256([]byte("alpha"))
	if info, err := store.Stat(ctx, blob.Digest(hex.EncodeToString(d[:]))); err != nil || info.Size != 5 {
		t.Fatalf("blob missing in target: %v", err)
	}
	// Idempotent re-apply and a clean second dry run.
	again, err := Import(ctx, dst.pool, dst.stores, dst.instance, b.Document, b, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != 0 || !again.Clean() {
		t.Fatalf("re-apply was not a no-op: %+v", again.Items)
	}
	// Round trip: exporting the target reproduces the same document.
	re, err := Build(ctx, dst.pool, dst.instance, ExportOptions{Content: true})
	if err != nil {
		t.Fatal(err)
	}
	orig := b.Document
	orig.CreatedAt, re.CreatedAt = time.Time{}, time.Time{}
	for _, d := range []*Document{&orig, &re} {
		for i := range d.Sources {
			d.Sources[i].Credential = nil // secrets are never carried over
		}
		for i := range d.Components {
			for j := range d.Components[i].Versions {
				for k := range d.Components[i].Versions[j].Assets {
					d.Components[i].Versions[j].Assets[k].Provenance = nil
				}
			}
			d.Components[i].Metadata = nil
		}
	}
	a, _ := json.MarshalIndent(orig, "", " ")
	c, _ := json.MarshalIndent(re, "", " ")
	if !bytes.Equal(a, c) {
		n := 0
		for n < len(a) && n < len(c) && a[n] == c[n] {
			n++
		}
		lo := max(n-150, 0)
		t.Fatalf("round trip differs near byte %d:\nwant ...%s\ngot  ...%s", n, a[lo:min(n+150, len(a))], c[lo:min(n+150, len(c))])
	}
}

func TestSelectionByFeedAndNamespace(t *testing.T) {
	src := seed(t)
	src.exec(t, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'empty','raw')`, src.instance)
	doc, err := Build(context.Background(), src.pool, src.instance, ExportOptions{Selection: Selection{Feeds: []string{"libs"}, Namespaces: []string{"team"}}, Content: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Feeds) != 1 || len(doc.Components) != 2 || len(doc.Sources) != 2 {
		t.Fatalf("selection: feeds=%d components=%d sources=%d", len(doc.Feeds), len(doc.Components), len(doc.Sources))
	}
	if len(doc.SkippedContent) != 1 || doc.SkippedContent[0] != "proxy" {
		t.Fatalf("remote cache content must be skipped: %v", doc.SkippedContent)
	}
	if _, err = Build(context.Background(), src.pool, src.instance, ExportOptions{Selection: Selection{Feeds: []string{"nope"}}}); err == nil {
		t.Fatal("unknown feed accepted")
	}
}

func TestImportConflictsAndIntegrity(t *testing.T) {
	ctx := context.Background()
	src := seed(t)
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := ExportBundle(ctx, src.pool, src.stores, src.instance, dir, ExportOptions{Content: true}); err != nil {
		t.Fatal(err)
	}
	// Corrupt one blob (content "beta"): verification reports it.
	d := sha256.Sum256([]byte("beta"))
	blobFile := filepath.Join(dir, "blobs", hex.EncodeToString(d[:]))
	if err := os.Chmod(blobFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobFile, []byte("BETA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, dir, ""); err == nil {
		t.Fatal("corrupt bundle verified")
	}
	// A target that already has a different object at team/a.txt, and a
	// source of the same name but another kind.
	dst := newEnv(t)
	dst.exec(t, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'proxy','local','raw')`, dst.instance)
	hosted := dst.id(t, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'hosted','local','raw') RETURNING id::text`, dst.instance)
	domain := dst.id(t, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id::text`, dst.instance, "source:"+hosted)
	od, osz := dst.put(t, domain, []byte("different"))
	comp := dst.id(t, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,'team/a.txt','team') RETURNING id::text`, dst.instance, hosted)
	ver := dst.id(t, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'first') RETURNING id::text`, dst.instance, comp)
	bl := dst.id(t, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) RETURNING id::text`, dst.instance, domain, od, osz)
	dst.exec(t, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'team/a.txt',$3)`, dst.instance, ver, bl)

	b, err := OpenBundle(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, dry := range []bool{true, false} {
		rep, err := Import(ctx, dst.pool, dst.stores, dst.instance, b.Document, b, ImportOptions{DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Count("asset", ActionConflict) != 1 || rep.Count("source", ActionConflict) != 1 {
			t.Fatalf("dry=%v conflicts: %+v", dry, rep.Summary)
		}
		if len(rep.IntegrityFailures) != 1 || rep.IntegrityFailures[0].Digest != hex.EncodeToString(d[:]) {
			t.Fatalf("dry=%v integrity: %+v", dry, rep.IntegrityFailures)
		}
		if rep.Count("asset", ActionSkipped) != 1 || rep.Count("asset", ActionCreated) != 1 || rep.Clean() {
			t.Fatalf("dry=%v asset outcomes: %+v", dry, rep.Summary)
		}
	}
	// The conflicting object was not overwritten and the corrupt one never landed.
	if dst.scalar(t, `SELECT count(*) FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.asset_key='team/a.txt' AND b.digest=$1`, od) != 1 {
		t.Fatal("existing content was overwritten")
	}
	if dst.scalar(t, `SELECT count(*) FROM blobs WHERE digest=$1`, hex.EncodeToString(d[:])) != 0 {
		t.Fatal("corrupt blob was recorded")
	}
}

func TestOpenBundleRejectsTamperedDocument(t *testing.T) {
	ctx := context.Background()
	src := seed(t)
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := ExportBundle(ctx, src.pool, src.stores, src.instance, dir, ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportBundle(ctx, src.pool, src.stores, src.instance, dir, ExportOptions{}); err == nil {
		t.Fatal("export into a non-empty directory accepted")
	}
	p := filepath.Join(dir, "document.json")
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, bytes.Replace(data, []byte(`"libs"`), []byte(`"evil"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBundle(dir, ""); err == nil {
		t.Fatal("tampered document accepted")
	}
}
