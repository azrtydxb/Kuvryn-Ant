package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors reported by a Backend; the worker maps them to delivery outcomes.
var (
	// ErrTargetNotFound means the referenced job, feed or source no longer exists.
	ErrTargetNotFound = errors.New("target not found")
	// ErrTargetDenied means the hook creator may no longer perform the action.
	ErrTargetDenied = errors.New("target action denied")
	// ErrTargetBusy is a transient condition worth retrying.
	ErrTargetBusy = errors.New("target busy")
)

// CacheCleaned is the outcome of a cache clean.
type CacheCleaned struct {
	Matched, Tags, Versions, Components, Bytes int64
	Truncated                                  bool
}

// Backend connects actions to the rest of Ant. A nil function makes the actions
// that need it fail with a clear, non-retried error.
type Backend struct {
	// TriggerReplication requests an immediate run of a replication job.
	TriggerReplication func(ctx context.Context, instance, actor, job string) error
	// CleanCache deletes cache-feed content by prefix as the principal; it
	// applies the feed's delete authorization and audit exactly like the API.
	CleanCache func(ctx context.Context, p identity.Principal, feed, prefix string) (CacheCleaned, error)
	// InvalidateMetadata drops cached metadata and negative-cache entries of a
	// remote source for the given names and returns how many were dropped.
	InvalidateMetadata func(source string, names []string) int
	// NotifyPrewarm wakes the prewarm scheduler after a run was queued.
	NotifyPrewarm func()
}

// Config configures a Service.
type Config struct {
	DB         *pgxpool.Pool
	InstanceID string
	// Secrets resolves a signing secret's encrypted record to its value.
	Secrets events.SecretResolver
	Backend Backend
	// PublicURL is the public origin of the instance (ANT_PUBLIC_URL).
	PublicURL string
	// MaxBody caps a request body (default 1 MiB, at most 8 MiB).
	MaxBody int64
	// LogRetentionDays is how long deliveries are kept (default 14).
	LogRetentionDays int
	// AllowHTTP and AllowPrivate relax the relay destination policy (ANT_WEBHOOK_ALLOW_HTTP, tests).
	AllowHTTP, AllowPrivate bool
	Client                  *http.Client
	Logger                  *slog.Logger
}

// Service receives, records and executes incoming hook deliveries.
type Service struct {
	cfg    Config
	tokens identity.Tokens
	roles  identity.Roles
	sender *events.Sender
	poke   chan struct{}

	mu    sync.Mutex
	cache map[string]cachedHook
	ip    *limiter
	hooks *limiter
	rejec *limiter
	count map[metricKey]int64
}

type cachedHook struct {
	hook *Hook
	at   time.Time
}

// New builds a Service. It does not start workers; call Run under the writer lease.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.InstanceID == "" {
		return nil, ErrInput
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if cfg.MaxBody > AbsoluteMaxBody {
		cfg.MaxBody = AbsoluteMaxBody
	}
	if cfg.LogRetentionDays <= 0 {
		cfg.LogRetentionDays = DefaultLogDays
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	sender, err := events.NewSender(cfg.Client, events.Options{AllowHTTP: cfg.AllowHTTP, AllowPrivate: cfg.AllowPrivate})
	if err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, tokens: identity.Tokens{DB: cfg.DB}, roles: identity.Roles{DB: cfg.DB}, sender: sender, poke: make(chan struct{}, 1),
		cache: map[string]cachedHook{}, ip: newLimiter(), hooks: newLimiter(), rejec: newLimiter(), count: map[metricKey]int64{}}, nil
}

// Close releases idle relay connections.
func (s *Service) Close() { s.sender.Close() }

// PublicURL is the public origin used to build hook URLs.
func (s *Service) PublicURL() string { return strings.TrimRight(s.cfg.PublicURL, "/") }

// MaxBody is the request body cap in bytes.
func (s *Service) MaxBody() int64 { return s.cfg.MaxBody }

// LogRetentionDays is how many days deliveries are kept.
func (s *Service) LogRetentionDays() int { return s.cfg.LogRetentionDays }

// Instance is the instance this service serves.
func (s *Service) Instance() string { return s.cfg.InstanceID }

