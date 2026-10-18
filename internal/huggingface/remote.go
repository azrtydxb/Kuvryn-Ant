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
)

// Pull-through for Hugging Face Hub upstreams (huggingface.co style). The source
// origin is the hub endpoint; the file CDN hosts the hub redirects to (cdn-lfs,
// xethub, ...) must be configured as redirect origins of the source. The source's
// credential, if any, is attached to hub requests only (gated and private repos);
// neither the client's token nor the source credential ever reaches a CDN host.
//
// Layout: files are cached per upstream commit under the repository coordinate
// (models/<repo> or datasets/<repo>), one asset per file path, so a branch or tag is
// only ever a short-lived pointer (the revision document passes through the
// metadata cache, and the resolved commit is remembered as a ref so cached files
// stay reachable by branch name during an outage).
//
//   - api/{models,datasets}/<repo>[/revision/<rev>]: the upstream document, passed
//     through with its commit validated.
//   - api/.../tree/<rev>[/path]: directory listings, built from upstream tree
//     documents (recursive listings walk the directories, bounded).
//   - <repo>/resolve/<rev>/<path>: the file. The sha256 and size come from the
//     upstream tree entry (LFS oid). A miss streams to the client while it is staged;
//     the sha256 is checked at EOF and a mismatch aborts the connection and caches
//     nothing. Range requests on a miss are served from the store after a verified
//     fill that keeps running if the client goes away, so a resumed download hits
//     the cache. HEAD answers from the tree entry. Non-LFS files carry only a git
//     sha1 upstream, so they are length checked only.

const (
	maxTreeEntries  = 10000
	maxTreeDirs     = 64
	smallFileCutoff = 16 << 20
	fillTimeout     = 6 * time.Hour
)

func (t target) upstreamResolve(commit, path string) string {
	prefix := ""
	if t.typ == "datasets" {
		prefix = "datasets/"
	}
	return prefix + artifact.EscapePath(t.repo) + "/resolve/" + commit + "/" + artifact.EscapePath(path)
}

func (t target) upstreamAPI(tail string) string {
	return "api/" + t.typ + "/" + artifact.EscapePath(t.repo) + tail
}

func (s *Repository) metadata(ctx context.Context, p identity.Principal, t target, path string) (artifact.Document, error) {
	return s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.coordinate()}, artifact.Origin{Path: path})
}

// remoteRevision resolves rev to an upstream commit. An outage falls back to the
// last commit remembered for the ref.
func (s *Repository) remoteRevision(ctx context.Context, p identity.Principal, t target, rev string) (string, artifact.Document, error) {
	doc, err := s.metadata(ctx, p, t, t.upstreamAPI("/revision/"+rev))
	if err != nil {
		if errors.Is(err, artifact.ErrUnavailable) && !commitRe.MatchString(rev) {
			if commit, e := s.resolveRef(ctx, p, t, rev); e == nil {
				return commit, doc, nil
			}
		}
		return "", doc, err
	}
	var in struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(doc.Body, &in) != nil || !commitRe.MatchString(in.SHA) {
		return "", doc, artifact.ErrUnavailable
	}
	if !commitRe.MatchString(rev) {
		s.rememberRef(ctx, p, t, rev, in.SHA, doc.SourceID)
	}
	return in.SHA, doc, nil
}

// rememberRef records the commit a ref resolved to (best effort), unless it is
// already the newest remembered value.
func (s *Repository) rememberRef(ctx context.Context, p identity.Principal, t target, rev, commit, source string) {
	if current, err := s.resolveRef(ctx, p, t, rev); err == nil && current == commit {
		return
	}
	pub := artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.refCoordinate(), Version: fmt.Sprintf("%s@%020d", rev, time.Now().UnixNano()), AssetKey: refKey, MediaType: "text/plain"}
	_ = s.service.CacheDocument(ctx, p, pub, source, []byte(commit))
}

type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

