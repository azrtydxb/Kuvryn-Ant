// Package remoteruntime materializes authorized remote sources. Callers must
// authorize artifact reads before selecting a source and authorization domain.
package remoteruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/cache"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/upstream"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInput    = errors.New("invalid remote runtime request")
	ErrClosed   = errors.New("remote runtime closed")
	ErrCapacity = errors.New("remote runtime capacity exceeded")
	// ErrTooLarge reports a metadata document above the in-memory cache bound; it
	// is a capacity error whose caller may stream the document instead.
	ErrTooLarge    = fmt.Errorf("%w: metadata document exceeds the cache bound", ErrCapacity)
	ErrUnavailable = errors.New("remote runtime unavailable")
	ErrReloaded    = errors.New("remote source generation changed")
	ErrPolicy      = errors.New("remote source policy denied request")
)

type Options struct {
	CAFiles                                  map[string]string
	MaxSources, MaxInflight, MaxCacheEntries int
	MaxCacheBytes                            int64
	MaxMetadataBytes                         int64
}

// Request intentionally has no arbitrary headers, absolute URL or query string.
// AuthorizationDomain must identify the already-authorized caller and its scope
// generation. Reuse across authorization changes can expose caller-specific data.
//
// Exactly one of Path or URL names the resource. URL is an absolute https URL
// for a resource the source's own metadata referenced on a different host (for
// example a PyPI file CDN). It is honoured only when its origin is the source
// origin or one of the source's configured redirect origins, and credentials are
// attached solely for the source origin, so a URL can never carry a secret to
// another origin.
type Request struct {
	SourceID, AuthorizationDomain, Path, Representation string
	URL                                                 string
	AllowStale                                          bool
}
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}
type entry struct {
	revision, credentialRevision int64
	caHash                       [32]byte
	cfg                          remoteconfig.Config
	fetcher                      *upstream.Fetcher
	cache                        *cache.Cache
	ctx                          context.Context
	cancel                       context.CancelFunc
	users, loads                 int
	retired                      bool
	drained                      chan struct{}
	used                         uint64
	negMu                        sync.Mutex
	negative                     map[negativeKey]time.Time
}
type negativeKey struct{ domain, identity, representation string }

const maxNegativeEntries = 1024

// negativeHit reports whether an identical resource recently answered 404/410
// for this authorization domain. Entries live no longer than the source's
// configured negative TTL and are dropped with the entry on any reload.
func (e *entry) negativeHit(k negativeKey) bool {
	e.negMu.Lock()
	defer e.negMu.Unlock()
	until, ok := e.negative[k]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(e.negative, k)
		return false
	}
	return true
}
func (e *entry) negativeStore(k negativeKey) {
	e.negMu.Lock()
	defer e.negMu.Unlock()
	if e.negative == nil {
		e.negative = map[negativeKey]time.Time{}
	}
	now := time.Now()
	if len(e.negative) >= maxNegativeEntries {
		for key, until := range e.negative {
			if now.After(until) {
				delete(e.negative, key)
			}
		}
		if len(e.negative) >= maxNegativeEntries {
			return
		}
	}
	e.negative[k] = now.Add(time.Duration(e.cfg.Cache.NegativeTTLSeconds) * time.Second)
}

type Manager struct {
	mu                sync.Mutex
	db                *pgxpool.Pool
	instance          string
	keys              *secrets.Keyring
	options           Options
	entries           map[string]*entry
	tick              uint64
	closed            bool
	done              chan struct{}
	requests, network chan struct{}
	closeOnce         sync.Once
	health            *Health
	cacheObserver     func(string, bool)
}

