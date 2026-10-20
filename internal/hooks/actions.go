package hooks

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/prewarm"
	"github.com/jackc/pgx/v5"
)

// audit records what a hook did in the caller's transaction (audit row plus
// outbox event), attributed to the principal that created the hook.
func (s *Service) audit(ctx context.Context, tx pgx.Tx, hook Hook, topic, resource string, details map[string]any) error {
	details["hook"] = hook.Slug
	return events.Emit(ctx, tx, s.cfg.InstanceID, hook.CreatedBy, topic, resource, details)
}

func render(tpl string, doc any) (string, error) {
	if tpl == "" {
		return "", nil
	}
	return Render(tpl, doc)
}

func (s *Service) doPrewarm(ctx context.Context, hook Hook, c claimed) outcome {
	a := hook.Action
	doc := decodePayload(c.payload)
	name, err := render(a.Name, doc)
	if err != nil {
		return failOutcome("template_error", err.Error(), false)
	}
	version, err := render(a.Version, doc)
	if err != nil {
		return failOutcome("template_error", err.Error(), false)
	}
	kind := prewarm.KindPackages
	if a.Kind == "image" {
		kind = prewarm.KindImages
	}
	run, err := s.startPrewarm(ctx, hook, c.id, a.FeedID, kind, prewarm.Item{Name: name, Version: version})
	if err != nil {
		return s.mapPrewarmError(err)
	}
	return okOutcome("prewarm_started", map[string]any{"feed_id": a.FeedID, "name": name, "version": version, "job_id": run.JobID, "run_id": run.ID})
}

func (s *Service) mapPrewarmError(err error) outcome {
	switch {
	case errors.Is(err, prewarm.ErrActive):
		return failOutcome("prewarm_busy", "a prewarm run for this delivery is still active", true)
	case errors.Is(err, errNoFeed):
		return failOutcome("feed_not_found", "the feed no longer exists", false)
	case errors.Is(err, errBadItem):
		return failOutcome("invalid_item", strings.TrimPrefix(err.Error(), errBadItem.Error()+": "), false)
	}
	return failOutcome("storage_unavailable", "could not queue the prewarm run", true)
}

var (
	errNoFeed  = errors.New("feed not found")
	errBadItem = errors.New("invalid prewarm item")
)

