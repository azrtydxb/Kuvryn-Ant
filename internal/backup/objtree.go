package backup

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// ObjectAPI is the subset of the S3 client a backup needs. *s3.Client satisfies
// it; tests substitute an in-memory implementation.
type ObjectAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

const objectPartSize = 16 << 20

// ParseS3Location splits s3://bucket/key/prefix. ok is false when loc is not an
// s3:// URL (it is then a local directory). The key must not be empty or contain
// dot segments.
func ParseS3Location(loc string) (bucket, key string, ok bool, err error) {
	if !strings.HasPrefix(loc, "s3://") {
		return "", "", false, nil
	}
	u, perr := url.Parse(loc)
	if perr != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", true, fmt.Errorf("%w: invalid s3 location", ErrTarget)
	}
	key = strings.Trim(u.Path, "/")
	if key == "" {
		return "", "", true, fmt.Errorf("%w: s3 location needs a key prefix, e.g. s3://bucket/prefix/backups/name", ErrTarget)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			return "", "", true, fmt.Errorf("%w: invalid s3 key prefix", ErrTarget)
		}
	}
	return u.Host, key, true, nil
}

// ObjectTree is a backup held as objects under bucket/prefix/.
type ObjectTree struct {
	api    ObjectAPI
	bucket string
	prefix string // no trailing slash
}

func NewObjectTree(api ObjectAPI, bucket, prefix string) *ObjectTree {
	return &ObjectTree{api: api, bucket: bucket, prefix: strings.Trim(prefix, "/")}
}

func (o *ObjectTree) Close() error { return nil }

func (o *ObjectTree) key(name string) string { return o.prefix + "/" + name }

func (o *ObjectTree) PutAtomic(ctx context.Context, name string, data []byte) error {
	_, err := o.api.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(o.bucket), Key: aws.String(o.key(name)), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentMD5: md5b64(data)})
	return err
}

func md5b64(b []byte) *string {
	s := md5.Sum(b)
	return aws.String(base64.StdEncoding.EncodeToString(s[:]))
}

func (o *ObjectTree) Open(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	out, err := o.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(o.bucket), Key: aws.String(o.key(name))})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, ErrIntegrity
		}
		return nil, 0, err
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func isNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return true
		}
	}
	var nf *types.NoSuchKey
	return errors.As(err, &nf)
}

// listKeys returns every key under prefix (which must end in "/" or be empty).
func (o *ObjectTree) listKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := o.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(o.bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, c := range out.Contents {
			keys = append(keys, aws.ToString(c.Key))
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return keys, nil
		}
		token = out.NextContinuationToken
	}
}

func (o *ObjectTree) List(ctx context.Context) ([]string, error) {
	keys, err := o.listKeys(ctx, o.prefix+"/")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, strings.TrimPrefix(k, o.prefix+"/"))
	}
	sort.Strings(names)
	return names, nil
}

func (o *ObjectTree) RemoveAll(ctx context.Context, name string) error {
	keys, err := o.listKeys(ctx, o.key(name))
	if err != nil {
		return err
	}
	for _, k := range keys {
		// Only the exact object or children below it, never a name that merely
		// shares a string prefix ("tables" vs "tables2").
		if k != o.key(name) && !strings.HasPrefix(k, o.key(name)+"/") {
			continue
		}
		if _, err = o.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(o.bucket), Key: aws.String(k)}); err != nil {
			return err
		}
	}
	return nil
}

// childNames lists the immediate "directories" under the tree's parent, which
// is how sibling backups are found for retention.
func childNames(ctx context.Context, api ObjectAPI, bucket, parent string) ([]string, error) {
	var names []string
	var token *string
	prefix := strings.Trim(parent, "/") + "/"
	for {
		out, err := api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/"), ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, p := range out.CommonPrefixes {
			names = append(names, strings.Trim(strings.TrimPrefix(aws.ToString(p.Prefix), prefix), "/"))
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			sort.Strings(names)
			return names, nil
		}
		token = out.NextContinuationToken
	}
}

func (o *ObjectTree) Create(ctx context.Context, name string) (io.WriteCloser, error) {
	return &objectWriter{ctx: ctx, o: o, key: o.key(name), buf: make([]byte, 0, objectPartSize)}, nil
}

// objectWriter streams into one object: a single PutObject when the data fits
// one part, otherwise a multipart upload. Nothing is visible until Close.
type objectWriter struct {
	ctx   context.Context
	o     *ObjectTree
	key   string
	buf   []byte
	id    *string
	parts []types.CompletedPart
	err   error
}

func (w *objectWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		n := min(objectPartSize-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		written += n
		if len(w.buf) == objectPartSize && len(p) > 0 {
			if w.err = w.flush(); w.err != nil {
				return written, w.err
			}
		}
	}
	return written, nil
}

func (w *objectWriter) flush() error {
	if w.id == nil {
		out, err := w.o.api.CreateMultipartUpload(w.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(w.o.bucket), Key: aws.String(w.key)})
		if err != nil {
			return err
		}
		w.id = out.UploadId
	}
	n := int32(len(w.parts) + 1)
	out, err := w.o.api.UploadPart(w.ctx, &s3.UploadPartInput{Bucket: aws.String(w.o.bucket), Key: aws.String(w.key), UploadId: w.id, PartNumber: aws.Int32(n), Body: bytes.NewReader(w.buf), ContentLength: aws.Int64(int64(len(w.buf))), ContentMD5: md5b64(w.buf)})
	if err != nil {
		return err
	}
	w.parts = append(w.parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)})
	w.buf = w.buf[:0]
	return nil
}

func (w *objectWriter) Close() error {
	if w.err != nil {
		w.Abort()
		return w.err
	}
	if w.id == nil {
		w.err = w.o.PutAtomic(w.ctx, strings.TrimPrefix(w.key, w.o.prefix+"/"), w.buf)
		return w.err
	}
	if len(w.buf) > 0 {
		if w.err = w.flush(); w.err != nil {
			w.Abort()
			return w.err
		}
	}
	_, w.err = w.o.api.CompleteMultipartUpload(w.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(w.o.bucket), Key: aws.String(w.key), UploadId: w.id, MultipartUpload: &types.CompletedMultipartUpload{Parts: w.parts}})
	if w.err != nil {
		w.Abort()
	}
	return w.err
}

// Abort discards an unfinished multipart upload.
func (w *objectWriter) Abort() {
	if w.id != nil {
		_, _ = w.o.api.AbortMultipartUpload(context.WithoutCancel(w.ctx), &s3.AbortMultipartUploadInput{Bucket: aws.String(w.o.bucket), Key: aws.String(w.key), UploadId: w.id})
		w.id = nil
	}
	if w.err == nil {
		w.err = errors.New("upload aborted")
	}
}
