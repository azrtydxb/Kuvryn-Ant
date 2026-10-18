package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Cross-instance search (docs/planning/phase-2-federation.md section 7). The
// entry member searches locally and sends POST /federation/v1/search with a
// signed principal assertion to the other active members. Every member
// evaluates the asserted principal against its own live policy, so the entry
// member never sees a hit the member would not show that caller. Results are
// merged on (feed_key, coordinate); a member that is slow, down or refuses is
// reported in the per-member status and never fails the query.

// Search defaults (section 7).
const (
	DefaultSearchConcurrency     = 8
	DefaultSearchMemberTimeout   = 1500 * time.Millisecond
	DefaultSearchTimeout         = 2 * time.Second
	DefaultSearchCacheTTL        = 30 * time.Second
	DefaultSearchPeerRate        = 50 // requests per second per calling member
	DefaultSearchPrincipalRate   = 20 // requests per second per asserted principal
	MaxSearchLimit               = 100
	maxSearchMetadata            = 8 << 10
	maxSearchCacheEntries        = 512
	searchRatePrincipalKeyPrefix = "principal:"
)

// SearchQuery is the filter set of one search. Hits are ordered by
// (feed_key, coordinate); After* is the exclusive keyset watermark.
type SearchQuery struct {
	Q               string `json:"q"`
	Format          string `json:"format"`
	Namespace       string `json:"namespace"`
	HasNamespace    bool   `json:"has_namespace"`
	Digest          string `json:"digest"`
	FeedKey         string `json:"feed_key"`
	AfterFeedKey    string `json:"after_feed_key"`
	AfterCoordinate string `json:"after_coordinate"`
	Limit           int    `json:"limit"`
}

