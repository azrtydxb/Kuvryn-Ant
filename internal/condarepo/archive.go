package condarepo

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
)

const maxPackage = 128 << 20
const maxMetadata = 256 << 10
const maxExpanded = 512 << 20

var errInvalid = errors.New("invalid Conda package")
var namePattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.-]{0,127}$`)
var partPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.+!]{0,63}$`)

func validSubdir(s string) bool {
	switch s {
	case "noarch", "linux-64", "linux-aarch64", "linux-ppc64le", "linux-s390x", "osx-64", "osx-arm64", "win-64", "win-32":
		return true
	}
	return false
}
func validFilename(s string) bool {
	if len(s) > 240 || !strings.HasSuffix(s, ".tar.bz2") && !strings.HasSuffix(s, ".conda") {
		return false
	}
	p := strings.Split(strings.TrimSuffix(strings.TrimSuffix(s, ".tar.bz2"), ".conda"), "-")
	return len(p) >= 3 && namePattern.MatchString(strings.Join(p[:len(p)-2], "-")) && partPattern.MatchString(p[len(p)-2]) && partPattern.MatchString(p[len(p)-1])
}

type metadata struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Build       string   `json:"build"`
	BuildNumber int64    `json:"build_number"`
	Subdir      string   `json:"subdir"`
	Depends     []string `json:"depends"`
	Constrains  []string `json:"constrains,omitempty"`
	Noarch      string   `json:"noarch,omitempty"`
	License     string   `json:"license,omitempty"`
	Timestamp   int64    `json:"timestamp,omitempty"`
}

func parseMetadata(b []byte) (metadata, error) {
	var m metadata
	if len(b) > maxMetadata || !uniqueJSON(b) || json.Unmarshal(b, &m) != nil || !namePattern.MatchString(m.Name) || !partPattern.MatchString(m.Version) || !partPattern.MatchString(m.Build) || !validSubdir(m.Subdir) || m.BuildNumber < 0 || m.Timestamp < 0 || len(m.License) > 1024 {
		return m, errInvalid
	}
	if m.Noarch != "" && m.Noarch != "generic" && m.Noarch != "python" || m.Noarch != "" && m.Subdir != "noarch" {
		return m, errInvalid
	}
	if m.Depends == nil {
		m.Depends = []string{}
	}
	for _, deps := range [][]string{m.Depends, m.Constrains} {
		if len(deps) > 1024 {
			return m, errInvalid
		}
		for _, d := range deps {
			fields := strings.Fields(d)
			if len(d) > 1024 || len(fields) == 0 || len(fields) > 3 || !namePattern.MatchString(fields[0]) || strings.ContainsAny(d, "\x00\r\n:/\\@") {
				return m, errInvalid
			}
		}
	}
	return m, nil
}
func safePath(s string) bool {
	return s != "" && len(s) <= 1024 && strings.Count(s, "/") <= 32 && s != "." && s != ".." && !strings.HasPrefix(s, "../") && !strings.HasPrefix(s, "/") && path.Clean(s) == s && !strings.ContainsAny(s, "\\:\x00\r\n")
}

// inspectPackage never writes archive members or runs scripts. A compressed-input
// context wrapper plus expansion checks bound work even while tar skips members.
func inspectPackage(ctx context.Context, r io.Reader, size int64) (metadata, error) {
	var zero metadata
	limit := int64(maxExpanded)
	if ratio := size*200 + (1 << 20); ratio < limit {
		limit = ratio
	}
	expanded := &io.LimitedReader{R: contextReader{ctx, bzip2.NewReader(contextReader{ctx, r})}, N: limit + 1}
	tr := tar.NewReader(expanded)
	seen := map[string]bool{}
	files := map[string]bool{}
	var manifest []byte
	var metadataBytes int64
	for count := 0; ; count++ {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil || count >= 10000 || expanded.N <= 0 {
			return zero, errInvalid
		}
		n := strings.TrimSuffix(h.Name, "/")
		if !safePath(n) || seen[n] || h.Size < 0 || h.Size > limit || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return zero, errInvalid
		}
		seen[n] = true
		if h.Typeflag == tar.TypeReg {
			files[n] = true
		}
		if strings.HasPrefix(n, "info/") {
			metadataBytes += h.Size
			if metadataBytes > 8<<20 {
				return zero, errInvalid
			}
		}
		if n == "info/index.json" {
			if h.Size > maxMetadata || h.Typeflag != tar.TypeReg {
				return zero, errInvalid
			}
			manifest, e = io.ReadAll(tr)
			if e != nil {
				return zero, errInvalid
			}
		} else {
			if _, e = io.CopyBuffer(io.Discard, contextReader{ctx, tr}, make([]byte, 64<<10)); e != nil {
				return zero, errInvalid
			}
		}
	}
	// Drain the bzip2 CRC/trailer and reject nonzero data after the tar terminator.
	buf := make([]byte, 64<<10)
	for {
		n, e := expanded.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return zero, errInvalid
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil || expanded.N <= 0 {
			return zero, errInvalid
		}
	}
	if expanded.N <= 0 {
		return zero, errInvalid
	}
	for n := range seen {
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			if files[p] {
				return zero, errInvalid
			}
		}
	}
	return parseMetadata(manifest)
}
