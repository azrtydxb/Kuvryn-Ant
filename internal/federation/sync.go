package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

// applyDocuments verifies and applies documents in order on top of the
// applied chain. Documents at or below the applied generation that are
// byte-identical to the stored ones are skipped; any other document that fails
// verification (tampered, stale, forged, gap) is rejected, audited and stops
// the batch. The previously applied generation stays in force.
func (s *Service) applyDocuments(ctx context.Context, docs []Membership, source string, leaving bool) (int, error) {
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
			if doc.FederationID == st.FederationID && doc.Generation <= st.AppliedGeneration && doc.Generation > 0 {
				if old, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, doc.Generation); err == nil && Hash(old) == Hash(doc) {
					return nil // already applied
				}
			}
			cur, err := loadDocument(ctx, tx, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
			if err != nil {
				return err
			}
			if err := Verify(doc, cur); err != nil {
				rejectErr = err
				return err
			}
			if err := storeDocument(ctx, tx, s.cfg.InstanceID, doc); err != nil {
				return err
			}
			if self, ok := doc.Member(s.cfg.InstanceID); ok && self.Status == "removed" {
				next := "removed"
				if leaving {
					next = "left"
				}
				if _, err := tx.Exec(ctx, `UPDATE federation_state SET state=$2 WHERE instance_id=$1`, s.cfg.InstanceID, next); err != nil {
					return err
				}
				if err := dropFederatedIdentity(ctx, tx, s.cfg.InstanceID); err != nil {
					return err
				}
			}
			applied++
			return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.applied", st.FederationID, "success",
				map[string]any{"kind": "membership", "generation": doc.Generation, "hash": Hash(doc), "source": source})
		})
		if rejectErr != nil {
			reason := RejectReason(rejectErr)
			s.recordRejection(ctx, doc, source, reason)
			_ = s.Refresh(ctx)
			return applied, &RejectedError{Reason: reason, Err: rejectErr}
		}
		if err != nil {
			_ = s.Refresh(ctx)
			return applied, err
		}
	}
	if applied > 0 {
		if err := s.Refresh(ctx); err != nil {
			s.cfg.Logger.Warn("federation trust refresh failed", "reason", err)
		}
	}
	return applied, nil
}

