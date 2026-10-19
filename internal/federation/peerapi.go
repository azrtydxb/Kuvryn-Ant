package federation

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

func x509VerifyOptions(pool *x509.CertPool) x509.VerifyOptions {
	return x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
}

type peerStatus struct {
	InstanceID      string        `json:"instance_id"`
	Name            string        `json:"name"`
	State           string        `json:"state"`
	TimeUnixMS      int64         `json:"time_unix_ms"`
	SoftwareVersion string        `json:"software_version"`
	Protocol        ProtocolRange `json:"protocol"`
	Membership      struct {
		Generation int64  `json:"generation"`
		Hash       string `json:"hash"`
	} `json:"membership"`
}

func peerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func peerFail(w http.ResponseWriter, status int, code string) {
	peerJSON(w, status, map[string]string{"code": code})
}

// peerErr maps service errors onto stable peer error codes.
func peerErr(w http.ResponseWriter, err error) {
	var in *InputError
	var rej *RejectedError
	switch {
	case errors.Is(err, ErrInvitationInvalid):
		peerFail(w, http.StatusForbidden, "invitation_invalid")
	case errors.Is(err, ErrDuplicateInstance):
		peerFail(w, http.StatusConflict, "duplicate_instance")
	case errors.Is(err, ErrNotAuthority):
		peerFail(w, http.StatusConflict, "not_authority")
	case errors.Is(err, ErrNotConfigured):
		peerFail(w, http.StatusConflict, "federation_not_configured")
	case errors.Is(err, ErrProtocol):
		peerFail(w, http.StatusConflict, "protocol_unsupported")
	case errors.Is(err, ErrSelf):
		peerFail(w, http.StatusConflict, "conflict")
	case errors.As(err, &rej):
		peerFail(w, http.StatusConflict, rej.Reason)
	case errors.As(err, &in):
		peerFail(w, http.StatusBadRequest, "invalid_request")
	default:
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
	}
}

// PeerHandler is the federation listener's handler. It must only be served
// by the dedicated mTLS listener (Serve), never by the public mux.
func (s *Service) PeerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /federation/v1/join", s.peerJoin)
	mux.HandleFunc("GET /federation/v1/heads", s.peerHeads)
	mux.HandleFunc("GET /federation/v1/documents/{kind}", s.peerDocuments)
	mux.HandleFunc("POST /federation/v1/nudge", s.peerNudge)
	mux.HandleFunc("POST /federation/v1/leave", s.peerLeave)
	mux.HandleFunc("POST /federation/v1/csr/renew", s.peerRenew)
	mux.HandleFunc("GET /federation/v1/status", s.peerStatus)
	mux.HandleFunc("POST /federation/v1/access-check", s.peerAccessCheck)
	mux.HandleFunc("POST /federation/v1/search", s.peerSearch)

	mux.HandleFunc("GET /federation/v1/replication/feeds/{fed}/{slug}", s.peerHomeFeed)
	mux.HandleFunc("GET /federation/v1/replication/transfer-state", s.peerTransferState)
	mux.HandleFunc("GET /federation/v1/replication/status", s.peerReplicationStatus)
	// Placement-aware routing (P2-M5): reads only; writes are never routed.
	mux.HandleFunc("GET /federation/v1/routes/feeds/{fed}/{slug}", s.peerRouteInfo)
	mux.HandleFunc("GET /federation/v1/proxy/feeds/{fed}/{slug}/{rest...}", s.peerProxy)
	mux.HandleFunc("/federation/v1/proxy/", s.peerProxy)
	s.peerMu.Lock()
	for pattern, h := range s.peerRoutes {
		mux.Handle(pattern, h)
	}
	s.peerMu.Unlock()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { peerFail(w, http.StatusNotFound, "not_found") })
	return &peer.Handler{Trust: s, Mux: mux, Public: map[string]bool{"POST /federation/v1/join": true},
		// A nudge may carry the generation that announces the sender's renewed
		// certificate; peerNudge re-checks the pin after applying it.
		DeferPin: map[string]bool{"POST /federation/v1/nudge": true}, Logger: s.cfg.Logger, Reject: s.peerRejected}
}

