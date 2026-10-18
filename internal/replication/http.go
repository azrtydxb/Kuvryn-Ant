package replication

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
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
func (s *Service) Register(mux *http.ServeMux, auth identity.Authenticator) {
	bearer := identity.NewBearer(auth)
	h := handlers{s: s, roles: identity.Roles{DB: s.cfg.DB}}
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/replication/peers":               h.listPeers,
		"POST /api/v1/replication/peers":              h.createPeer,
		"GET /api/v1/replication/peers/{id}":          h.getPeer,
		"PATCH /api/v1/replication/peers/{id}":        h.patchPeer,
		"DELETE /api/v1/replication/peers/{id}":       h.deletePeer,
		"GET /api/v1/replication/jobs":                h.listJobs,
		"POST /api/v1/replication/jobs":               h.createJob,
		"GET /api/v1/replication/jobs/{id}":           h.getJob,
		"PATCH /api/v1/replication/jobs/{id}":         h.patchJob,
		"DELETE /api/v1/replication/jobs/{id}":        h.deleteJob,
		"GET /api/v1/replication/jobs/{id}/status":    h.jobStatus,
		"POST /api/v1/replication/jobs/{id}/runs":     h.triggerJob,
		"GET /api/v1/replication/jobs/{id}/deletions": h.previewDeletions,
		"GET /api/v1/replication/status":              h.allStatus,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

type handlers struct {
	s     *Service
	roles identity.Roles
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
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
	peer, err := h.s.CreatePeer(r.Context(), a.p.InstanceID, a.p.PrincipalID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusCreated, peer)
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
	peer, err := h.s.UpdatePeer(r.Context(), a.p.InstanceID, a.p.PrincipalID, r.PathValue("id"), in)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, peer)
}
func (h handlers) deletePeer(w http.ResponseWriter, r *http.Request) {
	a, ok := h.admin(w, r)
	if !ok {
		return
	}
	if err := h.s.DeletePeer(r.Context(), a.p.InstanceID, a.p.PrincipalID, r.PathValue("id")); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	job, err := h.s.CreateJob(r.Context(), a.p.InstanceID, a.p.PrincipalID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusCreated, job)
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
	updated, err := h.s.UpdateJob(r.Context(), a.p.InstanceID, a.p.PrincipalID, job.ID, in)
	if err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, updated)
}
func (h handlers) deleteJob(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Configure) })
	if !ok {
		return
	}
	if err := h.s.DeleteJob(r.Context(), a.p.InstanceID, a.p.PrincipalID, job.ID); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h handlers) triggerJob(w http.ResponseWriter, r *http.Request) {
	a, job, ok := h.authorizeJob(w, r, func(a authz, feed string) bool { return a.feed(feed, identity.Replicate) })
	if !ok {
		return
	}
	if err := h.s.Trigger(r.Context(), a.p.InstanceID, a.p.PrincipalID, job.ID); err != nil {
		fail(w, r, err)
		return
	}
	management.JSON(w, http.StatusAccepted, map[string]string{"job_id": job.ID, "state": "requested"})
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
