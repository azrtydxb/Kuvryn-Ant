package management

// Console read APIs: cross-feed package search, resolution explain and the
// publish-policy dry run. All authorization is delegated to identity.Policy.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5/pgxpool"
)

type consoleHandlers struct{ identityHandlers }

// RegisterConsole mounts the console-facing management additions. Every route
// requires bearer authentication; authorization is enforced per handler.
func RegisterConsole(mux *http.ServeMux, pool *pgxpool.Pool, codec CursorCodec) {
	h := consoleHandlers{identityHandlers{feedHandlers{db: pool, codec: codec}}}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/packages":                        h.searchPackages,
		"PUT /api/v1/feeds/{id}/sources":              h.reorderSources,
		"POST /api/v1/feeds/batch":                    h.batchCreateFeed,
		"GET /api/v1/feeds/{id}/resolve":              h.resolveExplain,
		"POST /api/v1/feeds/{id}/publish-policy:test": h.testPublishPolicy,
		"DELETE /api/v1/feeds/{id}":                   h.deleteFeed,
		"DELETE /api/v1/sources/{id}/cache":           h.purgeSourceCache,
		"DELETE /api/v1/sources/{id}":                 h.deleteSource,
		"GET /api/v1/tokens":                          h.listTokens,
		"GET /api/v1/identity-bindings":               h.listBindings,
		"DELETE /api/v1/identity-bindings/{id}":       h.deleteBinding,
		"GET /api/v1/audit":                           h.listAudit,
		"GET /api/v1/audit/export":                    h.exportAudit,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

// problem is a typed, client-safe failure raised inside a mutation callback.
// database.ExecuteMutation sanitizes arbitrary callback errors, so callers keep
// the problem in a closure variable and render it after the mutation returns.
type problem struct {
	Status  int
	Code    api.ErrorCode
	Message string
}

func (p *problem) Error() string { return p.Message }

func writeProblem(w http.ResponseWriter, r *http.Request, p *problem) {
	Error(w, r, p.Status, p.Code, p.Message)
}

func validText(v string, max int) bool {
	return len(v) <= max && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

// singleParams rejects unknown or repeated query parameters.
func singleParams(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	names := map[string]bool{}
	for _, a := range allowed {
		names[a] = true
	}
	for k, v := range r.URL.Query() {
		if !names[k] || len(v) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid query parameters")
			return false
		}
	}
	return true
}

func pageLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' || n > 1000 {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n < 1 || n > 200 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "limit must be between 1 and 200")
			return 0, false
		}
		limit = n
	}
	return limit, true
}

