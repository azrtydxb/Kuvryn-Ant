package database

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMutationAtomicReplayAndIsolation(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var instance, principal, other string
	if err := pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('mutations') RETURNING id").Scan(&instance); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []*string{&principal, &other} {
		if err := pool.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user',gen_random_uuid()::text) RETURNING id", instance).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	req := MutationRequest{InstanceID: instance, PrincipalID: principal, Operation: "create-feed", Resource: "feeds", Key: "private-key", Fingerprint: sha256.Sum256([]byte("body")), NonSecretResult: true}
	var calls atomic.Int32
	apply := func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		n := calls.Add(1)
		_, err := tx.Exec(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,$2,'raw')", instance, fmt.Sprintf("feed-%d", n))
		return json.RawMessage(`{"ok":true}`), err
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	var fresh atomic.Int32
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := ExecuteMutation(ctx, pool, req, apply)
			if err == nil && !r.Replayed {
				fresh.Add(1)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || fresh.Load() != 1 {
		t.Fatalf("callbacks=%d fresh=%d", calls.Load(), fresh.Load())
	}
	changed := req
	changed.Fingerprint = sha256.Sum256([]byte("different"))
	if _, err := ExecuteMutation(ctx, pool, changed, apply); !errors.Is(err, ErrFingerprintConflict) {
		t.Fatalf("fingerprint conflict: %v", err)
	}
	for _, change := range []func(*MutationRequest){func(r *MutationRequest) { r.PrincipalID = other }, func(r *MutationRequest) { r.Operation = "other" }, func(r *MutationRequest) { r.Resource = "other" }} {
		independent := req
		change(&independent)
		if r, err := ExecuteMutation(ctx, pool, independent, apply); err != nil || r.Replayed {
			t.Fatalf("scope collision: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE mutation_results SET expires_at=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if r, err := ExecuteMutation(ctx, pool, changed, apply); err != nil || r.Replayed {
		t.Fatalf("expiry failed: %v", err)
	}
	secret := req
	secret.NonSecretResult = false
	if _, err := ExecuteMutation(ctx, pool, secret, apply); !errors.Is(err, ErrMutationInput) {
		t.Fatalf("secret response enabled: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM feeds").Scan(&count); err != nil || count != 5 {
		t.Fatalf("effects=%d err=%v", count, err)
	}
}

func TestMutationRollbackAndRevisions(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var instance, principal, feed, other string
	for _, dest := range []*string{&instance, &other} {
		if err := pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('revision') RETURNING id").Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','subject') RETURNING id", instance).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'original','raw') RETURNING id", instance).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	req := MutationRequest{InstanceID: instance, PrincipalID: principal, Operation: "update", Resource: feed, Key: "key", NonSecretResult: true}
	update := FeedUpdate{InstanceID: instance, ID: feed, Slug: "updated", ExpectedRevision: 1}
	callback := func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		_, err := UpdateFeed(ctx, tx, update)
		return json.RawMessage(`{"revision":2}`), err
	}
	for _, failure := range []func(context.Context, pgx.Tx) (json.RawMessage, error){
		func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
			_, err := callback(ctx, tx)
			if err != nil {
				return nil, err
			}
			return nil, errors.New("secret database detail")
		},
		func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
			_, err := callback(ctx, tx)
			return json.RawMessage("invalid JSON"), err
		},
	} {
		if _, err := ExecuteMutation(ctx, pool, req, failure); err == nil || err.Error() == "secret database detail" {
			t.Fatalf("unsafe failure: %v", err)
		}
		var revision, count int
		if err := pool.QueryRow(ctx, "SELECT revision FROM feeds WHERE id=$1", feed).Scan(&revision); err != nil || revision != 1 {
			t.Fatalf("mutation survived rollback: %d %v", revision, err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM mutation_results").Scan(&count); err != nil || count != 0 {
			t.Fatalf("replay survived rollback: %d %v", count, err)
		}
	}
	if _, err := ExecuteMutation(ctx, pool, req, callback); err != nil {
		t.Fatal(err)
	}
	req.Key = "stale"
	if _, err := ExecuteMutation(ctx, pool, req, callback); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: %v", err)
	}
	update.InstanceID = other
	if _, err := ExecuteMutation(ctx, pool, req, callback); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-instance update: %v", err)
	}
}

func TestFeedConcurrentRevisionWinner(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var instance, feed string
	if err := pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('race') RETURNING id").Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'race','raw') RETURNING id", instance).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	outcomes := make(chan error, 2)
	for range 2 {
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				outcomes <- err
				return
			}
			defer tx.Rollback(ctx)
			_, err = UpdateFeed(ctx, tx, FeedUpdate{InstanceID: instance, ID: feed, Slug: "winner", ExpectedRevision: 1})
			if err == nil {
				err = tx.Commit(ctx)
			}
			outcomes <- err
		}()
	}
	wins, conflicts := 0, 0
	for range 2 {
		err := <-outcomes
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrRevisionConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}
