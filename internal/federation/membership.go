package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

const recoveryExportPrefix = "ant-recovery-v1:"

// FoundInput founds a federation with this instance as authority.
type FoundInput struct {
	Name       string
	MemberName string // defaults to Name
	Labels     map[string]string
}

// FoundResult carries the recovery private key, shown exactly once (D2).
type FoundResult struct {
	Status            Status `json:"status"`
	RecoveryKey       string `json:"recovery_key"`
	RecoveryPublicKey string `json:"recovery_public_key"`
	CASHA256          string `json:"ca_sha256"`
}

func exportRecovery(sk ed25519.PrivateKey) string {
	return recoveryExportPrefix + base64.RawURLEncoding.EncodeToString(sk.Seed())
}

func parseRecovery(s string) (ed25519.PrivateKey, error) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(s), recoveryExportPrefix)
	if !ok {
		return nil, ErrRecoveryMismatch
	}
	seed, err := peer.DecodeCanonical(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, ErrRecoveryMismatch
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func canStart(st State) bool {
	return st.State == "none" || st.State == "left" || st.State == "removed"
}

func (s *Service) selfMember(signer ed25519.PrivateKey, certSerial, certSHA string, labels map[string]string, now time.Time) Member {
	return Member{InstanceID: s.cfg.InstanceID, Status: "active", APIURL: s.cfg.APIURL, FederationURL: s.cfg.PublicURL, CertSHA256: certSHA,
		CertSerial: certSerial, SigningKey: encodeKey(signer.Public().(ed25519.PublicKey)), Labels: nonNil(labels), SoftwareVersion: s.cfg.SoftwareVersion,
		Protocol: ProtocolRange{ProtocolMin, ProtocolMax}, JoinedAt: now.Format(time.RFC3339Nano)}
}

// Found creates the federation CA, the epoch-1 authority key and the offline
// recovery key, issues this instance's member certificate and publishes
// membership generation 1. The recovery private key is returned once and
// never stored; the instance keeps only its public half.
func (s *Service) Found(ctx context.Context, actor string, in FoundInput) (FoundResult, error) {
	if s.cfg.Keys == nil {
		return FoundResult{}, ErrKeysUnavailable
	}
	if !ValidFederationURL(s.cfg.PublicURL) {
		return FoundResult{}, ErrListenerMissing
	}
	if !ValidName(in.Name) {
		return FoundResult{}, invalid("federation name must match ^[a-z0-9][a-z0-9-]{0,62}$")
	}
	if in.MemberName == "" {
		in.MemberName = in.Name
	}
	if !ValidName(in.MemberName) {
		return FoundResult{}, invalid("member name must match ^[a-z0-9][a-z0-9-]{0,62}$")
	}
	if !ValidLabels(in.Labels) {
		return FoundResult{}, invalid("invalid labels")
	}
	now := s.now()
	ca, caCert, err := newCA(in.Name, now)
	if err != nil {
		return FoundResult{}, err
	}
	authPub, authKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return FoundResult{}, err
	}
	recPub, recKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return FoundResult{}, err
	}
	mat, tlsKey, signer, err := newMemberMaterial()
	if err != nil {
		return FoundResult{}, err
	}
	csr, err := newCSR(tlsKey, s.cfg.InstanceID, s.cfg.PublicURL)
	if err != nil {
		return FoundResult{}, err
	}
	certPEM, serial, certSHA, notAfter, err := issueMemberCert(ca, csr, s.cfg.InstanceID, s.cfg.PublicURL, now)
	if err != nil {
		return FoundResult{}, err
	}
	var fid string
	if err := s.cfg.DB.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&fid); err != nil {
		return FoundResult{}, err
	}
	self := s.selfMember(signer, serial, certSHA, in.Labels, now)
	self.Name = in.MemberName
	doc := &Membership{Kind: "membership", FederationID: fid, FederationName: in.Name, Generation: 1, AuthorityEpoch: 1,
		Protocol: ProtocolRange{ProtocolMin, ProtocolMax}, RecoveryPublicKey: encodeKey(recPub), CACertificates: []string{ca.CertPEM},
		Members: []Member{self}, RevokedCertSerials: []string{}, IssuedAt: now.Format(time.RFC3339Nano), IssuedBy: actor}
	doc.Authority = AuthorityCert{Epoch: 1, InstanceID: s.cfg.InstanceID, PublicKey: encodeKey(authPub), CertifiedBy: "recovery"}
	doc.Authority.Signature = sign(recKey, authorityStatement(fid, doc.Authority))
	if err := Sign(doc, authKey); err != nil {
		return FoundResult{}, err
	}
	if err := Verify(doc, nil); err != nil {
		return FoundResult{}, err
	}
	caSHA := CertSHA256(caCert.Raw)
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if !canStart(st) {
			return ErrAlreadyFederated
		}
		caRec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, actor, "federation-ca", ca.marshal())
		if err != nil {
			return err
		}
		authRec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, actor, authorityPurpose(1), authKey.Seed())
		if err != nil {
			return err
		}
		memRec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, actor, "federation-member-key", mat.marshal())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET federation_id=$2,name=$3,role='authority',state='active',authority_url=$4,federation_url=$4,ca_sha256=$5,
