package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrStorage          = errors.New("secret storage unavailable")
	ErrNotFound         = errors.New("secret record not found")
	ErrRevisionConflict = errors.New("secret record revision changed")
	ErrStoreInput       = errors.New("invalid secret storage request")
)

// Store requires authorization by its caller for every operation. It never puts
// secret results in the generic idempotency cache. Keys must be externally loaded.
type Store struct {
	DB   *pgxpool.Pool
	Keys *Keyring
}
type Record struct {
	ID, Purpose string
	Revision    int64
}
type RotationResult struct{ Rotated int }

// One MiB of ciphertext expands to about 1.4 MiB in JSON base64. The persisted
// envelope bound leaves room for metadata while bounding malformed DB records.
const maxStoredEnvelopeBytes = 2 << 20

func decodeStoredEnvelope(encoded []byte, version string) (Envelope, error) {
	if len(encoded) == 0 || len(encoded) > maxStoredEnvelopeBytes {
		return Envelope{}, ErrInvalidEnvelope
	}
	var envelope Envelope
	if json.Unmarshal(encoded, &envelope) != nil || !versionPattern.MatchString(version) || envelope.KeyVersion != version {
		return Envelope{}, ErrInvalidEnvelope
	}
	return envelope, nil
}

func storageRollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func canonicalIDs(ctx context.Context, tx pgx.Tx, instance, id string) (string, string, error) {
	var a, b string
	if err := tx.QueryRow(ctx, "SELECT $1::uuid::text,$2::uuid::text", instance, id).Scan(&a, &b); err != nil {
		return "", "", ErrStoreInput
	}
	return a, b, nil
}
func secretContext(instance, id, purpose string) Context {
	return Context{RecordID: instance + "/" + id, Purpose: purpose}
}
func secretEvent(ctx context.Context, tx pgx.Tx, instance, actor string, r Record, action string) error {
	var audit int64
	if err := tx.QueryRow(ctx, "INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,$3,$4) RETURNING id", instance, actor, action, r.ID).Scan(&audit); err != nil {
		return ErrStorage
	}
	if _, err := tx.Exec(ctx, "INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('secret_id',$3::text,'revision',$4::bigint))", audit, action, r.ID, r.Revision); err != nil {
		return ErrStorage
	}
	return nil
}

// Create persists ciphertext and its audit/outbox event in one transaction.
// A random record ID is allocated before encryption to bind it into the AAD.
func (s Store) Create(ctx context.Context, instance, actor, purpose string, plaintext []byte) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s.Keys == nil || s.DB == nil {
		return Record{}, ErrStoreInput
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Record{}, ErrStorage
	}
	defer storageRollback(tx)
	var id string
	if err = tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		return Record{}, ErrStorage
	}
	instance, id, err = canonicalIDs(ctx, tx, instance, id)
	if err != nil {
		return Record{}, err
	}
	envelope, err := s.Keys.Seal(secretContext(instance, id, purpose), plaintext)
	if err != nil {
		return Record{}, err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return Record{}, ErrInvalidEnvelope
	}
	r := Record{ID: id, Purpose: purpose, Revision: 1}
	if _, err = tx.Exec(ctx, "INSERT INTO encrypted_secrets(instance_id,id,purpose,envelope,key_version) VALUES($1,$2,$3,$4,$5)", instance, id, purpose, encoded, envelope.KeyVersion); err != nil {
		return Record{}, ErrStorage
	}
	if err = secretEvent(ctx, tx, instance, actor, r, "secret.create"); err != nil {
		return Record{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Record{}, ErrStorage
	}
	return r, nil
}

// Open requires the expected purpose from trusted caller metadata. The returned
// plaintext belongs to the caller and should be cleared promptly after use.
func (s Store) Open(ctx context.Context, instance, id, purpose string) (Record, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if s.Keys == nil || s.DB == nil {
		return Record{}, nil, ErrStoreInput
	}
	var r Record
	var encoded []byte
	var storedVersion string
	var canonicalInstance string
	err := s.DB.QueryRow(ctx, "SELECT instance_id::text,id::text,purpose,revision,envelope,key_version FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose=$3", instance, id, purpose).Scan(&canonicalInstance, &r.ID, &r.Purpose, &r.Revision, &encoded, &storedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, nil, ErrNotFound
	}
	if err != nil {
		return Record{}, nil, ErrStorage
	}
	envelope, err := decodeStoredEnvelope(encoded, storedVersion)
	if err != nil {
		return Record{}, nil, err
	}
	plaintext, err := s.Keys.Open(secretContext(canonicalInstance, r.ID, r.Purpose), envelope)
	if err != nil {
		return Record{}, nil, err
	}
	return r, plaintext, nil
}

