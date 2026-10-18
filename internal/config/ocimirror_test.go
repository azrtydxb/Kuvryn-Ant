package config

import "testing"

func TestLoadOCIMirror(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	m, err := loadOCIMirror(env(map[string]string{"ANT_OCI_DEFAULT_FEED": "dockerhub", "ANT_OCI_MIRROR_MAP": "Docker.io=dockerhub, ghcr.io=ghcr,localhost:5000=local"}))
	if err != nil || m.DefaultFeed != "dockerhub" || m.Namespaces["docker.io"] != "dockerhub" || m.Namespaces["ghcr.io"] != "ghcr" || m.Namespaces["localhost:5000"] != "local" {
		t.Fatalf("%+v %v", m, err)
	}
	if m, err = loadOCIMirror(env(nil)); err != nil || m.DefaultFeed != "" || m.Namespaces != nil {
		t.Fatalf("empty: %+v %v", m, err)
	}
	for _, bad := range []map[string]string{
		{"ANT_OCI_DEFAULT_FEED": "Bad Slug"},
		{"ANT_OCI_MIRROR_MAP": "docker.io"},
		{"ANT_OCI_MIRROR_MAP": "docker.io=a,docker.io=b"},
		{"ANT_OCI_MIRROR_MAP": "http://x=a"},
	} {
		if _, err := loadOCIMirror(env(bad)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
