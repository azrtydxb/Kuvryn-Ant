package hooks

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // Nexus signs webhooks with HMAC-SHA1; this is a verifier for that wire format.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// HMACFormats lists the supported signature formats with the header each uses.
var HMACFormats = map[string]string{
	FormatGitHub:      "X-Hub-Signature-256",
	FormatGitLab:      "X-Gitlab-Token",
	FormatAnt:         "X-Ant-Signature",
	FormatNPM:         "X-NPM-Signature",
	FormatArtifactory: "X-JFrog-Event-Auth",
	FormatNexus:       "X-Nexus-Webhook-Signature",
}

// VerifyHMAC reports whether the request proves knowledge of one of the keys
// (the current secret and, inside a rotation grace window, the previous one).
// Every comparison is constant time: signatures are compared as decoded bytes
// with subtle.ConstantTimeCompare and the GitLab shared token is compared by
// digest so neither length nor content of the secret leaks through timing. All
// keys are always evaluated, so a match on the first key does not shorten the
// work.
func VerifyHMAC(format string, header http.Header, body []byte, keys [][]byte) bool {
	name, ok := HMACFormats[format]
	if !ok {
		return false
	}
	got := strings.TrimSpace(header.Get(name))
	if format == FormatGitLab {
		sum := sha256.Sum256([]byte(got))
		match := 0
		for _, key := range keys {
			want := sha256.Sum256(key)
			match |= subtle.ConstantTimeCompare(sum[:], want[:])
		}
		return got != "" && match == 1
	}
	candidates := signatureCandidates(got)
	match := 0
	for _, key := range keys {
		var mac []byte
		if format == FormatNexus {
			m := hmac.New(sha1.New, key) //nolint:gosec
			m.Write(body)
			mac = m.Sum(nil)
		} else {
			m := hmac.New(sha256.New, key)
			m.Write(body)
			mac = m.Sum(nil)
		}
		for _, c := range candidates {
			match |= subtle.ConstantTimeCompare(mac, c)
		}
		if len(candidates) == 0 {
			// Same work as a real comparison, so a missing signature is not faster.
			_ = subtle.ConstantTimeCompare(mac, make([]byte, len(mac)))
		}
		if format == FormatArtifactory {
			// Older JFrog versions send the configured secret itself in this header.
			want := sha256.Sum256(key)
			sum := sha256.Sum256([]byte(got))
			if got != "" {
				match |= subtle.ConstantTimeCompare(sum[:], want[:])
			}
		}
	}
	return match == 1
}

// signatureCandidates decodes a header value into raw signatures: optional
// "sha256=" / "sha1=" prefix, optional comma-separated list, hex digits.
func signatureCandidates(v string) [][]byte {
	if v == "" || len(v) > 1024 {
		return nil
	}
	var out [][]byte
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, '='); i >= 0 {
			part = part[i+1:]
		}
		if b, err := hex.DecodeString(part); err == nil && len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// SignBody produces the header value a sender would attach for the format; the
// console's "Send test" uses it so a test exercises the real verification.
func SignBody(format string, body, key []byte) (header, value string) {
	header = HMACFormats[format]
	switch format {
	case FormatGitLab:
		return header, string(key)
	case FormatNexus:
		m := hmac.New(sha1.New, key) //nolint:gosec
		m.Write(body)
		return header, hex.EncodeToString(m.Sum(nil))
	case FormatArtifactory:
		m := hmac.New(sha256.New, key)
		m.Write(body)
		return header, hex.EncodeToString(m.Sum(nil))
	}
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return header, "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// deliveryRef returns the provider's delivery identifier, if the request has one.
func deliveryRef(h http.Header) string {
	for _, name := range []string{"X-GitHub-Delivery", "X-Gitlab-Event-UUID", "X-Ant-Delivery-Id", "X-Nexus-Webhook-Delivery"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" && len(v) <= 200 && printable(v, 200) {
			return name + ":" + v
		}
	}
	return ""
}

// tokenDigest is the stored form of a URL token.
func tokenDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func tokenMatches(token string, stored []byte) bool {
	return subtle.ConstantTimeCompare(tokenDigest(token), stored) == 1
}
