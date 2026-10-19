package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

// Authority operations (P2-M6, docs/planning/phase-2-federation.md section 3.1
// and 5.1).
//
// An epoch change moves the authority role to another member:
//
//   - Planned handover (current authority is alive and cooperating). The target
//     generates the epoch key itself and returns only its public half; the
//     current authority certifies it with its own epoch key (certified_by
//     previous_epoch) and hands over the federation CA key over the mutually
//     authenticated channel; the target accepts with its member signing key and
//     publishes the first document of the new epoch, signed by the new key. The
//     epoch key of the old authority is never exported. The old authority
//     applies that document and fences itself (it deletes its authority and CA
//     keys).
//   - Forced recovery (current authority lost or compromised). The operator who
//     holds the offline recovery key certifies the new authority key (certified_by
//     recovery) from a template produced by the target; the target accepts,
//     creates a new federation CA (the old CA key died with the old authority;
//     both CAs are trusted for one certificate lifetime) and publishes the
//     first document of the new epoch. A returning old authority learns of the
//     higher epoch from its peers and fences itself.
//
// Members accept the highest epoch that has a valid chain back to the pinned
// recovery key; documents of a lower epoch are rejected (stale_epoch).

// handoverTTL bounds how long a started handover blocks publishing on the
// current authority before it is considered abandoned.
const handoverTTL = 10 * time.Minute

// Authority operation errors.
var (
	ErrHandoverPending  = errors.New("federation: an authority handover is in progress; wait for it or abort it")
	ErrMemberNotCurrent = errors.New("federation: the target member has not applied the latest documents; retry shortly")
	ErrIsAuthority      = errors.New("federation: this instance is the authority")
	ErrNoHandover       = errors.New("federation: no authority handover in progress")
	ErrRecoveryProof    = errors.New("federation: the recovery signature is invalid for this challenge")
)

// --- status ---

// EpochRecord is one authority epoch of the membership chain.
type EpochRecord struct {
	Epoch           int64  `json:"epoch"`
	InstanceID      string `json:"instance_id"`
	Name            string `json:"name,omitempty"`
	CertifiedBy     string `json:"certified_by"`
	FromGeneration  int64  `json:"from_generation"`
	KeyFingerprint  string `json:"key_fingerprint"`
	AppliedAt       string `json:"applied_at,omitempty"`
	IssuedAtMembers string `json:"-"`
}

// HandoverStatus is a handover this authority started.
type HandoverStatus struct {
	ToMemberID     string    `json:"to_member_id"`
	ToName         string    `json:"to_name,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	BaseGeneration int64     `json:"base_generation"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// FenceStatus records that this instance was the authority and learned of a
// higher epoch.
type FenceStatus struct {
	At      time.Time `json:"at"`
	ByEpoch int64     `json:"by_epoch"`
}

// RecoveryDrill reports the recovery-key possession drill.
type RecoveryDrill struct {
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// Due is true when the key was never verified or was last verified more
	// than RecoveryDrillInterval ago.
	Due bool `json:"due"`
}

// RecoveryDrillInterval is how often the recovery key should be proven.
const RecoveryDrillInterval = 180 * 24 * time.Hour

// AuthorityStatus is GET /api/v1/federation/authority.
type AuthorityStatus struct {
	Role                string          `json:"role"`
	Epoch               int64           `json:"epoch"`
	AuthorityInstanceID string          `json:"authority_instance_id"`
	AuthorityName       string          `json:"authority_name,omitempty"`
	KeyFingerprint      string          `json:"key_fingerprint"`
	RecoveryKey         string          `json:"recovery_key_fingerprint"`
	Epochs              []EpochRecord   `json:"epochs"`
	Handover            *HandoverStatus `json:"handover,omitempty"`
	Fenced              *FenceStatus    `json:"fenced,omitempty"`
	Recovery            RecoveryDrill   `json:"recovery"`
	PendingKey          bool            `json:"pending_authority_key"`
}

// AuthorityStatus describes the epoch chain, handover, fencing and recovery
// drill state of this instance's view.
func (s *Service) AuthorityStatus(ctx context.Context) (AuthorityStatus, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return AuthorityStatus{}, err
	}
	if st.State != "active" || st.FederationID == "" || st.AppliedGeneration == 0 {
		return AuthorityStatus{}, ErrNotConfigured
	}
	doc, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return AuthorityStatus{}, err
	}
	out := AuthorityStatus{Role: st.Role, Epoch: doc.AuthorityEpoch, AuthorityInstanceID: doc.Authority.InstanceID, KeyFingerprint: KeyFingerprint(doc.Authority.PublicKey),
		RecoveryKey: KeyFingerprint(doc.RecoveryPublicKey), Epochs: []EpochRecord{}, PendingKey: st.PendingAuthoritySecretID != ""}
	if m, ok := doc.Member(doc.Authority.InstanceID); ok {
		out.AuthorityName = m.Name
	}
	rows, err := s.cfg.DB.Query(ctx, `SELECT DISTINCT ON(authority_epoch) authority_epoch,generation,body->'authority'->>'instance_id',body->'authority'->>'certified_by',
body->'authority'->>'public_key',received_at FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='membership' ORDER BY authority_epoch,generation`,
		s.cfg.InstanceID, st.FederationID)
	if err != nil {
		return AuthorityStatus{}, err
	}
	for rows.Next() {
		var e EpochRecord
		var key string
		var at time.Time
		if err := rows.Scan(&e.Epoch, &e.FromGeneration, &e.InstanceID, &e.CertifiedBy, &key, &at); err != nil {
			rows.Close()
			return AuthorityStatus{}, err
		}
		e.KeyFingerprint, e.AppliedAt = KeyFingerprint(key), at.UTC().Format(time.RFC3339)
		if m, ok := doc.Member(e.InstanceID); ok {
			e.Name = m.Name
		}
		out.Epochs = append(out.Epochs, e)
	}
	rows.Close()
	if st.HandoverTo != "" && st.HandoverStartedAt != nil {
		h := &HandoverStatus{ToMemberID: st.HandoverTo, StartedAt: *st.HandoverStartedAt, BaseGeneration: st.HandoverBaseGeneration, ExpiresAt: st.HandoverStartedAt.Add(handoverTTL)}
		if m, ok := doc.Member(st.HandoverTo); ok {
			h.ToName = m.Name
		}
		out.Handover = h
	}
	if st.FencedAt != nil {
		out.Fenced = &FenceStatus{At: *st.FencedAt, ByEpoch: st.FencedByEpoch}
	}
	out.Recovery = RecoveryDrill{VerifiedAt: st.RecoveryVerifiedAt, Due: st.RecoveryVerifiedAt == nil || s.now().Sub(*st.RecoveryVerifiedAt) > RecoveryDrillInterval}
	return out, rows.Err()
}

