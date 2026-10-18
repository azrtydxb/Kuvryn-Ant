package helmrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/semver"
)

var errChart = errors.New("invalid Helm chart or provenance")
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func validCoordinate(name, version string) bool {
	return namePattern.MatchString(name) && len(version) <= 128 && !strings.HasPrefix(version, "v") && strings.Count(strings.SplitN(strings.SplitN(version, "+", 2)[0], "-", 2)[0], ".") == 2 && semver.IsValid("v"+version)
}

// Limits bound compressed input, expanded tar bytes, metadata and member count.
// They may be raised by operators before serving requests.
type Limits struct {
	ChartBytes, ExpandedBytes, MetadataBytes, ProvenanceBytes int64
	IndexBytes                                                int64
	Members, IndexEntries                                     int
}

func DefaultLimits() Limits {
	return Limits{math.MaxInt64 - (4 << 20), math.MaxInt64 - 1, 1 << 20, 2 << 20, 16 << 20, 10000, 10000}
}

func decodeYAML(data []byte, target any) error {
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&node); err != nil {
		return errChart
	}
	var extra yaml.Node
	if dec.Decode(&extra) != io.EOF {
		return errChart
	}
	var check func(*yaml.Node, int) bool
	check = func(n *yaml.Node, depth int) bool {
		if depth > 32 || n.Kind == yaml.AliasNode {
			return false
		}
		for _, c := range n.Content {
			if !check(c, depth+1) {
				return false
			}
		}
		return true
	}
	if !check(&node, 0) || node.Decode(target) != nil {
		return errChart
	}
	return nil
}

// inspectChart never extracts members, follows links, or executes templates.
func inspectChart(reader io.Reader, name, version string, limits Limits) ([]byte, error) {
	return inspectChartContext(context.Background(), reader, name, version, limits)
}
func inspectChartContext(ctx context.Context, reader io.Reader, name, version string, limits Limits) ([]byte, error) {
	gz, err := gzip.NewReader(&cancelReader{ctx: ctx, reader: reader})
	if err != nil {
		return nil, errChart
	}
	defer gz.Close()
	expanded := &io.LimitedReader{R: &cancelReader{ctx: ctx, reader: gz}, N: limits.ExpandedBytes + 1}
	tr := tar.NewReader(expanded)
	seen := map[string]bool{}
	var metadata []byte
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil || count >= limits.Members {
			return nil, errChart
		}
		cleaned := strings.TrimSuffix(h.Name, "/")
		if len(cleaned) > 4096 || cleaned == "" || strings.ContainsAny(cleaned, "\\\x00\r\n") || path.IsAbs(cleaned) || path.Clean(cleaned) != cleaned || (cleaned != name && !strings.HasPrefix(cleaned, name+"/")) || seen[cleaned] {
			return nil, errChart
		}
		seen[cleaned] = true
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse") || strings.HasPrefix(key, "SCHILY.") {
				return nil, errChart
			}
		}

		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
			return nil, errChart
		}
		if h.Size < 0 || h.Size > limits.ExpandedBytes || (h.Typeflag == tar.TypeDir && h.Size != 0) {
			return nil, errChart
		}
		if cleaned == name+"/Chart.yaml" {
			if h.Typeflag == tar.TypeDir || h.Size > limits.MetadataBytes {
				return nil, errChart
			}
			metadata, err = io.ReadAll(tr)
			if err != nil {
				return nil, errChart
			}
		}
	}
	// Consume the gzip trailer as well: tar EOF alone does not check its CRC.
	if _, err = io.Copy(zeroWriter{}, expanded); err != nil || expanded.N <= 0 {
		return nil, errChart
	}
	var m map[string]any
	if len(metadata) == 0 || decodeYAML(metadata, &m) != nil || m["name"] != name || m["version"] != version || (m["apiVersion"] != "v1" && m["apiVersion"] != "v2") {
		return nil, errChart
	}
	return metadata, nil
}

// validateProvenance checks the declared package identity and checksum only.
// The original bytes are stored unchanged. No signature trust is asserted.
func validateProvenance(data []byte, name, version, digest string) error {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "-----BEGIN PGP SIGNED MESSAGE-----\n") {
		return errChart
	}
	_, body, ok := strings.Cut(text, "\n\n")
	if !ok {
		return errChart
	}
	body, signature, ok := strings.Cut(body, "\n-----BEGIN PGP SIGNATURE-----\n")
	if !ok || !strings.HasSuffix(strings.TrimSpace(signature), "-----END PGP SIGNATURE-----") {
		return errChart
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "- ")
	}
	body = strings.Join(lines, "\n")
	// Helm uses an explicit YAML document end before its checksum document.
	first, second, ok := strings.Cut(body, "\n...\n")
	if !ok {
		return errChart
	}
	var meta map[string]any
	var hashes struct {
		Files map[string]string `yaml:"files"`
	}
	if decodeYAML([]byte(first), &meta) != nil || decodeYAML([]byte(second), &hashes) != nil || meta["name"] != name || meta["version"] != version || len(hashes.Files) != 1 || hashes.Files[name+"-"+version+".tgz"] != "sha256:"+digest {
		return errChart
	}
	return nil
}

// Tar permits trailing zero padding, never a second hidden archive.
type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errChart
		}
	}
	return len(p), nil
}

type cancelReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return r.reader.Read(p)
}
