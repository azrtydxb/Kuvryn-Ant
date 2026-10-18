package database

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrMutationInput       = errors.New("invalid mutation request")
	ErrMutationFailed      = errors.New("mutation transaction failed")
	ErrFingerprintConflict = errors.New("idempotency key reused with a different request")
	ErrConflict            = errors.New("resource conflict")
	ErrNotFound            = errors.New("resource not found")
	ErrRevisionConflict    = errors.New("resource revision changed")
	// ErrFederationManaged reports a change to a federation-managed row
	// (SQLSTATE AFM01 from the origin guard triggers, P2-M2).
	ErrFederationManaged = errors.New("resource is managed by the federation policy")
)

// MutationRequest scopes replay to an authenticated principal and one resource.
// Fingerprint must cover the canonical request, including conditional headers.
// NonSecretResult must be explicitly true: token/secret issuance is excluded.
// Key values are hashed before storage and are never included in errors.
type MutationRequest struct {
	InstanceID, PrincipalID, Operation, Resource, Key string
	Fingerprint                                       [32]byte
	NonSecretResult                                   bool
}
type MutationResult struct {
	JSON     json.RawMessage
	Replayed bool
}

// ExecuteMutation commits database effects and their replay result atomically.
// The callback must perform only database effects through tx, must not commit tx,
// and must return non-secret JSON. External effects belong in a transactional
// outbox. Concurrent matching requests execute the callback once. After 24 hours
// a key can be reused. Callers remain responsible for authorization on every call,
// including replays, and for periodic expiry cleanup to bound storage.
func ExecuteMutation(ctx context.Context, pool *pgxpool.Pool, req MutationRequest, apply func(context.Context, pgx.Tx) (json.RawMessage, error)) (MutationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	fail := func(err error) (MutationResult, error) { return MutationResult{}, err }
	if !req.NonSecretResult || req.InstanceID == "" || req.PrincipalID == "" || len(req.Operation) < 1 || len(req.Operation) > 128 || len(req.Resource) < 1 || len(req.Resource) > 1024 || len(req.Key) < 1 || len(req.Key) > 256 || apply == nil {
		return fail(ErrMutationInput)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fail(ErrMutationFailed)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	// Normalize UUID spellings before deriving the lock, matching PostgreSQL's
	// equality semantics even for uppercase or hyphenless inputs.
	if err = tx.QueryRow(ctx, "SELECT $1::uuid::text,$2::uuid::text", req.InstanceID, req.PrincipalID).Scan(&req.InstanceID, &req.PrincipalID); err != nil {
		return fail(ErrMutationInput)
	}
	key := sha256.Sum256([]byte(req.Key))
	// JSON array encoding preserves field boundaries. Hash collisions merely
	// serialize unrelated mutations; the full composite key decides replay.
	scope, _ := json.Marshal([]string{req.InstanceID, req.PrincipalID, req.Operation, req.Resource, req.Key})
	lock := sha256.Sum256(scope)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(lock[:8]))); err != nil {
		return fail(ErrMutationFailed)
	}
	args := []any{req.InstanceID, req.PrincipalID, req.Operation, req.Resource, key[:]}
	// clock_timestamp, unlike now(), reflects time after a contended lock wait.
	if _, err = tx.Exec(ctx, `DELETE FROM mutation_results WHERE instance_id=$1 AND principal_id=$2 AND operation=$3 AND resource=$4 AND key_hash=$5 AND expires_at<=clock_timestamp()`, args...); err != nil {
		return fail(ErrMutationFailed)
	}
	var fingerprint, response []byte
	err = tx.QueryRow(ctx, `SELECT fingerprint,response FROM mutation_results WHERE instance_id=$1 AND principal_id=$2 AND operation=$3 AND resource=$4 AND key_hash=$5`, args...).Scan(&fingerprint, &response)
	if err == nil {
		if string(fingerprint) != string(req.Fingerprint[:]) {
			return fail(ErrFingerprintConflict)
		}
		if err = tx.Commit(ctx); err != nil {
			return fail(ErrMutationFailed)
		}
		return MutationResult{JSON: response, Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fail(ErrMutationFailed)
	}
	response, err = apply(ctx, tx)
	if err != nil {
		switch {
		case errors.Is(err, ErrConflict):
			return fail(ErrConflict)
		case errors.Is(err, ErrNotFound):
			return fail(ErrNotFound)
		case errors.Is(err, ErrRevisionConflict):
			return fail(ErrRevisionConflict)
		case errors.Is(err, ErrFederationManaged):
			return fail(ErrFederationManaged)
		default:
			return fail(ErrMutationFailed)
		}
	}
	if len(response) > 1<<20 || !json.Valid(response) {
		return fail(ErrMutationInput)
	}
	args = append(args, req.Fingerprint[:], string(response))
	if _, err = tx.Exec(ctx, `INSERT INTO mutation_results(instance_id,principal_id,operation,resource,key_hash,fingerprint,response,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+interval '24 hours')`, args...); err != nil {
		return fail(ErrMutationFailed)
	}
	if err = tx.Commit(ctx); err != nil {
		return fail(ErrMutationFailed)
	}
	return MutationResult{JSON: response}, nil
}

// FeedUpdate changes mutable feed settings under an If-Match revision.
// Format is deliberately immutable. Caller must authorize the instance scope.
type FeedUpdate struct {
	InstanceID, ID, Slug string
	ExpectedRevision     int64
	AnonymousRead        bool
}

// UpdateFeed runs inside the caller's transaction so audit/outbox/idempotency
// can share its atomic commit. A row lock gives an unambiguous missing/stale
// distinction under concurrent updates and deletes.
func UpdateFeed(ctx context.Context, tx pgx.Tx, update FeedUpdate) (int64, error) {
	if update.ExpectedRevision < 1 {
		return 0, ErrRevisionConflict
	}
	var revision int64
	err := tx.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, update.InstanceID, update.ID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, ErrMutationFailed
	}
	if revision != update.ExpectedRevision {
		return 0, ErrRevisionConflict
	}
	err = tx.QueryRow(ctx, `UPDATE feeds SET slug=$3,anonymous_read=$4,revision=revision+1 WHERE instance_id=$1 AND id=$2 AND revision=$5 RETURNING revision`, update.InstanceID, update.ID, update.Slug, update.AnonymousRead, update.ExpectedRevision).Scan(&revision)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return 0, ErrConflict
		}
		return 0, ErrMutationFailed
	}
	return revision, nil
}
