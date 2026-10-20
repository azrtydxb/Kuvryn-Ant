package retention

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
)

const manifestReadLimit = 4 << 20

// SQL fragments over an alias v of versions. A child is a manifest an image
// index names (version_children rows exist only while the parent does). Ranking
// for keep_last/min_keep covers top-level versions only; the last-used time of a
// top-level version includes its children, so an image is idle only when every
// platform manifest is.
const (
	childSQL    = `EXISTS(SELECT 1 FROM version_children vc WHERE vc.component_id=v.component_id AND vc.child_version=v.version)`
	lastUsedSQL = `GREATEST(COALESCE((SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM assets a LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE a.version_id=v.id),v.created_at),(SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM version_children vc JOIN versions cv ON cv.component_id=vc.component_id AND cv.version=vc.child_version JOIN assets a ON a.version_id=cv.id LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE vc.parent_version_id=v.id))`
	rankSQL     = `CASE WHEN v.blob_only OR ` + childSQL + ` THEN 2147483647 ELSE row_number() OVER (PARTITION BY (v.blob_only OR ` + childSQL + `) ORDER BY v.created_at DESC,v.id DESC)::int END`
)

func allIncludeTagged(policies []Policy) bool {
	for _, p := range policies {
		if !p.IncludeTagged {
			return false
		}
	}
	return len(policies) > 0
}

func deletionIDs(batch []Deletion) []string {
	out := make([]string, len(batch))
	for i, d := range batch {
		out[i] = d.VersionID
	}
	return out
}

