package blob

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// CapabilityError identifies an unsupported backend feature or adapter limit.
// No fallback silently removes integrity or conditional-publication guarantees.
type CapabilityError struct{ Feature string }

func (e *CapabilityError) Error() string { return "blob backend capability unavailable: " + e.Feature }

const s3MinimumPartSize = 5 << 20
const s3DefaultPartSize = 64 << 20
const s3MaximumParts = 10000

// S3Config accepts any compatible S3 endpoint. HTTPClient may supply private CA
// roots and transport limits. Plain HTTP requires an explicit opt-in. Credentials
// are injected; this package neither reads environment secrets nor requires AWS.
// PartSize is 5–64 MiB (default64MiB); MaxObjectSize is PartSize*10000. Plain Stage
// transfers are not resumable; StageResumable with a StateStore is. The bucket must support conditional
// multipart completion and have application-private objects/staging namespaces.
type S3Config struct {
	Endpoint, Region, Bucket, Prefix string
	PathStyle, AllowHTTP             bool
	Credentials                      aws.CredentialsProvider
	HTTPClient                       aws.HTTPClient
	PartSize, BufferBudget           int64
	// StateStore, when set, makes StageResumable persist multipart progress so an
	// interrupted upload can continue after a process restart.
	StateStore StateStore
	// Now overrides the clock used for stale-upload decisions (tests).
	Now func() time.Time
}

type s3API interface {
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	UploadPartCopy(context.Context, *s3.UploadPartCopyInput, ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListParts(context.Context, *s3.ListPartsInput, ...func(*s3.Options)) (*s3.ListPartsOutput, error)
	ListMultipartUploads(context.Context, *s3.ListMultipartUploadsInput, ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
}

// S3 holds one shared upload buffer budget. Callers must abort uploads they do
// not finalize. Caller-owned Write buffers and HTTP transport buffers are separate.
type S3 struct {
	client s3API
	config S3Config
	slots  chan struct{}
}

func NewS3(config S3Config) (*S3, error) {
	if config.Credentials == nil {
		return nil, fmt.Errorf("S3 credentials provider is required")
	}
	store, err := newS3(config, nil)
	if err != nil {
		return nil, err
	}
	options := s3.Options{Region: config.Region, Credentials: config.Credentials, HTTPClient: config.HTTPClient, UsePathStyle: config.PathStyle, RetryMaxAttempts: 3, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
	if config.Endpoint != "" {
		options.BaseEndpoint = aws.String(config.Endpoint)
	}
	store.client = s3.New(options)
	return store, nil
}
func newS3(config S3Config, client s3API) (*S3, error) {
	if config.Region == "" || config.Bucket == "" || strings.ContainsAny(config.Bucket, "/\\") {
		return nil, fmt.Errorf("S3 region and plain bucket name are required")
	}
	if config.Endpoint != "" {
		endpoint, err := url.Parse(config.Endpoint)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || (endpoint.Scheme != "https" && !(config.AllowHTTP && endpoint.Scheme == "http")) {
			return nil, fmt.Errorf("invalid S3 endpoint; HTTPS is required unless AllowHTTP is explicit")
		}
	}
	config.Prefix = strings.Trim(config.Prefix, "/")
	if config.Prefix != "" {
		for _, part := range strings.Split(config.Prefix, "/") {
			if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
				return nil, fmt.Errorf("invalid S3 prefix")
			}
		}
	}
	if config.PartSize == 0 {
		config.PartSize = s3DefaultPartSize
	}
	if config.PartSize < s3MinimumPartSize || config.PartSize > s3DefaultPartSize {
		return nil, &CapabilityError{"part size must be between 5 and 64 MiB"}
	}
	if config.BufferBudget == 0 {
		config.BufferBudget = DefaultBufferBudget
	}
	if config.BufferBudget < config.PartSize {
		return nil, fmt.Errorf("S3 buffer budget must fit one part")
	}
	return &S3{client: client, config: config, slots: make(chan struct{}, config.BufferBudget/config.PartSize)}, nil
}
func (s *S3) MaxObjectSize() int64 { return s.config.PartSize * s3MaximumParts }
func (s *S3) key(kind, name string) string {
	if s.config.Prefix == "" {
		return kind + "/" + name
	}
	return s.config.Prefix + "/" + kind + "/" + name
}
func (s *S3) Stage(ctx context.Context) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	key := s.key("staging", rand.Text())
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(key)})
	if err != nil {
		<-s.slots
		return nil, s3Error(err)
	}
	if out == nil || aws.ToString(out.UploadId) == "" {
		<-s.slots
		return nil, &CapabilityError{"multipart upload ID"}
	}
	return &s3Upload{store: s, ctx: ctx, key: key, id: aws.ToString(out.UploadId), buffer: make([]byte, s.config.PartSize), digest: sha256.New(), createdAt: s.now()}, nil
}

func (s *S3) now() time.Time {
	if s.config.Now != nil {
		return s.config.Now()
	}
	return time.Now()
}