ca_secret_id=$6,authority_secret_id=$7,member_secret_id=$8,member_cert=$9,member_cert_serial=$10,member_cert_expires_at=$11,applied_generation=0,applied_hash='',
last_sync_error='',last_sync_source='',last_sync_attempt_at=NULL,last_sync_success_at=NULL WHERE instance_id=$1`,
			s.cfg.InstanceID, fid, in.Name, s.cfg.PublicURL, caSHA, caRec.ID, authRec.ID, memRec.ID, certPEM, serial, notAfter); err != nil {
			return err
		}
		if err := storeDocument(ctx, tx, s.cfg.InstanceID, doc); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.created", fid, "success", map[string]any{"name": in.Name, "generation": 1, "ca_sha256": caSHA,
			"recovery_key_fingerprint": KeyFingerprint(doc.RecoveryPublicKey)})
	})
	clear(authKey)
	if err != nil {
		clear(recKey)
		return FoundResult{}, err
	}
	if err := s.Refresh(ctx); err != nil {
		s.cfg.Logger.Warn("federation trust refresh failed", "reason", err)
	}
	out := FoundResult{RecoveryKey: exportRecovery(recKey), RecoveryPublicKey: doc.RecoveryPublicKey, CASHA256: caSHA}
	clear(recKey)
	out.Status, err = s.Status(ctx)
	return out, err
}

// authorityState returns the locked state of an active authority.
func (s *Service) authorityState(ctx context.Context, tx pgx.Tx) (State, *Membership, error) {
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

// publish signs and applies the next membership generation inside tx. The
// caller holds the state lock (authorityState).
func (s *Service) publish(ctx context.Context, tx pgx.Tx, st State, applied *Membership, actor string, mutate func(*Membership) error) (*Membership, error) {
	next := applied.Clone()
	next.Generation = applied.Generation + 1
	next.PrevHash = Hash(applied)
	next.IssuedAt = s.now().Format(time.RFC3339Nano)
	next.IssuedBy = actor
	if err := mutate(next); err != nil {
		return nil, err
	}
	if s.cfg.Keys == nil {
		return nil, ErrKeysUnavailable
	}
	_, seed, err := s.store.Open(ctx, s.cfg.InstanceID, st.AuthoritySecretID, authorityPurpose(applied.AuthorityEpoch))
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		clear(seed)
		return nil, errors.New("federation: authority key invalid")
	}
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	err = Sign(next, key)
	clear(key)
	if err != nil {
		return nil, err
	}
	if err := Verify(next, applied); err != nil {
		return nil, err
	}
	if err := storeDocument(ctx, tx, s.cfg.InstanceID, next); err != nil {
		return nil, err
	}
	return next, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.document.published", st.FederationID, "success", map[string]any{"kind": "membership", "generation": next.Generation, "hash": Hash(next), "issued_by": actor})
}

// afterPublish refreshes trust and nudges members. It never fails the caller.
func (s *Service) afterPublish(ctx context.Context, doc *Membership, extra ...string) {
	if err := s.Refresh(ctx); err != nil {
		s.cfg.Logger.Warn("federation trust refresh failed", "reason", err)
	}
	if doc != nil {
		go s.nudgeMembers(context.WithoutCancel(ctx), extra...)
	}
}

// --- invitations ---

// Invitation is a join invitation without its secret.
type Invitation struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	ExpiresAt  time.Time         `json:"expires_at"`
	CreatedAt  time.Time         `json:"created_at"`
	ConsumedAt *time.Time        `json:"consumed_at,omitempty"`
	ConsumedBy string            `json:"consumed_by,omitempty"`
	RevokedAt  *time.Time        `json:"revoked_at,omitempty"`
	State      string            `json:"state"` // pending | consumed | expired | revoked
}

// IssuedInvitation is returned once at creation.
type IssuedInvitation struct {
	Invitation
	Secret       string `json:"secret"`
	CASHA256     string `json:"ca_sha256"`
	AuthorityURL string `json:"authority_url"`
	// AuthorityCertSHA256 pins the authority's listener certificate for the
	// join (with the CA fingerprint); pass it to join as authority_cert_sha256.
	AuthorityCertSHA256 string `json:"authority_cert_sha256"`
}

// InvitationInput creates an invitation bound to a member name.
type InvitationInput struct {
	Name   string
	Labels map[string]string
	TTL    time.Duration
}

func invitationVerifier(secret string) []byte {
	sum := sha256.Sum256([]byte("kuvryn-ant/federation/invitation/v1\n" + secret))
	return sum[:]
}

// CreateInvitation issues a one-time invitation on the authority.
func (s *Service) CreateInvitation(ctx context.Context, actor string, in InvitationInput) (IssuedInvitation, error) {
	if !ValidName(in.Name) {
		return IssuedInvitation{}, invalid("member name must match ^[a-z0-9][a-z0-9-]{0,62}$")
	}
	if !ValidLabels(in.Labels) {
		return IssuedInvitation{}, invalid("invalid labels")
	}
	if in.TTL == 0 {
		in.TTL = time.Hour
	}
	if in.TTL < time.Minute || in.TTL > 7*24*time.Hour {
		return IssuedInvitation{}, invalid("ttl_seconds must be between 60 and 604800")
	}
	var out IssuedInvitation
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		for _, m := range applied.Members {
			if m.Name == in.Name && m.Status != "removed" {
				return ErrDuplicateInstance
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return err
		}
		random := make([]byte, 32)
		_, _ = rand.Read(random)
		secret := id + "." + base64.RawURLEncoding.EncodeToString(random)
		labels, _ := json.Marshal(nonNil(in.Labels))
		expires := s.now().Add(in.TTL)
		if err := tx.QueryRow(ctx, `INSERT INTO federation_invitations(instance_id,id,federation_id,verifier,name,labels,expires_at,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,nullif($8,'')::uuid) RETURNING created_at`,
			s.cfg.InstanceID, id, st.FederationID, invitationVerifier(secret), in.Name, labels, expires, actor).Scan(&out.CreatedAt); err != nil {
			return err
		}
		out.Invitation = Invitation{ID: id, Name: in.Name, Labels: nonNil(in.Labels), ExpiresAt: expires, CreatedAt: out.CreatedAt, State: "pending"}
		out.Secret, out.CASHA256, out.AuthorityURL = secret, st.CASHA256, st.FederationURL
		if cert, err := parseCertPEM(st.MemberCert); err == nil {
			out.AuthorityCertSHA256 = CertSHA256(cert.Raw)
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.invitation.created", id, "success", map[string]any{"name": in.Name, "expires_at": expires})
	})
	return out, err
}

// ListInvitations returns the authority's invitations, newest first.
func (s *Service) ListInvitations(ctx context.Context) ([]Invitation, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	out := []Invitation{}
	if st.FederationID == "" {
		return out, nil
	}
	if st.Role != "authority" {
		return nil, ErrNotAuthority
	}
	rows, err := s.cfg.DB.Query(ctx, `SELECT id::text,name,labels,expires_at,created_at,consumed_at,coalesce(consumed_by::text,''),revoked_at FROM federation_invitations
