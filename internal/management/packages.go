package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PackageRecord struct {
	ID         string          `json:"id"`
	Coordinate string          `json:"coordinate"`
	Namespace  string          `json:"namespace"`
	SourceID   string          `json:"source_id"`
	Metadata   json.RawMessage `json:"metadata"`
	PackageStats
}
type PackageVersion struct {
	ID          string    `json:"id"`
	ComponentID string    `json:"component_id"`
	Version     string    `json:"version"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
	// ParentVersion and Platform are set for a per-platform manifest grouped
	// under an image index: the index version and the os/arch it provides.
	ParentVersion *string `json:"parent_version"`
	Platform      *string `json:"platform"`
}
type PackageTag struct {
	Name       string     `json:"name"`
	VersionID  string     `json:"version_id"`
	Revision   int64      `json:"revision"`
	ObservedAt *time.Time `json:"observed_at"`
}
type PackageAsset struct {
	ID          string          `json:"id"`
	ComponentID string          `json:"component_id"`
	VersionID   string          `json:"version_id"`
	SourceID    string          `json:"source_id"`
	Key         string          `json:"key"`
	MediaType   string          `json:"media_type"`
	Digest      string          `json:"digest"`
	SizeBytes   int64           `json:"size_bytes"`
	CreatedAt   time.Time       `json:"created_at"`
	UploaderID  *string         `json:"uploader_id,omitempty"`
	Provenance  json.RawMessage `json:"provenance"`
}
type packageHandlers struct{ feedHandlers }

// RegisterPackages exposes metadata from authorized package owners only. It
// never enumerates blob storage or falls back to shadowed source versions.
func RegisterPackages(mux *http.ServeMux, pool *pgxpool.Pool, codec CursorCodec) {
	h := packageHandlers{feedHandlers{db: pool, codec: codec}}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/feeds/{id}/packages":                                       h.listPackages,
		"GET /api/v1/feeds/{id}/packages/{component}/versions":                  h.listVersions,
		"GET /api/v1/feeds/{id}/packages/{component}/tags":                      h.listTags,
		"GET /api/v1/feeds/{id}/packages/{component}/versions/{version}/assets": h.listAssets,
		"GET /api/v1/feeds/{id}/assets/{asset}":                                 h.getAsset,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
	h.registerDeletes(mux, bearer)
}

type packagePermission struct {
	SourceID   string   `json:"source_id"`
	Base       bool     `json:"base"`
	Namespaces []string `json:"namespaces"`
}

// For a fixed source/feed, authorization can only change at explicit namespace
// selectors. Evaluate the shared policy at every such partition, then pass its
// exact truth table to SQL. No authorization semantics are reimplemented in SQL.
func packagePermissions(p identity.Principal, policy identity.Policy, feed string, sources []string) ([]packagePermission, error) {
	names := map[string]bool{}
	grantCount := len(policy.Grants)
	for _, role := range policy.Roles {
		grantCount += len(role.Grants)
	}
	if grantCount > 4096 {
		return nil, identity.ErrUnavailable
	}
	for _, g := range policy.Grants {
		if g.Namespace != "" {
			names[g.Namespace] = true
		}
	}
	for _, r := range policy.Roles {
		for _, g := range r.Grants {
			if g.Namespace != "" {
				names[g.Namespace] = true
			}
		}
	}
	for _, scope := range p.Scopes {
		if scope.Namespace != "" {
			names[scope.Namespace] = true
		}
	}
	if len(sources) > 1024 || len(names) > 4096 || len(sources)*(len(names)+1) > 8192 {
		return nil, identity.ErrUnavailable
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	result := []packagePermission{}
	for _, source := range sources {
		resource := identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Kind: identity.ArtifactResource}
		record := packagePermission{SourceID: source, Base: policy.Authorize(p, resource, identity.Read), Namespaces: []string{}}
		for _, name := range sorted {
			resource.Namespace = name
			if policy.Authorize(p, resource, identity.Read) != record.Base {
				record.Namespaces = append(record.Namespaces, name)
			}
		}
		if record.Base || len(record.Namespaces) > 0 {
			result = append(result, record)
		}
	}
	return result, nil
}
func packageError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, database.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, 404, api.ErrorCodeNotFound, "package resource not found")
	} else {
		Error(w, r, 503, api.ErrorCodeUnavailable, "package discovery unavailable")
	}
}
func packageInput(w http.ResponseWriter, r *http.Request) {
	Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid package query")
}
func (h packageHandlers) access(w http.ResponseWriter, r *http.Request) (identity.Principal, string, []byte, bool) {
	feed, ok := feedID(r.PathValue("id"))
	if !ok {
		packageInput(w, r)
		return identity.Principal{}, "", nil, false
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return p, "", nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)`, p.InstanceID, feed).Scan(&exists); err != nil {
		packageError(w, r, err)
		return p, "", nil, false
	}
	if !exists {
		packageError(w, r, database.ErrNotFound)
		return p, "", nil, false
	}
	rows, err := h.db.Query(ctx, `SELECT source_id::text FROM feed_sources WHERE instance_id=$1 AND feed_id=$2 ORDER BY priority LIMIT 1025`, p.InstanceID, feed)
	if err != nil {
		packageError(w, r, err)
		return p, "", nil, false
	}
	var sources []string
	for rows.Next() {
		var source string
		if err = rows.Scan(&source); err != nil {
			break
		}
		sources = append(sources, source)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil {
		packageError(w, r, identity.ErrUnavailable)
		return p, "", nil, false
	}
	permissions, err := packagePermissions(p, policy, feed, sources)
	if err != nil {
		packageError(w, r, err)
		return p, "", nil, false
	}
	if len(permissions) == 0 && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
		packageError(w, r, database.ErrNotFound)
		return p, "", nil, false
	}
	encoded, _ := json.Marshal(permissions)
	return p, feed, encoded, true
}