func (s *Repository) remoteTree(ctx context.Context, p identity.Principal, t target, commit, dir string) ([]treeEntry, error) {
	path := t.upstreamAPI("/tree/" + commit)
	if dir != "" {
		path += "/" + artifact.EscapePath(dir)
	}
	doc, err := s.metadata(ctx, p, t, path)
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	if json.Unmarshal(doc.Body, &entries) != nil || len(entries) > maxTreeEntries {
		return nil, artifact.ErrUnavailable
	}
	out := entries[:0]
	for _, e := range entries {
		if (e.Type == "file" || e.Type == "directory") && validPath(e.Path) && e.Size >= 0 {
			out = append(out, e)
		}
	}
	return out, nil
}

// remoteFile looks up the size and (for LFS files) the sha256 of one file.
func (s *Repository) remoteFile(ctx context.Context, p identity.Principal, t target, commit, path string) (size int64, sha string, err error) {
	dir := ""
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir = path[:i]
	}
	entries, err := s.remoteTree(ctx, p, t, commit, dir)
	if err != nil {
		return 0, "", err
	}
	for _, e := range entries {
		if e.Type != "file" || e.Path != path {
			continue
		}
		if e.LFS != nil && commitSHA256(e.LFS.OID) {
			return e.LFS.Size, e.LFS.OID, nil
		}
		return e.Size, "", nil
	}
	return 0, "", artifact.ErrNotFound
}

func commitSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Repository) infoRemote(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, rev string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	commit, doc, err := s.remoteRevision(ctx, p, t, rev)
	if err != nil {
		failure(w, err)
		return
	}
	if doc.Body == nil {
		failure(w, artifact.ErrUnavailable) // only the remembered commit is known
		return
	}
	var out map[string]any
	if json.Unmarshal(doc.Body, &out) != nil || out["sha"] != commit {
		failure(w, artifact.ErrUnavailable)
		return
	}
	setStale(w, doc)
	writeJSON(w, r, out)
}

func setStale(w http.ResponseWriter, doc artifact.Document) {
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
}

