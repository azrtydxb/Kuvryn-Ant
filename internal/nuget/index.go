package nuget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func versionLess(a, b string) bool {
	ap, ar, _ := strings.Cut(a, "-")
	bp, br, _ := strings.Cut(b, "-")
	aa := strings.Split(ap, ".")
	bb := strings.Split(bp, ".")
	for i := 0; i < 4; i++ {
		x, y := 0, 0
		if i < len(aa) {
			x, _ = strconv.Atoi(aa[i])
		}
		if i < len(bb) {
			y, _ = strconv.Atoi(bb[i])
		}
		if x != y {
			return x < y
		}
	}
	if ar == "" || br == "" {
		return ar != "" && br == ""
	}
	aa = strings.Split(ar, ".")
	bb = strings.Split(br, ".")
	for i := 0; i < len(aa) && i < len(bb); i++ {
		if aa[i] == bb[i] {
			continue
		}
		an := strings.Trim(aa[i], "0123456789") == ""
		bn := strings.Trim(bb[i], "0123456789") == ""
		if an != bn {
			return an
		}
		if an && len(aa[i]) != len(bb[i]) {
			return len(aa[i]) < len(bb[i])
		}
		return aa[i] < bb[i]
	}
	return len(aa) < len(bb)
}
func contentURL(base, id, v string) string {
	return base + "v3/flatcontainer/" + id + "/" + v + "/" + id + "." + v + ".nupkg"
}
func (s *Repository) readMetadata(ctx context.Context, p identity.Principal, feed, id, v, source string) (metadata, error) {
	var m metadata
	rec, b, e := s.service.Open(ctx, p, artifact.Publication{FeedID: feed, Coordinate: id, Version: v, AssetKey: "metadata"}, 0, -1)
	if e != nil {
		return m, e
	}
	defer b.Close()
	if rec.Info.Size > maxMetadata || source != "" && source != rec.SourceID {
		return m, artifact.ErrUnavailable
	}
	data, e := io.ReadAll(io.LimitReader(contextReader{ctx, b}, maxMetadata+1))
	if e != nil {
		return m, e
	}
	if len(data) > maxMetadata || json.Unmarshal(data, &m) != nil || m.ID != id || m.Version != v {
		return m, artifact.ErrUnavailable
	}
	return m, nil
}
func (s *Repository) leaf(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id, v, base string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, e := s.readMetadata(ctx, p, feed, id, v, "")
	if e != nil {
		failure(w, e)
		return
	}
	writeJSON(w, r, map[string]any{"@id": base + "v3/registration/" + id + "/" + v + ".json", "listed": true, "packageContent": contentURL(base, id, v), "registration": base + "v3/registration/" + id + "/index.json"})
}
func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id, base string, registration bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='metadata' ORDER BY v.version LIMIT 4097`, p.InstanceID, feed, id)
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
	if len(entries) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return versionLess(entries[i].version, entries[j].version) })
	versions := []string{}
	items := []any{}
	total := 0
	for _, v := range entries {
		rec, e := s.service.Lookup(ctx, p, artifact.Publication{FeedID: feed, Coordinate: id, Version: v.version, AssetKey: "nupkg"})
		if e != nil {
			failure(w, e)
			return
		}
		if rec.SourceID != v.source {
			failure(w, artifact.ErrUnavailable)
			return
		}
		versions = append(versions, v.version)
		if registration {
			m, e := s.readMetadata(ctx, p, feed, id, v.version, v.source)
			if e != nil {
				failure(w, e)
				return
			}
			catalog := map[string]any{"@id": base + "v3/registration/" + id + "/" + v.version + ".json#catalog", "id": m.ID, "version": m.Version, "authors": m.Authors, "description": m.Description, "dependencyGroups": m.Groups, "listed": true}
			item := map[string]any{"@id": base + "v3/registration/" + id + "/" + v.version + ".json", "catalogEntry": catalog, "packageContent": contentURL(base, id, v.version)}
			encoded, _ := json.Marshal(item)
			total += len(encoded)
			if total > 15<<20 {
				failure(w, artifact.ErrUnavailable)
				return
			}
			items = append(items, item)
		}
	}
	if !registration {
		writeJSON(w, r, map[string]any{"versions": versions})
		return
	}
	pages := []any{}
	for start := 0; start < len(items); start += 64 {
		end := start + 64
		if end > len(items) {
			end = len(items)
		}
		pages = append(pages, map[string]any{"@id": base + "v3/registration/" + id + "/index.json#page-" + strconv.Itoa(start/64), "count": end - start, "lower": versions[start], "upper": versions[end-1], "items": items[start:end]})
	}
	writeJSON(w, r, map[string]any{"@id": base + "v3/registration/" + id + "/index.json", "count": len(pages), "items": pages})
}
