// Package huggingface implements a local Hugging Face Hub-compatible model and
// dataset repository (HF_ENDPOINT subset) with an authenticated ingest API.
package huggingface

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	namespace     = "huggingface"
	manifestKey   = ".ant-manifest.json"
	refKey        = "ref"
	tombstone     = "deleted"
	maxFileSize   = 1 << 40
	maxSessionFil = 10000
	maxSessions   = 64
	maxManifest   = 8 << 20
	stageTTL      = time.Hour
)

var (
	errInvalid  = errors.New("invalid Hugging Face request")
	errChecksum = errors.New("checksum mismatch")
	nameRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	refRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	commitRe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sessionRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	keywords    = map[string]bool{"revision": true, "tree": true, "resolve": true, "ant-upload": true, "ant-commit": true, "ant-ref": true, "refs": true}
)

// Repository serves huggingface feeds.
type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
	mu        sync.Mutex
	stages    map[string]*stage
}

// New returns a handler for feeds with format huggingface.
func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Hugging Face configuration")
	}
	return &Repository{db: db, service: s, instance: s.Lease().InstanceID, auth: identity.NewBearer(identity.Tokens{DB: db}), transfers: artifact.NewUploadSlots(), stages: map[string]*stage{}}, nil
}

// SetPublicURL configures the trusted external origin before serving.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid Hugging Face public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("Hugging Face public URL requires HTTPS or literal loopback")
	}
	s.base.Store(raw)
	return nil
}

// target identifies a repository inside a feed.
type target struct {
	feed, typ, repo string
}

func (t target) coordinate() string { return t.typ + "/" + t.repo }
func (t target) refCoordinate() string {
	return "refs/" + t.typ + "/" + t.repo
}

// parseRepo splits "org/name/keyword/..." or "name/keyword/..." paths.
func parseRepo(segs []string) (repo string, tail []string, ok bool) {
	valid := func(n int) bool {
		for _, x := range segs[:n] {
			if !nameRe.MatchString(x) || keywords[x] || strings.Contains(x, "..") {
				return false
			}
		}
		return true
	}
	if len(segs) >= 1 && (len(segs) == 1 || keywords[segs[1]]) && valid(1) {
		return segs[0], segs[1:], true
	}
	if len(segs) >= 2 && (len(segs) == 2 || keywords[segs[2]]) && valid(2) {
		return segs[0] + "/" + segs[1], segs[2:], true
	}
	return "", nil, false
}

func validPath(p string) bool {
	if p == "" || len(p) > 1024 || p == manifestKey || strings.ContainsAny(p, "\\\x00\r\n") {
		return false
	}
	for _, x := range strings.Split(p, "/") {
		if x == "" || x == "." || x == ".." {
			return false
		}
	}
	return true
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "huggingface" || p.InstanceID != s.instance || len(r.URL.Path) > 2048 || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='huggingface'`, p.InstanceID, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	rest := a[3:]
	t := target{feed: feed}
	switch {
	case rest[0] == "api" && len(rest) == 2 && rest[1] == "whoami-v2":
		writeJSON(w, r, map[string]any{"type": "user", "name": p.PrincipalID, "auth": map[string]any{"type": "access_token"}})
	case rest[0] == "api" && len(rest) >= 3 && (rest[1] == "models" || rest[1] == "datasets"):
		t.typ = rest[1]
		repo, tail, ok := parseRepo(rest[2:])
		if !ok {
			failure(w, artifact.ErrNotFound)
			return
		}
		t.repo = repo
		s.api(w, r, p, t, tail)
	case rest[0] != "api":
		segs := rest
		t.typ = "models"
		if rest[0] == "datasets" {
			t.typ, segs = "datasets", rest[1:]
		}
		repo, tail, ok := parseRepo(segs)
		if !ok || len(tail) < 3 || tail[0] != "resolve" {
			failure(w, artifact.ErrNotFound)
			return
		}
		t.repo = repo
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(405)
			return
		}
		s.resolve(w, r, p, t, tail[1], strings.Join(tail[2:], "/"))
	default:
		failure(w, artifact.ErrNotFound)
	}
}

func (s *Repository) api(w http.ResponseWriter, r *http.Request, p identity.Principal, t target, tail []string) {
	read := r.Method == "GET" || r.Method == "HEAD"
	switch {
	case len(tail) == 0 && read:
		s.info(w, r, p, t, "main")
	case len(tail) == 2 && tail[0] == "revision" && read:
		s.info(w, r, p, t, tail[1])
	case len(tail) >= 2 && tail[0] == "tree" && read:
		s.tree(w, r, p, t, tail[1], strings.Join(tail[2:], "/"))
	case len(tail) >= 3 && tail[0] == "ant-upload" && r.Method == "PUT":
		s.upload(w, r, p, t, tail[1], strings.Join(tail[2:], "/"))
	case len(tail) == 2 && tail[0] == "ant-commit" && r.Method == "POST":
		s.commit(w, r, p, t, tail[1])
	case len(tail) == 2 && tail[0] == "ant-ref" && r.Method == "DELETE":
		s.deleteRef(w, r, p, t, tail[1])
	case len(tail) == 0 || keywords[tail[0]]:
		w.WriteHeader(405)
	default:
		failure(w, artifact.ErrNotFound)
	}
}

func isNotFound(e error) bool {
	return errors.Is(e, artifact.ErrNotFound) || errors.Is(e, blob.ErrNotFound)
}
