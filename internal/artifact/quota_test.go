package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/limits"
)

func (f publishFixture) setQuotas(t *testing.T, quotas string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO limits_config(instance_id,quotas) VALUES($1,$2::jsonb) ON CONFLICT(instance_id) DO UPDATE SET quotas=excluded.quotas`, f.principal.InstanceID, quotas); err != nil {
		t.Fatal(err)
	}
}

func (f publishFixture) publishAs(t *testing.T, namespace, coordinate, version string, data []byte) error {
	t.Helper()
	p := f.publication(data)
	p.Namespace, p.Coordinate, p.Version = namespace, coordinate, version
	_, err := f.service.Publish(context.Background(), f.principal, p, bytes.NewReader(data))
	return err
}

func TestQuotaRejectsAtCommitAndLeavesNothingBehind(t *testing.T) {
	f := newPublishFixture(t)
	f.setQuotas(t, fmt.Sprintf(`[{"scope":"feed","feed_id":%q,"max_bytes":20}]`, f.feed))
	if err := f.publishAs(t, "team", "team/a", "1", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if err := f.publishAs(t, "team", "team/a", "2", []byte("abcdefghij")); err != nil {
		t.Fatal("publication exactly at the limit must be accepted:", err)
	}
	before := f.count(t, "SELECT count(*) FROM assets")
	err := f.publishAs(t, "team", "team/b", "1", []byte("x"))
	var quota *limits.QuotaError
	if !errors.As(err, &quota) || !errors.Is(err, limits.ErrQuota) || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected quota error, got %v", err)
	}
	if quota.Dimension != "bytes" || quota.Limit != 20 || quota.Used != 20 || quota.Requested != 1 || !strings.Contains(err.Error(), "storage quota exceeded for feed "+f.feed) {
		t.Fatalf("unclear quota error: %+v / %v", quota, err)
	}
	// Atomic: neither the component, version, blob nor audit of the rejected publication exist.
	if f.count(t, "SELECT count(*) FROM assets") != before || f.count(t, "SELECT count(*) FROM components WHERE coordinate='team/b'") != 0 ||
		f.count(t, "SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='team/b'") != 0 ||
		f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.publish'") != before {
		t.Fatal("rejected publication left state behind")
	}
	// Replaying an accepted publication is not a new asset and stays allowed at the limit.
	if err = f.publishAs(t, "team", "team/a", "1", []byte("0123456789")); err != nil {
		t.Fatal("idempotent replay must not be quota-checked:", err)
	}
}

func TestPackageAndNamespaceQuotas(t *testing.T) {
	f := newPublishFixture(t)
	f.setQuotas(t, fmt.Sprintf(`[{"scope":"feed","feed_id":%q,"max_packages":2},{"scope":"namespace","feed_id":%q,"namespace":"small","max_bytes":4}]`, f.feed, f.feed))
	for _, c := range []string{"p1", "p2"} {
		if err := f.publishAs(t, "big", c, "1", []byte("data-"+c)); err != nil {
			t.Fatal(err)
		}
	}
	// A new version of an existing package is not another package.
	if err := f.publishAs(t, "big", "p1", "2", []byte("data-p1-v2")); err != nil {
		t.Fatal(err)
	}
	err := f.publishAs(t, "big", "p3", "1", []byte("data-p3"))
	var quota *limits.QuotaError
	if !errors.As(err, &quota) || quota.Dimension != "packages" || quota.Limit != 2 || quota.Used != 2 {
		t.Fatalf("package quota: %v", err)
	}
	// Namespace quota only constrains its own namespace; here the feed package quota binds first.
	f.setQuotas(t, fmt.Sprintf(`[{"scope":"namespace","feed_id":%q,"namespace":"small","max_bytes":4}]`, f.feed))
	if err = f.publishAs(t, "small", "s1", "1", []byte("12345")); !errors.Is(err, limits.ErrQuota) {
		t.Fatalf("namespace quota not enforced: %v", err)
	}
	if err = f.publishAs(t, "small", "s1", "1", []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if err = f.publishAs(t, "big", "p3", "1", []byte("data-p3-is-large-but-unconstrained")); err != nil {
		t.Fatal("other namespaces must be unaffected:", err)
	}
}

func TestQuotaConcurrentPublicationsCannotOversubscribe(t *testing.T) {
	f := newPublishFixture(t)
	f.setQuotas(t, fmt.Sprintf(`[{"scope":"feed","feed_id":%q,"max_bytes":30}]`, f.feed))
	errs := make(chan error, 6)
	for i := range 6 {
		go func() {
			errs <- f.publishAs(t, "team", fmt.Sprintf("team/c%d", i), "1", []byte("0123456789-"))
		}()
	}
	accepted := 0
	for range 6 {
		err := <-errs
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, limits.ErrQuota):
		default:
			t.Fatal(err)
		}
	}
	var used int64
	if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(b.size_bytes),0) FROM assets a JOIN blobs b ON b.id=a.blob_id`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if accepted != 2 || used != 22 {
		t.Fatalf("accepted=%d used=%d, want 2 publications of 11 bytes under a 30 byte quota", accepted, used)
	}
}