// authorityAlerts derives the alerts of the authority operations.
func (s *Service) authorityAlerts(st State, now, since time.Time) []Alert {
	out := []Alert{}
	if st.State != "active" {
		return out
	}
	if st.FencedAt != nil && now.Sub(*st.FencedAt) < 7*24*time.Hour {
		out = append(out, Alert{Code: "authority_fenced", Severity: "warning",
			Message: "this instance was the federation authority and learned of epoch " + itoa(st.FencedByEpoch) + "; federation administration here is read-only, republish any edits made after the epoch change on the new authority"})
	}
	if st.Role == "authority" && st.HandoverTo != "" && st.HandoverStartedAt != nil && now.Sub(*st.HandoverStartedAt) < handoverTTL {
		out = append(out, Alert{Code: "handover_pending", Severity: "warning", Message: "an authority handover is in progress; federation publishing is paused until it completes or is aborted"})
	}
	if st.Role == "authority" && ((st.RecoveryVerifiedAt == nil && !since.IsZero() && now.Sub(since) > 30*24*time.Hour) || (st.RecoveryVerifiedAt != nil && now.Sub(*st.RecoveryVerifiedAt) > RecoveryDrillInterval)) {
		out = append(out, Alert{Code: "recovery_key_unverified", Severity: "warning",
			Message: "possession of the offline recovery key has not been proven in the last 180 days (run the recovery-key drill)"})
	}
	return out
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// --- authority state guard ---

// authorityStateAllowPending is authorityState without the handover guard.
func (s *Service) authorityStateAllowPending(ctx context.Context, tx pgx.Tx) (State, *Membership, error) {
	st, err := lockState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return st, nil, err
	}
	if st.State != "active" || st.FederationID == "" {
		return st, nil, ErrNotConfigured
	}
	if st.Role != "authority" {
		return st, nil, ErrNotAuthority
	}
	applied, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	return st, applied, err
}

func (s *Service) handoverActive(st State) bool {
	return st.HandoverTo != "" && st.HandoverStartedAt != nil && s.now().Sub(*st.HandoverStartedAt) < handoverTTL
}

// --- planned handover: current authority ---

// HandoverResult reports a completed handover.
type HandoverResult struct {
	Status Status `json:"status"`
	Epoch  int64  `json:"epoch"`
	// Applied is false when the new authority published the epoch but this
	// instance has not applied it yet (it will, on the next sync).
	Applied bool `json:"applied"`
}

type handoverPrepareRequest struct {
	Epoch              int64  `json:"epoch"`
	BaseGeneration     int64  `json:"base_generation"`
	BaseHash           string `json:"base_hash"`
	PolicyGeneration   int64  `json:"policy_generation"`
	PolicyHash         string `json:"policy_hash"`
	TopologyGeneration int64  `json:"topology_generation"`
	TopologyHash       string `json:"topology_hash"`
}

type handoverPrepareResponse struct {
	PublicKey string `json:"public_key"`
	Epoch     int64  `json:"epoch"`
}

type handoverCommitRequest struct {
	BaseGeneration int64         `json:"base_generation"`
	BaseHash       string        `json:"base_hash"`
	Authority      AuthorityCert `json:"authority"`
	// CA is the federation CA (key and certificate), sent over the mutually
	// authenticated channel to the new authority, which becomes the issuer.
	CA caMaterial `json:"ca"`
}

