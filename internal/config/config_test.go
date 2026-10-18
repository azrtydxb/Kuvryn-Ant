package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	valid := map[string]string{"ANT_DATABASE_URL": "postgres://ant:secret@localhost/ant", "ANT_ROOT_KEY_FILE": key}
	tests := []struct{ name, key, value string }{
		{"missing database", "ANT_DATABASE_URL", ""},
		{"malformed database", "ANT_DATABASE_URL", "postgres://sensitive:%zz@localhost/ant"},
		{"missing key", "ANT_ROOT_KEY_FILE", ""},
		{"unreadable key", "ANT_ROOT_KEY_FILE", "/nonexistent/sensitive"},
		{"invalid port", "ANT_LISTEN", "localhost:99999"},
		{"unbounded requests", "ANT_MAX_REQUESTS", "0"},
		{"invalid duration", "ANT_SHUTDOWN_TIMEOUT", "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(func(k string) string {
				if k == tt.key {
					return tt.value
				}
				return valid[k]
			})
			if err == nil {
				t.Fatal("expected rejection")
			}
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration value leaked")
			}
		})
	}
	c, err := Load(func(k string) string { return valid[k] })
	if err != nil {
		t.Fatal(err)
	}
	if len(c.RootKey) != 32 || c.Database.MaxConns != 16 {
		t.Fatal("invalid config")
	}
	if err := os.WriteFile(key, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(func(k string) string { return valid[k] }); err == nil {
		t.Fatal("accepted short key")
	}
}
