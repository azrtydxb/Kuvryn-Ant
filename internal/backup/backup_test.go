package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const secretPlaintext = "super-secret-webhook-token-0123456789"

var rootKeyBytes = bytes.Repeat([]byte{0x5a}, 32)

// freshDatabase creates a new database on the fixture server (not just a
// schema), so restore is tested into a genuinely second database.
func freshDatabase(t *testing.T, migrate bool) *pgxpool.Pool {
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
	name := fmt.Sprintf("ant_backup_%d", time.Now().UnixNano())
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
	if migrate {
		if err = database.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func keyring(t *testing.T, key []byte) *secrets.Keyring {
	k, err := secrets.New("v1", map[string][]byte{"v1": key})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newStores(t *testing.T) *storage.Registry {
	r, err := storage.New(storage.Config{Backend: "disk", Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func putBlob(t *testing.T, stores *storage.Registry, domain string, data []byte) blob.Info {
	t.Helper()
	ctx := context.Background()
	store, err := stores.Resolve(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	up, err := store.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Abort()
	up.Write(data)
	sum := sha256.Sum256(data)
	info, err := up.Finalize(ctx, blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

type source struct {
	pool     *pgxpool.Pool
	stores   *storage.Registry
	owner    identity.BootstrapIdentity
	domain   string
	blob     blob.Info
	orphan   blob.Info
	secretID string
}

func populate(t *testing.T) source {
	pool := freshDatabase(t, true)
	ctx := context.Background()
	stores := newStores(t)
	owner, err := identity.Bootstrap(ctx, pool, "backup-test")
	if err != nil {
		t.Fatal(err)
	}
	var domain string
	if err = pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,'default') RETURNING id::text", owner.InstanceID).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("artifact-bytes-"), 50000)
	info := putBlob(t, stores, domain, payload)
	orphan := putBlob(t, stores, domain, []byte("never referenced by metadata"))
	if _, err = pool.Exec(ctx, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,'k')", owner.InstanceID, domain, string(info.Digest), info.Size); err != nil {
		t.Fatal(err)
	}
	rec, err := (secrets.Store{DB: pool, Keys: keyring(t, rootKeyBytes)}).Create(ctx, owner.InstanceID, owner.PrincipalID, "webhook", []byte(secretPlaintext))
	if err != nil {
		t.Fatal(err)
	}
	return source{pool, stores, owner, domain, info, orphan, rec.ID}
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRoundTripIsConsistentAndKeepsSecretsEncrypted(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "bk")
	res, err := Create(ctx, src.pool, src.stores, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Manifest
	if m.SchemaVersion < 18 || len(m.Blobs) != 1 || m.Blobs[0].Digest != string(src.blob.Digest) || m.Blobs[0].Size != src.blob.Size {
		t.Fatalf("unexpected manifest %+v", m.Blobs)
	}
	if len(m.Skipped) == 0 {
		t.Fatal("ephemeral tables not skipped")
	}
	// Root key and plaintext secrets must be absent from every backup file.
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(p)
			if bytes.Contains(data, []byte(secretPlaintext)) || bytes.Contains(data, rootKeyBytes) {
				t.Errorf("%s leaks key or plaintext", p)
			}
		}
		return nil
	})
	if sum, err := Verify(ctx, dir, res.ManifestSHA256, 2); err != nil || sum.Blobs != 1 {
		t.Fatalf("verify: %v", err)
	}

	dst := freshDatabase(t, false)
	dstStores := newStores(t)
	out, err := Restore(ctx, dst, dstStores, dir, RestoreOptions{Keys: keyring(t, rootKeyBytes), ExpectManifestSHA256: res.ManifestSHA256})
	if err != nil {
		t.Fatal(err)
	}
	if out.SecretsVerified != 1 || !out.MigratedForward || out.Blobs != 1 {
		t.Fatalf("summary %+v", out)
	}
	for _, tb := range m.Tables {
		if a, b := count(t, src.pool, quote(tb.Name)), count(t, dst, quote(tb.Name)); a != b || int64(a) != tb.Rows {
			t.Errorf("table %s: source %d restored %d manifest %d", tb.Name, a, b, tb.Rows)
		}
	}
	store, _ := dstStores.Resolve(ctx, src.domain)
	if info, err := store.Stat(ctx, src.blob.Digest); err != nil || info != src.blob {
		t.Fatalf("blob not restored: %v", err)
	}
	if _, err := store.Stat(ctx, src.orphan.Digest); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("unreferenced blob was copied")
	}
	// Secret decrypts only with the original keyring and stays ciphertext at rest.
	_, plain, err := (secrets.Store{DB: dst, Keys: keyring(t, rootKeyBytes)}).Open(ctx, src.owner.InstanceID, src.secretID, "webhook")
	if err != nil || string(plain) != secretPlaintext {
		t.Fatalf("restored secret unreadable: %v", err)
	}
	var raw string
	dst.QueryRow(ctx, "SELECT envelope::text FROM encrypted_secrets").Scan(&raw)
	if bytes.Contains([]byte(raw), []byte(secretPlaintext)) {
		t.Fatal("secret stored in clear")
	}
	// Identity sequences continue after restored rows.
	if _, err = dst.Exec(ctx, "INSERT INTO audit_events(instance_id,principal_id,action,resource_id) SELECT instance_id,principal_id,'post-restore',resource_id FROM audit_events LIMIT 1"); err != nil {
		t.Fatalf("post-restore audit insert: %v", err)
	}
}

func TestTamperingIsDetectedAndRestoreLeavesTargetEmpty(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	for name, tamper := range map[string]func(t *testing.T, dir string, m Manifest){
		"blob byte flip": func(t *testing.T, dir string, m Manifest) {
			p := filepath.Join(dir, blobPath(m.Blobs[0]))
			data, _ := os.ReadFile(p)
			data[len(data)/2] ^= 1
			os.Chmod(p, 0o600)
			os.WriteFile(p, data, 0o600)
		},
		"blob truncated": func(t *testing.T, dir string, m Manifest) {
			os.Truncate(filepath.Join(dir, blobPath(m.Blobs[0])), 10)
		},
		"table edited": func(t *testing.T, dir string, m Manifest) {
			p := filepath.Join(dir, tablePath("principals"))
			data, _ := os.ReadFile(p)
			os.WriteFile(p, append(data, '\n'), 0o600)
		},
		"extra file": func(t *testing.T, dir string, m Manifest) {
			os.WriteFile(filepath.Join(dir, "blobs", "planted"), []byte("x"), 0o600)
		},
		"symlinked blob": func(t *testing.T, dir string, m Manifest) {
			p := filepath.Join(dir, blobPath(m.Blobs[0]))
			os.Remove(p)
			os.Symlink("/etc/hosts", p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bk")
			res, err := Create(ctx, src.pool, src.stores, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			tamper(t, dir, res.Manifest)
			if _, err := Verify(ctx, dir, "", 2); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("verify accepted tampering: %v", err)
			}
			dst := freshDatabase(t, false)
			if _, err := Restore(ctx, dst, newStores(t), dir, RestoreOptions{}); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("restore accepted tampering: %v", err)
			}
			var tables int
			dst.QueryRow(ctx, "SELECT count(*) FROM pg_class WHERE relname='instances'").Scan(&tables)
			if tables == 1 && count(t, dst, "instances") != 0 {
				t.Fatal("tampered restore left data behind")
			}
		})
	}
	t.Run("manifest digest pin", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "bk")
		if _, err := Create(ctx, src.pool, src.stores, dir, Options{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(ctx, dir, "00"+string(bytes.Repeat([]byte("a"), 62)), 1); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("pin ignored: %v", err)
		}
	})
	t.Run("manifest edited", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "bk")
		if _, err := Create(ctx, src.pool, src.stores, dir, Options{}); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, manifestName), []byte(`{"format_version":1}`), 0o600)
		if _, err := Verify(ctx, dir, "", 1); !errors.Is(err, ErrManifest) {
			t.Fatalf("bad manifest accepted: %v", err)
		}
	})
}

