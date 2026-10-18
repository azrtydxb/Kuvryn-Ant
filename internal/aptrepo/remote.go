package aptrepo

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/ulikunitz/xz"
)

// Pull-through for APT. Trust model:
//
//   - Release, InRelease and Release.gpg are served verbatim from upstream
//     (through the runtime's metadata cache, with stale-if-error). They are never
//     rewritten, merged or re-signed: the client verifies the upstream
//     signature itself with the upstream archive key (signed-by), so this feed
//     is not part of the trust chain for what apt accepts.
//   - Every other file under dists/ (Packages*, Contents, Translations, ...)
//     is served only if the upstream Release lists it. It is fetched by hash
//     when upstream offers Acquire-By-Hash, verified against the SHA256 of the
//     Release before the first byte is served, and cached as an immutable
//     content-addressed asset. A different Release can never replace it.
//   - A .deb is served only if a Packages index of a distribution this feed
//     already served lists its pool path; its SHA256 from that index is
//     verified before the bytes are served and cached.
//   - Upstream URLs need no rewriting: indexes carry pool paths relative to the
//     repository root, which resolve against the feed root.
//   - A distribution whose Release is owned by a local source is never
//     answered from upstream.
//
// The Release signature is not verified here (the operator's upstream key is
// not configured); integrity of upstream bytes therefore rests on TLS to the
// source plus the clients' own signature checks.

