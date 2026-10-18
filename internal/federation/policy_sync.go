package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

// applyPolicyDocuments verifies and applies policy documents in order. A
// document that fails verification (forged, tampered, stale, gap, unknown
// epoch) is rejected and audited, the previous generation stays in force
// (last known good, D4) and the error is shown in status and alerts.
// confirmed is when the newest document was last known current at its source.
func (s *Service) applyPolicyDocuments(ctx context.Context, docs []PolicyDocument, source string, confirmed time.Time) (int, error) {
	applied := 0
	for i := range docs {
		doc := &docs[i]
		var rejectErr error
		err := s.tx(ctx, func(tx pgx.Tx) error {
			st, err := lockState(ctx, tx, s.cfg.InstanceID)
			if err != nil {
				return err
			}
			if st.State != "active" || st.FederationID == "" {
				return ErrNotConfigured
			}
			ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
			if err != nil {
				return err
			}
			if doc.FederationID == st.FederationID && doc.Generation > 0 && doc.Generation <= ps.Generation {
				if old, err := loadPolicy(ctx, tx, s.cfg.InstanceID, st.FederationID, doc.Generation); err == nil && PolicyHash(old) == PolicyHash(doc) {
					return nil // re-delivery of an applied document
				}
			}
			cur, err := loadPolicy(ctx, tx, s.cfg.InstanceID, st.FederationID, ps.Generation)
			if err != nil {
				return err
			}
			membership, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
			if err != nil {
				return err
			}
			if err := VerifyPolicy(doc, cur, membership); err != nil {
				rejectErr = err
				return err
			}
			report, err := s.storePolicy(ctx, tx, st.FederationID, doc, confirmed)
			if err != nil {
				return err
			}
			applied++
			details := map[string]any{"kind": "policy", "generation": doc.Generation, "hash": PolicyHash(doc), "source": source}
			if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.applied", st.FederationID, "success", details); err != nil {
				return err
			}
			return audit(ctx, tx, s.cfg.InstanceID, "", "federation.policy.applied", st.FederationID, "success", map[string]any{"generation": doc.Generation,
				"roles": report.Roles, "principals": report.Principals, "revoked": report.Revoked, "unresolved_feed_keys": report.UnresolvedFeedKeys, "conflicts": len(report.Conflicts)})
		})
		if rejectErr != nil {
			reason := RejectReason(rejectErr)
			if errors.Is(rejectErr, ErrEpochUnknown) {
				reason = "unknown_epoch"
			}
			s.recordPolicyRejection(ctx, doc, source, reason)
			return applied, &RejectedError{Reason: reason, Err: rejectErr}
		}
		if err != nil {
			return applied, err
		}
	}
	if applied > 0 {
		s.policyCache.Store(nil)
		s.search.resetCache()
	}
	return applied, nil
}

