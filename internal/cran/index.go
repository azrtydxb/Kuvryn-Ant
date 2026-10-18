package cran

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func newer(a, b string) bool {
	aa := strings.FieldsFunc(a, func(r rune) bool { return r == '.' || r == '-' })
	bb := strings.FieldsFunc(b, func(r rune) bool { return r == '.' || r == '-' })
	for i := 0; i < len(aa) || i < len(bb); i++ {
		x, y := "", ""
		if i < len(aa) {
			x = strings.TrimLeft(aa[i], "0")
		}
		if i < len(bb) {
			y = strings.TrimLeft(bb[i], "0")
		}
		if len(x) != len(y) {
			return len(x) > len(y)
		}
		if x != y {
			return x > y
		}
	}
	return a > b
}
func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, file string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,o.namespace,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='DESCRIPTION' LIMIT 4097`, p.InstanceID, feed, channel)
	if e != nil {
		failure(w, e)
		return
	}
	type entry struct{ source, namespace, version string }
	entries := []entry{}
	for rows.Next() {
		var x entry
		if e = rows.Scan(&x.source, &x.namespace, &x.version); e != nil {
			break
		}
		entries = append(entries, x)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	if len(entries) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if len(entries) == 0 {
		policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if e != nil {
			failure(w, e)
			return
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Namespace: channel, Kind: identity.ArtifactResource}, identity.Read) {
			failure(w, artifact.ErrDenied)
			return
		}
	}
	latest := map[string]map[string]string{}
	total := 0
	for _, x := range entries {
		if x.namespace != channel {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub := artifact.Publication{FeedID: feed, Namespace: x.namespace, Coordinate: channel, Version: x.version, AssetKey: "DESCRIPTION"}
		rec, body, e := s.service.Open(ctx, p, pub, 0, -1)
		if e != nil {
			failure(w, e)
			return
		}
		if rec.SourceID != x.source || rec.Info.Size > maxMetadata {
			body.Close()
			failure(w, artifact.ErrUnavailable)
			return
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, body}, maxMetadata+1))
		body.Close()
		total += len(data)
		var m map[string]string
		if e != nil || len(data) > maxMetadata || total > 8<<20 || json.Unmarshal(data, &m) != nil || !validName(m["Package"]) || !validVersion(m["Version"]) || m["Package"]+"_"+m["Version"] != x.version || len(m["MD5sum"]) != 32 {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub.AssetKey = "archive"
		archive, e := s.service.Lookup(ctx, p, pub)
		if e != nil {
			failure(w, e)
			return
		}
		if archive.SourceID != x.source {
			failure(w, artifact.ErrConflict)
			return
		}
		old := latest[m["Package"]]
		if old == nil || newer(m["Version"], old["Version"]) {
			latest[m["Package"]] = m
		}
	}
	names := make([]string, 0, len(latest))
	for n := range latest {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		m := latest[n]
		for _, key := range []string{"Package", "Version", "Depends", "Imports", "LinkingTo", "Suggests", "Enhances", "License", "OS_type", "Archs", "MD5sum", "NeedsCompilation"} {
			if v := m[key]; v != "" {
				b.WriteString(key + ": " + v + "\n")
			}
		}
		b.WriteByte('\n')
	}
	data := b.Bytes()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if file == "PACKAGES.gz" {
		var z bytes.Buffer
		g := gzip.NewWriter(&z)
		_, _ = g.Write(data)
		_ = g.Close()
		data = z.Bytes()
		w.Header().Set("Content-Type", "application/gzip")
	}
	w.Header().Set("ETag", `"`+string(info(data).Digest)+`"`)
	http.ServeContent(w, r, file, time.Time{}, bytes.NewReader(data))
}
