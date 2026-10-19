package retention

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Policy targets exactly one feed or one source. Zero rule values are unset and at
// least one rule is required. A version is selected when it violates any rule:
//
//	KeepLast: more than KeepLast newer versions exist in its component.
//	MaxAge:   created longer ago than MaxAge.
//	MaxIdle:  not accessed (latest of created_at, last_accessed_at and last download) within MaxIdle.
//
// MinKeep newest versions per component are exempt from MaxAge and MaxIdle (not
// from KeepLast). Tagged versions are exempt unless IncludeTagged. Content in
// local or replica sources is release content and is exempt unless
// AllowReleaseDelete; remote-source (cache) content is not.
type Policy struct {
	ID                 string        `json:"id,omitempty"`
	FeedID             string        `json:"feed_id,omitempty"`
	SourceID           string        `json:"source_id,omitempty"`
	KeepLast           int           `json:"keep_last,omitempty"`
	MaxAge             time.Duration `json:"max_age,omitempty"`
	MaxIdle            time.Duration `json:"max_idle,omitempty"`
	MinKeep            int           `json:"min_keep"`
	IncludeTagged      bool          `json:"include_tagged,omitempty"`
	AllowReleaseDelete bool          `json:"allow_release_delete,omitempty"`
	Enabled            bool          `json:"enabled"`
}

func (p Policy) validate() error {
	if (p.FeedID == "") == (p.SourceID == "") {
		return fmt.Errorf("%w: exactly one of feed or source", ErrInvalidPolicy)
	}
	for _, id := range []string{p.FeedID, p.SourceID} {
		if id != "" && !uuidPattern.MatchString(id) {
			return fmt.Errorf("%w: malformed identifier", ErrInvalidPolicy)
		}
	}
	if p.KeepLast < 0 || p.MaxAge < 0 || p.MaxIdle < 0 || p.MinKeep < 0 || p.MaxAge != 0 && p.MaxAge < time.Second || p.MaxIdle != 0 && p.MaxIdle < time.Second {
		return fmt.Errorf("%w: negative or sub-second rule", ErrInvalidPolicy)
	}
	if p.KeepLast == 0 && p.MaxAge == 0 && p.MaxIdle == 0 {
		return fmt.Errorf("%w: at least one rule", ErrInvalidPolicy)
	}
	return nil
}

// SetPolicy creates or replaces the policy for its feed or source.
func (s *Service) SetPolicy(ctx context.Context, p Policy) (Policy, error) {
	if err := s.validate(); err != nil {
		return Policy{}, err
	}
	if err := p.validate(); err != nil {
		return Policy{}, err
	}
	var keep, age, idle any
	if p.KeepLast > 0 {
		keep = p.KeepLast
	}
	if p.MaxAge > 0 {
		age = int64(p.MaxAge / time.Second)
	}
	if p.MaxIdle > 0 {
		idle = int64(p.MaxIdle / time.Second)
	}
	conflict := "(instance_id,feed_id) WHERE feed_id IS NOT NULL"
	if p.SourceID != "" {
		conflict = "(instance_id,source_id) WHERE source_id IS NOT NULL"
	}
	err := s.DB.QueryRow(ctx, `INSERT INTO retention_policies(instance_id,feed_id,source_id,keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,allow_release_delete,enabled) VALUES($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT `+conflict+` DO UPDATE SET keep_last=excluded.keep_last,max_age_seconds=excluded.max_age_seconds,max_idle_seconds=excluded.max_idle_seconds,min_keep=excluded.min_keep,include_tagged=excluded.include_tagged,allow_release_delete=excluded.allow_release_delete,enabled=excluded.enabled,revision=retention_policies.revision+1,updated_at=now() RETURNING id::text`,
		s.InstanceID, p.FeedID, p.SourceID, keep, age, idle, p.MinKeep, p.IncludeTagged, p.AllowReleaseDelete, p.Enabled).Scan(&p.ID)
	return p, err
}

// DeletePolicy removes a policy; its content is then never selected for deletion.
func (s *Service) DeletePolicy(ctx context.Context, id string) error {
	if err := s.validate(); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `DELETE FROM retention_policies WHERE instance_id=$1 AND id=$2`, s.InstanceID, id)
	return err
}

