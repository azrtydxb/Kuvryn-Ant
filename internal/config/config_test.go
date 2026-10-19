package config

import (
	"os"
	"path/filepath"
	"runtime"
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
		{"tiny db pool", "ANT_DB_MAX_CONNS", "1"},
		{"no upload slots", "ANT_UPLOAD_SLOTS", "0"},
		{"invalid duration", "ANT_SHUTDOWN_TIMEOUT", "0s"},
		{"invalid trusted proxy", "ANT_TRUSTED_PROXIES", "10.0.0.0/8,sensitive"},
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
	if len(c.RootKey) != 32 || int(c.Database.MaxConns) != DefaultDBMaxConns(runtime.GOMAXPROCS(0)) {
		t.Fatal("invalid config")
	}
	if len(c.TrustedProxies) != 0 {
		t.Fatal("proxies trusted by default")
	}
	c, err = Load(func(k string) string {
		if k == "ANT_TRUSTED_PROXIES" {
			return "10.42.0.0/16, 192.168.10.5"
		}
		return valid[k]
	})
	if err != nil || len(c.TrustedProxies) != 2 {
		t.Fatalf("trusted proxies: %v %v", c.TrustedProxies, err)
	}
	if err := os.WriteFile(key, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(func(k string) string { return valid[k] }); err == nil {
		t.Fatal("accepted short key")
	}
}

func TestProjectedRootKeyAndInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "projected-key")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	load := func(path string) (Config, error) {
		return Load(func(k string) string {
			switch k {
			case "ANT_DATABASE_URL":
				return "postgres://ant:secret@localhost/ant"
			case "ANT_ROOT_KEY_FILE":
				return path
			}
			return ""
		})
	}
	cfg, err := load(link)
	if err != nil || len(cfg.RootKey) != 32 {
		t.Fatalf("projected secret rejected: %v", err)
	}
	clear(cfg.RootKey)
	if _, err := load(dir); err == nil {
		t.Fatal("accepted directory as key")
	}
	if err := os.WriteFile(key, make([]byte, 33), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(link); err == nil {
		t.Fatal("accepted oversized key")
	}
}

func TestResourceLimitsScaleWithCPUAndAreConfigurable(t *testing.T) {
	if DefaultDBMaxConns(1) != 16 || DefaultDBMaxConns(4) != 32 || DefaultDBMaxConns(64) != 64 {
		t.Fatalf("db pool scaling: %d %d %d", DefaultDBMaxConns(1), DefaultDBMaxConns(4), DefaultDBMaxConns(64))
	}
	if DefaultMaxRequests(1) != 128 || DefaultMaxRequests(16) != 512 || DefaultMaxRequests(1000) != 1024 {
		t.Fatal("request limit scaling")
	}
	if DefaultUploadSlots(1) != 4 || DefaultUploadSlots(8) != 16 || DefaultUploadSlots(100) != 32 {
		t.Fatal("upload slot scaling")
	}
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ANT_DATABASE_URL": "postgres://ant:secret@localhost/ant", "ANT_ROOT_KEY_FILE": key, "ANT_DB_MAX_CONNS": "48", "ANT_UPLOAD_SLOTS": "12", "ANT_MAX_REQUESTS": "300"}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.MaxConns != 48 || c.UploadSlots != 12 || c.MaxRequests != 300 {
		t.Fatalf("overrides ignored: %d %d %d", c.Database.MaxConns, c.UploadSlots, c.MaxRequests)
	}
}
