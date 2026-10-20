package retention

import "context"

// The pressure projection estimates how many bytes evicting a set of top-level
// versions frees. Evicting an OCI index also deletes its platform manifests and,
// through followers, the config and layer rows only those manifests named, so
// the estimate counts them too: a child once every parent is picked, and a
// blob-only row once every manifest naming it is picked. A blob is freed when
// all assets referencing it are removed. The estimate can run high (a manifest
// it could not see may name a blob), never causing a deletion by itself: the
// real eviction recomputes under the coordinate lock.

type assetHold struct {
	asset, blob string
	size        int64
}

type imageGraph struct {
	ok        bool                   // every manifest was readable (blob rows can be counted)
	kids      map[string][]string    // parent version id -> child version ids
	parents   map[string][]string    // child version id -> parent version ids
	holds     map[string][]assetHold // version id (child or blob-only) -> its assets
	digestsOf map[string][]string    // manifest version id -> digests it names
	namers    map[string][]string    // digest -> manifest version ids naming it
	blobRow   map[string]string      // digest -> blob-only version id
}

func (s *Service) loadImageGraph(ctx context.Context, component string) (*imageGraph, error) {
	g := &imageGraph{ok: true, kids: map[string][]string{}, parents: map[string][]string{}, holds: map[string][]assetHold{}, digestsOf: map[string][]string{}, namers: map[string][]string{}, blobRow: map[string]string{}}
	rows, err := s.DB.Query(ctx, `SELECT vc.parent_version_id::text,cv.id::text FROM version_children vc JOIN versions cv ON cv.component_id=vc.component_id AND cv.version=vc.child_version WHERE vc.component_id=$1`, component)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var parent, child string
		if err = rows.Scan(&parent, &child); err != nil {
			rows.Close()
			return nil, err
		}
		g.kids[parent] = append(g.kids[parent], child)
		g.parents[child] = append(g.parents[child], parent)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT v.id::text,v.version,v.blob_only,a.id::text,b.id::text,b.size_bytes FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND (v.blob_only OR EXISTS(SELECT 1 FROM version_children vc WHERE vc.component_id=v.component_id AND vc.child_version=v.version))`, component)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, version, asset, blob string
		var only bool
		var size int64
		if err = rows.Scan(&id, &version, &only, &asset, &blob, &size); err != nil {
			rows.Close()
			return nil, err
		}
		g.holds[id] = append(g.holds[id], assetHold{asset, blob, size})
		if only {
			g.blobRow[version] = id
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT v.id::text,b.digest,b.dedup_domain_id::text,b.size_bytes FROM versions v JOIN assets a ON a.version_id=v.id AND a.asset_key='manifest' JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND NOT v.blob_only`, component)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		manifests = append(manifests, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range manifests {
		if m.size > manifestReadLimit {
			g.ok = false
			continue
		}
		refs, readable := s.manifestRefs(ctx, m.domain, m.digest)
		if !readable {
			g.ok = false
			continue
		}
		g.digestsOf[m.id] = refs
		for _, r := range refs {
			g.namers[r] = append(g.namers[r], m.id)
		}
	}
	return g, nil
}

type projector struct {
	s       *Service
	graphs  map[string]*imageGraph
	picked  map[string]map[string]bool // component -> picked version ids (parents and the children that follow)
	counted map[string]bool            // asset ids already counted
	removed map[string]int             // blob id -> assets removed
	total   map[string]int             // blob id -> assets and route-cache entries referencing it
}

func newProjector(s *Service) *projector {
	return &projector{s: s, graphs: map[string]*imageGraph{}, picked: map[string]map[string]bool{}, counted: map[string]bool{}, removed: map[string]int{}, total: map[string]int{}}
}

// add projects the extra bytes freed by picking top-level version c, beyond the
// assets of c itself.
func (p *projector) add(ctx context.Context, c evictCandidate) (int64, error) {
	g := p.graphs[c.ComponentID]
	if g == nil {
		var err error
		if g, err = p.s.loadImageGraph(ctx, c.ComponentID); err != nil {
			return 0, err
		}
		p.graphs[c.ComponentID] = g
	}
	if p.picked[c.ComponentID] == nil {
		p.picked[c.ComponentID] = map[string]bool{}
	}
	picked := p.picked[c.ComponentID]
	picked[c.VersionID] = true
	manifests := []string{c.VersionID}
	var freed int64
	take := func(version string) error {
		for _, h := range g.holds[version] {
			if p.counted[h.asset] {
				continue
			}
			p.counted[h.asset] = true
			if _, ok := p.total[h.blob]; !ok {
				var n int
				if err := p.s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM assets WHERE blob_id=$1)+(SELECT count(*) FROM federation_route_cache WHERE blob_id=$1)`, h.blob).Scan(&n); err != nil {
					return err
				}
				p.total[h.blob] = n
			}
			p.removed[h.blob]++
			if p.removed[h.blob] == p.total[h.blob] {
				freed += h.size
			}
		}
		return nil
	}
	for _, kid := range g.kids[c.VersionID] {
		all := true
		for _, parent := range g.parents[kid] {
			if !picked[parent] {
				all = false
			}
		}
		if !all {
			continue
		}
		picked[kid] = true
		manifests = append(manifests, kid)
		if err := take(kid); err != nil {
			return 0, err
		}
	}
	if g.ok {
		for _, m := range manifests {
			for _, digest := range g.digestsOf[m] {
				row, isBlob := g.blobRow[digest]
				if !isBlob {
					continue
				}
				all := true
				for _, namer := range g.namers[digest] {
					if !picked[namer] {
						all = false
					}
				}
				if all {
					if err := take(row); err != nil {
						return 0, err
					}
				}
			}
		}
	}
	return freed, nil
}
