package hooks

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

func TestTemplateRendersPathsAndFilters(t *testing.T) {
	doc := decodePayload([]byte(`{"release":{"tag_name":"v1.2.3","draft":false},"repository":{"full_name":"Acme/Tool"},"ref":"refs/tags/v2.0.0","list":[{"v":"x"},{"v":"y"}],"n":42}`))
	for tpl, want := range map[string]string{
		"{{ .release.tag_name }}":                    "v1.2.3",
		"{{.release.tag_name | trimv}}":              "1.2.3",
		"{{ .repository.full_name | lower }}":        "acme/tool",
		"{{ .repository.full_name | upper }}":        "ACME/TOOL",
		"{{ .ref | base | trimv }}":                  "2.0.0",
		"img:{{ .list.1.v }}-{{ .n }}":               "img:y-42",
		"{{ .release.draft }}":                       "false",
		"plain text":                                 "plain text",
		"{{ .repository.full_name | lower | base }}": "tool",
	} {
		got, err := Render(tpl, doc)
		if err != nil || got != want {
			t.Fatalf("%q -> %q (%v), want %q", tpl, got, err, want)
		}
	}
}

func TestTemplateRejectsAnythingBeyondPaths(t *testing.T) {
	doc := decodePayload([]byte(`{"a":{"b":"c"},"arr":[1],"empty":"","nil":null}`))
	for _, tpl := range []string{
		"{{ .missing }}", "{{ .a }}", "{{ .arr }}", "{{ .empty }}", "{{ .nil }}", "{{ .arr.5 }}", // unresolved or structural
		"{{ printf \"%s\" .a.b }}", "{{ exec \"id\" }}", "{{ .a.b | html }}", "{{ range .arr }}x{{ end }}",
		"{{ if .a }}x{{ end }}", "{{ $x := .a.b }}", "{{ .a.b }", "{{ a.b }}", "{{ . }}", "{{ .a..b }}", "{{ .a.b() }}",
		"{{ ." + strings.Repeat("a", 200) + " }}", strings.Repeat("x", 2000),
	} {
		if _, err := Render(tpl, doc); err == nil {
			t.Fatalf("template accepted: %q", tpl)
		}
	}
	if _, err := Render("{{ .a.b }}", nil); err == nil {
		t.Fatal("rendered against a non-JSON payload")
	}
	// Output stays bounded.
	big := decodePayload([]byte(`{"s":"` + strings.Repeat("y", 900) + `"}`))
	if _, err := Render("{{ .s }}{{ .s }}", big); err == nil {
		t.Fatal("oversized result accepted")
	}
}

func TestPreviewRedactsSecrets(t *testing.T) {
	body := `{"event":"push","token":"abc123","nested":{"password":"hunter2","api_key":"k","ok":"visible","ghp":"ghp_` + strings.Repeat("A", 36) + `"},"list":["Bearer abcdefghijklmnop","fine"],"url":"https://x.example.com"}`
	p := Preview([]byte(body))
	for _, leak := range []string{"abc123", "hunter2", "ghp_AAAA", "abcdefghijklmnop"} {
		if strings.Contains(p, leak) {
			t.Fatalf("preview leaks %q: %s", leak, p)
		}
	}
	for _, keep := range []string{"visible", "fine", "https://x.example.com", redacted} {
		if !strings.Contains(p, keep) {
			t.Fatalf("preview lost %q: %s", keep, p)
		}
	}
	text := Preview([]byte("Authorization: Bearer abcdefghijklmnop\npassword=hunter2&x=1 token: sekret"))
	for _, leak := range []string{"abcdefghijklmnop", "hunter2", "sekret"} {
		if strings.Contains(text, leak) {
			t.Fatalf("text preview leaks %q: %s", leak, text)
		}
	}
	long := Preview([]byte(`{"a":"` + strings.Repeat("é", 5000) + `"}`))
	if len(long) > PreviewBytes+4 || !strings.HasSuffix(long, "…") {
		t.Fatalf("preview not truncated: %d", len(long))
	}
}

