package cocoapods

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for the CocoaPods CDN layout (cdn.cocoapods.org style). Two kinds of
// reads are proxied:
//
//   - all_pods_versions_<a>_<b>_<c>.txt shards, read through the metadata cache and
//     merged by package owner: every pod name is served by exactly one source, and a
//     pod owned by a local source never takes versions from upstream.
//   - Specs/<a>/<b>/<c>/<Pod>/<ver>/<Pod>.podspec.json, cached on first read.
//
// Podspecs are served verbatim. Pod sources stay where the podspec says (git or
// http); only specs are proxied, so clients still reach those hosts directly.
// Archive routes (archives/...) serve only locally published archives.

// remoteShard fetches and validates one upstream shard for the given prefix.
// The returned map is pod name to its versions.
func (s *Repository) remoteShard(ctx context.Context, p identity.Principal, feed string, want [3]string) (map[string][]string, artifact.Document, error) {
	name := "all_pods_versions_" + want[0] + "_" + want[1] + "_" + want[2] + ".txt"
	// The coordinate only drives the owner lookup; a ':' can never be part of a pod name.
	doc, err := s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Coordinate: "index:" + name}, artifact.Origin{Path: name, Representation: "text/plain"})
	if err != nil {
		return nil, doc, err
	}
	out := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(doc.Body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), "/")
		if len(fields) < 2 || !validName(fields[0]) || shard(fields[0]) != want {
			continue
		}
		var versions []string
		for _, v := range fields[1:] {
			if validVersion(v) {
				versions = append(versions, v)
			}
		}
		if len(versions) > 0 {
			out[fields[0]] = versions
		}
		if len(out) > maxPods {
			return nil, doc, artifact.ErrUnavailable
		}
	}
	if sc.Err() != nil {
		return nil, doc, artifact.ErrUnavailable
	}
	return out, doc, nil
}

func specPath(pod, version string) string {
	sh := shard(pod)
	return artifact.EscapePath("Specs/" + sh[0] + "/" + sh[1] + "/" + sh[2] + "/" + pod + "/" + version + "/" + pod + ".podspec.json")
}

// pullSpec materializes a missing podspec. retry is true once it is in the store;
// served is true when a streamed response was written.
func (s *Repository) pullSpec(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, pod, version string) (retry, served bool, err error) {
	pub := artifact.Publication{FeedID: feed, Coordinate: pod, Version: version, AssetKey: podspecKey, MediaType: "application/json"}
	stream, again, err := s.service.PullThrough(r.Context(), p, pub, artifact.Origin{Path: specPath(pod, version)})
	if err != nil {
		return false, false, err
	}
	if again {
		return true, false, nil
	}
	stream.ServeStream(w, r.Method)
	return false, true, nil
}

func isNotFound(err error) bool { return errors.Is(err, artifact.ErrNotFound) }