// Notify wakes the worker after a delivery was queued.
func (s *Service) Notify() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Invalidate drops the in-memory hook cache after a configuration change.
func (s *Service) Invalidate() {
	s.mu.Lock()
	clear(s.cache)
	s.mu.Unlock()
}

const hookCacheTTL = 2 * time.Second

func (s *Service) lookup(ctx context.Context, slug string) (*Hook, error) {
	s.mu.Lock()
	if c, ok := s.cache[slug]; ok && time.Since(c.at) < hookCacheTTL {
		s.mu.Unlock()
		if c.hook == nil {
			return nil, ErrNotFound
		}
		return c.hook, nil
	}
	s.mu.Unlock()
	h, err := GetHookBySlug(ctx, s.cfg.DB, s.cfg.InstanceID, slug)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	s.mu.Lock()
	if len(s.cache) > 4096 {
		clear(s.cache)
	}
	if errors.Is(err, ErrNotFound) {
		s.cache[slug] = cachedHook{nil, time.Now()}
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	s.cache[slug] = cachedHook{&h, time.Now()}
	s.mu.Unlock()
	return &h, nil
}

// signingKeys returns the current and, in a rotation grace window, previous
// values of a signing secret. The caller clears them.
func (s *Service) signingKeys(ctx context.Context, secret string) ([][]byte, error) {
	var current string
	var previous *string
	err := s.cfg.DB.QueryRow(ctx, `SELECT secret_id::text,CASE WHEN previous_expires_at>clock_timestamp() THEN previous_secret_id::text END FROM webhook_signing_secrets WHERE instance_id=$1 AND id=$2`, s.cfg.InstanceID, secret).Scan(&current, &previous)
	if err != nil {
		return nil, ErrNotFound
	}
	if s.cfg.Secrets == nil {
		return nil, ErrUnavailable
	}
	var keys [][]byte
	for _, ref := range []*string{&current, previous} {
		if ref == nil {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		key, rerr := s.cfg.Secrets.Resolve(rctx, s.cfg.InstanceID, *ref)
		cancel()
		if rerr == nil && len(key) > 0 {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, ErrUnavailable
	}
	return keys, nil
}

func clearKeys(keys [][]byte) {
	for _, k := range keys {
		clear(k)
	}
}

// ---- metrics ----

type metricKey struct{ hook, result string }

func (s *Service) record(hook, result string) {
	if hook == "" {
		hook = "_unknown"
	}
	s.mu.Lock()
	s.count[metricKey{hook, result}]++
	s.mu.Unlock()
}

// WriteMetrics writes Prometheus text-format samples. The hook label is the
// slug of a configured hook (unknown slugs are folded into "_unknown", so a
// scanner cannot create series); result is one of accepted, unauthorized,
// duplicate, too_large, rate_limited, succeeded, failed.
func (s *Service) WriteMetrics(w io.Writer) {
	fmt.Fprintln(w, "# HELP ant_incoming_hooks_total Incoming hook calls by hook slug and result.\n# TYPE ant_incoming_hooks_total counter")
	s.mu.Lock()
	type kv struct {
		k metricKey
		v int64
	}
	rows := make([]kv, 0, len(s.count))
	for k, v := range s.count {
		rows = append(rows, kv{k, v})
	}
	s.mu.Unlock()
	for _, r := range sortKV(rows, func(a, b kv) bool {
		if a.k.hook != b.k.hook {
			return a.k.hook < b.k.hook
		}
		return a.k.result < b.k.result
	}) {
		fmt.Fprintf(w, "ant_incoming_hooks_total{hook=%q,result=%q} %d\n", r.k.hook, r.k.result, r.v)
	}
}

func sortKV[T any](in []T, less func(a, b T) bool) []T {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && less(in[j], in[j-1]); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	return in
}

// ---- rate limiting ----

// limiter is a set of token buckets keyed by string. It is process-local: with
// one writer instance serving hooks that is the per-hook limit; behind several
// replicas the effective limit is per replica.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

// allow takes one token from the bucket for key, refilled at perMinute with a
// burst of max(1, perMinute/4) at least 5.
func (l *limiter) allow(key string, perMinute int) bool {
	burst := float64(max(5, perMinute/4))
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) > 50000 {
			for k, v := range l.buckets {
				if now.Sub(v.last) > time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) > 50000 {
				return false
			}
		}
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*float64(perMinute)/60)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
