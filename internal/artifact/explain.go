package artifact

import (
	"context"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

// AttachedSource describes one source attached to a feed for a coordinate, in
// read order. HasComponent is true when the source contains the coordinate.
type AttachedSource struct {
	SourceID, Name, Kind string
	Priority             int32
	HasComponent         bool
	Namespace            string
	Versions             int64
	LastCachedAt         *time.Time
	LastAccessedAt       *time.Time
	// Remote cache policy; zero unless the source is remote and configured.
	MetadataTTLSeconds, StaleIfErrorSeconds int
}

// ExplainRead lists attached sources in read order with the coordinate's
// presence. Package-owner composition (Lookup, ResolveTag, plan) makes the first
// source that contains the coordinate the owner of every version; the rest are
// shadowed. The ordering clause is intentionally identical to those queries.
func (p Policies) ExplainRead(ctx context.Context, feed, coordinate string) ([]AttachedSource, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := p.DB.Query(ctx, `SELECT fs.source_id::text,s.name,s.kind,fs.priority,c.id IS NOT NULL,COALESCE(c.namespace,''),
 COALESCE((SELECT count(*) FROM versions v WHERE NOT v.blob_only AND v.component_id=c.id),0),
 (SELECT max(a.created_at) FROM versions v JOIN assets a ON a.version_id=v.id WHERE v.component_id=c.id),
 (SELECT max(a.last_accessed_at) FROM versions v JOIN assets a ON a.version_id=v.id WHERE v.component_id=c.id),
 COALESCE((rc.config->'cache'->>'metadata_ttl_seconds')::int,0),COALESCE((rc.config->'cache'->>'stale_if_error_seconds')::int,0)
 FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id
 LEFT JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id AND c.coordinate=$3
 LEFT JOIN remote_source_configs rc ON rc.source_id=fs.source_id AND rc.instance_id=fs.instance_id
 WHERE fs.instance_id=$1 AND fs.feed_id=$2 ORDER BY fs.priority LIMIT $4`, p.InstanceID, feed, coordinate, routing.MaxSources+1)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	var result []AttachedSource
	for rows.Next() {
		var s AttachedSource
		if rows.Scan(&s.SourceID, &s.Name, &s.Kind, &s.Priority, &s.HasComponent, &s.Namespace, &s.Versions, &s.LastCachedAt, &s.LastAccessedAt, &s.MetadataTTLSeconds, &s.StaleIfErrorSeconds) != nil {
			return nil, ErrUnavailable
		}
		result = append(result, s)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}

// Compile builds a router for an unsaved candidate policy against the feed's
// currently attached, same-format sources, without persisting anything.
func (p Policies) Compile(ctx context.Context, feed, defaultSource string, rules []routing.Rule) (*routing.Router, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sources, err := loadSources(ctx, p.DB, p.InstanceID, feed)
	if err != nil {
		return nil, err
	}
	eff, _, err := effectivePublish(ctx, p.DB, p.InstanceID, feed, defaultSource, rules)
	if err != nil {
		return nil, err
	}
	return routing.Compile(eff.Config(sources))
}
