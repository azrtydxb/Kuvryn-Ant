package nuget

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for NuGet V3 feeds. The remote source origin is the upstream
// host and its service index is read from v3/index.json (through the runtime's
// metadata cache). The upstream PackageBaseAddress and RegistrationsBaseUrl
// resources are discovered from it, so the client never sees an upstream URL:
// this adapter's own service index already advertises the feed's flat
// container and registration endpoints, and every registration document is
// re-rendered with @id, packageContent and registration URLs rebuilt from the
// feed base. Registration pages that upstream does not inline are fetched and
// inlined (up to the local 4096 version ceiling). Package archives and nuspecs
// are cached into the store on first download. NuGet publishes no sha256 for
// them, so cache fills are length checked but not hash verified.

var (
	flatTypes         = []string{"PackageBaseAddress/3.0.0"}
	registrationTypes = []string{"RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/Versioned", "RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl"}
)

const serviceIndexPath = "v3/index.json"

// resource returns the upstream URL of the first advertised resource whose
// type is in types (in preference order).
func resource(body []byte, types []string) string {
	var doc struct {
		Resources []struct {
			ID   string `json:"@id"`
			Type any    `json:"@type"`
		} `json:"resources"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	for _, want := range types {
		for _, r := range doc.Resources {
			var kinds []string
			switch t := r.Type.(type) {
			case string:
				kinds = []string{t}
			case []any:
				for _, v := range t {
					if s, ok := v.(string); ok {
						kinds = append(kinds, s)
					}
				}
			}
			for _, k := range kinds {
				if k == want && strings.HasPrefix(r.ID, "http") {
					return strings.TrimSuffix(r.ID, "/") + "/"
				}
			}
		}
	}
	return ""
}

// upstream locates an upstream resource of one of the types. The returned
// origin joins suffix to the discovered base; SourceID pins the source that
// served the service index.
func (s *Repository) upstream(ctx context.Context, p identity.Principal, feed, id, only string, types []string, suffix string) (artifact.Origin, string, error) {
	pub := artifact.Publication{FeedID: feed, Coordinate: id}
	doc, err := s.service.RemoteMetadata(ctx, p, pub, artifact.Origin{Path: serviceIndexPath, SourceID: only})
	if err != nil {
		return artifact.Origin{}, "", err
	}
	base := resource(doc.Body, types)
	if base == "" {
		return artifact.Origin{}, "", artifact.ErrUnavailable
	}
	path, absolute, err := artifact.ResolveReference(serviceIndexPath, base+suffix)
	if err != nil {
		return artifact.Origin{}, "", artifact.ErrUnavailable
	}
	return artifact.Origin{Path: path, URL: absolute, SourceID: doc.SourceID}, base, nil
}

func (s *Repository) remoteDoc(ctx context.Context, p identity.Principal, feed, id string, types []string, suffix string) (artifact.Document, string, error) {
	origin, base, err := s.upstream(ctx, p, feed, id, "", types, suffix)
	if err != nil {
		return artifact.Document{}, "", err
	}
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: id}, origin)
	if errors.Is(err, artifact.ErrNotFound) {
		return doc, base, artifact.ErrNotFound
	}
	return doc, base, err
}

func markStale(w http.ResponseWriter, doc artifact.Document) {
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
}

// remoteVersions serves the flat-container version list from upstream.
// It returns true once the response was written; a non-nil error is the
// degraded upstream failure the caller may fall back from.
func (s *Repository) remoteVersions(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	doc, _, err := s.remoteDoc(ctx, p, feed, id, flatTypes, url.PathEscape(id)+"/index.json")
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	var up struct {
		Versions []string `json:"versions"`
	}
	if json.Unmarshal(doc.Body, &up) != nil {
		return false, artifact.ErrUnavailable
	}
	versions := []string{}
	seen := map[string]bool{}
	for _, v := range up.Versions {
		n, e := normalizeVersion(v)
		if e != nil || seen[n] {
			continue
		}
		seen[n] = true
		versions = append(versions, n)
		if len(versions) > 4096 {
			failure(w, artifact.ErrUnavailable)
			return true, nil
		}
	}
	if len(versions) == 0 {
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	markStale(w, doc)
	writeJSON(w, r, map[string]any{"versions": versions})
	return true, nil
}

type regItem struct {
	version string
	listed  any
	catalog map[string]any
}

// remoteRegistration fetches the upstream registration index of id and
// returns its validated items sorted by version.
func (s *Repository) remoteRegistration(ctx context.Context, p identity.Principal, feed, id string) ([]regItem, artifact.Document, error) {
	doc, base, err := s.remoteDoc(ctx, p, feed, id, registrationTypes, url.PathEscape(id)+"/index.json")
	if err != nil {
		return nil, doc, err
	}
	var index struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(doc.Body, &index) != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	var items []regItem
	seen := map[string]bool{}
	add := func(raw any) {
		item, _ := raw.(map[string]any)
		catalog, _ := item["catalogEntry"].(map[string]any)
		cid, _ := catalog["id"].(string)
		cv, _ := catalog["version"].(string)
		v, e := normalizeVersion(cv)
		if e != nil || !strings.EqualFold(cid, id) || seen[v] {
			return
		}
		seen[v] = true
		items = append(items, regItem{version: v, listed: catalog["listed"], catalog: catalog})
	}
	fetched := 0
	for _, page := range index.Items {
		list, inline := page["items"].([]any)
		if !inline {
			pageURL, _ := page["@id"].(string)
			rest, ok := strings.CutPrefix(pageURL, base)
			if !ok || rest == "" || strings.ContainsAny(rest, "?#\\") || strings.Contains(rest, "..") || fetched >= 256 {
				return nil, doc, artifact.ErrUnavailable
			}
			fetched++
			origin, _, e := s.upstream(ctx, p, feed, id, doc.SourceID, registrationTypes, rest)
			if e != nil {
				return nil, doc, artifact.ErrUnavailable
			}
			pd, e := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: id}, origin)
			if e != nil {
				return nil, doc, artifact.ErrUnavailable
			}
			doc.Stale = doc.Stale || pd.Stale
			var sub struct {
				Items []any `json:"items"`
			}
			if json.Unmarshal(pd.Body, &sub) != nil {
				return nil, doc, artifact.ErrUnavailable
			}
			list = sub.Items
		}
		for _, raw := range list {
			add(raw)
			if len(items) > 4096 {
				return nil, doc, artifact.ErrUnavailable
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return versionLess(items[i].version, items[j].version) })
	return items, doc, nil
}

func (it regItem) render(base, id string) map[string]any {
	leaf := base + "v3/registration/" + id + "/" + it.version + ".json"
	catalog := map[string]any{}
	for k, v := range it.catalog {
		catalog[k] = v
	}
	catalog["@id"] = leaf + "#catalog"
	catalog["packageContent"] = contentURL(base, id, it.version)
	return map[string]any{"@id": leaf, "catalogEntry": catalog, "packageContent": contentURL(base, id, it.version), "registration": base + "v3/registration/" + id + "/index.json"}
}

// serveRemoteRegistration answers a registration index (leaf == "") or leaf.
func (s *Repository) serveRemoteRegistration(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id, leaf, base string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	items, doc, err := s.remoteRegistration(ctx, p, feed, id)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	markStale(w, doc)
	if leaf != "" {
		for _, it := range items {
			if it.version == leaf {
				rendered := it.render(base, id)
				listed := it.listed
				if listed == nil {
					listed = true
				}
				writeJSON(w, r, map[string]any{"@id": rendered["@id"], "listed": listed, "packageContent": rendered["packageContent"], "registration": rendered["registration"]})
				return true, nil
			}
		}
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	if len(items) == 0 {
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	rendered := make([]any, 0, len(items))
	total := 0
	for _, it := range items {
		x := it.render(base, id)
		encoded, _ := json.Marshal(x)
		if total += len(encoded); total > 15<<20 {
			failure(w, artifact.ErrUnavailable)
			return true, nil
		}
		rendered = append(rendered, x)
	}
	pages := []any{}
	for start := 0; start < len(rendered); start += 64 {
		end := min(start+64, len(rendered))
		pages = append(pages, map[string]any{"@id": base + "v3/registration/" + id + "/index.json#page-" + strconv.Itoa(start/64), "count": end - start, "lower": items[start].version, "upper": items[end-1].version, "items": rendered[start:end]})
	}
	writeJSON(w, r, map[string]any{"@id": base + "v3/registration/" + id + "/index.json", "count": len(pages), "items": pages})
	return true, nil
}

// pullThrough materializes a missing nupkg or nuspec. It returns served=true
// when the response was written, retry=true when the asset is now in the store.
func (s *Repository) pullThrough(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) (retry, served bool, err error) {
	file := pub.Coordinate + ".nuspec"
	pub.MediaType = "text/xml"
	if pub.AssetKey == "nupkg" {
		file = pub.Coordinate + "." + pub.Version + ".nupkg"
		pub.MediaType = "application/octet-stream"
	}
	origin, _, err := s.upstream(r.Context(), p, pub.FeedID, pub.Coordinate, "", flatTypes, url.PathEscape(pub.Coordinate)+"/"+url.PathEscape(pub.Version)+"/"+url.PathEscape(file))
	if err != nil {
		return false, false, err
	}
	stream, again, err := s.service.PullThrough(r.Context(), p, pub, origin)
	if err != nil {
		return false, false, err
	}
	if again {
		return true, false, nil
	}
	stream.ServeStream(w, r.Method)
	return false, true, nil
}
