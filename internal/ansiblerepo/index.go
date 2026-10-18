package ansiblerepo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"golang.org/x/mod/semver"
)

const maxVersions = 4096

type entry struct {
	source, namespace, version string
	created                    time.Time
}

// index serves the Galaxy v3 collection detail, versions list and per-version
// metadata pages. The coordinate is namespace.name with the namespace as the
// authorization unit; each entry is rechecked through shared Open/Lookup.
// info=true renders the collection page, otherwise a versions list (version
// empty) or a single version detail.
func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, ns, name, version, base string, info bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	coordinate := ns + "." + name
	rows, e := s.db.Query(ctx, `WITH owner AS(SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,o.namespace,v.version,v.created_at FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='metadata' ORDER BY v.created_at,v.version LIMIT 4097`, p.InstanceID, feed, coordinate)
	if e != nil {
		failure(w, e)
		return
	}
	entries := []entry{}
	for rows.Next() {
		var x entry
		if e = rows.Scan(&x.source, &x.namespace, &x.version, &x.created); e != nil {
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
	if len(entries) > maxVersions {
		failure(w, artifact.ErrUnavailable)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return semver.Compare("v"+entries[i].version, "v"+entries[j].version) > 0 })
	if info {
		s.collectionPage(w, r, p, feed, ns, name, coordinate, base, entries)
		return
	}
	if version == "" {
		s.versionsList(w, r, p, feed, ns, name, coordinate, base, entries)
		return
	}
	s.versionDetail(w, r, p, feed, ns, name, coordinate, version, base, entries)
}

