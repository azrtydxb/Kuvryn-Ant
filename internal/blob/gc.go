package blob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object describes one stored blob together with its backend modification time.
type Object struct {
	Info
	ModTime time.Time
}

// Deleter is implemented by stores that can remove published objects. Delete is
// idempotent: a missing object is not an error. Only the retention service, which
// owns reachability decisions, may call it.
type Deleter interface {
	Delete(context.Context, Digest) error
}

// Lister is implemented by stores that can enumerate published objects in
// ascending digest order. List returns at most limit objects whose digest is
// strictly greater than after (empty after starts from the beginning). Listing
// is used only to discover unreferenced bytes, never as a source of truth.
type Lister interface {
	List(ctx context.Context, after Digest, limit int) ([]Object, error)
}

// Delete removes a published object and syncs the directory.
func (d *Disk) Delete(ctx context.Context, digest Digest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !digest.Valid() {
		return ErrInvalidDigest
	}
	if err := d.safeDir("objects"); err != nil {
		return err
	}
	if err := d.root.Remove(filepath.Join("objects", string(digest))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.syncDir("objects")
}

// List enumerates regular objects with valid digest names in ascending order.
func (d *Disk) List(ctx context.Context, after Digest, limit int) ([]Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, ErrRange
	}
	if err := d.safeDir("objects"); err != nil {
		return nil, err
	}
	dir, err := d.root.Open("objects")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out []Object
	for _, name := range names {
		if !Digest(name).Valid() || name <= string(after) {
			continue
		}
		info, err := d.root.Lstat(filepath.Join("objects", name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, Object{Info{Digest(name), info.Size()}, info.ModTime()})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type s3Lister interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Delete removes an object. S3 DeleteObject succeeds for missing keys.
func (s *S3) Delete(ctx context.Context, digest Digest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !digest.Valid() {
		return ErrInvalidDigest
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(s.key("objects", string(digest)))})
	if err != nil && !hasS3Code(err, "NoSuchKey", "NotFound", "404") {
		return s3Error(err)
	}
	return nil
}

// List enumerates objects below the objects/ prefix in key (digest) order.
func (s *S3) List(ctx context.Context, after Digest, limit int) ([]Object, error) {
	lister, ok := s.client.(s3Lister)
	if !ok {
		return nil, &CapabilityError{"object listing"}
	}
	if limit < 1 {
		return nil, ErrRange
	}
	prefix := s.key("objects", "")
	var out []Object
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.config.Bucket), Prefix: aws.String(prefix)}
	if after != "" {
		input.StartAfter = aws.String(prefix + string(after))
	}
	for len(out) < limit {
		input.MaxKeys = aws.Int32(int32(min(limit-len(out), 1000)))
		page, err := lister.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, s3Error(err)
		}
		for _, item := range page.Contents {
			name := strings.TrimPrefix(aws.ToString(item.Key), prefix)
			if !Digest(name).Valid() || item.Size == nil || item.LastModified == nil {
				continue
			}
			out = append(out, Object{Info{Digest(name), *item.Size}, *item.LastModified})
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			break
		}
		input.ContinuationToken = page.NextContinuationToken
		input.StartAfter = nil
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var (
	_ Deleter = (*Disk)(nil)
	_ Lister  = (*Disk)(nil)
	_ Deleter = (*S3)(nil)
	_ Lister  = (*S3)(nil)
)
