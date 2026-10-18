package cargo

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

const maxMetadata = 1 << 20

// Expansion ratio limits decompression bombs without imposing a small artifact ceiling.
const maxExpansionRatio = 1000

var errInvalid = errors.New("invalid Cargo publication")

// One canonical spelling prevents case and hyphen/underscore aliases from
// acquiring separate ownership. Noncanonical spellings are rejected explicitly.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

func validVersion(v string) bool {
	if len(v) > 128 || !versionRE.MatchString(v) {
		return false
	}
	base, _, _ := strings.Cut(v, "+")
	_, pre, ok := strings.Cut(base, "-")
	if ok {
		for _, s := range strings.Split(pre, ".") {
			if len(s) > 1 && s[0] == '0' && strings.Trim(s, "0123456789") == "" {
				return false
			}
		}
	}
	return true
}
func versionKey(v string) string { v, _, _ = strings.Cut(v, "+"); return v }
func indexPath(name string) string {
	name = strings.ToLower(name)
	switch len(name) {
	case 0:
		return ""
	case 1:
		return "1/" + name
	case 2:
		return "2/" + name
	case 3:
		return "3/" + name[:1] + "/" + name
	default:
		return name[:2] + "/" + name[2:4] + "/" + name
	}
}

type dependency struct {
	Name            string   `json:"name"`
	Requirement     string   `json:"version_req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target"`
	Kind            *string  `json:"kind"`
	Registry        *string  `json:"registry"`
	Rename          *string  `json:"explicit_name_in_toml"`
}
type metadata struct {
	Name        string              `json:"name"`
	Version     string              `json:"vers"`
	Deps        []dependency        `json:"deps"`
	Features    map[string][]string `json:"features"`
	Links       *string             `json:"links"`
	RustVersion *string             `json:"rust_version"`
}

func readMetadata(r io.Reader) (metadata, error) {
	var m metadata
	var n uint32
	if binary.Read(r, binary.LittleEndian, &n) != nil || n == 0 || n > maxMetadata {
		return m, errInvalid
	}
	data := make([]byte, int(n))
	if _, e := io.ReadFull(r, data); e != nil {
		return m, errInvalid
	}
	if json.Unmarshal(data, &m) != nil || !nameRE.MatchString(m.Name) || !validVersion(m.Version) {
		return m, errInvalid
	}
	for _, d := range m.Deps {
		if d.Name == "" || d.Requirement == "" || (d.Kind != nil && *d.Kind != "normal" && *d.Kind != "dev" && *d.Kind != "build") {
			return m, errInvalid
		}
	}
	return m, nil
}
func (m metadata) index(info blob.Info) []byte {
	deps := make([]map[string]any, 0, len(m.Deps))
	for _, d := range m.Deps {
		name := d.Name
		var pkg *string
		if d.Rename != nil {
			name = *d.Rename
			pkg = &d.Name
		}
		features := d.Features
		if features == nil {
			features = []string{}
		}
		deps = append(deps, map[string]any{"name": name, "package": pkg, "req": d.Requirement, "features": features, "optional": d.Optional, "default_features": d.DefaultFeatures, "target": d.Target, "kind": d.Kind, "registry": d.Registry})
	}
	features := m.Features
	if features == nil {
		features = map[string][]string{}
	}
	b, _ := json.Marshal(map[string]any{"name": m.Name, "vers": m.Version, "deps": deps, "features": features, "cksum": string(info.Digest), "yanked": false, "links": m.Links, "rust_version": m.RustVersion, "v": 2})
	return append(b, '\n')
}
func bytesInfo(b []byte) blob.Info {
	sum := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(b))}
}

