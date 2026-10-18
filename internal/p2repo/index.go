package p2repo

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/ulikunitz/xz"
)

type mUnit struct{ ID, Version, XML string }
type mArt struct {
	Classifier, ID, Version, XML, SHA256 string
	Size                                 int64
}
type manifest struct {
	Units     []mUnit
	Artifacts []mArt
}
type state struct {
	units map[[2]string]mUnit
	arts  map[[3]string]mArt
	stamp int64
}

// load merges every committed manifest. A publication is visible only once its
// manifest exists, so partially uploaded content is never indexed.
func (s *Repository) load(ctx context.Context, p identity.Principal, feed string) (*state, error) {
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,o.namespace,v.version,(extract(epoch from v.created_at)*1000)::bigint FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='manifest.json' ORDER BY v.created_at,v.version LIMIT 1025`, p.InstanceID, feed, manifestCoordinate)
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	type entry struct {
		source, ns, version string
		stamp               int64
	}
	var entries []entry
	for rows.Next() {
		var x entry
		if e = rows.Scan(&x.source, &x.ns, &x.version, &x.stamp); e != nil {
			break
		}
		entries = append(entries, x)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil || len(entries) > 1024 {
		return nil, artifact.ErrUnavailable
	}
	if len(entries) == 0 {
		policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if e != nil {
			return nil, artifact.ErrUnavailable
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Namespace: namespace, Kind: identity.ArtifactResource}, identity.Read) {
			return nil, artifact.ErrDenied
		}
	}
	st := &state{units: map[[2]string]mUnit{}, arts: map[[3]string]mArt{}}
	total := 0
	for _, x := range entries {
		if x.ns != namespace {
			return nil, artifact.ErrUnavailable
		}
		rec, body, e := s.service.Open(ctx, p, artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: manifestCoordinate, Version: x.version, AssetKey: "manifest.json"}, 0, -1)
		if e != nil {
			return nil, e
		}
		if rec.SourceID != x.source || rec.Info.Size > maxManifest {
			body.Close()
			return nil, artifact.ErrUnavailable
		}
		data, e := readLimited(contextReader{ctx, body}, maxManifest)
		body.Close()
		total += len(data)
		var m manifest
		if e != nil || total > maxManifest || json.Unmarshal(data, &m) != nil {
			return nil, artifact.ErrUnavailable
		}
		for _, u := range m.Units {
			k := [2]string{u.ID, u.Version}
			if old, ok := st.units[k]; ok && old.XML != u.XML {
				return nil, artifact.ErrUnavailable
			}
			st.units[k] = u
		}
		for _, a := range m.Artifacts {
			k := [3]string{a.Classifier, a.ID, a.Version}
			if old, ok := st.arts[k]; ok && old.XML != a.XML {
				return nil, artifact.ErrUnavailable
			}
			st.arts[k] = a
		}
		if x.stamp > st.stamp {
			st.stamp = x.stamp
		}
	}
	return st, nil
}

func esc(s string) string { return html.EscapeString(s) }

func (st *state) contentXML(name string) []byte {
	keys := make([][2]string, 0, len(st.units))
	for k := range st.units {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	var b bytes.Buffer
	b.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n<?metadataRepository class='org.eclipse.equinox.internal.p2.metadata.repository.LocalMetadataRepository' version='1.1.0'?>\n")
	b.WriteString("<repository name='" + esc(name) + "' type='org.eclipse.equinox.internal.p2.metadata.repository.LocalMetadataRepository' version='1'>\n")
	b.WriteString("  <properties size='2'>\n    <property name='p2.timestamp' value='" + strconv.FormatInt(st.stamp, 10) + "'/>\n    <property name='p2.compressed' value='true'/>\n  </properties>\n")
	b.WriteString("  <units size='" + strconv.Itoa(len(keys)) + "'>\n")
	for _, k := range keys {
		b.WriteString(st.units[k].XML)
		b.WriteByte('\n')
	}
	b.WriteString("  </units>\n</repository>\n")
	return b.Bytes()
}

func (st *state) artifactsXML(name string) []byte {
	keys := make([][3]string, 0, len(st.arts))
	for k := range st.arts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		for n := 0; n < 3; n++ {
			if keys[i][n] != keys[j][n] {
				return keys[i][n] < keys[j][n]
			}
		}
		return false
	})
	var b bytes.Buffer
	b.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n<?artifactRepository class='org.eclipse.equinox.internal.p2.artifact.repository.simple.SimpleArtifactRepository' version='1.1.0'?>\n")
	b.WriteString("<repository name='" + esc(name) + "' type='org.eclipse.equinox.p2.artifact.repository.simpleRepository' version='1'>\n")
	b.WriteString("  <properties size='2'>\n    <property name='p2.timestamp' value='" + strconv.FormatInt(st.stamp, 10) + "'/>\n    <property name='p2.compressed' value='true'/>\n  </properties>\n")
	b.WriteString("  <mappings size='3'>\n")
	b.WriteString("    <rule filter='(&amp; (classifier=osgi.bundle))' output='${repoUrl}/plugins/${id}_${version}.jar'/>\n")
	b.WriteString("    <rule filter='(&amp; (classifier=binary))' output='${repoUrl}/binary/${id}_${version}'/>\n")
	b.WriteString("    <rule filter='(&amp; (classifier=org.eclipse.update.feature))' output='${repoUrl}/features/${id}_${version}.jar'/>\n")
	b.WriteString("  </mappings>\n  <artifacts size='" + strconv.Itoa(len(keys)) + "'>\n")
	for _, k := range keys {
		b.WriteString(st.arts[k].XML)
		b.WriteByte('\n')
	}
	b.WriteString("  </artifacts>\n</repository>\n")
	return b.Bytes()
}

func jarOf(name string, data []byte) []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	_, _ = f.Write(data)
	_ = z.Close()
	return b.Bytes()
}
func xzOf(data []byte) []byte {
	var b bytes.Buffer
	w, _ := xz.NewWriter(&b)
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes()
}

const p2Index = "version=1\nmetadata.repository.factory.order=content.xml.xz,content.jar,content.xml,!\nartifact.repository.factory.order=artifacts.xml.xz,artifacts.jar,artifacts.xml,!\n"

func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, slug, file string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, e := s.load(ctx, p, feed)
	if e != nil {
		failure(w, e)
		return
	}
	var data []byte
	ct := "application/xml"
	switch file {
	case "p2.index":
		data, ct = []byte(p2Index), "text/plain; charset=utf-8"
	case "content.xml":
		data = st.contentXML(slug)
	case "content.jar":
		data, ct = jarOf("content.xml", st.contentXML(slug)), "application/java-archive"
	case "content.xml.xz":
		data, ct = xzOf(st.contentXML(slug)), "application/x-xz"
	case "artifacts.xml":
		data = st.artifactsXML(slug)
	case "artifacts.jar":
		data, ct = jarOf("artifacts.xml", st.artifactsXML(slug)), "application/java-archive"
	case "artifacts.xml.xz":
		data, ct = xzOf(st.artifactsXML(slug)), "application/x-xz"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", `"`+string(info(data).Digest)+`"`)
	http.ServeContent(w, r, file, time.Time{}, bytes.NewReader(data))
}
