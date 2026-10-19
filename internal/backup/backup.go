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
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diskusage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stores resolves the blob store of a deduplication domain (storage.Registry).
type Stores interface {
	Resolve(ctx context.Context, domain string) (blob.Store, error)
}

type Options struct {
	// Concurrency bounds simultaneous blob transfers. Default 4.
	Concurrency int
	// Blobs selects BlobsCopy (default) or BlobsReference. Reference mode records
	// the blob list in the manifest and verifies each blob exists with the right
	// size, but copies no blob bytes.
	Blobs string
	// MaxDiskPercent, when positive, refuses to write a backup into a local
	// directory whose filesystem is, or would become, fuller than this.
	MaxDiskPercent int
}

// Result summarizes a created backup. ManifestSHA256 should be recorded outside
// the backup location so a swapped backup can be detected later.
type Result struct {
	Manifest       Manifest
	ManifestSHA256 string
}

// ErrDiskFull is returned (wrapping ErrTarget) when a local backup would push
// its filesystem over Options.MaxDiskPercent.
var ErrDiskFull = fmt.Errorf("%w: not enough free space", ErrTarget)

// Create writes a consistent backup into dir, which must be absent or empty.
func Create(ctx context.Context, pool *pgxpool.Pool, stores Stores, dir string, opts Options) (Result, error) {
	t, err := OpenDirTree(dir, true)
	if err != nil {
		return Result{}, err
	}
	defer t.Close()
	return CreateTree(ctx, pool, stores, t, opts)
}

// CreateTree writes a consistent backup into an empty tree (a directory or an
// object-storage prefix).
//
// Consistency: every table is read inside one REPEATABLE READ, read-only
// transaction (a single PostgreSQL snapshot), and the blob list is taken from
// that same snapshot. Writers are never blocked. Blobs are immutable and
// content-addressed, so copying them after the snapshot is safe as long as
// nothing deletes them; blobs not referenced by the snapshot are not copied.
// Every blob is hashed while it is copied and must match its recorded digest
// and size. In reference mode every blob is instead checked to exist with the
// recorded size, and the backup stays valid only while those objects are kept.
// On any failure the partial output is removed.
func CreateTree(ctx context.Context, pool *pgxpool.Pool, stores Stores, root Tree, opts Options) (res Result, err error) {
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	if opts.Blobs == "" {
		opts.Blobs = BlobsCopy
	}
	if opts.Blobs != BlobsCopy && opts.Blobs != BlobsReference {
		return Result{}, fmt.Errorf("%w: unknown blob mode %q", ErrTarget, opts.Blobs)
	}
	if err = emptyTree(ctx, root); err != nil {
		return Result{}, err
	}
	if err = checkDisk(root, opts.MaxDiskPercent, 0); err != nil {
		return Result{}, err
	}
	defer func() {
		if err != nil {
			clean := context.WithoutCancel(ctx)
			for _, n := range []string{"tables", "blobs", manifestName, manifestName + ".tmp"} {
				_ = root.RemoveAll(clean, n)
			}
		}
	}()
	m, err := snapshotTables(ctx, pool, root)
	if err != nil {
		return Result{}, err
	}
	m.BlobMode = opts.Blobs
	if opts.Blobs == BlobsReference {
		err = checkBlobs(ctx, stores, m.Blobs, opts.Concurrency)
	} else {
		if err = checkDisk(root, opts.MaxDiskPercent, m.BlobBytes()); err == nil {
			err = copyBlobs(ctx, stores, root, m.Blobs, opts.Concurrency)
		}
	}
	if err != nil {
		return Result{}, err
	}
	m.CreatedAt = time.Now().UTC()
	m.Notice = notice
	if m.referenced() {
		m.Notice = notice + " " + referenceNotice
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return Result{}, err
	}
	if err = root.PutAtomic(ctx, manifestName, data); err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(data)
	return Result{m, hex.EncodeToString(sum[:])}, nil
}

// checkDisk refuses to write into a local directory that is, or after adding
// extra more bytes would be, over maxPercent full. Object storage is unchecked.
func checkDisk(t Tree, maxPercent int, extra int64) error {
	d, ok := t.(*DirTree)
	if !ok || maxPercent <= 0 {
		return nil
	}
	u, err := diskusage.Of(d.Dir())
	if err != nil || u.Total == 0 {
		return nil // cannot inspect (unsupported platform): do not block backups
	}
	used := u.Total - u.Free
	limit := u.Total / 100 * uint64(maxPercent) // no overflow for any real capacity
	if add := uint64(max(extra, 0)); add > limit || used > limit-add {
		projected := 100.0 * (float64(used) + float64(add)) / float64(u.Total)
		return fmt.Errorf("%w: the backup would raise filesystem usage to %.0f%% (limit %d%%); write to object storage (s3://...) or another volume, use --blobs=reference, or raise --max-disk-percent", ErrDiskFull, projected, maxPercent)
	}
	return nil
}

// checkBlobs confirms each referenced blob exists with the recorded size.
func checkBlobs(ctx context.Context, stores Stores, blobs []Blob, workers int) error {
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
	}
	return forEach(ctx, len(blobs), workers, func(ctx context.Context, i int) error {
		b := blobs[i]
		info, err := made[b.Domain].Stat(ctx, blob.Digest(b.Digest))
		if err != nil {
			return fmt.Errorf("blob %s (domain %s): %w", b.Digest, b.Domain, err)
		}
		if info.Size != b.Size {
			return fmt.Errorf("blob %s (domain %s): %w", b.Digest, b.Domain, blob.ErrIntegrity)
		}
		return nil
	})
}

func snapshotTables(ctx context.Context, pool *pgxpool.Pool, root Tree) (Manifest, error) {
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

func dumpTable(ctx context.Context, tx pgx.Tx, root Tree, table string, cols []string) (rows, size int64, sum string, err error) {
	h := sha256.New()
	var counter *countWriter
	var tag pgconn.CommandTag
	err = copyOut(ctx, root, tablePath(table), func(w io.Writer) error {
		buffered := bufio.NewWriterSize(w, 256<<10)
		counter = &countWriter{w: io.MultiWriter(buffered, h)}
		var cerr error
		tag, cerr = tx.Conn().PgConn().CopyTo(ctx, counter, "COPY "+quote(table)+" ("+quoteList(cols)+") TO STDOUT")
		if cerr != nil {
			return cerr
		}
		return buffered.Flush()
	})
	if err != nil {
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

func copyBlobs(ctx context.Context, stores Stores, root Tree, blobs []Blob, workers int) error {
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
	}
	return forEach(ctx, len(blobs), workers, func(ctx context.Context, i int) error {
		b := blobs[i]
		if err := copyBlobOut(ctx, made[b.Domain], root, b); err != nil {
			return fmt.Errorf("blob %s (domain %s): %w", b.Digest, b.Domain, err)
		}
		return nil
	})
}

func copyBlobOut(ctx context.Context, store blob.Store, root Tree, b Blob) error {
	src, err := store.OpenRange(ctx, blob.Digest(b.Digest), 0, -1)
	if err != nil {
		return err // blob.ErrNotFound means metadata references a missing object
	}
	defer src.Close()
	return copyOut(ctx, root, blobPath(b), func(w io.Writer) error {
		h := sha256.New()
		n, err := io.CopyBuffer(io.MultiWriter(w, h), ctxReader{ctx, src}, make([]byte, blob.TransferBufferSize))
		if err != nil {
			return err
		}
		if n != b.Size || hex.EncodeToString(h.Sum(nil)) != b.Digest {
			return blob.ErrIntegrity
		}
		return nil
	})
}
