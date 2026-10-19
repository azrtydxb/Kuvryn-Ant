package prewarm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ParseLockfile turns the content of a supported lockfile into items. Entries
// that cannot be fetched from a registry (git, file and path dependencies,
// unpinned requirements) are returned as ignored instead of failing the file.
func ParseLockfile(kind string, content []byte) ([]Item, []Ignored, error) {
	if len(content) == 0 || len(content) > MaxLockfileSize {
		return nil, nil, fmt.Errorf("the lockfile must be between 1 byte and %d MiB", MaxLockfileSize>>20)
	}
	var items []Item
	var ignored []Ignored
	var err error
	switch kind {
	case "package-lock.json":
		items, ignored, err = parsePackageLock(content)
	case "pnpm-lock.yaml":
		items, ignored, err = parsePnpmLock(content)
	case "go.sum":
		items, ignored, err = parseGoSum(content)
	case "Cargo.lock":
		items, ignored, err = parseCargoLock(content)
	case "requirements.txt":
		items, ignored, err = parseRequirements(content)
	case "poetry.lock":
		items, ignored, err = parsePoetryLock(content)
	default:
		return nil, nil, fmt.Errorf("unsupported lockfile type %q", kind)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(items) > MaxItems {
		return nil, nil, fmt.Errorf("the lockfile lists %d packages; at most %d are allowed", len(items), MaxItems)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Version < items[j].Version
	})
	if len(ignored) > 1000 {
		ignored = ignored[:1000]
	}
	return items, ignored, nil
}

type collector struct {
	items   []Item
	ignored []Ignored
	seen    map[[2]string]int
}

func newCollector() *collector { return &collector{seen: map[[2]string]int{}} }

func (c *collector) add(it Item) {
	k := [2]string{it.Name, it.Version}
	if i, ok := c.seen[k]; ok {
		if c.items[i].Integrity == "" {
			c.items[i].Integrity = it.Integrity
		}
		if c.items[i].Aux == "" {
			c.items[i].Aux = it.Aux
		}
		return
	}
	c.seen[k] = len(c.items)
	c.items = append(c.items, it)
}

func (c *collector) skip(name, reason string) { c.ignored = append(c.ignored, Ignored{name, reason}) }

var plainVersion = regexp.MustCompile(`^[0-9][0-9A-Za-z.+_~-]*$`)

// --- npm package-lock.json (lockfileVersion 1, 2 and 3) ---

type lockEntry struct {
	Name         string               `json:"name"`
	Version      string               `json:"version"`
	Resolved     string               `json:"resolved"`
	Integrity    string               `json:"integrity"`
	Link         bool                 `json:"link"`
	InBundle     bool                 `json:"inBundle"`
	Bundled      bool                 `json:"bundled"`
	Dependencies map[string]lockEntry `json:"dependencies"`
}

func parsePackageLock(content []byte) ([]Item, []Ignored, error) {
	var doc struct {
		Packages     map[string]lockEntry `json:"packages"`
		Dependencies map[string]lockEntry `json:"dependencies"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, nil, errors.New("package-lock.json is not valid JSON")
	}
	c := newCollector()
	registryEntry := func(name string, e lockEntry) {
		switch {
		case e.Link || e.InBundle || e.Bundled:
			c.skip(name, "linked or bundled")
		case e.Version == "" || !plainVersion.MatchString(e.Version):
			c.skip(name, "not a registry version")
		case e.Resolved != "" && !strings.HasPrefix(e.Resolved, "http://") && !strings.HasPrefix(e.Resolved, "https://"):
			c.skip(name, "not resolved from a registry")
		case strings.Contains(e.Resolved, "codeload.github.com") || strings.Contains(e.Resolved, "/archive/"):
			c.skip(name, "git dependency")
		default:
			c.add(Item{Name: name, Version: e.Version, Integrity: e.Integrity})
		}
	}
	for key, e := range doc.Packages {
		if key == "" {
			continue
		}
		name := key
		if i := strings.LastIndex(key, "node_modules/"); i >= 0 {
			name = key[i+len("node_modules/"):]
		}
		if e.Name != "" {
			name = e.Name
		}
		registryEntry(name, e)
	}
	var walk func(map[string]lockEntry, int)
	walk = func(deps map[string]lockEntry, depth int) {
		if depth > 64 {
			return
		}
		for name, e := range deps {
			registryEntry(name, e)
			walk(e.Dependencies, depth+1)
		}
	}
	if len(doc.Packages) == 0 {
		walk(doc.Dependencies, 0)
	}
	if len(c.items) == 0 && len(c.ignored) == 0 {
		return nil, nil, errors.New("package-lock.json lists no packages")
	}
	return c.items, c.ignored, nil
}

// --- pnpm-lock.yaml (v5, v6, v9) ---

func parsePnpmLock(content []byte) ([]Item, []Ignored, error) {
	var doc struct {
		Packages map[string]struct {
			Resolution struct {
				Integrity string `yaml:"integrity"`
				Tarball   string `yaml:"tarball"`
				Type      string `yaml:"type"`
				Repo      string `yaml:"repo"`
				Commit    string `yaml:"commit"`
			} `yaml:"resolution"`
		} `yaml:"packages"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, nil, errors.New("pnpm-lock.yaml is not valid YAML")
	}
	c := newCollector()
	for key, p := range doc.Packages {
		name, version := splitPnpmKey(key)
		switch {
		case name == "" || version == "":
			c.skip(key, "unrecognised package key")
		case p.Resolution.Repo != "" || p.Resolution.Commit != "" || p.Resolution.Type != "":
			c.skip(name, "git or directory dependency")
		case p.Resolution.Integrity == "":
			c.skip(name, "no integrity (tarball or local dependency)")
		default:
			c.add(Item{Name: name, Version: version, Integrity: p.Resolution.Integrity})
		}
	}
	if len(c.items) == 0 && len(c.ignored) == 0 {
		return nil, nil, errors.New("pnpm-lock.yaml lists no packages")
	}
	return c.items, c.ignored, nil
}

