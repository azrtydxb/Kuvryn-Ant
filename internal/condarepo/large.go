package condarepo

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/klauspost/compress/zstd"
)

// Large remote repodata.
//
// A repodata.json above the remote metadata bound (conda-forge's linux-64 and
// noarch are hundreds of MiB) cannot be held in the metadata cache, and
// decoding it into maps would multiply that. Such a document is processed as a
// stream instead:
//
//   - The client response is produced entry by entry while the upstream is read:
//     memory is one repodata entry plus fixed buffers, whatever the document size.
//     repodata.json.zst is preferred upstream (an order of magnitude less to
//     transfer) and decoded on the fly with a bounded decoder.
//   - While streaming, a compact digest index (filename hash -> sha256) is spooled
//     to disk in sorted runs. A later package download looks its verified digest
//     up there instead of reading the whole document again; only a miss or an
//     expired index costs another stream. The index is derived state and is
//     rebuilt on demand, so losing it (restart, eviction) is harmless.
//
// The document is not cached between client requests: each fetch of a large
// repodata re-reads the upstream (the .zst form keeps that affordable). A
// stream that fails after the response has started aborts the connection so the
// client never sees a truncated document as valid.

const (
	// maxRepodataStream bounds the decoded document (protects against
	// decompression bombs); conda-forge's largest subdir is far below it.
	maxRepodataStream = 4 << 30
	// maxRepodataEntries bounds the entries of one subdir.
	maxRepodataEntries = 3_000_000
	// maxRepodataEntry bounds a single package record.
	maxRepodataEntry = 1 << 20
	// indexLife is how long a spooled digest index answers lookups.
	indexLife = 15 * time.Minute
	// indexRunRecords is the in-memory sort chunk of the index builder.
	indexRunRecords = 1 << 15
	// maxIndexes bounds the spooled indexes held per process.
	maxIndexes = 16
)

var errRepodataLimit = errors.New("repodata exceeds the supported size")

// repodataEntry is one validated package record of an upstream repodata.
type repodataEntry struct {
	filename, sha256 string
	conda            bool
	raw              json.RawMessage
}

// validateEntry applies the per-entry rules shared by the cached and the
// streaming paths: the key must be a well-formed package name matching the
// record's name-version-build, with a sha256 and the right subdir.
func validateEntry(subdir, filename string, conda bool, raw json.RawMessage) (repodataEntry, bool) {
	ext := ".tar.bz2"
	if conda {
		ext = ".conda"
	}
	var e struct {
		Name, Version, Build, Subdir, SHA256 string
	}
	if json.Unmarshal(raw, &e) != nil || !strings.HasSuffix(filename, ext) || !validRemoteFilename(filename) || !sha256RE.MatchString(e.SHA256) || (e.Subdir != "" && e.Subdir != subdir) || e.Name+"-"+e.Version+"-"+e.Build+ext != filename {
		return repodataEntry{}, false
	}
	return repodataEntry{filename: filename, sha256: e.SHA256, conda: conda, raw: raw}, true
}

