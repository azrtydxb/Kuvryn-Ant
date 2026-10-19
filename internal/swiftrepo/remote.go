package swiftrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// Pull-through for Swift registry (SE-0292) sources. The remote source origin is
// the registry root (the URL a client would pass to `swift package-registry
// set`). The release list and release resource are read through the runtime's
// metadata cache and validated; the source archive is fetched on first download
// and verified against the sha256 checksum of the upstream release resource
// before the first byte is served. Signatures the upstream states (archive and
// metadata) are kept in the cached release record and re-served unchanged so the
// Swift client verifies them against the publisher's key; Ant never signs.

const (
	registryJSON = "application/vnd.swift.registry.v1+json"
	registryZip  = "application/vnd.swift.registry.v1+zip"
	registrySwft = "application/vnd.swift.registry.v1+swift"
)

var hexChecksum = regexp.MustCompile(`^[0-9a-f]{64}$`)

type remoteSigning struct {
	Sig    string `json:"signatureBase64Encoded"`
	Format string `json:"signatureFormat"`
}

type upstreamRelease struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Resources []struct {
		Name     string         `json:"name"`
		Type     string         `json:"type"`
		Checksum string         `json:"checksum"`
		Signing  *remoteSigning `json:"signing"`
	} `json:"resources"`
	Metadata          map[string]any `json:"metadata"`
	MetadataSignature *remoteSigning `json:"metadataSignature"`
}

