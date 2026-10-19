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
)

// SourceDeleted reports what DELETE /api/v1/sources/{id} removed.
type SourceDeleted struct {
	ID                string `json:"id"`
	Deleted           bool   `json:"deleted"`
	FeedsDetached     int64  `json:"feeds_detached"`
	ComponentsRemoved int64  `json:"components_removed"`
	VersionsRemoved   int64  `json:"versions_removed"`
	AssetsRemoved     int64  `json:"assets_removed"`
	BytesReleased     int64  `json:"bytes_released"`
}

// deleteSource removes a source. Without force it is refused while the source
// is attached to a feed or holds content. With force (instance administration)
// it detaches the source from its feeds and removes its content; blob bytes
// that become unreferenced are reclaimed later by garbage collection. A source
// owned by a replication job is never removed here: delete the job first.
func (h consoleHandlers) deleteSource(w http.ResponseWriter, r *http.Request) {
	source, valid := feedID(r.PathValue("id"))
	if !valid || !singleParams(w, r, "force") {
		if !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source ID")
		}
		return
	}
	force := false
	if v := r.URL.Query().Get("force"); v != "" {
		var err error
		if force, err = strconv.ParseBool(v); err != nil || (v != "true" && v != "false") {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "force must be true or false")
			return
		}
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
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
	if !sourceAllowed(policy, p, source, identity.Configure) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	if force && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		Error(w, r, 403, api.ErrorCodeForbidden, "forced source deletion requires instance administration")
		return
	}
	canonical, _ := json.Marshal(struct {
		Revision int64
		Force    bool
	}{revision, force})
	var pr *problem
	conflict := func(msg string) (json.RawMessage, error) {
		pr = &problem{409, api.ErrorCodeConflict, msg}
		return nil, database.ErrConflict
	}
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "source.delete", Resource: source, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var name, kind string
		var current int64
		err := tx.QueryRow(ctx, `SELECT name,kind,revision FROM sources WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, source).Scan(&name, &kind, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		var jobs, attached, components, publishRefs int64
		if err = tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM replication_jobs WHERE instance_id=$1 AND replica_source_id=$2),
 (SELECT count(*) FROM feed_sources WHERE instance_id=$1 AND source_id=$2),
 (SELECT count(*) FROM components WHERE instance_id=$1 AND source_id=$2),
 (SELECT count(*) FROM publish_policies WHERE instance_id=$1 AND rules::text LIKE '%'||$2::text||'%')`, p.InstanceID, source).Scan(&jobs, &attached, &components, &publishRefs); err != nil {
			return nil, err
		}
		if publishRefs > 0 {
			return conflict("source is a target of publish rules; update the publish policy first")
		}
		if jobs > 0 {
			return conflict("source belongs to a replication job; delete the job first")
		}
		if attached > 0 && !force {
			return conflict("source is attached to feeds; detach it or retry with force=true")
		}
		if components > 0 && !force {
			return conflict("source holds content; retry with force=true to remove it")
		}
		out := SourceDeleted{ID: source, Deleted: true}
		// Feed revisions move so concurrent edits of the feed are detected.
		feeds, err := tx.Exec(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id IN (SELECT feed_id FROM feed_sources WHERE instance_id=$1 AND source_id=$2)`, p.InstanceID, source)
		if err != nil {
			return nil, err
		}
		out.FeedsDetached = feeds.RowsAffected()
		comps := `SELECT id FROM components WHERE instance_id=$1 AND source_id=$2`
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint FROM assets a JOIN blobs b ON b.id=a.blob_id JOIN versions v ON v.id=a.version_id WHERE v.component_id IN (`+comps+`)`, p.InstanceID, source).Scan(&out.BytesReleased); err != nil {
			return nil, err
		}
		var ignored int64
		for _, step := range []struct {
			dst *int64
			sql string
		}{
			{&ignored, `DELETE FROM tags WHERE component_id IN (` + comps + `)`},
			{&out.AssetsRemoved, `DELETE FROM assets WHERE version_id IN (SELECT id FROM versions WHERE component_id IN (` + comps + `))`},
			{&out.VersionsRemoved, `DELETE FROM versions WHERE component_id IN (` + comps + `)`},
			{&out.ComponentsRemoved, `DELETE FROM components WHERE instance_id=$1 AND source_id=$2`},
			{&ignored, `DELETE FROM feed_sources WHERE instance_id=$1 AND source_id=$2`},
			{&ignored, `DELETE FROM role_grants WHERE instance_id=$1 AND source_id=$2`},
			{&ignored, `DELETE FROM oci_uploads WHERE instance_id=$1 AND source_id=$2`},
			{&ignored, `DELETE FROM retention_policies WHERE instance_id=$1 AND source_id=$2`},
			{&ignored, `UPDATE publish_policies SET default_source_id=NULL WHERE instance_id=$1 AND default_source_id=$2`},
			{&ignored, `DELETE FROM remote_source_health WHERE instance_id=$1 AND source_id=$2`},
		} {
			tag, e := tx.Exec(ctx, step.sql, p.InstanceID, source)
			if e != nil {
				return nil, e
			}
			*step.dst = tag.RowsAffected()
		}
		// The remote configuration row references the credential, so it goes first.
		var credential *string
		if e := tx.QueryRow(ctx, `DELETE FROM remote_source_configs WHERE instance_id=$1 AND source_id=$2 RETURNING credential_id::text`, p.InstanceID, source).Scan(&credential); e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return nil, e
		}
		if credential != nil {
			if _, e := tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2`, p.InstanceID, *credential); e != nil {
				return nil, e
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM sources WHERE instance_id=$1 AND id=$2`, p.InstanceID, source); err != nil {
			return nil, err
		}
		if err = auditTx(ctx, tx, p, "source.delete", source, map[string]any{"name": name, "kind": kind, "force": force, "feeds_detached": out.FeedsDetached, "components": out.ComponentsRemoved, "versions": out.VersionsRemoved, "assets": out.AssetsRemoved}, map[string]any{"source_id": source}); err != nil {
			return nil, err
		}
		return json.Marshal(out)
	})
	if err != nil {
		consoleMutationError(w, r, err, pr)
		return
	}
	var out SourceDeleted
	if json.Unmarshal(result.JSON, &out) != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "invalid mutation result")
		return
	}
	JSON(w, 200, out)
}
