package management

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

type PageQuery struct {
	Limit          int
	Cursor, Prefix string
}

func ParsePage(q url.Values) (PageQuery, error) {
	p := PageQuery{Limit: 50}
	for k, v := range q {
		if (k != "limit" && k != "cursor" && k != "prefix") || len(v) != 1 {
			return p, errors.New("unknown or repeated page parameter")
		}
	}
	if v, ok := q["limit"]; ok {
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 1 || n > 200 {
			return p, errors.New("limit must be between 1 and 200")
		}
		p.Limit = n
	}
	p.Cursor = q.Get("cursor")
	p.Prefix = q.Get("prefix")
	if len(p.Cursor) > 2048 || utf8.RuneCountInString(p.Prefix) > 200 {
		return p, errors.New("page parameter too long")
	}
	return p, nil
}

type CursorCodec struct{ key []byte }
type cursor struct {
	Scope string `json:"scope"`
	After string `json:"after"`
}

// NewCursorCodec derives a purpose-specific authentication key from the root key.
func NewCursorCodec(root []byte) CursorCodec {
	if len(root) != 32 {
		return CursorCodec{}
	}
	m := hmac.New(sha256.New, root)
	m.Write([]byte("kuvryn-ant/api/v1/cursor"))
	return CursorCodec{m.Sum(nil)}
}

// Scope must bind the authorized principal, resource and normalized filters.
// A valid cursor does not replace authorization of the request or result rows.
func (c CursorCodec) Encode(scope, after string) (string, error) {
	if len(c.key) != 32 {
		return "", errors.New("invalid cursor key configuration")
	}
	body, _ := json.Marshal(cursor{scope, after})
	m := hmac.New(sha256.New, c.key)
	m.Write(body)
	result := base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if len(result) > 2048 {
		return "", errors.New("cursor is too large")
	}
	return result, nil
}
func (c CursorCodec) Decode(scope, token string) (string, error) {
	invalid := errors.New("invalid page cursor")
	if len(c.key) != 32 || len(token) > 2048 {
		return "", invalid
	}
	bodyPart, signaturePart, ok := strings.Cut(token, ".")
	if !ok {
		return "", invalid
	}
	body, err := base64.RawURLEncoding.DecodeString(bodyPart)
	if err != nil {
		return "", invalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return "", invalid
	}
	m := hmac.New(sha256.New, c.key)
	m.Write(body)
	if !hmac.Equal(m.Sum(nil), signature) {
		return "", invalid
	}
	var value cursor
	if err = json.Unmarshal(body, &value); err != nil || value.Scope != scope || value.After == "" {
		return "", invalid
	}
	return value.After, nil
}
