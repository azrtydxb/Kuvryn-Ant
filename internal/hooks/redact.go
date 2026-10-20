package hooks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const redacted = "[redacted]"

var sensitiveKey = regexp.MustCompile(`(?i)(token|secret|passw|authoriz|api[_-]?key|private[_-]?key|credential|signature|cookie|session|bearer|client[_-]?secret)`)

// tokenShapes are values that look like credentials wherever they appear.
var tokenShapes = regexp.MustCompile(`(?i)\b(ant_[A-Za-z0-9_-]{43}|whsec_[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}|bearer\s+[A-Za-z0-9._~+/=-]{12,})`)

var textSecret = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|authorization|api[_-]?key)["']?\s*[:=]\s*["']?)[^\s"',&}]+`)

// Preview renders a payload for the delivery log: JSON is walked and every
// value under a sensitive-looking key replaced; credential-shaped strings are
// replaced anywhere; the result is truncated to PreviewBytes on a rune boundary.
// The stored preview never contains a secret a sender put in the body.
func Preview(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var out string
	if v := decodePayload(body); v != nil {
		out = marshalCompact(scrub(v))
	} else {
		text := strings.ToValidUTF8(string(body[:min(len(body), 4*PreviewBytes)]), "?")
		out = textSecret.ReplaceAllString(tokenShapes.ReplaceAllString(text, redacted), "${1}"+redacted)
	}
	return truncate(out, PreviewBytes)
}

func marshalCompact(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSpace(b.String())
}

func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if sensitiveKey.MatchString(k) {
				out[k] = redacted
				continue
			}
			out[k] = scrub(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = scrub(val)
		}
		return out
	case string:
		return tokenShapes.ReplaceAllString(x, redacted)
	}
	return v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// storedHeaders are the request headers kept on a delivery: provider event and
// delivery identifiers and the content type. Credentials never appear here
// (Authorization, X-Gitlab-Token, cookies are not in the list).
var storedHeaders = []string{
	"Content-Type", "User-Agent",
	"X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID",
	"X-Gitlab-Event", "X-Gitlab-Event-UUID", "X-Gitlab-Instance",
	"X-Event-Key", "X-Request-UUID",
	"X-Nexus-Webhook-ID", "X-Nexus-Webhook-Delivery",
	"X-JFrog-Event-Type",
	"X-Ant-Delivery-Id",
}

// signatureHeaders are kept only for pass-through relays (not re-signed): the
// body signature is meaningless without the secret and the target may hold it.
var signatureHeaders = []string{"X-Hub-Signature-256", "X-NPM-Signature", "X-Ant-Signature"}

// pickHeaders copies the allowlisted headers of a request.
func pickHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, name := range append(append([]string{}, storedHeaders...), signatureHeaders...) {
		if v := h.Get(name); v != "" && len(v) <= 512 && printable(v, 512) {
			out[http.CanonicalHeaderKey(name)] = v
		}
	}
	return out
}
