package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Federation management API (docs/planning/phase-2-federation.md §12.1, P2-M1).
// Reads need read on the instance; every mutation needs instance admin, and
// authority-only operations answer 409 not_authority on other members.
// Founding and invitations return a secret exactly once and reject
// Idempotency-Key (like token issuance); recovery-key rotation accepts only
// statements signed offline, bound to one membership generation, and rejects
// it too (a repeat answers 409 stale_generation). Suspend, resume,
// remove and invitation revocation require Idempotency-Key and replay from
// the shared store. Join and leave call another member; they are guarded by
// the federation state instead (a repeat answers 409).

type federationHandlers struct {
	identityHandlers
	svc *federation.Service
}

// RegisterFederation mounts /api/v1/federation. svc is nil when the instance
// is not serving or has no secret keyring; reads then report federation as
// disabled and mutations answer 503.
func RegisterFederation(mux *http.ServeMux, pool *pgxpool.Pool, svc *federation.Service) {
	if svc != nil {
		routeExplainers.Store(svc.InstanceID(), svc)
	}
	h := federationHandlers{identityHandlers{feedHandlers{db: pool}}, svc}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/federation/status":                             h.status,
		"POST /api/v1/federation":                                   h.found,
		"POST /api/v1/federation/join":                              h.join,
		"POST /api/v1/federation/leave":                             h.leave,
		"GET /api/v1/federation/members":                            h.members,
		"GET /api/v1/federation/members/{id}":                       h.member,
		"POST /api/v1/federation/members/{id}/{action}":             h.memberAction,
		"DELETE /api/v1/federation/members/{id}":                    h.removeMember,
		"GET /api/v1/federation/invitations":                        h.invitations,
		"POST /api/v1/federation/invitations":                       h.createInvitation,
		"DELETE /api/v1/federation/invitations/{id}":                h.revokeInvitation,
		"GET /api/v1/federation/documents/{kind}":                   h.document,
		"GET /api/v1/federation/documents/{kind}/{generation}":      h.document,
		"GET /api/v1/federation/recovery-key/rotation":              h.recoveryTemplate,
		"POST /api/v1/federation/recovery-key/rotate":               h.rotateRecovery,
		"GET /api/v1/federation/policy":                             h.getPolicy,
		"GET /api/v1/federation/policy/draft":                       h.getDraft,
		"PUT /api/v1/federation/policy/draft":                       h.putDraft,
		"POST /api/v1/federation/policy/publish":                    h.publishPolicy,
		"GET /api/v1/federation/policy/diff":                        h.policyDiff,
		"GET /api/v1/federation/feed-bindings":                      h.feedBindings,
		"POST /api/v1/federation/feed-bindings":                     h.bindFeed,
		"DELETE /api/v1/federation/feed-bindings/{fed}/{slug}":      h.unbindFeed,
		"GET /api/v1/federation/principals":                         h.principals,
		"POST /api/v1/federation/principals/{id}/adopt":             h.adoptPrincipal,
		"GET /api/v1/federation/settings":                           h.getSettings,
		"PUT /api/v1/federation/settings":                           h.putSettings,
		"POST /api/v1/federation/access-check":                      h.accessCheck,
		"GET /api/v1/federation/topology":                           h.topology,
		"POST /api/v1/federation/feeds":                             h.createFeed,
		"DELETE /api/v1/federation/feeds/{fed}/{slug}":              h.deleteFeed,
		"POST /api/v1/federation/feeds/{fed}/{slug}/transfer":       h.startTransfer,
		"POST /api/v1/federation/feeds/{fed}/{slug}/transfer/abort": h.abortTransfer,
		"GET /api/v1/federation/replication/rules":                  h.listRules,
		"POST /api/v1/federation/replication/rules":                 h.createRule,
		"GET /api/v1/federation/replication/rules/{id}":             h.getRule,
		"PATCH /api/v1/federation/replication/rules/{id}":           h.patchRule,
		"DELETE /api/v1/federation/replication/rules/{id}":          h.deleteRule,
		"GET /api/v1/federation/replication/rules/{id}/compliance":  h.ruleCompliance,
		"GET /api/v1/federation/replication/status":                 h.replicationStatus,
		"POST /api/v1/federation/replication/adopt":                 h.adoptJob,
		"GET /api/v1/federation/quarantines":                        h.quarantines,
		"POST /api/v1/federation/quarantines/{id}/resolve":          h.resolveQuarantine,
		"PUT /api/v1/federation/feeds/{fed}/{slug}/routing":         h.setFeedRouting,
		"GET /api/v1/federation/routes/{fed}/{slug}":                h.explainRoute,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

func (h federationHandlers) ctx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// reader authorizes read on the instance.
func (h federationHandlers) reader(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return p, false
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Read) {
		Error(w, r, 403, api.ErrorCodeForbidden, "instance read denied")
		return p, false
	}
	return p, true
}

