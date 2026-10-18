package npmregistry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	pub := artifact.Publication{FeedID: feed, Namespace: namespace(name), Coordinate: name}
	origin := artifact.Origin{Path: artifact.EscapePath(name), Representation: representation}
	doc, err := s.service.RemoteMetadata(ctx, p, pub, origin)
	var stream *artifact.StreamDocument
	if errors.Is(err, artifact.ErrTooLarge) && tail == "" && !tagsOnly {
		// Too large for the metadata cache: stream it through the rewriter
		// without holding the document in memory.
		var scancel context.CancelFunc
		ctx, scancel = context.WithTimeout(r.Context(), 15*time.Minute)
		defer scancel()
		stream, err = s.service.RemoteStream(ctx, p, pub, origin)
		if stream != nil {
			defer stream.Body.Close()
		}
	}
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
	if tail == "" && !tagsOnly {
		var src io.Reader
		var sourceID, validator string
		if stream != nil {
			src, sourceID, validator = stream.Body, stream.SourceID, stream.ETag
		} else {
			sum := sha256.Sum256(doc.Body)
			src, sourceID, validator = bytes.NewReader(doc.Body), doc.SourceID, hex.EncodeToString(sum[:])
		}
		s.streamPackument(w, r, src, sourceID, validator, doc.Stale, name, slug, representation)
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

// streamPackument writes the rewritten packument as it reads src. A document
// that turns out to be invalid before any byte is sent is a 502; one that
// fails afterwards aborts the connection so the client never sees a truncated
// document as complete.
func (s *Registry) streamPackument(w http.ResponseWriter, r *http.Request, src io.Reader, sourceID, validator string, stale bool, name, slug, representation string) {
	etag := documentETag(validator, s.publicURL, slug, name, sourceID, representation)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	lw := &lazyWriter{w: w, rc: http.NewResponseController(w)}
	lw.start = func() {
		if stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("Content-Type", representation)
		w.WriteHeader(200)
	}
	base := s.publicURL + "/feeds/" + url.PathEscape(slug) + "/npm/" + name + "/-/" + pathBase(name) + "-"
	suffix := ".tgz?source=" + url.QueryEscape(sourceID)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", representation)
		w.WriteHeader(200)
		return
	}
	if err := rewritePackument(lw, src, name, func(version string) string { return base + version + suffix }); err != nil {
		if !lw.started {
			w.Header().Del("ETag")
			failure(w, 502, "invalid upstream metadata")
			return
		}
		panic(http.ErrAbortHandler)
	}
}