func (*Manager) String() string   { return "remoteruntime.Manager{redacted}" }
func (*Manager) GoString() string { return "remoteruntime.Manager{redacted}" }
func New(pool *pgxpool.Pool, instanceID string, keys *secrets.Keyring, o Options) (*Manager, error) {
	var id pgtype.UUID
	if pool == nil || keys == nil || id.Scan(instanceID) != nil || !id.Valid {
		return nil, ErrInput
	}
	if o.MaxSources == 0 {
		o.MaxSources = 8
	}
	if o.MaxInflight == 0 {
		o.MaxInflight = 16
	}
	if o.MaxCacheEntries == 0 {
		o.MaxCacheEntries = 2048
	}
	if o.MaxCacheBytes == 0 {
		o.MaxCacheBytes = 64 << 20
	}
	if o.MaxMetadataBytes == 0 {
		o.MaxMetadataBytes = 1 << 20
	}
	if o.MaxSources < 1 || o.MaxSources > 64 || o.MaxInflight < 1 || o.MaxInflight > 64 || o.MaxCacheEntries < o.MaxSources || o.MaxCacheEntries > 65536 || o.MaxCacheBytes < int64(o.MaxSources)*1024 || o.MaxCacheBytes > 1<<30 || o.MaxMetadataBytes < 1 || o.MaxMetadataBytes > 32<<20 || o.MaxMetadataBytes > o.MaxCacheBytes/int64(o.MaxSources) {
		return nil, ErrInput
	}
	caFiles := map[string]string{}
	if len(o.CAFiles) > 64 {
		return nil, ErrInput
	}
	for ref, path := range o.CAFiles {
		if len(ref) == 0 || len(ref) > 64 || len(path) == 0 || len(path) > 4096 || strings.ContainsRune(path, 0) {
			return nil, ErrInput
		}
		caFiles[ref] = path
	}
	o.CAFiles = caFiles
	return &Manager{db: pool, instance: id.String(), keys: keys, options: o, entries: map[string]*entry{}, done: make(chan struct{}), requests: make(chan struct{}, o.MaxInflight), network: make(chan struct{}, o.MaxInflight), health: NewHealth()}, nil
}
func safeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, io.EOF):
		return io.EOF
	case errors.As(err, new(*upstream.RateLimitError)):
		return err // typed so callers can surface 429 and Retry-After
	case errors.Is(err, upstream.ErrPolicy), errors.Is(err, ErrPolicy):
		return ErrPolicy
	case errors.Is(err, ErrTooLarge):
		return ErrTooLarge
	case errors.Is(err, cache.ErrCapacity), errors.Is(err, ErrCapacity):
		return ErrCapacity
	case errors.Is(err, ErrReloaded):
		return ErrReloaded
	case errors.Is(err, ErrClosed):
		return ErrClosed
	default:
		return ErrUnavailable
	}
}
func validate(r Request) error {
	if remoteconfig.CredentialPurpose(r.SourceID) == "" || len(r.AuthorizationDomain) == 0 || len(r.AuthorizationDomain) > 8192 || len(r.Representation) > 256 || strings.ContainsFunc(r.Representation, unicode.IsControl) || !utf8.ValidString(r.Representation) {
		return ErrInput
	}
	if (r.Path == "") == (r.URL == "") {
		return ErrInput
	}
	if r.URL != "" {
		return validateURL(r.URL)
	}
	if len(r.Path) > 4096 || !utf8.ValidString(r.Path) || strings.ContainsFunc(r.Path, unicode.IsControl) || strings.Contains(r.Path, "\\") {
		return ErrInput
	}
	u, err := url.Parse(r.Path)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasPrefix(u.Path, "/") {
		return ErrInput
	}
	// One trailing slash is meaningful for some protocols (PEP 503 project pages).
	for _, part := range strings.Split(strings.TrimSuffix(u.Path, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInput
		}
	}
	return nil
}
func validateURL(raw string) error {
	if len(raw) > 4096 || !utf8.ValidString(raw) || strings.ContainsFunc(raw, unicode.IsControl) || strings.Contains(raw, "\\") {
		return ErrInput
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return ErrInput
	}
	return nil
}