// skipValue consumes one JSON value without retaining it.
func skipValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("unexpected delimiter")
	}
	depth := 1
	for depth > 0 {
		t, err = d.Token()
		if err != nil {
			return err
		}
		if delim, ok = t.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// scanRepodata streams an upstream repodata document and calls emit for every
// valid package record, one at a time, in document order. Other top-level
// members (info, removed, repodata_version, ...) are skipped. Entries that fail
// validation are dropped, like in the cached path.
func scanRepodata(src io.Reader, subdir string, emit func(repodataEntry) error) error {
	d := json.NewDecoder(io.LimitReader(src, maxRepodataStream))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("repodata is not a JSON object")
	}
	entries := 0
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		conda := key == "packages.conda"
		if key != "packages" && !conda {
			if err = skipValue(d); err != nil {
				return err
			}
			continue
		}
		if t, err = d.Token(); err != nil || t != json.Delim('{') {
			return fmt.Errorf("repodata %s is not an object", key)
		}
		for d.More() {
			t, err = d.Token()
			if err != nil {
				return err
			}
			filename, _ := t.(string)
			var raw json.RawMessage
			if err = d.Decode(&raw); err != nil {
				return err
			}
			if len(raw) > maxRepodataEntry {
				continue
			}
			if entries++; entries > maxRepodataEntries {
				return errRepodataLimit
			}
			if e, ok := validateEntry(subdir, filename, conda, raw); ok {
				if err = emit(e); err != nil {
					return err
				}
			}
		}
		if _, err = d.Token(); err != nil { // closing brace of the section
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	return nil
}

// openRepodataStream opens the upstream repodata of a subdir for streaming,
// preferring the zstd form. The returned reader decodes it; closing releases
// the upstream connection and the decoder.
func (s *Repository) openRepodataStream(ctx context.Context, p identity.Principal, feed, subdir, name string) (io.ReadCloser, string, error) {
	pub := artifact.Publication{FeedID: feed, Namespace: subdir, Coordinate: "conda/" + subdir}
	if strings.HasSuffix(name, ".json") {
		doc, err := s.service.RemoteStream(ctx, p, pub, artifact.Origin{Path: subdir + "/" + name + ".zst", Representation: "application/zstd"})
		if err == nil {
			z, derr := zstd.NewReader(doc.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
			if derr != nil {
				doc.Body.Close()
				return nil, "", artifact.ErrUnavailable
			}
			return &zstdBody{z: z, body: doc.Body}, doc.SourceID, nil
		}
		// Only a missing .zst falls back to the plain document; an outage or
		// policy error is reported as it is.
		if !errors.Is(err, artifact.ErrNotFound) {
			return nil, "", err
		}
	}
	doc, err := s.service.RemoteStream(ctx, p, pub, artifact.Origin{Path: subdir + "/" + name, Representation: "application/json"})
	if err != nil {
		return nil, "", err
	}
	return doc.Body, doc.SourceID, nil
}

type zstdBody struct {
	z    *zstd.Decoder
	body io.ReadCloser
}

func (b *zstdBody) Read(p []byte) (int, error) { return b.z.Read(p) }
func (b *zstdBody) Close() error {
	b.z.Close()
	return b.body.Close()
}

// startWriter commits the response on its first write, so a failure before any
// output can still be reported with a proper status.
type startWriter struct {
	w       http.ResponseWriter
	start   func()
	started bool
}

func (s *startWriter) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		s.start()
	}
	return s.w.Write(p)
}

// repodataWriter renders the sanitized document: info reduced to the subdir
// (upstream info.base_url would send clients away from this feed), only valid
// entries, and the two package sections.
type repodataWriter struct {
	w      *bufio.Writer
	open   string // section currently open, "" when none
	done   map[string]bool
	prefix bool
}

func newRepodataWriter(w io.Writer, subdir string) (*repodataWriter, error) {
	t := &repodataWriter{w: bufio.NewWriterSize(w, 64<<10), done: map[string]bool{}}
	b, _ := json.Marshal(subdir)
	_, err := t.w.WriteString(`{"info":{"subdir":` + string(b) + `}`)
	return t, err
}

func (t *repodataWriter) section(name string) error {
	if t.open == name {
		return nil
	}
	if t.done[name] {
		return errors.New("repodata section repeated")
	}
	if err := t.closeSection(); err != nil {
		return err
	}
	t.open, t.prefix = name, false
	_, err := t.w.WriteString(`,"` + name + `":{`)
	return err
}

func (t *repodataWriter) closeSection() error {
	if t.open == "" {
		return nil
	}
	t.done[t.open], t.open = true, ""
	return t.w.WriteByte('}')
}

