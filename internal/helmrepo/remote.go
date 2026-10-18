package helmrepo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"golang.org/x/mod/semver"
)

// Pull-through for classic Helm repositories. The upstream index.yaml is fetched
// through the runtime's metadata cache (never persisted), reduced to the
// charts this adapter would itself accept, and merged into the feed index with
// every chart URL rewritten to this feed's pinned route. A chart whose package
// owner is a local source is never taken from upstream. Chart archives (and
// .prov files) are cached into the store on first download.

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

const indexPath = "index.yaml"

// remoteChart is one servable upstream chart version.
type remoteChart struct {
	name, version, digest string
	urls                  []string
	meta                  map[string]any
}

// remoteCharts returns the validated charts of the upstream index of the first
// remote source that has one (restricted to only when non-empty). coordinate
// selects the ownership check: the chart name for downloads, indexPath for the
// whole feed.
func (h *Repository) remoteCharts(ctx context.Context, p identity.Principal, feed, coordinate, only string) ([]remoteChart, artifact.Document, error) {
	doc, err := h.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: coordinate}, artifact.Origin{Path: indexPath, SourceID: only})
	if err != nil {
		return nil, doc, err
	}
	var up struct {
		Entries map[string][]map[string]any `yaml:"entries"`
	}
	if decodeYAML(doc.Body, &up) != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	var charts []remoteChart
	for name, versions := range up.Entries {
		for _, meta := range versions {
			version, _ := meta["version"].(string)
			if n, _ := meta["name"].(string); n != name || !validCoordinate(name, version) {
				continue
			}
			c := remoteChart{name: name, version: version, meta: meta}
			if d, _ := meta["digest"].(string); sha256Re.MatchString(d) {
				c.digest = d
			}
			if list, ok := meta["urls"].([]any); ok {
				for _, v := range list {
					if s, ok := v.(string); ok {
						c.urls = append(c.urls, s)
					}
				}
			}
			if len(c.urls) == 0 {
				continue
			}
			charts = append(charts, c)
			if len(charts) > h.Limits.IndexEntries {
				return nil, doc, artifact.ErrUnavailable
			}
		}
	}
	sort.Slice(charts, func(i, j int) bool {
		if charts[i].name != charts[j].name {
			return charts[i].name < charts[j].name
		}
		return charts[i].version < charts[j].version
	})
	return charts, doc, nil
}

// localOwned returns the chart names whose package owner in the feed is a
// local source; those charts are never completed from upstream.
func (h *Repository) localOwned(ctx context.Context, p identity.Principal, feed string) (map[string]bool, error) {
	rows, err := h.db.Query(ctx, `SELECT DISTINCT ON(c.coordinate) c.coordinate,s.kind FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id JOIN sources s ON s.id=c.source_id AND s.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 ORDER BY c.coordinate,fs.priority`, p.InstanceID, feed)
	if err != nil {
		return nil, artifact.ErrUnavailable
	}
	defer rows.Close()
	owned := map[string]bool{}
	for rows.Next() {
		var name, kind string
		if err = rows.Scan(&name, &kind); err != nil {
			return nil, artifact.ErrUnavailable
		}
		owned[name] = kind != "remote"
	}
	if rows.Err() != nil {
		return nil, artifact.ErrUnavailable
	}
	return owned, nil
}

func chartURL(c remoteChart, source string) string {
	file := url.PathEscape(c.name + "-" + c.version + ".tgz")
	base := "charts/" + url.PathEscape(c.name) + "/" + url.PathEscape(c.version) + "/"
	if c.digest == "" {
		return base + file
	}
	return base + source + "/" + c.digest + "/" + file
}

// mergeRemote adds the upstream charts to idx. It returns ErrNotFound when no
// remote source applies and ErrUnavailable when upstream failed with nothing
// cached (the caller keeps the local view).
func (h *Repository) mergeRemote(ctx context.Context, p identity.Principal, feed string, idx *indexFile) (artifact.Document, error) {
	charts, doc, err := h.remoteCharts(ctx, p, feed, indexPath, "")
	if err != nil {
		return doc, err
	}
	owned, err := h.localOwned(ctx, p, feed)
	if err != nil {
		return doc, err
	}
	for _, c := range charts {
		if owned[c.name] {
			continue
		}
		meta := map[string]any{}
		for k, v := range c.meta {
			meta[k] = v
		}
		meta["urls"] = []string{chartURL(c, doc.SourceID)}
		if c.digest == "" {
			delete(meta, "digest")
		}
		idx.Entries[c.name] = append(idx.Entries[c.name], meta)
	}
	for _, entries := range idx.Entries {
		sort.SliceStable(entries, func(i, j int) bool {
			return semver.Compare("v"+entries[i]["version"].(string), "v"+entries[j]["version"].(string)) > 0
		})
	}
	return doc, nil
}

// pullThrough materializes a missing chart (or its .prov) from upstream. It
// returns served=true when the response was written, retry=true when the asset
// is now in the store, or the error to report.
func (h *Repository) pullThrough(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, filename, source, digest string) (retry, served bool, err error) {
	charts, doc, err := h.remoteCharts(r.Context(), p, pub.FeedID, pub.Coordinate, source)
	if err != nil {
		return false, false, err
	}
	var found *remoteChart
	for i := range charts {
		c := &charts[i]
		if c.name == pub.Coordinate && c.version == pub.Version && (digest == "" || c.digest == digest) {
			found = c
		}
	}
	if found == nil {
		return false, false, artifact.ErrNotFound
	}
	source = doc.SourceID // bytes come from the source that served the metadata
	chartPub := pub
	chartPub.AssetKey = pub.Coordinate + "-" + pub.Version + ".tgz"
	chartPub.MediaType = "application/gzip"
	target := chartPub
	suffix := ""
	if filename != chartPub.AssetKey {
		suffix = ".prov"
		target.AssetKey = filename
		target.MediaType = "text/plain"
	}
	lastErr := artifact.ErrNotFound
	for _, ref := range found.urls {
		path, absolute, e := artifact.ResolveReference(indexPath, ref)
		if e != nil {
			lastErr = artifact.ErrUnavailable
			continue
		}
		origin := artifact.Origin{Path: path, URL: absolute, SourceID: source}
		if suffix == "" {
			origin.Digest = blob.Digest(found.digest)
		} else {
			// Provenance is bound to its chart: make sure the (digest verified)
			// chart is in the store first.
			if _, e := h.service.Lookup(r.Context(), p, chartPub); errors.Is(e, artifact.ErrNotFound) {
				co := origin
				co.Digest = blob.Digest(found.digest)
				m, _, e := h.service.PullThrough(r.Context(), p, chartPub, co)
				if e != nil {
					return false, false, e
				}
				if m != nil { // unverified stream: drain so the cache commits on EOF
					_, e = io.Copy(io.Discard, m.Body)
					_ = m.Body.Close()
					if e != nil {
						return false, false, artifact.ErrUnavailable
					}
				}
			}
			if origin.Path != "" {
				origin.Path += suffix
			} else {
				u, e := url.Parse(origin.URL)
				if e != nil {
					return false, false, artifact.ErrUnavailable
				}
				u.Path += suffix
				origin.URL = u.String()
			}
		}
		stream, again, e := h.service.PullThrough(r.Context(), p, target, origin)
		switch {
		case e == nil && again:
			return true, false, nil
		case e == nil:
			stream.ServeStream(w, r.Method)
			return false, true, nil
		case errors.Is(e, artifact.ErrNotFound):
			lastErr = e
		default:
			return false, false, e
		}
	}
	return false, false, lastErr
}
