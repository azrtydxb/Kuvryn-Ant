package replication

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/jackc/pgx/v5"
)

// Register mounts the destination-side management API. All routes require a
// bearer token. Peers need instance "admin"; jobs are authorized on the
// destination feed ("configure" to change, "replicate" to run or inspect).
//
//	GET    /api/v1/replication/peers              POST   /api/v1/replication/peers
//	GET    /api/v1/replication/peers/{id}         PATCH  /api/v1/replication/peers/{id}   DELETE /api/v1/replication/peers/{id}
//	GET    /api/v1/replication/jobs               POST   /api/v1/replication/jobs
//	GET    /api/v1/replication/jobs/{id}          PATCH  /api/v1/replication/jobs/{id}    DELETE /api/v1/replication/jobs/{id}
//	GET    /api/v1/replication/jobs/{id}/status   POST   /api/v1/replication/jobs/{id}/runs
//	GET    /api/v1/replication/jobs/{id}/deletions   (dry-run deletion preview)
//	GET    /api/v1/replication/status             (all jobs: progress, lag, failures, last success)
//	POST   /api/v1/replication/jobs/{id}/template/preview   (diff a feed configuration template; read-only)
//	POST   /api/v1/replication/jobs/{id}/template/apply     (apply a reviewed template)
//
// Every POST, PATCH and DELETE that changes state requires an Idempotency-Key
// header (1-256 bytes). A retry with the same key and request replays the
// stored result; the same key with a different request is rejected with 409.
// Requests that carry a peer token are fingerprinted with a keyed MAC, never a
// plain hash.
func (s *Service) Register(mux *http.ServeMux, auth identity.Authenticator) {
	bearer := identity.NewBearer(auth)
	h := handlers{s: s, roles: identity.Roles{DB: s.cfg.DB}}
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/replication/peers":                       h.listPeers,
		"POST /api/v1/replication/peers":                      h.createPeer,
		"GET /api/v1/replication/peers/{id}":                  h.getPeer,
		"PATCH /api/v1/replication/peers/{id}":                h.patchPeer,
		"DELETE /api/v1/replication/peers/{id}":               h.deletePeer,
		"GET /api/v1/replication/jobs":                        h.listJobs,
		"POST /api/v1/replication/jobs":                       h.createJob,
		"GET /api/v1/replication/jobs/{id}":                   h.getJob,
		"PATCH /api/v1/replication/jobs/{id}":                 h.patchJob,
		"DELETE /api/v1/replication/jobs/{id}":                h.deleteJob,
		"GET /api/v1/replication/jobs/{id}/status":            h.jobStatus,
		"POST /api/v1/replication/jobs/{id}/runs":             h.triggerJob,
		"GET /api/v1/replication/jobs/{id}/deletions":         h.previewDeletions,
		"GET /api/v1/replication/status":                      h.allStatus,
		"POST /api/v1/replication/jobs/{id}/template/preview": h.previewTemplate,
		"POST /api/v1/replication/jobs/{id}/template/apply":   h.applyTemplate,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

type handlers struct {
	s     *Service
	roles identity.Roles
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	var loop *feedgraph.LoopError
	switch {
	case errors.As(err, &loop):
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, loop.Error())
	case errors.Is(err, ErrInput):
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "invalid replication configuration")
	case errors.Is(err, ErrNotFound):
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "replication resource not found")
	case errors.Is(err, ErrConflict):
		management.Error(w, r, http.StatusConflict, api.ErrorCodeConflict, "replication resource conflicts with current state")
	case errors.Is(err, ErrDenied):
		management.Error(w, r, http.StatusForbidden, api.ErrorCodeForbidden, "replication access denied")
	case errors.Is(err, ErrPeer):
		management.Error(w, r, http.StatusBadGateway, api.ErrorCodeUnavailable, "replication peer request failed")
	default:
		management.Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "replication unavailable")
	}
}

type authz struct {
	p      identity.Principal
	policy identity.Policy
}