// ListPolicies returns all policies of the instance.
func (s *Service) ListPolicies(ctx context.Context) ([]Policy, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,COALESCE(feed_id::text,''),COALESCE(source_id::text,''),COALESCE(keep_last,0),COALESCE(max_age_seconds,0),COALESCE(max_idle_seconds,0),min_keep,include_tagged,allow_release_delete,enabled FROM retention_policies WHERE instance_id=$1 ORDER BY id`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		var age, idle int64
		if err = rows.Scan(&p.ID, &p.FeedID, &p.SourceID, &p.KeepLast, &age, &idle, &p.MinKeep, &p.IncludeTagged, &p.AllowReleaseDelete, &p.Enabled); err != nil {
			return nil, err
		}
		p.MaxAge, p.MaxIdle = time.Duration(age)*time.Second, time.Duration(idle)*time.Second
		out = append(out, p)
	}
	return out, rows.Err()
}

// Deletion is one version removed (or, in dry-run, that would be removed).
type Deletion struct {
	SourceID   string `json:"source_id"`
	Coordinate string `json:"coordinate"`
	Version    string `json:"version"`
	VersionID  string `json:"version_id"`
	Reason     string `json:"reason"`
}

// RetentionReport summarizes policy application. Protected counts show versions
// that a rule selected but an exemption retained.
type RetentionReport struct {
	DryRun           bool       `json:"dry_run"`
	Deleted          []Deletion `json:"deleted"`
	ProtectedRelease int        `json:"protected_release"`
	ProtectedTagged  int        `json:"protected_tagged"`
}

type versionRow struct {
	ID, Version string
	Created     time.Time
	LastUsed    time.Time
	Rank        int // 1 = newest
	Tagged      bool
}

// decide evaluates one policy against a component's versions.
func decide(p Policy, sourceKind string, rows []versionRow, now time.Time) (selected map[string]string, protectedRelease, protectedTagged int) {
	selected = map[string]string{}
	for _, v := range rows {
		var reasons []string
		if p.KeepLast > 0 && v.Rank > p.KeepLast {
			reasons = append(reasons, "keep_last")
		}
		if p.MaxAge > 0 && now.Sub(v.Created) > p.MaxAge && v.Rank > p.MinKeep {
			reasons = append(reasons, "max_age")
		}
		if p.MaxIdle > 0 && now.Sub(v.LastUsed) > p.MaxIdle && v.Rank > p.MinKeep {
			reasons = append(reasons, "max_idle")
		}
		if len(reasons) == 0 {
			continue
		}
		if v.Tagged && !p.IncludeTagged {
			protectedTagged++
			continue
		}
		if sourceKind != "remote" && !p.AllowReleaseDelete {
			protectedRelease++
			continue
		}
		selected[v.ID] = strings.Join(reasons, ",")
	}
	return
}

// effectivePolicies returns the policies that must all select a version. A source
// policy overrides feed policies; otherwise every attached feed needs an enabled
// policy (a feed without one means keep everything). nil means no deletion.
func effectivePolicies(sourcePolicy *Policy, feedPolicies []*Policy, feedCount int) []Policy {
	if sourcePolicy != nil {
		if !sourcePolicy.Enabled {
			return nil
		}
		return []Policy{*sourcePolicy}
	}
	if feedCount == 0 || len(feedPolicies) != feedCount {
		return nil
	}
	var out []Policy
	for _, p := range feedPolicies {
		if p == nil || !p.Enabled {
			return nil
		}
		out = append(out, *p)
	}
	return out
}

type sourceInfo struct {
	ID, Kind string
	Policies []Policy
}

func (s *Service) loadSources(ctx context.Context) ([]sourceInfo, error) {
	policies, err := s.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	// The instance default fills in for remote-cache feeds without a policy.
	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	fallback := settings.Default.policy()
	bySource, byFeed := map[string]*Policy{}, map[string]*Policy{}
	for i := range policies {
		if policies[i].SourceID != "" {
			bySource[policies[i].SourceID] = &policies[i]
		} else {
			byFeed[policies[i].FeedID] = &policies[i]
		}
	}
	feeds := map[string][]string{}
	rows, err := s.DB.Query(ctx, `SELECT source_id::text,feed_id::text FROM feed_sources WHERE instance_id=$1`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, feed string
		if err = rows.Scan(&src, &feed); err != nil {
			rows.Close()
			return nil, err
		}
		feeds[src] = append(feeds[src], feed)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT id::text,kind FROM sources WHERE instance_id=$1 ORDER BY id`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sourceInfo
	for rows.Next() {
		var si sourceInfo
		if err = rows.Scan(&si.ID, &si.Kind); err != nil {
			return nil, err
		}
		var fp []*Policy
		for _, f := range feeds[si.ID] {
			if p := byFeed[f]; p != nil {
				fp = append(fp, p)
			} else if fallback != nil && si.Kind == "remote" {
				fp = append(fp, fallback)
			}
		}
		if si.Policies = effectivePolicies(bySource[si.ID], fp, len(feeds[si.ID])); len(si.Policies) > 0 {
			out = append(out, si)
		}
	}
	return out, rows.Err()
}

