package federation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Topology (docs/planning/phase-2-federation.md §9): the signed, versioned
// document that records federated feeds (feed key, home member, descriptor,
// transfer state machine) and replication rules. It uses the same envelope
// as membership and policy: JCS canonical form, Ed25519 signature by the
// authority key of its epoch, hash chain over generations.

// FeedDescriptor is the reviewed, non-secret part of a federated feed that a
// target member uses to create the destination feed when it has none. It
// seeds creation only and never overwrites local settings. Secrets, upstream
// URLs and credentials are never part of it.
type FeedDescriptor struct {
	Format        string `json:"format"`
	AnonymousRead bool   `json:"anonymous_read"`
}

// Transfer states (§9.5). freeze → drain → promote → complete for planned
// transfers, promote → complete for forced ones; freeze and drain can abort.
const (
	TransferFreeze   = "freeze"
	TransferDrain    = "drain"
	TransferPromote  = "promote"
	TransferComplete = "complete"
	TransferAborted  = "aborted"
)

// FeedTransfer is the home transfer state machine of one feed key.
type FeedTransfer struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Mode  string `json:"mode"` // planned | forced
	State string `json:"state"`
	// Head is the old home's change head recorded at freeze (planned).
	Head int64 `json:"head"`
	// Checkpoint is the target's applied change sequence of the old home at
	// promotion; for a forced transfer every later change of the old home is
	// possibly lost and quarantined there.
	Checkpoint int64 `json:"checkpoint"`
	// Baseline is the new home's change head at promotion.
	Baseline  int64  `json:"baseline"`
	StartedAt string `json:"started_at"`
	Deadline  string `json:"deadline,omitempty"`
	UpdatedAt string `json:"updated_at"`
	Reason    string `json:"reason,omitempty"`
}

// Active reports a transfer that has not completed or aborted.
func (t *FeedTransfer) Active() bool {
	return t != nil && t.State != TransferComplete && t.State != TransferAborted
}

// TopologyFeed is one federated feed key with its single home member.
type TopologyFeed struct {
	Key        string         `json:"key"`
	Home       string         `json:"home"`
	Descriptor FeedDescriptor `json:"descriptor"`
	Revision   int64          `json:"revision"`
	Transfer   *FeedTransfer  `json:"transfer,omitempty"`
}

// RuleTargets selects target members by labels (all must match) or by
// explicit member instance IDs.
type RuleTargets struct {
	Labels  map[string]string `json:"labels,omitempty"`
	Members []string          `json:"members,omitempty"`
}

// RuleFilters mirror replication selective-sync filters.
type RuleFilters struct {
	Namespaces      []string `json:"namespaces,omitempty"`
	Packages        []string `json:"packages,omitempty"`
	VersionPrefixes []string `json:"version_prefixes,omitempty"`
}

// ReplicationRule replicates a feed key from its home to every target member.
type ReplicationRule struct {
	ID                    string      `json:"id"`
	FeedKey               string      `json:"feed_key"`
	Targets               RuleTargets `json:"targets"`
	Mode                  string      `json:"mode"`
	IntervalSeconds       int         `json:"interval_seconds"`
	IncludeCached         bool        `json:"include_cached"`
	Filters               RuleFilters `json:"filters"`
	DeletionPolicy        string      `json:"deletion_policy"`
	DestinationReadOnly   bool        `json:"destination_read_only"`
	CreateDestinationFeed bool        `json:"create_destination_feed"`
	AdoptExisting         bool        `json:"adopt_existing"`
	MaxLagSeconds         int         `json:"max_lag_seconds"`
	Revision              int64       `json:"revision"`
}

// TopologyBody is the editable content of a topology document.
type TopologyBody struct {
	Feeds []TopologyFeed    `json:"feeds"`
	Rules []ReplicationRule `json:"rules"`
}

// TopologyDocument is the signed topology document.
type TopologyDocument struct {
	Kind           string `json:"kind"`
	FederationID   string `json:"federation_id"`
	FederationName string `json:"federation_name"`
	Generation     int64  `json:"generation"`
	AuthorityEpoch int64  `json:"authority_epoch"`
	PrevHash       string `json:"prev_hash"`
	TopologyBody
	IssuedAt  string     `json:"issued_at"`
	IssuedBy  string     `json:"issued_by"`
	Signature *Signature `json:"signature,omitempty"`
}

// Normalize replaces nil collections so the signed form is stable.
func (b *TopologyBody) Normalize() {
	if b.Feeds == nil {
		b.Feeds = []TopologyFeed{}
	}
	if b.Rules == nil {
		b.Rules = []ReplicationRule{}
	}
	sort.Slice(b.Feeds, func(i, j int) bool { return b.Feeds[i].Key < b.Feeds[j].Key })
	sort.Slice(b.Rules, func(i, j int) bool { return b.Rules[i].ID < b.Rules[j].ID })
	for i := range b.Rules {
		r := &b.Rules[i]
		if len(r.Targets.Labels) == 0 {
			r.Targets.Labels = nil
		}
		if len(r.Targets.Members) == 0 {
			r.Targets.Members = nil
		}
		sort.Strings(r.Targets.Members)
	}
}

