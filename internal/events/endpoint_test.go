package events

import (
	"strings"
	"testing"
)

func TestConfigurationEndpointPolicy(t *testing.T) {
	for _, endpoint := range []string{"http://example.com/hook", "https://localhost/hook", "https://a.localhost/hook", "https://127.0.0.1/hook", "https://[::1]/hook", "https://169.254.169.254/hook", "https://user:password@example.com/hook", "https://example.com/hook?secret=value"} {
		if _, err := ValidateEndpoint(endpoint); err == nil {
			t.Fatalf("unsafe endpoint accepted %q", endpoint)
		}
	}
	if got, err := ValidateEndpoint(" https://example.com/hook "); err != nil || got != "https://example.com/hook" {
		t.Fatal("valid endpoint rejected")
	}
}

func TestSlugValidation(t *testing.T) {
	for _, bad := range []string{"", "Up", "a_b", "a/b", "a b", strings.Repeat("a", 64)} {
		if ValidSlug(bad) {
			t.Fatalf("slug accepted: %q", bad)
		}
	}
	for _, good := range []string{"a", "deploy-1", strings.Repeat("a", 63)} {
		if !ValidSlug(good) {
			t.Fatalf("slug rejected: %q", good)
		}
	}
}

func TestEndpointAllowHTTP(t *testing.T) {
	if _, err := ValidateEndpointFor("http://hooks.example.com/x", true); err != nil {
		t.Fatal("http refused although allowed")
	}
	if _, err := ValidateEndpointFor("http://hooks.example.com/x", false); err == nil {
		t.Fatal("http accepted although not allowed")
	}
}
