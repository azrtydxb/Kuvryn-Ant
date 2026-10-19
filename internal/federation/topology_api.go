package federation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/jackc/pgx/v5"
)

// Coordinated replication errors.
var (
	ErrFeedHomeElsewhere = errors.New("federation: the feed key already has a home")
	ErrTransferActive    = errors.New("federation: a home transfer is in progress for this feed key")
	ErrTransferState     = errors.New("federation: the transfer cannot change in its current state")
	ErrReplicationOff    = errors.New("federation: replication is not serving on this instance")
)

// ReplicationHooks connect federation to the replication backend
// (internal/replication), which owns jobs, peers and replica sources.
type ReplicationHooks struct {
	// Reconcile applies the applied topology to local replication: rule jobs,
	// adoption, release, re-pointing, promotion and demotion.
	Reconcile func(ctx context.Context) error
	// AdoptTx switches an existing local job to federation management.
	AdoptTx func(ctx context.Context, tx pgx.Tx, actor, jobID, ruleID string) (AdoptResult, error)
}

// AdoptResult describes an adopted job; checkpoint and phase are unchanged.
type AdoptResult struct {
	JobID          string `json:"job_id"`
	RuleID         string `json:"rule_id"`
	FeedKey        string `json:"feed_key"`
	Phase          string `json:"phase"`
	Checkpoint     int64  `json:"checkpoint"`
	PreviousPeerID string `json:"previous_peer_id"`
	PeerID         string `json:"peer_id"`
}

// SetReplication wires the replication backend.
func (s *Service) SetReplication(h *ReplicationHooks) { s.replication.Store(h) }

// MountPeer adds a handler to the federation listener (before Serve). Every
// request reaching it is already authenticated as an active member.
func (s *Service) MountPeer(pattern string, h http.Handler) {
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	s.peerRoutes[pattern] = h
}

// ReconcileReplication requests a reconcile pass (non-blocking).
func (s *Service) ReconcileReplication() {
	select {
	case s.reconcile <- struct{}{}:
	default:
	}
}

// ReconcileOnce runs the local replication reconciler and, on the authority,
// advances home transfers. It never affects serving.
func (s *Service) ReconcileOnce(ctx context.Context) error {
	var first error
	if h := s.replication.Load(); h != nil && h.Reconcile != nil {
		if err := h.Reconcile(ctx); err != nil {
			first = err
		}
	}
	if err := s.DriveTransfers(ctx); err != nil && first == nil {
		first = err
	}
	return first
}

func (s *Service) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		step, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := s.ReconcileOnce(step); err != nil && ctx.Err() == nil {
			s.logOnce("reconcile", "federation replication reconcile incomplete", "reason", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.reconcile:
		}
	}
}

// --- identity for replication transport ---

// ReplicationIdentity returns this member's client identity and the current
// federation URL of an active member (replication over mTLS).
func (s *Service) ReplicationIdentity(member string) (peer.Identity, string, error) {
	id, t, err := s.identity()
	if err != nil {
		return peer.Identity{}, "", err
	}
	if t.state.State != "active" {
		return peer.Identity{}, "", ErrNotConfigured
	}
	m, ok := t.doc.Member(member)
	if !ok || m.Status != "active" {
		return peer.Identity{}, "", ErrNotFound
	}
	return id, m.FederationURL, nil
}

// SelfID is this member's instance ID; Membership is the applied membership.
func (s *Service) Membership() *Membership { return s.trust.Load().doc }

// Topology returns the applied topology (nil when none) for the backend.
func (s *Service) Topology(ctx context.Context) (*TopologyDocument, error) {
	return s.appliedTopology(ctx)
}

// HomeFeed is a federated feed resolved on its home member.
type HomeFeed struct {
	FeedKey string `json:"feed_key"`
	FeedID  string `json:"feed_id"`
	Format  string `json:"format"`
	Head    int64  `json:"head"`
}

func (s *Service) localHomeFeed(ctx context.Context, key string) (HomeFeed, error) {
	topo, err := s.appliedTopology(ctx)
	if err != nil {
		return HomeFeed{}, err
	}
	if topo == nil {
		return HomeFeed{}, ErrNotFound
	}
	f, ok := topo.Feed(key)
	if !ok || f.Home != s.cfg.InstanceID {
		return HomeFeed{}, ErrNotFound
	}
	out := HomeFeed{FeedKey: key}
	err = s.cfg.DB.QueryRow(ctx, `SELECT b.feed_id::text,f.format FROM federated_feed_bindings b JOIN feeds f ON (f.instance_id,f.id)=(b.instance_id,b.feed_id) WHERE b.instance_id=$1 AND b.feed_key=$2`, s.cfg.InstanceID, key).Scan(&out.FeedID, &out.Format)
	if errors.Is(err, pgx.ErrNoRows) {
		return HomeFeed{}, ErrNotFound
	}
	if err != nil {
		return HomeFeed{}, err
	}
	out.Head, err = replicationHead(ctx, s.cfg.DB, s.cfg.InstanceID)
	return out, err
}

