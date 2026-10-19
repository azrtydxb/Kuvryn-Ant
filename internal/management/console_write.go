package management

// Console mutation APIs: atomic read-order reorder, batch feed creation, feed
// deletion and per-source cache purge. Each is idempotent, audited and
// revision-checked in one transaction, like the existing management mutations.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

// consoleMutationError renders a captured typed problem, falling back to the
// standard sanitized mapping.
func consoleMutationError(w http.ResponseWriter, r *http.Request, err error, pr *problem) {
	if pr != nil && (errors.Is(err, database.ErrConflict) || errors.Is(err, database.ErrNotFound)) {
		writeProblem(w, r, pr)
		return
	}
	sourceMutationError(w, r, err)
}

func auditTx(ctx context.Context, tx pgx.Tx, p identity.Principal, action, resource string, details any, payload any) error {
	d, err := json.Marshal(details)
	if err != nil {
		return err
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,$3,$4,$5) RETURNING id`, p.InstanceID, p.PrincipalID, action, resource, d).Scan(&audit); err != nil {
		return err
	}
	pl, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,$3)`, audit, action, pl)
	return err
}

// ---- Atomic reorder ----

type SourceOrderItem struct {
	SourceID string `json:"source_id"`
	Priority int32  `json:"priority"`
}
type SourceOrder struct {
	FeedID       string            `json:"feed_id"`
	FeedRevision int64             `json:"feed_revision"`
	Items        []SourceOrderItem `json:"items"`
}

