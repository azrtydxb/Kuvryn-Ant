// Package apkrepo implements local Alpine APK v2 repositories; see README.md.
package apkrepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
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

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	keyring   atomic.Pointer[secrets.Keyring]
	keys      sync.Map // feed id -> *rsa.PrivateKey
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid APK configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: artifact.NewUploadSlots()}, nil
}

// SetKeyring supplies the root keyring protecting per-feed signing keys. Without
// it package ingest and download work but index/key endpoints are unavailable.
func (s *Repository) SetKeyring(k *secrets.Keyring) { s.keyring.Store(k) }

func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(r.Context())
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="apk"`)
		w.WriteHeader(401)
		return
	}
	if _, password, ok := r.BasicAuth(); ok {
		r.Header.Set("Authorization", "Bearer "+password)
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}

// Layout: /feeds/{slug}/apk/{branch}/{repository}/{arch}/{file} and
// /feeds/{slug}/apk/keys/{keyname}.
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 5 || a[0] != "feeds" || a[2] != "apk" || p.InstanceID != s.instance || strings.Contains(r.URL.EscapedPath(), "%") || (len(a) != 5 && len(a) != 7) {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='apk'`, s.instance, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "PUT" && r.Method != "DELETE" {
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		w.WriteHeader(405)
		return
	}
	if len(a) == 5 {
		if a[3] != "keys" || r.Method != "GET" && r.Method != "HEAD" {
			failure(w, artifact.ErrNotFound)
			return
		}
		s.publicKey(w, r, p, feed, a[4])
		return
	}
	branch, repo, arch, file := a[3], a[4], a[5], a[6]
	if !segmentPattern.MatchString(branch) || !segmentPattern.MatchString(repo) || !archPattern.MatchString(arch) {
		failure(w, artifact.ErrNotFound)
		return
	}
	channel := branch + "/" + repo + "/" + arch
	if file == "APKINDEX.tar.gz" {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(405)
			return
		}
		s.index(w, r, p, feed, channel)
		return
	}
	stem := strings.TrimSuffix(file, ".apk")
	if stem == file || len(stem) > 256 || !validStem(stem) {
		failure(w, artifact.ErrNotFound)
		return
	}
	pub := artifact.Publication{FeedID: feed, Namespace: channel, Coordinate: channel, Version: stem, AssetKey: "apk", MediaType: "application/vnd.alpine.apk"}
	switch r.Method {
	case "PUT":
		s.ingest(w, r, p, pub, arch)
	case "DELETE":
		s.remove(w, r, p, pub)
	default:
		// apk fetches a noarch package from the arch (or noarch) path of the
		// repository it asked, not necessarily the one it was published under.
		if _, e := s.service.Lookup(r.Context(), p, pub); errors.Is(e, artifact.ErrNotFound) {
			if alt, ok := s.noarchChannel(r.Context(), p, feed, branch, repo, stem); ok {
				pub.Namespace, pub.Coordinate = alt, alt
			}
		}
		s.download(w, r, p, pub)
	}
}

func validStem(s string) bool {
	i := strings.LastIndex(s, "-r")
	if i < 0 {
		return false
	}
	j := strings.LastIndex(s[:i], "-")
	return j > 0 && namePattern.MatchString(s[:j]) && versionPattern.MatchString(s[j+1:])
}

func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}

