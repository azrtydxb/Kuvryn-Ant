package apkrepo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Alpine APK v2 repositories. The remote source origin is the
// repository root that holds <branch>/<repository>/<arch>/ (for example
// https://dl-cdn.alpinelinux.org/alpine). Trust model, as for APT and RPM: the
// signed APKINDEX.tar.gz is served verbatim from upstream, never rewritten,
// merged or re-signed by Ant, so apk verifies the upstream signature with the
// upstream key in /etc/apk/keys (Ant's own feed key signs local channels only and
// is not involved). A package is served only if the upstream index of its
// channel lists it; its control segment is checked against the index checksum
// (C:Q1<base64 sha1 of the control segment>, the identity apk itself uses) and
// its size against S: before the first byte is served, then cached. Signature
// segments inside packages are preserved. A channel is answered by exactly one
// owner: a channel with locally published packages is never extended from
// upstream. Indexes come from the in-process metadata cache (no offline index
// beyond its stale window); cached packages are durable.

const maxIndexInflated = 128 << 20

type indexEntry struct {
	checksum string // C: value, "Q1" + base64(sha1(control segment))
	size     int64  // S: value, or -1
	arch     string
}

var parsedIndexes = struct {
	sync.Mutex
	m     map[[32]byte]map[string]indexEntry
	order [][32]byte
}{m: map[[32]byte]map[string]indexEntry{}}

// parseIndex reads the APKINDEX member of an index archive (signature segment
// followed by the index segment) into package-version -> entry.
func parseIndex(body []byte) (map[string]indexEntry, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, errInvalid
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errInvalid
		}
		if h.Name != "APKINDEX" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, maxIndexInflated+1))
		if err != nil || len(raw) > maxIndexInflated {
			return nil, errInvalid
		}
		out := map[string]indexEntry{}
		for _, block := range strings.Split(string(raw), "\n\n") {
			if strings.TrimSpace(block) == "" {
				continue
			}
			f := entryFields(block + "\n")
			name, ver, c := f["P"], f["V"], f["C"]
			if !namePattern.MatchString(name) || !versionPattern.MatchString(ver) || len(c) < 3 || !strings.HasPrefix(c, "Q1") {
				continue
			}
			size := int64(-1)
			if s := f["S"]; s != "" {
				n, err := strconv.ParseInt(s, 10, 64)
				if err != nil || n < 0 {
					continue
				}
				size = n
			}
			out[name+"-"+ver] = indexEntry{checksum: c, size: size, arch: f["A"]}
			if len(out) > 500_000 {
				return nil, errInvalid
			}
		}
		return out, nil
	}
}

func channelPub(feed, channel string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: channel, Coordinate: channel}
}

func indexPath(channel string) string { return channel + "/APKINDEX.tar.gz" }

// serveRemoteIndex answers APKINDEX.tar.gz from upstream. It returns true when
// the response was written; a degraded upstream is returned so the caller can
// fall back to the locally held view.
func (s *Repository) serveRemoteIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, channel string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	doc, err := s.service.RemoteMetadata(ctx, p, channelPub(feed, channel), artifact.Origin{Path: artifact.EscapePath(indexPath(channel))})
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(doc.Body)))
		sum := sha256.Sum256(doc.Body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		if doc.Stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		if r.Method != http.MethodHead {
			_, _ = w.Write(doc.Body)
		}
		return true, nil
	case errors.Is(err, artifact.ErrNotFound):
		if s.service.HasRemote(ctx, p, channelPub(feed, channel)) {
			failure(w, artifact.ErrNotFound) // upstream has no such channel and no local snapshot owns it
			return true, nil
		}
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	}
	failure(w, err)
	return true, nil
}

// remoteEntries returns the parsed upstream index of a channel.
func (s *Repository) remoteEntries(ctx context.Context, p identity.Principal, feed, channel string) (map[string]indexEntry, string, error) {
	doc, err := s.service.RemoteMetadata(ctx, p, channelPub(feed, channel), artifact.Origin{Path: artifact.EscapePath(indexPath(channel))})
	if err != nil {
		return nil, "", err
	}
	key := sha256.Sum256(append([]byte(doc.SourceID+"\x00"), doc.Body...))
	parsedIndexes.Lock()
	entries, ok := parsedIndexes.m[key]
	parsedIndexes.Unlock()
	if ok {
		return entries, doc.SourceID, nil
	}
	if entries, err = parseIndex(doc.Body); err != nil {
		return nil, "", artifact.ErrUnavailable
	}
	parsedIndexes.Lock()
	if len(parsedIndexes.order) >= 8 {
		delete(parsedIndexes.m, parsedIndexes.order[0])
		parsedIndexes.order = parsedIndexes.order[1:]
	}
	parsedIndexes.m[key] = entries
	parsedIndexes.order = append(parsedIndexes.order, key)
	parsedIndexes.Unlock()
	return entries, doc.SourceID, nil
}

// pullPackage materializes a package missing from the store. served is true when
// a streamed response was written; otherwise the package is cached and the
// caller repeats the local read.
func (s *Repository) pullPackage(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, arch string) (served bool, err error) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	entries, source, err := s.remoteEntries(ctx, p, pub.FeedID, pub.Namespace)
	cancel()
	if err != nil {
		return false, err
	}
	entry, ok := entries[pub.Version]
	if !ok || (entry.arch != "" && entry.arch != arch && entry.arch != "noarch") {
		return false, artifact.ErrNotFound
	}
	stream, again, err := s.service.PullThrough(r.Context(), p, pub, artifact.Origin{
		Path: artifact.EscapePath(pub.Namespace + "/" + pub.Version + ".apk"), SourceID: source,
		Verify: func(rs io.ReadSeeker) error {
			f, ok := rs.(*os.File)
			if !ok {
				return errInvalid
			}
			st, err := f.Stat()
			if err != nil || (entry.size >= 0 && st.Size() != entry.size) || st.Size() > maxPackageRemote {
				return errInvalid
			}
			block, err := inspect(f, st.Size(), pub.Version, arch)
			if err != nil || entryFields(block)["C"] != entry.checksum {
				return errInvalid
			}
			return nil
		},
	})
	if err != nil {
		return false, err
	}
	if again {
		return false, nil
	}
	stream.ServeStream(w, r.Method)
	return true, nil
}

// maxPackageRemote bounds a cached upstream package. Local publishing keeps the
// stricter maxPackage; Alpine packages such as linux firmware exceed it.
const maxPackageRemote = 1 << 30
