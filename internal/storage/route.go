package storage

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Route names the backend that receives writes and serves reads first, and the
// backend that still serves reads that miss on the active one. Previous is empty
// once the old store has been cleaned up (or if no migration ever happened).
type Route struct{ Active, Previous string }

// Router decides the current Route. It is consulted on every store operation and
// must therefore be cheap (cache with a short lifetime).
type Router interface {
	Route(ctx context.Context) (Route, error)
}

// SetRouter installs the router. Without one, the configured backend is active.
func (r *Registry) SetRouter(router Router) {
	r.mu.Lock()
	r.router = router
	r.mu.Unlock()
}

func (r *Registry) route(ctx context.Context) (Route, error) {
	r.mu.Lock()
	router := r.router
	r.mu.Unlock()
	if router == nil {
		return Route{Active: r.config.Backend}, nil
	}
	return router.Route(ctx)
}

func (r *Registry) backend(ctx context.Context, kind, domain string) (blob.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.backendLocked(kind, domain)
}

// routed is the store handed to services. It forwards to whichever backend is
// active at call time, so a migration switch needs no re-resolution.
type routed struct {
	r      *Registry
	domain string
}

func (s *routed) active(ctx context.Context) (blob.Store, Route, error) {
	rt, err := s.r.route(ctx)
	if err != nil {
		return nil, rt, err
	}
	st, err := s.r.backend(ctx, rt.Active, s.domain)
	return st, rt, err
}

func (s *routed) Stage(ctx context.Context) (blob.Upload, error) {
	st, _, err := s.active(ctx)
	if err != nil {
		return nil, err
	}
	return st.Stage(ctx)
}

func (s *routed) previous(ctx context.Context, rt Route) (blob.Store, bool) {
	if rt.Previous == "" || rt.Previous == rt.Active {
		return nil, false
	}
	st, err := s.r.backend(ctx, rt.Previous, s.domain)
	return st, err == nil
}

func (s *routed) Stat(ctx context.Context, d blob.Digest) (blob.Info, error) {
	st, rt, err := s.active(ctx)
	if err != nil {
		return blob.Info{}, err
	}
	info, err := st.Stat(ctx, d)
	if errors.Is(err, blob.ErrNotFound) {
		if old, ok := s.previous(ctx, rt); ok {
			return old.Stat(ctx, d)
		}
	}
	return info, err
}

func (s *routed) OpenRange(ctx context.Context, d blob.Digest, offset, length int64) (io.ReadCloser, error) {
	st, rt, err := s.active(ctx)
	if err != nil {
		return nil, err
	}
	rc, err := st.OpenRange(ctx, d, offset, length)
	if errors.Is(err, blob.ErrNotFound) {
		if old, ok := s.previous(ctx, rt); ok {
			return old.OpenRange(ctx, d, offset, length)
		}
	}
	return rc, err
}

// Delete removes the object from the active store and, while it still exists,
// from the previous one so garbage collection cannot resurrect bytes.
func (s *routed) Delete(ctx context.Context, d blob.Digest) error {
	st, rt, err := s.active(ctx)
	if err != nil {
		return err
	}
	del, ok := st.(blob.Deleter)
	if !ok {
		return &blob.CapabilityError{Feature: "object deletion"}
	}
	errs := []error{del.Delete(ctx, d)}
	if old, ok := s.previous(ctx, rt); ok {
		if od, ok := old.(blob.Deleter); ok {
			errs = append(errs, od.Delete(ctx, d))
		}
	}
	return errors.Join(errs...)
}

func (s *routed) List(ctx context.Context, after blob.Digest, limit int) ([]blob.Object, error) {
	st, _, err := s.active(ctx)
	if err != nil {
		return nil, err
	}
	l, ok := st.(blob.Lister)
	if !ok {
		return nil, &blob.CapabilityError{Feature: "object listing"}
	}
	return l.List(ctx, after, limit)
}

func (s *routed) AbortStaleUploads(ctx context.Context, olderThan time.Duration, dryRun bool) (int, error) {
	st, rt, err := s.active(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	var errs []error
	stores := []blob.Store{st}
	if old, ok := s.previous(ctx, rt); ok {
		stores = append(stores, old)
	}
	for _, x := range stores {
		if a, ok := x.(blob.StaleUploadAborter); ok {
			n, err := a.AbortStaleUploads(ctx, olderThan, dryRun)
			total += n
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

func (s *routed) StageResumable(ctx context.Context, id string) (blob.ResumableUpload, error) {
	st, _, err := s.active(ctx)
	if err != nil {
		return nil, err
	}
	x, ok := st.(blob.ResumableStager)
	if !ok {
		return nil, &blob.CapabilityError{Feature: "resumable uploads"}
	}
	return x.StageResumable(ctx, id)
}

var (
	_ blob.Store              = (*routed)(nil)
	_ blob.Deleter            = (*routed)(nil)
	_ blob.Lister             = (*routed)(nil)
	_ blob.StaleUploadAborter = (*routed)(nil)
	_ blob.ResumableStager    = (*routed)(nil)
)

// DBRouter reads the persisted active backend of one instance. Rows are cached
// briefly; Invalidate makes an in-process switch visible immediately, other
// processes converge within TTL. A database error serves the last known route.
type DBRouter struct {
	DB         *pgxpool.Pool
	InstanceID string
	// Default is the configured backend, used while no switch has been recorded.
	Default string
	TTL     time.Duration

	mu      sync.Mutex
	cached  Route
	fetched time.Time
	have    bool
}

func (d *DBRouter) Route(ctx context.Context) (Route, error) {
	ttl := d.TTL
	if ttl <= 0 {
		ttl = 2 * time.Second
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.have && time.Since(d.fetched) < ttl {
		return d.cached, nil
	}
	rt, err := LoadRoute(ctx, d.DB, d.InstanceID, d.Default)
	if err != nil {
		if d.have {
			return d.cached, nil
		}
		return Route{}, err
	}
	d.cached, d.fetched, d.have = rt, time.Now(), true
	return rt, nil
}

// Invalidate drops the cached route.
func (d *DBRouter) Invalidate() {
	d.mu.Lock()
	d.have = false
	d.mu.Unlock()
}

// LoadRoute reads the persisted route, defaulting to def when none exists. An
// empty instance selects the bootstrapped instance.
func LoadRoute(ctx context.Context, db *pgxpool.Pool, instance, def string) (Route, error) {
	if def == "" {
		def = "disk"
	}
	var active string
	var previous *string
	err := db.QueryRow(ctx, `SELECT backend,previous_backend FROM storage_active WHERE instance_id=COALESCE(NULLIF($1,'')::uuid,(SELECT instance_id FROM bootstrap_state WHERE singleton=true))`, instance).Scan(&active, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{Active: def}, nil
	}
	if err != nil {
		return Route{}, err
	}
	rt := Route{Active: active}
	if previous != nil {
		rt.Previous = *previous
	}
	return rt, nil
}