// service returns the federation service for the caller's instance.
func (h federationHandlers) service(w http.ResponseWriter, r *http.Request, p identity.Principal) (*federation.Service, bool) {
	if h.svc == nil || h.svc.InstanceID() != p.InstanceID {
		Error(w, r, 503, api.ErrorCodeUnavailable, "federation unavailable: artifact serving and secret keys are required")
		return nil, false
	}
	return h.svc, true
}

func federationError(w http.ResponseWriter, r *http.Request, err error) {
	var in *federation.InputError
	var un *federation.UnreachableError
	var rej *federation.RejectedError
	switch {
	case errors.As(err, &in):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, in.Msg)
	case errors.Is(err, federation.ErrNotConfigured):
		Error(w, r, 409, api.ErrorCodeFederationNotConfigured, "this instance is not an active federation member")
	case errors.Is(err, federation.ErrListenerMissing):
		Error(w, r, 409, api.ErrorCodeFederationNotConfigured, "set ANT_FEDERATION_LISTEN and ANT_FEDERATION_PUBLIC_URL (https) before founding or joining")
	case errors.Is(err, federation.ErrAlreadyFederated):
		Error(w, r, 409, api.ErrorCodeConflict, "this instance already belongs to a federation")
	case errors.Is(err, federation.ErrNotAuthority):
		Error(w, r, 409, api.ErrorCodeNotAuthority, "only the federation authority can do this")
	case errors.Is(err, federation.ErrInvitationInvalid):
		Error(w, r, 403, api.ErrorCodeInvitationInvalid, "invitation invalid, expired, already used or revoked")
	case errors.Is(err, federation.ErrDuplicateInstance):
		Error(w, r, 409, api.ErrorCodeConflict, "instance ID or member name already in the federation")
	case errors.Is(err, federation.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "federation resource not found")
	case errors.Is(err, federation.ErrAuthorityLeave):
		Error(w, r, 409, api.ErrorCodeConflict, "the authority cannot leave; hand over authority first")
	case errors.Is(err, federation.ErrSelf):
		Error(w, r, 409, api.ErrorCodeConflict, "the authority cannot suspend or remove itself")
	case errors.Is(err, federation.ErrRecoveryMismatch):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "recovery key does not match the federation recovery key")
	case errors.Is(err, federation.ErrRevisionMismatch):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "revision changed; reload and retry")
	case errors.Is(err, federation.ErrRotationStale):
		Error(w, r, 409, api.ErrorCodeStaleGeneration, "the rotation is signed for another membership generation; fetch a new template and sign again")
	case errors.Is(err, federation.ErrStaleBase):
		Error(w, r, 409, api.ErrorCodeStaleGeneration, "base_generation is not the applied policy generation; review the latest policy and retry")
	case errors.Is(err, federation.ErrNotFederated):
		Error(w, r, 403, api.ErrorCodeNotFederated, "only federated principals can be asserted to other members")
	case errors.Is(err, federation.ErrConflict):
		Error(w, r, 409, api.ErrorCodeConflict, "the feed key or the feed is already bound")
	case errors.Is(err, federation.ErrFeedHomeElsewhere):
		Error(w, r, 409, api.ErrorCodeFeedHomeElsewhere, "the feed key already has a home; move it with a home transfer")
	case errors.Is(err, federation.ErrTransferActive):
		Error(w, r, 409, api.ErrorCodeFeedTransferring, "a home transfer is in progress for this feed key")
	case errors.Is(err, federation.ErrTransferState):
		Error(w, r, 409, api.ErrorCodeConflict, "the transfer is past promotion and can no longer be aborted")
	case errors.Is(err, federation.ErrReplicationOff):
		Error(w, r, 503, api.ErrorCodeUnavailable, "replication is not serving on this instance")
	case errors.Is(err, federation.ErrProtocol):
		Error(w, r, 409, api.ErrorCodeConflict, "no common federation protocol version")
	case errors.As(err, &un):
		Error(w, r, 503, api.ErrorCodeMemberUnreachable, "federation peer unreachable")
	case errors.As(err, &rej):
		if rej.Reason == "stale_generation" || rej.Reason == "stale_epoch" {
			Error(w, r, 409, api.ErrorCodeStaleGeneration, "document rejected: "+rej.Reason)
		} else {
			Error(w, r, 409, api.ErrorCodeDocumentRejected, "document rejected: "+rej.Reason)
		}
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "federation unavailable")
	}
}

