package management

import (
	"errors"
	"net/http"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
)

// StorageMigrationStatus is the storage configuration plus the latest job.
// CleanupPending is true while the previous backend still holds a full copy.
type StorageMigrationStatus struct {
	ActiveBackend     string       `json:"active_backend"`
	PreviousBackend   string       `json:"previous_backend,omitempty"`
	CleanupPending    bool         `json:"cleanup_pending"`
	AvailableBackends []string     `json:"available_backends"`
	CurrentJob        *storage.Job `json:"current_job,omitempty"`
}

// StorageJobPage lists recent migration and cleanup jobs, newest first.
type StorageJobPage struct {
	Items []storage.Job `json:"items"`
}

// StartStorageJob requests a migration to another backend or the cleanup of the
// previous backend. A dry run copies and deletes nothing and reports exactly what
// a real run would do.
type StartStorageJob struct {
	Action      string `json:"action"`
	To          string `json:"to,omitempty"`
	DryRun      bool   `json:"dry_run"`
	BytesPerSec int64  `json:"throttle_bytes_per_second,omitempty"`
}

func (h opsHandlers) storageMigrator(w http.ResponseWriter, r *http.Request) (*storage.Migrator, identity.Principal, bool) {
	p, ok := h.admin(w, r)
	if !ok {
		return nil, p, false
	}
	var m *storage.Migrator
	if h.d.StorageMigrator != nil {
		m = h.d.StorageMigrator(p.InstanceID)
	}
	if m == nil || h.d.Background == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage migration is not available on this instance")
		return nil, p, false
	}
	return m, p, true
}

func (h opsHandlers) storageMigrationStatus(w http.ResponseWriter, r *http.Request) {
	m, _, ok := h.storageMigrator(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	st, err := m.Status(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage status unavailable")
		return
	}
	JSON(w, 200, StorageMigrationStatus{ActiveBackend: st.Active, PreviousBackend: st.Previous, CleanupPending: st.Previous != "", AvailableBackends: st.Available, CurrentJob: st.Current})
}

func (h opsHandlers) listStorageJobs(w http.ResponseWriter, r *http.Request) {
	m, _, ok := h.storageMigrator(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	jobs, err := m.List(ctx, 50)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage jobs unavailable")
		return
	}
	JSON(w, 200, StorageJobPage{Items: jobs})
}

func (h opsHandlers) getStorageJob(w http.ResponseWriter, r *http.Request) {
	m, _, ok := h.storageMigrator(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		identityInput(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	j, err := m.Get(ctx, id)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	JSON(w, 200, j)
}

func (h opsHandlers) storageError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFoundJob):
		Error(w, r, 404, api.ErrorCodeNotFound, "storage job not found")
	case errors.Is(err, storage.ErrBusy):
		Error(w, r, 409, api.ErrorCodeConflict, "another storage job is unfinished")
	case errors.Is(err, storage.ErrCleanupPending):
		Error(w, r, 409, api.ErrorCodeConflict, "clean up the previous storage before migrating again")
	case errors.Is(err, storage.ErrNothingToClean):
		Error(w, r, 409, api.ErrorCodeConflict, "no previous storage to clean up")
	case errors.Is(err, storage.ErrBadTarget):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid storage job request")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage job unavailable")
	}
}

// run executes a claimed job in the background, bound to the server lifetime:
// shutdown pauses it and a later start (or resume) continues from the cursor.
func (h opsHandlers) runStorageJob(m *storage.Migrator, id string) {
	go func() { _, _ = m.Run(h.d.Background, id) }()
}

func (h opsHandlers) startStorageJob(w http.ResponseWriter, r *http.Request) {
	m, principal, ok := h.storageMigrator(w, r)
	if !ok {
		return
	}
	var in StartStorageJob
	if !sourceJSON(w, r, &in, "action", "to", "dry_run", "throttle_bytes_per_second") {
		return
	}
	opts := storage.StartOptions{To: in.To, DryRun: in.DryRun, BytesPerSec: in.BytesPerSec, RequestedBy: principal.PrincipalID}
	switch in.Action {
	case "migrate":
		opts.Kind = storage.KindMigrate
	case "cleanup":
		opts.Kind = storage.KindCleanup
	default:
		identityInput(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	job, _, err := m.Start(ctx, opts)
	if err != nil {
		h.storageError(w, r, err)
		return
	}
	_, _ = h.db.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,'storage.'||$3::text,$4,jsonb_build_object('dry_run',$5::boolean,'from',$6::text,'to',$7::text))`, principal.InstanceID, principal.PrincipalID, in.Action, job.ID, in.DryRun, job.From, job.To)
	h.runStorageJob(m, job.ID)
	w.Header().Set("Location", "/api/v1/storage/migrations/"+job.ID)
	JSON(w, 202, job)
}

func (h opsHandlers) storageJobAction(w http.ResponseWriter, r *http.Request) {
	m, _, ok := h.storageMigrator(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		identityInput(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	switch r.PathValue("action") {
	case "pause":
		j, err := m.Pause(ctx, id)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		JSON(w, 200, j)
	case "resume":
		j, err := m.Resume(ctx, id)
		if err != nil {
			h.storageError(w, r, err)
			return
		}
		h.runStorageJob(m, j.ID)
		JSON(w, 202, j)
	default:
		Error(w, r, 404, api.ErrorCodeNotFound, "unknown action")
	}
}
