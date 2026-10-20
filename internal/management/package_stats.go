package management

import (
	"context"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/versionorder"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PackageStats are the discovery summary fields shared by the per-feed and the
// cross-feed package listings.
type PackageStats struct {
	// DisplayName is the name a person recognises. It differs from Coordinate
	// only for Docker Hub official images, stored as library/<name>.
	DisplayName string `json:"display_name"`
	// LatestVersion is format aware; null when the package has no version or tag
	// to name (for example a pull-through image never resolved by tag).
	LatestVersion *string    `json:"latest_version"`
	VersionCount  int64      `json:"version_count"`
	TagCount      int64      `json:"tag_count"`
	UpdatedAt     *time.Time `json:"updated_at"`
}

// statsWindow bounds how many of a package's newest versions and tags are read
// to choose the latest one, so a package with a huge history costs a bounded
// amount per listing. Counts are exact; the latest is exact within the window.
const statsWindow = 128

// packageStatsSQL computes everything for one page of components in a single
// statement (index lookups on versions(component_id,...), tags and
// version_children), never one query per package. Index-grouped children are
// excluded from the version list and count.
const packageStatsSQL = `SELECT c.id::text,s.format,c.coordinate,
 (SELECT count(*) FROM versions v WHERE NOT v.blob_only AND v.component_id=c.id AND NOT EXISTS (SELECT 1 FROM version_children vc WHERE vc.component_id=c.id AND vc.child_version=v.version)),
 (SELECT count(*) FROM tags t WHERE t.component_id=c.id),
 (SELECT max(v.created_at) FROM versions v WHERE NOT v.blob_only AND v.component_id=c.id),
 COALESCE((SELECT array_agg(x.version ORDER BY x.created_at DESC,x.version) FROM (SELECT v.version,v.created_at FROM versions v WHERE NOT v.blob_only AND v.component_id=c.id AND NOT EXISTS (SELECT 1 FROM version_children vc WHERE vc.component_id=c.id AND vc.child_version=v.version) ORDER BY v.created_at DESC,v.version LIMIT $2) x),'{}'::text[]),
 COALESCE((SELECT array_agg(x.name ORDER BY (x.name='latest') DESC,x.created_at DESC,x.name) FROM (SELECT t.name,v.created_at FROM tags t JOIN versions v ON v.id=t.version_id WHERE t.component_id=c.id ORDER BY (t.name='latest') DESC,v.created_at DESC,t.name LIMIT $2) x),'{}'::text[]),
 COALESCE(rc.config->>'origin','')
 FROM components c JOIN sources s ON s.id=c.source_id AND s.instance_id=c.instance_id
 LEFT JOIN remote_source_configs rc ON rc.instance_id=c.instance_id AND rc.source_id=c.source_id
 WHERE c.instance_id=$3 AND c.id=ANY($1::uuid[])`

// loadPackageStats returns the summary for each component ID.
func loadPackageStats(ctx context.Context, db *pgxpool.Pool, instance string, ids []string) (map[string]PackageStats, error) {
	out := make(map[string]PackageStats, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, packageStatsSQL, ids, statsWindow, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, format, coordinate, origin string
		var s PackageStats
		var versions, tags []string
		if err = rows.Scan(&id, &format, &coordinate, &s.VersionCount, &s.TagCount, &s.UpdatedAt, &versions, &tags, &origin); err != nil {
			return nil, err
		}
		s.DisplayName = coordinate
		if remoteconfig.IsDockerHub(origin) {
			s.DisplayName = remoteconfig.DockerHubDisplayName(coordinate)
		}
		s.LatestVersion = latestVersion(format, versions, tags)
		out[id] = s
	}
	return out, rows.Err()
}

func isOCIFormat(format string) bool { return format == "oci" || format == "oci-artifacts" }

// latestVersion chooses the version to show as "latest". versions and tags are
// newest first (tags: "latest" first).
//   - OCI: the latest tag, else the highest release-looking tag, else the newest
//     tag that is not a CI commit tag, else the newest tag, else null. Digests
//     are never named: a pull-through image whose tag was never resolved shows
//     no latest.
//   - Other formats: the highest semver-like version (releases before
//     prereleases), else the newest.
func latestVersion(format string, versions, tags []string) *string {
	if isOCIFormat(format) {
		if len(tags) == 0 {
			return nil
		}
		if tags[0] == "latest" {
			return &tags[0]
		}
		var semver []string
		for _, t := range tags {
			if _, ok := versionorder.Parse(t); ok {
				semver = append(semver, t)
			}
		}
		if best, ok := versionorder.Latest(semver); ok {
			return &best
		}
		for i := range tags {
			if !strings.HasPrefix(tags[i], "sha-") {
				return &tags[i]
			}
		}
		return &tags[0]
	}
	if best, ok := versionorder.Latest(versions); ok {
		return &best
	}
	return nil
}
