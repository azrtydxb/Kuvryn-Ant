package blob

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type fixtureMultipart struct {
	key       string
	parts     map[int32][]byte
	initiated time.Time
}
type s3Fixture struct {
	mu                                               sync.Mutex
	objects                                          map[string][]byte
	uploads                                          map[string]*fixtureMultipart
	sequence, partCalls, copyCalls, conditionalCalls int
	failPart, failCopy, failConditional, failDelete  bool
	ignoreRange, allowPublishedDelete                bool
	partDelay                                        time.Duration
	activeParts                                      atomic.Int32
	now                                              func() time.Time
}

func (f *s3Fixture) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}
func (f *s3Fixture) ListParts(ctx context.Context, in *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	up := f.uploads[aws.ToString(in.UploadId)]
	if up == nil {
		return nil, apiError("NoSuchUpload")
	}
	out := &s3.ListPartsOutput{}
	for n := int32(1); n <= 10000; n++ {
		if _, ok := up.parts[n]; ok {
			out.Parts = append(out.Parts, types.Part{PartNumber: aws.Int32(n), ETag: aws.String(fmt.Sprintf("opaque-part-%d", n))})
		}
	}
	return out, nil
}
func (f *s3Fixture) ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &s3.ListMultipartUploadsOutput{}
	for id, up := range f.uploads {
		if strings.HasPrefix(up.key, aws.ToString(in.Prefix)) {
			out.Uploads = append(out.Uploads, types.MultipartUpload{Key: aws.String(up.key), UploadId: aws.String(id), Initiated: aws.Time(up.initiated)})
		}
	}
	return out, nil
}