func uniqueUUIDs(raw []string, max int) ([]string, bool) {
	if len(raw) == 0 || len(raw) > max {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		id, ok := feedID(v)
		if !ok || seen[id] {
			return nil, false
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

func (h consoleHandlers) reorderSources(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match feed revision required")
		return
	}
	var body struct {
		SourceIDs []string `json:"source_ids"`
	}
	if !sourceJSON(w, r, &body, "source_ids") {
		return
	}
	ids, ok := uniqueUUIDs(body.SourceIDs, routing.MaxSources)
	if !ok {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "source_ids must be a non-empty list of unique source IDs")
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
	// Reordering changes feed configuration only; it needs configure on the feed
	// and visibility (read) of every source whose position it states.
	if !feedAllowed(policy, p, feed, identity.Configure) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	for _, id := range ids {
		if !sourceAllowed(policy, p, id, identity.Read) {
			sourceMutationError(w, r, database.ErrNotFound)
			return
		}
	}
	canonical, _ := json.Marshal(struct {
		IDs      []string
		Revision int64
	}{ids, revision})
	var pr *problem
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "feed.source.order", Resource: feed, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var current int64
		err := tx.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, feed).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		rows, err := tx.Query(ctx, `SELECT source_id::text FROM feed_sources WHERE instance_id=$1 AND feed_id=$2`, p.InstanceID, feed)
		if err != nil {
			return nil, err
		}
		attached := map[string]bool{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			attached[id] = true
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		match := len(attached) == len(ids)
		for _, id := range ids {
			match = match && attached[id]
		}
		if !match {
			pr = &problem{409, api.ErrorCodeConflict, "source_ids must list exactly the sources attached to the feed"}
			return nil, database.ErrConflict
		}
		// priority is unique per feed and checked per row: park every row on a
		// negative value, then assign the final positions.
		if _, err = tx.Exec(ctx, `UPDATE feed_sources SET priority=-priority-1 WHERE instance_id=$1 AND feed_id=$2`, p.InstanceID, feed); err != nil {
			return nil, err
		}
		out := SourceOrder{FeedID: feed, Items: make([]SourceOrderItem, 0, len(ids))}
		for i, id := range ids {
			if _, err = tx.Exec(ctx, `UPDATE feed_sources SET priority=$4 WHERE instance_id=$1 AND feed_id=$2 AND source_id=$3`, p.InstanceID, feed, id, i); err != nil {
				return nil, err
			}
			out.Items = append(out.Items, SourceOrderItem{SourceID: id, Priority: int32(i)})
		}
		if err = tx.QueryRow(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING revision`, p.InstanceID, feed).Scan(&out.FeedRevision); err != nil {
			return nil, err
		}
		if err = auditTx(ctx, tx, p, "feed.source.order", feed, map[string]any{"source_ids": ids}, map[string]any{"feed_id": feed, "feed_revision": out.FeedRevision}); err != nil {
			return nil, err
		}
		return json.Marshal(out)
	})
	if err != nil {
		consoleMutationError(w, r, err, pr)
		return
	}
	var out SourceOrder
	if json.Unmarshal(result.JSON, &out) != nil {
		sourceMutationError(w, r, errors.New("invalid result"))
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(out.FeedRevision, 10)))
	JSON(w, 200, out)
}

// ---- Batch feed creation ----

type BatchFeedResult struct {
	Feed          Feed              `json:"feed"`
	Sources       []SourceOrderItem `json:"sources"`
	PublishPolicy *PublishPolicy    `json:"publish_policy,omitempty"`
}

func (h consoleHandlers) batchCreateFeed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug          string            `json:"slug"`
		Format        string            `json:"format"`
		AnonymousRead bool              `json:"anonymous_read"`
		SourceIDs     []string          `json:"source_ids"`
		PublishPolicy json.RawMessage   `json:"publish_policy"`
		Purpose       string            `json:"purpose"`
		CachePolicy   *CachePolicyInput `json:"cache_policy"`
	}
	if !sourceJSON(w, r, &body, "slug", "format", "anonymous_read", "source_ids", "publish_policy", "purpose", "cache_policy") {
		return
	}
	if !slugPattern.MatchString(body.Slug) || !formats[body.Format] {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed configuration")
		return
	}
	if body.Purpose == "" {
		body.Purpose = PurposeArtifacts
	}
	if rejectCacheInput(w, r, validateCacheInput(body.Purpose, body.Format, body.CachePolicy)) {
		return
	}
	var ids []string
	if len(body.SourceIDs) > 0 {
		var ok bool
		if ids, ok = uniqueUUIDs(body.SourceIDs, 64); !ok {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "source_ids must be unique source IDs (at most 64)")
			return
		}
	}
	var input PublishPolicyInput
	var rules []routing.Rule
	hasPolicy := len(body.PublishPolicy) > 0
	if hasPolicy {
		fields, err := exactObject(body.PublishPolicy, []string{"default_source_id", "rules"}, []string{"rules"})
		if err == nil {
			input, rules, err = parsePublishPolicy(fields)
		}
		if err != nil {
			policyError(w, r, artifact.ErrInput)
			return
		}
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, "", identity.Configure) {
		Error(w, r, 403, api.ErrorCodeForbidden, "feed configuration denied")
		return
	}
	for _, id := range ids {
		if !sourceAllowed(policy, p, id, identity.Configure) {
			sourceMutationError(w, r, database.ErrNotFound)
			return
		}
	}
	if hasPolicy && !policyAllowed(policy, p, "", input.DefaultSourceID, rules) {
		policyError(w, r, artifact.ErrDenied)
		return
	}
	canonical, _ := json.Marshal(struct {
		Slug, Format string
		Anonymous    bool
		IDs          []string
		Policy       *PublishPolicyInput
		Purpose      string
		Cache        *CachePolicyInput
	}{body.Slug, body.Format, body.AnonymousRead, ids, func() *PublishPolicyInput {
		if hasPolicy {
			return &input
		}
		return nil
	}(), body.Purpose, body.CachePolicy})
	var pr *problem
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "feed.create.batch", Resource: "feeds", Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		out := BatchFeedResult{Sources: []SourceOrderItem{}}
		idle, maxBytes := cachePolicyValues(body.CachePolicy, DefaultCacheMaxIdleSeconds, nil)
		err := scanFeed(tx.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format,anonymous_read,purpose,cache_max_idle_seconds,cache_max_bytes) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+feedColumns, p.InstanceID, body.Slug, body.Format, body.AnonymousRead, body.Purpose, idle, maxBytes), &out.Feed)
		if err != nil {
			pr = &problem{409, api.ErrorCodeConflict, "a feed with this slug already exists"}
			if feedConflict(err) == database.ErrConflict {
				return nil, database.ErrConflict
			}
			return nil, err
		}
		if !feedAllowed(policy, p, out.Feed.ID, identity.Configure) {
			return nil, database.ErrNotFound
		}
		if err = feedEvent(ctx, tx, p, out.Feed, "feed.create"); err != nil {
			return nil, err
		}
		if out.Feed.Purpose == PurposeCache {
			if err = feedPurposeAudit(ctx, tx, p, out.Feed, ""); err != nil {
				return nil, err
			}
		}
		for i, id := range ids {
			var format string
			err = tx.QueryRow(ctx, `SELECT format FROM sources WHERE instance_id=$1 AND id=$2 FOR SHARE`, p.InstanceID, id).Scan(&format)
			if errors.Is(err, pgx.ErrNoRows) {
				pr = &problem{404, api.ErrorCodeNotFound, "source not found"}
				return nil, database.ErrNotFound
			}
			if err != nil {
				return nil, err
			}
			if format != body.Format {
				pr = &problem{409, api.ErrorCodeConflict, "source format does not match the feed format"}
				return nil, database.ErrConflict
			}
			if reason, e := checkCacheSourcePlacement(ctx, tx, p.InstanceID, out.Feed.ID, out.Feed.Purpose, id); e != nil {
				return nil, e
			} else if reason != "" {
				pr = &problem{409, api.ErrorCodeConflict, reason}
				return nil, database.ErrConflict
			}
			if _, err = tx.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,$4)`, p.InstanceID, out.Feed.ID, id, i); err != nil {
				return nil, err
			}
			if err = auditTx(ctx, tx, p, "feed.source.attach", out.Feed.ID, map[string]any{"source_id": id, "priority": i}, map[string]any{"feed_id": out.Feed.ID, "source_id": id, "feed_revision": out.Feed.Revision, "priority": i}); err != nil {
				return nil, err
			}
			out.Sources = append(out.Sources, SourceOrderItem{SourceID: id, Priority: int32(i)})
		}
		if hasPolicy {
			next, e := (artifact.Policies{DB: h.db, InstanceID: p.InstanceID}).SetTx(ctx, tx, p, policy, out.Feed.ID, out.Feed.Revision, input.DefaultSourceID, rules)
			if e != nil {
				applyErr = e
				return nil, e
			}
			out.Feed.Revision = next
			out.PublishPolicy = &PublishPolicy{FeedID: out.Feed.ID, Revision: next, DefaultSourceID: input.DefaultSourceID, Rules: input.Rules}
		}
		return json.Marshal(out)
	})
	if err != nil {
		if applyErr != nil {
			policyError(w, r, applyErr)
			return
		}
		consoleMutationError(w, r, err, pr)
		return
	}
	var out BatchFeedResult
	if json.Unmarshal(result.JSON, &out) != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "invalid mutation result")
		return
	}
	// Replays are re-authorized against the concrete feed they refer to.
	if !feedAllowed(policy, p, out.Feed.ID, identity.Configure) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	w.Header().Set("Location", "/api/v1/feeds/"+out.Feed.ID)
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(out.Feed.Revision, 10)))
	JSON(w, 201, out)
}

