package artifact

// Pull-through reads: the shared seam between format adapters and the remote
// source runtime.
//
// An adapter keeps its local read path unchanged and adds two hooks:
//
//   - RemoteMetadata for mutable, composed documents (index, packument,
//     maven-metadata.xml, tag lists). The document is proxied through the
//     runtime's bounded metadata cache (TTL, negative caching, stale-if-error)
//     and the adapter rewrites it to its own feed URLs. Nothing is persisted.
//   - Materialize for immutable artifacts. On a local miss the bytes are
//     fetched upstream, streamed to the client and cached into the artifact
//     store under the remote source, so that the next read is an ordinary
//     local hit through Lookup/Open.
//
// Both hooks apply package-owner semantics before any upstream request: if a
// source already owns the coordinate and it is not a remote source (or the
// caller cannot read it) the answer is ErrNotFound and no upstream is
// contacted. Remote sources are otherwise searched in feed priority order, and
// only a 404/410 moves on to the next source; an upstream error stops the
// search (fail closed) so an outage of a private source can never silently
// resolve a name from a lower priority public one.
//
// Publishes never reach this file: Publish and Prepare accept only local
// sources, and nothing here issues a non-GET request.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/cache"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
	"github.com/jackc/pgx/v5"
)

// RemoteBackend is implemented by *remoteruntime.Manager.
type RemoteBackend interface {
	Metadata(context.Context, remoteruntime.Request) (cache.Value, error)
	Fetch(context.Context, remoteruntime.Request) (*remoteruntime.Response, error)
}

// Origin names one upstream resource. Exactly one of Path (relative to the
// source origin, no leading slash, no query) or URL (absolute https on the
// source origin or a configured redirect origin) must be set. Adapters build
// it from validated coordinates, never from raw client input.
type Origin struct {
	Path, URL, Representation string
	// SourceID, when set, restricts the search to one remote source of the
	// feed. Adapters pass the source identifier embedded in the metadata they
	// served so bytes cannot come from a different source than the metadata.
	SourceID string
	// Digest, when valid, is verified before the first byte is served: the
	// artifact is fully staged and checked, then served from the store.
	Digest blob.Digest
}

// Document is an upstream metadata response. Stale is true when it was served
// from cache after revalidation failed (stale-if-error); adapters should mark
// such responses with StaleHeaders.
type Document struct {
	Body                      []byte
	MediaType, ETag, SourceID string
	Stale                     bool
	LastModified              string
}

// StaleHeaders returns the response headers adapters add to a stale document.
func StaleHeaders() map[string]string {
	return map[string]string{"X-Kuvryn-Cache": "stale", "Warning": `110 - "Response is Stale"`}
}

// Materialized is the result of a pull-through read. Body is a sequential
// stream of the complete artifact. Record is populated (Committed) only when
// the artifact is already in the store; on a streamed miss the cache commit
// happens when the stream reaches EOF.
type Materialized struct {
	Record    Record
	Body      io.ReadCloser
	Size      int64 // -1 when the upstream did not declare a length
	MediaType string
	SourceID  string
	Committed bool
}

func mapRemoteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, remoteruntime.ErrInput):
		return ErrInput
	default:
		return ErrUnavailable
	}
}

type remotePlan struct {
	domain  string
	sources []string
}

