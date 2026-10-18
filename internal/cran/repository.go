// Package cran implements local CRAN source repositories; see README.md for gates.
package cran

import (
	"bytes"
	"context"
	"crypto/md5" // CRAN's native integrity metadata, alongside the core SHA-256.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const channel = "src/contrib"

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid CRAN configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4)}, nil
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
		w.Header().Set("WWW-Authenticate", `Basic realm="cran"`)
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
	var extra extraRoute
	isExtra := false
	if len(a) > 6 && a[0] == "feeds" && a[2] == "cran" {
		extra, isExtra = parseExtra(a)
	}
	if !isExtra && (len(a) != 6 || a[3] != "src" || a[4] != "contrib") || len(a) < 6 || a[0] != "feeds" || a[2] != "cran" || p.InstanceID != s.instance || strings.Contains(r.URL.EscapedPath(), "%") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='cran'`, s.instance, a[1]).Scan(&feed)
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
	if isExtra {
		if r.Method == "PUT" {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(405)
			return
		}
		s.serveExtra(w, r, p, feed, extra)
		return
	}
	if (a[5] == "PACKAGES" || a[5] == "PACKAGES.gz") && r.Method != "PUT" {
		s.index(w, r, p, feed, a[5])
		return
	}
	stem := strings.TrimSuffix(a[5], ".tar.gz")
	name, version, ok := strings.Cut(stem, "_")
	if stem == a[5] || !ok || !validName(name) || !validVersion(version) {
		failure(w, artifact.ErrNotFound)
		return
	}
	pub := artifact.Publication{FeedID: feed, Namespace: channel, Coordinate: channel, Version: stem, AssetKey: "archive", MediaType: "application/gzip"}
	if r.Method == "PUT" {
		s.ingest(w, r, p, pub, name, version)
		return
	}
	s.download(w, r, p, pub)
}
func (s *Repository) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, name, version string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e == nil && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: pub.FeedID, Kind: identity.FeedResource}, identity.Admin) {
		e = artifact.ErrDenied
	}
	if e == nil {
		pub.ExpectedSource, e = s.service.Prepare(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, pub.AssetKey)
	}
	cancel()
	if e != nil {
		failure(w, e)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	f, e := os.CreateTemp("", "ant-cran-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sha := sha256.New()
	md := md5.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, sha, md), io.LimitReader(contextReader{r.Context(), r.Body}, maxArchive+1), make([]byte, 64<<10))
	if e != nil || n > maxArchive {
		failure(w, errInvalid)
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	meta, e := inspect(ctx, f, name, version)
	if e != nil {
		failure(w, e)
		return
	}
	meta["MD5sum"] = hex.EncodeToString(md.Sum(nil))
	b, e := json.Marshal(meta)
	if e != nil || len(b) > maxMetadata {
		failure(w, errInvalid)
		return
	}
	pub.Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(sha.Sum(nil)))}
	metadata := pub
	metadata.AssetKey = "DESCRIPTION"
	metadata.MediaType = "application/json"
	metadata.Info = info(b)
	source, e := s.service.Prepare(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, metadata.AssetKey)
	if e != nil {
		failure(w, e)
		return
	}
	if source != pub.ExpectedSource {
		failure(w, artifact.ErrConflict)
		return
	}
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	_, e = s.service.PublishBundle(ctx, p, []artifact.Publication{pub, metadata}, []io.Reader{contextReader{ctx, f}, bytes.NewReader(b)})
	if e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
}
