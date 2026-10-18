// Package cache holds bounded metadata and coalesces misses in one process.
// Blob durability and distributed coalescing are deliberately separate concerns.
package cache

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrCapacity = errors.New("metadata cache capacity exceeded")
var ErrInput = errors.New("invalid metadata cache input")

type Key struct{ Source, Identity, Representation, AuthorizationDomain string }
type Value struct {
	Status                        int
	Body                          []byte
	ETag, LastModified, MediaType string
	SignedExpiry                  time.Time
	// Stale is set only on a copy returned because revalidation failed and the
	// entry was still inside its stale-if-error window. It is never stored.
	Stale bool
}
type Config struct {
	MaxEntries                         int
	MaxBytes                           int64
	MetadataTTL, NegativeTTL, StaleFor time.Duration
	Now                                func() time.Time
}
type GetOptions struct{ AllowStale bool }
type Loader func(context.Context) (Value, error)
type entry struct {
	value            Value
	fresh, stale     time.Time
	cost             int64
	used, generation uint64
}
type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   Value
	err     error
}
type Cache struct {
	mu      sync.Mutex
	cfg     Config
	entries map[Key]entry
	flights map[Key]*flight
	bytes   int64
	tick    uint64
	running int
}

func New(c Config) (*Cache, error) {
	if c.MaxEntries == 0 {
		c.MaxEntries = 1024
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 16 << 20
	}
	if c.MetadataTTL == 0 {
		c.MetadataTTL = 5 * time.Minute
	}
	if c.NegativeTTL == 0 {
		c.NegativeTTL = 30 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MaxEntries < 1 || c.MaxBytes < 1 || c.MetadataTTL < 0 || c.NegativeTTL < 0 || c.StaleFor < 0 {
		return nil, ErrInput
	}
	return &Cache{cfg: c, entries: map[Key]entry{}, flights: map[Key]*flight{}}, nil
}
func clone(v Value) Value { v.Body = append([]byte(nil), v.Body...); return v }
func valid(k Key) bool {
	for _, s := range []string{k.Source, k.Identity, k.Representation, k.AuthorizationDomain} {
		if len(s) == 0 || len(s) > 8192 {
			return false
		}
	}
	return true
}
func cost(k Key, v Value) int64 {
	return int64(len(k.Source) + len(k.Identity) + len(k.Representation) + len(k.AuthorizationDomain) + len(v.Body) + len(v.ETag) + len(v.LastModified) + len(v.MediaType) + 256)
}
func signedValid(v Value, now time.Time) bool {
	return v.SignedExpiry.IsZero() || now.Before(v.SignedExpiry)
}
func (c *Cache) Get(ctx context.Context, k Key, o GetOptions, loader Loader) (Value, error) {
	if loader == nil {
		return Value{}, ErrInput
	}
	return c.Revalidate(ctx, k, o, func(ctx context.Context, _ *Value) (Value, error) { return loader(ctx) })
}

// Revalidate supplies a private copy of the previous response to the loader for
// conditional requests. A 304 refreshes freshness but never signed validity.
func (c *Cache) Revalidate(ctx context.Context, k Key, o GetOptions, loader func(context.Context, *Value) (Value, error)) (Value, error) {
	if !valid(k) || loader == nil {
		return Value{}, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	c.mu.Lock()
	now := c.cfg.Now()
	old, exists := c.entries[k]
	if exists && now.Before(old.fresh) && signedValid(old.value, now) {
		c.tick++
		old.used = c.tick
		c.entries[k] = old
		c.mu.Unlock()
		return clone(old.value), nil
	}
	f := c.flights[k]
	if f == nil {
		if c.running >= c.cfg.MaxEntries {
			c.mu.Unlock()
			return Value{}, ErrCapacity
		}
		loadCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{done: make(chan struct{}), cancel: cancel}
		c.flights[k] = f
		c.running++
		var previous *Value
		if exists {
			v := clone(old.value)
			previous = &v
		}
		go c.load(loadCtx, k, f, func(ctx context.Context) (Value, error) {
			var original Value
			if previous != nil {
				original = clone(*previous)
			}
			value, err := loader(ctx, previous)
			if err == nil && value.Status == 304 {
				if previous == nil {
					return Value{}, ErrInput
				}
				renewed := original
				if value.ETag != "" {
					renewed.ETag = value.ETag
				}
				if value.LastModified != "" {
					renewed.LastModified = value.LastModified
				}
				return renewed, nil
			}
			return value, err
		})
	}
	f.waiters++
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.leave(k, f)
		return Value{}, ctx.Err()
	case <-f.done:
	}
	c.leave(k, f)
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	if f.err != nil && o.AllowStale && exists && c.canStale(k, old) {
		stale := clone(old.value)
		stale.Stale = true
		return stale, nil
	}
	return clone(f.value), f.err
}
func (c *Cache) leave(k Key, f *flight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.waiters--
	if f.waiters == 0 {
		f.cancel()
		if c.flights[k] == f {
			delete(c.flights, k)
		}
	}
}
func (c *Cache) load(ctx context.Context, k Key, f *flight, loader Loader) {
	value, err := loader(ctx)
	if err == nil && (value.Status < 100 || value.Status > 599) {
		err = ErrInput
	}
	if err == nil && !signedValid(value, c.cfg.Now()) {
		err = ErrInput
	}
	if err == nil && cost(k, value) > c.cfg.MaxBytes {
		err = ErrCapacity
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer f.cancel()
	c.running--
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		value = clone(value)
	} else {
		value = Value{}
	}
	f.value, f.err = value, err
	if c.flights[k] == f {
		delete(c.flights, k)
		if err == nil {
			// Unauthorized responses must neither be cached nor leave stale content
			// available for later failures under the same authorization-domain key.
			if value.Status == 401 || value.Status == 403 {
				c.remove(k)
			} else if (value.Status >= 200 && value.Status < 300) || value.Status == 404 || value.Status == 410 {
				c.insert(k, value)
			}
		}
	}
	close(f.done)
}
func (c *Cache) remove(k Key) {
	if e, ok := c.entries[k]; ok {
		c.bytes -= e.cost
		delete(c.entries, k)
	}
}
func (c *Cache) insert(k Key, v Value) {
	now := c.cfg.Now()
	if !signedValid(v, now) {
		c.remove(k)
		return
	}
	ttl := c.cfg.MetadataTTL
	if v.Status == 404 || v.Status == 410 {
		ttl = c.cfg.NegativeTTL
	}
	fresh := now.Add(ttl)
	stale := fresh
	if v.Status >= 200 && v.Status < 300 {
		stale = stale.Add(c.cfg.StaleFor)
	}
	if !v.SignedExpiry.IsZero() {
		if v.SignedExpiry.Before(fresh) {
			fresh = v.SignedExpiry
		}
		if v.SignedExpiry.Before(stale) {
			stale = v.SignedExpiry
		}
	}
	c.remove(k)
	n := cost(k, v)
	for len(c.entries) >= c.cfg.MaxEntries || c.bytes+n > c.cfg.MaxBytes {
		var victim Key
		var minimum uint64
		first := true
		for key, e := range c.entries {
			if first || e.used < minimum {
				first = false
				victim = key
				minimum = e.used
			}
		}
		if first {
			return
		}
		c.remove(victim)
	}
	c.tick++
	c.entries[k] = entry{v, fresh, stale, n, c.tick, c.tick}
	c.bytes += n
}

// Invalidate prevents an in-flight old generation from repopulating this key.
func (c *Cache) Invalidate(k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(k)
	if f := c.flights[k]; f != nil {
		delete(c.flights, k)
		f.cancel()
	}
}

type Stats struct {
	Entries, Inflight int
	Bytes             int64
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{len(c.entries), c.running, c.bytes}
}

func (c *Cache) canStale(k Key, old entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.entries[k]
	now := c.cfg.Now()
	return ok && current.generation == old.generation && old.value.Status >= 200 && old.value.Status < 300 && now.Before(old.stale) && signedValid(old.value, now)
}
