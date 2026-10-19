package gitlfs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Git LFS. The remote source origin is the upstream LFS
// endpoint, the URL a client would use as lfs.url (for example
// https://lfs.example.com/org/repo.git/info/lfs; the batch API lives at
// <origin>/objects/batch). Ant asks the upstream batch API (operation
// "download", basic transfer, sha256) whether it has an object and answers the
// client with its own download action. On the first download it asks upstream
// again for a fresh action, fetches the object from the action href (an
// expiring or signed URL, often on another host) and caches it into the artifact
// store under the remote source. The object id is the sha256 of the content, so
// every byte is verified against the id requested by the client: a mismatch
// aborts the transfer and is never cached. Objects are owned per oid within a
// feed: an oid held by a local source is never answered from upstream. Uploads
// and verify never go upstream.
//
// Limits: only the "basic" transfer; the upstream action headers are not
// forwarded (the source credential is sent only to the source origin), so
// upstreams that require per-action headers or Authorization on another host
// cannot be proxied; an action href on a host other than the source origin must
// be a configured redirect origin and must be https; LFS locks, custom transfer
// adapters and refs are not proxied.

type upstreamBatch struct {
	Objects []struct {
		OID     string       `json:"oid"`
		Size    int64        `json:"size"`
		Error   *objectError `json:"error"`
		Actions map[string]struct {
			Href string `json:"href"`
		} `json:"actions"`
	} `json:"objects"`
}

// remoteAction asks the upstream batch API for a download action of one object.
// It returns the absolute href, the serving source and the upstream-declared size.
func (s *Repository) remoteAction(ctx context.Context, p identity.Principal, feed, oid string, size int64) (href, source string, err error) {
	body, _ := json.Marshal(map[string]any{
		"operation": "download",
		"transfers": []string{"basic"},
		"hash_algo": "sha256",
		"objects":   []map[string]any{{"oid": oid, "size": size}},
	})
	doc, err := s.service.RemoteQuery(ctx, p, artifact.Publication{FeedID: feed, Namespace: "git-lfs", Coordinate: oid}, artifact.Origin{Path: "objects/batch", Representation: mediaType}, mediaType, body)
	if err != nil {
		return "", "", err
	}
	var out upstreamBatch
	if json.Unmarshal(doc.Body, &out) != nil || len(out.Objects) != 1 || out.Objects[0].OID != oid {
		return "", "", artifact.ErrUnavailable
	}
	o := out.Objects[0]
	if o.Error != nil {
		if o.Error.Code == 404 || o.Error.Code == 410 {
			return "", "", artifact.ErrNotFound
		}
		return "", "", artifact.ErrUnavailable
	}
	if o.Size != size {
		return "", "", artifact.ErrInput // the client declared a size the upstream does not hold
	}
	act, ok := o.Actions["download"]
	if !ok {
		return "", "", artifact.ErrNotFound
	}
	u, e := url.Parse(act.Href)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", "", artifact.ErrUnavailable
	}
	return act.Href, doc.SourceID, nil
}

// remoteKnown reports whether an upstream source has the object (batch time).
func (s *Repository) remoteKnown(ctx context.Context, p identity.Principal, feed, oid string, size int64) error {
	_, _, err := s.remoteAction(ctx, p, feed, oid, size)
	return err
}

// downloadRemote serves a granted download for an object that the batch step
// found upstream. A cached copy is served through the normal path; a miss is
// fetched, verified against the oid and cached.
func (s *Repository) downloadRemote(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	ctx := r.Context()
	if rec, err := s.service.Lookup(ctx, p, pub); err == nil {
		s.download(w, r, p, pub, rec.SourceID)
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		fail(w, err)
		return
	}
	if r.Method == http.MethodHead {
		// Metadata is known from the verified batch grant; nothing is fetched.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", strconv.Quote(string(pub.Info.Digest)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(pub.Info.Size, 10))
		w.WriteHeader(200)
		return
	}
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	href, source, err := s.remoteAction(actx, p, pub.FeedID, string(pub.Info.Digest), pub.Info.Size)
	cancel()
	if err != nil {
		fail(w, err)
		return
	}
	origin := artifact.Origin{URL: href, SourceID: source, Digest: pub.Info.Digest, StreamVerified: true}
	if r.Header.Get("Range") != "" {
		// A resumed transfer needs the whole object: fill the cache completely
		// (verified, detached from the client so a retry hits), then answer the
		// range from the store.
		origin.StreamVerified = false
		fill, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer stop()
		ctx = fill
	}
	stream, again, err := s.service.PullThrough(ctx, p, pub, origin)
	if err != nil {
		fail(w, err)
		return
	}
	if !again {
		stream.ServeStream(w, r.Method)
		return
	}
	rec, err := s.service.Lookup(r.Context(), p, pub)
	if err != nil {
		fail(w, artifact.ErrUnavailable)
		return
	}
	s.download(w, r, p, pub, rec.SourceID)
}
