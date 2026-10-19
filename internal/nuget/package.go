package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
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
const maxPackage = 512 << 20
const maxExpanded = 2 << 30

var errInvalid = errors.New("invalid NuGet package")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)
var versionPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+){0,3})(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

func normalizeVersion(v string) (string, error) {
	if len(v) > 256 {
		return "", errInvalid
	}
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return "", errInvalid
	}
	parts := strings.Split(m[1], ".")
	for i, p := range parts {
		n, e := strconv.ParseUint(p, 10, 31)
		if e != nil {
			return "", errInvalid
		}
		parts[i] = strconv.FormatUint(n, 10)
	}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	if len(parts) == 4 && parts[3] == "0" {
		parts = parts[:3]
	}
	v = strings.Join(parts, ".")
	if m[2] != "" {
		for _, p := range strings.Split(m[2], ".") {
			if len(p) > 1 && p[0] == '0' && strings.Trim(p, "0123456789") == "" {
				return "", errInvalid
			}
		}
		v += "-" + strings.ToLower(m[2])
	}
	return v, nil
}

type dependency struct {
	ID    string `xml:"id,attr" json:"id"`
	Range string `xml:"version,attr" json:"range,omitempty"`
}
type dependencyGroup struct {
	Framework    string       `xml:"targetFramework,attr" json:"targetFramework,omitempty"`
	Dependencies []dependency `xml:"dependency" json:"dependencies"`
}
type metadata struct {
	ID           string `xml:"id" json:"id"`
	Version      string `xml:"version" json:"version"`
	Authors      string `xml:"authors" json:"authors"`
	Description  string `xml:"description" json:"description"`
	Dependencies struct {
		Direct []dependency      `xml:"dependency"`
		Groups []dependencyGroup `xml:"group"`
	} `xml:"dependencies" json:"-"`
	Groups []dependencyGroup `xml:"-" json:"dependencyGroups"`
}

func parseManifest(data []byte) (metadata, error) {
	var m metadata
	// dotnet pack writes the nuspec with a UTF-8 byte-order mark.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	d := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	roots := 0
	seen := map[string]bool{}
	var stack []string
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return m, errInvalid
		}
		switch x := t.(type) {
		case xml.Directive:
			return m, errInvalid
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(x)) != "" {
				return m, errInvalid
			}
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return m, errInvalid
				}
			}
			depth++
			if depth > 32 {
				return m, errInvalid
			}
			stack = append(stack, x.Name.Local)
			if depth == 1 && x.Name.Local != "package" {
				return m, errInvalid
			}
			if depth == 2 && x.Name.Local == "metadata" || depth == 3 && len(stack) > 1 && stack[1] == "metadata" && (x.Name.Local == "id" || x.Name.Local == "version") {
				k := strings.Join(stack, "/")
				if seen[k] {
					return m, errInvalid
				}
				seen[k] = true
			}
		case xml.EndElement:
			depth--
			stack = stack[:len(stack)-1]
		}
	}
	var doc struct {
		Metadata metadata `xml:"metadata"`
	}
	if xml.Unmarshal(data, &doc) != nil {
		return m, errInvalid
	}
	m = doc.Metadata
	if !idPattern.MatchString(m.ID) || !seen["package/metadata/id"] || !seen["package/metadata/version"] {
		return m, errInvalid
	}
	m.ID = strings.ToLower(m.ID)
	var e error
	m.Version, e = normalizeVersion(m.Version)
	if e != nil {
		return m, e
	}
	m.Groups = append([]dependencyGroup{}, m.Dependencies.Groups...)
	if len(m.Dependencies.Direct) > 0 {
		m.Groups = append(m.Groups, dependencyGroup{Dependencies: m.Dependencies.Direct})
	}
	for _, g := range m.Groups {
		for _, d := range g.Dependencies {
			if !idPattern.MatchString(d.ID) || len(d.Range) > 256 {
				return m, errInvalid
			}
		}
	}
	return m, nil
}
func info(data []byte) blob.Info {
	h := sha256.Sum256(data)
	return blob.Info{Size: int64(len(data)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}
func inspectPackage(ctx context.Context, f *os.File, size int64) (metadata, []byte, error) {
	var m metadata
	// Bound central-directory allocation before archive/zip parses it. ZIP64
	// is unnecessary under this adapter's 512 MiB / 10,000-entry limits.
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
	var manifest []byte
	for _, entry := range z.File {
		if ctx.Err() != nil {
			return m, nil, ctx.Err()
		}
		name := entry.Name
		key := strings.ToLower(name)
		if name == "" || strings.ContainsAny(name, "\\\x00:") || strings.HasPrefix(name, "/") || path.Clean(strings.TrimSuffix(name, "/")) != strings.TrimSuffix(name, "/") || strings.HasPrefix(name, "../") || seen[key] || entry.Mode()&os.ModeSymlink != 0 {
			return m, nil, errInvalid
		}
		seen[key] = true
		if entry.UncompressedSize64 > maxExpanded-expanded {
			return m, nil, errInvalid
		}
		expanded += entry.UncompressedSize64
		if entry.UncompressedSize64 > 1<<20 && (entry.CompressedSize64 == 0 || entry.UncompressedSize64/entry.CompressedSize64 > 1000) {
			return m, nil, errInvalid
		}
		isManifest := !strings.Contains(name, "/") && strings.HasSuffix(key, ".nuspec")
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
	if e != nil {
		return m, nil, e
	}
	if !seen[m.ID+".nuspec"] {
		return m, nil, errInvalid
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
