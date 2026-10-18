package rubygems

import (
	"bytes"
	"context"
	"crypto/md5" // RubyGems compact-index protocol checksum, not artifact integrity.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (s *Repository) entries(ctx context.Context, p identity.Principal, feed, name, filename string) ([]metadata, error) {
	// Materialize and close rows before any nested artifact call: pools may have a
	// single connection. Owner is chosen before filtering versions or assets.
	rows, e := s.db.Query(ctx, `WITH owners AS (SELECT DISTINCT ON(c.coordinate) c.id,c.coordinate,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 ORDER BY c.coordinate,fs.priority) SELECT o.coordinate,o.source_id::text,v.version FROM owners o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='metadata' AND ($3='' OR o.coordinate=$3) AND ($4='' OR o.coordinate||'-'||v.version=$4) ORDER BY o.coordinate,v.version LIMIT 4097`, p.InstanceID, feed, name, filename)
	if e != nil {
		return nil, e
	}
	type entry struct{ name, source, version string }
	list := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.name, &v.source, &v.version); e != nil {
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
	if len(list) > 4096 {
		return nil, artifact.ErrUnavailable
	}
	result := []metadata{}
	total := 0
	for _, v := range list {
		pub := artifact.Publication{FeedID: feed, Coordinate: v.name, Version: v.version, AssetKey: "metadata"}
		rec, b, e := s.service.Open(ctx, p, pub, 0, -1)
		if errors.Is(e, artifact.ErrNotFound) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if rec.SourceID != v.source || rec.Info.Size > maxMetadata {
			b.Close()
			return nil, artifact.ErrUnavailable
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, b}, maxMetadata+1))
		b.Close()
		if e != nil {
			return nil, e
		}
		total += len(data)
		var m metadata
		if len(data) > maxMetadata || total > 16<<20 || json.Unmarshal(data, &m) != nil || m.Name != v.name || m.key() != v.version {
			return nil, artifact.ErrUnavailable
		}
		pub.AssetKey = "gem"
		gem, e := s.service.Lookup(ctx, p, pub)
		if e != nil {
			return nil, e
		}
		if gem.SourceID != v.source || string(gem.Info.Digest) != m.SHA256 {
			return nil, artifact.ErrUnavailable
		}
		result = append(result, m)
	}
	return result, nil
}
func compactInfo(ms []metadata) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, m := range ms {
		b.WriteString(m.key() + " ")
		deps := []string{}
		for _, d := range m.Dependencies {
			if d.Type == ":runtime" {
				deps = append(deps, d.Name+":"+strings.Join(d.Requirements, "&"))
			}
		}
		b.WriteString(strings.Join(deps, ","))
		b.WriteString("|checksum:" + m.SHA256 + ",ruby:" + strings.Join(m.Ruby, "&") + ",rubygems:" + strings.Join(m.RubyGems, "&") + "\n")
	}
	return []byte(b.String())
}
func respond(w http.ResponseWriter, r *http.Request, b []byte, content string) {
	h := sha256.Sum256(b)
	w.Header().Set("Content-Type", content)
	w.Header().Set("ETag", `"`+hex.EncodeToString(h[:])+`"`)
	w.Header().Set("Repr-Digest", `sha-256=:`+base64.StdEncoding.EncodeToString(h[:])+`:`)
	w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(h[:]))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}
