package composer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id, base string, dev bool) {
	// Remote-owned (or unowned) packages are answered from upstream; a local
	// owner falls through to the local composition. During an upstream outage
	// only the versions cached earlier are listed.
	served, remoteErr := s.serveRemoteIndex(w, r, p, feed, id, base, dev)
	if served {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if remoteErr != nil {
		items, e := s.offlineIndex(ctx, p, feed, id, base, dev)
		if e != nil || len(items) == 0 {
			failure(w, artifact.ErrUnavailable)
			return
		}
		w.Header().Set("X-Kuvryn-Cache", "offline")
		writeJSON(w, r, map[string]any{"packages": map[string]any{id: items}})
		return
	}
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='metadata' AND o.namespace=$4 ORDER BY v.version LIMIT 1025`, p.InstanceID, feed, id, strings.Split(id, "/")[0])
	if e != nil {
		failure(w, e)
		return
	}
	type entry struct{ source, version string }
	entries := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.source, &v.version); e != nil {
			break
		}
		entries = append(entries, v)
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
		failure(w, artifact.ErrNotFound)
		return
	}
	if len(entries) > 1024 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	items := []any{}
	total := 0
	for _, v := range entries {
		pub := artifact.Publication{FeedID: feed, Namespace: strings.Split(id, "/")[0], Coordinate: id, Version: v.version, AssetKey: "dist.zip"}
		dist, e := s.service.Lookup(ctx, p, pub)
		if e != nil {
			failure(w, e)
			return
		}
		if dist.SourceID != v.source {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub.AssetKey = "metadata"
		rec, b, e := s.service.Open(ctx, p, pub, 0, -1)
		if e != nil {
			failure(w, e)
			return
		}
		if rec.Info.Size > maxMetadata || rec.SourceID != v.source {
			b.Close()
			failure(w, artifact.ErrUnavailable)
			return
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, b}, maxMetadata+1))
		b.Close()
		if e != nil {
			failure(w, e)
			return
		}
		m, e := parseManifest(data)
		if e != nil || m.Name != id || m.Version != v.version {
			failure(w, artifact.ErrUnavailable)
			return
		}
		total += len(data)
		if total > 8<<20 {
			failure(w, artifact.ErrUnavailable)
			return
		}
		if !dev {
			var item map[string]any
			if json.Unmarshal(data, &item) != nil {
				failure(w, artifact.ErrUnavailable)
				return
			}
			item["dist"] = map[string]string{"type": "zip", "url": base + "dist/" + id + "/" + v.version + "/archive.zip", "reference": string(dist.Info.Digest), "shasum": m.ArchiveSHA1}
			delete(item, "_ant-sha1")
			items = append(items, item)
		}
	}
	writeJSON(w, r, map[string]any{"packages": map[string]any{id: items}})
}
