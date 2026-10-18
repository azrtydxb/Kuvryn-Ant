package pubrepo

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/semver"
)

const maxArchive = 32 << 20
const maxMetadata = 256 << 10

var errInvalid = errors.New("invalid Pub archive")
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validVersion(v string) bool {
	return len(v) <= 128 && semver.IsValid("v"+v) && strings.Count(strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0], ".") == 2
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}
func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}
func inspect(ctx context.Context, r io.Reader) (map[string]any, error) {
	g, e := gzip.NewReader(contextReader{ctx, r})
	if e != nil {
		return nil, errInvalid
	}
	defer g.Close()
	limited := &io.LimitedReader{R: g, N: 128<<20 + 1}
	tr := tar.NewReader(limited)
	seen := map[string]bool{}
	var spec map[string]any
	for count := 0; ; count++ {
		if count > 10000 {
			return nil, errInvalid
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errInvalid
		}
		n := strings.TrimSuffix(h.Name, "/")
		if n == "" || path.Clean(n) != n || strings.HasPrefix(n, "/") || n == ".." || strings.HasPrefix(n, "../") || strings.ContainsAny(n, "\\:\x00") || seen[n] {
			return nil, errInvalid
		}
		seen[n] = true
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return nil, errInvalid
		}
		if h.Name == "pubspec.yaml" {
			if h.Size > maxMetadata || h.Typeflag != tar.TypeReg {
				return nil, errInvalid
			}
			b, e := io.ReadAll(tr)
			if e != nil {
				return nil, errInvalid
			}
			var node yaml.Node
			if yaml.Unmarshal(b, &node) != nil {
				return nil, errInvalid
			}
			nodes := 0
			var check func(*yaml.Node, int) bool
			check = func(n *yaml.Node, d int) bool {
				nodes++
				if d > 32 || nodes > 10000 || n.Kind == yaml.AliasNode {
					return false
				}
				for _, c := range n.Content {
					if !check(c, d+1) {
						return false
					}
				}
				return true
			}
			if !check(&node, 0) || node.Decode(&spec) != nil {
				return nil, errInvalid
			}
		}
	}
	// Consume gzip trailer and reject oversized expansion, including trailing data.
	if _, e = io.CopyBuffer(io.Discard, contextReader{ctx, limited}, make([]byte, 64<<10)); e != nil || limited.N <= 0 {
		return nil, errInvalid
	}
	name, _ := spec["name"].(string)
	v, _ := spec["version"].(string)
	if !namePattern.MatchString(name) || !validVersion(v) {
		return nil, errInvalid
	}
	if _, e = json.Marshal(spec); e != nil {
		return nil, errInvalid
	}
	return spec, nil
}
