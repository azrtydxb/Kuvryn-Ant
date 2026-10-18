// Package aptrepo implements local APT/Debian repositories; see README.md for gates.
package aptrepo

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository serves /feeds/{slug}/apt/. Index publication is serialized per feed
// in-process; the core ownership lease guarantees a single writing instance.
type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	keys      atomic.Value
	transfers chan struct{}
	locks     sync.Map
	now       func() time.Time
	validity  time.Duration
}

// New validates the service lease and returns an unmounted repository.
func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid APT configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4), now: time.Now, validity: 30 * 24 * time.Hour}, nil
}

// SetValidity changes the Valid-Until horizon for newly signed Release files.
func (s *Repository) SetValidity(d time.Duration) {
	if d > 0 {
		s.validity = d
	}
}

func (s *Repository) feedLock(feed string) *sync.Mutex {
	m, _ := s.locks.LoadOrStore(feed, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ServeHTTP authenticates with a Bearer token or a Basic password holding one.
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(r.Context())
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="apt"`)
		w.WriteHeader(401)
		return
	}
	if _, password, ok := r.BasicAuth(); ok {
		r.Header.Set("Authorization", "Bearer "+password)
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "apt" || p.InstanceID != s.instance || strings.Contains(r.URL.EscapedPath(), "%") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='apt'`, s.instance, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	rest := a[3:]
	read := r.Method == "GET" || r.Method == "HEAD"
	switch {
	case rest[0] == "dists" && len(rest) >= 3 && namePattern.MatchString(rest[1]) && read:
		s.dist(w, r, p, feed, rest[1], rest[2:])
	case len(rest) == 1 && (rest[0] == "signing-key.asc" || rest[0] == "signing-key.gpg") && read:
		s.key(w, r, p, feed, rest[0] == "signing-key.gpg")
	case rest[0] == "pool" && len(rest) == 5 && namePattern.MatchString(rest[1]):
		s.pool(w, r, p, feed, rest[1], rest[2]+"/"+rest[3], rest[4])
	default:
		failure(w, artifact.ErrNotFound)
	}
}
