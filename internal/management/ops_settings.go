package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/maintenance"
	"github.com/jackc/pgx/v5"
)

// Defaults and hard limits for editable settings; they mirror the limits the
// browser session layer enforces.
const (
	defaultSessionMaxAge  = 8 * 3600
	defaultSessionIdle    = 30 * 60
	maxSessionMaxAge      = 8 * 3600
	maxSessionIdleTimeout = 30 * 60
)

// SettingItem is one instance setting. Value is always a string. Source is
// env (deployment-provided, read-only), stored or default. Enforced is false
// when the value is recorded for the console but the runtime still applies its
// built-in limit.
type SettingItem struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	ReadOnly    bool   `json:"read_only"`
	Enforced    bool   `json:"enforced"`
	Description string `json:"description,omitempty"`
}
type InstanceSettings struct {
	Revision  int64         `json:"revision"`
	Items     []SettingItem `json:"items"`
	UpdatedAt *time.Time    `json:"updated_at,omitempty"`
}

// PutSettings changes the editable values. Fields that are absent keep their
// stored value (a partial update never resets the others); to return a field to
// its default, send the default value.
type PutSettings struct {
	SessionMaxAgeSeconds      *int  `json:"session_max_age_seconds,omitempty"`
	SessionIdleTimeoutSeconds *int  `json:"session_idle_timeout_seconds,omitempty"`
	DefaultAnonymousRead      *bool `json:"default_anonymous_read,omitempty"`
	TokenCleanupAfterDays     *int  `json:"token_cleanup_after_days,omitempty"`
}

type settingsRow struct {
	maxAge, idle *int32
	anon         *bool
	tokenDays    *int32
	revision     int64
	updated      *time.Time
}

func (h opsHandlers) renderSettings(row settingsRow) InstanceSettings {
	item := func(key, def, desc string, stored *string) SettingItem {
		s := SettingItem{Key: key, Value: def, Source: "default", Description: desc}
		if stored != nil {
			s.Value, s.Source = *stored, "stored"
		}
		return s
	}
	var maxAge, idle, anon, tokenDays *string
	if row.maxAge != nil {
		v := strconv.Itoa(int(*row.maxAge))
		maxAge = &v
	}
	if row.idle != nil {
		v := strconv.Itoa(int(*row.idle))
		idle = &v
	}
	if row.anon != nil {
		v := strconv.FormatBool(*row.anon)
		anon = &v
	}
	if row.tokenDays != nil {
		v := strconv.Itoa(int(*row.tokenDays))
		tokenDays = &v
	}
	tokenItem := item("token_cleanup_after_days", strconv.Itoa(maintenance.DefaultTokenCleanupDays), "Delete tokens that have been expired or revoked for more than this many days (0 to 3650; 0 keeps them forever).", tokenDays)
	if h.d.TokenCleanupDays != nil {
		tokenItem.Value, tokenItem.Source, tokenItem.ReadOnly, tokenItem.Enforced = strconv.Itoa(*h.d.TokenCleanupDays), "env", true, true
		tokenItem.Description += " Set by ANT_TOKEN_CLEANUP_AFTER_DAYS."
	} else {
		tokenItem.Enforced = true
	}
	out := InstanceSettings{Revision: row.revision, UpdatedAt: row.updated, Items: []SettingItem{
		item("session_max_age_seconds", strconv.Itoa(defaultSessionMaxAge), "Maximum browser session length (1 to 28800).", maxAge),
		item("session_idle_timeout_seconds", strconv.Itoa(defaultSessionIdle), "Browser session idle timeout (1 to 1800).", idle),
		item("default_anonymous_read", "false", "Suggested anonymous-read default for new feeds in the console.", anon),
		tokenItem,
	}}
	for _, e := range h.d.Env {
		out.Items = append(out.Items, SettingItem{Key: e.Key, Value: e.Value, Source: "env", ReadOnly: true, Enforced: true, Description: e.Description})
	}
	return out
}

