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

// TopologyState is the applied topology head of this instance.
type TopologyState struct {
	Generation  int64
	Hash        string
	AppliedAt   *time.Time
	ConfirmedAt *time.Time
	Error       string
	ErrorAt     *time.Time
}

func loadTopologyState(ctx context.Context, q queryer, instance string) (TopologyState, error) {
	var t TopologyState
	err := q.QueryRow(ctx, `SELECT topology_generation,topology_hash,topology_applied_at,topology_confirmed_at,topology_error,topology_error_at FROM federation_state WHERE instance_id=$1`, instance).
		Scan(&t.Generation, &t.Hash, &t.AppliedAt, &t.ConfirmedAt, &t.Error, &t.ErrorAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TopologyState{}, nil
	}
	return t, err
}

func loadTopology(ctx context.Context, q queryer, instance, federation string, generation int64) (*TopologyDocument, error) {
	if generation == 0 {
		return nil, nil
	}
	var body []byte
	if err := q.QueryRow(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='topology' AND generation=$3`, instance, federation, generation).Scan(&body); err != nil {
		return nil, err
	}
	var d TopologyDocument
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	d.Normalize()
	return &d, nil
}

func topologyDocumentsAfter(ctx context.Context, q queryer, instance, federation string, after int64, limit int) ([]TopologyDocument, error) {
	rows, err := q.Query(ctx, `SELECT body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='topology' AND generation>$3 ORDER BY generation LIMIT $4`, instance, federation, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopologyDocument{}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d TopologyDocument
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// boundFeed returns the local feed bound to a feed key ("" when unbound).
func boundFeed(ctx context.Context, q queryer, instance, key string) (string, error) {
	var feed string
	err := q.QueryRow(ctx, `SELECT feed_id::text FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2`, instance, key).Scan(&feed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return feed, err
}

// replicationHead is this instance's durable change head.
func replicationHead(ctx context.Context, q queryer, instance string) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT seq FROM replication_heads WHERE instance_id=$1),0)`, instance).Scan(&seq)
	return seq, err
}

// storeTopology appends a verified topology document, applies its local
// fencing effects in the same transaction and moves the topology head:
//
//   - a feed this instance holds as home is recorded in federation_home_feeds;
//   - a generation that names another home for such a feed freezes it at once
//     (demotion completes later in the replication reconciler, but no write is
//     accepted from this transaction on);
//   - a planned transfer in freeze/drain from this instance freezes the feed
//     and records the change head; an aborted one unfreezes it.
func (s *Service) storeTopology(ctx context.Context, tx pgx.Tx, fid string, doc *TopologyDocument, confirmed time.Time) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO federation_documents(instance_id,federation_id,kind,generation,authority_epoch,hash,prev_hash,body) VALUES($1,$2,'topology',$3,$4,$5,$6,$7)
ON CONFLICT(instance_id,federation_id,kind,generation) DO NOTHING`, s.cfg.InstanceID, fid, doc.Generation, doc.AuthorityEpoch, TopologyHash(doc), doc.PrevHash, body); err != nil {
		return err
	}
	if err := s.applyTopologyEffects(ctx, tx, fid, doc); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE federation_state SET topology_generation=$2,topology_hash=$3,topology_applied_at=$4,topology_confirmed_at=$5,topology_error='',topology_error_at=NULL,
revision=revision+1,updated_at=clock_timestamp() WHERE instance_id=$1`, s.cfg.InstanceID, doc.Generation, TopologyHash(doc), s.now(), confirmed)
	return err
}

