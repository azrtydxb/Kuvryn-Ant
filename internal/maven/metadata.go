package maven

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

type metadataXML struct {
	XMLName    xml.Name      `xml:"metadata"`
	Group      string        `xml:"groupId"`
	Artifact   string        `xml:"artifactId"`
	Versioning versioningXML `xml:"versioning"`
}
type versioningXML struct {
	Latest   string   `xml:"latest"`
	Release  string   `xml:"release"`
	Versions []string `xml:"versions>version"`
	Updated  string   `xml:"lastUpdated"`
}

func (r *Repository) metadata(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string) {
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	// Remote-owned (or unowned) artifacts: the upstream maven-metadata.xml and its
	// checksum sidecars are authoritative and carry no absolute URLs, so they are
	// proxied verbatim through the cached metadata path. Versions already cached
	// locally never replace the upstream version list; they only answer when the
	// upstream is unreachable.
	doc, remoteErr := r.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: feed, Namespace: l.group, Coordinate: l.group + ":" + l.name}, artifact.Origin{Path: artifact.EscapePath(l.key)})
	switch {
	case remoteErr == nil:
		w.Header().Set("Content-Type", doc.MediaType)
		if doc.MediaType == "" {
			w.Header().Set("Content-Type", "application/xml")
		}
		if doc.Stale {
			for k, v := range artifact.StaleHeaders() {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("ETag", `"sha256:`+string(info(doc.Body).Digest)+`"`)
		http.ServeContent(w, req, "", time.Time{}, bytes.NewReader(doc.Body))
		return
	case errors.Is(remoteErr, artifact.ErrNotFound):
		remoteErr = nil
	case !errors.Is(remoteErr, artifact.ErrUnavailable):
		fail(w, remoteErr)
		return
	default:
		w.Header().Set("X-Kuvryn-Cache", "offline")
	}
	r.localMetadata(w, req, p, l, feed, remoteErr)
}

// localMetadata composes maven-metadata.xml from the owning source's stored
// versions. upstreamErr is non-nil when an upstream outage forced this path.
func (r *Repository) localMetadata(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string, upstreamErr error) {
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	// One owner, one statement snapshot; never combine sources or fall back.
	rows, err := r.db.Query(ctx, `WITH owner AS (
 SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1
 ) SELECT o.source_id::text,v.version,v.created_at FROM owner o JOIN versions v ON v.component_id=o.id WHERE v.instance_id=$1 ORDER BY v.created_at,v.version LIMIT 10001`, p.InstanceID, feed, l.group+":"+l.name)
	if err != nil {
		fail(w, err)
		return
	}
	type version struct {
		name    string
		created time.Time
	}
	var versions []version
	source := ""
	for rows.Next() {
		var v version
		if err = rows.Scan(&source, &v.name, &v.created); err != nil {
			break
		}
		versions = append(versions, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close() // Release pool before authorization/storage calls.
	if err != nil {
		fail(w, err)
		return
	}
	if len(versions) == 0 {
		if upstreamErr != nil {
			fail(w, upstreamErr)
			return
		}
		fail(w, artifact.ErrNotFound)
		return
	}
	if len(versions) > 10000 {
		fail(w, artifact.ErrUnavailable)
		return
	}
	policy, err := (identity.Roles{DB: r.db}).Resolve(ctx, p)
	if err != nil {
		fail(w, err)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: l.group, Kind: identity.ArtifactResource}, identity.Read) {
		fail(w, artifact.ErrNotFound)
		return
	}
	m := metadataXML{Group: l.group, Artifact: l.name}
	for _, v := range versions {
		if ctx.Err() != nil {
			fail(w, ctx.Err())
			return
		}
		if !segment.MatchString(v.name) || strings.Contains(strings.ToUpper(v.name), "SNAPSHOT") {
			continue
		}
		pub := l.publication(feed)
		pub.Version = v.name
		pub.AssetKey = strings.ReplaceAll(l.group, ".", "/") + "/" + l.name + "/" + v.name + "/" + l.name + "-" + v.name + ".pom"
		rec, e := r.service.Lookup(ctx, p, pub)
		if e != nil {
			fail(w, e)
			return
		}
		if rec.SourceID != source {
			fail(w, artifact.ErrConflict)
			return
		}
		m.Versioning.Versions = append(m.Versioning.Versions, v.name)
		m.Versioning.Latest = v.name
		m.Versioning.Release = v.name
		m.Versioning.Updated = v.created.UTC().Format("20060102150405")
	}
	if len(m.Versioning.Versions) == 0 {
		fail(w, artifact.ErrNotFound)
		return
	}
	data, err := xml.Marshal(m)
	if err != nil || len(data) > 16<<20 {
		fail(w, artifact.ErrUnavailable)
		return
	}
	data = append([]byte(xml.Header), data...)
	w.Header().Set("Content-Type", "application/xml")
	switch checksumSuffix(l.file) {
	case ".sha256":
		data = []byte(info(data).Digest)
	case ".sha1":
		sum := sha1.Sum(data)
		data = []byte(hex.EncodeToString(sum[:]))
	case ".md5":
		sum := md5.Sum(data)
		data = []byte(hex.EncodeToString(sum[:]))
	}
	if checksumSuffix(l.file) != "" {
		w.Header().Set("Content-Type", "text/plain")
	}
	w.Header().Set("ETag", `"sha256:`+string(info(data).Digest)+`"`)
	http.ServeContent(w, req, "", time.Time{}, bytes.NewReader(data))
}