const nsRemote = "apt-remote"

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,127}$`)

type relEntry struct {
	sha  string
	size int64
}

type relInfo struct {
	files  map[string]relEntry
	archs  []string
	byHash bool
}

// parseRemoteRelease reads the SHA256 section of a Release or clear-signed
// InRelease. Unlike parseRelease it tolerates large entries and ignores
// malformed lines.
func parseRemoteRelease(b []byte) relInfo {
	info := relInfo{files: map[string]relEntry{}}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "-----BEGIN PGP SIGNATURE"):
			return info
		case line == "SHA256:":
			in = true
		case in && strings.HasPrefix(line, " "):
			f := strings.Fields(line)
			if len(f) != 3 || !hexPattern.MatchString(f[0]) {
				continue
			}
			if n, e := strconv.ParseInt(f[1], 10, 64); e == nil && n >= 0 {
				info.files[f[2]] = relEntry{f[0], n}
			}
		default:
			in = false
			if v, ok := strings.CutPrefix(line, "Architectures:"); ok {
				info.archs = strings.Fields(v)
			}
			if v, ok := strings.CutPrefix(line, "Acquire-By-Hash:"); ok {
				info.byHash = strings.EqualFold(strings.TrimSpace(v), "yes")
			}
		}
	}
	return info
}

func (s *Repository) remoteDoc(ctx context.Context, p identity.Principal, feed, dist, name string) (artifact.Document, error) {
	return s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Namespace: nsDists, Coordinate: distCoord(dist)}, artifact.Origin{Path: "dists/" + dist + "/" + name})
}

// remoteRelease returns the parsed upstream Release listing of dist, preferring
// Release and falling back to the clear-signed InRelease.
func (s *Repository) remoteRelease(ctx context.Context, p identity.Principal, feed, dist string) (relInfo, artifact.Document, error) {
	doc, err := s.remoteDoc(ctx, p, feed, dist, "Release")
	if errors.Is(err, artifact.ErrNotFound) {
		doc, err = s.remoteDoc(ctx, p, feed, dist, "InRelease")
	}
	if err != nil {
		return relInfo{}, doc, err
	}
	return parseRemoteRelease(doc.Body), doc, nil
}

// remoteDist answers dists/{dist}/{rest...}. It returns true once the response
// was written; the error is a degraded upstream failure (local fallback).
func (s *Repository) remoteDist(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, dist string, rest []string) (bool, error) {
	if r.Method != "GET" && r.Method != "HEAD" {
		return false, nil
	}
	for _, seg := range rest {
		if !segmentPattern.MatchString(seg) {
			return false, nil
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if len(rest) == 1 && (rest[0] == "Release" || rest[0] == "InRelease" || rest[0] == "Release.gpg") {
		doc, err := s.remoteDoc(ctx, p, feed, dist, rest[0])
		switch {
		case errors.Is(err, artifact.ErrNotFound):
			return false, nil
		case errors.Is(err, artifact.ErrUnavailable):
			return false, err
		case err != nil:
			failure(w, err)
			return true, nil
		}
		media := "text/plain; charset=utf-8"
		if rest[0] == "Release.gpg" {
			media = "application/pgp-signature"
		}
		w.Header().Set("Content-Type", media)
		w.Header().Set("ETag", `"`+sum(doc.Body)+`"`)
		if doc.Stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(doc.Body))
		return true, nil
	}
	if len(rest) < 2 {
		return false, nil
	}
	rel, doc, err := s.remoteRelease(ctx, p, feed, dist)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	file := strings.Join(rest, "/")
	entry, ok := rel.files[file]
	paths := []string{file}
	if i := strings.Index(file, "/by-hash/SHA256/"); i > 0 {
		hash := file[i+len("/by-hash/SHA256/"):]
		dir := file[:i]
		ok = false
		for listed, e := range rel.files {
			if e.sha == hash && path.Dir(listed) == dir {
				entry, ok = e, true
				paths = []string{file, listed}
				break
			}
		}
	} else if ok && rel.byHash {
		paths = []string{path.Dir(file) + "/by-hash/SHA256/" + entry.sha, file}
	}
	if !ok {
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	rec, body, err := s.openIndex(ctx, p, feed, dist, doc.SourceID, entry.sha, paths)
	if err != nil {
		failure(w, err)
		return true, nil
	}
	defer body.Close()
	if rec.Info.Size != entry.size {
		failure(w, errCorrupt)
		return true, nil
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(rec.Info.Size, 10))
	w.Header().Set("ETag", `"`+entry.sha+`"`)
	w.Header().Set("X-Kuvryn-Cache", "hit")
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
	return true, nil
}

func indexPub(feed, dist, sha string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: nsRemote, Coordinate: "index/" + dist, Version: sha, AssetKey: "blob", MediaType: "application/octet-stream"}
}

// openIndex returns the verified, content-addressed copy of an upstream index
// file, fetching it from the first of paths that exists upstream.
func (s *Repository) openIndex(ctx context.Context, p identity.Principal, feed, dist, source, sha string, paths []string) (artifact.Record, io.ReadCloser, error) {
	pub := indexPub(feed, dist, sha)
	rec, body, err := s.service.Open(ctx, p, pub, 0, -1)
	if err == nil || !errors.Is(err, artifact.ErrNotFound) {
		return rec, body, err
	}
	err = artifact.ErrNotFound
	for _, rel := range paths {
		m, again, e := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: artifact.EscapePath("dists/" + dist + "/" + rel), SourceID: source, Digest: blob.Digest(sha)})
		if e != nil {
			if errors.Is(e, artifact.ErrNotFound) {
				continue
			}
			return rec, nil, e
		}
		if m != nil && !again { // unreachable with a digest; never leak the stream
			_ = m.Body.Close()
			return rec, nil, artifact.ErrUnavailable
		}
		return s.service.Open(ctx, p, pub, 0, -1)
	}
	return rec, nil, err
}

// remoteDists lists the distributions whose indexes this feed has cached.
func (s *Repository) remoteDists(ctx context.Context, p identity.Principal, feed string) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT substr(c.coordinate,7) FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.namespace=$3 AND c.coordinate LIKE 'index/%' ORDER BY 1`, p.InstanceID, feed, nsRemote)
	if err != nil {
		return nil, artifact.ErrUnavailable
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			return nil, artifact.ErrUnavailable
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		return nil, artifact.ErrUnavailable
	}
	return out, nil
}

type pkgSum struct {
	sha  [32]byte
	size int64
}

var (
	pkgCacheMu sync.Mutex
	pkgCache   []pkgCacheEntry
)

type pkgCacheEntry struct {
	key   string
	items map[string]pkgSum
}

func pkgCacheGet(key string) map[string]pkgSum {
	pkgCacheMu.Lock()
	defer pkgCacheMu.Unlock()
	for _, e := range pkgCache {
		if e.key == key {
			return e.items
		}
	}
	return nil
}

func pkgCachePut(key string, items map[string]pkgSum) {
	pkgCacheMu.Lock()
	defer pkgCacheMu.Unlock()
	pkgCache = append(pkgCache, pkgCacheEntry{key, items})
	if len(pkgCache) > 3 {
		pkgCache = pkgCache[1:]
	}
}

const maxPackagesInflated = 1 << 30