func TestVerifyHMACFormats(t *testing.T) {
	key := []byte(strings.Repeat("k", 40))
	other := []byte(strings.Repeat("o", 40))
	body := []byte(`{"hello":"world"}`)
	for format := range HMACFormats {
		name, value := SignBody(format, body, key)
		h := http.Header{}
		h.Set(name, value)
		if !VerifyHMAC(format, h, body, [][]byte{key}) {
			t.Fatalf("%s: valid signature rejected", format)
		}
		if VerifyHMAC(format, h, append([]byte(nil), body[:len(body)-1]...), [][]byte{key}) && format != FormatGitLab {
			t.Fatalf("%s: tampered body accepted", format)
		}
		if VerifyHMAC(format, h, body, [][]byte{other}) {
			t.Fatalf("%s: wrong key accepted", format)
		}
		if VerifyHMAC(format, http.Header{}, body, [][]byte{key}) {
			t.Fatalf("%s: missing header accepted", format)
		}
		// A rotation grace window holds two keys; either one verifies.
		if !VerifyHMAC(format, h, body, [][]byte{other, key}) || !VerifyHMAC(format, h, body, [][]byte{key, other}) {
			t.Fatalf("%s: previous key not honoured", format)
		}
		bad := http.Header{}
		bad.Set(name, "sha256=zz")
		if VerifyHMAC(format, bad, body, [][]byte{key}) {
			t.Fatalf("%s: malformed signature accepted", format)
		}
	}
	if VerifyHMAC("nope", http.Header{}, body, [][]byte{key}) || VerifyHMAC(FormatGitHub, http.Header{}, body, nil) {
		t.Fatal("unknown format or no key accepted")
	}
	// Wire formats against independently computed values.
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !VerifyHMAC(FormatGitHub, h, body, [][]byte{key}) {
		t.Fatal("GitHub wire format")
	}
	m1 := hmac.New(sha1.New, key)
	m1.Write(body)
	n := http.Header{}
	n.Set("X-Nexus-Webhook-Signature", hex.EncodeToString(m1.Sum(nil)))
	if !VerifyHMAC(FormatNexus, n, body, [][]byte{key}) {
		t.Fatal("Nexus wire format")
	}
	g := http.Header{}
	g.Set("X-Gitlab-Token", string(key))
	if !VerifyHMAC(FormatGitLab, g, body, [][]byte{key}) {
		t.Fatal("GitLab token")
	}
	g.Set("X-Gitlab-Token", string(key)+"x")
	if VerifyHMAC(FormatGitLab, g, body, [][]byte{key}) {
		t.Fatal("GitLab token of a different length accepted")
	}
	// The JFrog header may carry the secret itself on older servers.
	j := http.Header{}
	j.Set("X-JFrog-Event-Auth", string(key))
	if !VerifyHMAC(FormatArtifactory, j, body, [][]byte{key}) {
		t.Fatal("Artifactory shared secret")
	}
}

func TestDeliveryRefHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Delivery", "abc")
	if deliveryRef(h) != "X-GitHub-Delivery:abc" {
		t.Fatal(deliveryRef(h))
	}
	g := http.Header{}
	g.Set("X-Gitlab-Event-UUID", "u-1")
	if deliveryRef(g) != "X-Gitlab-Event-UUID:u-1" || deliveryRef(http.Header{}) != "" {
		t.Fatal("gitlab/none")
	}
}

func TestPickHeadersDropsCredentials(t *testing.T) {
	h := http.Header{}
	for k, v := range map[string]string{"Authorization": "Bearer x", "X-Gitlab-Token": "t", "Cookie": "c", "X-GitHub-Event": "release", "Content-Type": "application/json", "X-Hub-Signature-256": "sha256=ab", "X-Forwarded-For": "1.2.3.4"} {
		h.Set(k, v)
	}
	got := pickHeaders(h)
	for _, forbidden := range []string{"Authorization", "X-Gitlab-Token", "Cookie", "X-Forwarded-For"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("%s kept", forbidden)
		}
	}
	if got["X-Github-Event"] != "release" && got["X-GitHub-Event"] != "release" {
		t.Fatalf("event header lost: %v", got)
	}
	// A re-signing relay drops the original body signature; pass-through keeps it.
	if relayHeaders(got, true).Get("X-Hub-Signature-256") != "" || relayHeaders(got, false).Get("X-Hub-Signature-256") == "" {
		t.Fatal("signature header handling")
	}
}

func TestSlugAndActionValidationBasics(t *testing.T) {
	for _, bad := range []string{"", "Up", "a_b", "a/b", strings.Repeat("a", 64)} {
		if ValidSlug(bad) {
			t.Fatalf("slug accepted %q", bad)
		}
	}
	if !ValidSlug("release-notify") || !ValidSlug(strings.Repeat("a", 63)) {
		t.Fatal("good slug rejected")
	}
	if err := CheckTemplate("{{ .a.b | lower }}"); err != nil {
		t.Fatal(err)
	}
	if got := TemplatePaths("{{ .a.b }}-{{ .c }}"); len(got) != 2 || got[0][1] != "b" {
		t.Fatalf("paths %v", got)
	}
}