func (s *Service) recordPolicyRejection(ctx context.Context, doc *PolicyDocument, source, reason string) {
	ctx = context.WithoutCancel(ctx)
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET policy_error=$2,policy_error_at=$3 WHERE instance_id=$1`, s.cfg.InstanceID,
			truncate(fmt.Sprintf("policy document rejected (%s) from %s", reason, source), 1024), s.now()); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.rejected", st.FederationID, "denied",
			map[string]any{"kind": "policy", "generation": doc.Generation, "reason": reason, "source": source, "hash": PolicyHash(doc)})
	})
	s.logOnce("policy-reject:"+source+":"+reason, "federation policy document rejected", "reason", reason, "source", source, "generation", doc.Generation)
}

// ApplyPolicyDocuments applies externally obtained policy documents (tests, relays).
func (s *Service) ApplyPolicyDocuments(ctx context.Context, docs []PolicyDocument, source string) (int, error) {
	return s.applyPolicyDocuments(ctx, docs, source, s.now())
}

// policyHead is the policy part of GET /federation/v1/heads.
type policyHead struct {
	Generation     int64  `json:"generation"`
	Hash           string `json:"hash"`
	AuthorityEpoch int64  `json:"authority_epoch"`
	// ConfirmedUnixMS is when the source last knew this generation to be
	// current. Relays pass on their own confirmation time, so staleness
	// propagates and a relayed copy never looks fresher than its source.
	ConfirmedUnixMS int64 `json:"confirmed_unix_ms"`
}

// localPolicyHead reports this instance's policy head for peers.
func (s *Service) localPolicyHead(ctx context.Context, role string) (policyHead, error) {
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return policyHead{}, err
	}
	h := policyHead{Generation: ps.Generation, Hash: ps.Hash}
	t := s.trust.Load()
	if t.doc != nil {
		h.AuthorityEpoch = t.doc.AuthorityEpoch
	}
	switch {
	case role == "authority":
		h.ConfirmedUnixMS = s.now().UnixMilli()
	case ps.ConfirmedAt != nil:
		h.ConfirmedUnixMS = ps.ConfirmedAt.UnixMilli()
	case ps.AppliedAt != nil:
		h.ConfirmedUnixMS = ps.AppliedAt.UnixMilli()
	}
	return h, nil
}

// syncPolicyFrom brings the policy up to the source's head and records the
// source's confirmation time. It returns nil when already current.
func (s *Service) syncPolicyFrom(ctx context.Context, id peer.Identity, src Member, head policyHead) error {
	for range 100 {
		ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		confirmed := time.UnixMilli(head.ConfirmedUnixMS).UTC()
		if head.ConfirmedUnixMS <= 0 || confirmed.After(s.now()) {
			confirmed = s.now()
		}
		if head.Generation <= ps.Generation {
			// Current (or the source is behind us): move the confirmation
			// forward only when the source holds the same generation.
			if head.Generation == ps.Generation && ps.Generation > 0 && head.Hash == ps.Hash && (ps.ConfirmedAt == nil || confirmed.After(*ps.ConfirmedAt) || ps.Error != "") {
				// Confirmation by a valid source also clears an earlier rejection
				// (for example a forged document from a relay).
				_, err := s.cfg.DB.Exec(ctx, `UPDATE federation_state SET policy_confirmed_at=GREATEST(coalesce(policy_confirmed_at,$2),$2),policy_error='',policy_error_at=NULL WHERE instance_id=$1`, s.cfg.InstanceID, confirmed)
				return err
			}
			return nil
		}
		raw, err := s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/documents/policy?after="+strconv.FormatInt(ps.Generation, 10)+"&limit=50", nil, src.InstanceID)
		if err != nil {
			return err
		}
		var docs struct {
			Documents []PolicyDocument `json:"documents"`
		}
		if err := json.Unmarshal(raw, &docs); err != nil {
			return &RejectedError{Reason: "malformed", Err: ErrMalformed}
		}
		if len(docs.Documents) == 0 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		n, err := s.applyPolicyDocuments(ctx, docs.Documents, src.Name, confirmed)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}

// --- authority: draft and publish ---

// PolicyDraft is the authority's working copy of the next policy.
type PolicyDraft struct {
	Body           PolicyBody `json:"policy"`
	BaseGeneration int64      `json:"base_generation"`
	Revision       int64      `json:"revision"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Diff           PolicyDiff `json:"diff"`
}

