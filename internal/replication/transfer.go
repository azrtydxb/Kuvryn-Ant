package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// ensureBlobs makes every referenced blob present and verified in the replica
// source's store before metadata that references it commits. A blob whose digest
// and size are already present is never fetched again: stores publish only
// digest-verified content, so presence is the dedup record that survives
// interruption and restart. A blob interrupted mid-transfer resumes from the
// bytes already staged (see transfer).
func (s *Service) ensureBlobs(ctx context.Context, rt *run, items []Item, d *delta) error {
	type want struct {
		digest string
		size   int64
	}
	var todo []want
	seen := map[string]bool{}
	for _, it := range items {
		if err := validItem(it); err != nil {
			return err
		}
		if seen[it.Digest] || rt.verified[it.Digest] {
			continue
		}
		seen[it.Digest] = true
		info, err := rt.store.Stat(ctx, blob.Digest(it.Digest))
		if err == nil && info.Size == it.Size {
			rt.verified[it.Digest] = true
			d.deduplicated++
			continue
		}
		if err != nil && !errors.Is(err, blob.ErrNotFound) {
			return ErrUnavailable
		}
		todo = append(todo, want{it.Digest, it.Size})
	}
	if len(todo) == 0 {
		return nil
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, s.cfg.BlobConcurrency)
	for _, w := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			streamed, resumed, err := s.transfer(gctx, rt, w.digest, w.size)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				if streamed > 0 {
					// Bytes that crossed the wire count even when the blob did not
					// complete; they are not part of any page commit.
					s.addStreamed(rt, streamed)
				}
				return
			}
			rt.verified[w.digest] = true
			d.transferred++
			d.bytes += streamed
			d.resumed += resumed
		}()
	}
	wg.Wait()
	return firstErr
}

// addStreamed records partially transferred bytes outside a page commit.
func (s *Service) addStreamed(rt *run, n int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.cfg.DB.Exec(ctx, `UPDATE replication_jobs SET bytes_transferred=bytes_transferred+$3 WHERE id=$1 AND lease_owner=$2`, rt.job.ID, s.owner, n)
}

// stageFor opens the staging object for digest. Stores that can keep a partial
// transfer (disk, or S3 with a resume state store) return it with its offset; others (and a busy or
// unsupported partial) fall back to a plain stage that always starts at zero.
func stageFor(ctx context.Context, store blob.Store, digest string) (blob.Upload, blob.ResumableUpload, error) {
	if r, ok := store.(blob.ResumableStager); ok {
		ru, err := r.StageResumable(ctx, digest)
		if err == nil {
			return ru, ru, nil
		}
		var capability *blob.CapabilityError
		if !errors.As(err, &capability) && !errors.Is(err, blob.ErrResumeBusy) {
			return nil, nil, err
		}
	}
	up, err := store.Stage(ctx)
	return up, nil, err
}

// transfer fetches one blob into a staging object and publishes it only after
// the digest and size verify. If a previous attempt left staged bytes, the
// request resumes from that offset with a Range request, so completed bytes are
// not transferred again. A peer that ignores the Range (or a partial that does
// not belong) restarts from zero. An attempt that does not complete keeps its
// staged bytes for the next one, except when the content proves corrupt, which
// discards them. streamed is the number of bytes received over the wire this
// attempt; resumed is the number of already-staged bytes that were not
// re-fetched.
func (s *Service) transfer(ctx context.Context, rt *run, digest string, size int64) (streamed, resumed int64, err error) {
	up, ru, err := stageFor(ctx, rt.store, digest)
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		return 0, 0, ErrUnavailable
	}
	// keep reports whether staged bytes survive this attempt (resumable only).
	keep := ru != nil
	defer func() {
		if s, ok := up.(blob.Suspender); ok && ru != nil && keep {
			_ = s.Suspend()
		} else {
			_ = up.Abort()
		}
	}()
	// restart discards the staged bytes and begins a fresh staging object.
	restart := func() error {
		_ = up.Abort()
		var e error
		if up, ru, e = stageFor(ctx, rt.store, digest); e != nil {
			up, ru = nopUpload{}, nil
			return ErrUnavailable
		}
		keep = ru != nil
		return nil
	}
	offset := int64(0)
	if ru != nil {
		if offset = ru.Offset(); offset > size {
			if err = restart(); err != nil {
				return 0, 0, err
			}
			offset = 0
		}
	}
	if offset == size {
		resumed = size
	} else {
		body, start, length, err := rt.client.Blob(ctx, rt.job.RemoteFeedID, digest, rt.job.IncludeCached, offset)
		if err != nil {
			return 0, 0, err
		}
		defer body.Close()
		if start != offset {
			// The peer sent the whole blob despite the Range request.
			if err = restart(); err != nil {
				return 0, 0, err
			}
			offset = 0
		}
		if length >= 0 && length != size-offset {
			keep = false
			return 0, 0, fmt.Errorf("%w: peer announced %d bytes, manifest says %d", ErrCorrupt, length+offset, size)
		}
		n, err := io.Copy(up, io.LimitReader(body, size-offset+1))
		streamed = n
		recordTransferBytes(n)
		if err != nil {
			if ctx.Err() != nil {
				return streamed, 0, ctx.Err()
			}
			return streamed, 0, fmt.Errorf("%w: transfer interrupted", ErrPeer)
		}
		if offset+n > size {
			keep = false
			return streamed, 0, fmt.Errorf("%w: received more than the %d bytes expected", ErrCorrupt, size)
		}
		if offset+n < size {
			// The stream ended early without an error: retryable, partial kept.
			return streamed, 0, fmt.Errorf("%w: transfer truncated at %d of %d bytes", ErrPeer, offset+n, size)
		}
		resumed = offset
	}
	if _, err = up.Finalize(ctx, blob.Info{Digest: blob.Digest(digest), Size: size}); err != nil {
		keep = false
		if errors.Is(err, blob.ErrIntegrity) {
			return streamed, 0, fmt.Errorf("%w: sha256 mismatch for %s", ErrCorrupt, digest)
		}
		if ctx.Err() != nil {
			return streamed, 0, ctx.Err()
		}
		return streamed, 0, ErrUnavailable
	}
	return streamed, resumed, nil
}

// nopUpload stands in for a staging object that could not be reopened.
type nopUpload struct{}

func (nopUpload) Write(p []byte) (int, error) { return len(p), nil }
func (nopUpload) Finalize(context.Context, blob.Info) (blob.Info, error) {
	return blob.Info{}, ErrUnavailable
}
func (nopUpload) Abort() error { return nil }