func (s *Repository) read(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, route string) {
	name, filename, asset := "", "", ""
	switch {
	case strings.HasPrefix(route, "info/"):
		name = strings.TrimPrefix(route, "info/")
		if !namePattern.MatchString(name) {
			failure(w, artifact.ErrNotFound)
			return
		}
	case strings.HasPrefix(route, "gems/") && strings.HasSuffix(route, ".gem"):
		filename = strings.TrimSuffix(strings.TrimPrefix(route, "gems/"), ".gem")
		asset = "gem"
	case strings.HasPrefix(route, "quick/Marshal.4.8/") && strings.HasSuffix(route, ".gemspec.rz"):
		filename = strings.TrimSuffix(strings.TrimPrefix(route, "quick/Marshal.4.8/"), ".gemspec.rz")
		asset = "gemspec"
	case route == "names" || route == "versions" || route == "specs.4.8.gz" || route == "prerelease_specs.4.8.gz":
	default:
		failure(w, artifact.ErrNotFound)
		return
	}
	if asset != "" && (filename == "" || len(filename) > 360 || strings.ContainsAny(filename, "/\\")) {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// Feed-wide authorization also protects empty indexes.
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		failure(w, e)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
		failure(w, artifact.ErrNotFound)
		return
	}
	var remoteErr error
	if name != "" {
		// Remote-owned (or unowned) gems are answered from upstream; a local
		// owner, or an upstream outage, falls through to the local composition.
		var served bool
		if served, remoteErr = s.serveRemoteInfo(w, r, p, feed, name); served {
			return
		}
		if remoteErr != nil {
			w.Header().Set("X-Kuvryn-Cache", "offline")
		}
	}
	ms, e := s.entries(ctx, p, feed, name, filename)
	if e == nil && asset != "" {
		// Local miss: try to materialize the gem (or its legacy gemspec).
		var fe error
		switch {
		case len(ms) == 0:
			fe = s.fillGem(ctx, p, feed, filename)
			if fe == nil && asset == "gemspec" {
				if ms, e = s.entries(ctx, p, feed, name, filename); e == nil && len(ms) == 1 {
					fe = s.fillGemspec(ctx, p, artifact.Publication{FeedID: feed, Coordinate: ms[0].Name, Version: ms[0].key(), AssetKey: "gem"}, filename)
				}
			}
		case len(ms) == 1 && asset == "gemspec":
			if _, le := s.service.Lookup(ctx, p, artifact.Publication{FeedID: feed, Coordinate: ms[0].Name, Version: ms[0].key(), AssetKey: "gemspec"}); errors.Is(le, artifact.ErrNotFound) {
				fe = s.fillGemspec(ctx, p, artifact.Publication{FeedID: feed, Coordinate: ms[0].Name, Version: ms[0].key(), AssetKey: "gem"}, filename)
			}
		}
		if fe != nil && !errors.Is(fe, artifact.ErrNotFound) {
			failure(w, fe)
			return
		}
		if fe == nil {
			ms, e = s.entries(ctx, p, feed, name, filename)
		}
	}
	if e != nil {
		failure(w, e)
		return
	}
	if asset != "" {
		if len(ms) != 1 {
			failure(w, artifact.ErrNotFound)
			return
		}
		m := ms[0]
		s.download(w, r, p, artifact.Publication{FeedID: feed, Coordinate: m.Name, Version: m.key(), AssetKey: asset})
		return
	}
	if name != "" {
		if len(ms) == 0 {
			w.Header().Del("X-Kuvryn-Cache")
			if remoteErr != nil {
				failure(w, artifact.ErrUnavailable)
				return
			}
			failure(w, artifact.ErrNotFound)
			return
		}
		respond(w, r, compactInfo(ms), "text/plain; charset=utf-8")
		return
	}
	if strings.HasSuffix(route, ".gz") {
		respond(w, r, legacyIndex(ms, route == "prerelease_specs.4.8.gz"), "application/octet-stream")
		return
	}
	groups := map[string][]metadata{}
	for _, m := range ms {
		groups[m.Name] = append(groups[m.Name], m)
	}
	names := []string{}
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("---\n")
	for _, n := range names {
		b.WriteString(n)
		if route == "versions" {
			vs := []string{}
			for _, m := range groups[n] {
				vs = append(vs, m.key())
			}
			hash := md5.Sum(compactInfo(groups[n]))
			b.WriteString(" " + strings.Join(vs, ",") + " " + hex.EncodeToString(hash[:]))
		}
		b.WriteByte('\n')
	}
	respond(w, r, []byte(b.String()), "text/plain; charset=utf-8")
}