func scopeHash(parts ...any) string {
	encoded, _ := json.Marshal(parts)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

type PackageSearchResult struct {
	FeedID     string          `json:"feed_id"`
	FeedSlug   string          `json:"feed_slug"`
	Format     string          `json:"format"`
	ID         string          `json:"id"`
	Coordinate string          `json:"coordinate"`
	Namespace  string          `json:"namespace"`
	SourceID   string          `json:"source_id"`
	Origin     string          `json:"origin"`
	Metadata   json.RawMessage `json:"metadata"`
	PackageStats
}
type PackageSearchPage struct {
	Items []PackageSearchResult `json:"items"`
	Page  api.PageInfo          `json:"page"`
	Total int64                 `json:"total"`
}
type searchPermission struct {
	FeedID string `json:"feed_id"`
	packagePermission
}

const maxSearchPermissions = 20000

// searchPackagesSQL applies package-owner selection per feed before any
// authorization, namespace, origin or text filtering that depends on the owner,
// exactly like the per-feed listing. The text filter depends only on the
// coordinate, so it is safe to apply before owner selection. $2 is the exact
// authorization truth table produced by identity.Policy.
const searchPackagesSQL = `WITH perms AS (
 SELECT * FROM jsonb_to_recordset($2::jsonb) AS p(feed_id uuid,source_id uuid,base boolean,namespaces text[])
), owners AS (
 SELECT DISTINCT ON(fs.feed_id,c.coordinate) fs.feed_id,c.id,c.source_id,c.coordinate,c.namespace,c.metadata
 FROM feed_sources fs JOIN components c ON (c.instance_id,c.source_id)=(fs.instance_id,fs.source_id)
 WHERE fs.instance_id=$1 AND fs.feed_id IN (SELECT feed_id FROM perms) AND ($3::text='' OR strpos(lower(c.coordinate),lower($3::text))>0)
 ORDER BY fs.feed_id,c.coordinate,fs.priority
), visible AS (
 SELECT o.*,f.slug,f.format,s.kind FROM owners o
 JOIN perms p ON p.feed_id=o.feed_id AND p.source_id=o.source_id
 JOIN feeds f ON f.instance_id=$1 AND f.id=o.feed_id JOIN sources s ON s.instance_id=$1 AND s.id=o.source_id
 WHERE o.namespace IS NOT NULL AND p.base<>(o.namespace=ANY(p.namespaces)) AND ($4::text='' OR s.kind=$4::text)
) `

func (h consoleHandlers) searchPackages(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "q", "feed", "format", "origin", "limit", "cursor") {
		return
	}
	values := r.URL.Query()
	q, feed, format, origin := values.Get("q"), values.Get("feed"), values.Get("format"), values.Get("origin")
	if len(q) > 256 || !validText(q, 256) || (format != "" && !formats[format]) || (origin != "" && !validSourceKind(origin)) || len(values.Get("cursor")) > 2048 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid package search")
		return
	}
	if feed != "" {
		var ok bool
		if feed, ok = feedID(feed); !ok {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid package search")
			return
		}
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	scope := scopeHash("package-search", p.InstanceID, p.PrincipalID, p.TokenID, q, feed, format, origin)
	afterFeed, afterID := "", ""
	if c := values.Get("cursor"); c != "" {
		after, err := h.codec.Decode(scope, c)
		var cut bool
		afterFeed, afterID, cut = strings.Cut(after, "/")
		_, v1 := feedID(afterFeed)
		_, v2 := feedID(afterID)
		if err != nil || !cut || !v1 || !v2 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Candidate feeds are authorization-filtered below; a feed the caller cannot
	// read at all contributes no permission rows and so no packages or counts.
	frows, err := h.db.Query(ctx, `SELECT f.id::text,fs.source_id::text FROM feeds f LEFT JOIN feed_sources fs ON fs.instance_id=f.instance_id AND fs.feed_id=f.id
 WHERE f.instance_id=$1 AND ($2='' OR f.id=NULLIF($2,'')::uuid) AND ($3='' OR f.format=$3) ORDER BY f.id,fs.priority`, p.InstanceID, feed, format)
	if err != nil {
		packageError(w, r, err)
		return
	}
	var feedOrder []string
	sources := map[string][]string{}
	for frows.Next() {
		var f string
		var s *string
		if err = frows.Scan(&f, &s); err != nil {
			break
		}
		if _, seen := sources[f]; !seen {
			feedOrder = append(feedOrder, f)
			sources[f] = []string{}
		}
		if s != nil {
			sources[f] = append(sources[f], *s)
		}
	}
	if err == nil {
		err = frows.Err()
	}
	frows.Close()
	if err != nil {
		packageError(w, r, err)
		return
	}
	permissions := []searchPermission{}
	for _, f := range feedOrder {
		perms, e := packagePermissions(p, policy, f, sources[f])
		if e != nil {
			packageError(w, r, e)
			return
		}
		for _, perm := range perms {
			permissions = append(permissions, searchPermission{f, perm})
		}
		if len(permissions) > maxSearchPermissions {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "search too broad; narrow it with the feed or format filter")
			return
		}
	}
	page := PackageSearchPage{Items: []PackageSearchResult{}}
	if len(permissions) == 0 {
		JSON(w, 200, page)
		return
	}
	encoded, _ := json.Marshal(permissions)
	if err = h.db.QueryRow(ctx, searchPackagesSQL+`SELECT count(*) FROM visible`, p.InstanceID, encoded, q, origin).Scan(&page.Total); err != nil {
		packageError(w, r, err)
		return
	}
	rows, err := h.db.Query(ctx, searchPackagesSQL+`SELECT feed_id::text,slug,format,id::text,coordinate,namespace,source_id::text,kind,CASE WHEN octet_length(metadata::text)<=65536 THEN metadata END FROM visible
 WHERE ($5::text='' OR (feed_id,id)>(NULLIF($5::text,'')::uuid,NULLIF($6::text,'')::uuid)) ORDER BY feed_id,id LIMIT $7`, p.InstanceID, encoded, q, origin, afterFeed, afterID, limit+1)
	if err != nil {
		packageError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var item PackageSearchResult
		if err = rows.Scan(&item.FeedID, &item.FeedSlug, &item.Format, &item.ID, &item.Coordinate, &item.Namespace, &item.SourceID, &item.Origin, &item.Metadata); err != nil || item.Metadata == nil {
			packageError(w, r, identity.ErrUnavailable)
			return
		}
		page.Items = append(page.Items, item)
	}
	if rows.Err() != nil {
		packageError(w, r, rows.Err())
		return
	}
	more := len(page.Items) > limit
	if more {
		page.Items = page.Items[:limit]
	}
	ids := make([]string, len(page.Items))
	for i := range page.Items {
		ids[i] = page.Items[i].ID
	}
	stats, err := loadPackageStats(ctx, h.db, p.InstanceID, ids)
	if err != nil {
		packageError(w, r, err)
		return
	}
	for i := range page.Items {
		page.Items[i].PackageStats = stats[page.Items[i].ID]
	}
	if more {
		last := page.Items[limit-1]
		token, e := h.codec.Encode(scope, last.FeedID+"/"+last.ID)
		if e != nil {
			Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
			return
		}
		page.Page.HasMore, page.Page.NextCursor = true, &token
	}
	JSON(w, 200, page)
}

