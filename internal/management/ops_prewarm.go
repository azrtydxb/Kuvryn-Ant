package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/prewarm"
	"github.com/jackc/pgx/v5"
)

// PrewarmBackend is the running prewarm service (see internal/prewarm). Notify
// wakes its scheduler after a run was queued.
type PrewarmBackend interface{ Notify() }

// maxPrewarmBody bounds a request carrying a lockfile (a lockfile may be 8 MiB;
// JSON escaping needs some headroom).
const maxPrewarmBody = 12 << 20

// PrewarmPackage is one entry of a package list.
type PrewarmPackage struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// PrewarmLockfile is a lockfile uploaded as text.
type PrewarmLockfile struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// PrewarmSource says what to warm.
type PrewarmSource struct {
	Kind     string           `json:"kind"`
	Packages []PrewarmPackage `json:"packages,omitempty"`
	Lockfile *PrewarmLockfile `json:"lockfile,omitempty"`
	Images   []string         `json:"images,omitempty"`
	FeedID   string           `json:"feed_id,omitempty"`
}

// CreatePrewarmJob defines a job.
type CreatePrewarmJob struct {
	FeedID          string           `json:"feed_id"`
	Name            string           `json:"name"`
	Source          PrewarmSource    `json:"source"`
	Options         *prewarm.Options `json:"options,omitempty"`
	Concurrency     *int             `json:"concurrency,omitempty"`
	RatePerSecond   *int             `json:"rate_per_second,omitempty"`
	IntervalSeconds *int             `json:"interval_seconds,omitempty"`
	Enabled         *bool            `json:"enabled,omitempty"`
	RunNow          bool             `json:"run_now,omitempty"`
}

// PreviewPrewarm asks what a source would warm, without creating anything.
type PreviewPrewarm struct {
	FeedID string        `json:"feed_id"`
	Source PrewarmSource `json:"source"`
}

// PrewarmPreview is what a source expands to.
type PrewarmPreview struct {
	Total        int               `json:"total"`
	Items        []prewarm.Item    `json:"items"`
	Ignored      []prewarm.Ignored `json:"ignored"`
	IgnoredCount int               `json:"ignored_count"`
}

// PatchPrewarmJob changes a job. interval_seconds may be null to remove the schedule.
type PatchPrewarmJob struct {
	Name            *string          `json:"name,omitempty"`
	Enabled         *bool            `json:"enabled,omitempty"`
	Concurrency     *int             `json:"concurrency,omitempty"`
	RatePerSecond   *int             `json:"rate_per_second,omitempty"`
	IntervalSeconds json.RawMessage  `json:"interval_seconds,omitempty"`
	Options         *prewarm.Options `json:"options,omitempty"`
}

// PrewarmFormat lists what one feed format accepts.
type PrewarmFormat struct {
	Format    string   `json:"format"`
	Packages  bool     `json:"packages"`
	Images    bool     `json:"images"`
	Feed      bool     `json:"feed"`
	Lockfiles []string `json:"lockfiles"`
}

func prewarmJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPrewarmBody))
	d.DisallowUnknownFields()
	if err = d.Decode(dst); err == nil {
		if _, perr := d.Token(); perr == io.EOF {
			return true
		}
		err = errors.New("trailing data")
	}
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		Error(w, r, 413, api.ErrorCodeInvalidRequest, "request body too large")
	} else {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid JSON body")
	}
	return false
}

func (h opsHandlers) prewarmDown(w http.ResponseWriter, r *http.Request) bool {
	if h.d.Prewarm == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm is not available on this instance")
		return true
	}
	return false
}