func (t *repodataWriter) entry(e repodataEntry) error {
	name := "packages"
	if e.conda {
		name = "packages.conda"
	}
	if err := t.section(name); err != nil {
		return err
	}
	if t.prefix {
		if err := t.w.WriteByte(','); err != nil {
			return err
		}
	}
	t.prefix = true
	key, _ := json.Marshal(e.filename)
	if _, err := t.w.Write(key); err != nil {
		return err
	}
	if err := t.w.WriteByte(':'); err != nil {
		return err
	}
	_, err := t.w.Write(e.raw)
	return err
}

func (t *repodataWriter) finish() error {
	if err := t.closeSection(); err != nil {
		return err
	}
	for _, name := range []string{"packages", "packages.conda"} {
		if !t.done[name] {
			if _, err := t.w.WriteString(`,"` + name + `":{}`); err != nil {
				return err
			}
		}
	}
	if _, err := t.w.WriteString(`,"removed":[],"repodata_version":1}`); err != nil {
		return err
	}
	return t.w.Flush()
}

// serveLargeIndex streams a repodata document that is too large to cache.
func (s *Repository) serveLargeIndex(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, subdir, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	body, source, err := s.openRepodataStream(ctx, p, feed, subdir, name)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	defer body.Close()
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		return true, nil
	}
	gzipped := acceptsGzip(r)
	sw := &startWriter{w: w, start: func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Vary", "Accept-Encoding")
		if gzipped {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(200)
	}}
	var out io.Writer = sw
	var gz *gzip.Writer
	if gzipped {
		gz, _ = gzip.NewWriterLevel(sw, gzip.BestSpeed)
		out = gz
	}
	rw, err := newRepodataWriter(out, subdir)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	builder := s.indexes.begin(feed + "/" + subdir)
	err = scanRepodata(contextReader{ctx, body}, subdir, func(e repodataEntry) error {
		builder.add(e.filename, e.sha256)
		return rw.entry(e)
	})
	if err == nil {
		err = rw.finish()
	}
	if err == nil && gz != nil {
		err = gz.Close()
	}
	if err != nil {
		builder.abort()
		if !sw.started {
			failure(w, artifact.ErrUnavailable)
			return true, nil
		}
		panic(http.ErrAbortHandler) // never end a truncated document cleanly
	}
	builder.commit(source)
	return true, nil
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(v, ",") {
			name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
			if strings.EqualFold(strings.TrimSpace(name), "gzip") && !strings.Contains(strings.ReplaceAll(q, " ", ""), "q=0") {
				return true
			}
		}
	}
	return false
}

// largeDigest finds the verified sha256 and source of a package in a repodata
// that is too large to cache, through the spooled digest index.
func (s *Repository) largeDigest(ctx context.Context, p identity.Principal, feed, subdir, filename string) (sum, source string, err error) {
	key := feed + "/" + subdir
	build := func() (*digestIndex, error) {
		body, src, err := s.openRepodataStream(ctx, p, feed, subdir, "repodata.json")
		if err != nil {
			return nil, err
		}
		defer body.Close()
		b := s.indexes.begin(key)
		if err = scanRepodata(contextReader{ctx, body}, subdir, func(e repodataEntry) error {
			b.add(e.filename, e.sha256)
			return nil
		}); err != nil {
			b.abort()
			return nil, artifact.ErrUnavailable
		}
		return b.commit(src), nil
	}
	idx, err := s.indexes.lookupOrBuild(key, build)
	if err != nil {
		return "", "", err
	}
	if idx.f == nil {
		// Nothing could be spooled (no writable temp directory): find the one
		// digest by scanning the stream, which stops at the match.
		return s.scanDigest(ctx, p, feed, subdir, filename)
	}
	sum, ok := idx.lookup(filename)
	if !ok {
		return "", "", artifact.ErrNotFound
	}
	return sum, idx.source, nil
}

// ---- digest index ----

const recordSize = 48 // 16-byte filename hash + 32-byte sha256

type indexRecord struct {
	key [16]byte
	sum [32]byte
}

