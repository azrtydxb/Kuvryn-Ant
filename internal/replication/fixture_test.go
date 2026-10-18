package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// site is one independent instance: its own database, blob directory,
// writer lease, secrets keyring and bootstrap administrator.
type site struct {
	t         *testing.T
	pool      *pgxpool.Pool
	instance  string
	admin     identity.Principal
	adminTok  string
	stores    *storage.Registry
	mgr       *ownership.Manager
	keys      *secrets.Keyring
	feed      string // primary feed
	source    string // local source of the primary feed
	domain    string
	publisher *artifact.Service
	dir       string // blob storage directory
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	return dsn
}

var siteCounter int

// newSite creates a separate database (not just a schema) in the shared
// PostgreSQL fixture, migrates it and bootstraps an instance.
func newSite(t *testing.T, name, feedSlug string) *site {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	base, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	siteCounter++
	db := fmt.Sprintf("ant_repl_%s_%d_%d", name, time.Now().UnixNano(), siteCounter)
	if _, err = adminPool.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg := base.Copy()
	cfg.ConnConfig.Database = db
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		adminPool.Close()
	})
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, name)
	if err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, pool: pool, instance: owner.InstanceID}
	all := []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}
	tok, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: all}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.adminTok = tok.Secret
	if s.admin, err = (identity.Tokens{DB: pool}).Authenticate(ctx, tok.Secret); err != nil {
		t.Fatal(err)
	}
	s.keys, err = secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{byte(len(name))}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	s.dir = t.TempDir()
	s.stores, err = storage.New(storage.Config{Directory: s.dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stores.Close)
	var ownerID string
	if err = pool.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	s.mgr, err = ownership.Start(context.WithoutCancel(ctx), pool, s.instance, ownerID, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.mgr.Close() })
	s.feed, s.source, s.domain = s.newFeed(feedSlug, "local")
	s.publisher = s.publisherFor(s.feed, s.source)
	return s
}

// newFeed creates a raw-format feed with one source of the given kind attached
// at priority 0 and its dedup domain.
func (s *site) newFeed(slug, kind string) (feed, source, domain string) {
	s.t.Helper()
	ctx := context.Background()
	must := func(err error) {
		s.t.Helper()
		if err != nil {
			s.t.Fatal(err)
		}
	}
	must(s.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,$2,'raw') RETURNING id::text", s.instance, slug).Scan(&feed))
	source, domain = s.newSource(slug+"-"+kind, kind)
	_, err := s.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", s.instance, feed, source)
	must(err)
	return
}
func (s *site) newSource(name, kind string) (source, domain string) {
	s.t.Helper()
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,$3,'raw') RETURNING id::text", s.instance, name, kind).Scan(&source); err != nil {
		s.t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id::text", s.instance, "source:"+source).Scan(&domain); err != nil {
		s.t.Fatal(err)
	}
	return
}
func (s *site) attach(feed, source string, priority int) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,$4)", s.instance, feed, source, priority); err != nil {
		s.t.Fatal(err)
	}
}
func (s *site) publisherFor(feed, source string) *artifact.Service {
	compiled, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: source, Kind: routing.Local}}, DefaultTarget: source})
	if err != nil {
		s.t.Fatal(err)
	}
	return &artifact.Service{DB: s.pool, Lease: s.mgr.Lease, Router: func(context.Context, string) (*routing.Router, error) { return compiled, nil }, Storage: s.stores.Resolve}
}

func digestOf(data []byte) blob.Info {
	sum := sha256.Sum256(data)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}
}

// publish uses the real publication path (and so the real change triggers).
func (s *site) publish(svc *artifact.Service, feed, ns, coord, version, key, tag string, data []byte) {
	s.t.Helper()
	_, err := svc.Publish(context.Background(), s.admin, artifact.Publication{FeedID: feed, Namespace: ns, Coordinate: coord, Version: version, AssetKey: key, MediaType: "application/octet-stream", Tag: tag, Info: digestOf(data)}, bytes.NewReader(data))
	if err != nil {
		s.t.Fatalf("publish %s %s %s: %v", coord, version, key, err)
	}
}