// plan applies package-owner semantics and read authorization. It returns
// ErrNotFound when no remote source may be consulted.
func (s *Service) plan(ctx context.Context, p identity.Principal, r Publication, only string) (remotePlan, error) {
	if s == nil || s.DB == nil || s.Remote == nil || p.PrincipalID == "" || !bounded(r.FeedID, 36) || !bounded(r.Coordinate, 256) || len(r.Namespace) > 256 {
		if s != nil && s.Remote == nil {
			return remotePlan{}, ErrNotFound
		}
		return remotePlan{}, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ownerSource, ownerKind, ownerNamespace string
	err := s.DB.QueryRow(ctx, `SELECT c.source_id::text,s.kind,COALESCE(c.namespace,'') FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id JOIN sources s ON s.id=c.source_id AND s.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, r.FeedID, r.Coordinate).Scan(&ownerSource, &ownerKind, &ownerNamespace)
	owned := true
	if errors.Is(err, pgx.ErrNoRows) {
		owned, err = false, nil
	}
	if err != nil {
		return remotePlan{}, ErrUnavailable
	}
	if owned && (ownerKind != "remote" || ownerNamespace != r.Namespace) {
		return remotePlan{}, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT s.id::text,f.revision FROM feeds f JOIN feed_sources fs ON fs.feed_id=f.id AND fs.instance_id=f.instance_id JOIN sources s ON s.id=fs.source_id AND s.instance_id=f.instance_id WHERE f.instance_id=$1 AND f.id=$2 AND s.kind='remote' AND s.format=f.format ORDER BY fs.priority`, p.InstanceID, r.FeedID)
	if err != nil {
		return remotePlan{}, ErrUnavailable
	}
	var ids []string
	var revision int64
	for rows.Next() {
		var id string
		if err = rows.Scan(&id, &revision); err != nil {
			break
		}
		if (!owned || id == ownerSource) && (only == "" || id == only) {
			ids = append(ids, id)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return remotePlan{}, ErrUnavailable
	}
	if len(ids) == 0 {
		return remotePlan{}, ErrNotFound
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if err != nil {
		return remotePlan{}, ErrUnavailable
	}
	result := remotePlan{domain: "feed:" + r.FeedID + "@" + strconv.FormatInt(revision, 10)}
	for _, id := range ids {
		if policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: r.FeedID, SourceID: id, Namespace: r.Namespace, Kind: identity.ArtifactResource}, identity.Read) {
			result.sources = append(result.sources, id)
		}
	}
	if len(result.sources) == 0 {
		return remotePlan{}, ErrNotFound
	}
	return result, nil
}

// HasRemote reports whether any remote source could answer for this
// publication. Adapters use it to skip upstream-specific parsing cheaply.
func (s *Service) HasRemote(ctx context.Context, p identity.Principal, r Publication) bool {
	_, err := s.plan(ctx, p, r, "")
	return err == nil
}

// RemoteMetadata proxies a mutable upstream document through the runtime's
// metadata cache. Only Namespace, Coordinate and FeedID of r are used.
func (s *Service) RemoteMetadata(ctx context.Context, p identity.Principal, r Publication, o Origin) (Document, error) {
	plan, err := s.plan(ctx, p, r, o.SourceID)
	if err != nil {
		return Document{}, err
	}
	for _, id := range plan.sources {
		value, err := s.Remote.Metadata(ctx, remoteruntime.Request{SourceID: id, AuthorizationDomain: plan.domain, Path: o.Path, URL: o.URL, Representation: o.Representation, AllowStale: true})
		if err != nil {
			return Document{}, mapRemoteError(err)
		}
		switch {
		case value.Status == 200:
			return Document{Body: value.Body, MediaType: value.MediaType, ETag: value.ETag, LastModified: value.LastModified, SourceID: id, Stale: value.Stale}, nil
		case value.Status == 404 || value.Status == 410:
		default:
			return Document{}, ErrUnavailable
		}
	}
	return Document{}, ErrNotFound
}

type flight struct{ done chan struct{} }

var errDigestMismatch = fmt.Errorf("%w: upstream content does not match the requested digest", ErrUnavailable)

func flightKey(r Publication) string {
	return r.FeedID + "\x00" + r.Coordinate + "\x00" + r.Version + "\x00" + r.AssetKey
}
func (s *Service) joinFlight(key string) (*flight, bool) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	if f := s.flights[key]; f != nil {
		return f, false
	}
	if s.flights == nil {
		s.flights = map[string]*flight{}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[key] = f
	return f, true
}
func (s *Service) endFlight(key string, f *flight) {
	s.flightMu.Lock()
	if s.flights[key] == f {
		delete(s.flights, key)
	}
	s.flightMu.Unlock()
	close(f.done)
}

func (s *Service) materializeLimit() int64 {
	if s.MaxMaterializeBytes > 0 {
		return s.MaxMaterializeBytes
	}
	return 8 << 30
}

// Materialize resolves a missing artifact from the feed's remote sources,
// caches it under the serving remote source and returns the bytes.
// Concurrent misses for the same asset are coalesced: one request goes
// upstream and the others are served from the store once it is committed.
// The caller must have established that Lookup/Open found nothing locally.
func (s *Service) Materialize(ctx context.Context, p identity.Principal, r Publication, o Origin) (*Materialized, error) {
	if s == nil || s.Lease == nil || s.Storage == nil || !bounded(r.Version, 256) || !bounded(r.AssetKey, 2048) {
		return nil, ErrInput
	}
	plan, err := s.plan(ctx, p, r, o.SourceID)
	if err != nil {
		return nil, err
	}
	key := flightKey(r)
	for attempt := 0; attempt < 4; attempt++ {
		f, leader := s.joinFlight(key)
		if !leader {
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			rec, body, err := s.Open(ctx, p, r, 0, -1)
			if err == nil {
				return &Materialized{Record: rec, Body: body, Size: rec.Info.Size, MediaType: rec.MediaType, SourceID: rec.SourceID, Committed: true}, nil
			}
			if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			continue // the leader failed; try to become the leader
		}
		var once sync.Once
		release := func() { once.Do(func() { s.endFlight(key, f) }) }
		m, err := s.lead(ctx, p, r, o, plan, release)
		if err != nil {
			release()
			return nil, err
		}
		return m, nil
	}
	return nil, ErrUnavailable
}

// lead fetches upstream. It owns the flight and calls release exactly once on
// every path that returns a Materialized, after the cache decision is final.
func (s *Service) lead(ctx context.Context, p identity.Principal, r Publication, o Origin, plan remotePlan, release func()) (*Materialized, error) {
	// A concurrent leader may have committed between the caller's miss and now.
	if rec, body, err := s.Open(ctx, p, r, 0, -1); err == nil {
		release()
		return &Materialized{Record: rec, Body: body, Size: rec.Info.Size, MediaType: rec.MediaType, SourceID: rec.SourceID, Committed: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, id := range plan.sources {
		response, err := s.Remote.Fetch(ctx, remoteruntime.Request{SourceID: id, AuthorizationDomain: plan.domain, Path: o.Path, URL: o.URL, Representation: o.Representation})
		if err != nil {
			return nil, mapRemoteError(err)
		}
		if response.StatusCode == 404 || response.StatusCode == 410 {
			response.Body.Close()
			continue
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return nil, ErrUnavailable
		}
		return s.cacheStream(ctx, p, r, o, id, response, release)
	}
	return nil, ErrNotFound
}

func declaredSize(h interface{ Get(string) string }) int64 {
	if v := h.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return -1
}

func (s *Service) cacheStream(ctx context.Context, p identity.Principal, r Publication, o Origin, source string, response *remoteruntime.Response, release func()) (*Materialized, error) {
	size := declaredSize(response.Header)
	if size > s.materializeLimit() {
		response.Body.Close()
		return nil, ErrUnavailable
	}
	if r.MediaType == "" {
		r.MediaType = "application/octet-stream"
		if parsed, _, err := mime.ParseMediaType(response.Header.Get("Content-Type")); err == nil && len(parsed) <= 256 {
			r.MediaType = parsed
		}
	}
	domain, err := s.remoteDomain(ctx, p, source)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	var store blob.Store
	if store, err = s.Storage(ctx, domain); err != nil || store == nil {
		store = nil // degrade to an uncached passthrough rather than failing the read
	}
	var upload blob.Upload
	if store != nil {
		if upload, err = store.Stage(ctx); err != nil {
			upload = nil
		}
	}
	t := &teeReader{want: o.Digest, svc: s, ctx: ctx, principal: p, pub: r, source: source, domain: domain, body: response.Body, upload: upload, hash: sha256.New(), expected: size, limit: s.materializeLimit(), release: release}
	if o.Digest.Valid() {
		// Verify before serving: stage everything, check, commit, then serve
		// from the store. The client never sees unverified bytes.
		defer release()
		defer response.Body.Close()
		if upload == nil {
			return nil, ErrUnavailable
		}
		if _, err = io.Copy(io.Discard, t); err != nil {
			t.abort()
			if errors.Is(err, errDigestMismatch) {
				return nil, err
			}
			return nil, ErrUnavailable
		}
		rec, body, err := s.Open(ctx, p, r, 0, -1)
		if err != nil {
			return nil, ErrUnavailable
		}
		return &Materialized{Record: rec, Body: body, Size: rec.Info.Size, MediaType: rec.MediaType, SourceID: rec.SourceID, Committed: true}, nil
	}
	return &Materialized{Body: t, Size: size, MediaType: r.MediaType, SourceID: source}, nil
}

// teeReader copies upstream bytes to the client while staging them. Only a
// clean EOF with matching length commits the cache entry; every other ending
// aborts the staged bytes, so a truncated or failed transfer is never visible
// as a package. Staging problems degrade to plain passthrough.
type teeReader struct {
	svc       *Service
	ctx       context.Context
	principal identity.Principal
	pub       Publication
	source    string
	domain    string
	body      io.ReadCloser
	upload    blob.Upload
	hash      hash.Hash
	n         int64
	expected  int64
	limit     int64
	release   func()
	want      blob.Digest
	finished  bool
	err       error
}

func (t *teeReader) abort() {
	if t.upload != nil {
		_ = t.upload.Abort()
		t.upload = nil
	}
}
func (t *teeReader) finish() {
	if !t.finished {
		t.finished = true
		t.release()
	}
}
func (t *teeReader) Read(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	n, err := t.body.Read(p)
	if n > 0 {
		t.n += int64(n)
		if t.n > t.limit {
			t.abort()
			t.err = ErrUnavailable
			t.finish()
			return n, t.err
		}
		if t.upload != nil {
			t.hash.Write(p[:n])
			if _, werr := t.upload.Write(p[:n]); werr != nil {
				t.abort()
			}
		}
	}
	switch {
	case err == io.EOF:
		if t.expected >= 0 && t.n != t.expected {
			t.abort()
			t.err = io.ErrUnexpectedEOF
			t.finish()
			return n, t.err
		}
		if t.want.Valid() && blob.Digest(hex.EncodeToString(t.hash.Sum(nil))) != t.want {
			t.abort()
			t.err = errDigestMismatch
			t.finish()
			return n, t.err
		}
		t.commit()
		t.err = io.EOF
		t.finish()
		return n, io.EOF
	case err != nil:
		t.abort()
		t.err = err
		t.finish()
		return n, err
	}
	return n, nil
}
func (t *teeReader) commit() {
	if t.upload == nil {
		return
	}
	upload := t.upload
	t.upload = nil
	// The client has the complete bytes; committing must outlive its context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), 30*time.Second)
	defer cancel()
	info := blob.Info{Digest: blob.Digest(hex.EncodeToString(t.hash.Sum(nil))), Size: t.n}
	if _, err := upload.Finalize(ctx, info); err != nil {
		_ = upload.Abort()
		return
	}
	t.pub.Info = info
	_, _ = t.svc.commitMaterialized(ctx, t.principal, t.pub, t.source, t.domain)
}
func (t *teeReader) Close() error {
	if !t.finished {
		t.abort()
	}
	err := t.body.Close()
	t.finish()
	return err
}

// remoteDomain returns the deduplication domain of a remote source, creating it
// on first use. Remote sources have no domain until something is cached.
func (s *Service) remoteDomain(ctx context.Context, p identity.Principal, source string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var domain string
	err := s.DB.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) SELECT $1,'source:'||s.id::text FROM sources s WHERE s.instance_id=$1 AND s.id=$2 AND s.kind='remote' ON CONFLICT(instance_id,name) DO UPDATE SET name=excluded.name RETURNING id::text`, p.InstanceID, source).Scan(&domain)
	if err != nil {
		return "", ErrUnavailable
	}
	return domain, nil
}

// commitMaterialized records already-finalized bytes as an asset of a remote
// source. The reading principal is recorded as the initiator; no publish
// permission is involved because remote sources never accept client writes.
func (s *Service) commitMaterialized(ctx context.Context, p identity.Principal, r Publication, source, domain string) (Record, error) {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Record{}, ErrUnavailable
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return Record{}, ErrDenied
	}
	if err = ownership.Require(ctx, tx, lease); err != nil {
		return Record{}, err
	}
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id JOIN feeds f ON f.id=fs.feed_id AND f.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3 AND s.kind='remote' AND s.format=f.format FOR SHARE OF fs,s`, p.InstanceID, r.FeedID, source).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrUnavailable
	}
	digest := sha256.Sum256([]byte(p.InstanceID + "\x00" + source + "\x00" + r.Coordinate))
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		return Record{}, ErrUnavailable
	}
	var component, version string
	err = tx.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate WHERE components.namespace=excluded.namespace RETURNING id::text`, p.InstanceID, source, r.Coordinate, r.Namespace).Scan(&component)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrConflict
	}
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if err = tx.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) ON CONFLICT(component_id,version) DO UPDATE SET version=excluded.version RETURNING id::text`, p.InstanceID, component, r.Version).Scan(&version); err != nil {
		return Record{}, ErrUnavailable
	}
	result := Record{SourceID: source, VersionID: version, DomainID: domain, Info: r.Info, MediaType: r.MediaType}
	var oldDigest string
	var oldSize int64
	err = tx.QueryRow(ctx, `SELECT a.id::text,b.digest,b.size_bytes FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.instance_id=$1 AND a.version_id=$2 AND a.asset_key=$3`, p.InstanceID, version, r.AssetKey).Scan(&result.ID, &oldDigest, &oldSize)
	if err == nil {
		// Immutable: a different upstream answer never replaces cached bytes.
		if oldDigest != string(r.Info.Digest) || oldSize != r.Info.Size {
			return Record{}, ErrConflict
		}
		result.Replayed = true
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrUnavailable
	}
	var blobID string
	var size int64
	if err = tx.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text,size_bytes`, p.InstanceID, domain, r.Info.Digest, r.Info.Size).Scan(&blobID, &size); err != nil {
		return Record{}, ErrUnavailable
	}
	if size != r.Info.Size {
		return Record{}, blob.ErrIntegrity
	}
	store, serr := s.Storage(ctx, domain)
	if serr != nil {
		return Record{}, ErrUnavailable
	}
	if err = requireBlobBytes(ctx, store, r.Info); err != nil {
		return Record{}, err
	}
	if err = tx.QueryRow(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,media_type,provenance) VALUES($1,$2,$3,$4,$5,jsonb_build_object('source_id',$6::text,'principal_id',$7::text,'feed_id',$8::text,'origin','remote')) RETURNING id::text`, p.InstanceID, version, r.AssetKey, blobID, r.MediaType, source, p.PrincipalID, r.FeedID).Scan(&result.ID); err != nil {
		return Record{}, ErrUnavailable
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'artifact.materialize',$3) RETURNING id`, p.InstanceID, p.PrincipalID, result.ID).Scan(&audit); err != nil {
		return Record{}, ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'artifact.materialize',jsonb_build_object('asset_id',$2::text,'source_id',$3::text,'digest',$4::text))`, audit, result.ID, source, r.Info.Digest); err != nil {
		return Record{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Record{}, ErrUnavailable
	}
	return result, nil
}

