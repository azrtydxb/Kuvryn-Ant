// Package backup creates and restores consistent snapshots of Kuvryn Ant
// metadata (PostgreSQL) and the blobs those metadata reference.
//
// A backup is a directory:
//
//	manifest.json                  written last; its presence marks a complete backup
//	tables/<table>.copy            PostgreSQL COPY text format, one snapshot
//	blobs/<dedup-domain>/<digest>  verbatim blob bytes
//
// Root keys and plaintext secrets are never written. encrypted_secrets rows are
// copied exactly as stored (envelope ciphertext); restore needs the same keyring.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const (
	FormatVersion   = 1
	manifestName    = "manifest.json"
	maxManifestSize = 512 << 20
)

var (
	ErrManifest  = errors.New("backup manifest is invalid")
	ErrIntegrity = errors.New("backup integrity verification failed")
	ErrTarget    = errors.New("backup or restore target is not acceptable")
	ErrSchema    = errors.New("backup schema is incompatible with this binary or target")
	ErrKeyring   = errors.New("configured keyring cannot open restored secrets; mount the original keyring")

	identPattern  = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Migration records the schema history entry the backup was taken at.
type Migration struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Checksum string `json:"checksum"`
}

// Table describes one COPY file. Columns exclude generated columns.
type Table struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	Bytes   int64    `json:"bytes"`
	SHA256  string   `json:"sha256"`
}

// Blob is one content-addressed object in one deduplication domain.
type Blob struct {
	Domain string `json:"domain"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	// BestEffort marks a blob no live artifact needs: it is referenced only by
	// purpose=cache sources (or by nothing). Its bytes may be missing at backup
	// time without failing the backup.
	BestEffort bool `json:"best_effort,omitempty"`
	// Missing records that a best-effort blob had no bytes when the backup ran.
	// It is not copied, and restore drops the cache versions that used it.
	Missing bool `json:"missing,omitempty"`
}

type Manifest struct {
	FormatVersion int         `json:"format_version"`
	CreatedAt     time.Time   `json:"created_at"`
	ServerVersion int         `json:"postgres_server_version_num"`
	SchemaVersion int         `json:"schema_version"`
	Migrations    []Migration `json:"migrations"`
	Tables        []Table     `json:"tables"`
	Skipped       []string    `json:"skipped_ephemeral_tables"`
	Blobs         []Blob      `json:"blobs"`
	// BlobMode is "copy" (blob bytes are in the backup; the default and the only
	// mode older backups have) or "reference" (the backup holds metadata and the
	// blob list only; the blobs stay in the durable object store).
	BlobMode string `json:"blob_mode,omitempty"`
	Notice   string `json:"notice"`
}

const (
	BlobsCopy      = "copy"
	BlobsReference = "reference"
)

const referenceNotice = "Blob bytes are not part of this backup; it references objects in the durable blob store. Restore needs those objects to still exist (same bucket and prefix): keep blob garbage collection from sweeping them, or use object-store versioning."

// referenced reports whether blob bytes live outside the backup.
func (m Manifest) referenced() bool { return m.BlobMode == BlobsReference }

// Mode is the effective blob mode (copy for backups that predate the field).
func (m Manifest) Mode() string {
	if m.referenced() {
		return BlobsReference
	}
	return BlobsCopy
}

const cacheNotice = "Some cache-only blobs were missing at backup time (blobs with missing=true). Cache content is disposable: restore drops the cache versions that used them, so those objects are cache misses until clients rebuild them."

const notice = "Root keys are not part of this backup. Restore requires the same external keyring that protected the source deployment."

func tablePath(name string) string { return "tables/" + name + ".copy" }
func blobPath(b Blob) string       { return "blobs/" + b.Domain + "/" + b.Digest }

func (m Manifest) BlobBytes() (n int64) {
	for _, b := range m.Blobs {
		if b.Missing {
			continue
		}
		n += b.Size
	}
	return n
}

func (m Manifest) validate() error {
	if m.FormatVersion != FormatVersion || m.SchemaVersion < 1 || len(m.Migrations) != m.SchemaVersion {
		return ErrManifest
	}
	for i, x := range m.Migrations {
		if x.Version != i+1 || x.Name == "" || !digestPattern.MatchString(x.Checksum) {
			return ErrManifest
		}
	}
	if m.BlobMode != "" && m.BlobMode != BlobsCopy && m.BlobMode != BlobsReference {
		return ErrManifest
	}
	seen := map[string]bool{}
	for _, t := range m.Tables {
		if !identPattern.MatchString(t.Name) || seen[t.Name] || len(t.Columns) == 0 || t.Rows < 0 || t.Bytes < 0 || !digestPattern.MatchString(t.SHA256) {
			return ErrManifest
		}
		seen[t.Name] = true
		cols := map[string]bool{}
		for _, c := range t.Columns {
			if !identPattern.MatchString(c) || cols[c] {
				return ErrManifest
			}
			cols[c] = true
		}
	}
	for _, s := range m.Skipped {
		if !identPattern.MatchString(s) || seen[s] {
			return ErrManifest
		}
	}
	pairs := map[[2]string]bool{}
	for _, b := range m.Blobs {
		key := [2]string{b.Domain, b.Digest}
		if !uuidPattern.MatchString(b.Domain) || !digestPattern.MatchString(b.Digest) || b.Size < 0 || (b.Missing && !b.BestEffort) || pairs[key] {
			return ErrManifest
		}
		pairs[key] = true
	}
	return nil
}

// readManifest returns the parsed manifest and the SHA-256 of its exact bytes.
// Operators can record that value out of band and pass it back as the expected
// manifest digest to detect replacement of the whole backup.
func readManifest(ctx context.Context, t Tree) (Manifest, string, error) {
	f, size, err := t.Open(ctx, manifestName)
	if err != nil || size > maxManifestSize {
		return Manifest{}, "", fmt.Errorf("%w: cannot open manifest", ErrManifest)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifestSize+1))
	if err != nil || len(data) > maxManifestSize {
		return Manifest{}, "", ErrManifest
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || m.validate() != nil {
		return Manifest{}, "", ErrManifest
	}
	sum := sha256.Sum256(data)
	return m, hex.EncodeToString(sum[:]), nil
}

// missingBlobs returns the blobs recorded as missing at backup time.
func (m Manifest) missingBlobs() (out []Blob) {
	for _, b := range m.Blobs {
		if b.Missing {
			out = append(out, b)
		}
	}
	return out
}

// MissingCount is the number of best-effort blobs recorded as missing.
func (m Manifest) MissingCount() int { return len(m.missingBlobs()) }
