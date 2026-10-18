package huggingface

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

type stageFile struct {
	tmp  string
	info blob.Info
}

// stage spools uploaded files to disk until commit; nothing is visible before
// the manifest is published, so partial trees are never served.
type stage struct {
	feed, source string
	files        map[string]*stageFile
	committing   bool
	expires      time.Time
}

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type manifest struct {
	Version int            `json:"version"`
	Files   []manifestFile `json:"files"`
}

func (m manifest) find(path string) (manifestFile, bool) {
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= path })
	if i < len(m.Files) && m.Files[i].Path == path {
		return m.Files[i], true
	}
	return manifestFile{}, false
}

func sessionID(p identity.Principal, t target, session string) string {
	return p.PrincipalID + "\x00" + t.feed + "\x00" + t.coordinate() + "\x00" + session
}

func (s *Repository) acquire() bool {
	select {
	case s.transfers <- struct{}{}:
		return true
	default:
		return false
	}
}
func (s *Repository) release() { <-s.transfers }

func (s *stage) cleanup() {
	for _, f := range s.files {
		_ = os.Remove(f.tmp)
	}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.r.Read(b)
}

// upload spools one file (streamed, bounded memory) into a pending session.
func (s *Repository) upload(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, session, path string) {
	if !sessionRe.MatchString(session) || !validPath(path) {
		failure(w, errInvalid)
		return
	}
	if !s.acquire() {
		w.WriteHeader(429)
		return
	}
	defer s.release()
	ctx := r.Context()
	source, e := s.service.Prepare(ctx, p, t.feed, namespace, t.coordinate(), path)
	if e != nil {
		failure(w, e)
		return
	}
	id := sessionID(p, t, session)
	s.mu.Lock()
	now := time.Now()
	for k, st := range s.stages {
		if now.After(st.expires) && !st.committing {
			st.cleanup()
			delete(s.stages, k)
		}
	}
	st := s.stages[id]
	if st == nil {
		if len(s.stages) >= maxSessions {
			s.mu.Unlock()
			failure(w, artifact.ErrUnavailable)
			return
		}
		st = &stage{feed: t.feed, source: source, files: map[string]*stageFile{}}
		s.stages[id] = st
	}
	if st.committing || st.source != source || (len(st.files) >= maxSessionFil && st.files[path] == nil) {
		s.mu.Unlock()
		failure(w, artifact.ErrConflict)
		return
	}
	st.expires = now.Add(stageTTL)
	s.mu.Unlock()

	tmp, e := os.CreateTemp("", "ant-hf-*")
	if e != nil {
		failure(w, e)
		return
	}
	h := sha256.New()
	lr := &io.LimitedReader{R: ctxReader{ctx, r.Body}, N: maxFileSize + 1}
	n, e := io.CopyBuffer(io.MultiWriter(tmp, h), lr, make([]byte, 64<<10))
	cerr := tmp.Close()
	sum := hex.EncodeToString(h.Sum(nil))
	if e != nil || cerr != nil || n > maxFileSize {
		_ = os.Remove(tmp.Name())
		failure(w, errInvalid)
		return
	}
	if want := r.Header.Get("X-Checksum-Sha256"); want != "" && !strings.EqualFold(want, sum) {
		_ = os.Remove(tmp.Name())
		failure(w, errChecksum)
		return
	}
	s.mu.Lock()
	if cur := s.stages[id]; cur != st || st.committing {
		s.mu.Unlock()
		_ = os.Remove(tmp.Name())
		failure(w, artifact.ErrConflict)
		return
	}
	if old := st.files[path]; old != nil {
		_ = os.Remove(old.tmp)
	}
	st.files[path] = &stageFile{tmp: tmp.Name(), info: blob.Info{Digest: blob.Digest(sum), Size: n}}
	s.mu.Unlock()
	writeJSON(w, r, map[string]any{"path": path, "sha256": sum, "size": n})
}

