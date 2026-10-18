package oci

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
	"github.com/jackc/pgx/v5"
)

type referrerDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

func (r *Registry) referrers(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, subject string) {
	digest, ok := parseDigest(subject)
	if !ok {
		failure(w, 400, "DIGEST_INVALID", "invalid subject digest")
		return
	}
	q := req.URL.Query()
	limit := 100
	for key, values := range q {
		if len(values) != 1 || (key != "n" && key != "last" && key != "artifactType" && key != "source") {
			failure(w, 400, "UNSUPPORTED", "invalid referrers query")
			return
		}
	}
	if raw := q.Get("n"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 100 {
			failure(w, 400, "UNSUPPORTED", "invalid page size")
			return
		}
		limit = n
	}
	last := q.Get("last")
	if last != "" {
		d, valid := parseDigest(last)
		if !valid {
			failure(w, 400, "DIGEST_INVALID", "invalid cursor")
			return
		}
		last = string(d)
	}
	filter := q.Get("artifactType")
	if len(filter) > 256 {
		failure(w, 400, "UNSUPPORTED", "invalid artifact type")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	policy, err := (identity.Roles{DB: r.db}).Resolve(ctx, p)
	if err != nil {
		failService(w, artifact.ErrUnavailable)
		return
	}
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		failService(w, artifact.ErrUnavailable)
		return
	}
	defer rollback(tx)
	var component, source string
	var namespace *string
	err = tx.QueryRow(ctx, `SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`, p.InstanceID, rep.feed, rep.name).Scan(&component, &source, &namespace)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		failService(w, artifact.ErrUnavailable)
		return
	}
	if err == nil && (namespace == nil || *namespace != rep.namespace) {
		failService(w, artifact.ErrNotFound)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: rep.feed, SourceID: source, Namespace: rep.namespace, Kind: identity.ArtifactResource}, identity.Read) {
		failService(w, artifact.ErrNotFound)
		return
	}
	if pinned := q.Get("source"); pinned != "" && pinned != source {
		failService(w, artifact.ErrConflict)
		return
	}
	entries := []referrerDescriptor{}
	if component != "" {
		rows, e := tx.Query(ctx, `SELECT a.media_type,b.digest,b.size_bytes,s.artifact_type,s.annotations FROM artifact_subjects s JOIN assets a ON a.id=s.asset_id JOIN blobs b ON b.id=a.blob_id WHERE s.instance_id=$1 AND s.component_id=$2 AND s.subject_digest=$3 AND ($4='' OR s.artifact_type=$4) AND b.digest>$5 ORDER BY b.digest LIMIT $6`, p.InstanceID, component, string(digest), filter, last, limit+1)
		if e != nil {
			failService(w, artifact.ErrUnavailable)
			return
		}
		for rows.Next() {
			var d referrerDescriptor
			var annotations []byte
			if e = rows.Scan(&d.MediaType, &d.Digest, &d.Size, &d.ArtifactType, &annotations); e != nil {
				break
			}
			if len(annotations) > 64<<10 || json.Unmarshal(annotations, &d.Annotations) != nil {
				e = artifact.ErrUnavailable
				break
			}
			d.Digest = "sha256:" + d.Digest
			entries = append(entries, d)
		}
		rowErr := rows.Err()
		rows.Close()
		if e != nil || rowErr != nil {
			failService(w, artifact.ErrUnavailable)
			return
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
		next := url.Values{"n": {strconv.Itoa(limit)}, "last": {entries[len(entries)-1].Digest}, "source": {source}}
		if filter != "" {
			next.Set("artifactType", filter)
		}
		w.Header().Set("Link", "</v2/"+rep.full+"/referrers/"+subject+"?"+next.Encode()+">; rel=\"next\"")
	}
	if err = tx.Commit(ctx); err != nil {
		failService(w, artifact.ErrUnavailable)
		return
	}
	body, err := json.Marshal(struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Manifests     []referrerDescriptor `json:"manifests"`
	}{2, imageIndex, entries})
	if err != nil {
		failService(w, artifact.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", imageIndex)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if filter != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	w.WriteHeader(200)
	if req.Method != "HEAD" {
		_, _ = w.Write(body)
	}
}
