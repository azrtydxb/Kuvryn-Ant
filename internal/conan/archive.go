package conan

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" // Conan's native manifest identity; shared storage uses SHA-256.
	"crypto/sha256"
	"encoding/hex"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

const maxFile = 16 << 20
const maxMetadata = 1 << 20
const maxExpanded = 128 << 20
const maxMembers = 4096

func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(h[:])), Size: int64(len(b))}
}
func md5hex(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }
func safePath(s string) bool {
	return s != "" && len(s) <= 1024 && path.Clean(s) == s && s != "." && s != ".." && !strings.HasPrefix(s, "../") && !strings.HasPrefix(s, "/") && !strings.ContainsAny(s, "\\:\x00\r\n")
}
func manifest(b []byte, rev string) (map[string]string, error) {
	if len(b) > maxMetadata || bytes.Contains(b, []byte{'\r'}) {
		return nil, errInvalid
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 2 || len(lines) > maxMembers+2 {
		return nil, errInvalid
	}
	if n, e := strconv.ParseInt(lines[0], 10, 64); e != nil || n < 0 {
		return nil, errInvalid
	}
	sums := map[string]string{}
	ordered := []string{}
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		i := strings.LastIndex(l, ": ")
		if i < 0 {
			return nil, errInvalid
		}
		name, h := l[:i], l[i+2:]
		if !safePath(name) || !revision.MatchString(h) || sums[name] != "" {
			return nil, errInvalid
		}
		sums[name] = h
		ordered = append(ordered, l)
	}
	sort.Strings(ordered)
	if len(sums) == 0 || md5hex([]byte(strings.Join(ordered, "\n")+"\n")) != rev {
		return nil, errInvalid
	}
	return sums, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
func validate(ctx context.Context, v reference, files map[string][]byte) error {
	rev := v.rrev
	required := "conanfile.py"
	if v.pid != "" {
		rev = v.prev
		required = "conaninfo.txt"
		if _, ok := files["conan_package.tgz"]; !ok {
			return errInvalid
		}
	}
	sums, e := manifest(files["conanmanifest.txt"], rev)
	if e != nil {
		return e
	}
	if _, ok := files[required]; !ok {
		return errInvalid
	}
	actual := map[string]string{}
	expanded := int64(0)
	members := 0
	for key, b := range files {
		if key == "conanmanifest.txt" {
			continue
		}
		if !strings.HasSuffix(key, ".tgz") {
			if len(b) > maxMetadata {
				return errInvalid
			}
			actual[key] = md5hex(b)
			continue
		}
		gz, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			return errInvalid
		}
		// Count all decompressed tar bytes (including padding and extensions), not just files.
		limited := &io.LimitedReader{R: contextReader{ctx, gz}, N: maxExpanded - expanded + 1}
		tr := tar.NewReader(limited)
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				gz.Close()
				return errInvalid
			}
			members++
			if members > maxMembers || !safePath(strings.TrimSuffix(h.Name, "/")) {
				gz.Close()
				return errInvalid
			}
			if h.Typeflag == tar.TypeDir {
				continue
			}
			if h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxExpanded {
				gz.Close()
				return errInvalid
			}
			name := h.Name
			if key == "conan_sources.tgz" {
				name = "export_source/" + name
			}
			if _, ok := actual[name]; ok {
				gz.Close()
				return errInvalid
			}
			hash := md5.New()
			if _, e = io.CopyBuffer(hash, contextReader{ctx, tr}, make([]byte, 64<<10)); e != nil {
				gz.Close()
				return errInvalid
			}
			actual[name] = hex.EncodeToString(hash.Sum(nil))
		}
		// Drain for gzip checksum validation, with the same expansion and CPU budget.
		_, e = io.CopyBuffer(io.Discard, limited, make([]byte, 64<<10))
		used := maxExpanded - expanded + 1 - limited.N
		expanded += used
		gz.Close()
		if e != nil || expanded > maxExpanded {
			return errInvalid
		}
	}
	if len(actual) != len(sums) {
		return errInvalid
	}
	for name, h := range sums {
		if actual[name] != h {
			return errInvalid
		}
	}
	return ctx.Err()
}
