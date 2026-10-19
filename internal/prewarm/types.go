// Package prewarm warms the remote cache of a feed ahead of demand (spec 6.5):
// from a package list, a lockfile, a list of images, or everything another feed
// references, now or on a schedule. A job is the durable definition, a run one
// execution; the per-item rows of a run are the durable checkpoint a restart
// resumes from. Warming goes through the feed's own protocol routes, in process
// and as the principal that owns the run, so every adapter applies its normal
// authorization, package-owner rules, integrity checks and artifact.Materialize
// pull-through: prewarm never writes to the store by any other path.
package prewarm

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits shared by the API and the runner.
const (
	MaxItems        = 20000
	MaxLockfileSize = 8 << 20
	MaxNameLength   = 512
	DefaultRate     = 10
	DefaultWorkers  = 4
	MaxWorkers      = 32
	MaxRate         = 1000
	MinInterval     = 60
	MaxInterval     = 31 * 24 * 3600
	KeepRuns        = 20
)

// Source kinds.
const (
	KindPackages = "packages"
	KindLockfile = "lockfile"
	KindImages   = "images"
	KindFeed     = "feed"
)

// Run states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StatePartial   = "partial"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// Item states.
const (
	ItemPending = "pending"
	ItemWarmed  = "warmed"
	ItemCached  = "cached"
	ItemFailed  = "failed"
	ItemSkipped = "skipped"
)

// Failure codes recorded on items (a closed set: upstream detail never reaches
// the API).
const (
	ErrNotFound   = "not_found"
	ErrUnavail    = "upstream_unavailable"
	ErrIntegrity  = "integrity_mismatch"
	ErrDenied     = "denied"
	ErrTimeout    = "timeout"
	ErrInternal   = "internal"
	ErrUnsupport  = "unsupported"
	ErrCanceled   = "canceled"
	ErrNoMatching = "no_matching_file"
)

// Item is one thing to warm. Its meaning depends on the format:
//
//	npm, cargo, go, pypi     Name is the package (module, crate) and Version its version
//	maven                    Name is group:artifact[:packaging[:classifier]]
//	raw                      Name is the path inside the feed
//	oci (images)             Name is the repository and Version a tag or digest
//
// Integrity is what the source pins for the bytes (a lockfile hash): an SRI
// value (sha512-<base64>), algo:hex, or for Go the go.sum h1: hash of the module
// zip. The fetched bytes are compared against it. Aux carries the go.sum h1:
// hash of the go.mod file.
type Item struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Integrity string `json:"integrity,omitempty"`
	Aux       string `json:"aux,omitempty"`
}

// Ignored is an entry of an input document that was not turned into an item.
type Ignored struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Options tune how items are warmed. All are optional.
type Options struct {
	// Platforms limits which images of a multi-platform index are pulled, as
	// os/arch[/variant]; "all" pulls every one. Default linux/amd64.
	Platforms []string `json:"platforms,omitempty"`
	// FileFilters limits which PyPI files of a version are warmed: a file is
	// warmed when its name contains one of the entries. Without filters and
	// without pinned hashes, pure-Python wheels and source distributions are
	// warmed. Pinned hashes from a lockfile always win.
	FileFilters []string `json:"file_filters,omitempty"`
}

// Validate bounds the options.
func (o Options) Validate() error {
	if len(o.Platforms) > 16 || len(o.FileFilters) > 32 {
		return errors.New("too many options")
	}
	for _, p := range o.Platforms {
		if p != "all" && !platformPattern.MatchString(p) {
			return fmt.Errorf("invalid platform %q", p)
		}
	}
	for _, f := range o.FileFilters {
		if f == "" || len(f) > 128 || !printable(f) {
			return errors.New("invalid file filter")
		}
	}
	return nil
}

var platformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9_]+(/[a-z0-9]+)?$`)

func printable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == '\\' })
}

// LockfileFormat maps a lockfile type to the feed format it warms.
var LockfileFormat = map[string]string{
	"package-lock.json": "npm",
	"pnpm-lock.yaml":    "npm",
	"go.sum":            "go",
	"Cargo.lock":        "cargo",
	"requirements.txt":  "pypi",
	"poetry.lock":       "pypi",
}

// Formats returns the feed formats that can be prewarmed and what each accepts.
func Formats() map[string]FormatInfo {
	out := map[string]FormatInfo{}
	for name, f := range formats {
		info := FormatInfo{Packages: f.packages, Images: f.images, Feed: f.feed}
		for lock, format := range LockfileFormat {
			if format == name {
				info.Lockfiles = append(info.Lockfiles, lock)
			}
		}
		sort.Strings(info.Lockfiles)
		out[name] = info
	}
	return out
}

// FormatInfo describes the inputs accepted for one feed format.
type FormatInfo struct {
	Lockfiles []string
	Packages  bool
	Images    bool
	Feed      bool
}

// Supported reports whether feeds of a format can be prewarmed.
func Supported(format string) bool { _, ok := formats[format]; return ok }

// Spec is the validated definition of a job source.
type Spec struct {
	Kind         string
	LockfileType string
	SourceFeedID string
	Items        []Item
	Ignored      []Ignored
}

// CheckKind verifies that a source kind is accepted for a feed format.
func CheckKind(format, kind, lockfileType string) error {
	f, ok := formats[format]
	if !ok {
		return fmt.Errorf("feeds of format %q cannot be prewarmed", format)
	}
	switch kind {
	case KindPackages:
		if !f.packages {
			return fmt.Errorf("a package list is not supported for %s feeds", format)
		}
	case KindImages:
		if !f.images {
			return fmt.Errorf("an image list is not supported for %s feeds", format)
		}
	case KindFeed:
		if !f.feed {
			return fmt.Errorf("warming from another feed is not supported for %s feeds", format)
		}
	case KindLockfile:
		if LockfileFormat[lockfileType] != format {
			return fmt.Errorf("lockfile %q does not apply to %s feeds", lockfileType, format)
		}
	default:
		return fmt.Errorf("unknown source kind %q", kind)
	}
	return nil
}

// ValidateItems checks a package or image list for a format and normalizes it.
func ValidateItems(format, kind string, in []Item) ([]Item, error) {
	f, ok := formats[format]
	if !ok {
		return nil, fmt.Errorf("feeds of format %q cannot be prewarmed", format)
	}
	if len(in) == 0 {
		return nil, errors.New("the list is empty")
	}
	if len(in) > MaxItems {
		return nil, fmt.Errorf("at most %d items are allowed", MaxItems)
	}
	out := make([]Item, 0, len(in))
	seen := map[Item]bool{}
	for i, it := range in {
		if len(it.Name) > MaxNameLength || len(it.Version) > 256 || len(it.Integrity) > 4096 || len(it.Aux) > 1024 || !printable(it.Name+it.Version+it.Integrity+it.Aux) {
			return nil, fmt.Errorf("item %d is malformed", i+1)
		}
		var err error
		if kind == KindImages {
			if f.checkImage == nil {
				return nil, fmt.Errorf("an image list is not supported for %s feeds", format)
			}
			it, err = f.checkImage(it)
		} else {
			if f.check == nil {
				return nil, fmt.Errorf("a package list is not supported for %s feeds", format)
			}
			it, err = f.check(it)
		}
		if err != nil {
			return nil, fmt.Errorf("item %d (%s): %w", i+1, it.Name, err)
		}
		if !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return out, nil
}

// FilterLockfileItems keeps the lockfile entries that are valid for the feed
// format and reports the rest as ignored, so one odd entry never rejects a
// whole lockfile.
func FilterLockfileItems(format string, in []Item, ignored []Ignored) ([]Item, []Ignored) {
	f, ok := formats[format]
	if !ok || f.check == nil {
		return nil, ignored
	}
	out := make([]Item, 0, len(in))
	for _, it := range in {
		if len(it.Name) > MaxNameLength || len(it.Version) > 256 || len(it.Integrity) > 4096 || len(it.Aux) > 1024 || !printable(it.Name+it.Version+it.Integrity+it.Aux) {
			ignored = append(ignored, Ignored{it.Name, "malformed entry"})
			continue
		}
		checked, err := f.check(it)
		if err != nil {
			ignored = append(ignored, Ignored{it.Name, err.Error()})
			continue
		}
		out = append(out, checked)
	}
	return out, ignored
}
