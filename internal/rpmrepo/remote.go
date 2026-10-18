package rpmrepo

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
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

// Pull-through for RPM-MD repositories. Trust model:
//
//   - repomd.xml, repomd.xml.asc and repomd.xml.key are served verbatim from
//     upstream (runtime metadata cache, stale-if-error). They are never
//     rewritten, merged or re-signed, so the client verifies upstream's
//     signature itself (repo_gpgcheck) and package signatures (gpgcheck) are
//     inside the untouched RPMs.
//   - Every other repodata file is served only if the upstream repomd.xml lists
//     it with a sha256 checksum; it is fetched, verified against that checksum
//     before the first byte is served and cached content-addressed.
//   - A package path (any depth, as written in primary.xml location hrefs,
//     usually Packages/<letter>/<file>.rpm) is served only if the upstream
//     primary index lists it with a sha256 checksum; the package bytes are
//     verified against it before being served and cached.
//   - Location hrefs are relative to the repository root, which is the feed
//     root, so nothing needs rewriting.
//   - A repository owned by a local source (any local RPM) is never answered
//     from upstream.
//
// Limitations: zstd-compressed primary.xml (Fedora) is not decodable; checksums
// other than sha256 cannot be verified and are refused; the repomd signature
// itself is not verified by this server.

const (
	nsRemote         = "rpm-remote"
	remoteCoordinate = "repodata/remote"
)

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+~^-]{0,255}$`)

type remoteData struct {
	Type     string `xml:"type,attr"`
	Checksum struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Size int64 `xml:"size"`
}

func parseRepomd(b []byte) (map[string]remoteData, string, error) {
	var doc struct {
		Data []remoteData `xml:"data"`
	}
	if xml.Unmarshal(b, &doc) != nil {
		return nil, "", errMalformed
	}
	byHref := map[string]remoteData{}
	primary := ""
	for _, d := range doc.Data {
		href := d.Location.Href
		if !strings.HasPrefix(href, "repodata/") || strings.Contains(href, "..") || strings.Count(href, "/") != 1 {
			continue
		}
		d.Checksum.Value = strings.TrimSpace(d.Checksum.Value)
		if d.Checksum.Type != "sha256" || !sha256Hex.MatchString(d.Checksum.Value) || !segmentPattern.MatchString(href[len("repodata/"):]) {
			continue
		}
		byHref[href] = d
		if d.Type == "primary" {
			primary = href
		}
	}
	return byHref, primary, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *Repository) remoteMeta(ctx context.Context, p identity.Principal, feed, file string) (artifact.Document, error) {
	return s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: snapshotCoordinate}, artifact.Origin{Path: "repodata/" + file})
}

func repoPub(feed, sha string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: nsRemote, Coordinate: remoteCoordinate, Version: sha, AssetKey: "blob", MediaType: "application/octet-stream"}
}

// openRepodata returns the verified content-addressed copy of a repodata file.
func (s *Repository) openRepodata(ctx context.Context, p identity.Principal, feed, source, href, sha string) (artifact.Record, io.ReadCloser, error) {
	pub := repoPub(feed, sha)
	rec, body, err := s.service.Open(ctx, p, pub, 0, -1)
	if err == nil || !errors.Is(err, artifact.ErrNotFound) {
		return rec, body, err
	}
	m, again, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: artifact.EscapePath(href), SourceID: source, Digest: blob.Digest(sha)})
	if err != nil {
		return rec, nil, err
	}
	if m != nil && !again {
		_ = m.Body.Close()
		return rec, nil, artifact.ErrUnavailable
	}
	return s.service.Open(ctx, p, pub, 0, -1)
}

// remoteRepodata answers GET/HEAD repodata/{file}. It returns true once the
// response was written; the error is a degraded upstream failure.
func (s *Repository) remoteRepodata(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, file string) (bool, error) {
	if !segmentPattern.MatchString(file) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	doc, err := s.remoteMeta(ctx, p, feed, "repomd.xml")
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	switch file {
	case "repomd.xml", "repomd.xml.asc", "repomd.xml.key":
		body, media := doc.Body, "application/xml"
		if file != "repomd.xml" {
			sub, err := s.remoteMeta(ctx, p, feed, file)
			switch {
			case errors.Is(err, artifact.ErrNotFound):
				failure(w, artifact.ErrNotFound)
				return true, nil
			case err != nil:
				failure(w, err)
				return true, nil
			}
			body, media, doc.Stale = sub.Body, "application/pgp-signature", doc.Stale || sub.Stale
			if file == "repomd.xml.key" {
				media = "application/pgp-keys"
			}
		}
		w.Header().Set("Content-Type", media)
		w.Header().Set("ETag", `"`+hexSum(body)+`"`)
		if doc.Stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		return true, nil
	}
	files, _, err := parseRepomd(doc.Body)
	if err != nil {
		failure(w, artifact.ErrUnavailable)
		return true, nil
	}
	entry, ok := files["repodata/"+file]
	if !ok {
		failure(w, artifact.ErrNotFound)
		return true, nil
	}
	rec, body, err := s.openRepodata(ctx, p, feed, doc.SourceID, "repodata/"+file, entry.Checksum.Value)
	if err != nil {
		failure(w, err)
		return true, nil
	}
	defer body.Close()
	if entry.Size > 0 && rec.Info.Size != entry.Size {
		failure(w, errMalformed)
		return true, nil
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(rec.Info.Size, 10))
	w.Header().Set("ETag", `"`+entry.Checksum.Value+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
	return true, nil
}

