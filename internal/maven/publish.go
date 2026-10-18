package maven

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"  // Maven compatibility sidecar, not a trust primitive.
	"crypto/sha1" // Maven compatibility sidecar, not a trust primitive.
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const maxAsset = 128 << 20
const maxBundle = 256 << 20
const maxPOM = 1 << 20

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}

func (r *Repository) publish(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string) {
	if req.Header.Get("Content-Encoding") != "" {
		fail(w, artifact.ErrInput)
		return
	}
	media, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
		fail(w, artifact.ErrInput)
		return
	}
	base := l.publication(feed)
	source, err := r.service.Prepare(req.Context(), p, feed, base.Namespace, base.Coordinate, strings.TrimSuffix(l.key, "_bundle")+l.name+"-"+l.version+".pom")
	if err != nil {
		fail(w, err)
		return
	}
	select {
	case r.uploads <- struct{}{}:
		defer func() { <-r.uploads }()
	case <-req.Context().Done():
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxBundle)
	mr := multipart.NewReader(req.Body, params["boundary"])
	var pubs []artifact.Publication
	var bodies []io.Reader
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	seen := map[string]bool{}
	packaging := ""
	for {
		part, e := mr.NextRawPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(w, artifact.ErrInput)
			return
		}
		// Do not use FileName(), which silently strips traversal directories.
		_, disposition, e := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		name := disposition["filename"]
		if e != nil || len(seen) >= 4 || !validAsset(l, name, false) || seen[name] || part.Header.Get("Content-Transfer-Encoding") != "" {
			fail(w, artifact.ErrInput)
			return
		}
		seen[name] = true
		key := strings.TrimSuffix(l.key, "_bundle") + name
		pinned, e := r.service.Prepare(req.Context(), p, feed, base.Namespace, base.Coordinate, key)
		if e != nil {
			fail(w, e)
			return
		}
		if pinned != source {
			fail(w, artifact.ErrConflict)
			return
		}
		f, e := os.CreateTemp("", "ant-maven-*")
		if e != nil {
			fail(w, e)
			return
		}
		files = append(files, f)
		h256, h1, hmd5 := sha256.New(), sha1.New(), md5.New()
		limit := int64(maxAsset)
		if strings.HasSuffix(name, ".pom") {
			limit = maxPOM
		}
		n, e := io.CopyBuffer(io.MultiWriter(f, h256, h1, hmd5), io.LimitReader(contextReader{req.Context(), part}, limit+1), make([]byte, 64<<10))
		if e != nil || n > limit {
			fail(w, artifact.ErrInput)
			return
		}
		if strings.HasSuffix(name, ".pom") {
			data := make([]byte, n)
			if _, e = f.ReadAt(data, 0); e != nil {
				fail(w, e)
				return
			}
			packaging, e = validatePOM(data, l)
			if e != nil {
				fail(w, e)
				return
			}
		} else if e = validateJAR(req.Context(), f, n); e != nil {
			fail(w, e)
			return
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			fail(w, e)
			return
		}
		pub := base
		pub.AssetKey = key
		pub.ExpectedSource = source
		pub.MediaType = "application/java-archive"
		if strings.HasSuffix(name, ".pom") {
			pub.MediaType = "application/xml"
		}
		pub.Info = blob.Info{Digest: blob.Digest(hex.EncodeToString(h256.Sum(nil))), Size: n}
		for _, check := range []struct{ header, value string }{{"X-Checksum-Sha256", hex.EncodeToString(h256.Sum(nil))}, {"X-Checksum-Sha1", hex.EncodeToString(h1.Sum(nil))}, {"X-Checksum-Md5", hex.EncodeToString(hmd5.Sum(nil))}} {
			if values := part.Header.Values(check.header); len(values) > 1 || len(values) == 1 && values[0] != check.value {
				fail(w, blob.ErrIntegrity)
				return
			}
		}
		pubs = append(pubs, pub)
		bodies = append(bodies, f)
		for _, sum := range []struct{ suffix, value string }{{".sha256", hex.EncodeToString(h256.Sum(nil))}, {".sha1", hex.EncodeToString(h1.Sum(nil))}, {".md5", hex.EncodeToString(hmd5.Sum(nil))}} {
			side := pub
			side.AssetKey += sum.suffix
			side.MediaType = "text/plain"
			side.Info = info([]byte(sum.value))
			pubs = append(pubs, side)
			bodies = append(bodies, strings.NewReader(sum.value))
		}
	}
	pom := l.name + "-" + l.version + ".pom"
	jar := l.name + "-" + l.version + ".jar"
	if !seen[pom] || packaging != "pom" && !seen[jar] || packaging == "pom" && len(seen) != 1 {
		fail(w, artifact.ErrInput)
		return
	}
	records, err := r.service.PublishBundle(req.Context(), p, pubs, bodies)
	if err != nil {
		fail(w, err)
		return
	}
	code := 201
	if len(records) > 0 && records[0].Replayed {
		code = 204
	}
	w.WriteHeader(code)
}
func info(b []byte) blob.Info {
	sum := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(b))}
}

