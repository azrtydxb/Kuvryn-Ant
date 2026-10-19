package retention

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
)

// ErrSettingLocked reports an attempt to change a value the deployment
// environment controls.
var ErrSettingLocked = errors.New("setting is controlled by the environment")

// DefaultPolicy is the instance-wide retention policy. It applies to every
// remote (cache) source whose feeds have no policy of their own, and only to
// remote-cached content: local and replica sources are release content and are
// never touched by it.
type DefaultPolicy struct {
	Enabled       bool          `json:"enabled"`
	KeepLast      int           `json:"keep_last,omitempty"`
	MaxAge        time.Duration `json:"max_age,omitempty"`
	MaxIdle       time.Duration `json:"max_idle,omitempty"`
	MinKeep       int           `json:"min_keep"`
	IncludeTagged bool          `json:"include_tagged,omitempty"`
}

// HasRule reports whether at least one limit is set.
func (d DefaultPolicy) HasRule() bool { return d.KeepLast > 0 || d.MaxAge > 0 || d.MaxIdle > 0 }

// policy converts an enabled default into a Policy. Release deletion is never
// part of a default.
func (d DefaultPolicy) policy() *Policy {
	if !d.Enabled || !d.HasRule() {
		return nil
	}
	return &Policy{KeepLast: d.KeepLast, MaxAge: d.MaxAge, MaxIdle: d.MaxIdle, MinKeep: d.MinKeep, IncludeTagged: d.IncludeTagged, Enabled: true}
}

// PressureConfig configures the storage-pressure trigger. Usage at or above
// HighPercent of capacity starts eviction, which continues until the projected
// usage is at or below LowPercent. CapacityBytes is an operator quota; without
// it, disk capacity comes from the filesystem and object storage has none (the
// trigger is then disabled).
type PressureConfig struct {
	Enabled       bool  `json:"enabled"`
	HighPercent   int   `json:"high_percent"`
	LowPercent    int   `json:"low_percent"`
	CapacityBytes int64 `json:"capacity_bytes,omitempty"`
}

// Settings are the effective instance retention settings: stored values with
// environment overrides applied. Locked lists the dotted names of fields the
// environment controls.
type Settings struct {
	Default   DefaultPolicy  `json:"default_policy"`
	Pressure  PressureConfig `json:"pressure"`
	Revision  int64          `json:"revision"`
	Locked    []string       `json:"locked"`
	UpdatedAt *time.Time     `json:"updated_at,omitempty"`
}

// Overrides are deployment-provided values. A nil field is not overridden.
type Overrides struct {
	DefaultEnabled  *bool
	DefaultKeepLast *int
	DefaultMaxAge   *time.Duration
	DefaultMaxIdle  *time.Duration
	PressureEnabled *bool
	HighPercent     *int
	LowPercent      *int
	CapacityBytes   *int64
}

// Locked field names.
const (
	LockDefaultEnabled  = "default_policy.enabled"
	LockDefaultKeepLast = "default_policy.keep_last"
	LockDefaultMaxAge   = "default_policy.max_age_seconds"
	LockDefaultMaxIdle  = "default_policy.max_idle_seconds"
	LockPressureEnabled = "pressure.enabled"
	LockHighPercent     = "pressure.high_percent"
	LockLowPercent      = "pressure.low_percent"
	LockCapacityBytes   = "pressure.capacity_bytes"
)

// DefaultSettings are the values of an instance that never stored any.
func DefaultSettings() Settings {
	return Settings{
		Default:  DefaultPolicy{MinKeep: 1},
		Pressure: PressureConfig{HighPercent: 85, LowPercent: 75},
		Revision: 1,
		Locked:   []string{},
	}
}

// Validate checks the cross-field rules. Disabled sections still have to be
// well formed so enabling them later cannot surprise the operator.
func (s Settings) Validate() error {
	d, p := s.Default, s.Pressure
	if d.KeepLast < 0 || d.MaxAge < 0 || d.MaxIdle < 0 || d.MinKeep < 0 || d.MaxAge != 0 && d.MaxAge < time.Second || d.MaxIdle != 0 && d.MaxIdle < time.Second {
		return fmt.Errorf("%w: negative or sub-second default rule", ErrInvalidPolicy)
	}
	if d.Enabled && !d.HasRule() {
		return fmt.Errorf("%w: an enabled default policy needs at least one rule", ErrInvalidPolicy)
	}
	if p.HighPercent < 2 || p.HighPercent > 99 || p.LowPercent < 1 || p.LowPercent >= p.HighPercent {
		return fmt.Errorf("%w: watermarks must satisfy 1 <= low < high <= 99", ErrInvalidPolicy)
	}
	if p.CapacityBytes < 0 {
		return fmt.Errorf("%w: negative capacity", ErrInvalidPolicy)
	}
	return nil
}

