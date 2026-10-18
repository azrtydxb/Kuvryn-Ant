package aptrepo

import (
	"context"
	"crypto/md5"  // Debian Packages checksum alongside SHA-1/SHA-256.
	"crypto/sha1" // #nosec G505 -- legacy Packages field, never used for trust.
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// ingest validates and stores one .deb, then lists it in a new signed
// generation of ?distribution=. The package is invisible to apt until the
// Release bundle commits; a failed listing can be retried with the same PUT.
func (s *Repository) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, comp, filename string) {
	dist, ok := distParam(r)
	if !ok {
		failure(w, errInvalid)
		return
	}
	if _, e := s.store(); e != nil {
		failure(w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e == nil && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: pub.FeedID, Kind: identity.FeedResource}, identity.Admin) {
		e = artifact.ErrDenied
	}
	if e == nil {
		pub.ExpectedSource, e = s.sources(ctx, p, pub.FeedID, dist, &pub)
	}
	cancel()
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
	f, e := os.CreateTemp("", "ant-apt-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h256, h1, hm := sha256.New(), sha1.New(), md5.New() // #nosec G401
	n, e := io.CopyBuffer(io.MultiWriter(f, h256, h1, hm), io.LimitReader(contextReader{r.Context(), r.Body}, maxDeb+1), make([]byte, 64<<10))
	if e != nil || n > maxDeb {
		failure(w, errInvalid)
		return
	}
	digest := hex.EncodeToString(h256.Sum(nil))
	if want := r.Header.Get("X-Checksum-Sha256"); want != "" && want != digest {
		failure(w, errInvalid)
		return
	}
	ctl, e := inspectDeb(f, n)
	if e != nil {
		failure(w, e)
		return
	}
	if filename != "pool/"+comp+"/"+ctl.poolDir()+"/"+ctl.fileName() {
		failure(w, errInvalid) // path must be the canonical pool location of this package
		return
	}
	text := ctl.stanza(filename, n, hex.EncodeToString(hm.Sum(nil)), hex.EncodeToString(h1.Sum(nil)), digest)
	x, e := parseEntry(text)
	if e != nil {
		failure(w, errInvalid)
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	deb := pub
	deb.Info = blob.Info{Size: n, Digest: blob.Digest(digest)}
	meta := pub
	meta.AssetKey, meta.MediaType, meta.Info = "control", "text/plain", info([]byte(text))
	if _, e = s.service.PublishBundle(ctx, p, []artifact.Publication{deb, meta}, []io.Reader{contextReader{ctx, f}, strings.NewReader(text)}); e != nil {
		failure(w, e)
		return
	}
	mu := s.feedLock(pub.FeedID)
	mu.Lock()
	defer mu.Unlock()
	st, seq, e := s.loadState(ctx, p, pub.FeedID, dist)
	if e != nil {
		failure(w, e)
		return
	}
	if old, exists := st[comp][filename]; exists {
		if old.checksum != x.checksum {
			failure(w, artifact.ErrConflict)
			return
		}
		if old.text == x.text {
			w.WriteHeader(201) // identical retry: already listed
			return
		}
	}
	if st[comp] == nil {
		st[comp] = map[string]entry{}
	}
	st[comp][filename] = x
	if e = s.commit(ctx, p, pub.FeedID, dist, pub.ExpectedSource, st, seq); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
}

// unlist removes a package from a new signed generation of the distribution.
// Delete authorization is a separate grant from publish. The stored bytes stay
// referenced in the core (and by-hash history) until retention policy collects
// them, so earlier generations remain installable by readers still using them.
func (s *Repository) unlist(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, comp, filename string) {
	dist, ok := distParam(r)
	if !ok {
		failure(w, errInvalid)
		return
	}
	if _, e := s.store(); e != nil {
		failure(w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	source, e := s.sources(ctx, p, pub.FeedID, dist, nil)
	if e != nil {
		failure(w, e)
		return
	}
	mu := s.feedLock(pub.FeedID)
	mu.Lock()
	defer mu.Unlock()
	st, seq, e := s.loadState(ctx, p, pub.FeedID, dist)
	if e != nil {
		failure(w, e)
		return
	}
	if _, exists := st[comp][filename]; !exists {
		failure(w, artifact.ErrNotFound)
		return
	}
	// The shared core authorizes (delete permission), applies the feed delete
	// policy and records audit, outbox and the unlisted marker atomically. It
	// runs before the index changes, so a refusal leaves the index untouched and
	// a retry after a failed index commit is idempotent.
	if _, e = s.service.Unlist(ctx, p, artifact.Target{FeedID: pub.FeedID, Namespace: pub.Namespace, Coordinate: pub.Coordinate, Version: pub.Version}); e != nil {
		failure(w, e)
		return
	}
	delete(st[comp], filename)
	if e = s.commit(ctx, p, pub.FeedID, dist, source, st, seq); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(204)
}