// prewarmFeed resolves a feed and checks the caller's permission. Feeds the
// caller cannot read answer 404; a reader without the permission gets 403.
func (h opsHandlers) prewarmFeed(w http.ResponseWriter, r *http.Request, p identity.Principal, policy identity.Policy, id string, action identity.Action) (format string, ok bool) {
	if !feedAllowed(policy, p, id, identity.Read) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return "", false
	}
	if action != identity.Read && !feedAllowed(policy, p, id, action) {
		Error(w, r, 403, api.ErrorCodeForbidden, "prewarm requires permission to configure the feed")
		return "", false
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	if err := h.db.QueryRow(ctx, `SELECT format FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, id).Scan(&format); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		} else {
			Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
		}
		return "", false
	}
	return format, true
}

func (h opsHandlers) listPrewarmFormats(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.policy(w, r); !ok {
		return
	}
	out := []PrewarmFormat{}
	info := prewarm.Formats()
	for _, name := range prewarm.SupportedFormats() {
		f := info[name]
		lock := f.Lockfiles
		if lock == nil {
			lock = []string{}
		}
		out = append(out, PrewarmFormat{Format: name, Packages: f.Packages, Images: f.Images, Feed: f.Feed, Lockfiles: lock})
	}
	JSON(w, 200, struct {
		Items []PrewarmFormat `json:"items"`
	}{out})
}

// resolveSource validates a source against the target feed and expands it.
func (h opsHandlers) resolveSource(w http.ResponseWriter, r *http.Request, p identity.Principal, policy identity.Policy, format string, s PrewarmSource) (prewarm.Spec, string, bool) {
	bad := func(msg string) (prewarm.Spec, string, bool) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, msg)
		return prewarm.Spec{}, "", false
	}
	spec := prewarm.Spec{Kind: s.Kind}
	lockType := ""
	if s.Lockfile != nil {
		lockType = s.Lockfile.Type
	}
	if err := prewarm.CheckKind(format, s.Kind, lockType); err != nil {
		return bad(err.Error())
	}
	var err error
	switch s.Kind {
	case prewarm.KindPackages:
		if s.Lockfile != nil || len(s.Images) > 0 || s.FeedID != "" {
			return bad("source.packages cannot be combined with other source fields")
		}
		in := make([]prewarm.Item, len(s.Packages))
		for i, pk := range s.Packages {
			in[i] = prewarm.Item{Name: pk.Name, Version: pk.Version}
		}
		spec.Items, err = prewarm.ValidateItems(format, prewarm.KindPackages, in)
	case prewarm.KindImages:
		if s.Lockfile != nil || len(s.Packages) > 0 || s.FeedID != "" {
			return bad("source.images cannot be combined with other source fields")
		}
		in := make([]prewarm.Item, len(s.Images))
		for i, ref := range s.Images {
			in[i] = prewarm.ParseImageRef(ref)
		}
		spec.Items, err = prewarm.ValidateItems(format, prewarm.KindImages, in)
	case prewarm.KindLockfile:
		if len(s.Packages) > 0 || len(s.Images) > 0 || s.FeedID != "" {
			return bad("source.lockfile cannot be combined with other source fields")
		}
		spec.LockfileType = s.Lockfile.Type
		var items []prewarm.Item
		var ignored []prewarm.Ignored
		if items, ignored, err = prewarm.ParseLockfile(s.Lockfile.Type, []byte(s.Lockfile.Content)); err == nil {
			spec.Items, spec.Ignored = prewarm.FilterLockfileItems(format, items, ignored)
			if len(spec.Items) == 0 {
				err = errors.New("the lockfile lists nothing that can be warmed from a registry")
			}
		}
	case prewarm.KindFeed:
		if s.Lockfile != nil || len(s.Packages) > 0 || len(s.Images) > 0 {
			return bad("source.feed cannot be combined with other source fields")
		}
		id, ok := feedID(s.FeedID)
		if !ok {
			return bad("source.feed_id must be a feed ID")
		}
		sourceFormat, ok := h.prewarmFeedFormat(w, r, p, policy, id)
		if !ok {
			return prewarm.Spec{}, "", false
		}
		if sourceFormat != format {
			return bad("the source feed must have the same format as the feed being warmed")
		}
		spec.SourceFeedID = id
	}
	if err != nil {
		return bad(err.Error())
	}
	return spec, lockType, true
}

// prewarmFeedFormat reads the format of a feed the caller can read.
func (h opsHandlers) prewarmFeedFormat(w http.ResponseWriter, r *http.Request, p identity.Principal, policy identity.Policy, id string) (string, bool) {
	return h.prewarmFeed(w, r, p, policy, id, identity.Read)
}

func (h opsHandlers) hasRemote(ctx context.Context, instance, feed string) bool {
	var ok bool
	err := h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind='remote')`, instance, feed).Scan(&ok)
	return err == nil && ok
}

func validJobName(s string) bool { return validSourceName(s) }

