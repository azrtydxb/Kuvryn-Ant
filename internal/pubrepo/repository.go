// Package pubrepo implements local authenticated Pub hosted repositories.
package pubrepo

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type session struct {
	mu                                    sync.Mutex
	feed, source, token, path, uploadPath string
	expires                               time.Time
	done                                  bool
}
type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	mu        sync.Mutex
	sessions  map[string]*session
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Pub configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, sessions: map[string]*session{}, transfers: make(chan struct{}, 4)}, nil
}

// Close removes private staged uploads. Call after stopping HTTP serving.
func (s *Repository) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.sessions {
		v.mu.Lock()
		os.Remove(v.path)
		v.mu.Unlock()
		delete(s.sessions, k)
	}
}
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(r.Context())
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	// Multipart uploads carry a random, short-lived capability, as native Pub does
	// not send repository Authorization headers on the storage POST.
	if r.Method == "POST" && strings.Contains(r.URL.Path, "/pub/uploads/") {
		s.upload(w, r)
		return
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(&challengeWriter{w}, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "pub" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='pub'`, p.InstanceID, a[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	base, _ := s.base.Load().(string)
	if base == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	base += "/feeds/" + url.PathEscape(a[1]) + "/pub/"
	route := strings.Join(a[3:], "/")
	if (r.Method == "DELETE" || r.Method == "PUT") && len(a) >= 8 && a[3] == "api" && a[4] == "packages" && namePattern.MatchString(a[5]) && a[6] == "versions" && validVersion(a[7]) {
		// pub.dev has no client-side delete. DELETE removes the version and
		// PUT/DELETE .../retract retracts (unlists) / restores it; all are
		// policy-gated by the shared artifact core.
		t := artifact.Target{FeedID: feed, Coordinate: a[5], Version: a[7]}
		var e error
		switch {
		case len(a) == 8 && r.Method == "DELETE":
			_, e = s.service.Delete(ctx, p, t)
		case len(a) == 9 && a[8] == "retract" && r.Method == "PUT":
			_, e = s.service.Unlist(ctx, p, t)
		case len(a) == 9 && a[8] == "retract" && r.Method == "DELETE":
			_, e = s.service.Relist(ctx, p, t)
		default:
			w.WriteHeader(405)
			return
		}
		if e != nil {
			failure(w, e)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	if route == "api/packages/versions/new" && r.Method == "GET" {
		source, e := s.service.Prepare(r.Context(), p, feed, "", "pub-upload", "archive")
		if e != nil {
			failure(w, e)
			return
		}
		s.mu.Lock()
		for k, v := range s.sessions {
			if time.Now().After(v.expires) && v.mu.TryLock() {
				os.Remove(v.path)
				v.mu.Unlock()
				delete(s.sessions, k)
			}
		}
		if len(s.sessions) >= 64 {
			s.mu.Unlock()
			failure(w, artifact.ErrUnavailable)
			return
		}
		id := rand.Text()
		s.sessions[id] = &session{feed: feed, source: source, token: p.TokenID, uploadPath: "/feeds/" + a[1] + "/pub/uploads/" + id, expires: time.Now().Add(10 * time.Minute)}
		s.mu.Unlock()
		writeJSON(w, r, map[string]any{"url": base + "uploads/" + id, "fields": map[string]string{}})
		return
	}
	if len(a) == 5 && a[3] == "finalize" && r.Method == "GET" {
		s.finalize(w, r, p, feed, a[4])
		return
	}
	if len(a) >= 6 && a[3] == "api" && a[4] == "packages" && namePattern.MatchString(a[5]) {
		if len(a) == 6 {
			s.index(w, r, p, feed, a[5], "", base)
			return
		}
		if len(a) == 8 && a[6] == "versions" && validVersion(a[7]) {
			s.index(w, r, p, feed, a[5], a[7], base)
			return
		}
	}
	if len(a) == 7 && a[3] == "packages" && a[5] == "versions" && namePattern.MatchString(a[4]) {
		v := strings.TrimSuffix(a[6], ".tar.gz")
		if v != a[6] && validVersion(v) {
			s.download(w, r, p, artifact.Publication{FeedID: feed, Coordinate: a[4], Version: v, AssetKey: "archive"})
			return
		}
	}
	failure(w, artifact.ErrNotFound)
}
