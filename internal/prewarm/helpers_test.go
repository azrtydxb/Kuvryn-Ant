package prewarm

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
)

func b64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func sha512Sum(b []byte) []byte { s := sha512.Sum512(b); return s[:] }

func b64FromHex(h string) string {
	raw, _ := hex.DecodeString(h)
	return base64.StdEncoding.EncodeToString(raw)
}