// Feed returns a feed key entry.
func (b *TopologyBody) Feed(key string) (*TopologyFeed, bool) {
	for i := range b.Feeds {
		if b.Feeds[i].Key == key {
			return &b.Feeds[i], true
		}
	}
	return nil, false
}

// Rule returns a rule by ID.
func (b *TopologyBody) Rule(id string) (*ReplicationRule, bool) {
	for i := range b.Rules {
		if b.Rules[i].ID == id {
			return &b.Rules[i], true
		}
	}
	return nil, false
}

// Targets reports whether a member (with its labels) is targeted by the rule.
func (r *ReplicationRule) Selects(member string, labels map[string]string) bool {
	if len(r.Targets.Members) > 0 {
		for _, m := range r.Targets.Members {
			if m == member {
				return true
			}
		}
		return false
	}
	if len(r.Targets.Labels) == 0 {
		return false
	}
	for k, v := range r.Targets.Labels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Matches reports whether the rule targets a member of the membership.
func (r *ReplicationRule) Matches(m Member) bool {
	return m.Status == "active" && r.Selects(m.InstanceID, m.Labels)
}

var formatPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validFilterList(list []string, nonEmpty bool) bool {
	if len(list) > 256 {
		return false
	}
	for _, v := range list {
		if len(v) > 256 || strings.ContainsAny(v, "\x00\r\n") || (nonEmpty && v == "") {
			return false
		}
	}
	return true
}

// ValidateTopology checks a topology body against the federation name and the
// applied membership and returns a client-safe message for the first problem.
func ValidateTopology(federationName string, b *TopologyBody, membership *Membership) error {
	bad := func(format string, args ...any) error { return invalid(fmt.Sprintf(format, args...)) }
	if len(b.Feeds) > 1024 || len(b.Rules) > 4096 {
		return bad("topology exceeds size limits")
	}
	member := func(id string) (Member, bool) {
		if membership == nil {
			return Member{}, false
		}
		return membership.Member(id)
	}
	feeds := map[string]*TopologyFeed{}
	for i := range b.Feeds {
		f := &b.Feeds[i]
		if !ValidFeedKey(federationName, f.Key) || feeds[f.Key] != nil {
			return bad("feed key %q must be %s/<feed-slug> and unique (one home per feed key)", f.Key, federationName)
		}
		feeds[f.Key] = f
		if !uuidPattern.MatchString(f.Home) {
			return bad("feed %s: home must be a member instance ID", f.Key)
		}
		if _, ok := member(f.Home); !ok {
			return bad("feed %s: home %s is not a member", f.Key, f.Home)
		}
		if !formatPattern.MatchString(f.Descriptor.Format) {
			return bad("feed %s: descriptor format is required", f.Key)
		}
		if f.Revision < 1 {
			return bad("feed %s: revision must be positive", f.Key)
		}
		if t := f.Transfer; t != nil {
			if !uuidPattern.MatchString(t.ID) || !uuidPattern.MatchString(t.From) || !uuidPattern.MatchString(t.To) || t.From == t.To {
				return bad("feed %s: transfer needs an ID and distinct from/to members", f.Key)
			}
			if t.Mode != "planned" && t.Mode != "forced" {
				return bad("feed %s: transfer mode must be planned or forced", f.Key)
			}
			switch t.State {
			case TransferFreeze, TransferDrain:
				if t.Mode != "planned" {
					return bad("feed %s: only planned transfers freeze and drain", f.Key)
				}
				fallthrough
			case TransferPromote:
				if f.Home != t.From {
					return bad("feed %s: an active transfer must start from the current home", f.Key)
				}
			case TransferComplete:
				if f.Home != t.To {
					return bad("feed %s: a completed transfer must name the new home", f.Key)
				}
			case TransferAborted:
				if f.Home != t.From {
					return bad("feed %s: an aborted transfer keeps the old home", f.Key)
				}
			default:
				return bad("feed %s: invalid transfer state %q", f.Key, t.State)
			}
			if t.Head < 0 || t.Checkpoint < 0 || t.Baseline < 0 || len(t.Reason) > 512 {
				return bad("feed %s: invalid transfer counters", f.Key)
			}
			for _, ts := range []string{t.StartedAt, t.UpdatedAt} {
				if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
					return bad("feed %s: transfer timestamps must be RFC 3339", f.Key)
				}
			}
			if t.Deadline != "" {
				if _, err := time.Parse(time.RFC3339Nano, t.Deadline); err != nil {
					return bad("feed %s: transfer deadline must be RFC 3339", f.Key)
				}
			}
		}
	}
	rules := map[string]bool{}
	for _, r := range b.Rules {
		if !uuidPattern.MatchString(r.ID) || rules[r.ID] {
			return bad("rule id %q must be a unique UUID", r.ID)
		}
		rules[r.ID] = true
		f := feeds[r.FeedKey]
		if f == nil {
			return bad("rule %s: feed key %q is not in the topology", r.ID, r.FeedKey)
		}
		if len(r.Targets.Members) == 0 && len(r.Targets.Labels) == 0 {
			return bad("rule %s: targets need labels or members", r.ID)
		}
		if len(r.Targets.Members) > 0 && len(r.Targets.Labels) > 0 {
			return bad("rule %s: targets use labels or members, not both", r.ID)
		}
		if len(r.Targets.Members) > 256 || !ValidLabels(r.Targets.Labels) {
			return bad("rule %s: invalid targets", r.ID)
		}
		seen := map[string]bool{}
		for _, m := range r.Targets.Members {
			if !uuidPattern.MatchString(m) || seen[m] {
				return bad("rule %s: target members must be unique instance IDs", r.ID)
			}
			seen[m] = true
			if _, ok := member(m); !ok {
				return bad("rule %s: target %s is not a member", r.ID, m)
			}
		}
		if r.Mode != "continuous" && r.Mode != "scheduled" && r.Mode != "manual" {
			return bad("rule %s: mode must be continuous, scheduled or manual", r.ID)
		}
		if r.IntervalSeconds < 1 || r.IntervalSeconds > 604800 {
			return bad("rule %s: interval_seconds must be 1..604800", r.ID)
		}
		if r.DeletionPolicy != "ignore" && r.DeletionPolicy != "propagate" {
			return bad("rule %s: deletion_policy must be ignore or propagate", r.ID)
		}
		if r.MaxLagSeconds < 0 || r.MaxLagSeconds > 31536000 {
			return bad("rule %s: max_lag_seconds out of range", r.ID)
		}
		if !validFilterList(r.Filters.Namespaces, false) || !validFilterList(r.Filters.Packages, true) || !validFilterList(r.Filters.VersionPrefixes, true) {
			return bad("rule %s: invalid filters", r.ID)
		}
		if r.Revision < 1 {
			return bad("rule %s: revision must be positive", r.ID)
		}
	}
	return nil
}

