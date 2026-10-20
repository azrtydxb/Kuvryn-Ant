package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/jackc/pgx/v5"
)

// TemplateVersion is the only template schema version.
const TemplateVersion = 1

// Template is a reviewed, non-secret snapshot of a feed's configuration that a
// destination may adopt (spec 6.7). It is separate from content sync and never
// carries anything destination-specific: no endpoint or origin, credentials,
// CA references, identity or access policy (grants, anonymous read, publish
// policy), and not the retention policy's release-delete switch.
type Template struct {
	Version       int                `json:"version"`
	Format        string             `json:"format"`
	Retention     *TemplateRetention `json:"retention,omitempty"`
	RemoteSources []TemplateRemote   `json:"remote_sources"`
	// Sources lists the source layout for review only; it is never applied.
	Sources []TemplateSource `json:"sources"`
	// Excluded names the setting groups deliberately left out of every template.
	Excluded []string `json:"excluded"`
}

// TemplateRetention is the feed-level retention policy minus allow_release_delete.
type TemplateRetention struct {
	KeepLast       int   `json:"keep_last,omitempty"`
	MaxAgeSeconds  int64 `json:"max_age_seconds,omitempty"`
	MaxIdleSeconds int64 `json:"max_idle_seconds,omitempty"`
	MinKeep        int   `json:"min_keep"`
	IncludeTagged  bool  `json:"include_tagged"`
	Enabled        bool  `json:"enabled"`
}

// TemplateRemote carries the cache and timeout tunables of a remote source,
// matched at the destination by source name. Its origin, credential, CA
// reference and private-network setting stay destination-specific.
type TemplateRemote struct {
	Name           string                   `json:"name"`
	TimeoutSeconds int                      `json:"timeout_seconds"`
	Cache          remoteconfig.CachePolicy `json:"cache"`
}

// TemplateSource is one attached source, for review.
type TemplateSource struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority"`
}

var templateExcluded = []string{
	"feed.slug", "feed.anonymous_read", "access.grants", "access.publish_policy", "identity",
	"remote_source.origin", "remote_source.redirect_origins", "remote_source.credential", "remote_source.ca_ref", "remote_source.allow_private",
	"retention.allow_release_delete",
}