// ResolveHomeFeed asks a member for the local feed it holds as home of key.
func (s *Service) ResolveHomeFeed(ctx context.Context, member, key string) (HomeFeed, error) {
	if member == s.cfg.InstanceID {
		return s.localHomeFeed(ctx, key)
	}
	id, base, err := s.ReplicationIdentity(member)
	if err != nil {
		return HomeFeed{}, err
	}
	fed, slug, _ := strings.Cut(key, "/")
	raw, err := s.client.Do(ctx, id, base, "GET", "/federation/v1/replication/feeds/"+url.PathEscape(fed)+"/"+url.PathEscape(slug), nil, member)
	if err != nil {
		var pe *peer.ErrPeer
		if errors.As(err, &pe) && pe.Code == "not_found" {
			return HomeFeed{}, ErrNotFound
		}
		return HomeFeed{}, peerError(base, err)
	}
	var out HomeFeed
	if err := json.Unmarshal(raw, &out); err != nil || out.FeedKey != key || !uuidPattern.MatchString(out.FeedID) {
		return HomeFeed{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	return out, nil
}

// AuthorizeReplication decides whether a member may read a local feed through
// the federation export API: the feed must be bound to a key this instance is
// home of, and the caller a target of a rule for that key, or the other side
// of its active transfer. The applied topology is signed by the authority, so
// the decision needs no bearer token.
func (s *Service) AuthorizeReplication(ctx context.Context, caller, feed string) bool {
	topo, err := s.appliedTopology(ctx)
	if err != nil || topo == nil {
		return false
	}
	var key string
	if err := s.cfg.DB.QueryRow(ctx, `SELECT feed_key FROM federated_feed_bindings WHERE instance_id=$1 AND feed_id=$2`, s.cfg.InstanceID, feed).Scan(&key); err != nil {
		return false
	}
	return exportAllowed(&topo.TopologyBody, s.trust.Load().doc, s.cfg.InstanceID, caller, key)
}

// exportAllowed is the export decision for a feed key on member self. A
// transfer grants its two sides access only while it is active; an aborted
// transfer still recorded in topology grants nothing. The one exception is
// the former home of a completed transfer to this member: it demotes into a
// replica that follows this member (section 9.5) and held the content
// anyway.
func exportAllowed(topo *TopologyBody, doc *Membership, self, caller, key string) bool {
	f, ok := topo.Feed(key)
	if !ok || f.Home != self {
		return false
	}
	if t := f.Transfer; t != nil {
		switch {
		case t.Active() && (t.To == caller || t.From == caller):
			return true
		case t.State == TransferComplete && t.From == caller && t.To == self:
			return true
		}
	}
	if doc == nil {
		return false
	}
	m, ok := doc.Member(caller)
	if !ok {
		return false
	}
	for i := range topo.Rules {
		if r := &topo.Rules[i]; r.FeedKey == key && r.Matches(m) {
			return true
		}
	}
	return false
}

// --- views ---

// TopologyStatus summarizes the applied topology in federation status.
type TopologyStatus struct {
	Generation  int64      `json:"generation"`
	Hash        string     `json:"hash,omitempty"`
	AppliedAt   *time.Time `json:"applied_at,omitempty"`
	AgeSeconds  int64      `json:"age_seconds"`
	Error       string     `json:"error,omitempty"`
	Feeds       int        `json:"feeds"`
	Rules       int        `json:"rules"`
	HomeFeeds   int        `json:"home_feeds"`
	Targeted    int        `json:"rules_targeting_self"`
	Transfers   int        `json:"active_transfers"`
	Quarantines int        `json:"unresolved_quarantines"`
}

// TopologyFeedView is a feed key with its local role on this member.
type TopologyFeedView struct {
	TopologyFeed
	HomeName    string `json:"home_name"`
	LocalFeedID string `json:"local_feed_id,omitempty"`
	LocalRole   string `json:"local_role"` // home | replica | unbound
	Frozen      bool   `json:"frozen"`
}

// TopologyView is GET /api/v1/federation/topology.
type TopologyView struct {
	Generation int64              `json:"generation"`
	Hash       string             `json:"hash,omitempty"`
	AppliedAt  *time.Time         `json:"applied_at,omitempty"`
	Error      string             `json:"error,omitempty"`
	Authority  bool               `json:"authority"`
	Feeds      []TopologyFeedView `json:"feeds"`
	Rules      []ReplicationRule  `json:"rules"`
}

func (s *Service) topologyStatus(ctx context.Context, st State) (*TopologyStatus, error) {
	ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	out := &TopologyStatus{Generation: ts.Generation, Hash: ts.Hash, AppliedAt: ts.AppliedAt, Error: ts.Error}
	if ts.Generation > 0 {
		fresh := *ts.AppliedAt
		if ts.ConfirmedAt != nil && ts.ConfirmedAt.After(fresh) {
			fresh = *ts.ConfirmedAt
		}
		if st.Role == "authority" {
			fresh = s.now()
		}
		out.AgeSeconds = max(0, int64(s.now().Sub(fresh)/time.Second))
		topo, err := loadTopology(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, ts.Generation)
		if err != nil {
			return nil, err
		}
		out.Feeds, out.Rules = len(topo.Feeds), len(topo.Rules)
		self, _ := s.trust.Load().doc.Member(s.cfg.InstanceID)
		for _, f := range topo.Feeds {
			if f.Home == s.cfg.InstanceID {
				out.HomeFeeds++
			}
			if f.Transfer.Active() {
				out.Transfers++
			}
		}
		for i := range topo.Rules {
			if r := &topo.Rules[i]; r.Matches(self) {
				if f, ok := topo.Feed(r.FeedKey); ok && f.Home != s.cfg.InstanceID {
					out.Targeted++
				}
			}
		}
	}
	if err := s.cfg.DB.QueryRow(ctx, `SELECT count(*) FROM federation_quarantines WHERE instance_id=$1 AND resolved_at IS NULL`, s.cfg.InstanceID).Scan(&out.Quarantines); err != nil {
		return nil, err
	}
	s.topologyGauge.Store(out)
	return out, nil
}

// TopologyView returns the applied topology with local roles.
func (s *Service) TopologyView(ctx context.Context) (TopologyView, error) {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return TopologyView{}, err
	}
	if st.State != "active" || st.FederationID == "" {
		return TopologyView{}, ErrNotConfigured
	}
	ts, err := loadTopologyState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil {
		return TopologyView{}, err
	}
	out := TopologyView{Generation: ts.Generation, Hash: ts.Hash, AppliedAt: ts.AppliedAt, Error: ts.Error, Authority: st.Role == "authority", Feeds: []TopologyFeedView{}, Rules: []ReplicationRule{}}
	if ts.Generation == 0 {
		return out, nil
	}
	topo, err := loadTopology(ctx, s.cfg.DB, s.cfg.InstanceID, st.FederationID, ts.Generation)
	if err != nil {
		return TopologyView{}, err
	}
	doc := s.trust.Load().doc
	for _, f := range topo.Feeds {
		v := TopologyFeedView{TopologyFeed: f, LocalRole: "unbound"}
		if m, ok := doc.Member(f.Home); ok {
			v.HomeName = m.Name
		}
		if v.LocalFeedID, err = boundFeed(ctx, s.cfg.DB, s.cfg.InstanceID, f.Key); err != nil {
			return TopologyView{}, err
		}
		if v.LocalFeedID != "" {
			v.LocalRole = "replica"
			if f.Home == s.cfg.InstanceID {
				v.LocalRole = "home"
			}
			if err := s.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_feed_freezes WHERE instance_id=$1 AND feed_id=$2)`, s.cfg.InstanceID, v.LocalFeedID).Scan(&v.Frozen); err != nil {
				return TopologyView{}, err
			}
		}
		out.Feeds = append(out.Feeds, v)
	}
	out.Rules = append(out.Rules, topo.Rules...)
	return out, nil
}

// --- authority mutations ---

// FeedInput publishes a feed key with its home member.
type FeedInput struct {
	FeedKey      string          `json:"feed_key"`
	HomeMemberID string          `json:"home_member_id"`
	Descriptor   *FeedDescriptor `json:"descriptor"`
	// Routing is optional; nil means proxy routing (D5).
	Routing *FeedRouting `json:"routing"`
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CreateFeedTx adds a feed key with one home member (a second claim of the
// same key is rejected).
func (s *Service) CreateFeedTx(ctx context.Context, tx pgx.Tx, actor string, in FeedInput) (*TopologyDocument, TopologyFeed, error) {
	var out TopologyFeed
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, m *Membership) error {
		if !ValidFeedKey(m.FederationName, in.FeedKey) {
			return invalid("feed_key must be " + m.FederationName + "/<feed-slug>")
		}
		if _, ok := b.Feed(in.FeedKey); ok {
			return ErrFeedHomeElsewhere
		}
		home, ok := m.Member(in.HomeMemberID)
		if !ok || home.Status != "active" {
			return invalid("home_member_id must be an active member")
		}
		d := FeedDescriptor{}
		if in.Descriptor != nil {
			d = *in.Descriptor
		}
		if d.Format == "" && home.InstanceID == s.cfg.InstanceID {
			// Seed the descriptor from the authority's own bound feed.
			_ = tx.QueryRow(ctx, `SELECT f.format,f.anonymous_read FROM federated_feed_bindings b JOIN feeds f ON (f.instance_id,f.id)=(b.instance_id,b.feed_id) WHERE b.instance_id=$1 AND b.feed_key=$2`,
				s.cfg.InstanceID, in.FeedKey).Scan(&d.Format, &d.AnonymousRead)
		}
		out = TopologyFeed{Key: in.FeedKey, Home: home.InstanceID, Descriptor: d, Revision: 1, Routing: in.Routing}
		b.Feeds = append(b.Feeds, out)
		return nil
	})
	if err != nil {
		return nil, out, err
	}
	return doc, out, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.feed.created", "", "success", map[string]any{"feed_key": in.FeedKey, "home": in.HomeMemberID, "generation": doc.Generation})
}

// DeleteFeedTx removes a feed key that no rule references and that has no
// active transfer. Local content on every member is untouched.
func (s *Service) DeleteFeedTx(ctx context.Context, tx pgx.Tx, actor, key string) (*TopologyDocument, error) {
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, _ *Membership) error {
		f, ok := b.Feed(key)
		if !ok {
			return ErrNotFound
		}
		if f.Transfer.Active() {
			return ErrTransferActive
		}
		for _, r := range b.Rules {
			if r.FeedKey == key {
				return invalid("remove the replication rules of " + key + " first")
			}
		}
		feeds := b.Feeds[:0]
		for _, x := range b.Feeds {
			if x.Key != key {
				feeds = append(feeds, x)
			}
		}
		b.Feeds = feeds
		return nil
	})
	if err != nil {
		return nil, err
	}
	return doc, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.feed.deleted", "", "success", map[string]any{"feed_key": key, "generation": doc.Generation})
}

// RuleInput creates a replication rule. Unset options take the defaults:
// continuous, 300 s, include cached, ignore deletions, read-only destination,
// create the destination feed from the descriptor, adopt matching local jobs,
// 900 s lag budget.
type RuleInput struct {
	FeedKey               string       `json:"feed_key"`
	Targets               RuleTargets  `json:"targets"`
	Mode                  string       `json:"mode"`
	IntervalSeconds       *int         `json:"interval_seconds"`
	IncludeCached         *bool        `json:"include_cached"`
	Filters               *RuleFilters `json:"filters"`
	DeletionPolicy        string       `json:"deletion_policy"`
	DestinationReadOnly   *bool        `json:"destination_read_only"`
	CreateDestinationFeed *bool        `json:"create_destination_feed"`
	AdoptExisting         *bool        `json:"adopt_existing"`
	MaxLagSeconds         *int         `json:"max_lag_seconds"`
}

// RulePatch changes a rule at an exact revision.
type RulePatch struct {
	Revision              int64        `json:"revision"`
	Targets               *RuleTargets `json:"targets"`
	Mode                  *string      `json:"mode"`
	IntervalSeconds       *int         `json:"interval_seconds"`
	IncludeCached         *bool        `json:"include_cached"`
	Filters               *RuleFilters `json:"filters"`
	DeletionPolicy        *string      `json:"deletion_policy"`
	DestinationReadOnly   *bool        `json:"destination_read_only"`
	CreateDestinationFeed *bool        `json:"create_destination_feed"`
	AdoptExisting         *bool        `json:"adopt_existing"`
	MaxLagSeconds         *int         `json:"max_lag_seconds"`
}

func orBool(p *bool, d bool) bool {
	if p == nil {
		return d
	}
	return *p
}

func orInt(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}

// CreateRuleTx adds a replication rule.
func (s *Service) CreateRuleTx(ctx context.Context, tx pgx.Tx, actor string, in RuleInput) (*TopologyDocument, ReplicationRule, error) {
	r := ReplicationRule{ID: newUUID(), FeedKey: in.FeedKey, Targets: in.Targets, Mode: in.Mode, IntervalSeconds: orInt(in.IntervalSeconds, 300),
		IncludeCached: orBool(in.IncludeCached, true), DeletionPolicy: in.DeletionPolicy, DestinationReadOnly: orBool(in.DestinationReadOnly, true),
		CreateDestinationFeed: orBool(in.CreateDestinationFeed, true), AdoptExisting: orBool(in.AdoptExisting, true), MaxLagSeconds: orInt(in.MaxLagSeconds, 900), Revision: 1}
	if r.Mode == "" {
		r.Mode = "continuous"
	}
	if r.DeletionPolicy == "" {
		r.DeletionPolicy = "ignore"
	}
	if in.Filters != nil {
		r.Filters = *in.Filters
	}
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, _ *Membership) error {
		if err := rejectHomeTarget(b, &r); err != nil {
			return err
		}
		b.Rules = append(b.Rules, r)
		return nil
	})
	if err != nil {
		return nil, r, err
	}
	return doc, r, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.replication.rule_created", r.ID, "success", map[string]any{"feed_key": r.FeedKey, "generation": doc.Generation})
}

// UpdateRuleTx patches a rule; target members apply the change on their jobs.
func (s *Service) UpdateRuleTx(ctx context.Context, tx pgx.Tx, actor, id string, in RulePatch) (*TopologyDocument, ReplicationRule, error) {
	var out ReplicationRule
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, _ *Membership) error {
		r, ok := b.Rule(id)
		if !ok {
			return ErrNotFound
		}
		if in.Revision != r.Revision {
			return ErrRevisionMismatch
		}
		if in.Targets != nil {
			r.Targets = *in.Targets
		}
		if in.Mode != nil {
			r.Mode = *in.Mode
		}
		r.IntervalSeconds = orInt(in.IntervalSeconds, r.IntervalSeconds)
		r.IncludeCached = orBool(in.IncludeCached, r.IncludeCached)
		if in.Filters != nil {
			r.Filters = *in.Filters
		}
		if in.DeletionPolicy != nil {
			r.DeletionPolicy = *in.DeletionPolicy
		}
		r.DestinationReadOnly = orBool(in.DestinationReadOnly, r.DestinationReadOnly)
		r.CreateDestinationFeed = orBool(in.CreateDestinationFeed, r.CreateDestinationFeed)
		r.AdoptExisting = orBool(in.AdoptExisting, r.AdoptExisting)
		r.MaxLagSeconds = orInt(in.MaxLagSeconds, r.MaxLagSeconds)
		r.Revision++
		out = *r
		if in.Targets != nil {
			return rejectHomeTarget(b, r)
		}
		return nil
	})
	if err != nil {
		return nil, out, err
	}
	return doc, out, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.replication.rule_updated", id, "success", map[string]any{"feed_key": out.FeedKey, "revision": out.Revision, "generation": doc.Generation})
}

// DeleteRuleTx removes a rule. Targets disable and release the job, keeping
// content and the replica source (an offline copy is never destroyed).
func (s *Service) DeleteRuleTx(ctx context.Context, tx pgx.Tx, actor, id string) (*TopologyDocument, error) {
	var key string
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, _ *Membership) error {
		r, ok := b.Rule(id)
		if !ok {
			return ErrNotFound
		}
		key = r.FeedKey
		rules := b.Rules[:0]
		for _, x := range b.Rules {
			if x.ID != id {
				rules = append(rules, x)
			}
		}
		b.Rules = rules
		return nil
	})
	if err != nil {
		return nil, err
	}
	return doc, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.replication.rule_deleted", id, "success", map[string]any{"feed_key": key, "generation": doc.Generation})
}

// Rules returns the applied replication rules.
func (s *Service) Rules(ctx context.Context) ([]ReplicationRule, error) {
	v, err := s.TopologyView(ctx)
	if err != nil {
		return nil, err
	}
	return v.Rules, nil
}

// TransferInput starts a home transfer.
type TransferInput struct {
	ToMemberID          string `json:"to_member_id"`
	Mode                string `json:"mode"`
	Confirm             bool   `json:"confirm"`
	AcceptDataLoss      bool   `json:"accept_data_loss"`
	DrainTimeoutSeconds int    `json:"drain_timeout_seconds"`
}

// StartTransferTx starts a planned or forced home transfer (§9.5, D6).
// Safety rules: one transfer per key at a time; the target is an active
// member other than the home that a rule for the key targets (it holds a
// replica); planned transfers freeze the home, wait for the target to drain to
// the frozen head and abort on timeout; forced transfers skip freeze and
// drain, require accept_data_loss and quarantine on the old home whatever it
// accepted after the target's checkpoint.
func (s *Service) StartTransferTx(ctx context.Context, tx pgx.Tx, actor, key string, in TransferInput) (*TopologyDocument, FeedTransfer, error) {
	var out FeedTransfer
	if !in.Confirm {
		return nil, out, invalid("a home transfer requires confirm: true")
	}
	if in.Mode == "" {
		in.Mode = "planned"
	}
	if in.Mode != "planned" && in.Mode != "forced" {
		return nil, out, invalid("mode must be planned or forced")
	}
	if in.Mode == "forced" && !in.AcceptDataLoss {
		return nil, out, invalid("a forced transfer skips the drain and can lose writes the target never received: set accept_data_loss: true")
	}
	if in.DrainTimeoutSeconds == 0 {
		in.DrainTimeoutSeconds = 900
	}
	if in.DrainTimeoutSeconds < 10 || in.DrainTimeoutSeconds > 86400 {
		return nil, out, invalid("drain_timeout_seconds must be 10..86400")
	}
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, m *Membership) error {
		f, ok := b.Feed(key)
		if !ok {
			return ErrNotFound
		}
		if f.Transfer.Active() {
			return ErrTransferActive
		}
		to, ok := m.Member(in.ToMemberID)
		if !ok || to.Status != "active" || to.InstanceID == f.Home {
			return invalid("to_member_id must be an active member other than the current home")
		}
		targeted := false
		for i := range b.Rules {
			if r := &b.Rules[i]; r.FeedKey == key && r.Matches(to) {
				targeted = true
			}
		}
		if !targeted {
			return invalid("the target must hold a replica: no replication rule for " + key + " targets it")
		}
		if from, ok := m.Member(f.Home); in.Mode == "planned" && (!ok || from.Status != "active") {
			return invalid("the current home is not active; only a forced transfer is possible")
		}
		now := s.now()
		out = FeedTransfer{ID: newUUID(), From: f.Home, To: to.InstanceID, Mode: in.Mode, State: TransferFreeze, StartedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}
		if in.Mode == "forced" {
			out.State = TransferPromote
		} else {
			out.Deadline = now.Add(time.Duration(in.DrainTimeoutSeconds) * time.Second).Format(time.RFC3339Nano)
		}
		f.Transfer = &out
		f.Revision++
		return nil
	})
	if err != nil {
		return nil, out, err
	}
	return doc, out, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.transfer.started", out.ID, "success",
		map[string]any{"feed_key": key, "from": out.From, "to": out.To, "mode": out.Mode, "generation": doc.Generation})
}

// AbortTransferTx aborts a planned transfer before promotion; the old home
// unfreezes when it applies the generation.
func (s *Service) AbortTransferTx(ctx context.Context, tx pgx.Tx, actor, key, reason string) (*TopologyDocument, FeedTransfer, error) {
	var out FeedTransfer
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, _ *Membership) error {
		f, ok := b.Feed(key)
		if !ok || f.Transfer == nil {
			return ErrNotFound
		}
		if f.Transfer.State != TransferFreeze && f.Transfer.State != TransferDrain {
			return ErrTransferState
		}
		f.Transfer.State = TransferAborted
		f.Transfer.Reason = truncate(reason, 512)
		f.Transfer.UpdatedAt = s.now().Format(time.RFC3339Nano)
		f.Revision++
		out = *f.Transfer
		return nil
	})
	if err != nil {
		return nil, out, err
	}
	return doc, out, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.transfer.aborted", out.ID, "success", map[string]any{"feed_key": key, "reason": out.Reason, "generation": doc.Generation})
}

// --- transfer driver (authority) ---

// TransferState is one member's local view of a transfer step.
type TransferState struct {
	FeedKey            string `json:"feed_key"`
	TransferID         string `json:"transfer_id"`
	Frozen             bool   `json:"frozen"`
	Head               int64  `json:"head"`
	JobPhase           string `json:"job_phase,omitempty"`
	JobPeerMember      string `json:"job_peer_member,omitempty"`
	Checkpoint         int64  `json:"checkpoint"`
	Promoted           bool   `json:"promoted"`
	PromotedCheckpoint int64  `json:"promoted_checkpoint"`
	Baseline           int64  `json:"baseline"`
	Demoted            bool   `json:"demoted"`
}

func (s *Service) localTransferState(ctx context.Context, key, transfer string) (TransferState, error) {
	out := TransferState{FeedKey: key, TransferID: transfer}
	steps, err := s.cfg.DB.Query(ctx, `SELECT step,detail FROM federation_transfer_steps WHERE instance_id=$1 AND transfer_id=$2`, s.cfg.InstanceID, transfer)
	if err != nil {
		return out, err
	}
	for steps.Next() {
		var step string
		var raw []byte
		if err := steps.Scan(&step, &raw); err != nil {
			steps.Close()
			return out, err
		}
		var d struct {
			Head       int64 `json:"head"`
			Checkpoint int64 `json:"checkpoint"`
			Baseline   int64 `json:"baseline"`
		}
		_ = json.Unmarshal(raw, &d)
		switch step {
		case "freeze":
			out.Head = d.Head
		case "promote":
			out.Promoted, out.PromotedCheckpoint, out.Baseline = true, d.Checkpoint, d.Baseline
		case "demote":
			out.Demoted = true
		}
	}
	steps.Close()
	if err := steps.Err(); err != nil {
		return out, err
	}
	if err := s.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_feed_freezes WHERE instance_id=$1 AND transfer_id=$2 AND reason='transfer')`, s.cfg.InstanceID, transfer).Scan(&out.Frozen); err != nil {
		return out, err
	}
	var member *string
	err = s.cfg.DB.QueryRow(ctx, `SELECT j.phase,j.checkpoint,p.member_instance_id::text FROM replication_jobs j JOIN replication_peers p ON (p.instance_id,p.id)=(j.instance_id,j.peer_id)
WHERE j.instance_id=$1 AND j.origin='federation' AND j.federation_feed_key=$2 ORDER BY (j.federation_rule_id IS NULL),j.created_at LIMIT 1`, s.cfg.InstanceID, key).Scan(&out.JobPhase, &out.Checkpoint, &member)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if member != nil {
		out.JobPeerMember = *member
	}
	return out, nil
}

func (s *Service) transferState(ctx context.Context, member, key, transfer string) (TransferState, error) {
	if member == s.cfg.InstanceID {
		return s.localTransferState(ctx, key, transfer)
	}
	id, base, err := s.ReplicationIdentity(member)
	if err != nil {
		return TransferState{}, err
	}
	raw, err := s.client.Do(ctx, id, base, "GET", "/federation/v1/replication/transfer-state?feed_key="+url.QueryEscape(key)+"&transfer="+url.QueryEscape(transfer), nil, member)
	if err != nil {
		return TransferState{}, peerError(base, err)
	}
	var out TransferState
	if err := json.Unmarshal(raw, &out); err != nil || out.TransferID != transfer {
		return TransferState{}, &RejectedError{Reason: "malformed", Err: ErrMalformed}
	}
	return out, nil
}

// DriveTransfers advances active home transfers on the authority. Each step
// waits for the member that has to act (old home freezes, target drains and
// promotes) and publishes the next state; planned transfers abort when their
// deadline passes before promotion.
func (s *Service) DriveTransfers(ctx context.Context) error {
	st, err := loadState(ctx, s.cfg.DB, s.cfg.InstanceID)
	if err != nil || st.State != "active" || st.Role != "authority" {
		return err
	}
	topo, err := s.appliedTopology(ctx)
	if err != nil || topo == nil {
		return err
	}
	var first error
	for _, f := range topo.Feeds {
		t := f.Transfer
		if !t.Active() {
			continue
		}
		if err := s.driveTransfer(ctx, f.Key, *t); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Service) driveTransfer(ctx context.Context, key string, t FeedTransfer) error {
	expired := false
	if t.Deadline != "" {
		if d, err := time.Parse(time.RFC3339Nano, t.Deadline); err == nil && s.now().After(d) {
			expired = true
		}
	}
	var next func(f *TopologyFeed)
	switch t.State {
	case TransferFreeze:
		ls, err := s.transferState(ctx, t.From, key, t.ID)
		switch {
		case err == nil && ls.Frozen:
			next = func(f *TopologyFeed) { f.Transfer.State, f.Transfer.Head = TransferDrain, ls.Head }
		case expired:
			next = abortStep("freeze timed out: the home did not confirm the freeze")
		default:
			return err
		}
	case TransferDrain:
		ls, err := s.transferState(ctx, t.To, key, t.ID)
		switch {
		case err == nil && ls.JobPhase == "incremental" && ls.JobPeerMember == t.From && ls.Checkpoint >= t.Head:
			next = func(f *TopologyFeed) { f.Transfer.State = TransferPromote }
		case expired:
			next = abortStep("drain timed out: the target did not reach the frozen head")
		default:
			return err
		}
	case TransferPromote:
		ls, err := s.transferState(ctx, t.To, key, t.ID)
		if err != nil || !ls.Promoted {
			return err
		}
		next = func(f *TopologyFeed) {
			f.Home = t.To
			f.Transfer.State, f.Transfer.Checkpoint, f.Transfer.Baseline = TransferComplete, ls.PromotedCheckpoint, ls.Baseline
		}
	default:
		return nil
	}
	var doc *TopologyDocument
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		doc, err = s.publishTopologyTx(ctx, tx, "transfer:"+t.ID, func(b *TopologyBody, _ *Membership) error {
			f, ok := b.Feed(key)
			if !ok || f.Transfer == nil || f.Transfer.ID != t.ID || f.Transfer.State != t.State {
				return ErrTransferState // raced with an operator action
			}
			next(f)
			f.Transfer.UpdatedAt = s.now().Format(time.RFC3339Nano)
			f.Revision++
			return nil
		})
		if err != nil {
			return err
		}
		return audit(ctx, tx, s.cfg.InstanceID, "", "federation.transfer.advanced", t.ID, "success", map[string]any{"feed_key": key, "from_state": t.State, "generation": doc.Generation})
	})
	if errors.Is(err, ErrTransferState) {
		return nil
	}
	if err != nil {
		return err
	}
	s.AfterTopologyPublish(ctx, doc)
	return nil
}

