package cocoapods

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
)

const (
	maxArchive  = 256 << 20
	maxExpanded = 1 << 30
	maxMembers  = 50000
	maxPodspec  = 1 << 20
)

var errInvalid = errors.New("invalid CocoaPods input")
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9_.+-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)

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

func safeName(s string) bool {
	s = strings.TrimSuffix(s, "/")
	if s == "" || len(s) > 1024 || strings.ContainsAny(s, "\\\x00") || strings.HasPrefix(s, "/") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func safeLink(name, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\\\x00") {
		return false
	}
	return !strings.HasPrefix(path.Join(path.Dir(name), target), "..")
}

// archiveKind sniffs the container: asset suffix and CocoaPods :type value.
func archiveKind(head []byte) (ext, typ string, ok bool) {
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "tar.gz", "tgz", true
	case len(head) >= 4 && head[0] == 'P' && head[1] == 'K':
		return "zip", "zip", true
	}
	return "", "", false
}

// validateArchive checks structure and bounds without extracting anything.
func validateArchive(ctx context.Context, f io.ReaderAt, size int64, typ string) error {
	if typ == "zip" {
		zr, e := zip.NewReader(f, size)
		if e != nil || len(zr.File) == 0 || len(zr.File) > maxMembers {
			return errInvalid
		}
		var total uint64
		for _, m := range zr.File {
			if !safeName(m.Name) {
				return errInvalid
			}
			total += m.UncompressedSize64
			if total > maxExpanded {
				return errInvalid
			}
		}
		return ctx.Err()
	}
	gz, e := gzip.NewReader(io.NewSectionReader(f, 0, size))
	if e != nil {
		return errInvalid
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: contextReader{ctx, gz}, N: maxExpanded + 1}
	tr := tar.NewReader(limited)
	n := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errInvalid
		}
		n++
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if n > maxMembers || !safeName(h.Name) {
			return errInvalid
		}
		switch h.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			if !safeLink(h.Name, h.Linkname) {
				return errInvalid
			}
		case tar.TypeReg, tar.TypeDir:
		default:
			return errInvalid
		}
	}
	if n == 0 {
		return errInvalid
	}
	if _, e = io.Copy(io.Discard, limited); e != nil || limited.N <= 0 {
		return errInvalid
	}
	return ctx.Err()
}

// parsePodspec validates the podspec JSON for the requested pod and version.
func parsePodspec(b []byte, name, version string) (map[string]any, error) {
	if len(b) == 0 || len(b) > maxPodspec {
		return nil, errInvalid
	}
	var m map[string]any
	if e := json.Unmarshal(b, &m); e != nil || m == nil {
		return nil, errInvalid
	}
	if s, _ := m["name"].(string); s != name {
		return nil, errInvalid
	}
	if s, _ := m["version"].(string); s != version {
		return nil, errInvalid
	}
	if s, _ := m["summary"].(string); s == "" {
		return nil, errInvalid
	}
	return m, nil
}
