package conan

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5" // Conan's native manifest identity.
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Conan v2 remotes (for example center2.conan.io). The origin
// is the server root, so the feed route `v2/conans/...` maps one to one onto
// the upstream path; the Conan v2 API carries no absolute URLs. Revision
// listings (`latest`, `revisions`, package id search, file inventories) are
// mutable and go through the runtime's metadata cache after shape validation.
// Revision files are immutable and cached on first download. Conan publishes
// no sha256: the recipe or package revision is the md5 of the sorted manifest
// lines, so `conanmanifest.txt` is verified against the revision in the URL
// and every other file against the manifest (a file by its md5, a tarball by
// the md5 of each member). The seam's Origin.Verify stages and checks the
// bytes before anything is served or cached. `v2/conans/search` stays local.

// remoteFilePath is the upstream path of one revision file.
func remoteFilePath(v reference, key string) string {
	p := "v2/conans/" + v.coordinate + "/revisions/" + v.rrev
	if v.pid != "" {
		p += "/packages/" + v.pid + "/revisions/" + v.prev
	}
	return p + "/files/" + key
}

func manifestVerify(rev string) func(io.ReadSeeker) error {
	return func(rs io.ReadSeeker) error {
		b, err := io.ReadAll(io.LimitReader(rs, maxMetadata+1))
		if err != nil || len(b) > maxMetadata {
			return errInvalid
		}
		_, err = manifest(b, rev)
		return err
	}
}

func fileVerify(key string, sums map[string]string) func(io.ReadSeeker) error {
	return func(rs io.ReadSeeker) error {
		if !strings.HasSuffix(key, ".tgz") {
			h := md5.New()
			if n, err := io.Copy(h, io.LimitReader(rs, maxMetadata+1)); err != nil || n > maxMetadata || sums[key] != hex.EncodeToString(h.Sum(nil)) {
				return errInvalid
			}
			return nil
		}
		gz, err := gzip.NewReader(rs)
		if err != nil {
			return errInvalid
		}
		defer gz.Close()
		limited := &io.LimitedReader{R: gz, N: maxExpanded + 1}
		tr := tar.NewReader(limited)
		members := 0
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return errInvalid
			}
			members++
			if members > maxMembers || !safePath(strings.TrimSuffix(h.Name, "/")) {
				return errInvalid
			}
			if h.Typeflag == tar.TypeDir {
				continue
			}
			if h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxExpanded {
				return errInvalid
			}
			name := h.Name
			if key == "conan_sources.tgz" {
				name = "export_source/" + name
			}
			want, ok := sums[name]
			if !ok {
				return errInvalid
			}
			hash := md5.New()
			if _, err = io.CopyBuffer(hash, tr, make([]byte, 64<<10)); err != nil || hex.EncodeToString(hash.Sum(nil)) != want {
				return errInvalid
			}
		}
		// Drain for the gzip checksum, within the same expansion budget.
		if _, err = io.Copy(io.Discard, limited); err != nil || limited.N <= 0 {
			return errInvalid
		}
		return nil
	}
}

// drainStream finishes a streamed fill (only reachable when the seam could not
// verify before serving) so the cache entry is committed.
func drainStream(m *artifact.Materialized) {
	if m != nil {
		_, _ = io.Copy(io.Discard, m.Body)
		_ = m.Body.Close()
	}
}

// pullThrough materializes a missing revision file. retry=true means the file
// is now in the store.
func (s *Repository) pullThrough(ctx context.Context, p identity.Principal, feed string, v reference, key string) (bool, error) {
	rev := v.rrev
	if v.pid != "" {
		rev = v.prev
	}
	mpub := v.pub(feed, "conanmanifest.txt")
	mpub.MediaType = mediaType(mpub.AssetKey)
	rec, body, err := s.service.Open(ctx, p, mpub, 0, -1)
	if errors.Is(err, artifact.ErrNotFound) {
		stream, _, perr := s.service.PullThrough(ctx, p, mpub, artifact.Origin{Path: remoteFilePath(v, "conanmanifest.txt"), Verify: manifestVerify(rev)})
		if perr != nil {
			return false, perr
		}
		drainStream(stream)
		rec, body, err = s.service.Open(ctx, p, mpub, 0, -1)
	}
	if err != nil {
		return false, err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxMetadata+1))
	body.Close()
	if err != nil || len(data) > maxMetadata {
		return false, artifact.ErrUnavailable
	}
	if key == "conanmanifest.txt" {
		return true, nil
	}
	sums, err := manifest(data, rev)
	if err != nil {
		return false, artifact.ErrUnavailable
	}
	pub := v.pub(feed, key)
	pub.MediaType = mediaType(key)
	stream, _, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: remoteFilePath(v, key), SourceID: rec.SourceID, Verify: fileVerify(key, sums)})
	if err != nil {
		return false, err
	}
	drainStream(stream)
	return true, nil
}

// renderRemote validates an upstream listing and returns the document to serve.
func renderRemote(kind string, body []byte) (any, bool) {
	switch kind {
	case "latest":
		var in struct{ Revision, Time string }
		if json.Unmarshal(body, &in) != nil || !revision.MatchString(in.Revision) || len(in.Time) > 64 {
			return nil, false
		}
		return map[string]any{"revision": in.Revision, "time": in.Time}, true
	case "revisions":
		var in struct {
			Revisions []struct{ Revision, Time string } `json:"revisions"`
		}
		if json.Unmarshal(body, &in) != nil || len(in.Revisions) > 4096 {
			return nil, false
		}
		out := []map[string]string{}
		for _, r := range in.Revisions {
			if revision.MatchString(r.Revision) && len(r.Time) <= 64 {
				out = append(out, map[string]string{"revision": r.Revision, "time": r.Time})
			}
		}
		return map[string]any{"revisions": out}, true
	case "files":
		var in struct {
			Files map[string]json.RawMessage `json:"files"`
		}
		if json.Unmarshal(body, &in) != nil || len(in.Files) > 4096 {
			return nil, false
		}
		files := map[string]any{}
		for name := range in.Files {
			if name == "conanmanifest.txt" || name == "conanfile.py" || name == "conan_export.tgz" || name == "conan_sources.tgz" || name == "conaninfo.txt" || name == "conan_package.tgz" {
				files[name] = map[string]any{}
			}
		}
		return map[string]any{"files": files}, true
	case "search":
		var in map[string]json.RawMessage
		if json.Unmarshal(body, &in) != nil || len(in) > 4096 {
			return nil, false
		}
		out := map[string]json.RawMessage{}
		for id, raw := range in {
			if packageID.MatchString(id) {
				out[id] = raw
			}
		}
		return out, true
	}
	return nil, false
}

// serveRemoteMetadata answers a listing request from upstream. It returns true
// once the response was written; a degraded upstream error is returned so the
// caller can fall back to the locally cached view.
func (s *Repository) serveRemoteMetadata(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, v reference, kind string) (bool, error) {
	switch kind {
	case "latest", "revisions", "files", "search":
	default:
		return false, nil
	}
	route := strings.Join(strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[3:], "/")
	doc, err := s.service.RemoteMetadata(r.Context(), p, artifact.Publication{FeedID: feed, Namespace: v.namespace, Coordinate: v.coordinate}, artifact.Origin{Path: route, Representation: "application/json"})
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	out, ok := renderRemote(kind, doc.Body)
	if !ok {
		failure(w, artifact.ErrUnavailable)
		return true, nil
	}
	if doc.Stale {
		for k, val := range artifact.StaleHeaders() {
			w.Header().Set(k, val)
		}
	}
	writeJSON(w, r, out)
	return true, nil
}
