package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"go.yaml.in/yaml/v3"
)

const maxPackage = 256 << 20
const maxMetadata = 1 << 20

var errInvalid = errors.New("invalid gem")
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z]+)*$`)
var platformPattern = regexp.MustCompile(`^[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*$`)

type dependency struct {
	Name         string
	Requirements []string
	Type         string
}
type metadata struct {
	Name, Version, Platform, Summary, SHA256     string
	RubygemsVersion, Date, Description, Homepage string
	Authors, Licenses                            []string
	Ruby, RubyGems                               []string
	Dependencies                                 []dependency
}

func (m metadata) key() string {
	if m.Platform == "ruby" {
		return m.Version
	}
	return m.Version + "-" + m.Platform
}
func (m metadata) filename() string { return m.Name + "-" + m.key() }

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

// YAML is parsed into inert nodes, never Ruby objects. Reject aliases, duplicate
// keys, merge keys, arbitrary tags and deep graphs before extracting fields.
func parseMetadata(b []byte) (metadata, error) {
	var m metadata
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if dec.Decode(&doc) != nil {
		return m, errInvalid
	}
	var extra yaml.Node
	if dec.Decode(&extra) != io.EOF {
		return m, errInvalid
	}
	nodes := 0
	var check func(*yaml.Node, int) bool
	check = func(n *yaml.Node, d int) bool {
		nodes++
		if d > 24 || nodes > 20000 || n.Kind == yaml.AliasNode || n.Anchor != "" {
			return false
		}
		switch n.Tag {
		case "", "!!str", "!!int", "!!bool", "!!null", "!!float", "!!timestamp", "!!seq", "!!map", "!ruby/object:Gem::Specification", "!ruby/object:Gem::Version", "!ruby/object:Gem::Requirement", "!ruby/object:Gem::Dependency":
		default:
			return false
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Value == "<<" || seen[k.Value] {
					return false
				}
				seen[k.Value] = true
			}
		}
		for _, c := range n.Content {
			if !check(c, d+1) {
				return false
			}
		}
		return true
	}
	if !check(&doc, 0) || len(doc.Content) != 1 {
		return m, errInvalid
	}
	root := doc.Content[0]
	if root.Tag != "!ruby/object:Gem::Specification" || root.Kind != yaml.MappingNode {
		return m, errInvalid
	}
	if v := field(root, "version"); v == nil || v.Tag != "!ruby/object:Gem::Version" {
		return m, errInvalid
	}
	m.Name = scalar(field(root, "name"))
	m.Version = scalar(field(field(root, "version"), "version"))
	m.Platform = scalar(field(root, "platform"))
	m.Summary = scalar(field(root, "summary"))
	m.RubygemsVersion = scalar(field(root, "rubygems_version"))
	m.Date = "1970-01-01"
	if d := scalar(field(root, "date")); d != "" {
		if len(d) < 10 {
			return m, errInvalid
		}
		m.Date = d[:10]
		if _, e := time.Parse("2006-01-02", m.Date); e != nil {
			return m, errInvalid
		}
	}
	m.Description = scalar(field(root, "description"))
	m.Homepage = scalar(field(root, "homepage"))
	var err error
	m.Authors, err = stringList(field(root, "authors"))
	if err != nil {
		return m, err
	}
	m.Licenses, err = stringList(field(root, "licenses"))
	if err != nil {
		return m, err
	}
	for _, key := range []string{"files", "require_paths", "executables", "extensions", "extra_rdoc_files", "test_files"} {
		paths, e := stringList(field(root, key))
		if e != nil {
			return m, e
		}
		for _, p := range paths {
			if !safePath(p) {
				return m, errInvalid
			}
		}
	}

	if !namePattern.MatchString(m.Name) || len(m.Version) > 128 || !versionPattern.MatchString(m.Version) || len(m.Platform) > 100 || !platformPattern.MatchString(m.Platform) {
		return m, errInvalid
	}
	var e error
	m.Ruby, e = requirements(field(root, "required_ruby_version"))
	if e != nil {
		return m, e
	}
	m.RubyGems, e = requirements(field(root, "required_rubygems_version"))
	if e != nil {
		return m, e
	}
	deps := field(root, "dependencies")
	if deps != nil {
		if deps.Kind != yaml.SequenceNode || len(deps.Content) > 256 {
			return m, errInvalid
		}
		seen := map[string]bool{}
		for _, d := range deps.Content {
			dep := dependency{Name: scalar(field(d, "name")), Type: scalar(field(d, "type"))}
			if d.Tag != "!ruby/object:Gem::Dependency" || !namePattern.MatchString(dep.Name) || (dep.Type != ":runtime" && dep.Type != ":development") || seen[dep.Name] {
				return m, errInvalid
			}
			seen[dep.Name] = true
			dep.Requirements, e = requirements(field(d, "requirement"))
			if e != nil {
				return m, e
			}
			m.Dependencies = append(m.Dependencies, dep)
		}
	}
	return m, nil
}
func field(n *yaml.Node, k string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == k {
			return n.Content[i+1]
		}
	}
	return nil
}
func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}
func requirements(n *yaml.Node) ([]string, error) {
	if n == nil {
		return []string{">= 0"}, nil
	}
	if n.Tag != "!ruby/object:Gem::Requirement" {
		return nil, errInvalid
	}
	a := field(n, "requirements")
	if a == nil || a.Kind != yaml.SequenceNode || len(a.Content) == 0 || len(a.Content) > 16 {
		return nil, errInvalid
	}
	out := []string{}
	for _, p := range a.Content {
		if p.Kind != yaml.SequenceNode || len(p.Content) != 2 {
			return nil, errInvalid
		}
		op := scalar(p.Content[0])
		switch op {
		case "=", "!=", ">", ">=", "<", "<=", "~>":
		default:
			return nil, errInvalid
		}
		if p.Content[1].Tag != "!ruby/object:Gem::Version" {
			return nil, errInvalid
		}
		v := scalar(field(p.Content[1], "version"))
		if len(v) > 128 || !versionPattern.MatchString(v) {
			return nil, errInvalid
		}
		out = append(out, op+" "+v)
	}
	return out, nil
}
func safePath(s string) bool {
	return s != "" && len(s) < 2048 && !strings.ContainsAny(s, "\\\x00") && !strings.HasPrefix(s, "/") && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../")
}
func inflate(ctx context.Context, b []byte, limit int64) ([]byte, error) {
	g, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return nil, errInvalid
	}
	defer g.Close()
	out, e := io.ReadAll(io.LimitReader(contextReader{ctx, g}, limit+1))
	if e != nil || int64(len(out)) > limit {
		return nil, errInvalid
	}
	return out, nil
}
func inspectPackage(ctx context.Context, r io.Reader) (metadata, error) {
	var m metadata
	tr := tar.NewReader(contextReader{ctx, r})
	seen := map[string]bool{}
	var raw, checks []byte
	hashes := map[string]map[string]string{"SHA256": {}, "SHA512": {}}
	buffer := make([]byte, 64<<10)
	dataFound := false
	for count := 0; ; count++ {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil || count > 8 || h.Typeflag != tar.TypeReg || seen[h.Name] || h.Size < 0 || h.Size > maxPackage {
			return m, errInvalid
		}
		seen[h.Name] = true
		switch h.Name {
		case "metadata.gz":
			if h.Size > maxMetadata {
				return m, errInvalid
			}
			b, e := io.ReadAll(tr)
			if e != nil {
				return m, e
			}
			h256 := sha256.Sum256(b)
			h512 := sha512.Sum512(b)
			hashes["SHA256"][h.Name] = hex.EncodeToString(h256[:])
			hashes["SHA512"][h.Name] = hex.EncodeToString(h512[:])
			raw, e = inflate(ctx, b, maxMetadata)
			if e != nil {
				return m, e
			}
		case "data.tar.gz":
			h256, h512 := sha256.New(), sha512.New()
			compressed := io.TeeReader(tr, io.MultiWriter(h256, h512))
			g, e := gzip.NewReader(compressed)
			if e != nil {
				return m, errInvalid
			}
			lr := &io.LimitedReader{R: contextReader{ctx, g}, N: 512<<20 + 1}
			dt := tar.NewReader(lr)
			files := map[string]bool{}
			for i := 0; ; i++ {
				d, e := dt.Next()
				if e == io.EOF {
					break
				}
				if e != nil || i >= 20000 || !safePath(d.Name) || files[d.Name] || (d.Typeflag != tar.TypeReg && d.Typeflag != tar.TypeDir) {
					g.Close()
					return m, errInvalid
				}
				files[d.Name] = true
				if _, e = io.CopyBuffer(io.Discard, contextReader{ctx, dt}, buffer); e != nil {
					g.Close()
					return m, e
				}
			}
			_, e = io.CopyBuffer(zeroWriter{}, lr, buffer)
			g.Close()
			if e != nil || lr.N <= 0 {
				return m, errInvalid
			}
			hashes["SHA256"][h.Name] = hex.EncodeToString(h256.Sum(nil))
			hashes["SHA512"][h.Name] = hex.EncodeToString(h512.Sum(nil))
			dataFound = true
		case "checksums.yaml.gz":
			if h.Size > maxMetadata {
				return m, errInvalid
			}
			b, e := io.ReadAll(tr)
			if e != nil {
				return m, e
			}
			if checks, e = inflate(ctx, b, maxMetadata); e != nil {
				return m, e
			}
		default:
			return m, errInvalid
		}
	}
	if _, e := io.CopyBuffer(zeroWriter{}, contextReader{ctx, r}, buffer); e != nil {
		return m, errInvalid
	}
	if len(raw) == 0 || !dataFound {
		return m, errInvalid
	}
	if seen["checksums.yaml.gz"] && !validChecksums(checks, hashes) {
		return m, errInvalid
	}
	return parseMetadata(raw)
}

// Checksum files are optional in older gem archives; when present, require a
// complete SHA256 table and verify every advertised SHA256/SHA512 digest.
func validChecksums(b []byte, hashes map[string]map[string]string) bool {
	var n yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(b))
	if d.Decode(&n) != nil || len(n.Content) != 1 {
		return false
	}
	var extra yaml.Node
	if d.Decode(&extra) != io.EOF {
		return false
	}
	root := n.Content[0]
	if root.Kind != yaml.MappingNode || root.Tag != "!!map" || len(root.Content) > 4 {
		return false
	}
	seen := map[string]bool{}
	for i := 0; i < len(root.Content); i += 2 {
		key, v := root.Content[i], root.Content[i+1]
		algo := key.Value
		if key.Kind != yaml.ScalarNode || seen[algo] || hashes[algo] == nil || v.Kind != yaml.MappingNode || v.Tag != "!!map" || len(v.Content) != 4 {
			return false
		}
		seen[algo] = true
		members := map[string]bool{}
		for j := 0; j < len(v.Content); j += 2 {
			k, digest := v.Content[j], v.Content[j+1]
			if k.Kind != yaml.ScalarNode || digest.Kind != yaml.ScalarNode || members[k.Value] || hashes[algo][k.Value] == "" || hashes[algo][k.Value] != digest.Value {
				return false
			}
			members[k.Value] = true
		}
	}
	return seen["SHA256"]
}

// TAR permits zero padding after its end marker, never hidden nonzero payloads.
type zeroWriter struct{}

func (zeroWriter) Write(b []byte) (int, error) {
	for _, c := range b {
		if c != 0 {
			return 0, errInvalid
		}
	}
	return len(b), nil
}

func stringList(n *yaml.Node) ([]string, error) {
	if n == nil || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode || len(n.Content) > 20000 {
		return nil, errInvalid
	}
	out := []string{}
	for _, c := range n.Content {
		if c.Kind != yaml.ScalarNode || c.Tag != "!!str" {
			return nil, errInvalid
		}
		out = append(out, c.Value)
	}
	return out, nil
}
