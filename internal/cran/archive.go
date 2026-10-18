package cran

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxArchive = 32 << 20
const maxMetadata = 256 << 10

var errInvalid = errors.New("invalid R source package")
var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]{1,63}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+(?:[.-][0-9]+)+$`)
var fieldPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9./-]*$`)

func validName(n string) bool    { return namePattern.MatchString(n) && !strings.HasSuffix(n, ".") }
func validVersion(v string) bool { return len(v) <= 64 && versionPattern.MatchString(v) }

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
	h := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}

// parseDCF accepts a single bounded DESCRIPTION record, preserving folded fields.
func parseDCF(b []byte) (map[string]string, error) {
	if len(b) > maxMetadata || !utf8.Valid(b) || strings.ContainsAny(string(b), "\x00\r") {
		return nil, errInvalid
	}
	m := map[string]string{}
	key := ""
	ended := false
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			ended = true
			continue
		}
		if ended {
			return nil, errInvalid
		}
		if line[0] == ' ' || line[0] == '\t' {
			if key == "" {
				return nil, errInvalid
			}
			m[key] += "\n " + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !fieldPattern.MatchString(k) || len(k) > 128 || m[k] != "" {
			return nil, errInvalid
		}
		if _, ok = m[k]; ok {
			return nil, errInvalid
		}
		key = k
		m[k] = strings.TrimSpace(v)
	}
	if !validName(m["Package"]) || !validVersion(m["Version"]) || m["Title"] == "" || m["Description"] == "" || m["License"] == "" || m["Built"] != "" || (m["Type"] != "" && m["Type"] != "Package") {
		return nil, errInvalid
	}
	if n := m["NeedsCompilation"]; n != "" && n != "yes" && n != "no" {
		return nil, errInvalid
	}
	return m, nil
}
func inspect(ctx context.Context, r io.Reader, name, version string) (map[string]string, error) {
	g, e := gzip.NewReader(contextReader{ctx, r})
	if e != nil {
		return nil, errInvalid
	}
	defer g.Close()
	limit := &io.LimitedReader{R: contextReader{ctx, g}, N: 128<<20 + 1}
	tr := tar.NewReader(limit)
	seen := map[string]bool{}
	var m map[string]string
	for count := 0; ; count++ {
		if count >= 10000 {
			return nil, errInvalid
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errInvalid
		}
		n := strings.TrimSuffix(h.Name, "/")
		if path.Clean(n) != n || (n != name && !strings.HasPrefix(n, name+"/")) || strings.ContainsAny(n, "\\:\x00") || seen[n] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return nil, errInvalid
		}
		seen[n] = true
		if n == name+"/DESCRIPTION" {
			if h.Size > maxMetadata || h.Typeflag != tar.TypeReg {
				return nil, errInvalid
			}
			b, e := io.ReadAll(tr)
			if e != nil {
				return nil, errInvalid
			}
			m, e = parseDCF(b)
			if e != nil {
				return nil, e
			}
		}
	}
	if _, e = io.CopyBuffer(io.Discard, limit, make([]byte, 64<<10)); e != nil || limit.N <= 0 || m == nil || m["Package"] != name || m["Version"] != version {
		return nil, errInvalid
	}
	return m, nil
}
