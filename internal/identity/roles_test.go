package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBootstrapConcurrentAndRecovery(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, "TRUNCATE instances CASCADE"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 6)
	identities := make(chan BootstrapIdentity, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			identity, err := Bootstrap(ctx, f.pool, "Initial")
			results <- err
			if err == nil {
				identities <- identity
			}
		}()
	}
	wg.Wait()
	close(results)
	close(identities)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrAlreadyBootstrapped) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("bootstrap successes=%d", wins)
	}
	identity := <-identities
	recovered, err := LoadBootstrapIdentity(ctx, f.pool)
	if err != nil || recovered != identity {
		t.Fatalf("recovery mismatch: %v", err)
	}
	if f.count(t, "instances") != 1 || f.count(t, "principals") != 1 || f.count(t, "roles") != 1 || f.count(t, "role_grants") != 4 || f.count(t, "audit_events") != 1 || f.count(t, "outbox_events") != 1 || f.count(t, "service_tokens") != 0 {
		t.Fatal("bootstrap did not atomically create expected records without a token")
	}
	principal := Principal{InstanceID: identity.InstanceID, PrincipalID: identity.PrincipalID}
	policy, err := (Roles{DB: f.pool}).Resolve(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ResourceKind{ArtifactResource, FeedResource, SourceResource, InstanceResource} {
		for _, action := range []Action{Read, Publish, Delete, Configure, Replicate, Admin} {
			if !policy.Authorize(principal, Resource{InstanceID: identity.InstanceID, Kind: kind}, action) {
				t.Fatalf("bootstrap missing %s/%s", kind, action)
			}
		}
	}
	// Even bootstrap role grants cannot exceed a machine credential's scopes.
	principal.TokenID = "scoped"
	principal.Scopes = []Scope{{Actions: []Action{Read}}}
	if policy.Authorize(principal, Resource{InstanceID: identity.InstanceID, Kind: InstanceResource}, Admin) {
		t.Fatal("bootstrap role escaped token scope")
	}
	if _, err = f.pool.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", identity.PrincipalID); err != nil {
		t.Fatal(err)
	}
	policy, err = (Roles{DB: f.pool}).Resolve(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Authorize(principal, Resource{InstanceID: identity.InstanceID, Kind: InstanceResource}, Read) {
		t.Fatal("disabled principal retained role")
	}
	if _, err = LoadBootstrapIdentity(ctx, f.pool); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled bootstrap recovery accepted")
	}
}

func TestBootstrapFailureAtomicity(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	if _, err := Bootstrap(ctx, f.pool, "Existing"); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatal("bootstrap accepted preexisting instance")
	}
	if _, err := f.pool.Exec(ctx, "TRUNCATE instances CASCADE"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "   ", "line\nbreak"} {
		if _, err := Bootstrap(ctx, f.pool, name); !errors.Is(err, ErrBootstrapInput) {
			t.Fatal("invalid bootstrap name accepted")
		}
	}
	if _, err := f.pool.Exec(ctx, "ALTER TABLE outbox_events ADD CONSTRAINT reject_bootstrap CHECK(topic<>'instance.bootstrap')"); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, f.pool, "Initial"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("bootstrap survived failed event")
	}
	for _, table := range []string{"instances", "principals", "roles", "principal_roles", "role_grants", "bootstrap_state", "audit_events", "outbox_events"} {
		if f.count(t, table) != 0 {
			t.Fatalf("partial bootstrap in %s", table)
		}
	}
	if _, err := f.pool.Exec(ctx, "ALTER TABLE outbox_events DROP CONSTRAINT reject_bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := Bootstrap(ctx, f.pool, "Retry"); err != nil {
		t.Fatal("failed bootstrap not retryable", err)
	}
}

func TestRoleResolutionIsolationAndDeny(t *testing.T) {
	f := newTokenFixture(t)
	ctx := context.Background()
	var role, source string
	if err := f.pool.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,'publisher') RETURNING id", f.instance).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind) VALUES($1,'local','local') RETURNING id", f.instance).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)", f.instance, f.service, role); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$2,'feed','allow',$3,ARRAY['read','publish'])", f.instance, role, f.feed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$2,'feed','deny',$3,ARRAY['publish'])", f.instance, role, f.feed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,source_id,actions) VALUES($1,$2,'source','allow',$3,ARRAY['configure'])", f.instance, role, source); err != nil {
		t.Fatal(err)
	}
	p := Principal{InstanceID: f.instance, PrincipalID: f.service, TokenID: "token", Scopes: []Scope{{Actions: []Action{Read, Publish, Configure}}}}
	policy, err := (Roles{DB: f.pool}).Resolve(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	resource := Resource{InstanceID: f.instance, FeedID: f.feed, Kind: FeedResource}
	if !policy.Authorize(p, resource, Read) || policy.Authorize(p, resource, Publish) {
		t.Fatal("persisted allow/deny mismatch")
	}
	if !policy.Authorize(p, Resource{InstanceID: f.instance, SourceID: source, Kind: SourceResource}, Configure) {
		t.Fatal("source selector not loaded")
	}
	if policy.Authorize(p, Resource{InstanceID: f.instance, SourceID: f.feed, Kind: SourceResource}, Configure) {
		t.Fatal("source selector escaped")
	}
	p.PrincipalID = f.actor
	policy, err = (Roles{DB: f.pool}).Resolve(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Authorize(p, resource, Read) {
		t.Fatal("unbound principal gained role")
	}
	p.PrincipalID = f.service
	p.InstanceID = f.other
	policy, err = (Roles{DB: f.pool}).Resolve(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Roles) != 0 {
		t.Fatal("role crossed instance boundary")
	}
	for _, query := range []string{
		"INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)",
		"INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$3,'feed','allow',$2,ARRAY['read'])",
	} {
		if _, err := f.pool.Exec(ctx, query, f.other, f.service, role); err == nil {
			t.Fatal("cross-instance role relation accepted")
		}
	}
	if _, err := f.pool.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$2,'feed','allow',$3,ARRAY['read'])", f.instance, role, f.foreignFeed); err == nil {
		t.Fatal("foreign feed grant accepted")
	}
}