// Apply overlays env onto s and returns the locked field names.
func (o Overrides) Apply(s *Settings) []string {
	locked := []string{}
	if o.DefaultKeepLast != nil {
		s.Default.KeepLast = *o.DefaultKeepLast
		locked = append(locked, LockDefaultKeepLast)
	}
	if o.DefaultMaxAge != nil {
		s.Default.MaxAge = *o.DefaultMaxAge
		locked = append(locked, LockDefaultMaxAge)
	}
	if o.DefaultMaxIdle != nil {
		s.Default.MaxIdle = *o.DefaultMaxIdle
		locked = append(locked, LockDefaultMaxIdle)
	}
	switch {
	case o.DefaultEnabled != nil:
		s.Default.Enabled = *o.DefaultEnabled
		locked = append(locked, LockDefaultEnabled)
	case o.DefaultKeepLast != nil || o.DefaultMaxAge != nil || o.DefaultMaxIdle != nil:
		// Supplying a rule through the environment is an instruction to apply it.
		s.Default.Enabled = true
		locked = append(locked, LockDefaultEnabled)
	}
	if o.PressureEnabled != nil {
		s.Pressure.Enabled = *o.PressureEnabled
		locked = append(locked, LockPressureEnabled)
	}
	if o.HighPercent != nil {
		s.Pressure.HighPercent = *o.HighPercent
		locked = append(locked, LockHighPercent)
	}
	if o.LowPercent != nil {
		s.Pressure.LowPercent = *o.LowPercent
		locked = append(locked, LockLowPercent)
	}
	if o.CapacityBytes != nil {
		s.Pressure.CapacityBytes = *o.CapacityBytes
		locked = append(locked, LockCapacityBytes)
	}
	return locked
}

// IsLocked reports whether the field is controlled by the environment.
func (s Settings) IsLocked(field string) bool { return slices.Contains(s.Locked, field) }

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const settingsColumns = `default_enabled,COALESCE(default_keep_last,0),COALESCE(default_max_age_seconds,0),COALESCE(default_max_idle_seconds,0),default_min_keep,default_include_tagged,pressure_enabled,pressure_high_percent,pressure_low_percent,COALESCE(capacity_bytes,0),revision,updated_at`

func scanSettings(row pgx.Row) (Settings, error) {
	s := DefaultSettings()
	var age, idle int64
	var updated time.Time
	err := row.Scan(&s.Default.Enabled, &s.Default.KeepLast, &age, &idle, &s.Default.MinKeep, &s.Default.IncludeTagged,
		&s.Pressure.Enabled, &s.Pressure.HighPercent, &s.Pressure.LowPercent, &s.Pressure.CapacityBytes, &s.Revision, &updated)
	if err != nil {
		return Settings{}, err
	}
	s.Default.MaxAge, s.Default.MaxIdle = time.Duration(age)*time.Second, time.Duration(idle)*time.Second
	s.UpdatedAt = &updated
	return s, nil
}

// LoadStored returns the stored settings without environment overrides. An
// instance that never saved any has the defaults at virtual revision 1.
func LoadStored(ctx context.Context, q querier, instance string) (Settings, error) {
	s, err := scanSettings(q.QueryRow(ctx, `SELECT `+settingsColumns+` FROM retention_settings WHERE instance_id=$1`, instance))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultSettings(), nil
	}
	return s, err
}

// Settings returns the effective settings: stored values with the service's
// environment overrides applied.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	if err := s.validate(); err != nil {
		return Settings{}, err
	}
	stored, err := LoadStored(ctx, s.DB, s.InstanceID)
	if err != nil {
		return Settings{}, err
	}
	stored.Locked = s.Env.Apply(&stored)
	return stored, nil
}