// scanPackages indexes Filename -> (sha256, size) of a Packages file.
func scanPackages(r io.Reader, compression string) (map[string]pkgSum, error) {
	switch compression {
	case ".gz":
		g, err := gzip.NewReader(r)
		if err != nil {
			return nil, errCorrupt
		}
		defer g.Close()
		r = g
	case ".xz":
		x, err := xz.NewReader(r)
		if err != nil {
			return nil, errCorrupt
		}
		r = x
	case ".bz2":
		r = bzip2.NewReader(r)
	}
	sc := bufio.NewScanner(io.LimitReader(r, maxPackagesInflated))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	items := map[string]pkgSum{}
	var filename, shaHex string
	var size int64
	flush := func() {
		if filename != "" && hexPattern.MatchString(shaHex) {
			var s pkgSum
			if b, e := hex.DecodeString(shaHex); e == nil {
				copy(s.sha[:], b)
				s.size = size
				items[filename] = s
			}
		}
		filename, shaHex, size = "", "", 0
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Filename: "):
			filename = strings.TrimSpace(line[len("Filename: "):])
		case strings.HasPrefix(line, "SHA256: "):
			shaHex = strings.TrimSpace(line[len("SHA256: "):])
		case strings.HasPrefix(line, "Size: "):
			size, _ = strconv.ParseInt(strings.TrimSpace(line[len("Size: "):]), 10, 64)
		}
	}
	flush()
	if sc.Err() != nil {
		return nil, errCorrupt
	}
	return items, nil
}

// findPackage resolves the SHA256 and size of a pool file from the Packages
// indexes of the distributions this feed has already served.
func (s *Repository) findPackage(ctx context.Context, p identity.Principal, feed, comp, filename, arch string) (pkgSum, string, error) {
	dists, err := s.remoteDists(ctx, p, feed)
	if err != nil {
		return pkgSum{}, "", err
	}
	var unavailable error
	for _, dist := range dists {
		rel, doc, err := s.remoteRelease(ctx, p, feed, dist)
		if err != nil {
			if errors.Is(err, artifact.ErrNotFound) {
				continue // local owner or gone upstream
			}
			unavailable = err
			continue
		}
		archs := []string{arch}
		if arch == "all" {
			archs = rel.archs
		}
		for _, a := range archs {
			for _, ext := range []string{".gz", ".xz", "", ".bz2"} {
				name := comp + "/binary-" + a + "/Packages" + ext
				entry, ok := rel.files[name]
				if !ok {
					continue
				}
				items := pkgCacheGet(entry.sha)
				if items == nil {
					paths := []string{name}
					if rel.byHash {
						paths = []string{path.Dir(name) + "/by-hash/SHA256/" + entry.sha, name}
					}
					_, body, e := s.openIndex(ctx, p, feed, dist, doc.SourceID, entry.sha, paths)
					if e != nil {
						unavailable = e
						continue
					}
					items, e = scanPackages(body, ext)
					_ = body.Close()
					if e != nil {
						unavailable = e
						continue
					}
					pkgCachePut(entry.sha, items)
				}
				if sum, ok := items[filename]; ok {
					return sum, doc.SourceID, nil
				}
				break
			}
		}
	}
	if unavailable != nil {
		return pkgSum{}, "", artifact.ErrUnavailable
	}
	return pkgSum{}, "", artifact.ErrNotFound
}

// remoteDeb materializes a missing pool file. retry=true means the asset (and a
// synthesized control record) is now in the store.
func (s *Repository) remoteDeb(ctx context.Context, p identity.Principal, pub artifact.Publication, comp, filename string) (bool, error) {
	ver, arch, _ := strings.Cut(pub.Version, "_")
	pkg := strings.TrimPrefix(pub.Coordinate, "pool/"+comp+"/")
	sumv, source, err := s.findPackage(ctx, p, pub.FeedID, comp, filename, arch)
	if err != nil {
		return false, err
	}
	hexSum := hex.EncodeToString(sumv.sha[:])
	m, again, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: artifact.EscapePath(filename), SourceID: source, Digest: blob.Digest(hexSum)})
	if err != nil {
		return false, err
	}
	if m != nil && !again {
		_ = m.Body.Close()
		return false, artifact.ErrUnavailable
	}
	// The control record binds the pool path to the verified bytes for the
	// local read path. It is synthesized from the verified index fields.
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nFilename: %s\nSize: %d\nSHA256: %s\n", pkg, ver, arch, filename, sumv.size, hexSum)
	cp := pub
	cp.AssetKey, cp.MediaType = "control", "text/plain"
	if err = s.service.CacheDocument(ctx, p, cp, source, []byte(control)); err != nil {
		return false, err
	}
	return true, nil
}