func (h handlers) begin(w http.ResponseWriter, r *http.Request) (authz, bool) {
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok {
		management.Error(w, r, http.StatusUnauthorized, api.ErrorCodeUnauthenticated, "authentication required")
		return authz{}, false
	}
	policy, err := h.roles.Resolve(r.Context(), p)
	if err != nil {
		management.Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "authorization unavailable")
		return authz{}, false
	}
	return authz{p, policy}, true
}
func (a authz) admin() bool {
	return a.policy.Authorize(a.p, identity.Resource{InstanceID: a.p.InstanceID, Kind: identity.InstanceResource}, identity.Admin)
}
func (a authz) feed(feed string, action identity.Action) bool {
	return a.policy.Authorize(a.p, identity.Resource{InstanceID: a.p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, action)
}
func (a authz) inspect(feed string) bool {
	return a.feed(feed, identity.Replicate) || a.feed(feed, identity.Configure)
}

func (h handlers) admin(w http.ResponseWriter, r *http.Request) (authz, bool) {
	a, ok := h.begin(w, r)
	if !ok {
		return a, false
	}
	if !a.admin() {
		management.Error(w, r, http.StatusForbidden, api.ErrorCodeForbidden, "instance administration denied")
		return a, false
	}
	return a, true
}

// idempotencyKey enforces the management convention: exactly one header value
// of 1-256 bytes without surrounding whitespace.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || len(values[0]) < 1 || len(values[0]) > 256 || strings.TrimSpace(values[0]) != values[0] {
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "Idempotency-Key required (1-256 bytes)")
		return "", false
	}
	return values[0], true
}

// mutate runs apply in the shared idempotent-mutation transaction. canonical is
// the request identity (everything that distinguishes two requests); with
// secret set it is fingerprinted with the service's keyed MAC. Domain errors
// from apply keep their HTTP meaning; a replay returns the stored JSON.
func (h handlers) mutate(w http.ResponseWriter, r *http.Request, p identity.Principal, op, resource string, canonical any, secret bool, apply func(context.Context, pgx.Tx) (any, error)) (json.RawMessage, bool) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return nil, false
	}
	encoded, _ := json.Marshal(canonical)
	var fingerprint [32]byte
	if secret {
		mac := hmac.New(sha256.New, h.s.fingerprintKey)
		_, _ = mac.Write(encoded)
		copy(fingerprint[:], mac.Sum(nil))
	} else {
		fingerprint = sha256.Sum256(encoded)
	}
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), h.s.cfg.DB, database.MutationRequest{
		InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: op, Resource: resource, Key: key, Fingerprint: fingerprint, NonSecretResult: true,
	}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		v, err := apply(ctx, tx)
		if err != nil {
			applyErr = err
			return nil, err
		}
		return json.Marshal(v)
	})
	switch {
	case err == nil:
		return result.JSON, true
	case applyErr != nil:
		fail(w, r, applyErr)
	case errors.Is(err, database.ErrFingerprintConflict):
		management.Error(w, r, http.StatusConflict, api.ErrorCodeConflict, "Idempotency-Key was already used for a different request")
	case errors.Is(err, database.ErrMutationInput):
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "invalid mutation")
	default:
		management.Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "replication unavailable")
	}
	return nil, false
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		management.Error(w, r, http.StatusUnsupportedMediaType, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(dst) != nil || d.Decode(&extra) != io.EOF {
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "invalid request body")
		return false
	}
	return true
}

