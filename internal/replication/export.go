package replication

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exporter is the authoritative side. Callers must authorize replicate access
// to the feed before invoking any method; every query is scoped to the instance
// and to sources attached to that feed. Replica-kind sources are never exported:
// an instance only exports content it is the authority for.
type Exporter struct {
	DB      *pgxpool.Pool
	Storage func(context.Context, string) (blob.Store, error)
	// Lease fences history purging by the instance writer lease. Optional for
	// the read-only export methods; PurgeHistory requires it.
	Lease func() ownership.Lease
	// Consumers, when set, records which consumers pull from this instance
	// (outgoing replication visibility). Optional.
	Consumers *ConsumerTracker
}

const (
	maxPage        = 1000
	defaultPage    = 200
	manifestAssets = "asset"
	manifestTags   = "tag"
)

func kinds(includeCached bool) []string {
	if includeCached {
		return []string{"local", "remote"}
	}
	return []string{"local"}
}
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
func clampLimit(n int) int {
	if n <= 0 {
		return defaultPage
	}
	if n > maxPage {
		return maxPage
	}
	return n
}

// Head reports the feed format and the instance-wide change high-water mark.
func (e Exporter) Head(ctx context.Context, instance, feed string) (Head, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.notCache(ctx, instance, feed); err != nil {
		return Head{}, err
	}
	var h Head
	err := e.DB.QueryRow(ctx, `SELECT f.format,COALESCE(h.seq,0),h.changed_at,clock_timestamp(),COALESCE(h.low_water,0) FROM feeds f LEFT JOIN replication_heads h ON h.instance_id=f.instance_id WHERE f.instance_id=$1 AND f.id=$2`, instance, feed).Scan(&h.Format, &h.Seq, &h.ChangedAt, &h.Now, &h.LowWater)
	if errors.Is(err, pgx.ErrNoRows) {
		return Head{}, ErrNotFound
	}
	if err != nil {
		return Head{}, ErrUnavailable
	}
	return h, nil
}

// notCache refuses to export a cache feed: replicating a build cache makes no
// sense, and its changes are not recorded in the change sequence either.
func (e Exporter) notCache(ctx context.Context, instance, feed string) error {
	var purpose string
	err := e.DB.QueryRow(ctx, `SELECT purpose FROM feeds WHERE instance_id=$1 AND id=$2`, instance, feed).Scan(&purpose)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return ErrUnavailable
	case purpose == "cache":
		return ErrCacheFeed
	}
	return nil
}

const itemSelect = `SELECT s.id::text,s.name,s.kind,fs.priority,c.namespace,c.coordinate,c.metadata,v.version,a.asset_key,a.media_type,b.digest,b.size_bytes,
 sub.subject_digest,sub.subject_size,sub.subject_media_type,sub.artifact_type,sub.annotations
 FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id
 JOIN components c ON c.instance_id=s.instance_id AND c.source_id=s.id
 JOIN versions v ON v.component_id=c.id JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id
 LEFT JOIN artifact_subjects sub ON sub.asset_id=a.id`

func scanItem(row pgx.Row) (Item, error) {
	var it Item
	var sd, smt, sat *string
	var ss *int64
	var ann []byte
	var meta []byte
	err := row.Scan(&it.Origin.SourceID, &it.Origin.Name, &it.Origin.Kind, &it.Origin.Priority, &it.Namespace, &it.Coordinate, &meta, &it.Version, &it.AssetKey, &it.MediaType, &it.Digest, &it.Size, &sd, &ss, &smt, &sat, &ann)
	if err != nil {
		return Item{}, err
	}
	it.ComponentMetadata = meta
	if sd != nil {
		sub := &Subject{Digest: *sd}
		if ss != nil {
			sub.Size = *ss
		}
		if smt != nil {
			sub.MediaType = *smt
		}
		if sat != nil {
			sub.ArtifactType = *sat
		}
		if len(ann) > 0 {
			_ = json.Unmarshal(ann, &sub.Annotations)
		}
		it.Subject = sub
	}
	return it, nil
}

const tagSelect = `SELECT s.id::text,s.name,s.kind,fs.priority,c.namespace,c.coordinate,t.name,v.version
 FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id
 JOIN components c ON c.instance_id=s.instance_id AND c.source_id=s.id
 JOIN tags t ON t.component_id=c.id JOIN versions v ON v.id=t.version_id`

func scanTag(row pgx.Row) (TagItem, error) {
	var t TagItem
	err := row.Scan(&t.Origin.SourceID, &t.Origin.Name, &t.Origin.Kind, &t.Origin.Priority, &t.Namespace, &t.Coordinate, &t.Tag, &t.Version)
	return t, err
}

