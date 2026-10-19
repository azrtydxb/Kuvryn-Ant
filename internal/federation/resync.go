package federation

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

// Snapshot resync (§6.4 rule 6). A member whose applied chain diverged from
// the authority's (a prev_hash mismatch on the next generation, or the same
// generation with another hash) would otherwise stay stuck on its last
// generation forever. It recovers from the authority only: it finds the last
// generation it shares with the authority, verifies the authority's chain from
// there (signatures, certification, epochs, hash chain, the recovery-key pin
// of generation 1) and replaces the divergent segment in one audited
// transaction. It never moves the applied generation backwards.

// errNoDivergence means the local chain is a prefix of the authority's.
var errNoDivergence = errors.New("federation: chain does not diverge from the authority")

// resyncWindow is how many generations are fetched per step back.
const resyncWindow = 100

// fetchMembership returns up to resyncWindow authority documents after a generation.
func (s *Service) fetchMembership(ctx context.Context, id peer.Identity, src Member, after int64) ([]Membership, error) {
	raw, err := s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/documents/membership?after="+strconv.FormatInt(after, 10)+"&limit="+strconv.Itoa(resyncWindow), nil, src.InstanceID)
	if err != nil {
		return nil, err
	}
	var docs struct {
		Documents []Membership `json:"documents"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	return docs.Documents, nil
}

// resyncMembership replaces the membership chain segment that diverges from
// the authority's chain. src must be the authority of the applied membership.
func (s *Service) resyncMembership(ctx context.Context, id peer.Identity, src Member, higherEpoch bool) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	if st.State != "active" || st.AppliedGeneration == 0 {
		return ErrNotConfigured
	}
	local := func(g int64) (*Membership, error) {
		return loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, g)
	}
	// Find the first generation where the chains differ, stepping back a
	// window at a time; a document the authority does not have ends the search.
	var fork int64
	var segment []Membership
	for after := max(0, st.AppliedGeneration-resyncWindow); ; after = max(0, after-resyncWindow) {
		docs, err := s.fetchMembership(ctx, id, src, after)
		if err != nil {
			return err
		}
		if len(docs) == 0 || docs[0].Generation != after+1 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		fork = 0
		for i := range docs {
			g := docs[i].Generation
			if g != after+1+int64(i) {
				return &RejectedError{Reason: "generation_gap", Err: ErrGap}
			}
			if g > st.AppliedGeneration {
				break
			}
			mine, err := local(g)
			if err != nil {
				return err
			}
			if Hash(mine) != Hash(&docs[i]) {
				fork, segment = g, docs[i:]
				break
			}
		}
		if fork == 0 {
			if docs[len(docs)-1].Generation >= st.AppliedGeneration {
				return errNoDivergence
			}
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		if fork > after+1 || after == 0 {
			break // the generation before the fork is shared (or there is none)
		}
	}
	// Extend the authority segment to its head.
	for len(segment) < maxChain {
		next, err := s.fetchMembership(ctx, id, src, segment[len(segment)-1].Generation)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			break
		}
		segment = append(segment, next...)
	}
	head := &segment[len(segment)-1]
	prevDoc, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return err
	}
	// A segment never rolls the chain back, except when it moves to a higher
	// authority epoch: generations issued by a lower epoch after the fork are
	// void (a forced recovery happened while this member was partitioned).
	supersedes := higherEpoch && head.AuthorityEpoch > prevDoc.AuthorityEpoch
	if head.Generation < st.AppliedGeneration && !supersedes {
		return &RejectedError{Reason: "stale_generation", Err: ErrStaleGen} // never roll back
	}
	var base *Membership
	if fork > 1 {
		if base, err = local(fork - 1); err != nil {
			return err
		}
	} else {
		// Generation 1 itself differs: only the same federation under the
		// same pinned recovery key is acceptable.
		mine, err := local(1)
		if err != nil {
			return err
		}
		if segment[0].FederationID != mine.FederationID || segment[0].RecoveryPublicKey != mine.RecoveryPublicKey {
			return &RejectedError{Reason: "wrong_federation", Err: ErrWrongFed}
		}
	}
	applied := base
	for i := range segment {
		if err := Verify(&segment[i], applied); err != nil {
			s.recordRejection(ctx, &segment[i], src.Name, RejectReason(err))
			return &RejectedError{Reason: RejectReason(err), Err: err}
		}
		applied = &segment[i]
	}
	previous := st.AppliedHash
	err = s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if cur.State != "active" || cur.AppliedGeneration != st.AppliedGeneration || cur.AppliedHash != st.AppliedHash {
			return errors.New("federation: membership changed during resync; retrying later")
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ant.federation_resync','on',true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='membership' AND generation>=$3`, s.cfg.InstanceID, st.FederationID, fork); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ant.federation_resync','off',true)`); err != nil {
			return err
		}
		for i := range segment {
			if err := storeDocument(ctx, tx, s.cfg.InstanceID, &segment[i]); err != nil {
				return err
			}
		}
		if supersedes {
			// Members that exist only in the voided generations disappear.
			if _, err := tx.Exec(ctx, `DELETE FROM federation_members WHERE instance_id=$1 AND federation_id=$2 AND membership_generation<>$3`, s.cfg.InstanceID, st.FederationID, head.Generation); err != nil {
				return err
			}
		}
		if head.AuthorityEpoch > prevDoc.AuthorityEpoch {
			if err := s.applyEpochChange(ctx, tx, cur, prevDoc, head); err != nil {
				return err
			}
		}
		if self, ok := head.Member(s.cfg.InstanceID); ok && self.Status == "removed" {
			if _, err := tx.Exec(ctx, `UPDATE federation_state SET state='removed' WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
				return err
			}
			if err := dropFederatedIdentity(ctx, tx, s.cfg.InstanceID); err != nil {
				return err
			}
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.resynced", st.FederationID, "success", map[string]any{"kind": "membership",
			"from_generation": fork, "previous_generation": st.AppliedGeneration, "previous_hash": previous, "generation": head.Generation, "hash": Hash(head), "source": src.Name})
	})
	if err != nil {
		return err
	}
	return s.Refresh(ctx)
}

// fetchPolicy returns up to 50 authority policy documents after a generation.
func (s *Service) fetchPolicy(ctx context.Context, id peer.Identity, src Member, after int64) ([]PolicyDocument, error) {
	raw, err := s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/documents/policy?after="+strconv.FormatInt(after, 10)+"&limit=50", nil, src.InstanceID)
	if err != nil {
		return nil, err
	}
	var docs struct {
		Documents []PolicyDocument `json:"documents"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	return docs.Documents, nil
}

// resyncPolicy is resyncMembership for the policy chain. Policy documents
// verify against the applied membership, so the authority segment must be
// signed in the current authority epoch.
func (s *Service) resyncPolicy(ctx context.Context, id peer.Identity, src Member, confirmed time.Time) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	if st.State != "active" || ps.Generation == 0 {
		return ErrNotConfigured
	}
	local := func(g int64) (*PolicyDocument, error) {
		return loadPolicy(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, g)
	}
	const window = 50
	var fork int64
	var segment []PolicyDocument
	for after := max(0, ps.Generation-window); ; after = max(0, after-window) {
		docs, err := s.fetchPolicy(ctx, id, src, after)
		if err != nil {
			return err
		}
		if len(docs) == 0 || docs[0].Generation != after+1 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		fork = 0
		for i := range docs {
			g := docs[i].Generation
			if g != after+1+int64(i) {
				return &RejectedError{Reason: "generation_gap", Err: ErrGap}
			}
			if g > ps.Generation {
				break
			}
			mine, err := local(g)
			if err != nil {
				return err
			}
			if mine == nil || PolicyHash(mine) != PolicyHash(&docs[i]) {
				fork, segment = g, docs[i:]
				break
			}
		}
		if fork == 0 {
			if docs[len(docs)-1].Generation >= ps.Generation {
				return errNoDivergence
			}
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		if fork > after+1 || after == 0 {
			break
		}
	}
	for len(segment) < maxChain {
		next, err := s.fetchPolicy(ctx, id, src, segment[len(segment)-1].Generation)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			break
		}
		segment = append(segment, next...)
	}
	head := &segment[len(segment)-1]
	if head.Generation < ps.Generation {
		return &RejectedError{Reason: "stale_generation", Err: ErrStaleGen}
	}
	membership, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return err
	}
	var applied *PolicyDocument
	if fork > 1 {
		if applied, err = local(fork - 1); err != nil {
			return err
		}
	}
	for i := range segment {
		if err := VerifyPolicy(&segment[i], applied, membership); err != nil {
			reason := RejectReason(err)
			s.recordPolicyRejection(ctx, &segment[i], src.Name, reason)
			return &RejectedError{Reason: reason, Err: err}
		}
		applied = &segment[i]
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		now, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if cur.State != "active" || now.Generation != ps.Generation || now.Hash != ps.Hash {
			return errors.New("federation: policy changed during resync; retrying later")
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ant.federation_resync','on',true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='policy' AND generation>=$3`, s.cfg.InstanceID, st.FederationID, fork); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ant.federation_resync','off',true)`); err != nil {
			return err
		}
		// Materialization depends only on the newest document; the earlier
		// ones are stored for the audit log.
		for i := range segment[:len(segment)-1] {
			d := &segment[i]
			body, err := json.Marshal(d)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO federation_documents(instance_id,federation_id,kind,generation,authority_epoch,hash,prev_hash,body) VALUES($1,$2,'policy',$3,$4,$5,$6,$7)`,
				s.cfg.InstanceID, st.FederationID, d.Generation, d.AuthorityEpoch, PolicyHash(d), d.PrevHash, body); err != nil {
				return err
			}
		}
		if _, err := s.storePolicy(ctx, tx, st.FederationID, head, confirmed); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.resynced", st.FederationID, "success", map[string]any{"kind": "policy",
			"from_generation": fork, "previous_generation": ps.Generation, "previous_hash": ps.Hash, "generation": head.Generation, "hash": PolicyHash(head), "source": src.Name})
	})
	if err != nil {
		return err
	}
	s.policyCache.Store(nil)
	return nil
}

