// Package helmrepo serves classic Helm repositories using shared artifact policy
// and immutable publication. OCI Helm charts belong to the OCI adapter.
package helmrepo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	instance string
	db       *pgxpool.Pool
	service  *artifact.Service
	auth     *identity.Bearer
	Limits   Limits
	uploads  chan struct{}
}

func New(db *pgxpool.Pool, service *artifact.Service) *Repository {
	instance := ""
	if service != nil && service.Lease != nil {
		instance = service.Lease().InstanceID
	}
	return &Repository{instance: instance, db: db, service: service, auth: identity.NewBearer(identity.Tokens{DB: db}), Limits: DefaultLimits(), uploads: artifact.NewUploadSlots()}
}
func (h *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-cache")
	if h.db == nil || h.service == nil {
		fail(w, artifact.ErrUnavailable)
		return
	}
	req := r.Clone(r.Context())
	if len(req.Header.Values("Authorization")) == 1 {
		if _, password, ok := req.BasicAuth(); ok {
			req.Header.Set("Authorization", "Bearer "+password)
		}
	}
	h.auth.Wrap(http.HandlerFunc(h.serve)).ServeHTTP(w, req)
}
func fail(w http.ResponseWriter, err error) {
	status := 503
	switch {
	case errors.Is(err, artifact.ErrInput), errors.Is(err, errChart), errors.Is(err, blob.ErrIntegrity):
		status = 400
	case errors.Is(err, artifact.ErrDenied):
		status = 403
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		status = 404
	case errors.Is(err, artifact.ErrConflict):
		status = 409
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}
func (h *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	if h.instance == "" || p.InstanceID != h.instance {
		fail(w, artifact.ErrNotFound)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "feeds" || parts[2] != "helm" || strings.Contains(r.URL.EscapedPath(), "%") {
		http.NotFound(w, r)
		return
	}
	var feed string
	if err := h.db.QueryRow(r.Context(), "SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='helm'", p.InstanceID, parts[1]).Scan(&feed); err != nil {
		fail(w, err)
		return
	}
	if len(parts) == 4 && parts[3] == "index.yaml" && (r.Method == "GET" || r.Method == "HEAD") {
		h.index(w, r, p, feed)
		return
	}
	if len(parts) < 6 || parts[3] != "charts" || !validCoordinate(parts[4], parts[5]) {
		http.NotFound(w, r)
		return
	}
	pub := artifact.Publication{FeedID: feed, Coordinate: parts[4], Version: parts[5], AssetKey: parts[4] + "-" + parts[5] + ".tgz", MediaType: "application/gzip"}
	if len(parts) == 6 && r.Method == "POST" {
		h.ingest(w, r, p, pub)
		return
	}
	if len(parts) == 6 && r.Method == "DELETE" {
		// ChartMuseum-style `DELETE charts/{name}/{version}`, policy-gated by the
		// shared core; the index is generated from stored versions.
		if _, err := h.service.Delete(r.Context(), p, artifact.Target{FeedID: feed, Coordinate: parts[4], Version: parts[5]}); err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":true}`))
		return
	}
	if (len(parts) == 7 || len(parts) == 9) && (r.Method == "GET" || r.Method == "HEAD") {
		filename := parts[len(parts)-1]
		if filename != pub.AssetKey && filename != pub.AssetKey+".prov" {
			http.NotFound(w, r)
			return
		}
		source, digest := "", ""
		if len(parts) == 9 {
			source, digest = parts[6], parts[7]
			if !blob.Digest(digest).Valid() {
				fail(w, artifact.ErrInput)
				return
			}
		}
		h.download(w, r, p, pub, filename, source, digest)
		return
	}
	w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
	http.Error(w, "method or route not supported", 405)
}
func digestInfo(data []byte) blob.Info {
	sum := sha256.Sum256(data)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}
}

// POST /feeds/{slug}/helm/charts/{name}/{version} accepts multipart fields
// chart (.tgz) and optional provenance (.tgz.prov), committed in one bundle.
func (h *Repository) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	policy, err := (identity.Roles{DB: h.db}).Resolve(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: pub.FeedID, Kind: identity.FeedResource}, identity.Admin) {
		fail(w, artifact.ErrDenied)
		return
	}
	source, err := h.service.Prepare(r.Context(), p, pub.FeedID, "", pub.Coordinate, pub.AssetKey)
	if err != nil {
		fail(w, err)
		return
	}
	pub.ExpectedSource = source
	select {
	case h.uploads <- struct{}{}:
		defer func() { <-h.uploads }()
	default:
		http.Error(w, "upload capacity exceeded", 503)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = controller.SetWriteDeadline(time.Time{})
	defer controller.SetReadDeadline(time.Time{})
	r.Body = &idleBody{ReadCloser: r.Body, controller: controller}
	r.Body = http.MaxBytesReader(w, r.Body, h.Limits.ChartBytes+h.Limits.ProvenanceBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, artifact.ErrInput)
		return
	}
	tmp, err := os.CreateTemp("", "ant-helm-*.tgz")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	var prov []byte
	seen := map[string]bool{}
	for {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(w, errChart)
			return
		}
		field := part.FormName()
		if seen[field] || (field != "chart" && field != "provenance") {
			part.Close()
			fail(w, errChart)
			return
		}
		seen[field] = true
		expected := pub.AssetKey
		if field == "provenance" {
			expected += ".prov"
		}
		if part.FileName() != expected {
			part.Close()
			fail(w, errChart)
			return
		}
		if field == "chart" {
			hash := sha256.New()
			n, e := io.CopyBuffer(io.MultiWriter(tmp, hash), io.LimitReader(part, h.Limits.ChartBytes+1), make([]byte, 64<<10))
			if e != nil || n > h.Limits.ChartBytes {
				part.Close()
				fail(w, errChart)
				return
			}
			pub.Info = blob.Info{Digest: blob.Digest(hex.EncodeToString(hash.Sum(nil))), Size: n}
		} else {
			prov, e = io.ReadAll(io.LimitReader(part, h.Limits.ProvenanceBytes+1))
			if e != nil || int64(len(prov)) > h.Limits.ProvenanceBytes {
				part.Close()
				fail(w, errChart)
				return
			}
		}
		part.Close()
	}
	if !seen["chart"] {
		fail(w, errChart)
		return
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		fail(w, err)
		return
	}
	limits := h.Limits
	if pub.Info.Size <= (limits.ExpandedBytes-1)/1000 {
		limits.ExpandedBytes = pub.Info.Size * 1000
	}
	metadata, err := inspectChartContext(r.Context(), tmp, pub.Coordinate, pub.Version, limits)
	if err != nil {
		fail(w, err)
		return
	}
	if seen["provenance"] && validateProvenance(prov, pub.Coordinate, pub.Version, string(pub.Info.Digest)) != nil {
		fail(w, errChart)
		return
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		fail(w, err)
		return
	}
	meta := pub
	meta.AssetKey = "Chart.yaml"
	meta.MediaType = "text/yaml"
	meta.Info = digestInfo(metadata)
	publications := []artifact.Publication{pub, meta}
	readers := []io.Reader{tmp, bytes.NewReader(metadata)}
	if seen["provenance"] {
		pr := pub
		pr.AssetKey += ".prov"
		pr.MediaType = "application/pgp-signature"
		pr.Info = digestInfo(prov)
		publications = append(publications, pr)
		readers = append(readers, bytes.NewReader(prov))
	}
	records, err := h.service.PublishBundle(r.Context(), p, publications, readers)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": pub.Coordinate, "version": pub.Version, "digest": string(records[0].Info.Digest), "provenanceStored": seen["provenance"], "signatureVerified": false})
}

type idleBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (b *idleBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	return b.ReadCloser.Read(p)
}

type idleWriter struct {
	io.Writer
	controller *http.ResponseController
}

func (b *idleWriter) Write(p []byte) (int, error) {
	_ = b.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return b.Writer.Write(p)
}