// SavePolicyDraftTx validates and stores the authority draft. expected is the
// draft revision the caller saw (0 to create).
func (s *Service) SavePolicyDraftTx(ctx context.Context, tx pgx.Tx, actor string, body PolicyBody, expected int64) (PolicyDraft, error) {
	st, membership, err := s.authorityState(ctx, tx)
	if err != nil {
		return PolicyDraft{}, err
	}
	body.Normalize()
	if err := ValidatePolicy(membership.FederationName, &body); err != nil {
		return PolicyDraft{}, err
	}
	ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return PolicyDraft{}, err
	}
	raw, _ := json.Marshal(body)
	var out PolicyDraft
	var cur int64
	err = tx.QueryRow(ctx, `SELECT revision FROM federation_policy_drafts WHERE instance_id=$1 AND federation_id=$2 FOR UPDATE`, s.cfg.InstanceID, st.FederationID).Scan(&cur)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if expected != 0 {
			return PolicyDraft{}, ErrRevisionMismatch
		}
		err = tx.QueryRow(ctx, `INSERT INTO federation_policy_drafts(instance_id,federation_id,body,base_generation,updated_by) VALUES($1,$2,$3,$4,nullif($5,'')::uuid) RETURNING revision,updated_at`,
			s.cfg.InstanceID, st.FederationID, raw, ps.Generation, uuidOrEmpty(actor)).Scan(&out.Revision, &out.UpdatedAt)
	case err != nil:
		return PolicyDraft{}, err
	default:
		if expected != cur {
			return PolicyDraft{}, ErrRevisionMismatch
		}
		err = tx.QueryRow(ctx, `UPDATE federation_policy_drafts SET body=$3,base_generation=$4,revision=revision+1,updated_by=nullif($5,'')::uuid,updated_at=clock_timestamp()
WHERE instance_id=$1 AND federation_id=$2 RETURNING revision,updated_at`, s.cfg.InstanceID, st.FederationID, raw, ps.Generation, uuidOrEmpty(actor)).Scan(&out.Revision, &out.UpdatedAt)
	}
	if err != nil {
		return PolicyDraft{}, err
	}
	out.Body, out.BaseGeneration = body, ps.Generation
	cur2, err := loadPolicy(ctx, tx, s.cfg.InstanceID, st.FederationID, ps.Generation)
	if err != nil {
		return PolicyDraft{}, err
	}
	var from *PolicyBody
	if cur2 != nil {
		from = &cur2.PolicyBody
	}
	out.Diff = DiffPolicy(from, &out.Body)
	out.Diff.FromGeneration, out.Diff.ToGeneration = ps.Generation, ps.Generation+1
	return out, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.policy.draft_saved", st.FederationID, "success", map[string]any{"revision": out.Revision, "base_generation": ps.Generation, "changes": out.Diff.Changes})
}

// Draft returns the authority draft, or ErrNotFound.
func (s *Service) Draft(ctx context.Context) (PolicyDraft, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return PolicyDraft{}, err
	}
	if st.FederationID == "" {
		return PolicyDraft{}, ErrNotConfigured
	}
	var out PolicyDraft
	var raw []byte
	err = s.cfg.DB.QueryRow(ctx, `SELECT body,base_generation,revision,updated_at FROM federation_policy_drafts WHERE instance_id=$1 AND federation_id=$2`, s.cfg.InstanceID, st.FederationID).
		Scan(&raw, &out.BaseGeneration, &out.Revision, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyDraft{}, ErrNotFound
	}
	if err != nil {
		return PolicyDraft{}, err
	}
	if err := json.Unmarshal(raw, &out.Body); err != nil {
		return PolicyDraft{}, err
	}
	out.Body.Normalize()
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return PolicyDraft{}, err
	}
	cur, err := loadPolicy(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, ps.Generation)
	if err != nil {
		return PolicyDraft{}, err
	}
	var from *PolicyBody
	if cur != nil {
		from = &cur.PolicyBody
	}
	out.Diff = DiffPolicy(from, &out.Body)
	out.Diff.FromGeneration, out.Diff.ToGeneration = ps.Generation, ps.Generation+1
	return out, nil
}

// PublishInput publishes the next policy generation.
type PublishInput struct {
	// BaseGeneration must equal the applied generation (compare-and-swap).
	BaseGeneration int64
	// Body, when nil, publishes the stored draft.
	Body *PolicyBody
}