// isChainMismatch reports a rejection that a snapshot resync can repair.
func isChainMismatch(err error) bool {
	var rej *RejectedError
	return errors.As(err, &rej) && rej.Reason == "chain_mismatch"
}

// fetchTopology returns up to 50 authority topology documents after a generation.
func (s *Service) fetchTopology(ctx context.Context, id peer.Identity, src Member, after int64) ([]TopologyDocument, error) {
	raw, err := s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/documents/topology?after="+strconv.FormatInt(after, 10)+"&limit=50", nil, src.InstanceID)
	if err != nil {
		return nil, err
	}
	var docs struct {
		Documents []TopologyDocument `json:"documents"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	return docs.Documents, nil
}

// resyncTopology is resyncMembership for the topology chain. Topology documents
// verify against the applied membership, so the authority segment must be
// signed in the current authority epoch.
func (s *Service) resyncTopology(ctx context.Context, id peer.Identity, src Member, confirmed time.Time) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	ps, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	if st.State != "active" || ps.Generation == 0 {
		return ErrNotConfigured
	}
	local := func(g int64) (*TopologyDocument, error) {
		return loadTopology(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, g)
	}
	const window = 50
	var fork int64
	var segment []TopologyDocument
	for after := max(0, ps.Generation-window); ; after = max(0, after-window) {
		docs, err := s.fetchTopology(ctx, id, src, after)
		if err != nil {
			return err
		}
		if len(docs) == 0 || docs[0].Generation != after+1 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		fork = 0
		for i := range docs {
			g := docs[i].Generation
			if g != after+1+int64(i) {
				return &RejectedError{Reason: "generation_gap", Err: ErrGap}
			}
			if g > ps.Generation {
				break
			}
			mine, err := local(g)
			if err != nil {
				return err
			}
			if mine == nil || TopologyHash(mine) != TopologyHash(&docs[i]) {
				fork, segment = g, docs[i:]
				break
			}
		}
		if fork == 0 {
			if docs[len(docs)-1].Generation >= ps.Generation {
				return errNoDivergence
			}
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		if fork > after+1 || after == 0 {
			break
		}
	}
	for len(segment) < maxChain {
		next, err := s.fetchTopology(ctx, id, src, segment[len(segment)-1].Generation)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			break
		}
		segment = append(segment, next...)
	}
	head := &segment[len(segment)-1]
	if head.Generation < ps.Generation {
		return &RejectedError{Reason: "stale_generation", Err: ErrStaleGen}
	}
	membership, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return err
	}
	var applied *TopologyDocument
	if fork > 1 {
		if applied, err = local(fork - 1); err != nil {
			return err
		}
	}
	for i := range segment {
		if err := VerifyTopology(&segment[i], applied, membership); err != nil {
			reason := RejectReason(err)
			s.recordTopologyRejection(ctx, &segment[i], src.Name, reason)
			return &RejectedError{Reason: reason, Err: err}
		}
		applied = &segment[i]
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		now, err := loadTopologyState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if cur.State != "active" || now.Generation != ps.Generation || now.Hash != ps.Hash {
			return errors.New("federation: topology changed during resync; retrying later")
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ant.federation_resync','on',true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='topology' AND generation>=$3`, s.cfg.InstanceID, st.FederationID, fork); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('ant.federation_resync','off',true)`); err != nil {
			return err
		}
		// Topology effects (transfers, bindings) are applied in order.
		for i := range segment {
			if err := s.storeTopology(ctx, tx, st.FederationID, &segment[i], confirmed); err != nil {
				return err
			}
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.resynced", st.FederationID, "success", map[string]any{"kind": "topology",
			"from_generation": fork, "previous_generation": ps.Generation, "previous_hash": ps.Hash, "generation": head.Generation, "hash": TopologyHash(head), "source": src.Name})
	})
	if err != nil {
		return err
	}
	s.topologyCache.Store(nil)
	s.ReconcileReplication()
	return nil
}
