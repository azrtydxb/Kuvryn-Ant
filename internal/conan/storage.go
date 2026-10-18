package conan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

const maxStageFiles = 8
const maxStageBytes = maxExpanded + (1 << 21)
const stageTTL = time.Hour

type stageFile struct {
	path  string
	info  blob.Info
	media string
}

// stage accumulates the individually uploaded files of one Conan revision so
// the complete set can be validated against conanmanifest.txt before any bytes
// become visible. Files are spooled to disk; only validated metadata is held.
type stage struct {
	feed    string
	v       reference
	source  string
	files   map[string]*stageFile
	bytes   int64
	expires time.Time
}

func (s *Repository) acquire() bool {
	select {
	case s.transfers <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Repository) release() { <-s.transfers }

// stageFor returns the pending upload for one revision target, creating it
// with a pinned source. A conflicting source, a full stage, or an expired
// housekeeping run surfaces as an error; callers must not publish the stage.
func (s *Repository) stageFor(p identity.Principal, feed string, v reference, source, key string) (*stage, error) {
	id := p.PrincipalID + "\x00" + v.coordinate + "\x00" + v.version()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, stp := range s.stages {
		if now.After(stp.expires) {
			for _, f := range stp.files {
				_ = os.Remove(f.path)
			}
			delete(s.stages, k)
		}
	}
	st, ok := s.stages[id]
	if ok && st.source != source {
		return nil, artifact.ErrConflict
	}
	if !ok {
		if len(s.stages) >= 64 {
			return nil, artifact.ErrUnavailable
		}
		st = &stage{feed: feed, v: v, source: source, files: map[string]*stageFile{}, expires: now.Add(stageTTL)}
		s.stages[id] = st
	}
	if old := st.files[key]; old != nil {
		_ = os.Remove(old.path)
		st.bytes -= old.info.Size
		delete(st.files, key)
	}
	st.expires = now.Add(stageTTL)
	return st, nil
}

func (s *Repository) dropStage(p identity.Principal, v reference) {
	id := p.PrincipalID + "\x00" + v.coordinate + "\x00" + v.version()
	s.mu.Lock()
	st, ok := s.stages[id]
	if ok {
		for _, f := range st.files {
			_ = os.Remove(f.path)
		}
		delete(s.stages, id)
	}
	s.mu.Unlock()
}

func fileLimit(v reference, key string) int64 {
	if v.pid != "" {
		if key == "conan_package.tgz" {
			return maxExpanded
		}
		return maxMetadata
	}
	if strings.HasSuffix(key, ".tgz") {
		return maxExpanded
	}
	return maxMetadata
}

func mediaType(key string) string {
	if strings.HasSuffix(key, ".tgz") {
		return "application/gzip"
	}
	return "text/plain"
}

// upload spools one file of a revision. The transfer slot bounds concurrent
// bodies; Prepare pins the routing source. Conan delivers conanmanifest.txt
// last, so its arrival triggers whole-set validation and one atomic bundle.
func (s *Repository) upload(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, v reference, key string) {
	if !s.acquire() {
		failure(w, artifact.ErrUnavailable)
		return
	}
	defer s.release()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	source, e := s.service.Prepare(ctx, p, feed, v.namespace, v.coordinate, key)
	if e != nil {
		failure(w, e)
		return
	}
	st, e := s.stageFor(p, feed, v, source, key)
	if e != nil {
		failure(w, e)
		return
	}
	if st.bytes >= maxStageBytes || len(st.files) >= maxStageFiles {
		failure(w, artifact.ErrUnavailable)
		return
	}
	tmp, e := os.CreateTemp("", "ant-conan-*")
	if e != nil {
		failure(w, e)
		return
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx, r.Body}, N: fileLimit(v, key) + 1}
	n, e := io.CopyBuffer(io.MultiWriter(tmp, hash), limited, make([]byte, 64<<10))
	if e != nil || n > fileLimit(v, key) {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		failure(w, errInvalid)
		return
	}
	if hm := r.Header.Get("X-Checksum-Sha256"); hm != "" {
		if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, hm) {
			tmp.Close()
			_ = os.Remove(tmp.Name())
			failure(w, blob.ErrIntegrity)
			return
		}
	}
	if e = tmp.Close(); e != nil {
		_ = os.Remove(tmp.Name())
		failure(w, e)
		return
	}
	s.mu.Lock()
	if st.bytes+n > maxStageBytes || len(st.files) >= maxStageFiles {
		s.mu.Unlock()
		_ = os.Remove(tmp.Name())
		failure(w, artifact.ErrUnavailable)
		return
	}
	st.files[key] = &stageFile{path: tmp.Name(), info: blob.Info{Digest: blob.Digest(hex.EncodeToString(hash.Sum(nil))), Size: n}, media: mediaType(key)}
	st.bytes += n
	s.mu.Unlock()
	if key != "conanmanifest.txt" {
		w.WriteHeader(200)
		return
	}
	if e = s.commit(ctx, p, st); e != nil {
		s.dropStage(p, v)
		failure(w, e)
		return
	}
	s.dropStage(p, v)
	w.WriteHeader(200)
}

