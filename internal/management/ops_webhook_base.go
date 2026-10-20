package management

import (
	"context"
	"net/http"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/jackc/pgx/v5"
)

const webhookBaseKey = "webhook_base_url"

// webhookBaseItem renders the webhook base URL setting. A deployment override
// (ANT_WEBHOOK_BASE_URL, listed in Env) replaces the stored value and is read-only.
func (h opsHandlers) webhookBaseItem(stored *string) []SettingItem {
	for _, e := range h.d.Env {
		if e.Key == webhookBaseKey {
			return nil // the env item rendered with the other deployment settings
		}
	}
	item := SettingItem{Key: webhookBaseKey, Value: "", Source: "default", Enforced: true, Description: "Base URL that webhook names are appended to, for example https://hooks.example.com/ant/."}
	if stored != nil {
		item.Value, item.Source = *stored, "stored"
	}
	return []SettingItem{item}
}

// reresolveWebhooks rewrites the stored endpoint of every slug webhook so that it
// equals base + slug, auditing each change. Custom endpoints are never touched.
// An empty base leaves endpoints as they are. It returns the number changed.
func reresolveWebhooks(ctx context.Context, tx pgx.Tx, instance, actor, base string, allowHTTP bool) (int, error) {
	if base == "" {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text,name,endpoint FROM event_subscriptions WHERE instance_id=$1 AND NOT custom_endpoint ORDER BY id FOR UPDATE`, instance)
	if err != nil {
		return 0, err
	}
	type change struct{ id, endpoint string }
	var changes []change
	for rows.Next() {
		var id, name, endpoint string
		if err = rows.Scan(&id, &name, &endpoint); err != nil {
			rows.Close()
			return 0, err
		}
		want, rerr := events.ResolveEndpoint(base, name, allowHTTP)
		if rerr == nil && want != endpoint {
			changes = append(changes, change{id, want})
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	for _, c := range changes {
		var revision int64
		if err = tx.QueryRow(ctx, `UPDATE event_subscriptions SET endpoint=$3,revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING revision`, instance, c.id, c.endpoint).Scan(&revision); err != nil {
			return 0, err
		}
		var audit int64
		if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,NULLIF($2,'')::uuid,'webhook.reresolve',$3) RETURNING id`, instance, actor, c.id).Scan(&audit); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'webhook.reresolve',jsonb_build_object('resource_id',$2::text,'revision',$3::bigint))`, audit, c.id, revision); err != nil {
			return 0, err
		}
	}
	return len(changes), nil
}

// ReconcileWebhookEndpoints re-resolves slug webhooks against an effective base
// URL; the server calls it at startup when ANT_WEBHOOK_BASE_URL is set, so a
// changed deployment value is applied like an edited setting. It is a no-op when
// every endpoint already matches.
func ReconcileWebhookEndpoints(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}, instance, base string, allowHTTP bool) (int, error) {
	if base == "" {
		return 0, nil
	}
	normalized, err := events.NormalizeBaseURL(base, allowHTTP)
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	n, err := reresolveWebhooks(ctx, tx, instance, "", normalized, allowHTTP)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// putWebhookBaseURL sets or clears the webhook base URL. It has its own route so
// that it never interacts with the replace-all semantics of PUT /settings.
// Changing it re-resolves every slug webhook in the same transaction.
func (h opsHandlers) putWebhookBaseURL(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var in struct {
		Value string `json:"value"`
	}
	if !sourceJSON(w, r, &in, "value") {
		return
	}
	for _, e := range h.d.Env {
		if e.Key == webhookBaseKey {
			Error(w, r, 409, api.ErrorCodeConflict, "the webhook base URL is set by the deployment (ANT_WEBHOOK_BASE_URL) and is read-only")
			return
		}
	}
	base := ""
	if in.Value != "" {
		var err error
		if base, err = events.NormalizeBaseURL(in.Value, h.d.WebhookAllowHTTP); err != nil {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid webhook base URL: use https, no credentials, query or fragment")
			return
		}
	}
	h.mutate(w, r, p, "settings.webhook_base_url", "settings", struct {
		Value    string
		Revision int64
	}{base, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		if _, err := tx.Exec(ctx, `INSERT INTO instance_settings(instance_id) VALUES($1) ON CONFLICT DO NOTHING`, p.InstanceID); err != nil {
			return nil, err
		}
		var current int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM instance_settings WHERE instance_id=$1 FOR UPDATE`, p.InstanceID).Scan(&current); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		var stored any
		if base != "" {
			stored = base
		}
		if _, err := tx.Exec(ctx, `UPDATE instance_settings SET webhook_base_url=$2,revision=revision+1,updated_at=now() WHERE instance_id=$1`, p.InstanceID, stored); err != nil {
			return nil, err
		}
		if _, err := reresolveWebhooks(ctx, tx, p.InstanceID, p.PrincipalID, base, h.d.WebhookAllowHTTP); err != nil {
			return nil, err
		}
		row := settingsRow{}
		err := tx.QueryRow(ctx, `SELECT session_max_age_seconds,session_idle_timeout_seconds,default_anonymous_read,webhook_base_url,revision,updated_at FROM instance_settings WHERE instance_id=$1`, p.InstanceID).Scan(&row.maxAge, &row.idle, &row.anon, &row.webhookBase, &row.revision, &row.updated)
		if err != nil {
			return nil, err
		}
		return h.renderSettings(row), identityEvent(ctx, tx, p, p.InstanceID, "settings.webhook_base_url", row.revision)
	})
}