func fixture(t *testing.T) (*S3, *s3Fixture) {
	t.Helper()
	f := &s3Fixture{objects: map[string][]byte{}, uploads: map[string]*fixtureMultipart{}}
	s, err := newS3(S3Config{Region: "test-region", Bucket: "test-bucket", Prefix: "ant/test", PartSize: s3MinimumPartSize, BufferBudget: s3MinimumPartSize * 4}, f)
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}
func apiError(code string) error { return &smithy.GenericAPIError{Code: code, Message: "fixture"} }
func (f *s3Fixture) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.sequence++
	id := strconv.Itoa(f.sequence)
	f.uploads[id] = &fixtureMultipart{key: aws.ToString(in.Key), parts: map[int32][]byte{}, initiated: f.clock()}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id)}, nil
}
func (f *s3Fixture) UploadPart(ctx context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	f.activeParts.Add(1)
	defer f.activeParts.Add(-1)
	if f.partDelay > 0 {
		time.Sleep(f.partDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.failPart {
		return nil, apiError("FixturePartFailure")
	}
	upload := f.uploads[aws.ToString(in.UploadId)]
	if upload == nil {
		return nil, apiError("NoSuchUpload")
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	sum := md5.Sum(data)
	if aws.ToString(in.ContentMD5) != base64.StdEncoding.EncodeToString(sum[:]) || int64(len(data)) != aws.ToInt64(in.ContentLength) {
		return nil, apiError("BadDigest")
	}
	f.partCalls++
	upload.parts[aws.ToInt32(in.PartNumber)] = data
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("opaque-part-%d", aws.ToInt32(in.PartNumber)))}, nil
}
func (f *s3Fixture) UploadPartCopy(ctx context.Context, in *s3.UploadPartCopyInput, _ ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.failCopy {
		return nil, apiError("FixtureCopyFailure")
	}
	source, err := url.PathUnescape(aws.ToString(in.CopySource))
	if err != nil {
		return nil, err
	}
	key := strings.TrimPrefix(source, "test-bucket/")
	data, ok := f.objects[key]
	if !ok {
		return nil, apiError("NoSuchKey")
	}
	if aws.ToString(in.CopySourceIfMatch) != "opaque-completed-etag" {
		return nil, apiError("PreconditionFailed")
	}
	if in.CopySourceRange != nil {
		var start, end int64
		if _, err := fmt.Sscanf(*in.CopySourceRange, "bytes=%d-%d", &start, &end); err != nil {
			return nil, err
		}
		if int64(len(data)) <= s3MinimumPartSize {
			return nil, apiError("InvalidRange")
		}
		data = data[start : end+1]
	}
	upload := f.uploads[aws.ToString(in.UploadId)]
	if upload == nil {
		return nil, apiError("NoSuchUpload")
	}
	f.copyCalls++
	upload.parts[aws.ToInt32(in.PartNumber)] = bytes.Clone(data)
	return &s3.UploadPartCopyOutput{CopyPartResult: &types.CopyPartResult{ETag: aws.String("opaque-copy-part")}}, nil
}
func (f *s3Fixture) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	upload := f.uploads[aws.ToString(in.UploadId)]
	if upload == nil {
		return nil, apiError("NoSuchUpload")
	}
	if strings.Contains(upload.key, "/objects/") {
		if aws.ToString(in.IfNoneMatch) != "*" {
			return nil, apiError("MissingConditionalHeader")
		}
		f.conditionalCalls++
		if f.failConditional {
			return nil, apiError("NotImplemented")
		}
		if _, exists := f.objects[upload.key]; exists {
			return nil, apiError("PreconditionFailed")
		}
	}
	var data []byte
	for _, part := range in.MultipartUpload.Parts {
		body, ok := upload.parts[aws.ToInt32(part.PartNumber)]
		if !ok {
			return nil, apiError("InvalidPart")
		}
		data = append(data, body...)
	}
	f.objects[upload.key] = data
	delete(f.uploads, aws.ToString(in.UploadId))
	return &s3.CompleteMultipartUploadOutput{ETag: aws.String("opaque-completed-etag")}, nil
}
func (f *s3Fixture) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := aws.ToString(in.UploadId)
	if _, ok := f.uploads[id]; !ok {
		return nil, apiError("NoSuchUpload")
	}
	delete(f.uploads, id)
	return &s3.AbortMultipartUploadOutput{}, nil
}
func (f *s3Fixture) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if aws.ToString(in.IfNoneMatch) != "*" {
		return nil, apiError("MissingConditionalHeader")
	}
	f.conditionalCalls++
	key := aws.ToString(in.Key)
	if _, exists := f.objects[key]; exists {
		return nil, apiError("PreconditionFailed")
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[key] = data
	return &s3.PutObjectOutput{}, nil
}
func (f *s3Fixture) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.failDelete {
		return nil, apiError("FixtureDeleteFailure")
	}
	if !f.allowPublishedDelete && !strings.Contains(aws.ToString(in.Key), "/staging/") {
		return nil, errors.New("fixture refuses published-object deletion")
	}
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}
func (f *s3Fixture) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, apiError("NotFound")
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data)))}, nil
}
func (f *s3Fixture) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, apiError("NoSuchKey")
	}
	out := &s3.GetObjectOutput{}
	if in.Range != nil && !f.ignoreRange {
		var start, end int64
		if _, err := fmt.Sscanf(*in.Range, "bytes=%d-%d", &start, &end); err != nil {
			return nil, err
		}
		out.ContentRange = aws.String(fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data = data[start : end+1]
	}
	out.Body = io.NopCloser(bytes.NewReader(bytes.Clone(data)))
	out.ContentLength = aws.Int64(int64(len(data)))
	return out, nil
}
func fixturePut(ctx context.Context, s *S3, data []byte, expected Info) (Info, error) {
	u, err := s.Stage(ctx)
	if err != nil {
		return Info{}, err
	}
	defer u.Abort()
	if _, err := u.Write(data); err != nil {
		return Info{}, err
	}
	return u.Finalize(ctx, expected)
}
func (f *s3Fixture) assertClean(t *testing.T, published int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 0 {
		t.Fatalf("leaked uploads: %d", len(f.uploads))
	}
	if len(f.objects) != published {
		t.Fatalf("objects=%d, expected%d", len(f.objects), published)
	}
	for key := range f.objects {
		if strings.Contains(key, "/staging/") {
			t.Fatal("staging leak")
		}
	}
}

