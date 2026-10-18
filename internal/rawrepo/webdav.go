package rawrepo

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

// Minimal WebDAV class-1 subset so WebDAV-only clients (sccache's WebDAV
// backend, built on OpenDAL) can use a raw feed as a build cache. Together with
// the existing GET/HEAD/PUT it provides:
//
//   - PROPFIND: existence and size of a file or of a collection. Directories are
//     virtual (the raw feed has a flat key space); a collection exists once
//     some file lives below it, and the feed root always exists. Only the
//     resource itself is reported: children of a collection are not listed.
//   - MKCOL: authorizes publishing under the path and answers 201 without
//     creating state (the directory appears with its first file).
//
// OPTIONS advertises the methods. DELETE, COPY, MOVE and locking stay
// unsupported: cache entries are immutable (a PUT with different bytes for an
// existing key is rejected with 409, which sccache treats as a non-fatal
// cache-write failure).

// parseDAV is like parsePath but accepts the feed root and collection paths
// with a trailing slash.
func parseDAV(req *http.Request) (slug, key string, collection bool, err error) {
	raw := req.URL.EscapedPath()
	collection = strings.HasSuffix(raw, "/")
	normalized, err := routing.NormalizePath(raw)
	if err != nil {
		return "", "", false, artifact.ErrInput
	}
	parts := strings.SplitN(normalized, "/", 5)
	if len(parts) < 4 || parts[1] != "feeds" || parts[3] != "raw" || len(parts[2]) > 128 {
		return "", "", false, artifact.ErrInput
	}
	if len(parts) == 4 {
		return parts[2], "", true, nil
	}
	if len(parts[4]) > 256 {
		return "", "", false, artifact.ErrInput
	}
	return parts[2], parts[4], collection, nil
}

func isDAV(method string) bool {
	return method == "PROPFIND" || method == "MKCOL" || method == http.MethodOptions
}

type davProp struct {
	collection bool
	size       int64
	etag, ctyp string
	modified   time.Time
}

func writeMultistatus(w http.ResponseWriter, href string, prop davProp) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>`)
	_ = xml.EscapeText(&b, []byte(href))
	b.WriteString(`</D:href><D:propstat><D:prop>`)
	// OpenDAL (sccache) refuses a PROPFIND response without getlastmodified.
	modified := prop.modified
	if modified.IsZero() {
		modified = time.Now()
	}
	b.WriteString(`<D:getlastmodified>` + modified.UTC().Format(http.TimeFormat) + `</D:getlastmodified>`)
	if prop.collection {
		b.WriteString(`<D:resourcetype><D:collection/></D:resourcetype>`)
	} else {
		b.WriteString(`<D:resourcetype/><D:getcontentlength>` + strconv.FormatInt(prop.size, 10) + `</D:getcontentlength>`)
		b.WriteString(`<D:getetag>`)
		_ = xml.EscapeText(&b, []byte(prop.etag))
		b.WriteString(`</D:getetag><D:getcontenttype>`)
		_ = xml.EscapeText(&b, []byte(prop.ctyp))
		b.WriteString(`</D:getcontenttype>`)
	}
	b.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

func (r *Repository) webdav(w http.ResponseWriter, req *http.Request, p identity.Principal) {
	if req.Method == http.MethodOptions {
		w.Header().Set("DAV", "1")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, PROPFIND, MKCOL")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	slug, key, collection, err := parseDAV(req)
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	var feed string
	err = r.db.QueryRow(ctx, "SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='raw'", p.InstanceID, slug).Scan(&feed)
	if errors.Is(err, pgx.ErrNoRows) {
		err = artifact.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	dir := strings.TrimSuffix(key, "/")
	if !collection {
		dir = ""
		if i := strings.LastIndexByte(key, '/'); i >= 0 {
			dir = key[:i]
		}
	}
	switch req.Method {
	case "MKCOL":
		if req.ContentLength > 0 {
			http.Error(w, "MKCOL bodies are unsupported", http.StatusUnsupportedMediaType)
			return
		}
		if !collection || key == "" {
			http.Error(w, "collection exists", http.StatusMethodNotAllowed)
			return
		}
		if _, err = r.service.Prepare(ctx, p, feed, dir, dir+"/.collection", dir+"/.collection"); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case "PROPFIND":
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 64<<10))
		href := req.URL.EscapedPath()
		if !collection && key != "" {
			rec, err := r.service.Lookup(ctx, p, artifact.Publication{FeedID: feed, Namespace: dir, Coordinate: key, Version: "first", AssetKey: key})
			if err == nil {
				var created time.Time
				_ = r.db.QueryRow(ctx, "SELECT created_at FROM versions WHERE id=$1", rec.VersionID).Scan(&created)
				writeMultistatus(w, href, davProp{size: rec.Info.Size, etag: etag(rec), ctyp: rec.MediaType, modified: created})
				return
			}
			if !errors.Is(err, artifact.ErrNotFound) {
				fail(w, err)
				return
			}
		}
		// Collection (also asked for without the trailing slash).
		policy, err := (identity.Roles{DB: r.db}).Resolve(ctx, p)
		if err != nil {
			fail(w, artifact.ErrUnavailable)
			return
		}
		sub := key
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Namespace: sub, Kind: identity.ArtifactResource}, identity.Read) {
			fail(w, artifact.ErrNotFound)
			return
		}
		if sub != "" {
			var exists bool
			err = r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND (c.namespace=$3 OR left(c.namespace,length($3)+1)=$3||'/'))`, p.InstanceID, feed, strings.TrimSuffix(sub, "/")).Scan(&exists)
			if err != nil {
				fail(w, artifact.ErrUnavailable)
				return
			}
			if !exists {
				fail(w, artifact.ErrNotFound)
				return
			}
		}
		if !strings.HasSuffix(href, "/") {
			href += "/"
		}
		writeMultistatus(w, href, davProp{collection: true})
	}
}