// SaveSettings replaces the stored settings inside tx. in carries the values
// the caller wants to see; fields the environment controls must equal their
// effective value (ErrSettingLocked otherwise) and keep their stored value.
// expected is the revision the caller read (1 for an instance with no row).
func SaveSettings(ctx context.Context, tx pgx.Tx, instance string, env Overrides, expected int64, in Settings) (Settings, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO retention_settings(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, instance); err != nil {
		return Settings{}, err
	}
	current, err := scanSettings(tx.QueryRow(ctx, `SELECT `+settingsColumns+` FROM retention_settings WHERE instance_id=$1 FOR UPDATE`, instance))
	if err != nil {
		return Settings{}, err
	}
	if current.Revision != expected {
		return Settings{}, database.ErrRevisionConflict
	}
	effective := current
	locked := env.Apply(&effective)
	next := in
	keep := func(field string, differs bool, restore func()) error {
		if !slices.Contains(locked, field) {
			return nil
		}
		if differs {
			return fmt.Errorf("%w: %s", ErrSettingLocked, field)
		}
		restore()
		return nil
	}
	for _, f := range []struct {
		name    string
		differs bool
		restore func()
	}{
		{LockDefaultEnabled, in.Default.Enabled != effective.Default.Enabled, func() { next.Default.Enabled = current.Default.Enabled }},
		{LockDefaultKeepLast, in.Default.KeepLast != effective.Default.KeepLast, func() { next.Default.KeepLast = current.Default.KeepLast }},
		{LockDefaultMaxAge, in.Default.MaxAge != effective.Default.MaxAge, func() { next.Default.MaxAge = current.Default.MaxAge }},
		{LockDefaultMaxIdle, in.Default.MaxIdle != effective.Default.MaxIdle, func() { next.Default.MaxIdle = current.Default.MaxIdle }},
		{LockPressureEnabled, in.Pressure.Enabled != effective.Pressure.Enabled, func() { next.Pressure.Enabled = current.Pressure.Enabled }},
		{LockHighPercent, in.Pressure.HighPercent != effective.Pressure.HighPercent, func() { next.Pressure.HighPercent = current.Pressure.HighPercent }},
		{LockLowPercent, in.Pressure.LowPercent != effective.Pressure.LowPercent, func() { next.Pressure.LowPercent = current.Pressure.LowPercent }},
		{LockCapacityBytes, in.Pressure.CapacityBytes != effective.Pressure.CapacityBytes, func() { next.Pressure.CapacityBytes = current.Pressure.CapacityBytes }},
	} {
		if err = keep(f.name, f.differs, f.restore); err != nil {
			return Settings{}, err
		}
	}
	// Validate what will actually be effective (stored under the overrides).
	check := next
	env.Apply(&check)
	if err = check.Validate(); err != nil {
		return Settings{}, err
	}
	// The stored row must itself satisfy the table constraints.
	if next.Pressure.LowPercent >= next.Pressure.HighPercent || next.Pressure.HighPercent < 2 || next.Pressure.HighPercent > 99 || next.Pressure.LowPercent < 1 {
		return Settings{}, fmt.Errorf("%w: watermarks must satisfy 1 <= low < high <= 99", ErrInvalidPolicy)
	}
	var keepLast, age, idle, capacity any
	if next.Default.KeepLast > 0 {
		keepLast = next.Default.KeepLast
	}
	if next.Default.MaxAge > 0 {
		age = int64(next.Default.MaxAge / time.Second)
	}
	if next.Default.MaxIdle > 0 {
		idle = int64(next.Default.MaxIdle / time.Second)
	}
	if next.Pressure.CapacityBytes > 0 {
		capacity = next.Pressure.CapacityBytes
	}
	saved, err := scanSettings(tx.QueryRow(ctx, `UPDATE retention_settings SET default_enabled=$2,default_keep_last=$3,default_max_age_seconds=$4,default_max_idle_seconds=$5,default_min_keep=$6,default_include_tagged=$7,pressure_enabled=$8,pressure_high_percent=$9,pressure_low_percent=$10,capacity_bytes=$11,revision=revision+1,updated_at=now() WHERE instance_id=$1 RETURNING `+settingsColumns,
		instance, next.Default.Enabled, keepLast, age, idle, next.Default.MinKeep, next.Default.IncludeTagged,
		next.Pressure.Enabled, next.Pressure.HighPercent, next.Pressure.LowPercent, capacity))
	if err != nil {
		return Settings{}, err
	}
	saved.Locked = env.Apply(&saved)
	return saved, nil
}
