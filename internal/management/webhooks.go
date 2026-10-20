package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Webhook is one outgoing delivery subscription. Name is a unique display
// label; Endpoint is the full https URL that is delivered to.
type Webhook struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Endpoint   string    `json:"endpoint"`
	SecretID   string    `json:"secret_id,omitempty"`
	SecretName string    `json:"secret_name,omitempty"`
	Enabled    bool      `json:"enabled"`
	Revision   int64     `json:"revision"`
	CreatedAt  time.Time `json:"created_at"`
}
type WebhookDelivery struct {
	EventID     int64      `json:"event_id"`
	State       string     `json:"state"`
	Attempts    int32      `json:"attempts"`
	AvailableAt time.Time  `json:"available_at"`
	LastFailure *string    `json:"last_failure,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// WebhookOptions carries deployment policy for webhook configuration.
type WebhookOptions struct {
	// AllowHTTP permits plain http destinations (ANT_WEBHOOK_ALLOW_HTTP).
	AllowHTTP bool
}
type webhookHandlers struct {
	identityHandlers
	keys *secrets.Keyring
	opts WebhookOptions
}

const webhookSelect = `SELECT s.id::text,s.name,s.endpoint,COALESCE(s.signing_secret_id::text,''),COALESCE(w.name,''),s.enabled,s.revision,s.created_at
 FROM event_subscriptions s LEFT JOIN webhook_signing_secrets w ON w.instance_id=s.instance_id AND w.id=s.signing_secret_id`

func scanWebhook(row rowScanner) (Webhook, error) {
	var item Webhook
	err := row.Scan(&item.ID, &item.Name, &item.Endpoint, &item.SecretID, &item.SecretName, &item.Enabled, &item.Revision, &item.CreatedAt)
	return item, err
}

// RegisterWebhooks requires externally loaded root keys. It exposes no key,
// secret-reference, envelope, event payload or raw delivery diagnostics.
func RegisterWebhooks(mux *http.ServeMux, pool *pgxpool.Pool, keys *secrets.Keyring, options ...WebhookOptions) {
	h := webhookHandlers{identityHandlers: identityHandlers{feedHandlers{db: pool}}, keys: keys}
	if len(options) > 0 {
		h.opts = options[0]
	}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/webhooks": h.list, "POST /api/v1/webhooks": h.create,
		"PATCH /api/v1/webhooks/{id}": h.patch, "PUT /api/v1/webhooks/{id}/secret": h.rotate,
		"GET /api/v1/webhooks/{id}/deliveries": h.deliveries, "GET /api/v1/webhooks/{id}/failures": h.deliveries,
		"GET /api/v1/webhook-secrets": h.listSecrets, "POST /api/v1/webhook-secrets": h.createSecret,
		"GET /api/v1/webhook-secrets/{id}": h.getSecret, "POST /api/v1/webhook-secrets/{id}/rotate": h.rotateSecret,
		"DELETE /api/v1/webhook-secrets/{id}": h.deleteSecret,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}
func webhookInput(w http.ResponseWriter, r *http.Request) {
	Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid webhook configuration")
}

func (h webhookHandlers) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	after, limit, ok := identityPage(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := h.db.Query(ctx, webhookSelect+` WHERE s.instance_id=$1 AND ($2='' OR s.id>NULLIF($2,'')::uuid) ORDER BY s.id LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	items := []Webhook{}
	for rows.Next() {
		item, err := scanWebhook(rows)
		if err != nil {
			identityError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	JSON(w, 200, struct {
		Items []Webhook `json:"items"`
		Next  string    `json:"next_after,omitempty"`
	}{items, next})
}

// validWebhookName accepts a display label: 1 to 100 bytes of printable text.
func validWebhookName(name string) bool {
	if name != strings.TrimSpace(name) || name == "" || len(name) > 100 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// create takes {name, endpoint, secret_id}. The endpoint is the full https
// URL. For backward compatibility an inline secret ({endpoint, secret}) is
// still accepted and stored as a new signing secret, and a missing name is
// generated.
func (h webhookHandlers) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var input struct {
		Name     string `json:"name"`
		SecretID string `json:"secret_id"`
		Endpoint string `json:"endpoint"`
		Secret   string `json:"secret"`
	}
	if !sourceJSON(w, r, &input, "name", "secret_id", "endpoint", "secret") {
		return
	}
	if input.Name != "" && !validWebhookName(input.Name) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "name must be 1 to 100 printable characters")
		return
	}
	if input.Endpoint == "" {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "endpoint is required: the full https URL to deliver to")
		return
	}
	if input.SecretID != "" {
		var ok bool
		if input.SecretID, ok = feedID(input.SecretID); !ok || input.Secret != "" {
			webhookInput(w, r)
			return
		}
	}
	endpoint, err := events.ValidateEndpointFor(input.Endpoint, h.opts.AllowHTTP)
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "endpoint must be a public https URL without credentials, query or fragment")
		return
	}
	input.Endpoint = endpoint
	if (input.SecretID == "") == (input.Secret == "") || (input.Secret != "" && !validSecretValue(input.Secret)) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "choose a signing secret (secret_id)")
		return
	}
	if input.Secret != "" && h.keys == nil {
		identityError(w, r, secrets.ErrKeyConfiguration)
		return
	}
	// ExecuteMutation retains only the hash of this canonical (possibly secret-bearing)
	// request and the public subscription result, never the input or ciphertext.
	h.mutate(w, r, p, "webhook.create", "webhooks", input, 201, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return nil, err
		}
		name := input.Name
		if name == "" {
			name = "webhook-" + id[:8]
		}
		signing := input.SecretID
		if input.Secret != "" {
			plaintext := []byte(input.Secret)
			defer clear(plaintext)
			record, err := (secrets.Store{Keys: h.keys}).CreateTx(ctx, tx, p.InstanceID, p.PrincipalID, "webhook", plaintext)
			if err != nil {
				return nil, err
			}
			if err = tx.QueryRow(ctx, `INSERT INTO webhook_signing_secrets(instance_id,name,description,secret_id,created_by) VALUES($1,'webhook-'||left($2::text,8),'Created with the webhook.',$3,$4) RETURNING id::text`, p.InstanceID, id, record.ID, p.PrincipalID).Scan(&signing); err != nil {
				return nil, err
			}
		} else {
			// Share-lock the secret so a concurrent delete cannot slip between check and insert.
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT true FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2 FOR SHARE`, p.InstanceID, signing).Scan(&exists); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_subscriptions(id,instance_id,name,endpoint,signing_secret_id) VALUES($1,$2,$3,$4,$5)`, id, p.InstanceID, name, endpoint, signing); err != nil {
			return nil, err
		}
		item, err := scanWebhook(tx.QueryRow(ctx, webhookSelect+` WHERE s.instance_id=$1 AND s.id=$2`, p.InstanceID, id))
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, item.ID, "webhook.create", item.Revision)
	})
}
func webhookLocked(ctx context.Context, tx pgx.Tx, instance, id string, expected int64) (Webhook, string, error) {
	var secretRef *string
	item, err := scanWebhook(tx.QueryRow(ctx, webhookSelect+` WHERE s.instance_id=$1 AND s.id=$2 FOR UPDATE OF s`, instance, id))
	if err != nil {
		return item, "", identityDBError(err)
	}
	if err = tx.QueryRow(ctx, `SELECT secret_ref FROM event_subscriptions WHERE instance_id=$1 AND id=$2`, instance, id).Scan(&secretRef); err != nil {
		return item, "", err
	}
	if item.Revision != expected {
		return item, "", database.ErrRevisionConflict
	}
	if secretRef == nil {
		return item, "", nil
	}
	return item, *secretRef, nil
}

