package main

import (
	"context"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
)

func TestWorkersWithoutRuntimeAreInactive(t *testing.T) {
	// No pool or keyring is available before bootstrap. The no-op must not touch
	// either dependency or start a dispatcher whose empty instance means all.
	workers, err := startWorkers(context.Background(), nil, nil, nil)
	if err != nil || workers != nil {
		t.Fatalf("unbootstrapped workers: %v %v", workers, err)
	}
}

func TestWorkersRejectMissingOwnershipBeforeDependencies(t *testing.T) {
	for name, runtime := range map[string]*artifactRuntime{
		"missing instance": {},
		"nil manager":      {instanceID: "configured-instance"},
		"inactive manager": {instanceID: "configured-instance", manager: new(ownership.Manager)},
	} {
		t.Run(name, func(t *testing.T) {
			// Nil dependencies make the ownership rejection observable independently
			// of dispatcher configuration validation or database side effects.
			workers, err := startWorkers(context.Background(), nil, runtime, nil)
			if workers != nil || err == nil || err.Error() != "instance ownership unavailable" {
				t.Fatalf("expected ownership rejection before dependencies: %v %v", workers, err)
			}
		})
	}
}

func TestWorkersRejectTerminatedManagerAndInstanceMismatch(t *testing.T) {
	pool, _ := runtimeFixture(t)
	owner, err := identity.Bootstrap(context.Background(), pool, "Worker regression")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := ownership.Start(context.Background(), pool, owner.InstanceID, "00000000-0000-4000-8000-000000000041", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	runtime := &artifactRuntime{instanceID: "different-instance", manager: manager}
	workers, err := startWorkers(context.Background(), nil, runtime, nil)
	if workers != nil || err == nil || err.Error() != "instance ownership unavailable" {
		t.Fatal("accepted mismatched ownership", err)
	}
	runtime.instanceID = owner.InstanceID
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.Lease().InstanceID != "" {
		t.Fatal("terminated manager retained active lease")
	}
	workers, err = startWorkers(context.Background(), nil, runtime, nil)
	if workers != nil || err == nil || err.Error() != "instance ownership unavailable" {
		t.Fatal("lost manager activated unscoped dispatcher", err)
	}
}
