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
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Standard Maven deploy (maven-deploy-plugin, Gradle maven-publish) uploads each
// file with its own PUT: the artifacts and their checksum files, then the
// artifact level maven-metadata.xml. There is no other completion marker, so
// the PUT of that metadata document is the commit point: files received before
// it are staged privately, validated like a bundle, and published atomically
// as one version by the metadata PUT. Nothing is advertised before then, and
// an interrupted deploy leaves nothing behind once the staging TTL expires.
//
// Staging is local to the process, so every PUT of one deployment must reach the
// same replica (single replica or session affinity).

const (
	stagingTTL     = 15 * time.Minute
	maxStagedSets  = 16
	maxStagedFiles = 4
)

type stagedFile struct {
	path           string
	size           int64
	sha256, sha512 string
	sha1, md5      string
	mediaType      string
}

type stagedSet struct {
	owner     string // principal
	feed      string
	group     string
	name      string
	version   string
	packaging string
	files     map[string]*stagedFile
	timer     *time.Timer
}

type staging struct {
	mu   sync.Mutex
	sets map[string]*stagedSet
}

func setKey(owner, feed string, l location) string {
	return owner + "|" + feed + "|" + l.group + ":" + l.name + "|" + l.version
}

func (s *stagedSet) discard() {
	if s.timer != nil {
		s.timer.Stop()
	}
	for _, f := range s.files {
		os.Remove(f.path)
	}
	s.files = nil
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
	file := &stagedFile{path: f.Name(), size: n, sha256: hex.EncodeToString(h256.Sum(nil)), sha512: hex.EncodeToString(h512.Sum(nil)), sha1: hex.EncodeToString(h1.Sum(nil)), md5: hex.EncodeToString(hmd5.Sum(nil)), mediaType: media}
	key := setKey(p.PrincipalID, feed, l)
	r.stage.mu.Lock()
	defer r.stage.mu.Unlock()
	if r.stage.sets == nil {
		r.stage.sets = map[string]*stagedSet{}
	}
	set := r.stage.sets[key]
	if set == nil {
		if len(r.stage.sets) >= maxStagedSets {
			fail(w, artifact.ErrUnavailable)
			return
		}
		set = &stagedSet{owner: p.PrincipalID, feed: feed, group: l.group, name: l.name, version: l.version, files: map[string]*stagedFile{}}
		r.stage.sets[key] = set
		set.timer = time.AfterFunc(stagingTTL, func() { r.expire(key, set) })
	}
	if _, replace := set.files[l.file]; !replace && len(set.files) >= maxStagedFiles {
		fail(w, artifact.ErrInput)
		return
	}
	if old := set.files[l.file]; old != nil {
		os.Remove(old.path)
	}
	set.files[l.file] = file
	if isPOM {
		set.packaging = packaging
	}
	set.timer.Reset(stagingTTL)
	keep = true
	f.Close()
	w.WriteHeader(http.StatusCreated)
}

func (r *Repository) expire(key string, set *stagedSet) {
	r.stage.mu.Lock()
	defer r.stage.mu.Unlock()
	if r.stage.sets[key] == set {
		delete(r.stage.sets, key)
		set.discard()
	}
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
	r.stage.mu.Lock()
	defer r.stage.mu.Unlock()
	set := r.stage.sets[setKey(p.PrincipalID, feed, l)]
	var f *stagedFile
	if set != nil {
		f = set.files[asset]
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
	r.stage.mu.Lock()
	var sets []*stagedSet
	for key, set := range r.stage.sets {
		if set.owner == p.PrincipalID && set.feed == feed && set.group == l.group && set.name == l.name {
			sets = append(sets, set)
			set.timer.Stop()
			delete(r.stage.sets, key)
		}
	}
	r.stage.mu.Unlock()
	code := http.StatusCreated
	for i, set := range sets {
		replayed, err := r.commitStaged(req.Context(), p, feed, set)
		if err != nil {
			// A rejected deploy is dropped, so a stale set can never block the next one;
			// only a transient failure keeps the sets for a retry within the TTL.
			r.stage.mu.Lock()
			for _, rest := range sets[i:] {
				k := setKey(rest.owner, rest.feed, location{group: rest.group, name: rest.name, version: rest.version})
				if errors.Is(err, artifact.ErrUnavailable) && r.stage.sets[k] == nil && rest.files != nil {
					r.stage.sets[k] = rest
					rest.timer.Reset(stagingTTL)
				} else {
					rest.discard()
				}
			}
			r.stage.mu.Unlock()
			fail(w, err)
			return
		}
		if replayed {
			code = http.StatusNoContent
		}
		set.discard()
	}
	w.WriteHeader(code)
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
	var opened []*os.File
	defer func() {
		for _, f := range opened {
			f.Close()
		}
	}()
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
		fd, err := os.Open(f.path)
		if err != nil {
			return false, artifact.ErrUnavailable
		}
		opened = append(opened, fd)
		pub := base
		pub.AssetKey, pub.ExpectedSource, pub.MediaType = key, source, f.mediaType
		pub.Info = blob.Info{Digest: blob.Digest(f.sha256), Size: f.size}
		pubs = append(pubs, pub)
		bodies = append(bodies, fd)
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