// patch changes enabled, the display name, the signing secret or the endpoint.
func (h webhookHandlers) patch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	var input struct {
		Enabled  *bool   `json:"enabled"`
		Name     *string `json:"name"`
		SecretID *string `json:"secret_id"`
		Endpoint *string `json:"endpoint"`
	}
	if !sourceJSON(w, r, &input, "enabled", "name", "secret_id", "endpoint") {
		return
	}
	if input.Enabled == nil && input.Name == nil && input.SecretID == nil && input.Endpoint == nil {
		webhookInput(w, r)
		return
	}
	if input.Name != nil && !validWebhookName(*input.Name) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "name must be 1 to 100 printable characters")
		return
	}
	if input.SecretID != nil {
		canonical, ok := feedID(*input.SecretID)
		if !ok {
			webhookInput(w, r)
			return
		}
		input.SecretID = &canonical
	}
	var endpoint *string
	if input.Endpoint != nil {
		v, verr := events.ValidateEndpointFor(*input.Endpoint, h.opts.AllowHTTP)
		if verr != nil {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "endpoint must be a public https URL without credentials, query or fragment")
			return
		}
		endpoint = &v
	}
	h.mutate(w, r, p, "webhook.patch", id, struct {
		Enabled  *bool
		Name     *string
		SecretID *string
		Endpoint *string
		Revision int64
	}{input.Enabled, input.Name, input.SecretID, endpoint, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		if _, _, err := webhookLocked(ctx, tx, p.InstanceID, id, revision); err != nil {
			return nil, err
		}
		var signing any
		if input.SecretID != nil {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT true FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2 FOR SHARE`, p.InstanceID, *input.SecretID).Scan(&exists); err != nil {
				return nil, err
			}
			signing = *input.SecretID
		}
		// A signing secret supersedes any legacy inline reference.
		if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET enabled=COALESCE($4,enabled),name=COALESCE($5,name),endpoint=COALESCE($6,endpoint),signing_secret_id=COALESCE($7::uuid,signing_secret_id),secret_ref=CASE WHEN $7::uuid IS NULL THEN secret_ref ELSE NULL END,revision=revision+1 WHERE instance_id=$1 AND id=$2 AND revision=$3`, p.InstanceID, id, revision, input.Enabled, input.Name, endpoint, signing); err != nil {
			return nil, err
		}
		item, err := scanWebhook(tx.QueryRow(ctx, webhookSelect+` WHERE s.instance_id=$1 AND s.id=$2`, p.InstanceID, id))
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, id, "webhook.patch", item.Revision)
	})
}

