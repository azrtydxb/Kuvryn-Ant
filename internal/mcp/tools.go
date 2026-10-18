package mcp

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type param struct {
	name, typ, desc string // typ: string, uuid, integer, boolean, object, array
	required        bool
	enum            []string
}

type tool struct {
	name, title, desc string
	params            []param
	readOnly          bool
	destructive       bool
	idempotent        bool
	run               func(c *call) *toolResult
}

func (t *tool) allParams() []param {
	ps := t.params
	if !t.readOnly {
		ps = append(ps[:len(ps):len(ps)], param{name: "idempotency_key", typ: "string", desc: "Optional key (1-256 bytes) making retries replay-safe; generated when omitted."})
	}
	if t.destructive {
		ps = append(ps[:len(ps):len(ps)], param{name: "confirm", typ: "boolean", required: true, desc: "Must be true. Destructive operations are refused without explicit confirmation."})
	}
	return ps
}

func (t *tool) descriptor() map[string]any {
	props := map[string]any{}
	var required []string
	for _, p := range t.allParams() {
		typ := p.typ
		schema := map[string]any{"description": p.desc}
		switch typ {
		case "uuid":
			schema["type"] = "string"
			schema["format"] = "uuid"
		case "integer", "boolean", "object", "array", "string":
			schema["type"] = typ
		}
		if typ == "array" {
			schema["items"] = map[string]any{"type": "object"}
		}
		if len(p.enum) > 0 {
			schema["enum"] = p.enum
		}
		props[p.name] = schema
		if p.required {
			required = append(required, p.name)
		}
	}
	input := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if required != nil {
		input["required"] = required
	}
	ann := map[string]any{"title": t.title, "readOnlyHint": t.readOnly, "openWorldHint": false}
	if !t.readOnly {
		ann["destructiveHint"] = t.destructive
		ann["idempotentHint"] = t.idempotent
	}
	return map[string]any{"name": t.name, "title": t.title, "description": t.desc, "inputSchema": input, "annotations": ann}
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 32)
		return int(i), err == nil
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

// validate checks arguments against the declared schema (strict: unknown or
// mistyped arguments are rejected, as additionalProperties is false).
func (t *tool) validate(args map[string]any) string {
	declared := map[string]param{}
	for _, p := range t.allParams() {
		declared[p.name] = p
	}
	for k, v := range args {
		p, ok := declared[k]
		if !ok {
			return "unknown argument: " + k
		}
		valid := false
		switch p.typ {
		case "string":
			s, ok := v.(string)
			valid = ok && len(s) <= 4096
		case "uuid":
			s, ok := v.(string)
			valid = ok && uuidPattern.MatchString(s)
		case "integer":
			_, valid = toInt(v)
		case "boolean":
			_, valid = v.(bool)
		case "object":
			_, valid = v.(map[string]any)
		case "array":
			_, valid = v.([]any)
		}
		if valid && len(p.enum) > 0 {
			valid = false
			for _, e := range p.enum {
				valid = valid || v == e
			}
		}
		if !valid {
			return "invalid argument: " + k
		}
	}
	for _, p := range declared {
		if _, ok := args[p.name]; p.required && !ok && p.name != "confirm" {
			return "missing required argument: " + p.name
		}
	}
	if k, ok := args["idempotency_key"].(string); ok && (k == "" || len(k) > 256 || strings.TrimSpace(k) != k) {
		return "invalid argument: idempotency_key"
	}
	return ""
}

func (c *call) str(name string) string { s, _ := c.args[name].(string); return s }

// page copies the standard pagination arguments into a query.
func (c *call) page(q url.Values, keys ...string) url.Values {
	if q == nil {
		q = url.Values{}
	}
	for _, k := range keys {
		switch v := c.args[k].(type) {
		case string:
			q.Set(k, v)
		case json.Number, float64:
			n, _ := toInt(v)
			q.Set(k, strconv.Itoa(n))
		}
	}
	return q
}