func (h federationHandlers) status(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	if h.svc == nil || h.svc.InstanceID() != p.InstanceID {
		JSON(w, 200, federation.Status{InstanceID: p.InstanceID, State: "none", Health: "disabled", Alerts: []federation.Alert{}, Protocol: federation.ProtocolRange{Min: federation.ProtocolMin, Max: federation.ProtocolMax}})
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	st, err := h.svc.Status(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, st)
}

func (h federationHandlers) found(w http.ResponseWriter, r *http.Request) {
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
		Name       string            `json:"name"`
		MemberName string            `json:"member_name"`
		Labels     map[string]string `json:"labels"`
	}
	if !sourceJSON(w, r, &in, "name", "member_name", "labels") {
		return
	}
	ctx, cancel := h.ctx(r, 30*time.Second)
	defer cancel()
	out, err := svc.Found(ctx, p.PrincipalID, federation.FoundInput{Name: in.Name, MemberName: in.MemberName, Labels: in.Labels})
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 201, out)
}

func (h federationHandlers) join(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	var in struct {
		AuthorityURL string            `json:"authority_url"`
		CASHA256     string            `json:"ca_sha256"`
		CertSHA256   string            `json:"authority_cert_sha256"`
		Invitation   string            `json:"invitation"`
		Labels       map[string]string `json:"labels"`
	}
	if !sourceJSON(w, r, &in, "authority_url", "ca_sha256", "authority_cert_sha256", "invitation", "labels") {
		return
	}
	ctx, cancel := h.ctx(r, 45*time.Second)
	defer cancel()
	st, err := svc.Join(ctx, p.PrincipalID, federation.JoinInput{AuthorityURL: in.AuthorityURL, CASHA256: in.CASHA256, AuthorityCertSHA256: in.CertSHA256, Invitation: in.Invitation, Labels: in.Labels})
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, st)
}

func (h federationHandlers) leave(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
		Force   bool `json:"force"`
	}
	if !sourceJSON(w, r, &in, "confirm", "force") {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "leaving the federation requires confirm: true")
		return
	}
	ctx, cancel := h.ctx(r, 30*time.Second)
	defer cancel()
	st, err := svc.Leave(ctx, p.PrincipalID, in.Force)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, st)
}

