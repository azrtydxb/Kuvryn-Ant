// Package events provides durable, at-least-once signed webhook delivery.
// Subscription administration requires external authorization; no public API is
// registered here. Consumers must deduplicate stable event IDs and validate the
// HMAC and timestamp. Outbox and delivery rows must not be pruned independently.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxPayload = 1 << 20
const maxAttempts = 8
const leaseDuration = 30 * time.Second

// Resolve must honor cancellation and return an owned key copy of at least32
// bytes. Its errors are never reflected in persisted failures or public errors.
type SecretResolver interface {
	Resolve(context.Context, string, string) ([]byte, error)
}
type Options struct {
	AllowPrivate, AllowHTTP bool
	InstanceID              string
	// Observe receives counts only, never errors, URLs, credentials or payloads.
	// It must be concurrency-safe and nonblocking; nil disables reporting.
	Observe func(Progress)
}
type Dispatcher struct {
	db      *pgxpool.Pool
	client  *http.Client
	secrets SecretResolver
	options Options
	running atomic.Bool
}

func New(db *pgxpool.Pool, client *http.Client, secrets SecretResolver, options Options) (*Dispatcher, error) {
	if db == nil || secrets == nil {
		return nil, ErrConfiguration
	}
	if options.InstanceID != "" {
		canonical, ok := instanceUUID(options.InstanceID)
		if !ok {
			return nil, ErrConfiguration
		}
		options.InstanceID = canonical
	}
	safe, err := safeClient(client, options)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{db: db, client: safe, secrets: secrets, options: options}, nil
}
func (d *Dispatcher) Close() { d.client.CloseIdleConnections() }
func (d *Dispatcher) Subscribe(ctx context.Context, instance, endpoint, secretRef string) (string, error) {
	canonical, ok := instanceUUID(instance)
	if !ok || (d.options.InstanceID != "" && canonical != d.options.InstanceID) {
		return "", ErrConfiguration
	}
	instance = canonical
	endpoint = canonicalEndpoint(endpoint)
	if !endpointAllowed(endpoint, d.options) || secretRef == "" || len(secretRef) > 256 {
		return "", ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id string
	if err := d.db.QueryRow(ctx, `INSERT INTO event_subscriptions(instance_id,endpoint,secret_ref) VALUES($1,$2,$3) RETURNING id`, instance, endpoint, secretRef).Scan(&id); err != nil {
		return "", ErrUnavailable
	}
	return id, nil
}

// Materialize discovers committed events using a durable anti-join, not id >
// checkpoint: PostgreSQL sequence allocation is not commit order. Thus a late
// commit with a lower event ID cannot disappear behind another transaction.
// Checkpoint is diagnostic high-water only. New subscriptions include backlog.
func (d *Dispatcher) Materialize(ctx context.Context, subscription string, batch int) (int, error) {
	if batch < 1 || batch > 200 {
		return 0, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if d.options.InstanceID != "" {
		var target string
		err := d.db.QueryRow(ctx, "SELECT instance_id::text FROM event_subscriptions WHERE id=$1", subscription).Scan(&target)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, ErrUnavailable
		}
		if target != d.options.InstanceID {
			return 0, ErrConfiguration
		}
	}
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return 0, ErrUnavailable
	}
	defer rollback(tx)
	var instance string
	if err = tx.QueryRow(ctx, `SELECT instance_id FROM event_subscriptions WHERE id=$1 AND enabled AND ($2='' OR instance_id=NULLIF($2,'')::uuid) FOR UPDATE`, subscription, d.options.InstanceID).Scan(&instance); errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, ErrUnavailable
	}
	tag, err := tx.Exec(ctx, `WITH candidates AS (
 SELECT o.id FROM outbox_events o JOIN audit_events a ON a.id=o.audit_id
 WHERE a.instance_id=$2 AND NOT EXISTS(SELECT 1 FROM event_deliveries e WHERE e.subscription_id=$1 AND e.event_id=o.id)
 ORDER BY o.id LIMIT $3)
 INSERT INTO event_deliveries(subscription_id,event_id) SELECT $1,id FROM candidates ON CONFLICT DO NOTHING`, subscription, instance, batch)
	if err != nil {
		return 0, ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `UPDATE event_subscriptions SET checkpoint=GREATEST(checkpoint,COALESCE((SELECT max(event_id) FROM event_deliveries WHERE subscription_id=$1),0)) WHERE id=$1`, subscription); err != nil {
		return 0, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, ErrUnavailable
	}
	return int(tag.RowsAffected()), nil
}

