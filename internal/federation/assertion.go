package federation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// --- issuing (entry member) ---

// AssertedRequest is a signed cross-member request for a caller.
type AssertedRequest struct {
	Member string
	Method string
	Path   string
	Body   []byte
	Header string // X-Ant-Fed-Assertion value
}

// currentPolicy returns the applied policy document (cached).
func (s *Service) currentPolicy(ctx context.Context, q queryer) (*PolicyDocument, string, error) {
	st, err := loadState(ctx, q, s.cfg.InstanceID)
	if err != nil {
		return nil, "", err
	}
	if st.State != "active" || st.FederationID == "" {
		return nil, "", ErrNotConfigured
	}
	if d := s.policyCache.Load(); d != nil && d.FederationID == st.FederationID {
		return d, st.FederationID, nil
	}
	ps, err := loadPolicyState(ctx, q, s.cfg.InstanceID)
	if err != nil {
		return nil, "", err
	}
	d, err := loadPolicy(ctx, q, s.cfg.InstanceID, st.FederationID, ps.Generation)
	if err != nil {
		return nil, "", err
	}
	if d != nil {
		s.policyCache.Store(d)
	}
	return d, st.FederationID, nil
}

// Assert builds an assertion of a local federated principal for a request to
// another member. Local-only principals are refused with ErrNotFederated.
func (s *Service) Assert(ctx context.Context, p identity.Principal, member, method, path string, body []byte) (AssertedRequest, error) {
	_, t, err := s.identity()
	if err != nil {
		return AssertedRequest{}, err
	}
	if m, ok := t.doc.Member(member); !ok || m.Status != "active" || member == s.cfg.InstanceID {
		return AssertedRequest{}, ErrNotFound
	}
	if p.InstanceID != s.cfg.InstanceID || p.PrincipalID == "" || p.Anonymous || p.Delegated {
		return AssertedRequest{}, ErrNotFederated
	}
	policy, fid, err := s.currentPolicy(ctx, s.cfg.DB)
	if err != nil {
		return AssertedRequest{}, err
	}
	var origin, issuer, subject string
	var disabled bool
	var federatedID *string
	err = s.cfg.DB.QueryRow(ctx, `SELECT origin,issuer,subject,disabled,federated_id::text FROM principals WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, p.PrincipalID).
		Scan(&origin, &issuer, &subject, &disabled, &federatedID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (origin != "federation" || disabled || policy == nil)) {
		return AssertedRequest{}, ErrNotFederated
	}
	if err != nil {
		return AssertedRequest{}, err
	}
	a := peer.Assertion{Issuer: s.cfg.InstanceID, Federation: fid, Subject: subject, Audience: member}
	if federatedID != nil {
		a.FederatedID = *federatedID
	}
	if issuer == ServiceAccountIssuer(fid) {
		a.Domain = ServiceAccountDomain
	} else if d, ok := policy.DomainByIssuer(issuer); ok {
		a.Domain = d.ID
	} else {
		return AssertedRequest{}, ErrNotFederated
	}
	if p.TokenID != "" {
		bindings, err := s.feedBindings(ctx, s.cfg.DB)
		if err != nil {
			return AssertedRequest{}, err
		}
		keys := map[string]string{}
		for k, id := range bindings {
			keys[id] = k
		}
		a.Scopes = []peer.AssertedScope{}
		for _, sc := range p.Scopes {
			out := peer.AssertedScope{Namespace: sc.Namespace, Actions: []string{}}
			if sc.FeedID != "" {
				key, ok := keys[sc.FeedID]
				if !ok {
					continue // a feed without a federation key cannot be asserted
				}
				out.FeedKey = key
			}
			for _, act := range sc.Actions {
				out.Actions = append(out.Actions, string(act))
			}
			a.Scopes = append(a.Scopes, out)
		}
	}
	now := s.cfg.Now()
	a.IssuedAt, a.Expires = now.Unix(), now.Add(peer.AssertionLifetime).Unix()
	a.Request = peer.RequestHash(method, path, body)
	header, err := peer.SignAssertion(a, t.signer)
	if err != nil {
		return AssertedRequest{}, err
	}
	s.metrics.issued.Add(1)
	return AssertedRequest{Member: member, Method: method, Path: path, Body: body, Header: header}, nil
}

// SendAsserted sends an asserted request to its member over mTLS.
func (s *Service) SendAsserted(ctx context.Context, req AssertedRequest) ([]byte, error) {
	id, t, err := s.identity()
	if err != nil {
		return nil, err
	}
	m, ok := t.doc.Member(req.Member)
	if !ok {
		return nil, ErrNotFound
	}
	return s.client.DoHeader(ctx, id, m.FederationURL, req.Method, req.Path, req.Body, m.InstanceID, map[string]string{peer.HeaderAssertion: req.Header})
}

// AccessCheckInput asks another member whether the caller may act there.
type AccessCheckInput struct {
	Member    string `json:"member_id"`
	Kind      string `json:"kind"`               // artifact | feed | instance
	FeedKey   string `json:"feed_key,omitempty"` // required for artifact and feed
	Namespace string `json:"namespace,omitempty"`
	Action    string `json:"action"`
}

// AccessCheckResult is the remote member's own decision.
type AccessCheckResult struct {
	Member           string `json:"member_id"`
	Allowed          bool   `json:"allowed"`
	Reason           string `json:"reason,omitempty"` // feed_not_bound | principal_unknown | denied
	PolicyGeneration int64  `json:"policy_generation"`
	FederatedID      string `json:"federated_id,omitempty"`
}

// CheckAccess asserts the caller to a member and returns that member's
// decision under its own policy (local overrides included).
func (s *Service) CheckAccess(ctx context.Context, p identity.Principal, in AccessCheckInput) (AccessCheckResult, error) {
	if !validAction(in.Action) || (in.Kind != "artifact" && in.Kind != "feed" && in.Kind != "instance") || (in.Kind == "instance") != (in.FeedKey == "") ||
		(in.Namespace != "" && in.Kind != "artifact") || len(in.Namespace) > 200 {
		return AccessCheckResult{}, invalid("kind (artifact, feed, instance), feed_key for artifact and feed, and a valid action are required")
	}
	body, _ := json.Marshal(map[string]string{"kind": in.Kind, "feed_key": in.FeedKey, "namespace": in.Namespace, "action": in.Action})
	req, err := s.Assert(ctx, p, in.Member, "POST", "/federation/v1/access-check", body)
	if err != nil {
		return AccessCheckResult{}, err
	}
	raw, err := s.SendAsserted(ctx, req)
	if err != nil {
		var pe *peer.ErrPeer
		if errors.As(err, &pe) {
			return AccessCheckResult{}, &RejectedError{Reason: pe.Code, Err: err}
		}
		return AccessCheckResult{}, &UnreachableError{Peer: in.Member, Err: err}
	}
	var out AccessCheckResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return AccessCheckResult{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	out.Member = in.Member
	return out, nil
}

func validAction(a string) bool { return validActions[a] }

// --- verifying (receiving member) ---

// authenticateAssertion verifies the assertion on a peer request (after the
// listener verified mTLS and the member request signature) and maps it to the
// local federated principal. Replays are refused through the durable nonce
// table, so a restart does not reopen the window. Every rejection is audited.
func (s *Service) authenticateAssertion(r *http.Request, body []byte) (identity.Principal, peer.Assertion, string) {
	caller, _ := peer.Caller(r)
	a, err := peer.VerifyAssertion(r.Header.Get(peer.HeaderAssertion), caller, s.cfg.InstanceID, s.cfg.Now(), r.Method, r.URL.RequestURI(), body)
	if err != nil {
		return identity.Principal{}, a, peer.AssertionReason(err)
	}
	ctx := r.Context()
	policy, fid, err := s.currentPolicy(ctx, s.cfg.DB)
	if err != nil || policy == nil || a.Federation != fid {
		return identity.Principal{}, a, "assertion_federation_mismatch"
	}
	// The replay record is refused once the issuer holds MaxAssertionNonces
	// unexpired records, so one member cannot grow the table without bound.
	jti := a.Issuer + "/" + a.ID
	tag, err := s.cfg.DB.Exec(ctx, `INSERT INTO federation_assertion_nonces(instance_id,jti,issuer,expires_at)
SELECT $1,$2,$3,$4 WHERE (SELECT count(*) FROM (SELECT 1 FROM federation_assertion_nonces WHERE instance_id=$1 AND issuer=$3 AND expires_at>$5 LIMIT $6) n) < $6
ON CONFLICT DO NOTHING`, s.cfg.InstanceID, jti, a.Issuer, time.Unix(a.Expires, 0).Add(peer.MaxSkew+time.Minute), s.cfg.Now(), MaxAssertionNonces)
	if err != nil {
		return identity.Principal{}, a, "unavailable"
	}
	if tag.RowsAffected() != 1 {
		var seen bool
		if err := s.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_assertion_nonces WHERE instance_id=$1 AND jti=$2)`, s.cfg.InstanceID, jti).Scan(&seen); err != nil {
			return identity.Principal{}, a, "unavailable"
		}
		if seen {
			return identity.Principal{}, a, "assertion_replayed"
		}
		return identity.Principal{}, a, "rate_limited"
	}
	issuer := ""
	if a.Domain == ServiceAccountDomain {
		issuer = ServiceAccountIssuer(fid)
	} else if d, ok := policy.Domain(a.Domain); ok {
		issuer = d.Issuer
	} else {
		return identity.Principal{}, a, "assertion_unknown_domain"
	}
	// The federated ID is derived here, never taken from the issuer's claim.
	a.FederatedID = FederatedID(fid, issuer, a.Subject)
	var id string
	err = s.cfg.DB.QueryRow(ctx, `SELECT id::text FROM principals WHERE instance_id=$1 AND issuer=$2 AND subject=$3 AND origin='federation' AND NOT disabled`, s.cfg.InstanceID, issuer, a.Subject).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Principal{}, a, "principal_unknown"
	}
	if err != nil {
		return identity.Principal{}, a, "unavailable"
	}
	p := identity.Principal{InstanceID: s.cfg.InstanceID, PrincipalID: id, TokenID: "assertion:" + a.ID, Delegated: true}
	if a.Scopes != nil {
		bindings, err := s.feedBindings(ctx, s.cfg.DB)
		if err != nil {
			return identity.Principal{}, a, "unavailable"
		}
		p.Scopes = []identity.Scope{}
		for _, sc := range a.Scopes {
			out := identity.Scope{Namespace: sc.Namespace}
			if sc.FeedKey != "" {
				feed, ok := bindings[sc.FeedKey]
				if !ok {
					continue // scope on a feed this member does not hold: matches nothing
				}
				out.FeedID = feed
			}
			for _, act := range sc.Actions {
				if validAction(act) {
					out.Actions = append(out.Actions, identity.Action(act))
				}
			}
			p.Scopes = append(p.Scopes, out)
		}
	}
	return p, a, ""
}