func TestS3MultipartPublicationAndRanges(t *testing.T) {
	s, f := fixture(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), s3MinimumPartSize+17)
	want := content(data)
	upload, err := s.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, want.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatal("visible before finalize")
	}
	got, err := upload.Finalize(ctx, want)
	if err != nil || got != want {
		t.Fatalf("finalize %+v %v", got, err)
	}
	f.assertClean(t, 1)
	if f.partCalls != 2 || f.copyCalls != 1 || f.conditionalCalls != 1 {
		t.Fatalf("multipart/copy/conditional calls %d/%d/%d", f.partCalls, f.copyCalls, f.conditionalCalls)
	}
	reader, err := s.OpenRange(ctx, want.Digest, 5, 11)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(gotBytes, data[5:16]) {
		t.Fatal("range mismatch", err)
	}
	f.ignoreRange = true
	if _, err := s.OpenRange(ctx, want.Digest, 5, 11); err == nil {
		t.Fatal("backend ignored range accepted")
	}
	f.ignoreRange = false
	for _, pair := range [][2]int64{{-1, 1}, {0, -2}, {want.Size + 1, 0}, {1, want.Size}} {
		if _, err := s.OpenRange(ctx, want.Digest, pair[0], pair[1]); !errors.Is(err, ErrRange) {
			t.Fatal(err)
		}
	}
	if _, err := s.Stat(ctx, "../escape"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatal(err)
	}
}
func TestS3ConcurrentDedupAndCorruptWinner(t *testing.T) {
	s, f := fixture(t)
	data := []byte("concurrent content")
	want := content(data)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := fixturePut(context.Background(), s, data, want); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.assertClean(t, 1)
	f.objects[s.key("objects", string(want.Digest))] = []byte("corrupted content")
	if _, err := fixturePut(context.Background(), s, data, want); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt winner accepted: %v", err)
	}
	f.assertClean(t, 1)
	if string(f.objects[s.key("objects", string(want.Digest))]) != "corrupted content" {
		t.Fatal("existing object overwritten")
	}
}
func TestS3FailuresAbortAndReleaseBudget(t *testing.T) {
	for _, kind := range []string{"digest", "part", "copy", "conditional", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			s, f := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			data := []byte("body")
			want := content(data)
			switch kind {
			case "digest":
				want.Size++
			case "part":
				f.failPart = true
			case "copy":
				f.failCopy = true
			case "conditional":
				f.failConditional = true
			}
			upload, err := s.Stage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := upload.Write(data); err != nil {
				t.Fatal(err)
			}
			if kind == "part" || kind == "copy" || kind == "conditional" {
				// Small objects are published with one PutObject; start the
				// multipart path these failures belong to.
				if err := upload.(*s3Upload).ensureMultipart(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "cancellation" {
				cancel()
			}
			if _, err := upload.Finalize(context.Background(), want); err == nil {
				t.Fatal("failure accepted")
			}
			f.assertClean(t, 0)
			if len(s.slots) != 0 {
				t.Fatal("buffer reservation leak")
			}
			if err := upload.Abort(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestS3EmptyBudgetAndCapabilityLimit(t *testing.T) {
	s, f := fixture(t)
	ctx := context.Background()
	for range 2 {
		if _, err := fixturePut(ctx, s, nil, content(nil)); err != nil {
			t.Fatal(err)
		}
	}
	f.assertClean(t, 1)
	var uploads []Upload
	for range cap(s.slots) {
		u, err := s.Stage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		uploads = append(uploads, u)
	}
	limited, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := s.Stage(limited); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for _, u := range uploads {
		if err := u.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	upload, err := s.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	concrete := upload.(*s3Upload)
	concrete.size = s.MaxObjectSize()
	_, err = upload.Write([]byte("overflow"))
	var capability *CapabilityError
	if !errors.As(err, &capability) {
		t.Fatalf("expected capability error: %v", err)
	}
	upload.Abort()
	f.assertClean(t, 1)
}
func TestS3CleanupFailureIsObservable(t *testing.T) {
	s, f := fixture(t)
	f.failDelete = true
	big := bytes.Repeat([]byte("d"), s3MinimumPartSize+1) // staged through multipart; small objects have no staging object
	_, err := fixturePut(context.Background(), s, big, content(big))
	if err == nil || !errors.Is(err, ErrBackend) {
		t.Fatalf("cleanup failure hidden: %v", err)
	}
	if len(s.slots) != 0 {
		t.Fatal("budget reservation leak")
	}
}
func TestS3EndpointConfiguration(t *testing.T) {
	credentials := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
	})
	client := &http.Client{Timeout: time.Second}
	config := S3Config{Endpoint: "https://objects.example.test", Region: "region", Bucket: "bucket", PathStyle: true, Credentials: credentials, HTTPClient: client}
	store, err := NewS3(config)
	if err != nil {
		t.Fatal(err)
	}
	options := store.client.(*s3.Client).Options()
	if !options.UsePathStyle || aws.ToString(options.BaseEndpoint) != config.Endpoint || options.HTTPClient != client {
		t.Fatal("endpoint/transport options lost")
	}
	for _, endpoint := range []string{"http://objects.example.test", "https://user:secret@objects.example.test", "https://objects.example.test/path", "https://objects.example.test?secret=x"} {
		config.Endpoint = endpoint
		if _, err := NewS3(config); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	config.Endpoint = "http://localhost:9000"
	config.AllowHTTP = true
	if _, err := NewS3(config); err != nil {
		t.Fatal(err)
	}
	config.Prefix = "../outside"
	if _, err := NewS3(config); err == nil {
		t.Fatal("unsafe prefix accepted")
	}
}

func TestS3ShortRangeBodyFails(t *testing.T) {
	reader := &s3Reader{Reader: bytes.NewReader([]byte("short")), Closer: io.NopCloser(bytes.NewReader(nil)), remaining: 20}
	if _, err := io.ReadAll(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated range accepted: %v", err)
	}
}

func TestS3ErrorsNeverExposeProviderDetails(t *testing.T) {
	const secret = "private-provider-secret-signed-url"
	for _, input := range []error{
		errors.New(secret),
		&smithy.GenericAPIError{Code: "NotImplemented", Message: secret},
		&smithy.GenericAPIError{Code: "InternalError", Message: secret},
		fmt.Errorf("%s: %w", secret, context.Canceled),
		fmt.Errorf("%s: %w", secret, context.DeadlineExceeded),
	} {
		output := s3Error(input)
		if output == nil || strings.Contains(fmt.Sprintf("%+v", output), secret) || errors.Unwrap(output) != nil {
			t.Fatalf("provider error leaked: %T", output)
		}
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, ErrNotFound, ErrIntegrity, ErrBackend} {
		if !errors.Is(s3Error(fmt.Errorf("%s: %w", secret, sentinel)), sentinel) {
			t.Fatalf("lost safe category %v", sentinel)
		}
	}
	if !errors.Is(s3Error(&smithy.GenericAPIError{Code: "NoSuchKey", Message: secret}), ErrNotFound) {
		t.Fatal("notfound category lost")
	}
	var capability *CapabilityError
	if !errors.As(s3Error(&smithy.GenericAPIError{Code: "NotImplemented", Message: secret}), &capability) {
		t.Fatal("capability category lost")
	}
}

type sensitiveBody struct{}

func (sensitiveBody) Read([]byte) (int, error) { return 0, errors.New("private-response-body-secret") }
func (sensitiveBody) Close() error             { return errors.New("private-close-secret") }
func TestS3BodyErrorsAreSanitized(t *testing.T) {
	reader := &s3Reader{Reader: sensitiveBody{}, Closer: sensitiveBody{}, remaining: 1}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), "private") {
		t.Fatal("body read error escaped")
	}
	if err := reader.Close(); !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), "private") {
		t.Fatal("body close error escaped")
	}
}

func TestS3CredentialProviderErrorIsSanitized(t *testing.T) {
	const secret = "private-credential-provider-material"
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, errors.New(secret) })
	store, err := NewS3(S3Config{Endpoint: "https://objects.example.test", Region: "test", Bucket: "bucket", Credentials: provider})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := store.Stage(context.Background())
	if err == nil {
		// The multipart upload starts lazily; the first S3 call fails at Finalize.
		_, _ = upload.Write([]byte("data"))
		_, err = upload.Finalize(context.Background(), content([]byte("data")))
	}
	if !errors.Is(err, ErrBackend) || strings.Contains(fmt.Sprintf("%+v", err), secret) {
		t.Fatal("SDK credential retrieval error escaped")
	}
	if len(store.slots) != 0 {
		t.Fatal("failed credential retrieval retained buffer reservation")
	}
}

