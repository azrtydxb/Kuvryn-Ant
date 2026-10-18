package federation

import (
	"fmt"
	"io"
)

// WriteMetrics emits federation gauges from the in-memory trust state (no
// database access on scrape). Labels carry only fixed values.
func (s *Service) WriteMetrics(w io.Writer) {
	t := s.trust.Load()
	active := 0
	if t.state.State == "active" {
		active = 1
	}
	fmt.Fprintln(w, "# HELP ant_federation_active Whether this instance is an active federation member.\n# TYPE ant_federation_active gauge")
	fmt.Fprintf(w, "ant_federation_active %d\n", active)
	if t.doc == nil {
		return
	}
	fmt.Fprintln(w, "# HELP ant_federation_document_generation Applied federation document generation.\n# TYPE ant_federation_document_generation gauge")
	fmt.Fprintf(w, "ant_federation_document_generation{kind=\"membership\"} %d\n", t.doc.Generation)
	if p := s.policyHeadGauge.Load(); p != nil {
		fmt.Fprintf(w, "ant_federation_document_generation{kind=\"policy\"} %d\n", p.Generation)
	}
	if tp := s.topologyGauge.Load(); tp != nil {
		fmt.Fprintf(w, "ant_federation_document_generation{kind=\"topology\"} %d\n", tp.Generation)
	}
	fmt.Fprintln(w, "# HELP ant_federation_authority_epoch Applied authority epoch.\n# TYPE ant_federation_authority_epoch gauge")
	fmt.Fprintf(w, "ant_federation_authority_epoch %d\n", t.doc.AuthorityEpoch)
	counts := map[string]int{"active": 0, "suspended": 0, "removed": 0}
	for _, m := range t.doc.Members {
		counts[m.Status]++
	}
	fmt.Fprintln(w, "# HELP ant_federation_members Federation members by status in the applied membership.\n# TYPE ant_federation_members gauge")
	for _, st := range []string{"active", "suspended", "removed"} {
		fmt.Fprintf(w, "ant_federation_members{status=%q} %d\n", st, counts[st])
	}
	if p := s.policyHeadGauge.Load(); p != nil {
		fmt.Fprintln(w, "# HELP ant_federation_document_age_seconds Time since the applied document was last confirmed current.\n# TYPE ant_federation_document_age_seconds gauge")
		fmt.Fprintf(w, "ant_federation_document_age_seconds{kind=\"policy\"} %d\n", p.AgeSeconds)
		fmt.Fprintln(w, "# HELP ant_federation_policy_unresolved_feed_keys Policy feed keys not bound to a local feed (inert grants).\n# TYPE ant_federation_policy_unresolved_feed_keys gauge")
		fmt.Fprintf(w, "ant_federation_policy_unresolved_feed_keys %d\n", len(p.Report.UnresolvedFeedKeys))
	}
	if tp := s.topologyGauge.Load(); tp != nil {
		fmt.Fprintln(w, "# HELP ant_federation_topology_rules_targeting_self Replication rules that target this member.\n# TYPE ant_federation_topology_rules_targeting_self gauge")
		fmt.Fprintf(w, "ant_federation_topology_rules_targeting_self %d\n", tp.Targeted)
		fmt.Fprintln(w, "# HELP ant_federation_topology_home_feeds Federated feeds this member is home of.\n# TYPE ant_federation_topology_home_feeds gauge")
		fmt.Fprintf(w, "ant_federation_topology_home_feeds %d\n", tp.HomeFeeds)
		fmt.Fprintln(w, "# HELP ant_federation_transfers_active Home transfers in progress.\n# TYPE ant_federation_transfers_active gauge")
		fmt.Fprintf(w, "ant_federation_transfers_active %d\n", tp.Transfers)
		fmt.Fprintln(w, "# HELP ant_federation_quarantines_unresolved Quarantined old-home sources awaiting review.\n# TYPE ant_federation_quarantines_unresolved gauge")
		fmt.Fprintf(w, "ant_federation_quarantines_unresolved %d\n", tp.Quarantines)
	}
	fmt.Fprintln(w, "# HELP ant_federation_alert Active federation alerts (1 per code and severity).\n# TYPE ant_federation_alert gauge")
	if a := s.alerts.Load(); a != nil {
		for _, x := range *a {
			fmt.Fprintf(w, "ant_federation_alert{code=%q,severity=%q} 1\n", x.Code, x.Severity)
		}
	}
	fmt.Fprintln(w, "# HELP ant_federation_assertions_total Principal assertions by outcome.\n# TYPE ant_federation_assertions_total counter")
	fmt.Fprintf(w, "ant_federation_assertions_total{outcome=\"issued\"} %d\n", s.metrics.issued.Load())
	fmt.Fprintf(w, "ant_federation_assertions_total{outcome=\"accepted\"} %d\n", s.metrics.accepted.Load())
	fmt.Fprintf(w, "ant_federation_assertions_total{outcome=\"rejected\"} %d\n", s.metrics.rejected.Load())
	fmt.Fprintln(w, "# HELP ant_federation_join_rejected_total Refused join attempts (invalid, expired, consumed or revoked invitations).\n# TYPE ant_federation_join_rejected_total counter")
	fmt.Fprintf(w, "ant_federation_join_rejected_total %d\n", s.metrics.joinRejected.Load())
	s.writeSearchMetrics(w)
}

// writeSearchMetrics emits the cross-instance search counters.
func (s *Service) writeSearchMetrics(w io.Writer) {
	fmt.Fprintln(w, "# HELP ant_federation_search_requests_total Federated searches fanned out by this member.\n# TYPE ant_federation_search_requests_total counter")
	fmt.Fprintf(w, "ant_federation_search_requests_total %d\n", s.search.requests.Load())
	fmt.Fprintln(w, "# HELP ant_federation_search_partial_total Federated searches that reported a member as not ok.\n# TYPE ant_federation_search_partial_total counter")
	fmt.Fprintf(w, "ant_federation_search_partial_total %d\n", s.search.partial.Load())
	fmt.Fprintln(w, "# HELP ant_federation_search_cache_hits_total Per-member search answers served from the 30 s cache.\n# TYPE ant_federation_search_cache_hits_total counter")
	fmt.Fprintf(w, "ant_federation_search_cache_hits_total %d\n", s.search.cacheHits.Load())
	fmt.Fprintln(w, "# HELP ant_federation_search_rate_limited_total Peer search requests refused by the per-peer or per-principal limit.\n# TYPE ant_federation_search_rate_limited_total counter")
	fmt.Fprintf(w, "ant_federation_search_rate_limited_total %d\n", s.search.rateLimited.Load())
}
