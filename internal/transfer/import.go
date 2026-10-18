package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Content supplies blob bytes for an import. Check is a cheap or cached
// verification used by dry runs; Open streams the object for an apply, whose
// digest is verified again while it is written.
type Content interface {
	Check(ctx context.Context, digest string, size int64) error
	Open(ctx context.Context, digest string) (io.ReadCloser, error)
}

// Check implements Content.
func (b *Bundle) Check(ctx context.Context, digest string, size int64) error {
	n, ok := b.has(digest)
	if !ok {
		return errors.New("not listed in the bundle manifest")
	}
	return b.verifyBlob(ctx, BlobEntry{Digest: digest, Size: n})
}

// Open implements Content.
func (b *Bundle) Open(_ context.Context, digest string) (io.ReadCloser, error) {
	return b.openBlob(digest)
}

// ImportOptions control Import.
type ImportOptions struct {
	// DryRun computes the report in a transaction that is rolled back and
	// writes no content.
	DryRun bool
}

type srcState struct {
	id, kind, format, domain string
	usable                   bool
}

type importer struct {
	tx       pgx.Tx
	instance string
	stores   Stores
	content  Content
	dry      bool
	rep      *Report
	sources  map[string]*srcState
	checked  map[string]error
}

// Import applies doc to the instance, or with DryRun reports what it would do.
// Apply is idempotent: re-running the same import reports everything unchanged.
// Existing content is never overwritten; a different digest at an existing path
// is reported as a conflict. content may be nil, in which case assets are only
// imported when their blob already exists in the target store.
func Import(ctx context.Context, pool *pgxpool.Pool, stores Stores, instance string, doc Document, content Content, opt ImportOptions) (*Report, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	im := &importer{tx: tx, instance: instance, stores: stores, content: content, dry: opt.DryRun, rep: newReport(opt.DryRun), sources: map[string]*srcState{}, checked: map[string]error{}}
	// Serialize imports per instance so concurrent applies cannot interleave.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('transfer-import:'||$1::text,0))`, instance); err != nil {
		return nil, err
	}
	for _, s := range doc.Sources {
		if err = im.source(ctx, s); err != nil {
			return nil, err
		}
	}
	for _, f := range doc.Feeds {
		if err = im.feed(ctx, f); err != nil {
			return nil, err
		}
	}
	for _, c := range doc.Components {
		if err = im.component(ctx, c); err != nil {
			return nil, err
		}
	}
	if opt.DryRun {
		return im.rep, nil
	}
	return im.rep, tx.Commit(ctx)
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

