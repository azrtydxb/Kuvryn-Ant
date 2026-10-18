// Package cocoapods implements a CDN-style CocoaPods specs repository with
// hosted pod source archives; see README.md.
package cocoapods

import (
	"context"
	"crypto/md5" // CocoaPods CDN shard prefixes are defined as md5(pod name).
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const podspecKey = "podspec.json"
const maxPods = 50000

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid CocoaPods configuration")
	}
	return &Repository{db: db, service: s, instance: s.Lease().InstanceID, auth: identity.NewBearer(identity.Tokens{DB: db}), transfers: make(chan struct{}, 2)}, nil
}

func shard(name string) [3]string {
	h := md5.Sum([]byte(name))
	x := hex.EncodeToString(h[:])
	return [3]string{x[0:1], x[1:2], x[2:3]}
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "cocoapods" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='cocoapods'`, p.InstanceID, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	route := a[3:]
	switch {
	case len(route) == 3 && route[0] == "pods" && validName(route[1]) && validVersion(route[2]):
		switch r.Method {
		case "PUT", "POST":
			s.publish(w, r, p, feed, a[1], route[1], route[2])
		case "DELETE":
			s.remove(w, r, p, feed, route[1], route[2])
		default:
			w.Header().Set("Allow", "PUT, DELETE")
			w.WriteHeader(405)
		}
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(405)
		return
	}
	switch {
	case len(route) == 1 && route[0] == "CocoaPods-version.yml":
		s.authorizeFeed(w, r, p, feed, func() {
			serveBytes(w, r, "text/yaml", []byte("---\nmin: 1.0.0\nlast: 1.17.0\nprefix_lengths:\n- 1\n- 1\n- 1\n"))
		})
	case len(route) == 1 && route[0] == "deprecated_podspecs.txt":
		s.authorizeFeed(w, r, p, feed, func() { serveBytes(w, r, "text/plain", nil) })
	case len(route) == 1 && strings.HasPrefix(route[0], "all_pods_versions_") && strings.HasSuffix(route[0], ".txt"):
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(route[0], "all_pods_versions_"), ".txt"), "_")
		if len(parts) != 3 {
			failure(w, artifact.ErrNotFound)
			return
		}
		s.index(w, r, p, feed, [3]string{parts[0], parts[1], parts[2]})
	case len(route) == 7 && route[0] == "Specs" && validName(route[4]) && validVersion(route[5]) && route[6] == route[4]+".podspec.json":
		if shard(route[4]) != [3]string{route[1], route[2], route[3]} {
			failure(w, artifact.ErrNotFound)
			return
		}
		s.download(w, r, p, feed, route[4], route[5], podspecKey, "application/json")
	case len(route) == 4 && route[0] == "archives" && validName(route[1]) && validVersion(route[2]):
		for _, ext := range []string{"tar.gz", "zip"} {
			if route[3] == route[1]+"-"+route[2]+"."+ext {
				mt := "application/gzip"
				if ext == "zip" {
					mt = "application/zip"
				}
				s.download(w, r, p, feed, route[1], route[2], "source."+ext, mt)
				return
			}
		}
		failure(w, artifact.ErrNotFound)
	default:
		failure(w, artifact.ErrNotFound)
	}
}

func serveBytes(w http.ResponseWriter, r *http.Request, ct string, b []byte) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method != "HEAD" {
		_, _ = w.Write(b)
	}
}

func validName(n string) bool {
	return namePattern.MatchString(n) && n != "." && n != ".."
}
func validVersion(v string) bool { return versionPattern.MatchString(v) }

func (s *Repository) authorizeFeed(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, next func()) {
	policy, e := (identity.Roles{DB: s.db}).Resolve(r.Context(), p)
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
		failure(w, artifact.ErrDenied)
		return
	}
	next()
}

// owner resolves the package owner of one pod and whether the version is
// unlisted. Read permission is checked on the owning source.
func (s *Repository) owner(ctx context.Context, p identity.Principal, feed, pod, version string, action identity.Action) (source string, unlisted bool, err error) {
	err = s.db.QueryRow(ctx, `SELECT c.source_id::text, COALESCE(c.metadata->'unlisted' @> to_jsonb($4::text),false) FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, feed, pod, version).Scan(&source, &unlisted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, artifact.ErrNotFound
	}
	if err != nil {
		return "", false, err
	}
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		return "", false, e
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Kind: identity.ArtifactResource}, action) {
		if action == identity.Read {
			return "", false, artifact.ErrNotFound
		}
		return "", false, artifact.ErrDenied
	}
	return source, unlisted, nil
}

