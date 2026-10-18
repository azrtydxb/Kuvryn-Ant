package swiftrepo

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// versions lists releases of the owning source for the requested package.
func (q *req) versions(ctx context.Context) ([]string, error) {
	rows, e := q.s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key=$4 AND o.namespace=$5 LIMIT 4097`, q.p.InstanceID, q.feed, q.coord(), releaseAsset, q.ns())
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			return nil, artifact.ErrUnavailable
		}
		out = append(out, v)
	}
	if rows.Err() != nil || len(out) > maxListed {
		return nil, artifact.ErrUnavailable
	}
	return out, nil
}

func (q *req) loadRelease(ctx context.Context) (*release, artifact.Record, error) {
	rec, b, e := readAsset(ctx, q.s.service, q.p, q.pub(q.version, releaseAsset, jsonMediaType), maxMetadata*4)
	if e != nil {
		return nil, rec, e
	}
	var rel release
	if json.Unmarshal(b, &rel) != nil || len(rel.Checksum) != 64 {
		return nil, rec, artifact.ErrUnavailable
	}
	return &rel, rec, nil
}

func (q *req) list() {
	ctx, cancel := context.WithTimeout(q.r.Context(), 15*time.Second)
	defer cancel()
	vs, e := q.versions(ctx)
	if e != nil {
		failure(q.w, e)
		return
	}
	if len(vs) == 0 {
		failure(q.w, artifact.ErrNotFound)
		return
	}
	sortVersions(vs)
	q.version = vs[len(vs)-1]
	// Lookup applies source and artifact read authorization for the owner.
	if _, e = q.s.service.Lookup(ctx, q.p, q.pub(q.version, releaseAsset, jsonMediaType)); e != nil {
		failure(q.w, e)
		return
	}
	releases := map[string]any{}
	for i := len(vs) - 1; i >= 0; i-- {
		releases[vs[i]] = map[string]string{"url": q.url(q.scope, q.name, vs[i])}
	}
	links := []string{
		fmt.Sprintf(`<%s>; rel="latest-version"`, q.url(q.scope, q.name, vs[len(vs)-1])),
	}
	q.w.Header().Set("Link", strings.Join(links, ", "))
	writeJSON(q.w, q.r, map[string]any{"releases": releases})
}

func (q *req) release() {
	ctx, cancel := context.WithTimeout(q.r.Context(), 10*time.Second)
	defer cancel()
	rel, _, e := q.loadRelease(ctx)
	if e != nil {
		failure(q.w, e)
		return
	}
	res := map[string]any{"name": "source-archive", "type": zipMediaType, "checksum": rel.Checksum}
	if rel.ArchiveSig != "" {
		res["signing"] = map[string]string{"signatureBase64Encoded": rel.ArchiveSig, "signatureFormat": rel.SignatureFormat}
	}
	out := map[string]any{"id": q.coord(), "version": q.version, "resources": []any{res}, "metadata": rel.Metadata}
	if rel.MetadataSig != "" {
		out["metadataSignature"] = map[string]string{"signatureBase64Encoded": rel.MetadataSig, "signatureFormat": rel.SignatureFormat}
	}
	q.w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="latest-version"`, q.url(q.scope, q.name, q.version)))
	writeJSON(q.w, q.r, out)
}

