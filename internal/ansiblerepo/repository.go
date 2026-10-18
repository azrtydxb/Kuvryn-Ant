// Package ansiblerepo implements the local Galaxy v3 collection workflow.
package ansiblerepo

import (
	"context"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type importTask struct {
	token, feed string
	expires     time.Time
	result      map[string]any
}
type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
	mu        sync.Mutex
	tasks     map[string]importTask
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Galaxy configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4), tasks: map[string]importTask{}}, nil
}
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	if len(r.Header.Values("Authorization")) == 1 {
		fields := strings.Fields(r.Header.Get("Authorization"))
		if len(fields) == 2 && strings.EqualFold(fields[0], "Token") {
			r.Header.Set("Authorization", "Bearer "+fields[1])
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "ansible" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='ansible'`, s.instance, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	origin, _ := s.base.Load().(string)
	if origin == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	base := origin + "/feeds/" + url.PathEscape(a[1]) + "/ansible/"
	route := strings.TrimSuffix(strings.Join(a[3:], "/"), "/")
	if r.Method == "POST" && route == "v3/artifacts/collections" {
		s.publish(w, r, p, feed, base)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	if route == "" {
		writeJSON(w, r, map[string]any{"available_versions": map[string]string{"v3": "v3/"}})
		return
	}
	parts := strings.Split(route, "/")
	if len(parts) == 3 && parts[0] == "v3" && parts[1] == "imports" {
		s.mu.Lock()
		task, ok := s.tasks[parts[2]]
		s.mu.Unlock()
		if !ok || task.feed != feed || task.token != p.TokenID || time.Now().After(task.expires) {
			failure(w, artifact.ErrNotFound)
			return
		}
		writeJSON(w, r, task.result)
		return
	}
	if len(parts) >= 4 && parts[0] == "v3" && parts[1] == "collections" && namePattern.MatchString(parts[2]) && namePattern.MatchString(parts[3]) {
		ns, name := parts[2], parts[3]
		if len(parts) == 4 {
			s.index(w, r, p, feed, ns, name, "", base, true)
			return
		}
		if len(parts) == 5 && parts[4] == "versions" {
			s.index(w, r, p, feed, ns, name, "", base, false)
			return
		}
		if len(parts) == 6 && parts[4] == "versions" && validVersion(parts[5]) {
			s.index(w, r, p, feed, ns, name, parts[5], base, false)
			return
		}
	}
	if len(parts) == 5 && parts[0] == "downloads" && namePattern.MatchString(parts[1]) && namePattern.MatchString(parts[2]) && validVersion(parts[3]) && parts[4] == "collection.tar.gz" {
		s.download(w, r, p, artifact.Publication{FeedID: feed, Namespace: parts[1], Coordinate: parts[1] + "." + parts[2], Version: parts[3], AssetKey: "archive"})
		return
	}
	failure(w, artifact.ErrNotFound)
}