WHERE instance_id=$1 AND federation_id=$2 ORDER BY created_at DESC,id LIMIT 500`, s.cfg.InstanceID, st.FederationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var i Invitation
		var labels []byte
		if err := rows.Scan(&i.ID, &i.Name, &labels, &i.ExpiresAt, &i.CreatedAt, &i.ConsumedAt, &i.ConsumedBy, &i.RevokedAt); err != nil {
			return nil, err
		}
		i.Labels = map[string]string{}
		_ = json.Unmarshal(labels, &i.Labels)
		switch {
		case i.RevokedAt != nil:
			i.State = "revoked"
		case i.ConsumedAt != nil:
			i.State = "consumed"
		case !i.ExpiresAt.After(s.now()):
			i.State = "expired"
		default:
			i.State = "pending"
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvitation revokes a pending invitation.
func (s *Service) RevokeInvitation(ctx context.Context, actor, id string) error {
	return s.tx(ctx, func(tx pgx.Tx) error { return s.RevokeInvitationTx(ctx, tx, actor, id) })
}

// RevokeInvitationTx revokes a pending invitation inside the caller's transaction.
func (s *Service) RevokeInvitationTx(ctx context.Context, tx pgx.Tx, actor, id string) error {
	st, _, err := s.authorityState(ctx, tx)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE federation_invitations SET revoked_at=clock_timestamp() WHERE instance_id=$1 AND federation_id=$2 AND id=$3 AND revoked_at IS NULL AND consumed_at IS NULL`, s.cfg.InstanceID, st.FederationID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.invitation.revoked", id, "success", map[string]any{})
}

