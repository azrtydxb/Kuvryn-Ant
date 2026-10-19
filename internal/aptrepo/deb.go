package aptrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxDeb       = 128 << 20
	maxControl   = 1 << 20 // compressed control member and expanded control.tar
	maxControlFl = 64 << 10
)

var (
	errInvalid = errors.New("invalid Debian package")
	errCorrupt = errors.New("corrupt repository metadata")

	pkgPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	versionPattern = regexp.MustCompile(`^(?:[0-9]+:)?[0-9][A-Za-z0-9.+~-]*$`)
	archPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`) // distribution and component
	fieldPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
)

func validPackage(n string) bool { return len(n) <= 128 && pkgPattern.MatchString(n) }
func validVersion(v string) bool {
	return len(v) <= 128 && versionPattern.MatchString(v) && strings.Count(v, ":") <= 1
}
func stripEpoch(v string) string {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		return rest
	}
	return v
}

// field keeps the original spelling and folded continuation lines.
type field struct{ Name, Value string }

// control is one bounded Debian control paragraph.
type control struct{ fields []field }

func (c control) get(name string) string {
	for _, f := range c.fields {
		if strings.EqualFold(f.Name, name) {
			return f.Value
		}
	}
	return ""
}

// derived fields are computed by the repository and must never be trusted from
// package-supplied metadata.
var derived = map[string]bool{"filename": true, "size": true, "md5sum": true, "sha1": true, "sha256": true, "sha512": true, "description-md5": true}

// parseParagraph parses exactly one paragraph. Values keep continuation lines
// with their leading whitespace so the emitted index preserves them verbatim.
func parseParagraph(b []byte) (control, error) {
	if len(b) == 0 || len(b) > maxControlFl || !utf8.Valid(b) || bytes.ContainsAny(b, "\x00\r") {
		return control{}, errInvalid
	}
	var c control
	seen := map[string]bool{}
	ended := false
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			ended = true
			continue
		}
		if ended {
			return control{}, errInvalid
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(c.fields) == 0 {
				return control{}, errInvalid
			}
			c.fields[len(c.fields)-1].Value += "\n" + strings.TrimRight(line, " \t")
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !fieldPattern.MatchString(k) || len(k) > 64 || seen[strings.ToLower(k)] {
			return control{}, errInvalid
		}
		seen[strings.ToLower(k)] = true
		c.fields = append(c.fields, field{k, strings.TrimSpace(v)})
	}
	return c, nil
}

func (c control) validate() error {
	if !validPackage(c.get("Package")) || !validVersion(c.get("Version")) || !archPattern.MatchString(c.get("Architecture")) {
		return errInvalid
	}
	if c.get("Maintainer") == "" || c.get("Description") == "" {
		return errInvalid
	}
	if s := c.get("Source"); s != "" && !validPackage(sourceName(s)) {
		return errInvalid
	}
	return nil
}

func sourceName(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// poolDir returns the Debian pool directory (prefix/source) for the control.
func (c control) poolDir() string {
	src := c.get("Package")
	if s := c.get("Source"); s != "" {
		src = sourceName(s)
	}
	prefix := src[:1]
	if strings.HasPrefix(src, "lib") && len(src) > 3 {
		prefix = src[:4]
	}
	return prefix + "/" + src
}

func (c control) fileName() string {
	return c.get("Package") + "_" + stripEpoch(c.get("Version")) + "_" + c.get("Architecture") + ".deb"
}

// stanza renders the Packages paragraph: package fields in order (without
// derived keys), then Filename and checksums computed by the repository.
func (c control) stanza(filename string, size int64, md5, sha1, sha256 string) string {
	var b strings.Builder
	for _, f := range c.fields {
		if derived[strings.ToLower(f.Name)] {
			continue
		}
		b.WriteString(f.Name + ": " + f.Value + "\n")
	}
	b.WriteString("Filename: " + filename + "\n")
	b.WriteString("Size: " + strconv.FormatInt(size, 10) + "\n")
	b.WriteString("MD5sum: " + md5 + "\n")
	b.WriteString("SHA1: " + sha1 + "\n")
	b.WriteString("SHA256: " + sha256 + "\n")
	return b.String()
}

// inspectDeb validates the ar container and extracts the control paragraph. It
// never executes maintainer scripts and bounds every member it reads.
func inspectDeb(r io.ReaderAt, size int64) (control, error) {
	var magic [8]byte
	if size < 8+60 || size > maxDeb {
		return control{}, errInvalid
	}
	if _, e := r.ReadAt(magic[:], 0); e != nil || string(magic[:]) != "!<arch>\n" {
		return control{}, errInvalid
	}
	pos := int64(8)
	var ctl []byte
	var haveData, haveVersion bool
	for n := 0; pos < size; n++ {
		if n >= 8 || pos+60 > size {
			return control{}, errInvalid
		}
		var h [60]byte
		if _, e := r.ReadAt(h[:], pos); e != nil || string(h[58:60]) != "`\n" {
			return control{}, errInvalid
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(h[:16])), "/")
		msize, e := strconv.ParseInt(strings.TrimSpace(string(h[48:58])), 10, 64)
		pos += 60
		if e != nil || msize < 0 || pos+msize > size {
			return control{}, errInvalid
		}
		switch {
		case n == 0:
			if name != "debian-binary" || msize != 4 {
				return control{}, errInvalid
			}
			v := make([]byte, 4)
			if _, e := r.ReadAt(v, pos); e != nil || !bytes.HasPrefix(v, []byte("2.")) {
				return control{}, errInvalid
			}
			haveVersion = true
		case name == "control.tar" || strings.HasPrefix(name, "control.tar."):
			if ctl != nil || msize > maxControl {
				return control{}, errInvalid
			}
			src, closeSrc, e := controlDecoder(strings.TrimPrefix(name, "control.tar"), io.NewSectionReader(r, pos, msize))
			if e != nil {
				return control{}, errInvalid
			}
			b, e := readControlTar(src)
			closeSrc()
			if e != nil {
				return control{}, e
			}
			ctl = b
		case name == "data.tar" || strings.HasPrefix(name, "data.tar."):
			if haveData || msize == 0 {
				return control{}, errInvalid
			}
			haveData = true
		default:
			return control{}, errInvalid
		}
		pos += msize + msize%2
	}
	if !haveVersion || !haveData || ctl == nil {
		return control{}, errInvalid
	}
	c, e := parseParagraph(ctl)
	if e != nil {
		return control{}, e
	}
	return c, c.validate()
}

