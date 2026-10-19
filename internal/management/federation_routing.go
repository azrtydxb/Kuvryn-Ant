package management

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Placement-aware routing management (docs/planning/phase-2-federation.md
// §8, P2-M5). The routing of a feed key is part of the signed topology:
// PUT …/feeds/{fed}/{slug}/routing on the authority (instance admin,
// Idempotency-Key) publishes it. GET /api/v1/federation/routes/{fed}/{slug}
// explains, on any member, which members would answer a read that misses
// locally and in which order; GET /api/v1/feeds/{id}/resolve embeds the same
// plan for a bound feed.

// routeExplainers holds the federation service per serving instance, used by
// the feed resolve explanation.
var routeExplainers sync.Map // instance ID → *federation.Service

func (h federationHandlers) setFeedRouting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("fed") + "/" + r.PathValue("slug")
	var in federation.FeedRouting
	if !policyJSON(w, r, &in) {
		return
	}
	h.topologyMutation(w, r, "federation.feed.routing", key, in, 200, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, f, err := svc.SetFeedRoutingTx(ctx, tx, actor, key, in)
		return doc, f, err
	})
}

func (h federationHandlers) explainRoute(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	plan, err := svc.ExplainRoute(ctx, r.PathValue("fed")+"/"+r.PathValue("slug"))
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, plan)
}

// feedRoute returns the federated routing plan of a local feed for the
// resolve explanation, only for callers who can read the instance (member
// names and health are instance-level information).
func feedRoute(ctx context.Context, p identity.Principal, policy identity.Policy, feed string) *federation.RoutePlan {
	v, _ := routeExplainers.Load(p.InstanceID)
	svc, _ := v.(*federation.Service)
	if svc == nil ||
		!policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Read) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	plan, ok, err := svc.ExplainFeedRoute(ctx, feed)
	if err != nil || !ok {
		return nil
	}
	return &plan
}
