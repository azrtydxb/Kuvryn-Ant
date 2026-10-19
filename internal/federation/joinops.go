package federation

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Two-phase join and certificate-revocation pruning (P2-M6).
//
// A join first claims its invitation (the authority adds the member and issues
// its certificate); the joiner then verifies the answer — that it came from
// the authority itself — and confirms with a statement signed by the member
// key it submitted, which consumes the invitation. A join that another member
// relayed, or that was interrupted, never confirms: after joinClaimTTL the
// authority removes the provisional member, revokes its certificate and the
// invitation is usable again, so interception cannot burn it.

func joinConfirmStatement(federationID, invitationID, instanceID string) []byte {
	b, _ := CanonicalJSON(map[string]any{"type": "join-confirm", "federation_id": federationID, "invitation_id": invitationID, "instance_id": instanceID})
	return b
}

type joinConfirmRequest struct {
	Invitation string `json:"invitation"`
	InstanceID string `json:"instance_id"`
	Signature  string `json:"signature"`
}

// confirmJoin runs on the authority for POST /federation/v1/join/confirm.
func (s *Service) confirmJoin(ctx context.Context, req joinConfirmRequest) error {
	invID, _, ok := strings.Cut(req.Invitation, ".")
	if !ok || !uuidPattern.MatchString(invID) || !uuidPattern.MatchString(req.InstanceID) {
		s.rejectInvitation(ctx, invID, req.InstanceID, "malformed")
		return ErrInvitationInvalid
	}
	var reason string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		_, _, claimedBy, why, err := s.checkInvitation(ctx, tx, st.FederationID, invID, req.Invitation, req.InstanceID, true)
		if err != nil {
			reason = why
			return err
		}
		member, found := applied.Member(req.InstanceID)
		if claimedBy != req.InstanceID || !found || member.Status != "active" || !verify(member.SigningKey, req.Signature, joinConfirmStatement(applied.FederationID, invID, req.InstanceID)) {
			reason = "confirm_mismatch"
			return ErrInvitationInvalid
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_invitations SET consumed_at=clock_timestamp(),consumed_by=$4 WHERE instance_id=$1 AND federation_id=$2 AND id=$3`,
			s.cfg.InstanceID, st.FederationID, invID, req.InstanceID); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.join.confirmed", req.InstanceID, "success", map[string]any{"invitation_id": invID, "name": member.Name})
	})
	if err != nil && reason != "" {
		s.rejectInvitation(ctx, invID, req.InstanceID, reason)
	}
	return err
}

func (s *Service) peerJoinConfirm(w http.ResponseWriter, r *http.Request) {
	var req joinConfirmRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.confirmJoin(ctx, req); err != nil {
		peerErr(w, err)
		return
	}
	peerJSON(w, http.StatusOK, map[string]any{"confirmed": true})
}

// unconfirmedClaims lists the claims older than joinClaimTTL (invitation to claimant).
func (s *Service) unconfirmedClaims(ctx context.Context, q queryer, federationID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text,claimed_by::text FROM federation_invitations WHERE instance_id=$1 AND federation_id=$2 AND claimed_at IS NOT NULL AND consumed_at IS NULL
AND revoked_at IS NULL AND claimed_at<$3`, s.cfg.InstanceID, federationID, s.now().Add(-joinClaimTTL))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, by string
		if err := rows.Scan(&id, &by); err != nil {
			return nil, err
		}
		out[id] = by
	}
	return out, rows.Err()
}

// ReapUnconfirmedJoins runs on the authority: it removes the provisional
// member of every claim that was not confirmed within joinClaimTTL, revokes
// its certificate and releases the invitation.
func (s *Service) ReapUnconfirmedJoins(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.State != "active" || st.Role != "authority" || st.FederationID == "" {
		return err
	}
	due, err := s.unconfirmedClaims(ctx, s.cfg.DB, st.FederationID)
	if err != nil || len(due) == 0 {
		return err
	}
	var doc *Membership
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		due, err := s.unconfirmedClaims(ctx, tx, st.FederationID)
		if err != nil || len(due) == 0 {
			return err
		}
		claimed := map[string]bool{}
		for _, by := range due {
			claimed[by] = true
		}
		doc, err = s.publish(ctx, tx, st, applied, "join-reaper", func(next *Membership) error {
			for i := range next.Members {
				m := &next.Members[i]
				if !claimed[m.InstanceID] || m.Status == "removed" {
					continue
				}
				m.Status = "removed"
				for _, serial := range []string{m.CertSerial, m.PreviousCertSerial} {
					if serial != "" && !next.Revoked(serial) {
						next.RevokedCertSerials = append(next.RevokedCertSerials, serial)
					}
				}
				m.PreviousCertSHA256, m.PreviousCertSerial, m.PreviousCertUntil = "", "", ""
			}
			return nil
		})
		if err != nil {
			return err
		}
		for id, by := range due {
			if _, err := tx.Exec(ctx, `UPDATE federation_invitations SET claimed_at=NULL,claimed_by=NULL WHERE instance_id=$1 AND federation_id=$2 AND id=$3`, s.cfg.InstanceID, st.FederationID, id); err != nil {
				return err
			}
			if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.join.abandoned", by, "success", map[string]any{"invitation_id": id, "generation": doc.Generation}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || doc == nil {
		return err
	}
	s.afterPublish(ctx, doc)
	return nil
}