// Handover transfers the authority role to another active member (planned
// epoch change). It runs on the current authority; confirm is checked by the
// caller. Publishing on this authority is paused from the start of the
// handover until the new epoch is applied, aborted or the pause expires.
func (s *Service) Handover(ctx context.Context, actor, toMember string) (HandoverResult, error) {
	if !uuidPattern.MatchString(toMember) {
		return HandoverResult{}, invalid("to_member_id must be a member instance ID")
	}
	var st State
	var applied *Membership
	var target Member
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		st, applied, err = s.authorityStateAllowPending(ctx, tx)
		if err != nil {
			return err
		}
		if s.handoverActive(st) {
			return ErrHandoverPending
		}
		if toMember == s.cfg.InstanceID {
			return ErrSelf
		}
		var ok bool
		if target, ok = applied.Member(toMember); !ok || target.Status != "active" {
			return ErrNotFound
		}
		if target.Protocol.Max < ProtocolMin || target.Protocol.Min > ProtocolMax {
			return ErrProtocol
		}
		topo, err := loadTopologyState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if topo.Generation > 0 {
			if ts, terr := s.topologyStatus(ctx, st); terr == nil && ts.Transfers > 0 {
				return ErrTransferActive
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET handover_to=$2,handover_base_generation=$3,handover_started_at=clock_timestamp(),revision=revision+1 WHERE instance_id=$1`,
			s.cfg.InstanceID, toMember, applied.Generation); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.authority.handover_started", toMember, "success", map[string]any{"to": target.Name, "epoch": applied.AuthorityEpoch + 1,
			"base_generation": applied.Generation})
	})
	if err != nil {
		return HandoverResult{}, err
	}
	abandon := func(reason string) {
		_ = s.clearHandover(context.WithoutCancel(ctx), actor, toMember, reason)
	}
	id, _, err := s.identity()
	if err != nil {
		abandon("identity")
		return HandoverResult{}, err
	}
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		abandon("state")
		return HandoverResult{}, err
	}
	ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		abandon("state")
		return HandoverResult{}, err
	}
	epoch := applied.AuthorityEpoch + 1
	body, _ := json.Marshal(handoverPrepareRequest{Epoch: epoch, BaseGeneration: applied.Generation, BaseHash: Hash(applied), PolicyGeneration: ps.Generation, PolicyHash: ps.Hash,
		TopologyGeneration: ts.Generation, TopologyHash: ts.Hash})
	var prepared handoverPrepareResponse
	// The target may be a moment behind a document that was just published:
	// nudge it and retry briefly before reporting it as not current.
	for attempt := 0; ; attempt++ {
		raw, perr := s.client.Do(ctx, id, target.FederationURL, "POST", "/federation/v1/authority/prepare", body, target.InstanceID)
		if perr == nil {
			if err = json.Unmarshal(raw, &prepared); err != nil {
				abandon("malformed")
				return HandoverResult{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
			}
			break
		}
		var pe *peer.ErrPeer
		if errors.As(perr, &pe) && pe.Code == "member_not_current" && attempt < 4 {
			go s.nudgeMembers(context.WithoutCancel(ctx))
			select {
			case <-time.After(time.Duration(attempt+1) * time.Second):
			case <-ctx.Done():
				abandon("cancelled")
				return HandoverResult{}, ctx.Err()
			}
			continue
		}
		abandon("prepare_failed")
		return HandoverResult{}, peerError(target.FederationURL, perr)
	}
	if _, err := decodeKey(prepared.PublicKey); err != nil || prepared.Epoch != epoch {
		abandon("malformed")
		return HandoverResult{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	// Certify the target's key with this epoch's key, under the state lock, and
	// make sure nothing was published since the handover started.
	var commit handoverCommitRequest
	err = s.tx(ctx, func(tx pgx.Tx) error {
		cur, doc, err := s.authorityStateAllowPending(ctx, tx)
		if err != nil {
			return err
		}
		if cur.HandoverTo != toMember || doc.Generation != applied.Generation || Hash(doc) != Hash(applied) {
			return ErrHandoverPending
		}
		_, seed, err := s.store.Open(ctx, s.cfg.InstanceID, cur.AuthoritySecretID, authorityPurpose(doc.AuthorityEpoch))
		if err != nil {
			return err
		}
		if len(seed) != ed25519.SeedSize {
			clear(seed)
			return errors.New("federation: authority key invalid")
		}
		key := ed25519.NewKeyFromSeed(seed)
		clear(seed)
		cert := AuthorityCert{Epoch: epoch, InstanceID: toMember, PublicKey: prepared.PublicKey, CertifiedBy: "previous_epoch"}
		cert.Signature = sign(key, authorityStatement(doc.FederationID, cert))
		clear(key)
		_, caPlain, err := s.store.Open(ctx, s.cfg.InstanceID, cur.CASecretID, "federation-ca")
		if err != nil {
			return err
		}
		err = json.Unmarshal(caPlain, &commit.CA)
		clear(caPlain)
		if err != nil {
			return err
		}
		commit.BaseGeneration, commit.BaseHash, commit.Authority = doc.Generation, Hash(doc), cert
		return nil
	})
	if err != nil {
		abandon("certify_failed")
		return HandoverResult{}, err
	}
	body, _ = json.Marshal(commit)
	clear(commit.CA.KeyDER)
	raw, err := s.client.Do(ctx, id, target.FederationURL, "POST", "/federation/v1/authority/commit", body, target.InstanceID)
	if err != nil {
		// The target may have committed although the answer was lost: the
		// handover marker stays (publishing paused) and the next sync applies
		// the new epoch. An operator can abort after the pause.
		s.logOnce("handover-commit", "authority handover commit did not complete", "reason", err)
		return HandoverResult{}, peerError(target.FederationURL, err)
	}
	var resp struct {
		Documents []Membership `json:"documents"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Documents) == 0 {
		return HandoverResult{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	out := HandoverResult{Epoch: epoch}
	if _, err := s.applyDocuments(ctx, resp.Documents, target.Name, false); err != nil {
		s.logOnce("handover-apply", "the new authority published the epoch; applying it here failed and will be retried by sync", "reason", err)
	} else {
		out.Applied = true
	}
	out.Status, err = s.Status(ctx)
	return out, err
}

func (s *Service) clearHandover(ctx context.Context, actor, member, reason string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if st.HandoverTo == "" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET handover_to=NULL,handover_base_generation=NULL,handover_started_at=NULL,revision=revision+1 WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.authority.handover_aborted", member, "success", map[string]any{"reason": reason})
	})
}

// AbortHandover clears a handover marker so this authority publishes again. If
// the target already published the new epoch, the next sync still applies it
// and fences this instance (the highest epoch wins).
func (s *Service) AbortHandover(ctx context.Context, actor string) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	if st.State != "active" {
		return ErrNotConfigured
	}
	if st.Role != "authority" {
		return ErrNotAuthority
	}
	if st.HandoverTo == "" {
		return ErrNoHandover
	}
	if err := s.clearHandover(ctx, actor, st.HandoverTo, "operator"); err != nil {
		return err
	}
	return nil
}

