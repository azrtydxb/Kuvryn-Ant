package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RestoreOptions struct {
	// Force permits restoring into a schema that already holds the Kuvryn tables
	// as long as every one of them is empty (for example after the service was
	// started once and migrated an unused database). It never overwrites data.
	Force bool
	// Keys is the deployment keyring. When set, every restored secret must open
	// with it before the restore commits.
	Keys *secrets.Keyring
	// ExpectManifestSHA256 pins the exact manifest recorded at backup time.
	ExpectManifestSHA256 string
	Concurrency          int
}

// RestoreSummary extends Summary with restore-only facts.
type RestoreSummary struct {
	Summary
	SecretsVerified int  `json:"secrets_verified"`
	MigratedForward bool `json:"migrated_to_current_schema"`
	// DroppedCacheVersions counts cache versions removed because a blob they used
	// was missing when the backup ran (or from the target store, in reference mode).
	DroppedCacheVersions int `json:"dropped_cache_versions,omitempty"`
}

// Restore loads a backup into an empty database and blob store.
//
// Order: validate manifest and table digests, check the target is empty, migrate
// to the backup's schema version, copy and digest-verify every blob into the
// store, then load all tables in ONE transaction (digests, row counts and secret
// decryptability are verified before commit), then migrate forward to the
// current schema. A failure before commit leaves the tables empty; blobs already
// written are unreferenced content-addressed objects that are harmless.
func Restore(ctx context.Context, pool *pgxpool.Pool, stores Stores, dir string, opts RestoreOptions) (RestoreSummary, error) {
	t, err := OpenDirTree(dir, false)
	if err != nil {
		return RestoreSummary{}, err
	}
	defer t.Close()
	return RestoreTree(ctx, pool, stores, t, opts)
}

