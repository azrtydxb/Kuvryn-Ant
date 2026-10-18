package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for OCI distribution reads. Content addressed objects (blobs and
// manifests by digest) are immutable: they are fetched with their digest
// verified before the first byte is served and then cached under the remote
// source. A tag is a mutable pointer, so a tag read is proxied through the
// bounded metadata cache (TTL, negative cache, stale-if-error) and the manifest
// it resolves to is also cached by digest; no tag row is ever written for a
// remote repository. Upstream authentication is the source's static credential
// only: registries that require a bearer-token handshake are not supported.

// manifestAccept lists the manifest types requested upstream (kept under the
// runtime's 256 byte representation bound).
const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

func upstreamPath(rep repository, kind, reference string) string {
	return "v2/" + artifact.EscapePath(rep.name) + "/" + kind + "/" + reference
}

// pullThroughDigest fetches a missing blob or manifest by digest. It returns
// retry=true when the object is now in the store.
func (r *Registry) pullThroughDigest(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, version, key string) (retry, handled bool, err error) {
	digest := blob.Digest(version)
	if !digest.Valid() {
		return false, false, artifact.ErrNotFound
	}
	pub := rep.publication(version, key)
	origin := artifact.Origin{Digest: digest}
	if key == "manifest" {
		origin.Path = upstreamPath(rep, "manifests", "sha256:"+version)
		origin.Representation = manifestAccept
	} else {
		origin.Path = upstreamPath(rep, "blobs", "sha256:"+version)
		pub.MediaType = "application/octet-stream"
	}
	stream, again, err := r.service.PullThrough(req.Context(), p, pub, origin)
	if err != nil {
		return false, false, err
	}
	if again {
		return true, false, nil
	}
	w.Header().Set("Docker-Content-Digest", "sha256:"+version)
	stream.ServeStream(w, req.Method)
	return false, true, nil
}

// remoteTag serves a manifest tag from the remote sources. It returns false
// when nothing upstream applies, so the caller reports the usual not-found.
func (r *Registry) remoteTag(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, tag string) bool {
	ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
	defer cancel()
	doc, err := r.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: rep.feed, Namespace: rep.namespace, Coordinate: rep.name}, artifact.Origin{Path: upstreamPath(rep, "manifests", tag), Representation: manifestAccept})
	if errors.Is(err, artifact.ErrNotFound) {
		return false
	}
	if err != nil {
		failService(w, err)
		return true
	}
	media, _, perr := mime.ParseMediaType(doc.MediaType)
	if perr != nil || len(doc.Body) == 0 {
		failure(w, 503, "UNKNOWN", "upstream manifest unusable")
		return true
	}
	sum := sha256.Sum256(doc.Body)
	digest := hex.EncodeToString(sum[:])
	if !accepts(req.Header.Get("Accept"), media) {
		failure(w, 406, "MANIFEST_UNKNOWN", "manifest media type not accepted")
		return true
	}
	pub := rep.publication(digest, "manifest")
	if _, lookupErr := r.service.Lookup(ctx, p, pub); errors.Is(lookupErr, artifact.ErrNotFound) {
		pub.MediaType = media
		_ = r.service.CacheDocument(ctx, p, pub, doc.SourceID, doc.Body)
	}
	etag := strconv.Quote("sha256:" + digest)
	h := w.Header()
	h.Set("Docker-Content-Digest", "sha256:"+digest)
	h.Set("ETag", etag)
	h.Set("Content-Type", media)
	h.Set("Cache-Control", "private, no-cache")
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			h.Set(k, v)
		}
	}
	if req.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return true
	}
	h.Set("Content-Length", strconv.Itoa(len(doc.Body)))
	w.WriteHeader(200)
	if req.Method != "HEAD" {
		_, _ = w.Write(doc.Body)
	}
	return true
}

// remoteTags proxies an unpaginated tag list for a remote repository, naming it
// with the feed-qualified repository the client addressed. It returns false
// when the caller should answer from the local view.
func (r *Registry) remoteTags(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository) bool {
	ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
	defer cancel()
	doc, err := r.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: rep.feed, Namespace: rep.namespace, Coordinate: rep.name}, artifact.Origin{Path: upstreamPath(rep, "tags", "list"), Representation: "application/json"})
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrUnavailable) {
			return false
		}
		failService(w, err)
		return true
	}
	var list struct {
		Tags []string `json:"tags"`
	}
	if json.Unmarshal(doc.Body, &list) != nil {
		return false
	}
	valid := make([]string, 0, len(list.Tags))
	for _, tag := range list.Tags {
		if tagPattern.MatchString(tag) {
			valid = append(valid, tag)
		}
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{rep.full, valid})
	return true
}