func (s *Service) applyTopologyEffects(ctx context.Context, tx pgx.Tx, fid string, doc *TopologyDocument) error {
	self := s.cfg.InstanceID
	for _, f := range doc.Feeds {
		feed, err := boundFeed(ctx, tx, self, f.Key)
		if err != nil {
			return err
		}
		if f.Home == self && feed != "" && !f.Transfer.Active() {
			if _, err := tx.Exec(ctx, `INSERT INTO federation_home_feeds(instance_id,feed_key,feed_id,since_generation) VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,feed_key) DO NOTHING`, self, f.Key, feed, doc.Generation); err != nil {
				return err
			}
		}
		t := f.Transfer
		if t == nil || t.From != self || feed == "" {
			continue
		}
		switch t.State {
		case TransferFreeze, TransferDrain:
			if _, err := tx.Exec(ctx, `INSERT INTO federation_home_feeds(instance_id,feed_key,feed_id,since_generation) VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,feed_key) DO NOTHING`, self, f.Key, feed, doc.Generation); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO federation_feed_freezes(instance_id,feed_id,feed_key,transfer_id,reason) VALUES($1,$2,$3,$4,'transfer') ON CONFLICT(instance_id,feed_id) DO NOTHING`, self, feed, f.Key, t.ID)
			if err != nil {
				return err
			}
			head, err := replicationHead(ctx, tx, self)
			if err != nil {
				return err
			}
			detail, _ := json.Marshal(map[string]any{"head": head, "feed_id": feed})
			if _, err := tx.Exec(ctx, `INSERT INTO federation_transfer_steps(instance_id,transfer_id,feed_key,step,detail) VALUES($1,$2,$3,'freeze',$4) ON CONFLICT DO NOTHING`, self, t.ID, f.Key, detail); err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				if err := audit(ctx, tx, self, "", "federation.transfer.frozen", feed, "success", map[string]any{"feed_key": f.Key, "transfer_id": t.ID, "head": head}); err != nil {
					return err
				}
			}
		case TransferAborted:
			tag, err := tx.Exec(ctx, `DELETE FROM federation_feed_freezes WHERE instance_id=$1 AND feed_id=$2 AND transfer_id=$3 AND reason='transfer'`, self, feed, t.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				if err := audit(ctx, tx, self, "", "federation.transfer.unfrozen", feed, "success", map[string]any{"feed_key": f.Key, "transfer_id": t.ID, "reason": t.Reason}); err != nil {
					return err
				}
			}
		}
	}
	// Feeds held as home that another member now owns: freeze before
	// anything else (§13 "two homes for a feed").
	rows, err := tx.Query(ctx, `SELECT feed_key,feed_id::text FROM federation_home_feeds WHERE instance_id=$1`, self)
	if err != nil {
		return err
	}
	type held struct{ key, feed string }
	var homes []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.key, &h.feed); err != nil {
			rows.Close()
			return err
		}
		homes = append(homes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range homes {
		f, ok := doc.Feed(h.key)
		if !ok {
			// The key left the topology: the content stays local, nothing to demote.
			if _, err := tx.Exec(ctx, `DELETE FROM federation_home_feeds WHERE instance_id=$1 AND feed_key=$2`, self, h.key); err != nil {
				return err
			}
			continue
		}
		if f.Home == self {
			continue
		}
		tag, err := tx.Exec(ctx, `INSERT INTO federation_feed_freezes(instance_id,feed_id,feed_key,reason) VALUES($1,$2,$3,'demote') ON CONFLICT(instance_id,feed_id) DO UPDATE SET reason='demote'`, self, h.feed, h.key)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if err := audit(ctx, tx, self, "", "federation.transfer.fenced", h.feed, "success", map[string]any{"feed_key": h.key, "new_home": f.Home, "generation": doc.Generation}); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyTopologyDocuments verifies and applies topology documents in order
// (last known good on rejection, like policy).
func (s *Service) applyTopologyDocuments(ctx context.Context, docs []TopologyDocument, source string, confirmed time.Time) (int, error) {
	applied := 0
	for i := range docs {
		doc := &docs[i]
		doc.Normalize()
		var rejectErr error
		err := s.tx(ctx, func(tx pgx.Tx) error {
			st, err := lockState(ctx, tx, s.cfg.InstanceID)
			if err != nil {
				return err
			}
			if st.State != "active" || st.FederationID == "" {
				return ErrNotConfigured
			}
			ts, err := loadTopologyState(ctx, tx, s.cfg.InstanceID)
			if err != nil {
				return err
			}
			if doc.FederationID == st.FederationID && doc.Generation > 0 && doc.Generation <= ts.Generation {
				if old, err := loadTopology(ctx, tx, s.cfg.InstanceID, st.FederationID, doc.Generation); err == nil && TopologyHash(old) == TopologyHash(doc) {
					return nil
				}
			}
			cur, err := loadTopology(ctx, tx, s.cfg.InstanceID, st.FederationID, ts.Generation)
			if err != nil {
				return err
			}
			membership, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
			if err != nil {
				return err
			}
			if err := VerifyTopology(doc, cur, membership); err != nil {
				rejectErr = err
				return err
			}
			if err := s.storeTopology(ctx, tx, st.FederationID, doc, confirmed); err != nil {
				return err
			}
			applied++
			return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.applied", st.FederationID, "success",
				map[string]any{"kind": "topology", "generation": doc.Generation, "hash": TopologyHash(doc), "source": source})
		})
		if rejectErr != nil {
			reason := RejectReason(rejectErr)
			if errors.Is(rejectErr, ErrEpochUnknown) {
				reason = "unknown_epoch"
			}
			s.recordTopologyRejection(ctx, doc, source, reason)
			return applied, &RejectedError{Reason: reason, Err: rejectErr}
		}
		if err != nil {
			return applied, err
		}
	}
	if applied > 0 {
		s.topologyCache.Store(nil)
		s.ReconcileReplication()
	}
	return applied, nil
}

func (s *Service) recordTopologyRejection(ctx context.Context, doc *TopologyDocument, source, reason string) {
	ctx = context.WithoutCancel(ctx)
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET topology_error=$2,topology_error_at=$3 WHERE instance_id=$1`, s.cfg.InstanceID,
			truncate(fmt.Sprintf("topology document rejected (%s) from %s", reason, source), 1024), s.now()); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.rejected", st.FederationID, "denied",
			map[string]any{"kind": "topology", "generation": doc.Generation, "reason": reason, "source": source, "hash": TopologyHash(doc)})
	})
	s.logOnce("topology-reject:"+source+":"+reason, "federation topology document rejected", "reason", reason, "source", source, "generation", doc.Generation)
}

