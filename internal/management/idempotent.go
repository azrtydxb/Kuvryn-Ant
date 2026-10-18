package management

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

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

// reqError is a client-visible domain failure raised inside an idempotent
// apply callback. It is rendered with its own status instead of the generic
// store mapping.
type reqError struct {
	status  int
	code    api.ErrorCode
	message string
}

func (e reqError) Error() string { return e.message }

func badInput(what string) error {
	return reqError{400, api.ErrorCodeInvalidRequest, "invalid " + what}
}

// replayWrite renders a stored idempotent outcome with the original status.
// 204 has no body; documents that carry a revision keep their ETag on replay.
func replayWrite(w http.ResponseWriter, r *http.Request, out idempotentOutcome, status int) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	var rev struct {
		Revision int64 `json:"revision"`
	}
	if json.Unmarshal(out.JSON, &rev) == nil && rev.Revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(rev.Revision, 10)))
	}
	JSON(w, status, out.JSON)
}

// idempotentRun requires the Idempotency-Key, runs apply under the shared
// replay store and renders domain/store failures. ok is false when a response
// has already been written. Callers write the success response themselves via
// replayWrite so secrets (never stored) can be added to first-time responses.
func idempotentRun(w http.ResponseWriter, r *http.Request, db *pgxpool.Pool, p identity.Principal, operation, resource string, input any, apply func(context.Context, pgx.Tx) (any, error)) (idempotentOutcome, bool) {
	key, ok := mutationKey(w, r)
	if !ok {
		return idempotentOutcome{}, false
	}
	out, callErr, storeErr := idempotent(r, db, p, key, operation, resource, input, apply)
	if callErr != nil {
		var re reqError
		if errors.As(callErr, &re) {
			Error(w, r, re.status, re.code, re.message)
			return out, false
		}
		identityError(w, r, identityDBError(callErr))
		return out, false
	}
	if storeErr != nil {
		storeError(w, r, storeErr)
		return out, false
	}
	return out, true
}
