package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
)

// maxGraceSeconds bounds how long a rotated-out signing value stays valid.
const maxGraceSeconds = 7 * 24 * 3600

// WebhookRef names a webhook that uses a signing secret.
type WebhookRef struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Endpoint string `json:"endpoint"`
}

// WebhookSecret is the metadata of a signing secret. The value is carried only
// by the response to the create or rotate call that produced it, never stored in
// the idempotency cache and never returned again.
type WebhookSecret struct {
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	CreatedAt          time.Time    `json:"created_at"`
	CreatedBy          string       `json:"created_by,omitempty"`
	RotatedAt          *time.Time   `json:"rotated_at,omitempty"`
	Revision           int64        `json:"revision"`
	PreviousValidUntil *time.Time   `json:"previous_valid_until,omitempty"`
	UsedByCount        int          `json:"used_by_count"`
	UsedBy             []WebhookRef `json:"used_by"`
	Value              string       `json:"value,omitempty"`
}

const webhookSecretColumns = `w.id::text,w.name,w.description,COALESCE(w.created_by::text,''),w.created_at,w.rotated_at,w.revision,CASE WHEN w.previous_expires_at>clock_timestamp() THEN w.previous_expires_at END`

func scanWebhookSecret(row rowScanner) (WebhookSecret, error) {
	var s WebhookSecret
	err := row.Scan(&s.ID, &s.Name, &s.Description, &s.CreatedBy, &s.CreatedAt, &s.RotatedAt, &s.Revision, &s.PreviousValidUntil)
	s.UsedBy = []WebhookRef{}
	return s, err
}

type dbQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadWebhookUsers(ctx context.Context, q dbQuerier, instance string, byID map[string]*WebhookSecret) error {
	if len(byID) == 0 {
		return nil
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	rows, err := q.Query(ctx, `SELECT signing_secret_id::text,id::text,COALESCE(name,''),endpoint FROM event_subscriptions WHERE instance_id=$1 AND signing_secret_id=ANY($2::uuid[]) ORDER BY created_at,id`, instance, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var secret string
		var ref WebhookRef
		if err = rows.Scan(&secret, &ref.ID, &ref.Name, &ref.Endpoint); err != nil {
			return err
		}
		if s := byID[secret]; s != nil {
			s.UsedBy = append(s.UsedBy, ref)
			s.UsedByCount = len(s.UsedBy)
		}
	}
	return rows.Err()
}

func loadWebhookSecret(ctx context.Context, q dbQuerier, instance, id string) (WebhookSecret, error) {
	s, err := scanWebhookSecret(q.QueryRow(ctx, `SELECT `+webhookSecretColumns+` FROM webhook_signing_secrets w WHERE w.instance_id=$1 AND w.id=$2`, instance, id))
	if err != nil {
		return s, identityDBError(err)
	}
	err = loadWebhookUsers(ctx, q, instance, map[string]*WebhookSecret{s.ID: &s})
	return s, err
}

func validSecretText(v string, max int, allowEmpty bool) bool {
	if v == "" {
		return allowEmpty
	}
	return len(v) <= max && utf8.ValidString(v) && strings.TrimSpace(v) == v && !strings.ContainsFunc(v, unicode.IsControl)
}

func validSecretValue(v string) bool { return len(v) >= 32 && len(v) <= 4096 }

// generateSecretValue returns 32 random bytes in URL-safe base64 with a
// recognisable prefix.
func generateSecretValue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	defer clear(raw)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (h webhookHandlers) listSecrets(w http.ResponseWriter, r *http.Request) {
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
	rows, err := h.db.Query(ctx, `SELECT `+webhookSecretColumns+` FROM webhook_signing_secrets w WHERE w.instance_id=$1 AND ($2='' OR w.id>NULLIF($2,'')::uuid) ORDER BY w.id LIMIT $3`, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	items := []WebhookSecret{}
	for rows.Next() {
		s, err := scanWebhookSecret(rows)
		if err != nil {
			rows.Close()
			identityError(w, r, err)
			return
		}
		items = append(items, s)
	}
	rows.Close()
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	index := map[string]*WebhookSecret{}
	for i := range items {
		index[items[i].ID] = &items[i]
	}
	if err = loadWebhookUsers(ctx, h.db, p.InstanceID, index); err != nil {
		identityError(w, r, err)
		return
	}
	JSON(w, 200, struct {
		Items []WebhookSecret `json:"items"`
		Next  string          `json:"next_after,omitempty"`
	}{items, next})
}

func (h webhookHandlers) getSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		identityInput(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	s, err := loadWebhookSecret(ctx, h.db, p.InstanceID, id)
	if err != nil {
		identityError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(s.Revision, 10)))
	JSON(w, 200, s)
}

// secretMutate runs apply as an idempotent audited mutation. The cached result
// is value-free; value is attached to the response only when apply really ran,
// so a replay returns the metadata without the secret.
func (h webhookHandlers) secretMutate(w http.ResponseWriter, r *http.Request, p identity.Principal, operation, resource string, input any, status int, value string, apply func(context.Context, pgx.Tx) (any, error)) {
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	canonical, _ := json.Marshal(input)
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: operation, Resource: resource, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		v, err := apply(ctx, tx)
		if err != nil {
			return nil, identityDBError(err)
		}
		return json.Marshal(v)
	})
	if err != nil {
		identityError(w, r, err)
		return
	}
	body := result.JSON
	var meta struct {
		Revision int64 `json:"revision"`
	}
	_ = json.Unmarshal(body, &meta)
	if meta.Revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(meta.Revision, 10)))
	}
	if value != "" && !result.Replayed {
		var s WebhookSecret
		if json.Unmarshal(body, &s) == nil {
			s.Value = value
			JSON(w, status, s)
			return
		}
	}
	JSON(w, status, body)
}

