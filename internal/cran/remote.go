package cran

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for CRAN style upstreams (cloud.r-project.org).
//
// Ownership is per package name. Locally ingested packages live under the fixed
// channel component (see repository.go); anything cached from upstream is stored
// under its own coordinate (the package name for source packages, the channel
// directory plus name for binaries), so cached upstream bytes never become part
// of the local snapshot. A package name that exists locally is never fetched from
// upstream (no dependency confusion), and the merged PACKAGES index takes each
// name from exactly one side.
//
// Served from upstream:
//   - src/contrib/PACKAGES, PACKAGES.gz: upstream PACKAGES.gz is read through the
//     metadata cache, merged with the locally generated index by package name.
//     PACKAGES.rds is not offered; R falls back to PACKAGES.gz.
//   - src/contrib/<pkg>_<ver>.tar.gz and src/contrib/Archive/<pkg>/<pkg>_<ver>.tar.gz
//     share one cache entry per package version.
//   - bin/<platform>/contrib/<rver>/PACKAGES[.gz] verbatim and the binary files
//     (<pkg>_<ver>.zip|.tgz), cached on first download.
//
// CRAN publishes only MD5 sums, which the seam cannot verify, so fills are
// length checked but not hash verified.

const (
	maxRemotePackages = 100000
	maxRemoteIndex    = 128 << 20
	maxParagraph      = 64 << 10
)

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type remoteBlock struct {
	name, text string
}

func isNotFound(err error) bool { return errors.Is(err, artifact.ErrNotFound) }

// remotePackages reads and validates upstream src/contrib/PACKAGES.gz.
func (s *Repository) remotePackages(ctx context.Context, p identity.Principal, feed string) ([]remoteBlock, artifact.Document, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: "index:" + channel}, artifact.Origin{Path: channel + "/PACKAGES.gz"})
	if err != nil {
		return nil, doc, err
	}
	g, e := gzip.NewReader(bytes.NewReader(doc.Body))
	if e != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	defer g.Close()
	raw, e := io.ReadAll(io.LimitReader(g, maxRemoteIndex+1))
	if e != nil || len(raw) > maxRemoteIndex {
		return nil, doc, artifact.ErrUnavailable
	}
	var out []remoteBlock
	seen := map[string]bool{}
	for _, para := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
		para = strings.Trim(para, "\n")
		if para == "" || len(para) > maxParagraph {
			continue
		}
		var name, version string
		for _, line := range strings.Split(para, "\n") {
			switch {
			case strings.HasPrefix(line, "Package: "):
				name = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
			case strings.HasPrefix(line, "Version: "):
				version = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
			}
		}
		if !validName(name) || !validVersion(version) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, remoteBlock{name, para})
		if len(out) > maxRemotePackages {
			return nil, doc, artifact.ErrUnavailable
		}
	}
	return out, doc, nil
}

// locallyOwned reports whether a local source of the feed has any version of the
// package. Such a name is never answered from upstream.
func (s *Repository) locallyOwned(ctx context.Context, p identity.Principal, feed, name string) (bool, error) {
	var owned bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id AND c.coordinate=$3 JOIN versions v ON v.component_id=c.id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.kind='local' AND split_part(v.version,'_',1)=$4)`, p.InstanceID, feed, channel, name).Scan(&owned)
	return owned, err
}

// fetchRemote serves a missing package archive from upstream. It returns served=true
// when a streamed response was written, otherwise the cached record and body.
func (s *Repository) fetchRemote(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, name, path string) (rec artifact.Record, body io.ReadCloser, served bool, err error) {
	if rec, body, err = s.service.Open(r.Context(), p, pub, 0, -1); err == nil || !isNotFound(err) {
		return rec, body, false, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	owned, e := s.locallyOwned(ctx, p, pub.FeedID, name)
	cancel()
	if e != nil {
		return rec, nil, false, artifact.ErrUnavailable
	}
	if owned {
		return rec, nil, false, artifact.ErrNotFound
	}
	stream, again, e := s.service.PullThrough(r.Context(), p, pub, artifact.Origin{Path: artifact.EscapePath(path)})
	if e != nil {
		return rec, nil, false, e
	}
	if again {
		rec, body, err = s.service.Open(r.Context(), p, pub, 0, -1)
		return rec, body, false, err
	}
	stream.ServeStream(w, r.Method)
	return rec, nil, true, nil
}

// extraRoute describes a read-only upstream route outside src/contrib/<file>.
type extraRoute struct {
	dir, file string // upstream directory (no trailing slash) and file name
	name, ver string // package and version for archives
	index     bool
	media     string
}

// parseExtra recognizes src/contrib/Archive/<pkg>/<file> and bin/.../contrib/<rver>/<file>.
func parseExtra(a []string) (extraRoute, bool) {
	rest := a[3:]
	for _, seg := range rest {
		if !segmentPattern.MatchString(seg) {
			return extraRoute{}, false
		}
	}
	var x extraRoute
	switch {
	case len(rest) == 5 && rest[0] == "src" && rest[1] == "contrib" && rest[2] == "Archive":
		x.dir, x.file = strings.Join(rest[:4], "/"), rest[4]
		if !validName(rest[3]) {
			return extraRoute{}, false
		}
	case len(rest) >= 5 && rest[0] == "bin" && rest[len(rest)-3] == "contrib":
		x.dir, x.file = strings.Join(rest[:len(rest)-1], "/"), rest[len(rest)-1]
	default:
		return extraRoute{}, false
	}
	switch {
	case x.file == "PACKAGES" && rest[0] == "bin":
		x.index, x.media = true, "text/plain; charset=utf-8"
	case x.file == "PACKAGES.gz" && rest[0] == "bin":
		x.index, x.media = true, "application/gzip"
	default:
		var stem string
		for _, ext := range []string{".tar.gz", ".zip", ".tgz"} {
			if t, ok := strings.CutSuffix(x.file, ext); ok && (ext == ".tar.gz") == (rest[0] == "src") {
				stem, x.media = t, "application/gzip"
				if ext == ".zip" {
					x.media = "application/zip"
				}
			}
		}
		name, version, ok := strings.Cut(stem, "_")
		if stem == "" || !ok || !validName(name) || !validVersion(version) {
			return extraRoute{}, false
		}
		if rest[0] == "src" && name != rest[3] {
			return extraRoute{}, false
		}
		x.name, x.ver = name, version
	}
	return x, true
}

// serveExtra answers the read-only upstream routes. Everything here is remote only.
func (s *Repository) serveExtra(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, x extraRoute) {
	if x.index {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: "index:" + x.dir}, artifact.Origin{Path: x.dir + "/" + x.file})
		if err != nil {
			failure(w, err)
			return
		}
		if doc.Stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("Content-Type", x.media)
		w.Header().Set("Content-Length", strconv.Itoa(len(doc.Body)))
		if r.Method != "HEAD" {
			_, _ = w.Write(doc.Body)
		}
		return
	}
	// Source archives share one cache entry whichever route asked.
	coordinate, path := x.name, x.dir+"/"+x.file
	if strings.HasPrefix(x.dir, "bin/") {
		coordinate = x.dir + "/" + x.name
	}
	pub := artifact.Publication{FeedID: feed, Coordinate: coordinate, Version: x.ver, AssetKey: "archive", MediaType: x.media}
	rec, body, served, err := s.fetchRemote(w, r, p, pub, x.name, path)
	if served {
		return
	}
	s.writeArchive(w, r, rec, body, err)
}

func (s *Repository) writeArchive(w http.ResponseWriter, r *http.Request, record artifact.Record, body io.ReadCloser, e error) {
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
