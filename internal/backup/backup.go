package backup

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stores resolves the blob store of a deduplication domain (storage.Registry).
type Stores interface {
	Resolve(ctx context.Context, domain string) (blob.Store, error)
}

type Options struct {
	// Concurrency bounds simultaneous blob transfers. Default 4.
	Concurrency int
}

// Result summarizes a created backup. ManifestSHA256 should be recorded outside
// the backup location so a swapped backup can be detected later.
type Result struct {
	Manifest       Manifest
	ManifestSHA256 string
}

// Create writes a consistent backup into dir, which must be absent or empty.
//
// Consistency: every table is read inside one REPEATABLE READ, read-only
// transaction (a single PostgreSQL snapshot), and the blob list is taken from
// that same snapshot. Writers are never blocked. Blobs are immutable and
// content-addressed, so copying them after the snapshot is safe as long as
// nothing deletes them; blobs not referenced by the snapshot are not copied.
// Every blob is hashed while it is copied and must match its recorded digest
// and size. On any failure the partial output is removed.
func Create(ctx context.Context, pool *pgxpool.Pool, stores Stores, dir string, opts Options) (res Result, err error) {
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	root, err := prepareTarget(dir)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	defer func() {
		if err != nil {
			_ = root.RemoveAll("tables")
			_ = root.RemoveAll("blobs")
			_ = root.Remove(manifestName + ".tmp")
		}
	}()
	m, err := snapshotTables(ctx, pool, root)
	if err != nil {
		return Result{}, err
	}
	if err = copyBlobs(ctx, stores, root, m.Blobs, opts.Concurrency); err != nil {
		return Result{}, err
	}
	m.CreatedAt = time.Now().UTC()
	m.Notice = notice
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return Result{}, err
	}
	if err = writeAtomic(root, manifestName, data); err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(data)
	return Result{m, hex.EncodeToString(sum[:])}, nil
}

func prepareTarget(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: cannot create directory", ErrTarget)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return nil, fmt.Errorf("%w: backup directory must be empty", ErrTarget)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open directory", ErrTarget)
	}
	for _, d := range []string{"tables", "blobs"} {
		if err := root.Mkdir(d, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

func snapshotTables(ctx context.Context, pool *pgxpool.Pool, root *os.Root) (Manifest, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Manifest{}, errors.New("cannot acquire database connection")
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Manifest{}, errors.New("cannot start snapshot transaction")
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	m := Manifest{FormatVersion: FormatVersion}
	if err = tx.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&m.ServerVersion); err != nil {
		return Manifest{}, err
	}
	rows, err := tx.Query(ctx, "SELECT version,name,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: schema history unreadable (database not initialized?)", ErrSchema)
	}
	for rows.Next() {
		var x Migration
		if err = rows.Scan(&x.Version, &x.Name, &x.Checksum); err != nil {
			rows.Close()
			return Manifest{}, err
		}
		m.Migrations = append(m.Migrations, x)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Manifest{}, err
	}
	m.SchemaVersion = len(m.Migrations)
	tables, err := allTables(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	var included []string
	m.Skipped = []string{}
	for _, t := range tables {
		if ephemeral[t] {
			m.Skipped = append(m.Skipped, t)
		} else {
			included = append(included, t)
		}
	}
	fks, err := foreignKeys(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	order, err := loadOrder(included, fks)
	if err != nil {
		return Manifest{}, err
	}
	for _, t := range order {
		cols, err := tableColumns(ctx, tx, t)
		if err != nil {
			return Manifest{}, err
		}
		entry := Table{Name: t, Columns: cols}
		entry.Rows, entry.Bytes, entry.SHA256, err = dumpTable(ctx, tx, root, t, cols)
		if err != nil {
			return Manifest{}, fmt.Errorf("dump of table %s failed: %w", t, err)
		}
		m.Tables = append(m.Tables, entry)
	}
	m.Blobs = []Blob{}
	brows, err := tx.Query(ctx, "SELECT DISTINCT dedup_domain_id::text, digest, size_bytes FROM blobs ORDER BY 1,2")
	if err != nil {
		return Manifest{}, err
	}
	for brows.Next() {
		var b Blob
		if err = brows.Scan(&b.Domain, &b.Digest, &b.Size); err != nil {
			brows.Close()
			return Manifest{}, err
		}
		m.Blobs = append(m.Blobs, b)
	}
	brows.Close()
	if err = brows.Err(); err != nil {
		return Manifest{}, err
	}
	if err = m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func dumpTable(ctx context.Context, tx pgx.Tx, root *os.Root, table string, cols []string) (rows, size int64, sum string, err error) {
	f, err := root.OpenFile(tablePath(table), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	buffered := bufio.NewWriterSize(f, 256<<10)
	counter := &countWriter{w: io.MultiWriter(buffered, h)}
	tag, err := tx.Conn().PgConn().CopyTo(ctx, counter, "COPY "+quote(table)+" ("+quoteList(cols)+") TO STDOUT")
	if err != nil {
		return 0, 0, "", err
	}
	if err = buffered.Flush(); err != nil {
		return 0, 0, "", err
	}
	if err = f.Sync(); err != nil {
		return 0, 0, "", err
	}
	return tag.RowsAffected(), counter.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func copyBlobs(ctx context.Context, stores Stores, root *os.Root, blobs []Blob, workers int) error {
	made := map[string]blob.Store{}
	for _, b := range blobs {
		if made[b.Domain] != nil {
			continue
		}
		store, err := stores.Resolve(ctx, b.Domain)
		if err != nil {
			return fmt.Errorf("cannot resolve blob store for domain %s", b.Domain)
		}
		made[b.Domain] = store
		if err := root.Mkdir(path.Join("blobs", b.Domain), 0o700); err != nil {
			return err
		}
	}
	return forEach(ctx, len(blobs), workers, func(ctx context.Context, i int) error {
		b := blobs[i]
		if err := copyBlobOut(ctx, made[b.Domain], root, b); err != nil {
			return fmt.Errorf("blob %s (domain %s): %w", b.Digest, b.Domain, err)
		}
		return nil
	})
}

func copyBlobOut(ctx context.Context, store blob.Store, root *os.Root, b Blob) error {
	src, err := store.OpenRange(ctx, blob.Digest(b.Digest), 0, -1)
	if err != nil {
		return err // blob.ErrNotFound means metadata references a missing object
	}
	defer src.Close()
	f, err := root.OpenFile(blobPath(b), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, h), ctxReader{ctx, src}, make([]byte, blob.TransferBufferSize))
	if err != nil {
		return err
	}
	if n != b.Size || hex.EncodeToString(h.Sum(nil)) != b.Digest {
		return blob.ErrIntegrity
	}
	return f.Sync()
}

func writeAtomic(root *os.Root, name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return root.Rename(tmp, name)
}
