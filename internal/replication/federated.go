package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/jackc/pgx/v5"
)

// Coordinated replication (docs/planning/phase-2-federation.md §9): the
// federation replication reconciler turns the applied, authority-signed
// topology into local replication state on this member:
//
//   - rule → job: for every rule that targets this member (and a feed key it is
//     not home of) it ensures the destination feed (bound, or created from the
//     descriptor), a federation-mode peer for the home member and one job with
//     origin federation, and patches the job when the rule revision changes;
//   - adoption: an existing local job that already replicates the home feed
//     into the bound feed (same home, same remote feed, same destination) is
//     switched to the rule in place, keeping checkpoint, phase and replica
//     source, so it never reseeds;
//   - release: a job whose rule is gone is disabled and returned to origin
//     local, keeping content and replica source;
//   - home changes: jobs of a feed whose home moved are re-pointed to the new
//     home (a restartable manifest walk that deduplicates every blob already
//     present), the transfer target promotes its replica source to local, and
//     the old home demotes (planned) or quarantines (forced) its local content.
//
// Every step is idempotent and retried on the next pass; nothing here is on
// the serving path.

// AttachFederation connects the replication service to federation: the
// export API is mounted on the federation listener for federation-mode
// peers, the reconciler and adoption are registered, and federation-mode
// peers use the member identity. Call before the federation listener serves.
func (s *Service) AttachFederation(fed *federation.Service, exporter Exporter) {
	if fed == nil {
		return
	}
	s.fed.Store(fed)
	fed.MountPeer("/federation/v1/replication/export/", FederationExport(exporter, s.cfg.InstanceID, fed.AuthorizeReplication))
	fed.SetReplication(&federation.ReplicationHooks{Reconcile: s.ReconcileFederation, AdoptTx: s.AdoptTx})
}

// errBusy means a job is running; the step is retried on the next pass.
var errBusy = errors.New("replication job busy; retrying on the next reconcile pass")

type fedJob struct {
	id, name, peerID, peerMember, peerMode, peerURL, remoteFeed, destFeed, replicaSource, phase, origin, ruleID, feedKey string
	checkpoint, ruleRevision                                                                                             int64
	running, enabled                                                                                                     bool
}

const fedJobSelect = `SELECT j.id::text,j.name,j.peer_id::text,COALESCE(p.member_instance_id::text,''),p.auth_mode,p.url,j.remote_feed_id::text,j.dest_feed_id::text,j.replica_source_id::text,
 j.phase,j.origin,COALESCE(j.federation_rule_id::text,''),COALESCE(j.federation_feed_key,''),j.checkpoint,COALESCE(j.federation_rule_revision,0),
 COALESCE(j.lease_until>clock_timestamp(),false),j.enabled
 FROM replication_jobs j JOIN replication_peers p ON (p.instance_id,p.id)=(j.instance_id,j.peer_id)`

func scanFedJobs(rows pgx.Rows, err error) ([]fedJob, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fedJob
	for rows.Next() {
		var j fedJob
		if err := rows.Scan(&j.id, &j.name, &j.peerID, &j.peerMember, &j.peerMode, &j.peerURL, &j.remoteFeed, &j.destFeed, &j.replicaSource,
			&j.phase, &j.origin, &j.ruleID, &j.feedKey, &j.checkpoint, &j.ruleRevision, &j.running, &j.enabled); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Service) fedJobs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, where string, args ...any) ([]fedJob, error) {
	return scanFedJobs(q.Query(ctx, fedJobSelect+` WHERE j.instance_id=$1 AND `+where, append([]any{s.cfg.InstanceID}, args...)...))
}

