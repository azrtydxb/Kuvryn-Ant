package composer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

const maxMetadata = 1 << 20
const maxPackage = 128 << 20
const maxExpanded = 512 << 20

var errInvalid = errors.New("invalid Composer package")
var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:[_.-][a-z0-9]+)*/[a-z0-9]+(?:[_.-][a-z0-9]+)*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
var constraintPattern = regexp.MustCompile(`^(?:[~^]|>=?|<=?|!=|==?)?(?:[0-9]+|[xX*])(?:\.(?:[0-9]+|[xX*])){0,3}$`)
var platformPattern = regexp.MustCompile(`^(?:ext|lib)-[a-z0-9]+(?:[-_.][a-z0-9]+)*$`)

func validConstraint(c string) bool {
	if len(c) == 0 || len(c) > 256 {
		return false
	}
	for _, branch := range strings.Split(c, "||") {
		terms := strings.Fields(strings.ReplaceAll(branch, ",", " "))
		if len(terms) == 0 {
			return false
		}
		for _, term := range terms {
			if !constraintPattern.MatchString(term) {
				return false
			}
		}
	}
	return true
}

type sourceReference struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Reference string `json:"reference"`
}

type metadata struct {
	Source      *sourceReference                      `json:"source,omitempty"`
	ArchiveSHA1 string                                `json:"_ant-sha1,omitempty"`
	Name        string                                `json:"name"`
	Version     string                                `json:"version"`
	Type        string                                `json:"type,omitempty"`
	Description string                                `json:"description,omitempty"`
	Require     map[string]string                     `json:"require,omitempty"`
	RequireDev  map[string]string                     `json:"require-dev,omitempty"`
	Autoload    map[string]map[string]json.RawMessage `json:"autoload,omitempty"`
}