type pkgSum struct {
	sha  [32]byte
	size int64
}

var (
	cacheMu sync.Mutex
	cache   []cacheEntry
)

type cacheEntry struct {
	key   string
	items map[string]pkgSum
}

func cacheGet(key string) map[string]pkgSum {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for _, e := range cache {
		if e.key == key {
			return e.items
		}
	}
	return nil
}

func cachePut(key string, items map[string]pkgSum) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = append(cache, cacheEntry{key, items})
	if len(cache) > 3 {
		cache = cache[1:]
	}
}

const maxPrimaryInflated = 1 << 30

// scanPrimary maps every package location href to its sha256 and size.
func scanPrimary(r io.Reader, href string) (map[string]pkgSum, error) {
	switch {
	case strings.HasSuffix(href, ".gz"):
		g, err := gzip.NewReader(r)
		if err != nil {
			return nil, errMalformed
		}
		defer g.Close()
		r = g
	case strings.HasSuffix(href, ".xz"):
		x, err := xz.NewReader(r)
		if err != nil {
			return nil, errMalformed
		}
		r = x
	case strings.HasSuffix(href, ".bz2"):
		r = bzip2.NewReader(r)
	case strings.HasSuffix(href, ".xml"):
	default:
		return nil, artifact.ErrUnavailable // zstd and unknown encodings
	}
	dec := xml.NewDecoder(io.LimitReader(r, maxPrimaryInflated))
	items := map[string]pkgSum{}
	var sum pkgSum
	var haveSum bool
	var location string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errMalformed
		}
		start, ok := tok.(xml.StartElement)
		if ok {
			switch start.Name.Local {
			case "package":
				sum, haveSum, location = pkgSum{}, false, ""
			case "checksum":
				var typ string
				for _, a := range start.Attr {
					if a.Name.Local == "type" {
						typ = a.Value
					}
				}
				var text string
				if e := dec.DecodeElement(&text, &start); e != nil {
					return nil, errMalformed
				}
				if b, e := hex.DecodeString(strings.TrimSpace(text)); typ == "sha256" && e == nil && len(b) == 32 {
					copy(sum.sha[:], b)
					haveSum = true
				}
			case "size":
				for _, a := range start.Attr {
					if a.Name.Local == "package" {
						sum.size, _ = strconv.ParseInt(a.Value, 10, 64)
					}
				}
			case "location":
				for _, a := range start.Attr {
					if a.Name.Local == "href" {
						location = a.Value
					}
				}
			}
			continue
		}
		if end, ok := tok.(xml.EndElement); ok && end.Name.Local == "package" && haveSum && location != "" {
			items[location] = sum
		}
	}
	return items, nil
}

// remotePackageSum looks up path in the upstream primary index.
func (s *Repository) remotePackageSum(ctx context.Context, p identity.Principal, feed, path string) (pkgSum, string, error) {
	doc, err := s.remoteMeta(ctx, p, feed, "repomd.xml")
	if err != nil {
		return pkgSum{}, "", err
	}
	files, primary, err := parseRepomd(doc.Body)
	if err != nil || primary == "" {
		return pkgSum{}, "", artifact.ErrUnavailable
	}
	sha := files[primary].Checksum.Value
	items := cacheGet(sha)
	if items == nil {
		_, body, err := s.openRepodata(ctx, p, feed, doc.SourceID, primary, sha)
		if err != nil {
			return pkgSum{}, "", err
		}
		items, err = scanPrimary(body, primary)
		_ = body.Close()
		if err != nil {
			return pkgSum{}, "", err
		}
		cachePut(sha, items)
	}
	sum, ok := items[path]
	if !ok {
		return pkgSum{}, "", artifact.ErrNotFound
	}
	return sum, doc.SourceID, nil
}

// remotePackage serves a package path from upstream. It returns true once the
// response was written. Not-found and degraded errors are returned so a flat
// local route can still answer.
func (s *Repository) remotePackage(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, rest []string, n nevra) (bool, error) {
	for _, seg := range rest {
		if !segmentPattern.MatchString(seg) {
			return false, artifact.ErrNotFound
		}
	}
	href := strings.Join(rest, "/")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	pub := n.pub(feed, packageAsset)
	pub.Namespace, pub.MediaType = nsRemote, "application/x-rpm"
	// Package-owner semantics apply to cache hits as well as misses.
	if !s.service.HasRemote(ctx, p, pub) {
		return false, artifact.ErrNotFound
	}
	record, body, err := s.service.Open(ctx, p, pub, 0, -1)
	if errors.Is(err, artifact.ErrNotFound) {
		sum, source, e := s.remotePackageSum(ctx, p, feed, href)
		if e != nil {
			return false, e
		}
		m, again, e := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: artifact.EscapePath(href), SourceID: source, Digest: blob.Digest(hex.EncodeToString(sum.sha[:]))})
		if e != nil {
			return false, e
		}
		if m != nil && !again {
			_ = m.Body.Close()
			return false, artifact.ErrUnavailable
		}
		record, body, err = s.service.Open(ctx, p, pub, 0, -1)
	}
	if err != nil {
		return false, err
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/x-rpm")
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
	return true, nil
}