func (s *Repository) commit(ctx context.Context, p identity.Principal, st *stage) error {
	s.mu.Lock()
	files := make(map[string][]byte, len(st.files))
	for key, sf := range st.files {
		b, e := os.ReadFile(sf.path)
		if e != nil {
			s.mu.Unlock()
			return e
		}
		files[key] = b
	}
	s.mu.Unlock()
	// Merge already-published assets so a re-upload of a partially delivered
	// revision (client retry) validates against the full set, not just the new
	// files. Open enforces read authorization per asset.
	existing, e := s.assetKeys(ctx, p, st.feed, st.v)
	if e != nil {
		return e
	}
	for _, key := range existing {
		if _, ok := files[key]; ok {
			continue
		}
		pub := st.v.pub(st.feed, key)
		rec, body, oerr := s.service.Open(ctx, p, pub, 0, -1)
		if oerr != nil {
			return oerr
		}
		b, rerr := io.ReadAll(io.LimitReader(contextReader{ctx, body}, maxExpanded+1))
		body.Close()
		if rerr != nil || info(b) != rec.Info {
			return blob.ErrIntegrity
		}
		files[key] = b
	}
	if e = validate(ctx, st.v, files); e != nil {
		return e
	}
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pubs := make([]artifact.Publication, 0, len(keys))
	readers := make([]io.Reader, 0, len(keys))
	for _, key := range keys {
		pub := st.v.pub(st.feed, key)
		pub.ExpectedSource = st.source
		pub.Info = info(files[key])
		pub.MediaType = mediaType(key)
		pubs = append(pubs, pub)
		readers = append(readers, strings.NewReader(string(files[key])))
	}
	_, e = s.service.PublishBundle(ctx, p, pubs, readers)
	return e
}

// assetKeys lists the asset keys of one published version through package-owner
// composition; the caller authorizes reads per asset via Open.
func (s *Repository) assetKeys(ctx context.Context, p identity.Principal, feed string, v reference) ([]string, error) {
	rows, e := s.db.Query(ctx, `WITH owner AS(SELECT c.id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT a.asset_key FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE v.version=$4`, p.InstanceID, feed, v.coordinate, v.version())
	if e != nil {
		return nil, e
	}
	keys := []string{}
	for rows.Next() {
		var k string
		if e = rows.Scan(&k); e != nil {
			break
		}
		keys = append(keys, k)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, e
	}
	return keys, nil
}

func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

// metadata serves the v2 reference/package listing endpoints. route is one of
// latest, revisions, packages, files or search-files. Ownership is resolved
// through the feed's package owner; read permission is checked on that source.
func (s *Repository) metadata(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, v reference, route string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	component, _, err := s.owner(ctx, p, feed, v.coordinate, v.namespace)
	if err != nil {
		failure(w, err)
		return
	}
	switch route {
	case "latest":
		s.latest(w, r, ctx, component, v)
	case "revisions":
		s.revisions(w, r, ctx, component, v)
	case "packages":
		s.packages(w, r, ctx, component, v)
	case "files":
		s.files(w, r, p, feed, v)
	case "search":
		s.packageSearch(w, r, ctx, component, v)
	default:
		failure(w, artifact.ErrNotFound)
	}
}

// owner resolves the component id and routing source for a coordinate and
// verifies the principal may read the namespace on that source (the same
// index-level feed read barrier the other adapters enforce).
func (s *Repository) owner(ctx context.Context, p identity.Principal, feed, coordinate, namespace string) (component, source string, err error) {
	err = s.db.QueryRow(ctx, `WITH owner AS(SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.id::text,o.source_id::text FROM owner o WHERE (SELECT namespace FROM components WHERE id=o.id)=$4`, p.InstanceID, feed, coordinate, namespace).Scan(&component, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", artifact.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		return "", "", e
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: namespace, Kind: identity.ArtifactResource}, identity.Read) {
		return "", "", artifact.ErrNotFound
	}
	return component, source, nil
}

func (s *Repository) latest(w http.ResponseWriter, r *http.Request, ctx context.Context, component string, v reference) {
	if v.pid == "" {
		var rev string
		var at time.Time
		e := s.db.QueryRow(ctx, `SELECT version,created_at FROM versions WHERE component_id=$1 AND version ~ '^[0-9a-f]{32}$' ORDER BY created_at DESC LIMIT 1`, component).Scan(&rev, &at)
		if errors.Is(e, pgx.ErrNoRows) || e != nil {
			failure(w, artifact.ErrNotFound)
			return
		}
		writeJSON(w, r, map[string]any{"revision": rev, "time": at.UTC().Format(time.RFC3339)})
		return
	}
	prefix := v.rrev + ":" + v.pid + ":"
	var rev string
	var at time.Time
	e := s.db.QueryRow(ctx, `SELECT split_part(version,':',3),created_at FROM versions WHERE component_id=$1 AND version LIKE $2 ORDER BY created_at DESC LIMIT 1`, component, prefix+"%").Scan(&rev, &at)
	if errors.Is(e, pgx.ErrNoRows) || e != nil {
		failure(w, artifact.ErrNotFound)
		return
	}
	writeJSON(w, r, map[string]any{"revision": rev, "time": at.UTC().Format(time.RFC3339)})
}