// startPrewarm creates a one-off prewarm job (named hook:<slug>:<delivery>) with
// one item and queues its run, exactly like POST /prewarm/jobs with run_now.
func (s *Service) startPrewarm(ctx context.Context, hook Hook, delivery, feed, kind string, item prewarm.Item) (prewarm.Run, error) {
	var format string
	if err := s.cfg.DB.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, feed).Scan(&format); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return prewarm.Run{}, errNoFeed
		}
		return prewarm.Run{}, err
	}
	if err := prewarm.CheckKind(format, kind, ""); err != nil {
		return prewarm.Run{}, errors.Join(errBadItem, err)
	}
	items, err := prewarm.ValidateItems(format, kind, []prewarm.Item{item})
	if err != nil {
		return prewarm.Run{}, errors.Join(errBadItem, err)
	}
	tx, err := s.cfg.DB.Begin(ctx)
	if err != nil {
		return prewarm.Run{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	job, err := prewarm.InsertJob(ctx, tx, s.cfg.InstanceID, prewarm.NewJob{FeedID: feed, Name: "hook:" + hook.Slug + ":" + delivery[:8], Spec: prewarm.Spec{Kind: kind, Items: items},
		Concurrency: prewarm.DefaultWorkers, RatePerSecond: prewarm.DefaultRate, Enabled: true, CreatedBy: hook.CreatedBy})
	if err != nil {
		return prewarm.Run{}, err
	}
	run, err := prewarm.StartRun(ctx, tx, s.cfg.InstanceID, job.ID, "manual", hook.CreatedBy, "")
	if err != nil {
		return prewarm.Run{}, err
	}
	if err = s.audit(ctx, tx, hook, "incoming_hook.prewarm", job.ID, map[string]any{"feed_id": feed, "name": item.Name, "version": item.Version, "run_id": run.ID}); err != nil {
		return prewarm.Run{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return prewarm.Run{}, err
	}
	if s.cfg.Backend.NotifyPrewarm != nil {
		s.cfg.Backend.NotifyPrewarm()
	}
	return run, nil
}

func (s *Service) doReplication(ctx context.Context, hook Hook, _ claimed) outcome {
	if s.cfg.Backend.TriggerReplication == nil {
		return failOutcome("unavailable", "replication is not available on this instance", false)
	}
	err := s.cfg.Backend.TriggerReplication(ctx, s.cfg.InstanceID, hook.CreatedBy, hook.Action.JobID)
	switch {
	case err == nil:
	case errors.Is(err, ErrTargetNotFound):
		return failOutcome("job_not_found", "the replication job does not exist or is disabled", false)
	case errors.Is(err, ErrTargetBusy):
		return failOutcome("busy", "the replication job is busy", true)
	default:
		return failOutcome("storage_unavailable", "could not request the replication run", true)
	}
	_ = s.auditStandalone(ctx, hook, "incoming_hook.replication_run", hook.Action.JobID, map[string]any{"job_id": hook.Action.JobID})
	return okOutcome("replication_requested", map[string]any{"job_id": hook.Action.JobID})
}

func (s *Service) auditStandalone(ctx context.Context, hook Hook, topic, resource string, details map[string]any) error {
	tx, err := s.cfg.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = s.audit(ctx, tx, hook, topic, resource, details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) doPurge(ctx context.Context, hook Hook, c claimed) outcome {
	a := hook.Action
	coordinate, err := render(a.Coordinate, decodePayload(c.payload))
	if err != nil {
		return failOutcome("template_error", err.Error(), false)
	}
	tx, err := s.cfg.DB.Begin(ctx)
	if err != nil {
		return failOutcome("storage_unavailable", "storage unavailable", true)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	res, err := artifact.PurgeRemoteCacheTx(ctx, tx, s.cfg.InstanceID, a.SourceID, coordinate)
	switch {
	case err == nil:
	case errors.Is(err, artifact.ErrNotFound):
		return failOutcome("source_not_found", "the remote source no longer exists", false)
	case errors.Is(err, artifact.ErrNotRemote):
		return failOutcome("not_remote", "the source is not a remote source", false)
	default:
		return failOutcome("storage_unavailable", "storage unavailable", true)
	}
	details := map[string]any{"source_id": a.SourceID, "coordinate": coordinate, "components": res.Components, "versions": res.Versions, "assets": res.Assets, "bytes": res.Bytes}
	// Same audit action as DELETE /api/v1/sources/{id}/cache.
	if err = s.audit(ctx, tx, hook, "source.cache.purge", a.SourceID, map[string]any{"coordinate": coordinate, "components": res.Components, "versions": res.Versions, "assets": res.Assets, "bytes": res.Bytes}); err != nil {
		return failOutcome("storage_unavailable", "storage unavailable", true)
	}
	if err = tx.Commit(ctx); err != nil {
		return failOutcome("storage_unavailable", "storage unavailable", true)
	}
	return okOutcome("cache_purged", details)
}

func (s *Service) doCleanCache(ctx context.Context, hook Hook, c claimed) outcome {
	if s.cfg.Backend.CleanCache == nil {
		return failOutcome("unavailable", "this instance is not serving artifacts", false)
	}
	prefix, err := render(hook.Action.Prefix, decodePayload(c.payload))
	if err != nil {
		return failOutcome("template_error", err.Error(), false)
	}
	if prefix == "" || len(prefix) > 512 {
		return failOutcome("template_error", "the prefix template produced an empty or oversized value", false)
	}
	res, err := s.cfg.Backend.CleanCache(ctx, identity.Principal{InstanceID: s.cfg.InstanceID, PrincipalID: hook.CreatedBy}, hook.Action.FeedID, prefix)
	switch {
	case err == nil:
	case errors.Is(err, ErrTargetNotFound):
		return failOutcome("feed_not_found", "the cache feed does not exist or is not a cache feed", false)
	case errors.Is(err, ErrTargetDenied):
		return failOutcome("denied", "the hook creator no longer has delete permission on the feed", false)
	case errors.Is(err, ErrTargetBusy):
		return failOutcome("busy", "the feed is busy", true)
	default:
		return failOutcome("unavailable", "cache cleanup unavailable", true)
	}
	return okOutcome("cache_cleaned", map[string]any{"feed_id": hook.Action.FeedID, "prefix": prefix, "matched": res.Matched, "tags_removed": res.Tags, "versions_removed": res.Versions, "components_removed": res.Components, "bytes_released": res.Bytes, "truncated": res.Truncated})
}

type remoteSource struct{ id, format, origin string }

func (s *Service) remoteSources(ctx context.Context) ([]remoteSource, error) {
	rows, err := s.cfg.DB.Query(ctx, `SELECT s.id::text,s.format,COALESCE(c.config->>'origin','') FROM sources s JOIN remote_source_configs c ON c.instance_id=s.instance_id AND c.source_id=s.id WHERE s.instance_id=$1 AND s.kind='remote' ORDER BY s.name`, s.cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []remoteSource
	for rows.Next() {
		var r remoteSource
		if err = rows.Scan(&r.id, &r.format, &r.origin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func originHost(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// matchSources selects the remote sources an upstream change applies to: the
// sources named in the action when it lists any, else those whose origin host
// is one of the action's origins, else those matching the hosts the provider or
// payload implies. A container change applies only to OCI sources and any other
// change only to non-OCI sources, unless sources are named explicitly.
func matchSources(all []remoteSource, a Action, ch Change) []remoteSource {
	var out []remoteSource
	if len(a.SourceIDs) > 0 {
		want := map[string]bool{}
		for _, id := range a.SourceIDs {
			want[id] = true
		}
		for _, r := range all {
			if want[r.id] {
				out = append(out, r)
			}
		}
		return out
	}
	hosts := ch.Origins
	if len(a.Origins) > 0 {
		hosts = a.Origins
	}
	set := map[string]bool{}
	for _, h := range hosts {
		set[strings.ToLower(h)] = true
	}
	for _, r := range all {
		if !set[originHost(r.origin)] {
			continue
		}
		if (ch.Kind == "image") != (r.format == "oci") {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *Service) doUpstream(ctx context.Context, hook Hook, c claimed) outcome {
	a := hook.Action
	ch, err := ParseUpstream(a.Provider, headerOf(c.headers), c.payload)
	if err != nil {
		return failOutcome("unrecognized_payload", "the payload is not a notification of the configured provider", false)
	}
	detail := map[string]any{"provider": ch.Provider, "event": ch.Event}
	if ch.Ignored != "" {
		detail["reason"] = ch.Ignored
		return okOutcome("ignored", detail)
	}
	detail["names"], detail["version"], detail["kind"] = ch.Names, ch.Version, ch.Kind
	all, err := s.remoteSources(ctx)
	if err != nil {
		return failOutcome("storage_unavailable", "storage unavailable", true)
	}
	matched := matchSources(all, a, ch)
	detail["sources_matched"] = len(matched)
	if len(matched) == 0 {
		return okOutcome("no_matching_source", detail)
	}
	dropped, runs, skipped := 0, 0, 0
	ids := make([]string, 0, len(matched))
	prewarmed := map[string]bool{}
	for _, src := range matched {
		ids = append(ids, src.id)
		if s.cfg.Backend.InvalidateMetadata != nil {
			dropped += s.cfg.Backend.InvalidateMetadata(src.id, ch.Names)
		}
		if !a.Prewarm {
			continue
		}
		rows, qerr := s.cfg.DB.Query(ctx, `SELECT f.id::text,f.format FROM feed_sources fs JOIN feeds f ON f.instance_id=fs.instance_id AND f.id=fs.feed_id WHERE fs.instance_id=$1 AND fs.source_id=$2 AND f.purpose='artifacts' AND ($3='' OR f.id::text=$3)`, s.cfg.InstanceID, src.id, a.FeedID)
		if qerr != nil {
			return failOutcome("storage_unavailable", "storage unavailable", true)
		}
		type feedRef struct{ id, format string }
		var feeds []feedRef
		for rows.Next() {
			var f feedRef
			if rows.Scan(&f.id, &f.format) == nil {
				feeds = append(feeds, f)
			}
		}
		rows.Close()
		for _, f := range feeds {
			if prewarmed[f.id] {
				continue
			}
			prewarmed[f.id] = true
			kind := prewarm.KindPackages
			if ch.Kind == "image" {
				kind = prewarm.KindImages
			}
			if !prewarm.Supported(f.format) || (ch.Kind == "image") != (f.format == "oci") || ch.Version == "" || len(ch.Names) == 0 {
				skipped++
				continue
			}
			run, perr := s.startPrewarm(ctx, hook, c.id+f.id, f.id, kind, prewarm.Item{Name: ch.Names[0], Version: ch.Version})
			if perr != nil {
				if errors.Is(perr, errBadItem) || errors.Is(perr, errNoFeed) || errors.Is(perr, prewarm.ErrActive) {
					skipped++
					continue
				}
				return failOutcome("storage_unavailable", "could not queue the prewarm run", true)
			}
			_ = run
			runs++
		}
	}
	detail["source_ids"], detail["entries_invalidated"] = ids, dropped
	if a.Prewarm {
		detail["prewarm_runs"], detail["prewarm_skipped"] = runs, skipped
	}
	_ = s.auditStandalone(ctx, hook, "incoming_hook.upstream_notify", ids[0], map[string]any{"provider": ch.Provider, "names": ch.Names, "version": ch.Version, "sources": len(matched), "invalidated": dropped, "prewarm_runs": runs})
	return okOutcome("cache_invalidated", detail)
}

// relayHeaders is the allowlist of original headers forwarded by a relay.
// Credentials (Authorization, X-Gitlab-Token, cookies) are never in it. The
// original body signature is forwarded only for a pass-through relay.
func relayHeaders(stored map[string]string, resign bool) http.Header {
	h := http.Header{}
	allowed := map[string]bool{}
	for _, n := range storedHeaders {
		allowed[http.CanonicalHeaderKey(n)] = true
	}
	if !resign {
		for _, n := range signatureHeaders {
			allowed[http.CanonicalHeaderKey(n)] = true
		}
	}
	for k, v := range stored {
		if allowed[http.CanonicalHeaderKey(k)] {
			h.Set(k, v)
		}
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}
	return h
}

func (s *Service) doRelay(ctx context.Context, hook Hook, c claimed) outcome {
	a := hook.Action
	header := relayHeaders(c.headers, a.ResignSecretID != "")
	header.Set("X-Ant-Hook", hook.Slug)
	header.Set("X-Ant-Delivery", c.id)
	var signing *events.Signing
	if a.ResignSecretID != "" {
		keys, err := s.signingKeys(ctx, a.ResignSecretID)
		if err != nil {
			return failOutcome("secret_unavailable", "the re-signing secret is unavailable", false)
		}
		defer clearKeys(keys)
		signing = &events.Signing{EventID: c.id, Keys: keys}
	}
	failure := s.sender.Post(ctx, a.TargetURL, c.payload, header, signing)
	detail := map[string]any{"target": relayTarget(a.TargetURL), "attempt": c.attempt, "resigned": signing != nil}
	switch failure {
	case "":
		return okOutcome("relayed", detail)
	case "invalid_destination":
		return outcome{result: "invalid_destination", err: "the target is not an allowed destination", detail: detail}
	}
	return outcome{result: failure, err: "the target did not accept the payload (" + failure + ")", retry: true, detail: detail}
}

// relayTarget is the target without path or query, for the log.
func relayTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
