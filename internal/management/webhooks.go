package management

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Webhook struct {
	ID        string    `json:"id"`
	Endpoint  string    `json:"endpoint"`
	Enabled   bool      `json:"enabled"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
}
type WebhookDelivery struct {
	EventID     int64      `json:"event_id"`
	State       string     `json:"state"`
	Attempts    int32      `json:"attempts"`
	AvailableAt time.Time  `json:"available_at"`
	LastFailure *string    `json:"last_failure,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}
type webhookHandlers struct {
	identityHandlers
	keys *secrets.Keyring
}

// RegisterWebhooks requires externally loaded root keys. It exposes no key,
// secret-reference, envelope, event payload or raw delivery diagnostics.
func RegisterWebhooks(mux *http.ServeMux, pool *pgxpool.Pool, keys *secrets.Keyring) {
	h := webhookHandlers{identityHandlers{feedHandlers{db: pool}}, keys}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/webhooks": h.list, "POST /api/v1/webhooks": h.create,
		"PATCH /api/v1/webhooks/{id}": h.patch, "PUT /api/v1/webhooks/{id}/secret": h.rotate,
		"GET /api/v1/webhooks/{id}/deliveries": h.deliveries, "GET /api/v1/webhooks/{id}/failures": h.deliveries,
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
	rows, err := h.db.Query(ctx, `SELECT id::text,endpoint,enabled,revision,created_at FROM event_subscriptions WHERE instance_id=$1 AND ($2='' OR id>NULLIF($2,'')::uuid) ORDER BY id LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	items := []Webhook{}
	for rows.Next() {
		var item Webhook
		if err = rows.Scan(&item.ID, &item.Endpoint, &item.Enabled, &item.Revision, &item.CreatedAt); err != nil {
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
func (h webhookHandlers) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var input struct {
		Endpoint string `json:"endpoint"`
		Secret   string `json:"secret"`
	}
	if !sourceJSON(w, r, &input, "endpoint", "secret") {
		return
	}
	endpoint, err := events.ValidateEndpoint(input.Endpoint)
	if err != nil || len(input.Secret) < 32 || len(input.Secret) > 4096 {
		webhookInput(w, r)
		return
	}
	input.Endpoint = endpoint
	if h.keys == nil {
		identityError(w, r, secrets.ErrKeyConfiguration)
		return
	}
	// ExecuteMutation retains only the hash of this canonical secret-bearing
	// request and the public subscription result, never the input or ciphertext.
	h.mutate(w, r, p, "webhook.create", "webhooks", input, 201, func(ctx context.Context, tx pgx.Tx) (any, error) {
		plaintext := []byte(input.Secret)
		defer clear(plaintext)
		record, err := (secrets.Store{Keys: h.keys}).CreateTx(ctx, tx, p.InstanceID, p.PrincipalID, "webhook", plaintext)
		if err != nil {
			return nil, err
		}
		var item Webhook
		err = tx.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) VALUES($1,$2,$3) RETURNING id::text,endpoint,enabled,revision,created_at`, p.InstanceID, endpoint, record.ID).Scan(&item.ID, &item.Endpoint, &item.Enabled, &item.Revision, &item.CreatedAt)
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, item.ID, "webhook.create", item.Revision)
	})
}
func webhookLocked(ctx context.Context, tx pgx.Tx, instance, id string, expected int64) (Webhook, string, error) {
	var item Webhook
	var secretRef string
	err := tx.QueryRow(ctx, `SELECT id::text,endpoint,enabled,revision,created_at,secret_ref FROM event_subscriptions WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, id).Scan(&item.ID, &item.Endpoint, &item.Enabled, &item.Revision, &item.CreatedAt, &secretRef)
	if err != nil {
		return item, "", identityDBError(err)
	}
	if item.Revision != expected {
		return item, "", database.ErrRevisionConflict
	}
	return item, secretRef, nil
}
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
		Enabled *bool `json:"enabled"`
	}
	if !sourceJSON(w, r, &input, "enabled") {
		return
	}
	if input.Enabled == nil {
		webhookInput(w, r)
		return
	}
	h.mutate(w, r, p, "webhook.patch", id, struct {
		Enabled  bool
		Revision int64
	}{*input.Enabled, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		item, _, err := webhookLocked(ctx, tx, p.InstanceID, id, revision)
		if err != nil {
			return nil, err
		}
		item.Enabled = *input.Enabled
		if err = tx.QueryRow(ctx, `UPDATE event_subscriptions SET enabled=$4,revision=revision+1 WHERE instance_id=$1 AND id=$2 AND revision=$3 RETURNING revision`, p.InstanceID, id, revision, item.Enabled).Scan(&item.Revision); err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, id, "webhook.patch", item.Revision)
	})
}
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
		var secretRevision int64
		if err = tx.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose='webhook' FOR UPDATE`, p.InstanceID, ref).Scan(&secretRevision); err != nil {
			return nil, err
		}
		plaintext := []byte(input.Secret)
		defer clear(plaintext)
		if _, err = (secrets.Store{Keys: h.keys}).UpdateTx(ctx, tx, p.InstanceID, p.PrincipalID, ref, "webhook", secretRevision, plaintext); err != nil {
			return nil, err
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