// RestoreTree is Restore for a backup in any Tree. A reference-mode backup
// carries no blob bytes: restore then requires every referenced blob to exist
// in the target store with the recorded size and fails before touching the
// database otherwise.
func RestoreTree(ctx context.Context, pool *pgxpool.Pool, stores Stores, root Tree, opts RestoreOptions) (RestoreSummary, error) {
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	m, sum, err := readManifest(ctx, root)
	if err != nil {
		return RestoreSummary{}, err
	}
	if err = checkExpected(sum, opts.ExpectManifestSHA256); err != nil {
		return RestoreSummary{}, err
	}
	if err = checkMigrations(m); err != nil {
		return RestoreSummary{}, err
	}
	if err = verifyTables(ctx, root, m); err != nil {
		return RestoreSummary{}, err
	}
	if err = rejectUnlisted(ctx, root, m); err != nil {
		return RestoreSummary{}, err
	}
	if err = checkTarget(ctx, pool, opts.Force); err != nil {
		return RestoreSummary{}, err
	}
	// Best-effort (cache-only) blobs absent from the backup or, in reference mode,
	// from the target store are tolerated; their cache versions are dropped below.
	blobs := slices.Clone(m.Blobs)
	if m.referenced() {
		if err = checkBlobs(ctx, stores, blobs, opts.Concurrency, true); err != nil {
			return RestoreSummary{}, fmt.Errorf("%w: a reference-mode backup needs its blobs in the target store: %v", ErrTarget, err)
		}
	}
	if err = database.MigrateTo(ctx, pool, m.SchemaVersion); err != nil {
		return RestoreSummary{}, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	order, err := checkSchemaMatches(ctx, pool, m)
	if err != nil {
		return RestoreSummary{}, err
	}
	if !m.referenced() {
		if err = restoreBlobs(ctx, stores, root, blobs, opts.Concurrency); err != nil {
			return RestoreSummary{}, err
		}
	}
	verified, err := loadTables(ctx, pool, root, m, order, opts.Keys)
	if err != nil {
		return RestoreSummary{}, err
	}
	result := RestoreSummary{Summary: summarize(m, sum), SecretsVerified: verified}
	if err = database.Migrate(ctx, pool); err != nil {
		return result, fmt.Errorf("data restored at schema %d but upgrade to current schema failed: %w", m.SchemaVersion, err)
	}
	result.MigratedForward = true
	var missing []Blob
	for _, b := range blobs {
		if b.Missing {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		if result.DroppedCacheVersions, err = dropMissingCache(ctx, pool, missing); err != nil {
			return result, fmt.Errorf("data restored but dropping cache content with missing blobs failed: %w", err)
		}
	}
	return result, nil
}

// checkMigrations requires the backup's schema history to be a prefix of this
// binary's embedded history (same names and checksums).
func checkMigrations(m Manifest) error {
	embedded, err := database.Migrations()
	if err != nil {
		return err
	}
	if m.SchemaVersion > len(embedded) {
		return fmt.Errorf("%w: backup is at schema %d, this binary supports %d; use a newer ant release", ErrSchema, m.SchemaVersion, len(embedded))
	}
	for i, x := range m.Migrations {
		if embedded[i].Name != x.Name || embedded[i].Checksum != x.Checksum {
			return fmt.Errorf("%w: migration %d differs from this binary", ErrSchema, x.Version)
		}
	}
	return nil
}

func relationCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND c.relkind IN ('r','p','v','m','f')`).Scan(&n)
	return n, err
}

// checkTarget enforces "restore only into an empty database". With force, the
// only accepted non-empty state is a Kuvryn schema (schema_migrations present)
// whose data tables all contain zero rows.
func checkTarget(ctx context.Context, pool *pgxpool.Pool, force bool) error {
	n, err := relationCount(ctx, pool)
	if err != nil {
		return errors.New("cannot inspect target database")
	}
	if n == 0 {
		return nil
	}
	if !force {
		return fmt.Errorf("%w: target database is not empty; restore needs a fresh database (--force only accepts a migrated schema with zero rows)", ErrTarget)
	}
	var ledger bool
	if err = pool.QueryRow(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&ledger); err != nil || !ledger {
		return fmt.Errorf("%w: --force requires an unused Kuvryn schema", ErrTarget)
	}
	tables, err := allTables(ctx, pool)
	if err != nil {
		return errors.New("cannot inspect target database")
	}
	for _, t := range tables {
		var has bool
		if err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+quote(t)+")").Scan(&has); err != nil {
			return errors.New("cannot inspect target database")
		}
		if has {
			return fmt.Errorf("%w: table %s contains data; refusing to overwrite", ErrTarget, t)
		}
	}
	return nil
}

// checkSchemaMatches confirms the migrated target has exactly the tables and
// columns the backup recorded and returns the load order from the target's own
// foreign keys.
func checkSchemaMatches(ctx context.Context, pool *pgxpool.Pool, m Manifest) ([]string, error) {
	tables, err := allTables(ctx, pool)
	if err != nil {
		return nil, err
	}
	want := map[string]Table{}
	var names []string
	for _, t := range m.Tables {
		want[t.Name] = t
		names = append(names, t.Name)
	}
	for _, t := range tables {
		if _, ok := want[t]; !ok && !slices.Contains(m.Skipped, t) {
			return nil, fmt.Errorf("%w: target table %s is not in the backup", ErrSchema, t)
		}
	}
	for _, t := range m.Tables {
		if !slices.Contains(tables, t.Name) {
			return nil, fmt.Errorf("%w: backup table %s does not exist in target schema", ErrSchema, t.Name)
		}
		cols, err := tableColumns(ctx, pool, t.Name)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(cols, t.Columns) {
			return nil, fmt.Errorf("%w: columns of %s differ", ErrSchema, t.Name)
		}
	}
	fks, err := foreignKeys(ctx, pool)
	if err != nil {
		return nil, err
	}
	return loadOrder(names, fks)
}

func restoreBlobs(ctx context.Context, stores Stores, root Tree, blobs []Blob, workers int) error {
	resolved := map[string]blob.Store{}
	for _, b := range blobs {
		if resolved[b.Domain] != nil {
			continue
		}
		store, err := stores.Resolve(ctx, b.Domain)
		if err != nil {
			return fmt.Errorf("cannot resolve blob store for domain %s", b.Domain)
		}
		resolved[b.Domain] = store
	}
	return forEach(ctx, len(blobs), workers, func(ctx context.Context, i int) error {
		b := blobs[i]
		if b.Missing {
			return nil
		}
		if err := copyBlobIn(ctx, resolved[b.Domain], root, b); err != nil {
			if errors.Is(err, blob.ErrIntegrity) {
				err = ErrIntegrity
			}
			return fmt.Errorf("blob %s (domain %s): %w", b.Digest, b.Domain, err)
		}
		return nil
	})
}

// copyBlobIn streams one blob into the store. Finalize recomputes digest and
// size from the bytes actually written and refuses a mismatch (or a different
// object already stored under the same digest), so a corrupt backup file cannot
// be published under a valid address.
func copyBlobIn(ctx context.Context, store blob.Store, root Tree, b Blob) error {
	f, size, err := root.Open(ctx, blobPath(b))
	if err != nil {
		return err
	}
	defer f.Close()
	if size != b.Size {
		return ErrIntegrity
	}
	up, err := store.Stage(ctx)
	if err != nil {
		return err
	}
	defer up.Abort()
	if _, err = io.CopyBuffer(up, ctxReader{ctx, f}, make([]byte, blob.TransferBufferSize)); err != nil {
		return err
	}
	want := blob.Info{Digest: blob.Digest(b.Digest), Size: b.Size}
	got, err := up.Finalize(ctx, want)
	if err != nil {
		return err
	}
	if got != want {
		return ErrIntegrity
	}
	stat, err := store.Stat(ctx, want.Digest)
	if err != nil || stat != want {
		return ErrIntegrity
	}
	return nil
}

func loadTables(ctx context.Context, pool *pgxpool.Pool, root Tree, m Manifest, order []string, keys *secrets.Keyring) (int, error) {
	byName := map[string]Table{}
	for _, t := range m.Tables {
		byName[t.Name] = t
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, errors.New("cannot start restore transaction")
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	// Restored rows already carry the changes their triggers would record (and the
	// federation origin guards must not veto them), so user triggers stay off
	// for the load only. Needs table ownership, not superuser.
	for _, name := range order {
		if _, err = tx.Exec(ctx, "ALTER TABLE "+quote(name)+" DISABLE TRIGGER USER"); err != nil {
			return 0, fmt.Errorf("cannot defer triggers of table %s: %w", name, err)
		}
	}
	for _, name := range order {
		t := byName[name]
		if err = loadTable(ctx, tx, root, t); err != nil {
			return 0, fmt.Errorf("load of table %s failed: %w", t.Name, err)
		}
	}
	for _, name := range order {
		if _, err = tx.Exec(ctx, "ALTER TABLE "+quote(name)+" ENABLE TRIGGER USER"); err != nil {
			return 0, fmt.Errorf("cannot restore triggers of table %s: %w", name, err)
		}
	}
	if err = resetIdentities(ctx, tx); err != nil {
		return 0, err
	}
	verified := 0
	if keys != nil {
		if verified, err = verifySecrets(ctx, tx, keys); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, errors.New("cannot commit restore")
	}
	return verified, nil
}

func loadTable(ctx context.Context, tx pgx.Tx, root Tree, t Table) error {
	f, _, err := root.Open(ctx, tablePath(t.Name))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	counter := &countWriter{w: h}
	tag, err := tx.Conn().PgConn().CopyFrom(ctx, io.TeeReader(ctxReader{ctx, f}, counter), "COPY "+quote(t.Name)+" ("+quoteList(t.Columns)+") FROM STDIN")
	if err != nil {
		return err
	}
	if counter.n != t.Bytes || hex.EncodeToString(h.Sum(nil)) != t.SHA256 {
		return ErrIntegrity
	}
	if tag.RowsAffected() != t.Rows {
		return fmt.Errorf("%w: row count %d does not match manifest %d", ErrIntegrity, tag.RowsAffected(), t.Rows)
	}
	return nil
}

// resetIdentities moves identity sequences past the restored rows so new audit
// and outbox events cannot collide with restored ids.
func resetIdentities(ctx context.Context, tx pgx.Tx) error {
	ids, err := identityColumns(ctx, tx)
	if err != nil {
		return err
	}
	for table, cols := range ids {
		for _, c := range cols {
			q := "SELECT setval(pg_get_serial_sequence($1,$2), COALESCE(max(" + quote(c) + "),1), max(" + quote(c) + ") IS NOT NULL) FROM " + quote(table)
			if _, err := tx.Exec(ctx, q, quote(table), c); err != nil {
				return fmt.Errorf("cannot reset identity of %s: %w", table, err)
			}
		}
	}
	return nil
}

// verifySecrets proves the configured keyring is the one that protected the
// backed-up secrets. Plaintext is cleared immediately and never logged.
func verifySecrets(ctx context.Context, tx pgx.Tx, keys *secrets.Keyring) (int, error) {
	rows, err := tx.Query(ctx, "SELECT instance_id::text,id::text,purpose,envelope FROM encrypted_secrets")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var instance, id, purpose string
		var raw []byte
		if err = rows.Scan(&instance, &id, &purpose, &raw); err != nil {
			return 0, err
		}
		var env secrets.Envelope
		if json.Unmarshal(raw, &env) != nil {
			return 0, ErrKeyring
		}
		plain, err := keys.Open(secrets.Context{RecordID: instance + "/" + id, Purpose: purpose}, env)
		if err != nil {
			return 0, ErrKeyring
		}
		clear(plain)
		n++
	}
	return n, rows.Err()
}

// dropMissingCache removes the cache content that depended on blobs whose bytes
// are gone: every version with an asset on a missing blob, the versions whose
// manifests name those versions (version_refs, transitively: an index names
// manifests, a manifest names layers), their tags and assets, and the blob rows
// themselves. The cache feed then simply has fewer objects, so a client sees a
// cache miss and rebuilds it. Only best-effort blobs reach this function, so a
// dropped version never held live (non-cache) content. Bytes of the dropped
// versions' other blobs become unreferenced and are collected by normal GC.
func dropMissingCache(ctx context.Context, pool *pgxpool.Pool, missing []Blob) (int, error) {
	domains := make([]string, len(missing))
	digests := make([]string, len(missing))
	for i, b := range missing {
		domains[i], digests[i] = b.Domain, b.Digest
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE dropped_cache_versions ON COMMIT DROP AS
 WITH RECURSIVE gone(version_id) AS (
  SELECT a.version_id FROM assets a JOIN blobs b ON b.id=a.blob_id
   JOIN unnest($1::uuid[],$2::text[]) m(dom,dig) ON b.dedup_domain_id=m.dom AND b.digest=m.dig
  UNION
  SELECT r.parent_version_id FROM version_refs r
   JOIN versions cv ON cv.component_id=r.component_id AND cv.version=r.ref_version
   JOIN gone g ON g.version_id=cv.id
 ) SELECT version_id FROM gone`, domains, digests); err != nil {
		return 0, err
	}
	var n int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM dropped_cache_versions").Scan(&n); err != nil {
		return 0, err
	}
	for _, q := range []string{
		`DELETE FROM tags WHERE version_id IN (SELECT version_id FROM dropped_cache_versions)`,
		`DELETE FROM assets WHERE version_id IN (SELECT version_id FROM dropped_cache_versions)`,
		`DELETE FROM versions WHERE id IN (SELECT version_id FROM dropped_cache_versions)`,
	} {
		if _, err = tx.Exec(ctx, q); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM blobs b USING unnest($1::uuid[],$2::text[]) m(dom,dig)
 WHERE b.dedup_domain_id=m.dom AND b.digest=m.dig
 AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id) AND NOT EXISTS(SELECT 1 FROM federation_route_cache rc WHERE rc.blob_id=b.id)`, domains, digests); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}