// The wire archive is spooled to a private temporary file, never held in RAM or
// extracted. Expanded bytes and entry count are bounded to reject decompression bombs.
func readCrate(r io.Reader, m metadata) (*os.File, blob.Info, error) {
	return readCrateContext(context.Background(), r, m)
}
func readCrateContext(ctx context.Context, r io.Reader, m metadata) (*os.File, blob.Info, error) {
	var n uint32
	if binary.Read(r, binary.LittleEndian, &n) != nil || n == 0 {
		return nil, blob.Info{}, errInvalid
	}
	f, e := os.CreateTemp("", "ant-cargo-*.crate")
	if e != nil {
		return nil, blob.Info{}, e
	}
	fail := func(e error) (*os.File, blob.Info, error) { f.Close(); os.Remove(f.Name()); return nil, blob.Info{}, e }
	h := sha256.New()
	if count, copyErr := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(r, int64(n)), make([]byte, 64<<10)); copyErr != nil || count != int64(n) {
		return fail(errInvalid)
	}
	var extra [1]byte
	if count, e := io.ReadFull(r, extra[:]); count != 0 || e != io.EOF {
		return fail(errInvalid)
	}
	if _, e = f.Seek(0, 0); e != nil {
		return fail(e)
	}
	if e = validateArchiveContext(ctx, f, m, int64(n)*maxExpansionRatio); e != nil {
		return fail(e)
	}
	if _, e = f.Seek(0, 0); e != nil {
		return fail(e)
	}
	return f, blob.Info{Digest: blob.Digest(hex.EncodeToString(h.Sum(nil))), Size: int64(n)}, nil
}
func validateArchive(r io.Reader, m metadata, maxExpanded int64) error {
	return validateArchiveContext(context.Background(), r, m, maxExpanded)
}
func validateArchiveContext(ctx context.Context, r io.Reader, m metadata, maxExpanded int64) error {
	gz, e := gzip.NewReader(&cancelReader{ctx: ctx, reader: r})
	if e != nil {
		return errInvalid
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: &cancelReader{ctx: ctx, reader: gz}, N: maxExpanded + 1}
	tr := tar.NewReader(limited)
	root := m.Name + "-" + m.Version
	seen := map[string]bool{}
	manifest := false
	for count := 0; ; count++ {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil || count >= 100000 {
			return errInvalid
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name != path.Clean(name) || strings.ContainsAny(name, "\\\x00") || (name != root && !strings.HasPrefix(name, root+"/")) || seen[name] {
			return errInvalid
		}
		seen[name] = true
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return errInvalid
		}
		if h.Name == root+"/Cargo.toml" {
			if h.Typeflag != tar.TypeReg || h.Size == 0 || h.Size > maxMetadata {
				return errInvalid
			}
			b, e := io.ReadAll(io.LimitReader(tr, maxMetadata+1))
			if e != nil || bytes.IndexByte(b, 0) >= 0 || !manifestIdentity(b, m) {
				return errInvalid
			}
			manifest = true
		}
	}
	// Consume gzip trailer so CRC errors are not hidden by tar's end marker.
	if _, e = io.Copy(io.Discard, limited); e != nil || limited.N <= 0 || !manifest {
		return errInvalid
	}
	return nil
}

// Cargo emits a normalized manifest with explicit package identity. Accept only
// literal single-line identity fields; workspace inheritance cannot occur in a
// published crate. This is deliberately not an interpreter for arbitrary TOML.
func manifestIdentity(data []byte, m metadata) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxMetadata)
	inPackage, seenPackage := false, false
	values := map[string]string{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section, _, _ := strings.Cut(line, "#")
			section = strings.TrimSpace(section)
			inPackage = section == "[package]"
			if inPackage {
				if seenPackage {
					return false
				}
				seenPackage = true
			}
			continue
		}
		if !inPackage {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || (key != "name" && key != "version") {
			continue
		}
		if _, ok = values[key]; ok {
			return false
		}
		value = strings.TrimSpace(value)
		if len(value) < 2 || (value[0] != '"' && value[0] != '\'') {
			return false
		}
		quote := value[0]
		end := 1
		for ; end < len(value); end++ {
			if value[end] == quote {
				break
			}
			if value[end] == '\\' && quote == '"' {
				end++
			}
		}
		if end >= len(value) {
			return false
		}
		tail := strings.TrimSpace(value[end+1:])
		if tail != "" && !strings.HasPrefix(tail, "#") {
			return false
		}
		literal := value[:end+1]
		if quote == '"' {
			v, e := strconv.Unquote(literal)
			if e != nil {
				return false
			}
			values[key] = v
		} else {
			values[key] = literal[1 : len(literal)-1]
		}
	}
	return scanner.Err() == nil && values["name"] == m.Name && values["version"] == m.Version
}

type cancelReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return r.reader.Read(p)
}
