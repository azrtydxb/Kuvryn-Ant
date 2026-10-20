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

func TestSlugAndBaseURLResolution(t *testing.T) {
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
	for _, base := range []string{"https://hooks.example.com/ant", "https://hooks.example.com/ant/", "https://hooks.example.com/ant//"} {
		got, err := ResolveEndpoint(base, "deploys", false)
		if err != nil || got != "https://hooks.example.com/ant/deploys" {
			t.Fatalf("%s -> %q %v", base, got, err)
		}
	}
	if got, _ := ResolveEndpoint("https://hooks.example.com", "x", false); got != "https://hooks.example.com/x" {
		t.Fatalf("root base: %q", got)
	}
	for _, base := range []string{"http://hooks.example.com/", "https://u:p@hooks.example.com/", "https://hooks.example.com/?q=1", "https://localhost/", "https://10.0.0.1/", ""} {
		if _, err := NormalizeBaseURL(base, false); err == nil {
			t.Fatalf("base accepted: %q", base)
		}
	}
	if _, err := NormalizeBaseURL("http://hooks.example.com/", true); err != nil {
		t.Fatal("http refused although allowed")
	}
}
