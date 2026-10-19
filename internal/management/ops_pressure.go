package management

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/jackc/pgx/v5"
)

// DefaultPolicyView is the instance-wide retention default. It only ever
// applies to remote-cached content.
type DefaultPolicyView struct {
	Enabled        bool   `json:"enabled"`
	KeepLast       *int32 `json:"keep_last,omitempty"`
	MaxAgeSeconds  *int64 `json:"max_age_seconds,omitempty"`
	MaxIdleSeconds *int64 `json:"max_idle_seconds,omitempty"`
	MinKeep        int32  `json:"min_keep"`
	IncludeTagged  bool   `json:"include_tagged"`
}

// PressureSettingsView configures the storage-pressure trigger.
type PressureSettingsView struct {
	Enabled       bool   `json:"enabled"`
	HighPercent   int32  `json:"high_percent"`
	LowPercent    int32  `json:"low_percent"`
	CapacityBytes *int64 `json:"capacity_bytes,omitempty"`
}

// GCSettingsView is the read-only, environment-provided maintenance schedule.
type GCSettingsView struct {
	IntervalSeconds           int64 `json:"interval_seconds"`
	GraceSeconds              int64 `json:"grace_seconds"`
	DryRun                    bool  `json:"dry_run"`
	PressureIntervalSeconds   int64 `json:"pressure_interval_seconds"`
	PressureSweepGraceSeconds int64 `json:"pressure_sweep_grace_seconds"`
}

// RetentionSettings are the instance retention defaults and pressure trigger.
// Locked names fields set by the deployment environment (read-only here).
type RetentionSettings struct {
	Revision      int64                `json:"revision"`
	DefaultPolicy DefaultPolicyView    `json:"default_policy"`
	Pressure      PressureSettingsView `json:"pressure"`
	Locked        []string             `json:"locked"`
	GC            GCSettingsView       `json:"gc"`
	UpdatedAt     *time.Time           `json:"updated_at,omitempty"`
}

// PutRetentionSettings replaces the editable settings.
type PutRetentionSettings struct {
	DefaultPolicy DefaultPolicyView    `json:"default_policy"`
	Pressure      PressureSettingsView `json:"pressure"`
}

func viewSettings(s retention.Settings, gc GCSettingsView) RetentionSettings {
	d := s.Default
	out := RetentionSettings{Revision: s.Revision, Locked: s.Locked, GC: gc, UpdatedAt: s.UpdatedAt,
		DefaultPolicy: DefaultPolicyView{Enabled: d.Enabled, MinKeep: int32(d.MinKeep), IncludeTagged: d.IncludeTagged},
		Pressure:      PressureSettingsView{Enabled: s.Pressure.Enabled, HighPercent: int32(s.Pressure.HighPercent), LowPercent: int32(s.Pressure.LowPercent)}}
	if d.KeepLast > 0 {
		v := int32(d.KeepLast)
		out.DefaultPolicy.KeepLast = &v
	}
	if d.MaxAge > 0 {
		v := int64(d.MaxAge / time.Second)
		out.DefaultPolicy.MaxAgeSeconds = &v
	}
	if d.MaxIdle > 0 {
		v := int64(d.MaxIdle / time.Second)
		out.DefaultPolicy.MaxIdleSeconds = &v
	}
	if s.Pressure.CapacityBytes > 0 {
		v := s.Pressure.CapacityBytes
		out.Pressure.CapacityBytes = &v
	}
	if out.Locked == nil {
		out.Locked = []string{}
	}
	return out
}

func (h opsHandlers) retentionService(p string) *retention.Service {
	if h.d.Retention == nil {
		return nil
	}
	return h.d.Retention(p)
}

func (h opsHandlers) getRetentionSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc := h.retentionService(p.InstanceID)
	if svc == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "maintenance is not available on this instance")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	s, err := svc.Settings(ctx)
	if err != nil {
		identityError(w, r, err)
		return
	}
	JSON(w, 200, viewSettings(s, h.d.GC))
}

func (h opsHandlers) putRetentionSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	svc := h.retentionService(p.InstanceID)
	if svc == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "maintenance is not available on this instance")
		return
	}
	var in PutRetentionSettings
	if !sourceJSON(w, r, &in, "default_policy", "pressure") {
		return
	}
	d, pr := in.DefaultPolicy, in.Pressure
	next := retention.Settings{
		Default:  retention.DefaultPolicy{Enabled: d.Enabled, MinKeep: int(d.MinKeep), IncludeTagged: d.IncludeTagged},
		Pressure: retention.PressureConfig{Enabled: pr.Enabled, HighPercent: int(pr.HighPercent), LowPercent: int(pr.LowPercent)},
	}
	if d.KeepLast != nil {
		next.Default.KeepLast = int(*d.KeepLast)
	}
	if d.MaxAgeSeconds != nil {
		next.Default.MaxAge = time.Duration(*d.MaxAgeSeconds) * time.Second
	}
	if d.MaxIdleSeconds != nil {
		next.Default.MaxIdle = time.Duration(*d.MaxIdleSeconds) * time.Second
	}
	if pr.CapacityBytes != nil {
		next.Pressure.CapacityBytes = *pr.CapacityBytes
	}
	if d.KeepLast != nil && *d.KeepLast < 1 || d.MaxAgeSeconds != nil && *d.MaxAgeSeconds < 1 || d.MaxIdleSeconds != nil && *d.MaxIdleSeconds < 1 ||
		pr.CapacityBytes != nil && *pr.CapacityBytes < 1 || d.MinKeep < 0 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid retention settings")
		return
	}
	// Environment-controlled fields are validated against the effective view.
	effective := next
	svc.Env.Apply(&effective)
	if err := effective.Validate(); err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid retention settings: "+err.Error())
		return
	}
	h.mutate(w, r, p, "retention.settings.put", "retention-settings", struct {
		In       PutRetentionSettings
		Revision int64
	}{in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		saved, err := retention.SaveSettings(ctx, tx, p.InstanceID, svc.Env, revision, next)
		if err != nil {
			if errors.Is(err, retention.ErrSettingLocked) || errors.Is(err, retention.ErrInvalidPolicy) {
				return nil, database.ErrConflict
			}
			return nil, err
		}
		return viewSettings(saved, h.d.GC), identityEvent(ctx, tx, p, p.InstanceID, "retention.settings.put", saved.Revision)
	})
}

func (h opsHandlers) getPressureStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc := h.retentionService(p.InstanceID)
	if svc == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "maintenance is not available on this instance")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	st, err := svc.PressureStatus(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "pressure status unavailable")
		return
	}
	JSON(w, 200, st)
}
