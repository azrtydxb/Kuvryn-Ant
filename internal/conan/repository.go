// Package conan implements a bounded local Conan 2 revision repository.
package conan

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

var errInvalid = errors.New("invalid Conan revision")
var atom = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]{0,63}$`)
var revision = regexp.MustCompile(`^[0-9a-f]{32}$`)
var packageID = regexp.MustCompile(`^[0-9a-f]{40}$`)

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

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Conan configuration")
	}
	return &Repository{db: db, service: s, instance: s.Lease().InstanceID, auth: identity.NewBearer(identity.Tokens{DB: db}), transfers: artifact.NewUploadSlots(), stages: map[string]*stage{}}, nil
}

type reference struct{ coordinate, namespace, rrev, pid, prev string }

func (v reference) version() string {
	if v.pid != "" {
		return v.rrev + ":" + v.pid + ":" + v.prev
	}
	return v.rrev
}
func (v reference) pub(feed, key string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: v.namespace, Coordinate: v.coordinate, Version: v.version(), AssetKey: key}
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 5 || a[0] != "feeds" || a[2] != "conan" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='conan'`, p.InstanceID, a[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	route := strings.Join(a[3:], "/")
	if r.Method == "GET" || r.Method == "HEAD" {
		switch route {
		case "v1/ping":
			w.Header().Set("X-Conan-Server-Capabilities", "revisions")
			w.WriteHeader(200)
			return
		case "v2/users/authenticate":
			// The machine token is already verified by shared middleware. Conan treats
			// the returned opaque string as its bearer; no parallel token authority.
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			// PATs and directory logins reach here as single-use tickets: hand back
			// a reusable credential instead.
			secret, ok, err := identity.LoginToken(r.Context(), s.db, p, "conan remote login", 12*time.Hour)
			if err != nil {
				failure(w, artifact.ErrUnavailable)
				return
			}
			if ok {
				token = secret
			}
			w.Header().Set("Content-Type", "text/plain")
			if r.Method != "HEAD" {
				_, _ = w.Write([]byte(token))
			}
			return
		case "v2/users/check_credentials":
			w.WriteHeader(200)
			return
		case "v2/conans/search":
			s.search(w, r, p, feed)
			return
		}
	}
	if a[3] != "v2" || a[4] != "conans" || len(a) < 9 || (len(a) == 9 && r.Method != "DELETE") {
		failure(w, artifact.ErrNotFound)
		return
	}
	for _, part := range a[5:9] {
		if !atom.MatchString(part) || part == "." || part == ".." {
			failure(w, errInvalid)
			return
		}
	}
	v := reference{coordinate: strings.Join(a[5:9], "/"), namespace: a[7]}
	if v.namespace == "_" {
		v.namespace = ""
	}
	if len(v.coordinate) > 256 {
		failure(w, errInvalid)
		return
	}
	tail := a[9:]
	if len(tail) >= 2 && tail[0] == "revisions" {
		if !revision.MatchString(tail[1]) {
			failure(w, errInvalid)
			return
		}
		v.rrev = tail[1]
		tail = tail[2:]
		if len(tail) >= 2 && tail[0] == "packages" {
			if !packageID.MatchString(tail[1]) {
				failure(w, errInvalid)
				return
			}
			v.pid = tail[1]
			tail = tail[2:]
			if len(tail) >= 2 && tail[0] == "revisions" {
				if !revision.MatchString(tail[1]) {
					failure(w, errInvalid)
					return
				}
				v.prev = tail[1]
				tail = tail[2:]
			}
		}
	}
	if r.Method == "DELETE" && len(tail) == 0 {
		s.remove(w, r, p, feed, v)
		return
	}
	if len(tail) == 2 && tail[0] == "files" && v.rrev != "" && (v.pid == "" || v.prev != "") {
		if !allowedFile(v, tail[1]) {
			failure(w, errInvalid)
			return
		}
		switch r.Method {
		case "PUT":
			s.upload(w, r, p, feed, v, tail[1])
			return
		case "GET", "HEAD":
			s.download(w, r, p, feed, v, tail[1])
			return
		}
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	if len(tail) == 1 {
		s.metadata(w, r, p, feed, v, tail[0])
		return
	}
	failure(w, artifact.ErrNotFound)
}
func allowedFile(v reference, key string) bool {
	if key == "conanmanifest.txt" {
		return true
	}
	if v.pid != "" {
		return key == "conaninfo.txt" || key == "conan_package.tgz"
	}
	return key == "conanfile.py" || key == "conan_export.tgz" || key == "conan_sources.tgz"
}

// remove implements the Conan v2 REST deletes: a reference (all revisions), one
// recipe revision with its packages, the revisions of one package id, or one
// package revision. Each stored version is removed through the policy-gated
// artifact core (authorization, delete policy, audit, outbox, tombstones).
func (s *Repository) remove(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, v reference) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	prefix := ""
	switch {
	case v.pid == "" && v.rrev != "":
		prefix = v.rrev
	case v.pid != "" && v.prev == "":
		prefix = v.rrev + ":" + v.pid + ":"
	case v.prev != "":
		prefix = v.version()
	}
	all, e := s.service.ListVersions(ctx, p.InstanceID, feed, v.coordinate, prefix, 10000)
	if e != nil {
		failure(w, e)
		return
	}
	var versions []string
	for _, ver := range all {
		switch {
		case v.rrev == "", v.pid != "":
			versions = append(versions, ver)
		case ver == v.rrev || strings.HasPrefix(ver, v.rrev+":"):
			versions = append(versions, ver)
		}
	}
	if len(versions) == 0 {
		failure(w, artifact.ErrNotFound)
		return
	}
	for _, ver := range versions {
		if _, e = s.service.Delete(ctx, p, artifact.Target{FeedID: feed, Namespace: v.namespace, Coordinate: v.coordinate, Version: ver}); e != nil {
			failure(w, e)
			return
		}
	}
	w.WriteHeader(200)
}
