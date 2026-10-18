package blob

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// PartState records one uploaded multipart part.
type PartState struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag"`
}

// UploadState is the durable progress of one resumable multipart upload. Size is
// the number of leading bytes already stored in completed parts; Hash is the
// marshalled SHA-256 state over exactly those bytes. Bytes beyond Size lived only
// in the process buffer and must be sent again after a restart.
type UploadState struct {
	Key       string      `json:"key"`
	UploadID  string      `json:"upload_id"`
	PartSize  int64       `json:"part_size"`
	Size      int64       `json:"size"`
	Parts     []PartState `json:"parts"`
	Hash      []byte      `json:"hash"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// StateStore durably persists UploadState by caller-chosen upload identifier.
// Implementations must make Save atomic: a crash leaves either the previous or
// the new state, never a torn one.
type StateStore interface {
	Save(ctx context.Context, id string, state UploadState) error
	// Load reports ok=false when no state exists.
	Load(ctx context.Context, id string) (state UploadState, ok bool, err error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) (map[string]UploadState, error)
}

var stateIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ErrInvalidUploadID rejects resumable upload identifiers that are not safe file names.
var ErrInvalidUploadID = errors.New("invalid resumable upload identifier")

// FileStateStore keeps one JSON file per upload in a private directory.
type FileStateStore struct {
	Dir string
	mu  sync.Mutex
}

func (f *FileStateStore) path(id string) (string, error) {
	if !stateIDPattern.MatchString(id) || strings.HasSuffix(id, ".tmp") {
		return "", ErrInvalidUploadID
	}
	return filepath.Join(f.Dir, id+".json"), nil
}

func (f *FileStateStore) Save(ctx context.Context, id string, st UploadState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := f.path(id)
	if err != nil {
		return err
	}
	body, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err = os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.Dir, "."+id+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(body)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	return syncDirectory(f.Dir)
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (f *FileStateStore) Load(ctx context.Context, id string) (UploadState, bool, error) {
	if err := ctx.Err(); err != nil {
		return UploadState{}, false, err
	}
	path, err := f.path(id)
	if err != nil {
		return UploadState{}, false, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return UploadState{}, false, nil
	}
	if err != nil {
		return UploadState{}, false, err
	}
	var st UploadState
	if err = json.Unmarshal(body, &st); err != nil {
		// A torn or foreign file is treated as absent: the upload restarts.
		return UploadState{}, false, nil
	}
	return st, true, nil
}

func (f *FileStateStore) Delete(ctx context.Context, id string) error {
	path, err := f.path(id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (f *FileStateStore) List(ctx context.Context) (map[string]UploadState, error) {
	entries, err := os.ReadDir(f.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]UploadState{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]UploadState{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		if st, found, err := f.Load(ctx, id); err == nil && found {
			out[id] = st
		}
	}
	return out, nil
}

// ResumableUpload is an Upload whose first Offset bytes are already durable.
// After a restart the caller must send the content again starting at Offset.
type ResumableUpload interface {
	Upload
	Offset() int64
}

// ResumableStager is implemented by stores that can resume interrupted uploads.
type ResumableStager interface {
	StageResumable(ctx context.Context, id string) (ResumableUpload, error)
}

// StaleUploadAborter is implemented by stores that can reap abandoned multipart
// uploads. It returns how many uploads were (or, with dryRun, would be) aborted.
type StaleUploadAborter interface {
	AbortStaleUploads(ctx context.Context, olderThan time.Duration, dryRun bool) (int, error)
}

type resumableS3 struct{ *s3Upload }

func (r resumableS3) Offset() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persistedSize()
}

func (u *s3Upload) persistedSize() int64 { return u.size - int64(u.used) }

// persist writes the resume state; the caller holds u.mu and has just flushed.
func (u *s3Upload) persist(ctx context.Context) error {
	if u.stateID == "" || u.store.config.StateStore == nil {
		return nil
	}
	marshaler, ok := u.digest.(encoding.BinaryMarshaler)
	if !ok {
		return &CapabilityError{"resumable digest state"}
	}
	// The digest has consumed u.size bytes; at flush time the buffer is fully
	// uploaded (used was just reset), so Size equals the digested byte count.
	hashState, err := marshaler.MarshalBinary()
	if err != nil {
		return err
	}
	st := UploadState{Key: u.key, UploadID: u.id, PartSize: u.store.config.PartSize, Size: u.size, Hash: hashState, CreatedAt: u.createdAt, UpdatedAt: u.store.now()}
	for _, p := range u.parts {
		st.Parts = append(st.Parts, PartState{Number: aws.ToInt32(p.PartNumber), ETag: aws.ToString(p.ETag)})
	}
	if err = u.store.config.StateStore.Save(ctx, u.stateID, st); err != nil {
		return fmt.Errorf("persist upload state: %w", ErrBackend)
	}
	return nil
}

// StageResumable starts or resumes the upload named id. A saved state is reused
// only if the object store still holds exactly the recorded parts; otherwise the
// stale remote upload is aborted and a fresh one starts at offset 0. Progress is
// saved after every uploaded part.
func (s *S3) StageResumable(ctx context.Context, id string) (ResumableUpload, error) {
	store := s.config.StateStore
	if store == nil {
		return nil, &CapabilityError{"resumable upload state store"}
	}
	if !stateIDPattern.MatchString(id) {
		return nil, ErrInvalidUploadID
	}
	st, found, err := store.Load(ctx, id)
	if err != nil {
		return nil, ErrBackend
	}
	if found {
		if u, ok := s.tryResume(ctx, id, st); ok {
			return resumableS3{u}, nil
		}
		s.abortRemote(ctx, st.Key, st.UploadID)
		if err = store.Delete(ctx, id); err != nil {
			return nil, ErrBackend
		}
	}
	plain, err := s.Stage(ctx)
	if err != nil {
		return nil, err
	}
	u := plain.(*s3Upload)
	u.stateID = id
	// Persist immediately so a crash before the first part still leaves a record
	// that GC can use to find and abort the remote upload.
	u.mu.Lock()
	err = u.persist(ctx)
	u.mu.Unlock()
	if err != nil {
		_ = u.Abort()
		return nil, err
	}
	return resumableS3{u}, nil
}

func (s *S3) tryResume(ctx context.Context, id string, st UploadState) (*s3Upload, bool) {
	if st.PartSize != s.config.PartSize || st.UploadID == "" || !strings.HasPrefix(st.Key, s.key("staging", "")) ||
		int64(len(st.Parts)) > s3MaximumParts || st.Size != int64(len(st.Parts))*st.PartSize {
		return nil, false
	}
	hasher := sha256.New()
	unmarshaler, ok := hasher.(encoding.BinaryUnmarshaler)
	if !ok || unmarshaler.UnmarshalBinary(st.Hash) != nil {
		return nil, false
	}
	// Verify the remote upload still holds the recorded parts.
	remote := map[int32]string{}
	var marker *string
	for {
		out, err := s.client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(st.Key), UploadId: aws.String(st.UploadID), PartNumberMarker: marker})
		if err != nil || out == nil {
			return nil, false
		}
		for _, p := range out.Parts {
			remote[aws.ToInt32(p.PartNumber)] = aws.ToString(p.ETag)
		}
		if !aws.ToBool(out.IsTruncated) || out.NextPartNumberMarker == nil {
			break
		}
		marker = out.NextPartNumberMarker
	}
	parts := make([]types.CompletedPart, 0, len(st.Parts))
	for i, p := range st.Parts {
		if p.Number != int32(i+1) || remote[p.Number] != p.ETag || p.ETag == "" {
			return nil, false
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag)})
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	return &s3Upload{store: s, ctx: ctx, key: st.Key, id: st.UploadID, buffer: make([]byte, s.config.PartSize), size: st.Size, digest: hasher, parts: parts, stateID: id, createdAt: st.CreatedAt}, true
}

func (s *S3) abortRemote(ctx context.Context, key, uploadID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID)})
	if err != nil && !hasS3Code(err, "NoSuchUpload") {
		return s3Error(err)
	}
	return nil
}

// AbortStaleUploads aborts multipart uploads that were idle for longer than
// olderThan: persisted resumable uploads whose state was not updated since, and
// leaked uploads under this store's staging and publication prefixes that have
// no fresh persisted state. Live resumable uploads are never touched.
func (s *S3) AbortStaleUploads(ctx context.Context, olderThan time.Duration, dryRun bool) (int, error) {
	if olderThan <= 0 {
		return 0, ErrRange
	}
	cutoff := s.now().Add(-olderThan)
	live := map[string]bool{}
	var failures []error
	aborted := 0
	stagingPrefix := s.key("staging", "")
	if store := s.config.StateStore; store != nil {
		states, err := store.List(ctx)
		if err != nil {
			return 0, ErrBackend
		}
		ids := make([]string, 0, len(states))
		for id := range states {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			st := states[id]
			if !strings.HasPrefix(st.Key, stagingPrefix) {
				continue // belongs to another domain's store
			}
			if st.UpdatedAt.After(cutoff) {
				live[st.UploadID] = true
				continue
			}
			aborted++
			live[st.UploadID] = true // already counted; skip when listing remote uploads
			if dryRun {
				continue
			}
			if err := s.abortRemote(ctx, st.Key, st.UploadID); err != nil {
				failures = append(failures, err)
				aborted--
				continue
			}
			if err := store.Delete(ctx, id); err != nil {
				failures = append(failures, ErrBackend)
			}
		}
	}
	for _, prefix := range []string{stagingPrefix, s.key("objects", "")} {
		var keyMarker, idMarker *string
		for {
			out, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(s.config.Bucket), Prefix: aws.String(prefix), KeyMarker: keyMarker, UploadIdMarker: idMarker})
			if err != nil {
				failures = append(failures, s3Error(err))
				break
			}
			for _, up := range out.Uploads {
				if up.Initiated == nil || up.Initiated.After(cutoff) || live[aws.ToString(up.UploadId)] {
					continue
				}
				aborted++
				if dryRun {
					continue
				}
				if err := s.abortRemote(ctx, aws.ToString(up.Key), aws.ToString(up.UploadId)); err != nil {
					failures = append(failures, err)
					aborted--
				}
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
		}
	}
	return aborted, errors.Join(failures...)
}

var (
	_ ResumableStager    = (*S3)(nil)
	_ StaleUploadAborter = (*S3)(nil)
	_ StateStore         = (*FileStateStore)(nil)
)
