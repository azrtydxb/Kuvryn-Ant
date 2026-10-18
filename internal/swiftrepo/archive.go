package swiftrepo

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxArchive    = 128 << 20
	maxUnpacked   = 1 << 30
	maxEntries    = 20000
	maxManifest   = 1 << 20
	maxMetadata   = 256 << 10
	maxSignature  = 64 << 10
	maxVariants   = 10
	maxRepoURLs   = 20
	maxListed     = 4096
	releaseAsset  = "release"
	archiveAsset  = "archive"
	defaultManif  = "Package.swift"
	zipMediaType  = "application/zip"
	jsonMediaType = "application/json"
)

var errInvalid = errors.New("invalid Swift package")

var (
	scopePattern    = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9]|-[a-zA-Z0-9]){0,38}$`)
	namePattern     = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9]|[-_][a-zA-Z0-9]){0,99}$`)
	versionPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	variantPattern  = regexp.MustCompile(`^Package@swift-([0-9]+(?:\.[0-9]+){0,2})\.swift$`)
	toolsPattern    = regexp.MustCompile(`^//\s*swift-tools-version:\s*([0-9]+(?:\.[0-9]+){0,2})`)
	swiftVerPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,2}$`)
)

func validScope(s string) bool   { return scopePattern.MatchString(s) }
func validName(s string) bool    { return namePattern.MatchString(s) }
func validVersion(v string) bool { return len(v) <= 128 && versionPattern.MatchString(v) }

// semverLess orders versions by SemVer precedence (build metadata ignored).
func semverLess(a, b string) bool {
	am, bm := versionPattern.FindStringSubmatch(a), versionPattern.FindStringSubmatch(b)
	if am == nil || bm == nil {
		return a < b
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(am[i])
		y, _ := strconv.Atoi(bm[i])
		if x != y {
			return x < y
		}
	}
	pa, pb := am[4], bm[4]
	if pa == pb {
		return false
	}
	if pa == "" {
		return false
	}
	if pb == "" {
		return true
	}
	xs, ys := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		xi, xe := strconv.Atoi(xs[i])
		yi, ye := strconv.Atoi(ys[i])
		switch {
		case xe == nil && ye == nil:
			if xi != yi {
				return xi < yi
			}
		case xe == nil:
			return true
		case ye == nil:
			return false
		case xs[i] != ys[i]:
			return xs[i] < ys[i]
		}
	}
	return len(xs) < len(ys)
}

func sortVersions(v []string) { sort.Slice(v, func(i, j int) bool { return semverLess(v[i], v[j]) }) }

type manifestInfo struct {
	File         string `json:"file"`
	ToolsVersion string `json:"toolsVersion"`
}

type inspected struct {
	manifests map[string][]byte // file name -> content
	infos     []manifestInfo
}

func safeName(n string) bool {
	if n == "" || len(n) > 1024 || strings.ContainsAny(n, "\\\x00") || strings.HasPrefix(n, "/") || !utf8.ValidString(n) {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(n, "/"), "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return true
}

// inspect validates a source archive without extracting it to disk and returns
// the manifests it contains. Uploaded content is never executed.
func inspect(ctx context.Context, ra io.ReaderAt, size int64) (*inspected, error) {
	zr, e := zip.NewReader(ra, size)
	if e != nil || len(zr.File) == 0 || len(zr.File) > maxEntries {
		return nil, errInvalid
	}
	var total uint64
	top := map[string]bool{}
	for _, f := range zr.File {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !safeName(f.Name) || f.Mode()&0o170000 == 0o120000 {
			return nil, errInvalid
		}
		total += f.UncompressedSize64
		if total > maxUnpacked {
			return nil, errInvalid
		}
		top[strings.SplitN(f.Name, "/", 2)[0]] = true
	}
	prefix := ""
	has := func(n string) bool {
		for _, f := range zr.File {
			if f.Name == n {
				return true
			}
		}
		return false
	}
	if !has(defaultManif) {
		if len(top) != 1 {
			return nil, errInvalid
		}
		for k := range top {
			prefix = k + "/"
		}
		if !has(prefix + defaultManif) {
			return nil, errInvalid
		}
	}
	out := &inspected{manifests: map[string][]byte{}}
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || strings.Contains(rel, "/") || (rel != defaultManif && !variantPattern.MatchString(rel)) || f.FileInfo().IsDir() {
			continue
		}
		if len(out.manifests) > maxVariants || f.UncompressedSize64 > maxManifest {
			return nil, errInvalid
		}
		rc, e := f.Open()
		if e != nil {
			return nil, errInvalid
		}
		b, e := io.ReadAll(io.LimitReader(rc, maxManifest+1))
		_ = rc.Close()
		if e != nil || len(b) > maxManifest || !utf8.Valid(b) {
			return nil, errInvalid
		}
		out.manifests[rel] = b
	}
	if _, ok := out.manifests[defaultManif]; !ok {
		return nil, errInvalid
	}
	names := make([]string, 0, len(out.manifests))
	for n := range out.manifests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tv := ""
		if m := variantPattern.FindStringSubmatch(n); m != nil {
			tv = m[1]
		}
		first, _, _ := strings.Cut(string(out.manifests[n]), "\n")
		if m := toolsPattern.FindStringSubmatch(first); m != nil {
			tv = m[1]
		}
		out.infos = append(out.infos, manifestInfo{File: n, ToolsVersion: tv})
	}
	return out, nil
}

// validMetadata checks the optional publish metadata document.
func validMetadata(b []byte) (map[string]any, error) {
	if len(b) > maxMetadata || !utf8.Valid(b) {
		return nil, errInvalid
	}
	var m map[string]any
	if e := json.Unmarshal(b, &m); e != nil || m == nil {
		return nil, errInvalid
	}
	if v, ok := m["repositoryURLs"]; ok {
		list, ok := v.([]any)
		if !ok || len(list) > maxRepoURLs {
			return nil, errInvalid
		}
		for _, x := range list {
			s, ok := x.(string)
			if !ok || normalizeRepoURL(s) == "" {
				return nil, errInvalid
			}
		}
	}
	for _, k := range []string{"licenseURL", "readmeURL"} {
		if v, ok := m[k]; ok {
			s, ok := v.(string)
			u, e := url.Parse(s)
			if !ok || e != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return nil, errInvalid
			}
		}
	}
	if v, ok := m["description"]; ok {
		if _, ok := v.(string); !ok {
			return nil, errInvalid
		}
	}
	return m, nil
}

// normalizeRepoURL canonicalizes SCP/ssh/http(s)/git forms so equivalent
// clone URLs match. It returns "" for unusable input.
func normalizeRepoURL(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || len(s) > 2048 || strings.ContainsAny(s, " \x00\r\n\\") {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		switch s[:i] {
		case "http", "https", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return ""
		}
		s = s[i+3:]
	} else if at := strings.Index(s, "@"); at >= 0 {
		s = strings.Replace(s[at+1:], ":", "/", 1)
	} else {
		return ""
	}
	if at := strings.LastIndex(strings.SplitN(s, "/", 2)[0], "@"); at >= 0 {
		s = s[at+1:]
	}
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	if !strings.Contains(s, "/") {
		return ""
	}
	return s
}
