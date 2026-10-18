package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpstreamCAMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca-map.json")
	for _, raw := range []string{`null`, `{"a":"x","a":"y"}`, `{"../a":"x"}`, `{"a":null}`, `{"a":"x"} {}`, `[]`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := upstreamCAs(path); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := os.WriteFile(path, []byte(`{"minio":"ca.pem","corp":"/mounted/corp.pem"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := upstreamCAs(path)
	if err != nil || got["minio"] != filepath.Join(dir, "ca.pem") || got["corp"] != "/mounted/corp.pem" {
		t.Fatal(got, err)
	}
	if _, err = upstreamCAs(dir); err == nil {
		t.Fatal("accepted directory")
	}
	if got, err = upstreamCAs(""); err != nil || len(got) != 0 {
		t.Fatal("empty map rejected")
	}
}
