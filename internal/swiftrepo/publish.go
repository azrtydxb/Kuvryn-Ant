package swiftrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

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

func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}

// release is the persisted per-version record; it is the only mutable-looking
// document and is written atomically with the archive and manifests.
type release struct {
	Checksum        string         `json:"checksum"`
	Size            int64          `json:"size"`
	Metadata        map[string]any `json:"metadata"`
	Manifests       []manifestInfo `json:"manifests"`
	SignatureFormat string         `json:"signatureFormat,omitempty"`
	ArchiveSig      string         `json:"archiveSignature,omitempty"`
	MetadataSig     string         `json:"metadataSignature,omitempty"`
}

func readPart(part *multipart.Part, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(part, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, perr(413, "part too large")
	}
	return b, nil
}

func (q *req) publish() {
	w, r := q.w, q.r
	mt, params, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		failure(w, perr(415, "multipart/form-data required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	archivePub := q.pub(q.version, archiveAsset, zipMediaType)
	archivePub.ExpectedSource, e = q.s.service.Prepare(ctx, q.p, q.feed, archivePub.Namespace, archivePub.Coordinate, archivePub.AssetKey)
	cancel()
	if e != nil {
		failure(w, e)
		return
	}
	select {
	case q.s.transfers <- struct{}{}:
		defer func() { <-q.s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxArchive+4*maxMetadata)
	mr := multipart.NewReader(contextReader{r.Context(), body}, params["boundary"])
	f, e := os.CreateTemp("", "ant-swift-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	var (
		size                         int64
		sum                          [32]byte
		metaRaw, archiveSig, metaSig []byte
		haveArchive, haveMeta        bool
	)
	for {
		part, e := mr.NextPart()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			failure(w, perr(400, "malformed multipart body"))
			return
		}
		switch part.FormName() {
		case "source-archive":
			if haveArchive {
				failure(w, perr(400, "duplicate source-archive"))
				return
			}
			h := sha256.New()
			n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(part, maxArchive+1), make([]byte, 64<<10))
			if e != nil || n > maxArchive {
				failure(w, perr(413, "source archive too large or unreadable"))
				return
			}
			size, haveArchive = n, true
			copy(sum[:], h.Sum(nil))
		case "metadata":
			if haveMeta {
				failure(w, perr(400, "duplicate metadata"))
				return
			}
			if metaRaw, e = readPart(part, maxMetadata); e != nil {
				failure(w, e)
				return
			}
			haveMeta = true
		case "source-archive-signature":
			if archiveSig, e = readPart(part, maxSignature); e != nil {
				failure(w, e)
				return
			}
		case "metadata-signature":
			if metaSig, e = readPart(part, maxSignature); e != nil {
				failure(w, e)
				return
			}
		default:
			failure(w, perr(400, "unexpected part "+part.FormName()))
			return
		}
	}
	if !haveArchive || size == 0 {
		failure(w, perr(422, "source-archive part required"))
		return
	}
	sigFormat := r.Header.Get("X-Swift-Package-Signature-Format")
	if (len(archiveSig) > 0 || len(metaSig) > 0) && sigFormat == "" || sigFormat != "" && len(archiveSig) == 0 {
		failure(w, perr(422, "signature and X-Swift-Package-Signature-Format must be supplied together"))
		return
	}
	if len(sigFormat) > 64 {
		failure(w, perr(422, "invalid signature format"))
		return
	}
	checksum := hex.EncodeToString(sum[:])
	if want := r.Header.Get("X-Checksum-Sha256"); want != "" && want != checksum {
		failure(w, perr(422, "source archive checksum mismatch"))
		return
	}
	meta := map[string]any{}
	if haveMeta {
		if meta, e = validMetadata(metaRaw); e != nil {
			failure(w, perr(422, "invalid metadata"))
			return
		}
	}
	ctx, cancel = context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ins, e := inspect(ctx, f, size)
	if e != nil {
		failure(w, perr(422, "invalid source archive: must be a zip containing Package.swift"))
		return
	}
	rel := release{Checksum: checksum, Size: size, Metadata: meta, Manifests: ins.infos, SignatureFormat: sigFormat}
	if len(archiveSig) > 0 {
		rel.ArchiveSig = base64.StdEncoding.EncodeToString(archiveSig)
	}
	if len(metaSig) > 0 {
		rel.MetadataSig = base64.StdEncoding.EncodeToString(metaSig)
	}
	rb, e := json.Marshal(rel)
	if e != nil {
		failure(w, e)
		return
	}
	archivePub.Info = blob.Info{Size: size, Digest: blob.Digest(checksum)}
	pubs := []artifact.Publication{archivePub}
	relPub := q.pub(q.version, releaseAsset, jsonMediaType)
	relPub.Info = info(rb)
	pubs = append(pubs, relPub)
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		failure(w, e)
		return
	}
	readers := []io.Reader{contextReader{ctx, f}, bytes.NewReader(rb)}
	for _, m := range ins.infos {
		mp := q.pub(q.version, m.File, "text/x-swift")
		mp.Info = info(ins.manifests[m.File])
		pubs = append(pubs, mp)
		readers = append(readers, bytes.NewReader(ins.manifests[m.File]))
	}
	for _, p := range pubs {
		src, e := q.s.service.Prepare(ctx, q.p, q.feed, p.Namespace, p.Coordinate, p.AssetKey)
		if e != nil {
			failure(w, e)
			return
		}
		if src != archivePub.ExpectedSource {
			failure(w, artifact.ErrConflict)
			return
		}
	}
	for i := range pubs {
		pubs[i].ExpectedSource = archivePub.ExpectedSource
	}
	if _, e = q.s.service.PublishBundle(ctx, q.p, pubs, readers); e != nil {
		failure(w, e)
		return
	}
	w.Header().Set("Location", q.url(q.scope, q.name, q.version))
	w.WriteHeader(201)
}
