package federation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrCanonical reports input that cannot be canonicalized (invalid JSON,
// duplicate object keys, invalid UTF-8 or non-finite numbers).
var ErrCanonical = errors.New("federation: JSON cannot be canonicalized")

// Canonicalize returns the RFC 8785 (JCS) form of a JSON text: object members
// sorted by their UTF-16 code units, no insignificant whitespace, ECMAScript
// number serialization and minimal string escaping. Duplicate keys are
// rejected because their meaning differs between parsers.
func Canonicalize(input []byte) ([]byte, error) {
	if !utf8.Valid(input) {
		return nil, ErrCanonical
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var out bytes.Buffer
	if err := canonicalValue(dec, &out); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, ErrCanonical // trailing data
	}
	return out.Bytes(), nil
}

// CanonicalJSON marshals v and canonicalizes the result.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, ErrCanonical
	}
	return Canonicalize(raw)
}

func canonicalValue(dec *json.Decoder, out *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return ErrCanonical
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			type member struct {
				key   string
				value []byte
			}
			var members []member
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return ErrCanonical
				}
				key, ok := kt.(string)
				if !ok || seen[key] {
					return ErrCanonical
				}
				seen[key] = true
				var value bytes.Buffer
				if err := canonicalValue(dec, &value); err != nil {
					return err
				}
				members = append(members, member{key, value.Bytes()})
			}
			if _, err := dec.Token(); err != nil {
				return ErrCanonical
			}
			sort.Slice(members, func(i, j int) bool { return lessUTF16(members[i].key, members[j].key) })
			out.WriteByte('{')
			for i, m := range members {
				if i > 0 {
					out.WriteByte(',')
				}
				writeString(out, m.key)
				out.WriteByte(':')
				out.Write(m.value)
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if err := canonicalValue(dec, out); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return ErrCanonical
			}
			out.WriteByte(']')
		default:
			return ErrCanonical
		}
	case string:
		writeString(out, t)
	case json.Number:
		s, err := formatNumber(t)
		if err != nil {
			return err
		}
		out.WriteString(s)
	case bool:
		if t {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case nil:
		out.WriteString("null")
	default:
		return ErrCanonical
	}
	return nil
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeString follows ECMAScript JSON.stringify: only ", \, and control
// characters are escaped; \b \t \n \f \r use their short forms.
func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte("0123456789abcdef"[r>>4])
				out.WriteByte("0123456789abcdef"[r&0xf])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

// formatNumber implements the ECMAScript Number::toString algorithm for
// IEEE 754 doubles, as RFC 8785 §3.2.2.3 requires.
func formatNumber(n json.Number) (string, error) {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", ErrCanonical
	}
	if f == 0 {
		return "0", nil
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// Shortest round-trip digits and exponent: f = 0.d1d2...dk × 10^n.
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddddde±xx
	mantissa, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	k := len(digits)
	nExp := exp + 1
	var s string
	switch {
	case k <= nExp && nExp <= 21:
		s = digits + strings.Repeat("0", nExp-k)
	case 0 < nExp && nExp <= 21:
		s = digits[:nExp] + "." + digits[nExp:]
	case -6 < nExp && nExp <= 0:
		s = "0." + strings.Repeat("0", -nExp) + digits
	default:
		e := nExp - 1
		es := strconv.Itoa(e)
		if e >= 0 {
			es = "+" + es
		}
		if k == 1 {
			s = digits + "e" + es
		} else {
			s = digits[:1] + "." + digits[1:] + "e" + es
		}
	}
	return sign + s, nil
}

// maxSafeInteger is the largest integer every JSON number parser represents
// exactly (2^53 - 1, ECMAScript Number.MAX_SAFE_INTEGER).
const maxSafeInteger = 1<<53 - 1
