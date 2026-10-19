package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// opsCatalog returns tools for the operational management surface
// (replication, retention, storage, tokens, identity, audit, stats,
// settings). Each calls the management handler in-process, so authorization,
// idempotency and audit are exactly those of the HTTP API.
func opsCatalog() []*tool {
	id := func(name, desc string) param { return param{name: name, typ: "uuid", required: true, desc: desc} }
	after := param{name: "after", typ: "uuid", desc: "Return items after this ID (next_after of the previous page)."}
	lim := pageParams[0]
	key := func(c *call) map[string]string { return map[string]string{"Idempotency-Key": c.idempotencyKey()} }
	simple := func(name, title, desc, path string, params []param, qkeys ...string) *tool {
		return &tool{name: name, title: title, desc: desc, readOnly: true, params: params,
			run: func(c *call) *toolResult { return c.get(path, c.page(nil, qkeys...)) }}
	}
	byID := func(name, title, desc, argName, argDesc, prefix, suffix string) *tool {
		return &tool{name: name, title: title, desc: desc, readOnly: true, params: []param{id(argName, argDesc)},
			run: func(c *call) *toolResult {
				c.resourceID = c.str(argName)
				return c.get(prefix+c.str(argName)+suffix, nil)
			}}
	}
	dryRun := func(c *call) bool {
		if v, ok := c.args["dry_run"].(bool); ok {
			return v
		}
		return true
	}
	return []*tool{
		simple("list_replication_peers", "List replication peers", "List configured replication peers (credentials are never returned).", "/api/v1/replication/peers", nil),
		byID("get_replication_peer", "Get replication peer", "Get one replication peer.", "peer_id", "Peer ID.", "/api/v1/replication/peers/", ""),
		simple("list_replication_jobs", "List replication jobs", "List replication jobs.", "/api/v1/replication/jobs", nil),
		byID("get_replication_job", "Get replication job", "Get one replication job.", "job_id", "Replication job ID.", "/api/v1/replication/jobs/", ""),
		byID("get_replication_job_status", "Replication job status", "Lag, last run and error state of one replication job.", "job_id", "Replication job ID.", "/api/v1/replication/jobs/", "/status"),
		simple("get_replication_status", "Replication status", "Status summary of all replication jobs.", "/api/v1/replication/status", nil),
		byID("preview_replication_deletions", "Preview replication deletions", "Dry-run: what the next run of a job would delete on the destination. Changes nothing.", "job_id", "Replication job ID.", "/api/v1/replication/jobs/", "/deletions"),
		{name: "run_replication_job", title: "Run replication job", desc: "Trigger one replication run of a job now.", idempotent: true,
			params: []param{id("job_id", "Replication job ID.")},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("job_id")
				return c.api(http.MethodPost, "/api/v1/replication/jobs/"+c.str("job_id")+"/runs", nil, nil, key(c)).result()
			}},
		simple("list_retention_policies", "List retention policies", "List retention policies.", "/api/v1/retention/policies", []param{after, lim}, "after", "limit"),
		simple("list_retention_runs", "List retention runs", "List recent retention/garbage-collection runs.", "/api/v1/retention/runs", []param{after, lim}, "after", "limit"),
		byID("get_retention_run", "Get retention run", "Get one retention run with its report.", "run_id", "Retention run ID.", "/api/v1/retention/runs/", ""),
		simple("list_integrity_findings", "List integrity findings", "Unresolved blob integrity mismatches found by integrity scans.", "/api/v1/retention/integrity-findings", []param{after, lim}, "after", "limit"),
		{name: "start_retention_run", title: "Start retention run", desc: "DESTRUCTIVE unless dry_run is true (the default): start a retention/garbage-collection run that can delete versions and reclaim blobs. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{{name: "dry_run", typ: "boolean", desc: "Report only; change nothing (default true)."}},
			run: func(c *call) *toolResult {
				return c.api(http.MethodPost, "/api/v1/retention/runs", nil, map[string]any{"dry_run": dryRun(c)}, key(c)).result()
			}},
		simple("get_storage_usage", "Storage usage", "Blob storage usage.", "/api/v1/storage/usage", nil),
		simple("get_storage_migration", "Storage migration status", "Active storage backend and current migration/cleanup job.", "/api/v1/storage/migration", nil),
		simple("list_storage_jobs", "List storage jobs", "Recent storage migration and cleanup jobs.", "/api/v1/storage/migrations", nil),
		byID("get_storage_job", "Get storage job", "Get one storage migration job.", "storage_job_id", "Storage job ID.", "/api/v1/storage/migrations/", ""),
		{name: "start_storage_job", title: "Start storage job", desc: "DESTRUCTIVE: start a storage migration to another backend or cleanup of the previous backend (dry_run defaults to true). Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{{name: "action", typ: "string", required: true, desc: "migrate or cleanup.", enum: []string{"migrate", "cleanup"}},
				{name: "to", typ: "string", desc: "Target backend for migrate."},
				{name: "dry_run", typ: "boolean", desc: "Report only (default true)."},
				{name: "throttle_bytes_per_second", typ: "integer", desc: "Optional copy throttle."}},
			run: func(c *call) *toolResult {
				body := map[string]any{"action": c.args["action"], "dry_run": dryRun(c)}
				for _, k := range []string{"to", "throttle_bytes_per_second"} {
					if v, ok := c.args[k]; ok {
						body[k] = v
					}
				}
				return c.api(http.MethodPost, "/api/v1/storage/migrations", nil, body, key(c)).result()
			}},
		{name: "storage_job_action", title: "Pause or resume storage job", desc: "Pause or resume a storage migration job.", idempotent: true,
			params: []param{id("storage_job_id", "Storage job ID."), {name: "action", typ: "string", required: true, desc: "pause or resume.", enum: []string{"pause", "resume"}}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("storage_job_id")
				return c.api(http.MethodPost, "/api/v1/storage/migrations/"+c.str("storage_job_id")+"/"+c.str("action"), nil, nil, key(c)).result()
			}},
		simple("list_service_tokens", "List service tokens", "List service tokens (metadata only, never secrets).", "/api/v1/tokens",
			with(pageParams, param{name: "principal", typ: "uuid", desc: "Filter by principal ID."}, param{name: "status", typ: "string", desc: "active, expired or revoked.", enum: []string{"active", "expired", "revoked"}}),
			"limit", "cursor", "principal", "status"),
		{name: "revoke_service_token", title: "Revoke service token", desc: "DESTRUCTIVE: revoke a service token immediately. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{id("token_id", "Token ID.")},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("token_id")
				return c.api(http.MethodDelete, "/api/v1/tokens/"+c.str("token_id"), nil, nil, key(c)).result()
			}},
		simple("list_identity_bindings", "List identity bindings", "List external identity bindings.", "/api/v1/identity-bindings",
			with(pageParams, param{name: "principal", typ: "uuid", desc: "Filter by principal ID."}), "limit", "cursor", "principal"),
		simple("query_audit", "Query audit log", "Query audit events by principal, action, result and time range.", "/api/v1/audit",
			with(pageParams, param{name: "principal", typ: "uuid", desc: "Principal ID."}, param{name: "action", typ: "string", desc: "Exact action, e.g. feed.create."},
				param{name: "result", typ: "string", desc: "Result filter."}, param{name: "from", typ: "string", desc: "RFC 3339 start."}, param{name: "to", typ: "string", desc: "RFC 3339 end."}),
			"limit", "cursor", "principal", "action", "result", "from", "to"),
		{name: "purge_source_cache", title: "Purge source cache", desc: "DESTRUCTIVE: purge cached content of a remote source, optionally for one coordinate. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{id("source_id", "Source ID."), {name: "coordinate", typ: "string", desc: "Limit the purge to one package coordinate."}},
			run: func(c *call) *toolResult {
				q := url.Values{}
				if v := c.str("coordinate"); v != "" {
					q.Set("coordinate", v)
				}
				c.resourceID = c.str("source_id")
				return c.api(http.MethodDelete, "/api/v1/sources/"+c.str("source_id")+"/cache", q, nil, key(c)).result()
			}},
		{name: "delete_source", title: "Delete source", desc: "DESTRUCTIVE: delete a source. Refused while it is attached to a feed, holds content, is a publish-rule target or belongs to a replication job (delete the job first). force=true (instance admin) detaches it and removes its content; bytes are reclaimed later by garbage collection. Audited. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{id("source_id", "Source ID."), {name: "force", typ: "boolean", desc: "Detach from feeds and remove content (instance admin)."}, {name: "revision", typ: "integer", desc: "Source revision; the current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				src := c.str("source_id")
				c.resourceID = src
				rev, bad := c.revision("revision", func() (int64, *toolResult) {
					resp := c.api(http.MethodGet, "/api/v1/sources/"+src, nil, nil, nil)
					if resp.status >= 400 {
						return 0, resp.result()
					}
					var s struct {
						Revision int64 `json:"revision"`
					}
					if json.Unmarshal(resp.body, &s) != nil || s.Revision < 1 {
						return 0, errResult("cannot determine revision")
					}
					return s.Revision, nil
				})
				if bad != nil {
					return bad
				}
				q := url.Values{}
				if c.args["force"] == true {
					q.Set("force", "true")
				}
				return c.api(http.MethodDelete, "/api/v1/sources/"+src, q, nil, map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
		{name: "resolve_coordinate", title: "Explain resolution", desc: "Explain which source of a feed would serve a package coordinate and why (read policy, local ownership, remote fall-through).", readOnly: true,
			params: []param{id("feed_id", "Feed ID."), {name: "coordinate", typ: "string", required: true, desc: "Package coordinate."}, {name: "namespace", typ: "string", desc: "Namespace."}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("feed_id")
				return c.get("/api/v1/feeds/"+c.str("feed_id")+"/resolve", c.page(nil, "coordinate", "namespace"))
			}},
		{name: "test_publish_policy", title: "Dry-run publish policy", desc: "Evaluate a candidate publish policy for a coordinate without saving it.", readOnly: true,
			params: []param{id("feed_id", "Feed ID."), {name: "policy", typ: "object", required: true, desc: "Candidate policy: {default_source_id, rules[]}."},
				{name: "coordinate", typ: "string", required: true, desc: "Package coordinate."}, {name: "namespace", typ: "string", desc: "Namespace."},
				{name: "path", typ: "string", desc: "Absolute slash path as the publish route supplies it."}, {name: "principal_id", typ: "uuid", desc: "Principal to evaluate (defaults to caller)."}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("feed_id")
				body := map[string]any{"policy": c.args["policy"], "coordinate": c.args["coordinate"]}
				for _, k := range []string{"namespace", "path", "principal_id"} {
					if v, ok := c.args[k]; ok {
						body[k] = v
					}
				}
				return c.api(http.MethodPost, "/api/v1/feeds/"+c.str("feed_id")+"/publish-policy:test", nil, body, nil).result()
			}},
		simple("list_feed_stats", "Feed stats", "Per-feed statistics.", "/api/v1/stats/feeds", pageParams, "limit", "cursor"),
		simple("list_source_stats", "Source stats", "Per-source statistics.", "/api/v1/stats/sources", pageParams, "limit", "cursor"),
		simple("list_source_health", "Source health", "Health of remote sources.", "/api/v1/source-health", pageParams, "limit", "cursor"),
		simple("get_traffic_metrics", "Traffic metrics", "Request/byte traffic time series.", "/api/v1/metrics/traffic",
			[]param{{name: "scope", typ: "string", desc: "instance, feed or source.", enum: []string{"instance", "feed", "source"}}, {name: "ref", typ: "uuid", desc: "Feed or source ID for that scope."},
				{name: "from", typ: "string", desc: "RFC 3339 start."}, {name: "to", typ: "string", desc: "RFC 3339 end."}, {name: "step", typ: "string", desc: "5m, 1h or 1d.", enum: []string{"5m", "1h", "1d"}}},
			"scope", "ref", "from", "to", "step"),
		simple("list_prewarm_formats", "Prewarm formats", "Feed formats that can be prewarmed and the inputs each accepts (package lists, lockfile types, image lists, another feed).", "/api/v1/prewarm/formats", nil),
		simple("list_prewarm_jobs", "List prewarm jobs", "List prewarm jobs (warm a feed's remote cache from a package list, lockfile, image list or another feed, now or on a schedule). Give feed_id, or be an instance administrator.", "/api/v1/prewarm/jobs",
			[]param{{name: "feed_id", typ: "uuid", desc: "Only the jobs of this feed."}, after, lim}, "feed_id", "after", "limit"),
		byID("get_prewarm_job", "Get prewarm job", "Get one prewarm job with its latest run.", "job_id", "Prewarm job ID.", "/api/v1/prewarm/jobs/", ""),
		{name: "preview_prewarm", title: "Preview prewarm source", desc: "Expand a prewarm source the way a job would and report what would be warmed and what is ignored. Creates nothing.", readOnly: true,
			params: []param{id("feed_id", "Feed to warm."), {name: "source", typ: "object", required: true, desc: "{kind: packages|lockfile|images|feed, packages: [{name, version}], lockfile: {type, content}, images: [string], feed_id}."}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("feed_id")
				return c.api(http.MethodPost, "/api/v1/prewarm/preview", nil, map[string]any{"feed_id": c.args["feed_id"], "source": c.args["source"]}, nil).result()
			}},
		{name: "create_prewarm_job", title: "Create prewarm job", desc: "Create a job that warms the remote cache of a feed from a package list, a lockfile (package-lock.json, pnpm-lock.yaml, go.sum, Cargo.lock, requirements.txt, poetry.lock), a list of images or everything another feed references. Optionally recurring (interval_seconds) and started at once (run_now). Warming is done through the feed's own routes as the calling principal.", idempotent: true,
			params: []param{id("feed_id", "Feed to warm (needs a remote source)."), {name: "name", typ: "string", required: true, desc: "Job name."},
				{name: "source", typ: "object", required: true, desc: "{kind: packages|lockfile|images|feed, packages: [{name, version}], lockfile: {type, content}, images: [string], feed_id}."},
				{name: "options", typ: "object", desc: "{platforms: [os/arch], file_filters: [string]}."},
				{name: "concurrency", typ: "integer", desc: "Parallel items, 1-32 (default 4)."}, {name: "rate_per_second", typ: "integer", desc: "Items started per second, 0 unlimited (default 10)."},
				{name: "interval_seconds", typ: "integer", desc: "Run on this schedule, 60 to 2678400."}, {name: "enabled", typ: "boolean", desc: "A disabled job keeps its schedule paused."}, {name: "run_now", typ: "boolean", desc: "Queue the first run immediately."}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("feed_id")
				body := map[string]any{}
				for _, k := range []string{"feed_id", "name", "source", "options", "concurrency", "rate_per_second", "interval_seconds", "enabled", "run_now"} {
					if v, ok := c.args[k]; ok {
						body[k] = v
					}
				}
				return c.api(http.MethodPost, "/api/v1/prewarm/jobs", nil, body, key(c)).result()
			}},
		{name: "update_prewarm_job", title: "Update prewarm job", desc: "Change a prewarm job's name, enabled flag, schedule, concurrency, rate or options. clear_schedule removes the schedule. The source cannot change: create a new job.", idempotent: true,
			params: []param{id("job_id", "Prewarm job ID."), {name: "name", typ: "string", desc: "New name."}, {name: "enabled", typ: "boolean", desc: "Enable or pause the schedule."},
				{name: "concurrency", typ: "integer", desc: "Parallel items, 1-32."}, {name: "rate_per_second", typ: "integer", desc: "Items per second, 0 unlimited."},
				{name: "interval_seconds", typ: "integer", desc: "New schedule, 60 to 2678400."}, {name: "clear_schedule", typ: "boolean", desc: "Remove the schedule."},
				{name: "options", typ: "object", desc: "{platforms, file_filters}."}, {name: "revision", typ: "integer", desc: "Job revision; the current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				job := c.str("job_id")
				c.resourceID = job
				rev, bad := c.revision("revision", func() (int64, *toolResult) {
					resp := c.api(http.MethodGet, "/api/v1/prewarm/jobs/"+job, nil, nil, nil)
					if resp.status >= 400 {
						return 0, resp.result()
					}
					var j struct {
						Revision int64 `json:"revision"`
					}
					if json.Unmarshal(resp.body, &j) != nil || j.Revision < 1 {
						return 0, errResult("cannot determine revision")
					}
					return j.Revision, nil
				})
				if bad != nil {
					return bad
				}
				body := map[string]any{}
				for _, k := range []string{"name", "enabled", "concurrency", "rate_per_second", "interval_seconds", "options"} {
					if v, ok := c.args[k]; ok {
						body[k] = v
					}
				}
				if c.args["clear_schedule"] == true {
					body["interval_seconds"] = nil
				}
				return c.api(http.MethodPatch, "/api/v1/prewarm/jobs/"+job, nil, body, map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
		{name: "delete_prewarm_job", title: "Delete prewarm job", desc: "DESTRUCTIVE: delete a prewarm job with its run history; a run in progress stops. Already cached content stays. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{id("job_id", "Prewarm job ID."), {name: "revision", typ: "integer", desc: "Job revision; the current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				job := c.str("job_id")
				c.resourceID = job
				rev, bad := c.revision("revision", func() (int64, *toolResult) {
					resp := c.api(http.MethodGet, "/api/v1/prewarm/jobs/"+job, nil, nil, nil)
					if resp.status >= 400 {
						return 0, resp.result()
					}
					var j struct {
						Revision int64 `json:"revision"`
					}
					if json.Unmarshal(resp.body, &j) != nil || j.Revision < 1 {
						return 0, errResult("cannot determine revision")
					}
					return j.Revision, nil
				})
				if bad != nil {
					return bad
				}
				return c.api(http.MethodDelete, "/api/v1/prewarm/jobs/"+job, nil, nil, map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
		{name: "run_prewarm_job", title: "Run prewarm job", desc: "Queue a run of a prewarm job now. Poll get_prewarm_run for progress.", idempotent: true,
			params: []param{id("job_id", "Prewarm job ID.")},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("job_id")
				return c.api(http.MethodPost, "/api/v1/prewarm/jobs/"+c.str("job_id")+"/runs", nil, map[string]any{}, key(c)).result()
			}},
		simple("list_prewarm_runs", "List prewarm runs", "List prewarm runs newest first, filtered by job_id or feed_id (or all, for instance administrators).", "/api/v1/prewarm/runs",
			[]param{{name: "job_id", typ: "uuid", desc: "Only the runs of this job."}, {name: "feed_id", typ: "uuid", desc: "Only the runs of this feed."},
				{name: "state", typ: "string", desc: "queued, running, succeeded, partial, failed or canceled.", enum: []string{"queued", "running", "succeeded", "partial", "failed", "canceled"}}, after, lim},
			"job_id", "feed_id", "state", "after", "limit"),
		byID("get_prewarm_run", "Get prewarm run", "Progress and outcome of one prewarm run: totals, warmed (fetched from upstream), cached (already present), failed, skipped and bytes.", "run_id", "Prewarm run ID.", "/api/v1/prewarm/runs/", ""),
		{name: "list_prewarm_run_items", title: "List prewarm run items", desc: "Per-item status of a prewarm run, including the failure code of failed items.", readOnly: true,
			params: []param{id("run_id", "Prewarm run ID."), {name: "state", typ: "string", desc: "Only items in this state.", enum: []string{"pending", "warmed", "cached", "failed", "skipped"}},
				{name: "after", typ: "integer", desc: "Return items after this sequence number (next_after of the previous page)."}, {name: "limit", typ: "integer", desc: "Page size, 1-500 (default 100)."}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("run_id")
				return c.get("/api/v1/prewarm/runs/"+c.str("run_id")+"/items", c.page(nil, "state", "after", "limit"))
			}},
		{name: "cancel_prewarm_run", title: "Cancel prewarm run", desc: "Cancel a queued prewarm run at once, or ask a running one to stop; unfinished items are marked skipped.", idempotent: true,
			params: []param{id("run_id", "Prewarm run ID.")},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("run_id")
				return c.api(http.MethodPost, "/api/v1/prewarm/runs/"+c.str("run_id")+"/cancel", nil, nil, key(c)).result()
			}},
		{name: "retry_prewarm_run", title: "Retry prewarm run", desc: "Queue a new run of the items of a finished prewarm run that failed, were canceled or never ran.", idempotent: true,
			params: []param{id("run_id", "Prewarm run ID.")},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("run_id")
				return c.api(http.MethodPost, "/api/v1/prewarm/runs/"+c.str("run_id")+"/retry", nil, nil, key(c)).result()
			}},
		simple("get_settings", "Get settings", "Read instance settings and their revision.", "/api/v1/settings", nil),
		simple("get_retention_settings", "Get retention settings", "Read the instance default retention policy (remote-cached content only), the storage-pressure watermarks and capacity quota, and which fields the environment locks.", "/api/v1/retention/settings", nil),
		simple("get_storage_pressure", "Get storage pressure", "Live storage-pressure status: usage against capacity, watermarks, state (including unevictable) and the last eviction.", "/api/v1/retention/pressure", nil),
		simple("get_diagnostics", "Diagnostics", "Instance diagnostics (admin only).", "/api/v1/diagnostics", nil),
		byID("get_delete_policy", "Get delete policy", "Get a feed's package delete policy.", "feed_id", "Feed ID.", "/api/v1/feeds/", "/delete-policy"),
		{name: "submit_security_result", title: "Submit security result", desc: "Record an external scanner result for an asset. Stored and emitted as an asset.security_result event.", idempotent: true,
			params: []param{id("asset_id", "Asset ID."), {name: "scanner", typ: "string", required: true, desc: "Scanner name."}, {name: "status", typ: "string", required: true, desc: "passed, failed or warning.", enum: []string{"passed", "failed", "warning"}},
				{name: "summary", typ: "object", desc: "Free-form summary (counts by severity, report URL)."}},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("asset_id")
				body := map[string]any{"scanner": c.args["scanner"], "status": c.args["status"]}
				if v, ok := c.args["summary"]; ok {
					body["summary"] = v
				}
				return c.api(http.MethodPost, "/api/v1/assets/"+c.str("asset_id")+"/security-results", nil, body, key(c)).result()
			}},
	}
}