func abortStep(reason string) func(f *TopologyFeed) {
	return func(f *TopologyFeed) { f.Transfer.State, f.Transfer.Reason = TransferAborted, reason }
}

// --- rule status and compliance ---

// RuleStatus is the state of one rule on one target member.
type RuleStatus struct {
	RuleID              string     `json:"rule_id"`
	FeedKey             string     `json:"feed_key"`
	MemberID            string     `json:"member_id"`
	MemberName          string     `json:"member_name,omitempty"`
	State               string     `json:"state"` // in_sync | lagging | seeding | failed | pending | missing_feed | conflict | disabled | unknown
	Detail              string     `json:"detail,omitempty"`
	JobID               string     `json:"job_id,omitempty"`
	Adopted             bool       `json:"adopted"`
	Phase               string     `json:"phase,omitempty"`
	Checkpoint          int64      `json:"checkpoint"`
	ChangesBehind       *int64     `json:"changes_behind,omitempty"`
	LagSeconds          *float64   `json:"lag_seconds,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	UpdatedAt           *time.Time `json:"updated_at,omitempty"`
}

// LocalRuleStatuses reports every rule of the applied topology that targets
// this member (excluding feeds it is home of).
func (s *Service) LocalRuleStatuses(ctx context.Context) ([]RuleStatus, error) {
	out := []RuleStatus{}
	topo, err := s.appliedTopology(ctx)
	if err != nil || topo == nil {
		return out, err
	}
	doc := s.trust.Load().doc
	self, _ := doc.Member(s.cfg.InstanceID)
	for i := range topo.Rules {
		r := &topo.Rules[i]
		f, ok := topo.Feed(r.FeedKey)
		if !ok || !r.Matches(self) || f.Home == s.cfg.InstanceID {
			continue
		}
		rs := RuleStatus{RuleID: r.ID, FeedKey: r.FeedKey, MemberID: s.cfg.InstanceID, MemberName: self.Name, State: "pending"}
		var job *string
		var updated time.Time
		err := s.cfg.DB.QueryRow(ctx, `SELECT state,detail,job_id::text,updated_at FROM federation_rule_status WHERE instance_id=$1 AND rule_id=$2`, s.cfg.InstanceID, r.ID).Scan(&rs.State, &rs.Detail, &job, &updated)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			rs.Detail = "not reconciled yet"
		case err != nil:
			return nil, err
		default:
			rs.UpdatedAt = &updated
		}
		var enabled bool
		var remoteHead *int64
		var remoteHeadAt, checkpointAt, adopted *time.Time
		var created, now time.Time
		err = s.cfg.DB.QueryRow(ctx, `SELECT id::text,enabled,phase,checkpoint,remote_head,remote_head_at,checkpoint_change_at,last_success_at,consecutive_failures,adopted_at,created_at,clock_timestamp()
FROM replication_jobs WHERE instance_id=$1 AND federation_rule_id=$2`, s.cfg.InstanceID, r.ID).
			Scan(&rs.JobID, &enabled, &rs.Phase, &rs.Checkpoint, &remoteHead, &remoteHeadAt, &checkpointAt, &rs.LastSuccessAt, &rs.ConsecutiveFailures, &adopted, &created, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			out = append(out, rs)
			continue
		}
		if err != nil {
			return nil, err
		}
		rs.Adopted = adopted != nil
		switch {
		case !enabled:
			rs.State = "disabled"
		case rs.ConsecutiveFailures > 0:
			rs.State = "failed"
		case rs.Phase != "incremental":
			rs.State = "seeding"
		default:
			behind := int64(0)
			if remoteHead != nil && *remoteHead > rs.Checkpoint {
				behind = *remoteHead - rs.Checkpoint
			}
			lag := 0.0
			if behind > 0 && remoteHeadAt != nil {
				ref := created
				if checkpointAt != nil {
					ref = *checkpointAt
				}
				lag = max(0, remoteHeadAt.Sub(ref).Seconds())
			}
			rs.ChangesBehind, rs.LagSeconds = &behind, &lag
			rs.State = "in_sync"
			if behind > 0 && r.MaxLagSeconds > 0 && lag > float64(r.MaxLagSeconds) {
				rs.State = "lagging"
			}
		}
		if rs.ConsecutiveFailures > 0 && rs.LastSuccessAt != nil {
			stale := now.Sub(*rs.LastSuccessAt).Seconds()
			rs.LagSeconds = &stale
		}
		out = append(out, rs)
	}
	return out, nil
}

// RuleCompliance is a rule's state on every target member.
type RuleCompliance struct {
	Rule    ReplicationRule `json:"rule"`
	Home    string          `json:"home"`
	Members []RuleStatus    `json:"members"`
	Summary map[string]int  `json:"summary"`
}

// Compliance asks every target member of a rule for its local status;
// unreachable members are reported as unknown (never blocking).
func (s *Service) Compliance(ctx context.Context, ruleID string) (RuleCompliance, error) {
	topo, err := s.appliedTopology(ctx)
	if err != nil {
		return RuleCompliance{}, err
	}
	if topo == nil {
		return RuleCompliance{}, ErrNotFound
	}
	r, ok := topo.Rule(ruleID)
	if !ok {
		return RuleCompliance{}, ErrNotFound
	}
	f, _ := topo.Feed(r.FeedKey)
	out := RuleCompliance{Rule: *r, Home: f.Home, Members: []RuleStatus{}, Summary: map[string]int{}}
	doc := s.trust.Load().doc
	for _, m := range doc.Members {
		if !r.Matches(m) || m.InstanceID == f.Home {
			continue
		}
		var list []RuleStatus
		if m.InstanceID == s.cfg.InstanceID {
			list, err = s.LocalRuleStatuses(ctx)
		} else {
			list, err = s.peerRuleStatuses(ctx, m)
		}
		found := RuleStatus{RuleID: r.ID, FeedKey: r.FeedKey, MemberID: m.InstanceID, MemberName: m.Name, State: "unknown"}
		if err != nil {
			found.Detail = "member unreachable"
		} else {
			found.Detail = "rule not applied yet (topology generation behind)"
			for _, x := range list {
				if x.RuleID == r.ID {
					found = x
					found.MemberName = m.Name
				}
			}
		}
		out.Members = append(out.Members, found)
		out.Summary[found.State]++
	}
	sort.Slice(out.Members, func(i, j int) bool { return out.Members[i].MemberName < out.Members[j].MemberName })
	return out, nil
}

func (s *Service) peerRuleStatuses(ctx context.Context, m Member) ([]RuleStatus, error) {
	id, _, err := s.identity()
	if err != nil {
		return nil, err
	}
	raw, err := s.client.Do(ctx, id, m.FederationURL, "GET", "/federation/v1/replication/status", nil, m.InstanceID)
	if err != nil {
		return nil, err
	}
	var body struct {
		Items []RuleStatus `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, ErrMalformed
	}
	return body.Items, nil
}