// ---- Feed delete ----

type FeedDeleted struct {
	ID                string `json:"id"`
	Deleted           bool   `json:"deleted"`
	SourcesDetached   int64  `json:"sources_detached"`
	ReplicationJobs   int64  `json:"replication_jobs_removed"`
	RoleGrantsRemoved int64  `json:"role_grants_removed"`
	RetentionPolicies int64  `json:"retention_policies_removed"`
	UploadSessions    int64  `json:"upload_sessions_removed"`
}

func (h consoleHandlers) deleteFeed(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	if !valid || !singleParams(w, r, "force") {
		if !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
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
	if !feedAllowed(policy, p, feed, identity.Configure) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	if force && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		Error(w, r, 403, api.ErrorCodeForbidden, "forced feed deletion requires instance administration")
		return
	}
	canonical, _ := json.Marshal(struct {
		Revision int64
		Force    bool
	}{revision, force})
	var pr *problem
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "feed.delete", Resource: feed, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		item := Feed{}
		err := tx.QueryRow(ctx, `SELECT id,slug,format,revision,anonymous_read FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, feed).Scan(&item.ID, &item.Slug, &item.Format, &item.Revision, &item.AnonymousRead)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if item.Revision != revision {
			return nil, database.ErrRevisionConflict
		}
		out := FeedDeleted{ID: feed, Deleted: true}
		var jobs int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM replication_jobs WHERE instance_id=$1 AND dest_feed_id=$2`, p.InstanceID, feed).Scan(&jobs); err != nil {
			return nil, err
		}
		if jobs > 0 && !force {
			pr = &problem{409, api.ErrorCodeConflict, "feed is the destination of replication jobs; remove them or retry with force=true"}
			return nil, database.ErrConflict
		}
		exec := func(dst *int64, sql string) error {
			tag, e := tx.Exec(ctx, sql, p.InstanceID, feed)
			if e == nil {
				*dst = tag.RowsAffected()
			}
			return e
		}
		var ignored int64
		for _, step := range []struct {
			dst *int64
			sql string
		}{
			{&out.ReplicationJobs, `DELETE FROM replication_jobs WHERE instance_id=$1 AND dest_feed_id=$2`},
			{&out.UploadSessions, `DELETE FROM oci_uploads WHERE instance_id=$1 AND feed_id=$2`},
			{&ignored, `DELETE FROM publish_policies WHERE instance_id=$1 AND feed_id=$2`},
			{&out.RetentionPolicies, `DELETE FROM retention_policies WHERE instance_id=$1 AND feed_id=$2`},
		} {
			if err = exec(step.dst, step.sql); err != nil {
				return nil, err
			}
		}
		// Federated grants on this feed become inert again (their feed key stays
		// unbound until another feed is bound to it); the feed-key binding goes
		// with the feed.
		if _, err = tx.Exec(ctx, `SELECT set_config('ant.federation_apply','on',true)`); err != nil {
			return nil, err
		}
		for _, q := range []string{
			`UPDATE role_grants SET feed_id=NULL,inert='unresolved_feed' WHERE instance_id=$1 AND feed_id=$2 AND origin='federation' AND feed_key IS NOT NULL`,
			`DELETE FROM role_grants WHERE instance_id=$1 AND feed_id=$2 AND origin='federation'`,
			`DELETE FROM federated_feed_bindings WHERE instance_id=$1 AND feed_id=$2`,
		} {
			if _, err = tx.Exec(ctx, q, p.InstanceID, feed); err != nil {
				return nil, err
			}
		}
		if _, err = tx.Exec(ctx, `SELECT set_config('ant.federation_apply','off',true)`); err != nil {
			return nil, err
		}
		// Feed-scoped grants cannot outlive their feed; bump the affected roles so
		// concurrent role edits are detected.
		rows, err := tx.Query(ctx, `DELETE FROM role_grants WHERE instance_id=$1 AND feed_id=$2 AND origin='local' RETURNING role_id::text`, p.InstanceID, feed)
		if err != nil {
			return nil, err
		}
		roles := map[string]bool{}
		for rows.Next() {
			var role string
			if err = rows.Scan(&role); err != nil {
				break
			}
			roles[role] = true
			out.RoleGrantsRemoved++
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		for role := range roles {
			if _, err = tx.Exec(ctx, `UPDATE roles SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, p.InstanceID, role); err != nil {
				return nil, err
			}
		}
		// Sources and their content are kept; only the attachments go.
		if err = exec(&out.SourcesDetached, `DELETE FROM feed_sources WHERE instance_id=$1 AND feed_id=$2`); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, feed); err != nil {
			return nil, err
		}
		if err = auditTx(ctx, tx, p, "feed.delete", feed, map[string]any{"slug": item.Slug, "force": force, "sources_detached": out.SourcesDetached, "replication_jobs_removed": out.ReplicationJobs}, map[string]any{"feed_id": feed, "revision": item.Revision}); err != nil {
			return nil, err
		}
		return json.Marshal(out)
	})
	if err != nil {
		consoleMutationError(w, r, err, pr)
		return
	}
	var out FeedDeleted
	if json.Unmarshal(result.JSON, &out) != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "invalid mutation result")
		return
	}
	JSON(w, 200, out)
}

// ---- Remote source cache purge ----

type CachePurge struct {
	SourceID          string `json:"source_id"`
	ComponentsRemoved int64  `json:"components_removed"`
	VersionsRemoved   int64  `json:"versions_removed"`
	AssetsRemoved     int64  `json:"assets_removed"`
	BytesReleased     int64  `json:"bytes_released"`
}

// purgeSourceCache removes cached components of a remote source. Blob bytes that
// become unreferenced are reclaimed by the two-phase garbage collector, never
// inline, so a purge cannot race a concurrent reader of shared content.
func (h consoleHandlers) purgeSourceCache(w http.ResponseWriter, r *http.Request) {
	source, valid := feedID(r.PathValue("id"))
	if !valid || !singleParams(w, r, "coordinate") {
		if !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source ID")
		}
		return
	}
	coordinate := r.URL.Query().Get("coordinate")
	if r.URL.Query().Has("coordinate") && !validCoordinate(coordinate) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid coordinate")
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
	canonical, _ := json.Marshal(coordinate)
	var pr *problem
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "source.cache.purge", Resource: source, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM sources WHERE instance_id=$1 AND id=$2 FOR SHARE`, p.InstanceID, source).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if kind != "remote" {
			pr = &problem{409, api.ErrorCodeConflict, "only remote sources have a cache"}
			return nil, database.ErrConflict
		}
		out := CachePurge{SourceID: source}
		comps := `SELECT id FROM components WHERE instance_id=$1 AND source_id=$2 AND ($3='' OR coordinate=$3)`
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint FROM assets a JOIN blobs b ON b.id=a.blob_id JOIN versions v ON v.id=a.version_id WHERE v.component_id IN (`+comps+`)`, p.InstanceID, source, coordinate).Scan(&out.BytesReleased); err != nil {
			return nil, err
		}
		for _, step := range []struct {
			dst *int64
			sql string
		}{
			{new(int64), `DELETE FROM tags WHERE component_id IN (` + comps + `)`},
			{&out.AssetsRemoved, `DELETE FROM assets WHERE version_id IN (SELECT id FROM versions WHERE component_id IN (` + comps + `))`},
			{&out.VersionsRemoved, `DELETE FROM versions WHERE component_id IN (` + comps + `)`},
			{&out.ComponentsRemoved, `DELETE FROM components WHERE id IN (` + comps + `)`},
		} {
			tag, e := tx.Exec(ctx, step.sql, p.InstanceID, source, coordinate)
			if e != nil {
				return nil, e
			}
			*step.dst = tag.RowsAffected()
		}
		if err = auditTx(ctx, tx, p, "source.cache.purge", source, map[string]any{"coordinate": coordinate, "components": out.ComponentsRemoved, "versions": out.VersionsRemoved, "assets": out.AssetsRemoved, "bytes": out.BytesReleased}, map[string]any{"source_id": source}); err != nil {
			return nil, err
		}
		return json.Marshal(out)
	})
	if err != nil {
		consoleMutationError(w, r, err, pr)
		return
	}
	var out CachePurge
	if json.Unmarshal(result.JSON, &out) != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "invalid mutation result")
		return
	}
	JSON(w, 200, out)
}
