package ansiblerepo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"golang.org/x/mod/semver"
)

// Pull-through for Galaxy v3 upstreams (galaxy.ansible.com style). The source
// origin is the API root that serves v3/ (for example https://galaxy.ansible.com/api/).
//
// Collection detail, the (fully paginated) versions list and per-version detail are
// read through the metadata cache and re-rendered with every href and download_url
// pointing back at this feed; upstream pagination links are consumed, never passed
// on. A collection archive is fetched on first download from the download_url of
// the upstream version detail (absolute URLs on other hosts need that host as a
// redirect origin), verified against artifact.sha256 before the first byte is served
// and cached together with a metadata asset, so cached versions stay listable during
// an upstream outage. A collection owned by a local source is never answered from
// upstream.

const maxRemotePages = 50

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func collectionPath(ns, name string) string { return "v3/collections/" + ns + "/" + name + "/" }

func (s *Repository) remoteDoc(ctx context.Context, p identity.Principal, feed, ns, name string, o artifact.Origin) (artifact.Document, error) {
	return s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Namespace: ns, Coordinate: ns + "." + name}, o)
}

// remoteVersions collects every page of the upstream version list.
func (s *Repository) remoteVersions(ctx context.Context, p identity.Principal, feed, ns, name string) ([]map[string]any, artifact.Document, error) {
	path := collectionPath(ns, name) + "versions/"
	first, err := s.remoteDoc(ctx, p, feed, ns, name, artifact.Origin{Path: path})
	if err != nil {
		return nil, first, err
	}
	var items []map[string]any
	seen := map[string]bool{}
	doc := first
	var origin *url.URL
	for page := 0; ; page++ {
		var in struct {
			Data  []map[string]any `json:"data"`
			Links struct {
				Next *string `json:"next"`
			} `json:"links"`
		}
		if json.Unmarshal(doc.Body, &in) != nil {
			return nil, first, artifact.ErrUnavailable
		}
		for _, item := range in.Data {
			v, _ := item["version"].(string)
			if !validVersion(v) || seen[v] {
				continue
			}
			seen[v] = true
			items = append(items, item)
			if href, _ := item["href"].(string); origin == nil && strings.HasPrefix(href, "https://") {
				origin, _ = url.Parse(href)
			}
		}
		if len(items) > maxVersions {
			return nil, first, artifact.ErrUnavailable
		}
		if in.Links.Next == nil || *in.Links.Next == "" || page+1 >= maxRemotePages {
			break
		}
		next, e := url.Parse(*in.Links.Next)
		if e != nil {
			break
		}
		if !next.IsAbs() && origin != nil {
			next = origin.ResolveReference(next)
		}
		// A relative link cannot be followed without a known origin.
		if next.Scheme != "https" {
			break
		}
		if doc, err = s.remoteDoc(ctx, p, feed, ns, name, artifact.Origin{URL: next.String(), SourceID: first.SourceID}); err != nil {
			return nil, first, err
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return semver.Compare("v"+items[i]["version"].(string), "v"+items[j]["version"].(string)) > 0
	})
	return items, first, nil
}

// remoteVersion reads and validates the upstream detail of one version.
func (s *Repository) remoteVersion(ctx context.Context, p identity.Principal, feed, ns, name, version, only string) (map[string]any, artifact.Document, error) {
	doc, err := s.remoteDoc(ctx, p, feed, ns, name, artifact.Origin{Path: collectionPath(ns, name) + "versions/" + version + "/", SourceID: only})
	if err != nil {
		return nil, doc, err
	}
	var d map[string]any
	if json.Unmarshal(doc.Body, &d) != nil || d["version"] != version {
		return nil, doc, artifact.ErrUnavailable
	}
	if n, _ := d["namespace"].(map[string]any); n == nil || n["name"] != ns {
		return nil, doc, artifact.ErrUnavailable
	}
	if c, _ := d["collection"].(map[string]any); c == nil || c["name"] != name {
		return nil, doc, artifact.ErrUnavailable
	}
	if a, _ := d["artifact"].(map[string]any); a != nil {
		if sha, ok := a["sha256"].(string); ok && !sha256Pattern.MatchString(sha) {
			return nil, doc, artifact.ErrUnavailable
		}
	}
	return d, doc, nil
}

func setStale(w http.ResponseWriter, doc artifact.Document) {
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
}