func (s *Repository) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, arch string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e == nil && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: pub.FeedID, Kind: identity.FeedResource}, identity.Admin) {
		e = artifact.ErrDenied
	}
	if e == nil {
		pub.ExpectedSource, e = s.service.Prepare(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, pub.AssetKey)
	}
	cancel()
	if e != nil {
		failure(w, e)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	f, e := os.CreateTemp("", "ant-apk-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sha := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, sha), io.LimitReader(contextReader{r.Context(), r.Body}, maxPackage+1), make([]byte, 64<<10))
	if e != nil || n > maxPackage || n == 0 {
		failure(w, errInvalid)
		return
	}
	ctx, cancel = context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	entry, e := inspect(f, n, pub.Version, arch)
	if e != nil {
		failure(w, e)
		return
	}
	pub.Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(sha.Sum(nil)))}
	meta := pub
	meta.AssetKey = "APKINDEX-entry"
	meta.MediaType = "text/plain"
	meta.Info = info([]byte(entry))
	source, e := s.service.Prepare(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, meta.AssetKey)
	if e != nil {
		failure(w, e)
		return
	}
	if source != pub.ExpectedSource {
		failure(w, artifact.ErrConflict)
		return
	}
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	// Package and its index entry become visible together or not at all.
	if _, e = s.service.PublishBundle(ctx, p, []artifact.Publication{pub, meta}, []io.Reader{contextReader{ctx, f}, strings.NewReader(entry)}); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
}

// remove deletes one package version through the shared policy-gated core:
// delete authorization, feed/namespace delete policy, audit, outbox event and
// replication tombstone commit together. Blobs are left for garbage collection.
func (s *Repository) remove(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	if _, e := s.service.Delete(r.Context(), p, artifact.Target{FeedID: pub.FeedID, Namespace: pub.Namespace, Coordinate: pub.Coordinate, Version: pub.Version}); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(204)
}

func keyName(feed string) string {
	return "ant-" + strings.ReplaceAll(feed, "-", "")[:8] + ".rsa.pub"
}

// feedKey returns the feed's signing key, creating it once under an advisory
// lock. The PKCS#8 private key is only ever stored through internal/secrets.
func (s *Repository) feedKey(ctx context.Context, p identity.Principal, feed string) (*rsa.PrivateKey, error) {
	if v, ok := s.keys.Load(feed); ok {
		return v.(*rsa.PrivateKey), nil
	}
	ring := s.keyring.Load()
	if ring == nil {
		return nil, artifact.ErrUnavailable
	}
	store := secrets.Store{DB: s.db, Keys: ring}
	purpose := "apk-feed-signing-key:" + feed
	tx, e := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	defer func() {
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = tx.Rollback(c)
	}()
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, purpose); e != nil {
		return nil, artifact.ErrUnavailable
	}
	var id string
	e = tx.QueryRow(ctx, `SELECT id::text FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2 ORDER BY created_at LIMIT 1`, s.instance, purpose).Scan(&id)
	var der []byte
	switch {
	case e == nil:
		_, der, e = store.Open(ctx, s.instance, id, purpose)
		if e != nil {
			return nil, artifact.ErrUnavailable
		}
	case errors.Is(e, pgx.ErrNoRows):
		k, ge := rsa.GenerateKey(rand.Reader, 2048)
		if ge != nil {
			return nil, artifact.ErrUnavailable
		}
		if der, ge = x509.MarshalPKCS8PrivateKey(k); ge != nil {
			return nil, artifact.ErrUnavailable
		}
		if _, ge = store.CreateTx(ctx, tx, s.instance, p.PrincipalID, purpose, der); ge != nil {
			return nil, artifact.ErrUnavailable
		}
		if ge = tx.Commit(ctx); ge != nil {
			return nil, artifact.ErrUnavailable
		}
	default:
		return nil, artifact.ErrUnavailable
	}
	parsed, e := x509.ParsePKCS8PrivateKey(der)
	key, ok := parsed.(*rsa.PrivateKey)
	if e != nil || !ok {
		return nil, artifact.ErrUnavailable
	}
	s.keys.Store(feed, key)
	return key, nil
}

func (s *Repository) publicKey(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		failure(w, e)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Read) {
		failure(w, artifact.ErrDenied)
		return
	}
	if name != keyName(feed) {
		failure(w, artifact.ErrNotFound)
		return
	}
	key, e := s.feedKey(ctx, p, feed)
	if e != nil {
		failure(w, e)
		return
	}
	b, e := publicPEM(key)
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Length", itoa(int64(len(b))))
	if r.Method != "HEAD" {
		_, _ = w.Write(b)
	}
}

