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
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/policytemplate"
	"github.com/jackc/pgx/v5"
)

// cfgError is a configuration rejection with a specific, client-safe message.
// It never carries database diagnostics.
type cfgError struct {
	status int
	code   api.ErrorCode
	msg    string
}

func (e *cfgError) Error() string { return e.msg }
func invalidConfig(msg string) error {
	return &cfgError{http.StatusBadRequest, api.ErrorCodeInvalidRequest, msg}
}
func conflictConfig(msg string) error {
	return &cfgError{http.StatusConflict, api.ErrorCodeConflict, msg}
}

// rejection writes the response for a typed configuration rejection (explicit
// messages, resolution loops, template cycles) and reports whether it did.
func rejection(w http.ResponseWriter, r *http.Request, err error) bool {
	var ce *cfgError
	var loop *feedgraph.LoopError
	switch {
	case errors.As(err, &ce):
		Error(w, r, ce.status, ce.code, ce.msg)
	case errors.As(err, &loop):
		Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, loop.Error())
	case errors.Is(err, policytemplate.ErrCycle):
		Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "policy cycle: "+err.Error())
	case errors.Is(err, policytemplate.ErrDepth):
		Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "policy template chain exceeds the maximum depth of 8")
	case errors.Is(err, policytemplate.ErrKind):
		Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "a template can only extend or be bound as a template of the same kind")
	default:
		return false
	}
	return true
}

// mutateCfg is identityHandlers.mutate for handlers whose apply function can
// reject with a specific message. status is the success status.
func (h identityHandlers) mutateCfg(w http.ResponseWriter, r *http.Request, p identity.Principal, operation, resource string, input any, status int, apply func(context.Context, pgx.Tx) (any, error)) {
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	canonical, _ := json.Marshal(input)
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: operation, Resource: resource, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		value, err := apply(ctx, tx)
		if err != nil {
			applyErr = err
			return nil, identityDBError(err)
		}
		return json.Marshal(value)
	})
	if err != nil {
		if applyErr != nil && rejection(w, r, applyErr) {
			return
		}
		identityError(w, r, err)
		return
	}
	var revision struct {
		Revision int64 `json:"revision"`
	}
	_ = json.Unmarshal(result.JSON, &revision)
	if revision.Revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(revision.Revision, 10)))
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	JSON(w, status, result.JSON)
}

// audited records an audit event and outbox message with extra detail, so a
// configuration change shows what it set (not only that it happened).
func audited(ctx context.Context, tx pgx.Tx, p identity.Principal, id, action string, revision int64, detail any) error {
	extra, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,$3,$4) RETURNING id`, p.InstanceID, p.PrincipalID, action, id).Scan(&audit); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('resource_id',$3::text,'revision',$4::bigint)||$5::jsonb)`, audit, action, id, revision, extra)
	return err
}
