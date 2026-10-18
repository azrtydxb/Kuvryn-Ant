// Package transfer exports repository definitions and content from Kuvryn Ant
// as a verifiable bundle, imports such bundles idempotently (with a dry run that
// reports exactly what an apply would do), and maps repositories of other
// artifact managers onto Ant feeds and sources.
//
// A bundle is a directory:
//
//	document.json        definitions and content inventory
//	blobs/<sha256>       verbatim content, referenced by document.json
//	manifest.json        written last; digests of document.json and every blob
//
// Credential values are never exported. A remote source carries only a
// reference (the purpose string that bound its credential); the secret must be
// provisioned again on the target.
package transfer

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

const FormatVersion = 1

var (
	ErrInput     = errors.New("transfer input is invalid")
	ErrIntegrity = errors.New("bundle integrity verification failed")
	ErrTarget    = errors.New("bundle target is not acceptable")

	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	slugPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// Selection limits an export. Empty Feeds selects every feed; empty Namespaces
// selects every namespace. A namespace matches itself and children separated by
// "/" or ".".
type Selection struct {
	Feeds      []string `json:"feeds,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// CredentialRef identifies the credential a remote source used without
// disclosing it. Purpose is the source-bound purpose string of the original.
type CredentialRef struct {
	Purpose string `json:"purpose"`
}

type RetentionDef struct {
	KeepLast           *int32 `json:"keep_last,omitempty"`
	MaxAgeSeconds      *int64 `json:"max_age_seconds,omitempty"`
	MaxIdleSeconds     *int64 `json:"max_idle_seconds,omitempty"`
	MinKeep            int32  `json:"min_keep"`
	IncludeTagged      bool   `json:"include_tagged"`
	AllowReleaseDelete bool   `json:"allow_release_delete"`
	Enabled            bool   `json:"enabled"`
}

type SourceDef struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Format string `json:"format"`
	// RemoteConfig is the remote source configuration without its credential ID.
	RemoteConfig *json.RawMessage `json:"remote_config,omitempty"`
	Credential   *CredentialRef   `json:"credential_ref,omitempty"`
	Retention    *RetentionDef    `json:"retention,omitempty"`
}

type FeedSourceRef struct {
	Source   string `json:"source"`
	Priority int    `json:"priority"`
}

// PublishPolicyDef is a publish policy whose targets are source names.
type PublishPolicyDef struct {
	DefaultSource string         `json:"default_source,omitempty"`
	Rules         []routing.Rule `json:"rules"`
}

type FeedDef struct {
	Slug          string            `json:"slug"`
	Format        string            `json:"format"`
	AnonymousRead bool              `json:"anonymous_read"`
	Sources       []FeedSourceRef   `json:"sources"`
	PublishPolicy *PublishPolicyDef `json:"publish_policy,omitempty"`
	Retention     *RetentionDef     `json:"retention,omitempty"`
}

type SubjectDef struct {
	Digest       string          `json:"digest"`
	Size         int64           `json:"size"`
	MediaType    string          `json:"media_type"`
	ArtifactType string          `json:"artifact_type,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

type AssetDef struct {
	Key        string          `json:"key"`
	MediaType  string          `json:"media_type"`
	Digest     string          `json:"digest"`
	Size       int64           `json:"size"`
	Provenance json.RawMessage `json:"provenance,omitempty"`
	Subject    *SubjectDef     `json:"subject,omitempty"`
}

type VersionDef struct {
	Version string     `json:"version"`
	Assets  []AssetDef `json:"assets"`
}

type TagDef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ComponentDef struct {
	Source     string          `json:"source"`
	Namespace  string          `json:"namespace"`
	Coordinate string          `json:"coordinate"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Versions   []VersionDef    `json:"versions"`
	Tags       []TagDef        `json:"tags,omitempty"`
}

// Document is the portable definition of a set of feeds, their sources and,
// optionally, their local content inventory.
type Document struct {
	FormatVersion int            `json:"format_version"`
	CreatedAt     time.Time      `json:"created_at"`
	Selection     Selection      `json:"selection"`
	Sources       []SourceDef    `json:"sources"`
	Feeds         []FeedDef      `json:"feeds"`
	Components    []ComponentDef `json:"components"`
	// SkippedContent lists sources whose content is deliberately not exported
	// (remote caches and replicas are re-fetched or re-replicated).
	SkippedContent []string `json:"skipped_content,omitempty"`
}

// BlobEntry is one content object in a bundle.
type BlobEntry struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Manifest seals a bundle directory.
type Manifest struct {
	FormatVersion  int         `json:"format_version"`
	CreatedAt      time.Time   `json:"created_at"`
	DocumentSHA256 string      `json:"document_sha256"`
	DocumentBytes  int64       `json:"document_bytes"`
	Blobs          []BlobEntry `json:"blobs"`
	Notice         string      `json:"notice"`
}

const notice = "Credential values are not part of this bundle; remote source credentials must be provisioned again on the target."

// Validate checks structural consistency of a document received from outside.
func (d Document) Validate() error {
	if d.FormatVersion != FormatVersion {
		return ErrInput
	}
	srcs := map[string]bool{}
	for _, s := range d.Sources {
		if s.Name == "" || len(s.Name) > 256 || srcs[s.Name] || (s.Kind != "local" && s.Kind != "remote" && s.Kind != "replica") || s.Format == "" {
			return ErrInput
		}
		srcs[s.Name] = true
	}
	feeds := map[string]bool{}
	for _, f := range d.Feeds {
		if !slugPattern.MatchString(f.Slug) || feeds[f.Slug] || f.Format == "" {
			return ErrInput
		}
		feeds[f.Slug] = true
		prio := map[int]bool{}
		for _, r := range f.Sources {
			if !srcs[r.Source] || prio[r.Priority] {
				return ErrInput
			}
			prio[r.Priority] = true
		}
	}
	for _, c := range d.Components {
		if !srcs[c.Source] || c.Coordinate == "" {
			return ErrInput
		}
		for _, v := range c.Versions {
			if v.Version == "" {
				return ErrInput
			}
			for _, a := range v.Assets {
				if a.Key == "" || !digestPattern.MatchString(a.Digest) || a.Size < -1 {
					return ErrInput
				}
			}
		}
	}
	return nil
}