// --- planned handover: target member (peer endpoints) ---

// currentPeerState loads and checks the target-side state for a handover call:
// the caller must be the authority of the applied membership and this instance
// an active plain member that has applied the same documents.
func (s *Service) currentPeerState(ctx context.Context, tx pgx.Tx, caller string, baseGen int64, baseHash string) (State, *Membership, error) {
	st, err := lockState(ctx, tx, s.cfg.InstanceID)
	if err != nil {
		return st, nil, err
	}
	if st.State != "active" || st.FederationID == "" || st.AppliedGeneration == 0 {
		return st, nil, ErrNotConfigured
	}
	if st.Role != "member" {
		return st, nil, ErrIsAuthority
	}
	doc, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return st, nil, err
	}
	if doc.Authority.InstanceID != caller {
		return st, nil, ErrNotAuthority
	}
	if self, ok := doc.Member(s.cfg.InstanceID); !ok || self.Status != "active" {
		return st, nil, ErrNotConfigured
	}
	if st.AppliedGeneration != baseGen || st.AppliedHash != baseHash {
		return st, nil, ErrMemberNotCurrent
	}
	return st, doc, nil
}

func (s *Service) peerAuthorityPrepare(w http.ResponseWriter, r *http.Request) {
	caller, _ := peer.Caller(r)
	var req handoverPrepareRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var pub string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, doc, err := s.currentPeerState(ctx, tx, caller.InstanceID, req.BaseGeneration, req.BaseHash)
		if err != nil {
			return err
		}
		if req.Epoch != doc.AuthorityEpoch+1 {
			return invalid("epoch must follow the applied epoch")
		}
		ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		ts, err := loadTopologyState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if ps.Generation != req.PolicyGeneration || ps.Hash != req.PolicyHash || ts.Generation != req.TopologyGeneration || ts.Hash != req.TopologyHash {
			return ErrMemberNotCurrent
		}
		pub, err = s.newPendingAuthorityKey(ctx, tx, st, "", req.Epoch)
		if err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.authority.handover_prepared", caller.InstanceID, "success", map[string]any{"epoch": req.Epoch, "key_fingerprint": KeyFingerprint(pub)})
	})
	if err != nil {
		peerErr(w, err)
		return
	}
	peerJSON(w, http.StatusOK, handoverPrepareResponse{PublicKey: pub, Epoch: req.Epoch})
}

