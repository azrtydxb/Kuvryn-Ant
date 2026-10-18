package aptrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Core mapping (all in the feed's resolved local source):
//
//	pool/{component}/{package}      versions "{version-no-epoch}_{arch}"  assets deb, control
//	by-hash/{dist}                  version = sha256 hex of an index file  asset blob
//	dists/{dist}                    version = zero-padded generation       assets Release, InRelease, Release.gpg
//
// The Release generation bundle is the single atomic switch: indexes are
// published first by content hash and stay reachable forever, so readers that
// hold an older Release can always complete their by-hash downloads.
const (
	nsPool  = "pool"
	nsDists = "dists"
)

var hexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func poolCoord(comp, pkg string) string { return "pool/" + comp + "/" + pkg }
func distCoord(dist string) string      { return "dists/" + dist }
func hashCoord(dist string) string      { return "by-hash/" + dist }

// readAsset reads a bounded asset fully and re-verifies its recorded digest.
func (s *Repository) readAsset(ctx context.Context, p identity.Principal, pub artifact.Publication, limit int64) ([]byte, artifact.Record, error) {
	rec, body, e := s.service.Open(ctx, p, pub, 0, -1)
	if e != nil {
		return nil, rec, e
	}
	defer body.Close()
	if rec.Info.Size > limit {
		return nil, rec, errCorrupt
	}
	b, e := io.ReadAll(io.LimitReader(contextReader{ctx, body}, limit+1))
	if e != nil {
		return nil, rec, e
	}
	if int64(len(b)) != rec.Info.Size || sum(b) != string(rec.Info.Digest) {
		return nil, rec, errCorrupt
	}
	return b, rec, nil
}

// currentGeneration returns the newest committed Release generation of a dist
// as owned by the first feed source holding the coordinate.
func (s *Repository) currentGeneration(ctx context.Context, feed, dist string) (string, error) {
	var version string
	e := s.db.QueryRow(ctx, `WITH owner AS (SELECT c.id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT v.version FROM owner o JOIN versions v ON v.component_id=o.id WHERE o.namespace=$4 ORDER BY v.version DESC LIMIT 1`, s.instance, feed, distCoord(dist), nsDists).Scan(&version)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", artifact.ErrNotFound
	}
	if e != nil {
		return "", artifact.ErrUnavailable
	}
	return version, nil
}

func (s *Repository) release(ctx context.Context, p identity.Principal, feed, dist, name string) ([]byte, string, error) {
	gen, e := s.currentGeneration(ctx, feed, dist)
	if e != nil {
		return nil, "", e
	}
	b, _, e := s.readAsset(ctx, p, artifact.Publication{FeedID: feed, Namespace: nsDists, Coordinate: distCoord(dist), Version: gen, AssetKey: name}, 4<<20)
	return b, gen, e
}

func (s *Repository) hashed(ctx context.Context, p identity.Principal, feed, dist, hash string) ([]byte, error) {
	b, _, e := s.readAsset(ctx, p, artifact.Publication{FeedID: feed, Namespace: nsDists, Coordinate: hashCoord(dist), Version: hash, AssetKey: "blob"}, maxIndex)
	if e == nil && sum(b) != hash {
		e = errCorrupt
	}
	return b, e
}