var pnpmAtKey = regexp.MustCompile(`^(@?[^@]+)@([0-9][^(]*)(\(.*)?$`)

func splitPnpmKey(key string) (name, version string) {
	key = strings.TrimPrefix(key, "/")
	if m := pnpmAtKey.FindStringSubmatch(key); m != nil && validPnpmName(m[1]) {
		// v6 and v9: name@version(peers)
		name, version = m[1], m[2]
	} else if i := strings.LastIndex(key, "/"); i > 0 {
		// v5: name/version_peers
		name, version = key[:i], key[i+1:]
		if j := strings.Index(version, "_"); j >= 0 {
			version = version[:j]
		}
	}
	if !plainVersion.MatchString(version) || name == "" {
		return "", ""
	}
	return name, version
}

// validPnpmName tells a package name from the v5 "name/version" form: only a
// scoped name may contain a slash, and then exactly one.
func validPnpmName(n string) bool {
	if strings.HasPrefix(n, "@") {
		return strings.Count(n, "/") == 1
	}
	return !strings.Contains(n, "/")
}

// --- go.sum ---

var goSumLine = regexp.MustCompile(`^(\S+) (v\S+?)(/go\.mod)? (h1:\S+)$`)

func parseGoSum(content []byte) ([]Item, []Ignored, error) {
	c := newCollector()
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	lines := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		m := goSumLine.FindStringSubmatch(line)
		if m == nil {
			c.skip(line, "unrecognised go.sum line")
			continue
		}
		it := Item{Name: m[1], Version: m[2]}
		if m[3] != "" {
			it.Aux = m[4]
		} else {
			it.Integrity = m[4]
		}
		c.add(it)
	}
	if err := sc.Err(); err != nil || lines == 0 {
		return nil, nil, errors.New("go.sum lists no modules")
	}
	return c.items, c.ignored, nil
}

// --- Cargo.lock and poetry.lock (TOML subset) ---

var (
	tomlKeyString = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*=\s*"([^"]*)"\s*$`)
	oldChecksum   = regexp.MustCompile(`^"checksum (\S+) (\S+) \(([^)]*)\)"\s*=\s*"([0-9a-fA-F]{64})"`)
	hex64         = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

func parseCargoLock(content []byte) ([]Item, []Ignored, error) {
	c := newCollector()
	type pkg struct{ name, version, source, checksum string }
	var cur *pkg
	var pkgs []pkg
	flush := func() {
		if cur != nil {
			pkgs = append(pkgs, *cur)
			cur = nil
		}
	}
	legacy := map[[2]string]string{}
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "[[package]]":
			flush()
			cur = &pkg{}
		case strings.HasPrefix(line, "["):
			flush()
		case cur != nil:
			if m := tomlKeyString.FindStringSubmatch(line); m != nil {
				switch m[1] {
				case "name":
					cur.name = m[2]
				case "version":
					cur.version = m[2]
				case "source":
					cur.source = m[2]
				case "checksum":
					cur.checksum = strings.ToLower(m[2])
				}
			}
		default:
			if m := oldChecksum.FindStringSubmatch(line); m != nil {
				legacy[[2]string{m[1], m[2]}] = strings.ToLower(m[4])
			}
		}
	}
	flush()
	if err := sc.Err(); err != nil || len(pkgs) == 0 {
		return nil, nil, errors.New("Cargo.lock lists no packages")
	}
	for _, p := range pkgs {
		switch {
		case p.name == "" || p.version == "":
			c.skip(p.name, "incomplete entry")
		case !strings.HasPrefix(p.source, "registry+") && !strings.HasPrefix(p.source, "sparse+"):
			c.skip(p.name, "path or git dependency")
		default:
			sum := p.checksum
			if sum == "" {
				sum = legacy[[2]string{p.name, p.version}]
			}
			it := Item{Name: p.name, Version: p.version}
			if hex64.MatchString(sum) {
				it.Integrity = "sha256:" + sum
			}
			c.add(it)
		}
	}
	return c.items, c.ignored, nil
}

