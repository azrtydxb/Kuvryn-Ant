package config

import "testing"

func TestLoadFederation(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	f, err := loadFederation(env(nil))
	if err != nil || f.Listen != "" || f.PublicURL != "" {
		t.Fatalf("federation must be off by default: %+v %v", f, err)
	}
	f, err = loadFederation(env(map[string]string{"ANT_FEDERATION_LISTEN": ":8444", "ANT_FEDERATION_PUBLIC_URL": "https://fed.example:8444/"}))
	if err != nil || f.Listen != ":8444" || f.PublicURL != "https://fed.example:8444" {
		t.Fatalf("valid federation config: %+v %v", f, err)
	}
	dir := t.TempDir()
	for name, bad := range map[string]map[string]string{
		"bad listen":         {"ANT_FEDERATION_LISTEN": "8444"},
		"http public url":    {"ANT_FEDERATION_LISTEN": ":8444", "ANT_FEDERATION_PUBLIC_URL": "http://fed.example"},
		"public url path":    {"ANT_FEDERATION_LISTEN": ":8444", "ANT_FEDERATION_PUBLIC_URL": "https://fed.example/x"},
		"url without listen": {"ANT_FEDERATION_PUBLIC_URL": "https://fed.example"},
		"join without url":   {"ANT_FEDERATION_LISTEN": ":8444", "ANT_FEDERATION_JOIN_DIR": dir},
		"relative join dir":  {"ANT_FEDERATION_LISTEN": ":8444", "ANT_FEDERATION_PUBLIC_URL": "https://fed.example", "ANT_FEDERATION_JOIN_DIR": "join"},
	} {
		if _, err := loadFederation(env(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLoadFederationRouteLimits(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	f, err := loadFederation(env(map[string]string{
		"ANT_FEDERATION_ROUTE_MAX_STREAMS": "128", "ANT_FEDERATION_ROUTE_STREAMS_PER_PRINCIPAL": "4", "ANT_FEDERATION_ROUTE_STREAMS_PER_CLIENT": "6",
		"ANT_FEDERATION_ROUTE_PRINCIPAL_RATE": "20", "ANT_FEDERATION_ROUTE_MIN_TRANSFER_RATE": "32768",
		"ANT_FEDERATION_ROUTE_TRANSFER_GRACE": "45s", "ANT_FEDERATION_ROUTE_TRANSFER_TIMEOUT": "30m"}))
	if err != nil || f.RouteMaxStreams != 128 || f.RouteStreamsPerPrincipal != 4 || f.RouteStreamsPerClient != 6 || f.RoutePrincipalRate != 20 ||
		f.RouteMinTransferRate != 32768 || f.RouteTransferGrace.Seconds() != 45 || f.RouteTransferTimeout.Minutes() != 30 {
		t.Fatalf("route limits: %+v %v", f, err)
	}
	if f, err = loadFederation(env(nil)); err != nil || f.RouteMaxStreams != 0 || f.RouteTransferTimeout != 0 {
		t.Fatalf("defaults must stay zero: %+v %v", f, err)
	}
	for _, bad := range []map[string]string{
		{"ANT_FEDERATION_ROUTE_MAX_STREAMS": "-1"}, {"ANT_FEDERATION_ROUTE_PRINCIPAL_RATE": "fast"},
		{"ANT_FEDERATION_ROUTE_TRANSFER_GRACE": "30"}, {"ANT_FEDERATION_ROUTE_TRANSFER_TIMEOUT": "-5s"},
	} {
		if _, err := loadFederation(env(bad)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