// remoteVersions reads the upstream release list.
func (q *req) remoteVersions(ctx context.Context) ([]string, artifact.Document, error) {
	doc, err := q.s.service.RemoteMetadata(ctx, q.p, artifact.Publication{FeedID: q.feed, Namespace: q.ns(), Coordinate: q.coord()}, artifact.Origin{Path: q.scope + "/" + q.name, Representation: registryJSON})
	if err != nil {
		return nil, doc, err
	}
	var in struct {
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if json.Unmarshal(doc.Body, &in) != nil || len(in.Releases) > maxListed {
		return nil, doc, artifact.ErrUnavailable
	}
	var out []string
	for v := range in.Releases {
		if validVersion(v) {
			out = append(out, v)
		}
	}
	return out, doc, nil
}

// remoteList serves the upstream release list. It returns true when the answer
// was written; a degraded upstream is returned as ErrUnavailable so the caller
// falls back to the locally cached view.
func (q *req) remoteList(ctx context.Context) (bool, error) {
	vs, doc, err := q.remoteVersions(ctx)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case err != nil:
		if errors.Is(err, artifact.ErrUnavailable) {
			return false, err
		}
		failure(q.w, err)
		return true, nil
	}
	if len(vs) == 0 {
		failure(q.w, artifact.ErrNotFound)
		return true, nil
	}
	sortVersions(vs)
	releases := map[string]any{}
	for i := len(vs) - 1; i >= 0; i-- {
		releases[vs[i]] = map[string]string{"url": q.url(q.scope, q.name, vs[i])}
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			q.w.Header().Set(k, v)
		}
	}
	q.w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="latest-version"`, q.url(q.scope, q.name, vs[len(vs)-1])))
	writeJSON(q.w, q.r, map[string]any{"releases": releases})
	return true, nil
}

// remoteRelease reads and validates the upstream release resource and converts
// it to the persisted release record. The archive checksum is the integrity
// anchor for the download.
func (q *req) remoteRelease(ctx context.Context) (*release, artifact.Document, error) {
	doc, err := q.s.service.RemoteMetadata(ctx, q.p, artifact.Publication{FeedID: q.feed, Namespace: q.ns(), Coordinate: q.coord()}, artifact.Origin{Path: q.scope + "/" + q.name + "/" + q.version, Representation: registryJSON})
	if err != nil {
		return nil, doc, err
	}
	var in upstreamRelease
	if json.Unmarshal(doc.Body, &in) != nil || (in.Version != "" && in.Version != q.version) || (in.ID != "" && !strings.EqualFold(in.ID, q.coord())) {
		return nil, doc, artifact.ErrUnavailable
	}
	rel := &release{Metadata: in.Metadata}
	if rel.Metadata == nil {
		rel.Metadata = map[string]any{}
	}
	found := false
	for _, res := range in.Resources {
		if res.Name != "source-archive" {
			continue
		}
		if !hexChecksum.MatchString(res.Checksum) {
			return nil, doc, artifact.ErrUnavailable
		}
		rel.Checksum, found = res.Checksum, true
		if res.Signing != nil && res.Signing.Sig != "" {
			rel.ArchiveSig, rel.SignatureFormat = res.Signing.Sig, res.Signing.Format
		}
	}
	if !found {
		return nil, doc, artifact.ErrUnavailable
	}
	if s := in.MetadataSignature; s != nil && s.Sig != "" {
		rel.MetadataSig = s.Sig
		if rel.SignatureFormat == "" {
			rel.SignatureFormat = s.Format
		}
	}
	if len(rel.ArchiveSig) > maxSignature || len(rel.MetadataSig) > maxSignature || len(rel.SignatureFormat) > 64 || !utf8.ValidString(rel.SignatureFormat) {
		return nil, doc, artifact.ErrUnavailable
	}
	return rel, doc, nil
}

// cacheRelease keeps the release record beside the cached bytes so the version is
// served and listed locally (including during an upstream outage).
func (q *req) cacheRelease(ctx context.Context, rel *release, source string) {
	if size := q.archiveSize(ctx); size > 0 {
		rel.Size = size
	}
	rb, err := json.Marshal(rel)
	if err != nil {
		return
	}
	_ = q.s.service.CacheDocument(ctx, q.p, q.pub(q.version, releaseAsset, jsonMediaType), source, rb)
}

func (q *req) archiveSize(ctx context.Context) int64 {
	rec, err := q.s.service.Lookup(ctx, q.p, q.pub(q.version, archiveAsset, zipMediaType))
	if err != nil {
		return 0
	}
	return rec.Info.Size
}

// remoteArchive materializes a missing archive. served is true when a streamed
// response was written; otherwise the archive and its release record are in the
// store and the caller repeats the local read.
func (q *req) remoteArchive(ctx context.Context) (served bool, err error) {
	rel, doc, err := q.remoteRelease(ctx)
	if err != nil {
		return false, err
	}
	pub := q.pub(q.version, archiveAsset, zipMediaType)
	stream, again, err := q.s.service.PullThrough(ctx, q.p, pub, artifact.Origin{
		Path: q.scope + "/" + q.name + "/" + q.version + ".zip", Representation: registryZip,
		SourceID: doc.SourceID, Digest: blob.Digest(rel.Checksum),
	})
	if err != nil {
		return false, err
	}
	if again {
		q.cacheRelease(ctx, rel, doc.SourceID)
		return false, nil
	}
	stream.ServeStream(q.w, q.r.Method)
	return true, nil
}

// remoteManifest caches the default Package.swift of a release. Alternate
// manifests (Package@swift-x.y.swift) are not proxied: the registry reports them
// only in Link headers, which the runtime does not expose.
func (q *req) remoteManifest(ctx context.Context) error {
	rel, doc, err := q.remoteRelease(ctx)
	if err != nil {
		return err
	}
	pub := q.pub(q.version, defaultManif, "text/x-swift")
	_, again, err := q.s.service.PullThrough(ctx, q.p, pub, artifact.Origin{
		Path: q.scope + "/" + q.name + "/" + q.version + "/Package.swift", Representation: registrySwft,
		SourceID: doc.SourceID,
		Verify: func(r io.ReadSeeker) error {
			b, err := io.ReadAll(io.LimitReader(r, maxManifest+1))
			if err != nil || len(b) > maxManifest || !utf8.Valid(b) {
				return errInvalid
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	if !again {
		return artifact.ErrUnavailable
	}
	q.cacheRelease(ctx, rel, doc.SourceID)
	return nil
}