// PublishPolicyTx signs, applies and stores the next policy generation on the
// authority. Call AfterPolicyPublish after commit to nudge members.
func (s *Service) PublishPolicyTx(ctx context.Context, tx pgx.Tx, actor string, in PublishInput) (*PolicyDocument, PolicyReport, error) {
	st, membership, err := s.authorityState(ctx, tx)
	if err != nil {
		return nil, PolicyReport{}, err
	}
	ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return nil, PolicyReport{}, err
	}
	if in.BaseGeneration != ps.Generation {
		return nil, PolicyReport{}, ErrStaleBase
	}
	var body PolicyBody
	if in.Body != nil {
		body = *in.Body
	} else {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT body FROM federation_policy_drafts WHERE instance_id=$1 AND federation_id=$2 FOR UPDATE`, s.cfg.InstanceID, st.FederationID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, PolicyReport{}, invalid("no policy draft to publish; save a draft or include policy")
		}
		if err != nil {
			return nil, PolicyReport{}, err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, PolicyReport{}, err
		}
	}
	body.Normalize()
	if err := ValidatePolicy(membership.FederationName, &body); err != nil {
		return nil, PolicyReport{}, err
	}
	cur, err := loadPolicy(ctx, tx, s.cfg.InstanceID, st.FederationID, ps.Generation)
	if err != nil {
		return nil, PolicyReport{}, err
	}
	doc := &PolicyDocument{Kind: "policy", FederationID: st.FederationID, FederationName: membership.FederationName, Generation: ps.Generation + 1,
		AuthorityEpoch: membership.AuthorityEpoch, PolicyBody: body, IssuedAt: s.now().Format(time.RFC3339Nano), IssuedBy: actor}
	if cur != nil {
		doc.PrevHash = PolicyHash(cur)
	}
	_, seed, err := s.store.Open(ctx, s.cfg.InstanceID, st.AuthoritySecretID, authorityPurpose(membership.AuthorityEpoch))
	if err != nil {
		return nil, PolicyReport{}, err
	}
	if len(seed) != ed25519.SeedSize {
		clear(seed)
		return nil, PolicyReport{}, errors.New("federation: authority key invalid")
	}
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	err = SignPolicy(doc, key)
	clear(key)
	if err != nil {
		return nil, PolicyReport{}, err
	}
	if err := VerifyPolicy(doc, cur, membership); err != nil {
		return nil, PolicyReport{}, err
	}
	report, err := s.storePolicy(ctx, tx, st.FederationID, doc, s.now())
	if err != nil {
		return nil, PolicyReport{}, err
	}
	if in.Body == nil {
		if _, err := tx.Exec(ctx, `DELETE FROM federation_policy_drafts WHERE instance_id=$1 AND federation_id=$2`, s.cfg.InstanceID, st.FederationID); err != nil {
			return nil, PolicyReport{}, err
		}
	}
	var from *PolicyBody
	if cur != nil {
		from = &cur.PolicyBody
	}
	diff := DiffPolicy(from, &body)
	if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.document.published", st.FederationID, "success", map[string]any{"kind": "policy", "generation": doc.Generation, "hash": PolicyHash(doc), "issued_by": actor, "changes": diff.Changes}); err != nil {
		return nil, PolicyReport{}, err
	}
	return doc, report, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.policy.applied", st.FederationID, "success", map[string]any{"generation": doc.Generation,
		"roles": report.Roles, "principals": report.Principals, "revoked": report.Revoked, "unresolved_feed_keys": report.UnresolvedFeedKeys, "conflicts": len(report.Conflicts)})
}

// AfterPolicyPublish pushes new policy documents to members (never fails).
func (s *Service) AfterPolicyPublish(ctx context.Context, doc *PolicyDocument) {
	s.policyCache.Store(nil)
	s.search.resetCache()
	if doc != nil {
		go s.nudgeMembers(context.WithoutCancel(ctx))
	}
}

func uuidOrEmpty(s string) string {
	if uuidPattern.MatchString(s) {
		return s
	}
	return ""
}