// followers returns the deletions that must accompany deleting ids from an OCI
// component:
//   - children (index-grouped manifests) whose every parent is being deleted; a
//     child another index still names, or a tagged one (unless includeTagged),
//     stays;
//   - blob-only versions (config and layers) named only by the manifests being
//     deleted, children included. A blob a remaining manifest names stays, and
//     every blob stays when a remaining manifest cannot be read (fail closed).
//
// Bytes are never deleted here; released rows leave their blobs to garbage
// collection with its usual grace period. Other formats return nothing.
func (s *Service) followers(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, sourceID, component, coordinate string, ids []string, includeTagged bool) ([]Deletion, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var format string
	if err := q.QueryRow(ctx, `SELECT format FROM sources WHERE id=$1`, sourceID).Scan(&format); err != nil {
		return nil, err
	}
	if format != "oci" && format != "oci-artifacts" {
		return nil, nil
	}
	type row struct {
		id, version     string
		blobOnly, child bool
		tagged          bool
	}
	rows, err := q.Query(ctx, `SELECT v.id::text,v.version,v.blob_only,`+childSQL+`,EXISTS(SELECT 1 FROM tags t WHERE t.component_id=v.component_id AND t.version_id=v.id) FROM versions v WHERE v.component_id=$1`, component)
	if err != nil {
		return nil, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.version, &r.blobOnly, &r.child, &r.tagged); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	doomed := map[string]bool{}
	for _, id := range ids {
		doomed[id] = true
	}
	// parents of each child, by child version string
	parents := map[string][]string{}
	prows, err := q.Query(ctx, `SELECT vc.child_version,vc.parent_version_id::text FROM version_children vc WHERE vc.component_id=$1`, component)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var child, parent string
		if err = prows.Scan(&child, &parent); err != nil {
			prows.Close()
			return nil, err
		}
		parents[child] = append(parents[child], parent)
	}
	prows.Close()
	if err = prows.Err(); err != nil {
		return nil, err
	}
	var out []Deletion
	for changed := true; changed; {
		changed = false
		for _, r := range all {
			if doomed[r.id] || !r.child || r.blobOnly || (r.tagged && !includeTagged) {
				continue
			}
			gone := len(parents[r.version]) > 0
			for _, p := range parents[r.version] {
				if !doomed[p] {
					gone = false
				}
			}
			if gone {
				doomed[r.id] = true
				changed = true
				out = append(out, Deletion{SourceID: sourceID, Coordinate: coordinate, Version: r.version, VersionID: r.id, Reason: "follows_parent"})
			}
		}
	}
	manifestsDoomed := false
	for _, r := range all {
		if doomed[r.id] && !r.blobOnly {
			manifestsDoomed = true
		}
	}
	if !manifestsDoomed {
		return out, nil
	}
	rows, err = q.Query(ctx, `SELECT v.id::text,b.digest,b.dedup_domain_id::text FROM versions v JOIN assets a ON a.version_id=v.id AND a.asset_key='manifest' JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND NOT v.blob_only AND b.size_bytes<=$2`, component, int64(manifestReadLimit))
	if err != nil {
		return nil, err
	}
	type manifest struct{ id, digest, domain string }
	var manifests []manifest
	for rows.Next() {
		var m manifest
		if err = rows.Scan(&m.id, &m.digest, &m.domain); err != nil {
			rows.Close()
			return nil, err
		}
		manifests = append(manifests, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	named := map[string]bool{}    // digests named by doomed manifests
	retained := map[string]bool{} // digests named by manifests that stay
	for _, m := range manifests {
		refs, ok := s.manifestRefs(ctx, m.domain, m.digest)
		switch {
		case doomed[m.id]:
			for _, r := range refs {
				named[r] = true
			}
		case !ok:
			return out, nil
		default:
			for _, r := range refs {
				retained[r] = true
			}
		}
	}
	for _, r := range all {
		if r.blobOnly && !doomed[r.id] && named[r.version] && !retained[r.version] {
			out = append(out, Deletion{SourceID: sourceID, Coordinate: coordinate, Version: r.version, VersionID: r.id, Reason: "unreferenced_blob"})
		}
	}
	return out, nil
}

// manifestRefs reads a stored manifest or index and returns the hex digests it
// names (config, layers, child manifests). ok is false when it cannot be read
// or parsed.
func (s *Service) manifestRefs(ctx context.Context, domain, digest string) (refs []string, ok bool) {
	store, err := s.Stores(ctx, domain)
	if err != nil {
		return nil, false
	}
	rc, err := store.OpenRange(ctx, blob.Digest(digest), 0, -1)
	if err != nil {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(rc, manifestReadLimit))
	_ = rc.Close()
	if err != nil {
		return nil, false
	}
	var doc struct {
		Config    *struct{ Digest string }  `json:"config"`
		Layers    []struct{ Digest string } `json:"layers"`
		Manifests []struct{ Digest string } `json:"manifests"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	add := func(d string) { refs = append(refs, strings.TrimPrefix(d, "sha256:")) }
	if doc.Config != nil {
		add(doc.Config.Digest)
	}
	for _, l := range doc.Layers {
		add(l.Digest)
	}
	for _, m := range doc.Manifests {
		add(m.Digest)
	}
	return refs, true
}

// rowQuerier is what the helpers need from a pool or a transaction.
type rowQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// protectedBlobs returns, among the doomed versions of a component, the
// blob-only versions that must NOT be deleted: those a remaining visible
// manifest (top-level or child) names, and all of them when a remaining
// manifest cannot be read (fail closed). A blob-only version no manifest names
// is an orphan and may go. Blob-only rows are never deleted on their own
// timestamps while a manifest still needs them.
func (s *Service) protectedBlobs(ctx context.Context, q rowQuerier, component string, doomed map[string]bool) (map[string]bool, error) {
	ids := make([]string, 0, len(doomed))
	for id := range doomed {
		ids = append(ids, id)
	}
	rows, err := q.Query(ctx, `SELECT id::text,version FROM versions WHERE component_id=$1 AND blob_only AND id=ANY($2::uuid[])`, component, ids)
	if err != nil {
		return nil, err
	}
	blobVersion := map[string]string{}
	for rows.Next() {
		var id, version string
		if err = rows.Scan(&id, &version); err != nil {
			rows.Close()
			return nil, err
		}
		blobVersion[id] = version
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	if len(blobVersion) == 0 {
		return keep, nil
	}
	named, ok, err := s.namedByManifests(ctx, q, component, doomed)
	if err != nil {
		return nil, err
	}
	for id, version := range blobVersion {
		if !ok || named[version] {
			keep[id] = true
		}
	}
	return keep, nil
}

// namedByManifests returns the digests (hex) named by the visible manifests of a
// component that are not in except. ok is false when any of them is unreadable.
func (s *Service) namedByManifests(ctx context.Context, q rowQuerier, component string, except map[string]bool) (named map[string]bool, ok bool, err error) {
	rows, err := q.Query(ctx, `SELECT v.id::text,b.digest,b.dedup_domain_id::text,b.size_bytes FROM versions v JOIN assets a ON a.version_id=v.id AND a.asset_key='manifest' JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND NOT v.blob_only`, component)
	if err != nil {
		return nil, false, err
	}
	type manifest struct {
		id, digest, domain string
		size               int64
	}
	var manifests []manifest
	for rows.Next() {
		var m manifest
		if err = rows.Scan(&m.id, &m.digest, &m.domain, &m.size); err != nil {
			rows.Close()
			return nil, false, err
		}
		manifests = append(manifests, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	named = map[string]bool{}
	for _, m := range manifests {
		if except[m.id] {
			continue
		}
		if m.size > manifestReadLimit {
			return nil, false, nil
		}
		refs, readable := s.manifestRefs(ctx, m.domain, m.digest)
		if !readable {
			return nil, false, nil
		}
		for _, r := range refs {
			named[r] = true
		}
	}
	return named, true, nil
}

// dropProtected removes the protected blob-only versions from deletions.
func (s *Service) dropProtected(ctx context.Context, q rowQuerier, component string, deletions []Deletion) ([]Deletion, error) {
	doomed := map[string]bool{}
	for _, d := range deletions {
		doomed[d.VersionID] = true
	}
	keep, err := s.protectedBlobs(ctx, q, component, doomed)
	if err != nil || len(keep) == 0 {
		return deletions, err
	}
	out := deletions[:0:0]
	for _, d := range deletions {
		if !keep[d.VersionID] {
			out = append(out, d)
		}
	}
	return out, nil
}
