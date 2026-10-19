package management

import (
	"context"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/jackc/pgx/v5"
)

// Coordinated replication management (docs/planning/phase-2-federation.md
// §9, §12.1, P2-M3). Reads need read on the instance. Feed keys, rules and
// home transfers need instance admin on the authority (409 not_authority
// elsewhere); adoption and quarantine review need instance admin on the
// member itself. Every mutation requires Idempotency-Key and replays.

func (h federationHandlers) topology(w http.ResponseWriter, r *http.Request) {
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
	v, err := svc.TopologyView(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, v)
}

// topologyMutation runs an authority topology change under Idempotency-Key
// and nudges members after a fresh (not replayed) publish.
func (h federationHandlers) topologyMutation(w http.ResponseWriter, r *http.Request, operation, resource string, input any, status int,
	apply func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error)) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var doc *federation.TopologyDocument
	out, callErr, storeErr := idempotent(r, h.db, p, key, operation, resource, input, func(ctx context.Context, tx pgx.Tx) (any, error) {
		d, v, err := apply(ctx, tx, svc, p.PrincipalID)
		doc = d
		return v, err
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
		return
	case storeErr != nil:
		storeError(w, r, storeErr)
		return
	}
	if !out.Replayed {
		svc.AfterTopologyPublish(r.Context(), doc)
	}
	if status == 204 {
		w.WriteHeader(204)
		return
	}
	JSON(w, status, out.JSON)
}

func (h federationHandlers) createFeed(w http.ResponseWriter, r *http.Request) {
	var in federation.FeedInput
	if !policyJSON(w, r, &in) {
		return
	}
	h.topologyMutation(w, r, "federation.feed.create", in.FeedKey, in, 201, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, f, err := svc.CreateFeedTx(ctx, tx, actor, in)
		return doc, f, err
	})
}

func (h federationHandlers) deleteFeed(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("fed") + "/" + r.PathValue("slug")
	h.topologyMutation(w, r, "federation.feed.delete", key, key, 204, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, err := svc.DeleteFeedTx(ctx, tx, actor, key)
		return doc, struct{}{}, err
	})
}

func (h federationHandlers) startTransfer(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("fed") + "/" + r.PathValue("slug")
	var in federation.TransferInput
	if !policyJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "a home transfer requires confirm: true")
		return
	}
	h.topologyMutation(w, r, "federation.feed.transfer", key, in, 202, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, t, err := svc.StartTransferTx(ctx, tx, actor, key, in)
		return doc, t, err
	})
}

func (h federationHandlers) abortTransfer(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("fed") + "/" + r.PathValue("slug")
	var in struct {
		Confirm bool   `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if !policyJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "aborting a home transfer requires confirm: true")
		return
	}
	if in.Reason == "" {
		in.Reason = "aborted by an administrator"
	}
	h.topologyMutation(w, r, "federation.feed.transfer_abort", key, in, 200, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, t, err := svc.AbortTransferTx(ctx, tx, actor, key, in.Reason)
		return doc, t, err
	})
}

func (h federationHandlers) listRules(w http.ResponseWriter, r *http.Request) {
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
	rules, err := svc.Rules(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": rules})
}

func (h federationHandlers) getRule(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 404, api.ErrorCodeNotFound, "rule not found")
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	rules, err := svc.Rules(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	for _, rule := range rules {
		if rule.ID == id {
			JSON(w, 200, rule)
			return
		}
	}
	Error(w, r, 404, api.ErrorCodeNotFound, "rule not found")
}

func (h federationHandlers) createRule(w http.ResponseWriter, r *http.Request) {
	var in federation.RuleInput
	if !policyJSON(w, r, &in) {
		return
	}
	h.topologyMutation(w, r, "federation.replication.rule_create", in.FeedKey, in, 201, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, rule, err := svc.CreateRuleTx(ctx, tx, actor, in)
		return doc, rule, err
	})
}

func (h federationHandlers) patchRule(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 404, api.ErrorCodeNotFound, "rule not found")
		return
	}
	var in federation.RulePatch
	if !policyJSON(w, r, &in) {
		return
	}
	if len(r.Header.Values("If-Match")) > 0 {
		rev, ok := revisionHeader(r)
		if !ok || (in.Revision != 0 && in.Revision != rev) {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid If-Match revision")
			return
		}
		in.Revision = rev
	}
	if in.Revision < 1 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "the rule revision (If-Match or revision) is required")
		return
	}
	h.topologyMutation(w, r, "federation.replication.rule_update", id, in, 200, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, rule, err := svc.UpdateRuleTx(ctx, tx, actor, id, in)
		return doc, rule, err
	})
}

func (h federationHandlers) deleteRule(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 404, api.ErrorCodeNotFound, "rule not found")
		return
	}
	h.topologyMutation(w, r, "federation.replication.rule_delete", id, id, 204, func(ctx context.Context, tx pgx.Tx, svc *federation.Service, actor string) (*federation.TopologyDocument, any, error) {
		doc, err := svc.DeleteRuleTx(ctx, tx, actor, id)
		return doc, struct{}{}, err
	})
}

func (h federationHandlers) ruleCompliance(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 404, api.ErrorCodeNotFound, "rule not found")
		return
	}
	ctx, cancel := h.ctx(r, 30*time.Second)
	defer cancel()
	out, err := svc.Compliance(ctx, id)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

func (h federationHandlers) replicationStatus(w http.ResponseWriter, r *http.Request) {
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
	items, err := svc.LocalRuleStatuses(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	quarantines, err := svc.Quarantines(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": items, "quarantines": quarantines})
}

func (h federationHandlers) adoptJob(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in struct {
		JobID  string `json:"job_id"`
		RuleID string `json:"rule_id"`
	}
	if !policyJSON(w, r, &in) {
		return
	}
	job, ok1 := feedID(in.JobID)
	rule, ok2 := feedID(in.RuleID)
	if !ok1 || !ok2 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "job_id and rule_id (uuid) are required")
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.replication.adopt", job, in, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return svc.AdoptReplicationJobTx(ctx, tx, p.PrincipalID, job, rule)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		JSON(w, 200, out.JSON)
	}
}

func (h federationHandlers) quarantines(w http.ResponseWriter, r *http.Request) {
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
	items, err := svc.Quarantines(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": items})
}

func (h federationHandlers) resolveQuarantine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 404, api.ErrorCodeNotFound, "quarantine not found")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !policyJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "resolving a quarantine requires confirm: true")
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.quarantine.resolve", id, in, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return svc.ResolveQuarantineTx(ctx, tx, p.PrincipalID, id)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		JSON(w, 200, out.JSON)
	}
}
