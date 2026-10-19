// Package p2repo serves Eclipse p2 repositories; see README.md.
package p2repo

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const namespace = "p2"
const manifestCoordinate = "manifest"

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
	mu        sync.Mutex // serializes publication so merge/conflict checks are race free
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid p2 configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: artifact.NewUploadSlots()}, nil
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
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="p2"`)
		w.WriteHeader(401)
		return
	}
	if _, password, ok := r.BasicAuth(); ok {
		r.Header.Set("Authorization", "Bearer "+password)
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}

var fileRE = regexp.MustCompile(`^(.+?)_(\d.*)$`)

var dirClassifier = map[string]string{"plugins": "osgi.bundle", "features": "org.eclipse.update.feature", "binary": "binary"}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || len(a) > 5 || a[0] != "feeds" || a[2] != "p2" || p.InstanceID != s.instance || strings.Contains(r.URL.EscapedPath(), "%") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='p2'`, s.instance, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "PUT" {
		w.Header().Set("Allow", "GET, HEAD, PUT")
		w.WriteHeader(405)
		return
	}
	if len(a) == 4 && a[3] == "repository.zip" {
		if r.Method != "PUT" {
			w.Header().Set("Allow", "PUT")
			w.WriteHeader(405)
			return
		}
		s.ingest(w, r, p, feed, a[1])
		return
	}
	if r.Method == "PUT" {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(405)
		return
	}
	if len(a) == 4 {
		switch a[3] {
		case "p2.index", "content.xml", "content.jar", "content.xml.xz", "artifacts.xml", "artifacts.jar", "artifacts.xml.xz":
			s.index(w, r, p, feed, a[1], a[3])
			return
		}
		failure(w, artifact.ErrNotFound)
		return
	}
	classifier, ok := dirClassifier[a[3]]
	stem := a[4]
	if ok && classifier != "binary" {
		var cut bool
		stem, cut = strings.CutSuffix(stem, ".jar")
		ok = cut
	}
	m := fileRE.FindStringSubmatch(stem)
	if !ok || m == nil || !validID(m[1]) || !validVersion(m[2]) {
		failure(w, artifact.ErrNotFound)
		return
	}
	pub := artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: "artifact/" + classifier + "/" + m[1], Version: m[2], AssetKey: "jar", MediaType: "application/java-archive"}
	if _, e := s.service.Lookup(r.Context(), p, pub); errors.Is(e, artifact.ErrNotFound) {
		_, served, e := s.pullArtifact(w, r, p, pub, classifier, m[1], m[2])
		if served {
			return
		}
		if e != nil {
			failure(w, e)
			return
		}
	}
	s.download(w, r, p, pub)
}
