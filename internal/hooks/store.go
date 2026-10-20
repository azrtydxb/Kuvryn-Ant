package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by pgx pools, connections and transactions.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// HookView is a hook with its signing secret name and seven-day statistics.
type HookView struct {
	Hook
	SecretName     string
	LastReceivedAt *time.Time
	Received7d     int
	Succeeded7d    int
	Failed7d       int
}

const hookColumns = `h.id::text,h.slug,h.description,h.enabled,h.auth_mode,COALESCE(h.hmac_format,''),COALESCE(h.signing_secret_id::text,''),h.token_hash,COALESCE(h.token_hint,''),COALESCE(h.bearer_scope,''),h.action,h.rate_per_minute,h.created_by::text,h.created_at,h.updated_at,h.revision`

func scanHook(row pgx.Row) (Hook, error) {
	var h Hook
	var action []byte
	if err := row.Scan(&h.ID, &h.Slug, &h.Description, &h.Enabled, &h.AuthMode, &h.HMACFormat, &h.SigningSecretID, &h.TokenHash, &h.TokenHint, &h.BearerScope, &action, &h.RatePerMinute, &h.CreatedBy, &h.CreatedAt, &h.UpdatedAt, &h.Revision); err != nil {
		return h, err
	}
	_ = json.Unmarshal(action, &h.Action)
	return h, nil
}

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// GetHook loads one hook of an instance.
func GetHook(ctx context.Context, q Querier, instance, id string) (Hook, error) {
	h, err := scanHook(q.QueryRow(ctx, `SELECT `+hookColumns+` FROM incoming_hooks h WHERE h.instance_id=$1 AND h.id=$2`, instance, id))
	return h, mapNoRows(err)
}

// LockHook is GetHook with a row lock, for updates.
func LockHook(ctx context.Context, tx pgx.Tx, instance, id string) (Hook, error) {
	h, err := scanHook(tx.QueryRow(ctx, `SELECT `+hookColumns+` FROM incoming_hooks h WHERE h.instance_id=$1 AND h.id=$2 FOR UPDATE`, instance, id))
	return h, mapNoRows(err)
}

// GetHookBySlug loads one hook by slug.
func GetHookBySlug(ctx context.Context, q Querier, instance, slug string) (Hook, error) {
	h, err := scanHook(q.QueryRow(ctx, `SELECT `+hookColumns+` FROM incoming_hooks h WHERE h.instance_id=$1 AND h.slug=$2`, instance, slug))
	return h, mapNoRows(err)
}

const viewSelect = `SELECT ` + hookColumns + `,COALESCE(w.name,''),
 (SELECT max(d.received_at) FROM incoming_hook_deliveries d WHERE d.hook_id=h.id),
 (SELECT count(*) FROM incoming_hook_deliveries d WHERE d.hook_id=h.id AND d.received_at>now()-interval '7 days' AND d.state<>'rejected'),
 (SELECT count(*) FROM incoming_hook_deliveries d WHERE d.hook_id=h.id AND d.received_at>now()-interval '7 days' AND d.state='succeeded'),
 (SELECT count(*) FROM incoming_hook_deliveries d WHERE d.hook_id=h.id AND d.received_at>now()-interval '7 days' AND d.state='failed')
 FROM incoming_hooks h LEFT JOIN webhook_signing_secrets w ON w.instance_id=h.instance_id AND w.id=h.signing_secret_id`

func scanView(row pgx.Row) (HookView, error) {
	var v HookView
	var action []byte
	err := row.Scan(&v.ID, &v.Slug, &v.Description, &v.Enabled, &v.AuthMode, &v.HMACFormat, &v.SigningSecretID, &v.TokenHash, &v.TokenHint, &v.BearerScope, &action, &v.RatePerMinute, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt, &v.Revision,
		&v.SecretName, &v.LastReceivedAt, &v.Received7d, &v.Succeeded7d, &v.Failed7d)
	if err == nil {
		_ = json.Unmarshal(action, &v.Action)
	}
	return v, err
}

// ViewHook loads a hook with statistics.
func ViewHook(ctx context.Context, q Querier, instance, id string) (HookView, error) {
	v, err := scanView(q.QueryRow(ctx, viewSelect+` WHERE h.instance_id=$1 AND h.id=$2`, instance, id))
	return v, mapNoRows(err)
}