// peerRejected audits refused peer requests (rate-limited per peer and reason).
func (s *Service) peerRejected(r *http.Request, member, reason string) {
	key := "peer:" + member + ":" + reason
	s.logMu.Lock()
	last, seen := s.logged[key]
	recent := seen && s.cfg.Now().Sub(last) < time.Minute
	if !recent {
		s.logged[key] = s.cfg.Now()
	}
	s.logMu.Unlock()
	if recent {
		return
	}
	if !uuidPattern.MatchString(member) {
		member = ""
	}
	ctx := context.WithoutCancel(r.Context())
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.peer.rejected", member, "denied", map[string]any{"reason": reason, "path": r.URL.Path, "remote": r.RemoteAddr})
	})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, peer.MaxBody))
	if err != nil || json.Unmarshal(b, v) != nil {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func (s *Service) peerJoin(w http.ResponseWriter, r *http.Request) {
	var req JoinRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := s.acceptJoin(ctx, req)
	if err != nil {
		peerErr(w, err)
		return
	}
	peerJSON(w, http.StatusOK, resp)
}

func (s *Service) peerHeads(w http.ResponseWriter, r *http.Request) {
	t := s.trust.Load()
	if t.doc == nil {
		peerFail(w, http.StatusConflict, "federation_not_configured")
		return
	}
	var h heads
	h.FederationID = t.doc.FederationID
	h.Membership.Generation, h.Membership.Hash, h.Membership.AuthorityEpoch = t.doc.Generation, Hash(t.doc), t.doc.AuthorityEpoch
	var err error
	if h.Policy, err = s.localPolicyHead(r.Context(), t.state.Role); err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if h.Topology, err = s.localTopologyHead(r.Context(), t.state.Role); err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	peerJSON(w, http.StatusOK, h)
}

func (s *Service) peerDocuments(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind != "membership" && kind != "policy" && kind != "topology" {
		peerFail(w, http.StatusNotFound, "not_found")
		return
	}
	t := s.trust.Load()
	if t.doc == nil {
		peerFail(w, http.StatusConflict, "federation_not_configured")
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		after = 0
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 100 {
		limit = 100
	}
	var docs any
	switch kind {
	case "policy":
		docs, err = policyDocumentsAfter(r.Context(), s.cfg.DB, s.cfg.InstanceID, t.doc.FederationID, after, min(limit, 50))
	case "topology":
		docs, err = topologyDocumentsAfter(r.Context(), s.cfg.DB, s.cfg.InstanceID, t.doc.FederationID, after, min(limit, 50))
	default:
		docs, err = documentsAfter(r.Context(), s.cfg.DB, s.cfg.InstanceID, t.doc.FederationID, after, limit)
	}
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	peerJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// peerNudge accepts documents pushed by the authority and applies the ones
// that extend the local chain; a gap triggers a full sync.
func (s *Service) peerNudge(w http.ResponseWriter, r *http.Request) {
	caller, _ := peer.Caller(r)
	t := s.trust.Load()
	if t.doc == nil || caller.InstanceID != t.doc.Authority.InstanceID {
		peerFail(w, http.StatusForbidden, "not_authority")
		return
	}
	var body struct {
		Documents         []Membership       `json:"documents"`
		PolicyDocuments   []PolicyDocument   `json:"policy_documents"`
		TopologyDocuments []TopologyDocument `json:"topology_documents"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	pending := []Membership{}
	for _, d := range body.Documents {
		if d.Generation > t.doc.Generation {
			pending = append(pending, d)
		}
	}
	if len(pending) > 0 && pending[0].Generation == t.doc.Generation+1 {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if _, err := s.applyDocuments(ctx, pending, caller.Name, false); err != nil {
			peerErr(w, err)
			return
		}
	} else if len(pending) > 0 {
		s.Nudge()
	}
	// The sender presented a certificate this member did not accept yet.
	// Membership documents are self-authenticating, so applying them was safe;
	// everything else needs the sender to be pinned by the applied membership.
	if peer.PinPending(r) && (!peer.Pinned(s, r, caller.InstanceID) || caller.InstanceID != s.trust.Load().authorityID()) {
		s.peerRejected(r, caller.InstanceID, "certificate_not_current")
		peerFail(w, http.StatusForbidden, "certificate_not_current")
		return
	}
	if len(body.PolicyDocuments) > 0 {
		ps, err := loadPolicyState(r.Context(), s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			peerErr(w, err)
			return
		}
		next := []PolicyDocument{}
		for _, d := range body.PolicyDocuments {
			if d.Generation > ps.Generation {
				next = append(next, d)
			}
		}
		if len(next) > 0 && next[0].Generation == ps.Generation+1 {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			// Pushed by the authority itself: current as of now.
			if _, err := s.applyPolicyDocuments(ctx, next, caller.Name, s.now()); err != nil {
				peerErr(w, err)
				return
			}
		} else if len(next) > 0 {
			s.Nudge()
		}
	}
	if len(body.TopologyDocuments) > 0 {
		ts, err := loadTopologyState(r.Context(), s.cfg.DB, s.cfg.InstanceID)
		if err != nil {
			peerErr(w, err)
			return
		}
		next := []TopologyDocument{}
		for _, d := range body.TopologyDocuments {
			if d.Generation > ts.Generation {
				next = append(next, d)
			}
		}
		if len(next) > 0 && next[0].Generation == ts.Generation+1 {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			if _, err := s.applyTopologyDocuments(ctx, next, caller.Name, s.now()); err != nil {
				peerErr(w, err)
				return
			}
		} else if len(next) > 0 {
			s.Nudge()
		}
	}
	peerJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

// peerLeave runs on the authority: the calling member is removed and its
// certificate serial revoked.
func (s *Service) peerLeave(w http.ResponseWriter, r *http.Request) {
	caller, _ := peer.Caller(r)
	var body struct {
		After int64 `json:"after"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var doc *Membership
	var docs []Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		doc, err = s.publish(ctx, tx, st, applied, "member:"+caller.InstanceID, func(next *Membership) error {
			for i := range next.Members {
				if m := &next.Members[i]; m.InstanceID == caller.InstanceID {
					m.Status = "removed"
					if m.CertSerial != "" && !next.Revoked(m.CertSerial) {
						next.RevokedCertSerials = append(next.RevokedCertSerials, m.CertSerial)
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.member.left", caller.InstanceID, "success", map[string]any{"name": caller.Name, "generation": doc.Generation}); err != nil {
			return err
		}
		docs, err = documentsAfter(ctx, tx, s.cfg.InstanceID, st.FederationID, max(0, body.After), 100)
		return err
	})
	if err != nil {
		peerErr(w, err)
		return
	}
	s.afterPublish(ctx, doc)
	peerJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// peerRenew runs on the authority: issue a fresh certificate for the caller.
func (s *Service) peerRenew(w http.ResponseWriter, r *http.Request) {
	caller, _ := peer.Caller(r)
	var body struct {
		CSR string `json:"csr"`
		// CSRSignature is required when the CSR carries a new key: the
		// current certificate key's signature over CSRRotationMessage(csr).
		CSRSignature string `json:"csr_signature,omitempty"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := checkRenewalKey(r.TLS.PeerCertificates[0], body.CSR, body.CSRSignature); err != nil {
		s.peerRejected(r, caller.InstanceID, "csr_key_mismatch")
		peerFail(w, http.StatusForbidden, "csr_key_mismatch")
		return
	}
	presented := peer.LeafSHA256(r)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var resp JoinResponse
	var doc *Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, applied, err := s.authorityState(ctx, tx)
		if err != nil {
			return err
		}
		m, ok := applied.Member(caller.InstanceID)
		if !ok || m.Status != "active" {
			return ErrNotFound
		}
		after := applied.Generation
		certPEM, serial, sha, _, err := s.issueFor(ctx, st, body.CSR, caller.InstanceID, m.FederationURL)
		if err != nil {
			return invalid("invalid certificate signing request")
		}
		doc, err = s.publish(ctx, tx, st, applied, "renewal:"+caller.InstanceID, setCert(caller.InstanceID, serial, sha, presented, s.now()))
		if err != nil {
			return err
		}
		if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.certificate.issued", caller.InstanceID, "success", map[string]any{"serial": serial, "generation": doc.Generation}); err != nil {
			return err
		}
		resp.Certificate = certPEM
		resp.Documents, err = documentsAfter(ctx, tx, s.cfg.InstanceID, st.FederationID, max(0, after-16), 100)
		return err
	})
	if err != nil {
		peerErr(w, err)
		return
	}
	s.afterPublish(ctx, doc)
	peerJSON(w, http.StatusOK, resp)
}

func (s *Service) peerStatus(w http.ResponseWriter, r *http.Request) {
	t := s.trust.Load()
	out := peerStatus{InstanceID: s.cfg.InstanceID, State: t.state.State, TimeUnixMS: s.cfg.Now().UnixMilli(), SoftwareVersion: s.cfg.SoftwareVersion,
		Protocol: ProtocolRange{ProtocolMin, ProtocolMax}}
	if t.doc != nil {
		if m, ok := t.doc.Member(s.cfg.InstanceID); ok {
			out.Name = m.Name
		}
		out.Membership.Generation, out.Membership.Hash = t.doc.Generation, Hash(t.doc)
	}
	peerJSON(w, http.StatusOK, out)
}

// Serve runs the dedicated federation listener (TLS 1.3 mTLS) until ctx
// ends. It is separate from the public HTTP server by construction.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.PeerHandler(),
		TLSConfig:         peer.ServerTLSConfig(s),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(sctx)
			cancel()
		case <-done:
		}
	}()
	err := srv.ServeTLS(ln, "", "")
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
