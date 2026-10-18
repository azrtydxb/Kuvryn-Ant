package artifact

import (
	"bytes"
	"context"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"strings"
	"testing"
)

func TestSubjectImmutableReplayAndTransactionRollback(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	data := []byte("subject metadata fixture")
	p := f.publication(data)
	p.AssetKey = "manifest"
	p.Subject = &Subject{Info: blob.Info{Digest: blob.Digest(strings.Repeat("a", 64)), Size: 100}, MediaType: "application/vnd.oci.image.manifest.v1+json"}
	p.ArtifactType = "example/sbom"
	p.Annotations = map[string]string{"annotation": "original"}
	if _, err := f.service.Publish(ctx, f.principal, p, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if rec, err := f.service.Publish(ctx, f.principal, p, bytes.NewReader(data)); err != nil || !rec.Replayed {
		t.Fatal(rec, err)
	}
	p.Annotations["annotation"] = "different"
	if _, err := f.service.Publish(ctx, f.principal, p, bytes.NewReader(data)); !errors.Is(err, ErrConflict) {
		t.Fatal("replay changed relation", err)
	}
	p.Annotations["annotation"] = "original"
	p.Version = "2.0"
	if _, err := f.pool.Exec(ctx, "ALTER TABLE audit_events ADD CONSTRAINT reject_subject CHECK(action<>'artifact.publish') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Publish(ctx, f.principal, p, bytes.NewReader(data)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	var n int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM artifact_subjects").Scan(&n); err != nil || n != 1 {
		t.Fatal("non-atomic association", n, err)
	}
	if _, err := f.service.Lookup(ctx, f.principal, p); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed transaction visible", err)
	}
}
func TestNamespaceCannotBeReinterpreted(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	data := []byte("namespace pinned")
	p := f.publication(data)
	if _, err := f.service.Publish(ctx, f.principal, p, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.Namespace = "another"
	reader := &countedReader{}
	if _, err := f.service.Publish(ctx, f.principal, wrong, reader); !errors.Is(err, ErrConflict) || reader.reads.Load() != 0 {
		t.Fatal("namespace rewrite consumed body", err)
	}
	if _, err := f.service.Lookup(ctx, f.principal, wrong); !errors.Is(err, ErrNotFound) {
		t.Fatal("read namespace spoof", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE components SET namespace=NULL WHERE coordinate=$1", p.Coordinate); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Lookup(ctx, f.principal, p); !errors.Is(err, ErrNotFound) {
		t.Fatal("legacy unknown namespace readable", err)
	}
	reader = &countedReader{}
	if _, err := f.service.Publish(ctx, f.principal, p, reader); !errors.Is(err, ErrConflict) || reader.reads.Load() != 0 {
		t.Fatal("legacy unknown namespace writable", err)
	}
}
