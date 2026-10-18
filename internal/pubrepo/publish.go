package pubrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func (s *Repository) getSession(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sessions[id]
	if v != nil && time.Now().Before(v.expires) {
		return v
	}
	return nil
}
func (s *Repository) upload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	v := s.getSession(id)
	if v == nil {
		failure(w, artifact.ErrNotFound)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Now().After(v.expires) {
		failure(w, artifact.ErrNotFound)
		return
	}
	if r.URL.Path != v.uploadPath || v.path != "" || v.done {
		failure(w, artifact.ErrConflict)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxArchive+1<<20)
	mr, e := r.MultipartReader()
	if e != nil {
		failure(w, errInvalid)
		return
	}
	part, e := mr.NextPart()
	if e != nil || part.FormName() != "file" {
		failure(w, errInvalid)
		return
	}
	f, e := os.CreateTemp("", "ant-pub-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer f.Close()
	keep := false
	defer func() {
		if !keep {
			os.Remove(f.Name())
		}
	}()
	n, e := io.CopyBuffer(f, io.LimitReader(contextReader{r.Context(), part}, maxArchive+1), make([]byte, 64<<10))
	if e != nil || n > maxArchive {
		failure(w, errInvalid)
		return
	}
	if _, e = mr.NextPart(); e != io.EOF {
		failure(w, errInvalid)
		return
	}
	v.path = f.Name()
	keep = true
	base, _ := s.base.Load().(string)
	w.Header().Set("Location", base+strings.Replace(v.uploadPath, "/uploads/", "/finalize/", 1))
	w.WriteHeader(204)
}
func (s *Repository) finalize(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id string) {
	v := s.getSession(id)
	if v == nil {
		failure(w, artifact.ErrNotFound)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Now().After(v.expires) {
		failure(w, artifact.ErrNotFound)
		return
	}
	if v.feed != feed || v.token != p.TokenID {
		failure(w, artifact.ErrDenied)
		return
	}
	if v.done {
		writeJSON(w, r, map[string]any{"success": map[string]string{"message": "Already published"}})
		return
	}
	if v.path == "" {
		failure(w, errInvalid)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	f, e := os.Open(v.path)
	if e != nil {
		failure(w, e)
		return
	}
	defer f.Close()
	spec, e := inspect(ctx, f)
	if e != nil {
		failure(w, e)
		return
	}
	name := spec["name"].(string)
	version := spec["version"].(string)
	data, e := json.Marshal(spec)
	if e != nil {
		failure(w, errInvalid)
		return
	}
	pubs := make([]artifact.Publication, 2)
	for i, key := range []string{"archive", "pubspec"} {
		source, e := s.service.Prepare(ctx, p, feed, "", name, key)
		if e != nil {
			failure(w, e)
			return
		}
		if source != v.source {
			failure(w, artifact.ErrConflict)
			return
		}
		pubs[i] = artifact.Publication{ExpectedSource: v.source, FeedID: feed, Coordinate: name, Version: version, AssetKey: key}
	}
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	h := sha256.New()
	n, e := io.CopyBuffer(h, contextReader{ctx, f}, make([]byte, 64<<10))
	if e != nil {
		failure(w, e)
		return
	}
	pubs[0].Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(h.Sum(nil)))}
	pubs[0].MediaType = "application/gzip"
	pubs[1].Info = info(data)
	pubs[1].MediaType = "application/json"
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	_, e = s.service.PublishBundle(ctx, p, pubs, []io.Reader{contextReader{ctx, f}, bytes.NewReader(data)})
	if e != nil {
		failure(w, e)
		return
	}
	v.done = true
	os.Remove(v.path)
	v.path = ""
	writeJSON(w, r, map[string]any{"success": map[string]string{"message": "Published"}})
}