// commitID derives the immutable revision from the sorted file tree.
func commitID(files []manifestFile) string {
	h := sha1.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\x00%d\n", f.Path, f.SHA256, f.Size)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Repository) commit(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, session string) {
	if !sessionRe.MatchString(session) {
		failure(w, errInvalid)
		return
	}
	var req struct {
		Refs []string `json:"refs"`
	}
	if r.ContentLength != 0 {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if e := d.Decode(&req); e != nil && e != io.EOF {
			failure(w, errInvalid)
			return
		}
	}
	if len(req.Refs) > 16 {
		failure(w, errInvalid)
		return
	}
	for _, name := range req.Refs {
		if !refRe.MatchString(name) || commitRe.MatchString(name) {
			failure(w, errInvalid)
			return
		}
	}
	id := sessionID(p, t, session)
	s.mu.Lock()
	st := s.stages[id]
	if st == nil || st.committing || len(st.files) == 0 {
		s.mu.Unlock()
		failure(w, artifact.ErrNotFound)
		return
	}
	st.committing = true
	s.mu.Unlock()
	release := func(ok bool) {
		s.mu.Lock()
		st.committing = false
		if ok {
			delete(s.stages, id)
		}
		s.mu.Unlock()
		if ok {
			st.cleanup()
		}
	}
	if !s.acquire() {
		release(false)
		w.WriteHeader(429)
		return
	}
	defer s.release()
	ctx := r.Context()
	paths := make([]string, 0, len(st.files))
	for k := range st.files {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	m := manifest{Version: 1}
	for _, k := range paths {
		f := st.files[k]
		m.Files = append(m.Files, manifestFile{Path: k, SHA256: string(f.info.Digest), Size: f.info.Size})
	}
	commit := commitID(m.Files)
	for _, k := range paths {
		f := st.files[k]
		fh, e := os.Open(f.tmp)
		if e != nil {
			release(false)
			failure(w, e)
			return
		}
		pub := artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.coordinate(), Version: commit, AssetKey: k, MediaType: "application/octet-stream", Info: f.info, ExpectedSource: st.source}
		_, e = s.service.Publish(ctx, p, pub, ctxReader{ctx, fh})
		fh.Close()
		if e != nil {
			release(false)
			failure(w, e)
			return
		}
	}
	mb, _ := json.Marshal(m)
	if len(mb) > maxManifest {
		release(false)
		failure(w, errInvalid)
		return
	}
	// The manifest is published last: it atomically makes the revision visible.
	pub := artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.coordinate(), Version: commit, AssetKey: manifestKey, MediaType: "application/json", Info: infoOf(mb), ExpectedSource: st.source}
	if _, e := s.service.Publish(ctx, p, pub, strings.NewReader(string(mb))); e != nil {
		release(false)
		failure(w, e)
		return
	}
	for _, name := range req.Refs {
		if e := s.setRef(ctx, p, t, name, commit, st.source); e != nil {
			release(false)
			failure(w, e)
			return
		}
	}
	release(true)
	writeJSON(w, r, map[string]any{"sha": commit, "refs": req.Refs, "files": len(m.Files)})
}

func infoOf(b []byte) blob.Info {
	sum := sha256.Sum256(b)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(b))}
}

// setRef appends a ref version; the newest version of a name is its value.
func (s *Repository) setRef(ctx context.Context, p identity.Principal, t target, name, value, source string) error {
	pub := artifact.Publication{FeedID: t.feed, Namespace: namespace, Coordinate: t.refCoordinate(), Version: fmt.Sprintf("%s@%020d", name, time.Now().UnixNano()), AssetKey: refKey, MediaType: "text/plain", Info: infoOf([]byte(value)), ExpectedSource: source}
	_, e := s.service.Publish(ctx, p, pub, strings.NewReader(value))
	return e
}

// deleteRef removes a branch/tag pointer (tombstone). Commits stay immutable.
func (s *Repository) deleteRef(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, name string) {
	if !refRe.MatchString(name) || commitRe.MatchString(name) {
		failure(w, errInvalid)
		return
	}
	ctx := r.Context()
	source, e := s.service.Prepare(ctx, p, t.feed, namespace, t.refCoordinate(), refKey)
	if e != nil {
		failure(w, e)
		return
	}
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: t.feed, SourceID: source, Namespace: namespace, Kind: identity.ArtifactResource}, identity.Delete) {
		failure(w, artifact.ErrDenied)
		return
	}
	if dp, pe := s.service.DeletePolicy(ctx, p.InstanceID, t.feed, namespace); pe != nil {
		failure(w, pe)
		return
	} else if dp.Mode == artifact.DeleteDenied {
		failure(w, artifact.ErrPolicy)
		return
	}
	if _, e = s.resolveRef(ctx, p, t, name); e != nil {
		failure(w, e)
		return
	}
	if e = s.setRef(ctx, p, t, name, tombstone, source); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(204)
}