// newPendingAuthorityKey generates (or, for the same epoch, returns) the
// authority key this instance would use at epoch. The seed is stored encrypted
// under the epoch's purpose; any earlier pending key is deleted.
func (s *Service) newPendingAuthorityKey(ctx context.Context, tx pgx.Tx, st State, actor string, epoch int64) (string, error) {
	if st.PendingAuthoritySecretID != "" && st.PendingAuthorityEpoch == epoch {
		if _, seed, err := s.store.Open(ctx, s.cfg.InstanceID, st.PendingAuthoritySecretID, authorityPurpose(epoch)); err == nil && len(seed) == ed25519.SeedSize {
			pub := encodeKey(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
			clear(seed)
			return pub, nil
		}
	}
	if st.PendingAuthoritySecretID != "" {
		if _, err := tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, st.PendingAuthoritySecretID); err != nil {
			return "", err
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	rec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, actor, authorityPurpose(epoch), priv.Seed())
	clear(priv)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE federation_state SET pending_authority_secret_id=$2,pending_authority_epoch=$3,revision=revision+1 WHERE instance_id=$1`, s.cfg.InstanceID, rec.ID, epoch); err != nil {
		return "", err
	}
	return encodeKey(pub), nil
}

// pendingAuthorityKey opens the pending authority key for an epoch.
func (s *Service) pendingAuthorityKey(ctx context.Context, st State, epoch int64) (ed25519.PrivateKey, error) {
	if st.PendingAuthoritySecretID == "" || st.PendingAuthorityEpoch != epoch {
		return nil, invalid("no pending authority key for this epoch; request a new one")
	}
	_, seed, err := s.store.Open(ctx, s.cfg.InstanceID, st.PendingAuthoritySecretID, authorityPurpose(epoch))
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		clear(seed)
		return nil, errors.New("federation: authority key invalid")
	}
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return key, nil
}

func (s *Service) peerAuthorityCommit(w http.ResponseWriter, r *http.Request) {
	caller, _ := peer.Caller(r)
	var req handoverCommitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var docs []Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.currentPeerState(ctx, tx, caller.InstanceID, req.BaseGeneration, req.BaseHash)
		if err != nil {
			return err
		}
		cert := req.Authority
		epoch := applied.AuthorityEpoch + 1
		if cert.Epoch != epoch || cert.InstanceID != s.cfg.InstanceID || cert.CertifiedBy != "previous_epoch" ||
			!verify(applied.Authority.PublicKey, cert.Signature, authorityStatement(applied.FederationID, cert)) {
			return &RejectedError{Reason: "bad_authority_certificate", Err: ErrAuthorityCert}
		}
		key, err := s.pendingAuthorityKey(ctx, st, epoch)
		if err != nil {
			return err
		}
		defer clear(key)
		if encodeKey(key.Public().(ed25519.PublicKey)) != cert.PublicKey {
			return &RejectedError{Reason: "bad_authority_certificate", Err: ErrAuthorityCert}
		}
		// The CA key must belong to a CA the membership already trusts.
		caKey, caCert, err := req.CA.parse()
		if err != nil {
			return invalid("invalid CA material")
		}
		defer clear(req.CA.KeyDER)
		trusted := false
		for _, pemCert := range applied.CACertificates {
			if c, perr := parseCertPEM(pemCert); perr == nil && CertSHA256(c.Raw) == CertSHA256(caCert.Raw) {
				trusted = true
			}
		}
		if !trusted || !caCert.IsCA || caKey.PublicKey.Equal(caCert.PublicKey) == false {
			return invalid("CA material is not a CA of this federation")
		}
		mat, err := s.openMember(ctx, st)
		if err != nil {
			return err
		}
		_, signer, err := mat.parse()
		clear(mat.TLSKeyDER)
		clear(mat.SigningKey)
		if err != nil {
			return err
		}
		cert.Acceptance = sign(signer, acceptanceStatement(applied.FederationID, cert))
		clear(signer)
		next := applied.Clone()
		next.Generation, next.PrevHash, next.AuthorityEpoch = applied.Generation+1, Hash(applied), epoch
		next.IssuedAt, next.IssuedBy = s.now().Format(time.RFC3339Nano), "handover:"+caller.InstanceID
		next.Authority = cert
		if err := Sign(next, key); err != nil {
			return err
		}
		if err := Verify(next, applied); err != nil {
			return &RejectedError{Reason: RejectReason(err), Err: err}
		}
		caRec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, "", "federation-ca", req.CA.marshal())
		if err != nil {
			return err
		}
		if err := storeDocument(ctx, tx, s.cfg.InstanceID, next); err != nil {
			return err
		}
		self, _ := next.Member(s.cfg.InstanceID)
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET role='authority',authority_secret_id=$2,ca_secret_id=$3,ca_sha256=$4,authority_url=$5,pending_authority_secret_id=NULL,pending_authority_epoch=NULL,
fenced_at=NULL,fenced_by_epoch=NULL,handover_to=NULL,handover_base_generation=NULL,handover_started_at=NULL,revision=revision+1 WHERE instance_id=$1`,
			s.cfg.InstanceID, st.PendingAuthoritySecretID, caRec.ID, CertSHA256(caCert.Raw), self.FederationURL); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.authority.epoch_changed", applied.FederationID, "success", map[string]any{"mode": "handover", "from_epoch": applied.AuthorityEpoch,
			"epoch": epoch, "previous_authority": caller.InstanceID, "generation": next.Generation, "key_fingerprint": KeyFingerprint(cert.PublicKey)}); err != nil {
			return err
		}
		docs = []Membership{*next}
		return nil
	})
	if err != nil {
		peerErr(w, err)
		return
	}
	s.afterPublish(ctx, &docs[0], caller.InstanceID)
	peerJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// --- epoch change application (every member) ---

// applyEpochChange updates this instance's role and keys when the applied
// membership moved to a higher authority epoch. st is the locked state before
// the change, prev the previously applied document and doc the new head. It
// runs inside the transaction that stores doc.
func (s *Service) applyEpochChange(ctx context.Context, tx pgx.Tx, st State, prev, doc *Membership) error {
	newAuthority, _ := doc.Member(doc.Authority.InstanceID)
	if _, err := tx.Exec(ctx, `UPDATE federation_state SET authority_url=$2 WHERE instance_id=$1`, s.cfg.InstanceID, newAuthority.FederationURL); err != nil {
		return err
	}
	details := map[string]any{"from_epoch": prev.AuthorityEpoch, "epoch": doc.AuthorityEpoch, "authority": doc.Authority.InstanceID, "generation": doc.Generation,
		"certified_by": doc.Authority.CertifiedBy, "key_fingerprint": KeyFingerprint(doc.Authority.PublicKey)}
	if st.Role == "authority" && doc.Authority.InstanceID != s.cfg.InstanceID {
		// A planned handover is the authority's own decision; only a forced
		// recovery (certified by the recovery key) is reported as being fenced.
		// Fence: this instance is no longer the authority. Its epoch key and the
		// CA key are destroyed (a retired signer must not stay usable), pending
		// invitations are revoked, and federation administration becomes read-only.
		for _, id := range []string{st.AuthoritySecretID, st.CASecretID} {
			if id != "" {
				if _, err := tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, id); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET role='member',authority_secret_id=NULL,ca_secret_id=NULL,fenced_at=CASE WHEN $3 THEN clock_timestamp() END,fenced_by_epoch=CASE WHEN $3 THEN $2::bigint END,
handover_to=NULL,handover_base_generation=NULL,handover_started_at=NULL,revision=revision+1 WHERE instance_id=$1`, s.cfg.InstanceID, doc.AuthorityEpoch, doc.Authority.CertifiedBy == "recovery"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_invitations SET revoked_at=clock_timestamp() WHERE instance_id=$1 AND federation_id=$2 AND consumed_at IS NULL AND revoked_at IS NULL`,
			s.cfg.InstanceID, st.FederationID); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.authority.fenced", st.FederationID, "success", details); err != nil {
			return err
		}
	}
	if st.PendingAuthoritySecretID != "" && st.PendingAuthorityEpoch <= doc.AuthorityEpoch && doc.Authority.InstanceID != s.cfg.InstanceID {
		if _, err := tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, st.PendingAuthoritySecretID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET pending_authority_secret_id=NULL,pending_authority_epoch=NULL WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
			return err
		}
	}
	return audit(ctx, tx, s.cfg.InstanceID, "", "federation.authority.epoch_changed", st.FederationID, "success", details)
}

