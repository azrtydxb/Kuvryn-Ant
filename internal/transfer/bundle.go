package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	documentName    = "document.json"
	manifestName    = "manifest.json"
	maxDocumentSize = 1 << 30
)

// Stores resolves the blob store of a deduplication domain.
type Stores interface {
	Resolve(ctx context.Context, domain string) (blob.Store, error)
}

// Summary describes a written or verified bundle.
type Summary struct {
	ManifestSHA256 string `json:"manifest_sha256"`
	Feeds          int    `json:"feeds"`
	Sources        int    `json:"sources"`
	Components     int    `json:"components"`
	Assets         int    `json:"assets"`
	Blobs          int    `json:"blobs"`
	BlobBytes      int64  `json:"blob_bytes"`
	Notice         string `json:"notice,omitempty"`
}

// CountAssets returns the number of assets in the document.
func (d Document) CountAssets() (n int) {
	for _, c := range d.Components {
		for _, v := range c.Versions {
			n += len(v.Assets)
		}
	}
	return n
}

func summarize(d Document, m Manifest, sum string) Summary {
	s := Summary{ManifestSHA256: sum, Feeds: len(d.Feeds), Sources: len(d.Sources), Components: len(d.Components), Assets: d.CountAssets(), Blobs: len(m.Blobs), Notice: m.Notice}
	for _, b := range m.Blobs {
		s.BlobBytes += b.Size
	}
	return s
}

func prepareTarget(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTarget, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return nil, fmt.Errorf("%w: directory must be empty", ErrTarget)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrTarget
	}
	if err = root.Mkdir("blobs", 0o700); err != nil {
		root.Close()
		return nil, ErrTarget
	}
	return root, nil
}

