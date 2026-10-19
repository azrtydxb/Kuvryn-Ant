// Package rpmrepo implements local Yum/RPM-MD repositories; see README.md.
package rpmrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// namespace is the core namespace of every RPM component of a feed.
	namespace = "rpm"
	// snapshotCoordinate cannot collide with a package name (names have no '/').
	snapshotCoordinate = "repodata/snapshot"
	packageAsset       = "package.rpm"
	metadataAsset      = "metadata.json"
)

var dataFile = regexp.MustCompile(`^[0-9a-f]{64}-(primary|filelists|other)\.xml\.gz$`)

// Repository serves RPM-MD feeds. Snapshots are owned by one local source: the
// first source in feed priority order that holds any RPM component.
type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
	secrets   atomic.Pointer[secrets.Store]
	keyBits   int
	locks     sync.Map // feed ID -> *sync.Mutex serializing snapshot generation
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid RPM configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: artifact.NewUploadSlots(), keyBits: 3072}, nil
}

// SetSecrets enables detached repomd.xml.asc signing with a per-feed key kept
// encrypted in the shared secret store. Without it repositories are unsigned.
func (s *Repository) SetSecrets(store secrets.Store) error {
	if store.DB == nil || store.Keys == nil {
		return errors.New("invalid RPM secret store")
	}
	s.secrets.Store(&store)
	return nil
}

