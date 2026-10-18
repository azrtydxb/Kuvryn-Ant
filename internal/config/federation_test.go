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
