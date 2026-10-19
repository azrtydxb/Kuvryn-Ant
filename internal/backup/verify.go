package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
	// BlobMode is "copy" or "reference" (blob bytes are not in the backup).
	BlobMode string `json:"blob_mode"`
}

func summarize(m Manifest, sum string) Summary {
	s := Summary{ManifestSHA256: sum, SchemaVersion: m.SchemaVersion, Tables: len(m.Tables), Blobs: len(m.Blobs), BlobBytes: m.BlobBytes(), BlobMode: m.Mode()}
	for _, t := range m.Tables {
		s.Rows += t.Rows
	}
	return s
}

// VerifyOptions tunes VerifyTree.
type VerifyOptions struct {
	// Expect pins the manifest digest recorded when the backup was created.
	Expect  string
	Workers int
	// Stores, when set, makes verification of a reference-mode backup also
	// confirm that every referenced blob exists with the recorded size.
	Stores Stores
}

// Verify checks a backup directory; see VerifyTree.
func Verify(ctx context.Context, dir, expectManifestSHA256 string, workers int) (Summary, error) {
	t, err := OpenDirTree(dir, false)
	if err != nil {
		return Summary{}, err
	}
	defer t.Close()
	return VerifyTree(ctx, t, VerifyOptions{Expect: expectManifestSHA256, Workers: workers})
}

// VerifyTree checks the manifest, every table file and every blob digest and
// size, and rejects files that the manifest does not list. It needs no database,
// blob store or keyring (except Stores for reference-mode blob checks).
// Expect, when non-empty, must equal the digest of manifest.json recorded when
// the backup was created.
func VerifyTree(ctx context.Context, root Tree, opts VerifyOptions) (Summary, error) {
	m, sum, err := readManifest(ctx, root)
	if err != nil {
		return Summary{}, err
	}
	if err = checkExpected(sum, opts.Expect); err != nil {
		return Summary{}, err
	}
	if err = verifyTables(ctx, root, m); err != nil {
		return Summary{}, err
	}
	workers := opts.Workers
	if workers == 0 {
		workers = 4
	}
	if m.referenced() {
		if opts.Stores != nil {
			if err = checkBlobs(ctx, opts.Stores, m.Blobs, workers); err != nil {
				return Summary{}, err
			}
		}
	} else {
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
	}
	if err = rejectUnlisted(ctx, root, m); err != nil {
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

func verifyTables(ctx context.Context, root Tree, m Manifest) error {
	for _, t := range m.Tables {
		got, n, err := hashFile(ctx, root, tablePath(t.Name))
		if err != nil || got != t.SHA256 || n != t.Bytes {
			return fmt.Errorf("%w: table %s", ErrIntegrity, t.Name)
		}
	}
	return nil
}

// hashFile hashes a regular file; symlinks and special files are rejected.
func hashFile(ctx context.Context, root Tree, name string) (string, int64, error) {
	f, _, err := root.Open(ctx, name)
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

func rejectUnlisted(ctx context.Context, root Tree, m Manifest) error {
	allowed := map[string]bool{manifestName: true}
	for _, t := range m.Tables {
		allowed[tablePath(t.Name)] = true
	}
	if !m.referenced() {
		for _, b := range m.Blobs {
			allowed[blobPath(b)] = true
		}
	}
	names, err := root.List(ctx)
	if err != nil {
		return err
	}
	for _, p := range names {
		if !allowed[p] {
			return fmt.Errorf("%w: unexpected file %q", ErrIntegrity, p)
		}
	}
	return nil
}