// ---- Resolution explain ----

type ResolveSource struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Priority int32  `json:"priority"`
	Role     string `json:"role"`
	Reason   string `json:"reason"`
	Versions int64  `json:"versions"`
}
type ResolveCache struct {
	State                string     `json:"state"`
	LastCachedAt         *time.Time `json:"last_cached_at,omitempty"`
	LastAccessedAt       *time.Time `json:"last_accessed_at,omitempty"`
	AgeSeconds           *int64     `json:"age_seconds,omitempty"`
	MetadataTTLSeconds   int        `json:"metadata_ttl_seconds"`
	StaleIfErrorSeconds  int        `json:"stale_if_error_seconds"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	ServableStaleOnError bool       `json:"servable_stale_on_error"`
}
type ResolveExplanation struct {
	FeedID        string          `json:"feed_id"`
	Coordinate    string          `json:"coordinate"`
	Namespace     string          `json:"namespace"`
	Status        string          `json:"status"`
	Serving       *ResolveSource  `json:"serving,omitempty"`
	Sources       []ResolveSource `json:"sources"`
	Cache         ResolveCache    `json:"cache"`
	HiddenSources int             `json:"hidden_sources"`
	// Federation is the placement-aware routing plan when the feed is bound
	// to a federation feed key (instance readers only, P2-M5).
	Federation *federation.RoutePlan `json:"federation,omitempty"`
}

func validCoordinate(c string) bool {
	return c != "" && len(c) <= 256 && utf8.ValidString(c) && !strings.ContainsAny(c, "\x00\r\n")
}

func (h consoleHandlers) resolveExplain(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	if !valid || !singleParams(w, r, "coordinate", "namespace") {
		if !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		}
		return
	}
	coordinate := r.URL.Query().Get("coordinate")
	namespace, hasNamespace := r.URL.Query().Get("namespace"), r.URL.Query().Has("namespace")
	if !validCoordinate(coordinate) || len(namespace) > 256 || !validText(namespace, 256) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid coordinate")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, feed, identity.Read) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	var exists bool
	if err := h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)`, p.InstanceID, feed).Scan(&exists); err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "resolution unavailable")
		return
	}
	if !exists {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	attached, err := (artifact.Policies{DB: h.db, InstanceID: p.InstanceID}).ExplainRead(r.Context(), feed, coordinate)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "resolution unavailable")
		return
	}
	out := ResolveExplanation{FeedID: feed, Coordinate: coordinate, Namespace: namespace, Status: "not_found", Sources: []ResolveSource{}, Cache: ResolveCache{State: "not_applicable"}}
	owner := -1
	for i, s := range attached {
		if s.HasComponent {
			owner = i
			break
		}
	}
	readable := func(s artifact.AttachedSource, ns string) bool {
		return policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: s.SourceID, Namespace: ns, Kind: identity.ArtifactResource}, identity.Read)
	}
	if owner >= 0 {
		o := attached[owner]
		if !hasNamespace {
			namespace = o.Namespace
			out.Namespace = namespace
		}
		switch {
		case !readable(o, o.Namespace) || !readable(o, namespace):
			// Reads return not-found for an owner the caller cannot read; do not
			// reveal that the coordinate exists or which source owns it.
			out.Namespace = namespace
			JSON(w, 200, out)
			return
		case o.Namespace != namespace:
			out.Status = "namespace_mismatch"
		default:
			out.Status = "served"
		}
	}
	now := time.Now()
	for i, s := range attached {
		item := ResolveSource{SourceID: s.SourceID, Name: s.Name, Kind: s.Kind, Priority: s.Priority, Versions: s.Versions}
		ns := namespace
		if s.HasComponent {
			ns = s.Namespace
		}
		if !readable(s, ns) {
			out.HiddenSources++
			continue
		}
		switch {
		case i == owner && out.Status == "served":
			item.Role, item.Reason = "serving", "first source in read order that contains the coordinate owns every version"
			copy := item
			out.Serving = &copy
		case i == owner:
			item.Role, item.Reason = "absent", "owner namespace differs from the requested namespace; reads return not found and never fall through"
		case s.HasComponent && owner >= 0 && i > owner:
			item.Role, item.Reason = "shadowed", "lower priority than "+attached[owner].Name+", which owns this coordinate"
		case s.HasComponent:
			item.Role, item.Reason = "absent", "not reached"
		case owner < 0 && s.Kind == "remote":
			item.Role, item.Reason = "pull_through_candidate", "not cached; would be fetched from the upstream on first read in read order"
		default:
			item.Role, item.Reason = "absent", "does not contain the coordinate"
		}
		out.Sources = append(out.Sources, item)
	}
	if owner < 0 {
		for _, s := range out.Sources {
			if s.Role == "pull_through_candidate" {
				out.Status = "pull_through"
				out.Cache.State = "not_cached"
				break
			}
		}
	} else if o := attached[owner]; o.Kind == "remote" && out.Status != "namespace_mismatch" {
		out.Cache = remoteCache(o, now)
	}
	if out.Federation = feedRoute(r.Context(), p, policy, feed); out.Federation != nil && out.Status == "not_found" &&
		(out.Federation.Decision == federation.RouteProxy || out.Federation.Decision == federation.RouteRedirect) {
		// A local miss is routed to the first eligible member holding the key.
		out.Status = "federated"
	}
	JSON(w, 200, out)
}