// ReconcileFederation runs one reconcile pass. Errors of individual feeds
// are recorded in rule status and the first one is returned.
func (s *Service) ReconcileFederation(ctx context.Context) error {
	fed := s.fed.Load()
	if fed == nil {
		return nil
	}
	topo, err := fed.Topology(ctx)
	if err != nil || topo == nil {
		return err
	}
	m := fed.Membership()
	if m == nil {
		return nil
	}
	self, ok := m.Member(s.cfg.InstanceID)
	if !ok || self.Status != "active" {
		return nil
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	// 1. Demotion: feeds this instance held as home that another member owns.
	homes, err := s.cfg.DB.Query(ctx, `SELECT feed_key,feed_id::text FROM federation_home_feeds WHERE instance_id=$1`, s.cfg.InstanceID)
	if err != nil {
		return err
	}
	type held struct{ key, feed string }
	var demote []held
	for homes.Next() {
		var h held
		if err := homes.Scan(&h.key, &h.feed); err != nil {
			homes.Close()
			return err
		}
		if f, ok := topo.Feed(h.key); ok && f.Home != s.cfg.InstanceID && (!f.Transfer.Active() || f.Transfer.From != s.cfg.InstanceID) {
			demote = append(demote, h)
		}
	}
	homes.Close()
	for _, h := range demote {
		f, _ := topo.Feed(h.key)
		keep(s.demote(ctx, fed, m, topo, f, h.feed))
	}
	// 2. Transfer steps on the target.
	for i := range topo.Feeds {
		f := &topo.Feeds[i]
		t := f.Transfer
		if !t.Active() || t.To != s.cfg.InstanceID {
			continue
		}
		switch t.State {
		case federation.TransferDrain:
			keep(s.triggerTransferJob(ctx, f.Key, t.From))
		case federation.TransferPromote:
			keep(s.promote(ctx, topo, f))
		}
	}
	// 3. Rules that target this member.
	active := map[string]bool{}
	for i := range topo.Rules {
		r := &topo.Rules[i]
		f, ok := topo.Feed(r.FeedKey)
		if !ok || !r.Matches(self) || f.Home == s.cfg.InstanceID {
			continue
		}
		active[r.ID] = true
		if f.Transfer.Active() && f.Transfer.To == s.cfg.InstanceID && f.Transfer.State == federation.TransferPromote {
			continue // the replica is being promoted; the job is gone or going
		}
		keep(s.reconcileRule(ctx, fed, m, f, r))
	}
	// 4. Release jobs of removed rules; re-point or release transfer jobs.
	jobs, err := s.fedJobs(ctx, s.cfg.DB, `j.origin='federation'`)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		switch {
		case j.ruleID != "" && !active[j.ruleID]:
			keep(s.release(ctx, j, "rule removed or no longer targets this member"))
		case j.ruleID == "":
			f, ok := topo.Feed(j.feedKey)
			if !ok {
				keep(s.release(ctx, j, "feed key removed from the topology"))
				continue
			}
			if f.Home == s.cfg.InstanceID {
				keep(s.release(ctx, j, "this member is the home of the feed"))
				continue
			}
			if j.peerMember != f.Home && !f.Transfer.Active() {
				keep(s.repoint(ctx, fed, m, j, f))
			}
		}
	}
	if _, err := s.cfg.DB.Exec(ctx, `DELETE FROM federation_rule_status WHERE instance_id=$1 AND NOT (rule_id = ANY($2::uuid[]))`, s.cfg.InstanceID, keys(active)); err != nil {
		keep(err)
	}
	return first
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (s *Service) ruleStatus(ctx context.Context, rule, key, state, detail, job, homeFeed string) error {
	_, err := s.cfg.DB.Exec(ctx, `INSERT INTO federation_rule_status(instance_id,rule_id,feed_key,state,detail,job_id,home_feed_id,updated_at) VALUES($1,$2,$3,$4,$5,nullif($6,'')::uuid,nullif($7,'')::uuid,clock_timestamp())
ON CONFLICT(instance_id,rule_id) DO UPDATE SET feed_key=EXCLUDED.feed_key,state=EXCLUDED.state,detail=EXCLUDED.detail,job_id=EXCLUDED.job_id,home_feed_id=COALESCE(EXCLUDED.home_feed_id,federation_rule_status.home_feed_id),updated_at=EXCLUDED.updated_at`,
		s.cfg.InstanceID, rule, key, state, truncateText(detail, 1000), job, homeFeed)
	return err
}

func truncateText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// memberURL is the informational URL of a federation-mode peer.
func memberURL(m federation.Member) string {
	if m.APIURL != "" {
		return m.APIURL
	}
	return m.FederationURL
}

func validName(base string) string {
	b := []byte{}
	for _, c := range []byte(base) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	if len(b) > 54 {
		b = b[:54]
	}
	return string(b)
}

// uniqueName returns base, or base-<n> when a row with that name exists.
func uniqueName(ctx context.Context, tx pgx.Tx, table, instance, base string) (string, error) {
	base = validName(base)
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE instance_id=$1 AND name=$2)`, instance, name).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
	}
	return "", ErrConflict
}

// ensureFederationPeer returns the federation-mode peer of a member,
// creating it on first use.
func (s *Service) ensureFederationPeer(ctx context.Context, tx pgx.Tx, m federation.Member) (string, error) {
	var id, current string
	err := tx.QueryRow(ctx, `SELECT id::text,url FROM replication_peers WHERE instance_id=$1 AND member_instance_id=$2`, s.cfg.InstanceID, m.InstanceID).Scan(&id, &current)
	switch {
	case err == nil:
		if current != memberURL(m) {
			if _, err := tx.Exec(ctx, `UPDATE replication_peers SET url=$3,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, id, memberURL(m)); err != nil {
				return "", err
			}
		}
		return id, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	name, err := uniqueName(ctx, tx, "replication_peers", s.cfg.InstanceID, "fed-"+m.Name)
	if err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO replication_peers(instance_id,name,url,auth_mode,member_instance_id) VALUES($1,$2,$3,'federation',$4) RETURNING id::text`,
		s.cfg.InstanceID, name, memberURL(m), m.InstanceID).Scan(&id); err != nil {
		return "", err
	}
	return id, audit(ctx, tx, s.cfg.InstanceID, "", "replication.peer.create", id, map[string]any{"name": name, "auth_mode": "federation", "member_instance_id": m.InstanceID})
}

func ruleFilters(f federation.RuleFilters) Filters {
	return Filters{Namespaces: f.Namespaces, Packages: f.Packages, VersionPrefixes: f.VersionPrefixes}
}

// applyRule writes the rule's settings onto a federation job (existing PATCH
// semantics: filter and cache changes affect future changes only).
func (s *Service) applyRule(ctx context.Context, tx pgx.Tx, job string, r *federation.ReplicationRule) error {
	filters, _ := json.Marshal(ruleFilters(r.Filters))
	_, err := tx.Exec(ctx, `UPDATE replication_jobs SET mode=$3,interval_seconds=$4,include_cached=$5,filters=$6,deletion_policy=$7,destination_read_only=$8,enabled=true,
 federation_rule_revision=$9,revision=revision+1,
 next_run_at=CASE WHEN $3='manual' THEN CASE WHEN run_requested THEN next_run_at END ELSE COALESCE(next_run_at,clock_timestamp()) END
 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, job, r.Mode, r.IntervalSeconds, r.IncludeCached, filters, r.DeletionPolicy, r.DestinationReadOnly, r.Revision)
	if err != nil {
		return err
	}
	return audit(ctx, tx, s.cfg.InstanceID, "", "federation.replication.job_updated", job, map[string]any{"rule_id": r.ID, "rule_revision": r.Revision, "mode": r.Mode, "deletion_policy": r.DeletionPolicy})
}

