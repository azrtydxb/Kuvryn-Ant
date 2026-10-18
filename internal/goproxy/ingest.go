package goproxy

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

const maxInfo = 1 << 20

type versionInfo struct {
	Version string
	Time    time.Time
}

type bundle struct {
	info, mod []byte
	zip       *os.File
}

func (b *bundle) close() {
	if b.zip != nil {
		name := b.zip.Name()
		b.zip.Close()
		os.Remove(name)
	}
}

// readBundle accepts exactly one part each named info, mod and zip, in any
// order. Files are never extracted, and compressed/uncompressed sizes are bound
// by the Go module protocol (500 MiB zip, 16 MiB go.mod).
func readBundle(w http.ResponseWriter, r *http.Request) (*bundle, error) {
	r.Body = http.MaxBytesReader(w, r.Body, modzip.MaxZipFile+modzip.MaxGoMod+maxInfo+(1<<20))
	parts, err := r.MultipartReader()
	if err != nil {
		return nil, artifact.ErrInput
	}
	b := new(bundle)
	success := false
	defer func() {
		if !success {
			b.close()
		}
	}()
	seen := map[string]bool{}
	for {
		part, e := parts.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, artifact.ErrInput
		}
		name := part.FormName()
		if seen[name] || (name != "info" && name != "mod" && name != "zip") {
			return nil, artifact.ErrInput
		}
		seen[name] = true
		if name == "zip" {
			b.zip, err = os.CreateTemp("", "ant-go-*.zip")
			if err != nil {
				return nil, err
			}
			n, e := io.CopyBuffer(b.zip, io.LimitReader(part, modzip.MaxZipFile+1), make([]byte, 64<<10))
			if e != nil || n > modzip.MaxZipFile {
				return nil, artifact.ErrInput
			}
		} else {
			limit := int64(maxInfo)
			if name == "mod" {
				limit = modzip.MaxGoMod
			}
			data, e := io.ReadAll(io.LimitReader(part, limit+1))
			if e != nil || int64(len(data)) > limit {
				return nil, artifact.ErrInput
			}
			if name == "mod" {
				b.mod = data
			} else {
				b.info = data
			}
		}
		if err = part.Close(); err != nil {
			return nil, artifact.ErrInput
		}
	}
	if len(seen) != 3 {
		return nil, artifact.ErrInput
	}
	success = true
	return b, nil
}

func validateBundle(ctx context.Context, m module.Version, b *bundle) error {
	var info versionInfo
	if json.Unmarshal(b.info, &info) != nil || info.Version != m.Version {
		return artifact.ErrInput
	}
	parsed, err := modfile.ParseLax("go.mod", b.mod, nil)
	if err != nil || parsed.Module == nil || parsed.Module.Mod.Path != m.Path {
		return artifact.ErrInput
	}
	if _, err = modzip.CheckZip(m, b.zip.Name()); err != nil {
		return artifact.ErrInput
	}
	stat, err := b.zip.Stat()
	if err != nil {
		return err
	}
	archive, err := zip.NewReader(b.zip, stat.Size())
	if err != nil {
		return artifact.ErrInput
	}
	// CheckZip validates names and declared sizes, but not decompression/CRC.
	// Drain every entry under the checked size bound before any publication.
	for _, entry := range archive.File {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		reader, e := entry.Open()
		if e != nil {
			return artifact.ErrInput
		}
		var n int64
		if entry.Name == m.Path+"@"+m.Version+"/go.mod" {
			data, e := io.ReadAll(io.LimitReader(reader, modzip.MaxGoMod+1))
			reader.Close()
			if e != nil || !bytes.Equal(data, b.mod) {
				return artifact.ErrInput
			}
			n = int64(len(data))
		} else {
			n, e = io.Copy(io.Discard, io.LimitReader(reader, int64(entry.UncompressedSize64)+1))
			reader.Close()
			if e != nil {
				return artifact.ErrInput
			}
		}
		if n != int64(entry.UncompressedSize64) {
			return artifact.ErrInput
		}
	}
	return nil
}
func contentInfo(data []byte) blob.Info {
	sum := sha256.Sum256(data)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}
}
func (s *Proxy) ingest(w http.ResponseWriter, r *http.Request, p identity.Principal, target route, feed string) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, artifact.ErrUnavailable)
		return
	}
	publications := make([]artifact.Publication, 3)
	source := ""
	for i, kind := range []string{"info", "mod", "zip"} {
		pub := target.publication(feed, kind)
		selected, err := s.service.Prepare(r.Context(), p, feed, pub.Namespace, pub.Coordinate, pub.AssetKey)
		if err != nil {
			fail(w, err)
			return
		}
		if source != "" && source != selected {
			fail(w, artifact.ErrConflict)
			return
		}
		source = selected
		pub.ExpectedSource = source
		publications[i] = pub
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = controller.SetWriteDeadline(time.Time{})
	r.Body = &progressReader{r.Body, controller}
	b, err := readBundle(w, r)
	if err != nil {
		fail(w, err)
		return
	}
	defer b.close()
	if err = validateBundle(r.Context(), module.Version{Path: target.module, Version: target.version}, b); err != nil {
		fail(w, err)
		return
	}
	publications[0].Info = contentInfo(b.info)
	publications[0].MediaType = "application/json"
	publications[1].Info = contentInfo(b.mod)
	publications[1].MediaType = "text/plain; charset=utf-8"
	hash := sha256.New()
	if _, err = b.zip.Seek(0, io.SeekStart); err != nil {
		fail(w, err)
		return
	}
	size, err := io.Copy(hash, b.zip)
	if err != nil {
		fail(w, err)
		return
	}
	publications[2].Info = blob.Info{Digest: blob.Digest(hex.EncodeToString(hash.Sum(nil))), Size: size}
	publications[2].MediaType = "application/zip"
	if _, err = b.zip.Seek(0, io.SeekStart); err != nil {
		fail(w, err)
		return
	}
	records, err := s.service.PublishBundle(r.Context(), p, publications, []io.Reader{bytes.NewReader(b.info), bytes.NewReader(b.mod), b.zip})
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if len(records) == 3 && records[0].Replayed && records[1].Replayed && records[2].Replayed {
		status = http.StatusOK
	}
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(status)
}