func validatePOM(data []byte, l location) (string, error) {
	if len(data) > maxPOM {
		return "", artifact.ErrInput
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	depth, roots := 0, 0
	seen := map[string]bool{}
	values := map[string]string{}
	field := ""
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", artifact.ErrInput
		}
		switch t := token.(type) {
		case xml.Directive:
			return "", artifact.ErrInput // Reject DTD/entity declarations entirely.
		case xml.StartElement:
			depth++
			if depth > 64 {
				return "", artifact.ErrInput
			}
			if depth == 1 {
				roots++
				if roots != 1 || t.Name.Local != "project" || t.Name.Space != "" && t.Name.Space != "http://maven.apache.org/POM/4.0.0" {
					return "", artifact.ErrInput
				}
			}
			if depth == 2 {
				field = t.Name.Local
				switch field {
				case "groupId", "artifactId", "version", "packaging", "modelVersion":
					if seen[field] {
						return "", artifact.ErrInput
					}
					seen[field] = true
				default:
					field = ""
				}
			}
			if depth > 2 && field != "" {
				return "", artifact.ErrInput
			}
		case xml.EndElement:
			if depth == 2 {
				field = ""
			}
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return "", artifact.ErrInput
			}
			if field != "" {
				if len(values[field])+len(t) > 256 {
					return "", artifact.ErrInput
				}
				values[field] += string(t)
			}
		}
	}
	if roots != 1 || depth != 0 || strings.TrimSpace(values["groupId"]) != l.group || strings.TrimSpace(values["artifactId"]) != l.name || strings.TrimSpace(values["version"]) != l.version || strings.TrimSpace(values["modelVersion"]) != "4.0.0" {
		return "", artifact.ErrInput
	}
	packaging := strings.TrimSpace(values["packaging"])
	if packaging == "" {
		packaging = "jar"
	}
	if packaging != "jar" && packaging != "pom" {
		return "", artifact.ErrInput
	}
	return packaging, nil
}
func validateJAR(ctx context.Context, f *os.File, size int64) error {
	z, err := zip.NewReader(f, size)
	if err != nil || len(z.File) > 10000 || len(z.File) == 0 {
		return artifact.ErrInput
	}
	var total uint64
	names := map[string]bool{}
	buf := make([]byte, 64<<10)
	for _, entry := range z.File {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00:") || names[name] || entry.Mode()&os.ModeSymlink != 0 {
			return artifact.ErrInput
		}
		names[name] = true
		if entry.UncompressedSize64 > 1<<30 || entry.UncompressedSize64 > 1000*(entry.CompressedSize64+1) {
			return artifact.ErrInput
		}
		total += entry.UncompressedSize64
		if total > 1<<30 {
			return artifact.ErrInput
		}
		rc, e := entry.Open()
		if e != nil {
			return artifact.ErrInput
		}
		n, e := io.CopyBuffer(io.Discard, contextReader{ctx, io.LimitReader(rc, int64(entry.UncompressedSize64)+1)}, buf)
		closeErr := rc.Close()
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return e
		}
		if e != nil || closeErr != nil || uint64(n) != entry.UncompressedSize64 {
			return artifact.ErrInput
		}
	}
	return nil
}
