package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// SecurityResult is an external scanner's verdict on one asset.
type SecurityResult struct {
	ID        string          `json:"id"`
	AssetID   string          `json:"asset_id"`
	Scanner   string          `json:"scanner"`
	Status    string          `json:"status"`
	Summary   json.RawMessage `json:"summary"`
	CreatedAt time.Time       `json:"created_at"`
}

// ingestSecurityResult stores a scanner result and emits an
// asset.security_result event in the same transaction. It requires configure
// permission on the asset's feed (or instance admin); an asset the caller
// cannot read is reported as not found.
func (h opsHandlers) ingestSecurityResult(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid asset ID")
		return
	}
	var in struct {
		Scanner string          `json:"scanner"`
		Status  string          `json:"status"`
		Summary json.RawMessage `json:"summary"`
	}
	if !sourceJSON(w, r, &in, "scanner", "status", "summary") {
		return
	}
	summary := in.Summary
	if len(summary) == 0 {
		summary = json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if len(in.Scanner) < 1 || len(in.Scanner) > 128 || (in.Status != "passed" && in.Status != "failed" && in.Status != "warning") || len(summary) > 16384 || json.Unmarshal(summary, &obj) != nil || obj == nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid security result")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	var feed, source, namespace string
	err := h.db.QueryRow(ctx, `SELECT COALESCE(a.provenance->>'feed_id',''),c.source_id::text,COALESCE(c.namespace,'') FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE a.instance_id=$1 AND a.id=$2`, p.InstanceID, id).Scan(&feed, &source, &namespace)
	if errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, 404, api.ErrorCodeNotFound, "asset not found")
		return
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "security results unavailable")
		return
	}
	resource := identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: namespace, Kind: identity.ArtifactResource}
	if !policy.Authorize(p, resource, identity.Read) {
		Error(w, r, 404, api.ErrorCodeNotFound, "asset not found")
		return
	}
	if !policy.Authorize(p, resource, identity.Configure) {
		Error(w, r, 403, api.ErrorCodeForbidden, "security result ingest denied")
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "asset.security_result", id, map[string]any{"id": id, "scanner": in.Scanner, "status": in.Status, "summary": summary}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var res SecurityResult
		var body []byte
		err := tx.QueryRow(ctx, `INSERT INTO asset_security_results(instance_id,asset_id,scanner,status,summary,reported_by) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text,asset_id::text,scanner,status,summary,created_at`,
			p.InstanceID, id, in.Scanner, in.Status, []byte(summary), p.PrincipalID).Scan(&res.ID, &res.AssetID, &res.Scanner, &res.Status, &body, &res.CreatedAt)
		if err != nil {
			return nil, err
		}
		res.Summary = body
		if err = events.Emit(ctx, tx, p.InstanceID, p.PrincipalID, "asset.security_result", id, map[string]any{"asset_id": id, "result_id": res.ID, "scanner": res.Scanner, "status": res.Status, "feed_id": feed}); err != nil {
			return nil, err
		}
		return res, nil
	})
	if callErr != nil {
		if errors.Is(callErr, database.ErrNotFound) {
			Error(w, r, 404, api.ErrorCodeNotFound, "asset not found")
			return
		}
		Error(w, r, 503, api.ErrorCodeUnavailable, "security results unavailable")
		return
	}
	if storeErr != nil {
		storeError(w, r, storeErr)
		return
	}
	JSON(w, 201, out.JSON)
}
