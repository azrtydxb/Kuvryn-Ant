package management

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// idempotentOutcome is the stored result of a replay-safe mutation.
type idempotentOutcome struct {
	JSON     json.RawMessage
	Replayed bool
}

// idempotent runs apply under the shared 24h idempotency store. The caller's
// own error (domain failures such as a delete-policy denial) is returned
// unchanged through callErr so it can keep its specific status code; store
// failures are rendered here and ok is false. The Idempotency-Key header is
// required and must already have been validated by the caller via mutationKey.
func idempotent(r *http.Request, db *pgxpool.Pool, p identity.Principal, key, operation, resource string, input any, apply func(context.Context, pgx.Tx) (any, error)) (out idempotentOutcome, callErr error, storeErr error) {
	canonical, _ := json.Marshal(input)
	res, err := database.ExecuteMutation(r.Context(), db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: operation, Resource: resource, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		v, err := apply(ctx, tx)
		if err != nil {
			callErr = err
			return nil, err
		}
		return json.Marshal(v)
	})
	if err != nil {
		if callErr != nil {
			return out, callErr, nil
		}
		return out, nil, err
	}
	return idempotentOutcome{JSON: res.JSON, Replayed: res.Replayed}, nil, nil
}

// storeError renders an idempotency-store failure.
func storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrFingerprintConflict), errors.Is(err, database.ErrConflict):
		Error(w, r, 409, api.ErrorCodeConflict, "idempotency key conflict")
	case errors.Is(err, database.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "resource not found")
	case errors.Is(err, database.ErrRevisionConflict):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "resource revision changed")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "mutation unavailable")
	}
}
