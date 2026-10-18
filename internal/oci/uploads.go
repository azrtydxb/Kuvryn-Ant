package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func (r *Registry) syncRoot() error {
	f, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (r *Registry) createUpload(ctx context.Context, p identity.Principal, rep repository, source string) (id string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,510))", p.InstanceID); err != nil {
		return "", err
	}
	// Lock order must match artifact publication (writer lease, then feed
	// rows). Inserting the session first takes a FK key-share lock on the feed
	// row and deadlocks against a publish that already holds the lease.
	if err = r.requireLease(ctx, tx, p); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM oci_uploads WHERE instance_id=$1 AND state='active' AND expires_at>clock_timestamp()`, p.InstanceID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 64 {
		return "", errors.New("upload capacity exhausted")
	}
	if err = tx.QueryRow(ctx, `INSERT INTO oci_uploads(instance_id,principal_id,feed_id,source_id,repository,upload_root) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, p.InstanceID, p.PrincipalID, rep.feed, source, rep.name, r.rootID).Scan(&id); err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = r.root.Remove(id)
			_ = r.syncRoot()
		}
	}()
	file, err := r.root.OpenFile(id, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = r.syncRoot(); err != nil {
		return "", err
	}
	committed = true // Ambiguous commit must not delete a possibly committed session file.
	if err = commit(ctx, tx); err != nil {
		return "", err
	}
	return id, nil
}
func uploadHeaders(w http.ResponseWriter, rep repository, id string, offset int64) {
	w.Header().Set("Location", "/v2/"+rep.full+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	end := offset - 1
	if end < 0 {
		end = 0
	}
	w.Header().Set("Range", fmt.Sprintf("0-%d", end))
	w.Header().Set("Content-Length", "0")
}
func blobCreated(w http.ResponseWriter, rep repository, digest string) {
	w.Header().Set("Location", "/v2/"+rep.full+"/blobs/sha256:"+digest)
	w.Header().Set("Docker-Content-Digest", "sha256:"+digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(201)
}
func (r *Registry) upload(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, id string) {
	if id == "" && req.Method != "POST" {
		failure(w, 405, "UNSUPPORTED", "upload initialization requires POST")
		return
	}
	if id != "" && req.Method != "GET" && req.Method != "PATCH" && req.Method != "PUT" && req.Method != "DELETE" {
		failure(w, 405, "UNSUPPORTED", "upload method unavailable")
		return
	}
	source, err := r.service.Prepare(req.Context(), p, rep.feed, rep.namespace, rep.name, "blob")
	if err != nil {
		failService(w, err)
		return
	}
	if id == "" {
		// A mount that cannot be satisfied (unknown or foreign source
		// repository, missing blob) falls back to a regular upload session
		// (202), as the distribution spec requires; clients then push the bytes.
		if req.URL.Query().Get("mount") != "" && r.mount(w, req, p, rep, source) {
			return
		}
		for attempt := 0; ; attempt++ {
			id, err = r.createUpload(req.Context(), p, rep, source)
			var pg *pgconn.PgError
			if err == nil || attempt >= 4 || !errors.As(err, &pg) || (pg.Code != "40P01" && pg.Code != "40001") {
				break
			}
			time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
		}
		if err != nil {
			failure(w, 503, "UNKNOWN", "cannot create upload")
			return
		}
		next := req.Clone(req.Context())
		next.Method = "PATCH"
		if req.URL.Query().Get("digest") != "" {
			next.Method = "PUT"
		}
		r.upload(w, next, p, rep, id)
		return
	}
	var uuid pgtype.UUID
	if uuid.Scan(id) != nil || !uuid.Valid || uuid.String() != id {
		failure(w, 404, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}
	ctx := req.Context()
	lockCtx, lockCancel := context.WithTimeout(ctx, 5*time.Second)
	defer lockCancel()
	tx, err := r.db.Begin(lockCtx)
	if err != nil {
		failService(w, err)
		return
	}
	defer rollback(tx)
	var offset int64
	var target, state string
	var completed, staging *string
	err = tx.QueryRow(lockCtx, `SELECT offset_bytes,source_id,state,digest,staging FROM oci_uploads WHERE id=$1 AND instance_id=$2 AND principal_id=$3 AND feed_id=$4 AND repository=$5 AND upload_root=$6 AND expires_at>clock_timestamp() FOR UPDATE`, id, p.InstanceID, p.PrincipalID, rep.feed, rep.name, r.rootID).Scan(&offset, &target, &state, &completed, &staging)
	lockCancel()
	if errors.Is(err, pgx.ErrNoRows) {
		failure(w, 404, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}
	if err != nil {
		failService(w, err)
		return
	}
	if target != source {
		failure(w, 409, "DENIED", "upload destination changed")
		return
	}
	if state == "complete" && req.Method == "PUT" && completed != nil && req.URL.Query().Get("digest") == "sha256:"+*completed {
		blobCreated(w, rep, *completed)
		return
	}
	if state != "active" {
		failure(w, 404, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}
	if req.Method == "GET" {
		uploadHeaders(w, rep, id, offset)
		w.WriteHeader(204)
		return
	}
	if req.Method == "DELETE" {
		if err = r.requireLease(ctx, tx, p); err != nil {
			failService(w, err)
			return
		}
		if _, err = execute(ctx, tx, "UPDATE oci_uploads SET state='cancelled' WHERE id=$1", id); err != nil {
			failService(w, err)
			return
		}
		if err = commit(ctx, tx); err != nil {
			failService(w, err)
			return
		}
		if staging != nil && *staging == "resumable" {
			r.abortResumable(ctx, p.InstanceID, source, id)
		} else {
			_ = r.root.Remove(id)
			_ = r.syncRoot()
		}
		w.WriteHeader(204)
		return
	}
	if staging == nil && offset == 0 {
		// First bytes of a new session: stage in the store's resumable
		// storage when it supports it, otherwise in the local session file.
		if ru, rerr := r.service.ResumableStage(ctx, p.InstanceID, source, id); rerr == nil {
			r.uploadResumable(ctx, w, req, tx, p, rep, id, source, offset, ru)
			return
		}
	} else if staging != nil && *staging == "resumable" {
		ru, rerr := r.service.ResumableStage(ctx, p.InstanceID, source, id)
		if rerr != nil {
			failure(w, 503, "UNKNOWN", "upload staging unavailable")
			return
		}
		r.uploadResumable(ctx, w, req, tx, p, rep, id, source, offset, ru)
		return
	}
	digest := blob.Digest("")
	if req.Method == "PUT" {
		values := req.URL.Query()["digest"]
		if len(values) != 1 {
			failure(w, 400, "DIGEST_INVALID", "one digest required")
			return
		}
		var valid bool
		digest, valid = parseDigest(values[0])
		if !valid {
			failure(w, 400, "DIGEST_INVALID", "invalid digest")
			return
		}
	}
	expectedChunk := int64(-1)
	if values := req.Header.Values("Content-Range"); len(values) > 1 {
		failure(w, 416, "RANGE_INVALID", "invalid upload range")
		return
	} else if len(values) == 1 {
		parts := strings.Split(values[0], "-")
		if len(parts) != 2 {
			failure(w, 416, "RANGE_INVALID", "invalid upload range")
			return
		}
		start, e1 := strconv.ParseInt(parts[0], 10, 64)
		end, e2 := strconv.ParseInt(parts[1], 10, 64)
		if e1 != nil || e2 != nil || start != offset || end < start || end == MaxBlob || (req.ContentLength >= 0 && end-start+1 != req.ContentLength) {
			uploadHeaders(w, rep, id, offset)
			w.Header().Del("Content-Length")
			failure(w, 416, "RANGE_INVALID", "upload offset mismatch")
			return
		}
		expectedChunk = end - start + 1
	}
	if offset < 0 || offset > MaxBlob {
		failure(w, 503, "UNKNOWN", "invalid upload state")
		return
	}
	stagedInfo, statErr := r.root.Lstat(id)
	if statErr != nil || !stagedInfo.Mode().IsRegular() {
		failure(w, 503, "UNKNOWN", "upload staging unavailable")
		return
	}
	file, err := r.root.OpenFile(id, os.O_RDWR, 0600)
	if err != nil {
		failure(w, 503, "UNKNOWN", "upload staging unavailable")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < offset {
		failure(w, 503, "UNKNOWN", "upload staging unavailable")
		return
	}
	// Bytes after the committed offset belong to an interrupted request and are
	// discarded under the database row lock before resuming.
	if err = file.Truncate(offset); err != nil {
		failService(w, err)
		return
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		failService(w, err)
		return
	}
	saved := false
	defer func() {
		if !saved {
			_ = file.Truncate(offset)
			_ = file.Sync()
		}
	}()
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	limited := http.MaxBytesReader(w, req.Body, MaxBlob-offset)
	n, err := io.CopyBuffer(file, &progressReader{ctx: ctx, reader: limited, controller: controller}, make([]byte, 64<<10))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			failure(w, 413, "SIZE_INVALID", "upload exceeds addressable size")
		} else {
			failure(w, 400, "BLOB_UPLOAD_INVALID", "upload chunk interrupted")
		}
		return
	}
	if expectedChunk >= 0 && n != expectedChunk {
		failure(w, 416, "RANGE_INVALID", "upload range length mismatch")
		return
	}
	if n > MaxBlob-offset {
		failure(w, 413, "SIZE_INVALID", "blob exceeds size limit")
		return
	}
	if err = file.Sync(); err != nil {
		failService(w, err)
		return
	}
	if req.Method == "PATCH" {
		if err = r.requireLease(ctx, tx, p); err != nil {
			failService(w, err)
			return
		}
		if _, err = execute(ctx, tx, "UPDATE oci_uploads SET offset_bytes=$2,staging=COALESCE(staging,'file') WHERE id=$1", id, offset+n); err != nil {
			failService(w, err)
			return
		}
		saved = true // Keep fsynced bytes if commit outcome is ambiguous.
		if err = commit(ctx, tx); err != nil {
			failService(w, err)
			return
		}
		uploadHeaders(w, rep, id, offset+n)
		w.WriteHeader(202)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		failService(w, err)
		return
	}
	// Publish verifies SHA-256 while streaming to durable storage; avoid an
	// additional full-file hashing pass for large resumed uploads.
	publication := rep.publication(string(digest), "blob")
	publication.ExpectedSource = source
	publication.MediaType = "application/octet-stream"
	publication.Info = blob.Info{Digest: digest, Size: offset + n}
	if _, err = r.service.Publish(ctx, p, publication, &progressReader{ctx: ctx, reader: file}); err != nil {
		failService(w, err)
		return
	}
	if err = r.requireLease(ctx, tx, p); err != nil {
		failService(w, err)
		return
	}
	if _, err = execute(ctx, tx, "UPDATE oci_uploads SET state='complete',digest=$2,offset_bytes=$3,staging=COALESCE(staging,'file') WHERE id=$1", id, string(digest), offset+n); err != nil {
		failService(w, err)
		return
	}
	saved = true // The next request reconciles any ambiguous commit using DB offset.
	if err = commit(ctx, tx); err != nil {
		failService(w, err)
		return
	}
	_ = file.Close()
	_ = r.root.Remove(id)
	_ = r.syncRoot()
	blobCreated(w, rep, string(digest))
}

type progressReader struct {
	ctx        context.Context
	reader     io.Reader
	controller *http.ResponseController
}

func (p *progressReader) Read(data []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	if p.controller != nil {
		_ = p.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	}
	return p.reader.Read(data)
}
func (r *Registry) mount(w http.ResponseWriter, req *http.Request, p identity.Principal, destination repository, source string) bool {
	digest, ok := parseDigest(req.URL.Query().Get("mount"))
	if !ok {
		return false
	}
	origin, err := r.repository(req.Context(), p, req.URL.Query().Get("from"))
	if err != nil {
		return false
	}
	record, reader, err := r.service.Open(req.Context(), p, origin.publication(string(digest), "blob"), 0, -1)
	if err != nil {
		return false
	}
	defer reader.Close()
	publication := destination.publication(string(digest), "blob")
	publication.ExpectedSource = source
	publication.Info = record.Info
	publication.MediaType = "application/octet-stream"
	if _, err = r.service.Publish(req.Context(), p, publication, reader); err != nil {
		return false
	}
	blobCreated(w, destination, string(digest))
	return true
}

// ReapExpired removes expired upload staging in bounded batches. It must run
// periodically; completed/cancelled records expire with the same 24-hour TTL.
func (r *Registry) ReapExpired(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 200 {
		return 0, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT id,source_id,staging FROM oci_uploads WHERE expires_at<=clock_timestamp() AND instance_id=$2 AND upload_root=$3 ORDER BY expires_at FOR UPDATE SKIP LOCKED LIMIT $1`, limit, r.instance, r.rootID)
	if err != nil {
		return 0, err
	}
	var ids []string
	resumable := map[string]string{} // id -> source for resumable staging
	for rows.Next() {
		var id, source string
		var staging *string
		if err = rows.Scan(&id, &source, &staging); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		if staging != nil && *staging == "resumable" {
			resumable[id] = source
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if source, ok := resumable[id]; ok {
			r.abortResumable(ctx, r.instance, source, id)
		}
		if err = r.root.Remove(id); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		_ = r.root.Remove(id + ".tmp")
		if _, err = execute(ctx, tx, `DELETE FROM oci_uploads WHERE id=$1`, id); err != nil {
			return 0, err
		}
	}
	if err = r.syncRoot(); err != nil {
		return 0, err
	}
	if err = commit(ctx, tx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (r *Registry) requireLease(ctx context.Context, tx pgx.Tx, p identity.Principal) error {
	lease := r.service.Lease()
	if lease.InstanceID != p.InstanceID {
		return artifact.ErrDenied
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ownership.Require(bounded, tx, lease)
}

func commit(ctx context.Context, tx pgx.Tx) error {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return tx.Commit(bounded)
}
func execute(ctx context.Context, tx pgx.Tx, sql string, args ...any) (pgconn.CommandTag, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return tx.Exec(bounded, sql, args...)
}

// Run periodically reaps expired sessions owned by this instance and upload
// root. Each pass is bounded by ReapExpired's ten-second deadline and 200 rows;
// a failed pass is retried on the next tick without exposing storage errors.
func (r *Registry) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = r.ReapExpired(ctx, 200)
		}
	}
}
