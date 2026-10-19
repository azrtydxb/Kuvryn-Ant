package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Federated audit query (docs/planning/phase-2-federation.md, Audit). Each
// member keeps its own audit stream; the entry member fans the query out like
// federated search, with a signed assertion of the caller, and merges the
// answers newest first. Every member evaluates the asserted principal against
// its own policy: reading a member's audit needs instance administration on
// that member (the same grant as GET /api/v1/audit), so a federated
// administrator whose federated admin grants are inert on a member (see
// accept_admin_grants) or who has no admin role there sees nothing from it, and
// that member is reported as forbidden. Local principals cannot use it.

// Audit query limits.
const (
	MaxAuditLimit        = 200
	maxAuditDetailsBytes = 8 << 10
	// AuditPeerRate is the receiving member's per-calling-member limit.
	AuditPeerRate = 20
)

// ErrAuditForbidden reports that the asserted principal may not read this
// member's audit log.
var ErrAuditForbidden = errors.New("federation: audit read denied")

// AuditMark is the keyset position after the last event of one member that
// was returned: events strictly older than (At, ID) are next.
type AuditMark struct {
	At time.Time `json:"at"`
	ID int64     `json:"id"`
}

// AuditQuery is the filter set of one federated audit query.
type AuditQuery struct {
	Action string     `json:"action,omitempty"` // exact action
	Prefix string     `json:"prefix,omitempty"` // action prefix, for example "federation."
	Result string     `json:"result,omitempty"`
	From   *time.Time `json:"from,omitempty"`
	To     *time.Time `json:"to,omitempty"`
	After  *AuditMark `json:"after,omitempty"` // this member's watermark
	Limit  int        `json:"limit"`
}

// Validate bounds every field.
func (q AuditQuery) Validate() error {
	text := func(v string, max int) bool {
		return len(v) <= max && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
	}
	if !text(q.Action, 128) || !text(q.Prefix, 128) || (q.Result != "" && q.Result != "success" && q.Result != "denied" && q.Result != "failure") ||
		q.Limit < 1 || q.Limit > MaxAuditLimit || (q.From != nil && q.To != nil && !q.To.After(*q.From)) || (q.After != nil && q.After.ID < 1) {
		return invalid("invalid federated audit query")
	}
	return nil
}

// AuditEntry is one audit event as a member reports it.
type AuditEntry struct {
	ID          int64           `json:"id"`
	CreatedAt   time.Time       `json:"created_at"`
	PrincipalID *string         `json:"principal_id,omitempty"`
	Action      string          `json:"action"`
	ResourceID  *string         `json:"resource_id,omitempty"`
	Result      string          `json:"result"`
	Details     json.RawMessage `json:"details,omitempty"`
}

// MergedAuditEntry is an audit event with the member it comes from.
type MergedAuditEntry struct {
	Member     string `json:"member"`
	MemberName string `json:"member_name"`
	AuditEntry
}

// MemberAuditStatus is the explicit per-member outcome.
type MemberAuditStatus struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ok | forbidden | timeout | error | skipped
	Code   string `json:"code,omitempty"`
	TookMS int64  `json:"took_ms"`
	Events int    `json:"events"`
}

// AuditResult is one merged page.
type AuditResult struct {
	Items   []MergedAuditEntry   `json:"items"`
	Members []MemberAuditStatus  `json:"members"`
	Partial bool                 `json:"partial"`
	More    bool                 `json:"more"`
	Marks   map[string]AuditMark `json:"-"`
}

// AuditSearcher reads this member's audit events for a principal under its
// resolved policy: at most Limit+1 events, newest first, strictly older than
// the watermark. It returns ErrAuditForbidden when the principal may not read
// the audit log.
type AuditSearcher func(ctx context.Context, p identity.Principal, policy identity.Policy, q AuditQuery) ([]AuditEntry, error)

// SetAuditSearcher installs the local audit query (the management package).
func (s *Service) SetAuditSearcher(f AuditSearcher) { s.auditor.Store(&f) }