// Validate bounds every field.
func (q SearchQuery) Validate() error {
	text := func(v string, max int) bool {
		return len(v) <= max && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
	}
	if !text(q.Q, 256) || !text(q.Format, 32) || !text(q.Namespace, 256) || !text(q.FeedKey, 200) || !text(q.AfterFeedKey, 200) || !text(q.AfterCoordinate, 1024) ||
		(q.Digest != "" && (len(q.Digest) != 64 || strings.ToLower(q.Digest) != q.Digest || !isHex(q.Digest))) || q.Limit < 1 || q.Limit > MaxSearchLimit ||
		(q.Namespace != "" && !q.HasNamespace) {
		return invalid("invalid federated search")
	}
	return nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// SearchHit is one package as a single member reports it.
type SearchHit struct {
	FeedKey    string          `json:"feed_key"`
	FeedSlug   string          `json:"feed_slug"`
	Format     string          `json:"format"`
	Coordinate string          `json:"coordinate"`
	Namespace  string          `json:"namespace"`
	Origin     string          `json:"origin"` // source kind that owns the package: local, remote or replica
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// PackageSearcher runs the member-local package search for a principal under
// its resolved policy, restricted to feeds that carry a federation key
// (bindings: feed key to local feed ID). It returns at most Limit+1 hits
// ordered by (feed_key, coordinate) after the watermark.
type PackageSearcher func(ctx context.Context, p identity.Principal, policy identity.Policy, q SearchQuery, bindings map[string]string) ([]SearchHit, error)

// SetPackageSearcher installs the local search implementation (the
// management package, which owns the package query logic).
func (s *Service) SetPackageSearcher(f PackageSearcher) { s.searcher.Store(&f) }

// SearchCopy is one member holding a hit.
type SearchCopy struct {
	Member     string `json:"member"`
	MemberName string `json:"member_name"`
	Origin     string `json:"origin"`
	Home       bool   `json:"home"`
	APIURL     string `json:"api_url"`
}

// MergedHit is a package merged across members. The primary copy is the
// home copy when one answered, else the first by member name.
type MergedHit struct {
	Member     string          `json:"member"`
	MemberName string          `json:"member_name"`
	FeedKey    string          `json:"feed_key"`
	FeedSlug   string          `json:"feed_slug"`
	Format     string          `json:"format"`
	Coordinate string          `json:"coordinate"`
	Namespace  string          `json:"namespace"`
	Origin     string          `json:"origin"`
	Home       bool            `json:"home"`
	APIURL     string          `json:"api_url"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Copies     []SearchCopy    `json:"copies"`
}

// MemberSearchStatus is the explicit per-member outcome; there is no silent
// truncation.
type MemberSearchStatus struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ok | timeout | error | skipped
	Code   string `json:"code,omitempty"`
	TookMS int64  `json:"took_ms"`
	Hits   int    `json:"hits"`
	Cached bool   `json:"cached,omitempty"`
}

// SearchResult is one merged page.
type SearchResult struct {
	Items   []MergedHit          `json:"items"`
	Members []MemberSearchStatus `json:"members"`
	Partial bool                 `json:"partial"`
	More    bool                 `json:"more"`
}

type peerSearchResponse struct {
	Hits []SearchHit `json:"hits"`
	More bool        `json:"more"`
}

// searchState is the search runtime of a service.
type searchState struct {
	cacheMu sync.Mutex
	cache   map[string]cachedSearch
	limitMu sync.Mutex
	buckets map[string]*bucket

	requests, partial, cacheHits, rateLimited atomic.Int64
}

type cachedSearch struct {
	resp    peerSearchResponse
	expires time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (st *searchState) resetCache() {
	st.cacheMu.Lock()
	st.cache = nil
	st.cacheMu.Unlock()
}

func (s *Service) searchSetting() (conc int, member, overall, ttl time.Duration, peerRate, principalRate int) {
	c := s.cfg
	conc, member, overall, ttl, peerRate, principalRate = c.SearchConcurrency, c.SearchMemberTimeout, c.SearchTimeout, c.SearchCacheTTL, c.SearchPeerRate, c.SearchPrincipalRate
	if conc <= 0 {
		conc = DefaultSearchConcurrency
	}
	if member <= 0 {
		member = DefaultSearchMemberTimeout
	}
	if overall <= 0 {
		overall = DefaultSearchTimeout
	}
	if ttl == 0 {
		ttl = DefaultSearchCacheTTL
	}
	if peerRate <= 0 {
		peerRate = DefaultSearchPeerRate
	}
	if principalRate <= 0 {
		principalRate = DefaultSearchPrincipalRate
	}
	return
}

// allow is a token bucket allowing rate requests per second with a burst of rate.
func (s *Service) allow(key string, rate int) bool {
	st := &s.search
	now := time.Now()
	st.limitMu.Lock()
	defer st.limitMu.Unlock()
	if st.buckets == nil || len(st.buckets) > 4096 {
		st.buckets = map[string]*bucket{}
	}
	b := st.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(rate), last: now}
		st.buckets[key] = b
	}
	b.tokens = min(float64(rate), b.tokens+now.Sub(b.last).Seconds()*float64(rate))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// FederatedSearch runs the fan-out for a federated principal. local computes
// this member's own hits (it runs only after the principal is known to be
// assertable). members optionally restricts the remote members by ID or name.
func (s *Service) FederatedSearch(ctx context.Context, p identity.Principal, q SearchQuery, members []string, local func(context.Context) ([]SearchHit, error)) (SearchResult, error) {
	if err := q.Validate(); err != nil {
		return SearchResult{}, err
	}
	_, t, err := s.identity()
	if err != nil {
		return SearchResult{}, err
	}
	self, _ := t.doc.Member(s.cfg.InstanceID)
	// Prove the caller is a federated principal before doing any work.
	policyDoc, _, err := s.currentPolicy(ctx, s.cfg.DB)
	if err != nil {
		return SearchResult{}, err
	}
	if policyDoc == nil {
		return SearchResult{}, ErrNotFederated
	}
	if !s.federatedPrincipal(ctx, p) {
		return SearchResult{}, ErrNotFederated
	}
	targets, err := searchTargets(t.doc, s.cfg.InstanceID, members)
	if err != nil {
		return SearchResult{}, err
	}
	conc, memberTimeout, overall, ttl, _, _ := s.searchSetting()
	s.search.requests.Add(1)
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	type answer struct {
		status MemberSearchStatus
		hits   []SearchHit
		more   bool
	}
	answers := make([]answer, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	body, _ := json.Marshal(q)
	policyGen := s.policyGeneration(ctx)
	scopeKey := scopesKey(p)
	for i, m := range targets {
		answers[i].status = MemberSearchStatus{ID: m.InstanceID, Name: m.Name, Status: "skipped", Code: "deadline"}
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
			key := ""
			if ttl > 0 {
				sum := sha256.Sum256(append([]byte(m.InstanceID+"\x00"+p.PrincipalID+"\x00"+scopeKey+"\x00"+policyGen+"\x00"), body...))
				key = hex.EncodeToString(sum[:])
				if r, ok := s.cachedSearch(key); ok {
					answers[i].hits, answers[i].more, st.Hits, st.Cached = r.Hits, r.More, len(r.Hits), true
					s.search.cacheHits.Add(1)
					finish("ok", "")
					return
				}
			}
			mctx, mcancel := context.WithTimeout(ctx, memberTimeout)
			defer mcancel()
			req, err := s.Assert(mctx, p, m.InstanceID, "POST", "/federation/v1/search", body)
			if err == nil {
				var raw []byte
				if raw, err = s.SendAsserted(mctx, req); err == nil {
					var resp peerSearchResponse
					if json.Unmarshal(raw, &resp) != nil || len(resp.Hits) > q.Limit+1 {
						finish("error", "malformed_response")
						return
					}
					for _, h := range resp.Hits {
						if h.FeedKey == "" || h.Coordinate == "" {
							finish("error", "malformed_response")
							return
						}
					}
					answers[i].hits, answers[i].more, st.Hits = resp.Hits, resp.More, len(resp.Hits)
					if key != "" {
						s.storeSearch(key, resp, ttl)
					}
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

	// Local hits run alongside the remote fan-out.
	localStatus := MemberSearchStatus{ID: s.cfg.InstanceID, Name: self.Name, Status: "ok"}
	var localHits []SearchHit
	start := time.Now()
	if local != nil {
		if localHits, err = local(ctx); err != nil {
			localStatus.Status, localStatus.Code = "error", "unavailable"
		}
	}
	localStatus.TookMS, localStatus.Hits = time.Since(start).Milliseconds(), len(localHits)
	wg.Wait()

	res := SearchResult{Items: []MergedHit{}, Members: []MemberSearchStatus{localStatus}}
	sets := []memberHits{{member: self, hits: localHits, more: len(localHits) > q.Limit}}
	for i, a := range answers {
		res.Members = append(res.Members, a.status)
		if a.status.Status == "ok" {
			sets = append(sets, memberHits{member: targets[i], hits: a.hits, more: a.more || len(a.hits) > q.Limit})
		}
	}
	if localStatus.Status != "ok" {
		sets = sets[1:]
	}
	for _, m := range res.Members {
		if m.Status != "ok" {
			res.Partial = true
		}
	}
	if res.Partial {
		s.search.partial.Add(1)
	}
	res.Items, res.More = mergeHits(sets, q.Limit)
	return res, nil
}

// federatedPrincipal reports whether p can be asserted at all (used when the
// federation has no other active member to assert to).
func (s *Service) federatedPrincipal(ctx context.Context, p identity.Principal) bool {
	if p.InstanceID != s.cfg.InstanceID || p.PrincipalID == "" || p.Anonymous || p.Delegated {
		return false
	}
	var origin string
	var disabled bool
	if err := s.cfg.DB.QueryRow(ctx, `SELECT origin,disabled FROM principals WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, p.PrincipalID).Scan(&origin, &disabled); err != nil {
		return false
	}
	return origin == "federation" && !disabled
}

func (s *Service) policyGeneration(ctx context.Context) string {
	ps, err := loadPolicyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return "?"
	}
	b, _ := json.Marshal(ps.Generation)
	return string(b)
}