func (h opsHandlers) previewPrewarm(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	var in PreviewPrewarm
	if !prewarmJSON(w, r, &in) {
		return
	}
	id, valid := feedID(in.FeedID)
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "feed_id must be a feed ID")
		return
	}
	format, ok := h.prewarmFeed(w, r, p, policy, id, identity.Configure)
	if !ok {
		return
	}
	spec, _, ok := h.resolveSource(w, r, p, policy, format, in.Source)
	if !ok {
		return
	}
	out := PrewarmPreview{Items: []prewarm.Item{}, Ignored: []prewarm.Ignored{}}
	if spec.Kind == prewarm.KindFeed {
		ctx, cancel := h.ctx(r)
		defer cancel()
		var n int
		if err := h.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id JOIN versions v ON v.component_id=c.id WHERE fs.instance_id=$1 AND fs.feed_id=$2 GROUP BY c.coordinate,v.version LIMIT $3) x`, p.InstanceID, spec.SourceFeedID, prewarm.MaxItems+1).Scan(&n); err != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
			return
		}
		out.Total = n
	} else {
		out.Total = len(spec.Items)
		out.Items = spec.Items[:min(len(spec.Items), 100)]
	}
	out.IgnoredCount = len(spec.Ignored)
	out.Ignored = spec.Ignored[:min(len(spec.Ignored), 100)]
	JSON(w, 200, out)
}

func (h opsHandlers) createPrewarmJob(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	var in CreatePrewarmJob
	if !prewarmJSON(w, r, &in) {
		return
	}
	feed, valid := feedID(in.FeedID)
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "feed_id must be a feed ID")
		return
	}
	format, ok := h.prewarmFeed(w, r, p, policy, feed, identity.Configure)
	if !ok || h.prewarmDown(w, r) {
		return
	}
	n := prewarm.NewJob{FeedID: feed, Name: in.Name, Concurrency: prewarm.DefaultWorkers, RatePerSecond: prewarm.DefaultRate, Enabled: true, CreatedBy: p.PrincipalID, IntervalSeconds: in.IntervalSeconds}
	if !validJobName(in.Name) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "name must be 1-200 printable characters")
		return
	}
	if in.Concurrency != nil {
		n.Concurrency = *in.Concurrency
	}
	if in.RatePerSecond != nil {
		n.RatePerSecond = *in.RatePerSecond
	}
	if in.Enabled != nil {
		n.Enabled = *in.Enabled
	}
	if in.Options != nil {
		n.Options = *in.Options
	}
	if msg := validPrewarmTuning(n.Concurrency, n.RatePerSecond, n.IntervalSeconds, n.Options); msg != "" {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, msg)
		return
	}
	spec, _, ok := h.resolveSource(w, r, p, policy, format, in.Source)
	if !ok {
		return
	}
	n.Spec = spec
	ctx, cancel := h.ctx(r)
	defer cancel()
	if !h.hasRemote(ctx, p.InstanceID, feed) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "the feed has no remote source to warm")
		return
	}
	fingerprint := struct {
		In          CreatePrewarmJob
		LockfileSHA string
	}{in, ""}
	if fingerprint.In.Source.Lockfile != nil {
		sum := sha256.Sum256([]byte(in.Source.Lockfile.Content))
		fingerprint.LockfileSHA = hex.EncodeToString(sum[:])
		lock := *in.Source.Lockfile
		lock.Content = ""
		fingerprint.In.Source.Lockfile = &lock
	}
	var started bool
	h.mutateCfg(w, r, p, "prewarm.job.create", "prewarm/jobs", fingerprint, 201, func(ctx context.Context, tx pgx.Tx) (any, error) {
		job, err := prewarm.InsertJob(ctx, tx, p.InstanceID, n)
		if err != nil {
			return nil, err
		}
		if in.RunNow {
			run, err := prewarm.StartRun(ctx, tx, p.InstanceID, job.ID, "manual", p.PrincipalID, "")
			if err != nil {
				return nil, prewarmStartError(err)
			}
			job.LastRun, started = &run, true
		}
		return job, audited(ctx, tx, p, job.ID, "prewarm.job.create", job.Revision, map[string]any{"feed_id": feed, "kind": n.Spec.Kind, "items": len(n.Spec.Items), "scheduled": n.IntervalSeconds != nil, "run_now": in.RunNow})
	})
	if started {
		h.d.Prewarm.Notify()
	}
}

func validPrewarmTuning(concurrency, rate int, interval *int, o prewarm.Options) string {
	switch {
	case concurrency < 1 || concurrency > prewarm.MaxWorkers:
		return "concurrency must be between 1 and " + strconv.Itoa(prewarm.MaxWorkers)
	case rate < 0 || rate > prewarm.MaxRate:
		return "rate_per_second must be between 0 (unlimited) and " + strconv.Itoa(prewarm.MaxRate)
	case interval != nil && (*interval < prewarm.MinInterval || *interval > prewarm.MaxInterval):
		return "interval_seconds must be between " + strconv.Itoa(prewarm.MinInterval) + " and " + strconv.Itoa(prewarm.MaxInterval)
	}
	if err := o.Validate(); err != nil {
		return err.Error()
	}
	return ""
}

// prewarmStartError maps run-creation failures to client errors inside a mutation.
func prewarmStartError(err error) error {
	switch {
	case errors.Is(err, prewarm.ErrActive):
		return conflictConfig("the job already has an active run")
	case errors.Is(err, prewarm.ErrEmpty):
		return invalidConfig("there is nothing to warm: the source lists no items")
	case errors.Is(err, prewarm.ErrTooMany):
		return invalidConfig("the source references more than 20000 items")
	}
	return err
}

// prewarmJobFeed loads a job and authorizes the caller on its feed.
func (h opsHandlers) prewarmJob(w http.ResponseWriter, r *http.Request, p identity.Principal, policy identity.Policy, id string, action identity.Action) (prewarm.Job, bool) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	job, err := prewarm.GetJob(ctx, h.db, p.InstanceID, id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			Error(w, r, 404, api.ErrorCodeNotFound, "prewarm job not found")
		} else {
			Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
		}
		return job, false
	}
	if _, ok := h.prewarmFeed(w, r, p, policy, job.FeedID, action); !ok {
		return job, false
	}
	return job, true
}

func (h opsHandlers) listPrewarmJobs(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "limit" && key != "feed_id") || len(values) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid query")
			return
		}
	}
	after, limit, ok := identityPage(w, r2(r, "feed_id"))
	if !ok {
		return
	}
	feed := ""
	if q.Has("feed_id") {
		var valid bool
		if feed, valid = feedID(q.Get("feed_id")); !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed_id")
			return
		}
		if _, ok = h.prewarmFeed(w, r, p, policy, feed, identity.Read); !ok {
			return
		}
	} else if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		Error(w, r, 403, api.ErrorCodeForbidden, "list the jobs of a feed (feed_id) or use an instance administrator")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	jobs, err := prewarm.ListJobs(ctx, h.db, p.InstanceID, feed, after, limit+1)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
		return
	}
	next := ""
	if len(jobs) > limit {
		jobs = jobs[:limit]
		next = jobs[len(jobs)-1].ID
	}
	if jobs == nil {
		jobs = []prewarm.Job{}
	}
	JSON(w, 200, struct {
		Items []prewarm.Job `json:"items"`
		Next  string        `json:"next_after,omitempty"`
	}{jobs, next})
}

// r2 returns a copy of the request whose query lacks the named parameters, so a
// shared pager can validate the remaining ones.
func r2(r *http.Request, drop ...string) *http.Request {
	c := r.Clone(r.Context())
	q := c.URL.Query()
	for _, k := range drop {
		q.Del(k)
	}
	c.URL.RawQuery = q.Encode()
	return c
}

func (h opsHandlers) getPrewarmJob(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid job ID")
		return
	}
	job, ok := h.prewarmJob(w, r, p, policy, id, identity.Read)
	if !ok {
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(job.Revision, 10)))
	JSON(w, 200, job)
}

func (h opsHandlers) patchPrewarmJob(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	var in PatchPrewarmJob
	if !prewarmJSON(w, r, &in) {
		return
	}
	job, ok := h.prewarmJob(w, r, p, policy, id, identity.Configure)
	if !ok || h.prewarmDown(w, r) {
		return
	}
	patch := prewarm.JobPatch{Name: in.Name, Enabled: in.Enabled, Concurrency: in.Concurrency, RatePerSecond: in.RatePerSecond, Options: in.Options}
	concurrency, rate, options := job.Concurrency, job.RatePerSecond, job.Options
	if in.Name != nil && !validJobName(*in.Name) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "name must be 1-200 printable characters")
		return
	}
	if in.Concurrency != nil {
		concurrency = *in.Concurrency
	}
	if in.RatePerSecond != nil {
		rate = *in.RatePerSecond
	}
	if in.Options != nil {
		options = *in.Options
	}
	var interval *int
	if len(in.IntervalSeconds) > 0 {
		if bytes.Equal(bytes.TrimSpace(in.IntervalSeconds), []byte("null")) {
			patch.ClearSchedule = true
		} else {
			var v int
			if json.Unmarshal(in.IntervalSeconds, &v) != nil {
				Error(w, r, 400, api.ErrorCodeInvalidRequest, "interval_seconds must be a number or null")
				return
			}
			interval, patch.IntervalSeconds = &v, &v
		}
	}
	if msg := validPrewarmTuning(concurrency, rate, interval, options); msg != "" {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, msg)
		return
	}
	h.mutateCfg(w, r, p, "prewarm.job.update", id, struct {
		In       PatchPrewarmJob
		Revision int64
	}{in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		updated, err := prewarm.UpdateJob(ctx, tx, p.InstanceID, id, revision, patch)
		if err != nil {
			return nil, err
		}
		return updated, audited(ctx, tx, p, id, "prewarm.job.update", updated.Revision, map[string]any{"feed_id": updated.FeedID})
	})
}

func (h opsHandlers) deletePrewarmJob(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	job, ok := h.prewarmJob(w, r, p, policy, id, identity.Configure)
	if !ok {
		return
	}
	h.mutateCfg(w, r, p, "prewarm.job.delete", id, revision, 204, func(ctx context.Context, tx pgx.Tx) (any, error) {
		if err := prewarm.DeleteJob(ctx, tx, p.InstanceID, id, revision); err != nil {
			return nil, err
		}
		return struct{}{}, audited(ctx, tx, p, id, "prewarm.job.delete", revision, map[string]any{"feed_id": job.FeedID})
	})
}

func (h opsHandlers) startPrewarmRun(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid job ID")
		return
	}
	var in struct{}
	if !prewarmJSON(w, r, &in) {
		return
	}
	job, ok := h.prewarmJob(w, r, p, policy, id, identity.Configure)
	if !ok || h.prewarmDown(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	if !h.hasRemote(ctx, p.InstanceID, job.FeedID) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "the feed has no remote source to warm")
		return
	}
	var done bool
	h.mutateCfg(w, r, p, "prewarm.run.start", id, struct{ Job string }{id}, 202, func(ctx context.Context, tx pgx.Tx) (any, error) {
		run, err := prewarm.StartRun(ctx, tx, p.InstanceID, id, "manual", p.PrincipalID, "")
		if err != nil {
			return nil, prewarmStartError(err)
		}
		done = true
		return run, audited(ctx, tx, p, run.ID, "prewarm.run.start", 0, map[string]any{"job_id": id, "feed_id": job.FeedID, "total": run.Total})
	})
	if done {
		h.d.Prewarm.Notify()
	}
}

// prewarmRun loads a run and authorizes the caller on its feed.
func (h opsHandlers) prewarmRun(w http.ResponseWriter, r *http.Request, p identity.Principal, policy identity.Policy, id string, action identity.Action) (prewarm.Run, bool) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	run, err := prewarm.GetRun(ctx, h.db, p.InstanceID, id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			Error(w, r, 404, api.ErrorCodeNotFound, "prewarm run not found")
		} else {
			Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
		}
		return run, false
	}
	if _, ok := h.prewarmFeed(w, r, p, policy, run.FeedID, action); !ok {
		return run, false
	}
	return run, true
}

func (h opsHandlers) listPrewarmRuns(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "limit" && key != "job_id" && key != "feed_id" && key != "state") || len(values) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid query")
			return
		}
	}
	after, limit, ok := identityPage(w, r2(r, "job_id", "feed_id", "state"))
	if !ok {
		return
	}
	var job, feed string
	state := q.Get("state")
	if state != "" {
		switch state {
		case prewarm.StateQueued, prewarm.StateRunning, prewarm.StateSucceeded, prewarm.StatePartial, prewarm.StateFailed, prewarm.StateCanceled:
		default:
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid state")
			return
		}
	}
	switch {
	case q.Has("job_id"):
		id, valid := feedID(q.Get("job_id"))
		if !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid job_id")
			return
		}
		j, ok := h.prewarmJob(w, r, p, policy, id, identity.Read)
		if !ok {
			return
		}
		job = j.ID
	case q.Has("feed_id"):
		var valid bool
		if feed, valid = feedID(q.Get("feed_id")); !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed_id")
			return
		}
		if _, ok = h.prewarmFeed(w, r, p, policy, feed, identity.Read); !ok {
			return
		}
	default:
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
			Error(w, r, 403, api.ErrorCodeForbidden, "list the runs of a job (job_id) or a feed (feed_id), or use an instance administrator")
			return
		}
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	runs, err := prewarm.ListRuns(ctx, h.db, p.InstanceID, job, feed, state, after, limit+1)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
		return
	}
	next := ""
	if len(runs) > limit {
		runs = runs[:limit]
		next = runs[len(runs)-1].ID
	}
	if runs == nil {
		runs = []prewarm.Run{}
	}
	JSON(w, 200, struct {
		Items []prewarm.Run `json:"items"`
		Next  string        `json:"next_after,omitempty"`
	}{runs, next})
}

func (h opsHandlers) getPrewarmRun(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid run ID")
		return
	}
	run, ok := h.prewarmRun(w, r, p, policy, id, identity.Read)
	if !ok {
		return
	}
	JSON(w, 200, run)
}

func (h opsHandlers) listPrewarmItems(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid run ID")
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "limit" && key != "state") || len(values) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid query")
			return
		}
	}
	after, limit := -1, 100
	var err error
	if q.Has("after") {
		if after, err = strconv.Atoi(q.Get("after")); err != nil || after < 0 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid after")
			return
		}
	}
	if q.Has("limit") {
		if limit, err = strconv.Atoi(q.Get("limit")); err != nil || limit < 1 || limit > 500 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid limit")
			return
		}
	}
	state := q.Get("state")
	switch state {
	case "", prewarm.ItemPending, prewarm.ItemWarmed, prewarm.ItemCached, prewarm.ItemFailed, prewarm.ItemSkipped:
	default:
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid state")
		return
	}
	if _, ok = h.prewarmRun(w, r, p, policy, id, identity.Read); !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	items, err := prewarm.ListItems(ctx, h.db, id, state, after, limit+1)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "prewarm unavailable")
		return
	}
	var next *int
	if len(items) > limit {
		items = items[:limit]
		seq := items[len(items)-1].Seq
		next = &seq
	}
	if items == nil {
		items = []prewarm.RunItem{}
	}
	JSON(w, 200, struct {
		Items []prewarm.RunItem `json:"items"`
		Next  *int              `json:"next_after,omitempty"`
	}{items, next})
}

func (h opsHandlers) cancelPrewarmRun(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid run ID")
		return
	}
	run, ok := h.prewarmRun(w, r, p, policy, id, identity.Configure)
	if !ok || h.prewarmDown(w, r) {
		return
	}
	h.mutateCfg(w, r, p, "prewarm.run.cancel", id, struct{ Run string }{id}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		out, err := prewarm.RequestCancel(ctx, tx, p.InstanceID, id)
		if errors.Is(err, prewarm.ErrState) {
			return nil, conflictConfig("the run has already finished")
		}
		if err != nil {
			return nil, err
		}
		return out, audited(ctx, tx, p, id, "prewarm.run.cancel", 0, map[string]any{"job_id": run.JobID, "feed_id": run.FeedID})
	})
}

func (h opsHandlers) retryPrewarmRun(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid run ID")
		return
	}
	run, ok := h.prewarmRun(w, r, p, policy, id, identity.Configure)
	if !ok || h.prewarmDown(w, r) {
		return
	}
	if run.State == prewarm.StateQueued || run.State == prewarm.StateRunning {
		Error(w, r, 409, api.ErrorCodeConflict, "the run has not finished")
		return
	}
	var done bool
	h.mutateCfg(w, r, p, "prewarm.run.retry", id, struct{ Run string }{id}, 202, func(ctx context.Context, tx pgx.Tx) (any, error) {
		out, err := prewarm.StartRun(ctx, tx, p.InstanceID, run.JobID, "retry", p.PrincipalID, id)
		if errors.Is(err, prewarm.ErrEmpty) {
			return nil, invalidConfig("the run has no failed, canceled or unfinished items to retry")
		}
		if err != nil {
			return nil, prewarmStartError(err)
		}
		done = true
		return out, audited(ctx, tx, p, out.ID, "prewarm.run.retry", 0, map[string]any{"job_id": run.JobID, "feed_id": run.FeedID, "retry_of": id, "total": out.Total})
	})
	if done {
		h.d.Prewarm.Notify()
	}
}