// --- certificate revocation pruning ---

// revocationRetention is how long a serial stays in the revoked list. A member
// certificate lives memberLifetime from issuance and is revoked no earlier
// than issuance, so a serial first published as revoked this long ago belongs
// to a certificate that has expired by itself (TLS verification refuses it
// anyway); the margin covers clock skew. This relies on memberLifetime being
// the longest certificate lifetime ever issued.
const revocationRetention = memberLifetime + 2*24*time.Hour

// maxRevoked mirrors the document limit; pruneSoftLimit starts pruning during
// publication before the limit is reached.
const (
	maxRevoked     = 4096
	pruneSoftLimit = 3800
)

// ErrRevocationListFull reports a revocation list at its limit that holds no
// expired serial to prune.
var ErrRevocationListFull = errors.New("federation: the revoked certificate list is full and holds no expired serial")

// firstRevoked returns when each currently listed serial first appeared in
// the local document log.
func (s *Service) firstRevoked(ctx context.Context, q queryer, federationID string) (map[string]time.Time, error) {
	rows, err := q.Query(ctx, `SELECT serial,min(d.received_at) FROM federation_documents d CROSS JOIN LATERAL jsonb_array_elements_text(d.body->'revoked_cert_serials') AS serial
WHERE d.instance_id=$1 AND d.federation_id=$2 AND d.kind='membership' GROUP BY serial`, s.cfg.InstanceID, federationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var serial string
		var at time.Time
		if err := rows.Scan(&serial, &at); err != nil {
			return nil, err
		}
		out[serial] = at
	}
	return out, rows.Err()
}

// pruneRevoked drops expired serials from doc and returns how many.
func (s *Service) pruneRevoked(ctx context.Context, q queryer, doc *Membership) (int, error) {
	if len(doc.RevokedCertSerials) == 0 {
		return 0, nil
	}
	first, err := s.firstRevoked(ctx, q, doc.FederationID)
	if err != nil {
		return 0, err
	}
	live := map[string]bool{}
	for _, m := range doc.Members {
		if m.Status != "removed" {
			live[m.CertSerial], live[m.PreviousCertSerial] = true, true
		}
	}
	cutoff := s.now().Add(-revocationRetention)
	kept := doc.RevokedCertSerials[:0:0]
	for _, serial := range doc.RevokedCertSerials {
		if at, ok := first[serial]; ok && at.Before(cutoff) && !live[serial] {
			continue
		}
		kept = append(kept, serial)
	}
	pruned := len(doc.RevokedCertSerials) - len(kept)
	doc.RevokedCertSerials = kept
	return pruned, nil
}

// PruneRevocations runs on the authority: it publishes one generation without
// the revoked serials whose certificates have expired, so the list stays far
// below its limit of 4096 entries.
func (s *Service) PruneRevocations(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.State != "active" || st.Role != "authority" || st.FederationID == "" {
		return err
	}
	t := s.trust.Load()
	if t.doc == nil || len(t.doc.RevokedCertSerials) == 0 {
		return nil
	}
	probe := t.doc.Clone()
	if n, err := s.pruneRevoked(ctx, s.cfg.DB, probe); err != nil || n == 0 {
		return err
	}
	var doc *Membership
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		pruned := 0
		doc, err = s.publish(ctx, tx, st, applied, "revocation-pruning", func(next *Membership) error {
			var perr error
			pruned, perr = s.pruneRevoked(ctx, tx, next)
			return perr
		})
		if err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.certificate.revocations_pruned", st.FederationID, "success", map[string]any{"pruned": pruned,
			"remaining": len(doc.RevokedCertSerials), "generation": doc.Generation})
	})
	if err != nil || doc == nil {
		return err
	}
	s.afterPublish(ctx, doc)
	return nil
}
