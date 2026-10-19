// Package backuptest provides an in-memory S3 for backup tests.
package backuptest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// MemObjects is an in-memory S3 that implements exactly what backup.ObjectAPI needs.
type MemObjects struct {
	mu sync.Mutex
	// Objects maps "bucket/key" to content. Tests may read and seed it.
	Objects map[string][]byte
	uploads map[string]map[int32][]byte
	keyOf   map[string]string
	seq     int
	// Multipart counts completed multipart uploads.
	Multipart int
}

func NewMemObjects() *MemObjects {
	return &MemObjects{Objects: map[string][]byte{}, uploads: map[string]map[int32][]byte{}, keyOf: map[string]string{}}
}

func (m *MemObjects) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)] = data
	return &s3.PutObjectOutput{}, nil
}

func (m *MemObjects) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.Objects[aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data)))}, nil
}

func (m *MemObjects) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Objects, aws.ToString(in.Bucket)+"/"+aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (m *MemObjects) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	base := aws.ToString(in.Bucket) + "/"
	prefix := aws.ToString(in.Prefix)
	var keys []string
	for k := range m.Objects {
		if strings.HasPrefix(k, base+prefix) {
			keys = append(keys, strings.TrimPrefix(k, base))
		}
	}
	sort.Strings(keys)
	out := &s3.ListObjectsV2Output{}
	seen := map[string]bool{}
	for _, k := range keys {
		if d := aws.ToString(in.Delimiter); d != "" {
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.Index(rest, d); i >= 0 {
				p := prefix + rest[:i+1]
				if !seen[p] {
					seen[p] = true
					out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: aws.String(p)})
				}
				continue
			}
		}
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k)})
	}
	return out, nil
}

func (m *MemObjects) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprint(m.seq)
	m.uploads[id] = map[int32][]byte{}
	m.keyOf[id] = aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id)}, nil
}

func (m *MemObjects) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	up := m.uploads[aws.ToString(in.UploadId)]
	if up == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchUpload"}
	}
	up[aws.ToInt32(in.PartNumber)] = data
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprint("etag", aws.ToInt32(in.PartNumber)))}, nil
}

func (m *MemObjects) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := aws.ToString(in.UploadId)
	up := m.uploads[id]
	if up == nil {
		return nil, &smithy.GenericAPIError{Code: "NoSuchUpload"}
	}
	var all []byte
	for i, p := range in.MultipartUpload.Parts {
		if aws.ToInt32(p.PartNumber) != int32(i+1) {
			return nil, &smithy.GenericAPIError{Code: "InvalidPartOrder"}
		}
		all = append(all, up[aws.ToInt32(p.PartNumber)]...)
	}
	m.Objects[m.keyOf[id]] = all
	m.Multipart++
	delete(m.uploads, id)
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (m *MemObjects) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.uploads, aws.ToString(in.UploadId))
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (m *MemObjects) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var k []string
	for key := range m.Objects {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}