// rotate keeps the original per-webhook route: it replaces the value of the
// signing secret this webhook uses, which every other webhook using that secret
// shares. New integrations should rotate through /api/v1/webhook-secrets.
func (h webhookHandlers) rotate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	var input struct {
		Secret string `json:"secret"`
	}
	if !sourceJSON(w, r, &input, "secret") {
		return
	}
	if len(input.Secret) < 32 || len(input.Secret) > 4096 {
		webhookInput(w, r)
		return
	}
	if h.keys == nil {
		identityError(w, r, secrets.ErrKeyConfiguration)
		return
	}
	h.mutate(w, r, p, "webhook.secret", id, struct {
		Secret   string
		Revision int64
	}{input.Secret, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		item, ref, err := webhookLocked(ctx, tx, p.InstanceID, id, revision)
		if err != nil {
			return nil, err
		}
		if item.SecretID != "" {
			if _, err = h.rotateSecretTx(ctx, tx, p, item.SecretID, 0, input.Secret, 0); err != nil {
				return nil, err
			}
		} else {
			var secretRevision int64
			if err = tx.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose='webhook' FOR UPDATE`, p.InstanceID, ref).Scan(&secretRevision); err != nil {
				return nil, err
			}
			plaintext := []byte(input.Secret)
			defer clear(plaintext)
			if _, err = (secrets.Store{Keys: h.keys}).UpdateTx(ctx, tx, p.InstanceID, p.PrincipalID, ref, "webhook", secretRevision, plaintext); err != nil {
				return nil, err
			}
		}
		if err = tx.QueryRow(ctx, `UPDATE event_subscriptions SET revision=revision+1 WHERE instance_id=$1 AND id=$2 AND revision=$3 RETURNING revision`, p.InstanceID, id, revision).Scan(&item.Revision); err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, id, "webhook.secret", item.Revision)
	})
}
func (h webhookHandlers) deliveries(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		webhookInput(w, r)
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "limit" && key != "state") || len(values) != 1 {
			webhookInput(w, r)
			return
		}
	}
	var after int64
	limit := 50
	var err error
	if q.Has("after") {
		after, err = strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil || after < 1 {
			webhookInput(w, r)
			return
		}
	}
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 200 {
			webhookInput(w, r)
			return
		}
	}
	state := q.Get("state")
	if q.Has("state") && state != "pending" && state != "delivered" && state != "dead" {
		webhookInput(w, r)
		return
	}
	failures := r.Pattern == "GET /api/v1/webhooks/{id}/failures"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var found bool
	if err = h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM event_subscriptions WHERE instance_id=$1 AND id=$2)`, p.InstanceID, id).Scan(&found); err != nil {
		identityError(w, r, err)
		return
	}
	if !found {
		identityError(w, r, database.ErrNotFound)
		return
	}
	rows, err := h.db.Query(ctx, `SELECT d.event_id,d.state,d.attempts,d.available_at,d.last_failure,d.delivered_at FROM event_deliveries d JOIN event_subscriptions s ON s.id=d.subscription_id WHERE s.instance_id=$1 AND s.id=$2 AND d.event_id>$3 AND ($4='' OR d.state=$4) AND (NOT $5 OR d.last_failure IS NOT NULL) ORDER BY d.event_id LIMIT $6`, p.InstanceID, id, after, state, failures, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	items := []WebhookDelivery{}
	for rows.Next() {
		var item WebhookDelivery
		if err = rows.Scan(&item.EventID, &item.State, &item.Attempts, &item.AvailableAt, &item.LastFailure, &item.DeliveredAt); err != nil {
			identityError(w, r, err)
			return
		}
		// Even malformed historical rows cannot turn the status endpoint into an
		// arbitrary error/credential echo. Delivery failure classes are a closed set.
		if item.LastFailure != nil {
			switch *item.LastFailure {
			case "invalid_destination", "invalid_payload", "secret_unavailable", "transport_failure", "http_failure", "attempts_exhausted":
			default:
				value := "delivery_failure"
				item.LastFailure = &value
			}
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	var next *int64
	if len(items) > limit {
		items = items[:limit]
		value := items[len(items)-1].EventID
		next = &value
	}
	JSON(w, 200, struct {
		Items []WebhookDelivery `json:"items"`
		Next  *int64            `json:"next_after,omitempty"`
	}{items, next})
}
