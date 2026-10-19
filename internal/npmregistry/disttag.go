package npmregistry

// npm dist-tag add/rm: PUT /-/package/{pkg}/dist-tags/{tag} with a JSON string
// body naming the version, and DELETE of the same path. Tags are core pointers
// (artifact.Service.SetTag/RemoveTag), authorized by the publish action,
// audited and replicated like any other mutation.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

var tagRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

func (s *Registry) distTag(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, tag string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if !tagRE.MatchString(tag) || validVersion(tag) {
		failure(w, 400, "invalid dist-tag name")
		return
	}
	t := artifact.Target{FeedID: feed, Namespace: namespace(name), Coordinate: name}
	var e error
	if r.Method == "DELETE" {
		_, e = s.service.RemoveTag(ctx, p, t, tag)
	} else {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		var version string
		if err != nil || json.Unmarshal(raw, &version) != nil {
			// Some clients send the bare version rather than a JSON string.
			version = strings.Trim(strings.TrimSpace(string(raw)), `"`)
		}
		if !validVersion(version) {
			failure(w, 400, "dist-tag value must be a version")
			return
		}
		t.Version = version
		_, e = s.service.SetTag(ctx, p, t, tag)
	}
	if e != nil {
		if errors.Is(e, artifact.ErrConflict) {
			failure(w, 409, "package is not owned by a local source")
			return
		}
		failService(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": name, "dist-tags": map[string]string{tag: t.Version}})
}