// ApplyTopologyDocuments applies externally obtained topology documents (tests, relays).
func (s *Service) ApplyTopologyDocuments(ctx context.Context, docs []TopologyDocument, source string) (int, error) {
	return s.applyTopologyDocuments(ctx, docs, source, s.now())
}

// localTopologyHead reports this instance's topology head for peers.
func (s *Service) localTopologyHead(ctx context.Context, role string) (policyHead, error) {
	ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return policyHead{}, err
	}
	h := policyHead{Generation: ts.Generation, Hash: ts.Hash}
	if t := s.trust.Load(); t.doc != nil {
		h.AuthorityEpoch = t.doc.AuthorityEpoch
	}
	switch {
	case role == "authority":
		h.ConfirmedUnixMS = s.now().UnixMilli()
	case ts.ConfirmedAt != nil:
		h.ConfirmedUnixMS = ts.ConfirmedAt.UnixMilli()
	case ts.AppliedAt != nil:
		h.ConfirmedUnixMS = ts.AppliedAt.UnixMilli()
	}
	return h, nil
}

// syncTopologyFrom brings the topology up to the source's head.
func (s *Service) syncTopologyFrom(ctx context.Context, id peer.Identity, src Member, head policyHead) error {
	for range 100 {
		ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		confirmed := time.UnixMilli(head.ConfirmedUnixMS).UTC()
		if head.ConfirmedUnixMS <= 0 || confirmed.After(s.now()) {
			confirmed = s.now()
		}
		if head.Generation <= ts.Generation {
			if head.Generation == ts.Generation && ts.Generation > 0 && head.Hash == ts.Hash && (ts.ConfirmedAt == nil || confirmed.After(*ts.ConfirmedAt) || ts.Error != "") {
				_, err := s.cfg.DB.Exec(ctx, `UPDATE federation_state SET topology_confirmed_at=GREATEST(coalesce(topology_confirmed_at,$2),$2),topology_error='',topology_error_at=NULL WHERE instance_id=$1`, s.cfg.InstanceID, confirmed)
				return err
			}
			return nil
		}
		raw, err := s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/documents/topology?after="+strconv.FormatInt(ts.Generation, 10)+"&limit=50", nil, src.InstanceID)
		if err != nil {
			return err
		}
		var docs struct {
			Documents []TopologyDocument `json:"documents"`
		}
		if err := json.Unmarshal(raw, &docs); err != nil {
			return &RejectedError{Reason: "malformed", Err: ErrMalformed}
		}
		if len(docs.Documents) == 0 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		n, err := s.applyTopologyDocuments(ctx, docs.Documents, src.Name, confirmed)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}

// publishTopologyTx signs, applies and stores the next topology generation on
// the authority. mutate edits a copy of the applied body. Call
// AfterTopologyPublish after commit.
func (s *Service) publishTopologyTx(ctx context.Context, tx pgx.Tx, actor string, mutate func(body *TopologyBody, membership *Membership) error) (*TopologyDocument, error) {
	st, membership, err := s.authorityState(ctx, tx)
	if err != nil {
		return nil, err
	}
	ts, err := loadTopologyState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	cur, err := loadTopology(ctx, tx, s.cfg.InstanceID, st.FederationID, ts.Generation)
	if err != nil {
		return nil, err
	}
	var body TopologyBody
	if cur != nil {
		raw, _ := json.Marshal(cur.TopologyBody)
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	body.Normalize()
	if err := mutate(&body, membership); err != nil {
		return nil, err
	}
	body.Normalize()
	if err := ValidateTopology(membership.FederationName, &body, membership); err != nil {
		return nil, err
	}
	doc := &TopologyDocument{Kind: "topology", FederationID: st.FederationID, FederationName: membership.FederationName, Generation: ts.Generation + 1,
		AuthorityEpoch: membership.AuthorityEpoch, TopologyBody: body, IssuedAt: s.now().Format(time.RFC3339Nano), IssuedBy: actor}
	if cur != nil {
		doc.PrevHash = TopologyHash(cur)
	}
	if s.cfg.Keys == nil {
		return nil, ErrKeysUnavailable
	}
	_, seed, err := s.store.Open(ctx, s.cfg.InstanceID, st.AuthoritySecretID, authorityPurpose(membership.AuthorityEpoch))
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		clear(seed)
		return nil, errors.New("federation: authority key invalid")
	}
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	err = SignTopology(doc, key)
	clear(key)
	if err != nil {
		return nil, err
	}
	if err := VerifyTopology(doc, cur, membership); err != nil {
		return nil, err
	}
	if err := s.storeTopology(ctx, tx, st.FederationID, doc, s.now()); err != nil {
		return nil, err
	}
	return doc, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.document.published", st.FederationID, "success",
		map[string]any{"kind": "topology", "generation": doc.Generation, "hash": TopologyHash(doc), "issued_by": actor})
}