func TestRestoreRefusesNonEmptyTargetAndForceIsNarrow(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "bk")
	if _, err := Create(ctx, src.pool, src.stores, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, src.pool, src.stores, dir, Options{}); !errors.Is(err, ErrTarget) {
		t.Fatalf("reused backup directory: %v", err)
	}
	// A populated database is refused with and without --force.
	for _, force := range []bool{false, true} {
		if _, err := Restore(ctx, src.pool, newStores(t), dir, RestoreOptions{Force: force}); !errors.Is(err, ErrTarget) {
			t.Fatalf("force=%v restored over live data: %v", force, err)
		}
	}
	// A migrated but unused database is refused without force and accepted with it.
	unused := freshDatabase(t, true)
	if _, err := Restore(ctx, unused, newStores(t), dir, RestoreOptions{}); !errors.Is(err, ErrTarget) {
		t.Fatalf("migrated schema accepted without force: %v", err)
	}
	if _, err := Restore(ctx, unused, newStores(t), dir, RestoreOptions{Force: true, Keys: keyring(t, rootKeyBytes)}); err != nil {
		t.Fatalf("forced restore into unused schema: %v", err)
	}
	if count(t, unused, "instances") != 1 {
		t.Fatal("restore incomplete")
	}
	// Restoring again now hits populated data.
	if _, err := Restore(ctx, unused, newStores(t), dir, RestoreOptions{Force: true}); !errors.Is(err, ErrTarget) {
		t.Fatalf("second restore accepted: %v", err)
	}
}

