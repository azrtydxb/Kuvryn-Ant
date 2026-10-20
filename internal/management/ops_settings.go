package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
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

// PutSettings replaces the editable values; an absent field reverts to its default.
type PutSettings struct {
	SessionMaxAgeSeconds      *int  `json:"session_max_age_seconds,omitempty"`
	SessionIdleTimeoutSeconds *int  `json:"session_idle_timeout_seconds,omitempty"`
	DefaultAnonymousRead      *bool `json:"default_anonymous_read,omitempty"`
}

type settingsRow struct {
	maxAge, idle *int32
	anon         *bool
	webhookBase  *string
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
	var maxAge, idle, anon *string
	var webhookBase *string
	if row.webhookBase != nil && *row.webhookBase != "" {
		v := *row.webhookBase
		webhookBase = &v
	}
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
	out := InstanceSettings{Revision: row.revision, UpdatedAt: row.updated, Items: []SettingItem{
		item("session_max_age_seconds", strconv.Itoa(defaultSessionMaxAge), "Maximum browser session length (1 to 28800).", maxAge),
		item("session_idle_timeout_seconds", strconv.Itoa(defaultSessionIdle), "Browser session idle timeout (1 to 1800).", idle),
		item("default_anonymous_read", "false", "Suggested anonymous-read default for new feeds in the console.", anon),
	}}
	out.Items = append(out.Items, h.webhookBaseItem(webhookBase)...)
	for _, e := range h.d.Env {
		out.Items = append(out.Items, SettingItem{Key: e.Key, Value: e.Value, Source: "env", ReadOnly: true, Enforced: true, Description: e.Description})
	}
	return out
}

func (h opsHandlers) loadSettings(ctx context.Context, instance string) (settingsRow, error) {
	row := settingsRow{revision: 1}
	err := h.db.QueryRow(ctx, `SELECT session_max_age_seconds,session_idle_timeout_seconds,default_anonymous_read,webhook_base_url,revision,updated_at FROM instance_settings WHERE instance_id=$1`, instance).Scan(&row.maxAge, &row.idle, &row.anon, &row.webhookBase, &row.revision, &row.updated)
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
	if !sourceJSON(w, r, &in, "session_max_age_seconds", "session_idle_timeout_seconds", "default_anonymous_read") {
		return
	}
	if in.SessionMaxAgeSeconds != nil && (*in.SessionMaxAgeSeconds < 1 || *in.SessionMaxAgeSeconds > maxSessionMaxAge) ||
		in.SessionIdleTimeoutSeconds != nil && (*in.SessionIdleTimeoutSeconds < 1 || *in.SessionIdleTimeoutSeconds > maxSessionIdleTimeout) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid settings")
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
		row := settingsRow{}
		if in.SessionMaxAgeSeconds != nil {
			v := int32(*in.SessionMaxAgeSeconds)
			row.maxAge = &v
		}
		if in.SessionIdleTimeoutSeconds != nil {
			v := int32(*in.SessionIdleTimeoutSeconds)
			row.idle = &v
		}
		row.anon = in.DefaultAnonymousRead
		if err := tx.QueryRow(ctx, `UPDATE instance_settings SET session_max_age_seconds=$2,session_idle_timeout_seconds=$3,default_anonymous_read=$4,revision=revision+1,updated_at=now() WHERE instance_id=$1 RETURNING webhook_base_url,revision,updated_at`, p.InstanceID, row.maxAge, row.idle, row.anon).Scan(&row.webhookBase, &row.revision, &row.updated); err != nil {
			return nil, err
		}
		return h.renderSettings(row), identityEvent(ctx, tx, p, p.InstanceID, "settings.put", row.revision)
	})
}