var (
	poetryHash = regexp.MustCompile(`hash\s*=\s*"sha256:([0-9a-fA-F]{64})"`)
	poetryKey  = regexp.MustCompile(`^"?([A-Za-z0-9_.-]+)"?\s*=\s*\[`)
)

func parsePoetryLock(content []byte) ([]Item, []Ignored, error) {
	c := newCollector()
	type pkg struct {
		name, version, sourceType string
		hashes                    []string
	}
	var pkgs []*pkg
	byName := map[string]*pkg{}
	var cur *pkg
	section := ""
	var legacy *pkg
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "[[package]]":
			cur = &pkg{}
			pkgs = append(pkgs, cur)
			section = "package"
			continue
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[]")
			legacy = nil
			continue
		}
		switch section {
		case "package", "package.files":
			if cur == nil {
				continue
			}
			if m := tomlKeyString.FindStringSubmatch(line); m != nil && section == "package" {
				switch m[1] {
				case "name":
					cur.name = m[2]
				case "version":
					cur.version = m[2]
				}
			}
			for _, m := range poetryHash.FindAllStringSubmatch(line, -1) {
				cur.hashes = append(cur.hashes, "sha256:"+strings.ToLower(m[1]))
			}
		case "package.source":
			if m := tomlKeyString.FindStringSubmatch(line); m != nil && m[1] == "type" && cur != nil {
				cur.sourceType = m[2]
			}
		case "metadata.files":
			if m := poetryKey.FindStringSubmatch(line); m != nil {
				legacy = byName[normalizePyName(m[1])]
			}
			if legacy != nil {
				for _, m := range poetryHash.FindAllStringSubmatch(line, -1) {
					legacy.hashes = append(legacy.hashes, "sha256:"+strings.ToLower(m[1]))
				}
			}
		}
		if section == "package" && cur != nil && cur.name != "" {
			byName[normalizePyName(cur.name)] = cur
		}
	}
	if err := sc.Err(); err != nil || len(pkgs) == 0 {
		return nil, nil, errors.New("poetry.lock lists no packages")
	}
	for _, p := range pkgs {
		switch {
		case p.name == "" || p.version == "":
			c.skip(p.name, "incomplete entry")
		case p.sourceType != "" && p.sourceType != "legacy":
			c.skip(p.name, "git, url or directory dependency")
		default:
			c.add(Item{Name: normalizePyName(p.name), Version: p.version, Integrity: strings.Join(dedupe(p.hashes), ",")})
		}
	}
	return c.items, c.ignored, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// --- requirements.txt ---

var (
	reqPin  = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^\]]*\])?\s*===?\s*([A-Za-z0-9][A-Za-z0-9._+!-]*)\s*$`)
	reqHash = regexp.MustCompile(`--hash[= ]sha256:([0-9a-fA-F]{64})`)
)

func parseRequirements(content []byte) ([]Item, []Ignored, error) {
	c := newCollector()
	// Join backslash continuations, then handle one requirement per logical line.
	var logical []string
	var buf strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasSuffix(line, "\\") {
			buf.WriteString(strings.TrimSuffix(line, "\\") + " ")
			continue
		}
		buf.WriteString(line)
		logical = append(logical, buf.String())
		buf.Reset()
	}
	if buf.Len() > 0 {
		logical = append(logical, buf.String())
	}
	for _, line := range logical {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		var hashes []string
		for _, m := range reqHash.FindAllStringSubmatch(line, -1) {
			hashes = append(hashes, "sha256:"+strings.ToLower(m[1]))
		}
		spec := line
		if i := strings.Index(spec, "--"); i >= 0 {
			spec = spec[:i]
		}
		if i := strings.Index(spec, ";"); i >= 0 {
			spec = spec[:i] // environment markers: the pin applies to the platform that installs it
		}
		spec = strings.TrimSpace(spec)
		m := reqPin.FindStringSubmatch(spec)
		if m == nil {
			c.skip(spec, "not an exact == pin")
			continue
		}
		c.add(Item{Name: normalizePyName(m[1]), Version: strings.ToLower(m[2]), Integrity: strings.Join(dedupe(hashes), ",")})
	}
	if len(c.items) == 0 && len(c.ignored) == 0 {
		return nil, nil, errors.New("requirements.txt lists no requirements")
	}
	return c.items, c.ignored, nil
}

var pyNameRun = regexp.MustCompile(`[-_.]+`)

func normalizePyName(s string) string { return strings.ToLower(pyNameRun.ReplaceAllString(s, "-")) }
