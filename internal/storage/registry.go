// Package storage binds deduplication domains to isolated backend namespaces.
package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/tracing"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

var ErrConfiguration = errors.New("invalid storage configuration")
var ErrClosed = errors.New("storage registry closed")
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Config struct {
	Backend, Directory, Endpoint, Region, Bucket, Prefix, CredentialsFile, CAFile string
	PathStyle, AllowInsecureHTTP                                                  bool
}
type Registry struct {
	mu     sync.Mutex
	closed bool
	config Config
	stores map[string]blob.Store // per-backend stores keyed "backend/domain"
	routed map[string]*routed
	slots  chan struct{}
	client *http.Client
	router Router
	s3ok   bool
	// factories override backend construction (tests and embedding).
	factories map[string]func(domain string) (blob.Store, error)
}

func New(c Config) (*Registry, error) {
	if c.Backend == "" {
		c.Backend = "disk"
	}
	if c.Directory == "" || len(c.Directory) > 4096 || len(c.Endpoint) > 2048 || len(c.Prefix) > 768 || len(c.Region) > 128 || len(c.Bucket) > 63 || len(c.CredentialsFile) > 4096 || len(c.CAFile) > 4096 {
		return nil, ErrConfiguration
	}
	if c.Backend != "disk" && c.Backend != "s3" {
		return nil, ErrConfiguration
	}
	r := &Registry{config: c, stores: map[string]blob.Store{}, routed: map[string]*routed{}, slots: make(chan struct{}, 16)}
	// S3 is also initialized for a disk deployment when fully configured, so a
	// storage migration can target it without changing environment.
	complete := c.CredentialsFile != "" && c.Region != "" && c.Bucket != ""
	if c.Backend == "s3" || complete {
		if !complete {
			return nil, ErrConfiguration
		}
		r.s3ok = true
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 15 * time.Second
		if c.CAFile != "" {
			pem, err := readBoundedFile(c.CAFile, 1<<20)
			if err != nil || len(pem) > 1<<20 {
				return nil, ErrConfiguration
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, ErrConfiguration
			}
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		}
		r.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if _, err := credentials(c.CredentialsFile); err != nil {
			return nil, err
		}
		// Validate endpoint/prefix/provider settings before serving traffic. This
		// creates no remote objects and performs no network requests.
		if _, err := r.s3("00000000-0000-0000-0000-000000000000"); err != nil {
			return nil, ErrConfiguration
		}
	}
	return r, nil
}
func credentials(path string) (aws.Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return aws.Credentials{}, ErrConfiguration
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return aws.Credentials{}, ErrConfiguration
	}
	var c struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SessionToken    string `json:"session_token"`
	}
	decoder := json.NewDecoder(io.LimitReader(f, 16385))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return aws.Credentials{}, ErrConfiguration
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return aws.Credentials{}, ErrConfiguration
	}
	return aws.Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: "mounted-file"}, nil
}

