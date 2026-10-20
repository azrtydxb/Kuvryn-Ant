package management

import (
	"context"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/maintenance"
	"github.com/jackc/pgx/v5"
)

// manualCleanupLimit bounds one manual cleanup per token table; the scheduled
// worker drains any remainder in batches, and the result reports has_more.
const manualCleanupLimit = 5000

// StartTokenCleanup is the request of POST /api/v1/tokens/cleanup.
type StartTokenCleanup struct {
	DryRun bool `json:"dry_run"`
}

// TokenCleanupResult reports what a cleanup removed (or would remove, for a dry
// run) under the effective token_cleanup_after_days.
type TokenCleanupResult struct {
	DryRun               bool      `json:"dry_run"`
	AfterDays            int       `json:"after_days"`
	Cutoff               time.Time `json:"cutoff"`
	ServiceTokens        int64     `json:"service_tokens"`
	PersonalAccessTokens int64     `json:"personal_access_tokens"`
	Total                int64     `json:"total"`
	HasMore              bool      `json:"has_more"`
}

// tokenCleanup removes service and personal access tokens that have been
// expired or revoked for longer than token_cleanup_after_days. Administrators
// only. A dry run counts without changing anything; a real run is idempotent,
// removes at most manualCleanupLimit per token kind and writes one summary
// audit event. Audit history is never touched.
func (h opsHandlers) tokenCleanup(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in StartTokenCleanup
	if !sourceJSON(w, r, &in, "dry_run") {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	days, _, err := maintenance.EffectiveTokenCleanupDays(ctx, h.db, p.InstanceID, h.d.TokenCleanupDays)
	if err != nil {
		identityError(w, r, err)
		return
	}
	if days <= 0 {
		Error(w, r, 409, api.ErrorCodeConflict, "token cleanup is disabled (token_cleanup_after_days is 0)")
		return
	}
	cutoff := maintenance.TokenCleanupCutoff(time.Now(), days)
	if in.DryRun {
		rep, err := maintenance.CountDeadTokens(ctx, h.db, p.InstanceID, cutoff)
		if err != nil {
			identityError(w, r, err)
			return
		}
		JSON(w, 200, cleanupResult(true, days, rep))
		return
	}
	h.mutate(w, r, p, "tokens.cleanup", "tokens", struct {
		In   StartTokenCleanup
		Days int
	}{in, days}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		rep, err := maintenance.DeleteDeadTokensTx(ctx, tx, p.InstanceID, "", cutoff, manualCleanupLimit)
		if err != nil {
			return nil, err
		}
		if rep.Total() > 0 {
			if err = maintenance.RecordTokenCleanupTx(ctx, tx, p.InstanceID, p.PrincipalID, "admin", days, rep); err != nil {
				return nil, err
			}
			maintenance.RecordTokenPurge("admin", rep)
		}
		out := cleanupResult(false, days, rep)
		out.HasMore = rep.ServiceTokens == manualCleanupLimit || rep.PersonalAccessTokens == manualCleanupLimit
		return out, nil
	})
}

func cleanupResult(dry bool, days int, rep maintenance.TokenPurgeReport) TokenCleanupResult {
	return TokenCleanupResult{DryRun: dry, AfterDays: days, Cutoff: rep.Cutoff, ServiceTokens: rep.ServiceTokens,
		PersonalAccessTokens: rep.PersonalAccessTokens, Total: rep.Total()}
}

// PersonalTokenCleanupResult is the response of the self-service cleanup.
type PersonalTokenCleanupResult struct {
	Deleted int64 `json:"deleted"`
}

// cleanupPATs deletes the signed-in user's own expired and revoked personal
// access tokens, whatever their age. Other users' tokens and active tokens are
// never touched; one summary audit event is recorded.
func (h authHandlers) cleanupPATs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.self(w, r)
	if !ok {
		return
	}
	out, ok := idempotentRun(w, r, h.db, p, "auth.pat.cleanup", "principal:"+p.PrincipalID, struct{}{}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		rep, err := maintenance.DeleteDeadTokensTx(ctx, tx, p.InstanceID, p.PrincipalID, time.Now(), manualCleanupLimit)
		if err != nil {
			return nil, patReqError(err)
		}
		if rep.Total() > 0 {
			if err = maintenance.RecordTokenCleanupTx(ctx, tx, p.InstanceID, p.PrincipalID, "self", 0, rep); err != nil {
				return nil, patReqError(err)
			}
			maintenance.RecordTokenPurge("self", rep)
		}
		return PersonalTokenCleanupResult{Deleted: rep.PersonalAccessTokens}, nil
	})
	if !ok {
		return
	}
	replayWrite(w, r, out, 200)
}
