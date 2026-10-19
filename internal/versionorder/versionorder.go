// Package versionorder gives package discovery one tolerant ordering for the
// version strings of every format (semver, Maven, PyPI, NuGet, Go, Helm, Cargo,
// OCI tags). It is used only to pick the "latest" version shown in listings;
// protocol adapters keep their own authoritative resolution.
package versionorder

import (
	"strings"
)

// Version is a parsed version: a numeric core, an optional prerelease suffix
// and the original text.
type Version struct {
	Raw     string
	Core    []uint64
	Pre     string
	Release bool // no prerelease suffix
}

// Parse accepts "1.2.3", "v1.2.3", "1.2.3-rc.1", "2.0", "1.0.0+build" and
// similar. It reports false when the text has no leading numeric core.
func Parse(raw string) (Version, bool) {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	v := Version{Raw: raw}
	i := 0
	for i < len(s) {
		j := i
		var n uint64
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			if j-i >= 18 {
				return Version{}, false
			}
			n = n*10 + uint64(s[j]-'0')
			j++
		}
		if j == i {
			break
		}
		v.Core = append(v.Core, n)
		i = j
		if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	if len(v.Core) == 0 {
		return Version{}, false
	}
	v.Pre = strings.TrimLeft(s[i:], "-.")
	v.Release = s[i:] == ""
	return v, true
}

// Compare orders a before b (-1), equal (0) or after (1). A release outranks
// any prerelease of the same core.
func Compare(a, b Version) int {
	for i := 0; i < len(a.Core) || i < len(b.Core); i++ {
		var x, y uint64
		if i < len(a.Core) {
			x = a.Core[i]
		}
		if i < len(b.Core) {
			y = b.Core[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Release && !b.Release:
		return 1
	case !a.Release && b.Release:
		return -1
	case a.Release:
		return 0
	}
	return comparePre(a.Pre, b.Pre)
}

func comparePre(a, b string) int {
	as, bs := strings.FieldsFunc(a, sep), strings.FieldsFunc(b, sep)
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, xn := number(as[i])
		y, yn := number(bs[i])
		switch {
		case xn && yn:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		case xn:
			return -1
		case yn:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func sep(r rune) bool { return r == '.' || r == '-' || r == '_' }

func number(s string) (uint64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}

// Latest picks the highest version: the highest release if any parses, else the
// highest prerelease, else the first entry (callers pass newest first). It
// returns false only for an empty list.
func Latest(newestFirst []string) (string, bool) {
	if len(newestFirst) == 0 {
		return "", false
	}
	var bestRelease, bestPre *Version
	for _, raw := range newestFirst {
		v, ok := Parse(raw)
		if !ok {
			continue
		}
		slot := &bestPre
		if v.Release {
			slot = &bestRelease
		}
		if *slot == nil || Compare(v, **slot) > 0 {
			c := v
			*slot = &c
		}
	}
	switch {
	case bestRelease != nil:
		return bestRelease.Raw, true
	case bestPre != nil:
		return bestPre.Raw, true
	}
	return newestFirst[0], true
}

// Less reports whether a sorts before b when ordering descending is wanted by
// the caller negating it; unparseable text sorts below parseable text.
func Less(a, b string) bool {
	x, xok := Parse(a)
	y, yok := Parse(b)
	switch {
	case xok && yok:
		return Compare(x, y) < 0
	case xok != yok:
		return yok
	}
	return a < b
}