func (im *importer) source(ctx context.Context, s SourceDef) error {
	st := &srcState{kind: s.Kind, format: s.Format}
	im.sources[s.Name] = st
	var kind, format string
	err := im.tx.QueryRow(ctx, `SELECT id::text,kind,format FROM sources WHERE instance_id=$1 AND name=$2`, im.instance, s.Name).Scan(&st.id, &kind, &format)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if s.Kind == "replica" {
			im.rep.add("source", s.Name, ActionSkipped, "replica sources require a replication peer; configure replication on the target")
			im.rep.unmapped("source", s.Name, "replica source not imported")
			return nil
		}
		if !KnownFormats[s.Format] {
			im.rep.add("source", s.Name, ActionSkipped, "format "+s.Format+" is not supported by the target")
			im.rep.unmapped("source", s.Name, "unsupported format "+s.Format)
			return nil
		}
		if err = im.tx.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,$3,$4) RETURNING id::text`, im.instance, s.Name, s.Kind, s.Format).Scan(&st.id); err != nil {
			return err
		}
		im.rep.add("source", s.Name, ActionCreated, "")
	case err != nil:
		return err
	case kind != s.Kind || format != s.Format:
		im.rep.add("source", s.Name, ActionConflict, fmt.Sprintf("target has kind %s/format %s, bundle has %s/%s", kind, format, s.Kind, s.Format))
		return nil
	default:
		im.rep.add("source", s.Name, ActionUnchanged, "")
	}
	st.usable = true
	if s.Kind == "local" {
		if _, err = im.tx.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) ON CONFLICT(instance_id,name) DO NOTHING`, im.instance, "source:"+st.id); err != nil {
			return err
		}
		if err = im.tx.QueryRow(ctx, `SELECT id::text FROM dedup_domains WHERE instance_id=$1 AND name=$2`, im.instance, "source:"+st.id).Scan(&st.domain); err != nil {
			return err
		}
	}
	if s.Kind == "remote" && s.RemoteConfig != nil {
		if err = im.remoteConfig(ctx, s, st); err != nil {
			return err
		}
	}
	if s.Credential != nil {
		var has bool
		if err = im.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM remote_source_configs WHERE instance_id=$1 AND source_id=$2 AND credential_id IS NOT NULL)`, im.instance, st.id).Scan(&has); err != nil {
			return err
		}
		if !has {
			im.rep.unmapped("credential", s.Name, "the source used a credential ("+s.Credential.Purpose+"); the secret is never exported, provision it with PUT /api/v1/sources/{id}/remote-credential")
		}
	}
	return im.retention(ctx, "source", s.Name, "source_id", st.id, s.Retention)
}

func (im *importer) remoteConfig(ctx context.Context, s SourceDef, st *srcState) error {
	var cfg remoteconfig.Config
	if err := json.Unmarshal(*s.RemoteConfig, &cfg); err != nil {
		im.rep.add("remote_config", s.Name, ActionSkipped, "configuration is not valid")
		im.rep.unmapped("remote_config", s.Name, "configuration is not valid JSON for a remote source")
		return nil
	}
	cfg.CredentialID = ""
	norm, err := remoteconfig.Normalize(cfg)
	if err != nil {
		im.rep.add("remote_config", s.Name, ActionSkipped, "configuration rejected: "+err.Error())
		im.rep.unmapped("remote_config", s.Name, "configuration rejected by the target: "+err.Error())
		return nil
	}
	var old []byte
	var cred *string
	err = im.tx.QueryRow(ctx, `SELECT config,credential_id::text FROM remote_source_configs WHERE instance_id=$1 AND source_id=$2`, im.instance, st.id).Scan(&old, &cred)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	want := norm
	if cred != nil {
		want.CredentialID = *cred
	}
	enc, _ := json.Marshal(want)
	if exists && sameJSON(old, enc) {
		im.rep.add("remote_config", s.Name, ActionUnchanged, "")
		return nil
	}
	if _, err = im.tx.Exec(ctx, `INSERT INTO remote_source_configs(instance_id,source_id,credential_id,config) VALUES($1,$2,$3::uuid,$4)
 ON CONFLICT(instance_id,source_id) DO UPDATE SET config=EXCLUDED.config,updated_at=now()`, im.instance, st.id, cred, enc); err != nil {
		return err
	}
	if _, err = im.tx.Exec(ctx, `UPDATE sources SET revision=revision+1 WHERE id=$1`, st.id); err != nil {
		return err
	}
	if exists {
		im.rep.add("remote_config", s.Name, ActionUpdated, "")
	} else {
		im.rep.add("remote_config", s.Name, ActionCreated, "")
	}
	return nil
}

func (im *importer) retention(ctx context.Context, kind, name, col, id string, r *RetentionDef) error {
	if r == nil {
		return nil
	}
	label := kind + " " + name
	cur, err := retentionRow(im.tx.QueryRow(ctx, `SELECT `+retentionCols+` FROM retention_policies WHERE instance_id=$1 AND `+col+`=$2`, im.instance, id))
	if err != nil {
		return err
	}
	if cur != nil && reflect.DeepEqual(*cur, *r) {
		im.rep.add("retention", label, ActionUnchanged, "")
		return nil
	}
	if cur == nil {
		_, err = im.tx.Exec(ctx, `INSERT INTO retention_policies(instance_id,`+col+`,keep_last,max_age_seconds,max_idle_seconds,min_keep,include_tagged,allow_release_delete,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			im.instance, id, r.KeepLast, r.MaxAgeSeconds, r.MaxIdleSeconds, r.MinKeep, r.IncludeTagged, r.AllowReleaseDelete, r.Enabled)
	} else {
		_, err = im.tx.Exec(ctx, `UPDATE retention_policies SET keep_last=$3,max_age_seconds=$4,max_idle_seconds=$5,min_keep=$6,include_tagged=$7,allow_release_delete=$8,enabled=$9,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND `+col+`=$2`,
			im.instance, id, r.KeepLast, r.MaxAgeSeconds, r.MaxIdleSeconds, r.MinKeep, r.IncludeTagged, r.AllowReleaseDelete, r.Enabled)
	}
	if err != nil {
		return err
	}
	if cur == nil {
		im.rep.add("retention", label, ActionCreated, "")
	} else {
		im.rep.add("retention", label, ActionUpdated, "")
	}
	return nil
}