// Owner selection precedes authorization, namespace/digest filtering, and
// pagination. Hidden higher-priority packages never expose lower-priority data.
const packageOwnerSQL = `WITH owners AS (
 SELECT DISTINCT ON(c.coordinate) c.id,c.source_id,c.coordinate,c.namespace,c.metadata
 FROM feed_sources fs JOIN components c ON (c.instance_id,c.source_id)=(fs.instance_id,fs.source_id)
 WHERE fs.instance_id=$1 AND fs.feed_id=$2 ORDER BY c.coordinate,fs.priority
), visible AS (
 SELECT o.* FROM owners o JOIN jsonb_to_recordset($3::jsonb) AS p(source_id uuid,base boolean,namespaces text[]) ON p.source_id=o.source_id
 WHERE o.namespace IS NOT NULL AND p.base<>(o.namespace=ANY(p.namespaces))
) `

var packageComponentSQL = strings.Replace(packageOwnerSQL, "ORDER BY c.coordinate,fs.priority", "AND c.coordinate=(SELECT coordinate FROM components WHERE instance_id=$1 AND id=$4) ORDER BY c.coordinate,fs.priority", 1)
var packageAssetSQL = strings.Replace(packageOwnerSQL, "ORDER BY c.coordinate,fs.priority", "AND c.coordinate=(SELECT c.coordinate FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE a.instance_id=$1 AND a.id=$4) ORDER BY c.coordinate,fs.priority", 1)
var packageSearchSQL = strings.Replace(packageOwnerSQL, "ORDER BY c.coordinate,fs.priority", "AND starts_with(c.coordinate,$5) ORDER BY c.coordinate,fs.priority", 1)

type packageQuery struct {
	PageQuery
	Namespace, Digest string
	HasNamespace      bool
	TopLevel          bool
	After, Scope      string
}