func (s *Repository) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, channel string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,o.namespace,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='APKINDEX-entry' ORDER BY v.version LIMIT 4097`, p.InstanceID, feed, channel)
	if e != nil {
		failure(w, e)
		return
	}
	type row struct{ source, namespace, version string }
	var list []row
	for rows.Next() {
		var x row
		if e = rows.Scan(&x.source, &x.namespace, &x.version); e != nil {
			break
		}
		list = append(list, x)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	if len(list) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		failure(w, e)
		return
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Namespace: channel, Kind: identity.ArtifactResource}, identity.Read) {
		failure(w, artifact.ErrDenied)
		return
	}
	entries := make([]string, 0, len(list))
	total := 0
	for _, x := range list {
		if x.namespace != channel {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub := artifact.Publication{FeedID: feed, Namespace: channel, Coordinate: channel, Version: x.version, AssetKey: "APKINDEX-entry"}
		rec, body, e := s.service.Open(ctx, p, pub, 0, -1)
		if e != nil {
			failure(w, e)
			return
		}
		if rec.SourceID != x.source || rec.Info.Size > 64<<10 {
			body.Close()
			failure(w, artifact.ErrUnavailable)
			return
		}
		data, e := io.ReadAll(io.LimitReader(contextReader{ctx, body}, 64<<10+1))
		body.Close()
		total += len(data)
		m := entryFields(string(data))
		if e != nil || len(data) > 64<<10 || total > 16<<20 || m["P"]+"-"+m["V"] != x.version || len(m["C"]) < 3 {
			failure(w, artifact.ErrUnavailable)
			return
		}
		pub.AssetKey = "apk"
		pk, e := s.service.Lookup(ctx, p, pub)
		if e != nil {
			failure(w, e)
			return
		}
		if pk.SourceID != x.source || itoa(pk.Info.Size) != m["S"] {
			failure(w, artifact.ErrConflict)
			return
		}
		entries = append(entries, string(data))
	}
	key, e := s.feedKey(ctx, p, feed)
	if e != nil {
		failure(w, e)
		return
	}
	sort.Strings(entries)
	data, e := buildIndex(entries, "Kuvryn Ant "+channel, key, keyName(feed))
	if e != nil {
		failure(w, artifact.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("ETag", `"`+string(info(data).Digest)+`"`)
	http.ServeContent(w, r, "APKINDEX.tar.gz", time.Time{}, bytes.NewReader(data))
}

// noarchChannel finds the channel of branch/repo that holds stem as a noarch
// package. Authorization is enforced by the reads it performs on the entry.
func (s *Repository) noarchChannel(ctx context.Context, p identity.Principal, feed, branch, repo, stem string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	prefix := branch + "/" + repo + "/"
	rows, e := s.db.Query(ctx, `SELECT DISTINCT c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id JOIN versions v ON v.component_id=c.id JOIN assets a ON a.version_id=v.id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND left(c.coordinate,length($3))=$3 AND c.coordinate=c.namespace AND v.version=$4 AND a.asset_key='APKINDEX-entry' ORDER BY c.namespace LIMIT 16`, p.InstanceID, feed, prefix, stem)
	if e != nil {
		return "", false
	}
	var candidates []string
	for rows.Next() {
		var ns string
		if rows.Scan(&ns) == nil {
			candidates = append(candidates, ns)
		}
	}
	rows.Close()
	for _, ns := range candidates {
		_, body, e := s.service.Open(ctx, p, artifact.Publication{FeedID: feed, Namespace: ns, Coordinate: ns, Version: stem, AssetKey: "APKINDEX-entry"}, 0, -1)
		if e != nil {
			continue
		}
		data, e := io.ReadAll(io.LimitReader(body, 64<<10+1))
		body.Close()
		if e == nil && len(data) <= 64<<10 && entryFields(string(data))["A"] == "noarch" {
			return ns, true
		}
	}
	return "", false
}
