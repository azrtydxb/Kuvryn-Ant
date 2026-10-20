package condarepo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Conda channels (for example conda.anaconda.org/conda-forge).
// The origin is the channel root, so `<subdir>/repodata.json` lives under it.
// repodata.json is read through the runtime's metadata cache, validated entry by
// entry (filename must match name-version-build, sha256 must be present) and
// re-rendered with `info` reduced to the subdir: repodata entries carry no URLs,
// but `info.base_url` would redirect clients away from this feed and is dropped.
// A subdir is one ownership unit, as for local channels. Packages (.tar.bz2 and
// .conda) are fetched on first download from `<subdir>/<file>`, verified against
// the repodata sha256 and cached. The verified repodata entry of a .tar.bz2 is
// cached beside it so the owner's packages remain listable during an outage.

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// remoteExt returns the package extension of a remote filename, or "".
func remoteExt(name string) string {
	for _, ext := range []string{".tar.bz2", ".conda"} {
		if strings.HasSuffix(name, ext) {
			return ext
		}
	}
	return ""
}

type remotePackage struct {
	filename, sha256 string
	conda            bool
	raw              json.RawMessage
}

// remoteRepodata fetches and validates the upstream repodata.json of a subdir.
func (s *Repository) remoteRepodata(ctx context.Context, p identity.Principal, feed, subdir, name string) ([]remotePackage, artifact.Document, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Namespace: subdir, Coordinate: "conda/" + subdir}, artifact.Origin{Path: subdir + "/" + name, Representation: "application/json"})
	if err != nil {
		return nil, doc, err
	}
	var in struct {
		Packages      map[string]json.RawMessage `json:"packages"`
		PackagesConda map[string]json.RawMessage `json:"packages.conda"`
	}
	if json.Unmarshal(doc.Body, &in) != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	var out []remotePackage
	for i, group := range []map[string]json.RawMessage{in.Packages, in.PackagesConda} {
		for filename, raw := range group {
			if e, ok := validateEntry(subdir, filename, i == 1, raw); ok {
				out = append(out, remotePackage{filename: e.filename, sha256: e.sha256, conda: e.conda, raw: e.raw})
			}
		}
	}
	return out, doc, nil
}

// validRemoteFilename applies the filename shape without the strict local
// charset rules: it only guards path construction.
func validRemoteFilename(s string) bool {
	if len(s) > 240 || remoteExt(s) == "" || strings.Count(strings.TrimSuffix(strings.TrimSuffix(s, ".tar.bz2"), ".conda"), "-") < 2 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.+!-", r)) {
			return false
		}
	}
	return s[0] != '.' && s[0] != '-'
}

// serveRemoteIndex answers a repodata.json request from upstream. It returns
// true once the response was written; a degraded upstream error is returned so
// the caller can fall back to the locally cached view.
func (s *Repository) serveRemoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, subdir, name string) (bool, error) {
	if s.indexes.isLarge(feed + "/" + subdir + "/" + name) {
		return s.serveLargeIndex(w, r, p, feed, subdir, name)
	}
	pkgs, doc, err := s.remoteRepodata(r.Context(), p, feed, subdir, name)
	switch {
	case errors.Is(err, artifact.ErrTooLarge):
		// Above the metadata bound: stream it instead of holding it in memory.
		s.indexes.markLarge(feed + "/" + subdir + "/" + name)
		return s.serveLargeIndex(w, r, p, feed, subdir, name)
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	packages, conda := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for _, x := range pkgs {
		if x.conda {
			conda[x.filename] = x.raw
		} else {
			packages[x.filename] = x.raw
		}
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	writeJSON(w, r, map[string]any{"info": map[string]string{"subdir": subdir}, "packages": packages, "packages.conda": conda, "removed": []string{}, "repodata_version": 1})
	return true, nil
}

// pullThrough materializes a missing package. It returns retry=true once the
// package is in the store.
func (s *Repository) pullThrough(ctx context.Context, p identity.Principal, pub artifact.Publication) (bool, error) {
	if !validRemoteFilename(pub.Version) {
		return false, artifact.ErrNotFound
	}
	if s.indexes.isLarge(pub.FeedID + "/" + pub.Namespace + "/repodata.json") {
		return s.pullThroughLarge(ctx, p, pub)
	}
	pkgs, doc, err := s.remoteRepodata(ctx, p, pub.FeedID, pub.Namespace, "repodata.json")
	if errors.Is(err, artifact.ErrTooLarge) {
		s.indexes.markLarge(pub.FeedID + "/" + pub.Namespace + "/repodata.json")
		return s.pullThroughLarge(ctx, p, pub)
	}
	if err != nil {
		return false, err
	}
	var found *remotePackage
	for i := range pkgs {
		if pkgs[i].filename == pub.Version {
			found = &pkgs[i]
		}
	}
	if found == nil {
		return false, artifact.ErrNotFound
	}
	if err := s.fetchVerified(ctx, p, pub, doc.SourceID, found.sha256, found.conda); err != nil {
		return false, err
	}
	// Best effort: keep the verified entry so the owner's packages remain
	// listable while upstream is unavailable (.tar.bz2 only; the local view
	// has no .conda listing).
	if !found.conda {
		if m, e := parseMetadata(found.raw); e == nil && m.Subdir == pub.Namespace {
			mp := pub
			mp.AssetKey, mp.MediaType = "metadata", "application/json"
			_ = s.service.CacheDocument(ctx, p, mp, doc.SourceID, found.raw)
		}
	}
	return true, nil
}

// fetchVerified materializes one package from the source whose repodata named
// it, checked against the repodata sha256 before it is served or cached.
func (s *Repository) fetchVerified(ctx context.Context, p identity.Principal, pub artifact.Publication, source, sum string, conda bool) error {
	pub.MediaType = "application/x-bzip2"
	if conda {
		pub.MediaType = "application/octet-stream"
	}
	stream, again, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: pub.Namespace + "/" + artifact.EscapePath(pub.Version), SourceID: source, Digest: blob.Digest(sum)})
	if err != nil {
		return err
	}
	if !again {
		// Digest is set so the object is normally committed before serving.
		_, _ = io.Copy(io.Discard, stream.Body)
		_ = stream.Body.Close()
	}
	return nil
}