func writeFile(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ExportBundle writes the selected definitions and content to a new or empty
// directory. Content objects are copied verbatim and verified while copying; the
// manifest is written last.
func ExportBundle(ctx context.Context, pool *pgxpool.Pool, stores Stores, instance, dir string, opt ExportOptions) (Summary, error) {
	doc, err := Build(ctx, pool, instance, opt)
	if err != nil {
		return Summary{}, err
	}
	root, err := prepareTarget(dir)
	if err != nil {
		return Summary{}, err
	}
	defer root.Close()
	domains := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT s.name,d.id::text FROM sources s JOIN dedup_domains d ON d.instance_id=s.instance_id AND d.name='source:'||s.id::text WHERE s.instance_id=$1`, instance)
	if err != nil {
		return Summary{}, err
	}
	for rows.Next() {
		var n, d string
		if err = rows.Scan(&n, &d); err != nil {
			rows.Close()
			return Summary{}, err
		}
		domains[n] = d
	}
	rows.Close()
	if rows.Err() != nil {
		return Summary{}, rows.Err()
	}
	seen := map[string]bool{}
	var entries []BlobEntry
	for _, c := range doc.Components {
		for _, v := range c.Versions {
			for _, a := range v.Assets {
				if seen[a.Digest] {
					continue
				}
				seen[a.Digest] = true
				domain, ok := domains[c.Source]
				if !ok {
					return Summary{}, fmt.Errorf("%w: source %q has no content domain", ErrIntegrity, c.Source)
				}
				store, err := stores.Resolve(ctx, domain)
				if err != nil {
					return Summary{}, err
				}
				if err = copyOut(ctx, store, root, a); err != nil {
					return Summary{}, fmt.Errorf("export blob %s: %w", a.Digest, err)
				}
				entries = append(entries, BlobEntry{Digest: a.Digest, Size: a.Size})
			}
		}
	}
	slices.SortFunc(entries, func(a, b BlobEntry) int { return bytes.Compare([]byte(a.Digest), []byte(b.Digest)) })
	data, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return Summary{}, err
	}
	if err = writeFile(root, documentName, data); err != nil {
		return Summary{}, err
	}
	dsum := sha256.Sum256(data)
	m := Manifest{FormatVersion: FormatVersion, CreatedAt: time.Now().UTC(), DocumentSHA256: hex.EncodeToString(dsum[:]), DocumentBytes: int64(len(data)), Blobs: entries, Notice: notice}
	mdata, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return Summary{}, err
	}
	if err = writeFile(root, manifestName, mdata); err != nil {
		return Summary{}, err
	}
	msum := sha256.Sum256(mdata)
	return summarize(doc, m, hex.EncodeToString(msum[:])), nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func copyOut(ctx context.Context, store blob.Store, root *os.Root, a AssetDef) (err error) {
	src, err := store.OpenRange(ctx, blob.Digest(a.Digest), 0, -1)
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := root.OpenFile("blobs/"+a.Digest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), ctxReader{ctx, src})
	if err != nil {
		return err
	}
	if n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.Digest {
		return ErrIntegrity
	}
	return f.Sync()
}

// Bundle is an opened, manifest-verified bundle directory.
type Bundle struct {
	Document       Document
	Manifest       Manifest
	ManifestSHA256 string
	root           *os.Root
}

func (b *Bundle) Close() error { return b.root.Close() }

// OpenBundle reads the manifest and document and verifies the document digest.
// expectManifestSHA256, when set, pins the exact manifest bytes (record it out of
// band at export time to detect replacement of the whole bundle). Blob contents
// are verified by VerifyBlobs or while being imported.
func OpenBundle(dir, expectManifestSHA256 string) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open bundle", ErrInput)
	}
	read := func(name string, limit int64) ([]byte, error) {
		f, err := root.Open(name)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot open %s", ErrIntegrity, name)
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, limit+1))
		if err != nil || int64(len(data)) > limit {
			return nil, fmt.Errorf("%w: cannot read %s", ErrIntegrity, name)
		}
		return data, nil
	}
	mdata, err := read(manifestName, 512<<20)
	if err != nil {
		root.Close()
		return nil, err
	}
	msum := sha256.Sum256(mdata)
	mhex := hex.EncodeToString(msum[:])
	if expectManifestSHA256 != "" && expectManifestSHA256 != mhex {
		root.Close()
		return nil, fmt.Errorf("%w: manifest digest does not match the expected value", ErrIntegrity)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(mdata))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil || m.FormatVersion != FormatVersion || !digestPattern.MatchString(m.DocumentSHA256) {
		root.Close()
		return nil, fmt.Errorf("%w: invalid manifest", ErrIntegrity)
	}
	seen := map[string]bool{}
	for _, e := range m.Blobs {
		if !digestPattern.MatchString(e.Digest) || e.Size < 0 || seen[e.Digest] {
			root.Close()
			return nil, fmt.Errorf("%w: invalid manifest blob list", ErrIntegrity)
		}
		seen[e.Digest] = true
	}
	ddata, err := read(documentName, maxDocumentSize)
	if err != nil {
		root.Close()
		return nil, err
	}
	dsum := sha256.Sum256(ddata)
	if hex.EncodeToString(dsum[:]) != m.DocumentSHA256 || int64(len(ddata)) != m.DocumentBytes {
		root.Close()
		return nil, fmt.Errorf("%w: document digest mismatch", ErrIntegrity)
	}
	var doc Document
	if err = json.Unmarshal(ddata, &doc); err != nil || doc.Validate() != nil {
		root.Close()
		return nil, fmt.Errorf("%w: invalid document", ErrInput)
	}
	return &Bundle{Document: doc, Manifest: m, ManifestSHA256: mhex, root: root}, nil
}

// Summary describes the bundle contents.
func (b *Bundle) Summary() Summary { return summarize(b.Document, b.Manifest, b.ManifestSHA256) }

func (b *Bundle) has(digest string) (int64, bool) {
	for _, e := range b.Manifest.Blobs {
		if e.Digest == digest {
			return e.Size, true
		}
	}
	return 0, false
}

// verifyBlob streams a bundle blob through SHA-256 and checks its listed size.
func (b *Bundle) verifyBlob(ctx context.Context, e BlobEntry) error {
	f, err := b.root.Open("blobs/" + e.Digest)
	if err != nil {
		return errors.New("missing from bundle")
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, f})
	if err != nil {
		return err
	}
	if n != e.Size {
		return errors.New("size differs from manifest")
	}
	if hex.EncodeToString(h.Sum(nil)) != e.Digest {
		return errors.New("content digest mismatch")
	}
	return nil
}

// Failure is one integrity problem found while verifying a bundle.
type Failure struct {
	Digest string `json:"digest"`
	Reason string `json:"reason"`
}

// VerifyBlobs checks every listed blob and returns the failures.
func (b *Bundle) VerifyBlobs(ctx context.Context) ([]Failure, error) {
	var out []Failure
	for _, e := range b.Manifest.Blobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := b.verifyBlob(ctx, e); err != nil {
			out = append(out, Failure{Digest: e.Digest, Reason: err.Error()})
		}
	}
	return out, nil
}

// Verify opens a bundle and checks every blob, failing on the first problem.
func Verify(ctx context.Context, dir, expectManifestSHA256 string) (Summary, error) {
	b, err := OpenBundle(dir, expectManifestSHA256)
	if err != nil {
		return Summary{}, err
	}
	defer b.Close()
	fails, err := b.VerifyBlobs(ctx)
	if err != nil {
		return Summary{}, err
	}
	if len(fails) > 0 {
		return Summary{}, fmt.Errorf("%w: blob %s: %s", ErrIntegrity, fails[0].Digest, fails[0].Reason)
	}
	return b.Summary(), nil
}

func (b *Bundle) openBlob(digest string) (io.ReadCloser, error) {
	return b.root.Open("blobs/" + digest)
}
