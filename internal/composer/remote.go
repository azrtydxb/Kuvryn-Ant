package composer

import (
	"archive/zip"
	"context"
	"crypto/sha1" // Composer dist.shasum is SHA-1; used only when the upstream states one.
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Composer v2 repositories (for example repo.packagist.org).
// The metadata template comes from the upstream `packages.json` (`metadata-url`,
// default `/p2/%package%.json`). `p2/<vendor>/<pkg>.json` and the `~dev` file are
// read through the runtime's metadata cache, expanded from Composer's minified
// form, and re-rendered without `source` (a VCS URL would send clients around
// the feed) and with every `dist.url` pointing at this feed. The feed's
// `dist/<id>/<version>/archive.zip` resolves the upstream `dist.url` again from
// the cached metadata, so the client never chooses the upstream URL. Packagist
// publishes no archive digest (`shasum` is empty), so the archive is checked to
// be a well-formed ZIP of bounded size and, when an upstream states a SHA-1
// `shasum`, against it. Dist URLs usually live on another host (GitHub
// `api.github.com`, redirecting to `codeload.github.com`): add those origins to
// the source's redirect origins; the source credential is never sent there.
// Each cached archive keeps its verified version entry (asset `remote-metadata`,
// ignored by the local composition) so the owner's versions stay listable during
// an outage.

var remoteVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

type remoteEntry struct {
	version string
	fields  map[string]json.RawMessage
	dist    string // upstream dist url
	sha1    string
}

// expand reverses Composer's metadata minification.
func expand(versions []map[string]json.RawMessage) []map[string]json.RawMessage {
	out := make([]map[string]json.RawMessage, 0, len(versions))
	var current map[string]json.RawMessage
	for _, v := range versions {
		next := map[string]json.RawMessage{}
		for k, val := range current {
			next[k] = val
		}
		if current == nil {
			next = v
		} else {
			for k, val := range v {
				if string(val) == `"__unset"` {
					delete(next, k)
				} else {
					next[k] = val
				}
			}
		}
		current = next
		out = append(out, next)
	}
	return out
}

func isDevVersion(v string) bool { return strings.HasPrefix(v, "dev-") || strings.HasSuffix(v, "-dev") }

// upstreamMetadata fetches one p2 document, honouring the upstream metadata-url.
func (s *Repository) upstreamMetadata(ctx context.Context, p identity.Principal, feed, id string, dev bool) (artifact.Document, error) {
	pub := artifact.Publication{FeedID: feed, Namespace: strings.Split(id, "/")[0], Coordinate: id}
	name := id
	if dev {
		name += "~dev"
	}
	path, absolute, source := "p2/"+artifact.EscapePath(name)+".json", "", ""
	root, err := s.service.RemoteMetadata(ctx, p, pub, artifact.Origin{Path: "packages.json", Representation: "application/json"})
	switch {
	case err == nil:
		var cfg struct {
			MetadataURL string `json:"metadata-url"`
		}
		if json.Unmarshal(root.Body, &cfg) == nil && strings.Contains(cfg.MetadataURL, "%package%") {
			if pth, abs, e := artifact.ResolveReference("packages.json", strings.ReplaceAll(cfg.MetadataURL, "%package%", artifact.EscapePath(name))); e == nil {
				path, absolute = pth, abs
			}
		}
		source = root.SourceID
	case errors.Is(err, artifact.ErrNotFound):
	default:
		return artifact.Document{}, err
	}
	return s.service.RemoteMetadata(ctx, p, pub, artifact.Origin{Path: path, URL: absolute, Representation: "application/json", SourceID: source})
}

// remoteEntries returns the validated, expanded version entries of one document.
func (s *Repository) remoteEntries(ctx context.Context, p identity.Principal, feed, id string, dev bool) ([]remoteEntry, artifact.Document, error) {
	doc, err := s.upstreamMetadata(ctx, p, feed, id, dev)
	if err != nil {
		return nil, doc, err
	}
	var in struct {
		Packages map[string][]map[string]json.RawMessage `json:"packages"`
	}
	if json.Unmarshal(doc.Body, &in) != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	var out []remoteEntry
	seen := map[string]bool{}
	for _, f := range expand(in.Packages[id]) {
		var e struct {
			Name, Version string
			Dist          *struct{ Type, URL, Shasum string }
		}
		raw, _ := json.Marshal(f)
		if json.Unmarshal(raw, &e) != nil || e.Name != id || !remoteVersionRE.MatchString(e.Version) || seen[e.Version] || isDevVersion(e.Version) != dev {
			continue
		}
		seen[e.Version] = true
		entry := remoteEntry{version: e.Version, fields: f}
		if e.Dist != nil && e.Dist.Type == "zip" && e.Dist.URL != "" {
			entry.dist = e.Dist.URL
			if len(e.Dist.Shasum) == 40 {
				entry.sha1 = strings.ToLower(e.Dist.Shasum)
			}
		}
		out = append(out, entry)
		if len(out) > 4096 {
			return nil, doc, artifact.ErrUnavailable
		}
	}
	return out, doc, nil
}

// rewrite renders one entry for the client: no VCS source, dist via this feed.
func rewrite(f map[string]json.RawMessage, id, version, base string, hasDist bool) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range f {
		if k != "source" && k != "dist" {
			out[k] = v
		}
	}
	if hasDist {
		reference := ""
		var d struct{ Reference string }
		if raw, ok := f["dist"]; ok && json.Unmarshal(raw, &d) == nil {
			reference = d.Reference
		}
		out["dist"], _ = json.Marshal(map[string]string{"type": "zip", "url": base + "dist/" + id + "/" + version + "/archive.zip", "reference": reference, "shasum": ""})
	}
	return out
}

