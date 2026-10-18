// Package federation implements Phase 2 federation membership: signed,
// versioned membership documents (RFC 8785 + Ed25519, hash-chained, with an
// offline recovery key and rotation path), founding, invitations, CSR-based
// member certificates, join/leave/remove/suspend, document sync with relay
// from any member, and the peer API served on the dedicated mTLS listener.
//
// Serving never depends on federation: every entry point degrades to an error
// for the federation feature only, and the sync worker only records status.
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors returned to the management API.
var (
	ErrNotConfigured     = errors.New("federation: not configured on this instance")
	ErrListenerMissing   = errors.New("federation: federation listener public URL not configured")
	ErrAlreadyFederated  = errors.New("federation: instance already belongs to a federation")
	ErrNotAuthority      = errors.New("federation: this instance is not the federation authority")
	ErrInvitationInvalid = errors.New("federation: invitation invalid, expired, consumed or revoked")
	ErrDuplicateInstance = errors.New("federation: instance ID or name already a member")
	ErrNotFound          = errors.New("federation: not found")
	ErrAuthorityLeave    = errors.New("federation: the authority cannot leave; hand over authority first")
	ErrRecoveryMismatch  = errors.New("federation: recovery key does not match the federation recovery key")
	ErrKeysUnavailable   = errors.New("federation: secret keyring not configured")
	ErrProtocol          = errors.New("federation: no common protocol version")
	ErrSelf              = errors.New("federation: the authority cannot change its own membership")
	ErrRevisionMismatch  = errors.New("federation: revision changed")
	ErrStaleBase         = errors.New("federation: base generation is not the applied generation")
	ErrNotFederated      = errors.New("federation: principal is not a federated principal")
)

// InputError is an invalid request with a client-safe message.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }
func invalid(msg string) error      { return &InputError{msg} }

// UnreachableError wraps a failure to reach a peer.
type UnreachableError struct {
	Peer string
	Err  error
}

func (e *UnreachableError) Error() string { return "federation: peer unreachable: " + e.Peer }
func (e *UnreachableError) Unwrap() error { return e.Err }

// RejectedError is a document rejected by verification (tampered, stale, ...).
type RejectedError struct {
	Reason string
	Err    error
}

func (e *RejectedError) Error() string { return "federation: document rejected: " + e.Reason }
func (e *RejectedError) Unwrap() error { return e.Err }

// Config wires the service to one serving instance.
type Config struct {
	DB         *pgxpool.Pool
	Keys       *secrets.Keyring
	InstanceID string
	// PublicURL is the https URL other members use to reach this instance's
	// federation listener (ANT_FEDERATION_PUBLIC_URL). Required to found or join.
	PublicURL string
	// ListenAddress is the configured listener address, for status only.
	ListenAddress string
	// APIURL is this instance's public management/feed origin (ANT_PUBLIC_URL).
	APIURL          string
	SoftwareVersion string
	Now             func() time.Time
	PollInterval    time.Duration
	PeerTimeout     time.Duration
	// Search tuning (zero means the section 7 defaults).
	SearchConcurrency, SearchPeerRate, SearchPrincipalRate int
	SearchMemberTimeout, SearchTimeout, SearchCacheTTL     time.Duration // negative TTL disables the cache
	Logger                                                 *slog.Logger
}

// Service is safe for concurrent use.
type Service struct {
	cfg    Config
	store  secrets.Store
	client peer.Client
	trust  atomic.Pointer[trust]
	nudge  chan struct{}
	logMu  sync.Mutex
	logged map[string]time.Time
	// suppressed counts throttled events per key (guarded by logMu).
	suppressed map[string]int
	// policyCache holds the applied policy document for sign-in and
	// assertion lookups; nil means reload from the database.
	policyCache atomic.Pointer[PolicyDocument]
	metrics     assertionMetrics
	// alerts is the last evaluated alert set (metrics without DB access).
	alerts atomic.Pointer[[]Alert]
	// policyHeadGauge is the last evaluated policy status (metrics).
	policyHeadGauge atomic.Pointer[PolicyStatus]
	// search is the cross-instance search runtime (search.go).
	search   searchState
	searcher atomic.Pointer[PackageSearcher]

	// topologyCache holds the applied topology document; nil means reload.
	topologyCache atomic.Pointer[TopologyDocument]
	// replication is the coordinated-replication backend (internal/replication),
	// wired after construction; nil when replication is not serving.
	replication   atomic.Pointer[ReplicationHooks]
	reconcile     chan struct{}
	peerRoutes    map[string]http.Handler
	peerMu        sync.Mutex
	topologyGauge atomic.Pointer[TopologyStatus]
}

