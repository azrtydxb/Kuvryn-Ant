package rubygems

import (
	"bytes"
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

// Pull-through for RubyGems compact-index upstreams (rubygems.org). The
// per-gem compact index file (`info/<gem>`) is read through the runtime's
// metadata cache and validated line by line; it carries no URLs, so it is
// served as received. A `.gem` is fetched on first download from
// `gems/<file>.gem`, verified against the sha256 `checksum` of its info line
// and cached together with a metadata record rebuilt from that line, so the
// local composition (info, versions, specs) lists it afterwards and keeps
// serving it during an outage. `/versions`, `/names` and the legacy specs
// listings are feed-local views: the upstream versions of a large registry
// exceed the metadata limit and are never unioned with local content.

var checksumRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type remoteVersion struct {
	key string
	m   metadata
}

// remoteInfo fetches and validates the upstream info file of a gem.
func (s *Repository) remoteInfo(ctx context.Context, p identity.Principal, feed, name string) ([]remoteVersion, artifact.Document, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name}, artifact.Origin{Path: "info/" + artifact.EscapePath(name), Representation: "text/plain"})
	if err != nil {
		return nil, doc, err
	}
	var out []remoteVersion
	seen := map[string]bool{}
	for _, line := range strings.Split(string(doc.Body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" {
			continue
		}
		v, ok := parseInfoLine(name, line)
		if !ok || seen[v.key] {
			continue
		}
		seen[v.key] = true
		out = append(out, v)
		if len(out) > 4096 {
			return nil, doc, artifact.ErrUnavailable
		}
	}
	return out, doc, nil
}

// parseInfoLine parses `<version>[-platform] dep:req&req,...|checksum:<sha>,ruby:<req>,rubygems:<req>`.
func parseInfoLine(name, line string) (remoteVersion, bool) {
	head, tail, ok := strings.Cut(line, "|")
	if !ok {
		return remoteVersion{}, false
	}
	key, deps, _ := strings.Cut(head, " ")
	version, platform, hasPlatform := strings.Cut(key, "-")
	if !hasPlatform {
		platform = "ruby"
	}
	m := metadata{Name: name, Version: version, Platform: platform}
	if len(version) > 128 || !versionPattern.MatchString(version) || len(platform) > 100 || !platformPattern.MatchString(platform) {
		return remoteVersion{}, false
	}
	for _, d := range strings.Split(strings.TrimSpace(deps), ",") {
		if d == "" {
			continue
		}
		dn, req, _ := strings.Cut(d, ":")
		if !namePattern.MatchString(dn) {
			return remoteVersion{}, false
		}
		m.Dependencies = append(m.Dependencies, dependency{Name: dn, Requirements: strings.Split(req, "&"), Type: ":runtime"})
	}
	for _, kv := range strings.Split(tail, ",") {
		k, v, _ := strings.Cut(kv, ":")
		switch k {
		case "checksum":
			m.SHA256 = v
		case "ruby":
			m.Ruby = strings.Split(v, "&")
		case "rubygems":
			m.RubyGems = strings.Split(v, "&")
		}
	}
	if !checksumRE.MatchString(m.SHA256) {
		return remoteVersion{}, false
	}
	return remoteVersion{key: m.key(), m: m}, true
}

// serveRemoteInfo answers an info request from upstream. It returns true once
// the response was written; a degraded upstream error is returned so the
// caller can fall back to the locally cached view.
func (s *Repository) serveRemoteInfo(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name string) (bool, error) {
	versions, doc, err := s.remoteInfo(r.Context(), p, feed, name)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	if len(versions) == 0 {
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	for _, line := range strings.Split(string(doc.Body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" {
			continue
		}
		if _, ok := parseInfoLine(name, line); ok {
			b.WriteString(line + "\n")
		}
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	respond(w, r, b.Bytes(), "text/plain; charset=utf-8")
	return true, nil
}

// fillGem materializes a missing gem (and its metadata record) from upstream.
// file is `<name>-<version>[-<platform>]`; the name is ambiguous because gem
// names contain dashes, so every split before a digit is tried.
func (s *Repository) fillGem(ctx context.Context, p identity.Principal, feed, file string) error {
	for i := 1; i+1 < len(file); i++ {
		if file[i] != '-' || file[i+1] < '0' || file[i+1] > '9' {
			continue
		}
		name, key := file[:i], file[i+1:]
		if !namePattern.MatchString(name) {
			continue
		}
		versions, doc, err := s.remoteInfo(ctx, p, feed, name)
		if errors.Is(err, artifact.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, v := range versions {
			if v.key == key {
				return s.pullGem(ctx, p, feed, doc.SourceID, file, v)
			}
		}
	}
	return artifact.ErrNotFound
}

func (s *Repository) pullGem(ctx context.Context, p identity.Principal, feed, source, file string, v remoteVersion) error {
	pub := artifact.Publication{FeedID: feed, Coordinate: v.m.Name, Version: v.key, AssetKey: "gem", MediaType: "application/octet-stream"}
	if _, err := s.service.Lookup(ctx, p, pub); errors.Is(err, artifact.ErrNotFound) {
		stream, _, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: "gems/" + artifact.EscapePath(file) + ".gem", SourceID: source, Digest: blob.Digest(v.m.SHA256)})
		if err != nil {
			return err
		}
		if stream != nil {
			// Digest is set, so the object is normally staged and committed.
			_, _ = io.Copy(io.Discard, stream.Body)
			_ = stream.Body.Close()
		}
	} else if err != nil {
		return err
	}
	data, _ := json.Marshal(v.m)
	mp := pub
	mp.AssetKey, mp.MediaType = "metadata", "application/json"
	if _, err := s.service.Lookup(ctx, p, mp); errors.Is(err, artifact.ErrNotFound) {
		if err = s.service.CacheDocument(ctx, p, mp, source, data); err != nil {
			return artifact.ErrUnavailable
		}
	}
	return nil
}

// fillGemspec caches the legacy quick gemspec of an already cached gem.
func (s *Repository) fillGemspec(ctx context.Context, p identity.Principal, pub artifact.Publication, file string) error {
	rec, err := s.service.Lookup(ctx, p, pub)
	if err != nil {
		return err
	}
	pub.AssetKey, pub.MediaType = "gemspec", "application/octet-stream"
	stream, _, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: "quick/Marshal.4.8/" + artifact.EscapePath(file) + ".gemspec.rz", SourceID: rec.SourceID})
	if err != nil {
		return err
	}
	if stream != nil {
		_, cerr := io.Copy(io.Discard, io.LimitReader(stream.Body, maxMetadata+1))
		_ = stream.Body.Close()
		if cerr != nil {
			return artifact.ErrUnavailable
		}
	}
	return nil
}
