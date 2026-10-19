package federation

import (
	"context"
	"encoding/json"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// PolicyStatus is the applied policy head in federation status.
type PolicyStatus struct {
	Generation  int64        `json:"generation"`
	Hash        string       `json:"hash,omitempty"`
	AppliedAt   *time.Time   `json:"applied_at,omitempty"`
	ConfirmedAt *time.Time   `json:"confirmed_at,omitempty"`
	AgeSeconds  int64        `json:"age_seconds"`
	Staleness   string       `json:"staleness"` // fresh | warning | critical
	Error       string       `json:"error,omitempty"`
	ErrorAt     *time.Time   `json:"error_at,omitempty"`
	Report      PolicyReport `json:"report"`
}

// Alert is an active federation alert. Alerts never affect serving; they mark
// federation health degraded and are raised once per transition as audit and
// outbox events (federation.alert.raised / federation.alert.cleared), so
// webhooks and audit sinks deliver them.
type Alert struct {
	Code     string `json:"code"`     // policy_stale | policy_rejected | membership_stale | policy_conflicts | topology_rejected | feed_home_conflict
	Severity string `json:"severity"` // warning | critical
	Message  string `json:"message"`
}

func (s *Service) policyStatus(st State, ps PolicyState) *PolicyStatus {
	out := &PolicyStatus{Generation: ps.Generation, Hash: ps.Hash, AppliedAt: ps.AppliedAt, ConfirmedAt: ps.ConfirmedAt, Error: ps.Error, ErrorAt: ps.ErrorAt, Report: ps.Report, Staleness: "fresh"}
	if ps.Generation == 0 {
		return out
	}
	fresh := time.Time{}
	if ps.AppliedAt != nil {
		fresh = *ps.AppliedAt
	}
	if ps.ConfirmedAt != nil && ps.ConfirmedAt.After(fresh) {
		fresh = *ps.ConfirmedAt
	}
	if st.Role == "authority" {
		fresh = s.now()
	}
	out.AgeSeconds = max(0, int64(s.now().Sub(fresh)/time.Second))
	switch age := time.Duration(out.AgeSeconds) * time.Second; {
	case age > StaleCritical:
		out.Staleness = "critical"
	case age > StaleWarning:
		out.Staleness = "warning"
	}
	return out
}

// computeAlerts derives the active alerts from status.
func computeAlerts(st State, ps PolicyState, pol *PolicyStatus, now time.Time) []Alert {
	out := []Alert{}
	if st.State != "active" {
		return out
	}
	if pol != nil && pol.Staleness != "fresh" {
		out = append(out, Alert{Code: "policy_stale", Severity: pol.Staleness,
			Message: "federation policy not confirmed current for " + (time.Duration(pol.AgeSeconds) * time.Second).String() + "; the last known good policy stays enforced"})
	}
	if ps.Error != "" {
		out = append(out, Alert{Code: "policy_rejected", Severity: "warning", Message: ps.Error + "; the last known good policy stays enforced"})
	}
	if st.Role != "authority" && st.AppliedGeneration > 0 {
		fresh := time.Time{}
		if st.AppliedAt != nil {
			fresh = *st.AppliedAt
		}
		if st.LastSyncSuccessAt != nil && st.LastSyncSuccessAt.After(fresh) {
			fresh = *st.LastSyncSuccessAt
		}
		if age := now.Sub(fresh); age > StaleWarning {
			sev := "warning"
			if age > StaleCritical {
				sev = "critical"
			}
			out = append(out, Alert{Code: "membership_stale", Severity: sev, Message: "membership not confirmed current for " + age.Truncate(time.Second).String()})
		}
	}
	if pol != nil && len(pol.Report.Conflicts) > 0 {
		out = append(out, Alert{Code: "policy_conflicts", Severity: "warning", Message: "federated bindings match local principals; adopt or resolve them"})
	}
	sortAlerts(out)
	return out
}

func sortAlerts(a []Alert) { sort.Slice(a, func(i, j int) bool { return a[i].Code < a[j].Code }) }

// topologyAlerts derives coordinated-replication alerts: a rejected topology
// document (the last known good stays applied) and quarantined content after
// a forced home transfer (feed_home_conflict, §13).
func topologyAlerts(t *TopologyStatus) []Alert {
	out := []Alert{}
	if t == nil {
		return out
	}
	if t.Error != "" {
		out = append(out, Alert{Code: "topology_rejected", Severity: "warning", Message: t.Error + "; the last known good topology stays applied"})
	}
	if t.Quarantines > 0 {
		out = append(out, Alert{Code: "feed_home_conflict", Severity: "critical", Message: "content written on an old home after a forced transfer is quarantined; review and resolve it"})
	}
	return out
}

// EvaluateAlerts records alert transitions as audit/outbox events and returns
// the active alerts. It runs in the federation worker after every sync.
func (s *Service) EvaluateAlerts(ctx context.Context) ([]Alert, error) {
	var active []Alert
	err := s.tx(ctx, func(tx pgx.Tx) error {
		st, err := lockState(ctx, tx, s.cfg.InstanceID)
		if err != nil || st.State != "active" {
			return err
		}
		ps, err := loadPolicyState(ctx, tx, s.cfg.InstanceID)
		if err != nil {
			return err
		}
		pol := s.policyStatus(st, ps)
		s.policyHeadGauge.Store(pol)
		active = computeAlerts(st, ps, pol, s.now())
		if topo, err := s.topologyStatus(ctx, st); err == nil {
			active = append(active, topologyAlerts(topo)...)
			sortAlerts(active)
		}
		next := map[string]string{}
		for _, a := range active {
			next[a.Code] = a.Severity
			if ps.Alerts[a.Code] != a.Severity {
				if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.alert.raised", st.FederationID, "success", map[string]any{"code": a.Code, "severity": a.Severity, "message": a.Message}); err != nil {
					return err
				}
			}
		}
		for code := range ps.Alerts {
			if _, ok := next[code]; !ok {
				if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.alert.cleared", st.FederationID, "success", map[string]any{"code": code}); err != nil {
					return err
				}
			}
		}
		raw, _ := json.Marshal(next)
		_, err = tx.Exec(ctx, `UPDATE federation_state SET alert_state=$2 WHERE instance_id=$1`, s.cfg.InstanceID, raw)
		s.alerts.Store(&active)
		return err
	})
	return active, err
}

// assertionMetrics counts assertion outcomes for ant_federation_assertions_total.
type assertionMetrics struct {
	accepted, rejected, issued atomic.Int64
}
