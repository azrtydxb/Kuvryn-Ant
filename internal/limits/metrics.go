package limits

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// Registry holds throttle counters and the last observed quota usage. The
// process-wide Default is exposed on /metrics through observability collectors.
type Registry struct {
	throttles  [4]atomic.Uint64 // indexed by scopeIndex
	rejections [2]atomic.Uint64 // feed, namespace

	mu    sync.Mutex
	rules map[string]QuotaRule
	usage map[string][2]int64
}

// Default is the process-wide registry.
var Default = &Registry{rules: map[string]QuotaRule{}, usage: map[string][2]int64{}}

func scopeIndex(scope string) int {
	switch scope {
	case ScopePrincipal:
		return 0
	case ScopeFeed:
		return 1
	case ScopeNamespace:
		return 2
	default:
		return 3
	}
}

func (r *Registry) throttled(scope string) { r.throttles[scopeIndex(scope)].Add(1) }

// Throttles are the per-process 429 counts by scope.
type Throttles struct {
	Principal int64 `json:"principal"`
	Feed      int64 `json:"feed"`
	Namespace int64 `json:"namespace"`
	Site      int64 `json:"site"`
}

// Throttled returns the current counters.
func (r *Registry) Throttled() Throttles {
	return Throttles{int64(r.throttles[0].Load()), int64(r.throttles[1].Load()), int64(r.throttles[2].Load()), int64(r.throttles[3].Load())}
}
func (r *Registry) quotaRejected(scope string) {
	i := 0
	if scope == ScopeNamespace {
		i = 1
	}
	r.rejections[i].Add(1)
}

// setConfig replaces the known quota rules and drops usage observations of
// quotas that no longer exist.
func (r *Registry) setConfig(c Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := map[string]QuotaRule{}
	for _, q := range c.Quotas {
		next[q.key()] = q
	}
	for k := range r.usage {
		if _, ok := next[k]; !ok {
			delete(r.usage, k)
		}
	}
	r.rules = next
}

func (r *Registry) observeQuota(rule QuotaRule, bytes, packages int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules[rule.key()] = rule
	r.usage[rule.key()] = [2]int64{bytes, packages}
}

// ObserveUsage records measured usage of a quota (management usage view).
func (r *Registry) ObserveUsage(rule QuotaRule, bytes, packages int64) {
	r.observeQuota(rule, bytes, packages)
}

// WriteMetrics writes Prometheus text exposition. Quota series are bounded by
// the configured rules (at most MaxRules); usage is the last observation made at
// publish time or by the usage view.
func (r *Registry) WriteMetrics(w io.Writer) {
	names := [4]string{ScopePrincipal, ScopeFeed, ScopeNamespace, ScopeSite}
	fmt.Fprintln(w, "# HELP ant_ratelimit_throttled_total Requests rejected with 429 by a rate limit, by scope.\n# TYPE ant_ratelimit_throttled_total counter")
	for i, n := range names {
		fmt.Fprintf(w, "ant_ratelimit_throttled_total{scope=%q} %d\n", n, r.throttles[i].Load())
	}
	fmt.Fprintln(w, "# HELP ant_quota_rejections_total Publications rejected by a storage quota, by scope.\n# TYPE ant_quota_rejections_total counter")
	fmt.Fprintf(w, "ant_quota_rejections_total{scope=\"feed\"} %d\nant_quota_rejections_total{scope=\"namespace\"} %d\n", r.rejections[0].Load(), r.rejections[1].Load())
	r.mu.Lock()
	keys := make([]string, 0, len(r.rules))
	for k := range r.rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type row struct {
		rule  QuotaRule
		usage [2]int64
		seen  bool
	}
	rows := make([]row, 0, len(keys))
	for _, k := range keys {
		u, seen := r.usage[k]
		rows = append(rows, row{r.rules[k], u, seen})
	}
	r.mu.Unlock()
	fmt.Fprintln(w, "# HELP ant_quota_usage Last observed quota usage.\n# TYPE ant_quota_usage gauge")
	for _, x := range rows {
		if !x.seen {
			continue
		}
		l := fmt.Sprintf("scope=%q,feed=%q,namespace=%q", x.rule.Scope, x.rule.FeedID, x.rule.Namespace)
		fmt.Fprintf(w, "ant_quota_usage{%s,unit=\"bytes\"} %d\nant_quota_usage{%s,unit=\"packages\"} %d\n", l, x.usage[0], l, x.usage[1])
	}
	fmt.Fprintln(w, "# HELP ant_quota_limit Configured quota limit.\n# TYPE ant_quota_limit gauge")
	for _, x := range rows {
		l := fmt.Sprintf("scope=%q,feed=%q,namespace=%q", x.rule.Scope, x.rule.FeedID, x.rule.Namespace)
		if x.rule.MaxBytes != nil {
			fmt.Fprintf(w, "ant_quota_limit{%s,unit=\"bytes\"} %d\n", l, *x.rule.MaxBytes)
		}
		if x.rule.MaxPackages != nil {
			fmt.Fprintf(w, "ant_quota_limit{%s,unit=\"packages\"} %d\n", l, *x.rule.MaxPackages)
		}
	}
}