// ResolveReference resolves a reference found in upstream metadata against the
// upstream document path (relative to the source origin). Same-origin results
// are returned as a relative Path; absolute references are returned as URL and
// are subject to the runtime's origin policy. Queries on same-origin relative
// references are rejected because Path cannot carry them.
func ResolveReference(documentPath, reference string) (path, absolute string, err error) {
	ref, err := url.Parse(reference)
	if err != nil || ref.User != nil {
		return "", "", ErrInput
	}
	ref.Fragment = ""
	if ref.IsAbs() {
		if ref.Scheme != "https" {
			return "", "", ErrInput
		}
		return "", ref.String(), nil
	}
	base := &url.URL{Scheme: "https", Host: "origin.invalid", Path: "/" + documentPath}
	resolved := base.ResolveReference(ref)
	if resolved.Host != "origin.invalid" {
		if resolved.Scheme == "" {
			resolved.Scheme = "https"
		}
		if resolved.Scheme != "https" {
			return "", "", ErrInput
		}
		return "", resolved.String(), nil
	}
	if resolved.RawQuery != "" || len(resolved.Path) < 2 {
		return "", "", ErrInput
	}
	return resolved.Path[1:], "", nil
}

// EscapePath percent-encodes each segment of a slash-separated upstream path so
// that coordinates containing spaces or other reserved characters round-trip
// through the runtime's relative-path validation.
func EscapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// ServeStream writes a streamed (cache-miss) artifact as a plain 200 response.
// Range and conditional headers are intentionally not honoured on a miss: the
// complete object is needed to populate the cache, and a full response is
// always valid. After the first byte, any failure aborts the connection so a
// truncated transfer can never be mistaken for a complete one, even when the
// length was not declared. HEAD requests cache nothing.
func (m *Materialized) ServeStream(w http.ResponseWriter, method string) {
	defer m.Body.Close()
	h := w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", m.MediaType)
	}
	if m.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(m.Size, 10))
	}
	h.Set("X-Kuvryn-Cache", "miss")
	w.WriteHeader(http.StatusOK)
	if method == http.MethodHead {
		return
	}
	if _, err := io.CopyBuffer(struct{ io.Writer }{w}, m.Body, make([]byte, 64<<10)); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// PullThrough is the adapter-facing entry point for an artifact read that just