func TestWrongKeyringRollsBackRestore(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "bk")
	if _, err := Create(ctx, src.pool, src.stores, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	dst := freshDatabase(t, false)
	_, err := Restore(ctx, dst, newStores(t), dir, RestoreOptions{Keys: keyring(t, bytes.Repeat([]byte{9}, 32))})
	if !errors.Is(err, ErrKeyring) {
		t.Fatalf("wrong keyring accepted: %v", err)
	}
	if count(t, dst, "instances") != 0 || count(t, dst, "encrypted_secrets") != 0 {
		t.Fatal("rollback left rows")
	}
	// The same target is still empty enough to retry with the right keyring.
	if _, err = Restore(ctx, dst, newStores(t), dir, RestoreOptions{Force: true, Keys: keyring(t, rootKeyBytes)}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestSnapshotExcludesConcurrentWrites(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	before := count(t, src.pool, "audit_events")
	// A writer commits after the snapshot is taken but before blobs finish; the
	// manifest row count must still reflect the snapshot.
	dir := filepath.Join(t.TempDir(), "bk")
	res, err := Create(ctx, src.pool, src.stores, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = src.pool.Exec(ctx, "INSERT INTO audit_events(instance_id,principal_id,action,resource_id) SELECT instance_id,principal_id,'late',resource_id FROM audit_events LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	for _, tb := range res.Manifest.Tables {
		if tb.Name == "audit_events" && int(tb.Rows) != before {
			t.Fatalf("snapshot rows %d want %d", tb.Rows, before)
		}
	}
}

// A backup of an instance with attached sources and replication history must
// restore: loading feed_sources and assets must not fire the replication
// triggers (they would duplicate the restored replication_changes rows).
func TestRestoreWithFeedSourcesAndReplicationRows(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	inst := src.owner.InstanceID
	var feed, source, comp, version, blobID string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(src.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'rt','raw') RETURNING id::text", inst).Scan(&feed))
	must(src.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'rt-local','local','raw') RETURNING id::text", inst).Scan(&source))
	must(src.pool.QueryRow(ctx, "INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'a/b') RETURNING id::text", inst, source).Scan(&comp))
	must(src.pool.QueryRow(ctx, "INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1') RETURNING id::text", inst, comp).Scan(&version))
	must(src.pool.QueryRow(ctx, "SELECT id::text FROM blobs WHERE instance_id=$1 LIMIT 1", inst).Scan(&blobID))
	_, err := src.pool.Exec(ctx, "INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'f',$3)", inst, version, blobID)
	must(err)
	_, err = src.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", inst, feed, source)
	must(err)
	if count(t, src.pool, "replication_changes") < 2 {
		t.Fatalf("fixture produced %d replication changes", count(t, src.pool, "replication_changes"))
	}
	dir := filepath.Join(t.TempDir(), "bk")
	res, err := Create(ctx, src.pool, src.stores, dir, Options{})
	must(err)
	dst := freshDatabase(t, false)
	_, err = Restore(ctx, dst, newStores(t), dir, RestoreOptions{Keys: keyring(t, rootKeyBytes), ExpectManifestSHA256: res.ManifestSHA256})
	must(err)
	for _, table := range []string{"feed_sources", "replication_changes", "assets"} {
		if a, b := count(t, src.pool, table), count(t, dst, table); a != b {
			t.Errorf("%s: source %d restored %d", table, a, b)
		}
	}
	// Triggers are active again after the load.
	before := count(t, dst, "replication_changes")
	_, err = dst.Exec(ctx, "INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'g',$3)", inst, version, blobID)
	must(err)
	if count(t, dst, "replication_changes") != before+1 {
		t.Fatal("replication triggers stayed disabled after restore")
	}
}