// dist serves Release, InRelease, Release.gpg, Packages[.gz] and by-hash paths.
func (s *Repository) dist(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, dist string, rest []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var data []byte
	var media string
	var e error
	switch {
	case len(rest) == 1 && (rest[0] == "Release" || rest[0] == "InRelease" || rest[0] == "Release.gpg"):
		media = "text/plain; charset=utf-8"
		if rest[0] == "Release.gpg" {
			media = "application/pgp-signature"
		}
		data, _, e = s.release(ctx, p, feed, dist, rest[0])
	case len(rest) == 3 && namePattern.MatchString(rest[0]) && strings.HasPrefix(rest[1], "binary-") && (rest[2] == "Packages" || rest[2] == "Packages.gz"):
		var rel []byte
		if rel, _, e = s.release(ctx, p, feed, dist, "Release"); e != nil {
			break
		}
		var files map[string]releaseFile
		if files, e = parseRelease(rel); e != nil {
			break
		}
		f, ok := files[strings.Join(rest, "/")]
		if !ok {
			e = artifact.ErrNotFound
			break
		}
		if data, e = s.hashed(ctx, p, feed, dist, f.sha); e == nil && int64(len(data)) != f.size {
			e = errCorrupt
		}
		media = "text/plain; charset=utf-8"
		if rest[2] == "Packages.gz" {
			media = "application/gzip"
		}
	case len(rest) == 5 && rest[2] == "by-hash" && rest[3] == "SHA256" && hexPattern.MatchString(rest[4]) && namePattern.MatchString(rest[0]) && strings.HasPrefix(rest[1], "binary-"):
		media = "application/octet-stream"
		data, e = s.hashed(ctx, p, feed, dist, rest[4])
	default:
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("ETag", `"`+sum(data)+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (s *Repository) key(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, binary bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
		failure(w, artifact.ErrNotFound)
		return
	}
	entity, e := s.signingKey(ctx, p, feed, false)
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	data, e := publicKey(entity, binary)
	if e != nil {
		failure(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/pgp-keys")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// pool dispatches GET/HEAD downloads, PUT ingestion and DELETE unlisting.
func (s *Repository) pool(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, comp, dir, file string) {
	stem, isDeb := strings.CutSuffix(file, ".deb")
	parts := strings.Split(stem, "_")
	if !isDeb || len(parts) != 3 || !validPackage(parts[0]) || !validVersion(parts[1]) || strings.Contains(parts[1], ":") || !archPattern.MatchString(parts[2]) {
		failure(w, artifact.ErrNotFound)
		return
	}
	filename := "pool/" + comp + "/" + dir + "/" + file
	pub := artifact.Publication{FeedID: feed, Namespace: nsPool, Coordinate: poolCoord(comp, parts[0]), Version: parts[1] + "_" + parts[2], AssetKey: "deb", MediaType: "application/vnd.debian.binary-package"}
	switch r.Method {
	case "GET", "HEAD":
		s.download(w, r, p, pub, filename)
	case "PUT":
		s.ingest(w, r, p, pub, comp, filename)
	case "DELETE":
		s.unlist(w, r, p, pub, comp, filename)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		w.WriteHeader(405)
	}
}

func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, filename string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	meta := pub
	meta.AssetKey = "control"
	ctl, _, e := s.readAsset(ctx, p, meta, maxControlFl+4096)
	cancel()
	if e != nil {
		failure(w, e)
		return
	}
	x, e := parseEntry(string(ctl))
	if e != nil || x.filename != filename {
		failure(w, artifact.ErrNotFound) // non-canonical path or corrupt metadata
		return
	}
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

func distParam(r *http.Request) (string, bool) {
	d := r.URL.Query().Get("distribution")
	return d, namePattern.MatchString(d) && len(r.URL.Query()) == 1
}

// loadState reconstructs a distribution's package set from its current signed
// Release and the by-hash indexes it names, verifying every digest.
func (s *Repository) loadState(ctx context.Context, p identity.Principal, feed, dist string) (state, int64, error) {
	st := state{}
	gen, e := s.currentGeneration(ctx, feed, dist)
	if errors.Is(e, artifact.ErrNotFound) {
		return st, 0, nil
	}
	if e != nil {
		return nil, 0, e
	}
	seq, e := strconv.ParseInt(gen, 10, 64)
	if e != nil {
		return nil, 0, errCorrupt
	}
	rel, _, e := s.release(ctx, p, feed, dist, "Release")
	if e != nil {
		return nil, 0, e
	}
	files, e := parseRelease(rel)
	if e != nil {
		return nil, 0, e
	}
	for path, f := range files {
		comp, rest, ok := strings.Cut(path, "/binary-")
		if !ok || !strings.HasSuffix(rest, "/Packages") || !namePattern.MatchString(comp) {
			continue
		}
		data, e := s.hashed(ctx, p, feed, dist, f.sha)
		if e != nil {
			return nil, 0, e
		}
		list, e := parseEntries(data)
		if e != nil {
			return nil, 0, e
		}
		if st[comp] == nil {
			st[comp] = map[string]entry{}
		}
		for _, x := range list {
			if old, dup := st[comp][x.filename]; dup && old.checksum != x.checksum {
				return nil, 0, errCorrupt
			}
			st[comp][x.filename] = x
		}
		if st.count() > maxStanzas {
			return nil, 0, errCorrupt
		}
	}
	return st, seq, nil
}

// sources resolves and cross-checks the single owning source for every
// coordinate this operation touches; disagreement fails closed.
func (s *Repository) sources(ctx context.Context, p identity.Principal, feed, dist string, pool *artifact.Publication) (string, error) {
	source, e := s.service.Prepare(ctx, p, feed, nsDists, distCoord(dist), "Release")
	if e != nil {
		return "", e
	}
	other, e := s.service.Prepare(ctx, p, feed, nsDists, hashCoord(dist), "blob")
	if e != nil {
		return "", e
	}
	if other != source {
		return "", artifact.ErrConflict
	}
	if pool != nil {
		if other, e = s.service.Prepare(ctx, p, feed, nsPool, pool.Coordinate, pool.AssetKey); e != nil {
			return "", e
		}
		if other != source {
			return "", artifact.ErrConflict
		}
	}
	return source, nil
}

// commit signs and atomically publishes generation seq+1 of dist for st.
func (s *Repository) commit(ctx context.Context, p identity.Principal, feed, dist, source string, st state, seq int64) error {
	entity, e := s.signingKey(ctx, p, feed, true)
	if e != nil {
		return e
	}
	release, files := buildGeneration(dist, st, s.now(), s.validity)
	inRelease, detached, e := s.signRelease(entity, release)
	if e != nil {
		return e
	}
	for _, f := range files {
		pub := artifact.Publication{FeedID: feed, Namespace: nsDists, Coordinate: hashCoord(dist), Version: f.sha, AssetKey: "blob", MediaType: f.media, ExpectedSource: source, Info: info(f.data)}
		if _, e = s.service.Publish(ctx, p, pub, bytes.NewReader(f.data)); e != nil {
			return e
		}
	}
	version := fmt.Sprintf("%012d", seq+1)
	base := artifact.Publication{FeedID: feed, Namespace: nsDists, Coordinate: distCoord(dist), Version: version, ExpectedSource: source}
	pubs := make([]artifact.Publication, 3)
	bodies := make([]io.Reader, 3)
	for i, a := range []struct {
		key, media string
		data       []byte
	}{{"Release", "text/plain", release}, {"InRelease", "text/plain", inRelease}, {"Release.gpg", "application/pgp-signature", detached}} {
		pubs[i] = base
		pubs[i].AssetKey, pubs[i].MediaType, pubs[i].Info = a.key, a.media, info(a.data)
		bodies[i] = bytes.NewReader(a.data)
	}
	_, e = s.service.PublishBundle(ctx, p, pubs, bodies)
	return e
}

func info(b []byte) blob.Info { return blob.Info{Size: int64(len(b)), Digest: blob.Digest(sum(b))} }
