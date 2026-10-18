package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for PyPI. The project page is fetched from the remote source
// (preferring the PEP 691 JSON form, accepting PEP 503 HTML), reduced to the
// files this adapter can serve safely, and re-rendered in the representation the
// client negotiated with every URL rewritten to this feed. File links embed the
// serving remote's source id and the file's sha256, exactly like locally
// published files, so the existing download route binds bytes to the metadata.
// Upstream file URLs (often on a separate CDN origin) are looked up server-side
// when a file is requested; clients never supply an upstream URL.

const upstreamAccept = "application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html;q=0.2, text/html;q=0.1"

// errServed means the remote path already wrote the response.
var errServed = errors.New("served")

func blobDigest(s string) blob.Digest {
	if sha256Re.MatchString(s) {
		return blob.Digest(s)
	}
	return ""
}

type remoteFile struct {
	Filename, URL, SHA256, RequiresPython, CoreSHA256 string
	Yanked                                            any
}

var (
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	anchorRe  = regexp.MustCompile(`(?is)<a\s+([^>]*)>(.*?)</a>`)
	attrRe    = regexp.MustCompile(`([A-Za-z_:][-A-Za-z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	hashQuery = regexp.MustCompile(`(?:^|&)sha256=([0-9a-f]{64})(?:&|$)`)
)

func coreHash(v any) string {
	if m, ok := v.(map[string]any); ok {
		if h, ok := m["hashes"].(map[string]any); ok {
			if s, ok := h["sha256"].(string); ok && sha256Re.MatchString(s) {
				return s
			}
		}
		if s, ok := m["sha256"].(string); ok && sha256Re.MatchString(s) {
			return s
		}
	}
	return ""
}

// parseProject extracts files from a PEP 691 JSON or PEP 503 HTML project page.
func parseProject(mediaType string, body []byte) ([]remoteFile, error) {
	var files []remoteFile
	if strings.Contains(mediaType, "json") {
		var doc struct {
			Files []struct {
				Filename       string            `json:"filename"`
				URL            string            `json:"url"`
				Hashes         map[string]string `json:"hashes"`
				RequiresPython string            `json:"requires-python"`
				Yanked         any               `json:"yanked"`
				CoreMetadata   any               `json:"core-metadata"`
				DistInfo       any               `json:"dist-info-metadata"`
			} `json:"files"`
		}
		if json.Unmarshal(body, &doc) != nil {
			return nil, errors.New("invalid upstream project page")
		}
		for _, f := range doc.Files {
			core := coreHash(f.CoreMetadata)
			if core == "" {
				core = coreHash(f.DistInfo)
			}
			files = append(files, remoteFile{Filename: f.Filename, URL: f.URL, SHA256: f.Hashes["sha256"], RequiresPython: f.RequiresPython, Yanked: f.Yanked, CoreSHA256: core})
		}
		return files, nil
	}
	for _, m := range anchorRe.FindAllSubmatch(body, 100000) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(string(m[1]), -1) {
			value := a[2]
			if value == "" {
				value = a[3]
			}
			attrs[strings.ToLower(a[1])] = html.UnescapeString(value)
		}
		href := attrs["href"]
		if href == "" {
			continue
		}
		f := remoteFile{Filename: strings.TrimSpace(html.UnescapeString(string(m[2]))), RequiresPython: attrs["data-requires-python"]}
		if i := strings.IndexByte(href, '#'); i >= 0 {
			if h := hashQuery.FindStringSubmatch(href[i+1:]); h != nil {
				f.SHA256 = h[1]
			}
			href = href[:i]
		}
		f.URL = href
		for _, key := range []string{"data-core-metadata", "data-dist-info-metadata"} {
			if v := strings.TrimPrefix(attrs[key], "sha256="); sha256Re.MatchString(v) {
				f.CoreSHA256 = v
			}
		}
		if y, ok := attrs["data-yanked"]; ok {
			f.Yanked = y
			if y == "" {
				f.Yanked = true
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// remoteFiles returns the servable files of project name from the remote
// sources of the feed, plus the serving source and staleness.
func (s *Repository) remoteFiles(ctx context.Context, p identity.Principal, feed, name, only string) ([]remoteFile, artifact.Document, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name}, artifact.Origin{Path: "simple/" + url.PathEscape(name) + "/", Representation: upstreamAccept, SourceID: only})
	if err != nil {
		return nil, doc, err
	}
	all, err := parseProject(doc.MediaType, doc.Body)
	if err != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	var files []remoteFile
	seen := map[string]bool{}
	for _, f := range all {
		n, version, _, e := filename(f.Filename)
		// Only distributions the local adapter would itself accept are served,
		// and only when the upstream states their sha256 (no unbound downloads).
		if e != nil || n != name || version == "" || !sha256Re.MatchString(f.SHA256) || seen[f.Filename] {
			continue
		}
		seen[f.Filename] = true
		files = append(files, f)
	}
	return files, doc, nil
}

// remoteIndex answers a per-project page. It returns errServed once it wrote
// the response, nil when the caller should use the local view, or the upstream
// error that forced the degraded local fallback.
func (s *Repository) remoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, base, name, representation string) error {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	files, doc, err := s.remoteFiles(ctx, p, feed, name, "")
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return nil
	case errors.Is(err, artifact.ErrUnavailable):
		return err
	case err != nil:
		fail(w, err)
		return errServed
	}
	if len(files) == 0 {
		failure(w, 404)
		return errServed
	}
	items := []map[string]any{}
	var markup strings.Builder
	markup.WriteString("<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"></head><body>\n")
	for _, f := range files {
		link := base + "files/" + url.PathEscape(name) + "/" + doc.SourceID + "/" + f.SHA256 + "/" + url.PathEscape(f.Filename)
		item := map[string]any{"filename": f.Filename, "url": link, "hashes": map[string]string{"sha256": f.SHA256}, "requires-python": f.RequiresPython, "yanked": false}
		if f.Yanked != nil && f.Yanked != false {
			item["yanked"] = f.Yanked
		}
		fmt.Fprintf(&markup, "<a href=\"%s#sha256=%s\" data-requires-python=\"%s\"", html.EscapeString(link), f.SHA256, html.EscapeString(f.RequiresPython))
		if f.CoreSHA256 != "" && strings.HasSuffix(f.Filename, ".whl") {
			item["core-metadata"] = map[string]string{"sha256": f.CoreSHA256}
			fmt.Fprintf(&markup, " data-core-metadata=\"sha256=%s\"", f.CoreSHA256)
		}
		if f.Yanked != nil && f.Yanked != false {
			fmt.Fprintf(&markup, " data-yanked=\"%s\"", html.EscapeString(fmt.Sprint(f.Yanked)))
		}
		fmt.Fprintf(&markup, ">%s</a>\n", html.EscapeString(f.Filename))
		items = append(items, item)
	}
	markup.WriteString("</body></html>")
	w.Header().Set("Vary", "Accept, Authorization")
	w.Header().Set("Cache-Control", "private, no-cache")
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	var data []byte
	if representation == "application/vnd.pypi.simple.v1+json" {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		data, _ = json.Marshal(map[string]any{"meta": map[string]string{"api-version": "1.0"}, "name": name, "files": items})
	} else {
		w.Header().Set("Content-Type", representation+"; charset=utf-8")
		data = []byte(markup.String())
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
	return errServed
}

// pullThroughFile materializes a missing distribution (or its PEP 658 .metadata
// sidecar) pinned to the source and sha256 embedded in the requested route. It
// returns retry=true once the asset is in the store.
func (s *Repository) pullThroughFile(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, source, digest, file string) (retry bool, handled bool, err error) {
	original := strings.TrimSuffix(file, ".metadata")
	sidecar := file != original
	files, _, err := s.remoteFiles(r.Context(), p, feed, name, source)
	if err != nil {
		return false, false, err
	}
	var found *remoteFile
	for i := range files {
		if files[i].Filename == original && files[i].SHA256 == digest {
			found = &files[i]
		}
	}
	if found == nil {
		return false, false, artifact.ErrNotFound
	}
	path, absolute, err := artifact.ResolveReference("simple/"+url.PathEscape(name)+"/", found.URL)
	if err != nil {
		return false, false, artifact.ErrUnavailable
	}
	origin := artifact.Origin{Path: path, URL: absolute, SourceID: source}
	pub := artifact.Publication{FeedID: feed, Coordinate: name, Version: versionKey(original), AssetKey: file, MediaType: "application/octet-stream"}
	origin.Digest = blobDigest(found.SHA256)
	if sidecar {
		pub.MediaType = "text/plain"
		origin.Digest = blobDigest(found.CoreSHA256) // empty (unverified) when upstream gave none
		if origin.Path != "" {
			origin.Path += ".metadata"
		} else {
			u, e := url.Parse(origin.URL)
			if e != nil {
				return false, false, artifact.ErrUnavailable
			}
			u.Path += ".metadata"
			origin.URL = u.String()
		}
	}
	stream, again, err := s.service.PullThrough(r.Context(), p, pub, origin)
	if err != nil {
		return false, false, err
	}
	if again {
		return true, false, nil
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	stream.ServeStream(w, r.Method)
	return false, true, nil
}
