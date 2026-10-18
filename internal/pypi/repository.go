// Package pypi serves authenticated Simple indexes and immutable legacy uploads.
package pypi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid PyPI configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4)}, nil
}
func failure(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}
func fail(w http.ResponseWriter, e error) {
	status := 503
	switch {
	case errors.Is(e, artifact.ErrNotFound), errors.Is(e, blob.ErrNotFound):
		status = 404
	case errors.Is(e, artifact.ErrDenied):
		status = 403
	case errors.Is(e, artifact.ErrConflict):
		status = 409
	case errors.Is(e, errInvalid), errors.Is(e, artifact.ErrInput), errors.Is(e, blob.ErrIntegrity):
		status = 400
	}
	failure(w, status)
}
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(r.Context())
	r.Body = &progressBody{ReadCloser: r.Body, ctx: r.Context(), c: c}
	w = &progressWriter{ResponseWriter: w, c: c}
	// Explicit credentials are mandatory, even in a browser-authenticated context.
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="pypi"`)
		failure(w, 401)
		return
	}
	if user, password, ok := r.BasicAuth(); ok {
		if user == "" || password == "" {
			failure(w, 401)
			return
		}
		r.Header.Set("Authorization", "Bearer "+password)
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "pypi" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='pypi'`, s.instance, a[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		failure(w, 404)
		return
	}
	if e != nil {
		fail(w, e)
		return
	}
	base := "/feeds/" + url.PathEscape(a[1]) + "/pypi/"
	route := strings.Join(a[3:], "/")
	if (route == "" || route == "legacy/") && r.Method == "POST" {
		s.publish(w, r, p, feed)
		return
	}
	if (r.Method == "DELETE" || r.Method == "PUT") && (len(a) == 6 || len(a) == 7) && a[3] == "files" && validName(a[4]) && validName(a[5]) {
		// PyPI clients have no delete/yank command (the Warehouse web UI does
		// both). DELETE files/{project}/{filename} removes the distribution and
		// PUT/DELETE files/{project}/{filename}/yanked yanks (unlists, PEP 592) /
		// restores it. All are policy-gated by the shared artifact core.
		t := artifact.Target{FeedID: feed, Coordinate: normalize(a[4]), Version: versionKey(a[5])}
		var e error
		switch {
		case len(a) == 6 && r.Method == "DELETE":
			_, e = s.service.Delete(r.Context(), p, t)
		case len(a) == 7 && a[6] == "yanked" && r.Method == "PUT":
			_, e = s.service.Unlist(r.Context(), p, t)
		case len(a) == 7 && a[6] == "yanked" && r.Method == "DELETE":
			_, e = s.service.Relist(r.Context(), p, t)
		default:
			failure(w, 405)
			return
		}
		if e != nil {
			fail(w, e)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		failure(w, 405)
		return
	}
	if route == "simple" {
		http.Redirect(w, r, base+"simple/", 301)
		return
	}
	if strings.HasPrefix(route, "simple/") {
		project := strings.TrimSuffix(strings.TrimPrefix(route, "simple/"), "/")
		if project != "" && !validName(project) {
			failure(w, 404)
			return
		}
		if !strings.HasSuffix(route, "/") || project != normalize(project) {
			http.Redirect(w, r, base+"simple/"+normalize(project)+"/", 301)
			return
		}
		s.index(w, r, p, feed, base, project)
		return
	}
	if len(a) == 8 && a[3] == "files" && validName(a[4]) {
		s.download(w, r, p, feed, a[4], a[5], a[6], a[7])
		return
	}
	failure(w, 404)
}
func (s *Repository) publish(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, 503)
		return
	}
	mr, e := r.MultipartReader()
	if e != nil {
		fail(w, errInvalid)
		return
	}
	fields := map[string]string{}
	var f *os.File
	var m metadata
	var core []byte
	var info blob.Info
	var source string
	var file string
	defer func() {
		if f != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	for count := 0; ; count++ {
		part, e := mr.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil || count >= 128 {
			fail(w, errInvalid)
			return
		}
		_, params, e := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if e != nil {
			fail(w, errInvalid)
			return
		}
		if part.FormName() == "content" {
			if f != nil {
				fail(w, errInvalid)
				return
			}
			file = params["filename"]
			name, _, _, e := filename(file)
			if e != nil {
				fail(w, e)
				return
			}
			source, e = s.service.Prepare(r.Context(), p, feed, "", name, file)
			if e != nil {
				fail(w, e)
				return
			}
			for _, key := range []string{"index", file + ".metadata"} {
				other, e := s.service.Prepare(r.Context(), p, feed, "", name, key)
				if e != nil {
					fail(w, e)
					return
				}
				if other != source {
					fail(w, artifact.ErrConflict)
					return
				}
			}
			f, e = os.CreateTemp("", "ant-pypi-*")
			if e != nil {
				fail(w, e)
				return
			}
			h := sha256.New()
			n, e := io.CopyBuffer(io.MultiWriter(f, h), part, make([]byte, 64<<10))
			if e != nil {
				fail(w, e)
				return
			}
			info = blob.Info{Digest: blob.Digest(hex.EncodeToString(h.Sum(nil))), Size: n}
			m, core, e = inspect(r.Context(), f, file)
			if e != nil {
				fail(w, e)
				return
			}
		} else {
			b, e := boundedRead(part, 64<<10)
			if e != nil {
				fail(w, e)
				return
			}
			key := part.FormName()
			if _, ok := fields[key]; ok && (key == "name" || key == "version" || key == ":action" || key == "sha256_digest") {
				fail(w, errInvalid)
				return
			}
			fields[key] = string(b)
		}
	}
	if f == nil || fields[":action"] != "file_upload" || normalize(fields["name"]) != m.Name || fields["version"] != m.Version || (fields["sha256_digest"] != "" && fields["sha256_digest"] != string(info.Digest)) {
		fail(w, errInvalid)
		return
	}
	m.Digest = string(info.Digest)
	data, _ := json.Marshal(m)
	version := versionKey(file)
	pubs := []artifact.Publication{}
	for i, key := range []string{file, file + ".metadata", "index"} {
		inf := info
		media := "application/octet-stream"
		if i == 1 {
			inf = bytesInfo(core)
			media = "text/plain"
		}
		if i == 2 {
			inf = bytesInfo(data)
			media = "application/json"
		}
		pubs = append(pubs, artifact.Publication{ExpectedSource: source, FeedID: feed, Coordinate: m.Name, Version: version, AssetKey: key, MediaType: media, Info: inf})
	}
	pubs[2].References = []artifact.Reference{{Version: version, AssetKey: file, Info: info}, {Version: version, AssetKey: file + ".metadata", Info: bytesInfo(core)}}
	_, e = s.service.PublishBundle(r.Context(), p, pubs, []io.Reader{f, bytes.NewReader(core), bytes.NewReader(data)})
	if e != nil {
		fail(w, e)
		return
	}
	w.WriteHeader(200)
}
func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, source, digest, file string) {
	original := strings.TrimSuffix(file, ".metadata")
	n, _, _, e := filename(original)
	if e != nil || n != name {
		failure(w, 404)
		return
	}
	record, body, e := s.service.Open(r.Context(), p, artifact.Publication{FeedID: feed, Coordinate: name, Version: versionKey(original), AssetKey: file}, 0, -1)
	if errors.Is(e, artifact.ErrNotFound) {
		retry, handled, through := s.pullThroughFile(w, r, p, feed, name, source, digest, file)
		switch {
		case through != nil:
			e = through
		case handled:
			return
		case retry:
			record, body, e = s.service.Open(r.Context(), p, artifact.Publication{FeedID: feed, Coordinate: name, Version: versionKey(original), AssetKey: file}, 0, -1)
		}
	}
	if e != nil {
		fail(w, e)
		return
	}
	defer body.Close()
	boundDigest := string(record.Info.Digest)
	if file != original {
		distribution, lookupErr := s.service.Lookup(r.Context(), p, artifact.Publication{FeedID: feed, Coordinate: name, Version: versionKey(original), AssetKey: original})
		if errors.Is(lookupErr, artifact.ErrNotFound) {
			// Pip fetches the PEP 658 sidecar before the wheel, so on a cold
			// cache only the sidecar is stored. Bind the route digest to the
			// distribution listed by the owning remote source instead.
			files, _, remoteErr := s.remoteFiles(r.Context(), p, feed, name, source)
			if remoteErr != nil {
				fail(w, remoteErr)
				return
			}
			for _, f := range files {
				if f.Filename == original && f.SHA256 == digest {
					lookupErr = nil
					distribution = artifact.Record{SourceID: source, Info: blob.Info{Digest: blobDigest(digest)}}
				}
			}
		}
		if lookupErr != nil {
			fail(w, lookupErr)
			return
		}
		if distribution.SourceID != source {
			failure(w, 404)
			return
		}
		boundDigest = string(distribution.Info.Digest)
	}
	if record.SourceID != source || boundDigest != digest {
		failure(w, 404)
		return
	}
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", `"`+digest+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

type entry struct{ name, source, version string }

func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, base, name string) {
	representation := negotiate(r.Header.Get("Accept"))
	if representation == "" {
		failure(w, 406)
		return
	}
	// Remote-owned (or unowned) projects come from the remote sources, rewritten
	// to this feed. Locally published projects fall through to the local index.
	var upstreamErr error
	if name != "" {
		if upstreamErr = s.remoteIndex(w, r, p, feed, base, name, representation); upstreamErr == errServed {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		fail(w, e)
		return
	}
	rows, e := s.db.Query(ctx, `WITH owners AS (SELECT DISTINCT ON(c.coordinate) c.id,c.coordinate,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND ($3='' OR c.coordinate=$3) ORDER BY c.coordinate,fs.priority) SELECT o.coordinate,o.source_id::text,v.version FROM owners o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='index' ORDER BY o.coordinate,v.version LIMIT 10001`, s.instance, feed, name)
	if e != nil {
		fail(w, e)
		return
	}
	entries := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.name, &v.source, &v.version); e != nil {
			break
		}
		entries = append(entries, v)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		fail(w, e)
		return
	}
	if len(entries) > 10000 {
		failure(w, 503)
		return
	}
	projects := []map[string]string{}
	files := []map[string]any{}
	seen := map[string]bool{}
	var markup strings.Builder
	markup.WriteString("<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"></head><body>\n")
	total := 0
	yankedFiles := map[string]bool{}
	if name != "" {
		if yankedFiles, e = s.service.UnlistedVersions(ctx, p.InstanceID, feed, name); e != nil {
			fail(w, e)
			return
		}
	}
	for _, v := range entries {
		if !policy.Authorize(p, identity.Resource{InstanceID: s.instance, FeedID: feed, SourceID: v.source, Kind: identity.ArtifactResource}, identity.Read) {
			continue
		}
		if name == "" {
			if !seen[v.name] && validName(v.name) {
				projects = append(projects, map[string]string{"name": v.name})
				fmt.Fprintf(&markup, "<a href=\"%s/\">%s</a>\n", html.EscapeString(v.name), html.EscapeString(v.name))
				seen[v.name] = true
			}
			continue
		}
		rec, body, e := s.service.Open(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name, Version: v.version, AssetKey: "index"}, 0, -1)
		if e != nil {
			fail(w, e)
			return
		}
		if rec.SourceID != v.source || rec.Info.Size > maxMetadata || rec.Info.Size < 0 || int64(total)+rec.Info.Size > 16<<20 {
			body.Close()
			failure(w, 503)
			return
		}
		data, e := boundedRead(body, maxMetadata)
		body.Close()
		total += len(data)
		var m metadata
		if e != nil || json.Unmarshal(data, &m) != nil || m.Name != name || versionKey(m.Filename) != v.version {
			failure(w, 503)
			return
		}
		fileRec, e := s.service.Lookup(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name, Version: v.version, AssetKey: m.Filename})
		if e != nil {
			fail(w, e)
			return
		}
		coreRec, e := s.service.Lookup(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name, Version: v.version, AssetKey: m.Filename + ".metadata"})
		if e != nil {
			fail(w, e)
			return
		}
		if fileRec.SourceID != v.source || coreRec.SourceID != v.source || string(fileRec.Info.Digest) != m.Digest || string(coreRec.Info.Digest) != m.CoreDigest {
			failure(w, 503)
			return
		}
		// PEP 658 appends .metadata to the file URL; bind both using the distribution digest route.
		link := base + "files/" + url.PathEscape(name) + "/" + v.source + "/" + m.Digest + "/" + url.PathEscape(m.Filename)
		if yankedFiles[v.version] && (m.Yanked == false || m.Yanked == nil) {
			m.Yanked = true
		}
		item := map[string]any{"filename": m.Filename, "url": link, "hashes": map[string]string{"sha256": m.Digest}, "requires-python": m.RequiresPython, "yanked": m.Yanked}
		if strings.HasSuffix(m.Filename, ".whl") {
			item["core-metadata"] = map[string]string{"sha256": m.CoreDigest}
		}
		files = append(files, item)
		fmt.Fprintf(&markup, "<a href=\"%s#sha256=%s\" data-requires-python=\"%s\"", html.EscapeString(link), m.Digest, html.EscapeString(m.RequiresPython))
		if strings.HasSuffix(m.Filename, ".whl") {
			fmt.Fprintf(&markup, " data-core-metadata=\"sha256=%s\"", m.CoreDigest)
		}
		if m.Yanked != false && m.Yanked != nil {
			fmt.Fprintf(&markup, " data-yanked=\"%s\"", html.EscapeString(fmt.Sprint(m.Yanked)))
		}
		fmt.Fprintf(&markup, ">%s</a>\n", html.EscapeString(m.Filename))
		if markup.Len() > 16<<20 {
			failure(w, 503)
			return
		}
	}
	if name != "" && len(files) == 0 {
		if upstreamErr != nil {
			fail(w, upstreamErr)
			return
		}
		failure(w, 404)
		return
	}
	markup.WriteString("</body></html>")
	w.Header().Set("Vary", "Accept, Authorization")
	w.Header().Set("Cache-Control", "private, no-cache")
	var data []byte
	if representation == "application/vnd.pypi.simple.v1+json" {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		result := map[string]any{"meta": map[string]string{"api-version": "1.0"}}
		if name == "" {
			result["projects"] = projects
		} else {
			result["name"] = name
			result["files"] = files
		}
		data, _ = json.Marshal(result)
	} else {
		w.Header().Set("Content-Type", representation+"; charset=utf-8")
		data = []byte(markup.String())
	}
	if len(data) > 16<<20 {
		failure(w, 503)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
}

type progressBody struct {
	io.ReadCloser
	ctx context.Context
	c   *http.ResponseController
}

func (p *progressBody) Read(b []byte) (int, error) {
	if e := p.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	_ = p.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	return p.ReadCloser.Read(b)
}

type progressWriter struct {
	http.ResponseWriter
	c *http.ResponseController
}

func (p *progressWriter) WriteHeader(n int) {
	_ = p.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	p.ResponseWriter.WriteHeader(n)
}
func (p *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		_ = p.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		n, e := p.ResponseWriter.Write(chunk)
		total += n
		b = b[n:]
		if e != nil {
			return total, e
		}
		if n != len(chunk) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// Honor q=0 exclusions and specificity, preferring JSON at equal quality.
func negotiate(accept string) string {
	if accept == "" {
		return "text/html"
	}
	best := ""
	quality := -1.0
	for _, candidate := range []string{"application/vnd.pypi.simple.v1+json", "application/vnd.pypi.simple.v1+html", "text/html"} {
		specificity := -1
		q := -1.0
		for _, part := range strings.Split(accept, ",") {
			media, params, e := mime.ParseMediaType(strings.TrimSpace(part))
			if e != nil {
				continue
			}
			level := -1
			if media == candidate {
				level = 2
			} else if media == strings.Split(candidate, "/")[0]+"/*" {
				level = 1
			} else if media == "*/*" {
				level = 0
			}
			value := 1.0
			if raw, ok := params["q"]; ok {
				value, e = strconv.ParseFloat(raw, 64)
				if e != nil || value < 0 || value > 1 {
					continue
				}
			}
			if level >= 0 && level > specificity {
				specificity = level
				q = value
			}
		}
		if q > 0 && q > quality {
			best = candidate
			quality = q
		}
	}
	return best
}
