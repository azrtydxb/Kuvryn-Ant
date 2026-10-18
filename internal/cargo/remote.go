package cargo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Cargo sparse registries. The upstream sparse index file of a
// crate is fetched through the runtime's metadata cache (never persisted as a
// mutable document), validated line by line and served to the client verbatim:
// index lines carry no registry URLs, and the client learns where to download
// from this feed's own config.json (dl/api point back at the feed). A crate
// archive is fetched on first download from the upstream `dl` template of the
// source's config.json, verified against the sha256 `cksum` of its index
// line, and cached into the store. The verified index line is cached beside
// it so the owner's versions can still be listed during an upstream outage.

// remoteNameRE is the read-side spelling accepted for crates.io style names
// (case-insensitive, underscores allowed). Local publication keeps nameRE.
var remoteNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

var checksumRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type remoteVersion struct {
	version, cksum string
	line           []byte
}

// remoteIndex fetches and validates the upstream index file of crate name.
func (s *Registry) remoteIndex(ctx context.Context, p identity.Principal, feed, name, only string) ([]remoteVersion, artifact.Document, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: strings.ToLower(name)}, artifact.Origin{Path: indexPath(name), Representation: "text/plain", SourceID: only})
	if err != nil {
		return nil, doc, err
	}
	var versions []remoteVersion
	seen := map[string]bool{}
	for _, line := range bytes.Split(doc.Body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var v struct {
			Name  string `json:"name"`
			Vers  string `json:"vers"`
			Cksum string `json:"cksum"`
		}
		if json.Unmarshal(line, &v) != nil || !strings.EqualFold(v.Name, name) || !validVersion(v.Vers) || !checksumRE.MatchString(v.Cksum) || seen[versionKey(v.Vers)] {
			continue
		}
		seen[versionKey(v.Vers)] = true
		versions = append(versions, remoteVersion{version: v.Vers, cksum: v.Cksum, line: append([]byte(nil), line...)})
		if len(versions) > 10000 {
			return nil, doc, artifact.ErrUnavailable
		}
	}
	return versions, doc, nil
}

// serveRemoteIndex answers a sparse index request from upstream. It returns
// true once the response was written. A degraded upstream error is returned so
// the caller can fall back to the locally cached view.
func (s *Registry) serveRemoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	versions, doc, err := s.remoteIndex(ctx, p, feed, name, "")
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failService(w, err)
		return true, nil
	}
	if len(versions) == 0 {
		failure(w, 404, "crate not found")
		return true, nil
	}
	var out bytes.Buffer
	for _, v := range versions {
		out.Write(v.line)
		out.WriteByte('\n')
	}
	sum := sha256.Sum256(out.Bytes())
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization")
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return true, nil
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(out.Len()))
	if r.Method != "HEAD" {
		_, _ = w.Write(out.Bytes())
	}
	return true, nil
}

func prefix(name string) string {
	switch len(name) {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3/" + name[:1]
	}
	return name[:2] + "/" + name[2:4]
}

// downloadReference expands the upstream config.json `dl` template.
func downloadReference(dl, name, version, cksum string) string {
	if !strings.Contains(dl, "{") {
		return strings.TrimSuffix(dl, "/") + "/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/download"
	}
	return strings.NewReplacer("{crate}", url.PathEscape(name), "{version}", url.PathEscape(version), "{prefix}", prefix(name), "{lowerprefix}", strings.ToLower(prefix(name)), "{sha256-checksum}", cksum).Replace(dl)
}

// pullThrough materializes a missing crate archive. It returns served=true
// when the response was written and retry=true once the crate is in the store.
func (s *Registry) pullThrough(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, name, version string) (retry, served bool, err error) {
	versions, doc, err := s.remoteIndex(r.Context(), p, pub.FeedID, name, "")
	if err != nil {
		return false, false, err
	}
	var found *remoteVersion
	for i := range versions {
		if versions[i].version == version {
			found = &versions[i]
		}
	}
	if found == nil {
		return false, false, artifact.ErrNotFound
	}
	cfg, err := s.service.RemoteMetadata(r.Context(), p, artifact.Publication{FeedID: pub.FeedID, Coordinate: pub.Coordinate}, artifact.Origin{Path: "config.json", SourceID: doc.SourceID})
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			err = artifact.ErrUnavailable
		}
		return false, false, err
	}
	var c struct {
		DL string `json:"dl"`
	}
	if json.Unmarshal(cfg.Body, &c) != nil || c.DL == "" {
		return false, false, artifact.ErrUnavailable
	}
	path, absolute, e := artifact.ResolveReference("config.json", downloadReference(c.DL, name, version, found.cksum))
	if e != nil {
		return false, false, artifact.ErrUnavailable
	}
	pub.MediaType = "application/octet-stream"
	stream, again, e := s.service.PullThrough(r.Context(), p, pub, artifact.Origin{Path: path, URL: absolute, SourceID: doc.SourceID, Digest: blob.Digest(found.cksum)})
	if e != nil {
		return false, false, e
	}
	if again {
		// Best effort: keep the verified index line so the owner's versions
		// remain listable while upstream is unavailable.
		ip := pub
		ip.AssetKey, ip.MediaType = "index", "application/json"
		_ = s.service.CacheDocument(r.Context(), p, ip, doc.SourceID, found.line)
		return true, false, nil
	}
	stream.ServeStream(w, r.Method)
	return false, true, nil
}