func (h handlers) listPeers(w http.ResponseWriter, r *http.Request) {
	a, ok := h.admin(w, r)
	if !ok {
		return
	}
	items, err := h.s.ListPeers(r.Context(), a.p.InstanceID)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, struct {
		Items []Peer `json:"items"`
	}{items})
}
func (h handlers) createPeer(w http.ResponseWriter, r *http.Request) {
	a, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in PeerInput
	if !decode(w, r, &in) {
		return
	}
	raw, ok := h.mutate(w, r, a.p, "replication.peer.create", "replication/peers", in, true, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return h.s.CreatePeerTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, in)
	})
	if ok {
		management.JSON(w, http.StatusCreated, raw)
	}
}
func (h handlers) getPeer(w http.ResponseWriter, r *http.Request) {
	a, ok := h.admin(w, r)
	if !ok {
		return
	}
	peer, err := h.s.GetPeer(r.Context(), a.p.InstanceID, r.PathValue("id"))
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, peer)
}
func (h handlers) patchPeer(w http.ResponseWriter, r *http.Request) {
	a, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in PeerPatch
	if !decode(w, r, &in) {
		return
	}
	if !applyIfMatch(w, r, &in.Revision) {
		return
	}
	id := r.PathValue("id")
	raw, ok := h.mutate(w, r, a.p, "replication.peer.update", id, in, true, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return h.s.UpdatePeerTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, id, in)
	})
	if ok {
		management.JSON(w, http.StatusOK, raw)
	}
}
func (h handlers) deletePeer(w http.ResponseWriter, r *http.Request) {
	a, ok := h.admin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := h.mutate(w, r, a.p, "replication.peer.delete", id, id, false, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return struct{}{}, h.s.DeletePeerTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, id)
	}); ok {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h handlers) listJobs(w http.ResponseWriter, r *http.Request) {
	a, ok := h.begin(w, r)
	if !ok {
		return
	}
	jobs, err := h.s.ListJobs(r.Context(), a.p.InstanceID)
	if err != nil {
		fail(w, r, err)
		return
	}
	items := []Job{}
	for _, j := range jobs {
		if a.inspect(j.DestFeedID) {
			items = append(items, j)
		}
	}
	management.JSON(w, http.StatusOK, struct {
		Items []Job `json:"items"`
	}{items})
}
func (h handlers) createJob(w http.ResponseWriter, r *http.Request) {
	a, ok := h.begin(w, r)
	if !ok {
		return
	}
	var in JobInput
	if !decode(w, r, &in) {
		return
	}
	if !uuidPattern.MatchString(in.DestFeedID) || !a.feed(in.DestFeedID, identity.Configure) || !a.feed(in.DestFeedID, identity.Replicate) {
		management.Error(w, r, http.StatusForbidden, api.ErrorCodeForbidden, "replication configuration denied")
		return
	}
	raw, ok := h.mutate(w, r, a.p, "replication.job.create", "replication/jobs", in, false, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return h.s.CreateJobTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, in)
	})
	if ok {
		management.JSON(w, http.StatusCreated, raw)
	}
}