// assertionRejected audits a refused assertion (rate-limited per peer and reason).
func (s *Service) assertionRejected(r *http.Request, a peer.Assertion, reason string) {
	s.metrics.rejected.Add(1)
	caller, _ := peer.Caller(r)
	key := "assertion:" + caller.InstanceID + ":" + reason
	s.logMu.Lock()
	last, seen := s.logged[key]
	recent := seen && s.cfg.Now().Sub(last) < time.Second
	if !recent {
		s.logged[key] = s.cfg.Now()
	}
	s.logMu.Unlock()
	if recent {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.assertion.rejected", caller.InstanceID, "denied",
			map[string]any{"reason": reason, "path": r.URL.Path, "issuer": a.Issuer, "domain": a.Domain, "subject": a.Subject, "jti": a.ID})
	})
}

func (s *Service) assertionAccepted(ctx context.Context, p identity.Principal, a peer.Assertion, path string) {
	s.metrics.accepted.Add(1)
	// Accepted reads are sampled: at most one audit record per asserted
	// principal and peer per minute.
	key := "assertion-ok:" + a.Issuer + ":" + p.PrincipalID
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
	ctx = context.WithoutCancel(ctx)
	_ = s.tx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.assertion.accepted", p.PrincipalID, "success",
			map[string]any{"path": path, "issuer": a.Issuer, "domain": a.Domain, "subject": a.Subject, "federated_id": a.FederatedID, "sampled": true})
	})
}

