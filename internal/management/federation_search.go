package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cross-instance search (docs/planning/phase-2-federation.md section 7).
//
//	GET /api/v1/federation/search?q=&format=&namespace=&digest=&feed_key=&members=&limit=&cursor=
//
// The member that receives the request searches its own feeds under the
// caller's policy and fans the query out to the other members with a signed
// principal assertion; each member applies its own authorization. Only feeds
// that carry a federation feed key take part. A digest query is the checksum
// lookup (find every holder of a blob). Pagination uses one global keyset
// watermark on (feed_key, coordinate) that every member honors, wrapped in
// this member's authenticated cursor, so a tampered or foreign cursor is a 400.

type federationSearchHandlers struct {
	federationHandlers
	codec CursorCodec
}

// RegisterFederationSearch mounts the federated search endpoint and installs
// this member's package query as the peer search implementation.
func RegisterFederationSearch(mux *http.ServeMux, pool *pgxpool.Pool, svc *federation.Service, codec CursorCodec) {
	h := federationSearchHandlers{federationHandlers{identityHandlers{feedHandlers{db: pool, codec: codec}}, svc}, codec}
	if svc != nil {
		svc.SetAuditSearcher(func(ctx context.Context, p identity.Principal, policy identity.Policy, q federation.AuditQuery) ([]federation.AuditEntry, error) {
			return queryFederatedAudit(ctx, pool, p, policy, q)
		})
		svc.SetPackageSearcher(func(ctx context.Context, p identity.Principal, policy identity.Policy, q federation.SearchQuery, bindings map[string]string) ([]federation.SearchHit, error) {
			return searchBoundPackages(ctx, pool, p, policy, q, bindings)
		})
	}
	mux.Handle("GET /api/v1/federation/search", identity.NewBearer(identity.Tokens{DB: pool}).Wrap(http.HandlerFunc(h.search)))
	mux.Handle("GET /api/v1/federation/audit", identity.NewBearer(identity.Tokens{DB: pool}).Wrap(http.HandlerFunc(h.audit)))
}

// searchBoundSQL mirrors searchPackagesSQL (owner selection per feed before
// authorization and owner-dependent filters) and orders by the portable
// (feed_key, coordinate) key in byte order, which is the order the merge uses.
const searchBoundSQL = `WITH perms AS (
 SELECT * FROM jsonb_to_recordset($2::jsonb) AS p(feed_id uuid,feed_key text,source_id uuid,base boolean,namespaces text[])
), owners AS (
 SELECT DISTINCT ON(fs.feed_id,c.coordinate) fs.feed_id,c.id,c.source_id,c.coordinate,c.namespace,c.metadata
 FROM feed_sources fs JOIN components c ON (c.instance_id,c.source_id)=(fs.instance_id,fs.source_id)
 WHERE fs.instance_id=$1 AND fs.feed_id IN (SELECT feed_id FROM perms) AND ($3::text='' OR strpos(lower(c.coordinate),lower($3::text))>0)
 ORDER BY fs.feed_id,c.coordinate,fs.priority
), visible AS (
 SELECT o.*,p.feed_key,f.slug,f.format,s.kind FROM owners o
 JOIN perms p ON p.feed_id=o.feed_id AND p.source_id=o.source_id
 JOIN feeds f ON f.instance_id=$1 AND f.id=o.feed_id JOIN sources s ON s.instance_id=$1 AND s.id=o.source_id
 WHERE o.namespace IS NOT NULL AND p.base<>(o.namespace=ANY(p.namespaces))
 AND (NOT $4::boolean OR o.namespace=$5::text)
 AND ($6::text='' OR EXISTS(SELECT 1 FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=o.id AND b.digest=$6::text))
) SELECT feed_key,slug,format,coordinate,namespace,kind,CASE WHEN octet_length(metadata::text)<=8192 THEN metadata END FROM visible
 WHERE ($7::text='' AND $8::text='') OR (feed_key COLLATE "C",coordinate COLLATE "C")>($7::text,$8::text)
 ORDER BY feed_key COLLATE "C",coordinate COLLATE "C" LIMIT $9`

