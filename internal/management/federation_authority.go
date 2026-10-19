package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Federation authority operations (P2-M6): epoch history, planned handover,
// forced recovery from the offline recovery key, the recovery-key drill, and
// the federated audit query. Mutations need instance admin, are guarded by an
// explicit confirm and are not replayable (a repeat answers 409 on state).

// authorityError maps the authority operation errors; everything else is
// federationError's.
func authorityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, federation.ErrHandoverPending):
		Error(w, r, 409, api.ErrorCodeConflict, "an authority handover is in progress; wait for it or abort it")
	case errors.Is(err, federation.ErrMemberNotCurrent):
		Error(w, r, 409, api.ErrorCodeStaleGeneration, "the target member has not applied the latest documents; retry shortly")
	case errors.Is(err, federation.ErrIsAuthority):
		Error(w, r, 409, api.ErrorCodeConflict, "this instance is already the authority")
	case errors.Is(err, federation.ErrNoHandover):
		Error(w, r, 404, api.ErrorCodeNotFound, "no authority handover is in progress")
	case errors.Is(err, federation.ErrRecoveryProof):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "the signature does not prove possession of the recovery key")
	case errors.Is(err, federation.ErrAuditForbidden):
		Error(w, r, 403, api.ErrorCodeForbidden, "audit read denied")
	case errors.Is(err, federation.ErrRevocationListFull):
		Error(w, r, 409, api.ErrorCodeConflict, "the revoked certificate list is full")
	default:
		federationError(w, r, err)
	}
}

