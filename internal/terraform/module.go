package terraform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const moduleArchive = "archive"

// modules routes the module registry protocol: rest is {ns}/{name}/{system}/...
func (s *Repository) modules(w http.ResponseWriter, r *http.Request, p identity.Principal, slug, feed string, rest []string) {
	if len(rest) < 4 || len(rest) > 5 || !namePattern.MatchString(rest[0]) || !namePattern.MatchString(rest[1]) || !systemPattern.MatchString(rest[2]) {
		failure(w, artifact.ErrNotFound)
		return
	}
	coordinate := "modules/" + rest[0] + "/" + rest[1] + "/" + rest[2]
	pub := artifact.Publication{FeedID: feed, Namespace: moduleNamespace, Coordinate: coordinate, AssetKey: moduleArchive, MediaType: "application/gzip"}
	if len(rest) == 4 {
		if rest[3] == "versions" && r.Method != "PUT" {
			s.moduleVersions(w, r, p, pub, rest)
			return
		}
		if r.Method == "PUT" && validVersion(rest[3]) {
			pub.Version = rest[3]
			s.moduleUpload(w, r, p, pub)
			return
		}
		failure(w, errInvalid)
		return
	}
	if !validVersion(rest[3]) {
		failure(w, errInvalid)
		return
	}
	pub.Version = rest[3]
	if r.Method == "PUT" {
		failureStatus(w, 405)
		return
	}
	switch rest[4] {
	case "download":
		// Resolve (and authorize) the exact version first; Terraform then fetches
		// the immutable archive URL named by X-Terraform-Get.
		subdir := ""
		_, e := s.service.Lookup(r.Context(), p, pub)
		if errors.Is(e, artifact.ErrNotFound) {
			// Local miss: cache the archive from the remote owner (a local
			// owner or an unknown version stays a 404).
			if subdir, e = s.fillModule(r.Context(), p, pub); e == nil {
				_, e = s.service.Lookup(r.Context(), p, pub)
			}
		} else if e == nil {
			subdir = s.moduleSubdir(r.Context(), p, pub)
		}
		if e != nil {
			failure(w, e)
			return
		}
		base := s.feedURL(slug)
		if base == "" {
			failure(w, artifact.ErrUnavailable)
			return
		}
		get := base + "/v1/modules/" + rest[0] + "/" + rest[1] + "/" + rest[2] + "/" + rest[3] + "/archive.tar.gz"
		if subdir != "" {
			get += "//" + subdir
		}
		w.Header().Set("X-Terraform-Get", get)
		w.WriteHeader(http.StatusNoContent)
	case "archive.tar.gz":
		if _, e := s.service.Lookup(r.Context(), p, pub); errors.Is(e, artifact.ErrNotFound) {
			if _, e = s.fillModule(r.Context(), p, pub); e != nil && !errors.Is(e, artifact.ErrNotFound) {
				failure(w, e)
				return
			}
		}
		s.serveAsset(w, r, p, pub)
	default:
		failure(w, artifact.ErrNotFound)
	}
}

func (s *Repository) moduleVersions(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, rest []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// Remote-owned (or unowned) modules are answered from upstream; a local
	// owner, or an upstream outage, falls through to the local composition.
	served, remoteErr := s.serveRemoteModuleVersions(w, r, p, pub, rest)
	if served {
		return
	}
	versions, e := s.listVersions(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, moduleArchive)
	if e != nil {
		if remoteErr != nil && errors.Is(e, artifact.ErrNotFound) {
			e = artifact.ErrUnavailable
		}
		failure(w, e)
		return
	}
	type entry struct {
		Version string `json:"version"`
	}
	list := make([]entry, 0, len(versions))
	for _, v := range versions {
		list = append(list, entry{v})
	}
	type module struct {
		Source   string  `json:"source"`
		Versions []entry `json:"versions"`
	}
	writeJSON(w, r, map[string][]module{"modules": {{Source: rest[0] + "/" + rest[1] + "/" + rest[2], Versions: list}}})
}

func (s *Repository) moduleUpload(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	source, e := s.prepare(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, pub.AssetKey)
	if e != nil {
		failure(w, e)
		return
	}
	if !s.acquire() {
		failure(w, artifact.ErrUnavailable)
		return
	}
	defer s.release()
	body, e := spool(ctx, r.Body, maxModuleArchive)
	if e != nil {
		failure(w, errInvalid)
		return
	}
	defer body.close()
	if want := r.Header.Get("X-Checksum-Sha256"); want != "" && want != body.info.digest {
		failure(w, blob.ErrIntegrity)
		return
	}
	if e = inspectModule(ctx, body.f); e != nil {
		failure(w, e)
		return
	}
	if _, e = body.f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	pub.ExpectedSource = source
	pub.Info = blob.Info{Size: body.info.size, Digest: blob.Digest(body.info.digest)}
	if _, e = s.service.PublishBundle(ctx, p, []artifact.Publication{pub}, []io.Reader{contextReader{ctx, body.f}}); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(http.StatusCreated)
}