func (im *importer) feed(ctx context.Context, f FeedDef) error {
	var id, format string
	var anon bool
	err := im.tx.QueryRow(ctx, `SELECT id::text,format,anonymous_read FROM feeds WHERE instance_id=$1 AND slug=$2`, im.instance, f.Slug).Scan(&id, &format, &anon)
	created, changed := false, false
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err = im.tx.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format,anonymous_read) VALUES($1,$2,$3,$4) RETURNING id::text`, im.instance, f.Slug, f.Format, f.AnonymousRead).Scan(&id); err != nil {
			return err
		}
		created = true
	case err != nil:
		return err
	case format != f.Format:
		im.rep.add("feed", f.Slug, ActionConflict, fmt.Sprintf("target feed has format %s, bundle has %s", format, f.Format))
		return nil
	case anon != f.AnonymousRead:
		changed = true
		if _, err = im.tx.Exec(ctx, `UPDATE feeds SET anonymous_read=$2 WHERE id=$1`, id, f.AnonymousRead); err != nil {
			return err
		}
	}
	// Feed source ordering.
	type link struct {
		id       string
		priority int
	}
	var want []link
	byName := map[string]link{}
	for _, ref := range f.Sources {
		st := im.sources[ref.Source]
		switch {
		case st == nil || !st.usable:
			im.rep.add("feed_source", f.Slug+"/"+ref.Source, ActionSkipped, "source was not imported")
		case st.format != f.Format:
			im.rep.add("feed_source", f.Slug+"/"+ref.Source, ActionConflict, fmt.Sprintf("source format %s differs from feed format %s", st.format, f.Format))
		default:
			l := link{st.id, ref.Priority}
			want = append(want, l)
			byName[ref.Source] = l
		}
	}
	rows, err := im.tx.Query(ctx, `SELECT source_id::text,priority FROM feed_sources WHERE feed_id=$1 ORDER BY priority`, id)
	if err != nil {
		return err
	}
	var have []link
	for rows.Next() {
		var l link
		if err = rows.Scan(&l.id, &l.priority); err != nil {
			rows.Close()
			return err
		}
		have = append(have, l)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	slices.SortFunc(want, func(a, b link) int { return a.priority - b.priority })
	if !slices.Equal(have, want) && len(want) > 0 {
		changed = true
		if _, err = im.tx.Exec(ctx, `DELETE FROM feed_sources WHERE feed_id=$1`, id); err != nil {
			return err
		}
		for _, l := range want {
			if _, err = im.tx.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,$4)`, im.instance, id, l.id, l.priority); err != nil {
				return err
			}
		}
		im.rep.add("feed_source", f.Slug, ternary(len(have) == 0, ActionCreated, ActionUpdated), fmt.Sprintf("%d ordered source(s)", len(want)))
	} else if len(want) > 0 {
		im.rep.add("feed_source", f.Slug, ActionUnchanged, "")
	}
	if f.PublishPolicy != nil {
		pc, err := im.publishPolicy(ctx, f, id, func(n string) (string, bool) { l, ok := byName[n]; return l.id, ok })
		if err != nil {
			return err
		}
		changed = changed || pc
	}
	if err = im.retention(ctx, "feed", f.Slug, "feed_id", id, f.Retention); err != nil {
		return err
	}
	if changed && !created {
		if _, err = im.tx.Exec(ctx, `UPDATE feeds SET revision=revision+1 WHERE id=$1`, id); err != nil {
			return err
		}
	}
	switch {
	case created:
		im.rep.add("feed", f.Slug, ActionCreated, "")
	case changed:
		im.rep.add("feed", f.Slug, ActionUpdated, "")
	default:
		im.rep.add("feed", f.Slug, ActionUnchanged, "")
	}
	return nil
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func (im *importer) publishPolicy(ctx context.Context, f FeedDef, feed string, resolve func(string) (string, bool)) (bool, error) {
	name := "feed " + f.Slug
	pp := f.PublishPolicy
	var dflt string
	if pp.DefaultSource != "" {
		id, ok := resolve(pp.DefaultSource)
		if !ok {
			im.rep.add("publish_policy", f.Slug, ActionSkipped, "default source "+pp.DefaultSource+" is not available")
			im.rep.unmapped("publish_policy", name, "default source "+pp.DefaultSource+" is not available on the target")
			return false, nil
		}
		dflt = id
	}
	rules := make([]routing.Rule, 0, len(pp.Rules))
	for _, r := range pp.Rules {
		if r.Selector.Principal != "" {
			im.rep.unmapped("publish_rule", name+"/"+r.ID, "rule selects a principal ID, which is not portable between instances; rule dropped")
			continue
		}
		if r.Target != "" {
			id, ok := resolve(r.Target)
			if !ok {
				im.rep.unmapped("publish_rule", name+"/"+r.ID, "target source "+r.Target+" is not available; rule dropped")
				continue
			}
			r.Target = id
		}
		rules = append(rules, r)
	}
	var srcs []routing.Source
	rows, err := im.tx.Query(ctx, `SELECT s.id::text,s.kind FROM feed_sources fs JOIN sources s ON s.id=fs.source_id WHERE fs.feed_id=$1`, feed)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var s routing.Source
		var k string
		if err = rows.Scan(&s.ID, &k); err != nil {
			rows.Close()
			return false, err
		}
		s.Kind = routing.SourceKind(k)
		srcs = append(srcs, s)
	}
	rows.Close()
	if _, err = routing.Compile(routing.Config{Sources: srcs, DefaultTarget: dflt, Rules: rules}); err != nil {
		im.rep.add("publish_policy", f.Slug, ActionSkipped, "policy rejected: "+err.Error())
		im.rep.unmapped("publish_policy", name, "policy rejected by routing validation: "+err.Error())
		return false, nil
	}
	enc, _ := json.Marshal(rules)
	var oldDefault *string
	var oldRules []byte
	err = im.tx.QueryRow(ctx, `SELECT default_source_id::text,rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2`, im.instance, feed).Scan(&oldDefault, &oldRules)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if exists && (oldDefault == nil && dflt == "" || oldDefault != nil && *oldDefault == dflt) && sameJSON(oldRules, enc) {
		im.rep.add("publish_policy", f.Slug, ActionUnchanged, "")
		return false, nil
	}
	if _, err = im.tx.Exec(ctx, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id,rules) VALUES($1,$2,NULLIF($3,'')::uuid,$4) ON CONFLICT(feed_id) DO UPDATE SET default_source_id=excluded.default_source_id,rules=excluded.rules`, im.instance, feed, dflt, enc); err != nil {
		return false, err
	}
	im.rep.add("publish_policy", f.Slug, ternary(exists, ActionUpdated, ActionCreated), "")
	return true, nil
}

func (im *importer) component(ctx context.Context, c ComponentDef) error {
	label := c.Source + ":" + c.Coordinate
	st := im.sources[c.Source]
	if st == nil || !st.usable || st.kind != "local" {
		im.rep.add("component", label, ActionSkipped, "source is not an imported local source")
		return nil
	}
	meta := []byte(c.Metadata)
	if len(meta) == 0 {
		meta = []byte("{}")
	}
	var compID, oldNS string
	var oldMeta []byte
	err := im.tx.QueryRow(ctx, `SELECT id::text,COALESCE(namespace,''),metadata FROM components WHERE source_id=$1 AND coordinate=$2`, st.id, c.Coordinate).Scan(&compID, &oldNS, &oldMeta)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err = im.tx.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace,metadata) VALUES($1,$2,$3,$4,$5) RETURNING id::text`, im.instance, st.id, c.Coordinate, c.Namespace, meta).Scan(&compID); err != nil {
			return err
		}
		im.rep.add("component", label, ActionCreated, "")
	case err != nil:
		return err
	case oldNS != c.Namespace || !sameJSON(oldMeta, meta):
		if _, err = im.tx.Exec(ctx, `UPDATE components SET namespace=$2,metadata=$3 WHERE id=$1`, compID, c.Namespace, meta); err != nil {
			return err
		}
		im.rep.add("component", label, ActionUpdated, "format metadata changed")
	default:
		im.rep.add("component", label, ActionUnchanged, "")
	}
	versions := map[string]string{}
	for _, v := range c.Versions {
		var vid string
		if err = im.tx.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) ON CONFLICT(component_id,version) DO UPDATE SET version=EXCLUDED.version RETURNING id::text`, im.instance, compID, v.Version).Scan(&vid); err != nil {
			return err
		}
		versions[v.Version] = vid
		for _, a := range v.Assets {
			if err = im.asset(ctx, st, label+"@"+v.Version, compID, vid, a); err != nil {
				return err
			}
		}
	}
	for _, t := range c.Tags {
		vid, ok := versions[t.Version]
		if !ok {
			im.rep.add("tag", label+"#"+t.Name, ActionSkipped, "tagged version not in bundle")
			continue
		}
		var cur string
		err = im.tx.QueryRow(ctx, `SELECT version_id::text FROM tags WHERE component_id=$1 AND name=$2`, compID, t.Name).Scan(&cur)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if _, err = im.tx.Exec(ctx, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3)`, compID, t.Name, vid); err != nil {
				return err
			}
			im.rep.add("tag", label+"#"+t.Name, ActionCreated, "")
		case err != nil:
			return err
		case cur != vid:
			if _, err = im.tx.Exec(ctx, `UPDATE tags SET version_id=$3,revision=revision+1 WHERE component_id=$1 AND name=$2`, compID, t.Name, vid); err != nil {
				return err
			}
			im.rep.add("tag", label+"#"+t.Name, ActionUpdated, "")
		default:
			im.rep.add("tag", label+"#"+t.Name, ActionUnchanged, "")
		}
	}
	return nil
}