type delivery struct {
	subscription, instance, endpoint, secretRef, previousRef, token string
	eventID                                                         int64
	attempt                                                         int
	topic                                                           string
	payload                                                         []byte
	created                                                         time.Time
}

func (d *Dispatcher) claim(ctx context.Context) (delivery, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return delivery{}, false, ErrUnavailable
	}
	defer rollback(tx)
	// An exhausted final claim that crashed is terminal once its lease expires.
	if _, err = tx.Exec(ctx, `WITH exhausted AS (
 SELECT e.subscription_id,e.event_id FROM event_deliveries e
 JOIN event_subscriptions s ON s.id=e.subscription_id
 WHERE e.state='pending' AND e.attempts>=$1 AND e.lease_until<=clock_timestamp()
 AND ($2='' OR s.instance_id=NULLIF($2,'')::uuid)
 ORDER BY e.event_id,e.subscription_id LIMIT 200 FOR UPDATE OF e SKIP LOCKED
 ) UPDATE event_deliveries e SET state='dead',lease_token=NULL,lease_until=NULL,last_failure='attempts_exhausted'
 FROM exhausted x WHERE e.subscription_id=x.subscription_id AND e.event_id=x.event_id`, maxAttempts, d.options.InstanceID); err != nil {
		return delivery{}, false, ErrUnavailable
	}
	var item delivery
	var previous *string
	err = tx.QueryRow(ctx, `SELECT e.subscription_id,e.event_id,s.instance_id,s.endpoint,COALESCE(w.secret_id::text,s.secret_ref),CASE WHEN w.previous_expires_at>clock_timestamp() THEN w.previous_secret_id::text END,left(o.topic,257),
 CASE WHEN octet_length(o.payload::text)<=$1 THEN o.payload::text ELSE NULL END,o.created_at
 FROM event_deliveries e JOIN event_subscriptions s ON s.id=e.subscription_id
 LEFT JOIN webhook_signing_secrets w ON w.instance_id=s.instance_id AND w.id=s.signing_secret_id
 JOIN outbox_events o ON o.id=e.event_id JOIN audit_events a ON a.id=o.audit_id AND a.instance_id=s.instance_id
 WHERE s.enabled AND ($3='' OR s.instance_id=NULLIF($3,'')::uuid) AND e.state='pending' AND e.attempts<$2 AND e.available_at<=clock_timestamp() AND (e.lease_until IS NULL OR e.lease_until<=clock_timestamp())
 ORDER BY e.event_id,e.available_at FOR UPDATE OF e SKIP LOCKED LIMIT 1`, maxPayload, maxAttempts, d.options.InstanceID).Scan(&item.subscription, &item.eventID, &item.instance, &item.endpoint, &item.secretRef, &previous, &item.topic, &item.payload, &item.created)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.Commit(ctx); err != nil {
			return delivery{}, false, ErrUnavailable
		}
		return delivery{}, false, nil
	}
	if err != nil {
		return delivery{}, false, ErrUnavailable
	}
	if previous != nil {
		item.previousRef = *previous
	}
	if err = tx.QueryRow(ctx, `UPDATE event_deliveries SET attempts=attempts+1,lease_token=gen_random_uuid(),lease_until=clock_timestamp()+interval '30 seconds' WHERE subscription_id=$1 AND event_id=$2 RETURNING lease_token,attempts`, item.subscription, item.eventID).Scan(&item.token, &item.attempt); err != nil {
		return delivery{}, false, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return delivery{}, false, ErrUnavailable
	}
	return item, true, nil
}