func recordKey(filename string) (k [16]byte) {
	h := sha256.Sum256([]byte(filename))
	copy(k[:], h[:16])
	return k
}

type run struct{ offset, count int64 }

// digestIndex is a read-only set of sorted runs in a spool file.
type digestIndex struct {
	path   string
	f      *os.File
	runs   []run
	source string
	built  time.Time
}

func (x *digestIndex) lookup(filename string) (string, bool) {
	want := recordKey(filename)
	var rec [recordSize]byte
	for _, r := range x.runs {
		lo, hi := int64(0), r.count
		for lo < hi {
			mid := lo + (hi-lo)/2
			if _, err := x.f.ReadAt(rec[:], r.offset+mid*recordSize); err != nil {
				return "", false
			}
			switch c := strings.Compare(string(rec[:16]), string(want[:])); {
			case c == 0:
				return hex.EncodeToString(rec[16:]), true
			case c < 0:
				lo = mid + 1
			default:
				hi = mid
			}
		}
	}
	return "", false
}

func (x *digestIndex) close() {
	if x.f != nil {
		_ = x.f.Close()
		_ = os.Remove(x.path)
	}
}

// indexBuilder spools records in sorted runs; memory is one run.
type indexBuilder struct {
	owner  *indexSet
	key    string
	f      *os.File
	buf    []indexRecord
	runs   []run
	offset int64
	failed bool
}

func (b *indexBuilder) add(filename, sum string) {
	if b == nil || b.failed {
		return
	}
	raw, err := hex.DecodeString(sum)
	if err != nil || len(raw) != 32 {
		return
	}
	rec := indexRecord{key: recordKey(filename)}
	copy(rec.sum[:], raw)
	b.buf = append(b.buf, rec)
	if len(b.buf) >= indexRunRecords {
		b.flush()
	}
}

func (b *indexBuilder) flush() {
	if len(b.buf) == 0 || b.failed {
		return
	}
	sort.Slice(b.buf, func(i, j int) bool { return string(b.buf[i].key[:]) < string(b.buf[j].key[:]) })
	out := make([]byte, 0, len(b.buf)*recordSize)
	for _, r := range b.buf {
		out = append(out, r.key[:]...)
		out = append(out, r.sum[:]...)
	}
	if _, err := b.f.WriteAt(out, b.offset); err != nil {
		b.failed = true
		return
	}
	b.runs = append(b.runs, run{b.offset, int64(len(b.buf))})
	b.offset += int64(len(out))
	b.buf = b.buf[:0]
}

func (b *indexBuilder) abort() {
	if b == nil || b.f == nil {
		return
	}
	_ = b.f.Close()
	_ = os.Remove(b.f.Name())
	b.f = nil
}

// commit publishes the finished index for lookups and returns it. A builder
// that failed to spool (disk full) yields an index that answers nothing.
func (b *indexBuilder) commit(source string) *digestIndex {
	if b == nil || b.f == nil {
		return &digestIndex{source: source}
	}
	b.flush()
	if b.failed {
		b.abort()
		return &digestIndex{source: source}
	}
	x := &digestIndex{path: b.f.Name(), f: b.f, runs: b.runs, source: source, built: time.Now()}
	b.owner.put(b.key, x)
	return x
}

// largeLife is how long a subdir whose repodata exceeded the metadata bound is
// streamed without first probing the cached path again. The probe reads up to
// the bound from the upstream, so repeating it on every request would waste
// that much transfer each time.
const largeLife = 10 * time.Minute

// indexSet owns the spooled indexes of one repository.
type indexSet struct {
	mu       sync.Mutex
	dir      string
	m        map[string]*digestIndex
	building map[string]*indexBuild
	large    map[string]time.Time
}

type indexBuild struct {
	done chan struct{}
	idx  *digestIndex
	err  error
}

