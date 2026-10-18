package events

import "testing"

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
