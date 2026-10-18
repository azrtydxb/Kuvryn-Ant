package condarepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"io"
)

func uniqueJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		t, e := d.Token()
		if e != nil {
			return false
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return true
		}
		if delim == '{' {
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return false
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return false
				}
				seen[s] = true
				if !value(depth + 1) {
					return false
				}
			}
		} else if delim == '[' {
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
		} else {
			return false
		}
		_, e = d.Token()
		return e == nil
	}
	return value(0)
}
func info(data []byte) blob.Info {
	h := sha256.Sum256(data)
	return blob.Info{Size: int64(len(data)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}
