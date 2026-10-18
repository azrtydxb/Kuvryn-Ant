package config

import (
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/tracing"
)

// loadTracing reads the optional OpenTelemetry settings. Tracing is off unless
// ANT_OTEL_ENABLED is true. The OTLP exporters additionally honor the standard
// OTEL_EXPORTER_OTLP_* variables; ANT_OTEL_ENDPOINT and ANT_OTEL_PROTOCOL take
// precedence over OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_PROTOCOL.
func loadTracing(getenv func(string) string) (tracing.Config, error) {
	t := tracing.Config{ServiceName: "kuvryn-ant", SampleRatio: 1}
	if v := getenv("ANT_OTEL_ENABLED"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return t, errors.New("ANT_OTEL_ENABLED is invalid")
		}
		t.Enabled = on
	}
	if !t.Enabled {
		return t, nil
	}
	first := func(names ...string) string {
		for _, n := range names {
			if v := getenv(n); v != "" {
				return v
			}
		}
		return ""
	}
	t.Endpoint = getenv("ANT_OTEL_ENDPOINT")
	if t.Endpoint != "" {
		u, err := url.Parse(t.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return t, errors.New("ANT_OTEL_ENDPOINT must be an http(s) URL without credentials")
		}
	}
	t.Protocol = first("ANT_OTEL_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	switch t.Protocol {
	case "", "http/protobuf", "grpc":
	default:
		return t, errors.New("ANT_OTEL_PROTOCOL must be http/protobuf or grpc")
	}
	if v := first("ANT_OTEL_SERVICE_NAME", "OTEL_SERVICE_NAME"); v != "" {
		t.ServiceName = v
	}
	if v := getenv("ANT_OTEL_SAMPLE_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r <= 0 || r > 1 {
			return t, errors.New("ANT_OTEL_SAMPLE_RATIO must be greater than 0 and at most 1")
		}
		t.SampleRatio = r
	}
	return t, nil
}

// AuditRetention bounds audit_events growth. A zero Retain disables purging.
type AuditRetention struct {
	Retain    time.Duration
	Interval  time.Duration
	BatchSize int
}

const day = 24 * time.Hour

func loadAuditRetention(getenv func(string) string) (AuditRetention, error) {
	a := AuditRetention{Retain: 400 * day, Interval: time.Hour, BatchSize: 5000}
	var err error
	if v := getenv("ANT_AUDIT_RETENTION"); v != "" {
		if a.Retain, err = time.ParseDuration(v); err != nil || a.Retain != 0 && (a.Retain < 24*time.Hour || a.Retain > 20*365*day) {
			return a, errors.New("ANT_AUDIT_RETENTION must be 0 (keep forever) or between 24h and 175200h")
		}
	}
	if v := getenv("ANT_AUDIT_PURGE_INTERVAL"); v != "" {
		if a.Interval, err = time.ParseDuration(v); err != nil || a.Interval < time.Minute || a.Interval > 30*day {
			return a, errors.New("ANT_AUDIT_PURGE_INTERVAL must be between 1m and 720h")
		}
	}
	if v := getenv("ANT_AUDIT_PURGE_BATCH"); v != "" {
		if a.BatchSize, err = strconv.Atoi(v); err != nil || a.BatchSize < 1 || a.BatchSize > 100000 {
			return a, errors.New("ANT_AUDIT_PURGE_BATCH must be between 1 and 100000")
		}
	}
	return a, nil
}