func (h packageHandlers) query(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, search bool) (packageQuery, bool) {
	values := r.URL.Query()
	q := packageQuery{}
	if search {
		if v, ok := values["namespace"]; ok {
			if len(v) != 1 || len(v[0]) > 256 || !utf8.ValidString(v[0]) || strings.ContainsFunc(v[0], unicode.IsControl) {
				packageInput(w, r)
				return q, false
			}
			q.HasNamespace = true
			q.Namespace = v[0]
			values.Del("namespace")
		}
		if v, ok := values["digest"]; ok {
			if len(v) != 1 || len(v[0]) != 71 || !strings.HasPrefix(v[0], "sha256:") {
				packageInput(w, r)
				return q, false
			}
			digest := strings.TrimPrefix(v[0], "sha256:")
			decoded, err := hex.DecodeString(digest)
			if err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
				packageInput(w, r)
				return q, false
			}
			q.Digest = digest
			values.Del("digest")
		}
	}
	if v, ok := values["top_level"]; ok && strings.HasSuffix(r.URL.Path, "/versions") {
		if len(v) != 1 || (v[0] != "true" && v[0] != "false") {
			packageInput(w, r)
			return q, false
		}
		q.TopLevel = v[0] == "true"
		values.Del("top_level")
	}
	page, err := ParsePage(values)
	if err != nil {
		packageInput(w, r)
		return q, false
	}
	q.PageQuery = page
	scope, _ := json.Marshal([]any{p.InstanceID, p.PrincipalID, p.TokenID, feed, r.URL.Path, q.Prefix, q.HasNamespace, q.Namespace, q.Digest, q.TopLevel})
	hash := sha256.Sum256(scope)
	q.Scope = hex.EncodeToString(hash[:])
	if q.Cursor != "" {
		q.After, err = h.codec.Decode(q.Scope, q.Cursor)
		if err != nil {
			packageInput(w, r)
			return q, false
		}
		if !strings.HasSuffix(r.URL.Path, "/tags") {
			if _, ok := feedID(q.After); !ok {
				packageInput(w, r)
				return q, false
			}
		}
	}
	return q, true
}
func (h packageHandlers) page(w http.ResponseWriter, r *http.Request, q packageQuery, items any, more bool, last string) {
	page := api.PageInfo{HasMore: more}
	if more {
		cursor, err := h.codec.Encode(q.Scope, last)
		if err != nil {
			packageError(w, r, err)
			return
		}
		page.NextCursor = &cursor
	}
	JSON(w, 200, struct {
		Items any          `json:"items"`
		Page  api.PageInfo `json:"page"`
	}{items, page})
}
func (h packageHandlers) listPackages(w http.ResponseWriter, r *http.Request) {
	p, feed, permissions, ok := h.access(w, r)
	if !ok {
		return
	}
	q, ok := h.query(w, r, p, feed, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := h.db.Query(ctx, packageSearchSQL+`SELECT o.id::text,o.coordinate,o.namespace,o.source_id::text,CASE WHEN octet_length(o.metadata::text)<=65536 THEN o.metadata END FROM visible o WHERE ($4='' OR o.id>NULLIF($4,'')::uuid) AND starts_with(o.coordinate,$5) AND (NOT $7 OR o.namespace=$8) AND ($9='' OR EXISTS(SELECT 1 FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=o.id AND b.digest=$9)) ORDER BY o.id LIMIT $6`, p.InstanceID, feed, permissions, q.After, q.Prefix, q.Limit+1, q.HasNamespace, q.Namespace, q.Digest)
	if err != nil {
		packageError(w, r, err)
		return
	}
	defer rows.Close()
	items := []PackageRecord{}
	for rows.Next() {
		var item PackageRecord
		if err = rows.Scan(&item.ID, &item.Coordinate, &item.Namespace, &item.SourceID, &item.Metadata); err != nil || item.Metadata == nil {
			packageError(w, r, identity.ErrUnavailable)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		packageError(w, r, rows.Err())
		return
	}
	more := len(items) > q.Limit
	if more {
		items = items[:q.Limit]
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	stats, err := loadPackageStats(ctx, h.db, p.InstanceID, ids)
	if err != nil {
		packageError(w, r, err)
		return
	}
	for i := range items {
		items[i].PackageStats = stats[items[i].ID]
	}
	last := ""
	if len(items) > 0 {
		last = items[len(items)-1].ID
	}
	h.page(w, r, q, items, more, last)
}
func packagePathID(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	id, ok := feedID(r.PathValue(key))
	if !ok {
		packageInput(w, r)
	}
	return id, ok
}
func (h packageHandlers) component(ctx context.Context, p identity.Principal, feed string, permissions []byte, component string) error {
	var exists bool
	if err := h.db.QueryRow(ctx, packageComponentSQL+`SELECT EXISTS(SELECT 1 FROM visible WHERE id=$4)`, p.InstanceID, feed, permissions, component).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return database.ErrNotFound
	}
	return nil
}
func (h packageHandlers) listVersions(w http.ResponseWriter, r *http.Request) {
	p, feed, permissions, ok := h.access(w, r)
	if !ok {
		return
	}
	component, ok := packagePathID(w, r, "component")
	if !ok {
		return
	}
	q, ok := h.query(w, r, p, feed, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.component(ctx, p, feed, permissions, component); err != nil {
		packageError(w, r, err)
		return
	}
	rows, err := h.db.Query(ctx, packageComponentSQL+`SELECT v.id::text,v.component_id::text,v.version,v.revision,v.created_at,par.version,par.platform FROM visible o JOIN versions v ON v.component_id=o.id LEFT JOIN LATERAL (SELECT pv.version,NULLIF(vc.platform,'') AS platform FROM version_children vc JOIN versions pv ON pv.id=vc.parent_version_id WHERE vc.component_id=v.component_id AND vc.child_version=v.version ORDER BY pv.created_at,pv.id LIMIT 1) par ON true WHERE o.id=$4 AND (NOT $8 OR par.version IS NULL) AND ($5='' OR v.id>NULLIF($5,'')::uuid) AND starts_with(v.version,$6) ORDER BY v.id LIMIT $7`, p.InstanceID, feed, permissions, component, q.After, q.Prefix, q.Limit+1, q.TopLevel)
	if err != nil {
		packageError(w, r, err)
		return
	}
	defer rows.Close()
	items := []PackageVersion{}
	for rows.Next() {
		var item PackageVersion
		if err = rows.Scan(&item.ID, &item.ComponentID, &item.Version, &item.Revision, &item.CreatedAt, &item.ParentVersion, &item.Platform); err != nil {
			packageError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		packageError(w, r, rows.Err())
		return
	}
	more := len(items) > q.Limit
	if more {
		items = items[:q.Limit]
	}
	last := ""
	if len(items) > 0 {
		last = items[len(items)-1].ID
	}
	h.page(w, r, q, items, more, last)
}
func (h packageHandlers) listTags(w http.ResponseWriter, r *http.Request) {
	p, feed, permissions, ok := h.access(w, r)
	if !ok {
		return
	}
	component, ok := packagePathID(w, r, "component")
	if !ok {
		return
	}
	q, ok := h.query(w, r, p, feed, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.component(ctx, p, feed, permissions, component); err != nil {
		packageError(w, r, err)
		return
	}
	rows, err := h.db.Query(ctx, packageComponentSQL+`SELECT t.name,t.version_id::text,t.revision,t.observed_at FROM visible o JOIN tags t ON t.component_id=o.id WHERE o.id=$4 AND t.name>$5 AND starts_with(t.name,$6) ORDER BY t.name LIMIT $7`, p.InstanceID, feed, permissions, component, q.After, q.Prefix, q.Limit+1)
	if err != nil {
		packageError(w, r, err)
		return
	}
	defer rows.Close()
	items := []PackageTag{}
	for rows.Next() {
		var item PackageTag
		if err = rows.Scan(&item.Name, &item.VersionID, &item.Revision, &item.ObservedAt); err != nil {
			packageError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		packageError(w, r, rows.Err())
		return
	}
	more := len(items) > q.Limit
	if more {
		items = items[:q.Limit]
	}
	last := ""
	if len(items) > 0 {
		last = items[len(items)-1].Name
	}
	h.page(w, r, q, items, more, last)
}

const assetColumns = `a.id::text,v.component_id::text,v.id::text,o.source_id::text,a.asset_key,a.media_type,'sha256:'||b.digest,b.size_bytes,a.created_at,a.provenance->>'principal_id',CASE WHEN octet_length(a.provenance::text)<=65536 THEN a.provenance END`

func scanPackageAsset(row interface{ Scan(...any) error }) (PackageAsset, error) {
	var a PackageAsset
	err := row.Scan(&a.ID, &a.ComponentID, &a.VersionID, &a.SourceID, &a.Key, &a.MediaType, &a.Digest, &a.SizeBytes, &a.CreatedAt, &a.UploaderID, &a.Provenance)
	if err == nil && a.Provenance == nil {
		err = identity.ErrUnavailable
	}
	return a, err
}
func (h packageHandlers) listAssets(w http.ResponseWriter, r *http.Request) {
	p, feed, permissions, ok := h.access(w, r)
	if !ok {
		return
	}
	component, ok := packagePathID(w, r, "component")
	if !ok {
		return
	}
	version, ok := packagePathID(w, r, "version")
	if !ok {
		return
	}
	q, ok := h.query(w, r, p, feed, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := h.db.QueryRow(ctx, packageComponentSQL+`SELECT EXISTS(SELECT 1 FROM visible o JOIN versions v ON v.component_id=o.id WHERE o.id=$4 AND v.id=$5)`, p.InstanceID, feed, permissions, component, version).Scan(&exists); err != nil {
		packageError(w, r, err)
		return
	}
	if !exists {
		packageError(w, r, database.ErrNotFound)
		return
	}
	rows, err := h.db.Query(ctx, packageComponentSQL+`SELECT `+assetColumns+` FROM visible o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE o.id=$4 AND v.id=$5 AND ($6='' OR a.id>NULLIF($6,'')::uuid) AND starts_with(a.asset_key,$7) ORDER BY a.id LIMIT $8`, p.InstanceID, feed, permissions, component, version, q.After, q.Prefix, q.Limit+1)
	if err != nil {
		packageError(w, r, err)
		return
	}
	defer rows.Close()
	items := []PackageAsset{}
	for rows.Next() {
		item, err := scanPackageAsset(rows)
		if err != nil {
			packageError(w, r, err)
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		packageError(w, r, rows.Err())
		return
	}
	more := len(items) > q.Limit
	if more {
		items = items[:q.Limit]
	}
	last := ""
	if len(items) > 0 {
		last = items[len(items)-1].ID
	}
	h.page(w, r, q, items, more, last)
}
func (h packageHandlers) getAsset(w http.ResponseWriter, r *http.Request) {
	p, feed, permissions, ok := h.access(w, r)
	if !ok {
		return
	}
	asset, ok := packagePathID(w, r, "asset")
	if !ok {
		return
	}
	if len(r.URL.Query()) != 0 {
		packageInput(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	item, err := scanPackageAsset(h.db.QueryRow(ctx, packageAssetSQL+`SELECT `+assetColumns+` FROM visible o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE a.id=$4`, p.InstanceID, feed, permissions, asset))
	if err != nil {
		packageError(w, r, err)
		return
	}
	JSON(w, 200, item)
}