func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) && strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// adoptable reports whether a job already replicates the home feed: its peer
// is the home member (federation mode) or a token peer at the home member's
// API URL, and it reads exactly the home feed.
func adoptable(j fedJob, home federation.Member, homeFeed string) bool {
	if j.remoteFeed != homeFeed {
		return false
	}
	if j.peerMode == "federation" {
		return j.peerMember == home.InstanceID
	}
	return home.APIURL != "" && sameOrigin(j.peerURL, home.APIURL)
}

func (s *Service) reconcileRule(ctx context.Context, fed *federation.Service, m *federation.Membership, f *federation.TopologyFeed, r *federation.ReplicationRule) error {
	home, ok := m.Member(f.Home)
	if !ok || home.Status != "active" {
		return s.ruleStatus(ctx, r.ID, r.FeedKey, "pending", "the home member is not active", "", "")
	}
	existing, err := s.fedJobs(ctx, s.cfg.DB, `j.federation_rule_id=$2`, r.ID)
	if err != nil {
		return err
	}
	if len(existing) == 1 {
		j := existing[0]
		if j.peerMember != f.Home && !f.Transfer.Active() {
			return s.repoint(ctx, fed, m, j, f)
		}
		if j.ruleRevision != r.Revision || !j.enabled {
			if _, err := inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
				return struct{}{}, s.applyRule(ctx, tx, j.id, r)
			}); err != nil {
				return err
			}
		}
		return s.ruleStatus(ctx, r.ID, r.FeedKey, "ready", "", j.id, j.remoteFeed)
	}
	local, err := s.ensureDestination(ctx, fed, f, r)
	if err != nil || local == "" {
		return err
	}
	hf, err := fed.ResolveHomeFeed(ctx, f.Home, f.Key)
	if err != nil {
		return s.ruleStatus(ctx, r.ID, r.FeedKey, "pending", "home feed not resolvable: "+err.Error(), "", "")
	}
	var format string
	if err := s.cfg.DB.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, local).Scan(&format); err != nil {
		return err
	}
	if format != hf.Format {
		return s.ruleStatus(ctx, r.ID, r.FeedKey, "conflict", fmt.Sprintf("local feed format %q does not match home format %q", format, hf.Format), "", hf.FeedID)
	}
	candidates, err := s.fedJobs(ctx, s.cfg.DB, `j.dest_feed_id=$2 AND (j.origin='local' OR (j.federation_rule_id IS NULL AND j.federation_feed_key=$3))`, local, f.Key)
	if err != nil {
		return err
	}
	for _, j := range candidates {
		if !adoptable(j, home, hf.FeedID) {
			continue
		}
		if j.origin == "local" && !r.AdoptExisting {
			return s.ruleStatus(ctx, r.ID, r.FeedKey, "conflict", "local job "+j.name+" already replicates this feed; adopt it (POST /api/v1/federation/replication/adopt) or enable adopt_existing", j.id, hf.FeedID)
		}
		if _, err := inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (AdoptResult, error) {
			return s.adoptJobTx(ctx, tx, "", j, r, home)
		}); err != nil {
			return err
		}
		return s.ruleStatus(ctx, r.ID, r.FeedKey, "ready", "adopted local job "+j.name+" without reseed", j.id, hf.FeedID)
	}
	for _, j := range candidates {
		if j.origin == "local" {
			return s.ruleStatus(ctx, r.ID, r.FeedKey, "conflict", "local job "+j.name+" replicates another source into the bound feed; remove or repoint it", j.id, hf.FeedID)
		}
	}
	job, err := inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (Job, error) {
		peerID, err := s.ensureFederationPeer(ctx, tx, home)
		if err != nil {
			return Job{}, err
		}
		slug := f.Key[strings.IndexByte(f.Key, '/')+1:]
		name, err := uniqueName(ctx, tx, "replication_jobs", s.cfg.InstanceID, "fed-"+slug)
		if err != nil {
			return Job{}, err
		}
		filters := ruleFilters(r.Filters)
		job, err := s.CreateJobTx(ctx, tx, s.cfg.InstanceID, "", JobInput{Name: name, PeerID: peerID, RemoteFeedID: hf.FeedID, DestFeedID: local, Mode: r.Mode,
			IntervalSeconds: &r.IntervalSeconds, IncludeCached: &r.IncludeCached, Filters: &filters, DeletionPolicy: r.DeletionPolicy, DestinationReadOnly: &r.DestinationReadOnly})
		if err != nil {
			return Job{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE replication_jobs SET origin='federation',federation_rule_id=$3,federation_feed_key=$4,federation_rule_revision=$5 WHERE instance_id=$1 AND id=$2`,
			s.cfg.InstanceID, job.ID, r.ID, r.FeedKey, r.Revision); err != nil {
			return Job{}, err
		}
		return job, audit(ctx, tx, s.cfg.InstanceID, "", "federation.replication.job_created", job.ID, map[string]any{"rule_id": r.ID, "feed_key": r.FeedKey, "home": f.Home})
	})
	if err != nil {
		_ = s.ruleStatus(ctx, r.ID, r.FeedKey, "pending", "job creation failed: "+err.Error(), "", hf.FeedID)
		return err
	}
	return s.ruleStatus(ctx, r.ID, r.FeedKey, "ready", "", job.ID, hf.FeedID)
}

// ensureDestination returns the local feed bound to the rule's feed key,
// creating and binding it from the descriptor when allowed. "" means the
// rule cannot proceed (status recorded).
func (s *Service) ensureDestination(ctx context.Context, fed *federation.Service, f *federation.TopologyFeed, r *federation.ReplicationRule) (string, error) {
	var local string
	err := s.cfg.DB.QueryRow(ctx, `SELECT feed_id::text FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2`, s.cfg.InstanceID, f.Key).Scan(&local)
	if err == nil {
		return local, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	slug := f.Key[strings.IndexByte(f.Key, '/')+1:]
	var exists bool
	if err := s.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND slug=$2)`, s.cfg.InstanceID, slug).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "", s.ruleStatus(ctx, r.ID, r.FeedKey, "conflict", "feed "+slug+" exists but is not bound to "+f.Key+"; bind it (POST /api/v1/federation/feed-bindings) to replicate into it", "", "")
	}
	if !r.CreateDestinationFeed {
		return "", s.ruleStatus(ctx, r.ID, r.FeedKey, "missing_feed", "no local feed is bound to "+f.Key+" and create_destination_feed is off", "", "")
	}
	return inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (string, error) {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format,anonymous_read) VALUES($1,$2,$3,$4) RETURNING id::text`, s.cfg.InstanceID, slug, f.Descriptor.Format, f.Descriptor.AnonymousRead).Scan(&id); err != nil {
			return "", err
		}
		if _, _, err := fed.BindFeedTx(ctx, tx, "", f.Key, id); err != nil {
			return "", err
		}
		return id, audit(ctx, tx, s.cfg.InstanceID, "", "federation.feed.created_from_descriptor", id, map[string]any{"feed_key": f.Key, "format": f.Descriptor.Format})
	})
}

// adoptJobTx switches a job to the rule in place: origin, rule and the
// federation-mode peer of the home change; checkpoint, phase, counters and
// the replica source do not, so the job continues without a reseed.
func (s *Service) adoptJobTx(ctx context.Context, tx pgx.Tx, actor string, j fedJob, r *federation.ReplicationRule, home federation.Member) (AdoptResult, error) {
	peerID, err := s.ensureFederationPeer(ctx, tx, home)
	if err != nil {
		return AdoptResult{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE replication_jobs SET origin='federation',federation_rule_id=$3,federation_feed_key=$4,federation_rule_revision=NULL,adopted_at=COALESCE(adopted_at,clock_timestamp()),peer_id=$5,revision=revision+1
 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, j.id, r.ID, r.FeedKey, peerID)
	if err != nil {
		return AdoptResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return AdoptResult{}, ErrNotFound
	}
	if err := audit(ctx, tx, s.cfg.InstanceID, actor, "federation.replication.job_adopted", j.id, map[string]any{"rule_id": r.ID, "feed_key": r.FeedKey,
		"previous_peer_id": j.peerID, "peer_id": peerID, "checkpoint": j.checkpoint, "phase": j.phase}); err != nil {
		return AdoptResult{}, err
	}
	if err := s.applyRule(ctx, tx, j.id, r); err != nil {
		return AdoptResult{}, err
	}
	return AdoptResult{JobID: j.id, RuleID: r.ID, FeedKey: r.FeedKey, Phase: j.phase, Checkpoint: j.checkpoint, PreviousPeerID: j.peerID, PeerID: peerID}, nil
}

// AdoptResult is federation.AdoptResult.
type AdoptResult = federation.AdoptResult

// AdoptTx is the explicit adoption (POST /api/v1/federation/replication/adopt):
// the job must match the rule exactly (home member, home feed, bound
// destination feed), otherwise it is refused.
func (s *Service) AdoptTx(ctx context.Context, tx pgx.Tx, actor, jobID, ruleID string) (AdoptResult, error) {
	fed := s.fed.Load()
	if fed == nil {
		return AdoptResult{}, federation.ErrReplicationOff
	}
	topo, err := fed.Topology(ctx)
	if err != nil {
		return AdoptResult{}, err
	}
	if topo == nil {
		return AdoptResult{}, federation.ErrNotFound
	}
	r, ok := topo.Rule(ruleID)
	if !ok {
		return AdoptResult{}, federation.ErrNotFound
	}
	f, _ := topo.Feed(r.FeedKey)
	m := fed.Membership()
	self, _ := m.Member(s.cfg.InstanceID)
	if !r.Matches(self) || f.Home == s.cfg.InstanceID {
		return AdoptResult{}, &federation.InputError{Msg: "the rule does not target this member"}
	}
	home, ok := m.Member(f.Home)
	if !ok || home.Status != "active" {
		return AdoptResult{}, &federation.InputError{Msg: "the home member is not active"}
	}
	jobs, err := s.fedJobs(ctx, tx, `j.id=$2`, jobID)
	if err != nil {
		return AdoptResult{}, err
	}
	if len(jobs) != 1 {
		return AdoptResult{}, federation.ErrNotFound
	}
	j := jobs[0]
	if j.ruleID == r.ID {
		return AdoptResult{JobID: j.id, RuleID: r.ID, FeedKey: r.FeedKey, Phase: j.phase, Checkpoint: j.checkpoint, PreviousPeerID: j.peerID, PeerID: j.peerID}, nil
	}
	if j.ruleID != "" {
		return AdoptResult{}, &federation.InputError{Msg: "the job already belongs to another federation rule"}
	}
	var other bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM replication_jobs WHERE instance_id=$1 AND federation_rule_id=$2)`, s.cfg.InstanceID, r.ID).Scan(&other); err != nil {
		return AdoptResult{}, err
	}
	if other {
		return AdoptResult{}, &federation.InputError{Msg: "the rule already has a job on this member"}
	}
	var bound string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT feed_id::text FROM federated_feed_bindings WHERE instance_id=$1 AND feed_key=$2),'')`, s.cfg.InstanceID, r.FeedKey).Scan(&bound); err != nil {
		return AdoptResult{}, err
	}
	if bound != j.destFeed {
		return AdoptResult{}, &federation.InputError{Msg: "the job's destination feed is not bound to " + r.FeedKey}
	}
	homeFeed := ""
	_ = tx.QueryRow(ctx, `SELECT COALESCE(home_feed_id::text,'') FROM federation_rule_status WHERE instance_id=$1 AND rule_id=$2`, s.cfg.InstanceID, r.ID).Scan(&homeFeed)
	if homeFeed == "" {
		hf, err := fed.ResolveHomeFeed(ctx, f.Home, f.Key)
		if err != nil {
			return AdoptResult{}, err
		}
		homeFeed = hf.FeedID
	}
	if !adoptable(j, home, homeFeed) {
		return AdoptResult{}, &federation.InputError{Msg: "the job does not replicate the home feed of " + r.FeedKey + " from " + home.Name + " (peer URL or remote feed differ)"}
	}
	return s.adoptJobTx(ctx, tx, actor, j, r, home)
}

// release returns a federation job to local management, disabled. Content
// and the replica source stay; deleting them is an explicit local action.
func (s *Service) release(ctx context.Context, j fedJob, reason string) error {
	_, err := inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		if _, err := tx.Exec(ctx, `UPDATE replication_jobs SET origin='local',federation_rule_id=NULL,federation_feed_key=NULL,federation_rule_revision=NULL,enabled=false,next_run_at=NULL,revision=revision+1
 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, j.id); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, audit(ctx, tx, s.cfg.InstanceID, "", "federation.replication.job_released", j.id, map[string]any{"rule_id": j.ruleID, "feed_key": j.feedKey, "reason": reason})
	})
	return err
}