// insertAsset writes bytes and metadata directly into a source, as a
// pull-through cache (or an out-of-band destination writer) would.
func (s *site) insertAsset(source, domain, ns, coord, version, key string, data []byte, provenance string) {
	s.t.Helper()
	ctx := context.Background()
	store, err := s.stores.Resolve(ctx, domain)
	if err != nil {
		s.t.Fatal(err)
	}
	up, err := store.Stage(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err = up.Write(data); err != nil {
		s.t.Fatal(err)
	}
	info := digestOf(data)
	if _, err = up.Finalize(ctx, info); err != nil {
		s.t.Fatal(err)
	}
	sql := `WITH c AS (INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=EXCLUDED.coordinate RETURNING id),
 v AS (INSERT INTO versions(instance_id,component_id,version) SELECT $1,id,$5 FROM c ON CONFLICT(component_id,version) DO UPDATE SET version=EXCLUDED.version RETURNING id),
 b AS (INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$7,$8,$9,$8) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=EXCLUDED.digest RETURNING id)
 INSERT INTO assets(instance_id,version_id,asset_key,blob_id,provenance) SELECT $1,v.id,$6,b.id,$10::jsonb FROM v,b`
	if _, err = s.pool.Exec(ctx, sql, s.instance, source, coord, ns, version, key, domain, string(info.Digest), info.Size, provenance); err != nil {
		s.t.Fatal(err)
	}
}

func (s *site) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatal(err)
	}
}
func (s *site) count(sql string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// grantReplicate creates a service principal that may replicate one feed and
// returns a token scoped to that feed only.
func (s *site) grantReplicate(name, feed string) string {
	s.t.Helper()
	ctx := context.Background()
	var principal, role string
	if err := s.pool.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'service',$2) RETURNING id::text", s.instance, name).Scan(&principal); err != nil {
		s.t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,$2) RETURNING id::text", s.instance, name).Scan(&role); err != nil {
		s.t.Fatal(err)
	}
	s.exec("INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)", s.instance, principal, role)
	s.exec("INSERT INTO role_grants(instance_id,role_id,kind,effect,feed_id,actions) VALUES($1,$2,'feed','allow',$3,ARRAY['replicate'])", s.instance, role, feed)
	tok, err := (identity.Tokens{DB: s.pool}).Issue(ctx, s.instance, principal, principal, []identity.Scope{{FeedID: feed, Actions: []identity.Action{identity.Replicate}}}, time.Now().Add(time.Hour))
	if err != nil {
		s.t.Fatal(err)
	}
	return tok.Secret
}

// peerServer exposes a site's export API over TLS and records blob traffic so
// tests can prove what was (not) transferred and inject faults.
type peerServer struct {
	*httptest.Server
	caPath string
	mu     sync.Mutex
	served map[string]int    // successful blob responses by digest
	failAt int               // fail blob requests once this many succeeded (-1: never)
	tamper map[string][]byte // digest -> bytes served instead
	total  int

	cutAt       int            // abort full (non-Range) blob responses after this many bytes (0: never)
	ignoreRange bool           // serve the whole blob even when a Range is requested
	bytesServed map[string]int // body bytes written per digest, all attempts
	ranges      []string       // Range headers received, in order
}

func (s *site) serve(t *testing.T) *peerServer {
	t.Helper()
	mux := http.NewServeMux()
	RegisterExport(mux, s.pool, Exporter{DB: s.pool, Storage: s.stores.Resolve})
	p := &peerServer{served: map[string]int{}, failAt: -1, tamper: map[string][]byte{}, bytesServed: map[string]int{}}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i := strings.Index(r.URL.Path, "/blobs/"); i >= 0 {
			digest := r.URL.Path[i+len("/blobs/"):]
			p.mu.Lock()
			fail := p.failAt >= 0 && p.total >= p.failAt
			bad, tampered := p.tamper[digest]
			cut, ignore := p.cutAt, p.ignoreRange
			if rg := r.Header.Get("Range"); rg != "" {
				p.ranges = append(p.ranges, rg)
			}
			p.mu.Unlock()
			if fail {
				http.Error(w, "injected failure", http.StatusInternalServerError)
				return
			}
			if tampered {
				w.Header().Set("Content-Length", fmt.Sprint(len(bad)))
				_, _ = w.Write(bad)
				return
			}
			if ignore {
				r.Header.Del("Range")
			}
			rec := &statusWriter{ResponseWriter: w, status: 200}
			if cut > 0 && r.Header.Get("Range") == "" {
				rec.limit = cut
			}
			rec.count = func(n int) { p.mu.Lock(); p.bytesServed[digest] += n; p.mu.Unlock() }
			mux.ServeHTTP(rec, r)
			if rec.status == 200 {
				p.mu.Lock()
				p.served[digest]++
				p.total++
				p.mu.Unlock()
			}
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	p.caPath = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.Certificate().Raw})
	if err := os.WriteFile(p.caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	limit   int // when > 0, abort the connection after this many body bytes
	written int
	count   func(int)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.limit > 0 && w.written+len(b) > w.limit {
		b = b[:w.limit-w.written]
		n, _ := w.ResponseWriter.Write(b)
		w.written += n
		if w.count != nil {
			w.count(n)
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	if w.count != nil {
		w.count(n)
	}
	return n, err
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (p *peerServer) successes() int { p.mu.Lock(); defer p.mu.Unlock(); return p.total }
func (p *peerServer) maxPerDigest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := 0
	for _, n := range p.served {
		m = max(m, n)
	}
	return m
}

// replicator builds the destination service for a site and returns it with a
// peer and job pointing at the source feed.
func (s *site) service(t *testing.T, peer *peerServer, tweak func(*Config)) *Service {
	t.Helper()
	cfg := Config{DB: s.pool, Keys: s.keys, InstanceID: s.instance, Lease: s.mgr.Lease, Storage: s.stores.Resolve,
		CAs: map[string]string{"peer-ca": peer.caPath}, PageSize: 2, BlobConcurrency: 2,
		PollInterval: 50 * time.Millisecond, ContinuousDelay: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond}
	if tweak != nil {
		tweak(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
