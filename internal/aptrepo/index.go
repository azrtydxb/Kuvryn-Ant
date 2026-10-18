package aptrepo

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxIndex    = 64 << 20
	maxRelease  = 1 << 20
	maxStanzas  = 50000
	releaseDate = "Mon, 02 Jan 2006 15:04:05 UTC"
)

// entry is one package paragraph of a Packages index.
type entry struct {
	text                                   string
	pkg, version, arch, filename, checksum string
}

func parseEntry(text string) (entry, error) {
	c, e := parseParagraph([]byte(text))
	if e != nil {
		return entry{}, errCorrupt
	}
	x := entry{text: text, pkg: c.get("Package"), version: c.get("Version"), arch: c.get("Architecture"), filename: c.get("Filename"), checksum: c.get("SHA256")}
	if !validPackage(x.pkg) || !validVersion(x.version) || !archPattern.MatchString(x.arch) || x.filename == "" || len(x.checksum) != 64 {
		return entry{}, errCorrupt
	}
	return x, nil
}

// parseEntries reads a bounded Packages file.
func parseEntries(data []byte) ([]entry, error) {
	var out []entry
	for _, p := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if len(out) >= maxStanzas {
			return nil, errCorrupt
		}
		x, e := parseEntry(p + "\n")
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, nil
}

// state maps component -> pool filename -> entry for one distribution.
type state map[string]map[string]entry

func (s state) count() int {
	n := 0
	for _, m := range s {
		n += len(m)
	}
	return n
}

// indexFile is one hashed object listed in Release.
type indexFile struct {
	path, media, sha string
	data             []byte
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func deterministicGzip(b []byte) []byte {
	var z bytes.Buffer
	g, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
	_, _ = g.Write(b) // Header has zero ModTime, so equal input yields equal bytes.
	_ = g.Close()
	return z.Bytes()
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// buildGeneration renders every Packages/Packages.gz index and the Release text
// for the state. Components without packages are omitted. Architecture "all"
// entries are merged into each concrete architecture index.
func buildGeneration(dist string, st state, now time.Time, validity time.Duration) (release []byte, files []indexFile) {
	archs := map[string]bool{}
	comps := []string{}
	for _, comp := range sortedKeys(st) {
		if len(st[comp]) == 0 {
			continue
		}
		comps = append(comps, comp)
		for _, e := range st[comp] {
			if e.arch != "all" {
				archs[e.arch] = true
			}
		}
	}
	if len(archs) == 0 && len(comps) > 0 {
		archs["all"] = true
	}
	archList := sortedKeys(archs)
	for _, comp := range comps {
		for _, arch := range archList {
			var list []entry
			for _, e := range st[comp] {
				if e.arch == arch || e.arch == "all" {
					list = append(list, e)
				}
			}
			sort.Slice(list, func(i, j int) bool {
				a, b := list[i], list[j]
				if a.pkg != b.pkg {
					return a.pkg < b.pkg
				}
				if a.version != b.version {
					return a.version < b.version
				}
				return a.filename < b.filename
			})
			var b bytes.Buffer
			for i, e := range list {
				if i > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(e.text)
			}
			base := comp + "/binary-" + arch + "/Packages"
			plain := b.Bytes()
			gz := deterministicGzip(plain)
			files = append(files,
				indexFile{path: base, media: "text/plain; charset=utf-8", sha: sum(plain), data: plain},
				indexFile{path: base + ".gz", media: "application/gzip", sha: sum(gz), data: gz})
		}
	}
	var r strings.Builder
	r.WriteString("Origin: Kuvryn Ant\nLabel: Kuvryn Ant\nSuite: " + dist + "\nCodename: " + dist + "\n")
	r.WriteString("Date: " + now.UTC().Format(releaseDate) + "\n")
	r.WriteString("Valid-Until: " + now.Add(validity).UTC().Format(releaseDate) + "\n")
	r.WriteString("Acquire-By-Hash: yes\n")
	r.WriteString("Architectures: " + strings.Join(archList, " ") + "\n")
	r.WriteString("Components: " + strings.Join(comps, " ") + "\n")
	r.WriteString("Description: Kuvryn Ant APT repository\nSHA256:\n")
	for _, f := range files {
		fmt.Fprintf(&r, " %s %d %s\n", f.sha, len(f.data), f.path)
	}
	return []byte(r.String()), files
}

type releaseFile struct {
	sha  string
	size int64
}

// parseRelease returns the SHA256 section of a Release file.
func parseRelease(b []byte) (map[string]releaseFile, error) {
	if len(b) > maxRelease {
		return nil, errCorrupt
	}
	out := map[string]releaseFile{}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case line == "SHA256:":
			in = true
		case in && strings.HasPrefix(line, " "):
			f := strings.Fields(line)
			if len(f) != 3 || len(f[0]) != 64 {
				return nil, errCorrupt
			}
			n, e := strconv.ParseInt(f[1], 10, 64)
			if e != nil || n < 0 || n > maxIndex {
				return nil, errCorrupt
			}
			out[f[2]] = releaseFile{f[0], n}
		default:
			in = false
		}
	}
	return out, nil
}
