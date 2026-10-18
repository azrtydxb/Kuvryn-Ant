package pubrepo

import (
	"context"
	"encoding/json"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"golang.org/x/mod/semver"
	"io"
	"net/http"
	"sort"
	"time"
)

func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, version, base string) {
	served, remoteErr := s.serveRemoteIndex(w, r, p, feed, name, version, base)
	if served {
		return
	}
	if remoteErr != nil {
		w.Header().Set("X-Kuvryn-Cache", "offline")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, e := s.db.Query(ctx, `WITH owner AS(SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='pubspec' AND ($4='' OR v.version=$4) LIMIT 4097`, p.InstanceID, feed, name, version)
	if e != nil {
		failure(w, e)
		return
	}
	type entry struct{ source, version string }
	entries := []entry{}
	for rows.Next() {
		var x entry
		if e = rows.Scan(&x.source, &x.version); e != nil {
			break
		}
		entries = append(entries, x)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	if len(entries) == 0 {
		if remoteErr != nil {
			w.Header().Del("X-Kuvryn-Cache")
			failure(w, artifact.ErrUnavailable)
			return
		}
		failure(w, artifact.ErrNotFound)
		return
	}
	if len(entries) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return semver.Compare("v"+entries[i].version, "v"+entries[j].version) < 0 })
	items := []any{}
	var latest any
	total := 0
	for _, x := range entries {
		pub := artifact.Publication{FeedID: feed, Coordinate: name, Version: x.version, AssetKey: "pubspec"}
		rec, b, e := s.service.Open(ctx, p, pub, 0, -1)
		if e != nil {
			failure(w, e)
			return
		}
		if rec.SourceID != x.source || rec.Info.Size > maxMetadata {
			b.Close()
			failure(w, artifact.ErrUnavailable)
			return
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, b}, maxMetadata+1))
		b.Close()
		var spec map[string]any
		total += len(data)
		if e != nil || len(data) > maxMetadata || total > 8<<20 || json.Unmarshal(data, &spec) != nil || spec["name"] != name || spec["version"] != x.version {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub.AssetKey = "archive"
		rec, e = s.service.Lookup(ctx, p, pub)
		if e != nil {
			failure(w, e)
			return
		}
		if rec.SourceID != x.source {
			failure(w, artifact.ErrUnavailable)
			return
		}
		item := map[string]any{"version": x.version, "pubspec": spec, "archive_url": base + "packages/" + name + "/versions/" + x.version + ".tar.gz", "archive_sha256": string(rec.Info.Digest)}
		items = append(items, item)
		if semver.Prerelease("v"+x.version) == "" {
			latest = item
		}
	}
	if version != "" {
		writeJSON(w, r, items[0])
		return
	}
	if latest == nil {
		latest = items[len(items)-1]
	}
	writeJSON(w, r, map[string]any{"name": name, "latest": latest, "versions": items})
}
