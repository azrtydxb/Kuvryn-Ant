package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (f *s3Fixture) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) && k > aws.ToString(in.StartAfter) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := &s3.ListObjectsV2Output{}
	for _, k := range keys {
		if len(out.Contents) == int(aws.ToInt32(in.MaxKeys)) {
			out.IsTruncated = aws.Bool(true)
			out.NextContinuationToken = aws.String("next")
			break
		}
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k), Size: aws.Int64(int64(len(f.objects[k]))), LastModified: aws.Time(time.Unix(1700000000, 0))})
	}
	return out, nil
}

func digestOf(data string) Digest {
	sum := sha256.Sum256([]byte(data))
	return Digest(hex.EncodeToString(sum[:]))
}

func TestDiskListAndDelete(t *testing.T) {
	disk, err := NewDisk(filepath.Join(t.TempDir(), "blobs"), TransferBufferSize*2)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	ctx := context.Background()
	var digests []string
	for _, body := range []string{"a", "bb", "ccc"} {
		d := digestOf(body)
		if _, err = disk.Put(ctx, strings.NewReader(body), Info{d, int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		digests = append(digests, string(d))
	}
	sort.Strings(digests)
	page, err := disk.List(ctx, "", 2)
	if err != nil || len(page) != 2 || string(page[0].Digest) != digests[0] || string(page[1].Digest) != digests[1] || page[0].ModTime.IsZero() {
		t.Fatalf("first page %v %v", page, err)
	}
	page, err = disk.List(ctx, page[1].Digest, 2)
	if err != nil || len(page) != 1 || string(page[0].Digest) != digests[2] {
		t.Fatalf("second page %v %v", page, err)
	}
	if err = disk.Delete(ctx, Digest(digests[0])); err != nil {
		t.Fatal(err)
	}
	if err = disk.Delete(ctx, Digest(digests[0])); err != nil {
		t.Fatal("delete must be idempotent", err)
	}
	if _, err = disk.Stat(ctx, Digest(digests[0])); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = disk.Delete(ctx, "../x"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatal(err)
	}
}

func TestS3ListAndDelete(t *testing.T) {
	store, f := fixture(t)
	f.allowPublishedDelete = true
	ctx := context.Background()
	var digests []string
	for _, body := range []string{"a", "bb", "ccc"} {
		d := string(digestOf(body))
		f.objects["ant/test/objects/"+d] = []byte(body)
		digests = append(digests, d)
	}
	f.objects["ant/test/staging/ignored"] = []byte("x")
	f.objects["ant/test/objects/not-a-digest"] = []byte("x")
	sort.Strings(digests)
	got, err := store.List(ctx, "", 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("list %v %v", got, err)
	}
	for i, o := range got {
		if string(o.Digest) != digests[i] || o.ModTime.IsZero() {
			t.Fatalf("object %d: %v", i, o)
		}
	}
	got, err = store.List(ctx, Digest(digests[0]), 1)
	if err != nil || len(got) != 1 || string(got[0].Digest) != digests[1] {
		t.Fatalf("resume %v %v", got, err)
	}
	if err = store.Delete(ctx, Digest(digests[0])); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(ctx, Digest(digests[0])); err != nil {
		t.Fatal("idempotent delete", err)
	}
	if _, ok := f.objects["ant/test/objects/"+digests[0]]; ok {
		t.Fatal("object not deleted")
	}
}