// collectionPage renders GET /v3/collections/{ns}/{name}/ with the created and
// updated timestamps and the ordered versions entrypoint. The highest version
// is opened so the page enforces the same read barrier as the lists.
func (s *Repository) collectionPage(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, ns, name, coordinate, base string, entries []entry) {
	if len(entries) == 0 {
		var source string
		e := s.db.QueryRow(r.Context(), `SELECT c.source_id::text FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, feed, coordinate).Scan(&source)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			failure(w, e)
			return
		}
		policy, err := (identity.Roles{DB: s.db}).Resolve(r.Context(), p)
		if err != nil {
			failure(w, err)
			return
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: ns, Kind: identity.ArtifactResource}, identity.Read) {
			failure(w, artifact.ErrNotFound)
			return
		}
		failure(w, artifact.ErrNotFound)
		return
	}
	meta := artifact.Publication{FeedID: feed, Namespace: entries[0].namespace, Coordinate: coordinate, Version: entries[0].version, AssetKey: "metadata"}
	rec, body, err := s.service.Open(r.Context(), p, meta, 0, -1)
	if err != nil {
		failure(w, err)
		return
	}
	var c collection
	data, rerr := io.ReadAll(io.LimitReader(contextReader{r.Context(), body}, maxMetadata+1))
	body.Close()
	if rerr != nil || rec.Info.Size > maxMetadata || info(data) != rec.Info || json.Unmarshal(data, &c) != nil || c.Namespace != ns || c.Name != name || c.Version != entries[0].version {
		failure(w, artifact.ErrUnavailable)
		return
	}
	created := entries[len(entries)-1].created
	updated := entries[0].created
	versions := make([]map[string]any, 0, len(entries))
	for _, x := range entries {
		href := base + "v3/collections/" + ns + "/" + name + "/versions/" + x.version + "/"
		versions = append(versions, map[string]any{"version": x.version, "href": href, "download_url": downloadURL(base, ns, name, x.version)})
	}
	writeJSON(w, r, map[string]any{
		"namespace":    map[string]any{"name": ns},
		"name":         name,
		"href":         base + "v3/collections/" + ns + "/" + name + "/",
		"versions":     versions,
		"created_at":   formatTime(created),
		"updated_at":   formatTime(updated),
		"dependencies": c.Dependencies,
	})
}

// versionsList renders GET /v3/collections/{ns}/{name}/versions/ with the
// paginated results shape the ansible-galaxy client consumes.
func (s *Repository) versionsList(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, ns, name, coordinate, base string, entries []entry) {
	results := make([]map[string]any, 0, len(entries))
	for _, x := range entries {
		if x.namespace != ns || !validVersion(x.version) {
			failure(w, artifact.ErrUnavailable)
			return
		}
		rec, body, err := s.service.Open(r.Context(), p, artifact.Publication{FeedID: feed, Namespace: x.namespace, Coordinate: coordinate, Version: x.version, AssetKey: "metadata"}, 0, -1)
		if err != nil {
			failure(w, err)
			return
		}
		data, err := io.ReadAll(io.LimitReader(contextReader{r.Context(), body}, maxMetadata+1))
		body.Close()
		if err != nil || rec.Info.Size > maxMetadata || info(data) != rec.Info {
			failure(w, artifact.ErrUnavailable)
			return
		}
		var c collection
		if json.Unmarshal(data, &c) != nil || c.Namespace != ns || c.Name != name || c.Version != x.version {
			failure(w, artifact.ErrUnavailable)
			return
		}
		results = append(results, map[string]any{
			"version":      x.version,
			"href":         base + "v3/collections/" + ns + "/" + name + "/versions/" + x.version + "/",
			"download_url": downloadURL(base, ns, name, x.version),
		})
	}
	writeJSON(w, r, map[string]any{"results": results, "links": map[string]any{"next": nil}})
}

// versionDetail renders GET /v3/collections/{ns}/{name}/versions/{version}/
// with download_url, artifact checksum and dependencies for installation.
func (s *Repository) versionDetail(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, ns, name, coordinate, version, base string, entries []entry) {
	for _, x := range entries {
		if x.version != version {
			continue
		}
		if x.namespace != ns {
			failure(w, artifact.ErrUnavailable)
			return
		}
		meta := artifact.Publication{FeedID: feed, Namespace: x.namespace, Coordinate: coordinate, Version: x.version, AssetKey: "metadata"}
		rec, body, err := s.service.Open(r.Context(), p, meta, 0, -1)
		if err != nil {
			failure(w, err)
			return
		}
		data, err := io.ReadAll(io.LimitReader(contextReader{r.Context(), body}, maxMetadata+1))
		body.Close()
		if err != nil || rec.Info.Size > maxMetadata || info(data) != rec.Info {
			failure(w, artifact.ErrUnavailable)
			return
		}
		var c collection
		if json.Unmarshal(data, &c) != nil || c.Namespace != ns || c.Name != name || c.Version != version {
			failure(w, artifact.ErrUnavailable)
			return
		}
		arch, err := s.service.Lookup(r.Context(), p, artifact.Publication{FeedID: feed, Namespace: x.namespace, Coordinate: coordinate, Version: x.version, AssetKey: "archive"})
		if err != nil {
			failure(w, err)
			return
		}
		if arch.SourceID != x.source {
			failure(w, artifact.ErrUnavailable)
			return
		}
		href := base + "v3/collections/" + ns + "/" + name + "/versions/" + x.version + "/"
		dl := downloadURL(base, ns, name, x.version)
		writeJSON(w, r, map[string]any{
			"namespace":    map[string]any{"name": ns},
			"collection":   map[string]any{"name": name},
			"version":      x.version,
			"href":         href,
			"download_url": dl,
			"artifact": map[string]any{
				"sha256":       string(arch.Info.Digest),
				"size":         arch.Info.Size,
				"filename":     ns + "-" + name + "-" + x.version + ".tar.gz",
				"download_url": dl,
			},
			"metadata":   map[string]any{"dependencies": c.Dependencies},
			"signatures": []any{},
		})
		return
	}
	failure(w, artifact.ErrNotFound)
}

func downloadURL(base, ns, name, version string) string {
	return base + "downloads/" + ns + "/" + name + "/" + version + "/collection.tar.gz"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