func (h opsHandlers) loadSettings(ctx context.Context, instance string) (settingsRow, error) {
	row := settingsRow{revision: 1}
	err := h.db.QueryRow(ctx, `SELECT session_max_age_seconds,session_idle_timeout_seconds,default_anonymous_read,token_cleanup_after_days,revision,updated_at FROM instance_settings WHERE instance_id=$1`, instance).Scan(&row.maxAge, &row.idle, &row.anon, &row.tokenDays, &row.revision, &row.updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return settingsRow{revision: 1}, nil
	}
	return row, err
}

func (h opsHandlers) getSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	row, err := h.loadSettings(ctx, p.InstanceID)
	if err != nil {
		identityError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(row.revision, 10)))
	JSON(w, 200, h.renderSettings(row))
}

func (h opsHandlers) putSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var in PutSettings
	if !sourceJSON(w, r, &in, "session_max_age_seconds", "session_idle_timeout_seconds", "default_anonymous_read", "token_cleanup_after_days") {
		return
	}
	if in.SessionMaxAgeSeconds != nil && (*in.SessionMaxAgeSeconds < 1 || *in.SessionMaxAgeSeconds > maxSessionMaxAge) ||
		in.SessionIdleTimeoutSeconds != nil && (*in.SessionIdleTimeoutSeconds < 1 || *in.SessionIdleTimeoutSeconds > maxSessionIdleTimeout) ||
		in.TokenCleanupAfterDays != nil && (*in.TokenCleanupAfterDays < 0 || *in.TokenCleanupAfterDays > maintenance.MaxTokenCleanupDays) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid settings")
		return
	}
	if in.TokenCleanupAfterDays != nil && h.d.TokenCleanupDays != nil {
		Error(w, r, 409, api.ErrorCodeConflict, "token_cleanup_after_days is set by ANT_TOKEN_CLEANUP_AFTER_DAYS and is read-only")
		return
	}
	h.mutate(w, r, p, "settings.put", "settings", struct {
		In       PutSettings
		Revision int64
	}{in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		if _, err := tx.Exec(ctx, `INSERT INTO instance_settings(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, p.InstanceID); err != nil {
			return nil, err
		}
		var current int64
		var created time.Time
		if err := tx.QueryRow(ctx, `SELECT revision,updated_at FROM instance_settings WHERE instance_id=$1 FOR UPDATE`, p.InstanceID).Scan(&current, &created); err != nil {
			return nil, err
		}
		// A row created by this very transaction is the virtual revision 1.
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		// Absent fields keep their stored value.
		var maxAge, idle, tokenDays *int32
		if in.SessionMaxAgeSeconds != nil {
			v := int32(*in.SessionMaxAgeSeconds)
			maxAge = &v
		}
		if in.SessionIdleTimeoutSeconds != nil {
			v := int32(*in.SessionIdleTimeoutSeconds)
			idle = &v
		}
		if in.TokenCleanupAfterDays != nil {
			v := int32(*in.TokenCleanupAfterDays)
			tokenDays = &v
		}
		row := settingsRow{}
		if err := tx.QueryRow(ctx, `UPDATE instance_settings SET session_max_age_seconds=COALESCE($2,session_max_age_seconds),session_idle_timeout_seconds=COALESCE($3,session_idle_timeout_seconds),default_anonymous_read=COALESCE($4,default_anonymous_read),token_cleanup_after_days=COALESCE($5,token_cleanup_after_days),revision=revision+1,updated_at=now() WHERE instance_id=$1 RETURNING session_max_age_seconds,session_idle_timeout_seconds,default_anonymous_read,token_cleanup_after_days,revision,updated_at`, p.InstanceID, maxAge, idle, in.DefaultAnonymousRead, tokenDays).Scan(&row.maxAge, &row.idle, &row.anon, &row.tokenDays, &row.revision, &row.updated); err != nil {
			return nil, err
		}
		return h.renderSettings(row), identityEvent(ctx, tx, p, p.InstanceID, "settings.put", row.revision)
	})
}