func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, pod, version, key, media string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	_, unlisted, e := s.owner(ctx, p, feed, pod, version, identity.Read)
	cancel()
	if e == nil && unlisted {
		e = artifact.ErrNotFound
	}
	pub := artifact.Publication{FeedID: feed, Coordinate: pod, Version: version, AssetKey: key}
	var record artifact.Record
	var body io.ReadCloser
	if e == nil {
		record, body, e = s.service.Open(r.Context(), p, pub, 0, -1)
	}
	if isNotFound(e) && !unlisted && key == podspecKey {
		// Only podspecs are proxied; the remote owner (or no owner) may answer.
		retry, served, pe := s.pullSpec(w, r, p, feed, pod, version)
		if served {
			return
		}
		if !retry {
			failure(w, pe)
			return
		}
		record, body, e = s.service.Open(r.Context(), p, pub, 0, -1)
	}
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

// index serves one all_pods_versions shard from the feed's package owners.
func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, want [3]string) {
	s.authorizeFeed(w, r, p, feed, func() {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		rows, e := s.db.Query(ctx, `WITH owner AS (SELECT DISTINCT ON (c.coordinate) c.id,c.source_id,c.coordinate,c.metadata FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 ORDER BY c.coordinate, fs.priority) SELECT o.coordinate,o.source_id::text,sk.kind,v.version FROM owner o JOIN sources sk ON sk.id=o.source_id AND sk.instance_id=$1 JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id AND a.asset_key='podspec.json' WHERE NOT COALESCE(o.metadata->'unlisted' @> to_jsonb(v.version),false) ORDER BY o.coordinate, v.version LIMIT 500001`, p.InstanceID, feed)
		if e != nil {
			failure(w, e)
			return
		}
		type pod struct {
			source, kind string
			versions     []string
		}
		pods := map[string]*pod{}
		count := 0
		for rows.Next() {
			var name, source, kind, version string
			if e = rows.Scan(&name, &source, &kind, &version); e != nil {
				break
			}
			count++
			if shard(name) != want {
				continue
			}
			if pods[name] == nil {
				pods[name] = &pod{source: source, kind: kind}
			}
			pods[name].versions = append(pods[name].versions, version)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil || count > 500000 {
			failure(w, artifact.ErrUnavailable)
			return
		}
		policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if e != nil {
			failure(w, artifact.ErrUnavailable)
			return
		}
		// Merge by package owner: a pod owned by a local source keeps its local
		// versions; every other name comes from upstream while it is reachable and
		// from the locally cached versions during an outage.
		remote, doc, re := s.remoteShard(ctx, p, feed, want)
		switch {
		case re == nil:
			for n, v := range pods {
				if v.kind == "remote" {
					delete(pods, n)
				}
			}
			for n, v := range remote {
				if pods[n] == nil {
					pods[n] = &pod{source: doc.SourceID, kind: "remote", versions: v}
				}
			}
			if doc.Stale {
				for k, v := range artifact.StaleHeaders() {
					w.Header().Set(k, v)
				}
			}
		case errors.Is(re, artifact.ErrUnavailable):
			if len(pods) == 0 {
				failure(w, artifact.ErrUnavailable)
				return
			}
			w.Header().Set("X-Kuvryn-Cache", "offline")
		}
		names := make([]string, 0, len(pods))
		for n, v := range pods {
			if policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: v.source, Kind: identity.ArtifactResource}, identity.Read) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			b.WriteString(n + "/" + strings.Join(pods[n].versions, "/") + "\n")
		}
		serveBytes(w, r, "text/plain; charset=utf-8", []byte(b.String()))
	})
}