type peerAuditResponse struct {
	Events []AuditEntry `json:"events"`
	More   bool         `json:"more"`
}

// sortedAuditLess orders events newest first, ties by member name then ID.
func auditLess(a, b MergedAuditEntry) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	if a.MemberName != b.MemberName {
		return a.MemberName < b.MemberName
	}
	return a.ID > b.ID
}

// FederatedAudit runs the audit fan-out for a federated principal. marks are
// the per-member watermarks of the previous page (nil for the first page);
// local computes this member's own events.
func (s *Service) FederatedAudit(ctx context.Context, p identity.Principal, q AuditQuery, members []string, marks map[string]AuditMark,
	local func(context.Context, AuditQuery) ([]AuditEntry, error)) (AuditResult, error) {
	if err := q.Validate(); err != nil {
		return AuditResult{}, err
	}
	_, t, err := s.identity()
	if err != nil {
		return AuditResult{}, err
	}
	self, _ := t.doc.Member(s.cfg.InstanceID)
	policyDoc, _, err := s.currentPolicy(ctx, s.cfg.DB)
	if err != nil {
		return AuditResult{}, err
	}
	if policyDoc == nil || !s.federatedPrincipal(ctx, p) {
		return AuditResult{}, ErrNotFederated
	}
	targets, err := searchTargets(t.doc, s.cfg.InstanceID, members)
	if err != nil {
		return AuditResult{}, err
	}
	// The local member is queried unless the caller restricted the members to
	// others only.
	includeSelf := len(members) == 0
	for _, f := range members {
		if f == s.cfg.InstanceID || f == self.Name {
			includeSelf = true
		}
	}
	conc, memberTimeout, overall, _, _, _ := s.searchSetting()
	ctx, cancel := context.WithTimeout(ctx, overall+time.Second)
	defer cancel()

	type answer struct {
		status MemberAuditStatus
		events []AuditEntry
		more   bool
	}
	answers := make([]answer, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for i, m := range targets {
		answers[i].status = MemberAuditStatus{ID: m.InstanceID, Name: m.Name, Status: "skipped", Code: "deadline"}
		mq := q
		if mark, ok := marks[m.InstanceID]; ok {
			mq.After = &mark
		}
		body, _ := json.Marshal(mq)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			start := time.Now()
			st := &answers[i].status
			finish := func(status, code string) {
				st.Status, st.Code, st.TookMS = status, code, time.Since(start).Milliseconds()
			}
			mctx, mcancel := context.WithTimeout(ctx, memberTimeout+time.Second)
			defer mcancel()
			req, err := s.Assert(mctx, p, m.InstanceID, "POST", "/federation/v1/audit/query", body)
			if err == nil {
				var raw []byte
				if raw, err = s.SendAsserted(mctx, req); err == nil {
					var resp peerAuditResponse
					if json.Unmarshal(raw, &resp) != nil || len(resp.Events) > q.Limit+1 {
						finish("error", "malformed_response")
						return
					}
					for _, e := range resp.Events {
						if e.ID < 1 || e.Action == "" {
							finish("error", "malformed_response")
							return
						}
					}
					answers[i].events, answers[i].more, st.Events = resp.Events, resp.More, len(resp.Events)
					finish("ok", "")
					return
				}
			}
			var pe *peer.ErrPeer
			var ne net.Error
			switch {
			case errors.As(err, &pe):
				switch pe.Code {
				case "principal_unknown", "assertion_unknown_domain":
					finish("skipped", "no_identity")
				case "forbidden":
					finish("forbidden", "audit_read_denied")
				case "":
					finish("error", "peer_error")
				default:
					finish("error", pe.Code)
				}
			case errors.Is(err, ErrNotFederated):
				finish("error", "not_federated")
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &ne) && ne.Timeout()):
				finish("timeout", "deadline")
			default:
				finish("error", "unreachable")
			}
		}()
	}

	res := AuditResult{Items: []MergedAuditEntry{}, Members: []MemberAuditStatus{}, Marks: map[string]AuditMark{}}
	var all []MergedAuditEntry
	more := false
	if includeSelf {
		st := MemberAuditStatus{ID: s.cfg.InstanceID, Name: self.Name, Status: "ok"}
		start := time.Now()
		lq := q
		if mark, ok := marks[s.cfg.InstanceID]; ok {
			lq.After = &mark
		}
		events, lerr := local(ctx, lq)
		switch {
		case errors.Is(lerr, ErrAuditForbidden):
			st.Status, st.Code = "forbidden", "audit_read_denied"
		case lerr != nil:
			st.Status, st.Code = "error", "unavailable"
		}
		st.TookMS, st.Events = time.Since(start).Milliseconds(), len(events)
		res.Members = append(res.Members, st)
		if st.Status == "ok" {
			more = more || len(events) > q.Limit
			for _, e := range events[:min(len(events), q.Limit)] {
				all = append(all, MergedAuditEntry{Member: s.cfg.InstanceID, MemberName: self.Name, AuditEntry: e})
			}
		}
	}
	wg.Wait()
	for i, a := range answers {
		res.Members = append(res.Members, a.status)
		if a.status.Status != "ok" {
			continue
		}
		more = more || a.more || len(a.events) > q.Limit
		for _, e := range a.events[:min(len(a.events), q.Limit)] {
			all = append(all, MergedAuditEntry{Member: targets[i].InstanceID, MemberName: targets[i].Name, AuditEntry: e})
		}
	}
	for _, m := range res.Members {
		if m.Status != "ok" {
			res.Partial = true
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return auditLess(all[i], all[j]) })
	if len(all) > q.Limit {
		all, more = all[:q.Limit], true
	}
	res.Items, res.More = append(res.Items, all...), more
	for k, v := range marks {
		res.Marks[k] = v // members with nothing consumed keep their position
	}
	for _, e := range all {
		res.Marks[e.Member] = AuditMark{At: e.CreatedAt, ID: e.ID} // items are newest first: the last one per member wins
	}
	return res, nil
}