// --- join ---

// JoinRequest is sent by a joining instance to the authority's /join.
type JoinRequest struct {
	Invitation      string            `json:"invitation"`
	InstanceID      string            `json:"instance_id"`
	APIURL          string            `json:"api_url"`
	FederationURL   string            `json:"federation_url"`
	CSR             string            `json:"csr"`
	SigningKey      string            `json:"signing_key"`
	SoftwareVersion string            `json:"software_version"`
	Protocol        ProtocolRange     `json:"protocol"`
	Labels          map[string]string `json:"labels,omitempty"`
}

// JoinResponse carries the member certificate and the membership chain.
type JoinResponse struct {
	Certificate string       `json:"certificate"`
	Documents   []Membership `json:"documents"`
}

// maxChain bounds a full chain transfer.
const maxChain = 10000

// acceptJoin runs on the authority for POST /federation/v1/join.
func (s *Service) acceptJoin(ctx context.Context, req JoinRequest) (JoinResponse, error) {
	invID, _, ok := strings.Cut(req.Invitation, ".")
	if !ok || !uuidPattern.MatchString(invID) || !uuidPattern.MatchString(req.InstanceID) || !ValidFederationURL(req.FederationURL) || !validAPIURL(req.APIURL) ||
		len(req.SoftwareVersion) > 128 || !ValidLabels(req.Labels) {
		s.rejectInvitation(ctx, invID, req.InstanceID, "malformed")
		return JoinResponse{}, ErrInvitationInvalid
	}
	if _, err := decodeKey(req.SigningKey); err != nil {
		return JoinResponse{}, invalid("invalid signing key")
	}
	if req.Protocol.Min > ProtocolMax || req.Protocol.Max < ProtocolMin || req.Protocol.Min < 1 || req.Protocol.Max < req.Protocol.Min {
		return JoinResponse{}, ErrProtocol
	}
	// The invitation is checked before the federation state row is locked:
	// the join endpoint is unauthenticated, and a stream of bogus invitations
	// must not serialize every federation change on the authority.
	pre, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return JoinResponse{}, err
	}
	if pre.State != "active" || pre.FederationID == "" {
		return JoinResponse{}, ErrNotConfigured
	}
	if pre.Role != "authority" {
		return JoinResponse{}, ErrNotAuthority
	}
	if _, _, reason, err := s.checkInvitation(ctx, s.cfg.DB, pre.FederationID, invID, req.Invitation, false); err != nil {
		if reason != "" {
			s.rejectInvitation(ctx, invID, req.InstanceID, reason)
		}
		return JoinResponse{}, err
	}
	var resp JoinResponse
	var published *Membership
	var reason string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		// Re-check under the lock: another join may have consumed it.
		name, labelsRaw, why, err := s.checkInvitation(ctx, tx, st.FederationID, invID, req.Invitation, true)
		if err != nil {
			reason = why
			return err
		}
		if req.InstanceID == s.cfg.InstanceID {
			reason = "duplicate_instance"
			return ErrDuplicateInstance
		}
		for _, m := range applied.Members {
			if m.Status == "removed" {
				continue
			}
			if m.InstanceID == req.InstanceID || m.Name == name {
				reason = "duplicate_instance"
				return ErrDuplicateInstance
			}
			// Member certificates carry the federation URL host as SAN: a
			// second member on the same host would hold a CA-issued
			// certificate valid for another member's (or the authority's)
			// listener and could intercept joins.
			if sameHost(m.FederationURL, req.FederationURL) {
				reason = "duplicate_host"
				return ErrDuplicateInstance
			}
		}
		certPEM, serial, certSHA, _, err := s.issueFor(ctx, st, req.CSR, req.InstanceID, req.FederationURL)
		if err != nil {
			return invalid("invalid certificate signing request")
		}
		labels := map[string]string{}
		_ = json.Unmarshal(labelsRaw, &labels)
		for k, v := range req.Labels {
			if _, set := labels[k]; !set {
				labels[k] = v // invitation labels win over requested labels
			}
		}
		if !ValidLabels(labels) {
			return invalid("invalid labels")
		}
		member := Member{InstanceID: req.InstanceID, Name: name, Status: "active", APIURL: req.APIURL, FederationURL: req.FederationURL, CertSHA256: certSHA,
			CertSerial: serial, SigningKey: req.SigningKey, Labels: labels, SoftwareVersion: req.SoftwareVersion,
			Protocol: ProtocolRange{max(req.Protocol.Min, ProtocolMin), min(req.Protocol.Max, ProtocolMax)}, JoinedAt: s.now().Format(time.RFC3339Nano)}
		published, err = s.publish(ctx, tx, st, applied, "invitation:"+invID, func(next *Membership) error {
			next.Members = slices.DeleteFunc(next.Members, func(m Member) bool { return m.InstanceID == req.InstanceID })
			next.Members = append(next.Members, member)
			return nil
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_invitations SET consumed_at=clock_timestamp(),consumed_by=$4 WHERE instance_id=$1 AND federation_id=$2 AND id=$3`,
			s.cfg.InstanceID, st.FederationID, invID, req.InstanceID); err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.member.added", req.InstanceID, "success", map[string]any{"name": name, "invitation_id": invID,
			"generation": published.Generation, "cert_serial": serial}); err != nil {
			return err
		}
		resp.Certificate = certPEM
		resp.Documents, err = documentsAfter(ctx, tx, s.cfg.InstanceID, st.FederationID, 0, maxChain)
		return err
	})
	if err != nil {
		if reason != "" {
			s.rejectInvitation(ctx, invID, req.InstanceID, reason)
		}
		return JoinResponse{}, err
	}
	s.afterPublish(ctx, published, req.InstanceID)
	return resp, nil
}

// checkInvitation validates an invitation secret. It returns the bound name
// and labels, or ErrInvitationInvalid with the audit reason. With lock the
// invitation row is locked for the caller's transaction.
func (s *Service) checkInvitation(ctx context.Context, q queryer, federationID, invID, secret string, lock bool) (string, []byte, string, error) {
	var name string
	var labelsRaw, verifier []byte
	var expires time.Time
	var consumed, revoked *time.Time
	query := `SELECT name,labels,verifier,expires_at,consumed_at,revoked_at FROM federation_invitations WHERE instance_id=$1 AND federation_id=$2 AND id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, query, s.cfg.InstanceID, federationID, invID).Scan(&name, &labelsRaw, &verifier, &expires, &consumed, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil, "unknown", ErrInvitationInvalid
	case err != nil:
		return "", nil, "", err
	case subtle.ConstantTimeCompare(verifier, invitationVerifier(secret)) != 1:
		return "", nil, "secret_mismatch", ErrInvitationInvalid
	case revoked != nil:
		return "", nil, "revoked", ErrInvitationInvalid
	case consumed != nil:
		return "", nil, "consumed", ErrInvitationInvalid
	case !expires.After(s.now()):
		return "", nil, "expired", ErrInvitationInvalid
	}
	return name, labelsRaw, "", nil
}