func (s *Repository) treeRemote(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, rev, sub string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	commit, _, err := s.remoteRevision(ctx, p, t, rev)
	if err != nil {
		failure(w, err)
		return
	}
	sub = strings.Trim(sub, "/")
	recursive := r.URL.Query().Get("recursive") != "" && r.URL.Query().Get("recursive") != "false"
	var out []treeEntry
	queue, dirs := []string{sub}, 0
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if dirs++; dirs > maxTreeDirs {
			failure(w, artifact.ErrUnavailable)
			return
		}
		entries, err := s.remoteTree(ctx, p, t, commit, dir)
		if err != nil {
			failure(w, err)
			return
		}
		for _, e := range entries {
			if e.Type == "directory" && recursive {
				queue = append(queue, e.Path)
			}
			out = append(out, e)
		}
		if len(out) > maxTreeEntries {
			failure(w, artifact.ErrUnavailable)
			return
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	items := make([]map[string]any, 0, len(out))
	for _, e := range out {
		item := map[string]any{"type": e.Type, "path": e.Path, "size": e.Size, "oid": ""}
		if e.LFS != nil {
			item["lfs"] = map[string]any{"oid": e.LFS.OID, "size": e.LFS.Size, "pointerSize": 134}
		}
		items = append(items, item)
	}
	writeJSON(w, r, items)
}

// resolveRemote serves a file that no local source owns.
func (s *Repository) resolveRemote(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, rev, path string) {
	ctx := r.Context()
	commit := rev
	pub := artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.coordinate(), AssetKey: path, MediaType: "application/octet-stream"}
	if commitRe.MatchString(rev) {
		pub.Version = commit
		if rec, e := s.service.Lookup(ctx, p, pub); e == nil {
			s.serveStored(w, r, p, pub, rec)
			return
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	commit, _, err := s.remoteRevision(rctx, p, t, rev)
	cancel()
	if err != nil {
		failure(w, err)
		return
	}
	pub.Version = commit
	if rec, e := s.service.Lookup(ctx, p, pub); e == nil {
		s.serveStored(w, r, p, pub, rec)
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	size, sha, err := s.remoteFile(fctx, p, t, commit, path)
	cancel()
	if err != nil {
		failure(w, err)
		return
	}
	origin := artifact.Origin{Path: t.upstreamResolve(commit, path)}
	if sha != "" {
		origin.Digest, origin.StreamVerified = blob.Digest(sha), true
	}
	hdr := w.Header()
	hdr.Set("X-Repo-Commit", commit)
	hdr.Set("X-Linked-Size", strconv.FormatInt(size, 10))
	if sha != "" {
		hdr.Set("ETag", `"`+sha+`"`)
		hdr.Set("X-Linked-Etag", `"`+sha+`"`)
	}
	hdr.Set("Accept-Ranges", "bytes")
	ranged := r.Method == "GET" && r.Header.Get("Range") != ""
	switch {
	case r.Method == "HEAD" && sha != "" && size > 0:
		// Metadata is known without downloading anything.
		hdr.Set("Content-Type", "application/octet-stream")
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(200)
		return
	case ranged && sha != "", r.Method == "HEAD" && size <= smallFileCutoff:
		// Fill the cache completely (verified), then answer from the store. The
		// fill is detached so a client that gives up lets the next attempt hit.
		fill, cancel := context.WithTimeout(context.WithoutCancel(ctx), fillTimeout)
		defer cancel()
		fo := origin
		fo.StreamVerified = false
		stream, again, e := s.service.PullThrough(fill, p, pub, fo)
		if e != nil {
			failure(w, e)
			return
		}
		if !again {
			// No digest: the fill streamed; drain it to complete the cache entry.
			_, e = io.Copy(io.Discard, stream.Body)
			stream.Body.Close()
			if e != nil {
				failure(w, artifact.ErrUnavailable)
				return
			}
		}
		rec, e := s.service.Lookup(ctx, p, pub)
		if e != nil {
			failure(w, artifact.ErrUnavailable)
			return
		}
		s.serveStored(w, r, p, pub, rec)
	default:
		stream, again, e := s.service.PullThrough(ctx, p, pub, origin)
		if e != nil {
			failure(w, e)
			return
		}
		if again {
			rec, e := s.service.Lookup(ctx, p, pub)
			if e != nil {
				failure(w, artifact.ErrUnavailable)
				return
			}
			s.serveStored(w, r, p, pub, rec)
			return
		}
		stream.ServeStream(w, r.Method)
	}
}

// serveStored answers from a cached file with ranges and conditional headers.
func (s *Repository) serveStored(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, rec artifact.Record) {
	ctx := r.Context()
	size := rec.Info.Size
	etag := `"` + string(rec.Info.Digest) + `"`
	offset, length, partial := int64(0), size, false
	if rh := r.Header.Get("Range"); r.Method == "GET" && rh != "" && size > 0 && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		var err error
		if offset, length, err = parseRange(rh, size); err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			http.Error(w, http.StatusText(416), 416)
			return
		}
		partial = true
	}
	openLen := length
	if r.Method == "HEAD" {
		openLen = 0
	}
	got, rc, err := s.service.Open(ctx, p, pub, offset, openLen)
	if err != nil {
		failure(w, err)
		return
	}
	defer rc.Close()
	if got.Info != rec.Info {
		failure(w, blob.ErrIntegrity)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", etag)
	h.Set("X-Linked-Etag", etag)
	h.Set("X-Linked-Size", strconv.FormatInt(size, 10))
	h.Set("X-Repo-Commit", pub.Version)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	code := 200
	if partial {
		code = 206
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size))
	}
	w.WriteHeader(code)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{ctxReader{ctx, rc}}, make([]byte, 64<<10))
	}
}