// repoint moves a job to the current home of its feed. The new home has its
// own change sequence, so the job restarts with a manifest walk (reseed):
// it only upserts, deduplicates every blob already present and records items
// the new home no longer lists as held deletions.
func (s *Service) repoint(ctx context.Context, fed *federation.Service, m *federation.Membership, j fedJob, f *federation.TopologyFeed) error {
	home, ok := m.Member(f.Home)
	if !ok || home.Status != "active" {
		return nil
	}
	hf, err := fed.ResolveHomeFeed(ctx, f.Home, f.Key)
	if err != nil {
		if j.ruleID != "" {
			_ = s.ruleStatus(ctx, j.ruleID, f.Key, "pending", "new home feed not resolvable: "+err.Error(), j.id, "")
		}
		return err
	}
	_, err = inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		peerID, err := s.ensureFederationPeer(ctx, tx, home)
		if err != nil {
			return struct{}{}, err
		}
		tag, err := tx.Exec(ctx, `UPDATE replication_jobs SET peer_id=$3,remote_feed_id=$4,phase='new',reseeding=true,seed_cursor='',seed_head=NULL,seed_done=0,seed_total=NULL,
 checkpoint=0,checkpoint_change_at=NULL,remote_head=NULL,remote_head_at=NULL,next_run_at=clock_timestamp(),revision=revision+1
 WHERE instance_id=$1 AND id=$2 AND (lease_until IS NULL OR lease_until<clock_timestamp())`, s.cfg.InstanceID, j.id, peerID, hf.FeedID)
		if err != nil {
			return struct{}{}, err
		}
		if tag.RowsAffected() == 0 {
			return struct{}{}, errBusy
		}
		if _, err := tx.Exec(ctx, `DELETE FROM replication_backfills WHERE job_id=$1`, j.id); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, audit(ctx, tx, s.cfg.InstanceID, "", "federation.replication.job_repointed", j.id, map[string]any{"feed_key": f.Key, "previous_member": j.peerMember, "home": f.Home})
	})
	return err
}