// searchBoundPackages is the member-local federated package search for one
// principal under its resolved policy, over federation-keyed feeds only.
func searchBoundPackages(ctx context.Context, db *pgxpool.Pool, p identity.Principal, policy identity.Policy, q federation.SearchQuery, bindings map[string]string) ([]federation.SearchHit, error) {
	keyOf := map[string]string{}
	var ids []string
	for key, id := range bindings {
		if q.FeedKey != "" && key != q.FeedKey {
			continue
		}
		keyOf[id] = key
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []federation.SearchHit{}, nil
	}
	sort.Strings(ids)
	rows, err := db.Query(ctx, `SELECT f.id::text,fs.source_id::text FROM feeds f LEFT JOIN feed_sources fs ON fs.instance_id=f.instance_id AND fs.feed_id=f.id
 WHERE f.instance_id=$1 AND f.id=ANY($2::uuid[]) AND ($3='' OR f.format=$3) ORDER BY f.id,fs.priority`, p.InstanceID, ids, q.Format)
	if err != nil {
		return nil, err
	}
	var order []string
	sources := map[string][]string{}
	for rows.Next() {
		var f string
		var s *string
		if err = rows.Scan(&f, &s); err != nil {
			break
		}
		if _, seen := sources[f]; !seen {
			order = append(order, f)
			sources[f] = []string{}
		}
		if s != nil {
			sources[f] = append(sources[f], *s)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	type permission struct {
		FeedID  string `json:"feed_id"`
		FeedKey string `json:"feed_key"`
		packagePermission
	}
	perms := []permission{}
	for _, f := range order {
		fp, e := packagePermissions(p, policy, f, sources[f])
		if e != nil {
			return nil, e
		}
		for _, x := range fp {
			perms = append(perms, permission{f, keyOf[f], x})
		}
		if len(perms) > maxSearchPermissions {
			return nil, identity.ErrUnavailable
		}
	}
	if len(perms) == 0 {
		return []federation.SearchHit{}, nil
	}
	encoded, _ := json.Marshal(perms)
	hrows, err := db.Query(ctx, searchBoundSQL, p.InstanceID, encoded, q.Q, q.HasNamespace, q.Namespace, q.Digest, q.AfterFeedKey, q.AfterCoordinate, q.Limit+1)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	hits := []federation.SearchHit{}
	for hrows.Next() {
		var h federation.SearchHit
		if err = hrows.Scan(&h.FeedKey, &h.FeedSlug, &h.Format, &h.Coordinate, &h.Namespace, &h.Origin, &h.Metadata); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, hrows.Err()
}

func (h federationSearchHandlers) search(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "q", "format", "namespace", "digest", "feed_key", "members", "limit", "cursor") {
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	values := r.URL.Query()
	q := federation.SearchQuery{Q: values.Get("q"), Format: values.Get("format"), Namespace: values.Get("namespace"), HasNamespace: values.Has("namespace"),
		FeedKey: values.Get("feed_key"), Limit: 50}
	if d := values.Get("digest"); d != "" {
		hexDigest, found := strings.CutPrefix(d, "sha256:")
		if !found {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "digest must be sha256:<64 lowercase hex>")
			return
		}
		q.Digest = hexDigest
	}
	if v := values.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > federation.MaxSearchLimit {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "limit must be between 1 and 100")
			return
		}
		q.Limit = n
	}
	if q.Format != "" && !formats[q.Format] {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid package search")
		return
	}
	var members []string
	if v := values.Get("members"); v != "" {
		members = strings.Split(v, ",")
		if len(members) > 64 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "too many members")
			return
		}
	}
	if err := q.Validate(); err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid package search")
		return
	}
	scope := scopeHash("federation-search", p.InstanceID, p.PrincipalID, p.TokenID, q.Q, q.Format, q.HasNamespace, q.Namespace, q.Digest, q.FeedKey, strings.Join(members, ","), q.Limit)
	if c := values.Get("cursor"); c != "" {
		after, err := h.codec.Decode(scope, c)
		var mark []string
		if err != nil || json.Unmarshal([]byte(after), &mark) != nil || len(mark) != 2 || mark[0] == "" || mark[1] == "" {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
		q.AfterFeedKey, q.AfterCoordinate = mark[0], mark[1]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := svc.FederatedSearch(ctx, p, q, members, func(ctx context.Context) ([]federation.SearchHit, error) {
		list, err := svc.FeedBindings(ctx)
		if err != nil {
			return nil, err
		}
		bindings := map[string]string{}
		for _, b := range list {
			bindings[b.FeedKey] = b.FeedID
		}
		return searchBoundPackages(ctx, h.db, p, policy, q, bindings)
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			Error(w, r, 503, api.ErrorCodeUnavailable, "federated search unavailable")
			return
		}
		federationError(w, r, err)
		return
	}
	page := api.PageInfo{HasMore: res.More}
	if res.More && len(res.Items) > 0 {
		last := res.Items[len(res.Items)-1]
		mark, _ := json.Marshal([]string{last.FeedKey, last.Coordinate})
		token, err := h.codec.Encode(scope, string(mark))
		if err != nil {
			Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
			return
		}
		page.NextCursor = &token
	}
	JSON(w, 200, struct {
		Items   []federation.MergedHit          `json:"items"`
		Members []federation.MemberSearchStatus `json:"members"`
		Partial bool                            `json:"partial"`
		Page    api.PageInfo                    `json:"page"`
	}{res.Items, res.Members, res.Partial, page})
}
