package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

func readAll(rc io.ReadCloser, limit int64) ([]byte, error) {
	defer rc.Close()
	b, e := io.ReadAll(io.LimitReader(rc, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, blob.ErrIntegrity
	}
	return b, nil
}

// resolveRef returns the commit a branch/tag currently points at.
func (s *Repository) resolveRef(ctx context.Context, p identity.Principal, t target, name string) (string, error) {
	if !refRe.MatchString(name) {
		return "", artifact.ErrNotFound
	}
	prefix := name + "@"
	var version string
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e := s.db.QueryRow(qctx, `WITH owner AS(SELECT c.id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 AND c.namespace=$4 ORDER BY fs.priority LIMIT 1) SELECT v.version FROM owner o JOIN versions v ON v.component_id=o.id WHERE left(v.version,length($5))=$5 ORDER BY v.version DESC LIMIT 1`, p.InstanceID, t.feed, t.refCoordinate(), namespace, prefix).Scan(&version)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", artifact.ErrNotFound
	}
	if e != nil {
		return "", artifact.ErrUnavailable
	}
	_, body, e := s.service.Open(ctx, p, artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.refCoordinate(), Version: version, AssetKey: refKey}, 0, -1)
	if e != nil {
		return "", e
	}
	b, e := readAll(body, 64)
	if e != nil {
		return "", e
	}
	v := strings.TrimSpace(string(b))
	if !commitRe.MatchString(v) {
		return "", artifact.ErrNotFound
	}
	return v, nil
}

// revision resolves a branch, tag or commit to a commit id and its manifest.
func (s *Repository) revision(ctx context.Context, p identity.Principal, t target, rev string) (string, manifest, error) {
	var m manifest
	commit := rev
	if !commitRe.MatchString(rev) {
		var e error
		if commit, e = s.resolveRef(ctx, p, t, rev); e != nil {
			return "", m, e
		}
	}
	_, body, e := s.service.Open(ctx, p, artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.coordinate(), Version: commit, AssetKey: manifestKey}, 0, -1)
	if e != nil {
		return "", m, e
	}
	b, e := readAll(body, maxManifest)
	if e != nil {
		return "", m, e
	}
	if e = json.Unmarshal(b, &m); e != nil || commitID(m.Files) != commit {
		return "", m, blob.ErrIntegrity
	}
	return commit, m, nil
}

type lfsInfo struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	PointerSize int    `json:"pointerSize"`
}

func (s *Repository) info(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, rev string) {
	commit, m, e := s.revision(r.Context(), p, t, rev)
	if e != nil {
		failure(w, e)
		return
	}
	siblings := make([]map[string]any, 0, len(m.Files))
	for _, f := range m.Files {
		siblings = append(siblings, map[string]any{"rfilename": f.Path, "size": f.Size, "blobId": f.SHA256[:40], "lfs": lfsInfo{f.SHA256, f.Size, 134}})
	}
	out := map[string]any{"_id": commit, "id": t.repo, "sha": commit, "private": false, "gated": false, "disabled": false, "tags": []string{}, "siblings": siblings}
	if t.typ == "models" {
		out["modelId"] = t.repo
	}
	writeJSON(w, r, out)
}

func (s *Repository) tree(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, rev, sub string) {
	_, m, e := s.revision(r.Context(), p, t, rev)
	if e != nil {
		failure(w, e)
		return
	}
	recursive := r.URL.Query().Get("recursive") != "" && r.URL.Query().Get("recursive") != "false"
	prefix := ""
	if sub != "" {
		prefix = strings.Trim(sub, "/") + "/"
	}
	out := []map[string]any{}
	dirs := map[string]bool{}
	for _, f := range m.Files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rel := f.Path[len(prefix):]
		if i := strings.Index(rel, "/"); i >= 0 && !recursive {
			dirs[prefix+rel[:i]] = true
			continue
		}
		out = append(out, map[string]any{"type": "file", "oid": f.SHA256[:40], "size": f.Size, "path": f.Path, "lfs": lfsInfo{f.SHA256, f.Size, 134}})
	}
	names := make([]string, 0, len(dirs))
	for d := range dirs {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		out = append(out, map[string]any{"type": "directory", "oid": "", "size": 0, "path": d})
	}
	writeJSON(w, r, out)
}

func (s *Repository) resolve(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, rev, path string) {
	if !validPath(path) {
		failure(w, artifact.ErrNotFound)
		return
	}
	if !s.acquire() {
		w.WriteHeader(429)
		return
	}
	defer s.release()
	ctx := r.Context()
	commit, m, e := s.revision(ctx, p, t, rev)
	if e != nil {
		failure(w, e)
		return
	}
	entry, ok := m.find(path)
	if !ok {
		failure(w, artifact.ErrNotFound)
		return
	}
	pub := artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.coordinate(), Version: commit, AssetKey: path}
	want := blob.Info{Digest: blob.Digest(entry.SHA256), Size: entry.Size}
	etag := `"` + entry.SHA256 + `"`
	offset, length, partial := int64(0), entry.Size, false
	if rh := r.Header.Get("Range"); r.Method == "GET" && rh != "" && entry.Size > 0 && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		var err error
		offset, length, err = parseRange(rh, entry.Size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.Size))
			http.Error(w, http.StatusText(416), 416)
			return
		}
		partial = true
	}
	var body io.ReadCloser
	// HEAD opens a zero-length range so the stored object is still verified.
	openLen := length
	if r.Method == "HEAD" {
		openLen = 0
	}
	rec, rc, err := s.service.Open(ctx, p, pub, offset, openLen)
	if err != nil {
		failure(w, err)
		return
	}
	defer rc.Close()
	if rec.Info != want {
		failure(w, blob.ErrIntegrity)
		return
	}
	if r.Method != "HEAD" {
		body = rc
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", etag)
	h.Set("X-Linked-Etag", etag)
	h.Set("X-Linked-Size", strconv.FormatInt(entry.Size, 10))
	h.Set("X-Repo-Commit", commit)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	code := 200
	if partial {
		code = 206
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, entry.Size))
	}
	w.WriteHeader(code)
	if body != nil {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{ctxReader{ctx, body}}, make([]byte, 64<<10))
	}
}

// parseRange accepts one byte range; multipart ranges are rejected to keep
// work bounded.
func parseRange(raw string, size int64) (int64, int64, error) {
	bad := artifact.ErrInput
	if !strings.HasPrefix(raw, "bytes=") || len(raw) > 128 || size <= 0 {
		return 0, 0, bad
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
	if !ok {
		return 0, 0, bad
	}
	number := func(s string) (int64, error) {
		if s == "" {
			return 0, bad
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, bad
			}
		}
		return strconv.ParseInt(s, 10, 64)
	}
	if a == "" {
		n, e := number(b)
		if e != nil || n <= 0 {
			return 0, 0, bad
		}
		if n > size {
			n = size
		}
		return size - n, n, nil
	}
	start, e := number(a)
	if e != nil || start >= size {
		return 0, 0, bad
	}
	end := size - 1
	if b != "" {
		end, e = number(b)
		if e != nil || end < start {
			return 0, 0, bad
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, nil
}