// identity is the stable resource name used for cache and negative-cache keys.
func (r Request) identity() string {
	if r.URL != "" {
		return r.URL
	}
	return r.Path
}
func (m *Manager) admission(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-m.done:
		return nil, ErrClosed
	default:
	}
	select {
	case m.requests <- struct{}{}:
		return func() { <-m.requests }, nil
	case <-m.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (m *Manager) drainLocked(e *entry) {
	if e.retired && e.users == 0 && e.loads == 0 {
		select {
		case <-e.drained:
		default:
			e.fetcher.Close()
			close(e.drained)
		}
	}
}
func (m *Manager) retireLocked(e *entry) {
	if !e.retired {
		e.retired = true
		e.cancel()
	}
	m.drainLocked(e)
}
func (m *Manager) release(e *entry) { m.mu.Lock(); e.users--; m.drainLocked(e); m.mu.Unlock() }
func (m *Manager) loadEnd(e *entry) { m.mu.Lock(); e.loads--; m.drainLocked(e); m.mu.Unlock() }
func (m *Manager) loadStart(e *entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.retired || m.closed {
		return ErrReloaded
	}
	e.loads++
	return nil
}
func (m *Manager) acquire(ctx context.Context, source string) (*entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		record, err := (remoteconfig.Store{DB: m.db, InstanceID: m.instance}).Load(ctx, source)
		var credentialRevision int64
		if err == nil && record.Config.CredentialID != "" {
			err = m.db.QueryRow(ctx, `SELECT revision FROM encrypted_secrets WHERE instance_id=$1 AND id=$2 AND purpose=$3`, m.instance, record.Config.CredentialID, remoteconfig.CredentialPurpose(source)).Scan(&credentialRevision)
		}
		var roots *x509.CertPool
		var caHash [32]byte
		if err == nil && record.Config.CARef != "" {
			path, ok := m.options.CAFiles[record.Config.CARef]
			if !ok {
				err = ErrPolicy
			} else {
				roots, caHash, err = loadCA(path)
			}
		}
		old := m.entries[source]
		if err != nil {
			if old != nil {
				m.retireLocked(old)
			}
			m.mu.Unlock()
			return nil, safeError(err)
		}
		if old != nil && !old.retired && old.revision == record.Revision && old.credentialRevision == credentialRevision && old.caHash == caHash {
			old.users++
			m.tick++
			old.used = m.tick
			m.mu.Unlock()
			return old, nil
		}
		if old != nil {
			m.retireLocked(old)
			select {
			case <-old.drained:
				delete(m.entries, source)
			default:
				done := old.drained
				m.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}
		if len(m.entries) >= m.options.MaxSources {
			var victim string
			var oldest uint64
			for id, e := range m.entries {
				if e.users == 0 && e.loads == 0 && (victim == "" || e.used < oldest) {
					victim = id
					oldest = e.used
				}
			}
			if victim == "" {
				m.mu.Unlock()
				return nil, ErrCapacity
			}
			m.retireLocked(m.entries[victim])
			delete(m.entries, victim)
		}
		headers := http.Header{}
		if record.Config.CredentialID != "" {
			secretRecord, plain, openErr := (secrets.Store{DB: m.db, Keys: m.keys}).Open(ctx, m.instance, record.Config.CredentialID, remoteconfig.CredentialPurpose(source))
			if openErr == nil {
				headers, openErr = decodeCredentials(plain)
			}
			clear(plain)
			if openErr != nil {
				m.mu.Unlock()
				return nil, safeError(openErr)
			}
			// A concurrent credential update between revision lookup and decrypt must
			// not publish a stale generation under a newer revision's identity.
			credentialRevision = secretRecord.Revision
		}
		fetcher, err := upstream.New(upstream.Config{Origin: record.Config.Origin, RedirectOrigins: withPublicOrigins(record.Config.RedirectOrigins), Credentials: headers, Roots: roots, AllowPrivate: record.Config.AllowPrivate})
		for key := range headers {
			delete(headers, key)
		}
		if err != nil {
			m.mu.Unlock()
			return nil, safeError(err)
		}
		quotaBytes := min(record.Config.Cache.MaxBytes, m.options.MaxCacheBytes/int64(m.options.MaxSources))
		quotaEntries := min(record.Config.Cache.MaxEntries, m.options.MaxCacheEntries/m.options.MaxSources)
		c, err := cache.New(cache.Config{MaxBytes: quotaBytes, MaxEntries: quotaEntries, MetadataTTL: time.Duration(record.Config.Cache.MetadataTTLSeconds) * time.Second, NegativeTTL: time.Duration(record.Config.Cache.NegativeTTLSeconds) * time.Second, StaleFor: time.Duration(record.Config.Cache.StaleIfErrorSeconds) * time.Second})
		if err != nil {
			fetcher.Close()
			m.mu.Unlock()
			return nil, ErrUnavailable
		}
		life, stop := context.WithCancel(context.Background())
		m.tick++
		e := &entry{revision: record.Revision, credentialRevision: credentialRevision, caHash: caHash, cfg: record.Config, fetcher: fetcher, cache: c, ctx: life, cancel: stop, users: 1, drained: make(chan struct{}), used: m.tick}
		m.entries[source] = e
		m.mu.Unlock()
		return e, nil
	}
}
func loadCA(path string) (*x509.CertPool, [32]byte, error) {
	var hash [32]byte
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 1<<20 {
		return nil, hash, ErrPolicy
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, hash, ErrPolicy
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, hash, ErrPolicy
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, hash, ErrPolicy
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, hash, ErrPolicy
	}
	return roots, sha256.Sum256(data), nil
}
func decodeCredentials(plain []byte) (http.Header, error) {
	if len(plain) == 0 || len(plain) > 32768 || !utf8.Valid(plain) {
		return nil, ErrPolicy
	}
	d := json.NewDecoder(bytes.NewReader(plain))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrPolicy
	}
	seen := map[string]bool{}
	headers := http.Header{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, ErrPolicy
		}
		name, ok := token.(string)
		if !ok || seen[name] || (name != "authorization" && name != "api_key") {
			return nil, ErrPolicy
		}
		seen[name] = true
		var value *string
		if d.Decode(&value) != nil || value == nil || len(*value) > 8192 || strings.ContainsFunc(*value, unicode.IsControl) {
			return nil, ErrPolicy
		}
		if *value != "" {
			key := "Authorization"
			if name == "api_key" {
				key = "X-Api-Key"
			}
			headers.Set(key, *value)
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrPolicy
	}
	var extra any
	if d.Decode(&extra) != io.EOF || len(headers) == 0 {
		return nil, ErrPolicy
	}
	return headers, nil
}
func operationContext(ctx context.Context, e *entry) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithTimeout(ctx, time.Duration(e.cfg.TimeoutSeconds)*time.Second)
	stop := context.AfterFunc(e.ctx, cancel)
	return bounded, func() { stop(); cancel() }
}
func (m *Manager) networkSlot(ctx context.Context) error {
	select {
	case m.network <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return ErrClosed
	}
}
func requestHeaders(r Request, previous *cache.Value) http.Header {
	h := http.Header{}
	if r.Representation != "" {
		h.Set("Accept", r.Representation)
	}
	if previous != nil {
		if previous.ETag != "" {
			h.Set("If-None-Match", previous.ETag)
		}
		if previous.LastModified != "" {
			h.Set("If-Modified-Since", previous.LastModified)
		}
	}
	return h
}
func target(e *entry, r Request) string {
	if r.URL != "" {
		return r.URL
	}
	return strings.TrimRight(e.cfg.Origin, "/") + "/" + r.Path
}
func (m *Manager) Metadata(ctx context.Context, r Request) (cache.Value, error) {
	if err := validate(r); err != nil {
		return cache.Value{}, err
	}
	release, err := m.admission(ctx)
	if err != nil {
		return cache.Value{}, err
	}
	defer release()
	e, err := m.acquire(ctx, r.SourceID)
	if err != nil {
		return cache.Value{}, err
	}
	defer m.release(e)
	ctx, cancel := operationContext(ctx, e)
	defer cancel()
	representation := r.Representation
	if representation == "" {
		representation = "*/*"
	}
	loaded := false
	value, err := e.cache.Revalidate(ctx, cache.Key{Source: r.SourceID, Identity: r.identity(), Representation: representation, AuthorizationDomain: r.AuthorizationDomain}, cache.GetOptions{AllowStale: r.AllowStale}, func(ctx context.Context, previous *cache.Value) (cache.Value, error) {
		if err := m.loadStart(e); err != nil {
			return cache.Value{}, err
		}
		defer m.loadEnd(e)
		ctx, cancel := operationContext(ctx, e)
		defer cancel()
		if err := m.networkSlot(ctx); err != nil {
			return cache.Value{}, err
		}
		defer func() { <-m.network }()
		loaded = true
		started := time.Now()
		response, err := e.fetcher.Fetch(ctx, http.MethodGet, target(e, r), requestHeaders(r, previous))
		if err != nil {
			m.health.Record(r.SourceID, 0, time.Since(started), err)
			return cache.Value{}, safeError(err)
		}
		m.health.Record(r.SourceID, response.StatusCode, time.Since(started), nil)
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 304 && response.StatusCode != 404 {
			return cache.Value{}, ErrUnavailable
		}
		if response.ContentLength > m.options.MaxMetadataBytes {
			return cache.Value{}, ErrTooLarge
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, m.options.MaxMetadataBytes+1))
		if err != nil {
			return cache.Value{}, safeError(err)
		}
		if int64(len(body)) > m.options.MaxMetadataBytes {
			return cache.Value{}, ErrTooLarge
		}
		return cache.Value{Status: response.StatusCode, Body: body, ETag: response.Header.Get("ETag"), LastModified: response.Header.Get("Last-Modified"), MediaType: response.Header.Get("Content-Type")}, nil
	})
	if err != nil {
		return cache.Value{}, safeError(err)
	}
	if ctx.Err() != nil {
		return cache.Value{}, ctx.Err()
	}
	m.observeCache(r.SourceID, !loaded)
	return value, nil
}

