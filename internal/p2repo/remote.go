package p2repo

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- p2 download.md5 is the integrity value some repositories publish.
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/ulikunitz/xz"
)

// Pull-through for p2 repositories. The remote source origin is the repository
// root (the directory holding p2.index, content.* and artifacts.*). Trust model,
// as for APT and RPM: the metadata indexes are served verbatim from upstream,
// never rewritten, merged or re-signed, and a repository is answered by exactly
// one owner (a local snapshot is never extended from upstream). Bundles,
// features and binaries are fetched on first download from the standard layout
// (plugins/, features/, binary/), served only if the upstream artifacts index
// lists them, verified against its download.checksum.sha-256 (or download.md5
// and download.size) before the first byte is served, and cached. Signed JAR
// bytes are never touched. Composite repositories and packed (Pack200) artifacts
// are not supported; indexes rely on the in-process metadata cache.

var indexMedia = map[string]string{
	"p2.index":         "text/plain; charset=utf-8",
	"content.xml":      "application/xml",
	"content.jar":      "application/java-archive",
	"content.xml.xz":   "application/x-xz",
	"artifacts.xml":    "application/xml",
	"artifacts.jar":    "application/java-archive",
	"artifacts.xml.xz": "application/x-xz",
}

func manifestPub(feed string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: manifestCoordinate}
}

// serveRemoteIndex answers an index file from upstream. It returns true when the
// response was written; a degraded upstream is returned so the caller can fall
// back to the locally cached view.
func (s *Repository) serveRemoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, file string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	o := artifact.Origin{Path: file}
	doc, err := s.service.RemoteMetadata(ctx, p, manifestPub(feed), o)
	switch {
	case err == nil:
		media := indexMedia[file]
		w.Header().Set("Content-Type", media)
		w.Header().Set("Content-Length", strconv.Itoa(len(doc.Body)))
		if doc.ETag != "" {
			w.Header().Set("ETag", doc.ETag)
		}
		if doc.Stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		if r.Method != http.MethodHead {
			_, _ = w.Write(doc.Body)
		}
		return true, nil
	case errors.Is(err, artifact.ErrNotFound):
		if s.service.HasRemote(ctx, p, manifestPub(feed)) {
			// The upstream answered 404 and no local snapshot owns this repository.
			failure(w, artifact.ErrNotFound)
			return true, nil
		}
		return false, nil
	case errors.Is(err, artifact.ErrTooLarge):
		stream, serr := s.service.RemoteStream(ctx, p, manifestPub(feed), o)
		if serr != nil {
			if errors.Is(serr, artifact.ErrNotFound) {
				return false, nil
			}
			if errors.Is(serr, artifact.ErrUnavailable) {
				return false, serr
			}
			failure(w, serr)
			return true, nil
		}
		defer stream.Body.Close()
		w.Header().Set("Content-Type", indexMedia[file])
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			if _, cerr := io.CopyBuffer(struct{ io.Writer }{w}, stream.Body, make([]byte, 64<<10)); cerr != nil {
				panic(http.ErrAbortHandler)
			}
		}
		return true, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	}
	failure(w, err)
	return true, nil
}

type remoteArtifact struct {
	sha256, md5 string
	size        int64
}

type remoteIndexEntry struct {
	arts   map[[3]string]remoteArtifact
	source string
}

var parsed = struct {
	sync.Mutex
	m     map[[32]byte]*remoteIndexEntry
	order [][32]byte
}{m: map[[32]byte]*remoteIndexEntry{}}

const maxParsedIndexes = 8

// decodeArtifactsXML returns the XML of an artifacts index served as xml, xml.xz or jar.
func decodeArtifactsXML(file string, body []byte) (io.Reader, error) {
	switch file {
	case "artifacts.xml":
		return bytes.NewReader(body), nil
	case "artifacts.xml.xz":
		return xz.NewReader(bytes.NewReader(body))
	case "artifacts.jar":
		z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, errInvalid
		}
		for _, f := range z.File {
			if f.Name == "artifacts.xml" {
				return f.Open()
			}
		}
	}
	return nil, errInvalid
}

