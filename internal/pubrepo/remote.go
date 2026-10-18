package pubrepo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"golang.org/x/mod/semver"
)

// Pull-through for Pub hosted repositories. The upstream package document
// (api/packages/<name>) is read through the runtime's metadata cache, validated
// and re-rendered with every archive_url pointing back at this feed. Archives are
// fetched on first download from the archive_url the upstream document names,
// verified against archive_sha256 before the first byte is served and cached. The
// pubspec of a cached archive is cached beside it so cached versions stay listable
// during an upstream outage.

const pubMedia = "application/vnd.pub.v2+json"

var hexSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

type remoteVersion struct {
	version, sha, archiveURL string
	item                     map[string]any
}

type remoteDoc struct {
	top      map[string]any
	versions []remoteVersion
	doc      artifact.Document
}

func (s *Repository) remoteDocument(ctx context.Context, p identity.Principal, feed, name, only string) (*remoteDoc, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name}, artifact.Origin{Path: "api/packages/" + name, Representation: pubMedia, SourceID: only})
	if err != nil {
		return nil, err
	}
	var in struct {
		Name     string           `json:"name"`
		Versions []map[string]any `json:"versions"`
	}
	top := map[string]any{}
	if json.Unmarshal(doc.Body, &in) != nil || json.Unmarshal(doc.Body, &top) != nil || in.Name != name {
		return nil, artifact.ErrUnavailable
	}
	out := &remoteDoc{top: top, doc: doc}
	seen := map[string]bool{}
	for _, item := range in.Versions {
		v, _ := item["version"].(string)
		spec, _ := item["pubspec"].(map[string]any)
		ref, _ := item["archive_url"].(string)
		sha, hasSHA := item["archive_sha256"].(string)
		if !validVersion(v) || seen[v] || spec == nil || spec["name"] != name || spec["version"] != v || ref == "" || (hasSHA && !hexSHA.MatchString(sha)) {
			continue
		}
		seen[v] = true
		out.versions = append(out.versions, remoteVersion{version: v, sha: sha, archiveURL: ref, item: item})
		if len(out.versions) > 4096 {
			return nil, artifact.ErrUnavailable
		}
	}
	sort.Slice(out.versions, func(i, j int) bool {
		return semver.Compare("v"+out.versions[i].version, "v"+out.versions[j].version) < 0
	})
	return out, nil
}

func (v remoteVersion) rendered(base, name string) map[string]any {
	item := make(map[string]any, len(v.item))
	for k, x := range v.item {
		item[k] = x
	}
	item["archive_url"] = base + "packages/" + name + "/versions/" + v.version + ".tar.gz"
	return item
}

// serveRemoteIndex answers api/packages/<name>[/versions/<version>] from upstream.
// It returns true when the response was written. A degraded-upstream error is
// returned so the caller can fall back to the locally cached view.
func (s *Repository) serveRemoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, version, base string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	d, err := s.remoteDocument(ctx, p, feed, name, "")
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	if d.doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	if version != "" {
		for _, v := range d.versions {
			if v.version == version {
				writeJSON(w, r, v.rendered(base, name))
				return true, nil
			}
		}
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	if len(d.versions) == 0 {
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	items := make([]any, 0, len(d.versions))
	var latest, last map[string]any
	upstreamLatest := ""
	if l, ok := d.top["latest"].(map[string]any); ok {
		upstreamLatest, _ = l["version"].(string)
	}
	for _, v := range d.versions {
		item := v.rendered(base, name)
		items = append(items, item)
		last = item
		if v.version == upstreamLatest || (upstreamLatest == "" && semver.Prerelease("v"+v.version) == "") {
			latest = item
		}
	}
	if latest == nil {
		latest = last
	}
	out := make(map[string]any, len(d.top))
	for k, v := range d.top {
		out[k] = v
	}
	out["latest"], out["versions"] = latest, items
	writeJSON(w, r, out)
	return true, nil
}

// pullThrough materializes a missing archive. retry is true once the archive is in
// the store; served is true when a streamed response was written.
func (s *Repository) pullThrough(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) (retry, served bool, err error) {
	d, err := s.remoteDocument(r.Context(), p, pub.FeedID, pub.Coordinate, "")
	if err != nil {
		return false, false, err
	}
	var found *remoteVersion
	for i := range d.versions {
		if d.versions[i].version == pub.Version {
			found = &d.versions[i]
		}
	}
	if found == nil {
		return false, false, artifact.ErrNotFound
	}
	path, absolute, e := artifact.ResolveReference("api/packages/"+pub.Coordinate, found.archiveURL)
	if e != nil {
		return false, false, artifact.ErrUnavailable
	}
	pub.MediaType = "application/gzip"
	o := artifact.Origin{Path: path, URL: absolute, SourceID: d.doc.SourceID}
	if found.sha != "" {
		o.Digest = blob.Digest(found.sha)
	}
	stream, again, e := s.service.PullThrough(r.Context(), p, pub, o)
	if e != nil {
		return false, false, e
	}
	if again {
		// Best effort: keep the pubspec so the cached version remains listable offline.
		if spec, e := json.Marshal(found.item["pubspec"]); e == nil && len(spec) <= maxMetadata {
			sp := pub
			sp.AssetKey, sp.MediaType = "pubspec", "application/json"
			_ = s.service.CacheDocument(r.Context(), p, sp, d.doc.SourceID, spec)
		}
		return true, false, nil
	}
	stream.ServeStream(w, r.Method)
	return false, true, nil
}
