package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTracingDefaultsOffAndValidates(t *testing.T) {
	c, err := loadTracing(env(nil))
	if err != nil || c.Enabled {
		t.Fatalf("tracing must default to off: %+v %v", c, err)
	}
	c, err = loadTracing(env(map[string]string{"ANT_OTEL_ENABLED": "true", "ANT_OTEL_ENDPOINT": "http://otel:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_SERVICE_NAME": "ant-prod", "ANT_OTEL_SAMPLE_RATIO": "0.25"}))
	if err != nil || !c.Enabled || c.Protocol != "grpc" || c.ServiceName != "ant-prod" || c.SampleRatio != 0.25 || c.Endpoint != "http://otel:4318" {
		t.Fatalf("unexpected config: %+v %v", c, err)
	}
	for name, vars := range map[string]map[string]string{
		"bad flag":     {"ANT_OTEL_ENABLED": "maybe"},
		"credentials":  {"ANT_OTEL_ENABLED": "1", "ANT_OTEL_ENDPOINT": "http://user:pw@otel:4318"},
		"scheme":       {"ANT_OTEL_ENABLED": "1", "ANT_OTEL_ENDPOINT": "ftp://otel"},
		"protocol":     {"ANT_OTEL_ENABLED": "1", "ANT_OTEL_PROTOCOL": "udp"},
		"ratio high":   {"ANT_OTEL_ENABLED": "1", "ANT_OTEL_SAMPLE_RATIO": "2"},
		"ratio zero":   {"ANT_OTEL_ENABLED": "1", "ANT_OTEL_SAMPLE_RATIO": "0"},
		"ratio nonnum": {"ANT_OTEL_ENABLED": "1", "ANT_OTEL_SAMPLE_RATIO": "x"},
	} {
		if _, err := loadTracing(env(vars)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAuditRetentionDefaultsAndBounds(t *testing.T) {
	a, err := loadAuditRetention(env(nil))
	if err != nil || a.Retain != 400*24*time.Hour || a.Interval != time.Hour || a.BatchSize != 5000 {
		t.Fatalf("defaults: %+v %v", a, err)
	}
	if a, err = loadAuditRetention(env(map[string]string{"ANT_AUDIT_RETENTION": "0"})); err != nil || a.Retain != 0 {
		t.Fatalf("0 must disable: %+v %v", a, err)
	}
	for _, vars := range []map[string]string{
		{"ANT_AUDIT_RETENTION": "1h"}, {"ANT_AUDIT_RETENTION": "soon"}, {"ANT_AUDIT_PURGE_INTERVAL": "1s"}, {"ANT_AUDIT_PURGE_BATCH": "0"},
	} {
		if _, err := loadAuditRetention(env(vars)); err == nil {
			t.Errorf("%v accepted", vars)
		}
	}
}

func TestTokenCleanupDaysFromEnvironment(t *testing.T) {
	if d, err := loadTokenCleanupDays(env(nil)); err != nil || d != nil {
		t.Fatalf("unset must defer to the stored setting: %v %v", d, err)
	}
	for in, want := range map[string]int{"0": 0, "30": 30, "3650": 3650} {
		if d, err := loadTokenCleanupDays(env(map[string]string{"ANT_TOKEN_CLEANUP_AFTER_DAYS": in})); err != nil || d == nil || *d != want {
			t.Fatalf("%s: %v %v", in, d, err)
		}
	}
	for _, bad := range []string{"-1", "3651", "x", "1.5"} {
		if _, err := loadTokenCleanupDays(env(map[string]string{"ANT_TOKEN_CLEANUP_AFTER_DAYS": bad})); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