// DeliverOne performs at most one network attempt. Callers bound worker count
// and scheduling. Leases exceed the20s total attempt deadline; crashed claims
// become retryable after30s. External success followed by a crash may redeliver.
func (d *Dispatcher) DeliverOne(ctx context.Context) (bool, error) {
	item, ok, err := d.claim(ctx)
	if err != nil || !ok {
		return ok, err
	}
	attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	failure := d.send(attemptCtx, item)
	cancel()
	state := "pending"
	if failure == "" {
		state = "delivered"
	} else if item.attempt >= maxAttempts || failure == "invalid_payload" || failure == "invalid_destination" {
		state = "dead"
	}
	// Finalization survives caller cancellation but is independently bounded.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	delay := backoff(item.attempt)
	tag, err := d.db.Exec(finish, `UPDATE event_deliveries SET state=$4,last_failure=NULLIF($5,''),available_at=clock_timestamp()+$6::bigint*interval '1 millisecond',delivered_at=CASE WHEN $4='delivered' THEN clock_timestamp() ELSE NULL END,lease_token=NULL,lease_until=NULL WHERE subscription_id=$1 AND event_id=$2 AND lease_token=$3 AND lease_until>clock_timestamp()`, item.subscription, item.eventID, item.token, state, failure, delay.Milliseconds())
	if err != nil {
		return true, ErrUnavailable
	}
	if tag.RowsAffected() != 1 {
		return true, ErrLeaseLost
	}
	return true, nil
}
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		attempt = 10
	}
	delay := time.Second * time.Duration(1<<uint(attempt-1))
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}
func (d *Dispatcher) send(ctx context.Context, item delivery) string {
	if d.options.InstanceID != "" && item.instance != d.options.InstanceID {
		return "invalid_destination"
	}
	if item.payload == nil || !json.Valid(item.payload) || len(item.topic) > 256 {
		return "invalid_payload"
	}
	if !endpointAllowed(item.endpoint, d.options) {
		return "invalid_destination"
	}
	secretCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	key, err := d.secrets.Resolve(secretCtx, item.instance, item.secretRef)
	cancel()
	defer clear(key)
	if err != nil || len(key) < 32 {
		return "secret_unavailable"
	}
	// During a rotation grace window the previous value signs too, so receivers
	// still holding either value can verify. An unavailable previous value is
	// skipped; only the current value is required.
	var previousKey []byte
	if item.previousRef != "" {
		prevCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		previous, perr := d.secrets.Resolve(prevCtx, item.instance, item.previousRef)
		stop()
		if perr == nil && len(previous) >= 32 {
			previousKey = previous
		}
	}
	defer clear(previousKey)
	id := strconv.FormatInt(item.eventID, 10)
	body, err := json.Marshal(struct {
		ID       string          `json:"id"`
		Instance string          `json:"instance_id"`
		Topic    string          `json:"topic"`
		Created  time.Time       `json:"created_at"`
		Payload  json.RawMessage `json:"payload"`
	}{id, item.instance, item.topic, item.created, item.payload})
	if err != nil {
		return "invalid_payload"
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	// Canonical signature input: timestamp + newline + event ID + newline + body.
	// X-Ant-Signature is a comma-separated list of "sha256=<hex>" entries: the
	// current secret first and, only inside a rotation grace window, the previous.
	// Receivers accept the request when any entry matches a secret they hold.
	signature := SignatureHeader(timestamp, id, body, key, previousKey)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, item.endpoint, bytes.NewReader(body))
	if err != nil {
		return "invalid_destination"
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Ant-Event-ID", id)
	request.Header.Set("X-Ant-Timestamp", timestamp)
	request.Header.Set("X-Ant-Signature", signature)
	response, err := d.client.Do(request)
	if err != nil {
		return "transport_failure"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return ""
	}
	return "http_failure"
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func instanceUUID(raw string) (string, bool) {
	var id pgtype.UUID
	if id.Scan(raw) != nil || !id.Valid {
		return "", false
	}
	return id.String(), true
}

// SignatureHeader returns the X-Ant-Signature value for a delivery: one
// "sha256=<hex>" entry per non-empty key, HMAC-SHA256 over timestamp, event ID
// and body joined by newlines. The same scheme signs outgoing webhook events
// and relayed incoming hooks, so receivers verify both identically.
func SignatureHeader(timestamp, id string, body []byte, keys ...[]byte) string {
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		if len(key) == 0 {
			continue
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(timestamp + "\n" + id + "\n"))
		mac.Write(body)
		entries = append(entries, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return strings.Join(entries, ",")
}
