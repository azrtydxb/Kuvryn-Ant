package backup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/backupfence"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// addAsset publishes data as one asset of a new feed with the given purpose and
// returns the blob and version ids. Bytes are stored; callers delete them to
// simulate loss.
func addAsset(t *testing.T, src source, slug, purpose string, data []byte) (info blob.Info, version string) {
	t.Helper()
	ctx := context.Background()
	inst := src.owner.InstanceID
	var feed, source, comp, blobID string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	info = putBlob(t, src.stores, src.domain, data)
	must(src.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format,purpose) VALUES($1,$2,'raw',$3) RETURNING id::text", inst, slug, purpose).Scan(&feed))
	must(src.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,'local','raw') RETURNING id::text", inst, slug).Scan(&source))
	must(src.pool.QueryRow(ctx, "INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,$3) RETURNING id::text", inst, source, slug+"/pkg").Scan(&comp))
	must(src.pool.QueryRow(ctx, "INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) RETURNING id::text", inst, comp, "v-"+slug).Scan(&version))
	must(src.pool.QueryRow(ctx, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) RETURNING id::text", inst, src.domain, string(info.Digest), info.Size).Scan(&blobID))
	_, err := src.pool.Exec(ctx, "INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'a',$3)", inst, version, blobID)
	must(err)
	_, err = src.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", inst, feed, source)
	must(err)
	return info, version
}

func lose(t *testing.T, src source, info blob.Info) {
	t.Helper()
	store, err := src.stores.Resolve(context.Background(), src.domain)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.(blob.Deleter).Delete(context.Background(), info.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestMissingNonCacheBlobStillFailsBackup(t *testing.T) {
	for _, mode := range []string{BlobsCopy, BlobsReference} {
		t.Run(mode, func(t *testing.T) {
			src := populate(t)
			info, _ := addAsset(t, src, "releases", "artifacts", []byte("a release artifact"))
			lose(t, src, info)
			_, err := Create(context.Background(), src.pool, src.stores, filepath.Join(t.TempDir(), "bk"), Options{Blobs: mode})
			if !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("a missing live blob must fail the backup loudly: %v", err)
			}
			var live bool
			if live, err = backupfence.Live(context.Background(), src.pool); err != nil || live {
				t.Fatalf("fence must be released after a failed backup: live=%v err=%v", live, err)
			}
		})
	}
}

func TestMissingCacheOnlyBlobWarnsAndRestoreDropsCacheVersions(t *testing.T) {
	for _, mode := range []string{BlobsCopy, BlobsReference} {
		t.Run(mode, func(t *testing.T) {
			src := populate(t)
			ctx := context.Background()
			inst := src.owner.InstanceID
			keep, _ := addAsset(t, src, "releases", "artifacts", []byte("a release artifact"))
			layer, layerVersion := addAsset(t, src, "buildcache", "cache", []byte("a cache layer that was swept"))
			// A manifest in the same cache component names the layer by version string,
			// and a tag points at it; its own blob is intact.
			var comp, manifestVersion, manifestBlob string
			if err := src.pool.QueryRow(ctx, "SELECT component_id::text FROM versions WHERE id=$1", layerVersion).Scan(&comp); err != nil {
				t.Fatal(err)
			}
			mf := putBlob(t, src.stores, src.domain, []byte("a cache manifest"))
			if err := src.pool.QueryRow(ctx, "INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'manifest') RETURNING id::text", inst, comp).Scan(&manifestVersion); err != nil {
				t.Fatal(err)
			}
			if err := src.pool.QueryRow(ctx, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) RETURNING id::text", inst, src.domain, string(mf.Digest), mf.Size).Scan(&manifestBlob); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				"INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES('" + inst + "','" + manifestVersion + "','m','" + manifestBlob + "')",
				"INSERT INTO version_refs(component_id,parent_version_id,ref_version) VALUES('" + comp + "','" + manifestVersion + "','v-buildcache')",
				"INSERT INTO tags(component_id,name,version_id) VALUES('" + comp + "','latest','" + manifestVersion + "')",
			} {
				if _, err := src.pool.Exec(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			lose(t, src, layer)

			dir := filepath.Join(t.TempDir(), "bk")
			res, err := Create(ctx, src.pool, src.stores, dir, Options{Blobs: mode})
			if err != nil {
				t.Fatalf("a missing cache-only blob must not fail the backup: %v", err)
			}
			if len(res.Warnings) != 1 || res.Manifest.MissingCount() != 1 {
				t.Fatalf("want one warning and one missing blob: %+v", res.Warnings)
			}
			for _, b := range res.Manifest.Blobs {
				if b.Digest == string(layer.Digest) && !(b.Missing && b.BestEffort) {
					t.Fatal("the lost cache blob must be recorded missing and best-effort")
				}
				if b.Digest == string(keep.Digest) && (b.Missing || b.BestEffort) {
					t.Fatal("a live blob must stay strict")
				}
			}
			opts := VerifyOptions{Expect: res.ManifestSHA256}
			if mode == BlobsReference {
				opts.Stores = src.stores
			}
			if sum, err := Verify(ctx, dir, res.ManifestSHA256, 2); err != nil || sum.MissingBlobs != 1 {
				t.Fatalf("verify: %+v %v", sum, err)
			}
			if mode == BlobsReference {
				tree, _ := OpenDirTree(dir, false)
				defer tree.Close()
				if _, err = VerifyTree(ctx, tree, opts); err != nil {
					t.Fatalf("verify with store check must tolerate the cache blob: %v", err)
				}
			}

			dst := freshDatabase(t, false)
			dstStores := newStores(t)
			if mode == BlobsReference {
				dstStores = src.stores // reference mode restores against the same durable store
			}
			out, err := Restore(ctx, dst, dstStores, dir, RestoreOptions{Keys: keyring(t, rootKeyBytes), ExpectManifestSHA256: res.ManifestSHA256})
			if err != nil {
				t.Fatalf("restore must tolerate the best-effort cache blob: %v", err)
			}
			if out.DroppedCacheVersions != 2 {
				t.Fatalf("the layer and the manifest naming it must be dropped, got %d", out.DroppedCacheVersions)
			}
			if n := count(t, dst, "versions WHERE version IN ('v-buildcache','manifest')"); n != 0 {
				t.Fatalf("cache versions survived: %d", n)
			}
			if n := count(t, dst, "tags"); n != 0 {
				t.Fatalf("tags of dropped versions survived: %d", n)
			}
			if n := count(t, dst, "blobs WHERE digest='"+string(layer.Digest)+"'"); n != 0 {
				t.Fatalf("row of the lost blob survived: %d", n)
			}
			if n := count(t, dst, "versions WHERE version='v-releases'"); n != 1 {
				t.Fatalf("live release version lost: %d", n)
			}
			if n := count(t, dst, "assets"); n != count(t, src.pool, "assets")-2 {
				t.Fatalf("assets after restore: %d", n)
			}
			store, _ := dstStores.Resolve(ctx, src.domain)
			if _, err = store.Stat(ctx, keep.Digest); err != nil {
				t.Fatalf("live blob missing after restore: %v", err)
			}
		})
	}
}

// The fence is held from before the snapshot until the backup is done and is
// gone afterwards; a crashed holder's fence expires on its own.
func TestFenceHeldDuringBackupAndExpiresWithoutHeartbeat(t *testing.T) {
	src := populate(t)
	ctx := context.Background()
	var during []bool
	hook := hookStores{Stores: src.stores, on: func() {
		live, err := backupfence.Live(ctx, src.pool)
		if err != nil {
			t.Error(err)
		}
		during = append(during, live)
	}}
	if _, err := Create(ctx, src.pool, hook, filepath.Join(t.TempDir(), "bk"), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(during) == 0 || !during[0] {
		t.Fatalf("no fence while blobs were copied: %v", during)
	}
	if live, err := backupfence.Live(ctx, src.pool); err != nil || live {
		t.Fatalf("fence not released: live=%v err=%v", live, err)
	}

	// Crash: the row is left behind with a stale expiry.
	if _, err := src.pool.Exec(ctx, "INSERT INTO backup_fences(holder,expires_at) VALUES('crashed',now()+interval '1 minute')"); err != nil {
		t.Fatal(err)
	}
	if live, _ := backupfence.Live(ctx, src.pool); !live {
		t.Fatal("a fresh fence must be live")
	}
	if _, err := src.pool.Exec(ctx, "UPDATE backup_fences SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if live, _ := backupfence.Live(ctx, src.pool); live {
		t.Fatal("an unrenewed fence must expire")
	}
}

type hookStores struct {
	Stores
	on func()
}

func (h hookStores) Resolve(ctx context.Context, domain string) (blob.Store, error) {
	h.on()
	return h.Stores.Resolve(ctx, domain)
}
