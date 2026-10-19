package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backup/backuptest"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

func TestObjectWriterMultipart(t *testing.T) {
	api := backuptest.NewMemObjects()
	tree := NewObjectTree(api, "b", "p/backups/x")
	big := bytes.Repeat([]byte("0123456789abcdef"), (2*objectPartSize+12345)/16)
	w, _ := tree.Create(context.Background(), "blobs/d/f")
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, size, err := tree.Open(context.Background(), "blobs/d/f")
	if err != nil || size != int64(len(big)) {
		t.Fatalf("open: %v %d", err, size)
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, big) || api.Multipart != 1 {
		t.Fatalf("multipart roundtrip failed (multi=%d)", api.Multipart)
	}
	// An aborted upload leaves nothing behind.
	w2, _ := tree.Create(context.Background(), "blobs/d/g")
	w2.Write(big)
	abortWriter(w2)
	if strings.Contains(strings.Join(api.Keys(), ","), "/g") {
		t.Fatal("aborted upload left state behind")
	}
	if _, _, err := tree.Open(context.Background(), "missing"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("missing object: %v", err)
	}
}

func TestParseS3Location(t *testing.T) {
	b, k, ok, err := ParseS3Location("s3://kuvryn-ant/ant/backups/pre-1")
	if err != nil || !ok || b != "kuvryn-ant" || k != "ant/backups/pre-1" {
		t.Fatalf("%v %v %v %v", b, k, ok, err)
	}
	if _, _, ok, _ := ParseS3Location("/var/backups/x"); ok {
		t.Fatal("directory parsed as s3")
	}
	for _, bad := range []string{"s3://bucket", "s3://bucket/", "s3://bucket/a/../b", "s3://bucket/a//b", "s3:///k"} {
		if _, _, _, err := ParseS3Location(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestObjectBackupRoundTripAndRetention(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	api := backuptest.NewMemObjects()
	parent := "ant/backups"
	var names []string
	for i := range 4 {
		name := fmt.Sprintf("pre-%d", i)
		tree := NewObjectTree(api, "kuvryn-ant", parent+"/"+name)
		res, err := CreateTree(ctx, src.pool, src.stores, tree, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = VerifyTree(ctx, tree, VerifyOptions{Expect: res.ManifestSHA256}); err != nil {
			t.Fatalf("verify object backup: %v", err)
		}
		names = append(names, name)
		time.Sleep(5 * time.Millisecond)
	}
	// A foreign prefix next to the backups must never be touched.
	api.Objects["kuvryn-ant/ant/backups/not-a-backup/data"] = []byte("keep me")
	sib := ObjectSiblings{API: api, Bucket: "kuvryn-ant", Parent: parent}
	removed, err := ApplyRetention(ctx, sib, "pre-3", time.Now(), RetentionPolicy{Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(removed)
	if strings.Join(removed, ",") != "pre-0,pre-1" {
		t.Fatalf("removed %v", removed)
	}
	for _, k := range api.Keys() {
		if strings.Contains(k, "/pre-0/") || strings.Contains(k, "/pre-1/") {
			t.Fatalf("leftover object %s", k)
		}
	}
	if _, ok := api.Objects["kuvryn-ant/ant/backups/not-a-backup/data"]; !ok {
		t.Fatal("retention deleted a non-backup prefix")
	}

	// Restore round trip from object storage (copy mode: blobs live in the backup).
	dst := freshDatabase(t, false)
	dstStores := newStores(t)
	tree := NewObjectTree(api, "kuvryn-ant", parent+"/pre-3")
	out, err := RestoreTree(ctx, dst, dstStores, tree, RestoreOptions{Keys: keyring(t, rootKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Blobs != 1 || out.SecretsVerified != 1 || out.BlobMode != BlobsCopy {
		t.Fatalf("summary %+v", out)
	}
	store, _ := dstStores.Resolve(ctx, src.domain)
	if info, err := store.Stat(ctx, src.blob.Digest); err != nil || info != src.blob {
		t.Fatalf("blob not restored from object storage: %v", err)
	}
}

func TestMaxAgeRetentionKeepsNewest(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	parent := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if _, err := Create(ctx, src.pool, src.stores, filepath.Join(parent, n), Options{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Everything is "older than max age", yet the newest backup must survive.
	removed, err := ApplyRetention(ctx, DirSiblings{Parent: parent}, "b", time.Now().Add(48*time.Hour), RetentionPolicy{MaxAge: time.Hour})
	if err != nil || len(removed) != 1 || removed[0] != "a" {
		t.Fatalf("removed %v err %v", removed, err)
	}
	if _, err := Verify(ctx, filepath.Join(parent, "b"), "", 1); err != nil {
		t.Fatalf("newest backup damaged: %v", err)
	}
}

func TestReferenceModeKeepsBlobsOutOfTheBackup(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "ref")
	res, err := Create(ctx, src.pool, src.stores, dir, Options{Blobs: BlobsReference})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.BlobMode != BlobsReference || len(res.Manifest.Blobs) != 1 {
		t.Fatalf("manifest %+v", res.Manifest)
	}
	if size := dirSize(t, dir); size > 1<<20 {
		t.Fatalf("reference backup holds %d bytes; blob bytes were copied", size)
	}
	// Verify works without any blob store, and with one it checks existence.
	if s, err := Verify(ctx, dir, res.ManifestSHA256, 1); err != nil || s.BlobMode != BlobsReference {
		t.Fatalf("verify: %v %+v", err, s)
	}
	tree, _ := OpenDirTree(dir, false)
	defer tree.Close()
	if _, err = VerifyTree(ctx, tree, VerifyOptions{Stores: src.stores}); err != nil {
		t.Fatalf("verify with blob check: %v", err)
	}
	if _, err = VerifyTree(ctx, tree, VerifyOptions{Stores: newStores(t)}); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing referenced blob not detected: %v", err)
	}
	// Restore into a target without the blobs fails before the database is touched.
	dst := freshDatabase(t, false)
	if _, err = Restore(ctx, dst, newStores(t), dir, RestoreOptions{}); !errors.Is(err, ErrTarget) {
		t.Fatalf("restore without referenced blobs: %v", err)
	}
	if n, _ := relationCount(ctx, dst); n != 0 {
		t.Fatalf("failed restore touched the database (%d relations)", n)
	}
	// Restore against the store that still holds the blobs succeeds.
	out, err := Restore(ctx, dst, src.stores, dir, RestoreOptions{Keys: keyring(t, rootKeyBytes)})
	if err != nil || out.BlobMode != BlobsReference || out.SecretsVerified != 1 {
		t.Fatalf("restore: %v %+v", err, out)
	}
}

func TestReferenceModeFailsOnMissingBlob(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	// Metadata references a blob that the store does not have.
	if _, err := src.pool.Exec(ctx, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,5,'k')", src.owner.InstanceID, src.domain, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "ref")
	if _, err := Create(ctx, src.pool, src.stores, dir, Options{Blobs: BlobsReference}); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if tree, err := OpenDirTree(dir, false); err == nil {
		names, _ := tree.List(ctx)
		tree.Close()
		if len(names) != 0 {
			t.Fatalf("failed backup left files: %v", names)
		}
	}
}

func TestLocalBackupRefusedWhenDiskTooFull(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "bk")
	// 1% is below any real filesystem's usage in practice; skip if the test
	// volume is genuinely almost empty.
	tree, err := OpenDirTree(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err = checkDisk(tree, 1, 0); err == nil {
		t.Skip("test filesystem is under 1% full")
	}
	_, err = CreateTree(ctx, src.pool, src.stores, tree, Options{MaxDiskPercent: 1})
	if !errors.Is(err, ErrDiskFull) || !errors.Is(err, ErrTarget) {
		t.Fatalf("got %v", err)
	}
	if names, _ := tree.List(ctx); len(names) != 0 {
		t.Fatalf("refused backup left files: %v", names)
	}
	// Projected growth counts: a small remainder with a huge extra is refused.
	if err = checkDisk(tree, 99, 1<<62); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("projected growth not refused: %v", err)
	}
	if err = checkDisk(tree, 0, 1<<62); err != nil {
		t.Fatal("disabled guard must never refuse")
	}
}

func dirSize(t *testing.T, dir string) (n int64) {
	t.Helper()
	tree, err := OpenDirTree(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	names, err := tree.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		r, size, err := tree.Open(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		n += size
	}
	return n
}