func (s *indexSet) directory() string {
	if s.dir == "" {
		s.dir = filepath.Join(os.TempDir(), "ant-conda-index")
		_ = os.MkdirAll(s.dir, 0o700)
		// Spool files of a previous process are unreachable; drop old ones.
		if entries, err := os.ReadDir(s.dir); err == nil {
			for _, e := range entries {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
					_ = os.Remove(filepath.Join(s.dir, e.Name()))
				}
			}
		}
	}
	return s.dir
}

// begin starts a builder. It returns nil when spooling is not possible, which
// callers treat as "no index" (the digest path then streams on each lookup).
func (s *indexSet) begin(key string) *indexBuilder {
	s.mu.Lock()
	dir := s.directory()
	s.mu.Unlock()
	f, err := os.CreateTemp(dir, "idx-*")
	if err != nil {
		return nil
	}
	return &indexBuilder{owner: s, key: key, f: f, buf: make([]indexRecord, 0, indexRunRecords)}
}

func (s *indexSet) put(key string, x *digestIndex) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*digestIndex{}
	}
	if old := s.m[key]; old != nil {
		old.close()
	}
	for len(s.m) >= maxIndexes {
		var victim string
		var oldest time.Time
		for k, v := range s.m {
			if victim == "" || v.built.Before(oldest) {
				victim, oldest = k, v.built
			}
		}
		s.m[victim].close()
		delete(s.m, victim)
	}
	s.m[key] = x
}

// lookupOrBuild returns the fresh index for key, building it once when several
// callers miss at the same time.
func (s *indexSet) lookupOrBuild(key string, build func() (*digestIndex, error)) (*digestIndex, error) {
	s.mu.Lock()
	if x := s.m[key]; x != nil && time.Since(x.built) < indexLife {
		s.mu.Unlock()
		return x, nil
	}
	if b := s.building[key]; b != nil {
		s.mu.Unlock()
		<-b.done
		return b.idx, b.err
	}
	b := &indexBuild{done: make(chan struct{})}
	if s.building == nil {
		s.building = map[string]*indexBuild{}
	}
	s.building[key] = b
	s.mu.Unlock()
	b.idx, b.err = build()
	s.mu.Lock()
	delete(s.building, key)
	s.mu.Unlock()
	close(b.done)
	return b.idx, b.err
}

var errFound = errors.New("found")

// scanDigest reads the repodata stream until it finds filename.
func (s *Repository) scanDigest(ctx context.Context, p identity.Principal, feed, subdir, filename string) (sum, source string, err error) {
	body, source, err := s.openRepodataStream(ctx, p, feed, subdir, "repodata.json")
	if err != nil {
		return "", "", err
	}
	defer body.Close()
	err = scanRepodata(contextReader{ctx, body}, subdir, func(e repodataEntry) error {
		if e.filename == filename {
			sum = e.sha256
			return errFound
		}
		return nil
	})
	switch {
	case errors.Is(err, errFound):
		return sum, source, nil
	case err != nil:
		return "", "", artifact.ErrUnavailable
	}
	return "", "", artifact.ErrNotFound
}

// pullThroughLarge is pullThrough for a subdir whose repodata is too large to
// cache: the verified digest comes from the spooled index instead.
func (s *Repository) pullThroughLarge(ctx context.Context, p identity.Principal, pub artifact.Publication) (bool, error) {
	sum, source, err := s.largeDigest(ctx, p, pub.FeedID, pub.Namespace, pub.Version)
	if err != nil {
		return false, err
	}
	if err = s.fetchVerified(ctx, p, pub, source, sum, strings.HasSuffix(pub.Version, ".conda")); err != nil {
		return false, err
	}
	return true, nil
}

// markLarge remembers that key's repodata is above the metadata bound.
func (s *indexSet) markLarge(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.large == nil || len(s.large) > 1024 {
		s.large = map[string]time.Time{}
	}
	s.large[key] = time.Now().Add(largeLife)
}

func (s *indexSet) isLarge(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.large[key]
	if ok && time.Now().After(until) {
		delete(s.large, key)
		return false
	}
	return ok
}
