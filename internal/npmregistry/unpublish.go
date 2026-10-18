package npmregistry

// npm unpublish semantics on top of the shared policy-gated core.
//
//   - `npm unpublish pkg --force`: DELETE /{pkg}/-rev/{rev} removes every version.
//   - `npm unpublish pkg@1.0.0`: PUT /{pkg}/-rev/{rev} with the packument minus
//     the version, then DELETE /{pkg}/-/{file}.tgz/-rev/{rev} for the tarball.
//
// Every removal is authorized, policy-checked, audited and tombstoned by
// artifact.Service.Delete; bytes are reclaimed later by garbage collection.
// `npm deprecate` rewrites the packument without removing versions and is
// accepted as a no-op. Mutable dist-tags remain unsupported.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const maxUnpublishBody = 32 << 20

// unpublishRoute classifies the tail of a package route.
func unpublishRoute(tail string) (kind, file string, ok bool) {
	rest, found := strings.CutPrefix(tail, "-rev/")
	if found && rest != "" && !strings.Contains(rest, "/") {
		return "package", "", true
	}
	if base, rev, cut := strings.Cut(strings.TrimPrefix(tail, "-/"), "/-rev/"); strings.HasPrefix(tail, "-/") && cut && rev != "" && !strings.Contains(rev, "/") && base != "" && !strings.Contains(base, "/") {
		return "tarball", base, true
	}
	return "", "", false
}

func (s *Registry) unpublish(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, kind, file string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	target := func(version string) artifact.Target {
		return artifact.Target{FeedID: feed, Namespace: namespace(name), Coordinate: name, Version: version}
	}
	switch {
	case kind == "package" && r.Method == "DELETE":
		versions, e := s.localVersions(ctx, p, feed, name)
		if e == nil && len(versions) == 0 {
			e = artifact.ErrNotFound
		}
		for _, v := range versions {
			if e != nil {
				break
			}
			_, e = s.service.Delete(ctx, p, target(v))
		}
		if e != nil {
			failService(w, e)
			return
		}
	case kind == "package" && r.Method == "PUT":
		var doc struct {
			Versions map[string]json.RawMessage `json:"versions"`
		}
		if e := json.NewDecoder(io.LimitReader(r.Body, maxUnpublishBody)).Decode(&doc); e != nil || doc.Versions == nil {
			failService(w, errInvalid)
			return
		}
		versions, e := s.localVersions(ctx, p, feed, name)
		if e == nil && len(versions) == 0 {
			e = artifact.ErrNotFound
		}
		for _, v := range versions {
			if e != nil {
				break
			}
			if _, kept := doc.Versions[v]; !kept {
				_, e = s.service.Delete(ctx, p, target(v))
			}
		}
		if e != nil {
			failService(w, e)
			return
		}
	case kind == "tarball" && r.Method == "DELETE":
		prefix := pathBase(name) + "-"
		v := strings.TrimSuffix(strings.TrimPrefix(file, prefix), ".tgz")
		if !strings.HasPrefix(file, prefix) || !strings.HasSuffix(file, ".tgz") || !validVersion(v) {
			failure(w, 404, "tarball not found")
			return
		}
		// The client removes the version through the packument PUT first; a
		// missing version therefore means the work is already done.
		if _, e := s.service.Delete(ctx, p, target(v)); e != nil && !errors.Is(e, artifact.ErrNotFound) {
			failService(w, e)
			return
		}
	default:
		failure(w, 405, "unsupported npm operation")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": name, "rev": "1-ant"})
}

// localVersions lists the owning source's versions that carry package metadata.
func (s *Registry) localVersions(ctx context.Context, p identity.Principal, feed, name string) ([]string, error) {
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT v.version FROM owner o JOIN versions v ON v.component_id=o.id ORDER BY v.version LIMIT 10001`, p.InstanceID, feed, name)
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			return nil, artifact.ErrUnavailable
		}
		versions = append(versions, v)
	}
	if rows.Err() != nil || len(versions) > 10000 {
		return nil, artifact.ErrUnavailable
	}
	return versions, nil
}
