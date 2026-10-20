package ansiblerepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func (s *Repository) publish(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, base string) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, maxArchive+(1<<20))
	mr, e := r.MultipartReader()
	if e != nil {
		failure(w, errInvalid)
		return
	}
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	var ns, name, version, source, digest string
	var archiveInfo blob.Info
	for count := 0; ; count++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || count >= 2 {
			failure(w, errInvalid)
			return
		}
		switch part.FormName() {
		case "sha256":
			if digest != "" {
				failure(w, errInvalid)
				return
			}
			b, err := io.ReadAll(io.LimitReader(part, 65))
			digest = string(b)
			if err != nil || !blob.Digest(digest).Valid() {
				failure(w, errInvalid)
				return
			}
		case "file":
			if f != nil {
				failure(w, errInvalid)
				return
			}
			filename := part.FileName()
			bits := strings.SplitN(strings.TrimSuffix(filename, ".tar.gz"), "-", 3)
			if !strings.HasSuffix(filename, ".tar.gz") || len(bits) != 3 || !namePattern.MatchString(bits[0]) || !namePattern.MatchString(bits[1]) || !validVersion(bits[2]) {
				failure(w, errInvalid)
				return
			}
			ns, name, version = bits[0], bits[1], bits[2]
			// The native filename identifies the routing unit before consuming the file.
			for _, key := range []string{"archive", "metadata"} {
				selected, err := s.service.Prepare(ctx, p, feed, ns, ns+"."+name, key)
				if err != nil {
					failure(w, err)
					return
				}
				if source != "" && source != selected {
					failure(w, artifact.ErrConflict)
					return
				}
				source = selected
			}
			f, e = os.CreateTemp("", "ant-ansible-*")
			if e != nil {
				failure(w, e)
				return
			}
			hash := sha256.New()
			var body io.Reader = contextReader{ctx, part}
			// ansible-galaxy sends the archive base64 encoded (Content-Transfer-Encoding: base64).
			if strings.EqualFold(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding")), "base64") {
				body = base64.NewDecoder(base64.StdEncoding, body)
			}
			n, err := io.CopyBuffer(io.MultiWriter(f, hash), io.LimitReader(body, maxArchive+1), make([]byte, 64<<10))
			if err != nil || n > maxArchive {
				failure(w, errInvalid)
				return
			}
			archiveInfo = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(hash.Sum(nil)))}
		default:
			failure(w, errInvalid)
			return
		}
	}
	if f == nil || digest != "" && digest != string(archiveInfo.Digest) {
		failure(w, errInvalid)
		return
	}
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	validationCtx, validationCancel := context.WithTimeout(ctx, 30*time.Second)
	metadata, e := inspect(validationCtx, f)
	validationCancel()
	if e != nil || metadata.Namespace != ns || metadata.Name != name || metadata.Version != version {
		failure(w, errInvalid)
		return
	}
	data, e := json.Marshal(metadata)
	if e != nil {
		failure(w, e)
		return
	}
	s.mu.Lock()
	for k, v := range s.tasks {
		if time.Now().After(v.expires) {
			delete(s.tasks, k)
		}
	}
	if len(s.tasks) >= 1024 {
		s.mu.Unlock()
		failure(w, artifact.ErrUnavailable)
		return
	}
	id := rand.Text()
	s.tasks[id] = importTask{feed: feed, token: p.TokenID, principal: p.PrincipalID, expires: time.Now().Add(time.Hour)}
	s.mu.Unlock()
	success := false
	defer func() {
		if !success {
			s.mu.Lock()
			delete(s.tasks, id)
			s.mu.Unlock()
		}
	}()
	pubs := []artifact.Publication{}
	for i, key := range []string{"archive", "metadata"} {
		pub := artifact.Publication{ExpectedSource: source, FeedID: feed, Namespace: ns, Coordinate: ns + "." + name, Version: version, AssetKey: key, Info: archiveInfo, MediaType: "application/gzip"}
		if i == 1 {
			pub.Info = info(data)
			pub.MediaType = "application/json"
		}
		pubs = append(pubs, pub)
	}
	if _, e = f.Seek(0, 0); e != nil {
		failure(w, e)
		return
	}
	if _, e = s.service.PublishBundle(ctx, p, pubs, []io.Reader{contextReader{ctx, f}, bytes.NewReader(data)}); e != nil {
		failure(w, e)
		return
	}
	s.mu.Lock()
	task := s.tasks[id]
	task.result = map[string]any{"state": "completed", "finished_at": time.Now().UTC().Format(time.RFC3339Nano), "messages": []any{}}
	s.tasks[id] = task
	s.mu.Unlock()
	success = true
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, r, map[string]string{"task": base + "v3/imports/" + id + "/"})
}
