package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

func byteRange(raw string, size int64) (offset, length int64, ok bool) {
	if raw == "" {
		return 0, size, true
	}
	if !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") || size == 0 {
		return 0, 0, false
	}
	start, end, found := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
	if !found {
		return 0, 0, false
	}
	if start == "" {
		n, err := strconv.ParseInt(end, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}
	offset, err := strconv.ParseInt(start, 10, 64)
	if err != nil || offset < 0 || offset >= size {
		return 0, 0, false
	}
	last := size - 1
	if end != "" {
		last, err = strconv.ParseInt(end, 10, 64)
		if err != nil || last < offset {
			return 0, 0, false
		}
		if last >= size {
			last = size - 1
		}
	}
	return offset, last - offset + 1, true
}
func (r *Registry) read(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, reference, key string) {
	digest, ok := parseDigest(reference)
	version := string(digest)
	if !ok {
		if key != "manifest" || !tagPattern.MatchString(reference) {
			failure(w, 400, "DIGEST_INVALID", "invalid content reference")
			return
		}
		var err error
		version, err = r.service.ResolveTag(req.Context(), p, rep.publication("", key), reference)
		if err != nil {
			if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrDenied) {
				failure(w, 404, "MANIFEST_UNKNOWN", "manifest not found")
			} else {
				failService(w, err)
			}
			return
		}
	}
	publication := rep.publication(version, key)
	record, err := r.service.Lookup(req.Context(), p, publication)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			code := "BLOB_UNKNOWN"
			if key == "manifest" {
				code = "MANIFEST_UNKNOWN"
			}
			failure(w, 404, code, "content not found")
		} else {
			failService(w, err)
		}
		return
	}
	if key == "manifest" && !accepts(req.Header.Get("Accept"), record.MediaType) {
		failure(w, 406, "MANIFEST_UNKNOWN", "manifest media type not accepted")
		return
	}
	etag := strconv.Quote("sha256:" + string(record.Info.Digest))
	w.Header().Set("Docker-Content-Digest", "sha256:"+string(record.Info.Digest))
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-cache")
	if req.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	rangeValue := req.Header.Get("Range")
	if value := req.Header.Get("If-Range"); value != "" && value != etag {
		rangeValue = ""
	}
	if req.Method == "HEAD" {
		rangeValue = ""
	}
	offset, length, valid := byteRange(rangeValue, record.Info.Size)
	if !valid {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", record.Info.Size))
		failure(w, 416, "RANGE_INVALID", "invalid byte range")
		return
	}
	status := 200
	if rangeValue != "" {
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, record.Info.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if req.Method == "HEAD" {
		w.WriteHeader(status)
		return
	}
	current, reader, err := r.service.Open(req.Context(), p, publication, offset, length)
	if err != nil {
		w.Header().Del("Content-Length")
		failService(w, err)
		return
	}
	defer reader.Close()
	if current.ID != record.ID || current.SourceID != record.SourceID || current.Info != record.Info {
		w.Header().Del("Content-Length")
		failure(w, 409, "UNKNOWN", "repository ownership changed")
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	w.WriteHeader(status)
	_, _ = io.CopyBuffer(&progressWriter{writer: w, controller: controller}, reader, make([]byte, 64<<10))
}

type progressWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (p *progressWriter) Write(data []byte) (int, error) {
	_ = p.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return p.writer.Write(data)
}
func (r *Registry) tags(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository) {
	limit := 100
	query := req.URL.Query()
	for key, values := range query {
		if (key != "n" && key != "last") || len(values) != 1 {
			failure(w, 400, "NAME_INVALID", "invalid tag pagination")
			return
		}
	}
	if raw := query.Get("n"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			failure(w, 400, "NAME_INVALID", "invalid tag page size")
			return
		}
		limit = n
	}
	last := query.Get("last")
	if last != "" && !tagPattern.MatchString(last) {
		failure(w, 400, "NAME_INVALID", "invalid tag cursor")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	policy, err := (identity.Roles{DB: r.db}).Resolve(ctx, p)
	if err != nil {
		failService(w, err)
		return
	}
	// Resolve the package owner and tags in one snapshot; never merge later
	// sources into the first source's repository.
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		failService(w, err)
		return
	}
	defer rollback(tx)
	var component, source string
	err = tx.QueryRow(ctx, `SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, rep.feed, rep.name).Scan(&component, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		failure(w, 404, "NAME_UNKNOWN", "repository not found")
		return
	}
	if err != nil {
		failService(w, err)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: rep.feed, SourceID: source, Namespace: rep.namespace, Kind: identity.ArtifactResource}, identity.Read) {
		failure(w, 404, "NAME_UNKNOWN", "repository not found")
		return
	}
	rows, err := tx.Query(ctx, `SELECT name FROM tags WHERE component_id=$1 AND name COLLATE "C">$2 ORDER BY name COLLATE "C" LIMIT $3`, component, last, limit+1)
	if err != nil {
		failService(w, err)
		return
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		names = append(names, name)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		failService(w, errors.New("tag query failed"))
		return
	}
	if len(names) > limit {
		names = names[:limit]
		w.Header().Set("Link", fmt.Sprintf("</v2/%s/tags/list?n=%d&last=%s>; rel=\"next\"", rep.full, limit, url.QueryEscape(names[len(names)-1])))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{rep.full, names})
}