// followEpoch brings the local chain to the higher epoch a peer reports:
// replacing the divergent segment when local generations were issued by a
// lower epoch, or applying the documents when the local chain is a prefix.
func (s *Service) followEpoch(ctx context.Context, id peer.Identity, src Member) error {
	before := int64(0)
	if t := s.trust.Load(); t.doc != nil {
		before = t.doc.AuthorityEpoch
	}
	err := s.resyncMembership(ctx, id, src, true)
	if errors.Is(err, errNoDivergence) {
		st, lerr := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
		if lerr != nil {
			return lerr
		}
		docs, ferr := s.fetchMembership(ctx, id, src, st.AppliedGeneration)
		if ferr != nil {
			return ferr
		}
		if len(docs) == 0 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		_, err = s.applyDocuments(ctx, docs, src.Name, false)
	}
	if err != nil {
		return err
	}
	if t := s.trust.Load(); t.doc == nil || t.doc.AuthorityEpoch <= before {
		return &RejectedError{Reason: "stale_epoch", Err: ErrStaleEpoch}
	}
	return nil
}

// detectFence runs on the authority: when any active member reports a higher
// authority epoch, this instance follows it (and fences itself).
func (s *Service) detectFence(ctx context.Context, st State) error {
	id, t, err := s.identity()
	if err != nil {
		return err
	}
	var others []Member
	for _, m := range t.doc.Members {
		if m.Status == "active" && m.InstanceID != s.cfg.InstanceID {
			others = append(others, m)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	for _, m := range others {
		raw, err := s.client.Do(ctx, id, m.FederationURL, "GET", "/federation/v1/heads", nil, m.InstanceID)
		if err != nil {
			continue
		}
		var h heads
		if json.Unmarshal(raw, &h) != nil || h.FederationID != st.FederationID || h.Membership.AuthorityEpoch <= t.doc.AuthorityEpoch {
			continue
		}
		if err := s.followEpoch(ctx, id, m); err != nil {
			s.logOnce("fence:"+m.InstanceID, "a member reports a higher authority epoch that could not be followed", "member", m.Name, "reason", err)
			continue
		}
		return nil
	}
	return nil
}

// --- forced recovery ---

// RecoverInput is the signed request of a forced recovery.
type RecoverInput struct {
	AuthorityCertificate AuthorityCert `json:"authority_certificate"`
	// RemovePreviousAuthority removes the previous authority from the
	// membership and revokes its certificate (key compromise).
	RemovePreviousAuthority bool `json:"remove_previous_authority"`
}

// RecoverResult reports a forced recovery.
type RecoverResult struct {
	Status          Status `json:"status"`
	Epoch           int64  `json:"epoch"`
	CASHA256        string `json:"ca_sha256"`
	PreviousRemoved bool   `json:"previous_authority_removed"`
}

// RecoveryEpochTemplate returns the public input the recovery-key holder signs
// offline (`ant federation recovery sign-authority-cert`) to make this member
// the authority of the next epoch. The authority key it names is generated
// here and stays pending until the certified request arrives.
func (s *Service) RecoveryEpochTemplate(ctx context.Context, actor string) (RecoveryTemplate, error) {
	var out RecoveryTemplate
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if st.State != "active" || st.FederationID == "" || st.AppliedGeneration == 0 {
			return ErrNotConfigured
		}
		if st.Role == "authority" {
			return ErrIsAuthority
		}
		doc, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
		if err != nil {
			return err
		}
		if self, ok := doc.Member(s.cfg.InstanceID); !ok || self.Status != "active" {
			return ErrNotConfigured
		}
		epoch := doc.AuthorityEpoch + 1
		pub, err := s.newPendingAuthorityKey(ctx, tx, st, actor, epoch)
		if err != nil {
			return err
		}
		out = RecoveryTemplate{FederationID: doc.FederationID, Generation: doc.Generation + 1, PreviousKey: doc.RecoveryPublicKey,
			Authority: AuthorityCert{Epoch: epoch, InstanceID: s.cfg.InstanceID, PublicKey: pub, CertifiedBy: "recovery"}}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.authority.recovery_template", doc.FederationID, "success", map[string]any{"epoch": epoch, "key_fingerprint": KeyFingerprint(pub)})
	})
	return out, err
}