// AfterTopologyPublish nudges members and the local reconciler (never fails).
func (s *Service) AfterTopologyPublish(ctx context.Context, doc *TopologyDocument) {
	s.topologyCache.Store(nil)
	if doc != nil {
		s.ReconcileReplication()
		go s.nudgeMembers(context.WithoutCancel(ctx))
	}
}

// appliedTopology returns the applied topology document (cached), or nil.
func (s *Service) appliedTopology(ctx context.Context) (*TopologyDocument, error) {
	if d := s.topologyCache.Load(); d != nil {
		return d, nil
	}
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.FederationID == "" || st.State != "active" {
		return nil, err
	}
	ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || ts.Generation == 0 {
		return nil, err
	}
	d, err := loadTopology(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, ts.Generation)
	if err != nil {
		return nil, err
	}
	s.topologyCache.Store(d)
	return d, nil
}

// dropFederatedReplication releases federation-managed replication when the
// instance leaves or is removed: jobs return to origin local (jobs on
// federation-mode peers are disabled, they can no longer authenticate),
// freezes, home records, rule status and transfer steps are cleared and the
// topology head resets. Replicated content and replica sources are untouched;
// quarantines stay for review.
func dropFederatedReplication(ctx context.Context, tx pgx.Tx, instance string) error {
	for _, q := range []string{
		`UPDATE replication_jobs j SET origin='local',federation_rule_id=NULL,federation_feed_key=NULL,federation_rule_revision=NULL,revision=revision+1,
 enabled=j.enabled AND NOT EXISTS(SELECT 1 FROM replication_peers p WHERE p.instance_id=j.instance_id AND p.id=j.peer_id AND p.auth_mode='federation'),
 next_run_at=CASE WHEN EXISTS(SELECT 1 FROM replication_peers p WHERE p.instance_id=j.instance_id AND p.id=j.peer_id AND p.auth_mode='federation') THEN NULL ELSE j.next_run_at END
 WHERE j.instance_id=$1 AND j.origin='federation'`,
		`DELETE FROM federation_feed_freezes WHERE instance_id=$1`,
		`DELETE FROM federation_home_feeds WHERE instance_id=$1`,
		`DELETE FROM federation_rule_status WHERE instance_id=$1`,
		`DELETE FROM federation_transfer_steps WHERE instance_id=$1`,
		`UPDATE federation_state SET topology_generation=0,topology_hash='',topology_applied_at=NULL,topology_confirmed_at=NULL,topology_error='',topology_error_at=NULL WHERE instance_id=$1`,
	} {
		if _, err := tx.Exec(ctx, q, instance); err != nil {
			return err
		}
	}
	return nil
}