func (im *importer) asset(ctx context.Context, st *srcState, label, comp, version string, a AssetDef) error {
	name := label + "/" + a.Key
	var assetID, oldDigest string
	err := im.tx.QueryRow(ctx, `SELECT a.id::text,b.digest FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.version_id=$1 AND a.asset_key=$2`, version, a.Key).Scan(&assetID, &oldDigest)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if exists && oldDigest != a.Digest {
		im.rep.add("asset", name, ActionConflict, "target has different content ("+oldDigest[:12]+") at this path; not overwritten")
		return nil
	}
	size, reason, err := im.ensureBlob(ctx, st, a)
	if err != nil {
		return err
	}
	if reason != "" {
		im.rep.add("asset", name, ActionSkipped, reason)
		return nil
	}
	var blobID string
	var have int64
	if err = im.tx.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3)
 ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=EXCLUDED.digest RETURNING id::text,size_bytes`, im.instance, st.domain, a.Digest, max(size, 0)).Scan(&blobID, &have); err != nil {
		return err
	}
	if size >= 0 && have != size && !(im.dry && have == 0) {
		im.rep.add("asset", name, ActionConflict, "target blob record has a different size")
		return nil
	}
	media := a.MediaType
	if media == "" {
		media = "application/octet-stream"
	}
	prov := []byte(a.Provenance)
	if len(prov) == 0 {
		prov = []byte("{}")
	}
	if !exists {
		if err = im.tx.QueryRow(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,media_type,provenance) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, im.instance, version, a.Key, blobID, media, prov).Scan(&assetID); err != nil {
			return err
		}
		im.rep.add("asset", name, ActionCreated, "")
	} else {
		im.rep.add("asset", name, ActionUnchanged, "")
	}
	if a.Subject != nil {
		ann := []byte(a.Subject.Annotations)
		if len(ann) == 0 {
			ann = []byte("{}")
		}
		if _, err = im.tx.Exec(ctx, `INSERT INTO artifact_subjects(asset_id,instance_id,component_id,subject_digest,subject_size,subject_media_type,artifact_type,annotations) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT(asset_id) DO NOTHING`, assetID, im.instance, comp, a.Subject.Digest, a.Subject.Size, a.Subject.MediaType, a.Subject.ArtifactType, ann); err != nil {
			return err
		}
	}
	return nil
}

// ensureBlob makes the object exist and verified in the source's blob store and
// returns its size. A non-empty reason means the asset must be skipped. In a dry
// run nothing is written; content is only checked.
func (im *importer) ensureBlob(ctx context.Context, st *srcState, a AssetDef) (int64, string, error) {
	var store blob.Store
	if im.stores != nil {
		var err error
		if store, err = im.stores.Resolve(ctx, st.domain); err != nil {
			return 0, "", err
		}
		if info, err := store.Stat(ctx, blob.Digest(a.Digest)); err == nil && (a.Size < 0 || info.Size == a.Size) {
			return info.Size, "", nil
		}
	}
	if im.content == nil {
		return 0, "content is not in the target store and no content was supplied", nil
	}
	if im.dry {
		err, ok := im.checked[a.Digest]
		if !ok {
			err = im.content.Check(ctx, a.Digest, a.Size)
			im.checked[a.Digest] = err
			if err != nil {
				im.rep.IntegrityFailures = append(im.rep.IntegrityFailures, Failure{Digest: a.Digest, Reason: err.Error()})
			}
		}
		if err != nil {
			return 0, "content failed verification: " + err.Error(), nil
		}
		return a.Size, "", nil
	}
	if store == nil {
		return 0, "no blob store is configured", nil
	}
	src, err := im.content.Open(ctx, a.Digest)
	if err != nil {
		im.rep.IntegrityFailures = append(im.rep.IntegrityFailures, Failure{Digest: a.Digest, Reason: "content unavailable: " + err.Error()})
		return 0, "content unavailable", nil
	}
	defer src.Close()
	up, err := store.Stage(ctx)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(up, h), ctxReader{ctx, src})
	if ctx.Err() != nil {
		_ = up.Abort()
		return 0, "", ctx.Err()
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != a.Digest {
		err = errors.New("content digest mismatch")
	}
	if err == nil && a.Size >= 0 && n != a.Size {
		err = errors.New("content size differs from the inventory")
	}
	if err != nil {
		_ = up.Abort()
		im.rep.IntegrityFailures = append(im.rep.IntegrityFailures, Failure{Digest: a.Digest, Reason: err.Error()})
		return 0, "content failed verification: " + err.Error(), nil
	}
	if _, err = up.Finalize(ctx, blob.Info{Digest: blob.Digest(a.Digest), Size: n}); err != nil {
		return 0, "", err
	}
	return n, "", nil
}

// KnownFormats lists the repository formats a source may have (see the sources
// format constraint in the schema).
var KnownFormats = map[string]bool{}

func init() {
	for _, f := range []string{"apk", "ansible", "apt", "cocoapods", "composer", "conan", "conda", "oci", "git-lfs", "go", "helm", "huggingface", "maven", "npm", "nuget", "oci-artifacts", "p2", "pub", "pypi", "cran", "raw", "rubygems", "cargo", "swift", "terraform", "rpm"} {
		KnownFormats[f] = true
	}
}
