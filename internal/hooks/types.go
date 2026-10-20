// Package hooks implements incoming webhooks: an external system calls
// <public URL>/hook/<slug>, the call is authenticated, recorded and queued, and
// a worker under the instance writer lease runs the hook's one configured
// action. The delivery table is the delivery log and the work queue at once, so
// slow actions never block the caller.
package hooks

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Authentication modes.
const (
	AuthHMAC     = "hmac"
	AuthURLToken = "url_token"
	AuthBearer   = "bearer"
)

// HMAC header formats.
const (
	FormatGitHub      = "github"      // X-Hub-Signature-256: sha256=<hex> over the body
	FormatGitLab      = "gitlab"      // X-Gitlab-Token: the shared secret itself
	FormatAnt         = "ant"         // X-Ant-Signature: sha256=<hex> over the body
	FormatNPM         = "npm"         // X-NPM-Signature: sha256=<hex> over the body
	FormatArtifactory = "artifactory" // X-JFrog-Event-Auth: <hex> HMAC-SHA256 over the body (or the secret itself)
	FormatNexus       = "nexus"       // X-Nexus-Webhook-Signature: <hex> HMAC-SHA1 over the body
)

// Token scopes a bearer hook may require (the action the token must hold on the instance).
const (
	ScopeConfigure = "configure"
	ScopeReplicate = "replicate"
	ScopeAdmin     = "admin"
)

// Action types.
const (
	ActionPrewarm        = "prewarm"
	ActionReplicationRun = "replication_run"
	ActionPurgeSource    = "purge_source_cache"
	ActionCleanCache     = "clean_cache"
	ActionUpstream       = "upstream_notify"
	ActionRelay          = "relay"
)

// Upstream providers. "auto" detects the provider from headers and payload.
const (
	ProviderAuto        = "auto"
	ProviderDockerHub   = "dockerhub"
	ProviderGitHub      = "github"
	ProviderGitLab      = "gitlab"
	ProviderNPM         = "npm"
	ProviderArtifactory = "artifactory"
	ProviderNexus       = "nexus"
)

// Delivery states and authentication results.
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateRejected  = "rejected"

	AuthOK          = "ok"
	AuthTest        = "test"
	AuthRedelivery  = "redelivery"
	AuthInvalid     = "invalid"
	AuthMissing     = "missing"
	AuthDuplicate   = "duplicate"
	AuthTooLarge    = "too_large"
	DefaultMaxBody  = 1 << 20
	AbsoluteMaxBody = 8 << 20
	DefaultLogDays  = 14
	// ReplayWindow is how long provider delivery ids are remembered.
	ReplayWindow = 24 * time.Hour
	// PreviewBytes bounds the stored, redacted payload preview.
	PreviewBytes = 2048
	// MaxAttempts bounds retries of one delivery.
	MaxAttempts = 8
)

// Errors returned by validation and the store.
var (
	ErrInput       = errors.New("invalid incoming hook configuration")
	ErrNotFound    = errors.New("incoming hook not found")
	ErrUnavailable = errors.New("incoming hooks unavailable")
)

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// ValidSlug reports whether s is a legal hook slug: lowercase letters, digits
// and hyphens, 1 to 63 characters.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// Action is the one thing a hook does. Which fields apply depends on Type:
//
//	prewarm             FeedID, Kind (package|image), Name, Version (templates)
//	replication_run     JobID
//	purge_source_cache  SourceID, Coordinate (template; empty purges everything)
//	clean_cache         FeedID (a cache feed), Prefix (template)
//	upstream_notify     Provider, SourceIDs/Origins (optional), Prewarm, FeedID (optional)
//	relay               TargetURL, ResignSecretID (optional)
//
// Templates use {{ .path.to.value }} against the JSON payload with an optional
// | lower, | upper, | trimv or | base filter; see Render.
type Action struct {
	Type           string   `json:"type"`
	FeedID         string   `json:"feed_id,omitempty"`
	Kind           string   `json:"kind,omitempty"`
	Name           string   `json:"name,omitempty"`
	Version        string   `json:"version,omitempty"`
	JobID          string   `json:"job_id,omitempty"`
	SourceID       string   `json:"source_id,omitempty"`
	Coordinate     string   `json:"coordinate,omitempty"`
	Prefix         string   `json:"prefix,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	SourceIDs      []string `json:"source_ids,omitempty"`
	Origins        []string `json:"origins,omitempty"`
	Prewarm        bool     `json:"prewarm,omitempty"`
	TargetURL      string   `json:"target_url,omitempty"`
	ResignSecretID string   `json:"resign_secret_id,omitempty"`
}

// Hook is a stored incoming hook (without the token, which is never stored).
type Hook struct {
	ID              string
	Slug            string
	Description     string
	Enabled         bool
	AuthMode        string
	HMACFormat      string
	SigningSecretID string
	TokenHash       []byte
	TokenHint       string
	BearerScope     string
	Action          Action
	RatePerMinute   int
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Revision        int64
}

// Delivery is one logged call.
type Delivery struct {
	ID           string
	HookID       string
	ReceivedAt   time.Time
	SourceIP     string
	AuthResult   string
	State        string
	Attempts     int
	AvailableAt  time.Time
	FinishedAt   *time.Time
	ActionResult string
	Detail       map[string]any
	Error        string
	PayloadSize  int
	Preview      string
	DeliveryRef  string
	RedeliveryOf string
}

func printable(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) })
}