var pageParams = []param{
	{name: "limit", typ: "integer", desc: "Page size, 1-200 (default 50)."},
	{name: "cursor", typ: "string", desc: "Opaque cursor from a previous page's next_cursor."},
}
var prefixParam = param{name: "prefix", typ: "string", desc: "Name prefix filter."}

func with(base []param, more ...param) []param {
	return append(append([]param{}, base...), more...)
}

func (c *call) get(path string, q url.Values) *toolResult {
	return c.api(http.MethodGet, path, q, nil, nil).result()
}

func catalog() []*tool {
	feed := param{name: "feed_id", typ: "uuid", required: true, desc: "Feed ID."}
	source := param{name: "source_id", typ: "uuid", required: true, desc: "Source ID."}
	pkg := param{name: "package_id", typ: "uuid", required: true, desc: "Package (component) ID from search_packages."}
	feedPath := func(c *call, suffix string) string {
		c.resourceID = c.str("feed_id")
		return "/api/v1/feeds/" + c.str("feed_id") + suffix
	}
	return append(opsCatalog(), []*tool{
		{name: "list_feeds", title: "List feeds", desc: "List feeds visible to the caller.", readOnly: true,
			params: with(pageParams, prefixParam),
			run: func(c *call) *toolResult {
				return c.get("/api/v1/feeds", c.page(nil, "limit", "cursor", "prefix"))
			}},
		{name: "get_feed", title: "Get feed", desc: "Get one feed by ID.", readOnly: true, params: []param{feed},
			run: func(c *call) *toolResult { return c.get(feedPath(c, ""), nil) }},
		{name: "list_sources", title: "List sources", desc: "List sources (local, remote, virtual) visible to the caller.", readOnly: true,
			params: with(pageParams, prefixParam),
			run: func(c *call) *toolResult {
				return c.get("/api/v1/sources", c.page(nil, "limit", "cursor", "prefix"))
			}},
		{name: "get_source", title: "Get source", desc: "Get one source by ID.", readOnly: true, params: []param{source},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("source_id")
				return c.get("/api/v1/sources/"+c.str("source_id"), nil)
			}},
		{name: "list_feed_sources", title: "List feed sources", desc: "List the sources attached to a feed, with priorities.", readOnly: true,
			params: with([]param{feed}, pageParams...),
			run: func(c *call) *toolResult {
				return c.get(feedPath(c, "/sources"), c.page(nil, "limit", "cursor"))
			}},
		{name: "get_publish_policy", title: "Get publish policy", desc: "Get the publish routing policy of a feed.", readOnly: true, params: []param{feed},
			run: func(c *call) *toolResult { return c.get(feedPath(c, "/publish-policy"), nil) }},
		{name: "search_packages", title: "Search packages", desc: "Package discovery in a feed by name prefix, namespace or sha256 digest.", readOnly: true,
			params: with([]param{feed}, append(pageParams, prefixParam,
				param{name: "namespace", typ: "string", desc: "Exact namespace filter."},
				param{name: "digest", typ: "string", desc: "Exact digest filter, sha256:<64 lowercase hex>."})...),
			run: func(c *call) *toolResult {
				return c.get(feedPath(c, "/packages"), c.page(nil, "limit", "cursor", "prefix", "namespace", "digest"))
			}},
		{name: "list_package_versions", title: "List package versions", desc: "List versions of a package.", readOnly: true,
			params: with([]param{feed, pkg}, append(pageParams, prefixParam)...),
			run: func(c *call) *toolResult {
				return c.get(feedPath(c, "/packages/"+c.str("package_id")+"/versions"), c.page(nil, "limit", "cursor", "prefix"))
			}},
		{name: "list_package_tags", title: "List package tags", desc: "List mutable tags of a package.", readOnly: true,
			params: with([]param{feed, pkg}, append(pageParams, prefixParam)...),
			run: func(c *call) *toolResult {
				return c.get(feedPath(c, "/packages/"+c.str("package_id")+"/tags"), c.page(nil, "limit", "cursor", "prefix"))
			}},
		{name: "list_package_assets", title: "List package assets", desc: "List assets (files, digests) of one package version.", readOnly: true,
			params: with([]param{feed, pkg, {name: "version_id", typ: "uuid", required: true, desc: "Version ID."}}, append(pageParams, prefixParam)...),
			run: func(c *call) *toolResult {
				return c.get(feedPath(c, "/packages/"+c.str("package_id")+"/versions/"+c.str("version_id")+"/assets"), c.page(nil, "limit", "cursor", "prefix"))
			}},
		{name: "get_asset", title: "Get asset", desc: "Get one asset with its checksums.", readOnly: true,
			params: []param{feed, {name: "asset_id", typ: "uuid", required: true, desc: "Asset ID."}},
			run:    func(c *call) *toolResult { return c.get(feedPath(c, "/assets/"+c.str("asset_id")), nil) }},
		{name: "lookup_checksum", title: "Look up checksum", desc: "Find packages in a feed containing an artifact with the given sha256 digest.", readOnly: true,
			params: []param{feed, {name: "digest", typ: "string", required: true, desc: "sha256:<64 lowercase hex>."}},
			run: func(c *call) *toolResult {
				return c.get(feedPath(c, "/packages"), c.page(nil, "digest"))
			}},
		{name: "get_health", title: "Service health", desc: "Liveness, readiness and saturation counters of this instance.", readOnly: true,
			run: healthTool},
		{name: "get_cache_insight", title: "Cache insight", desc: "Remote origin and cache policy configuration of a source (TTL, negative caching, revision), for diagnosing cache behaviour.", readOnly: true,
			params: []param{source},
			run: func(c *call) *toolResult {
				c.resourceID = c.str("source_id")
				return c.get("/api/v1/sources/"+c.str("source_id")+"/remote-config", nil)
			}},
		{name: "list_webhooks", title: "List webhooks", desc: "List webhook subscriptions (secrets are never returned).", readOnly: true,
			params: []param{{name: "after", typ: "uuid", desc: "Return items after this webhook ID."}, pageParams[0]},
			run: func(c *call) *toolResult {
				return c.get("/api/v1/webhooks", c.page(nil, "limit", "after"))
			}},
		{name: "create_feed", title: "Create feed", desc: "Create a feed. Requires configure permission.", idempotent: true,
			params: []param{{name: "slug", typ: "string", required: true, desc: "Unique feed slug."}, {name: "format", typ: "string", required: true, desc: "Package format, e.g. raw, oci, npm, pypi, maven."},
				{name: "anonymous_read", typ: "boolean", desc: "Allow anonymous reads (default false)."}},
			run: func(c *call) *toolResult {
				body := map[string]any{"slug": c.args["slug"], "format": c.args["format"]}
				if v, ok := c.args["anonymous_read"]; ok {
					body["anonymous_read"] = v
				}
				return c.api(http.MethodPost, "/api/v1/feeds", nil, body, map[string]string{"Idempotency-Key": c.idempotencyKey()}).result()
			}},
		{name: "attach_source", title: "Attach source to feed", desc: "Attach a source to a feed at a priority (lower resolves first). Requires configure on both.", idempotent: true,
			params: []param{feed, source, {name: "priority", typ: "integer", required: true, desc: "0-1000000."},
				{name: "revision", typ: "integer", desc: "Feed revision for optimistic concurrency; current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				feed := c.str("feed_id")
				rev, bad := c.revision("revision", func() (int64, *toolResult) { return c.feedRevision(feed) })
				if bad != nil {
					return bad
				}
				c.resourceID = feed
				return c.api(http.MethodPut, "/api/v1/feeds/"+feed+"/sources/"+c.str("source_id"), nil,
					map[string]any{"priority": c.args["priority"]}, map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
		{name: "set_publish_policy", title: "Set publish policy", desc: "Replace a feed's publish routing policy (default target source and ordered rules).", idempotent: true,
			params: []param{feed, {name: "default_source_id", typ: "uuid", required: true, desc: "Default publish target source."},
				{name: "rules", typ: "array", required: true, desc: "Ordered rules: {id?,priority,selector:{namespace,package,path_prefix,principal_id},target_source_id,deny}."},
				{name: "revision", typ: "integer", desc: "Feed revision; current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				feed := c.str("feed_id")
				rev, bad := c.revision("revision", func() (int64, *toolResult) { return c.feedRevision(feed) })
				if bad != nil {
					return bad
				}
				c.resourceID = feed
				return c.api(http.MethodPut, "/api/v1/feeds/"+feed+"/publish-policy", nil,
					map[string]any{"default_source_id": c.args["default_source_id"], "rules": c.args["rules"]},
					map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
		{name: "rotate_webhook_secret", title: "Rotate webhook secret", desc: "Replace a webhook signing secret. If no secret is supplied a strong one is generated and returned once; store it in the receiver.", idempotent: false,
			params: []param{{name: "webhook_id", typ: "uuid", required: true, desc: "Webhook ID."},
				{name: "secret", typ: "string", desc: "New secret, 32-4096 bytes; generated when omitted."},
				{name: "revision", typ: "integer", desc: "Webhook revision; current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				id := c.str("webhook_id")
				c.resourceID = id
				rev, bad := c.revision("revision", func() (int64, *toolResult) { return c.webhookRevision(id) })
				if bad != nil {
					return bad
				}
				secret, generated := c.str("secret"), false
				if secret == "" {
					secret, generated = rand.Text()+rand.Text(), true
				}
				resp := c.api(http.MethodPut, "/api/v1/webhooks/"+id+"/secret", nil, map[string]any{"secret": secret},
					map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)})
				if resp.status >= 400 || !generated || resp.overflow {
					return resp.result()
				}
				out, _ := json.Marshal(map[string]any{"webhook": json.RawMessage(bytes.TrimSpace(resp.body)), "secret": secret, "note": "Shown once; configure it in the receiver now."})
				return textResult(string(out))
			}},
		{name: "detach_source", title: "Detach source from feed", desc: "DESTRUCTIVE: detach a source from a feed. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{feed, source, {name: "revision", typ: "integer", desc: "Feed revision; current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				feed := c.str("feed_id")
				rev, bad := c.revision("revision", func() (int64, *toolResult) { return c.feedRevision(feed) })
				if bad != nil {
					return bad
				}
				c.resourceID = feed
				return c.api(http.MethodDelete, "/api/v1/feeds/"+feed+"/sources/"+c.str("source_id"), nil, nil,
					map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
		{name: "delete_package_version", title: "Delete or unlist package version", desc: "DESTRUCTIVE: delete a package version (default) or unlist/relist it (action). Subject to the feed delete policy (allowed, unlist_only, denied, immutable releases) and the delete permission; audited and replicated as a tombstone. Stored bytes are reclaimed later by garbage collection. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{feed, pkg, {name: "version", typ: "string", required: true, desc: "Version string as listed by list_package_versions."}, {name: "action", typ: "string", desc: "delete (default), unlist or relist."}},
			run: func(c *call) *toolResult {
				q := url.Values{}
				if a := c.str("action"); a != "" {
					q.Set("action", a)
				}
				c.resourceID = c.str("feed_id")
				return c.api(http.MethodDelete, "/api/v1/feeds/"+c.str("feed_id")+"/packages/"+c.str("package_id")+"/versions/"+url.PathEscape(c.str("version")), q, nil, map[string]string{"Idempotency-Key": c.idempotencyKey()}).result()
			}},
		{name: "disable_webhook", title: "Disable webhook", desc: "DESTRUCTIVE: stop delivering events to a webhook. Requires confirm: true.", destructive: true, idempotent: true,
			params: []param{{name: "webhook_id", typ: "uuid", required: true, desc: "Webhook ID."},
				{name: "revision", typ: "integer", desc: "Webhook revision; current revision is used when omitted."}},
			run: func(c *call) *toolResult {
				id := c.str("webhook_id")
				c.resourceID = id
				rev, bad := c.revision("revision", func() (int64, *toolResult) { return c.webhookRevision(id) })
				if bad != nil {
					return bad
				}
				return c.api(http.MethodPatch, "/api/v1/webhooks/"+id, nil, map[string]any{"enabled": false},
					map[string]string{"Idempotency-Key": c.idempotencyKey(), "If-Match": etag(rev)}).result()
			}},
	}...)
}

var healthMetrics = map[string]bool{"ant_http_active_requests": true, "ant_http_saturation_total": true, "ant_database_readiness_failures_total": true}

func healthTool(c *call) *toolResult {
	out := map[string]any{}
	for name, path := range map[string]string{"live": "/health/live", "ready": "/health/ready"} {
		resp := c.api(http.MethodGet, path, nil, nil, nil)
		out[name] = map[string]any{"ok": resp.status == http.StatusOK, "status": resp.status}
	}
	metrics := map[string]string{}
	if resp := c.api(http.MethodGet, "/metrics", nil, nil, nil); resp.status == http.StatusOK && !resp.overflow {
		for _, line := range strings.Split(string(resp.body), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && healthMetrics[f[0]] {
				metrics[f[0]] = f[1]
			}
		}
	}
	out["metrics"] = metrics
	b, _ := json.Marshal(out)
	return textResult(string(b))
}

// callTool implements tools/call.
func (s *Server) callTool(r *http.Request, p identity.Principal, params json.RawMessage) (any, *rpcError) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.Name == "" {
		return nil, &rpcError{codeInvalidParams, "invalid params"}
	}
	t, ok := s.tools[in.Name]
	if !ok {
		return nil, &rpcError{codeInvalidParams, "unknown tool"}
	}
	args := map[string]any{}
	if len(bytes.TrimSpace(in.Arguments)) > 0 && string(bytes.TrimSpace(in.Arguments)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(in.Arguments))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil || args == nil {
			return nil, &rpcError{codeInvalidParams, "arguments must be an object"}
		}
	}
	c := &call{s: s, r: r, p: p, args: args}
	event := AuditEvent{Action: "mcp.tool.call", Name: t.name, Destructive: t.destructive}
	finish := func(outcome string) *rpcError {
		event.Outcome, event.Status, event.ResourceID = outcome, c.status, c.resourceID
		if s.audit == nil {
			return nil
		}
		if err := s.audit(r.Context(), p, event); err != nil {
			s.logger.Error("mcp audit failed", "tool", t.name)
			return &rpcError{codeInternal, "audit unavailable; call result withheld"}
		}
		return nil
	}
	if msg := t.validate(args); msg != "" {
		if e := finish("invalid"); e != nil {
			return nil, e
		}
		return nil, &rpcError{codeInvalidParams, msg}
	}
	if t.destructive && args["confirm"] != true {
		if e := finish("confirmation_required"); e != nil {
			return nil, e
		}
		return nil, &rpcError{codeInvalidParams, fmt.Sprintf("%s is destructive: pass confirm: true to proceed", t.name)}
	}
	for _, k := range []string{"feed_id", "source_id", "webhook_id"} {
		if v, ok := args[k].(string); ok && c.resourceID == "" {
			c.resourceID = v
		}
	}
	res := t.run(c)
	outcome := "ok"
	if res.IsError {
		outcome = "error"
	}
	if e := finish(outcome); e != nil {
		return nil, e
	}
	return res, nil
}
