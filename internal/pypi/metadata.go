package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"net/textproto"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

const maxMetadata = 1 << 20
const maxExpanded = 512 << 20

var errInvalid = errors.New("invalid Python distribution")
var nameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
var separators = regexp.MustCompile(`[-_.]+`)
var versionRE = regexp.MustCompile(`^([0-9]+!)?[0-9]+(\.[0-9]+)*((a|b|rc)[0-9]+)?(\.post[0-9]+)?(\.dev[0-9]+)?(\+[a-z0-9]+(\.[a-z0-9]+)*)?$`)
var tagRE = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

func normalize(s string) string { return separators.ReplaceAllString(strings.ToLower(s), "-") }
func validName(s string) bool   { return len(s) <= 200 && nameRE.MatchString(s) }

type metadata struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Filename       string `json:"filename"`
	RequiresPython string `json:"requires-python,omitempty"`
	Yanked         any    `json:"yanked"`
	Digest         string `json:"digest"`
	CoreDigest     string `json:"core-digest"`
}

func filename(s string) (name, version string, wheel bool, err error) {
	if len(s) > 240 || strings.ContainsAny(s, "/\\\x00\r\n") {
		return "", "", false, errInvalid
	}
	wheel = strings.HasSuffix(s, ".whl")
	if wheel {
		p := strings.Split(strings.TrimSuffix(s, ".whl"), "-")
		if len(p) != 5 && len(p) != 6 {
			return "", "", false, errInvalid
		}
		name, version = p[0], p[1]
		for _, tag := range p[len(p)-3:] {
			if !tagRE.MatchString(tag) {
				return "", "", false, errInvalid
			}
		}
		if len(p) == 6 && (p[2] == "" || p[2][0] < '0' || p[2][0] > '9' || !tagRE.MatchString(p[2])) {
			return "", "", false, errInvalid
		}
	} else {
		if !strings.HasSuffix(s, ".tar.gz") {
			return "", "", false, errInvalid
		}
		stem := strings.TrimSuffix(s, ".tar.gz")
		i := strings.LastIndex(stem, "-")
		if i < 1 {
			return "", "", false, errInvalid
		}
		name, version = stem[:i], stem[i+1:]
	}
	if !validName(name) || len(version) > 128 || !versionRE.MatchString(version) {
		return "", "", false, errInvalid
	}
	return normalize(name), version, wheel, nil
}
func bytesInfo(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(h[:])), Size: int64(len(b))}
}
func versionKey(file string) string { return "file:" + string(bytesInfo([]byte(file)).Digest) }
func safePath(s string) bool {
	v := strings.TrimSuffix(s, "/")
	return v != "" && !strings.ContainsAny(v, "\\\x00\r\n:") && !strings.HasPrefix(v, "/") && path.Clean(v) == v && v != ".." && !strings.HasPrefix(v, "../")
}
func boundedRead(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errInvalid
	}
	return b, nil
}
func inspect(ctx context.Context, f *os.File, file string) (metadata, []byte, error) {
	st, e := f.Stat()
	if e != nil {
		return metadata{}, nil, e
	}
	expansionLimit := st.Size()*200 + maxMetadata
	if expansionLimit > maxExpanded {
		expansionLimit = maxExpanded
	}
	name, version, wheel, e := filename(file)
	if e != nil {
		return metadata{}, nil, e
	}
	m := metadata{Name: name, Version: version, Filename: file, Yanked: false}
	var core []byte
	seen := map[string]bool{}
	total := int64(0)
	if _, e = f.Seek(0, 0); e != nil {
		return m, nil, e
	}
	if wheel {
		st, e := f.Stat()
		if e != nil {
			return m, nil, e
		}
		if e := checkZIPDirectory(f, st.Size()); e != nil {
			return m, nil, e
		}
		z, e := zip.NewReader(f, st.Size())
		if e != nil || len(z.File) > 10000 {
			return m, nil, errInvalid
		}
		stem := strings.Join(strings.Split(file, "-")[:2], "-") + ".dist-info/"
		required := map[string]bool{"METADATA": false, "WHEEL": false, "RECORD": false}
		var record []byte
		verified := map[string]wheelFile{}
		for _, entry := range z.File {
			if !safePath(entry.Name) || seen[entry.Name] || entry.Mode()&os.ModeSymlink != 0 || (!entry.Mode().IsRegular() && !entry.FileInfo().IsDir()) || entry.UncompressedSize64 > maxExpanded {
				return m, nil, errInvalid
			}
			seen[entry.Name] = true
			total += int64(entry.UncompressedSize64)
			if total > expansionLimit {
				return m, nil, errInvalid
			}
			if strings.Contains(entry.Name, ".dist-info/") && !strings.HasPrefix(entry.Name, stem) {
				return m, nil, errInvalid
			}
			raw, e := entry.Open()
			if e != nil {
				return m, nil, errInvalid
			}
			h256, h384, h512 := sha256.New(), sha512.New384(), sha512.New()
			r := &contextReadCloser{contextReader: contextReader{ctx: ctx, r: io.TeeReader(raw, io.MultiWriter(h256, h384, h512))}, Closer: raw}
			key := strings.TrimPrefix(entry.Name, stem)
			if _, ok := required[key]; ok && strings.HasPrefix(entry.Name, stem) {
				b, e := boundedRead(r, maxMetadata)
				r.Close()
				if e != nil {
					return m, nil, e
				}
				required[key] = true
				if key == "METADATA" {
					core = b
				}
				if key == "RECORD" {
					record = b
				}
				if key == "WHEEL" && !validWheel(b, file) {
					return m, nil, errInvalid
				}
			} else {
				n, e := io.CopyBuffer(io.Discard, io.LimitReader(r, int64(entry.UncompressedSize64)+1), make([]byte, 64<<10))
				r.Close()
				if e != nil || n != int64(entry.UncompressedSize64) {
					return m, nil, errInvalid
				}
			}
			if !entry.FileInfo().IsDir() {
				verified[entry.Name] = wheelFile{size: entry.UncompressedSize64, hashes: map[string]string{"sha256": base64.RawURLEncoding.EncodeToString(h256.Sum(nil)), "sha384": base64.RawURLEncoding.EncodeToString(h384.Sum(nil)), "sha512": base64.RawURLEncoding.EncodeToString(h512.Sum(nil))}}
			}
		}
		if e := verifyRecord(record, stem, verified); e != nil {
			return m, nil, e
		}
		for _, ok := range required {
			if !ok {
				return m, nil, errInvalid
			}
		}
	} else {
		gz, e := gzip.NewReader(f)
		if e != nil {
			return m, nil, errInvalid
		}
		defer gz.Close()
		limited := &io.LimitedReader{R: &contextReader{ctx: ctx, r: gz}, N: expansionLimit + 1}
		tr := tar.NewReader(limited)
		root := strings.TrimSuffix(file, ".tar.gz") + "/"
		count := 0
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			count++
			if e != nil || count > 10000 || !safePath(h.Name) || !strings.HasPrefix(h.Name, root) || seen[h.Name] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
				return m, nil, errInvalid
			}
			seen[h.Name] = true
			if h.Name == root+"PKG-INFO" {
				core, e = boundedRead(tr, maxMetadata)
				if e != nil {
					return m, nil, e
				}
			} else {
				if _, e = io.CopyBuffer(io.Discard, tr, make([]byte, 64<<10)); e != nil {
					return m, nil, errInvalid
				}
			}
		}
		if _, e = io.CopyBuffer(io.Discard, limited, make([]byte, 64<<10)); e != nil || limited.N <= 0 {
			return m, nil, errInvalid
		}
	}
	if len(core) == 0 {
		return m, nil, errInvalid
	}
	headers, e := parseHeaders(core)
	if e != nil {
		return m, nil, errInvalid
	}
	for _, key := range []string{"Name", "Version", "Metadata-Version"} {
		if len(headers.Values(key)) != 1 {
			return m, nil, errInvalid
		}
	}
	if !validName(headers.Get("Name")) || normalize(headers.Get("Name")) != name || headers.Get("Version") != version || len(headers.Values("Requires-Python")) > 1 {
		return m, nil, errInvalid
	}
	m.RequiresPython = headers.Get("Requires-Python")
	if strings.ContainsAny(m.RequiresPython, "\r\n") {
		return m, nil, errInvalid
	}
	m.CoreDigest = string(bytesInfo(core).Digest)
	_, e = f.Seek(0, 0)
	return m, core, e
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}

