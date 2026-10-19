package npmregistry

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/upstream"
)

const (
	auditPrefix       = "-/npm/v1/security/"
	auditTTL          = 2 * time.Minute
	auditMaxEntries   = 128
	auditMaxCacheBody = 2 << 20
	auditTimeout      = 20 * time.Second
)

// auditPaths are the upstream audit endpoints that are forwarded. The legacy
// "audits" endpoint is kept for older npm clients.
var auditPaths = map[string]bool{
	"advisories/bulk": true,
	"audits/quick":    true,
	"audits":          true,
}

type auditEntry struct {
	body      []byte
	mediaType string
	expires   time.Time
}

// auditCache is a small bounded TTL cache for audit answers. Audit queries are
// read-only and repeat verbatim across CI runs, so a short cache protects the
// upstream without making advisories noticeably stale.
type auditCache struct {
	mu sync.Mutex
	m  map[[32]byte]auditEntry
}

func (c *auditCache) get(k [32]byte, now time.Time) (auditEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if ok && now.After(e.expires) {
		delete(c.m, k)
		ok = false
	}
	return e, ok
}

func (c *auditCache) put(k [32]byte, e auditEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[[32]byte]auditEntry{}
	}
	if len(c.m) >= auditMaxEntries {
		for key, v := range c.m {
			if now.After(v.expires) {
				delete(c.m, key)
			}
		}
		for key := range c.m {
			if len(c.m) < auditMaxEntries {
				break
			}
			delete(c.m, key)
		}
	}
	c.m[k] = e
}

// audit forwards an npm/pnpm audit request to the feed's remote npm source(s).
// A feed without a remote source answers 404 so clients report "audit
// unavailable" instead of failing the install.
func (s *Registry) audit(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, route string) {
	sub := strings.TrimPrefix(route, auditPrefix)
	if !auditPaths[sub] {
		failure(w, 404, "route not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "audit requires POST")
		return
	}
	var src io.Reader = http.MaxBytesReader(w, r.Body, upstream.MaxPostBody)
	if enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc == "gzip" {
		zr, err := gzip.NewReader(src)
		if err != nil {
			failure(w, 400, "invalid gzip body")
			return
		}
		defer zr.Close()
		src = zr
	} else if enc != "" && enc != "identity" {
		failure(w, 415, "unsupported content encoding")
		return
	}
	body, err := io.ReadAll(io.LimitReader(src, upstream.MaxPostBody+1))
	if err != nil {
		failure(w, 400, "unreadable audit request")
		return
	}
	if len(body) > upstream.MaxPostBody {
		failure(w, 413, "audit request too large")
		return
	}
	if len(body) == 0 {
		failure(w, 400, "empty audit request")
		return
	}
	pub := artifact.Publication{FeedID: feed, Coordinate: "-audit"}
	if !s.service.HasRemote(r.Context(), p, pub) {
		failure(w, 404, "audit unavailable: feed has no remote npm source")
		return
	}
	key := sha256.Sum256([]byte(feed + "\x00" + sub + "\x00" + string(body)))
	now := time.Now()
	if e, ok := s.audits.get(key, now); ok {
		writeAudit(w, e, "hit")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), auditTimeout)
	defer cancel()
	doc, err := s.service.RemoteQuery(ctx, p, pub, artifact.Origin{Path: auditPrefix + sub, Representation: "application/json"}, "application/json", body)
	switch {
	case err == nil:
	case errors.Is(err, artifact.ErrNotFound):
		failure(w, 404, "audit unavailable: upstream has no audit endpoint")
		return
	case errors.Is(err, artifact.ErrUnavailable), errors.Is(err, artifact.ErrTooLarge):
		failure(w, 502, "audit upstream unavailable")
		return
	default:
		failService(w, err)
		return
	}
	mt := doc.MediaType
	if mt == "" {
		mt = "application/json"
	}
	e := auditEntry{body: doc.Body, mediaType: mt, expires: now.Add(auditTTL)}
	if len(doc.Body) <= auditMaxCacheBody {
		s.audits.put(key, e, now)
	}
	writeAudit(w, e, "miss")
}

func writeAudit(w http.ResponseWriter, e auditEntry, state string) {
	w.Header().Set("Content-Type", e.mediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Kuvryn-Cache", state)
	w.WriteHeader(200)
	_, _ = w.Write(e.body)
}
