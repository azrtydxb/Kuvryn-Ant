package management

import (
	"testing"
	"time"
)

func TestHealthStatusTable(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	cases := []struct {
		name string
		h    SourceHealth
		want string
	}{
		{"no traffic", SourceHealth{}, "unknown"},
		{"only successes", SourceHealth{Requests: 5, LastSuccessAt: ago(time.Minute)}, "healthy"},
		{"never succeeded, fresh error", SourceHealth{Requests: 1, Failures: 1, LastErrorAt: ago(time.Minute)}, "failing"},
		{"never succeeded, stale error", SourceHealth{Requests: 1, Failures: 1, LastErrorAt: ago(48 * time.Hour)}, "unknown"},
		{"busy source, last event isolated error", SourceHealth{Requests: 5185, Failures: 414, LastSuccessAt: ago(2 * time.Second), LastErrorAt: ago(time.Second)}, "degraded"},
		{"error after success 2m ago", SourceHealth{Requests: 50, Failures: 1, LastSuccessAt: ago(2 * time.Minute), LastErrorAt: ago(time.Minute)}, "degraded"},
		{"broken for 20m", SourceHealth{Requests: 50, Failures: 9, LastSuccessAt: ago(20 * time.Minute), LastErrorAt: ago(time.Minute)}, "failing"},
		{"old success, error 1h ago", SourceHealth{Requests: 50, Failures: 9, LastSuccessAt: ago(3 * time.Hour), LastErrorAt: ago(time.Hour)}, "failing"},
		{"idle after failure for days", SourceHealth{Requests: 50, Failures: 9, LastSuccessAt: ago(100 * time.Hour), LastErrorAt: ago(99 * time.Hour)}, "unknown"},
		{"idle right after an isolated error", SourceHealth{Requests: 5278, Failures: 448, LastSuccessAt: ago(36*time.Minute + 12*time.Second), LastErrorAt: ago(36 * time.Minute)}, "degraded"},
		{"recovered, error 5m ago", SourceHealth{Requests: 50, Failures: 1, LastSuccessAt: ago(time.Minute), LastErrorAt: ago(5 * time.Minute)}, "degraded"},
		{"recovered, error 1h ago", SourceHealth{Requests: 50, Failures: 1, LastSuccessAt: ago(time.Minute), LastErrorAt: ago(time.Hour)}, "healthy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := healthStatusAt(c.h, now); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}
