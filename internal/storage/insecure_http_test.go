package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryAllowsHTTPEndpointOnlyWithOptIn(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "credentials.json")
	if err := os.WriteFile(file, []byte(`{"access_key_id":"access","secret_access_key":"private-value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Backend: "s3", Directory: root, Endpoint: "http://minio.example.test:9000", Region: "test", Bucket: "bucket", CredentialsFile: file, PathStyle: true}
	if _, err := New(c); !errors.Is(err, ErrConfiguration) {
		t.Fatal("http endpoint accepted without opt-in")
	}
	c.AllowInsecureHTTP = true
	if _, err := New(c); err != nil {
		t.Fatalf("http endpoint rejected with opt-in: %v", err)
	}
}