// --- quarantines ---

// Quarantine is content an old home kept after a forced transfer.
type Quarantine struct {
	ID               string     `json:"id"`
	FeedKey          string     `json:"feed_key"`
	TransferID       string     `json:"transfer_id,omitempty"`
	FeedID           string     `json:"feed_id,omitempty"`
	SourceID         string     `json:"source_id"`
	SourceName       string     `json:"source_name"`
	DivergentChanges *int64     `json:"divergent_changes,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
}

// Quarantines lists quarantined sources on this member.
func (s *Service) Quarantines(ctx context.Context) ([]Quarantine, error) {
	rows, err := s.cfg.DB.Query(ctx, `SELECT q.id::text,q.feed_key,coalesce(q.transfer_id::text,''),coalesce(q.feed_id::text,''),q.source_id::text,coalesce(s.name,''),q.divergent_changes,q.created_at,q.resolved_at
FROM federation_quarantines q LEFT JOIN sources s ON (s.instance_id,s.id)=(q.instance_id,q.source_id) WHERE q.instance_id=$1 ORDER BY q.created_at DESC LIMIT 500`, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Quarantine{}
	for rows.Next() {
		var q Quarantine
		if err := rows.Scan(&q.ID, &q.FeedKey, &q.TransferID, &q.FeedID, &q.SourceID, &q.SourceName, &q.DivergentChanges, &q.CreatedAt, &q.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ResolveQuarantineTx marks a quarantine reviewed. The quarantined source and
// its content stay until the operator deletes them.
func (s *Service) ResolveQuarantineTx(ctx context.Context, tx pgx.Tx, actor, id string) (Quarantine, error) {
	tag, err := tx.Exec(ctx, `UPDATE federation_quarantines SET resolved_at=clock_timestamp(),resolved_by=nullif($3,'')::uuid WHERE instance_id=$1 AND id=$2 AND resolved_at IS NULL`, s.cfg.InstanceID, id, uuidOrEmpty(actor))
	if err != nil {
		return Quarantine{}, err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_quarantines WHERE instance_id=$1 AND id=$2)`, s.cfg.InstanceID, id).Scan(&exists); err != nil {
			return Quarantine{}, err
		}
		if !exists {
			return Quarantine{}, ErrNotFound
		}
	}
	var q Quarantine
	err = tx.QueryRow(ctx, `SELECT q.id::text,q.feed_key,coalesce(q.transfer_id::text,''),coalesce(q.feed_id::text,''),q.source_id::text,coalesce(s.name,''),q.divergent_changes,q.created_at,q.resolved_at
FROM federation_quarantines q LEFT JOIN sources s ON (s.instance_id,s.id)=(q.instance_id,q.source_id) WHERE q.instance_id=$1 AND q.id=$2`, s.cfg.InstanceID, id).
		Scan(&q.ID, &q.FeedKey, &q.TransferID, &q.FeedID, &q.SourceID, &q.SourceName, &q.DivergentChanges, &q.CreatedAt, &q.ResolvedAt)
	if err != nil {
		return Quarantine{}, err
	}
	return q, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.quarantine.resolved", id, "success", map[string]any{"feed_key": q.FeedKey, "source_id": q.SourceID})
}

