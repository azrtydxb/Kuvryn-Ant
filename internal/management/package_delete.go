package management

// Policy-gated package deletion for the management API and console. All
// semantics (authorization, delete policy, audit, outbox, tombstones) live in
// artifact.MutateTx; blobs are left for garbage collection.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// PackageMutation is the result of a version or asset mutation.
type PackageMutation struct {
	Action           string `json:"action"`
	VersionRemoved   bool   `json:"version_removed"`
	ComponentRemoved bool   `json:"component_removed"`
	AssetsRemoved    int64  `json:"assets_removed"`
	TagsRemoved      int64  `json:"tags_removed"`
	Unchanged        bool   `json:"unchanged"`
}

// DeletePolicyBody is the feed-level delete policy document.
type DeletePolicyBody struct {
	Namespace         string `json:"namespace"`
	Mode              string `json:"mode"`
	ImmutableReleases bool   `json:"immutable_releases"`
	Revision          int64  `json:"revision"`
}

// RegisterPackageDeletes mounts the delete routes (called from RegisterPackages).
func (h packageHandlers) registerDeletes(mux *http.ServeMux, bearer *identity.Bearer) {
	mux.Handle("DELETE /api/v1/feeds/{id}/packages/{component}/versions/{version}", bearer.Wrap(http.HandlerFunc(h.deleteVersion)))
	mux.Handle("DELETE /api/v1/feeds/{id}/assets/{asset}", bearer.Wrap(http.HandlerFunc(h.deleteAsset)))
	mux.Handle("GET /api/v1/feeds/{id}/delete-policy", bearer.Wrap(http.HandlerFunc(h.getDeletePolicy)))
	mux.Handle("PUT /api/v1/feeds/{id}/delete-policy", bearer.Wrap(http.HandlerFunc(h.putDeletePolicy)))
}

func pkgMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, pgx.ErrNoRows), errors.Is(err, database.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "package resource not found")
	case errors.Is(err, artifact.ErrDenied):
		Error(w, r, 403, api.ErrorCodeForbidden, err.Error())
	case errors.Is(err, artifact.ErrConflict):
		Error(w, r, 409, api.ErrorCodeConflict, err.Error())
	case errors.Is(err, artifact.ErrInput):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid request")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "package mutation unavailable")
	}
}

// run executes one mutation in a transaction and renders the outcome.
func (h packageHandlers) run(w http.ResponseWriter, r *http.Request, p identity.Principal, policy identity.Policy, action artifact.Action, t artifact.Target) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	defer func() {
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = tx.Rollback(c)
	}()
	out, err := artifact.MutateTx(ctx, tx, p, policy, action, t)
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		pkgMutationError(w, r, err)
		return
	}
	JSON(w, 200, PackageMutation{Action: string(action), VersionRemoved: out.VersionRemoved, ComponentRemoved: out.ComponentRemoved, AssetsRemoved: out.AssetsRemoved, TagsRemoved: out.TagsRemoved, Unchanged: out.Unchanged})
}

func (h packageHandlers) deleteVersion(w http.ResponseWriter, r *http.Request) {
	feed, ok := packagePathID(w, r, "id")
	if !ok {
		return
	}
	component, ok := packagePathID(w, r, "component")
	if !ok {
		return
	}
	version := r.PathValue("version")
	action := artifact.ActionDelete
	if !singleParams(w, r, "action") {
		return
	}
	switch r.URL.Query().Get("action") {
	case "", "delete":
	case "unlist":
		action = artifact.ActionUnlist
	case "relist":
		action = artifact.ActionRelist
	default:
		packageInput(w, r)
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	var coordinate, namespace string
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := h.db.QueryRow(ctx, `SELECT c.coordinate,c.namespace FROM components c JOIN feed_sources fs ON fs.source_id=c.source_id AND fs.instance_id=c.instance_id WHERE c.instance_id=$1 AND fs.feed_id=$2 AND c.id=$3 LIMIT 1`, p.InstanceID, feed, component).Scan(&coordinate, &namespace)
	cancel()
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	h.run(w, r, p, policy, action, artifact.Target{FeedID: feed, Namespace: namespace, Coordinate: coordinate, Version: version})
}

func (h packageHandlers) deleteAsset(w http.ResponseWriter, r *http.Request) {
	feed, ok := packagePathID(w, r, "id")
	if !ok {
		return
	}
	asset, ok := packagePathID(w, r, "asset")
	if !ok {
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	var t artifact.Target
	t.FeedID = feed
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := h.db.QueryRow(ctx, `SELECT c.coordinate,c.namespace,v.version,a.asset_key FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id AND fs.instance_id=c.instance_id WHERE a.instance_id=$1 AND fs.feed_id=$2 AND a.id=$3 LIMIT 1`, p.InstanceID, feed, asset).Scan(&t.Coordinate, &t.Namespace, &t.Version, &t.AssetKey)
	cancel()
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	h.run(w, r, p, policy, artifact.ActionDelete, t)
}

func (h packageHandlers) getDeletePolicy(w http.ResponseWriter, r *http.Request) {
	feed, ok := packagePathID(w, r, "id")
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	d, err := artifact.EffectiveDeletePolicy(ctx, h.db, p.InstanceID, feed, "")
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	JSON(w, 200, DeletePolicyBody{Namespace: "", Mode: string(d.Mode), ImmutableReleases: d.ImmutableReleases, Revision: d.Revision})
}

func (h packageHandlers) putDeletePolicy(w http.ResponseWriter, r *http.Request) {
	feed, ok := packagePathID(w, r, "id")
	if !ok {
		return
	}
	var body DeletePolicyBody
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || !artifact.DeleteMode(body.Mode).Valid() || len(body.Namespace) > 256 {
		packageInput(w, r)
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
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	defer func() {
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = tx.Rollback(c)
	}()
	err = tx.QueryRow(ctx, `INSERT INTO delete_policies(instance_id,feed_id,namespace,mode,immutable_releases) SELECT $1,f.id,$3,$4,$5 FROM feeds f WHERE f.instance_id=$1 AND f.id=$2 ON CONFLICT(feed_id,namespace) DO UPDATE SET mode=excluded.mode,immutable_releases=excluded.immutable_releases,revision=delete_policies.revision+1,updated_at=now() RETURNING revision`, p.InstanceID, feed, body.Namespace, body.Mode, body.ImmutableReleases).Scan(&body.Revision)
	if err == nil {
		err = auditTx(ctx, tx, p, "feed.delete_policy.set", feed, body, map[string]any{"feed_id": feed, "namespace": body.Namespace, "mode": body.Mode, "immutable_releases": body.ImmutableReleases})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		pkgMutationError(w, r, err)
		return
	}
	JSON(w, 200, body)
}