func scopesKey(p identity.Principal) string {
	if p.TokenID == "" {
		return "session"
	}
	b, _ := json.Marshal(p.Scopes)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func searchTargets(doc *Membership, self string, filter []string) ([]Member, error) {
	want := map[string]bool{}
	for _, f := range filter {
		want[f] = true
	}
	var out []Member
	matched := map[string]bool{}
	for _, m := range doc.Members {
		if m.Status != "active" || m.InstanceID == self {
			continue
		}
		if len(want) > 0 && !want[m.InstanceID] && !want[m.Name] {
			continue
		}
		matched[m.InstanceID], matched[m.Name] = true, true
		out = append(out, m)
	}
	for f := range want {
		if !matched[f] {
			if m, ok := doc.Member(self); ok && (f == self || f == m.Name) {
				continue
			}
			return nil, invalid("unknown or inactive member in members filter")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type memberHits struct {
	member Member
	hits   []SearchHit
	more   bool
}

// mergeHits merges ordered per-member pages into one page of at most limit
// distinct (feed_key, coordinate) entries. Every member contributes its first
// Limit hits after the same watermark, so the merged prefix is exact.
func mergeHits(sets []memberHits, limit int) ([]MergedHit, bool) {
	type key struct{ feed, coord string }
	groups := map[key]*MergedHit{}
	var order []key
	for _, set := range sets {
		hits := set.hits
		if len(hits) > limit {
			hits = hits[:limit]
		}
		for _, h := range hits {
			k := key{h.FeedKey, h.Coordinate}
			copyOf := SearchCopy{Member: set.member.InstanceID, MemberName: set.member.Name, Origin: h.Origin, Home: h.Origin != "replica", APIURL: set.member.APIURL}
			g := groups[k]
			if g == nil {
				g = &MergedHit{FeedKey: h.FeedKey, FeedSlug: h.FeedSlug, Format: h.Format, Coordinate: h.Coordinate, Namespace: h.Namespace, Copies: []SearchCopy{}}
				groups[k] = g
				order = append(order, k)
			}
			if len(h.Metadata) <= maxSearchMetadata {
				if g.Metadata == nil || (copyOf.Home && !g.Home) {
					g.Metadata = h.Metadata
				}
			}
			if len(g.Copies) == 0 || (copyOf.Home && !g.Home) {
				g.Member, g.MemberName, g.Origin, g.Home, g.APIURL = copyOf.Member, copyOf.MemberName, copyOf.Origin, copyOf.Home, copyOf.APIURL
				g.FeedSlug, g.Format, g.Namespace = h.FeedSlug, h.Format, h.Namespace
			}
			g.Copies = append(g.Copies, copyOf)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].feed != order[j].feed {
			return order[i].feed < order[j].feed
		}
		return order[i].coord < order[j].coord
	})
	more := false
	for _, set := range sets {
		more = more || set.more
	}
	if len(order) > limit {
		order, more = order[:limit], true
	}
	out := make([]MergedHit, 0, len(order))
	for _, k := range order {
		g := groups[k]
		sort.SliceStable(g.Copies, func(i, j int) bool {
			if g.Copies[i].Home != g.Copies[j].Home {
				return g.Copies[i].Home
			}
			return g.Copies[i].MemberName < g.Copies[j].MemberName
		})
		out = append(out, *g)
	}
	return out, more
}

func (s *Service) cachedSearch(key string) (peerSearchResponse, bool) {
	st := &s.search
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	c, ok := st.cache[key]
	if !ok || !s.cfg.Now().Before(c.expires) {
		delete(st.cache, key)
		return peerSearchResponse{}, false
	}
	return c.resp, true
}

func (s *Service) storeSearch(key string, r peerSearchResponse, ttl time.Duration) {
	st := &s.search
	st.cacheMu.Lock()
	defer st.cacheMu.Unlock()
	if st.cache == nil || len(st.cache) >= maxSearchCacheEntries {
		st.cache = map[string]cachedSearch{}
	}
	st.cache[key] = cachedSearch{resp: r, expires: s.cfg.Now().Add(ttl)}
}

// peerSearch answers POST /federation/v1/search: the asserted principal's
// hits under this member's own policy.
func (s *Service) peerSearch(w http.ResponseWriter, r *http.Request) {
	_, _, _, _, peerRate, principalRate := s.searchSetting()
	caller, _ := peer.Caller(r)
	if !s.allow("peer:"+caller.InstanceID, peerRate) {
		s.search.rateLimited.Add(1)
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
	if !s.allow(searchRatePrincipalKeyPrefix+a.Issuer+"/"+p.PrincipalID, principalRate) {
		s.search.rateLimited.Add(1)
		peerFail(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var q SearchQuery
	if json.Unmarshal(body, &q) != nil || q.Validate() != nil {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	f := s.searcher.Load()
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
	bindings, err := s.feedBindings(r.Context(), s.cfg.DB)
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	hits, err := (*f)(r.Context(), p, policy, q, bindings)
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	out := peerSearchResponse{Hits: hits}
	if len(hits) > q.Limit {
		out.Hits, out.More = hits[:q.Limit], true
	}
	if out.Hits == nil {
		out.Hits = []SearchHit{}
	}
	for i := range out.Hits {
		if len(out.Hits[i].Metadata) > maxSearchMetadata {
			out.Hits[i].Metadata = nil
		}
	}
	peerJSON(w, http.StatusOK, out)
}

// assertionFailed audits and answers a refused assertion.
func (s *Service) assertionFailed(w http.ResponseWriter, r *http.Request, a peer.Assertion, reason string) {
	s.assertionRejected(r, a, reason)
	status := http.StatusUnauthorized
	switch reason {
	case "principal_unknown", "assertion_unknown_domain":
		status = http.StatusForbidden
	case "unavailable":
		status = http.StatusServiceUnavailable
	}
	peerFail(w, status, reason)
}
