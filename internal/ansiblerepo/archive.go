package ansiblerepo

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"golang.org/x/mod/semver"
	"io"
	"path"
	"regexp"
	"strings"
)

const maxArchive = 64 << 20
const maxMetadata = 2 << 20

var errInvalid = errors.New("invalid collection")
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var dependencyPattern = regexp.MustCompile(`^(\*|(?:>=|<=|!=|==|>|<|=)?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)(?:\s*,\s*(?:>=|<=|!=|==|>|<|=)?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)*$`)

func validVersion(v string) bool {
	return len(v) <= 128 && semver.IsValid("v"+v) && semver.Canonical("v"+v) != "" && strings.Count(strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0], ".") == 2
}

type collection struct {
	Namespace    string            `json:"namespace"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
}
type fileEntry struct {
	Name      string `json:"name"`
	Type      string `json:"ftype"`
	Sum       string `json:"chksum_sha256"`
	Algorithm string `json:"chksum_type"`
}
type manifest struct {
	Collection collection `json:"collection_info"`
	Files      fileEntry  `json:"file_manifest_file"`
	Format     int        `json:"format"`
}
type inventory struct {
	Files  []fileEntry `json:"files"`
	Format int         `json:"format"`
}
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}
func info(b []byte) blob.Info {
	sum := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(sum[:]))}
}
func safePath(n string) bool {
	return n != "" && len(n) <= 1024 && !strings.ContainsAny(n, "\\\x00\r\n") && !strings.HasPrefix(n, "/") && path.Clean(n) == n && n != ".." && !strings.HasPrefix(n, "../")
}
func inspect(ctx context.Context, in io.Reader) (collection, error) {
	bad := func() (collection, error) { return collection{}, errInvalid }
	gz, e := gzip.NewReader(contextReader{ctx, in})
	if e != nil {
		return bad()
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: contextReader{ctx, gz}, N: (256 << 20) + 1}
	tr := tar.NewReader(limited)
	entries := map[string]fileEntry{}
	var mb, fb []byte
	for count := 0; ; count++ {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil || count >= 10000 {
			return bad()
		}
		n := strings.TrimSuffix(h.Name, "/")
		if !safePath(n) || h.Size < 0 || h.Size > 256<<20 {
			return bad()
		}
		if _, ok := entries[n]; ok {
			return bad()
		}
		typ := "file"
		if h.Typeflag == tar.TypeDir {
			typ = "dir"
			if h.Size != 0 {
				return bad()
			}
		} else if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return bad()
		}
		hash := sha256.New()
		if n == "MANIFEST.json" || n == "FILES.json" {
			if typ != "file" || h.Size > maxMetadata {
				return bad()
			}
			b, e := io.ReadAll(io.TeeReader(tr, hash))
			if e != nil {
				return bad()
			}
			if n == "MANIFEST.json" {
				mb = b
			} else {
				fb = b
			}
		} else {
			if _, e = io.CopyBuffer(hash, tr, make([]byte, 64<<10)); e != nil {
				return bad()
			}
		}
		entries[n] = fileEntry{Name: n, Type: typ, Sum: hex.EncodeToString(hash.Sum(nil)), Algorithm: "sha256"}
	}
	// Drain to check the gzip trailer and bound trailing decompressed bytes too.
	if _, e = io.Copy(io.Discard, limited); e != nil || limited.N <= 0 {
		return bad()
	}
	var m manifest
	var files inventory
	if json.Unmarshal(mb, &m) != nil || json.Unmarshal(fb, &files) != nil || m.Format != 1 || files.Format != 1 {
		return bad()
	}
	c := m.Collection
	if !namePattern.MatchString(c.Namespace) || !namePattern.MatchString(c.Name) || !validVersion(c.Version) || len(c.Dependencies) > 256 {
		return bad()
	}
	for k, v := range c.Dependencies {
		a := strings.Split(k, ".")
		if len(a) != 2 || !namePattern.MatchString(a[0]) || !namePattern.MatchString(a[1]) || len(v) > 512 || !dependencyPattern.MatchString(v) || k == c.Namespace+"."+c.Name {
			return bad()
		}
	}
	if c.Dependencies == nil {
		c.Dependencies = map[string]string{}
	}
	if m.Files.Name != "FILES.json" || m.Files.Type != "file" || m.Files.Algorithm != "sha256" || m.Files.Sum != string(info(fb).Digest) {
		return bad()
	}
	delete(entries, "MANIFEST.json")
	delete(entries, "FILES.json")
	seen := map[string]bool{}
	for _, f := range files.Files {
		if seen[f.Name] || !safePath(f.Name) {
			return bad()
		}
		seen[f.Name] = true
		if f.Name == "." && f.Type == "dir" {
			delete(entries, ".")
			continue
		}
		actual, ok := entries[f.Name]
		if !ok || actual.Type != f.Type {
			return bad()
		}
		if f.Type == "file" && (f.Algorithm != "sha256" || f.Sum != actual.Sum) {
			return bad()
		}
		delete(entries, f.Name)
	}
	if len(entries) != 0 {
		return bad()
	}
	return c, nil
}