func parseManifest(data []byte) (metadata, error) {
	var m metadata
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if len(data) > maxMetadata || d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF || !namePattern.MatchString(m.Name) || len(m.Name) > 200 || !versionPattern.MatchString(m.Version) || (m.Type != "" && m.Type != "library") {
		return m, errInvalid
	}
	if m.Source != nil {
		u, e := url.Parse(m.Source.URL)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || len(m.Source.URL) > 2048 || m.Source.Type != "git" || len(m.Source.Reference) == 0 || len(m.Source.Reference) > 128 || strings.ContainsAny(m.Source.Reference, " \t\r\n") {
			return m, errInvalid
		}
	}
	// Reject duplicate object keys at every level rather than silently using last values.
	if !uniqueJSON(data) {
		return m, errInvalid
	}
	for _, deps := range []map[string]string{m.Require, m.RequireDev} {
		for name, c := range deps {
			if !(namePattern.MatchString(name) || name == "php" || platformPattern.MatchString(name)) || len(name) > 200 || !validConstraint(c) {
				return m, errInvalid
			}
		}
	}
	pathsCount := 0
	for kind, entries := range m.Autoload {
		if kind != "psr-4" && kind != "psr-0" {
			return m, errInvalid
		}
		for _, raw := range entries {
			var p string
			var paths []string
			if json.Unmarshal(raw, &p) == nil {
				paths = []string{p}
			} else if json.Unmarshal(raw, &paths) != nil {
				return m, errInvalid
			}
			pathsCount += len(paths)
			if pathsCount > 1024 {
				return m, errInvalid
			}
			for _, p := range paths {
				if p != "" && (p == ".." || p == "../" || strings.ContainsAny(p, "\\:\x00") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || path.Clean(strings.TrimSuffix(p, "/")) != strings.TrimSuffix(p, "/")) {
					return m, errInvalid
				}
			}
		}
	}
	return m, nil
}
func uniqueJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		t, e := d.Token()
		if e != nil {
			return false
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return true
		}
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return false
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return false
				}
				seen[s] = true
				if !value(depth + 1) {
					return false
				}
			}
		} else if delim == '[' {
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
		} else {
			return false
		}
		_, e = d.Token()
		return e == nil
	}
	return value(0)
}
func info(data []byte) blob.Info {
	h := sha256.Sum256(data)
	return blob.Info{Size: int64(len(data)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}
func inspectPackage(ctx context.Context, f *os.File, size int64) (metadata, []byte, error) {
	var m metadata
	// Bound central-directory allocation before archive/zip parses it. ZIP64
	// is unnecessary under this adapter's 128 MiB / 10,000-entry limits.
	tailSize := int64(65557)
	if size < tailSize {
		tailSize = size
	}
	if tailSize < 22 {
		return m, nil, errInvalid
	}
	tail := make([]byte, int(tailSize))
	if _, e := f.ReadAt(tail, size-tailSize); e != nil {
		return m, nil, errInvalid
	}
	end := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if bytes.Equal(tail[i:i+4], []byte{'P', 'K', 5, 6}) && i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) == len(tail) {
			end = i
			break
		}
	}
	if end < 0 {
		return m, nil, errInvalid
	}
	eocd := tail[end:]
	count := binary.LittleEndian.Uint16(eocd[10:])
	if count > 10000 || binary.LittleEndian.Uint16(eocd[4:]) != 0 || binary.LittleEndian.Uint16(eocd[6:]) != 0 || binary.LittleEndian.Uint16(eocd[8:]) != count || binary.LittleEndian.Uint32(eocd[12:]) > 8<<20 || binary.LittleEndian.Uint32(eocd[16:]) == 0xffffffff {
		return m, nil, errInvalid
	}
	z, e := zip.NewReader(f, size)
	if e != nil || len(z.File) > 10000 {
		return m, nil, errInvalid
	}
	var expanded uint64
	seen := map[string]bool{}
	original := map[string]bool{}
	files := map[string]bool{}
	var manifest []byte
	for _, entry := range z.File {
		if ctx.Err() != nil {
			return m, nil, ctx.Err()
		}
		name := entry.Name
		key := strings.ToLower(name)
		if len(name) > 1024 || strings.Count(name, "/") > 32 || name == "" || name == "." || name == ".." || name == "../" || strings.ContainsAny(name, "\\\x00:\r\n") || strings.HasPrefix(name, "/") || path.Clean(strings.TrimSuffix(name, "/")) != strings.TrimSuffix(name, "/") || strings.HasPrefix(name, "../") || seen[key] || entry.Mode()&os.ModeType != 0 && !entry.FileInfo().IsDir() {
			return m, nil, errInvalid
		}
		seen[key] = true
		original[name] = true
		if !entry.FileInfo().IsDir() {
			files[key] = true
		}
		if entry.UncompressedSize64 > maxExpanded-expanded {
			return m, nil, errInvalid
		}
		expanded += entry.UncompressedSize64
		if entry.UncompressedSize64 > 1<<20 && (entry.CompressedSize64 == 0 || entry.UncompressedSize64/entry.CompressedSize64 > 1000) {
			return m, nil, errInvalid
		}
		isManifest := name == "composer.json" || strings.Count(name, "/") == 1 && strings.HasSuffix(name, "/composer.json")
		if isManifest && (manifest != nil || entry.UncompressedSize64 > maxMetadata) {
			return m, nil, errInvalid
		}
		reader, e := entry.Open()
		if e != nil {
			return m, nil, errInvalid
		}
		var dst io.Writer = io.Discard
		var b bytes.Buffer
		if isManifest {
			dst = &b
		}
		n, e := io.CopyBuffer(dst, io.LimitReader(contextReader{ctx, reader}, int64(entry.UncompressedSize64)+1), make([]byte, 64<<10))
		reader.Close()
		if e != nil || uint64(n) != entry.UncompressedSize64 {
			return m, nil, errInvalid
		}
		if isManifest {
			manifest = b.Bytes()
		}
	}
	if manifest == nil {
		return m, nil, errInvalid
	}
	m, e = parseManifest(manifest)
	if e != nil || m.ArchiveSHA1 != "" {
		return m, nil, errInvalid
	}
	// A single optional wrapper directory must contain the entire archive.
	root := ""
	for name := range original {
		if strings.HasSuffix(name, "/composer.json") {
			root = strings.TrimSuffix(name, "composer.json")
		}
	}
	if root != "" {
		for name := range original {
			if !strings.HasPrefix(name, root) {
				return m, nil, errInvalid
			}
		}
	}
	for name := range seen {
		if ctx.Err() != nil {
			return m, nil, ctx.Err()
		}
		if strings.HasSuffix(name, "/") && files[strings.TrimSuffix(name, "/")] {
			return m, nil, errInvalid
		}
		for parent := path.Dir(strings.TrimSuffix(name, "/")); parent != "."; parent = path.Dir(parent) {
			if files[parent] {
				return m, nil, errInvalid
			}
		}
	}
	// PSR paths must be backed by this archive, never an external checkout.
	for _, entries := range m.Autoload {
		for _, raw := range entries {
			var one string
			var paths []string
			if json.Unmarshal(raw, &one) == nil {
				paths = []string{one}
			} else {
				_ = json.Unmarshal(raw, &paths)
			}
			for _, p := range paths {
				if ctx.Err() != nil {
					return m, nil, ctx.Err()
				}
				prefix := root + strings.TrimSuffix(p, "/")
				if prefix != "" && !strings.HasSuffix(prefix, "/") {
					prefix += "/"
				}
				found := false
				for name := range original {
					if strings.HasPrefix(name, prefix) && !strings.HasSuffix(name, "/") {
						found = true
						break
					}
				}
				if !found {
					return m, nil, errInvalid
				}
			}
		}
	}
	return m, manifest, nil
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