// A full part is sent in the background: the writer is released as soon as the
// buffer is handed over and can fill the next part while the previous uploads.
func TestS3PartUploadOverlapsWithWriting(t *testing.T) {
	s, f := fixture(t)
	f.partDelay = 300 * time.Millisecond
	ctx := context.Background()
	data := bytes.Repeat([]byte("overlap"), (3*s3MinimumPartSize+99)/7+1)
	want := content(data)
	upload, err := s.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := upload.Write(data[:s3MinimumPartSize]); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("first full part was uploaded synchronously (%v)", time.Since(start))
	}
	if _, err := upload.Write(data[s3MinimumPartSize:]); err != nil {
		t.Fatal(err)
	}
	got, err := upload.Finalize(ctx, want)
	if err != nil || got != want {
		t.Fatalf("finalize %+v %v", got, err)
	}
	f.assertClean(t, 1)
	reader, err := s.OpenRange(ctx, want.Digest, 0, want.Size)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := io.ReadAll(reader)
	reader.Close()
	if !bytes.Equal(back, data) {
		t.Fatal("overlapped upload corrupted the object")
	}
	// All budget slots, including the second buffer, are released.
	if len(s.slots) != 0 {
		t.Fatalf("%d slots still held", len(s.slots))
	}
}

// An object that fits the first buffer is published with a single PutObject:
// no staging multipart upload, copy or delete round trips.
func TestS3SmallObjectUsesSinglePut(t *testing.T) {
	s, f := fixture(t)
	data := []byte("small package metadata")
	if _, err := fixturePut(context.Background(), s, data, content(data)); err != nil {
		t.Fatal(err)
	}
	f.assertClean(t, 1)
	if f.sequence != 0 || f.copyCalls != 0 || f.conditionalCalls != 1 {
		t.Fatalf("multipart used for a small object: uploads=%d copies=%d puts=%d", f.sequence, f.copyCalls, f.conditionalCalls)
	}
	f.mu.Lock()
	for _, body := range f.objects {
		if !bytes.Equal(body, data) {
			t.Fatal("stored bytes differ")
		}
	}
	f.mu.Unlock()
	// A duplicate publish of the same content is verified, not an error.
	if _, err := fixturePut(context.Background(), s, data, content(data)); err != nil {
		t.Fatal(err)
	}
	f.assertClean(t, 1)
}

// Objects larger than the first buffer grow it and go through multipart.
func TestS3GrowingBufferSwitchesToMultipart(t *testing.T) {
	s, f := fixture(t)
	data := bytes.Repeat([]byte("g"), s3MinimumPartSize+17)
	if _, err := fixturePut(context.Background(), s, data, content(data)); err != nil {
		t.Fatal(err)
	}
	f.assertClean(t, 1)
	if f.sequence != 2 {
		t.Fatalf("expected staging and publication multipart uploads, got %d", f.sequence)
	}
	if len(s.slots) != 0 {
		t.Fatal("budget leak")
	}
}
