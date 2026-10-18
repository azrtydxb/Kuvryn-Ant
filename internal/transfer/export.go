package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExportOptions control Build.
type ExportOptions struct {
	Selection Selection
	// Content includes the inventory of local source content.
	Content bool
}

func namespaceMatch(sel []string, ns string) bool {
	if len(sel) == 0 {
		return true
	}
	for _, s := range sel {
		if ns == s || strings.HasPrefix(ns, s+"/") || strings.HasPrefix(ns, s+".") {
			return true
		}
	}
	return false
}

func retentionRow(row pgx.Row) (*RetentionDef, error) {
	var r RetentionDef
	if err := row.Scan(&r.KeepLast, &r.MaxAgeSeconds, &r.MaxIdleSeconds, &r.MinKeep, &r.IncludeTagged, &r.AllowReleaseDelete, &r.Enabled); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

const retentionCols = `keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,allow_release_delete,enabled`

// Build reads the selected definitions (and optionally content inventory) from a
// single repeatable-read snapshot. It never reads credential material.
func Build(ctx context.Context, pool *pgxpool.Pool, instance string, opt ExportOptions) (Document, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Document{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	doc := Document{FormatVersion: FormatVersion, CreatedAt: time.Now().UTC(), Selection: opt.Selection, Sources: []SourceDef{}, Feeds: []FeedDef{}, Components: []ComponentDef{}}
	feedSlugs := append([]string{}, opt.Selection.Feeds...)
	rows, err := tx.Query(ctx, `SELECT id::text,slug,format,anonymous_read FROM feeds WHERE instance_id=$1 AND (cardinality($2::text[])=0 OR slug=ANY($2)) ORDER BY slug`, instance, feedSlugs)
	if err != nil {
		return Document{}, err
	}
	type feedRow struct {
		id  string
		def FeedDef
	}
	var feeds []feedRow
	for rows.Next() {
		var f feedRow
		if err = rows.Scan(&f.id, &f.def.Slug, &f.def.Format, &f.def.AnonymousRead); err != nil {
			rows.Close()
			return Document{}, err
		}
		f.def.Sources = []FeedSourceRef{}
		feeds = append(feeds, f)
	}
	rows.Close()
	if rows.Err() != nil {
		return Document{}, rows.Err()
	}
	for _, want := range feedSlugs {
		if !slices.ContainsFunc(feeds, func(f feedRow) bool { return f.def.Slug == want }) {
			return Document{}, fmt.Errorf("%w: feed %q not found", ErrInput, want)
		}
	}
	feedIDs := make([]string, len(feeds))
	for i, f := range feeds {
		feedIDs[i] = f.id
	}
	// Source IDs to names, for every source reachable from the selected feeds.
	names := map[string]string{}
	var sourceIDs []string
	srows, err := tx.Query(ctx, `SELECT s.id::text,s.name,s.kind,s.format FROM sources s WHERE s.instance_id=$1 AND ($3 OR EXISTS(SELECT 1 FROM feed_sources fs WHERE fs.source_id=s.id AND fs.feed_id::text=ANY($2))) ORDER BY s.name`, instance, feedIDs, len(feedSlugs) == 0)
	if err != nil {
		return Document{}, err
	}
	kinds := map[string]string{}
	for srows.Next() {
		var id string
		var s SourceDef
		if err = srows.Scan(&id, &s.Name, &s.Kind, &s.Format); err != nil {
			srows.Close()
			return Document{}, err
		}
		names[id], kinds[id] = s.Name, s.Kind
		sourceIDs = append(sourceIDs, id)
		doc.Sources = append(doc.Sources, s)
	}
	srows.Close()
	if srows.Err() != nil {
		return Document{}, srows.Err()
	}
	for i := range doc.Sources {
		id := sourceIDs[i]
		s := &doc.Sources[i]
		if s.Kind == "remote" {
			var cfg json.RawMessage
			var cred *string
			err = tx.QueryRow(ctx, `SELECT config,credential_id::text FROM remote_source_configs WHERE instance_id=$1 AND source_id=$2`, instance, id).Scan(&cfg, &cred)
			if err == nil {
				var m map[string]json.RawMessage
				if json.Unmarshal(cfg, &m) != nil {
					return Document{}, fmt.Errorf("remote config of source %q is not an object", s.Name)
				}
				delete(m, "credential_id")
				stripped, _ := json.Marshal(m)
				raw := json.RawMessage(stripped)
				s.RemoteConfig = &raw
				if cred != nil {
					s.Credential = &CredentialRef{Purpose: "remote-source:" + id}
				}
			} else if err != pgx.ErrNoRows {
				return Document{}, err
			}
		}
		if s.Retention, err = retentionRow(tx.QueryRow(ctx, `SELECT `+retentionCols+` FROM retention_policies WHERE instance_id=$1 AND source_id=$2`, instance, id)); err != nil {
			return Document{}, err
		}
	}
	for _, f := range feeds {
		def := f.def
		frows, err := tx.Query(ctx, `SELECT source_id::text,priority FROM feed_sources WHERE instance_id=$1 AND feed_id=$2 ORDER BY priority`, instance, f.id)
		if err != nil {
			return Document{}, err
		}
		for frows.Next() {
			var id string
			var ref FeedSourceRef
			if err = frows.Scan(&id, &ref.Priority); err != nil {
				frows.Close()
				return Document{}, err
			}
			ref.Source = names[id]
			def.Sources = append(def.Sources, ref)
		}
		frows.Close()
		if frows.Err() != nil {
			return Document{}, frows.Err()
		}
		var dflt *string
		var encoded []byte
		err = tx.QueryRow(ctx, `SELECT default_source_id::text,rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2`, instance, f.id).Scan(&dflt, &encoded)
		switch {
		case err == nil:
			pp := &PublishPolicyDef{Rules: []routing.Rule{}}
			if dflt != nil {
				pp.DefaultSource = names[*dflt]
			}
			if err = json.Unmarshal(encoded, &pp.Rules); err != nil {
				return Document{}, err
			}
			for i := range pp.Rules {
				if pp.Rules[i].Target != "" {
					pp.Rules[i].Target = names[pp.Rules[i].Target]
				}
			}
			def.PublishPolicy = pp
		case err != pgx.ErrNoRows:
			return Document{}, err
		}
		if def.Retention, err = retentionRow(tx.QueryRow(ctx, `SELECT `+retentionCols+` FROM retention_policies WHERE instance_id=$1 AND feed_id=$2`, instance, f.id)); err != nil {
			return Document{}, err
		}
		doc.Feeds = append(doc.Feeds, def)
	}
	if opt.Content {
		for _, id := range sourceIDs {
			if kinds[id] != "local" {
				doc.SkippedContent = append(doc.SkippedContent, names[id])
				continue
			}
			comps, err := exportComponents(ctx, tx, instance, id, names[id], opt.Selection.Namespaces)
			if err != nil {
				return Document{}, err
			}
			doc.Components = append(doc.Components, comps...)
		}
	}
	return doc, tx.Commit(ctx)
}

func exportComponents(ctx context.Context, tx pgx.Tx, instance, source, name string, namespaces []string) ([]ComponentDef, error) {
	rows, err := tx.Query(ctx, `SELECT c.id::text,COALESCE(c.namespace,''),c.coordinate,c.metadata FROM components c WHERE c.instance_id=$1 AND c.source_id=$2 ORDER BY c.namespace,c.coordinate`, instance, source)
	if err != nil {
		return nil, err
	}
	type comp struct {
		id  string
		def ComponentDef
	}
	var comps []comp
	for rows.Next() {
		c := comp{def: ComponentDef{Source: name, Versions: []VersionDef{}}}
		if err = rows.Scan(&c.id, &c.def.Namespace, &c.def.Coordinate, &c.def.Metadata); err != nil {
			rows.Close()
			return nil, err
		}
		if namespaceMatch(namespaces, c.def.Namespace) {
			comps = append(comps, c)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	out := make([]ComponentDef, 0, len(comps))
	for _, c := range comps {
		arows, err := tx.Query(ctx, `SELECT v.version,a.asset_key,a.media_type,b.digest,b.size_bytes,a.provenance,s.subject_digest,s.subject_size,s.subject_media_type,s.artifact_type,s.annotations
 FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id LEFT JOIN artifact_subjects s ON s.asset_id=a.id
 WHERE v.component_id=$1 ORDER BY v.version,a.asset_key`, c.id)
		if err != nil {
			return nil, err
		}
		idx := map[string]int{}
		// Versions without assets are exported too, so tags and empty versions survive.
		for arows.Next() {
			var version string
			var a AssetDef
			var sd, smt, sat *string
			var ssz *int64
			var sann []byte
			if err = arows.Scan(&version, &a.Key, &a.MediaType, &a.Digest, &a.Size, &a.Provenance, &sd, &ssz, &smt, &sat, &sann); err != nil {
				arows.Close()
				return nil, err
			}
			if sd != nil {
				a.Subject = &SubjectDef{Digest: *sd, Size: *ssz, MediaType: *smt, ArtifactType: *sat, Annotations: sann}
			}
			i, ok := idx[version]
			if !ok {
				i = len(c.def.Versions)
				idx[version] = i
				c.def.Versions = append(c.def.Versions, VersionDef{Version: version})
			}
			c.def.Versions[i].Assets = append(c.def.Versions[i].Assets, a)
		}
		arows.Close()
		if arows.Err() != nil {
			return nil, arows.Err()
		}
		vrows, err := tx.Query(ctx, `SELECT v.version FROM versions v WHERE v.component_id=$1 AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.version_id=v.id) ORDER BY v.version`, c.id)
		if err != nil {
			return nil, err
		}
		for vrows.Next() {
			var v string
			if err = vrows.Scan(&v); err != nil {
				vrows.Close()
				return nil, err
			}
			c.def.Versions = append(c.def.Versions, VersionDef{Version: v, Assets: []AssetDef{}})
		}
		vrows.Close()
		trows, err := tx.Query(ctx, `SELECT t.name,v.version FROM tags t JOIN versions v ON v.id=t.version_id WHERE t.component_id=$1 ORDER BY t.name`, c.id)
		if err != nil {
			return nil, err
		}
		for trows.Next() {
			var t TagDef
			if err = trows.Scan(&t.Name, &t.Version); err != nil {
				trows.Close()
				return nil, err
			}
			c.def.Tags = append(c.def.Tags, t)
		}
		trows.Close()
		out = append(out, c.def)
	}
	return out, nil
}
