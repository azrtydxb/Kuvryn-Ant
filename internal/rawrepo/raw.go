// Package rawrepo serves immutable files. Each normalized full path owns its own
// coordinate; version "first" is stable across retries and conflicting PUTs.
package rawrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrConfiguration = errors.New("invalid raw repository configuration")

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	instance  string
	auth      http.Handler
	transfers chan struct{}
}

func New(db *pgxpool.Pool, service *artifact.Service) (*Repository, error) {
	if db == nil || service == nil || service.Lease == nil || service.Lease().InstanceID == "" {
		return nil, ErrConfiguration
	}
	r := &Repository{db: db, service: service, instance: service.Lease().InstanceID, transfers: make(chan struct{}, 4)}
	r.auth = identity.NewBearer(identity.Tokens{DB: db}).Wrap(http.HandlerFunc(r.serve))
	return r, nil
}
func (r *Repository) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	req = req.Clone(req.Context())
	req.Body = idleBody{ReadCloser: req.Body, ctx: req.Context(), controller: controller}
	r.auth.ServeHTTP(&idleWriter{ResponseWriter: w, controller: controller}, req)
}

func parsePath(req *http.Request) (slug, key string, err error) {
	raw := req.URL.EscapedPath()
	normalized, err := routing.NormalizePath(raw)
	if err != nil || strings.HasSuffix(raw, "/") {
		return "", "", artifact.ErrInput
	}
	parts := strings.SplitN(normalized, "/", 5)
	if len(parts) != 5 || parts[1] != "feeds" || parts[3] != "raw" || len(parts[2]) > 128 || len(parts[4]) > 256 {
		return "", "", artifact.ErrInput
	}
	return parts[2], parts[4], nil
}
func (r *Repository) serve(w http.ResponseWriter, req *http.Request) {
	p, ok := identity.PrincipalFromContext(req.Context())
	if !ok || p.InstanceID != r.instance {
		fail(w, artifact.ErrNotFound)
		return
	}
	slug, key, err := parsePath(req)
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	var feed string
	err = r.db.QueryRow(ctx, "SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='raw'", p.InstanceID, slug).Scan(&feed)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		err = artifact.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	namespace := ""
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		namespace = key[:i]
	}
	pub := artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: key, Version: "first", AssetKey: key}
	switch req.Method {
	case http.MethodPut:
		r.put(w, req, p, pub)
	case http.MethodGet, http.MethodHead:
		r.get(w, req, p, pub)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "method unsupported; deletion requires an explicit policy", http.StatusMethodNotAllowed)
	}
}
func (r *Repository) put(w http.ResponseWriter, req *http.Request, p identity.Principal, pub artifact.Publication) {
	// Conditional writes need an atomic policy operation in the core. Never
	// implement them with a racy Lookup followed by Publish.
	for _, h := range []string{"If-Match", "If-None-Match", "If-Unmodified-Since", "Content-Range", "Content-Encoding"} {
		if len(req.Header.Values(h)) > 0 {
			http.Error(w, "conditional writes, partial writes and encoded uploads unsupported", http.StatusNotImplemented)
			return
		}
	}
	digest := req.Header.Get("X-Checksum-Sha256")
	if len(req.Header.Values("X-Checksum-Sha256")) > 1 || (digest != "" && !blob.Digest(digest).Valid()) {
		fail(w, artifact.ErrInput)
		return
	}
	pub.MediaType = req.Header.Get("Content-Type")
	if pub.MediaType == "" {
		pub.MediaType = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(pub.MediaType); err != nil || len(pub.MediaType) > 256 {
		fail(w, artifact.ErrInput)
		return
	}
	source, err := r.service.Prepare(req.Context(), p, pub.FeedID, pub.Namespace, pub.Coordinate, pub.AssetKey)
	if err != nil {
		fail(w, err)
		return
	}
	pub.ExpectedSource = source
	select {
	case r.transfers <- struct{}{}:
		defer func() { <-r.transfers }()
	case <-req.Context().Done():
		return
	}
	pub.Info = blob.Info{Digest: blob.Digest(digest), Size: req.ContentLength}
	var body io.Reader = req.Body
	if digest == "" || req.ContentLength < 0 {
		file, err := os.CreateTemp("", "ant-raw-*")
		if err != nil {
			fail(w, err)
			return
		}
		defer os.Remove(file.Name())
		defer file.Close()
		hash := sha256.New()
		reader := io.Reader(req.Body)
		if req.ContentLength >= 0 {
			reader = io.LimitReader(reader, req.ContentLength)
		}
		size, err := io.CopyBuffer(io.MultiWriter(file, hash), reader, make([]byte, 64*1024))
		if err != nil {
			fail(w, err)
			return
		}
		if req.ContentLength >= 0 {
			var extra [1]byte
			n, e := io.ReadFull(req.Body, extra[:])
			if size != req.ContentLength || n != 0 || e != io.EOF {
				fail(w, blob.ErrIntegrity)
				return
			}
		}
		actual := hex.EncodeToString(hash.Sum(nil))
		if digest != "" && actual != digest {
			fail(w, blob.ErrIntegrity)
			return
		}
		pub.Info = blob.Info{Digest: blob.Digest(actual), Size: size}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			fail(w, err)
			return
		}
		body = file
	}
	record, err := r.service.Publish(req.Context(), p, pub, body)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(record))
	w.Header().Set("X-Checksum-Sha256", string(record.Info.Digest))
	if record.Replayed {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}
func etag(rec artifact.Record) string { return `"sha256:` + string(rec.Info.Digest) + `"` }
func fail(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, artifact.ErrInput), errors.Is(err, blob.ErrIntegrity):
		status = http.StatusBadRequest
	case errors.Is(err, artifact.ErrDenied):
		status = http.StatusForbidden
	case errors.Is(err, artifact.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, artifact.ErrConflict):
		status = http.StatusConflict
	}
	http.Error(w, http.StatusText(status), status)
}