func (h federationHandlers) authorityStatus(w http.ResponseWriter, r *http.Request) {
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
	out, err := svc.AuthorityStatus(ctx)
	if err != nil {
		authorityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

func (h federationHandlers) handover(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	if !noReplay(w, r) {
		return
	}
	var in struct {
		To      string `json:"to_member_id"`
		Confirm bool   `json:"confirm"`
	}
	if !sourceJSON(w, r, &in, "to_member_id", "confirm") {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "an authority handover requires confirm: true")
		return
	}
	to, valid := feedID(in.To)
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "to_member_id must be a member instance ID")
		return
	}
	ctx, cancel := h.ctx(r, 60*time.Second)
	defer cancel()
	out, err := svc.Handover(ctx, p.PrincipalID, to)
	if err != nil {
		authorityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

func (h federationHandlers) abortHandover(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	if !noReplay(w, r) {
		return
	}
	ctx, cancel := h.ctx(r, 15*time.Second)
	defer cancel()
	if err := svc.AbortHandover(ctx, p.PrincipalID); err != nil {
		authorityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recoveryEpochTemplate returns the input of the offline signer for a forced
// recovery on this member. It creates the pending authority key, so it is a POST.
func (h federationHandlers) recoveryEpochTemplate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	if !noReplay(w, r) {
		return
	}
	ctx, cancel := h.ctx(r, 15*time.Second)
	defer cancel()
	out, err := svc.RecoveryEpochTemplate(ctx, p.PrincipalID)
	if err != nil {
		authorityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

func (h federationHandlers) recoverAuthority(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	if !noReplay(w, r) {
		return
	}
	var in struct {
		federation.RecoverInput
		Confirm bool `json:"confirm"`
	}
	if !sourceJSON(w, r, &in, "authority_certificate", "remove_previous_authority", "confirm") {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "a forced authority recovery requires confirm: true")
		return
	}
	ctx, cancel := h.ctx(r, 60*time.Second)
	defer cancel()
	out, err := svc.RecoverAuthority(ctx, p.PrincipalID, in.RecoverInput)
	if err != nil {
		authorityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

func (h federationHandlers) recoveryChallenge(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	if !noReplay(w, r) {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	out, err := svc.NewRecoveryChallenge(ctx, p.PrincipalID)
	if err != nil {
		authorityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

func (h federationHandlers) recoveryVerify(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	if !noReplay(w, r) {
		return
	}
	var in struct {
		Challenge string `json:"challenge"`
		Signature string `json:"signature"`
	}
	if !sourceJSON(w, r, &in, "challenge", "signature") {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	out, err := svc.VerifyRecoveryChallenge(ctx, p.PrincipalID, in.Challenge, in.Signature)
	if err != nil {
		authorityError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

// --- federated audit ---

const federationAuditSQL = `SELECT id,created_at,principal_id::text,action,resource_id::text,result,details FROM audit_events
 WHERE instance_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3::bigint)) AND ($4='' OR action=$4) AND ($5='' OR left(action,length($5))=$5) AND ($6='' OR result=$6)
 AND ($7::timestamptz IS NULL OR created_at>=$7) AND ($8::timestamptz IS NULL OR created_at<$8) ORDER BY created_at DESC,id DESC LIMIT $9`

// queryFederatedAudit reads this member's audit events for a principal that
// holds instance administration here.
func queryFederatedAudit(ctx context.Context, db *pgxpool.Pool, p identity.Principal, policy identity.Policy, q federation.AuditQuery) ([]federation.AuditEntry, error) {
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		return nil, federation.ErrAuditForbidden
	}
	var at *time.Time
	var id int64
	if q.After != nil {
		at, id = &q.After.At, q.After.ID
	}
	rows, err := db.Query(ctx, federationAuditSQL, p.InstanceID, at, id, q.Action, q.Prefix, q.Result, q.From, q.To, q.Limit+1)
	if err != nil {
		return nil, err
	}
	return scanFederationAudit(rows)
}

func scanFederationAudit(rows pgx.Rows) ([]federation.AuditEntry, error) {
	defer rows.Close()
	out := []federation.AuditEntry{}
	for rows.Next() {
		var e federation.AuditEntry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.PrincipalID, &e.Action, &e.ResourceID, &e.Result, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (h federationSearchHandlers) audit(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "action", "prefix", "result", "from", "to", "members", "limit", "cursor") {
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	values := r.URL.Query()
	q := federation.AuditQuery{Action: values.Get("action"), Prefix: values.Get("prefix"), Result: values.Get("result"), Limit: 50}
	for name, dst := range map[string]**time.Time{"from": &q.From, "to": &q.To} {
		if v := values.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid federated audit query")
				return
			}
			*dst = &t
		}
	}
	if v := values.Get("limit"); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' || n > 1000 {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n < 1 || n > federation.MaxAuditLimit {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "limit must be between 1 and 200")
			return
		}
		q.Limit = n
	}
	var members []string
	if v := values.Get("members"); v != "" {
		members = strings.Split(v, ",")
		if len(members) > 64 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "too many members")
			return
		}
	}
	if err := q.Validate(); err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid federated audit query")
		return
	}
	var from, to string
	if q.From != nil {
		from = q.From.UTC().Format(time.RFC3339Nano)
	}
	if q.To != nil {
		to = q.To.UTC().Format(time.RFC3339Nano)
	}
	scope := scopeHash("federation-audit", p.InstanceID, p.PrincipalID, p.TokenID, q.Action, q.Prefix, q.Result, from, to, strings.Join(members, ","), q.Limit)
	var marks map[string]federation.AuditMark
	if c := values.Get("cursor"); c != "" {
		after, err := h.codec.Decode(scope, c)
		if err != nil || json.Unmarshal([]byte(after), &marks) != nil || len(marks) == 0 || len(marks) > 256 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := svc.FederatedAudit(ctx, p, q, members, marks, func(ctx context.Context, lq federation.AuditQuery) ([]federation.AuditEntry, error) {
		return queryFederatedAudit(ctx, h.db, p, policy, lq)
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			Error(w, r, 503, api.ErrorCodeUnavailable, "federated audit unavailable")
			return
		}
		authorityError(w, r, err)
		return
	}
	page := api.PageInfo{HasMore: res.More}
	if res.More {
		mark, _ := json.Marshal(res.Marks)
		token, err := h.codec.Encode(scope, string(mark))
		if err != nil {
			Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
			return
		}
		page.NextCursor = &token
	}
	JSON(w, 200, struct {
		Items   []federation.MergedAuditEntry  `json:"items"`
		Members []federation.MemberAuditStatus `json:"members"`
		Partial bool                           `json:"partial"`
		Page    api.PageInfo                   `json:"page"`
	}{res.Items, res.Members, res.Partial, page})
}