// remoteCache derives freshness from the newest cached asset and the source's
// configured metadata TTL. Immutable artifact bytes never expire; this reports
// when mutable upstream metadata for the package would be revalidated.
func remoteCache(o artifact.AttachedSource, now time.Time) ResolveCache {
	c := ResolveCache{State: "not_cached", LastCachedAt: o.LastCachedAt, LastAccessedAt: o.LastAccessedAt, MetadataTTLSeconds: o.MetadataTTLSeconds, StaleIfErrorSeconds: o.StaleIfErrorSeconds}
	if o.LastCachedAt == nil {
		return c
	}
	age := int64(now.Sub(*o.LastCachedAt).Seconds())
	if age < 0 {
		age = 0
	}
	c.AgeSeconds = &age
	if o.MetadataTTLSeconds > 0 {
		expires := o.LastCachedAt.Add(time.Duration(o.MetadataTTLSeconds) * time.Second)
		c.ExpiresAt = &expires
		if now.Before(expires) {
			c.State = "fresh"
		} else {
			c.State = "stale"
			c.ServableStaleOnError = o.StaleIfErrorSeconds > 0 && now.Before(expires.Add(time.Duration(o.StaleIfErrorSeconds)*time.Second))
		}
	} else {
		c.State = "stale"
	}
	return c
}

// ---- Publish-policy dry run ----

type PolicyTestTrace struct {
	RuleID   string `json:"rule_id"`
	Priority int64  `json:"priority"`
	Matched  bool   `json:"matched"`
	Selected bool   `json:"selected"`
	Reason   string `json:"reason"`
}
type PolicyTestResult struct {
	Allowed             bool              `json:"allowed"`
	Reason              string            `json:"reason"`
	MatchedRuleID       string            `json:"matched_rule_id,omitempty"`
	TargetSourceID      string            `json:"target_source_id,omitempty"`
	TargetSourceName    string            `json:"target_source_name,omitempty"`
	Denial              string            `json:"denial,omitempty"`
	PrincipalID         string            `json:"principal_id"`
	PrincipalAuthorized *bool             `json:"principal_authorized,omitempty"`
	Trace               []PolicyTestTrace `json:"trace"`
}

