package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Placement-aware routing (docs/planning/phase-2-federation.md section 8,
// P2-M5). When a read of a federated feed misses on the entry member, the
// placement engine orders the members that hold the feed key: replicas whose
// replication state is acceptable first (label preference, then replica
// health, then measured round-trip time), the home last as the authoritative
// fallback. Members whose circuit is open are skipped. The order is
// deterministic for identical inputs and is what the explain views show.

// Routing tuning.
const (
	routeBreakerFailures = 3                // consecutive failures that open a circuit
	routeBreakerOpen     = 30 * time.Second // first open period (doubles up to 8x)
	routeReplicaTTL      = 15 * time.Second // member replication status cache
	routeInfoTTL         = 5 * time.Minute  // member feed slug cache
	routeSlugTTL         = 5 * time.Second  // local slug → feed key cache
	routeMaxAttempts     = 4                // members tried for one read
	routeRTTAlpha        = 0.3              // EWMA weight of a new RTT sample
	DefaultRouteStreams  = 64               // concurrent proxied streams (transfer budget)
	DefaultRouteCacheTTL = 24 * time.Hour
	routeTicketLifetime  = 5 * time.Minute
)

// memberHealth is the locally observed health of one member: an RTT EWMA from
// status pings and proxied requests, and a circuit breaker.
type memberHealth struct {
	rttMS       float64
	failures    int
	openUntil   time.Time
	lastError   string
	lastSuccess time.Time
	lastFailure time.Time
}

type replicaSnapshot struct {
	at       time.Time
	ok       bool
	statuses map[string]RuleStatus // feed key → status
}

type routeInfo struct {
	FeedKey  string `json:"feed_key"`
	FeedSlug string `json:"feed_slug"`
	Format   string `json:"format"`
}

type routeInfoEntry struct {
	info  routeInfo
	until time.Time
}

type slugEntry struct {
	target routeTarget
	found  bool
	until  time.Time
	topo   *TopologyDocument // the applied topology it was resolved against
}

// routeState is the routing runtime of a service.
type routeState struct {
	mu         sync.Mutex
	members    map[string]*memberHealth
	replicas   map[string]replicaSnapshot
	refreshing map[string]bool
	info       map[string]routeInfoEntry
	slugs      map[string]slugEntry
	clients    map[string]*routeClient
	jtis       map[string]*issuerJTIs // proxied-read assertion replay cache per issuer
	streams    chan struct{}
	streamsCap int
	// per-principal and per-client shares of the transfer budget
	byPrincipal, byClient map[string]int
	limitMu               sync.Mutex
	buckets               map[string]*bucket // per-principal routed-read rate

	// counters (metrics)
	outcomes                                       sync.Map // "mechanism|outcome" → *atomic.Int64
	proxyBytes, servedRequests, servedBytes        atomic.Int64
	writesNotRouted, cacheHits, cacheStores        atomic.Int64
	cacheDrops                                     atomic.Int64
	ticketsIssued, ticketsAccepted, ticketsRefused atomic.Int64
	active                                         atomic.Int64
}

func (rs *routeState) count(mechanism, outcome string) {
	v, _ := rs.outcomes.LoadOrStore(mechanism+"|"+outcome, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func (s *Service) routeStreams() chan struct{} {
	rs := &s.route
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.streams == nil {
		n := s.cfg.RouteMaxStreams
		if n <= 0 {
			n = DefaultRouteStreams
		}
		rs.streams, rs.streamsCap = make(chan struct{}, n), n
	}
	return rs.streams
}

// observeMember records the outcome of a request to a member: success with
// its round-trip time, or a failure that counts towards opening the circuit.
func (s *Service) observeMember(member string, ok bool, rtt time.Duration, reason string) {
	rs := &s.route
	now := s.cfg.Now()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.members == nil {
		rs.members = map[string]*memberHealth{}
	}
	h := rs.members[member]
	if h == nil {
		h = &memberHealth{}
		rs.members[member] = h
	}
	if ok {
		ms := float64(rtt) / float64(time.Millisecond)
		if h.rttMS == 0 {
			h.rttMS = ms
		} else if ms > 0 {
			h.rttMS = routeRTTAlpha*ms + (1-routeRTTAlpha)*h.rttMS
		}
		h.failures, h.openUntil, h.lastError, h.lastSuccess = 0, time.Time{}, "", now
		return
	}
	h.failures++
	h.lastFailure, h.lastError = now, reason
	if h.failures >= routeBreakerFailures {
		shift := min(h.failures-routeBreakerFailures, 3)
		h.openUntil = now.Add(routeBreakerOpen << shift)
	}
}

// circuit returns the breaker state of a member: closed, open or half_open
// (the open period elapsed; the next request is a trial).
func (s *Service) circuit(member string) (state string, rttMS float64, failures int) {
	rs := &s.route
	rs.mu.Lock()
	defer rs.mu.Unlock()
	h := rs.members[member]
	if h == nil {
		return "closed", 0, 0
	}
	switch {
	case h.failures < routeBreakerFailures:
		return "closed", h.rttMS, h.failures
	case s.cfg.Now().Before(h.openUntil):
		return "open", h.rttMS, h.failures
	default:
		return "half_open", h.rttMS, h.failures
	}
}

// trial claims the half-open trial of a member so concurrent reads do not
// stampede a recovering member: the circuit stays open briefly while one
// request probes it.
func (s *Service) trial(member string) {
	rs := &s.route
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if h := rs.members[member]; h != nil && h.failures >= routeBreakerFailures {
		h.openUntil = s.cfg.Now().Add(5 * time.Second)
	}
}

// replicaStatuses returns the replication status of the given members for
// every rule targeting them (feed key → status). With wait, stale entries are
// refreshed synchronously (explain); otherwise a background refresh starts
// and the stale or missing snapshot is used (the read path never waits).
func (s *Service) replicaStatuses(ctx context.Context, members []Member, wait bool) map[string]replicaSnapshot {
	rs := &s.route
	now := s.cfg.Now()
	out := map[string]replicaSnapshot{}
	var stale []Member
	rs.mu.Lock()
	if rs.replicas == nil {
		rs.replicas, rs.refreshing = map[string]replicaSnapshot{}, map[string]bool{}
	}
	for _, m := range members {
		snap, ok := rs.replicas[m.InstanceID]
		out[m.InstanceID] = snap
		if !ok || now.Sub(snap.at) > routeReplicaTTL {
			if wait || !rs.refreshing[m.InstanceID] {
				stale = append(stale, m)
				rs.refreshing[m.InstanceID] = true
			}
		}
	}
	rs.mu.Unlock()
	if len(stale) == 0 {
		return out
	}
	fetch := func(ctx context.Context, m Member) replicaSnapshot {
		snap := replicaSnapshot{at: s.cfg.Now(), statuses: map[string]RuleStatus{}}
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		list, err := s.peerRuleStatuses(cctx, m)
		cancel()
		if err == nil {
			snap.ok = true
			for _, st := range list {
				snap.statuses[st.FeedKey] = st
			}
		}
		rs.mu.Lock()
		rs.replicas[m.InstanceID] = snap
		delete(rs.refreshing, m.InstanceID)
		rs.mu.Unlock()
		return snap
	}
	if !wait {
		for _, m := range stale {
			go fetch(context.WithoutCancel(ctx), m)
		}
		return out
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, m := range stale {
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			snap := fetch(ctx, m)
			mu.Lock()
			out[m.InstanceID] = snap
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	return out
}

// RouteCandidate is one member that holds a feed key, as the placement engine
// sees it from this member.
type RouteCandidate struct {
	MemberID     string   `json:"member_id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"` // home | replica
	APIURL       string   `json:"api_url"`
	ReplicaState string   `json:"replica_state,omitempty"`
	LagSeconds   *float64 `json:"lag_seconds,omitempty"`
	Circuit      string   `json:"circuit"` // closed | open | half_open
	Failures     int      `json:"consecutive_failures"`
	RTTMS        *float64 `json:"rtt_ms,omitempty"`
	Preference   int      `json:"preference"`
	Eligible     bool     `json:"eligible"`
	Order        int      `json:"order"`
	Reason       string   `json:"reason"`
}

// RoutePlan is the routing decision for a feed key on this member.
type RoutePlan struct {
	FeedKey     string           `json:"feed_key"`
	Format      string           `json:"format"`
	Home        string           `json:"home"`
	LocalRole   string           `json:"local_role"` // home | replica | unbound
	LocalFeedID string           `json:"local_feed_id,omitempty"`
	Routing     FeedRouting      `json:"routing"`
	Decision    string           `json:"decision"` // local | proxy | redirect | off | no_candidates | unavailable
	Reason      string           `json:"reason"`
	Candidates  []RouteCandidate `json:"candidates"`
}

// routeTarget is a feed slug on this member resolved to its feed key.
type routeTarget struct {
	key       string
	format    string
	localFeed string
	feed      TopologyFeed
	home      bool
}

func replicaRank(state string) int {
	switch state {
	case "in_sync":
		return 0
	case "unknown":
		return 1
	default:
		return 2
	}
}

// plan computes the ordered candidates for a feed key. wait refreshes stale
// replica status synchronously (explain only).
func (s *Service) plan(ctx context.Context, f TopologyFeed, topo *TopologyDocument, wait bool) ([]RouteCandidate, FeedRouting) {
	routing := f.EffectiveRouting()
	doc := s.trust.Load().doc
	if doc == nil {
		return []RouteCandidate{}, routing
	}
	self, _ := doc.Member(s.cfg.InstanceID)
	var holders []Member
	var replicas []Member
	for _, m := range doc.Members {
		if m.Status != "active" || m.InstanceID == s.cfg.InstanceID {
			continue
		}
		if m.InstanceID == f.Home {
			holders = append(holders, m)
			continue
		}
		for i := range topo.Rules {
			if r := &topo.Rules[i]; r.FeedKey == f.Key && r.Matches(m) {
				holders = append(holders, m)
				replicas = append(replicas, m)
				break
			}
		}
	}
	snaps := s.replicaStatuses(ctx, replicas, wait)
	out := make([]RouteCandidate, 0, len(holders))
	for _, m := range holders {
		c := RouteCandidate{MemberID: m.InstanceID, Name: m.Name, APIURL: m.APIURL, Role: "replica", Eligible: true}
		if m.InstanceID == f.Home {
			c.Role = "home"
		}
		var rtt float64
		c.Circuit, rtt, c.Failures = s.circuit(m.InstanceID)
		if rtt > 0 {
			v := math.Round(rtt*10) / 10
			c.RTTMS = &v
		}
		for _, k := range routing.Prefer {
			if v, ok := self.Labels[k]; ok && m.Labels[k] == v {
				c.Preference++
				continue
			}
			break
		}
		if c.Role == "replica" {
			c.ReplicaState = "unknown"
			snap := snaps[m.InstanceID]
			if st, ok := snap.statuses[f.Key]; ok {
				c.ReplicaState, c.LagSeconds = st.State, st.LagSeconds
			}
			// The member's own state applies the rule's lag budget; the
			// routing max_lag_seconds, when set, overrides it either way.
			switch c.ReplicaState {
			case "in_sync":
				c.Reason = "replica in sync"
				if routing.MaxLagSeconds > 0 && c.LagSeconds != nil && *c.LagSeconds > float64(routing.MaxLagSeconds) {
					c.Eligible, c.Reason = false, "replica lag exceeds the routing max_lag_seconds"
				}
			case "lagging":
				c.Eligible, c.Reason = false, "replica lag exceeds its budget"
				if routing.MaxLagSeconds > 0 && c.LagSeconds != nil && *c.LagSeconds <= float64(routing.MaxLagSeconds) {
					c.Eligible, c.Reason = true, "replica lag within the routing max_lag_seconds"
				}
			case "unknown":
				c.Reason = "replica state unknown (status not yet reported); tried after in-sync replicas"
			default:
				c.Eligible, c.Reason = false, "replica is "+strings.ReplaceAll(c.ReplicaState, "_", " ")
			}
		} else {
			c.Reason = "home: authoritative holder, tried after replicas"
		}
		if c.Circuit == "open" {
			c.Eligible, c.Reason = false, "circuit open after consecutive failures"
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if (a.Role == "home") != (b.Role == "home") {
			return b.Role == "home"
		}
		if a.Preference != b.Preference {
			return a.Preference > b.Preference
		}
		if ra, rb := replicaRank(a.ReplicaState), replicaRank(b.ReplicaState); ra != rb {
			return ra < rb
		}
		if a.Circuit != b.Circuit {
			return a.Circuit == "closed"
		}
		switch {
		case a.RTTMS != nil && b.RTTMS != nil && *a.RTTMS != *b.RTTMS:
			return *a.RTTMS < *b.RTTMS
		case (a.RTTMS == nil) != (b.RTTMS == nil):
			return a.RTTMS != nil
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.MemberID < b.MemberID
	})
	n := 0
	for i := range out {
		if out[i].Eligible {
			n++
			out[i].Order = n
		}
	}
	return out, routing
}

// lookupTarget resolves a feed slug of this member (with the request's
// format) to its federation feed key, or reports that the request is not
// federated. A slug without a local feed resolves to "<federation>/<slug>"
// when the topology has that key.
func (s *Service) lookupTarget(ctx context.Context, slug, format string) (routeTarget, bool) {
	rs := &s.route
	now := s.cfg.Now()
	cacheKey := slug + "\x00" + format
	// A newly applied topology (routing change, new key, home transfer)
	// invalidates every cached resolution at once.
	topo := s.topologyCache.Load()
	rs.mu.Lock()
	if e, ok := rs.slugs[cacheKey]; ok && now.Before(e.until) && e.topo == topo && topo != nil {
		rs.mu.Unlock()
		return e.target, e.found
	}
	rs.mu.Unlock()
	t, found := s.resolveTarget(ctx, slug, format)
	rs.mu.Lock()
	if rs.slugs == nil || len(rs.slugs) > 4096 {
		rs.slugs = map[string]slugEntry{}
	}
	rs.slugs[cacheKey] = slugEntry{target: t, found: found, until: now.Add(routeSlugTTL), topo: s.topologyCache.Load()}
	rs.mu.Unlock()
	return t, found
}

func formatMatches(descriptor, requested string) bool {
	if requested == "oci" {
		return descriptor == "oci" || descriptor == "oci-artifacts"
	}
	return descriptor == requested
}

func (s *Service) resolveTarget(ctx context.Context, slug, format string) (routeTarget, bool) {
	t := s.trust.Load()
	if t.doc == nil || t.state.State != "active" || !slugPattern.MatchString(slug) {
		return routeTarget{}, false
	}
	topo, err := s.appliedTopology(ctx)
	if err != nil || topo == nil {
		return routeTarget{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var feedID, feedFormat string
	var key *string
	err = s.cfg.DB.QueryRow(ctx, `SELECT f.id::text,f.format,b.feed_key FROM feeds f LEFT JOIN federated_feed_bindings b ON (b.instance_id,b.feed_id)=(f.instance_id,f.id) WHERE f.instance_id=$1 AND f.slug=$2`,
		s.cfg.InstanceID, slug).Scan(&feedID, &feedFormat, &key)
	out := routeTarget{format: format}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.key = t.doc.FederationName + "/" + slug
	case err != nil:
		return routeTarget{}, false
	case key == nil:
		return routeTarget{}, false // a local feed that is not federated
	default:
		out.key, out.localFeed = *key, feedID
	}
	f, ok := topo.Feed(out.key)
	if !ok || !formatMatches(f.Descriptor.Format, format) {
		return routeTarget{}, false
	}
	out.feed, out.home = *f, f.Home == s.cfg.InstanceID
	return out, true
}

// memberInfo returns the local feed slug a member holds for a feed key
// (GET /federation/v1/routes/feeds/{fed}/{slug}), cached for five minutes.
func (s *Service) memberInfo(ctx context.Context, m Member, key string) (routeInfo, error) {
	rs := &s.route
	cacheKey := m.InstanceID + "\x00" + key
	now := s.cfg.Now()
	rs.mu.Lock()
	if e, ok := rs.info[cacheKey]; ok && now.Before(e.until) {
		rs.mu.Unlock()
		return e.info, nil
	}
	rs.mu.Unlock()
	id, _, err := s.identity()
	if err != nil {
		return routeInfo{}, err
	}
	fed, slug, _ := strings.Cut(key, "/")
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := s.cfg.Now()
	raw, err := s.client.Do(cctx, id, m.FederationURL, http.MethodGet, "/federation/v1/routes/feeds/"+url.PathEscape(fed)+"/"+url.PathEscape(slug), nil, m.InstanceID)
	if err != nil {
		return routeInfo{}, err
	}
	s.observeMember(m.InstanceID, true, s.cfg.Now().Sub(start), "")
	var out routeInfo
	if json.Unmarshal(raw, &out) != nil || out.FeedKey != key || !slugPattern.MatchString(out.FeedSlug) {
		return routeInfo{}, ErrMalformed
	}
	rs.mu.Lock()
	if rs.info == nil || len(rs.info) > 4096 {
		rs.info = map[string]routeInfoEntry{}
	}
	rs.info[cacheKey] = routeInfoEntry{info: out, until: now.Add(routeInfoTTL)}
	rs.mu.Unlock()
	return out, nil
}

// heldFeed returns the local feed this member holds for a key (home or
// replica): it must be bound to the key and the key must be in the topology.
func (s *Service) heldFeed(ctx context.Context, key string) (id, slug, format string, err error) {
	topo, err := s.appliedTopology(ctx)
	if err != nil {
		return "", "", "", err
	}
	if topo == nil {
		return "", "", "", ErrNotFound
	}
	if _, ok := topo.Feed(key); !ok {
		return "", "", "", ErrNotFound
	}
	err = s.cfg.DB.QueryRow(ctx, `SELECT f.id::text,f.slug,f.format FROM federated_feed_bindings b JOIN feeds f ON (f.instance_id,f.id)=(b.instance_id,b.feed_id) WHERE b.instance_id=$1 AND b.feed_key=$2`,
		s.cfg.InstanceID, key).Scan(&id, &slug, &format)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	return id, slug, format, err
}

// peerRouteInfo answers GET /federation/v1/routes/feeds/{fed}/{slug}: the
// local slug of a feed key this member holds.
func (s *Service) peerRouteInfo(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("fed") + "/" + r.PathValue("slug")
	_, slug, format, err := s.heldFeed(r.Context(), key)
	if errors.Is(err, ErrNotFound) {
		peerFail(w, http.StatusNotFound, "not_held")
		return
	}
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	peerJSON(w, http.StatusOK, routeInfo{FeedKey: key, FeedSlug: slug, Format: format})
}

// ExplainRoute returns the routing plan of a feed key on this member (the
// federated part of GET /api/v1/feeds/{id}/resolve and
// GET /api/v1/federation/routes/{fed}/{slug}). It refreshes stale replica
// status synchronously and never contacts a member otherwise.
func (s *Service) ExplainRoute(ctx context.Context, key string) (RoutePlan, error) {
	t := s.trust.Load()
	if t.doc == nil || t.state.State != "active" {
		return RoutePlan{}, ErrNotConfigured
	}
	topo, err := s.appliedTopology(ctx)
	if err != nil {
		return RoutePlan{}, err
	}
	if topo == nil {
		return RoutePlan{}, ErrNotFound
	}
	f, ok := topo.Feed(key)
	if !ok {
		return RoutePlan{}, ErrNotFound
	}
	out := RoutePlan{FeedKey: key, Format: f.Descriptor.Format, Home: f.Home, LocalRole: "unbound"}
	if out.LocalFeedID, err = boundFeed(ctx, s.cfg.DB, s.cfg.InstanceID, key); err != nil {
		return RoutePlan{}, err
	}
	if out.LocalFeedID != "" {
		out.LocalRole = "replica"
	}
	if f.Home == s.cfg.InstanceID {
		out.LocalRole = "home"
	}
	out.Candidates, out.Routing = s.plan(ctx, *f, topo, true)
	out.Decision, out.Reason = decide(out.LocalRole, out.Routing, out.Candidates)
	return out, nil
}

// ExplainFeedRoute is ExplainRoute for a local feed; ok is false when the
// feed is not bound to a federation feed key.
func (s *Service) ExplainFeedRoute(ctx context.Context, feedID string) (RoutePlan, bool, error) {
	if t := s.trust.Load(); t.doc == nil || t.state.State != "active" {
		return RoutePlan{}, false, nil
	}
	var key string
	err := s.cfg.DB.QueryRow(ctx, `SELECT feed_key FROM federated_feed_bindings WHERE instance_id=$1 AND feed_id=$2`, s.cfg.InstanceID, feedID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoutePlan{}, false, nil
	}
	if err != nil {
		return RoutePlan{}, false, err
	}
	plan, err := s.ExplainRoute(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return RoutePlan{}, false, nil
	}
	return plan, err == nil, err
}

func decide(localRole string, routing FeedRouting, candidates []RouteCandidate) (string, string) {
	if localRole == "home" {
		return "local", "this member is the home: it answers from its own content and never routes"
	}
	if routing.Mode == RouteOff {
		return RouteOff, "routing is off for this feed key: only local content is served"
	}
	if len(candidates) == 0 {
		return "no_candidates", "no other active member holds this feed key"
	}
	if candidates[0].Eligible {
		switch {
		case routing.Mode == RouteRedirect && localRole == "replica":
			return RouteProxy, "redirect mode applies to members without a local copy; this member holds a replica and proxies its misses (no redirect chains)"
		case routing.Mode == RouteRedirect:
			return RouteRedirect, "a read is redirected (307) to the first candidate's public feed endpoint; the client must authenticate there"
		case routing.BlobURLs:
			return RouteProxy, "a local miss is proxied over the federation listener with the caller's assertion; immutable blob reads get a signed five-minute URL on the first candidate"
		}
		return RouteProxy, "a local miss is proxied over the federation listener with the caller's assertion"
	}
	return "unavailable", "every member holding this feed key is excluded (circuit open, lagging or not in sync); reads answer 503"
}

// writeRouteMetrics emits the routing counters and gauges.
func (s *Service) writeRouteMetrics(w io.Writer) {
	rs := &s.route
	fmt.Fprintln(w, "# HELP ant_federation_route_requests_total Routed reads by mechanism (proxy, redirect, blob_url) and outcome.\n# TYPE ant_federation_route_requests_total counter")
	var keys []string
	rs.outcomes.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true })
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := rs.outcomes.Load(k)
		mech, outcome, _ := strings.Cut(k, "|")
		fmt.Fprintf(w, "ant_federation_route_requests_total{mechanism=%q,outcome=%q} %d\n", mech, outcome, v.(*atomic.Int64).Load())
	}
	fmt.Fprintln(w, "# HELP ant_federation_proxy_bytes_total Bytes streamed to clients from other members by the federation proxy.\n# TYPE ant_federation_proxy_bytes_total counter")
	fmt.Fprintf(w, "ant_federation_proxy_bytes_total %d\n", rs.proxyBytes.Load())
	fmt.Fprintln(w, "# HELP ant_federation_proxy_active Proxied reads in progress (bounded by the transfer budget).\n# TYPE ant_federation_proxy_active gauge")
	fmt.Fprintf(w, "ant_federation_proxy_active %d\n", rs.active.Load())
	fmt.Fprintln(w, "# HELP ant_federation_route_served_total Proxied reads and blob URLs this member served for other members.\n# TYPE ant_federation_route_served_total counter")
	fmt.Fprintf(w, "ant_federation_route_served_total %d\n", rs.servedRequests.Load())
	fmt.Fprintln(w, "# HELP ant_federation_route_served_bytes_total Bytes this member served for other members' proxied reads and blob URLs.\n# TYPE ant_federation_route_served_bytes_total counter")
	fmt.Fprintf(w, "ant_federation_route_served_bytes_total %d\n", rs.servedBytes.Load())
	fmt.Fprintln(w, "# HELP ant_federation_route_writes_not_routed_total Writes to federated feeds answered locally (never routed, D6).\n# TYPE ant_federation_route_writes_not_routed_total counter")
	fmt.Fprintf(w, "ant_federation_route_writes_not_routed_total %d\n", rs.writesNotRouted.Load())
	fmt.Fprintln(w, "# HELP ant_federation_route_cache_total Entry-cache events of proxied immutable reads.\n# TYPE ant_federation_route_cache_total counter")
	fmt.Fprintf(w, "ant_federation_route_cache_total{event=\"hit\"} %d\n", rs.cacheHits.Load())
	fmt.Fprintf(w, "ant_federation_route_cache_total{event=\"store\"} %d\n", rs.cacheStores.Load())
	fmt.Fprintf(w, "ant_federation_route_cache_total{event=\"drop\"} %d\n", rs.cacheDrops.Load())
	fmt.Fprintln(w, "# HELP ant_federation_blob_urls_total Signed blob URLs issued by this member and verified on this member.\n# TYPE ant_federation_blob_urls_total counter")
	fmt.Fprintf(w, "ant_federation_blob_urls_total{event=\"issued\"} %d\n", rs.ticketsIssued.Load())
	fmt.Fprintf(w, "ant_federation_blob_urls_total{event=\"accepted\"} %d\n", rs.ticketsAccepted.Load())
	fmt.Fprintf(w, "ant_federation_blob_urls_total{event=\"refused\"} %d\n", rs.ticketsRefused.Load())
	open := 0
	rs.mu.Lock()
	now := s.cfg.Now()
	for _, h := range rs.members {
		if h.failures >= routeBreakerFailures && now.Before(h.openUntil) {
			open++
		}
	}
	rs.mu.Unlock()
	fmt.Fprintln(w, "# HELP ant_federation_route_circuits_open Members currently excluded from routing by an open circuit.\n# TYPE ant_federation_route_circuits_open gauge")
	fmt.Fprintf(w, "ant_federation_route_circuits_open %d\n", open)
}
