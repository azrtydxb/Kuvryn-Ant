package condarepo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, subdir string) {
	// Remote-owned (or unowned) subdirs are answered from upstream; a local
	// owner, or an upstream outage, falls through to the local composition.
	served, remoteErr := s.serveRemoteIndex(w, r, p, feed, subdir)
	if served {
		return
	}
	if remoteErr != nil {
		w.Header().Set("X-Kuvryn-Cache", "offline")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// A whole subdir is a single ownership unit. Never union sources by filename.
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,o.namespace,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='metadata' ORDER BY v.version LIMIT 4097`, p.InstanceID, feed, "conda/"+subdir)
	if e != nil {
		failure(w, e)
		return
	}
	type entry struct{ source, namespace, filename string }
	entries := []entry{}
	for rows.Next() {
		var x entry
		if e = rows.Scan(&x.source, &x.namespace, &x.filename); e != nil {
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
	if len(entries) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if len(entries) == 0 && remoteErr != nil {
		w.Header().Del("X-Kuvryn-Cache")
		failure(w, artifact.ErrUnavailable)
		return
	}
	if len(entries) == 0 {
		// Empty channels are necessary for native clients' noarch requests. Authorize
		// against the selected source even when it contains no complete versions.
		var source string
		e = s.db.QueryRow(ctx, `SELECT c.source_id::text FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, feed, "conda/"+subdir).Scan(&source)
		if e != nil && e.Error() != "no rows in result set" {
			failure(w, e)
			return
		}
		policy, err := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if err != nil {
			failure(w, err)
			return
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: subdir, Kind: identity.ArtifactResource}, identity.Read) {
			failure(w, artifact.ErrNotFound)
			return
		}
	}
	packages := map[string]any{}
	total := 0
	for _, x := range entries {
		if x.namespace != subdir || !validFilename(x.filename) {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub := publication(feed, x.namespace, x.filename, "metadata")
		rec, b, err := s.service.Open(ctx, p, pub, 0, -1)
		if err != nil {
			failure(w, err)
			return
		}
		if rec.SourceID != x.source || rec.Info.Size > maxMetadata {
			b.Close()
			failure(w, artifact.ErrUnavailable)
			return
		}
		data, err := io.ReadAll(io.LimitReader(contextReader{ctx, b}, maxMetadata+1))
		b.Close()
		total += len(data)
		if err != nil || total > 8<<20 || info(data) != rec.Info {
			failure(w, artifact.ErrUnavailable)
			return
		}
		m, err := parseMetadata(data)
		if err != nil || m.Subdir != subdir || m.Name+"-"+m.Version+"-"+m.Build+remoteExt(x.filename) != x.filename {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub.AssetKey = "archive"
		dist, err := s.service.Lookup(ctx, p, pub)
		if err != nil {
			failure(w, err)
			return
		}
		if dist.SourceID != x.source {
			failure(w, artifact.ErrUnavailable)
			return
		}
		var item map[string]any
		if json.Unmarshal(data, &item) != nil {
			failure(w, artifact.ErrUnavailable)
			return
		}
		item["sha256"] = string(dist.Info.Digest)
		item["size"] = dist.Info.Size
		packages[x.filename] = item
	}
	writeJSON(w, r, map[string]any{"info": map[string]string{"subdir": subdir}, "packages": packages, "packages.conda": map[string]any{}, "removed": []string{}, "repodata_version": 1})
}