// serveRemote answers the collection routes from upstream. It returns true when the
// response was written; a degraded-upstream error is returned for the local fallback.
func (s *Repository) serveRemote(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, ns, name, version, base string, info bool) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	self := base + collectionPath(ns, name)
	switch {
	case version != "":
		d, doc, err := s.remoteVersion(ctx, p, feed, ns, name, version, "")
		if served, e := remoteOutcome(w, err); served || e != nil {
			return served, e
		}
		dl := downloadURL(base, ns, name, version)
		d["href"], d["download_url"] = self+"versions/"+version+"/", dl
		if c, _ := d["collection"].(map[string]any); c != nil {
			c["href"] = self
		}
		if a, _ := d["artifact"].(map[string]any); a != nil {
			a["download_url"] = dl
		}
		setStale(w, doc)
		writeJSON(w, r, d)
		return true, nil
	case info:
		doc, err := s.remoteDoc(ctx, p, feed, ns, name, artifact.Origin{Path: collectionPath(ns, name)})
		if served, e := remoteOutcome(w, err); served || e != nil {
			return served, e
		}
		var d map[string]any
		if json.Unmarshal(doc.Body, &d) != nil || d["name"] != name {
			failure(w, artifact.ErrUnavailable)
			return true, nil
		}
		d["href"], d["versions_url"] = self, self+"versions/"
		if h, _ := d["highest_version"].(map[string]any); h != nil {
			if v, _ := h["version"].(string); validVersion(v) {
				h["href"] = self + "versions/" + v + "/"
			} else {
				delete(d, "highest_version")
			}
		}
		setStale(w, doc)
		writeJSON(w, r, d)
		return true, nil
	default:
		items, doc, err := s.remoteVersions(ctx, p, feed, ns, name)
		if served, e := remoteOutcome(w, err); served || e != nil {
			return served, e
		}
		if len(items) == 0 {
			failure(w, artifact.ErrNotFound)
			return true, nil
		}
		for _, item := range items {
			v := item["version"].(string)
			item["href"], item["download_url"] = self+"versions/"+v+"/", downloadURL(base, ns, name, v)
		}
		setStale(w, doc)
		writeJSON(w, r, map[string]any{
			"meta":  map[string]any{"count": len(items)},
			"links": map[string]any{"first": nil, "previous": nil, "next": nil, "last": nil},
			"data":  items,
		})
		return true, nil
	}
}

// remoteOutcome maps a metadata error: (false, nil) continues locally, (false, err)
// continues locally with the degraded flag, (true, nil) means the error was reported.
func remoteOutcome(w http.ResponseWriter, err error) (bool, error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, artifact.ErrNotFound):
		return false, errNoRemote
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	}
	failure(w, err)
	return true, nil
}

var errNoRemote = errors.New("no remote owner")

// pullThrough materializes a missing archive. retry is true once it is in the
// store; served is true when a streamed response was written.
func (s *Repository) pullThrough(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) (retry, served bool, err error) {
	ns, name, _ := strings.Cut(pub.Coordinate, ".")
	d, doc, err := s.remoteVersion(r.Context(), p, pub.FeedID, ns, name, pub.Version, "")
	if err != nil {
		return false, false, err
	}
	ref, _ := d["download_url"].(string)
	if a, _ := d["artifact"].(map[string]any); a != nil && ref == "" {
		ref, _ = a["download_url"].(string)
	}
	path, absolute, e := artifact.ResolveReference(collectionPath(ns, name)+"versions/"+pub.Version+"/", ref)
	if ref == "" || e != nil {
		return false, false, artifact.ErrUnavailable
	}
	o := artifact.Origin{Path: path, URL: absolute, SourceID: doc.SourceID}
	if a, _ := d["artifact"].(map[string]any); a != nil {
		if sha, _ := a["sha256"].(string); sha != "" {
			o.Digest = blob.Digest(sha)
		}
	}
	pub.MediaType = "application/gzip"
	stream, again, e := s.service.PullThrough(r.Context(), p, pub, o)
	if e != nil {
		return false, false, e
	}
	if !again {
		stream.ServeStream(w, r.Method)
		return false, true, nil
	}
	// Best effort: cache the metadata asset so the version stays listable offline.
	deps := map[string]string{}
	if m, _ := d["metadata"].(map[string]any); m != nil {
		if dm, _ := m["dependencies"].(map[string]any); dm != nil && len(dm) <= 256 {
			for k, v := range dm {
				if sv, ok := v.(string); ok && len(sv) <= 512 {
					deps[k] = sv
				}
			}
		}
	}
	if b, e := json.Marshal(collection{Namespace: ns, Name: name, Version: pub.Version, Dependencies: deps}); e == nil {
		mp := pub
		mp.AssetKey, mp.MediaType = "metadata", "application/json"
		_ = s.service.CacheDocument(r.Context(), p, mp, doc.SourceID, b)
	}
	return true, false, nil
}