func (s *Repository) feedLock(feed string) *sync.Mutex {
	m, _ := s.locks.LoadOrStore(feed, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(ioWindow))
	_ = c.SetWriteDeadline(time.Now().Add(ioWindow))
	r = r.Clone(r.Context())
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	// Only machine credentials enter: any browser principal is discarded.
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	if len(r.Header.Values("Authorization")) == 1 {
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(&challengeWriter{w}, r)
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || len(a) > 8 || a[0] != "feeds" || a[2] != "rpm" || p.InstanceID != s.instance || strings.Contains(r.URL.EscapedPath(), "%") || strings.ContainsAny(r.URL.Path, "\\\x00") {
		failure(w, artifact.ErrNotFound)
		return
	}
	for _, part := range a[3:] {
		if part == "" || part == "." || part == ".." {
			failure(w, artifact.ErrNotFound)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='rpm'`, s.instance, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	rest := a[3:]
	switch {
	case len(rest) == 1 && rest[0] == "ant.repo" && (r.Method == "GET" || r.Method == "HEAD"):
		s.repoFile(w, r, p, feed, a[1])
	case len(rest) == 2 && rest[0] == "repodata" && (r.Method == "GET" || r.Method == "HEAD"):
		// Upstream signed metadata first; a local snapshot owner or an upstream
		// outage falls through to the local composition.
		served, remoteErr := s.remoteRepodata(w, r, p, feed, rest[1])
		if !served {
			s.repodata(w, r, p, feed, rest[1], remoteErr)
		}
	case len(rest) == 2 && rest[0] == "Packages":
		pkg, ok := parseFilename(rest[1])
		if !ok {
			failure(w, artifact.ErrNotFound)
			return
		}
		switch r.Method {
		case "GET", "HEAD":
			s.download(w, r, p, feed, pkg, rest)
		case "PUT":
			s.ingest(w, r, p, feed, pkg)
		case "DELETE":
			s.remove(w, r, p, feed, pkg)
		default:
			w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
			w.WriteHeader(405)
		}
	case len(rest) >= 2 && rest[0] != "repodata" && (r.Method == "GET" || r.Method == "HEAD") && strings.HasSuffix(rest[len(rest)-1], ".rpm"):
		// Upstream layouts (Packages/<letter>/file.rpm) resolve against the feed root.
		pkg, ok := parseFilename(rest[len(rest)-1])
		if !ok {
			failure(w, artifact.ErrNotFound)
			return
		}
		served, err := s.remotePackage(w, r, p, feed, rest, pkg)
		if !served {
			failure(w, err)
		}
	default:
		failure(w, artifact.ErrNotFound)
	}
}

// nevra is the identity encoded in an RPM file name.
type nevra struct{ name, version, release, arch, filename string }

func parseFilename(f string) (nevra, bool) {
	stem, ok := strings.CutSuffix(f, ".rpm")
	if !ok || len(f) > 400 {
		return nevra{}, false
	}
	i := strings.LastIndexByte(stem, '.')
	if i < 0 {
		return nevra{}, false
	}
	stem, arch := stem[:i], stem[i+1:]
	i = strings.LastIndexByte(stem, '-')
	if i < 0 {
		return nevra{}, false
	}
	stem, release := stem[:i], stem[i+1:]
	i = strings.LastIndexByte(stem, '-')
	if i < 0 {
		return nevra{}, false
	}
	n := nevra{name: stem[:i], version: stem[i+1:], release: release, arch: arch, filename: f}
	return n, rpmName.MatchString(n.name) && rpmVersion.MatchString(n.version) && rpmVersion.MatchString(n.release) && rpmArch.MatchString(n.arch)
}

func (n nevra) pub(feed, key string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: n.name, Version: n.version + "-" + n.release + "." + n.arch, AssetKey: key}
}

func (s *Repository) requireAdmin(ctx context.Context, p identity.Principal, feed string) error {
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		return artifact.ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Admin) {
		return artifact.ErrDenied
	}
	return nil
}

func (s *Repository) requireRead(ctx context.Context, p identity.Principal, feed string) error {
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		return artifact.ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Namespace: namespace, Kind: identity.ArtifactResource}, identity.Read) {
		return artifact.ErrNotFound
	}
	return nil
}

// owner returns the snapshot-owning source, or ErrNotFound for an empty feed.
func (s *Repository) owner(ctx context.Context, feed string) (string, error) {
	var id string
	e := s.db.QueryRow(ctx, `SELECT fs.source_id::text FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.namespace=$3 ORDER BY fs.priority LIMIT 1`, s.instance, feed, namespace).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", artifact.ErrNotFound
	}
	if e != nil {
		return "", artifact.ErrUnavailable
	}
	return id, nil
}

// generationWith finds the newest snapshot generation containing assetKey.
func (s *Repository) generationWith(ctx context.Context, owner, assetKey string) (string, error) {
	var v string
	e := s.db.QueryRow(ctx, `SELECT v.version FROM versions v JOIN components c ON c.id=v.component_id AND c.instance_id=v.instance_id JOIN assets a ON a.version_id=v.id WHERE c.instance_id=$1 AND c.source_id=$2 AND c.coordinate=$3 AND c.namespace=$4 AND a.asset_key=$5 ORDER BY v.version DESC LIMIT 1`, s.instance, owner, snapshotCoordinate, namespace, assetKey).Scan(&v)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", artifact.ErrNotFound
	}
	if e != nil {
		return "", artifact.ErrUnavailable
	}
	return v, nil
}

// serveAsset streams one asset, rejecting any record owned by another source.
func (s *Repository) serveAsset(w http.ResponseWriter, r *http.Request, p identity.Principal, owner string, pub artifact.Publication, contentType string) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	if record.SourceID != owner {
		failure(w, artifact.ErrConflict)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, n nevra, rest []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	owner, e := s.owner(ctx, feed)
	if e == nil {
		_, e = s.service.Lookup(ctx, p, n.pub(feed, packageAsset))
	}
	cancel()
	if e == nil {
		s.serveAsset(w, r, p, owner, n.pub(feed, packageAsset), "application/x-rpm")
		return
	}
	if errors.Is(e, artifact.ErrNotFound) {
		// Local miss: pull through from a remote source (verified, then cached).
		served, err := s.remotePackage(w, r, p, feed, rest, n)
		if served {
			return
		}
		if err != nil && !errors.Is(err, artifact.ErrNotFound) {
			e = err
		}
	}
	failure(w, e)
}

func (s *Repository) repodata(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, file string, degraded error) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if file == "repomd.xml.key" {
		s.publicKey(w, r, ctx, p, feed)
		return
	}
	key, kind := file, "application/gzip"
	switch {
	case file == "repomd.xml":
		kind = "application/xml"
	case file == "repomd.xml.asc":
		kind = "application/pgp-signature"
	case !dataFile.MatchString(file):
		failure(w, artifact.ErrNotFound)
		return
	}
	owner, e := s.owner(ctx, feed)
	if e == nil {
		// Checksummed files may belong to an older generation a client is still
		// walking; repomd.xml and its signature always come from the newest one.
		var generation string
		if generation, e = s.generationWith(ctx, owner, key); e == nil {
			pub := artifact.Publication{FeedID: feed, Namespace: namespace, Coordinate: snapshotCoordinate, Version: generation, AssetKey: key}
			s.serveAsset(w, r, p, owner, pub, kind)
			return
		}
	}
	if degraded != nil && errors.Is(e, artifact.ErrNotFound) {
		e = degraded // upstream outage with nothing cached locally
	}
	failure(w, e)
}

func (s *Repository) publicKey(w http.ResponseWriter, r *http.Request, ctx context.Context, p identity.Principal, feed string) {
	if e := s.requireRead(ctx, p, feed); e != nil {
		failure(w, e)
		return
	}
	entity, e := s.signer(ctx, p, feed, false)
	if e != nil {
		failure(w, e)
		return
	}
	if entity == nil {
		failure(w, artifact.ErrNotFound)
		return
	}
	b, e := armoredPublicKey(entity)
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/pgp-keys")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method != "HEAD" {
		_, _ = w.Write(b)
	}
}

func (s *Repository) repoFile(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, slug string) {
	base, _ := s.base.Load().(string)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if base == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if e := s.requireRead(ctx, p, feed); e != nil {
		failure(w, e)
		return
	}
	entity, e := s.signer(ctx, p, feed, false)
	if e != nil {
		failure(w, e)
		return
	}
	url := base + "/feeds/" + slug + "/rpm/"
	text := "[ant-" + slug + "]\nname=Kuvryn Ant " + slug + "\nbaseurl=" + url + "\nenabled=1\ngpgcheck=0\n"
	if entity != nil {
		text += "repo_gpgcheck=1\ngpgkey=" + url + "repodata/repomd.xml.key\n"
	} else {
		text += "repo_gpgcheck=0\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	if r.Method != "HEAD" {
		_, _ = io.WriteString(w, text)
	}
}

// record is the stored per-package metadata asset: identity plus the exact
// XML fragments that are concatenated into each generation's indexes.
type record struct {
	Schema                              int
	Name, Epoch, Version, Release, Arch string
	Filename, SHA256                    string
	Size                                int64
	Signed                              bool
	Primary, Filelists, Other           string
}

func (s *Repository) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, n nevra) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	e := s.requireAdmin(ctx, p, feed)
	var source string
	if e == nil {
		source, e = s.service.Prepare(ctx, p, feed, namespace, n.name, packageAsset)
	}
	cancel()
	if e != nil {
		failure(w, e)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	case <-time.After(30 * time.Second):
		failure(w, artifact.ErrUnavailable)
		return
	case <-r.Context().Done():
		failure(w, artifact.ErrUnavailable)
		return
	}
	f, e := os.CreateTemp("", "ant-rpm-*")
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum := sha256.New()
	size, e := io.CopyBuffer(io.MultiWriter(f, sum), io.LimitReader(contextReader{r.Context(), r.Body}, maxPackage+1), make([]byte, 64<<10))
	digest := hex.EncodeToString(sum.Sum(nil))
	if e != nil || size > maxPackage {
		failure(w, errInvalid)
		return
	}
	if want := r.Header.Get("X-Checksum-Sha256"); want != "" && want != digest {
		failure(w, errInvalid)
		return
	}
	pkg, e := parseRPM(f, size)
	if e != nil {
		failure(w, e)
		return
	}
	// The URL is only a name: the signed header is the identity. A mismatch is
	// either a mislabelled or a tampered package.
	if pkg.Name != n.name || pkg.Version != n.version || pkg.Release != n.release || pkg.Arch != n.arch {
		failure(w, errInvalid)
		return
	}
	rec := record{Schema: 1, Name: pkg.Name, Epoch: pkg.Epoch, Version: pkg.Version, Release: pkg.Release, Arch: pkg.Arch, Filename: n.filename, SHA256: digest, Size: size, Signed: pkg.Signed,
		Primary: pkg.primaryXML(digest, "Packages/"+n.filename, size), Filelists: pkg.filelistsXML(digest), Other: pkg.otherXML(digest)}
	meta, e := json.Marshal(rec)
	if e != nil || len(meta) > maxMetadata {
		failure(w, errInvalid)
		return
	}
	metaSum := sha256.Sum256(meta)
	pub := n.pub(feed, packageAsset)
	pub.ExpectedSource, pub.MediaType, pub.Info = source, "application/x-rpm", blob.Info{Size: size, Digest: blob.Digest(digest)}
	mp := n.pub(feed, metadataAsset)
	mp.ExpectedSource, mp.MediaType, mp.Info = source, "application/json", blob.Info{Size: int64(len(meta)), Digest: blob.Digest(hex.EncodeToString(metaSum[:]))}
	pctx, pcancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer pcancel()
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if _, e = s.service.PublishBundle(pctx, p, []artifact.Publication{pub, mp}, []io.Reader{contextReader{pctx, f}, bytes.NewReader(meta)}); e != nil {
		failure(w, e)
		return
	}
	// The package is invisible to clients until a generation lists it. If the
	// refresh fails the request fails, and an identical retry completes it.
	e = s.withFeed(pctx, feed, func() error { return s.generate(pctx, p, feed, source, "") })
	if e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
}

// remove deletes one package through the shared policy-gated core. The policy
// is checked first, then the new generation (without the package) is published,
// then the core removes the version, so no generation lists a removed package.
func (s *Repository) remove(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, n nevra) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	t := artifact.Target{FeedID: feed, Namespace: namespace, Coordinate: n.name, Version: n.version + "-" + n.release + "." + n.arch}
	e := s.service.Check(ctx, p, artifact.ActionDelete, t)
	var source string
	if e == nil {
		source, e = s.service.Prepare(ctx, p, feed, namespace, n.name, packageAsset)
	}
	if e == nil {
		e = s.withFeed(ctx, feed, func() error {
			if err := s.generate(ctx, p, feed, source, n.filename); err != nil {
				return err
			}
			_, err := s.service.Delete(ctx, p, t)
			return err
		})
	}
	if e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(204)
}

func (s *Repository) withFeed(ctx context.Context, feed string, f func() error) error {
	m := s.feedLock(feed)
	locked := make(chan struct{})
	go func() { m.Lock(); close(locked) }()
	select {
	case <-locked:
	case <-ctx.Done():
		// Release the lock as soon as the abandoned goroutine acquires it.
		go func() { <-locked; m.Unlock() }()
		return artifact.ErrUnavailable
	}
	defer m.Unlock()
	return f()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.r.Read(b)
}

var _ = fmt.Sprint
