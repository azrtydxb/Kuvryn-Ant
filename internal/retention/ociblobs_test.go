package retention

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// keep_last ranks manifests only. Config and layer carriers neither push
// manifests out of the newest N nor are deleted directly: they are released
// when the manifests that named them go, and only if nothing else names them.
func TestKeepLastRanksManifestsOnlyAndReleasesUniqueBlobs(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	_, src, dom := f.newSource(t, "images", "local")
	f.svc.Stores = func(context.Context, string) (blob.Store, error) { return f.disk, nil }
	f.exec(t, `UPDATE sources SET format='oci' WHERE id=$1`, src)
	f.exec(t, `UPDATE feeds SET format='oci'`)
	var component string
	if err := f.pool.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'team/app') RETURNING id::text`, f.instance, src).Scan(&component); err != nil {
		t.Fatal(err)
	}
	put := func(version, key string, data []byte, ageHours int) {
		t.Helper()
		info := sum(data)
		if _, err := f.disk.Put(ctx, bytes.NewReader(data), info); err != nil {
			t.Fatal(err)
		}
		var vid, bid string
		age := fmt.Sprintf("%d hours", ageHours)
		if err := f.pool.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version,created_at) VALUES($1,$2,$3,now()-$4::interval) ON CONFLICT(component_id,version) DO UPDATE SET version=excluded.version RETURNING id::text`, f.instance, component, version, age).Scan(&vid); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text`, f.instance, dom, info.Digest, info.Size).Scan(&bid); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,created_at) VALUES($1,$2,$3,$4,now()-$5::interval)`, f.instance, vid, key, bid, age)
	}
	shared := []byte("shared base layer")
	put(string(sum(shared).Digest), "blob", shared, 100)
	var manifests, uniques []string
	for i := 1; i <= 3; i++ {
		cfg, layer := []byte(fmt.Sprintf("config-%d", i)), []byte(fmt.Sprintf("unique layer %d", i))
		put(string(sum(cfg).Digest), "blob", cfg, 100)
		put(string(sum(layer).Digest), "blob", layer, 100)
		uniques = append(uniques, string(sum(cfg).Digest), string(sum(layer).Digest))
		m := []byte(fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"sha256:%s"},"layers":[{"digest":"sha256:%s"},{"digest":"sha256:%s"}]}`, sum(cfg).Digest, sum(shared).Digest, sum(layer).Digest))
		put(string(sum(m).Digest), "manifest", m, 50-i*10) // manifest 1 is the oldest
		manifests = append(manifests, string(sum(m).Digest))
	}
	if _, err := f.svc.SetPolicy(ctx, Policy{SourceID: src, KeepLast: 2, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	dry, err := f.svc.ApplyRetention(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"team/app@" + manifests[0], "team/app@" + uniques[0], "team/app@" + uniques[1]}
	slices.Sort(want)
	got := names(dry.Deleted)
	if !slices.Equal(got, want) {
		t.Fatalf("dry run deletes %v, want %v", got, want)
	}
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !slices.Equal(names(rep.Deleted), want) {
		t.Fatalf("real run %+v %v", rep, err)
	}
	if n := f.count(t, `SELECT count(*) FROM versions WHERE NOT blob_only`); n != 2 {
		t.Fatalf("manifests left: %d", n)
	}
	f.ageBlobs(t, 5*time.Hour)
	for range 2 {
		if _, err = f.svc.RunOnce(ctx, false); err != nil {
			t.Fatal(err)
		}
		f.ageCandidates(t, 5*time.Hour)
	}
	if f.hasBytes(sum([]byte("config-1")).Digest) || f.hasBytes(sum([]byte("unique layer 1")).Digest) {
		t.Fatal("blobs unique to the deleted manifest were not freed")
	}
	for _, keep := range [][]byte{shared, []byte("config-2"), []byte("unique layer 2"), []byte("config-3"), []byte("unique layer 3")} {
		if !f.hasBytes(sum(keep).Digest) {
			t.Fatalf("referenced blob %q lost", keep)
		}
	}
}