// ApplyRetention evaluates every policy and deletes the selected versions (assets,
// tags and subject links). Blob bytes are not touched: garbage collection reclaims
// them after the grace period. With dryRun the report lists exactly the versions a
// real run at this moment would delete and nothing is modified.
func (s *Service) ApplyRetention(ctx context.Context, dryRun bool) (RetentionReport, error) {
	report := RetentionReport{DryRun: dryRun, Deleted: []Deletion{}}
	if err := s.validate(); err != nil {
		return report, err
	}
	sources, err := s.loadSources(ctx)
	if err != nil {
		return report, err
	}
	var errs []error
	for _, src := range sources {
		var after string
		for {
			rows, err := s.DB.Query(ctx, `SELECT id::text,coordinate FROM components WHERE instance_id=$1 AND source_id=$2 AND id::text>$3 ORDER BY id::text LIMIT 200`, s.InstanceID, src.ID, after)
			if err != nil {
				return report, errors.Join(append(errs, err)...)
			}
			type comp struct{ id, coordinate string }
			var comps []comp
			for rows.Next() {
				var c comp
				if err = rows.Scan(&c.id, &c.coordinate); err != nil {
					rows.Close()
					return report, errors.Join(append(errs, err)...)
				}
				comps = append(comps, c)
			}
			rows.Close()
			if len(comps) == 0 {
				break
			}
			after = comps[len(comps)-1].id
			for _, c := range comps {
				if err = ctx.Err(); err != nil {
					return report, errors.Join(append(errs, err)...)
				}
				if err = s.retainComponent(ctx, src, c.id, c.coordinate, dryRun, &report); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return report, errors.Join(errs...)
}

// retainComponent serializes with publication to the same coordinate (same
// advisory lock as artifact.Service) and decides under that lock.
func (s *Service) retainComponent(ctx context.Context, src sourceInfo, component, coordinate string, dryRun bool, report *RetentionReport) error {
	var q interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
		QueryRow(context.Context, string, ...any) pgx.Row
	} = s.DB
	var tx pgx.Tx
	if !dryRun {
		var err error
		if tx, err = s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}); err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		sum := sha256.Sum256([]byte(s.InstanceID + "\x00" + src.ID + "\x00" + coordinate))
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8]))); err != nil {
			return err
		}
		q = tx
	}
	var now time.Time
	if err := q.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `SELECT v.id::text,v.version,v.created_at,COALESCE((SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM assets a LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE a.version_id=v.id),v.created_at),row_number() OVER (ORDER BY v.created_at DESC,v.id DESC),EXISTS(SELECT 1 FROM tags t WHERE t.component_id=v.component_id AND t.version_id=v.id) FROM versions v WHERE v.component_id=$1 ORDER BY 5`, component)
	if err != nil {
		return err
	}
	var versions []versionRow
	for rows.Next() {
		var v versionRow
		if err = rows.Scan(&v.ID, &v.Version, &v.Created, &v.LastUsed, &v.Rank, &v.Tagged); err != nil {
			rows.Close()
			return err
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var selected map[string]string
	for i, p := range src.Policies {
		one, rel, tag := decide(p, src.Kind, versions, now)
		report.ProtectedRelease += rel
		report.ProtectedTagged += tag
		if i == 0 {
			selected = one
			continue
		}
		for id := range selected {
			if _, ok := one[id]; !ok {
				delete(selected, id)
			}
		}
	}
	var batch []Deletion
	for _, v := range versions {
		if reason, ok := selected[v.ID]; ok {
			batch = append(batch, Deletion{SourceID: src.ID, Coordinate: coordinate, Version: v.Version, VersionID: v.ID, Reason: reason})
		}
	}
	if len(batch) == 0 {
		return nil
	}
	if dryRun {
		report.Deleted = append(report.Deleted, batch...)
		return nil
	}
	ids := make([]string, len(batch))
	for i, d := range batch {
		ids[i] = d.VersionID
	}
	if _, err = tx.Exec(ctx, `DELETE FROM tags WHERE component_id=$1 AND version_id=ANY($2::uuid[])`, component, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM assets WHERE instance_id=$1 AND version_id=ANY($2::uuid[])`, s.InstanceID, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM versions WHERE instance_id=$1 AND component_id=$2 AND id=ANY($3::uuid[])`, s.InstanceID, component, ids); err != nil {
		return err
	}
	for _, d := range batch {
		details, _ := json.Marshal(map[string]string{"source_id": d.SourceID, "coordinate": d.Coordinate, "version": d.Version, "reason": d.Reason})
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(instance_id,action,resource_id,details) VALUES($1,'retention.version.delete',$2,$3)`, s.InstanceID, d.VersionID, details); err != nil {
			return err
		}
	}
	if err = s.requireLeaseTx(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	report.Deleted = append(report.Deleted, batch...)
	return nil
}
