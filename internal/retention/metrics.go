package retention

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// pressureStates is the closed set of state label values.
var pressureStates = []string{StateDisabled, StateNoCapacity, StateInvalid, StateOK, StatePressure, StateUnevictable}

// pressureMetrics holds the process view of the last real pressure check. Dry
// runs never touch it.
type pressureMetrics struct {
	mu              sync.Mutex
	seen            bool
	state           string
	enabled         bool
	used, capacity  int64
	ratio           float64
	reclaimable     int64
	lastRun         time.Time
	evictedVersions uint64
	evictedBytes    uint64
}

var pressureMetricsState = &pressureMetrics{}

func (m *pressureMetrics) observe(rep PressureReport, enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen, m.state, m.enabled = true, rep.State, enabled
	m.used, m.capacity, m.ratio, m.reclaimable = rep.UsedBytes, rep.CapacityBytes, rep.UsageRatio, rep.ReclaimableBytes
	m.lastRun = time.Now()
	m.evictedVersions += uint64(rep.EvictedVersions)
	m.evictedBytes += uint64(max(0, rep.EvictedBytes))
}

// PressureWarning returns a health warning while the last check found the
// storage above its high watermark with nothing evictable.
func PressureWarning() (string, bool) {
	m := pressureMetricsState
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen && m.state == StateUnevictable {
		return "storage_pressure_unevictable", true
	}
	return "", false
}

// WriteMetrics emits the storage-pressure metrics in Prometheus text format.
// It registers as an observability collector; it is cheap and allocation-light.
func WriteMetrics(w io.Writer) {
	m := pressureMetricsState
	m.mu.Lock()
	seen, state, enabled := m.seen, m.state, m.enabled
	used, capacity, ratio, reclaimable := m.used, m.capacity, m.ratio, m.reclaimable
	last, versions, bytes := m.lastRun, m.evictedVersions, m.evictedBytes
	m.mu.Unlock()
	if !seen {
		return
	}
	gauge := func(name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	counter := func(name, help string, v uint64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	gauge("ant_storage_pressure_enabled", "1 when storage-pressure eviction is enabled.", flag(enabled))
	gauge("ant_storage_usage_ratio", "Used share of storage capacity (filesystem or configured quota), 0 when capacity is unknown.", ratio)
	gauge("ant_storage_used_bytes", "Bytes used against the measured capacity.", used)
	gauge("ant_storage_capacity_bytes", "Capacity in bytes (filesystem size or configured quota), 0 when unknown.", capacity)
	gauge("ant_storage_reclaimable_bytes", "Unreferenced bytes that garbage collection will free.", reclaimable)
	gauge("ant_storage_pressure_unevictable", "1 when usage is above the high watermark and nothing is evictable.", flag(state == StateUnevictable))
	gauge("ant_storage_pressure_last_run_timestamp_seconds", "Unix time of the last storage-pressure check.", last.Unix())
	counter("ant_storage_pressure_evicted_versions_total", "Versions evicted under storage pressure by this process.", versions)
	counter("ant_storage_pressure_evicted_bytes_total", "Bytes made reclaimable by pressure eviction in this process.", bytes)
	fmt.Fprintln(w, "# HELP ant_storage_pressure_state Current storage-pressure state (one-hot).\n# TYPE ant_storage_pressure_state gauge")
	for _, s := range pressureStates {
		fmt.Fprintf(w, "ant_storage_pressure_state{state=%q} %d\n", s, flag(s == state))
	}
}
