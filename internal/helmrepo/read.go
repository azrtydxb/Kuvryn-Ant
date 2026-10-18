package helmrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/semver"
)

type indexFile struct {
	APIVersion string                      `yaml:"apiVersion"`
	Generated  time.Time                   `yaml:"generated"`
	Entries    map[string][]map[string]any `yaml:"entries"`
}
type candidate struct {
	name, version, source, digest string
	created                       time.Time
}

func (h *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	policy, err := (identity.Roles{DB: h.db}).Resolve(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	// This single statement selects all package owners and versions from one MVCC
	// snapshot. A second source never fills a missing version of an owned chart.
	rows, err := h.db.Query(r.Context(), `WITH owners AS (
 SELECT DISTINCT ON(c.coordinate) c.id,c.coordinate,c.source_id FROM feed_sources fs
 JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id
 WHERE fs.instance_id=$1 AND fs.feed_id=$2 ORDER BY c.coordinate,fs.priority
 ) SELECT o.coordinate,v.version,o.source_id::text,b.digest,v.created_at
 FROM owners o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id
 JOIN blobs b ON b.id=a.blob_id WHERE a.asset_key=o.coordinate||'-'||v.version||'.tgz'
 ORDER BY o.coordinate,v.version LIMIT $3`, p.InstanceID, feed, h.Limits.IndexEntries+1)
	if err != nil {
		fail(w, err)
		return
	}
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.name, &c.version, &c.source, &c.digest, &c.created); err != nil {
			break
		}
		candidates = append(candidates, c)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		fail(w, artifact.ErrUnavailable)
		return
	}
	if len(candidates) > h.Limits.IndexEntries {
		fail(w, artifact.ErrUnavailable)
		return
	}
	idx := indexFile{APIVersion: "v1", Generated: time.Unix(0, 0).UTC(), Entries: map[string][]map[string]any{}}
	remaining := h.Limits.IndexBytes
	for _, c := range candidates {
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: c.source, Kind: identity.ArtifactResource}, identity.Read) {
			continue
		}
		pub := artifact.Publication{FeedID: feed, Coordinate: c.name, Version: c.version, AssetKey: "Chart.yaml"}
		record, reader, e := h.service.Open(r.Context(), p, pub, 0, -1)
		if e != nil {
			fail(w, e)
			return
		}
		if record.SourceID != c.source {
			reader.Close()
			fail(w, artifact.ErrConflict)
			return
		}
		if record.Info.Size > h.Limits.MetadataBytes || record.Info.Size > remaining {
			reader.Close()
			fail(w, artifact.ErrUnavailable)
			return
		}
		data, e := io.ReadAll(io.LimitReader(reader, h.Limits.MetadataBytes+1))
		reader.Close()
		if e != nil {
			fail(w, e)
			return
		}
		remaining -= int64(len(data))
		var meta map[string]any
		if int64(len(data)) > h.Limits.MetadataBytes || decodeYAML(data, &meta) != nil || meta["name"] != c.name || meta["version"] != c.version {
			fail(w, artifact.ErrUnavailable)
			return
		}
		// Keep only genuine Chart.yaml metadata; server-owned fields override any
		// uploaded index-like keys and URLs remain source/digest bound.
		meta["digest"] = c.digest
		meta["created"] = c.created.UTC()
		filename := c.name + "-" + c.version + ".tgz"
		meta["urls"] = []string{"charts/" + url.PathEscape(c.name) + "/" + url.PathEscape(c.version) + "/" + c.source + "/" + c.digest + "/" + url.PathEscape(filename)}
		idx.Entries[c.name] = append(idx.Entries[c.name], meta)
		if c.created.After(idx.Generated) {
			idx.Generated = c.created.UTC()
		}
	}
	// Empty repositories still require an effective read grant.
	if len(idx.Entries) == 0 && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
		fail(w, artifact.ErrNotFound)
		return
	}
	for _, entries := range idx.Entries {
		sort.Slice(entries, func(i, j int) bool {
			return semver.Compare("v"+entries[i]["version"].(string), "v"+entries[j]["version"].(string)) > 0
		})
	}
	data, err := yaml.Marshal(idx)
	if err != nil {
		fail(w, err)
		return
	}
	sum := sha256.Sum256(data)
	w.Header().Set("ETag", strconv.Quote(hex.EncodeToString(sum[:])))
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	http.ServeContent(w, r, "index.yaml", time.Time{}, bytes.NewReader(data))
}

func parseRange(raw string, size int64) (int64, int64, bool) {
	if raw == "" {
		return 0, size, true
	}
	if !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") || size == 0 {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" {
		n, e := strconv.ParseInt(b, 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}
	start, e := strconv.ParseInt(a, 10, 64)
	if e != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		end, e = strconv.ParseInt(b, 10, 64)
		if e != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, true
}
func (h *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, filename, source, digest string) {
	// Provenance has the same pinned chart URL plus .prov, as Helm requires.
	chart, err := h.service.Lookup(r.Context(), p, pub)
	if err != nil {
		fail(w, err)
		return
	}
	if source != "" && (chart.SourceID != source || string(chart.Info.Digest) != digest) {
		fail(w, artifact.ErrConflict)
		return
	}
	pub.AssetKey = filename
	record, err := h.service.Lookup(r.Context(), p, pub)
	if err != nil {
		fail(w, err)
		return
	}
	if record.SourceID != chart.SourceID {
		fail(w, artifact.ErrConflict)
		return
	}
	etag := strconv.Quote("sha256:" + string(record.Info.Digest))
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Checksum-Sha256", string(record.Info.Digest))
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Accept-Ranges", "bytes")
	if r.Header.Get("If-None-Match") == etag || r.Header.Get("If-None-Match") == "*" {
		w.WriteHeader(304)
		return
	}
	raw := r.Header.Get("Range")
	if r.Method == "HEAD" || (r.Header.Get("If-Range") != "" && r.Header.Get("If-Range") != etag) {
		raw = ""
	}
	offset, length, ok := parseRange(raw, record.Info.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", record.Info.Size))
		http.Error(w, "invalid range", 416)
		return
	}
	status := 200
	if raw != "" {
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, record.Info.Size))
	}
	if r.Method == "HEAD" {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(status)
		return
	}
	current, reader, err := h.service.Open(r.Context(), p, pub, offset, length)
	if err != nil {
		fail(w, err)
		return
	}
	defer reader.Close()
	if current.ID != record.ID || current.Info != record.Info || current.SourceID != record.SourceID {
		fail(w, artifact.ErrConflict)
		return
	}
	// Recheck the chart binding for provenance after its own authorized Open.
	if filename != pub.Coordinate+"-"+pub.Version+".tgz" {
		latest, e := h.service.Lookup(r.Context(), p, artifact.Publication{FeedID: pub.FeedID, Coordinate: pub.Coordinate, Version: pub.Version, AssetKey: pub.Coordinate + "-" + pub.Version + ".tgz"})
		if e != nil || latest.ID != chart.ID {
			fail(w, artifact.ErrConflict)
			return
		}
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	_, _ = io.CopyBuffer(&idleWriter{Writer: w, controller: controller}, io.LimitReader(reader, length), make([]byte, 64<<10))
}
