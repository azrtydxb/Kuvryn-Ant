package oci

import (
	"context"
	"encoding/json"
	"io"
	"sort"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BlobRepairOptions selects what RepairBlobVersions looks at.
type BlobRepairOptions struct {
	// DryRun classifies and counts without writing.
	DryRun bool
	// Feed, when set, restricts the repair to components of sources attached to
	// the feed with this slug. A source attached to several feeds is repaired for
	// all of them (the rows belong to the source, not to the feed).
	Feed string
}

// BlobRepairComponent is one repository's converted blob versions.
type BlobRepairComponent struct {
	Coordinate string `json:"coordinate"`
	Converted  int64  `json:"converted"`
}

// BlobRepairFeed groups the converted components of one feed.
type BlobRepairFeed struct {
	Feed       string                `json:"feed"`
	Converted  int64                 `json:"converted"`
	Components []BlobRepairComponent `json:"components"`
}

// BlobRepairReport is the outcome of RepairBlobVersions. Counts are exact in a
// dry run: they are what a real run would convert.
type BlobRepairReport struct {
	DryRun bool `json:"dry_run"`
	// Examined is the number of digest-named versions without a manifest asset.
	Examined int64 `json:"examined"`
	// Converted is the number of blob versions marked blob-only (no longer
	// counted or listed as versions). Their assets and bytes are untouched.
	Converted int64 `json:"converted"`
	// ManifestsKept were stored through the blob path but parse as manifests or
	// indexes; they stay visible versions.
	ManifestsKept int64 `json:"manifests_kept"`
	// Unreadable could not be classified (bytes unavailable); a later run retries.
	Unreadable int64            `json:"unreadable"`
	Feeds      []BlobRepairFeed `json:"feeds"`
}

type blobCandidate struct {
	id, component, coordinate, media, digest, domain, instance string
	size                                                       int64
	feeds                                                      []string
}

const blobRepairBatch = 500

// RepairBlobVersions marks as blob-only the OCI versions that are really config
// or layer blobs (pull-through and pushes stored every blob as a digest-named
// version). It changes no asset, blob row or byte, so access timestamps, download
// counts, references and garbage collection are exactly as before; the versions
// simply stop counting as, and being listed as, package versions. A digest-named
// version is kept when its stored media type is a manifest type or its bytes
// parse as a manifest or index. It is idempotent and never contacts an upstream.
func RepairBlobVersions(ctx context.Context, db *pgxpool.Pool, resolve func(ctx context.Context, domain string) (blob.Store, error), opts BlobRepairOptions) (BlobRepairReport, error) {
	report := BlobRepairReport{DryRun: opts.DryRun, Feeds: []BlobRepairFeed{}}
	byFeed := map[string]map[string]int64{}
	perInstance := map[string]int64{}
	after := ""
	for {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		batch, err := blobCandidates(ctx, db, opts.Feed, after)
		if err != nil {
			return report, err
		}
		if len(batch) == 0 {
			break
		}
		after = batch[len(batch)-1].id
		var convert []string
		for _, c := range batch {
			report.Examined++
			isManifest, readable := classifyBlob(ctx, resolve, c)
			switch {
			case !readable:
				report.Unreadable++
			case isManifest:
				report.ManifestsKept++
			default:
				convert = append(convert, c.id)
				perInstance[c.instance]++
				for _, f := range c.feeds {
					if byFeed[f] == nil {
						byFeed[f] = map[string]int64{}
					}
					byFeed[f][c.coordinate]++
				}
			}
		}
		if len(convert) == 0 {
			continue
		}
		if opts.DryRun {
			report.Converted += int64(len(convert))
			continue
		}
		tag, err := db.Exec(ctx, `UPDATE versions v SET blob_only=true WHERE v.id=ANY($1::uuid[]) AND NOT v.blob_only
 AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.version_id=v.id AND a.asset_key<>'blob')`, convert)
		if err != nil {
			return report, err
		}
		report.Converted += tag.RowsAffected()
	}
	for feed, comps := range byFeed {
		entry := BlobRepairFeed{Feed: feed}
		for coordinate, n := range comps {
			entry.Components = append(entry.Components, BlobRepairComponent{Coordinate: coordinate, Converted: n})
			entry.Converted += n
		}
		sort.Slice(entry.Components, func(i, j int) bool { return entry.Components[i].Coordinate < entry.Components[j].Coordinate })
		report.Feeds = append(report.Feeds, entry)
	}
	sort.Slice(report.Feeds, func(i, j int) bool { return report.Feeds[i].Feed < report.Feeds[j].Feed })
	if opts.DryRun || report.Converted == 0 {
		return report, nil
	}
	// One summary audit event per instance, carrying the totals.
	for instance, n := range perInstance {
		details, _ := json.Marshal(map[string]any{"converted": n, "manifests_kept": report.ManifestsKept, "unreadable": report.Unreadable, "feed": opts.Feed})
		if _, err := db.Exec(ctx, `INSERT INTO audit_events(instance_id,action,details) VALUES($1,'repair.oci_blob_versions',$2)`, instance, details); err != nil {
			return report, err
		}
	}
	return report, nil
}

// blobCandidates returns the next page (by version id) of digest-named OCI
// versions that carry only blob assets and are not yet marked blob-only.
func blobCandidates(ctx context.Context, db *pgxpool.Pool, feed, after string) ([]blobCandidate, error) {
	rows, err := db.Query(ctx, `SELECT v.id::text,v.instance_id::text,c.id::text,c.coordinate,a.media_type,b.digest,b.dedup_domain_id::text,b.size_bytes,
 COALESCE(ARRAY(SELECT f.slug FROM feed_sources fs JOIN feeds f ON f.id=fs.feed_id AND f.instance_id=fs.instance_id WHERE fs.source_id=c.source_id AND ($1='' OR f.slug=$1) ORDER BY f.slug),'{}'::text[])
 FROM versions v JOIN components c ON c.id=v.component_id JOIN sources s ON s.id=c.source_id
 JOIN assets a ON a.version_id=v.id AND a.asset_key='blob' JOIN blobs b ON b.id=a.blob_id
 WHERE NOT v.blob_only AND s.format IN ('oci','oci-artifacts') AND v.version ~ '^[0-9a-f]{64}$'
 AND ($2='' OR v.id>$2::uuid)
 AND NOT EXISTS(SELECT 1 FROM assets o WHERE o.version_id=v.id AND o.asset_key<>'blob')
 AND ($1='' OR EXISTS(SELECT 1 FROM feed_sources fs JOIN feeds f ON f.id=fs.feed_id AND f.instance_id=fs.instance_id WHERE fs.source_id=c.source_id AND f.slug=$1))
 ORDER BY v.id LIMIT $3`, feed, after, blobRepairBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (blobCandidate, error) {
		var c blobCandidate
		err := row.Scan(&c.id, &c.instance, &c.component, &c.coordinate, &c.media, &c.digest, &c.domain, &c.size, &c.feeds)
		return c, err
	})
}

// classifyBlob reports whether the stored object is a manifest or index, from
// its media type or, for small objects, its bytes. readable is false when the
// bytes were needed but could not be read.
func classifyBlob(ctx context.Context, resolve func(ctx context.Context, domain string) (blob.Store, error), c blobCandidate) (isManifest, readable bool) {
	if manifestType(c.media) {
		return true, true
	}
	if c.size > manifestLimit || c.size == 0 {
		return false, true
	}
	store, err := resolve(ctx, c.domain)
	if err != nil {
		return false, false
	}
	rc, err := store.OpenRange(ctx, blob.Digest(c.digest), 0, -1)
	if err != nil {
		return false, false
	}
	body, err := io.ReadAll(io.LimitReader(rc, manifestLimit))
	_ = rc.Close()
	if err != nil {
		return false, false
	}
	var doc struct {
		SchemaVersion *int   `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Manifests     []json.RawMessage
		Layers        []json.RawMessage
		Config        json.RawMessage
	}
	if json.Unmarshal(body, &doc) != nil || doc.SchemaVersion == nil {
		return false, true
	}
	if doc.MediaType != "" {
		return manifestType(doc.MediaType), true
	}
	// Artifact manifests may omit mediaType: structure decides.
	return doc.Manifests != nil || (doc.Config != nil && doc.Layers != nil), true
}
