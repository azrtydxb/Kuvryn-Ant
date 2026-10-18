package remoteconfig

import (
	"reflect"
	"testing"
)

func TestNormalizeTrustAndPolicyBounds(t *testing.T) {
	c := DefaultConfig("https://EXAMPLE.org:443/repository/")
	c.RedirectOrigins = []string{"https://z.example:443/", "https://A.example"}
	got, err := Normalize(c)
	if err != nil || got.Origin != "https://example.org/repository/" || !reflect.DeepEqual(got.RedirectOrigins, []string{"https://a.example", "https://z.example"}) {
		t.Fatal(got, err)
	}
	got.RedirectOrigins[0] = "changed"
	if c.RedirectOrigins[0] != "https://z.example:443/" {
		t.Fatal("input mutated")
	}
	for _, raw := range []string{"http://example.org", "https://user:password@example.org", "https://example.org?token=secret", "https://example.org?", "https://example.org/#secret", "https://example.org/a/../b", "https://example.org/%2e%2e", "https://example.org/a%2fb", "https://example.org/a//b", "https://example.org/a\\b", "https://example.org:", "https://example.org:99999", "https://example.org.", "https://[fe80::1%25eth0]"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Normalize(DefaultConfig(raw)); err != ErrInput {
				t.Fatal("unsafe URL accepted", err)
			}
		})
	}
	for name, mutate := range map[string]func(*Config){
		"timeout zero": func(c *Config) { c.TimeoutSeconds = 0 }, "timeout max": func(c *Config) { c.TimeoutSeconds = 301 },
		"metadata zero": func(c *Config) { c.Cache.MetadataTTLSeconds = 0 }, "metadata max": func(c *Config) { c.Cache.MetadataTTLSeconds = 86401 },
		"negative max": func(c *Config) { c.Cache.NegativeTTLSeconds = 301 }, "negative zero": func(c *Config) { c.Cache.NegativeTTLSeconds = 0 },
		"stale negative": func(c *Config) { c.Cache.StaleIfErrorSeconds = -1 }, "stale max": func(c *Config) { c.Cache.StaleIfErrorSeconds = 86401 },
		"entries max": func(c *Config) { c.Cache.MaxEntries = 100001 }, "bytes max": func(c *Config) { c.Cache.MaxBytes = 1<<30 + 1 },
		"path CA": func(c *Config) { c.CARef = "/etc/passwd" }, "bad credential": func(c *Config) { c.CredentialID = "secret" },
		"redirect path": func(c *Config) { c.RedirectOrigins = []string{"https://redirect.example/path"} },
		"redirect duplicate": func(c *Config) {
			c.RedirectOrigins = []string{"https://redirect.example", "https://REDIRECT.example:443/"}
		},
		"redirect max": func(c *Config) { c.RedirectOrigins = make([]string, 17) },
	} {
		t.Run(name, func(t *testing.T) {
			c := DefaultConfig("https://example.org")
			mutate(&c)
			if _, err := Normalize(c); err != ErrInput {
				t.Fatal(err)
			}
		})
	}
	c = DefaultConfig("https://example.org")
	c.TimeoutSeconds = 300
	c.Cache.MetadataTTLSeconds = 86400
	c.Cache.NegativeTTLSeconds = 300
	c.Cache.StaleIfErrorSeconds = 86400
	c.Cache.MaxEntries = 100000
	c.Cache.MaxBytes = 1 << 30
	if _, err = Normalize(c); err != nil {
		t.Fatal("valid upper bounds", err)
	}
	if CredentialPurpose("unsafe") == "remote-source:unsafe" {
		t.Fatal("invalid source purpose")
	}
}
