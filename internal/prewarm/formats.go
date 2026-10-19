package prewarm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/sumdb/dirhash"
)

// format describes how one feed format is warmed. Supported formats are those
// whose artifact URL follows from a name and a version (or, for PyPI and OCI,
// from the feed's own index); the request always goes through the feed route.
type format struct {
	packages, images, feed bool
	check                  func(Item) (Item, error)
	checkImage             func(Item) (Item, error)
	warm                   func(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error
}

var formats = map[string]*format{}

func init() {
	formats["npm"] = &format{packages: true, feed: true, check: checkNPM, warm: warmNPM}
	formats["cargo"] = &format{packages: true, feed: true, check: checkCargo, warm: warmCargo}
	formats["go"] = &format{packages: true, feed: true, check: checkGo, warm: warmGo}
	formats["pypi"] = &format{packages: true, feed: true, check: checkPyPI, warm: warmPyPI}
	formats["maven"] = &format{packages: true, feed: true, check: checkMaven, warm: warmMaven}
	formats["raw"] = &format{packages: true, check: checkRaw, warm: warmRaw}
	formats["oci"] = &format{images: true, checkImage: checkImage, warm: warmImage}
}

var (
	versionToken = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_~!-]{0,127}$`)
	npmName      = regexp.MustCompile(`^(@[A-Za-z0-9._~-]+/)?[A-Za-z0-9._~-]+$`)
	crateName    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	goModule     = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+$`)
	pyName       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	mavenPart    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

func noDotDot(s string) bool {
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}

func needVersion(it Item) error {
	if !versionToken.MatchString(it.Version) {
		return errors.New("a valid version is required")
	}
	return nil
}

// --- npm ---

func checkNPM(it Item) (Item, error) {
	if len(it.Name) > 214 || !npmName.MatchString(it.Name) || !noDotDot(it.Name) {
		return it, errors.New("not a valid npm package name")
	}
	return it, needVersion(it)
}

func warmNPM(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	base := it.Name[strings.LastIndex(it.Name, "/")+1:]
	return w.get(ctx, t, feedPath(t, "npm", escapePath(it.Name), "-", url.PathEscape(base+"-"+it.Version+".tgz")), "", parseIntegrity(it.Integrity), res)
}

// --- cargo ---

func checkCargo(it Item) (Item, error) {
	if !crateName.MatchString(it.Name) {
		return it, errors.New("not a valid crate name")
	}
	return it, needVersion(it)
}

func warmCargo(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	return w.get(ctx, t, feedPath(t, "cargo", "api/v1/crates", url.PathEscape(it.Name), url.PathEscape(it.Version), "download"), "", parseIntegrity(it.Integrity), res)
}

// --- go ---

func checkGo(it Item) (Item, error) {
	if len(it.Name) > 300 || !goModule.MatchString(it.Name) || !noDotDot(it.Name) {
		return it, errors.New("not a valid module path")
	}
	if !strings.HasPrefix(it.Version, "v") || !versionToken.MatchString(it.Version[1:]) {
		return it, errors.New("a version such as v1.2.3 is required")
	}
	return it, nil
}

// goEscape implements the module proxy path encoding: capitals become !lower.
func goEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func goHashOfFile(content []byte) string {
	sum := sha256.Sum256(content)
	line := hex.EncodeToString(sum[:]) + "  go.mod\n"
	outer := sha256.Sum256([]byte(line))
	return "h1:" + base64.StdEncoding.EncodeToString(outer[:])
}

func warmGo(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	prefix := escapePath(goEscape(it.Name), "@v")
	file := func(ext string) string { return feedPath(t, "go", prefix, url.PathEscape(goEscape(it.Version)+ext)) }
	if it.Integrity != "" {
		if err := w.get(ctx, t, file(".info"), "", expectation{}, res); err != nil {
			return err
		}
	}
	// go.mod: compared with the go.sum "/go.mod" line when the lockfile has one.
	mod, err := w.getBody(ctx, t, file(".mod"), "", expectation{}, res, 4<<20)
	if err != nil {
		return err
	}
	if it.Aux != "" && goHashOfFile(mod) != it.Aux {
		return itemErr(ErrIntegrity, "go.mod does not match go.sum")
	}
	if it.Integrity == "" {
		return nil // go.sum pins only go.mod: the module graph needs nothing else
	}
	want := it.Integrity
	return w.get(ctx, t, file(".zip"), "", expectation{check: func(path string) error {
		got, err := dirhash.HashZip(path, dirhash.Hash1)
		if err != nil || got != want {
			return itemErr(ErrIntegrity, "the module zip does not match go.sum")
		}
		return nil
	}}, res)
}