type trust struct {
	state  State
	doc    *Membership
	cert   *tls.Certificate
	pool   *x509.CertPool
	signer ed25519.PrivateKey
}

// New builds the service; call Refresh before serving the listener.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.InstanceID == "" {
		return nil, errors.New("federation: database and instance required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SoftwareVersion == "" {
		cfg.SoftwareVersion = "unknown"
	}
	s := &Service{cfg: cfg, store: secrets.Store{DB: cfg.DB, Keys: cfg.Keys}, client: peer.Client{Timeout: cfg.PeerTimeout, Now: cfg.Now},
		nudge: make(chan struct{}, 1), logged: map[string]time.Time{}, reconcile: make(chan struct{}, 1), peerRoutes: map[string]http.Handler{}}
	s.trust.Store(&trust{state: State{State: "none"}})
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// InstanceID is the serving instance.
func (s *Service) InstanceID() string { return s.cfg.InstanceID }

// Refresh reloads the trust state (applied membership, own certificate and
// signing key) from the database. A failure keeps the previous trust state.
func (s *Service) Refresh(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	t := &trust{state: st}
	if st.FederationID != "" && st.AppliedGeneration > 0 {
		if t.doc, err = loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration); err != nil {
			return err
		}
		t.pool = x509.NewCertPool()
		for _, c := range t.doc.CACertificates {
			if cert, err := parseCertPEM(c); err == nil {
				t.pool.AddCert(cert)
			}
		}
	}
	// The certificate is kept after leaving or removal so the instance can
	// still present it (and be refused by peers); Lookup serves nobody then.
	if st.State != "none" && st.MemberSecretID != "" && st.MemberCert != "" && s.cfg.Keys != nil && t.doc != nil {
		mat, err := s.openMember(ctx, st)
		if err != nil {
			return err
		}
		key, signer, err := mat.parse()
		if err != nil {
			return err
		}
		ca := t.doc.CACertificates[0]
		for _, c := range t.doc.CACertificates {
			if cert, e := parseCertPEM(c); e == nil && CertSHA256(cert.Raw) == st.CASHA256 {
				ca = c
			}
		}
		if t.cert, err = tlsCertificate(st.MemberCert, ca, key); err != nil {
			return err
		}
		t.signer = signer
	}
	s.trust.Store(t)
	return nil
}

func (s *Service) openMember(ctx context.Context, st State) (memberMaterial, error) {
	_, plain, err := s.store.Open(ctx, s.cfg.InstanceID, st.MemberSecretID, "federation-member-key")
	if err != nil {
		return memberMaterial{}, err
	}
	defer clear(plain)
	var m memberMaterial
	err = json.Unmarshal(plain, &m)
	return m, err
}

// --- peer.Trust ---

func (s *Service) ServerCertificate() *tls.Certificate { return s.trust.Load().cert }
func (s *Service) CAPool() *x509.CertPool {
	if p := s.trust.Load().pool; p != nil {
		return p
	}
	return x509.NewCertPool()
}
func (s *Service) Revoked(serial string) bool {
	t := s.trust.Load()
	return t.doc != nil && t.doc.Revoked(serial)
}
func (s *Service) Lookup(id string) (peer.Member, bool) {
	t := s.trust.Load()
	if t.doc == nil || t.state.State != "active" {
		return peer.Member{}, false
	}
	m, ok := t.doc.Member(id)
	if !ok || id == s.cfg.InstanceID {
		return peer.Member{}, false
	}
	key, err := decodeKey(m.SigningKey)
	if err != nil {
		return peer.Member{}, false
	}
	return peer.Member{InstanceID: m.InstanceID, Name: m.Name, Status: m.Status, SigningKey: key}, true
}
func (s *Service) Now() time.Time { return s.cfg.Now() }

// identity is this member's client identity for signed peer requests.
func (s *Service) identity() (peer.Identity, *trust, error) {
	t := s.trust.Load()
	if t.cert == nil || t.signer == nil || t.pool == nil {
		return peer.Identity{}, t, ErrNotConfigured
	}
	return peer.Identity{InstanceID: s.cfg.InstanceID, SigningKey: t.signer, Certificate: t.cert, Roots: t.pool}, t, nil
}