func (h consoleHandlers) testPublishPolicy(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	var body struct {
		Policy      json.RawMessage `json:"policy"`
		Coordinate  string          `json:"coordinate"`
		Namespace   string          `json:"namespace"`
		Path        string          `json:"path"`
		PrincipalID string          `json:"principal_id"`
	}
	if !sourceJSON(w, r, &body, "policy", "coordinate", "namespace", "path", "principal_id") {
		return
	}
	if !validCoordinate(body.Coordinate) || len(body.Namespace) > 256 || !validText(body.Namespace, 256) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid dry-run request")
		return
	}
	fields, err := exactObject(body.Policy, []string{"default_source_id", "rules"}, []string{"rules"})
	if err != nil {
		policyError(w, r, artifact.ErrInput)
		return
	}
	input, rules, err := parsePublishPolicy(fields)
	if err != nil {
		policyError(w, r, err)
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	if !policyAllowed(policy, p, feed, input.DefaultSourceID, rules) {
		policyError(w, r, artifact.ErrDenied)
		return
	}
	subject := p.PrincipalID
	if body.PrincipalID != "" {
		id, ok := feedID(body.PrincipalID)
		if !ok {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid principal ID")
			return
		}
		if id != p.PrincipalID && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
			Error(w, r, 403, api.ErrorCodeForbidden, "evaluating another principal requires instance administration")
			return
		}
		subject = id
	}
	policies := artifact.Policies{DB: h.db, InstanceID: p.InstanceID}
	var exists bool
	if err = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)`, p.InstanceID, feed).Scan(&exists); err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "dry run unavailable")
		return
	}
	if !exists {
		policyError(w, r, artifact.ErrNotFound)
		return
	}
	// Compile against the feed's real attached sources so an unattached or
	// non-local target is rejected exactly as saving would reject it.
	for _, target := range append([]string{input.DefaultSourceID}, ruleTargets(rules)...) {
		if target == "" {
			continue
		}
		var attached bool
		if err = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM feed_sources WHERE instance_id=$1 AND feed_id=$2 AND source_id=$3)`, p.InstanceID, feed, target).Scan(&attached); err != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "dry run unavailable")
			return
		}
		if !attached {
			policyError(w, r, artifact.ErrNotFound)
			return
		}
	}
	router, err := policies.Compile(r.Context(), feed, input.DefaultSourceID, rules)
	if err != nil {
		policyError(w, r, err)
		return
	}
	path := body.Path
	decision, err := router.Resolve(routing.Request{Namespace: body.Namespace, Package: body.Coordinate, Path: path, Principal: subject})
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid dry-run request")
		return
	}
	result := PolicyTestResult{Allowed: decision.Allowed, Reason: string(decision.Reason), MatchedRuleID: decision.RuleID, TargetSourceID: decision.TargetID, PrincipalID: subject, Trace: []PolicyTestTrace{}}
	for _, t := range decision.Trace {
		result.Trace = append(result.Trace, PolicyTestTrace{RuleID: t.RuleID, Priority: int64(t.Priority), Matched: t.Matched, Selected: t.Selected, Reason: string(t.Reason)})
	}
	if !decision.Allowed {
		result.Denial = string(decision.Reason)
	} else {
		_ = h.db.QueryRow(r.Context(), `SELECT name FROM sources WHERE instance_id=$1 AND id=$2`, p.InstanceID, decision.TargetID).Scan(&result.TargetSourceName)
		// Routing selects a destination; the principal must also hold publish
		// permission on it. Evaluate the shared policy for that principal.
		subjectPolicy, e := (identity.Roles{DB: h.db}).Resolve(r.Context(), identity.Principal{InstanceID: p.InstanceID, PrincipalID: subject})
		if e == nil {
			allowed := subjectPolicy.Authorize(identity.Principal{InstanceID: p.InstanceID, PrincipalID: subject}, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: decision.TargetID, Namespace: body.Namespace, Kind: identity.ArtifactResource}, identity.Publish)
			result.PrincipalAuthorized = &allowed
			if !allowed {
				result.Denial = "principal_not_permitted"
			}
		} else if !errors.Is(e, identity.ErrUnauthenticated) {
			Error(w, r, 503, api.ErrorCodeUnavailable, "authorization unavailable")
			return
		} else {
			denied := false
			result.PrincipalAuthorized = &denied
			result.Denial = "principal_not_permitted"
		}
	}
	JSON(w, 200, result)
}
func ruleTargets(rules []routing.Rule) []string {
	var out []string
	for _, rule := range rules {
		out = append(out, rule.Target)
	}
	return out
}