// rejectionAuditWindow throttles federation.invitation.rejected records: at
// most one per reason per window, carrying the number suppressed since the
// previous record. Every rejection still counts in metrics.
const rejectionAuditWindow = 10 * time.Second

// rejectInvitation records a failed join as a security event (throttled, so
// an unauthenticated flood cannot fill the audit log).
func (s *Service) rejectInvitation(ctx context.Context, invitation, instance, reason string) {
	s.metrics.joinRejected.Add(1)
	ok, suppressed := s.throttle("invitation-rejected:"+reason, rejectionAuditWindow)
	if !ok {
		return
	}
	if !uuidPattern.MatchString(invitation) {
		invitation = ""
	}
	if !uuidPattern.MatchString(instance) {
		instance = ""
	}
	ctx = context.WithoutCancel(ctx)
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.invitation.rejected", invitation, "denied", map[string]any{"reason": reason, "instance_id": instance, "suppressed": suppressed})
	})
}

// JoinInput joins an existing federation.
type JoinInput struct {
	AuthorityURL string
	CASHA256     string
	// AuthorityCertSHA256, when set, pins the authority's listener
	// certificate before the invitation is sent.
	AuthorityCertSHA256 string
	Invitation          string
	Labels              map[string]string
}

// Join connects to the authority (TLS pinned to the CA fingerprint received
// out of band), presents the invitation with a fresh CSR and member signing
// key, verifies the returned membership chain from generation 1 and applies
// it in one transaction.
func (s *Service) Join(ctx context.Context, actor string, in JoinInput) (Status, error) {
	if s.cfg.Keys == nil {
		return Status{}, ErrKeysUnavailable
	}
	if !ValidFederationURL(s.cfg.PublicURL) {
		return Status{}, ErrListenerMissing
	}
	in.CASHA256 = strings.ToLower(strings.TrimSpace(in.CASHA256))
	in.AuthorityCertSHA256 = strings.ToLower(strings.TrimSpace(in.AuthorityCertSHA256))
	if in.AuthorityCertSHA256 != "" && !hexPattern.MatchString(in.AuthorityCertSHA256) {
		return Status{}, invalid("authority_cert_sha256 must be 64 hex characters")
	}
	if !ValidFederationURL(in.AuthorityURL) || !hexPattern.MatchString(in.CASHA256) || len(in.Invitation) < 40 || len(in.Invitation) > 256 || !ValidLabels(in.Labels) {
		return Status{}, invalid("authority_url (https), ca_sha256 (64 hex) and invitation are required")
	}
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return Status{}, err
	}
	if !canStart(st) {
		return Status{}, ErrAlreadyFederated
	}
	mat, tlsKey, signer, err := newMemberMaterial()
	if err != nil {
		return Status{}, err
	}
	csr, err := newCSR(tlsKey, s.cfg.InstanceID, s.cfg.PublicURL)
	if err != nil {
		return Status{}, err
	}
	req := JoinRequest{Invitation: in.Invitation, InstanceID: s.cfg.InstanceID, APIURL: s.cfg.APIURL, FederationURL: s.cfg.PublicURL, CSR: csr,
		SigningKey: encodeKey(signer.Public().(ed25519.PublicKey)), SoftwareVersion: s.cfg.SoftwareVersion, Protocol: ProtocolRange{ProtocolMin, ProtocolMax}, Labels: in.Labels}
	body, _ := json.Marshal(req)
	raw, leaf, err := s.client.DoPinnedLeaf(ctx, in.AuthorityURL, in.CASHA256, in.AuthorityCertSHA256, "POST", "/federation/v1/join", body)
	if err != nil {
		return Status{}, peerError(in.AuthorityURL, err)
	}
	var resp JoinResponse
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Documents) == 0 {
		return Status{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	// Verify the chain from generation 1 to the head.
	var applied *Membership
	for i := range resp.Documents {
		d := &resp.Documents[i]
		if err := Verify(d, applied); err != nil {
			return Status{}, &RejectedError{Reason: RejectReason(err), Err: err}
		}
		applied = d
	}
	head := applied
	caOK := false
	var caPEM string
	for _, c := range head.CACertificates {
		if cert, err := parseCertPEM(c); err == nil && CertSHA256(cert.Raw) == in.CASHA256 {
			caOK, caPEM = true, c
		}
	}
	// The server that answered must be the authority itself: its leaf
	// certificate is the one the verified membership publishes for the
	// authority (another member's certificate, also issued by the federation
	// CA, would otherwise pass the CA pin and relay the join).
	if authority, ok := head.Member(head.Authority.InstanceID); !ok || !slices.Contains(authority.AcceptedCerts(s.now()), leaf) {
		return Status{}, &RejectedError{Reason: "authority_certificate_mismatch", Err: ErrMalformed}
	}
	self, found := head.Member(s.cfg.InstanceID)
	if !caOK || !found || self.Status != "active" || self.SigningKey != req.SigningKey {
		return Status{}, &RejectedError{Reason: "membership_mismatch", Err: ErrMalformed}
	}
	cert, err := parseCertPEM(resp.Certificate)
	if err != nil || cert.Subject.CommonName != s.cfg.InstanceID || CertSHA256(cert.Raw) != self.CertSHA256 {
		return Status{}, &RejectedError{Reason: "certificate_mismatch", Err: ErrMalformed}
	}
	if _, err := tlsCertificate(resp.Certificate, caPEM, tlsKey); err != nil {
		return Status{}, &RejectedError{Reason: "certificate_mismatch", Err: err}
	}
	authority, _ := head.Member(head.Authority.InstanceID)
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if !canStart(st) {
			return ErrAlreadyFederated
		}
		memRec, err := s.store.CreateTx(ctx, tx, s.cfg.InstanceID, actor, "federation-member-key", mat.marshal())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_state SET federation_id=$2,name=$3,role='member',state='active',authority_url=$4,federation_url=$5,ca_sha256=$6,
ca_secret_id=NULL,authority_secret_id=NULL,member_secret_id=$7,member_cert=$8,member_cert_serial=$9,member_cert_expires_at=$10,applied_generation=0,applied_hash='',
last_sync_error='',last_sync_source=$4,last_sync_attempt_at=clock_timestamp(),last_sync_success_at=clock_timestamp() WHERE instance_id=$1`,
			s.cfg.InstanceID, head.FederationID, head.FederationName, authority.FederationURL, s.cfg.PublicURL, in.CASHA256, memRec.ID, resp.Certificate,
			cert.SerialNumber.Text(16), cert.NotAfter); err != nil {
			return err
		}
		for i := range resp.Documents {
			if err := storeDocument(ctx, tx, s.cfg.InstanceID, &resp.Documents[i]); err != nil {
				return err
			}
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.joined", head.FederationID, "success", map[string]any{"federation_name": head.FederationName,
			"generation": head.Generation, "authority_instance_id": head.Authority.InstanceID, "ca_sha256": in.CASHA256, "name": self.Name})
	})
	if err != nil {
		return Status{}, err
	}
	if err := s.Refresh(ctx); err != nil {
		s.cfg.Logger.Warn("federation trust refresh failed", "reason", err)
	}
	// Fetch the applied policy right away (best effort; the worker retries).
	if err := s.SyncOnce(ctx); err != nil {
		s.cfg.Logger.Warn("federation policy fetch after join failed; the sync worker retries", "reason", err)
	}
	return s.Status(ctx)
}

func peerError(url string, err error) error {
	var pe *peer.ErrPeer
	if errors.As(err, &pe) {
		switch pe.Code {
		case "invitation_invalid":
			return ErrInvitationInvalid
		case "duplicate_instance":
			return ErrDuplicateInstance
		case "not_authority":
			return ErrNotAuthority
		case "protocol_unsupported":
			return ErrProtocol
		}
	}
	return &UnreachableError{Peer: url, Err: err}
}

// --- leave / remove / suspend ---

// Leave asks the authority to remove this member (signed request), applies the
// resulting membership and marks the instance as having left. With force, a
// member whose authority is unreachable leaves locally only.
func (s *Service) Leave(ctx context.Context, actor string, force bool) (Status, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return Status{}, err
	}
	if st.State != "active" {
		return Status{}, ErrNotConfigured
	}
	if st.Role == "authority" {
		return Status{}, ErrAuthorityLeave
	}
	id, t, err := s.identity()
	var remoteErr error
	if err == nil {
		authority, _ := t.doc.Member(t.doc.Authority.InstanceID)
		body, _ := json.Marshal(map[string]any{"after": st.AppliedGeneration})
		var raw []byte
		raw, remoteErr = s.client.Do(ctx, id, authority.FederationURL, "POST", "/federation/v1/leave", body, authority.InstanceID)
		if remoteErr == nil {
			var docs struct {
				Documents []Membership `json:"documents"`
			}
			if json.Unmarshal(raw, &docs) == nil {
				_, _ = s.applyDocuments(ctx, docs.Documents, authority.Name, true)
			}
		}
	} else {
		remoteErr = err
	}
	if remoteErr != nil && !force {
		return Status{}, peerError(st.AuthorityURL, remoteErr)
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		if cur.State == "active" || cur.State == "removed" {
			if _, err := tx.Exec(ctx, `UPDATE federation_state SET state='left',revision=revision+1,updated_at=clock_timestamp() WHERE instance_id=$1`, s.cfg.InstanceID); err != nil {
				return err
			}
		}
		if err := dropFederatedIdentity(ctx, tx, s.cfg.InstanceID); err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.left", cur.FederationID, "success", map[string]any{"forced": remoteErr != nil, "generation": cur.AppliedGeneration})
	})
	if err != nil {
		return Status{}, err
	}
	_ = s.Refresh(ctx)
	return s.Status(ctx)
}

// SetMemberStatus suspends, resumes or removes a member (authority only).
// Removal revokes the member's certificate serial.
func (s *Service) SetMemberStatus(ctx context.Context, actor, member, action string) (MemberView, error) {
	var doc *Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		_, doc, err = s.SetMemberStatusTx(ctx, tx, actor, member, action)
		return err
	})
	if err != nil {
		return MemberView{}, err
	}
	s.AfterPublish(ctx, doc, member)
	return s.Member(ctx, member)
}

// SetMemberStatusTx is SetMemberStatus inside the caller's transaction (used
// by the management API's idempotency store). It returns the member as
// published and the new document, or a nil document when the member already
// had the requested status. Call AfterPublish after commit.
func (s *Service) SetMemberStatusTx(ctx context.Context, tx pgx.Tx, actor, member, action string) (MemberView, *Membership, error) {
	st, applied, err := s.authorityState(ctx, tx)
	if err != nil {
		return MemberView{}, nil, err
	}
	if member == s.cfg.InstanceID {
		return MemberView{}, nil, ErrSelf
	}
	cur, ok := applied.Member(member)
	if !ok {
		return MemberView{}, nil, ErrNotFound
	}
	target := map[string]string{"suspend": "suspended", "resume": "active", "remove": "removed"}[action]
	if target == "" {
		return MemberView{}, nil, invalid("unknown action")
	}
	if cur.Status == target {
		return viewFromDocument(applied, member, s.cfg.InstanceID), nil, nil // idempotent no-op
	}
	if cur.Status == "removed" {
		return MemberView{}, nil, ErrNotFound
	}
	var doc *Membership
	err = func() error {
		var err error
		doc, err = s.publish(ctx, tx, st, applied, actor, func(next *Membership) error {
			for i := range next.Members {
				if next.Members[i].InstanceID == member {
					next.Members[i].Status = target
					if target == "removed" && next.Members[i].CertSerial != "" && !next.Revoked(next.Members[i].CertSerial) {
						next.RevokedCertSerials = append(next.RevokedCertSerials, next.Members[i].CertSerial)
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, actor, "federation.member."+map[string]string{"suspend": "suspended", "resume": "resumed", "remove": "removed"}[action],
			member, "success", map[string]any{"name": cur.Name, "generation": doc.Generation})
	}()
	if err != nil {
		return MemberView{}, nil, err
	}
	return viewFromDocument(doc, member, s.cfg.InstanceID), doc, nil
}

// viewFromDocument renders a member from a document (no local observations).
func viewFromDocument(doc *Membership, member, self string) MemberView {
	m, _ := doc.Member(member)
	v := MemberView{InstanceID: m.InstanceID, Name: m.Name, Status: m.Status, APIURL: m.APIURL, FederationURL: m.FederationURL, CertSHA256: m.CertSHA256,
		CertSerial: m.CertSerial, Labels: nonNil(m.Labels), SoftwareVersion: m.SoftwareVersion, Protocol: m.Protocol, Authority: m.InstanceID == doc.Authority.InstanceID,
		Self: m.InstanceID == self, Generation: doc.Generation}
	if t, err := time.Parse(time.RFC3339Nano, m.JoinedAt); err == nil {
		v.JoinedAt = &t
	}
	return v
}

// AfterPublish refreshes trust and nudges members after a committed change.
func (s *Service) AfterPublish(ctx context.Context, doc *Membership, extra ...string) {
	s.afterPublish(ctx, doc, extra...)
}