// Resolve returns the store for one deduplication domain. The returned store is
// routed: every operation consults the active backend, so a completed storage
// migration takes effect without re-resolving, and reads fall back to the
// previous backend until its data is explicitly cleaned up.
func (r *Registry) Resolve(ctx context.Context, domain string) (blob.Store, error) {
	if !uuid.MatchString(domain) {
		return nil, ErrConfiguration
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	if s := r.routed[domain]; s != nil {
		return s, nil
	}
	if len(r.routed) >= 1024 {
		return nil, ErrConfiguration
	}
	s := &routed{r: r, domain: domain}
	r.routed[domain] = s
	return s, nil
}

// Backend returns the store of one specific backend ("disk" or "s3"), bypassing
// routing. Migration uses it to address source and target independently.
func (r *Registry) Backend(ctx context.Context, kind, domain string) (blob.Store, error) {
	if !uuid.MatchString(domain) || (kind != "disk" && kind != "s3") {
		return nil, ErrConfiguration
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.backendLocked(kind, domain)
}

func (r *Registry) backendLocked(kind, domain string) (blob.Store, error) {
	if r.closed {
		return nil, ErrClosed
	}
	key := kind + "/" + domain
	if s := r.stores[key]; s != nil {
		return s, nil
	}
	if len(r.stores) >= 2048 {
		return nil, ErrConfiguration
	}
	var store blob.Store
	var err error
	switch {
	case r.factories[kind] != nil:
		store, err = r.factories[kind](domain)
	case kind == "disk":
		store, err = blob.NewDisk(filepath.Join(r.config.Directory, "blobs", domain), 1<<30)
	case r.s3ok:
		store, err = r.s3(domain)
	default:
		err = ErrConfiguration
	}
	if err != nil {
		return nil, ErrConfiguration
	}
	wrapped := &limited{Store: store, slots: r.slots}
	r.stores[key] = wrapped
	return wrapped, nil
}

// Available lists the backends this deployment is configured to use.
func (r *Registry) Available() []string {
	out := []string{"disk"}
	if r.s3ok || r.factories["s3"] != nil {
		out = append(out, "s3")
	}
	return out
}

// DefaultBackend is the backend named by configuration; a persisted switch
// (see Router) takes precedence.
func (r *Registry) DefaultBackend() string { return r.config.Backend }

// SetFactory overrides how one backend's per-domain store is built. It exists
// for tests and embedders and must be called before the first Resolve.
func (r *Registry) SetFactory(kind string, f func(domain string) (blob.Store, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = map[string]func(string) (blob.Store, error){}
	}
	r.factories[kind] = f
}

// Sixteen concurrent uploads cap application S3 part buffers at1GiB across all
// domains. Disk uses smaller buffers but shares the same admission bound.
type limited struct {
	blob.Store
	slots chan struct{}
}

func (s *limited) Stat(ctx context.Context, d blob.Digest) (info blob.Info, err error) {
	ctx, span := tracing.Start(ctx, "blob stat")
	defer func() { tracing.End(span, err) }()
	return s.Store.Stat(ctx, d)
}

// OpenRange traces only opening the object; the caller streams the body.
func (s *limited) OpenRange(ctx context.Context, d blob.Digest, offset, length int64) (rc io.ReadCloser, err error) {
	ctx, span := tracing.Start(ctx, "blob open")
	defer func() { tracing.End(span, err) }()
	return s.Store.OpenRange(ctx, d, offset, length)
}

func (s *limited) Stage(ctx context.Context) (blob.Upload, error) {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	_, span := tracing.Start(ctx, "blob stage")
	u, err := s.Store.Stage(ctx)
	tracing.End(span, err)
	if err != nil {
		<-s.slots
		return nil, err
	}
	return &upload{Upload: u, release: func() { <-s.slots }}, nil
}

// Delete and List expose optional retention capabilities of the wrapped store.
func (s *limited) Delete(ctx context.Context, d blob.Digest) (err error) {
	ctx, span := tracing.Start(ctx, "blob delete")
	defer func() { tracing.End(span, err) }()
	if x, ok := s.Store.(blob.Deleter); ok {
		return x.Delete(ctx, d)
	}
	return &blob.CapabilityError{Feature: "object deletion"}
}
func (s *limited) AbortStaleUploads(ctx context.Context, olderThan time.Duration, dryRun bool) (int, error) {
	if x, ok := s.Store.(blob.StaleUploadAborter); ok {
		return x.AbortStaleUploads(ctx, olderThan, dryRun)
	}
	return 0, nil
}

// StageResumable exposes resumable multipart uploads of the wrapped store.
func (s *limited) StageResumable(ctx context.Context, id string) (blob.ResumableUpload, error) {
	x, ok := s.Store.(blob.ResumableStager)
	if !ok {
		return nil, &blob.CapabilityError{Feature: "resumable uploads"}
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	u, err := x.StageResumable(ctx, id)
	if err != nil {
		<-s.slots
		return nil, err
	}
	return &resumableUpload{upload: upload{Upload: u, release: func() { <-s.slots }}, offset: u.Offset}, nil
}

type resumableUpload struct {
	upload
	offset func() int64
}

func (u *resumableUpload) Offset() int64 { return u.offset() }

func (s *limited) List(ctx context.Context, after blob.Digest, limit int) (_ []blob.Object, err error) {
	ctx, span := tracing.Start(ctx, "blob list")
	defer func() { tracing.End(span, err) }()
	if x, ok := s.Store.(blob.Lister); ok {
		return x.List(ctx, after, limit)
	}
	return nil, &blob.CapabilityError{Feature: "object listing"}
}

type upload struct {
	blob.Upload
	once    sync.Once
	release func()
}

func (u *upload) Finalize(ctx context.Context, i blob.Info) (blob.Info, error) {
	_, span := tracing.Start(ctx, "blob finalize")
	result, err := u.Upload.Finalize(ctx, i)
	tracing.End(span, err)
	// Both configured backends close their upload on every Finalize outcome.
	// Releasing only on success would retain admission after integrity failures.
	u.once.Do(u.release)
	return result, err
}
func (u *upload) Abort() error { err := u.Upload.Abort(); u.once.Do(u.release); return err }

// Close requires callers to drain active transfers first. It is idempotent and
// prevents further resolution. Cached disk roots must not outlive the registry.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, store := range r.stores {
		if wrapped, ok := store.(*limited); ok {
			if disk, ok := wrapped.Store.(*blob.Disk); ok {
				_ = disk.Close()
			}
		}
	}
	if r.client != nil {
		r.client.CloseIdleConnections()
	}
}
func (r *Registry) s3(domain string) (*blob.S3, error) {
	return blob.NewS3(blob.S3Config{Endpoint: r.config.Endpoint, Region: r.config.Region, Bucket: r.config.Bucket, Prefix: strings.Trim(r.config.Prefix, "/") + "/" + domain, PathStyle: r.config.PathStyle, AllowHTTP: r.config.AllowInsecureHTTP, HTTPClient: r.client, Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return credentials(r.config.CredentialsFile) })})
}
func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrConfiguration
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrConfiguration
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, ErrConfiguration
	}
	return data, nil
}
func (*Registry) String() string   { return "storage.Registry{redacted}" }
func (*Registry) GoString() string { return "storage.Registry{redacted}" }
