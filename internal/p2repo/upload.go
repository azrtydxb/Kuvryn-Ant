package p2repo

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// ingest accepts a zipped p2 repository (content + artifacts metadata and the
// artifact files), verifies every artifact checksum and IU artifact closure, stores
// artifacts immutably, and finally commits one manifest that atomically exposes the
// whole upload in the merged indexes.
func (s *Repository) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, slug string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e == nil && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Admin) {
		e = artifact.ErrDenied
	}
	var source string
	if e == nil {
		source, e = s.service.Prepare(ctx, p, feed, namespace, manifestCoordinate, "manifest.json")
	}
	cancel()
	if e != nil {
		failure(w, e)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	f, e := os.CreateTemp("", "ant-p2-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	n, e := io.CopyBuffer(f, io.LimitReader(contextReader{r.Context(), r.Body}, maxUpload+1), make([]byte, 64<<10))
	if e != nil || n > maxUpload || n == 0 {
		failure(w, errInvalid)
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	zr, e := zip.NewReader(f, n)
	if e != nil {
		failure(w, errInvalid)
		return
	}
	idx, e := zipIndex(zr)
	if e != nil {
		failure(w, e)
		return
	}
	cdata, e := metadataXML(idx, "content")
	if e != nil {
		failure(w, e)
		return
	}
	adata, e := metadataXML(idx, "artifacts")
	if e != nil {
		failure(w, e)
		return
	}
	units, e := parseUnits(cdata)
	if e != nil {
		failure(w, e)
		return
	}
	descs, e := parseArtifacts(adata)
	if e != nil {
		failure(w, e)
		return
	}
	if len(units) == 0 && len(descs) == 0 {
		failure(w, errInvalid)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, e := s.load(ctx, p, feed)
	if e != nil {
		failure(w, e)
		return
	}
	man := manifest{}
	seenU := map[[2]string]bool{}
	for _, u := range units {
		k := [2]string{u.ID, u.Version}
		if seenU[k] {
			failure(w, errInvalid)
			return
		}
		seenU[k] = true
		if old, ok := st.units[k]; ok && old.XML != string(u.XML) {
			failure(w, artifact.ErrConflict)
			return
		}
		man.Units = append(man.Units, mUnit{u.ID, u.Version, string(u.XML)})
	}
	// Verify artifacts and stage nothing until every check passes.
	type item struct {
		d    artDesc
		file *zip.File
		size int64
		sha  string
	}
	var items []item
	have := map[[3]string]bool{}
	for _, d := range descs {
		k := [3]string{d.Classifier, d.ID, d.Version}
		if have[k] {
			failure(w, errInvalid)
			return
		}
		have[k] = true
		zf := idx[entryPath(d.Classifier, d.ID, d.Version)]
		if zf == nil {
			failure(w, errInvalid)
			return
		}
		data, e := readEntry(zf, 1<<30)
		if e != nil {
			failure(w, e)
			return
		}
		sha, md := hashes(data)
		if (d.SHA256 != "" && d.SHA256 != sha) || (d.MD5 != "" && d.MD5 != md) || (d.Size >= 0 && d.Size != int64(len(data))) {
			failure(w, blob.ErrIntegrity)
			return
		}
		if old, ok := st.arts[k]; ok && (old.SHA256 != sha || old.XML != string(d.XML)) {
			failure(w, artifact.ErrConflict)
			return
		}
		items = append(items, item{d, zf, int64(len(data)), sha})
		man.Artifacts = append(man.Artifacts, mArt{d.Classifier, d.ID, d.Version, string(d.XML), sha, int64(len(data))})
	}
	for _, u := range units {
		for _, ref := range u.Refs {
			if _, ok := st.arts[ref]; !ok && !have[ref] {
				failure(w, errInvalid)
				return
			}
		}
	}
	for _, it := range items {
		jar, e := readEntry(it.file, 1<<30)
		if e != nil {
			failure(w, e)
			return
		}
		coord := "artifact/" + it.d.Classifier + "/" + it.d.ID
		base := artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: coord, Version: it.d.Version, ExpectedSource: source}
		jp, dp := base, base
		jp.AssetKey, jp.MediaType, jp.Info = "jar", "application/java-archive", blob.Info{Size: it.size, Digest: blob.Digest(it.sha)}
		dp.AssetKey, dp.MediaType, dp.Info = "descriptor", "application/xml", info(it.d.XML)
		if _, e = s.service.PublishBundle(ctx, p, []artifact.Publication{jp, dp}, []io.Reader{bytes.NewReader(jar), bytes.NewReader(it.d.XML)}); e != nil {
			failure(w, e)
			return
		}
	}
	sort.Slice(man.Units, func(i, j int) bool {
		a, b := man.Units[i], man.Units[j]
		return a.ID < b.ID || a.ID == b.ID && a.Version < b.Version
	})
	sort.Slice(man.Artifacts, func(i, j int) bool {
		a, b := man.Artifacts[i], man.Artifacts[j]
		if a.Classifier != b.Classifier {
			return a.Classifier < b.Classifier
		}
		return a.ID < b.ID || a.ID == b.ID && a.Version < b.Version
	})
	mb, e := json.Marshal(man)
	if e != nil || len(mb) > maxManifest {
		failure(w, errInvalid)
		return
	}
	mi := info(mb)
	pub := artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: manifestCoordinate, Version: string(mi.Digest), AssetKey: "manifest.json", MediaType: "application/json", Info: mi, ExpectedSource: source}
	if _, e = s.service.PublishBundle(ctx, p, []artifact.Publication{pub}, []io.Reader{bytes.NewReader(mb)}); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
}
