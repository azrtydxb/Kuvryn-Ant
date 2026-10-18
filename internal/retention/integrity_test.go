package retention

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestIntegrityScanReportsMismatchesInBoundedBatches(t *testing.T) {
	f := newFixture(t, Options{IntegrityBatch: 2})
	ctx := context.Background()
	bodies := map[string][]byte{"intact": []byte("intact bytes"), "truncated": []byte("truncated bytes"), "flipped": []byte("flipped bytes!"), "gone": []byte("deleted bytes")}
	infos := map[string]string{}
	for name, body := range bodies {
		f.publish(t, name, body)
		infos[name] = string(sum(body).Digest)
	}
	object := func(name string) string { return filepath.Join(f.root, "objects", infos[name]) }
	for _, name := range []string{"truncated", "flipped"} {
		if err := os.Chmod(object(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(object("truncated"), bodies["truncated"][:4], 0o600); err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte(nil), bodies["flipped"]...)
	flipped[0] ^= 0xff
	if err := os.WriteFile(object("flipped"), flipped, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(object("gone")); err != nil {
		t.Fatal(err)
	}

	found := map[string]string{}
	runs, checked := 0, 0
	for {
		rep, err := f.svc.VerifyIntegrity(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		runs++
		if rep.Checked > 2 {
			t.Fatalf("batch bound exceeded: %d", rep.Checked)
		}
		checked += rep.Checked
		for _, m := range rep.Mismatches {
			found[m.Digest] = m.Kind
		}
		if rep.Wrapped {
			break
		}
		if runs > 5 {
			t.Fatal("scan never wrapped")
		}
	}
	if checked != 4 || runs != 3 {
		t.Fatalf("checked %d blobs in %d runs", checked, runs)
	}
	want := map[string]string{infos["truncated"]: "size", infos["flipped"]: "digest", infos["gone"]: "missing"}
	if len(found) != 3 {
		t.Fatalf("found %v", found)
	}
	for d, k := range want {
		if found[d] != k {
			t.Fatalf("digest %s: want %s got %q", d, k, found[d])
		}
	}
	open, err := f.svc.OpenFindings(ctx)
	if err != nil || len(open) != 3 {
		t.Fatalf("open findings %v %v", open, err)
	}
	// Re-running the scan does not duplicate findings; repair resolves them.
	for range 3 {
		if _, err = f.svc.VerifyIntegrity(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	if open, _ = f.svc.OpenFindings(ctx); len(open) != 3 {
		t.Fatalf("duplicated findings: %v", open)
	}
	if err = os.WriteFile(object("flipped"), bodies["flipped"], 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err = f.svc.VerifyIntegrity(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	open, _ = f.svc.OpenFindings(ctx)
	var kinds []string
	for _, o := range open {
		kinds = append(kinds, o.Kind)
	}
	sort.Strings(kinds)
	if len(kinds) != 2 || kinds[0] != "missing" || kinds[1] != "size" {
		t.Fatalf("repair not reflected: %v", kinds)
	}
	// Nothing is deleted by the scan.
	if f.count(t, "SELECT count(*) FROM blobs") != 4 {
		t.Fatal("integrity scan modified metadata")
	}
}

func TestIntegrityDryRunDoesNotPersist(t *testing.T) {
	f := newFixture(t, Options{})
	body := []byte("dry run bytes")
	f.publish(t, "1.0", body)
	if err := os.Remove(filepath.Join(f.root, "objects", string(sum(body).Digest))); err != nil {
		t.Fatal(err)
	}
	rep, err := f.svc.VerifyIntegrity(context.Background(), true)
	if err != nil || len(rep.Mismatches) != 1 || rep.Mismatches[0].Kind != "missing" {
		t.Fatalf("%+v %v", rep, err)
	}
	if f.count(t, "SELECT count(*) FROM blob_integrity_findings")+f.count(t, "SELECT count(*) FROM retention_cursors") != 0 {
		t.Fatal("dry-run persisted state")
	}
}