// Shared filter predicate: $1 instance, $2 feed, $3 kinds, $4 namespaces,
// $5 packages, $6 version prefixes, $7 restrict to one source (empty: all).
const scope = ` WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind=ANY($3::text[])
 AND (cardinality($4::text[])=0 OR COALESCE(c.namespace,'')=ANY($4::text[]))
 AND (cardinality($5::text[])=0 OR c.coordinate=ANY($5::text[]))
 AND (cardinality($6::text[])=0 OR EXISTS(SELECT 1 FROM unnest($6::text[]) p WHERE starts_with(v.version,p)))
 AND ($7::text='' OR s.id::text=$7::text)`

func encodeCursor(parts ...string) string {
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(s string, n int) ([]string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrInput
	}
	var parts []string
	if json.Unmarshal(b, &parts) != nil || len(parts) != n {
		return nil, ErrInput
	}
	return parts, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// ChangeQuery selects a page of changes after a cursor.
type ChangeQuery struct {
	After         int64
	Limit         int
	IncludeCached bool
	Filters       Filters
}

// ManifestQuery selects a manifest page.
type ManifestQuery struct {
	Kind          string // "asset" or "tag"
	After         string
	Limit         int
	IncludeCached bool
	Filters       Filters
	// SourceID restricts the manifest to one attached source (back-fill).
	SourceID string
}

// Manifest lists the feed's current assets or tags in a stable key order. The
// manifest is read in pages without a global snapshot: consumers seed from it
// and then replay the change feed from the head observed before seeding, and
// every change application is idempotent, so concurrent writes converge.
func (e Exporter) Manifest(ctx context.Context, instance, feed string, q ManifestQuery) (ManifestPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := e.notCache(ctx, instance, feed); err != nil {
		return ManifestPage{}, err
	}
	if (q.Kind != manifestAssets && q.Kind != manifestTags) || !q.Filters.valid() || (q.SourceID != "" && !uuidPattern.MatchString(q.SourceID)) {
		return ManifestPage{}, ErrInput
	}
	limit := clampLimit(q.Limit)
	args := []any{instance, feed, kinds(q.IncludeCached), nonNil(q.Filters.Namespaces), nonNil(q.Filters.Packages), nonNil(q.Filters.VersionPrefixes), q.SourceID}
	page := ManifestPage{Kind: q.Kind}
	cursorArgs := []any{false, zeroUUID, "", "", ""}
	if q.After != "" {
		n := 4
		if q.Kind == manifestTags {
			n = 3
		}
		parts, err := decodeCursor(q.After, n)
		if err != nil {
			return ManifestPage{}, err
		}
		for len(parts) < 4 {
			parts = append(parts, "")
		}
		if !uuidPattern.MatchString(parts[0]) {
			return ManifestPage{}, ErrInput
		}
		cursorArgs = []any{true, parts[0], parts[1], parts[2], parts[3]}
	} else {
		var total int64
		var err error
		if q.Kind == manifestAssets {
			err = e.DB.QueryRow(ctx, `SELECT count(*) FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id JOIN components c ON c.instance_id=s.instance_id AND c.source_id=s.id JOIN versions v ON v.component_id=c.id JOIN assets a ON a.version_id=v.id`+scope, args...).Scan(&total)
		} else {
			err = e.DB.QueryRow(ctx, `SELECT count(*) FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id JOIN components c ON c.instance_id=s.instance_id AND c.source_id=s.id JOIN tags t ON t.component_id=c.id JOIN versions v ON v.id=t.version_id`+scope, args...).Scan(&total)
		}
		if err != nil {
			return ManifestPage{}, ErrUnavailable
		}
		page.Total = &total
	}
	args = append(args, limit+1)
	limitArg := len(args)
	args = append(args, cursorArgs...)
	c := limitArg + 1
	if q.Kind == manifestAssets {
		sql := itemSelect + scope + ` AND (NOT $` + itoa(c) + `::boolean OR (s.id,c.coordinate,v.version,a.asset_key)>($` + itoa(c+1) + `::uuid,$` + itoa(c+2) + `,$` + itoa(c+3) + `,$` + itoa(c+4) + `))
 ORDER BY s.id,c.coordinate,v.version,a.asset_key LIMIT $` + itoa(limitArg)
		rows, err := e.DB.Query(ctx, sql, args...)
		if err != nil {
			return ManifestPage{}, ErrUnavailable
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return ManifestPage{}, ErrUnavailable
			}
			page.Items = append(page.Items, it)
		}
		if rows.Err() != nil {
			return ManifestPage{}, ErrUnavailable
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[limit-1]
			page.Next = encodeCursor(last.Origin.SourceID, last.Coordinate, last.Version, last.AssetKey)
		}
		return page, nil
	}
	sql := tagSelect + scope + ` AND (NOT $` + itoa(c) + `::boolean OR (s.id,c.coordinate,t.name)>($` + itoa(c+1) + `::uuid,$` + itoa(c+2) + `,$` + itoa(c+3) + `))
 ORDER BY s.id,c.coordinate,t.name LIMIT $` + itoa(limitArg)
	// The tag cursor has three parts; the unused fourth argument is dropped.
	args = args[:len(args)-1]
	rows, err := e.DB.Query(ctx, sql, args...)
	if err != nil {
		return ManifestPage{}, ErrUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return ManifestPage{}, ErrUnavailable
		}
		page.Tags = append(page.Tags, t)
	}
	if rows.Err() != nil {
		return ManifestPage{}, ErrUnavailable
	}
	if len(page.Tags) > limit {
		page.Tags = page.Tags[:limit]
		last := page.Tags[limit-1]
		page.Next = encodeCursor(last.Origin.SourceID, last.Coordinate, last.Tag)
	}
	return page, nil
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// Changes returns relevant changes with after < seq, in sequence order. Rows and
// the head are read from one snapshot, so a cursor never skips a change.
//
// Filters are applied here, not only by the consumer: package and version
// prefix match the change identity, and the namespace filter matches the
// package's current namespace. A tombstone whose package is gone cannot be
// resolved to a namespace and is passed through (consumers only ever delete
// what they replicated). Attachment changes are never filtered. A cursor below
// the purge low-water mark fails with *HistoryPurgedError.
func (e Exporter) Changes(ctx context.Context, instance, feed string, q ChangeQuery) (ChangePage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := e.notCache(ctx, instance, feed); err != nil {
		return ChangePage{}, err
	}
	after, includeCached := q.After, q.IncludeCached
	if after < 0 || !q.Filters.valid() {
		return ChangePage{}, ErrInput
	}
	limit := clampLimit(q.Limit)
	tx, err := e.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ChangePage{}, ErrUnavailable
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	page := ChangePage{Changes: []Change{}}
	err = tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0),max(changed_at),COALESCE(max(low_water),0) FROM replication_heads WHERE instance_id=$1`, instance).Scan(&page.Head, &page.HeadAt, &page.LowWater)
	if err != nil {
		return ChangePage{}, ErrUnavailable
	}
	if after < page.LowWater {
		return ChangePage{}, &HistoryPurgedError{LowWater: page.LowWater}
	}
	rows, err := tx.Query(ctx, `SELECT ch.seq,ch.created_at,ch.kind,ch.op,ch.source_id::text,ch.coordinate,ch.version,ch.item_key
 FROM replication_changes ch LEFT JOIN sources s ON s.instance_id=ch.instance_id AND s.id=ch.source_id
 WHERE ch.instance_id=$1 AND ch.seq>$3 AND ch.seq<=$4 AND (
  (ch.kind='attachment' AND ch.item_key=($2::uuid)::text AND (s.id IS NULL OR s.kind=ANY($5::text[])))
  OR (ch.kind<>'attachment' AND s.kind=ANY($5::text[])
   AND EXISTS(SELECT 1 FROM feed_sources fs WHERE fs.instance_id=ch.instance_id AND fs.feed_id=$2::uuid AND fs.source_id=ch.source_id)
   AND (cardinality($7::text[])=0 OR ch.coordinate=ANY($7::text[]))
   AND (cardinality($8::text[])=0 OR ch.version='' OR EXISTS(SELECT 1 FROM unnest($8::text[]) p WHERE starts_with(ch.version,p)))
   AND (cardinality($9::text[])=0
    OR NOT EXISTS(SELECT 1 FROM components c WHERE c.instance_id=ch.instance_id AND c.source_id=ch.source_id AND c.coordinate=ch.coordinate)
    OR EXISTS(SELECT 1 FROM components c WHERE c.instance_id=ch.instance_id AND c.source_id=ch.source_id AND c.coordinate=ch.coordinate AND COALESCE(c.namespace,'')=ANY($9::text[]))))
 ) ORDER BY ch.seq LIMIT $6`, instance, feed, after, page.Head, kinds(includeCached), limit, nonNil(q.Filters.Packages), nonNil(q.Filters.VersionPrefixes), nonNil(q.Filters.Namespaces))
	if err != nil {
		return ChangePage{}, ErrUnavailable
	}
	for rows.Next() {
		var c Change
		if err = rows.Scan(&c.Seq, &c.At, &c.Kind, &c.Op, &c.SourceID, &c.Coordinate, &c.Version, &c.Key); err != nil {
			rows.Close()
			return ChangePage{}, ErrUnavailable
		}
		page.Changes = append(page.Changes, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return ChangePage{}, ErrUnavailable
	}
	for i := range page.Changes {
		c := &page.Changes[i]
		if c.Op != "put" {
			continue
		}
		switch c.Kind {
		case "attachment":
			var o Origin
			err := tx.QueryRow(ctx, `SELECT s.id::text,s.name,s.kind,fs.priority FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3`, instance, feed, c.SourceID).Scan(&o.SourceID, &o.Name, &o.Kind, &o.Priority)
			if err == nil {
				c.Attachment = &Attachment{Origin: o}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return ChangePage{}, ErrUnavailable
			}
		case "asset":
			it, err := scanItem(tx.QueryRow(ctx, itemSelect+` WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3 AND c.coordinate=$4 AND v.version=$5 AND a.asset_key=$6`, instance, feed, c.SourceID, c.Coordinate, c.Version, c.Key))
			if err == nil {
				c.Asset = &it
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return ChangePage{}, ErrUnavailable
			}
		case "tag":
			t, err := scanTag(tx.QueryRow(ctx, tagSelect+` WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3 AND c.coordinate=$4 AND t.name=$5`, instance, feed, c.SourceID, c.Coordinate, c.Key))
			if err == nil {
				c.Tag = &t
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return ChangePage{}, ErrUnavailable
			}
		case "component":
			var ci ComponentItem
			var meta []byte
			err := tx.QueryRow(ctx, `SELECT s.id::text,s.name,s.kind,fs.priority,c.namespace,c.coordinate,c.metadata FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id JOIN components c ON c.instance_id=s.instance_id AND c.source_id=s.id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3 AND c.coordinate=$4`, instance, feed, c.SourceID, c.Coordinate).Scan(&ci.Origin.SourceID, &ci.Origin.Name, &ci.Origin.Kind, &ci.Origin.Priority, &ci.Namespace, &ci.Coordinate, &meta)
			if err == nil {
				ci.Metadata = meta
				c.Component = &ci
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return ChangePage{}, ErrUnavailable
			}
		}
	}
	if len(page.Changes) == limit {
		page.More = true
		page.Next = page.Changes[len(page.Changes)-1].Seq
	} else {
		page.Next = page.Head
		if page.Next < after {
			page.Next = after
		}
	}
	return page, nil
}

// OpenBlob returns a verified-size range reader for a blob referenced by an
// exportable asset of the feed. Blobs referenced only by other feeds, replica
// sources or no asset at all are indistinguishable from missing.
func (e Exporter) OpenBlob(ctx context.Context, instance, feed, digest string, includeCached bool, offset, length int64) (blob.Info, io.ReadCloser, error) {
	d := blob.Digest(digest)
	if !d.Valid() || offset < 0 {
		return blob.Info{}, nil, ErrInput
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.notCache(qctx, instance, feed); err != nil {
		return blob.Info{}, nil, err
	}
	var domain string
	var size int64
	err := e.DB.QueryRow(qctx, `SELECT b.dedup_domain_id::text,b.size_bytes FROM blobs b JOIN assets a ON a.blob_id=b.id JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id
 JOIN feed_sources fs ON fs.instance_id=c.instance_id AND fs.source_id=c.source_id AND fs.feed_id=$2 JOIN sources s ON s.id=c.source_id
 WHERE b.instance_id=$1 AND b.digest=$3 AND s.kind=ANY($4::text[]) LIMIT 1`, instance, feed, digest, kinds(includeCached)).Scan(&domain, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return blob.Info{}, nil, ErrNotFound
	}
	if err != nil {
		return blob.Info{}, nil, ErrUnavailable
	}
	if e.Storage == nil {
		return blob.Info{}, nil, ErrUnavailable
	}
	store, err := e.Storage(ctx, domain)
	if err != nil || store == nil {
		return blob.Info{}, nil, ErrUnavailable
	}
	info, err := store.Stat(ctx, d)
	if errors.Is(err, blob.ErrNotFound) {
		return blob.Info{}, nil, ErrNotFound
	}
	if err != nil {
		return blob.Info{}, nil, ErrUnavailable
	}
	if info.Size != size {
		return blob.Info{}, nil, blob.ErrIntegrity
	}
	r, err := store.OpenRange(ctx, d, offset, length)
	if err != nil {
		if errors.Is(err, blob.ErrRange) {
			return info, nil, ErrInput
		}
		return blob.Info{}, nil, ErrUnavailable
	}
	return info, r, nil
}
