package npmregistry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// errServed is returned by remotePackument when it already wrote the response.
var errServed = errors.New("served")

const (
	abbreviated = "application/vnd.npm.install-v1+json"
	fullDoc     = "application/json"
)

// remotePackument serves a packument from the feed's remote sources. The
// upstream document is parsed and every dist.tarball is rewritten to this feed
// (with a source token that pins the tarball to the remote that supplied the
// metadata); upstream absolute URLs never reach the client. Nothing else about
// the document is interpreted, so dist-tags, integrity, time and engines
// survive. It returns nil when the caller should fall back to the local view
// (not found upstream or local owner) or the upstream error when the upstream
// was unreachable, and errServed once the response has been written.
func (s *Registry) remotePackument(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, slug, tail string, tagsOnly bool) error {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	representation := fullDoc
	if strings.Contains(r.Header.Get("Accept"), abbreviated) {
		representation = abbreviated
	}
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Namespace: namespace(name), Coordinate: name}, artifact.Origin{Path: artifact.EscapePath(name), Representation: representation})
	if errors.Is(err, artifact.ErrNotFound) {
		return nil
	}
	if err != nil {
		if errors.Is(err, artifact.ErrUnavailable) {
			return err // degrade to whatever is cached locally
		}
		failService(w, err)
		return errServed
	}
	var packument map[string]json.RawMessage
	if json.Unmarshal(doc.Body, &packument) != nil {
		failure(w, 502, "invalid upstream metadata")
		return errServed
	}
	var upstream map[string]json.RawMessage
	if json.Unmarshal(packument["versions"], &upstream) != nil {
		failure(w, 502, "invalid upstream metadata")
		return errServed
	}
	versions := make(map[string]any, len(upstream))
	for version, raw := range upstream {
		if !validVersion(version) {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			failure(w, 502, "invalid upstream metadata")
			return errServed
		}
		dist, _ := m["dist"].(map[string]any)
		if dist == nil {
			dist = map[string]any{}
			m["dist"] = dist
		}
		dist["tarball"] = s.publicURL + "/feeds/" + url.PathEscape(slug) + "/npm/" + name + "/-/" + pathBase(name) + "-" + version + ".tgz?source=" + url.QueryEscape(doc.SourceID)
		versions[version] = m
	}
	tags := map[string]string{}
	var upstreamTags map[string]string
	_ = json.Unmarshal(packument["dist-tags"], &upstreamTags)
	for tag, version := range upstreamTags {
		if _, ok := versions[version]; ok && validTag(tag) {
			tags[tag] = version
		}
	}
	full := map[string]any{}
	for key, raw := range packument {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			full[key] = v
		}
	}
	full["name"], full["_id"], full["versions"], full["dist-tags"] = name, name, versions, tags
	delete(full, "_rev")
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	s.respond(w, r, full, versions, tags, tail, tagsOnly, representation)
	return errServed
}