// --- pypi ---

func checkPyPI(it Item) (Item, error) {
	if len(it.Name) > 200 || !pyName.MatchString(it.Name) {
		return it, errors.New("not a valid project name")
	}
	it.Name = normalizePyName(it.Name)
	it.Version = strings.ToLower(it.Version)
	return it, needVersion(it)
}

const pypiJSON = "application/vnd.pypi.simple.v1+json"

type pypiFile struct {
	Filename string            `json:"filename"`
	URL      string            `json:"url"`
	Hashes   map[string]string `json:"hashes"`
	Yanked   any               `json:"yanked"`
}

// pypiVersionOf extracts the version of a wheel or sdist filename of project.
func pypiVersionOf(project, filename string) (version string, wheel bool, ok bool) {
	norm := func(s string) string { return normalizePyName(s) }
	switch {
	case strings.HasSuffix(filename, ".whl"):
		parts := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
		if len(parts) < 5 || norm(parts[0]) != project {
			return "", false, false
		}
		return strings.ToLower(parts[1]), true, true
	case strings.HasSuffix(filename, ".tar.gz"), strings.HasSuffix(filename, ".zip"):
		stem := strings.TrimSuffix(strings.TrimSuffix(filename, ".tar.gz"), ".zip")
		i := strings.LastIndex(stem, "-")
		if i <= 0 || norm(stem[:i]) != project {
			return "", false, false
		}
		return strings.ToLower(stem[i+1:]), false, true
	}
	return "", false, false
}