func (s *Service) triggerTransferJob(ctx context.Context, key, from string) error {
	_, err := s.cfg.DB.Exec(ctx, `UPDATE replication_jobs j SET run_requested=true,next_run_at=LEAST(COALESCE(next_run_at,clock_timestamp()),clock_timestamp())
 FROM replication_peers p WHERE (p.instance_id,p.id)=(j.instance_id,j.peer_id) AND j.instance_id=$1 AND j.origin='federation' AND j.federation_feed_key=$2 AND p.member_instance_id=$3 AND j.enabled`, s.cfg.InstanceID, key, from)
	return err
}

func stepDone(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, instance, transfer, step string) (bool, error) {
	var done bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM federation_transfer_steps WHERE instance_id=$1 AND transfer_id=$2 AND step=$3)`, instance, transfer, step).Scan(&done)
	return done, err
}

// promote converts the target's replica source into a local source in one
// transaction (provenance kept), removes the job, makes the feed writable
// and records the new home's change baseline. Planned transfers require the
// job to have drained to the frozen head.
func (s *Service) promote(ctx context.Context, topo *federation.TopologyDocument, f *federation.TopologyFeed) error {
	t := f.Transfer
	if done, err := stepDone(ctx, s.cfg.DB, s.cfg.InstanceID, t.ID, "promote"); err != nil || done {
		return err
	}
	jobs, err := s.fedJobs(ctx, s.cfg.DB, `j.origin='federation' AND j.federation_feed_key=$2 AND p.member_instance_id=$3`, f.Key, t.From)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return fmt.Errorf("%w: no replica job for %s to promote", ErrConflict, f.Key)
	}
	j := jobs[0]
	if t.Mode == "planned" && (j.phase != "incremental" || j.checkpoint < t.Head) {
		return s.triggerTransferJob(ctx, f.Key, t.From)
	}
	_, err = inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var running bool
		var checkpoint int64
		var phase string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_until>clock_timestamp(),false),checkpoint,phase FROM replication_jobs WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.cfg.InstanceID, j.id).Scan(&running, &checkpoint, &phase); err != nil {
			return struct{}{}, err
		}
		if running {
			return struct{}{}, errBusy
		}
		if _, err := tx.Exec(ctx, `DELETE FROM replication_jobs WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, j.id); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE sources SET kind='local',revision=revision+1 WHERE instance_id=$1 AND id=$2 AND kind='replica'`, s.cfg.InstanceID, j.replicaSource); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, j.destFeed); err != nil {
			return struct{}{}, err
		}
		// A feed without a publish target publishes into the promoted source.
		if _, err := tx.Exec(ctx, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id) VALUES($1,$2,$3)
 ON CONFLICT(feed_id) DO UPDATE SET default_source_id=COALESCE(publish_policies.default_source_id,EXCLUDED.default_source_id)`, s.cfg.InstanceID, j.destFeed, j.replicaSource); err != nil {
			return struct{}{}, err
		}
		var baseline int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT seq FROM replication_heads WHERE instance_id=$1),0)`, s.cfg.InstanceID).Scan(&baseline); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_home_feeds(instance_id,feed_key,feed_id,since_generation) VALUES($1,$2,$3,$4) ON CONFLICT(instance_id,feed_key) DO UPDATE SET feed_id=EXCLUDED.feed_id`,
			s.cfg.InstanceID, f.Key, j.destFeed, topo.Generation); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_feed_freezes WHERE instance_id=$1 AND feed_id=$2`, s.cfg.InstanceID, j.destFeed); err != nil {
			return struct{}{}, err
		}
		detail, _ := json.Marshal(map[string]any{"checkpoint": checkpoint, "baseline": baseline, "source_id": j.replicaSource, "feed_id": j.destFeed, "mode": t.Mode, "phase": phase})
		if _, err := tx.Exec(ctx, `INSERT INTO federation_transfer_steps(instance_id,transfer_id,feed_key,step,detail) VALUES($1,$2,$3,'promote',$4)`, s.cfg.InstanceID, t.ID, f.Key, detail); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, audit(ctx, tx, s.cfg.InstanceID, "", "federation.transfer.promoted", j.destFeed, map[string]any{"feed_key": f.Key, "transfer_id": t.ID, "mode": t.Mode, "checkpoint": checkpoint, "baseline": baseline})
	})
	return err
}

// dropPublishTargets removes publish-policy references to sources that stop
// being local (they would no longer compile) and returns what was removed.
func dropPublishTargets(ctx context.Context, tx pgx.Tx, instance, feed string, sources []string) (map[string]any, error) {
	var dflt *string
	var rules []byte
	err := tx.QueryRow(ctx, `SELECT default_source_id::text,rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2 FOR UPDATE`, instance, feed).Scan(&dflt, &rules)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	removed := map[string]any{}
	if dflt != nil && has(sources, *dflt) {
		removed["default_source_id"] = *dflt
	}
	var list []map[string]any
	_ = json.Unmarshal(rules, &list)
	kept := []map[string]any{}
	dropped := []map[string]any{}
	for _, r := range list {
		if target, _ := r["Target"].(string); target != "" && has(sources, target) {
			dropped = append(dropped, r)
			continue
		}
		kept = append(kept, r)
	}
	if len(dropped) > 0 {
		removed["rules"] = dropped
	}
	encoded, _ := json.Marshal(kept)
	_, err = tx.Exec(ctx, `UPDATE publish_policies SET default_source_id=CASE WHEN default_source_id::text = ANY($3) THEN NULL ELSE default_source_id END,rules=$4 WHERE instance_id=$1 AND feed_id=$2`,
		instance, feed, sources, encoded)
	return removed, err
}

// demote runs on an old home once the topology names another home. Planned
// (the transfer froze this feed and the target drained to the frozen head):
// the single local source becomes the replica source of a federation job on
// the new home, its assets are re-stamped with that job's provenance and the
// job starts with a manifest walk that deduplicates every blob, so nothing is
// transferred again. Otherwise (forced, or several local sources): the local
// sources are detached and kept as quarantined sources with the count of
// changes the new home may not have, and a fresh replica is seeded from the
// new home. The feed stays frozen until this completes.
func (s *Service) demote(ctx context.Context, fed *federation.Service, m *federation.Membership, topo *federation.TopologyDocument, f *federation.TopologyFeed, feed string) error {
	home, ok := m.Member(f.Home)
	if !ok || home.Status != "active" {
		return nil
	}
	hf, err := fed.ResolveHomeFeed(ctx, f.Home, f.Key)
	if err != nil {
		return err
	}
	t := f.Transfer
	transferID := ""
	if t != nil && t.From == s.cfg.InstanceID {
		transferID = t.ID
	}
	planned := false
	if t != nil && t.From == s.cfg.InstanceID && t.Mode == "planned" && t.State == federation.TransferComplete && t.To == f.Home {
		if planned, err = stepDone(ctx, s.cfg.DB, s.cfg.InstanceID, t.ID, "freeze"); err != nil {
			return err
		}
	}
	slug := f.Key[strings.IndexByte(f.Key, '/')+1:]
	_, err = inTx(ctx, s, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		var format string
		if err := tx.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.cfg.InstanceID, feed).Scan(&format); err != nil {
			return struct{}{}, err
		}
		if format != hf.Format {
			return struct{}{}, fmt.Errorf("%w: home format %q does not match %q", ErrConflict, hf.Format, format)
		}
		rows, err := tx.Query(ctx, `SELECT s.id::text FROM feed_sources fs JOIN sources s ON (s.instance_id,s.id)=(fs.instance_id,fs.source_id) WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind='local' ORDER BY fs.priority`, s.cfg.InstanceID, feed)
		if err != nil {
			return struct{}{}, err
		}
		var locals []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return struct{}{}, err
			}
			locals = append(locals, id)
		}
		rows.Close()
		peerID, err := s.ensureFederationPeer(ctx, tx, home)
		if err != nil {
			return struct{}{}, err
		}
		removed, err := dropPublishTargets(ctx, tx, s.cfg.InstanceID, feed, locals)
		if err != nil {
			return struct{}{}, err
		}
		name, err := uniqueName(ctx, tx, "replication_jobs", s.cfg.InstanceID, "fed-"+slug)
		if err != nil {
			return struct{}{}, err
		}
		detail := map[string]any{"new_home": f.Home, "publish_policy_removed": removed}
		var job string
		if planned && len(locals) == 1 {
			src := locals[0]
			if _, err := tx.Exec(ctx, `UPDATE sources SET kind='replica',revision=revision+1 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, src); err != nil {
				return struct{}{}, err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO replication_jobs(instance_id,name,peer_id,remote_feed_id,dest_feed_id,replica_source_id,mode,interval_seconds,include_cached,filters,deletion_policy,next_run_at,destination_read_only,origin,federation_feed_key,reseeding)
 VALUES($1,$2,$3,$4,$5,$6,'continuous',60,false,'{}'::jsonb,'propagate',clock_timestamp(),true,'federation',$7,true) RETURNING id::text`,
				s.cfg.InstanceID, name, peerID, hf.FeedID, feed, src, f.Key).Scan(&job); err != nil {
				return struct{}{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE assets SET provenance=provenance||jsonb_build_object('replication_job',$2::text,'peer_id',$3::text,'origin_priority',2147483647,'seed_generation',0)
 WHERE version_id IN (SELECT v.id FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=$1)`, src, job, peerID); err != nil {
				return struct{}{}, err
			}
			if err := feedgraph.Check(ctx, tx, s.cfg.InstanceID); err != nil {
				return struct{}{}, err
			}
			detail["mode"], detail["source_id"] = "planned", src
		} else {
			var checkpoint *int64
			if t != nil && t.From == s.cfg.InstanceID && t.State == federation.TransferComplete {
				checkpoint = &t.Checkpoint
			}
			quarantined := []string{}
			for _, src := range locals {
				if _, err := tx.Exec(ctx, `DELETE FROM feed_sources WHERE instance_id=$1 AND feed_id=$2 AND source_id=$3`, s.cfg.InstanceID, feed, src); err != nil {
					return struct{}{}, err
				}
				qname, err := uniqueName(ctx, tx, "sources", s.cfg.InstanceID, "quarantine-"+slug)
				if err != nil {
					return struct{}{}, err
				}
				if _, err := tx.Exec(ctx, `UPDATE sources SET name=$3,revision=revision+1 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, src, qname); err != nil {
					return struct{}{}, err
				}
				var divergent *int64
				if checkpoint != nil {
					var n int64
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM replication_changes WHERE instance_id=$1 AND source_id=$2 AND seq>$3`, s.cfg.InstanceID, src, *checkpoint).Scan(&n); err != nil {
						return struct{}{}, err
					}
					divergent = &n
				}
				var qid string
				if err := tx.QueryRow(ctx, `INSERT INTO federation_quarantines(instance_id,feed_key,transfer_id,feed_id,source_id,divergent_changes) VALUES($1,$2,nullif($3,'')::uuid,$4,$5,$6) RETURNING id::text`,
					s.cfg.InstanceID, f.Key, transferID, feed, src, divergent).Scan(&qid); err != nil {
					return struct{}{}, err
				}
				if err := audit(ctx, tx, s.cfg.InstanceID, "", "federation.transfer.quarantined", qid, map[string]any{"feed_key": f.Key, "source_id": src, "divergent_changes": divergent}); err != nil {
					return struct{}{}, err
				}
				quarantined = append(quarantined, qid)
			}
			include, readOnly, interval := false, true, 60
			created, err := s.CreateJobTx(ctx, tx, s.cfg.InstanceID, "", JobInput{Name: name, PeerID: peerID, RemoteFeedID: hf.FeedID, DestFeedID: feed, Mode: "continuous",
				IntervalSeconds: &interval, IncludeCached: &include, DeletionPolicy: "propagate", DestinationReadOnly: &readOnly})
			if err != nil {
				return struct{}{}, err
			}
			job = created.ID
			if _, err := tx.Exec(ctx, `UPDATE replication_jobs SET origin='federation',federation_feed_key=$3 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, job, f.Key); err != nil {
				return struct{}{}, err
			}
			detail["mode"], detail["quarantines"] = "forced", quarantined
		}
		detail["job_id"] = job
		if _, err := tx.Exec(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, feed); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_home_feeds WHERE instance_id=$1 AND feed_key=$2`, s.cfg.InstanceID, f.Key); err != nil {
			return struct{}{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM federation_feed_freezes WHERE instance_id=$1 AND feed_id=$2`, s.cfg.InstanceID, feed); err != nil {
			return struct{}{}, err
		}
		stepID := transferID
		if stepID == "" {
			if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&stepID); err != nil {
				return struct{}{}, err
			}
		}
		raw, _ := json.Marshal(detail)
		if _, err := tx.Exec(ctx, `INSERT INTO federation_transfer_steps(instance_id,transfer_id,feed_key,step,detail) VALUES($1,$2,$3,'demote',$4) ON CONFLICT DO NOTHING`, s.cfg.InstanceID, stepID, f.Key, raw); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, audit(ctx, tx, s.cfg.InstanceID, "", "federation.transfer.demoted", feed, map[string]any{"feed_key": f.Key, "transfer_id": transferID, "mode": detail["mode"], "job_id": job, "generation": topo.Generation})
	})
	return err
}