type contextReadCloser struct {
	contextReader
	io.Closer
}

// Bound the central directory before archive/zip allocates entries. ZIP64 is
// deliberately rejected until it has an equally bounded directory parser.
func checkZIPDirectory(f *os.File, size int64) error {
	n := int64(65557)
	if size < n {
		n = size
	}
	if n < 22 {
		return errInvalid
	}
	tail := make([]byte, int(n))
	if _, e := f.ReadAt(tail, size-n); e != nil {
		return errInvalid
	}
	for i := len(tail) - 22; i >= 0; i-- {
		if !bytes.Equal(tail[i:i+4], []byte{'P', 'K', 5, 6}) {
			continue
		}
		b := tail[i:]
		if int(binary.LittleEndian.Uint16(b[20:22]))+22 != len(b) {
			continue
		}
		count := binary.LittleEndian.Uint16(b[10:12])
		directorySize := binary.LittleEndian.Uint32(b[12:16])
		if count == 0 || count > 10000 || directorySize > 16<<20 || binary.LittleEndian.Uint16(b[4:6]) != 0 || binary.LittleEndian.Uint16(b[6:8]) != 0 || binary.LittleEndian.Uint16(b[8:10]) != count {
			return errInvalid
		}
		return nil
	}
	return errInvalid
}