// AdoptReplicationJobTx adopts an existing local job into a rule.
func (s *Service) AdoptReplicationJobTx(ctx context.Context, tx pgx.Tx, actor, jobID, ruleID string) (AdoptResult, error) {
	h := s.replication.Load()
	if h == nil || h.AdoptTx == nil {
		return AdoptResult{}, ErrReplicationOff
	}
	return h.AdoptTx(ctx, tx, actor, jobID, ruleID)
}

// --- peer handlers ---

func (s *Service) peerHomeFeed(w http.ResponseWriter, r *http.Request) {
	out, err := s.localHomeFeed(r.Context(), r.PathValue("fed")+"/"+r.PathValue("slug"))
	if errors.Is(err, ErrNotFound) {
		peerFail(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	peerJSON(w, http.StatusOK, out)
}

func (s *Service) peerTransferState(w http.ResponseWriter, r *http.Request) {
	caller, _ := peer.Caller(r)
	t := s.trust.Load()
	if t.doc == nil || caller.InstanceID != t.doc.Authority.InstanceID {
		peerFail(w, http.StatusForbidden, "not_authority")
		return
	}
	key, transfer := r.URL.Query().Get("feed_key"), r.URL.Query().Get("transfer")
	if !uuidPattern.MatchString(transfer) || len(key) > 200 {
		peerFail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	out, err := s.localTransferState(r.Context(), key, transfer)
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	peerJSON(w, http.StatusOK, out)
}

func (s *Service) peerReplicationStatus(w http.ResponseWriter, r *http.Request) {
	items, err := s.LocalRuleStatuses(r.Context())
	if err != nil {
		peerFail(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	peerJSON(w, http.StatusOK, map[string]any{"items": items})
}

// rejectHomeTarget refuses a rule that explicitly targets the home member
// (§9.4). It is checked when a rule is created or changed; after a home
// transfer the rule keeps its targets and the new home simply skips it.
func rejectHomeTarget(b *TopologyBody, r *ReplicationRule) error {
	f, ok := b.Feed(r.FeedKey)
	if !ok {
		return nil
	}
	for _, m := range r.Targets.Members {
		if m == f.Home {
			return invalid("a rule cannot target the home member of " + r.FeedKey)
		}
	}
	return nil
}

// SetFeedRoutingTx replaces the routing of a feed key (authority only). The
// feed revision increases; members pick the change up with the topology.
func (s *Service) SetFeedRoutingTx(ctx context.Context, tx pgx.Tx, actor, key string, in FeedRouting) (*TopologyDocument, TopologyFeed, error) {
	var out TopologyFeed
	if in.Mode == "" {
		in.Mode = RouteProxy
	}
	if !validRouting(&in) {
		return nil, out, invalid("routing mode must be proxy, redirect or off; blob_urls and cache need proxy; blob_url_bind_client needs blob_urls; cache_ttl_seconds needs cache (at most 30 days); prefer lists up to 8 distinct label keys")
	}
	doc, err := s.publishTopologyTx(ctx, tx, actor, func(b *TopologyBody, _ *Membership) error {
		f, ok := b.Feed(key)
		if !ok {
			return ErrNotFound
		}
		r := in
		r.Prefer = append([]string(nil), in.Prefer...)
		f.Routing = &r
		f.Revision++
		out = *f
		return nil
	})
	if err != nil {
		return nil, out, err
	}
	return doc, out, audit(ctx, tx, s.cfg.InstanceID, actor, "federation.feed.routing_updated", "", "success",
		map[string]any{"feed_key": key, "mode": in.Mode, "blob_urls": in.BlobURLs, "blob_url_bind_client": in.BlobURLBindClient, "cache": in.Cache, "generation": doc.Generation})
}