func readControlTar(src io.Reader) ([]byte, error) {
	tr := tar.NewReader(io.LimitReader(src, maxControl+1))
	var out []byte
	for n := 0; ; n++ {
		if n > 1000 {
			return nil, errInvalid
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errInvalid
		}
		name := path.Clean(h.Name)
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "..") || strings.ContainsAny(name, "\x00\\") {
			return nil, errInvalid
		}
		if name == "control" {
			if h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > maxControlFl || out != nil {
				return nil, errInvalid
			}
			b, e := io.ReadAll(io.LimitReader(tr, maxControlFl+1))
			if e != nil || len(b) > maxControlFl {
				return nil, errInvalid
			}
			out = b
		}
	}
	if out == nil {
		return nil, errInvalid
	}
	return out, nil
}

// controlDecoder wraps the compressed control archive. ext is "" (plain) or the
// member suffix (.gz, .xz, .zst). dpkg-deb has written zstd by default since
// dpkg 1.21.18 (Debian trixie). Decompressed output is bounded by the caller.
func controlDecoder(ext string, src io.Reader) (io.Reader, func(), error) {
	switch ext {
	case "":
		return src, func() {}, nil
	case ".gz":
		g, e := gzip.NewReader(src)
		if e != nil {
			return nil, nil, e
		}
		return g, func() { g.Close() }, nil
	case ".xz":
		x, e := xz.NewReader(src)
		if e != nil {
			return nil, nil, e
		}
		return x, func() {}, nil
	case ".zst":
		z, e := zstd.NewReader(src, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderMaxWindow(8<<20))
		if e != nil {
			return nil, nil, e
		}
		return z, z.Close, nil
	}
	return nil, nil, errInvalid
}
