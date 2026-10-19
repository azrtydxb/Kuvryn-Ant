package maven

import (
	"context"
	"crypto/md5"  // Maven compatibility sidecar, not a trust primitive.
	"crypto/sha1" // Maven compatibility sidecar, not a trust primitive.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Standard Maven deploy (maven-deploy-plugin, Gradle maven-publish) uploads each
// file with its own PUT: the artifacts and their checksum files, then the
// artifact level maven-metadata.xml. There is no other completion marker, so
// the PUT of that metadata document is the commit point: files received before
// it are staged privately, validated like a bundle, and published atomically
// as one version by the metadata PUT. Nothing is advertised before then, and
// an interrupted deploy leaves nothing behind once the staging TTL expires.
//
// Staging is persisted in PostgreSQL (migration 040: maven_deploy_sets/files/
// chunks), so the PUTs of one deployment may reach different replicas and a
// restart between them loses nothing. The metadata PUT claims the staged sets
// with a lease, so two replicas never commit the same deployment twice.

const (
	stagingTTL     = 15 * time.Minute
	claimTimeout   = 2 * time.Minute
	maxStagedSets  = 16
	maxStagedFiles = 4
	chunkSize      = 1 << 20
)

type stagedFile struct {
	name           string
	size           int64
	sha256, sha512 string
	sha1, md5      string
	mediaType      string
}

type stagedSet struct {
	id        string
	owner     string // principal
	feed      string
	group     string
	name      string
	version   string
	packaging string
	files     map[string]*stagedFile
}

// put handles PUT of one repository file.
func (r *Repository) put(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string) {
	switch {
	case l.metadata:
		r.putMetadata(w, req, p, l, feed)
	case checksumSuffix(l.file) != "":
		r.putChecksum(w, req, p, l, feed)
	default:
		r.putAsset(w, req, p, l, feed)
	}
}

func drain(req *http.Request, limit int64) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	return b, err == nil && int64(len(b)) <= limit
}

func (r *Repository) putAsset(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string) {
	base := l.publication(feed)
	if _, err := r.service.Prepare(req.Context(), p, feed, base.Namespace, base.Coordinate, l.key); err != nil {
		fail(w, err)
		return
	}
	select {
	case r.uploads <- struct{}{}:
		defer func() { <-r.uploads }()
	case <-req.Context().Done():
		return
	}
	limit := int64(maxAsset)
	isPOM := strings.HasSuffix(l.file, ".pom")
	if isPOM {
		limit = maxPOM
	}
	if req.ContentLength > limit {
		fail(w, artifact.ErrInput)
		return
	}
	f, err := os.CreateTemp("", "ant-maven-put-*")
	if err != nil {
		fail(w, err)
		return
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h256, h512, h1, hmd5 := sha256.New(), sha512.New(), sha1.New(), md5.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, h256, h512, h1, hmd5), io.LimitReader(contextReader{req.Context(), req.Body}, limit+1), make([]byte, 64<<10))
	if err != nil || n > limit || n == 0 {
		fail(w, artifact.ErrInput)
		return
	}
	packaging := ""
	if isPOM {
		data := make([]byte, n)
		if _, err = f.ReadAt(data, 0); err != nil {
			fail(w, err)
			return
		}
		if packaging, err = validatePOM(data, l); err != nil {
			fail(w, err)
			return
		}
	} else if err = validateJAR(req.Context(), f, n); err != nil {
		fail(w, err)
		return
	}
	media := "application/java-archive"
	if isPOM {
		media = "application/xml"
	}
	file := &stagedFile{name: l.file, size: n, sha256: hex.EncodeToString(h256.Sum(nil)), sha512: hex.EncodeToString(h512.Sum(nil)), sha1: hex.EncodeToString(h1.Sum(nil)), md5: hex.EncodeToString(hmd5.Sum(nil)), mediaType: media}
	if err = r.stageFile(req.Context(), p, feed, l, file, f, packaging); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// purgeExpired drops staged sets nobody touched within the TTL (and whose
// claim, if any, lapsed). Cascades remove files and chunks.
func (r *Repository) purgeExpired(ctx context.Context) {
	_, _ = r.db.Exec(ctx, `DELETE FROM maven_deploy_sets WHERE instance_id=$1 AND updated_at<now()-$2::interval AND (claimed_at IS NULL OR claimed_at<now()-$3::interval)`,
		r.instance, stagingTTL.String(), claimTimeout.String())
}

