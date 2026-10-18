package replication

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
)

func TestDestinationReadOnlyRejectsPublish(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)

	publish := func(svc *artifact.Service, ns, coord string) error {
		_, err := svc.Publish(ctx, dst.admin, artifact.Publication{FeedID: dst.feed, Namespace: ns, Coordinate: coord, Version: "1", AssetKey: "f.bin", MediaType: "application/octet-stream", Info: digestOf(data(coord))}, strings.NewReader(string(data(coord))))
		return err
	}

	// Jobs are read-only replicas by default.
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	if !job.DestinationReadOnly {
		t.Fatal("new job is not read-only by default")
	}
	err := publish(dst.publisher, "team", "team/a")
	var replica *artifact.ReplicaFeedError
	if !errors.Is(err, artifact.ErrConflict) || !errors.As(err, &replica) || replica.Peer != srv.URL || !strings.Contains(err.Error(), "replica feed") || !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("publish to read-only replica feed: %v", err)
	}

	// An explicit publish rule routing to a separate local source is the override.
	other, otherDomain := dst.newSource("local-overlay", "local")
	_ = otherDomain
	dst.attach(dst.feed, other, 5)
	compiled, cerr := routing.Compile(routing.Config{
		Sources:       []routing.Source{{ID: dst.source, Kind: routing.Local}, {ID: other, Kind: routing.Local}},
		Rules:         []routing.Rule{{ID: "overlay", Priority: 1, Selector: routing.Selector{Namespace: "team"}, Target: other}},
		DefaultTarget: dst.source,
	})
	if cerr != nil {
		t.Fatal(cerr)
	}
	routed := &artifact.Service{DB: dst.pool, Lease: dst.mgr.Lease, Router: func(context.Context, string) (*routing.Router, error) { return compiled, nil }, Storage: dst.stores.Resolve}
	if err := publish(routed, "team", "team/b"); err != nil {
		t.Fatalf("explicit override rejected: %v", err)
	}
	if err := publish(routed, "elsewhere", "elsewhere/c"); !errors.Is(err, artifact.ErrConflict) {
		t.Fatalf("default route must stay read-only: %v", err)
	}

	// Switching the flag off allows default publishes again.
	if _, err := svc.UpdateJob(ctx, dst.instance, dst.admin.PrincipalID, job.ID, JobPatch{Revision: job.Revision, DestinationReadOnly: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := publish(dst.publisher, "team", "team/d"); err != nil {
		t.Fatalf("publish after clearing read-only: %v", err)
	}

	// A job created with the flag explicitly off never blocks publishes.
	dst2 := newSite(t, "dst2", "mirror2")
	svc2 := dst2.service(t, srv, nil)
	job2 := link(t, src, dst2, srv, svc2, src.feed, dst2.feed, JobInput{DestinationReadOnly: ptr(false)})
	if job2.DestinationReadOnly {
		t.Fatal("explicit false ignored")
	}
}

func TestDestinationReadOnlyRejectsBundlePublish(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "bsrc", "artifacts"), newSite(t, "bdst", "mirror")
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	body := data("team/bundle")
	pub := artifact.Publication{FeedID: dst.feed, Namespace: "team", Coordinate: "team/bundle", Version: "1", AssetKey: "f.bin", MediaType: "application/octet-stream", Info: digestOf(body)}
	_, err := dst.publisher.PublishBundle(ctx, dst.admin, []artifact.Publication{pub}, []io.Reader{strings.NewReader(string(body))})
	var replica *artifact.ReplicaFeedError
	if !errors.Is(err, artifact.ErrConflict) || !errors.As(err, &replica) {
		t.Fatalf("bundle publish to read-only replica feed: %v", err)
	}
}