type wheelFile struct {
	size   uint64
	hashes map[string]string
}

func verifyRecord(data []byte, stem string, files map[string]wheelFile) error {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = 3
	seen := map[string]bool{}
	for {
		row, e := reader.Read()
		if e == io.EOF {
			break
		}
		if e != nil || seen[row[0]] {
			return errInvalid
		}
		seen[row[0]] = true
		file, ok := files[row[0]]
		if !ok {
			return errInvalid
		}
		if row[0] == stem+"RECORD" {
			if row[1] != "" || row[2] != "" {
				return errInvalid
			}
			continue
		}
		algorithm, digest, ok := strings.Cut(row[1], "=")
		size, e := strconv.ParseUint(row[2], 10, 64)
		if !ok || digest == "" || file.hashes[algorithm] != digest || e != nil || size != file.size {
			return errInvalid
		}
	}
	for name := range files {
		if name == stem+"RECORD.jws" || name == stem+"RECORD.p7s" {
			continue
		}
		if !seen[name] {
			return errInvalid
		}
	}
	return nil
}
func validWheel(data []byte, file string) bool {
	h, e := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(append([]byte{}, data...), '\n')))).ReadMIMEHeader()
	if e != nil || len(h.Values("Wheel-Version")) != 1 || h.Get("Wheel-Version") != "1.0" || (h.Get("Root-Is-Purelib") != "true" && h.Get("Root-Is-Purelib") != "false") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(file, ".whl"), "-")
	tags := map[string]bool{}
	for _, tag := range h.Values("Tag") {
		tags[tag] = true
	}
	expected := 0
	for _, py := range strings.Split(parts[len(parts)-3], ".") {
		for _, abi := range strings.Split(parts[len(parts)-2], ".") {
			for _, platform := range strings.Split(parts[len(parts)-1], ".") {
				expected++
				if !tags[py+"-"+abi+"-"+platform] {
					return false
				}
			}
		}
	}
	return expected == len(tags)
}

// parseHeaders reads the RFC 822 header block of a METADATA file. The body
// (long description) and the blank-line terminator are optional: setuptools
// writes neither when a project has no long description.
func parseHeaders(core []byte) (textproto.MIMEHeader, error) {
	b := append(append([]byte{}, core...), '\n', '\n')
	return textproto.NewReader(bufio.NewReader(bytes.NewReader(b))).ReadMIMEHeader()
}
