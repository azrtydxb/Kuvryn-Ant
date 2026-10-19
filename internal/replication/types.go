// Package replication implements independent-instance feed replication
// (spec 6.7): an authenticated export API on the authoritative instance and a
// restartable, idempotent pull worker on destination instances.
package replication

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInput       = errors.New("invalid replication request")
	ErrDenied      = errors.New("replication access denied")
	ErrNotFound    = errors.New("replication resource not found")
	ErrConflict    = errors.New("replication resource conflicts")
	ErrUnavailable = errors.New("replication storage unavailable")
	ErrCorrupt     = errors.New("replicated blob failed digest verification")
	ErrLeaseLost   = errors.New("replication job lease lost")
	ErrPeer        = errors.New("replication peer request failed")
	// ErrManaged rejects local changes to federation-managed peers and jobs.
	ErrManaged = errors.New("replication resource is federation-managed")
	// ErrHistoryPurged means the peer no longer holds the change history the
	// caller asked for (cursor below the low-water mark); the consumer must reseed.
	ErrHistoryPurged = errors.New("replication change history purged below cursor")
)

// HistoryPurgedError carries the peer's low-water mark.
type HistoryPurgedError struct{ LowWater int64 }

func (e *HistoryPurgedError) Error() string {
	return fmt.Sprintf("%v (low-water %d)", ErrHistoryPurged, e.LowWater)
}
func (e *HistoryPurgedError) Is(target error) bool { return target == ErrHistoryPurged }

// Origin identifies the authoritative source an item belongs to. Kind "remote"
// marks a cached pull-through entry.
type Origin struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority"`
}

// Subject carries artifact_subjects metadata (OCI referrers and similar).
type Subject struct {
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	MediaType    string            `json:"media_type"`
	ArtifactType string            `json:"artifact_type"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Item is one asset manifest entry: a version asset and the blob it references.
type Item struct {
	Origin            Origin          `json:"origin"`
	Namespace         *string         `json:"namespace"`
	Coordinate        string          `json:"coordinate"`
	Version           string          `json:"version"`
	AssetKey          string          `json:"asset_key"`
	MediaType         string          `json:"media_type"`
	Digest            string          `json:"digest"`
	Size              int64           `json:"size"`
	ComponentMetadata json.RawMessage `json:"component_metadata,omitempty"`
	Subject           *Subject        `json:"subject,omitempty"`
}

// TagItem is a mutable tag pointer at a version of a component.
type TagItem struct {
	Origin     Origin  `json:"origin"`
	Namespace  *string `json:"namespace"`
	Coordinate string  `json:"coordinate"`
	Tag        string  `json:"tag"`
	Version    string  `json:"version"`
}

// ComponentItem carries package-level metadata/index state.
type ComponentItem struct {
	Origin     Origin          `json:"origin"`
	Namespace  *string         `json:"namespace"`
	Coordinate string          `json:"coordinate"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// Head reports the current durable change sequence of the exporting instance.
type Head struct {
	Seq       int64      `json:"seq"`
	ChangedAt *time.Time `json:"changed_at,omitempty"`
	Now       time.Time  `json:"now"`
	Format    string     `json:"format"`
	// LowWater is the highest purged change sequence: incremental service is
	// only possible for cursors at or above it.
	LowWater int64 `json:"low_water"`
}

// ManifestPage is one page of the current-state manifest. Next is empty at the
// end. Total (first page only) counts matching assets or tags.
type ManifestPage struct {
	Kind  string    `json:"kind"`
	Items []Item    `json:"items,omitempty"`
	Tags  []TagItem `json:"tags,omitempty"`
	Next  string    `json:"next,omitempty"`
	Total *int64    `json:"total,omitempty"`
}

// Change is one durable, ordered change. Puts carry the current state of the
// item at export time (nil when it has since disappeared; a later tombstone
// follows). Deletes carry only identity.
type Change struct {
	Seq        int64          `json:"seq"`
	At         time.Time      `json:"at"`
	Kind       string         `json:"kind"`
	Op         string         `json:"op"`
	SourceID   string         `json:"source_id"`
	Coordinate string         `json:"coordinate"`
	Version    string         `json:"version,omitempty"`
	Key        string         `json:"key,omitempty"`
	Asset      *Item          `json:"asset,omitempty"`
	Tag        *TagItem       `json:"tag,omitempty"`
	Component  *ComponentItem `json:"component,omitempty"`
	Attachment *Attachment    `json:"attachment,omitempty"`
}

// Attachment reports that a source was attached to (put) or detached from
// (delete) the exported feed. A put means content already in the source is now
// part of the feed and has to be back-filled by consumers.
type Attachment struct {
	Origin Origin `json:"origin"`
}

// ChangePage returns relevant changes after a cursor. Next is the new cursor:
// the last returned seq when More, otherwise the head observed in the same
// snapshot (so unrelated changes never stall the cursor).
type ChangePage struct {
	Changes []Change   `json:"changes"`
	Next    int64      `json:"next"`
	More    bool       `json:"more"`
	Head    int64      `json:"head"`
	HeadAt  *time.Time `json:"head_at,omitempty"`
	// LowWater is the peer's purge low-water mark at the time of the read.
	LowWater int64 `json:"low_water"`
}

// Filters select a subset of a feed. Empty lists match everything. Namespaces
// and packages match exactly; versions match by prefix.
type Filters struct {
	Namespaces      []string `json:"namespaces,omitempty"`
	Packages        []string `json:"packages,omitempty"`
	VersionPrefixes []string `json:"version_prefixes,omitempty"`
}

func (f Filters) valid() bool {
	for _, list := range [][]string{f.Namespaces, f.Packages, f.VersionPrefixes} {
		if len(list) > 256 {
			return false
		}
		for _, v := range list {
			if len(v) > 256 || strings.ContainsAny(v, "\x00\r\n") {
				return false
			}
		}
	}
	for _, v := range f.Packages {
		if v == "" {
			return false
		}
	}
	for _, v := range f.VersionPrefixes {
		if v == "" {
			return false
		}
	}
	return true
}

// Match evaluates the filter. version may be empty for package-level items.
func (f Filters) Match(namespace *string, coordinate, version string) bool {
	if len(f.Namespaces) > 0 {
		ns := ""
		if namespace != nil {
			ns = *namespace
		}
		if !has(f.Namespaces, ns) {
			return false
		}
	}
	if len(f.Packages) > 0 && !has(f.Packages, coordinate) {
		return false
	}
	if len(f.VersionPrefixes) > 0 && version != "" {
		ok := false
		for _, p := range f.VersionPrefixes {
			if strings.HasPrefix(version, p) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
func has(list []string, v string) bool {
	for _, c := range list {
		if c == v {
			return true
		}
	}
	return false
}