// parseRemoteArtifacts reads the artifact descriptors a client could be served.
// It is lenient: packed (format) artifacts and artifacts without a checksum are
// skipped instead of failing the repository, because they cannot be served
// verified.
func parseRemoteArtifacts(r io.Reader) (map[[3]string]remoteArtifact, error) {
	d := xml.NewDecoder(io.LimitReader(r, maxXML+1))
	out := map[[3]string]remoteArtifact{}
	var (
		depth                   int
		sawRoot                 bool
		inArtifacts             bool
		classifier, id, version string
		cur                     remoteArtifact
		packed, active          bool
	)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errInvalid
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			sawRoot = sawRoot || depth == 1
			switch {
			case depth == 1 && t.Name.Local != "repository":
				return nil, errInvalid
			case depth == 2:
				inArtifacts = t.Name.Local == "artifacts"
			case depth == 3 && inArtifacts && t.Name.Local == "artifact":
				active, packed, cur = true, false, remoteArtifact{size: -1}
				classifier, id, version = "", "", ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "classifier":
						classifier = a.Value
					case "id":
						id = a.Value
					case "version":
						version = a.Value
					}
				}
			case active && depth == 5 && t.Name.Local == "property":
				var name, value string
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "name":
						name = a.Value
					case "value":
						value = a.Value
					}
				}
				switch name {
				case "format":
					packed = true
				case "download.checksum.sha-256":
					cur.sha256 = strings.ToLower(value)
				case "download.md5":
					cur.md5 = strings.ToLower(value)
				case "download.size":
					if n, e := strconv.ParseInt(value, 10, 64); e == nil && n >= 0 {
						cur.size = n
					}
				}
			}
		case xml.EndElement:
			if active && depth == 3 {
				active = false
				if !packed && dirFor(classifier) != "" && validID(id) && validVersion(version) && (hexLen(cur.sha256, 64) || hexLen(cur.md5, 32)) {
					if !hexLen(cur.sha256, 64) {
						cur.sha256 = ""
					}
					if !hexLen(cur.md5, 32) {
						cur.md5 = ""
					}
					out[[3]string{classifier, id, version}] = cur
					if len(out) > 1_000_000 {
						return nil, errInvalid
					}
				}
			}
			depth--
		}
	}
	if depth != 0 || !sawRoot {
		return nil, errInvalid
	}
	return out, nil
}

func hexLen(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// remoteArtifacts loads the upstream artifacts index (xz, jar or xml, the first
// the upstream has) and returns its parsed, cached form.
func (s *Repository) remoteArtifacts(ctx context.Context, p identity.Principal, feed string) (*remoteIndexEntry, error) {
	var lastErr error = artifact.ErrNotFound
	for _, file := range []string{"artifacts.xml.xz", "artifacts.jar", "artifacts.xml"} {
		doc, err := s.service.RemoteMetadata(ctx, p, manifestPub(feed), artifact.Origin{Path: file})
		if errors.Is(err, artifact.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		key := sha256.Sum256(append([]byte(doc.SourceID+"\x00"+file+"\x00"), doc.Body...))
		parsed.Lock()
		entry := parsed.m[key]
		parsed.Unlock()
		if entry != nil {
			return entry, nil
		}
		rd, err := decodeArtifactsXML(file, doc.Body)
		if err != nil {
			return nil, artifact.ErrUnavailable
		}
		arts, err := parseRemoteArtifacts(rd)
		if c, ok := rd.(io.Closer); ok {
			_ = c.Close()
		}
		if err != nil {
			return nil, artifact.ErrUnavailable
		}
		entry = &remoteIndexEntry{arts: arts, source: doc.SourceID}
		parsed.Lock()
		if len(parsed.order) >= maxParsedIndexes {
			delete(parsed.m, parsed.order[0])
			parsed.order = parsed.order[1:]
		}
		parsed.m[key] = entry
		parsed.order = append(parsed.order, key)
		parsed.Unlock()
		return entry, nil
	}
	return nil, lastErr
}

// pullArtifact materializes a bundle, feature or binary missing from the store.
// retry is true once the file is cached; served is true when a streamed response
// was written.
func (s *Repository) pullArtifact(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, classifier, id, version string) (retry, served bool, err error) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	idx, err := s.remoteArtifacts(ctx, p, pub.FeedID)
	cancel()
	if err != nil {
		return false, false, err
	}
	desc, ok := idx.arts[[3]string{classifier, id, version}]
	if !ok {
		return false, false, artifact.ErrNotFound
	}
	if classifier == "binary" {
		pub.MediaType = "application/octet-stream"
	}
	o := artifact.Origin{Path: artifact.EscapePath(entryPath(classifier, id, version)), SourceID: idx.source}
	if desc.sha256 != "" {
		o.Digest = blob.Digest(desc.sha256)
	} else {
		o.Verify = func(rs io.ReadSeeker) error {
			h := md5.New() // #nosec G401
			n, err := io.Copy(h, rs)
			if err != nil || hex.EncodeToString(h.Sum(nil)) != desc.md5 || (desc.size >= 0 && n != desc.size) {
				return errInvalid
			}
			return nil
		}
	}
	stream, again, err := s.service.PullThrough(r.Context(), p, pub, o)
	if err != nil {
		return false, false, err
	}
	if again {
		return true, false, nil
	}
	stream.ServeStream(w, r.Method)
	return false, true, nil
}