func warmPyPI(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	page, err := w.getBody(ctx, t, feedPath(t, "pypi", "simple", url.PathEscape(it.Name))+"/", pypiJSON, expectation{}, res, 16<<20)
	if err != nil {
		return err
	}
	var doc struct {
		Files []pypiFile `json:"files"`
	}
	if json.Unmarshal(page, &doc) != nil {
		return itemErr(ErrUnavail, "unreadable project page")
	}
	pinned := map[string]bool{}
	for _, h := range parseIntegrity(it.Integrity).sha256 {
		pinned[h] = true
	}
	var chosen []pypiFile
	for _, f := range doc.Files {
		v, wheel, ok := pypiVersionOf(it.Name, f.Filename)
		if !ok || v != it.Version {
			continue
		}
		switch {
		case len(pinned) > 0:
			if !pinned[strings.ToLower(f.Hashes["sha256"])] {
				continue
			}
		case len(t.Options.FileFilters) > 0:
			if !containsAny(f.Filename, t.Options.FileFilters) {
				continue
			}
		default:
			if wheel && !strings.HasSuffix(f.Filename, "-none-any.whl") {
				continue
			}
		}
		chosen = append(chosen, f)
	}
	if len(chosen) == 0 {
		return &ItemError{Code: ErrNoMatching, Detail: "the project page lists no matching file", Skip: true}
	}
	prefix := feedPath(t, "pypi")
	for _, f := range chosen {
		u, err := url.Parse(f.URL)
		if err != nil || !strings.HasPrefix(u.EscapedPath(), prefix) {
			return itemErr(ErrUnavail, "unexpected file location")
		}
		exp := expectation{}
		if h := strings.ToLower(f.Hashes["sha256"]); h != "" {
			exp.sha256 = []string{h}
		}
		if err = w.get(ctx, t, u.EscapedPath(), "", exp, res); err != nil {
			return err
		}
	}
	return nil
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// --- maven ---

func checkMaven(it Item) (Item, error) {
	parts := strings.Split(it.Name, ":")
	if len(parts) < 2 || len(parts) > 4 {
		return it, errors.New("expected group:artifact[:packaging[:classifier]]")
	}
	for i, p := range parts {
		ok := mavenPart.MatchString(p)
		if i == 0 {
			ok = ok && noDotDot(strings.ReplaceAll(p, ".", "/"))
		}
		if !ok || len(p) > 200 {
			return it, errors.New("invalid coordinate")
		}
	}
	if strings.HasSuffix(it.Version, "-SNAPSHOT") {
		return it, errors.New("snapshot versions cannot be warmed")
	}
	return it, needVersion(it)
}

func warmMaven(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	parts := strings.Split(it.Name, ":")
	group, artifactID, packaging, classifier := parts[0], parts[1], "jar", ""
	if len(parts) > 2 {
		packaging = parts[2]
	}
	if len(parts) > 3 {
		classifier = parts[3]
	}
	dir := escapePath(strings.ReplaceAll(group, ".", "/"), artifactID, it.Version)
	stem := artifactID + "-" + it.Version
	main := stem + "." + packaging
	if classifier != "" {
		main = stem + "-" + classifier + "." + packaging
	}
	files := []string{stem + ".pom"}
	if packaging != "pom" {
		files = append(files, main)
	}
	for i, name := range files {
		err := w.get(ctx, t, feedPath(t, "maven", dir, url.PathEscape(name)), "", expectation{}, res)
		// A pom-only or bom artifact has no main file; only a missing pom is fatal.
		var ie *ItemError
		if i > 0 && errors.As(err, &ie) && ie.Code == ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// --- raw ---

func checkRaw(it Item) (Item, error) {
	if !noDotDot(it.Name) || strings.HasPrefix(it.Name, "/") || len(it.Name) > 1000 {
		return it, errors.New("not a valid path")
	}
	it.Version = ""
	return it, nil
}

func warmRaw(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	return w.get(ctx, t, feedPath(t, "raw", escapePath(it.Name)), "", parseIntegrity(it.Integrity), res)
}

// --- oci ---

var (
	ociRepo   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	ociTag    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	ociDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ParseImageRef splits "repo[:tag]" or "repo@sha256:..." into an Item.
func ParseImageRef(s string) Item {
	if i := strings.Index(s, "@"); i > 0 {
		return Item{Name: s[:i], Version: s[i+1:]}
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") && i > 0 {
		return Item{Name: s[:i], Version: s[i+1:]}
	}
	return Item{Name: s, Version: "latest"}
}

func checkImage(it Item) (Item, error) {
	if len(it.Name) > 255 || !ociRepo.MatchString(it.Name) {
		return it, errors.New("not a valid repository name")
	}
	if it.Version == "" {
		it.Version = "latest"
	}
	if !ociTag.MatchString(it.Version) && !ociDigest.MatchString(it.Version) {
		return it, errors.New("not a valid tag or digest")
	}
	return it, nil
}

const ociManifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type ociManifest struct {
	Manifests []ociDescriptor `json:"manifests"`
	Config    *ociDescriptor  `json:"config"`
	Layers    []ociDescriptor `json:"layers"`
}

func warmImage(ctx context.Context, w *Warmer, t Target, it Item, res *Result) error {
	platforms := t.Options.Platforms
	if len(platforms) == 0 {
		platforms = []string{"linux/amd64"}
	}
	base := "/v2/" + url.PathEscape(t.Slug) + "/" + escapePath(it.Name)
	pulled := map[string]bool{}
	var manifest func(ref string, depth int) error
	manifest = func(ref string, depth int) error {
		if depth > 3 {
			return itemErr(ErrUnsupport, "manifest nesting is too deep")
		}
		exp := expectation{}
		if ociDigest.MatchString(ref) {
			exp.sha256 = []string{ref[len("sha256:"):]}
		}
		raw, err := w.getBody(ctx, t, base+"/manifests/"+url.PathEscape(ref), ociManifestAccept, exp, res, 4<<20)
		if err != nil {
			return err
		}
		var m ociManifest
		if json.Unmarshal(raw, &m) != nil {
			return itemErr(ErrUnavail, "unreadable manifest")
		}
		for _, d := range m.Manifests {
			if !ociDigest.MatchString(d.Digest) || pulled[d.Digest] || !platformWanted(d, platforms) {
				continue
			}
			pulled[d.Digest] = true
			if err = manifest(d.Digest, depth+1); err != nil {
				return err
			}
		}
		blobs := m.Layers
		if m.Config != nil {
			blobs = append([]ociDescriptor{*m.Config}, blobs...)
		}
		for _, d := range blobs {
			if !ociDigest.MatchString(d.Digest) || pulled[d.Digest] {
				continue
			}
			pulled[d.Digest] = true
			err = w.get(ctx, t, base+"/blobs/"+url.PathEscape(d.Digest), "", expectation{sha256: []string{d.Digest[len("sha256:"):]}}, res)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return manifest(it.Version, 0)
}

func platformWanted(d ociDescriptor, wanted []string) bool {
	if d.Platform == nil {
		return false
	}
	if d.Platform.OS == "unknown" || d.Platform.Architecture == "unknown" {
		return false // attestations and other non-image entries
	}
	for _, p := range wanted {
		if p == "all" {
			return true
		}
		want := strings.Split(p, "/")
		if want[0] == d.Platform.OS && want[1] == d.Platform.Architecture && (len(want) < 3 || want[2] == d.Platform.Variant) {
			return true
		}
	}
	return false
}

// SupportedFormats lists the prewarmable formats in a stable order.
func SupportedFormats() []string {
	out := make([]string, 0, len(formats))
	for name := range formats {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