// RecoverAuthority makes this member the authority of the next epoch from a
// certificate signed by the offline recovery key. It creates a new federation
// CA (the old CA key is not available here), issues this member's certificate
// from it, publishes the first document of the new epoch and takes the role.
func (s *Service) RecoverAuthority(ctx context.Context, actor string, in RecoverInput) (RecoverResult, error) {
	if s.cfg.Keys == nil {
		return RecoverResult{}, ErrKeysUnavailable
	}
	var doc *Membership
	var caSHA string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if st.State != "active" || st.FederationID == "" || st.AppliedGeneration == 0 {
			return ErrNotConfigured
		}
		if st.Role == "authority" {
			return ErrIsAuthority
		}
		applied, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
		if err != nil {
			return err
		}
		self, ok := applied.Member(s.cfg.InstanceID)
		if !ok || self.Status != "active" {
			return ErrNotConfigured
		}
		cert := in.AuthorityCertificate
		epoch := applied.AuthorityEpoch + 1
		if cert.Epoch != epoch || cert.InstanceID != s.cfg.InstanceID || cert.CertifiedBy != "recovery" {
			return invalid("authority_certificate must certify this member at the next epoch")
		}
		if !verify(applied.RecoveryPublicKey, cert.Signature, authorityStatement(applied.FederationID, cert)) {
			return ErrRecoveryMismatch
		}
		key, err := s.pendingAuthorityKey(ctx, st, epoch)
		if err != nil {
			return err
		}
		defer clear(key)
		if encodeKey(key.Public().(ed25519.PublicKey)) != cert.PublicKey {
			return invalid("the certificate names another authority key than the one generated here; request a new template")
		}
		now := s.now()
		mat, err := s.openMember(ctx, st)
		if err != nil {
			return err
		}
		_, signer, err := mat.parse()
		clear(mat.TLSKeyDER)
		clear(mat.SigningKey)
		if err != nil {
			return err
		}
		cert.Acceptance = sign(signer, acceptanceStatement(applied.FederationID, cert))
		clear(signer)
		ca, caCert, err := newCA(applied.FederationName, now)
		if err != nil {
			return err
		}
		defer clear(ca.KeyDER)
		next := applied.Clone()
		next.Generation, next.PrevHash, next.AuthorityEpoch = applied.Generation+1, Hash(applied), epoch
		next.IssuedAt, next.IssuedBy = now.Format(time.RFC3339Nano), "recovery:"+actor
		next.Authority = cert
		// Both CAs are trusted so existing member certificates keep working
		// until they renew from the new CA; a third CA displaces the oldest.
		next.CACertificates = append(next.CACertificates, ca.CertPEM)
		if len(next.CACertificates) > 2 {
			next.CACertificates = next.CACertificates[len(next.CACertificates)-2:]
		}
		if in.RemovePreviousAuthority && applied.Authority.InstanceID != s.cfg.InstanceID {
			for i := range next.Members {
				m := &next.Members[i]
				if m.InstanceID != applied.Authority.InstanceID {
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
		}
		if err := Sign(next, key); err != nil {
			return err
		}
		if err := Verify(next, applied); err != nil {
			return &RejectedError{Reason: RejectReason(err), Err: err}
		}
		caRec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, actor, "federation-ca", ca.marshal())
		if err != nil {
			return err
		}
		if err := storeDocument(ctx, tx, s.cfg.InstanceID, next); err != nil {
			return err
		}
		caSHA = CertSHA256(caCert.Raw)
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET role='authority',authority_secret_id=$2,ca_secret_id=$3,ca_sha256=$4,authority_url=$5,
pending_authority_secret_id=NULL,pending_authority_epoch=NULL,fenced_at=NULL,fenced_by_epoch=NULL,handover_to=NULL,handover_base_generation=NULL,handover_started_at=NULL,revision=revision+1 WHERE instance_id=$1`,
			s.cfg.InstanceID, st.PendingAuthoritySecretID, caRec.ID, caSHA, self.FederationURL); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.authority.recovered", applied.FederationID, "success", map[string]any{"from_epoch": applied.AuthorityEpoch, "epoch": epoch,
			"previous_authority": applied.Authority.InstanceID, "previous_removed": in.RemovePreviousAuthority, "generation": next.Generation, "ca_sha256": caSHA,
			"key_fingerprint": KeyFingerprint(cert.PublicKey)}); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.authority.epoch_changed", applied.FederationID, "success", map[string]any{"mode": "recovery", "from_epoch": applied.AuthorityEpoch,
			"epoch": epoch, "previous_authority": applied.Authority.InstanceID, "generation": next.Generation}); err != nil {
			return err
		}
		doc = next
		return nil
	})
	if err != nil {
		return RecoverResult{}, err
	}
	s.afterPublish(ctx, doc)
	out := RecoverResult{Epoch: doc.AuthorityEpoch, CASHA256: caSHA, PreviousRemoved: in.RemovePreviousAuthority}
	out.Status, err = s.Status(ctx)
	return out, err
}

// --- recovery-key drill ---

// RecoveryChallenge is a one-time statement the recovery-key holder signs
// offline to prove possession without changing anything.
type RecoveryChallenge struct {
	FederationID string    `json:"federation_id"`
	Challenge    string    `json:"challenge"`
	ExpiresAt    time.Time `json:"expires_at"`
	PublicKey    string    `json:"recovery_public_key"`
}

func recoveryDrillStatement(federationID, challenge string) []byte {
	b, _ := CanonicalJSON(map[string]any{"type": "recovery-drill", "federation_id": federationID, "challenge": challenge})
	return b
}

// NewRecoveryChallenge issues a drill challenge (authority only, valid for one hour).
func (s *Service) NewRecoveryChallenge(ctx context.Context, actor string) (RecoveryChallenge, error) {
	var out RecoveryChallenge
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		raw := make([]byte, 24)
		_, _ = rand.Read(raw)
		out = RecoveryChallenge{FederationID: applied.FederationID, Challenge: base64.RawURLEncoding.EncodeToString(raw), ExpiresAt: s.now().Add(time.Hour), PublicKey: applied.RecoveryPublicKey}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET recovery_challenge=$2,recovery_challenge_expires_at=$3 WHERE instance_id=$1`, s.cfg.InstanceID, out.Challenge, out.ExpiresAt); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.recovery_key.challenge_issued", st.FederationID, "success", map[string]any{"expires_at": out.ExpiresAt})
	})
	return out, err
}