func (s *Repository) remove(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, pod, version string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, _, e := s.owner(ctx, p, feed, pod, version, identity.Delete); e != nil {
		failure(w, e)
		return
	}
	// Unlist: the version disappears from the index and every read; bytes stay
	// immutable until retention/GC dereferences them.
	tag, e := s.db.Exec(ctx, `UPDATE components c SET metadata=jsonb_set(c.metadata,'{unlisted}',COALESCE(c.metadata->'unlisted','[]'::jsonb)||to_jsonb($4::text)) WHERE c.id=(SELECT c2.id FROM feed_sources fs JOIN components c2 ON c2.source_id=fs.source_id AND c2.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c2.coordinate=$3 ORDER BY fs.priority LIMIT 1) AND EXISTS(SELECT 1 FROM versions v WHERE v.component_id=c.id AND v.version=$4) AND NOT COALESCE(c.metadata->'unlisted' @> to_jsonb($4::text),false)`, p.InstanceID, feed, pod, version)
	if e != nil {
		failure(w, e)
		return
	}
	if tag.RowsAffected() == 0 {
		failure(w, artifact.ErrNotFound)
		return
	}
	w.WriteHeader(204)
}

func (s *Repository) publish(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, slug, pod, version string) {
	base, _ := s.base.Load().(string)
	if base == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	mediaType, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		failure(w, errInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	source, e := s.service.Prepare(ctx, p, feed, "", pod, podspecKey)
	if e != nil {
		failure(w, e)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	// A tombstoned version is never silently resurrected.
	if _, unlisted, oe := s.owner(ctx, p, feed, pod, version, identity.Read); oe == nil && unlisted {
		failure(w, artifact.ErrConflict)
		return
	}
	f, e := os.CreateTemp("", "ant-cocoapods-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	var specBytes []byte
	var size int64
	var sum []byte
	claimed := ""
	mr := multipart.NewReader(contextReader{ctx, r.Body}, params["boundary"])
	for {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			failure(w, errInvalid)
			return
		}
		switch part.FormName() {
		case "podspec":
			specBytes, e = io.ReadAll(io.LimitReader(part, maxPodspec+1))
			if e != nil || len(specBytes) > maxPodspec {
				failure(w, errInvalid)
				return
			}
		case "sha256":
			b, _ := io.ReadAll(io.LimitReader(part, 129))
			claimed = strings.ToLower(strings.TrimSpace(string(b)))
		case "source":
			if size != 0 {
				failure(w, errInvalid)
				return
			}
			h := sha256.New()
			n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(part, maxArchive+1), make([]byte, 64<<10))
			if e != nil || n > maxArchive {
				failure(w, errInvalid)
				return
			}
			size, sum = n, h.Sum(nil)
		}
	}
	if size == 0 || specBytes == nil {
		failure(w, errInvalid)
		return
	}
	digest := hex.EncodeToString(sum)
	if claimed != "" && claimed != digest {
		failure(w, blob.ErrIntegrity)
		return
	}
	head := make([]byte, 4)
	if _, e = f.ReadAt(head, 0); e != nil && !errors.Is(e, io.EOF) {
		failure(w, e)
		return
	}
	ext, typ, ok := archiveKind(head)
	if !ok {
		failure(w, errInvalid)
		return
	}
	if e = validateArchive(ctx, f, size, typ); e != nil {
		failure(w, errInvalid)
		return
	}
	spec, e := parsePodspec(specBytes, pod, version)
	if e != nil {
		failure(w, e)
		return
	}
	url := fmt.Sprintf("%s/feeds/%s/cocoapods/archives/%s/%s/%s-%s.%s", base, slug, pod, version, pod, version, ext)
	spec["source"] = map[string]any{"http": url, "type": typ, "sha256": digest}
	out, e := json.Marshal(spec)
	if e != nil {
		failure(w, errInvalid)
		return
	}
	mt := "application/gzip"
	if ext == "zip" {
		mt = "application/zip"
	}
	archive := artifact.Publication{FeedID: feed, Coordinate: pod, Version: version, AssetKey: "source." + ext, MediaType: mt, ExpectedSource: source, Info: blob.Info{Digest: blob.Digest(digest), Size: size}}
	hs := sha256.Sum256(out)
	meta := artifact.Publication{FeedID: feed, Coordinate: pod, Version: version, AssetKey: podspecKey, MediaType: "application/json", ExpectedSource: source, Info: blob.Info{Digest: blob.Digest(hex.EncodeToString(hs[:])), Size: int64(len(out))}}
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	_, e = s.service.PublishBundle(ctx, p, []artifact.Publication{archive, meta}, []io.Reader{contextReader{ctx, f}, strings.NewReader(string(out))})
	if e != nil {
		failure(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(map[string]string{"name": pod, "version": version, "sha256": digest, "url": url})
}