func (s *Repository) revisions(w http.ResponseWriter, r *http.Request, ctx context.Context, component string, v reference) {
	type rev struct {
		Revision string `json:"revision"`
		Time     string `json:"time"`
	}
	var rows interface {
		Next() bool
		Scan(dest ...any) error
		Err() error
		Close()
	}
	var selectSQL string
	var args []any
	if v.pid == "" {
		selectSQL = `SELECT version,created_at FROM versions WHERE component_id=$1 AND version ~ '^[0-9a-f]{32}$' ORDER BY created_at DESC LIMIT 4097`
	} else {
		prefix := v.rrev + ":" + v.pid + ":"
		selectSQL = `SELECT split_part(version,':',3),created_at FROM versions WHERE component_id=$1 AND version LIKE $2 ORDER BY created_at DESC LIMIT 4097`
		args = []any{prefix + "%"}
	}
	revs := []rev{}
	r2, e := s.db.Query(ctx, selectSQL, append([]any{component}, args...)...)
	if e != nil {
		failure(w, e)
		return
	}
	rows = r2
	for rows.Next() {
		var rv string
		var at time.Time
		if e := rows.Scan(&rv, &at); e != nil {
			break
		}
		revs = append(revs, rev{Revision: rv, Time: at.UTC().Format(time.RFC3339)})
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		failure(w, e)
		return
	}
	rows.Close()
	if len(revs) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	writeJSON(w, r, map[string]any{"revisions": revs})
}

func (s *Repository) packages(w http.ResponseWriter, r *http.Request, ctx context.Context, component string, v reference) {
	prefix := v.rrev + ":"
	rows, e := s.db.Query(ctx, `SELECT DISTINCT split_part(version,':',2) FROM versions WHERE component_id=$1 AND version LIKE $2 AND split_part(version,':',2) ~ '^[0-9a-f]{40}$' ORDER BY 1 LIMIT 4097`, component, prefix+"%")
	if e != nil {
		failure(w, e)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	if len(ids) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	writeJSON(w, r, map[string]any{"package_ids": ids})
}

func (s *Repository) files(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string, v reference) {
	keys, err := s.assetKeys(r.Context(), p, feed, v)
	if err != nil {
		failure(w, err)
		return
	}
	if len(keys) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	files := map[string]any{}
	for _, key := range keys {
		files[key] = map[string]any{}
	}
	writeJSON(w, r, map[string]any{"files": files})
}

func (s *Repository) packageSearch(w http.ResponseWriter, r *http.Request, ctx context.Context, component string, v reference) {
	if v.rrev == "" {
		if e := s.db.QueryRow(ctx, `SELECT version FROM versions WHERE component_id=$1 AND version ~ '^[0-9a-f]{32}$' ORDER BY created_at DESC LIMIT 1`, component).Scan(&v.rrev); e != nil {
			failure(w, artifact.ErrNotFound)
			return
		}
	}
	prefix := v.rrev + ":"
	rows, e := s.db.Query(ctx, `SELECT DISTINCT split_part(version,':',2) FROM versions WHERE component_id=$1 AND version LIKE $2 AND split_part(version,':',2) ~ '^[0-9a-f]{40}$' ORDER BY 1 LIMIT 4097`, component, prefix+"%")
	if e != nil {
		failure(w, e)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		ids = append(ids, id)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	if len(ids) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	result := map[string]any{}
	for _, id := range ids {
		result[id] = map[string]any{}
	}
	writeJSON(w, r, result)
}

// search serves GET v2/conans/search?q=... with the compact recipe references
// the client parses back into RecipeReference strings.
func (s *Repository) search(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	q := r.URL.Query().Get("q")
	if q == "" {
		q = "*"
	}
	pattern := "%" + strings.ReplaceAll(q, "*", "%") + "%"
	rows, e := s.db.Query(ctx, `SELECT DISTINCT c.coordinate FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate LIKE $3 ESCAPE '\' ORDER BY c.coordinate LIMIT 4097`, p.InstanceID, feed, pattern)
	if e != nil {
		failure(w, e)
		return
	}
	results := []string{}
	for rows.Next() {
		var coordinate string
		if e = rows.Scan(&coordinate); e != nil {
			break
		}
		results = append(results, recipeString(coordinate))
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failure(w, e)
		return
	}
	if len(results) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	writeJSON(w, r, map[string]any{"results": results})
}

// recipeString renders a stored coordinate (name/version/user/channel with _
// placeholders) the way RecipeReference.__str__ does.
func recipeString(coordinate string) string {
	parts := strings.Split(coordinate, "/")
	if len(parts) != 4 {
		return coordinate
	}
	out := parts[0] + "/" + parts[1]
	if parts[2] != "_" {
		out += "@" + parts[2]
		if parts[3] != "_" {
			out += "/" + parts[3]
		}
	}
	return out
}
