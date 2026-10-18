package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestS3AllowInsecureHTTPOptIn(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	load := func(value string) (Config, error) {
		return Load(func(k string) string {
			switch k {
			case "ANT_DATABASE_URL":
				return "postgres://ant:secret@localhost/ant"
			case "ANT_ROOT_KEY_FILE":
				return key
			case "ANT_S3_ALLOW_INSECURE_HTTP":
				return value
			}
			return ""
		})
	}
	if c, err := load(""); err != nil || c.Storage.AllowInsecureHTTP {
		t.Fatal("insecure HTTP must default to off")
	}
	if c, err := load("true"); err != nil || !c.Storage.AllowInsecureHTTP {
		t.Fatal("opt-in not honoured")
	}
	if _, err := load("maybe"); err == nil {
		t.Fatal("accepted invalid value")
	}
}