// SignRecoveryChallenge signs a drill challenge with the recovery key. It runs
// offline.
func SignRecoveryChallenge(c RecoveryChallenge, recoveryKey string) (string, error) {
	key, err := parseRecovery(recoveryKey)
	if err != nil {
		return "", err
	}
	defer clear(key)
	if encodeKey(key.Public().(ed25519.PublicKey)) != c.PublicKey {
		return "", ErrRecoveryMismatch
	}
	return sign(key, recoveryDrillStatement(c.FederationID, c.Challenge)), nil
}

// VerifyRecoveryChallenge records that the recovery key was proven. The
// challenge is consumed whether or not the signature is valid.
func (s *Service) VerifyRecoveryChallenge(ctx context.Context, actor, challenge, signature string) (AuthorityStatus, error) {
	var bad bool
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		if st.RecoveryChallenge == "" || st.RecoveryChallenge != challenge || st.RecoveryChallengeExpires == nil || !st.RecoveryChallengeExpires.After(s.now()) {
			return invalid("no such challenge, or it expired; request a new one")
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET recovery_challenge='',recovery_challenge_expires_at=NULL WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
			return err
		}
		if !verify(applied.RecoveryPublicKey, signature, recoveryDrillStatement(applied.FederationID, challenge)) {
			bad = true
			return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.recovery_key.drill", st.FederationID, "denied", map[string]any{"fingerprint": KeyFingerprint(applied.RecoveryPublicKey)})
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET recovery_verified_at=clock_timestamp(),revision=revision+1 WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.recovery_key.drill", st.FederationID, "success", map[string]any{"fingerprint": KeyFingerprint(applied.RecoveryPublicKey)})
	})
	if err != nil {
		return AuthorityStatus{}, err
	}
	if bad {
		return AuthorityStatus{}, ErrRecoveryProof
	}
	return s.AuthorityStatus(ctx)
}

var _ = x509.ErrUnsupportedAlgorithm

// operationAlerts are the alerts that need the database beyond the policy and
// topology state: authority operations and member reachability.
func (s *Service) operationAlerts(ctx context.Context, st State) []Alert {
	var since time.Time
	if st.FederationID != "" {
		_ = s.cfg.DB.QueryRow(ctx, `SELECT min(received_at) FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind='membership'`, s.cfg.InstanceID, st.FederationID).Scan(&since)
	}
	out := s.authorityAlerts(st, s.now(), since)
	if st.State != "active" || st.FederationID == "" {
		return out
	}
	rows, err := s.cfg.DB.Query(ctx, `SELECT name,last_seen_at FROM federation_members WHERE instance_id=$1 AND federation_id=$2 AND status='active' AND member_instance_id<>$1
AND coalesce(joined_at,'epoch')<$3 ORDER BY name`, s.cfg.InstanceID, st.FederationID, s.now().Add(-StaleWarning))
	if err != nil {
		return out
	}
	defer rows.Close()
	var down []string
	critical := false
	for rows.Next() {
		var name string
		var seen *time.Time
		if rows.Scan(&name, &seen) != nil {
			return out
		}
		if seen == nil || s.now().Sub(*seen) > StaleWarning {
			down = append(down, name)
			critical = critical || seen == nil || s.now().Sub(*seen) > StaleCritical
		}
	}
	if len(down) > 0 {
		sev := "warning"
		if critical {
			sev = "critical"
		}
		shown := down
		if len(shown) > 5 {
			shown = shown[:5]
		}
		out = append(out, Alert{Code: "member_unreachable", Severity: sev, Message: "not reachable for over 15 minutes: " + joinNames(shown) + "; see the member-loss runbook"})
	}
	sortAlerts(out)
	return out
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// listenerCAFingerprint is the fingerprint of the CA that issued the
// certificate this instance's listener presents: the pin a joiner needs to
// verify the authority. After a forced recovery the authority holds a new CA
// but keeps presenting its old certificate until it renews (members only learn
// the new CA from the recovery document, so they must still reach the
// authority with the old one).
func listenerCAFingerprint(doc *Membership, st State) string {
	if leaf, err := parseCertPEM(st.MemberCert); err == nil && doc != nil {
		for _, c := range doc.CACertificates {
			if ca, err := parseCertPEM(c); err == nil && leaf.CheckSignatureFrom(ca) == nil {
				return CertSHA256(ca.Raw)
			}
		}
	}
	return st.CASHA256
}