type s3Upload struct {
	mu                                     sync.Mutex
	store                                  *S3
	ctx                                    context.Context
	key, id, publicationKey, publicationID string
	buffer                                 []byte
	used                                   int
	size                                   int64
	digest                                 hash.Hash
	parts                                  []types.CompletedPart
	failure                                error
	closed                                 bool
	// stateID, when non-empty, names the persisted resume state of this upload.
	stateID   string
	createdAt time.Time
}

func (u *s3Upload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return 0, ErrClosed
	}
	if u.failure != nil {
		return 0, u.failure
	}
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > u.store.MaxObjectSize()-u.size {
		u.failure = &CapabilityError{"object exceeds configured multipart part-count limit"}
		return 0, u.failure
	}
	written := 0
	for len(p) > 0 {
		if err := u.ctx.Err(); err != nil {
			u.failure = err
			return written, err
		}
		n := copy(u.buffer[u.used:], p)
		u.used += n
		u.size += int64(n)
		u.digest.Write(p[:n])
		written += n
		p = p[n:]
		if u.used == len(u.buffer) {
			if err := u.flush(u.ctx); err != nil {
				u.failure = err
				return written, err
			}
		}
	}
	return written, nil
}
func (u *s3Upload) flush(ctx context.Context) error {
	if u.used == 0 {
		return nil
	}
	sum := md5.Sum(u.buffer[:u.used]) // Transport integrity, never the content identity.
	number := int32(len(u.parts) + 1)
	out, err := u.store.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(u.key), UploadId: aws.String(u.id), PartNumber: aws.Int32(number), Body: bytes.NewReader(u.buffer[:u.used]), ContentLength: aws.Int64(int64(u.used)), ContentMD5: aws.String(base64.StdEncoding.EncodeToString(sum[:]))})
	if err != nil {
		return s3Error(err)
	}
	if out == nil || aws.ToString(out.ETag) == "" {
		return &CapabilityError{"multipart part ETag"}
	}
	u.parts = append(u.parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)})
	u.used = 0
	return u.persist(ctx)
}
func (u *s3Upload) Abort() error { u.mu.Lock(); defer u.mu.Unlock(); return u.cleanup() }
func (u *s3Upload) cleanup() error {
	if u.closed {
		return nil
	}
	u.closed = true
	// Cancellation must not prevent aborting remote uploads. Cleanup is bounded;
	// failures are returned so operators can reap leaked staging uploads later.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), 30*time.Second)
	defer cancel()
	var failures []error
	for _, pair := range [][2]string{{u.key, u.id}, {u.publicationKey, u.publicationID}} {
		if pair[1] == "" {
			continue
		}
		_, err := u.store.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(pair[0]), UploadId: aws.String(pair[1])})
		if err != nil && !hasS3Code(err, "NoSuchUpload") {
			failures = append(failures, s3Error(err))
		}
	}
	_, err := u.store.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(u.key)})
	if err != nil {
		failures = append(failures, s3Error(err))
	}
	if u.stateID != "" && u.store.config.StateStore != nil {
		if err := u.store.config.StateStore.Delete(ctx, u.stateID); err != nil {
			failures = append(failures, err)
		}
	}
	u.buffer = nil
	<-u.store.slots
	return errors.Join(failures...)
}
func (u *s3Upload) Finalize(ctx context.Context, expected Info) (result Info, resultErr error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return Info{}, ErrClosed
	}
	defer func() { resultErr = errors.Join(resultErr, u.cleanup()) }()
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := u.ctx.Err(); err != nil {
		return Info{}, err
	}
	if u.failure != nil {
		return Info{}, u.failure
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(u.ctx, cancel)
	defer cancel()
	defer stop()
	if !expected.Digest.Valid() {
		return Info{}, ErrInvalidDigest
	}
	actual := Info{Digest: Digest(hex.EncodeToString(u.digest.Sum(nil))), Size: u.size}
	if actual != expected {
		return Info{}, ErrIntegrity
	}
	key := u.store.key("objects", string(actual.Digest))
	if actual.Size == 0 {
		_, err := u.store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(key), Body: bytes.NewReader(nil), ContentLength: aws.Int64(0), IfNoneMatch: aws.String("*"), Metadata: map[string]string{"sha256": string(actual.Digest)}})
		if err != nil {
			if verifyErr := u.verifyExisting(ctx, actual); verifyErr != nil {
				return Info{}, errors.Join(s3Error(err), verifyErr)
			}
		}
		return actual, nil
	}
	if err := u.flush(ctx); err != nil {
		return Info{}, err
	}
	completed, err := u.store.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(u.key), UploadId: aws.String(u.id), MultipartUpload: &types.CompletedMultipartUpload{Parts: u.parts}})
	if err != nil {
		return Info{}, s3Error(err)
	}
	if completed == nil || aws.ToString(completed.ETag) == "" {
		return Info{}, &CapabilityError{"completed staging ETag"}
	}
	publication, err := u.store.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(key), Metadata: map[string]string{"sha256": string(actual.Digest)}})
	if err != nil {
		return Info{}, s3Error(err)
	}
	if publication == nil || aws.ToString(publication.UploadId) == "" {
		return Info{}, &CapabilityError{"publication multipart upload ID"}
	}
	u.publicationKey = key
	u.publicationID = aws.ToString(publication.UploadId)
	// Copy is server-side and bounded to 5 GiB per part; it supports objects larger
	// than CopyObject's single-request limit without reading them into application RAM.
	const copySize int64 = 5 << 30
	var copied []types.CompletedPart
	source := url.PathEscape(u.store.config.Bucket + "/" + u.key)
	for start := int64(0); start < actual.Size; start += copySize {
		end := start + copySize - 1
		if end >= actual.Size {
			end = actual.Size - 1
		}
		number := int32(len(copied) + 1)
		var copyRange *string
		if actual.Size > s3MinimumPartSize {
			copyRange = aws.String(fmt.Sprintf("bytes=%d-%d", start, end))
		}
		out, err := u.store.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(key), UploadId: publication.UploadId, PartNumber: aws.Int32(number), CopySource: aws.String(source), CopySourceRange: copyRange, CopySourceIfMatch: completed.ETag})
		if err != nil {
			return Info{}, s3Error(err)
		}
		if out == nil || out.CopyPartResult == nil || aws.ToString(out.CopyPartResult.ETag) == "" {
			return Info{}, &CapabilityError{"copied part ETag"}
		}
		copied = append(copied, types.CompletedPart{PartNumber: aws.Int32(number), ETag: out.CopyPartResult.ETag})
	}
	_, err = u.store.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(key), UploadId: publication.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: copied}, IfNoneMatch: aws.String("*")})
	if err != nil {
		// Includes concurrent winners and ambiguous completion responses. Accept only
		// a full SHA-256+size verification, not metadata or a multipart ETag assertion.
		if verifyErr := u.verifyExisting(ctx, actual); verifyErr != nil {
			return Info{}, errors.Join(s3Error(err), verifyErr)
		}
	}
	return actual, nil
}
func (u *s3Upload) verifyExisting(ctx context.Context, expected Info) error {
	out, err := u.store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(u.store.config.Bucket), Key: aws.String(u.store.key("objects", string(expected.Digest)))})
	if err != nil {
		return s3Error(err)
	}
	if out == nil || out.Body == nil {
		return &CapabilityError{"object response body"}
	}
	defer out.Body.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, contextReader{ctx, io.LimitReader(out.Body, expected.Size+1)}, u.buffer)
	if err != nil {
		return s3Error(err)
	}
	if n != expected.Size || hex.EncodeToString(h.Sum(nil)) != string(expected.Digest) {
		return ErrIntegrity
	}
	return nil
}
func (s *S3) Stat(ctx context.Context, digest Digest) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if !digest.Valid() {
		return Info{}, ErrInvalidDigest
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(s.key("objects", string(digest)))})
	if err != nil {
		return Info{}, s3Error(err)
	}
	if out == nil || out.ContentLength == nil || *out.ContentLength < 0 {
		return Info{}, &CapabilityError{"object content length"}
	}
	return Info{digest, *out.ContentLength}, nil
}
func (s *S3) OpenRange(ctx context.Context, digest Digest, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < -1 {
		return nil, ErrRange
	}
	info, err := s.Stat(ctx, digest)
	if err != nil {
		return nil, err
	}
	if offset > info.Size || (length >= 0 && length > info.Size-offset) {
		return nil, ErrRange
	}
	if length == -1 {
		length = info.Size - offset
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	expectedRange := fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, info.Size)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.config.Bucket), Key: aws.String(s.key("objects", string(digest))), Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))})
	if err != nil {
		return nil, s3Error(err)
	}
	if out == nil || out.Body == nil {
		return nil, &CapabilityError{"object response body"}
	}
	if aws.ToString(out.ContentRange) != expectedRange || out.ContentLength == nil || *out.ContentLength != length {
		out.Body.Close()
		return nil, &CapabilityError{"correct byte-range response"}
	}
	return &s3Reader{Reader: contextReader{ctx, io.LimitReader(out.Body, length)}, Closer: out.Body, remaining: length}, nil
}

type s3Reader struct {
	io.Reader
	io.Closer
	remaining int64
}

func (r *s3Reader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, s3Error(err)
}

func (r *s3Reader) Close() error { return s3Error(r.Closer.Close()) }

func hasS3Code(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		for _, code := range codes {
			if apiErr.ErrorCode() == code {
				return true
			}
		}
	}
	return false
}

// SDK/provider messages are untrusted and can contain credentials, signed URLs,
// or backend response details. Preserve only fixed error categories, never the
// original error chain (including via wrapping or CapabilityError.Feature).
func s3Error(err error) error {
	if err == nil {
		return nil
	}
	for _, safe := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, io.ErrShortWrite, ErrBackend, ErrInvalidDigest, ErrIntegrity, ErrNotFound, ErrRange, ErrClosed, ErrUnsafePath} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	if hasS3Code(err, "NoSuchKey", "NotFound", "404") {
		return ErrNotFound
	}
	if hasS3Code(err, "NotImplemented", "UnsupportedOperation") {
		return &CapabilityError{"required S3 operation"}
	}
	return ErrBackend
}

var _ Store = (*S3)(nil)
