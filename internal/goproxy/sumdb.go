package goproxy

import (
	"context"
	"errors"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// The GOPROXY protocol lets a proxy front the checksum database through
// {proxy}/sumdb/{name}/{supported,latest,lookup/...,tile/...}. Only the
// public sum.golang.org database is mirrored. The documents are signed by the
// database, so clients verify them regardless of what this proxy relays; the
// proxy only caches them through the feed's remote sources.
const sumdbName = "sum.golang.org"

var sumdbPath = regexp.MustCompile(`^(latest|lookup/[A-Za-z0-9!._~/+@-]{1,512}|tile/[0-9]{1,2}/([0-9]{1,3}|data)/[x0-9/.p]{1,64})$`)

// sumdbRoute recognises /feeds/{slug}/go/sumdb/{name}/{rest}.
func sumdbRoute(raw string) (slug, rest string, ok bool) {
	if !strings.HasPrefix(raw, "/feeds/") || len(raw) > 4096 {
		return "", "", false
	}
	slug, tail, found := strings.Cut(strings.TrimPrefix(raw, "/feeds/"), "/go/sumdb/")
	if !found || slug == "" || strings.Contains(slug, "/") {
		return "", "", false
	}
	return slug, tail, true
}

func (s *Proxy) sumdb(w http.ResponseWriter, r *http.Request, p identity.Principal, slug, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", 405)
		return
	}
	name, doc, _ := strings.Cut(rest, "/")
	// Unsupported databases answer 404 so the client falls back to contacting
	// the database directly (GOPROXY protocol, "sumdb/supported").
	if name != sumdbName || strings.Contains(doc, "..") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		http.Error(w, "checksum database not mirrored", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	err := s.db.QueryRow(ctx, `SELECT id FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='go'`, p.InstanceID, slug).Scan(&feed)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		err = artifact.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	pub := artifact.Publication{FeedID: feed, Coordinate: "sumdb"}
	if doc == "supported" {
		// Only advertise the mirror when a remote source can serve it.
		if !s.service.HasRemote(r.Context(), p, pub) {
			http.Error(w, "checksum database not mirrored", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		return
	}
	if !sumdbPath.MatchString(doc) {
		http.Error(w, "invalid checksum database request", 404)
		return
	}
	body, err := s.service.RemoteMetadata(r.Context(), p, pub, artifact.Origin{Path: path.Join("sumdb", name, doc), Representation: "text/plain"})
	if errors.Is(err, artifact.ErrNotFound) {
		// A GOPROXY upstream such as proxy.golang.org does not serve the checksum
		// database under /sumdb/; fetch the (signed, public) database itself.
		body, err = s.service.RemoteMetadata(r.Context(), p, pub, artifact.Origin{URL: s.sumdbBase() + "/" + doc, Representation: "text/plain"})
	}
	if errors.Is(err, artifact.ErrNotFound) {
		http.Error(w, "checksum database entry not found", 404)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if body.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body.Body)))
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Body)
	}
}

const defaultSumdbOrigin = "https://" + sumdbName

// sumdbBase is the checksum database origin used when the feed's remote source
// does not mirror it under /sumdb/.
func (s *Proxy) sumdbBase() string {
	if s.sumdbOrigin != "" {
		return s.sumdbOrigin
	}
	return defaultSumdbOrigin
}