func (s *Service) recordRejection(ctx context.Context, doc *Membership, source, reason string) {
	ctx = context.WithoutCancel(ctx)
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET last_sync_error=$2 WHERE instance_id=$1`, s.cfg.InstanceID, truncate(fmt.Sprintf("document rejected (%s) from %s", reason, source), 1024)); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.document.rejected", st.FederationID, "denied",
			map[string]any{"kind": "membership", "generation": doc.Generation, "reason": reason, "source": source, "hash": Hash(doc)})
	})
	s.logOnce("reject:"+source+":"+reason, "federation document rejected", "reason", reason, "source", source, "generation", doc.Generation)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ApplyDocuments applies externally obtained documents (tests, relays).
func (s *Service) ApplyDocuments(ctx context.Context, docs []Membership, source string) (int, error) {
	return s.applyDocuments(ctx, docs, source, false)
}

type heads struct {
	FederationID string `json:"federation_id"`
	Membership   struct {
		Generation     int64  `json:"generation"`
		Hash           string `json:"hash"`
		AuthorityEpoch int64  `json:"authority_epoch"`
	} `json:"membership"`
	Policy   policyHead `json:"policy"`
	Topology policyHead `json:"topology"`
}

// SyncOnce fetches newer membership generations, trying the authority first
// and then every other active member (documents are self-authenticating, so
// any member can relay them). It records the outcome in federation status and
// never affects serving.
func (s *Service) SyncOnce(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.State != "active" || st.Role == "authority" {
		return err
	}
	id, t, err := s.identity()
	if err != nil {
		s.recordSync(ctx, "", err)
		return err
	}
	sources := []Member{}
	if a, ok := t.doc.Member(t.doc.Authority.InstanceID); ok {
		sources = append(sources, a)
	}
	others := []Member{}
	for _, m := range t.doc.Members {
		if m.Status == "active" && m.InstanceID != s.cfg.InstanceID && m.InstanceID != t.doc.Authority.InstanceID {
			others = append(others, m)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	sources = append(sources, others...)
	var first error
	for _, src := range sources {
		err := s.syncFrom(ctx, id, src)
		if err == nil {
			s.recordSync(ctx, src.Name, nil)
			return nil
		}
		if first == nil {
			first = fmt.Errorf("%s: %w", src.Name, err)
		}
		var rej *RejectedError
		if errors.As(err, &rej) {
			continue
		}
	}
	if first == nil {
		first = errors.New("no reachable member")
	}
	s.recordSync(ctx, "", first)
	return first
}

func (s *Service) syncFrom(ctx context.Context, id peer.Identity, src Member) error {
	resynced := false
	for range 1000 {
		st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if st.State != "active" {
			return nil
		}
		raw, err := s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/heads", nil, src.InstanceID)
		if err != nil {
			return err
		}
		var h heads
		if err := json.Unmarshal(raw, &h); err != nil || h.FederationID != st.FederationID {
			return &RejectedError{Reason: "wrong_federation", Err: ErrWrongFed}
		}
		authority := src.InstanceID == s.trust.Load().authorityID()
		if authority && !resynced && h.Membership.Generation == st.AppliedGeneration && h.Membership.Hash != st.AppliedHash {
			resynced = true
			// Same generation, another document: the chains diverged.
			if err := s.resyncMembership(ctx, id, src); err != nil && !errors.Is(err, errNoDivergence) {
				return err
			}
			continue
		}
		if h.Membership.Generation <= st.AppliedGeneration {
			// Membership is current: bring policy and topology up to this source.
			if err := s.syncPolicyFrom(ctx, id, src, h.Policy, authority); err != nil {
				return err
			}
			return s.syncTopologyFrom(ctx, id, src, h.Topology)
		}
		raw, err = s.client.Do(ctx, id, src.FederationURL, "GET", "/federation/v1/documents/membership?after="+strconv.FormatInt(st.AppliedGeneration, 10)+"&limit=100", nil, src.InstanceID)
		if err != nil {
			return err
		}
		var docs struct {
			Documents []Membership `json:"documents"`
		}
		if err := json.Unmarshal(raw, &docs); err != nil {
			return &RejectedError{Reason: "malformed", Err: ErrMalformed}
		}
		if len(docs.Documents) == 0 {
			return &RejectedError{Reason: "generation_gap", Err: ErrGap}
		}
		n, err := s.applyDocuments(ctx, docs.Documents, src.Name, false)
		if err != nil && authority && !resynced && isChainMismatch(err) {
			resynced = true
			if rerr := s.resyncMembership(ctx, id, src); rerr != nil && !errors.Is(rerr, errNoDivergence) {
				return rerr
			}
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}

func (s *Service) recordSync(ctx context.Context, source string, err error) {
	msg := ""
	if err != nil {
		msg = truncate(err.Error(), 1024)
	}
	_, _ = s.cfg.DB.Exec(context.WithoutCancel(ctx), `UPDATE federation_state SET last_sync_attempt_at=$2,last_sync_success_at=CASE WHEN $3='' THEN $2 ELSE last_sync_success_at END,
last_sync_error=$3,last_sync_source=CASE WHEN $4<>'' THEN $4 ELSE last_sync_source END WHERE instance_id=$1`, s.cfg.InstanceID, s.now(), msg, source)
}

// Nudge requests an immediate sync (non-blocking).
func (s *Service) Nudge() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Run is the federation worker: document sync, member pings and certificate
// renewal. It runs under the instance writer lease and returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	reconciled := make(chan struct{})
	go func() { s.reconcileLoop(ctx); close(reconciled) }()
	defer func() { <-reconciled }()
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		step, cancel := context.WithTimeout(ctx, s.cfg.PollInterval+30*time.Second)
		if err := s.SyncOnce(step); err != nil && ctx.Err() == nil {
			s.logOnce("sync", "federation sync failed", "reason", err)
		}
		if _, err := s.EvaluateAlerts(step); err != nil && ctx.Err() == nil {
			s.logOnce("alerts", "federation alert evaluation failed", "reason", err)
		}
		s.purgeNonces(step)
		s.pruneLogged()
		s.PingMembers(step)
		if err := s.RenewIfDue(step); err != nil && ctx.Err() == nil {
			s.logOnce("renew", "federation certificate renewal failed", "reason", err)
		}
		if err := s.RetireCertificates(step); err != nil && ctx.Err() == nil {
			s.logOnce("retire", "federation certificate retirement failed", "reason", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.nudge:
		}
	}
}

// PingMembers measures RTT and clock skew to every active member.
func (s *Service) PingMembers(ctx context.Context) {
	id, t, err := s.identity()
	if err != nil || t.state.State != "active" {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, m := range t.doc.Members {
		if m.Status != "active" || m.InstanceID == s.cfg.InstanceID {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m Member) {
			defer wg.Done()
			defer func() { <-sem }()
			start := s.cfg.Now()
			raw, err := s.client.Do(ctx, id, m.FederationURL, "GET", "/federation/v1/status", nil, m.InstanceID)
			if err != nil {
				return
			}
			rtt := s.cfg.Now().Sub(start)
			var ps peerStatus
			if json.Unmarshal(raw, &ps) != nil {
				return
			}
			skew := time.UnixMilli(ps.TimeUnixMS).Sub(start.Add(rtt / 2))
			health, _ := json.Marshal(map[string]any{"membership_generation": ps.Membership.Generation, "software_version": ps.SoftwareVersion, "state": ps.State})
			_, _ = s.cfg.DB.Exec(context.WithoutCancel(ctx), `UPDATE federation_members SET last_seen_at=$4,rtt_ms=$5,skew_ms=$6,health=$7 WHERE instance_id=$1 AND federation_id=$2 AND member_instance_id=$3`,
				s.cfg.InstanceID, t.doc.FederationID, m.InstanceID, s.now(), int32(rtt/time.Millisecond), int32(skew/time.Millisecond), health)
		}(m)
	}
	wg.Wait()
}

// nudgeMembers pushes the latest documents to every active member and the
// listed extra members (for example a member that was just removed, so it
// learns its own removal although its certificate is now revoked).
func (s *Service) nudgeMembers(ctx context.Context, extra ...string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, t, err := s.identity()
	if err != nil || t.state.State != "active" || t.state.Role != "authority" {
		return
	}
	docs, err := documentsAfter(ctx, s.cfg.DB, s.cfg.InstanceID, t.doc.FederationID, max(0, t.doc.Generation-16), 16)
	if err != nil {
		return
	}
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return
	}
	policies, err := policyDocumentsAfter(ctx, s.cfg.DB, s.cfg.InstanceID, t.doc.FederationID, max(0, ps.Generation-16), 16)
	if err != nil {
		return
	}
	ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return
	}
	topologies, err := topologyDocumentsAfter(ctx, s.cfg.DB, s.cfg.InstanceID, t.doc.FederationID, max(0, ts.Generation-16), 16)
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"documents": docs, "policy_documents": policies, "topology_documents": topologies})
	targets := map[string]Member{}
	for _, m := range t.doc.Members {
		if m.InstanceID != s.cfg.InstanceID && (m.Status == "active" || contains(extra, m.InstanceID)) {
			targets[m.InstanceID] = m
		}
	}
	var wg sync.WaitGroup
	for _, m := range targets {
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			if _, err := s.client.Do(ctx, id, m.FederationURL, "POST", "/federation/v1/nudge", body, m.InstanceID); err != nil {
				s.logOnce("nudge:"+m.InstanceID, "federation nudge failed", "member", m.Name, "reason", err)
			}
		}(m)
	}
	wg.Wait()
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// RenewIfDue renews this member's certificate after two thirds of its life.
func (s *Service) RenewIfDue(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.State != "active" || st.MemberCertExpiresAt == nil {
		return err
	}
	if st.MemberCertExpiresAt.Sub(s.now()) > memberLifetime/3 {
		return nil
	}
	return s.RenewCertificate(ctx)
}

// RenewCertificate obtains a new member certificate for the existing TLS key:
// from the authority over mTLS, or locally on the authority. The new
// certificate fingerprint is published in the next membership generation.
func (s *Service) RenewCertificate(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	if st.State != "active" {
		return ErrNotConfigured
	}
	mat, err := s.openMember(ctx, st)
	if err != nil {
		return err
	}
	key, _, err := mat.parse()
	clear(mat.TLSKeyDER)
	clear(mat.SigningKey)
	if err != nil {
		return err
	}
	csr, err := newCSR(key, s.cfg.InstanceID, st.FederationURL)
	if err != nil {
		return err
	}
	if st.Role == "authority" {
		var doc *Membership
		err := s.tx(ctx, func(tx pgx.Tx) error {
			st, applied, err := s.authorityState(ctx, tx)
			if err != nil {
				return err
			}
			certPEM, serial, sha, notAfter, err := s.issueFor(ctx, st, csr, s.cfg.InstanceID, st.FederationURL)
			if err != nil {
				return err
			}
			presented := ""
			if cur, err := parseCertPEM(st.MemberCert); err == nil {
				presented = CertSHA256(cur.Raw)
			}
			doc, err = s.publish(ctx, tx, st, applied, "renewal:"+s.cfg.InstanceID, setCert(s.cfg.InstanceID, serial, sha, presented, s.now()))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE federation_state SET member_cert=$2,member_cert_serial=$3,member_cert_expires_at=$4 WHERE instance_id=$1`, s.cfg.InstanceID, certPEM, serial, notAfter); err != nil {
				return err
			}
			return audit(ctx, tx, s.cfg.InstanceID, "", "federation.certificate.renewed", s.cfg.InstanceID, "success", map[string]any{"serial": serial, "generation": doc.Generation})
		})
		if err != nil {
			return err
		}
		s.afterPublish(ctx, doc)
		return nil
	}
	id, t, err := s.identity()
	if err != nil {
		return err
	}
	authority, _ := t.doc.Member(t.doc.Authority.InstanceID)
	body, _ := json.Marshal(map[string]any{"csr": csr})
	raw, err := s.client.Do(ctx, id, authority.FederationURL, "POST", "/federation/v1/csr/renew", body, authority.InstanceID)
	if err != nil {
		return peerError(authority.FederationURL, err)
	}
	var resp JoinResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	cert, err := parseCertPEM(resp.Certificate)
	if err != nil || cert.Subject.CommonName != s.cfg.InstanceID {
		return &RejectedError{Reason: "certificate_mismatch", Err: ErrMalformed}
	}
	if _, err := cert.Verify(x509VerifyOptions(t.pool)); err != nil {
		return &RejectedError{Reason: "certificate_mismatch", Err: err}
	}
	// Apply the generation that publishes the new fingerprint first, then
	// switch to the new certificate.
	if _, err := s.applyDocuments(ctx, resp.Documents, authority.Name, false); err != nil {
		return err
	}
	if err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET member_cert=$2,member_cert_serial=$3,member_cert_expires_at=$4 WHERE instance_id=$1`,
			s.cfg.InstanceID, resp.Certificate, cert.SerialNumber.Text(16), cert.NotAfter); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.certificate.renewed", s.cfg.InstanceID, "success", map[string]any{"serial": cert.SerialNumber.Text(16)})
	}); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

// issueFor signs a member certificate with the federation CA.
func (s *Service) issueFor(ctx context.Context, st State, csr, instance, federationURL string) (string, string, string, time.Time, error) {
	_, caPlain, err := s.store.Open(ctx, s.cfg.InstanceID, st.CASecretID, "federation-ca")
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	var ca caMaterial
	err = json.Unmarshal(caPlain, &ca)
	clear(caPlain)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	defer clear(ca.KeyDER)
	return issueMemberCert(ca, csr, instance, federationURL, s.now())
}

// setCert publishes a renewed certificate. The certificate the member
// presented for the renewal (presentedSHA, its current or still-accepted
// previous one) stays accepted until the grace period ends; any other
// certificate it held is revoked at once, so at most two are ever accepted.
func setCert(instance, serial, sha, presentedSHA string, now time.Time) func(*Membership) error {
	return func(next *Membership) error {
		revoke := func(s string) {
			if s != "" && !next.Revoked(s) {
				next.RevokedCertSerials = append(next.RevokedCertSerials, s)
			}
		}
		for i := range next.Members {
			m := &next.Members[i]
			if m.InstanceID != instance {
				continue
			}
			keepSerial := m.CertSerial
			switch {
			case presentedSHA == m.CertSHA256:
				revoke(m.PreviousCertSerial)
			case presentedSHA != "" && presentedSHA == m.PreviousCertSHA256:
				keepSerial = m.PreviousCertSerial
				revoke(m.CertSerial) // issued but never used
				presentedSHA = m.PreviousCertSHA256
			default:
				return errors.New("federation: renewal presented a certificate that is not accepted")
			}
			m.PreviousCertSHA256, m.PreviousCertSerial, m.PreviousCertUntil = presentedSHA, keepSerial, now.Add(certGrace).UTC().Format(time.RFC3339Nano)
			m.CertSerial, m.CertSHA256 = serial, sha
		}
		return nil
	}
}

// RetireCertificates runs on the authority: it publishes one generation that
// revokes the previous certificate of every member whose grace period ended.
func (s *Service) RetireCertificates(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.State != "active" || st.Role != "authority" {
		return err
	}
	now := s.now()
	due := func(doc *Membership) bool {
		for _, m := range doc.Members {
			if until, err := time.Parse(time.RFC3339Nano, m.PreviousCertUntil); err == nil && !now.Before(until) {
				return true
			}
		}
		return false
	}
	if t := s.trust.Load(); t.doc == nil || !due(t.doc) {
		return nil
	}
	var doc *Membership
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil || !due(applied) {
			return err
		}
		doc, err = s.publish(ctx, tx, st, applied, "certificate-retirement", func(next *Membership) error {
			for i := range next.Members {
				m := &next.Members[i]
				if until, err := time.Parse(time.RFC3339Nano, m.PreviousCertUntil); err == nil && !now.Before(until) {
					if !next.Revoked(m.PreviousCertSerial) {
						next.RevokedCertSerials = append(next.RevokedCertSerials, m.PreviousCertSerial)
					}
					m.PreviousCertSHA256, m.PreviousCertSerial, m.PreviousCertUntil = "", "", ""
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.certificate.retired", st.FederationID, "success", map[string]any{"generation": doc.Generation})
	})
	if err != nil || doc == nil {
		return err
	}
	s.afterPublish(ctx, doc)
	return nil
}