type streamBody struct {
	cancel  context.CancelFunc
	body    io.ReadCloser
	once    sync.Once
	release func()
	ctx     context.Context
}

func (b *streamBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil {
		if b.ctx.Err() != nil {
			return n, b.ctx.Err()
		}
		return n, safeError(err)
	}
	return n, nil
}
func (b *streamBody) Close() error {
	b.once.Do(func() { b.cancel(); _ = b.body.Close(); b.release() })
	return nil
}
func (m *Manager) Fetch(ctx context.Context, r Request) (*Response, error) {
	if err := validate(r); err != nil {
		return nil, err
	}
	admit, err := m.admission(ctx)
	if err != nil {
		return nil, err
	}
	e, err := m.acquire(ctx, r.SourceID)
	if err != nil {
		admit()
		return nil, err
	}
	negative := negativeKey{r.AuthorizationDomain, r.identity(), r.Representation}
	if e.negativeHit(negative) {
		m.release(e)
		admit()
		return &Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	// The configured timeout bounds connecting and receiving response headers.
	// Once streaming starts, progress is bounded by the upstream idle timeouts so
	// that large artifacts are not cut off by a total-duration limit.
	ctx, cancelCtx := context.WithCancel(ctx)
	stopLife := context.AfterFunc(e.ctx, cancelCtx)
	headerTimer := time.AfterFunc(time.Duration(e.cfg.TimeoutSeconds)*time.Second, cancelCtx)
	cancel := func() { stopLife(); headerTimer.Stop(); cancelCtx() }
	if err = m.networkSlot(ctx); err != nil {
		cancel()
		m.release(e)
		admit()
		return nil, safeError(err)
	}
	finish := func() { cancel(); <-m.network; m.release(e); admit() }
	started := time.Now()
	response, err := e.fetcher.Fetch(ctx, http.MethodGet, target(e, r), requestHeaders(r, nil))
	headerTimer.Stop()
	m.observeCache(r.SourceID, false)
	if err != nil {
		m.health.Record(r.SourceID, 0, time.Since(started), err)
		finish()
		return nil, safeError(err)
	}
	m.health.Record(r.SourceID, response.StatusCode, time.Since(started), nil)
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		e.negativeStore(negative)
	}
	body := &streamBody{body: response.Body, ctx: ctx, release: finish, cancel: cancel}
	context.AfterFunc(ctx, func() { _ = body.Close() })
	if ctx.Err() != nil {
		body.Close()
		return nil, ctx.Err()
	}
	headers := http.Header{}
	for _, key := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Content-Range", "Accept-Ranges", "X-Terraform-Get"} {
		if value := response.Header.Get(key); value != "" {
			headers.Set(key, value)
		}
	}
	return &Response{StatusCode: response.StatusCode, Header: headers, Body: body}, nil
}