func (h federationHandlers) members(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	if h.svc == nil || h.svc.InstanceID() != p.InstanceID {
		JSON(w, 200, map[string]any{"items": []federation.MemberView{}})
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	items, err := h.svc.Members(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": items})
}

func (h federationHandlers) member(w http.ResponseWriter, r *http.Request) {
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
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid member ID")
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	m, err := svc.Member(ctx, id)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, m)
}

func (h federationHandlers) setMember(w http.ResponseWriter, r *http.Request, action string) {
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
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid member ID")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var published *federation.Membership
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.member."+action, id, struct{ Action string }{action}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		m, doc, err := svc.SetMemberStatusTx(ctx, tx, p.PrincipalID, id, action)
		published = doc
		return m, err
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
		svc.AfterPublish(r.Context(), published, id)
	}
	JSON(w, 200, out.JSON)
}

// noReplay rejects Idempotency-Key on operations that return a secret once:
// a replay could never return the original secret again.
func noReplay(w http.ResponseWriter, r *http.Request) bool {
	if len(r.Header.Values("Idempotency-Key")) > 0 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "this operation is not replayable; omit Idempotency-Key")
		return false
	}
	return true
}

func (h federationHandlers) memberAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "suspend" && action != "resume" {
		Error(w, r, 404, api.ErrorCodeNotFound, "management route not found")
		return
	}
	h.setMember(w, r, action)
}

func (h federationHandlers) removeMember(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("confirm") != "true" {
		if _, ok := h.admin(w, r); ok {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "removing a member requires confirm=true")
		}
		return
	}
	h.setMember(w, r, "remove")
}

func (h federationHandlers) invitations(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	items, err := svc.ListInvitations(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": items})
}

func (h federationHandlers) createInvitation(w http.ResponseWriter, r *http.Request) {
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
		Name       string            `json:"name"`
		Labels     map[string]string `json:"labels"`
		TTLSeconds int64             `json:"ttl_seconds"`
	}
	if !sourceJSON(w, r, &in, "name", "labels", "ttl_seconds") {
		return
	}
	if in.TTLSeconds < 0 || in.TTLSeconds > 7*24*3600 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "ttl_seconds must be between 60 and 604800")
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	out, err := svc.CreateInvitation(ctx, p.PrincipalID, federation.InvitationInput{Name: in.Name, Labels: in.Labels, TTL: time.Duration(in.TTLSeconds) * time.Second})
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 201, out)
}

func (h federationHandlers) revokeInvitation(w http.ResponseWriter, r *http.Request) {
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
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid invitation ID")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	_, callErr, storeErr := idempotent(r, h.db, p, key, "federation.invitation.revoke", id, struct{}{}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return struct{}{}, svc.RevokeInvitationTx(ctx, tx, p.PrincipalID, id)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		w.WriteHeader(204)
	}
}

func (h federationHandlers) document(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	var generation int64
	if raw := r.PathValue("generation"); raw != "" {
		var err error
		if generation, err = strconv.ParseInt(raw, 10, 64); err != nil || generation < 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid generation")
			return
		}
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	d, err := svc.Document(ctx, r.PathValue("kind"), generation)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, d)
}

// recoveryTemplate returns the public input of the offline rotation signer.
func (h federationHandlers) recoveryTemplate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	out, err := svc.RecoveryTemplate(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, out)
}

// rotateRecovery publishes a recovery-key rotation signed offline (`ant
// federation recovery sign-rotation` / `sign-authority-cert`). No recovery
// private key is ever sent to the server.
func (h federationHandlers) rotateRecovery(w http.ResponseWriter, r *http.Request) {
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
	var in federation.SignedRecoveryRotation
	if !sourceJSON(w, r, &in, "rotation", "authority_certificate") {
		return
	}
	ctx, cancel := h.ctx(r, 30*time.Second)
	defer cancel()
	out, err := svc.RotateRecoveryKey(ctx, p.PrincipalID, in)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, out)
}