// Probe performs a signed status request to a member of the applied
// membership (connectivity check). It returns the peer's refusal as an error.
func (s *Service) Probe(ctx context.Context, member string) error {
	id, t, err := s.identity()
	if err != nil {
		return err
	}
	m, ok := t.doc.Member(member)
	if !ok {
		return ErrNotFound
	}
	_, err = s.client.Do(ctx, id, m.FederationURL, "GET", "/federation/v1/status", nil, m.InstanceID)
	return err
}

// --- status views ---

// DocumentHead describes the applied membership document.
type DocumentHead struct {
	Generation int64     `json:"generation"`
	Hash       string    `json:"hash"`
	AppliedAt  time.Time `json:"applied_at"`
	AgeSeconds int64     `json:"age_seconds"`
	IssuedAt   string    `json:"issued_at"`
}

// SyncStatus reports the document sync worker.
type SyncStatus struct {
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Source        string     `json:"source,omitempty"`
}

// CertificateStatus describes this member's federation certificate.
type CertificateStatus struct {
	Serial    string     `json:"serial"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// MemberCounts summarizes membership.
type MemberCounts struct {
	Active    int `json:"active"`
	Suspended int `json:"suspended"`
	Removed   int `json:"removed"`
}

// Status is the local view of federation (GET /api/v1/federation/status).
type Status struct {
	InstanceID             string             `json:"instance_id"`
	State                  string             `json:"state"`
	Role                   string             `json:"role,omitempty"`
	Health                 string             `json:"health"`
	FederationID           string             `json:"federation_id,omitempty"`
	FederationName         string             `json:"federation_name,omitempty"`
	AuthorityEpoch         int64              `json:"authority_epoch,omitempty"`
	AuthorityInstanceID    string             `json:"authority_instance_id,omitempty"`
	AuthorityURL           string             `json:"authority_url,omitempty"`
	ListenerConfigured     bool               `json:"listener_configured"`
	ListenAddress          string             `json:"listen_address,omitempty"`
	FederationURL          string             `json:"federation_url,omitempty"`
	CASHA256               string             `json:"ca_sha256,omitempty"`
	RecoveryKeyFingerprint string             `json:"recovery_key_fingerprint,omitempty"`
	Membership             *DocumentHead      `json:"membership,omitempty"`
	Certificate            *CertificateStatus `json:"certificate,omitempty"`
	Sync                   SyncStatus         `json:"sync"`
	Members                MemberCounts       `json:"members"`
	SoftwareVersion        string             `json:"software_version"`
	Protocol               ProtocolRange      `json:"protocol"`
	Policy                 *PolicyStatus      `json:"policy,omitempty"`
	Topology               *TopologyStatus    `json:"topology,omitempty"`
	Alerts                 []Alert            `json:"alerts"`
}

// Staleness thresholds (§3.2): reported, not enforced (D4).
const (
	StaleWarning  = 15 * time.Minute
	StaleCritical = 24 * time.Hour
)

// Status reads the local federation view.
func (s *Service) Status(ctx context.Context) (Status, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return Status{}, err
	}
	out := Status{InstanceID: s.cfg.InstanceID, State: st.State, Role: st.Role, FederationID: st.FederationID, FederationName: st.Name,
		AuthorityURL: st.AuthorityURL, ListenerConfigured: s.cfg.ListenAddress != "", ListenAddress: s.cfg.ListenAddress, FederationURL: st.FederationURL,
		CASHA256: st.CASHA256, SoftwareVersion: s.cfg.SoftwareVersion, Protocol: ProtocolRange{ProtocolMin, ProtocolMax},
		Sync: SyncStatus{LastAttemptAt: st.LastSyncAttemptAt, LastSuccessAt: st.LastSyncSuccessAt, LastError: st.LastSyncError, Source: st.LastSyncSource}, Alerts: []Alert{}}
	if out.FederationURL == "" {
		out.FederationURL = s.cfg.PublicURL
	}
	out.Health = "disabled"
	if st.State == "none" || st.FederationID == "" {
		return out, nil
	}
	if st.MemberCertSerial != "" {
		out.Certificate = &CertificateStatus{Serial: st.MemberCertSerial, ExpiresAt: st.MemberCertExpiresAt}
	}
	if st.AppliedGeneration > 0 {
		doc, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
		if err != nil {
			return Status{}, err
		}
		out.AuthorityEpoch, out.AuthorityInstanceID = doc.AuthorityEpoch, doc.Authority.InstanceID
		out.RecoveryKeyFingerprint = KeyFingerprint(doc.RecoveryPublicKey)
		head := &DocumentHead{Generation: st.AppliedGeneration, Hash: st.AppliedHash, IssuedAt: doc.IssuedAt}
		if st.AppliedAt != nil {
			head.AppliedAt = *st.AppliedAt
		}
		// Age is measured from the last confirmation that this generation is
		// current (a successful sync), not from when it was first applied.
		fresh := head.AppliedAt
		if st.LastSyncSuccessAt != nil && st.LastSyncSuccessAt.After(fresh) {
			fresh = *st.LastSyncSuccessAt
		}
		if st.Role == "authority" {
			fresh = s.now()
		}
		head.AgeSeconds = int64(s.now().Sub(fresh) / time.Second)
		if head.AgeSeconds < 0 {
			head.AgeSeconds = 0
		}
		out.Membership = head
		for _, m := range doc.Members {
			switch m.Status {
			case "active":
				out.Members.Active++
			case "suspended":
				out.Members.Suspended++
			default:
				out.Members.Removed++
			}
		}
		out.Health = "ok"
		if st.State != "active" {
			out.Health = "disabled"
		} else if time.Duration(head.AgeSeconds)*time.Second > StaleWarning || st.LastSyncError != "" {
			out.Health = "degraded"
		}
		if st.State == "active" {
			ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
			if err != nil {
				return Status{}, err
			}
			out.Policy = s.policyStatus(st, ps)
			if out.Topology, err = s.topologyStatus(ctx, st); err != nil {
				return Status{}, err
			}
			out.Alerts = computeAlerts(st, ps, out.Policy, s.now())
			out.Alerts = append(out.Alerts, topologyAlerts(out.Topology)...)
			sortAlerts(out.Alerts)
			if len(out.Alerts) > 0 {
				out.Health = "degraded"
			}
		}
	}
	return out, nil
}

// MemberView is one member as seen by this instance.
type MemberView struct {
	InstanceID      string            `json:"instance_id"`
	Name            string            `json:"name"`
	Status          string            `json:"status"`
	APIURL          string            `json:"api_url,omitempty"`
	FederationURL   string            `json:"federation_url"`
	CertSHA256      string            `json:"cert_sha256,omitempty"`
	CertSerial      string            `json:"cert_serial,omitempty"`
	Labels          map[string]string `json:"labels"`
	SoftwareVersion string            `json:"software_version,omitempty"`
	Protocol        ProtocolRange     `json:"protocol"`
	JoinedAt        *time.Time        `json:"joined_at,omitempty"`
	Authority       bool              `json:"authority"`
	Self            bool              `json:"self"`
	LastSeenAt      *time.Time        `json:"last_seen_at,omitempty"`
	RTTMS           *int32            `json:"rtt_ms,omitempty"`
	SkewMS          *int32            `json:"skew_ms,omitempty"`
	Generation      int64             `json:"membership_generation"`
}

// Members lists the applied membership with local observations.
func (s *Service) Members(ctx context.Context) ([]MemberView, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	out := []MemberView{}
	if st.FederationID == "" || st.AppliedGeneration == 0 {
		return out, nil
	}
	doc, err := loadDocument(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, st.AppliedGeneration)
	if err != nil {
		return nil, err
	}
	rows, err := s.cfg.DB.Query(ctx, `SELECT member_instance_id::text,name,status,api_url,federation_url,cert_sha256,cert_serial,labels,software_version,protocol_min,protocol_max,joined_at,last_seen_at,rtt_ms,skew_ms,membership_generation
FROM federation_members WHERE instance_id=$1 AND federation_id=$2 ORDER BY (status='removed'),name,member_instance_id`, s.cfg.InstanceID, st.FederationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MemberView
		var labels []byte
		if err := rows.Scan(&m.InstanceID, &m.Name, &m.Status, &m.APIURL, &m.FederationURL, &m.CertSHA256, &m.CertSerial, &labels, &m.SoftwareVersion,
			&m.Protocol.Min, &m.Protocol.Max, &m.JoinedAt, &m.LastSeenAt, &m.RTTMS, &m.SkewMS, &m.Generation); err != nil {
			return nil, err
		}
		m.Labels = map[string]string{}
		_ = json.Unmarshal(labels, &m.Labels)
		m.Authority = m.InstanceID == doc.Authority.InstanceID
		m.Self = m.InstanceID == s.cfg.InstanceID
		out = append(out, m)
	}
	return out, rows.Err()
}

// Member returns one member.
func (s *Service) Member(ctx context.Context, id string) (MemberView, error) {
	all, err := s.Members(ctx)
	if err != nil {
		return MemberView{}, err
	}
	for _, m := range all {
		if m.InstanceID == id {
			return m, nil
		}
	}
	return MemberView{}, ErrNotFound
}

// StoredDocument is a document from the local log. Document is a
// *Membership or a *PolicyDocument depending on Kind.
type StoredDocument struct {
	Kind       string    `json:"kind"`
	Generation int64     `json:"generation"`
	Hash       string    `json:"hash"`
	ReceivedAt time.Time `json:"received_at"`
	Applied    bool      `json:"applied"`
	Document   any       `json:"document"`
}

// Document returns the applied document of a kind (generation 0) or a
// specific generation from the local log.
func (s *Service) Document(ctx context.Context, kind string, generation int64) (StoredDocument, error) {
	if kind != "membership" && kind != "policy" && kind != "topology" {
		return StoredDocument{}, ErrNotFound
	}
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return StoredDocument{}, err
	}
	if st.FederationID == "" || st.AppliedGeneration == 0 {
		return StoredDocument{}, ErrNotConfigured
	}
	current := st.AppliedGeneration
	if kind == "policy" {
		ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			return StoredDocument{}, err
		}
		current = ps.Generation
	}
	if kind == "topology" {
		ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			return StoredDocument{}, err
		}
		current = ts.Generation
	}
	if generation == 0 {
		generation = current
	}
	if generation == 0 {
		return StoredDocument{}, ErrNotFound
	}
	var out StoredDocument
	var body []byte
	err = s.cfg.DB.QueryRow(ctx, `SELECT generation,hash,received_at,body FROM federation_documents WHERE instance_id=$1 AND federation_id=$2 AND kind=$4 AND generation=$3`,
		s.cfg.InstanceID, st.FederationID, generation, kind).Scan(&out.Generation, &out.Hash, &out.ReceivedAt, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredDocument{}, ErrNotFound
	}
	if err != nil {
		return StoredDocument{}, err
	}
	out.Kind = kind
	out.Applied = generation == current
	if kind == "topology" {
		d := &TopologyDocument{}
		out.Document = d
		return out, json.Unmarshal(body, d)
	}
	if kind == "policy" {
		d := &PolicyDocument{}
		out.Document = d
		return out, json.Unmarshal(body, d)
	}
	d := &Membership{}
	out.Document = d
	return out, json.Unmarshal(body, d)
}

// tx runs fn in a read-committed transaction.
func (s *Service) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.cfg.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func authorityPurpose(epoch int64) string { return fmt.Sprintf("federation-authority:%d", epoch) }

// logOnce rate-limits repeated log lines per key to one per minute.
func (s *Service) logOnce(key string, msg string, args ...any) {
	s.logMu.Lock()
	last, ok := s.logged[key]
	if ok && s.cfg.Now().Sub(last) < time.Minute {
		s.logMu.Unlock()
		return
	}
	s.logged[key] = s.cfg.Now()
	s.logMu.Unlock()
	s.cfg.Logger.Warn(msg, args...)
}

// authorityID is the authority instance of the applied membership.
func (t *trust) authorityID() string {
	if t == nil || t.doc == nil {
		return ""
	}
	return t.doc.Authority.InstanceID
}

// throttle allows one event per key and window. It returns whether this
// event may be recorded and how many were suppressed since the last one.
// Keys must come from a bounded set (reasons, member IDs).
func (s *Service) throttle(key string, window time.Duration) (bool, int) {
	now := s.cfg.Now()
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.suppressed == nil {
		s.suppressed = map[string]int{}
	}
	if last, ok := s.logged[key]; ok && now.Sub(last) < window {
		s.suppressed[key]++
		return false, 0
	}
	s.logged[key] = now
	n := s.suppressed[key]
	delete(s.suppressed, key)
	return true, n
}