// Query sends one read-only JSON query to the source (a bounded POST whose
// answer is a document, such as the Git LFS batch API with operation "download")
// and returns the status and body. Nothing is cached, because such answers carry
// expiring links. The request must name a path on the source origin; the body is
// limited to upstream.MaxPostBody and the answer to the metadata limit.
func (m *Manager) Query(ctx context.Context, r Request, contentType string, body []byte) (cache.Value, error) {
	if r.URL != "" || validate(r) != nil || len(body) == 0 || len(body) > upstream.MaxPostBody || len(contentType) > 128 || strings.ContainsAny(contentType, "\r\n") {
		return cache.Value{}, ErrInput
	}
	release, err := m.admission(ctx)
	if err != nil {
		return cache.Value{}, err
	}
	defer release()
	e, err := m.acquire(ctx, r.SourceID)
	if err != nil {
		return cache.Value{}, err
	}
	defer m.release(e)
	ctx, cancel := operationContext(ctx, e)
	defer cancel()
	if err = m.networkSlot(ctx); err != nil {
		return cache.Value{}, safeError(err)
	}
	defer func() { <-m.network }()
	headers := http.Header{}
	if r.Representation != "" {
		headers.Set("Accept", r.Representation)
	}
	headers.Set("Content-Type", contentType)
	started := time.Now()
	response, err := e.fetcher.Post(ctx, target(e, r), headers, body)
	if err != nil {
		m.health.Record(r.SourceID, 0, time.Since(started), err)
		return cache.Value{}, safeError(err)
	}
	m.health.Record(r.SourceID, response.StatusCode, time.Since(started), nil)
	defer response.Body.Close()
	out := cache.Value{Status: response.StatusCode, MediaType: response.Header.Get("Content-Type")}
	if response.StatusCode != 200 {
		return out, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, m.options.MaxMetadataBytes+1))
	if err != nil {
		return cache.Value{}, safeError(err)
	}
	if int64(len(data)) > m.options.MaxMetadataBytes {
		return cache.Value{}, ErrTooLarge
	}
	out.Body = data
	return out, nil
}

// Close cancels streaming bodies and metadata work and drains owned resources.
// It is safe to call concurrently. It never closes the caller's PostgreSQL pool.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		close(m.done)
		entries := make([]*entry, 0, len(m.entries))
		for _, e := range m.entries {
			m.retireLocked(e)
			entries = append(entries, e)
		}
		m.mu.Unlock()
		for _, e := range entries {
			<-e.drained
		}
		m.mu.Lock()
		m.entries = map[string]*entry{}
		m.mu.Unlock()
	})
	return nil
}

// publicOrigins are signed, public, unauthenticated services every source may
// reference by absolute URL (the Go checksum database, which a GOPROXY serves
// on behalf of clients). Credentials are never attached to them.
var publicOrigins = []string{"https://sum.golang.org"}

func withPublicOrigins(configured []string) []string {
	out := make([]string, 0, len(configured)+len(publicOrigins))
	out = append(out, configured...)
	for _, o := range publicOrigins {
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}