// serveRemoteIndex answers a p2 request from upstream. It returns true once the
// response was written; a degraded upstream error is returned so the caller can
// fall back to the locally cached view.
func (s *Repository) serveRemoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id, base string, dev bool) (bool, error) {
	entries, doc, err := s.remoteEntries(r.Context(), p, feed, id, dev)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	items := []any{}
	for _, e := range entries {
		items = append(items, rewrite(e.fields, id, e.version, base, e.dist != ""))
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	writeJSON(w, r, map[string]any{"packages": map[string]any{id: items}})
	return true, nil
}

// offlineIndex lists the versions of a remote-owned package that were cached
// earlier, for use while the upstream is unavailable.
func (s *Repository) offlineIndex(ctx context.Context, p identity.Principal, feed, id, base string, dev bool) ([]any, error) {
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='remote-metadata' AND o.namespace=$4 ORDER BY v.version LIMIT 1025`, p.InstanceID, feed, id, strings.Split(id, "/")[0])
	if e != nil {
		return nil, e
	}
	type entry struct{ source, version string }
	var list []entry
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.source, &v.version); e != nil {
			break
		}
		list = append(list, v)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(list) > 1024 {
		return nil, artifact.ErrUnavailable
	}
	items := []any{}
	for _, v := range list {
		if isDevVersion(v.version) != dev {
			continue
		}
		rec, b, e := s.service.Open(ctx, p, artifact.Publication{FeedID: feed, Namespace: strings.Split(id, "/")[0], Coordinate: id, Version: v.version, AssetKey: "remote-metadata"}, 0, -1)
		if e != nil {
			return nil, e
		}
		if rec.SourceID != v.source || rec.Info.Size > maxMetadata {
			b.Close()
			return nil, artifact.ErrUnavailable
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, b}, maxMetadata+1))
		b.Close()
		var f map[string]json.RawMessage
		if e != nil || json.Unmarshal(data, &f) != nil {
			return nil, artifact.ErrUnavailable
		}
		items = append(items, rewrite(f, id, v.version, base, true))
	}
	return items, nil
}

func zipVerify(sha1hex string) func(io.ReadSeeker) error {
	return func(rs io.ReadSeeker) error {
		size, err := rs.Seek(0, io.SeekEnd)
		if err != nil || size <= 0 || size > maxPackage {
			return errInvalid
		}
		if sha1hex != "" {
			if _, err = rs.Seek(0, io.SeekStart); err != nil {
				return errInvalid
			}
			h := sha1.New()
			if _, err = io.Copy(h, rs); err != nil || hex.EncodeToString(h.Sum(nil)) != sha1hex {
				return errInvalid
			}
		}
		ra, ok := rs.(io.ReaderAt)
		if !ok {
			return errInvalid
		}
		_, err = zip.NewReader(ra, size)
		if err != nil {
			return errInvalid
		}
		return nil
	}
}

// pullThrough materializes a missing dist archive. retry=true means the archive
// is now in the store.
func (s *Repository) pullThrough(ctx context.Context, p identity.Principal, pub artifact.Publication) (bool, error) {
	if !namePattern.MatchString(pub.Coordinate) || !remoteVersionRE.MatchString(pub.Version) {
		return false, artifact.ErrNotFound
	}
	dev := isDevVersion(pub.Version)
	entries, doc, err := s.remoteEntries(ctx, p, pub.FeedID, pub.Coordinate, dev)
	if err != nil {
		return false, err
	}
	var found *remoteEntry
	for i := range entries {
		if entries[i].version == pub.Version {
			found = &entries[i]
		}
	}
	if found == nil || found.dist == "" {
		return false, artifact.ErrNotFound
	}
	path, absolute, err := artifact.ResolveReference("p2/"+pub.Coordinate+".json", found.dist)
	if err != nil {
		return false, artifact.ErrUnavailable
	}
	pub.MediaType = "application/zip"
	stream, _, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: path, URL: absolute, SourceID: doc.SourceID, Verify: zipVerify(found.sha1)})
	if err != nil {
		return false, err
	}
	if stream != nil {
		_, _ = io.Copy(io.Discard, stream.Body)
		_ = stream.Body.Close()
	}
	// Best effort: keep the verified entry so the owner's versions remain
	// listable while upstream is unavailable.
	if raw, e := json.Marshal(found.fields); e == nil && len(raw) <= maxMetadata {
		mp := pub
		mp.AssetKey, mp.MediaType = "remote-metadata", "application/json"
		_ = s.service.CacheDocument(ctx, p, mp, doc.SourceID, raw)
	}
	return true, nil
}