// peerAccessCheck evaluates this member's own policy for an asserted principal.
func (s *Service) peerAccessCheck(w http.ResponseWriter, r *http.Request) {
	body, ok := readPeerBody(w, r)
	if !ok {
		return
	}
	p, a, reason := s.authenticateAssertion(r, body)
	if reason != "" {
		s.assertionRejected(r, a, reason)
		status := http.StatusUnauthorized
		switch reason {
		case "principal_unknown", "assertion_unknown_domain":
			status = http.StatusForbidden
		case "rate_limited":
			status = http.StatusTooManyRequests
		case "unavailable":
			status = http.StatusServiceUnavailable
		}
		peerFail(w, status, reason)
		return
	}
	var in struct {
		Kind      string `json:"kind"`
		FeedKey   string `json:"feed_key"`
		Namespace string `json:"namespace"`
		Action    string `json:"action"`
	}
	if json.Unmarshal(body, &in) != nil || !validAction(in.Action) {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	s.assertionAccepted(r.Context(), p, a, r.URL.Path)
	ps, _ := loadPolicyState(r.Context(), s.cfg.DB, s.cfg.InstanceID)
	out := AccessCheckResult{Member: s.cfg.InstanceID, PolicyGeneration: ps.Generation, FederatedID: a.FederatedID}
	res := identity.Resource{InstanceID: s.cfg.InstanceID, Kind: identity.ResourceKind(in.Kind), Namespace: in.Namespace}
	if in.Kind != "instance" {
		bindings, err := s.feedBindings(r.Context(), s.cfg.DB)
		if err != nil {
			peerFail(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		feed, ok := bindings[in.FeedKey]
		if !ok {
			out.Reason = "feed_not_bound"
			peerJSON(w, http.StatusOK, out)
			return
		}
		res.FeedID = feed
	}
	policy, err := (identity.Roles{DB: s.cfg.DB}).Resolve(r.Context(), p)
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	out.Allowed = policy.Authorize(p, res, identity.Action(in.Action))
	if !out.Allowed {
		out.Reason = "denied"
	}
	peerJSON(w, http.StatusOK, out)
}

// readPeerBody reads the (already size-limited) peer request body.
func readPeerBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, peer.MaxBody))
	if err != nil {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return nil, false
	}
	return b, true
}

// MaxAssertionNonces caps the unexpired assertion replay records per issuing
// member (about 110 assertions per second sustained over their lifetime).
const MaxAssertionNonces = 20000

// purgeNonces drops expired assertion nonces in batches until none are left
// (at most 50 batches per run, so one run stays bounded).
func (s *Service) purgeNonces(ctx context.Context) {
	for range 50 {
		tag, err := s.cfg.DB.Exec(ctx, `DELETE FROM federation_assertion_nonces WHERE ctid IN (SELECT ctid FROM federation_assertion_nonces WHERE instance_id=$1 AND expires_at<$2 LIMIT 5000)`, s.cfg.InstanceID, s.cfg.Now())
		if err != nil || tag.RowsAffected() < 5000 {
			return
		}
	}
}