// stageFile stores one validated upload (read from src, already spooled to a
// local temporary file) as chunks of its deployment's staged set.
func (r *Repository) stageFile(ctx context.Context, p identity.Principal, feed string, l location, file *stagedFile, src *os.File, packaging string) error {
	r.purgeExpired(ctx)
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return artifact.ErrUnavailable
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var owned int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM maven_deploy_sets WHERE instance_id=$1 AND owner=$2 AND NOT (feed_id=$3 AND group_id=$4 AND artifact=$5 AND version=$6)`,
		p.InstanceID, p.PrincipalID, feed, l.group, l.name, l.version).Scan(&owned); err != nil {
		return artifact.ErrUnavailable
	}
	if owned >= maxStagedSets {
		return artifact.ErrUnavailable
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO maven_deploy_sets(instance_id,feed_id,owner,group_id,artifact,version) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(instance_id,feed_id,owner,group_id,artifact,version) DO UPDATE SET updated_at=now() WHERE maven_deploy_sets.claimed_at IS NULL OR maven_deploy_sets.claimed_at<now()-$7::interval
		RETURNING id::text`, p.InstanceID, feed, p.PrincipalID, l.group, l.name, l.version, claimTimeout.String()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.ErrConflict // the deployment is being committed right now
	}
	if err != nil {
		return artifact.ErrUnavailable
	}
	var files int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM maven_deploy_files WHERE set_id=$1 AND file<>$2`, id, file.name).Scan(&files); err != nil {
		return artifact.ErrUnavailable
	}
	if files >= maxStagedFiles {
		return artifact.ErrInput
	}
	if _, err = tx.Exec(ctx, `DELETE FROM maven_deploy_files WHERE set_id=$1 AND file=$2`, id, file.name); err != nil {
		return artifact.ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `INSERT INTO maven_deploy_files(set_id,file,size,sha256,sha512,sha1,md5,media_type) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, file.name, file.size, file.sha256, file.sha512, file.sha1, file.md5, file.mediaType); err != nil {
		return artifact.ErrUnavailable
	}
	if _, err = src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for idx := 0; ; idx++ {
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err = tx.Exec(ctx, `INSERT INTO maven_deploy_chunks(set_id,file,idx,data) VALUES($1,$2,$3,$4)`, id, file.name, idx, buf[:n]); err != nil {
				return artifact.ErrUnavailable
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			return rerr
		}
	}
	if packaging != "" {
		if _, err = tx.Exec(ctx, `UPDATE maven_deploy_sets SET packaging=$2 WHERE id=$1`, id, packaging); err != nil {
			return artifact.ErrUnavailable
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return artifact.ErrUnavailable
	}
	return nil
}

// putChecksum verifies a client supplied checksum file against the staged file.
// The service generates the sidecars it serves, so the upload is only checked.
func (r *Repository) putChecksum(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string) {
	body, ok := drain(req, 1024)
	if !ok {
		fail(w, artifact.ErrInput)
		return
	}
	suffix := checksumSuffix(l.file)
	asset := strings.TrimSuffix(l.file, suffix)
	got := strings.ToLower(strings.TrimSpace(string(body)))
	if i := strings.IndexAny(got, " \t\r\n"); i > 0 {
		got = got[:i] // "<hash>  <filename>" form
	}
	f := &stagedFile{}
	err := r.db.QueryRow(req.Context(), `SELECT f.sha256,f.sha512,f.sha1,f.md5 FROM maven_deploy_sets s JOIN maven_deploy_files f ON f.set_id=s.id
		WHERE s.instance_id=$1 AND s.feed_id=$2 AND s.owner=$3 AND s.group_id=$4 AND s.artifact=$5 AND s.version=$6 AND f.file=$7`,
		p.InstanceID, feed, p.PrincipalID, l.group, l.name, l.version, asset).Scan(&f.sha256, &f.sha512, &f.sha1, &f.md5)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		fail(w, artifact.ErrUnavailable)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		f = nil
	}
	if f == nil {
		// The artifact is already committed (a re-deploy) or arrives later;
		// the artifact itself is verified when it is published.
		w.WriteHeader(http.StatusCreated)
		return
	}
	want := map[string]string{".sha256": f.sha256, ".sha512": f.sha512, ".sha1": f.sha1, ".md5": f.md5}[suffix]
	if got != want {
		fail(w, blob.ErrIntegrity)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// putMetadata commits every staged version of the artifact. The uploaded
// metadata document itself is discarded: the service generates its own.
func (r *Repository) putMetadata(w http.ResponseWriter, req *http.Request, p identity.Principal, l location, feed string) {
	if _, ok := drain(req, 4<<20); !ok {
		fail(w, artifact.ErrInput)
		return
	}
	if l.file != "maven-metadata.xml" {
		w.WriteHeader(http.StatusCreated) // checksum of the metadata document
		return
	}
	r.purgeExpired(req.Context())
	sets, err := r.claimSets(req.Context(), p, feed, l)
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusCreated
	for i, set := range sets {
		replayed, err := r.commitStaged(req.Context(), p, feed, set)
		if err != nil {
			// A rejected deploy is dropped, so a stale set can never block the next one;
			// only a transient failure keeps the sets for a retry within the TTL.
			bg := context.WithoutCancel(req.Context())
			for _, rest := range sets[i:] {
				if errors.Is(err, artifact.ErrUnavailable) {
					_, _ = r.db.Exec(bg, `UPDATE maven_deploy_sets SET claimed_at=NULL,updated_at=now() WHERE id=$1`, rest.id)
				} else {
					_, _ = r.db.Exec(bg, `DELETE FROM maven_deploy_sets WHERE id=$1`, rest.id)
				}
			}
			fail(w, err)
			return
		}
		if replayed {
			code = http.StatusNoContent
		}
		_, _ = r.db.Exec(context.WithoutCancel(req.Context()), `DELETE FROM maven_deploy_sets WHERE id=$1`, set.id)
	}
	w.WriteHeader(code)
}

// claimSets leases every staged version of the artifact for this commit and
// loads the file descriptions. Concurrent commits on other replicas get none.
func (r *Repository) claimSets(ctx context.Context, p identity.Principal, feed string, l location) ([]*stagedSet, error) {
	rows, err := r.db.Query(ctx, `UPDATE maven_deploy_sets SET claimed_at=now() WHERE instance_id=$1 AND feed_id=$2 AND owner=$3 AND group_id=$4 AND artifact=$5
		AND (claimed_at IS NULL OR claimed_at<now()-$6::interval) RETURNING id::text,version,packaging`, p.InstanceID, feed, p.PrincipalID, l.group, l.name, claimTimeout.String())
	if err != nil {
		return nil, artifact.ErrUnavailable
	}
	var sets []*stagedSet
	for rows.Next() {
		set := &stagedSet{owner: p.PrincipalID, feed: feed, group: l.group, name: l.name, files: map[string]*stagedFile{}}
		if err = rows.Scan(&set.id, &set.version, &set.packaging); err != nil {
			rows.Close()
			return nil, artifact.ErrUnavailable
		}
		sets = append(sets, set)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, artifact.ErrUnavailable
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].version < sets[j].version })
	for _, set := range sets {
		frows, err := r.db.Query(ctx, `SELECT file,size,sha256,sha512,sha1,md5,media_type FROM maven_deploy_files WHERE set_id=$1`, set.id)
		if err != nil {
			return nil, artifact.ErrUnavailable
		}
		for frows.Next() {
			f := &stagedFile{}
			if err = frows.Scan(&f.name, &f.size, &f.sha256, &f.sha512, &f.sha1, &f.md5, &f.mediaType); err != nil {
				frows.Close()
				return nil, artifact.ErrUnavailable
			}
			set.files[f.name] = f
		}
		frows.Close()
		if frows.Err() != nil {
			return nil, artifact.ErrUnavailable
		}
	}
	return sets, nil
}