// ListHooks pages hooks by slug.
func ListHooks(ctx context.Context, q Querier, instance, after string, limit int) ([]HookView, error) {
	rows, err := q.Query(ctx, viewSelect+` WHERE h.instance_id=$1 AND h.slug>$2 ORDER BY h.slug LIMIT $3`, instance, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookView
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// InsertHook stores a new hook. token, when non-empty, is hashed; the plaintext
// is never stored.
func InsertHook(ctx context.Context, tx pgx.Tx, instance string, h Hook, token string) (string, error) {
	action, _ := json.Marshal(h.Action)
	var hash any
	var hint any
	if token != "" {
		hash = tokenDigest(token)
		hint = token[max(0, len(token)-4):]
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO incoming_hooks(instance_id,slug,description,enabled,auth_mode,hmac_format,signing_secret_id,token_hash,token_hint,bearer_scope,action_type,action,rate_per_minute,created_by)
 VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,'')::uuid,$8,$9,NULLIF($10,''),$11,$12,$13,$14) RETURNING id::text`,
		instance, h.Slug, h.Description, h.Enabled, h.AuthMode, h.HMACFormat, h.SigningSecretID, hash, hint, h.BearerScope, h.Action.Type, action, h.RatePerMinute, h.CreatedBy).Scan(&id)
	return id, err
}

// UpdateHook rewrites a hook locked by LockHook. token, when non-empty,
// replaces the stored URL token digest.
func UpdateHook(ctx context.Context, tx pgx.Tx, instance string, h Hook, token string) error {
	action, _ := json.Marshal(h.Action)
	hash, hint := any(h.TokenHash), any(h.TokenHint)
	if token != "" {
		hash, hint = tokenDigest(token), token[max(0, len(token)-4):]
	}
	if h.AuthMode != AuthURLToken {
		hash, hint = nil, nil
	}
	_, err := tx.Exec(ctx, `UPDATE incoming_hooks SET slug=$3,description=$4,enabled=$5,auth_mode=$6,hmac_format=NULLIF($7,''),signing_secret_id=NULLIF($8,'')::uuid,token_hash=$9,token_hint=$10,bearer_scope=NULLIF($11,''),action_type=$12,action=$13,rate_per_minute=$14,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2`,
		instance, h.ID, h.Slug, h.Description, h.Enabled, h.AuthMode, h.HMACFormat, h.SigningSecretID, hash, hint, h.BearerScope, h.Action.Type, action, h.RatePerMinute)
	return err
}

// DeleteHook removes a hook, its deliveries and its remembered delivery ids.
func DeleteHook(ctx context.Context, tx pgx.Tx, instance, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM incoming_hooks WHERE instance_id=$1 AND id=$2`, instance, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// DeliveryList pages the delivery log of one hook, newest first. before is the
// ID of the last row of the previous page.
func ListDeliveries(ctx context.Context, q Querier, instance, hook, before, state string, limit int) ([]Delivery, error) {
	rows, err := q.Query(ctx, `SELECT d.id::text,d.hook_id::text,d.received_at,d.source_ip,d.auth_result,d.state,d.attempts,d.available_at,d.finished_at,COALESCE(d.action_result,''),d.detail,COALESCE(d.error,''),d.payload_size,d.preview,COALESCE(d.delivery_ref,''),COALESCE(d.redelivery_of::text,'')
 FROM incoming_hook_deliveries d WHERE d.instance_id=$1 AND d.hook_id=$2 AND ($3='' OR d.state=$3)
 AND ($4='' OR (d.received_at,d.id)<(SELECT received_at,id FROM incoming_hook_deliveries WHERE id=NULLIF($4,'')::uuid))
 ORDER BY d.received_at DESC,d.id DESC LIMIT $5`, instance, hook, state, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var detail []byte
		if err := rows.Scan(&d.ID, &d.HookID, &d.ReceivedAt, &d.SourceIP, &d.AuthResult, &d.State, &d.Attempts, &d.AvailableAt, &d.FinishedAt, &d.ActionResult, &detail, &d.Error, &d.PayloadSize, &d.Preview, &d.DeliveryRef, &d.RedeliveryOf); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &d.Detail)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SecretUsers counts the incoming hooks that use a signing secret for
// verification or for re-signing relayed payloads.
func SecretUsers(ctx context.Context, q Querier, instance, secret string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM incoming_hooks WHERE instance_id=$1 AND (signing_secret_id::text=$2 OR action->>'resign_secret_id'=$2)`, instance, secret).Scan(&n)
	return n, err
}
