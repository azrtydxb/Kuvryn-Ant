package replication

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Deleting a job keeps the replicated content served but frees the job name.
func TestDeleteJobFreesNameAndKeepsContent(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "team", "team/app", "1.0", "app.bin", "", data("app"))
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{Name: "reuse-me"})
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteJob(ctx, dst.instance, dst.admin.PrincipalID, job.ID); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := dst.pool.QueryRow(ctx, `SELECT name FROM sources WHERE id=$1`, job.ReplicaSourceID).Scan(&name); err != nil || !strings.HasPrefix(name, "replica-retired:reuse-me-") {
		t.Fatalf("replica source not retired: %q %v", name, err)
	}
	if got, err := dst.read(dst.feed, "team", "team/app", "1.0", "app.bin"); err != nil || !bytes.Equal(got, data("app")) {
		t.Fatalf("replicated content lost after job delete: %v", err)
	}
	again := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{Name: "reuse-me"})
	if again.ReplicaSourceID == job.ReplicaSourceID {
		t.Fatal("new job reused the retired source")
	}
}