// authorizeJob loads the job and checks the action on its destination feed.
// Unauthorized callers cannot distinguish a missing job from a forbidden one.
func (h handlers) authorizeJob(w http.ResponseWriter, r *http.Request, check func(authz, string) bool) (authz, Job, bool) {
	a, ok := h.begin(w, r)
	if !ok {
		return a, Job{}, false
	}
	job, err := h.s.GetJob(r.Context(), a.p.InstanceID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) || err == nil && !check(a, job.DestFeedID) {
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "replication resource not found")
		return a, Job{}, false
	}
	if err != nil {
		fail(w, r, err)
		return a, Job{}, false
	}
	return a, job, true
}
func (h handlers) getJob(w http.ResponseWriter, r *http.Request) {
	_, job, ok := h.authorizeJob(w, r, authz.inspect)
	if ok {
		management.JSON(w, http.StatusOK, job)
	}
}
func (h handlers) jobStatus(w http.ResponseWriter, r *http.Request) {
	_, job, ok := h.authorizeJob(w, r, authz.inspect)
	if ok {
		management.JSON(w, http.StatusOK, job.Status)
	}
}
func (h handlers) patchJob(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Configure) })
	if !ok {
		return
	}
	var in JobPatch
	if !decode(w, r, &in) {
		return
	}
	if !applyIfMatch(w, r, &in.Revision) {
		return
	}
	raw, ok := h.mutate(w, r, a.p, "replication.job.update", job.ID, in, false, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return h.s.UpdateJobTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, job.ID, in)
	})
	if ok {
		management.JSON(w, http.StatusOK, raw)
	}
}
func (h handlers) deleteJob(w http.ResponseWriter, r *http.Request) {
	a, ok := h.begin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "replication resource not found")
		return
	}
	// A job that no longer exists still reaches the idempotent mutation so a
	// retry of a successful delete replays its stored result; without a stored
	// result the mutation itself reports not found.
	switch job, err := h.s.GetJob(r.Context(), a.p.InstanceID, id); {
	case err == nil && !a.feed(job.DestFeedID, identity.Configure):
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "replication resource not found")
		return
	case err != nil && !errors.Is(err, ErrNotFound):
		fail(w, r, err)
		return
	}
	if _, ok := h.mutate(w, r, a.p, "replication.job.delete", id, id, false, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return struct{}{}, h.s.DeleteJobTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, id)
	}); ok {
		w.WriteHeader(http.StatusNoContent)
	}
}
func (h handlers) triggerJob(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Replicate) })
	if !ok {
		return
	}
	raw, ok := h.mutate(w, r, a.p, "replication.job.run", job.ID, job.ID, false, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return map[string]string{"job_id": job.ID, "state": "requested"}, h.s.TriggerTx(ctx, tx, a.p.InstanceID, a.p.PrincipalID, job.ID)
	})
	if ok {
		management.JSON(w, http.StatusAccepted, raw)
	}
}
func (h handlers) previewDeletions(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Replicate) })
	if !ok {
		return
	}
	preview, err := h.s.PreviewDeletions(r.Context(), a.p.InstanceID, job.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, preview)
}
func (h handlers) allStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := h.begin(w, r)
	if !ok {
		return
	}
	jobs, err := h.s.ListJobs(r.Context(), a.p.InstanceID)
	if err != nil {
		fail(w, r, err)
		return
	}
	type entry struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		DestFeedID string `json:"dest_feed_id"`
		Enabled    bool   `json:"enabled"`
		Status     Status `json:"status"`
	}
	items := []entry{}
	for _, j := range jobs {
		if a.inspect(j.DestFeedID) {
			items = append(items, entry{j.ID, j.Name, j.DestFeedID, j.Enabled, j.Status})
		}
	}
	management.JSON(w, http.StatusOK, struct {
		Items []entry `json:"items"`
	}{items})
}

// TemplateRequest optionally supplies the template to review; without it the
// job's peer is asked for its feed's current template.
type TemplateRequest struct {
	Template *Template `json:"template,omitempty"`
}

// TemplateApplyRequest applies a template at the digest that was reviewed.
type TemplateApplyRequest struct {
	Digest   string    `json:"digest"`
	Template *Template `json:"template,omitempty"`
}

func (h handlers) previewTemplate(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Configure) })
	if !ok {
		return
	}
	var in TemplateRequest
	if !decode(w, r, &in) {
		return
	}
	preview, err := h.s.PreviewTemplate(r.Context(), a.p.InstanceID, job.ID, in.Template)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, preview)
}

func (h handlers) applyTemplate(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Configure) })
	if !ok {
		return
	}
	var in TemplateApplyRequest
	if !decode(w, r, &in) {
		return
	}
	if len(in.Digest) != 64 {
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "digest of the reviewed preview required")
		return
	}
	if _, ok := idempotencyKey(w, r); !ok {
		return
	}
	job2, tpl, err := h.s.FetchTemplate(r.Context(), a.p.InstanceID, job.ID, in.Template)
	if err != nil {
		fail(w, r, err)
		return
	}
	raw, ok := h.mutate(w, r, a.p, "replication.template.apply", job.ID, struct {
		Digest   string
		Template Template
	}{in.Digest, tpl}, false, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return h.s.ApplyTemplateTx(ctx, tx, a.p, a.policy, job2, tpl, in.Digest)
	})
	if ok {
		management.JSON(w, http.StatusOK, raw)
	}
}