// chunkReader streams one staged file back from its chunks.
type chunkReader struct {
	ctx     context.Context
	db      *pgxpool.Pool
	set     string
	file    string
	idx     int
	pending []byte
}

func (c *chunkReader) Read(b []byte) (int, error) {
	for len(c.pending) == 0 {
		var data []byte
		err := c.db.QueryRow(c.ctx, `SELECT data FROM maven_deploy_chunks WHERE set_id=$1 AND file=$2 AND idx=$3`, c.set, c.file, c.idx).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, artifact.ErrUnavailable
		}
		c.idx++
		c.pending = data
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (r *Repository) commitStaged(ctx context.Context, p identity.Principal, feed string, set *stagedSet) (bool, error) {
	l := location{group: set.group, name: set.name, version: set.version}
	pom := l.name + "-" + l.version + ".pom"
	jar := l.name + "-" + l.version + ".jar"
	_, havePOM := set.files[pom]
	_, haveJAR := set.files[jar]
	if !havePOM || set.packaging != "pom" && !haveJAR || set.packaging == "pom" && len(set.files) != 1 {
		return false, artifact.ErrInput
	}
	base := l.publication(feed)
	var source string
	var pubs []artifact.Publication
	var bodies []io.Reader
	for _, name := range sortedNames(set.files) {
		f := set.files[name]
		key := strings.ReplaceAll(l.group, ".", "/") + "/" + l.name + "/" + l.version + "/" + name
		pinned, err := r.service.Prepare(ctx, p, feed, base.Namespace, base.Coordinate, key)
		if err != nil {
			return false, err
		}
		if source == "" {
			source = pinned
		} else if pinned != source {
			return false, artifact.ErrConflict
		}
		pub := base
		pub.AssetKey, pub.ExpectedSource, pub.MediaType = key, source, f.mediaType
		pub.Info = blob.Info{Digest: blob.Digest(f.sha256), Size: f.size}
		pubs = append(pubs, pub)
		bodies = append(bodies, &chunkReader{ctx: ctx, db: r.db, set: set.id, file: name})
		for _, sum := range []struct{ suffix, value string }{{".sha256", f.sha256}, {".sha1", f.sha1}, {".md5", f.md5}} {
			side := pub
			side.AssetKey += sum.suffix
			side.MediaType = "text/plain"
			side.Info = info([]byte(sum.value))
			pubs = append(pubs, side)
			bodies = append(bodies, strings.NewReader(sum.value))
		}
	}
	records, err := r.service.PublishBundle(ctx, p, pubs, bodies)
	if err != nil {
		return false, err
	}
	return len(records) > 0 && records[0].Replayed, nil
}

func sortedNames(m map[string]*stagedFile) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