// --- envelope ---

func topologySigningPayload(d *TopologyDocument) ([]byte, error) {
	c := *d
	c.Signature = nil
	return CanonicalJSON(&c)
}

// TopologyHash is the chain hash of a signed topology document.
func TopologyHash(d *TopologyDocument) string {
	b, err := CanonicalJSON(d)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SignTopology sets the authority signature.
func SignTopology(d *TopologyDocument, authority ed25519.PrivateKey) error {
	payload, err := topologySigningPayload(d)
	if err != nil {
		return err
	}
	d.Signature = &Signature{KeyID: fmt.Sprintf("authority-epoch-%d", d.AuthorityEpoch), Alg: "Ed25519", Value: sign(authority, payload)}
	return nil
}

// VerifyTopology checks a topology document against the applied topology
// (nil when none) and the applied membership, exactly like VerifyPolicy.
// Structural validation does not require every referenced member to still be
// listed (a removed member stays in the membership with status removed).
func VerifyTopology(doc, applied *TopologyDocument, membership *Membership) error {
	if membership == nil {
		return ErrEpochUnknown
	}
	if doc.Kind != "topology" || !uuidPattern.MatchString(doc.FederationID) || doc.Generation < 1 || doc.AuthorityEpoch < 1 ||
		doc.Signature == nil || doc.Signature.Alg != "Ed25519" || doc.Signature.KeyID != fmt.Sprintf("authority-epoch-%d", doc.AuthorityEpoch) ||
		(doc.Generation == 1) != (doc.PrevHash == "") {
		return ErrMalformed
	}
	if _, err := time.Parse(time.RFC3339Nano, doc.IssuedAt); err != nil {
		return ErrMalformed
	}
	if doc.FederationID != membership.FederationID || doc.FederationName != membership.FederationName {
		return ErrWrongFed
	}
	if ValidateTopology(doc.FederationName, &doc.TopologyBody, membership) != nil {
		return ErrMalformed
	}
	if applied != nil {
		if doc.AuthorityEpoch < applied.AuthorityEpoch {
			return ErrStaleEpoch
		}
		if doc.Generation <= applied.Generation {
			return ErrStaleGen
		}
	} else if doc.Generation != 1 {
		return ErrGap
	}
	switch {
	case doc.AuthorityEpoch < membership.AuthorityEpoch:
		return ErrStaleEpoch
	case doc.AuthorityEpoch > membership.AuthorityEpoch:
		return ErrEpochUnknown
	}
	payload, err := topologySigningPayload(doc)
	if err != nil {
		return ErrMalformed
	}
	if !verify(membership.Authority.PublicKey, doc.Signature.Value, payload) {
		return ErrSignature
	}
	if applied != nil {
		if doc.Generation != applied.Generation+1 {
			return ErrGap
		}
		if doc.PrevHash != TopologyHash(applied) {
			return ErrChain
		}
	}
	return nil
}
