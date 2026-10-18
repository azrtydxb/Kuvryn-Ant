package npmregistry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

const maxPublish = 64 << 20 // Encoded JSON, including base64; protocol-specific cap.
const maxMetadata = 2 << 20

var errInvalid = errors.New("invalid npm publication")
var namePart = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?(\+([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$`)

func validName(s string) bool {
	if len(s) > 214 {
		return false
	}
	if strings.HasPrefix(s, "@") {
		p := strings.Split(s[1:], "/")
		return len(p) == 2 && namePart.MatchString(p[0]) && namePart.MatchString(p[1])
	}
	return namePart.MatchString(s)
}
func namespace(name string) string {
	if strings.HasPrefix(name, "@") {
		return strings.Split(name, "/")[0]
	}
	return ""
}
func validVersion(s string) bool {
	if len(s) > 128 || !versionRE.MatchString(s) {
		return false
	}
	core := strings.SplitN(s, "+", 2)[0]
	numeric := strings.Split(strings.SplitN(core, "-", 2)[0], ".")
	for _, part := range numeric {
		n, e := strconv.ParseUint(part, 10, 64)
		if e != nil || n > 9007199254740991 {
			return false
		}
	}
	p := strings.SplitN(core, "-", 2)
	if len(p) == 2 {
		for _, x := range strings.Split(p[1], ".") {
			if len(x) > 1 && x[0] == '0' && strings.Trim(x, "0123456789") == "" {
				return false
			}
		}
	}
	return true
}
func validTag(s string) bool {
	return len(s) <= 128 && namePart.MatchString(s) && !(s[0] >= '0' && s[0] <= '9')
}
func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(h[:])), Size: int64(len(b))}
}

type publishDocument struct {
	Name        string                     `json:"name"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Tags        map[string]string          `json:"dist-tags"`
	Attachments map[string]struct {
		Data        string `json:"data"`
		Length      int64  `json:"length"`
		ContentType string `json:"content_type"`
	} `json:"_attachments"`
}
type parsed struct {
	version, tag      string
	metadata, tarball []byte
}

func parsePublish(ctx context.Context, r io.Reader, name string) (parsed, error) {
	var result parsed
	var doc publishDocument
	dec := json.NewDecoder(r)
	if dec.Decode(&doc) != nil || dec.Decode(new(any)) != io.EOF || doc.Name != name || len(doc.Versions) != 1 || len(doc.Attachments) != 1 || len(doc.Tags) != 1 {
		return result, errInvalid
	}
	for v, m := range doc.Versions {
		result.version = v
		result.metadata = m
	}
	if !validVersion(result.version) || len(result.metadata) > maxMetadata {
		return result, errInvalid
	}
	for t, v := range doc.Tags {
		if !validTag(t) || v != result.version {
			return result, errInvalid
		}
		result.tag = t
	}
	// npm uses the full scoped name for the attachment key, but a basename in the URL.
	for key, a := range doc.Attachments {
		if key != name+"-"+result.version+".tgz" || a.ContentType != "application/octet-stream" || a.Length <= 0 || a.Length > maxPublish {
			return result, errInvalid
		}
		b, e := base64.StdEncoding.Strict().DecodeString(a.Data)
		if e != nil || int64(len(b)) != a.Length {
			return result, errInvalid
		}
		result.tarball = b
	}
	var manifest map[string]json.RawMessage
	if json.Unmarshal(result.metadata, &manifest) != nil {
		return result, errInvalid
	}
	var mn, mv string
	_ = json.Unmarshal(manifest["name"], &mn)
	_ = json.Unmarshal(manifest["version"], &mv)
	if mn != name || mv != result.version {
		return result, errInvalid
	}
	var dist struct {
		Shasum    string `json:"shasum"`
		Integrity string `json:"integrity"`
	}
	if json.Unmarshal(manifest["dist"], &dist) != nil {
		return result, errInvalid
	}
	h1 := sha1.Sum(result.tarball)
	h512 := sha512.Sum512(result.tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(h512[:])
	if dist.Shasum != hex.EncodeToString(h1[:]) || dist.Integrity != integrity {
		return result, errInvalid
	}
	if validateArchive(ctx, result.tarball, name, result.version) != nil {
		return result, errInvalid
	}
	// Persist deterministic metadata without uploader-controlled download destinations.
	manifest["dist"], _ = json.Marshal(map[string]string{"shasum": dist.Shasum, "integrity": integrity})
	result.metadata, _ = json.Marshal(manifest)
	return result, nil
}
func validateArchive(ctx context.Context, b []byte, name, version string) error {
	gz, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return errInvalid
	}
	defer gz.Close()
	limit := int64(len(b)) * 1000
	if limit > 256<<20 {
		limit = 256 << 20
	}
	bounded := &io.LimitedReader{R: &contextReader{ctx: ctx, r: gz}, N: limit}
	tr := tar.NewReader(bounded)
	found := false
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errInvalid
		}
		count++
		if count > 10000 || h.Size < 0 || strings.ContainsAny(h.Name, "\\\x00") || path.Clean(h.Name) != strings.TrimSuffix(h.Name, "/") || !strings.HasPrefix(h.Name, "package/") {
			return errInvalid
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return errInvalid
		}
		if h.Name == "package/package.json" {
			if found || h.Typeflag != tar.TypeReg || h.Size > maxMetadata {
				return errInvalid
			}
			found = true
			data, e := io.ReadAll(tr)
			if e != nil {
				return errInvalid
			}
			var m struct{ Name, Version string }
			if json.Unmarshal(data, &m) != nil || m.Name != name || m.Version != version {
				return errInvalid
			}
		}
	}
	// Consume the gzip trailer to verify checksum and bound padding/decompression.
	if _, e = io.Copy(io.Discard, bounded); e != nil || bounded.N == 0 || !found {
		return errInvalid
	}
	return nil
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