func (q *req) archive() {
	ctx := q.r.Context()
	rel, _, e := q.loadRelease(ctx)
	if e != nil {
		failure(q.w, e)
		return
	}
	rec, body, e := q.s.service.Open(ctx, q.p, q.pub(q.version, archiveAsset, zipMediaType), 0, -1)
	if e != nil {
		failure(q.w, e)
		return
	}
	defer body.Close()
	if string(rec.Info.Digest) != rel.Checksum {
		failure(q.w, artifact.ErrUnavailable)
		return
	}
	raw, _ := hex.DecodeString(rel.Checksum)
	h := q.w.Header()
	h.Set("Content-Type", zipMediaType)
	h.Set("Content-Length", strconv.FormatInt(rec.Info.Size, 10))
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, strings.ToLower(q.name), q.version))
	h.Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(raw))
	h.Set("ETag", `"`+rel.Checksum+`"`)
	if rel.ArchiveSig != "" {
		h.Set("X-Swift-Package-Signature-Format", rel.SignatureFormat)
		h.Set("X-Swift-Package-Signature", rel.ArchiveSig)
	}
	if q.r.Method != "HEAD" {
		_, _ = io.CopyBuffer(q.w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

func (q *req) manifest() {
	ctx, cancel := context.WithTimeout(q.r.Context(), 10*time.Second)
	defer cancel()
	rel, _, e := q.loadRelease(ctx)
	if e != nil {
		failure(q.w, e)
		return
	}
	file := defaultManif
	if sv := q.r.URL.Query().Get("swift-version"); sv != "" {
		if !swiftVerPattern.MatchString(sv) {
			failure(q.w, perr(400, "invalid swift-version"))
			return
		}
		for _, m := range rel.Manifests {
			if m.File != defaultManif && m.ToolsVersion == sv {
				file = m.File
			}
		}
	}
	_, b, e := readAsset(ctx, q.s.service, q.p, q.pub(q.version, file, "text/x-swift"), maxManifest)
	if e != nil {
		failure(q.w, e)
		return
	}
	var links []string
	for _, m := range rel.Manifests {
		if m.File == defaultManif {
			continue
		}
		links = append(links, fmt.Sprintf(`<%s>; rel="alternate"; filename="%s"; swift-tools-version="%s"`, q.url(q.scope, q.name, q.version, defaultManif)+"?swift-version="+url.QueryEscape(m.ToolsVersion), m.File, m.ToolsVersion))
	}
	h := q.w.Header()
	if len(links) > 0 {
		h.Set("Link", strings.Join(links, ", "))
	}
	h.Set("Content-Type", "text/x-swift")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, file))
	h.Set("Content-Length", strconv.Itoa(len(b)))
	if q.r.Method != "HEAD" {
		_, _ = q.w.Write(b)
	}
}

// identifiers resolves a repository URL to registry identifiers using the
// repositoryURLs recorded in publish metadata of owned releases.
func (q *req) identifiers() {
	raw := q.r.URL.Query().Get("url")
	want := normalizeRepoURL(raw)
	if want == "" {
		failure(q.w, perr(400, "invalid url parameter"))
		return
	}
	ctx, cancel := context.WithTimeout(q.r.Context(), 15*time.Second)
	defer cancel()
	rows, e := q.s.db.Query(ctx, `SELECT DISTINCT ON (c.coordinate,v.version) c.namespace,c.coordinate,v.version FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id JOIN versions v ON v.component_id=c.id JOIN assets a ON a.version_id=v.id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND a.asset_key=$3 AND c.id=(SELECT c2.id FROM feed_sources f2 JOIN components c2 ON c2.source_id=f2.source_id AND c2.instance_id=f2.instance_id WHERE f2.instance_id=$1 AND f2.feed_id=$2 AND c2.coordinate=c.coordinate ORDER BY f2.priority LIMIT 1) ORDER BY c.coordinate,v.version LIMIT 4097`, q.p.InstanceID, q.feed, releaseAsset)
	if e != nil {
		failure(q.w, artifact.ErrUnavailable)
		return
	}
	type entry struct{ ns, coord, version string }
	var entries []entry
	for rows.Next() {
		var x entry
		if e = rows.Scan(&x.ns, &x.coord, &x.version); e != nil {
			break
		}
		entries = append(entries, x)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil || len(entries) > maxListed {
		failure(q.w, artifact.ErrUnavailable)
		return
	}
	found := map[string]bool{}
	for _, x := range entries {
		if found[x.coord] {
			continue
		}
		pub := artifact.Publication{FeedID: q.feed, Namespace: x.ns, Coordinate: x.coord, Version: x.version, AssetKey: releaseAsset, MediaType: jsonMediaType}
		_, b, e := readAsset(ctx, q.s.service, q.p, pub, maxMetadata*4)
		if e != nil {
			continue // unreadable/unauthorized entries are simply not matched
		}
		var rel release
		if json.Unmarshal(b, &rel) != nil {
			continue
		}
		urls, _ := rel.Metadata["repositoryURLs"].([]any)
		for _, u := range urls {
			if s, _ := u.(string); normalizeRepoURL(s) == want {
				found[x.coord] = true
			}
		}
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		var p = q.p
		if !q.canRead(ctx, p) {
			failure(q.w, artifact.ErrDenied)
			return
		}
		failure(q.w, artifact.ErrNotFound)
		return
	}
	writeJSON(q.w, q.r, map[string]any{"identifiers": ids})
}

func (q *req) canRead(ctx context.Context, p identity.Principal) bool {
	policy, e := (identity.Roles{DB: q.s.db}).Resolve(ctx, p)
	return e == nil && policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: q.feed, Kind: identity.FeedResource}, identity.Read)
}

var _ = http.StatusOK