func (t Template) valid() bool {
	if t.Version != TemplateVersion || t.Format == "" || len(t.Format) > 64 || len(t.RemoteSources) > 256 || len(t.Sources) > 1024 {
		return false
	}
	if r := t.Retention; r != nil && (r.KeepLast < 0 || r.MaxAgeSeconds < 0 || r.MaxIdleSeconds < 0 || r.MinKeep < 0 ||
		r.KeepLast == 0 && r.MaxAgeSeconds == 0 && r.MaxIdleSeconds == 0) {
		return false
	}
	seen := map[string]bool{}
	for _, rs := range t.RemoteSources {
		if rs.Name == "" || len(rs.Name) > 256 || seen[rs.Name] {
			return false
		}
		seen[rs.Name] = true
	}
	return true
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Template exports the non-secret configuration of an exportable feed. Callers
// must authorize replicate access to the feed first.
func (e Exporter) Template(ctx context.Context, instance, feed string) (Template, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := e.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Template{}, ErrUnavailable
	}
	defer rollback(tx)
	t := Template{Version: TemplateVersion, RemoteSources: []TemplateRemote{}, Sources: []TemplateSource{}, Excluded: append([]string(nil), templateExcluded...)}
	if err = tx.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2`, instance, feed).Scan(&t.Format); err != nil {
		return Template{}, dbError(err)
	}
	if t.Retention, err = loadRetentionTemplate(ctx, tx, instance, feed); err != nil {
		return Template{}, err
	}
	rows, err := tx.Query(ctx, `SELECT s.name,s.kind,fs.priority,s.id::text FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind<>'replica' ORDER BY fs.priority`, instance, feed)
	if err != nil {
		return Template{}, ErrUnavailable
	}
	type remote struct{ name, id string }
	var remotes []remote
	for rows.Next() {
		var src TemplateSource
		var id string
		if err = rows.Scan(&src.Name, &src.Kind, &src.Priority, &id); err != nil {
			rows.Close()
			return Template{}, ErrUnavailable
		}
		t.Sources = append(t.Sources, src)
		if src.Kind == "remote" {
			remotes = append(remotes, remote{src.Name, id})
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return Template{}, ErrUnavailable
	}
	for _, r := range remotes {
		rec, err := remoteconfig.Store{InstanceID: instance}.LoadTx(ctx, tx, r.id)
		if errors.Is(err, remoteconfig.ErrNotFound) {
			continue
		}
		if err != nil {
			return Template{}, ErrUnavailable
		}
		t.RemoteSources = append(t.RemoteSources, TemplateRemote{Name: r.name, TimeoutSeconds: rec.Config.TimeoutSeconds, Cache: rec.Config.Cache})
	}
	return t, nil
}

func loadRetentionTemplate(ctx context.Context, q queryer, instance, feed string) (*TemplateRetention, error) {
	var r TemplateRetention
	var keep *int
	var age, idle *int64
	err := q.QueryRow(ctx, `SELECT keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,enabled FROM retention_policies WHERE instance_id=$1 AND feed_id=$2`, instance, feed).Scan(&keep, &age, &idle, &r.MinKeep, &r.IncludeTagged, &r.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	if keep != nil {
		r.KeepLast = *keep
	}
	if age != nil {
		r.MaxAgeSeconds = *age
	}
	if idle != nil {
		r.MaxIdleSeconds = *idle
	}
	return &r, nil
}

// Template actions in a preview.
const (
	TemplateAdd       = "add"       // setting does not exist at the destination yet
	TemplateChange    = "change"    // destination value will be replaced
	TemplateUnchanged = "unchanged" // destination already matches
	TemplatePreserved = "preserved" // destination-specific, never overwritten
	TemplateBlocked   = "blocked"   // cannot be applied; the reason says why
	TemplateInfo      = "info"      // informational only
)

// TemplateChange is one reviewed difference.
type TemplateDiff struct {
	Section  string `json:"section"`
	Field    string `json:"field"`
	Action   string `json:"action"`
	Current  any    `json:"current,omitempty"`
	Proposed any    `json:"proposed,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// TemplatePreview is the read-only diff of a template against the destination
// feed. Digest binds the template and the diff: applying requires the digest
// of the preview that was reviewed, and fails if either side changed since.
type TemplatePreview struct {
	Digest     string         `json:"digest"`
	Template   Template       `json:"template"`
	Diffs      []TemplateDiff `json:"diffs"`
	Applicable int            `json:"applicable"`
	Blocked    int            `json:"blocked"`
}

// TemplateApplyResult reports what an apply changed.
type TemplateApplyResult struct {
	Digest  string         `json:"digest"`
	Applied int            `json:"applied"`
	Diffs   []TemplateDiff `json:"diffs"`
}

// templatePlan is a computed diff plus the concrete writes it implies.
type templatePlan struct {
	diffs      []TemplateDiff
	retention  *TemplateRetention // nil: leave alone
	remoteCfgs []plannedRemote
}
type plannedRemote struct {
	sourceID string
	revision int64
	config   remoteconfig.Config
	name     string
}

func diffField(section, field string, current, proposed any, exists bool) TemplateDiff {
	d := TemplateDiff{Section: section, Field: field, Current: current, Proposed: proposed}
	switch {
	case !exists:
		d.Action = TemplateAdd
	case reflect.DeepEqual(current, proposed):
		d.Action = TemplateUnchanged
	default:
		d.Action = TemplateChange
	}
	return d
}

// planTemplate diffs tpl against the destination feed using q (a pool for the
// read-only preview, a transaction for apply, so the check and the writes see
// one snapshot under the feed lock).
func (s *Service) planTemplate(ctx context.Context, q queryer, instance, destFeed string, tpl Template) (templatePlan, error) {
	var plan templatePlan
	var format string
	if err := q.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2`, instance, destFeed).Scan(&format); err != nil {
		return plan, dbError(err)
	}
	add := func(d TemplateDiff) { plan.diffs = append(plan.diffs, d) }
	if tpl.Format != format {
		add(TemplateDiff{Section: "feed", Field: "format", Action: TemplateBlocked, Current: format, Proposed: tpl.Format, Reason: "template format does not match the destination feed; nothing from it can be applied"})
		return plan, nil
	}
	for _, field := range []string{"slug", "anonymous_read"} {
		add(TemplateDiff{Section: "feed", Field: field, Action: TemplatePreserved, Reason: "destination-specific feed identity and access policy"})
	}
	add(TemplateDiff{Section: "access", Field: "grants_and_publish_policy", Action: TemplatePreserved, Reason: "destination access policy and identity settings are never copied"})

	if r := tpl.Retention; r != nil {
		cur, err := loadRetentionTemplate(ctx, q, instance, destFeed)
		if err != nil {
			return plan, err
		}
		ds := []TemplateDiff{}
		field := func(name string, c, p any) {
			ds = append(ds, diffField("retention", name, c, p, cur != nil))
		}
		var c TemplateRetention
		if cur != nil {
			c = *cur
		}
		field("keep_last", c.KeepLast, r.KeepLast)
		field("max_age_seconds", c.MaxAgeSeconds, r.MaxAgeSeconds)
		field("max_idle_seconds", c.MaxIdleSeconds, r.MaxIdleSeconds)
		field("min_keep", c.MinKeep, r.MinKeep)
		field("include_tagged", c.IncludeTagged, r.IncludeTagged)
		field("enabled", c.Enabled, r.Enabled)
		changed := false
		for _, d := range ds {
			if d.Action == TemplateAdd || d.Action == TemplateChange {
				changed = true
			}
		}
		plan.diffs = append(plan.diffs, ds...)
		add(TemplateDiff{Section: "retention", Field: "allow_release_delete", Action: TemplatePreserved, Reason: "destination decides whether release content may be deleted"})
		if changed {
			plan.retention = r
		}
	}

	rows, err := q.Query(ctx, `SELECT s.name,s.id::text FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind='remote'`, instance, destFeed)
	if err != nil {
		return plan, ErrUnavailable
	}
	local := map[string]string{}
	for rows.Next() {
		var name, id string
		if err = rows.Scan(&name, &id); err != nil {
			rows.Close()
			return plan, ErrUnavailable
		}
		local[name] = id
	}
	rows.Close()
	if rows.Err() != nil {
		return plan, ErrUnavailable
	}
	store := remoteconfig.Store{InstanceID: instance}
	remotes := append([]TemplateRemote(nil), tpl.RemoteSources...)
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	for _, tr := range remotes {
		section := "remote_source:" + tr.Name
		id, ok := local[tr.Name]
		if !ok {
			add(TemplateDiff{Section: section, Field: "source", Action: TemplateBlocked, Reason: "the destination feed has no remote source with this name; its endpoint and credentials are destination-specific, so create it locally first"})
			continue
		}
		rec, err := store.LoadTx(ctx, q, id)
		if err != nil {
			if errors.Is(err, remoteconfig.ErrNotFound) {
				add(TemplateDiff{Section: section, Field: "source", Action: TemplateBlocked, Reason: "the destination source has no remote configuration yet"})
				continue
			}
			return plan, ErrUnavailable
		}
		cfg := rec.Config
		cfg.TimeoutSeconds = tr.TimeoutSeconds
		cfg.Cache = tr.Cache
		if _, err := remoteconfig.Normalize(cfg); err != nil {
			add(TemplateDiff{Section: section, Field: "config", Action: TemplateBlocked, Reason: "the template values are outside the allowed range"})
			continue
		}
		before := len(plan.diffs)
		add(diffField(section, "timeout_seconds", rec.Config.TimeoutSeconds, tr.TimeoutSeconds, true))
		for _, f := range []struct {
			name string
			c, p any
		}{
			{"cache.metadata_ttl_seconds", rec.Config.Cache.MetadataTTLSeconds, tr.Cache.MetadataTTLSeconds},
			{"cache.negative_ttl_seconds", rec.Config.Cache.NegativeTTLSeconds, tr.Cache.NegativeTTLSeconds},
			{"cache.stale_if_error_seconds", rec.Config.Cache.StaleIfErrorSeconds, tr.Cache.StaleIfErrorSeconds},
			{"cache.max_entries", rec.Config.Cache.MaxEntries, tr.Cache.MaxEntries},
			{"cache.max_bytes", rec.Config.Cache.MaxBytes, tr.Cache.MaxBytes},
			{"cache.max_metadata_bytes", rec.Config.Cache.MaxMetadataBytes, tr.Cache.MaxMetadataBytes},
		} {
			add(diffField(section, f.name, f.c, f.p, true))
		}
		for _, field := range []string{"origin", "redirect_origins", "credential", "ca_ref", "allow_private"} {
			add(TemplateDiff{Section: section, Field: field, Action: TemplatePreserved, Reason: "destination-specific endpoint, credential and trust settings"})
		}
		changed := false
		for _, d := range plan.diffs[before:] {
			if d.Action == TemplateChange {
				changed = true
			}
		}
		if changed {
			plan.remoteCfgs = append(plan.remoteCfgs, plannedRemote{sourceID: id, revision: rec.Revision, config: cfg, name: tr.Name})
		}
	}
	names := map[string]bool{}
	srows, err := q.Query(ctx, `SELECT s.name FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2`, instance, destFeed)
	if err != nil {
		return plan, ErrUnavailable
	}
	for srows.Next() {
		var n string
		if err = srows.Scan(&n); err != nil {
			srows.Close()
			return plan, ErrUnavailable
		}
		names[n] = true
	}
	srows.Close()
	for _, ts := range tpl.Sources {
		if !names[ts.Name] {
			add(TemplateDiff{Section: "sources", Field: ts.Name, Action: TemplateInfo, Proposed: fmt.Sprintf("%s source at priority %d", ts.Kind, ts.Priority), Reason: "source layout is informational and never applied"})
		}
	}
	return plan, nil
}

func templateDigest(tpl Template, diffs []TemplateDiff) string {
	b, _ := json.Marshal(struct {
		Template Template
		Diffs    []TemplateDiff
	}{tpl, diffs})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func summarize(plan templatePlan) (applicable, blocked int) {
	for _, d := range plan.diffs {
		switch d.Action {
		case TemplateAdd, TemplateChange:
			applicable++
		case TemplateBlocked:
			blocked++
		}
	}
	return
}

// loadTemplate returns the supplied template, or fetches the peer's current one.
func (s *Service) loadTemplate(ctx context.Context, instance, jobID string, supplied *Template) (Job, Template, error) {
	job, err := s.GetJob(ctx, instance, jobID)
	if err != nil {
		return Job{}, Template{}, err
	}
	if supplied != nil {
		if !supplied.valid() {
			return Job{}, Template{}, ErrInput
		}
		return job, *supplied, nil
	}
	client, err := s.peerFor(ctx, job.PeerID)
	if err != nil {
		return Job{}, Template{}, err
	}
	tpl, err := client.Template(ctx, job.RemoteFeedID)
	if err != nil {
		return Job{}, Template{}, err
	}
	if !tpl.valid() {
		return Job{}, Template{}, fmt.Errorf("%w: invalid template from peer", ErrPeer)
	}
	return job, tpl, nil
}

// PreviewTemplate diffs a template (supplied, or fetched from the job's peer
// for its remote feed) against the job's destination feed. It changes nothing.
func (s *Service) PreviewTemplate(ctx context.Context, instance, jobID string, supplied *Template) (TemplatePreview, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	job, tpl, err := s.loadTemplate(ctx, instance, jobID, supplied)
	if err != nil {
		return TemplatePreview{}, err
	}
	plan, err := s.planTemplate(ctx, s.cfg.DB, instance, job.DestFeedID, tpl)
	if err != nil {
		return TemplatePreview{}, err
	}
	applicable, blocked := summarize(plan)
	if plan.diffs == nil {
		plan.diffs = []TemplateDiff{}
	}
	return TemplatePreview{Digest: templateDigest(tpl, plan.diffs), Template: tpl, Diffs: plan.diffs, Applicable: applicable, Blocked: blocked}, nil
}

// FetchTemplate resolves the template an apply will use, before the mutation
// transaction opens (it may call the peer).
func (s *Service) FetchTemplate(ctx context.Context, instance, jobID string, supplied *Template) (Job, Template, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return s.loadTemplate(ctx, instance, jobID, supplied)
}

// ApplyTemplateTx applies a reviewed template inside the caller's transaction.
// The plan is recomputed under the feed lock and must produce exactly the
// reviewed digest; otherwise nothing is written (ErrConflict). Only retention
// tunables and remote-source cache/timeout tunables are written; every
// destination-specific setting is untouched. Blocked entries make the apply
// refuse rather than partially adopt a template that cannot be fully applied.
func (s *Service) ApplyTemplateTx(ctx context.Context, tx pgx.Tx, p identity.Principal, policy identity.Policy, job Job, tpl Template, reviewedDigest string) (TemplateApplyResult, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, job.DestFeedID); err != nil {
		return TemplateApplyResult{}, ErrUnavailable
	}
	plan, err := s.planTemplate(ctx, tx, p.InstanceID, job.DestFeedID, tpl)
	if err != nil {
		return TemplateApplyResult{}, err
	}
	if plan.diffs == nil {
		plan.diffs = []TemplateDiff{}
	}
	digest := templateDigest(tpl, plan.diffs)
	if digest != reviewedDigest {
		return TemplateApplyResult{}, ErrConflict
	}
	applicable, blocked := summarize(plan)
	if blocked > 0 {
		return TemplateApplyResult{}, ErrConflict
	}
	if r := plan.retention; r != nil {
		var keep, age, idle any
		if r.KeepLast > 0 {
			keep = r.KeepLast
		}
		if r.MaxAgeSeconds > 0 {
			age = r.MaxAgeSeconds
		}
		if r.MaxIdleSeconds > 0 {
			idle = r.MaxIdleSeconds
		}
		// allow_release_delete is deliberately absent from the update set and is
		// false for a newly created policy.
		if _, err := tx.Exec(ctx, `INSERT INTO retention_policies(instance_id,feed_id,keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,allow_release_delete,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,false,$8)
 ON CONFLICT(instance_id,feed_id) WHERE feed_id IS NOT NULL DO UPDATE SET keep_last=excluded.keep_last,max_age_seconds=excluded.max_age_seconds,max_idle_seconds=excluded.max_idle_seconds,min_keep=excluded.min_keep,include_tagged=excluded.include_tagged,enabled=excluded.enabled,revision=retention_policies.revision+1,updated_at=now()`,
			p.InstanceID, job.DestFeedID, keep, age, idle, r.MinKeep, r.IncludeTagged, r.Enabled); err != nil {
			return TemplateApplyResult{}, dbError(err)
		}
	}
	allowed := make([]string, 0, len(s.cfg.CAs))
	for id := range s.cfg.CAs {
		allowed = append(allowed, id)
	}
	store := remoteconfig.Store{DB: s.cfg.DB, InstanceID: p.InstanceID, AllowedCARefs: allowed}
	for _, rc := range plan.remoteCfgs {
		if _, err := store.SetTx(ctx, tx, p, policy, rc.sourceID, rc.revision, rc.config); err != nil {
			switch {
			case errors.Is(err, remoteconfig.ErrConflict):
				return TemplateApplyResult{}, ErrConflict
			case errors.Is(err, remoteconfig.ErrDenied), errors.Is(err, remoteconfig.ErrNotFound):
				return TemplateApplyResult{}, ErrDenied
			case errors.Is(err, remoteconfig.ErrInput):
				return TemplateApplyResult{}, ErrInput
			}
			return TemplateApplyResult{}, ErrUnavailable
		}
	}
	if err := audit(ctx, tx, p.InstanceID, p.PrincipalID, "replication.template.apply", job.ID, map[string]any{"digest": digest, "applied": applicable, "dest_feed_id": job.DestFeedID}); err != nil {
		return TemplateApplyResult{}, ErrUnavailable
	}
	return TemplateApplyResult{Digest: digest, Applied: applicable, Diffs: plan.diffs}, nil
}