// Update replaces a secret at an exact revision. Purpose and record identity
// are immutable. Failed CAS, encryption, or audit leaves the old record intact.
func (s Store) Update(ctx context.Context, instance, actor, id, purpose string, expected int64, plaintext []byte) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s.Keys == nil || s.DB == nil || expected < 1 {
		return Record{}, ErrStoreInput
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Record{}, ErrStorage
	}
	defer storageRollback(tx)
	instance, id, err = canonicalIDs(ctx, tx, instance, id)
	if err != nil {
		return Record{}, err
	}
	var revision int64
	err = tx.QueryRow(ctx, "SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose=$3 FOR UPDATE", instance, id, purpose).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrStorage
	}
	if revision != expected {
		return Record{}, ErrRevisionConflict
	}
	envelope, err := s.Keys.Seal(secretContext(instance, id, purpose), plaintext)
	if err != nil {
		return Record{}, err
	}
	encoded, _ := json.Marshal(envelope)
	r := Record{ID: id, Purpose: purpose}
	if err = tx.QueryRow(ctx, "UPDATE encrypted_secrets SET envelope=$4,key_version=$5,revision=revision+1,updated_at=clock_timestamp() WHERE instance_id=$1 AND id=$2 AND revision=$3 RETURNING revision", instance, id, expected, encoded, envelope.KeyVersion).Scan(&r.Revision); err != nil {
		return Record{}, ErrStorage
	}
	if err = secretEvent(ctx, tx, instance, actor, r, "secret.update"); err != nil {
		return Record{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Record{}, ErrStorage
	}
	return r, nil
}

// RotateBatch locks at most limit old-version records with SKIP LOCKED. Every
// selected record is authenticated; any failure rolls back the entire batch.
// Restart by calling again: old key_version rows are the persistent work queue.
// A zero result is not proof of global completion while other workers hold locks;
// stop workers and inventory old-version rows before retiring a root key.
func (s Store) RotateBatch(ctx context.Context, instance, actor string, limit int) (RotationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.Keys == nil || s.DB == nil || limit < 1 || limit > 100 {
		return RotationResult{}, ErrStoreInput
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RotationResult{}, ErrStorage
	}
	defer storageRollback(tx)
	var canonicalInstance string
	if err = tx.QueryRow(ctx, "SELECT $1::uuid::text", instance).Scan(&canonicalInstance); err != nil {
		return RotationResult{}, ErrStoreInput
	}
	rows, err := tx.Query(ctx, "SELECT id::text,purpose,revision,envelope,key_version FROM encrypted_secrets WHERE instance_id=$1 AND key_version<>$2 ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED", canonicalInstance, s.Keys.active, limit)
	if err != nil {
		return RotationResult{}, ErrStorage
	}
	type pending struct {
		record  Record
		encoded []byte
		version string
	}
	var batch []pending
	for rows.Next() {
		var item pending
		if err = rows.Scan(&item.record.ID, &item.record.Purpose, &item.record.Revision, &item.encoded, &item.version); err != nil {
			rows.Close()
			return RotationResult{}, ErrStorage
		}
		batch = append(batch, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RotationResult{}, ErrStorage
	}
	for _, item := range batch {
		old, err := decodeStoredEnvelope(item.encoded, item.version)
		if err != nil {
			return RotationResult{}, err
		}
		rotated, err := s.Keys.Rewrap(secretContext(canonicalInstance, item.record.ID, item.record.Purpose), old)
		if err != nil {
			return RotationResult{}, err
		}
		encoded, _ := json.Marshal(rotated)
		tag, err := tx.Exec(ctx, "UPDATE encrypted_secrets SET envelope=$4,key_version=$5,revision=revision+1,updated_at=clock_timestamp() WHERE instance_id=$1 AND id=$2 AND revision=$3", canonicalInstance, item.record.ID, item.record.Revision, encoded, rotated.KeyVersion)
		if err != nil {
			return RotationResult{}, ErrStorage
		}
		if tag.RowsAffected() != 1 {
			return RotationResult{}, ErrRevisionConflict
		}
		item.record.Revision++
		if err = secretEvent(ctx, tx, canonicalInstance, actor, item.record, "secret.rewrap"); err != nil {
			return RotationResult{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return RotationResult{}, ErrStorage
	}
	return RotationResult{Rotated: len(batch)}, nil
}

// Diagnostic formatting must not expose the database connection or keys.
func (Store) String() string   { return "secrets.Store{redacted}" }
func (Store) GoString() string { return "secrets.Store{redacted}" }