// peerAudit answers POST /federation/v1/audit/query: the asserted principal's
// view of this member's audit log under this member's own policy.
func (s *Service) peerAudit(w http.ResponseWriter, r *http.Request) {
	_, _, _, _, peerRate, principalRate := s.searchSetting()
	caller, _ := peer.Caller(r)
	if !s.allow("audit-peer:"+caller.InstanceID, min(peerRate, AuditPeerRate)) {
		peerFail(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	body, ok := readPeerBody(w, r)
	if !ok {
		return
	}
	p, a, reason := s.authenticateAssertion(r, body)
	if reason != "" {
		s.assertionFailed(w, r, a, reason)
		return
	}
	if !s.allow("audit-principal:"+a.Issuer+"/"+p.PrincipalID, min(principalRate, AuditPeerRate)) {
		peerFail(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var q AuditQuery
	if json.Unmarshal(body, &q) != nil || q.Validate() != nil {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	f := s.auditor.Load()
	if f == nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	s.assertionAccepted(r.Context(), p, a, r.URL.Path)
	policy, err := (identity.Roles{DB: s.cfg.DB}).Resolve(r.Context(), p)
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	events, err := (*f)(r.Context(), p, policy, q)
	if errors.Is(err, ErrAuditForbidden) {
		peerFail(w, http.StatusForbidden, "forbidden")
		return
	}
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	out := peerAuditResponse{Events: events}
	if len(events) > q.Limit {
		out.Events, out.More = events[:q.Limit], true
	}
	if out.Events == nil {
		out.Events = []AuditEntry{}
	}
	for i := range out.Events {
		if len(out.Events[i].Details) > maxAuditDetailsBytes {
			out.Events[i].Details = json.RawMessage(`{"truncated":true}`)
		}
	}
	peerJSON(w, http.StatusOK, out)
}
