package peer

import (
	"encoding/base64"
	"errors"
)

// errEncoding reports a value that is not canonical unpadded base64url.
var errEncoding = errors.New("federation: value is not canonical base64url")

var strictB64 = base64.RawURLEncoding.Strict()

// DecodeCanonical decodes unpadded base64url and accepts only the canonical
// encoding of the result: non-zero trailing bits (rejected by Strict) and the
// CR/LF characters the standard decoder silently skips would otherwise give
// one signature or key several accepted spellings. Signed documents hash the
// spelling, so every value must have exactly one.
func DecodeCanonical(s string) ([]byte, error) {
	b, err := strictB64.DecodeString(s)
	if err != nil || strictB64.EncodeToString(b) != s {
		return nil, errEncoding
	}
	return b, nil
}
