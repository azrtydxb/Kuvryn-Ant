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

// unreferencedBlobs finds the blob-only versions (OCI config and layers) of a
// component that only the manifests in batch named, so deleting those manifests
// releases them. A blob-only row still named by a remaining manifest is kept,
// as is every row when any remaining manifest cannot be read: the decision
// fails closed and the rows fall to idle or age eviction as before. Bytes are
// never deleted here; releasing the rows leaves their blobs to garbage
// collection with its usual grace period.
func (s *Service) unreferencedBlobs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, src sourceInfo, component, coordinate string, versions []versionRow, batch []Deletion) ([]Deletion, error) {
	doomed := map[string]bool{}
	for _, d := range batch {
		doomed[d.VersionID] = true
	}
	manifestsDoomed := false
	for _, v := range versions {
		if doomed[v.ID] && !v.BlobOnly {
			manifestsDoomed = true
		}
	}
	if !manifestsDoomed {
		return nil, nil
	}
	var format string
	if err := q.QueryRow(ctx, `SELECT format FROM sources WHERE id=$1`, src.ID).Scan(&format); err != nil {
		return nil, err
	}
	if format != "oci" && format != "oci-artifacts" {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT v.id::text,b.digest,b.dedup_domain_id::text FROM versions v JOIN assets a ON a.version_id=v.id AND a.asset_key='manifest' JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND NOT v.blob_only AND b.size_bytes<=$2`, component, int64(manifestReadLimit))
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
			return nil, nil
		default:
			for _, r := range refs {
				retained[r] = true
			}
		}
	}
	var out []Deletion
	for _, v := range versions {
		if v.BlobOnly && !doomed[v.ID] && named[v.Version] && !retained[v.Version] {
			out = append(out, Deletion{SourceID: src.ID, Coordinate: coordinate, Version: v.Version, VersionID: v.ID, Reason: "unreferenced_blob"})
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