// purgeExpiredPrevious drops previous values whose grace window has ended, in
// the caller's transaction, so no expired ciphertext lingers.
func purgeExpiredPrevious(ctx context.Context, tx pgx.Tx, instance string) error {
	rows, err := tx.Query(ctx, `SELECT id::text,previous_secret_id::text FROM webhook_signing_secrets WHERE instance_id=$1 AND previous_expires_at<=clock_timestamp() FOR UPDATE`, instance)
	if err != nil {
		return err
	}
	type pair struct{ id, previous string }
	var expired []pair
	for rows.Next() {
		var x pair
		if err = rows.Scan(&x.id, &x.previous); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, x)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, x := range expired {
		if _, err = tx.Exec(ctx, `UPDATE webhook_signing_secrets SET previous_secret_id=NULL,previous_purpose=NULL,previous_expires_at=NULL WHERE instance_id=$1 AND id=$2`, instance, x.id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose='webhook'`, instance, x.previous); err != nil {
			return err
		}
	}
	return nil
}

func (h webhookHandlers) createSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Value       string `json:"value"`
	}
	if !sourceJSON(w, r, &input, "name", "description", "value") {
		return
	}
	if !validSecretText(input.Name, 200, false) || !validSecretText(input.Description, 1000, true) || (input.Value != "" && !validSecretValue(input.Value)) {
		webhookInput(w, r)
		return
	}
	if h.keys == nil {
		identityError(w, r, secrets.ErrKeyConfiguration)
		return
	}
	value := input.Value
	fingerprint := struct{ Name, Description, Value string }{input.Name, input.Description, input.Value}
	if value == "" {
		var err error
		if value, err = generateSecretValue(); err != nil {
			identityError(w, r, err)
			return
		}
		fingerprint.Value = "" // generated: the retry carries no value
	}
	h.secretMutate(w, r, p, "webhook_secret.create", "webhook-secrets", fingerprint, 201, value, func(ctx context.Context, tx pgx.Tx) (any, error) {
		if err := purgeExpiredPrevious(ctx, tx, p.InstanceID); err != nil {
			return nil, err
		}
		plaintext := []byte(value)
		defer clear(plaintext)
		record, err := (secrets.Store{Keys: h.keys}).CreateTx(ctx, tx, p.InstanceID, p.PrincipalID, "webhook", plaintext)
		if err != nil {
			return nil, err
		}
		var id string
		if err = tx.QueryRow(ctx, `INSERT INTO webhook_signing_secrets(instance_id,name,description,secret_id,created_by) VALUES($1,$2,$3,$4,$5) RETURNING id::text`, p.InstanceID, input.Name, input.Description, record.ID, p.PrincipalID).Scan(&id); err != nil {
			return nil, err
		}
		item, err := loadWebhookSecret(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		return item, identityEvent(ctx, tx, p, id, "webhook_secret.create", item.Revision)
	})
}

// rotateSecretTx replaces the value of a signing secret in the caller's
// transaction. expected is the required revision (0 skips the check). With a
// grace window the previous value stays valid for signing, and is signed along
// with the new one (see internal/events), until the window ends.
func (h webhookHandlers) rotateSecretTx(ctx context.Context, tx pgx.Tx, p identity.Principal, id string, expected int64, value string, grace time.Duration) (WebhookSecret, error) {
	if err := purgeExpiredPrevious(ctx, tx, p.InstanceID); err != nil {
		return WebhookSecret{}, err
	}
	var current string
	var previous *string
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT secret_id::text,previous_secret_id::text,revision FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id).Scan(&current, &previous, &revision); err != nil {
		return WebhookSecret{}, identityDBError(err)
	}
	if expected != 0 && revision != expected {
		return WebhookSecret{}, database.ErrRevisionConflict
	}
	plaintext := []byte(value)
	defer clear(plaintext)
	store := secrets.Store{Keys: h.keys}
	if grace <= 0 {
		var secretRevision int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose='webhook' FOR UPDATE`, p.InstanceID, current).Scan(&secretRevision); err != nil {
			return WebhookSecret{}, err
		}
		if _, err := store.UpdateTx(ctx, tx, p.InstanceID, p.PrincipalID, current, "webhook", secretRevision, plaintext); err != nil {
			return WebhookSecret{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE webhook_signing_secrets SET previous_secret_id=NULL,previous_purpose=NULL,previous_expires_at=NULL,rotated_at=clock_timestamp(),revision=revision+1 WHERE instance_id=$1 AND id=$2`, p.InstanceID, id); err != nil {
			return WebhookSecret{}, err
		}
	} else {
		record, err := store.CreateTx(ctx, tx, p.InstanceID, p.PrincipalID, "webhook", plaintext)
		if err != nil {
			return WebhookSecret{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE webhook_signing_secrets SET secret_id=$3,previous_secret_id=$4,previous_purpose='webhook',previous_expires_at=clock_timestamp()+$5::bigint*interval '1 second',rotated_at=clock_timestamp(),revision=revision+1 WHERE instance_id=$1 AND id=$2`, p.InstanceID, id, record.ID, current, int64(grace/time.Second)); err != nil {
			return WebhookSecret{}, err
		}
		// A still-valid older previous value is superseded by the one just retired.
		if previous != nil {
			if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose='webhook'`, p.InstanceID, *previous); err != nil {
				return WebhookSecret{}, err
			}
		}
	}
	item, err := loadWebhookSecret(ctx, tx, p.InstanceID, id)
	if err != nil {
		return item, err
	}
	return item, identityEvent(ctx, tx, p, id, "webhook_secret.rotate", item.Revision)
}

func (h webhookHandlers) rotateSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	var input struct {
		Value        string `json:"value"`
		GraceSeconds int    `json:"grace_seconds"`
	}
	if !sourceJSON(w, r, &input, "value", "grace_seconds") {
		return
	}
	if (input.Value != "" && !validSecretValue(input.Value)) || input.GraceSeconds < 0 || input.GraceSeconds > maxGraceSeconds {
		webhookInput(w, r)
		return
	}
	if h.keys == nil {
		identityError(w, r, secrets.ErrKeyConfiguration)
		return
	}
	value := input.Value
	fingerprint := struct {
		Value        string
		GraceSeconds int
		Revision     int64
	}{input.Value, input.GraceSeconds, revision}
	if value == "" {
		var err error
		if value, err = generateSecretValue(); err != nil {
			identityError(w, r, err)
			return
		}
	}
	h.secretMutate(w, r, p, "webhook_secret.rotate", id, fingerprint, 200, value, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return h.rotateSecretTx(ctx, tx, p, id, revision, value, time.Duration(input.GraceSeconds)*time.Second)
	})
}

func (h webhookHandlers) deleteSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	// Friendly refusal before the mutation; the transaction re-checks under lock.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var used int
	err := h.db.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE instance_id=$1 AND signing_secret_id=$2`, p.InstanceID, id).Scan(&used)
	cancel()
	if err != nil {
		identityError(w, r, err)
		return
	}
	if used > 0 {
		Error(w, r, 409, api.ErrorCodeConflict, "signing secret is still used by "+strconv.Itoa(used)+" webhook(s); move them to another secret first")
		return
	}
	h.secretMutate(w, r, p, "webhook_secret.delete", id, struct{ Revision int64 }{revision}, 200, "", func(ctx context.Context, tx pgx.Tx) (any, error) {
		var current string
		var previous *string
		var found int64
		if err := tx.QueryRow(ctx, `SELECT secret_id::text,previous_secret_id::text,revision FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id).Scan(&current, &previous, &found); err != nil {
			return nil, err
		}
		if found != revision {
			return nil, database.ErrRevisionConflict
		}
		var users int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_subscriptions WHERE instance_id=$1 AND signing_secret_id=$2`, p.InstanceID, id).Scan(&users); err != nil {
			return nil, err
		}
		if users > 0 {
			return nil, database.ErrConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2`, p.InstanceID, id); err != nil {
			return nil, err
		}
		for _, ref := range []*string{&current, previous} {
			if ref == nil {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose='webhook'`, p.InstanceID, *ref); err != nil {
				return nil, err
			}
		}
		return struct {
			ID      string `json:"id"`
			Deleted bool   `json:"deleted"`
		}{id, true}, identityEvent(ctx, tx, p, id, "webhook_secret.delete", found)
	})
}
