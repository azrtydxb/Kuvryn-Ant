package oci

import (
	"context"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
)

// TestCreateUploadLockOrderMatchesPublish is the regression for deadlocks
// (40P01) seen on multi-platform pushes: publication locks the writer lease and
// then the feed row, so session creation must take the lease before inserting
// (the insert key-share locks the feed row through its foreign key).
func TestCreateUploadLockOrderMatchesPublish(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := identity.Principal{InstanceID: f.instance, PrincipalID: f.principal}
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if err = ownership.Require(ctx, holder, f.service.Lease()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, cerr := f.registry.createUpload(ctx, p, repository{feed: f.feed, name: "app", full: "images/app"}, f.source)
		done <- cerr
	}()
	time.Sleep(300 * time.Millisecond) // let createUpload reach its lock wait
	if _, err = holder.Exec(ctx, `SELECT 1 FROM feeds WHERE id=$1 FOR UPDATE`, f.feed); err != nil {
		t.Fatal("publish-order lock failed (deadlock with createUpload):", err)
	}
	if err = holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("createUpload did not finish")
	}
}
