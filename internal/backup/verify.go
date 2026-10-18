package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// Summary is the outcome of a verification or restore.
type Summary struct {
	ManifestSHA256 string `json:"manifest_sha256"`
	SchemaVersion  int    `json:"schema_version"`
	Tables         int    `json:"tables"`
	Rows           int64  `json:"rows"`
	Blobs          int    `json:"blobs"`
	BlobBytes      int64  `json:"blob_bytes"`
}

func summarize(m Manifest, sum string) Summary {
	s := Summary{ManifestSHA256: sum, SchemaVersion: m.SchemaVersion, Tables: len(m.Tables), Blobs: len(m.Blobs), BlobBytes: m.BlobBytes()}
	for _, t := range m.Tables {
		s.Rows += t.Rows
	}
	return s
}

// Verify checks the manifest, every table file and every blob digest and size,
// and rejects files that the manifest does not list. It needs no database,
// blob store or keyring. expectManifestSHA256, when non-empty, must equal the
// digest of manifest.json recorded when the backup was created.
func Verify(ctx context.Context, dir, expectManifestSHA256 string, workers int) (Summary, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Summary{}, fmt.Errorf("%w: cannot open backup directory", ErrTarget)
	}
	defer root.Close()
	m, sum, err := readManifest(root)
	if err != nil {
		return Summary{}, err
	}
	if err = checkExpected(sum, expectManifestSHA256); err != nil {
		return Summary{}, err
	}
	if err = verifyTables(ctx, root, m); err != nil {
		return Summary{}, err
	}
	if workers == 0 {
		workers = 4
	}
	err = forEach(ctx, len(m.Blobs), workers, func(ctx context.Context, i int) error {
		b := m.Blobs[i]
		got, n, err := hashFile(ctx, root, blobPath(b))
		if err != nil || n != b.Size || got != b.Digest {
			return fmt.Errorf("%w: blob %s (domain %s)", ErrIntegrity, b.Digest, b.Domain)
		}
		return nil
	})
	if err != nil {
		return Summary{}, err
	}
	if err = rejectUnlisted(root, m); err != nil {
		return Summary{}, err
	}
	return summarize(m, sum), nil
}

func checkExpected(actual, expected string) error {
	if expected != "" && !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%w: manifest digest does not match the expected value", ErrIntegrity)
	}
	return nil
}

func verifyTables(ctx context.Context, root *os.Root, m Manifest) error {
	for _, t := range m.Tables {
		got, n, err := hashFile(ctx, root, tablePath(t.Name))
		if err != nil || got != t.SHA256 || n != t.Bytes {
			return fmt.Errorf("%w: table %s", ErrIntegrity, t.Name)
		}
	}
	return nil
}

// hashFile hashes a regular file; symlinks and special files are rejected.
func hashFile(ctx context.Context, root *os.Root, name string) (string, int64, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, ErrIntegrity
	}
	f, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, ctxReader{ctx, f}, make([]byte, blob.TransferBufferSize))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func rejectUnlisted(root *os.Root, m Manifest) error {
	allowed := map[string]bool{manifestName: true}
	for _, t := range m.Tables {
		allowed[tablePath(t.Name)] = true
	}
	for _, b := range m.Blobs {
		allowed[blobPath(b)] = true
	}
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !allowed[p] || !d.Type().IsRegular() {
			return fmt.Errorf("%w: unexpected file %q", ErrIntegrity, p)
		}
		return nil
	})
}
