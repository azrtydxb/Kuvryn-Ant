package events

import (
	"context"
	"sync"
	"time"
)

const deliveryWorkers = 4
const subscriptionPageSize = 100
const materializeBatchSize = 200

// Progress contains operational counters for one completed iteration. Attempts
// includes retryable/permanent HTTP failures, not only successful deliveries.
type Progress struct{ Materialized, Attempts, StorageFailures int }

func (d *Dispatcher) observe(p Progress) {
	if d.options.Observe != nil {
		d.options.Observe(p)
	}
}
func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func retryDelay(failures int) time.Duration {
	if failures > 5 {
		failures = 5
	}
	if failures < 1 {
		failures = 1
	}
	return time.Second * time.Duration(1<<uint(failures-1))
}

// Run owns four delivery workers and one subscription discovery worker. Calling
// it concurrently on the same dispatcher is rejected. Cancellation joins all
// workers, including bounded delivery finalization. The caller owns Close and
// must call it only after Run returns. Storage outages retry with bounded delays.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d == nil || d.db == nil || !d.running.CompareAndSwap(false, true) {
		return ErrConfiguration
	}
	defer d.running.Store(false)
	var wg sync.WaitGroup
	for range deliveryWorkers {
		wg.Add(1)
		go func() { defer wg.Done(); d.deliveryLoop(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.discoveryLoop(ctx) }()
	wg.Wait()
	return ctx.Err()
}
func (d *Dispatcher) deliveryLoop(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		attempted, err := d.DeliverOne(ctx)
		if attempted {
			d.observe(Progress{Attempts: 1})
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			d.observe(Progress{StorageFailures: 1})
			if !wait(ctx, retryDelay(failures)) {
				return
			}
			continue
		}
		failures = 0
		if !attempted && !wait(ctx, time.Second) {
			return
		}
	}
}
func (d *Dispatcher) subscriptions(ctx context.Context, after string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := d.db.Query(ctx, "SELECT id::text FROM event_subscriptions WHERE enabled AND ($1='' OR instance_id=NULLIF($1,'')::uuid) AND ($2='' OR id>NULLIF($2,'')::uuid) ORDER BY id LIMIT $3", d.options.InstanceID, after, subscriptionPageSize)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	ids := make([]string, 0, subscriptionPageSize)
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			return nil, ErrUnavailable
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return ids, nil
}
func (d *Dispatcher) discoveryLoop(ctx context.Context) {
	after := ""
	failures := 0
	for ctx.Err() == nil {
		ids, err := d.subscriptions(ctx, after)
		if err == nil {
			for _, id := range ids {
				var count int
				count, err = d.Materialize(ctx, id, materializeBatchSize)
				if err != nil {
					break
				}
				after = id
				if count > 0 {
					d.observe(Progress{Materialized: count})
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			d.observe(Progress{StorageFailures: 1})
			if !wait(ctx, retryDelay(failures)) {
				return
			}
			continue
		}
		failures = 0
		if len(ids) < subscriptionPageSize {
			// Begin a fresh keyset sweep: subscriptions inserted below the previous
			// cursor and late-committing lower outbox IDs are discovered next time.
			after = ""
			if !wait(ctx, time.Second) {
				return
			}
		}
	}
}