// missed locally. It returns exactly one of:
//   - (stream, false, nil): a streamed miss; call stream.ServeStream.
//   - (nil, true, nil): the asset is now in the store (verified fetch or a
//     coalesced follower); repeat the adapter's normal local read, which also
//     provides ranges and conditionals.
//   - (nil, false, err): ErrNotFound (nothing upstream or not applicable),
//     ErrUnavailable (upstream failed; nothing is served from another source),
//     or an authorization/input error.
func (s *Service) PullThrough(ctx context.Context, p identity.Principal, r Publication, o Origin) (*Materialized, bool, error) {
	m, err := s.Materialize(ctx, p, r, o)
	if err != nil {
		return nil, false, err
	}
	if m.Committed {
		_ = m.Body.Close()
		return nil, true, nil
	}
	return m, false, nil
}

// CacheDocument stores small, already-verified bytes (for example an OCI
// manifest obtained while resolving a tag) as an immutable asset of the remote
// source that served them, so later reads by content address are local hits.
// Failure to cache never affects the response the caller already has.
func (s *Service) CacheDocument(ctx context.Context, p identity.Principal, r Publication, source string, data []byte) error {
	if s == nil || s.Storage == nil || s.Lease == nil || !bounded(r.Version, 256) || !bounded(r.AssetKey, 2048) || !bounded(r.MediaType, 256) || int64(len(data)) > 16<<20 {
		return ErrInput
	}
	sum := sha256.Sum256(data)
	r.Info = blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}
	domain, err := s.remoteDomain(ctx, p, source)
	if err != nil {
		return err
	}
	store, err := s.Storage(ctx, domain)
	if err != nil || store == nil {
		return ErrUnavailable
	}
	upload, err := store.Stage(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer upload.Abort()
	if _, err = upload.Write(data); err != nil {
		return ErrUnavailable
	}
	if _, err = upload.Finalize(ctx, r.Info); err != nil {
		return ErrUnavailable
	}
	_, err = s.commitMaterialized(ctx, p, r, source, domain)
	return err
}
