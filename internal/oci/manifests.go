package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const manifestLimit = 4 << 20
const imageManifest = "application/vnd.oci.image.manifest.v1+json"
const imageIndex = "application/vnd.oci.image.index.v1+json"
const dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
const dockerIndex = "application/vnd.docker.distribution.manifest.list.v2+json"

func manifestType(value string) bool {
	return value == imageManifest || value == imageIndex || value == dockerManifest || value == dockerIndex
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      *int64 `json:"size"`
}
type manifestDocument struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        *descriptor  `json:"config"`
	Layers        []descriptor `json:"layers"`
	Manifests     []descriptor `json:"manifests"`
	Subject       *descriptor  `json:"subject"`
}

func validateManifest(body []byte, media string) ([]artifact.Reference, error) {
	if !utf8.Valid(body) || uniqueJSON(body) != nil {
		return nil, errors.New("invalid manifest JSON")
	}
	var document manifestDocument
	if json.Unmarshal(body, &document) != nil || document.SchemaVersion != 2 || !manifestType(media) || (document.MediaType != "" && document.MediaType != media) {
		return nil, errors.New("invalid manifest")
	}
	var refs []artifact.Reference
	appendRef := func(desc descriptor, key string) bool {
		digest, ok := parseDigest(desc.Digest)
		if !ok || desc.Size == nil || *desc.Size < 0 || *desc.Size > MaxBlob || desc.MediaType == "" || len(desc.MediaType) > 256 {
			return false
		}
		refs = append(refs, artifact.Reference{Version: string(digest), AssetKey: key, Info: blob.Info{Digest: digest, Size: *desc.Size}})
		return true
	}
	if media == imageIndex || media == dockerIndex {
		if document.Manifests == nil || document.Config != nil || document.Layers != nil || len(document.Manifests) > 4096 {
			return nil, errors.New("invalid index")
		}
		for _, desc := range document.Manifests {
			if !manifestType(desc.MediaType) || !appendRef(desc, "manifest") {
				return nil, errors.New("invalid manifest reference")
			}
		}
	} else {
		if document.Config == nil || document.Layers == nil || document.Manifests != nil || len(document.Layers) > 4095 || !appendRef(*document.Config, "blob") {
			return nil, errors.New("invalid image manifest")
		}
		for _, desc := range document.Layers {
			if !appendRef(desc, "blob") {
				return nil, errors.New("invalid layer")
			}
		}
	}
	// OCI1.1 subjects may be absent from the repository at publication time.
	// Validate descriptor syntax, but never impose closure on this association.
	if document.Subject != nil {
		before := len(refs)
		if !appendRef(*document.Subject, "manifest") {
			return nil, errors.New("invalid subject")
		}
		refs = refs[:before]
	}
	return refs, nil
}
func (r *Registry) manifest(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, reference string) {
	expected, isDigest := parseDigest(reference)
	if !isDigest && !tagPattern.MatchString(reference) {
		failure(w, 400, "TAG_INVALID", "invalid manifest reference")
		return
	}
	source, err := r.service.Prepare(req.Context(), p, rep.feed, rep.namespace, rep.name, "manifest")
	if err != nil {
		failService(w, err)
		return
	}
	media, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || !manifestType(media) {
		failure(w, 415, "MANIFEST_INVALID", "unsupported manifest media type")
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	body, err := io.ReadAll(&progressReader{ctx: req.Context(), reader: http.MaxBytesReader(w, req.Body, manifestLimit), controller: controller})
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			failure(w, 413, "MANIFEST_INVALID", "manifest too large")
		} else {
			failure(w, 400, "MANIFEST_INVALID", "invalid manifest body")
		}
		return
	}
	references, err := validateManifest(body, media)
	if err != nil {
		failure(w, 400, "MANIFEST_INVALID", "invalid manifest document")
		return
	}
	sum := sha256.Sum256(body)
	digest := blob.Digest(hex.EncodeToString(sum[:]))
	if isDigest && digest != expected {
		failure(w, 400, "DIGEST_INVALID", "manifest digest does not match")
		return
	}
	publication := rep.publication(string(digest), "manifest")
	publication.ExpectedSource = source
	publication.Info = blob.Info{Digest: digest, Size: int64(len(body))}
	publication.MediaType = media
	publication.References = references
	if !isDigest {
		publication.Tag = reference
	}
	if _, err = r.service.Publish(req.Context(), p, publication, bytes.NewReader(body)); err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			failure(w, 400, "MANIFEST_BLOB_UNKNOWN", "manifest references unavailable content")
		} else {
			failService(w, err)
		}
		return
	}
	w.Header().Set("Location", "/v2/"+rep.full+"/manifests/sha256:"+string(digest))
	w.Header().Set("Docker-Content-Digest", "sha256:"+string(digest))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(201)
}
func accepts(raw, media string) bool {
	if raw == "" {
		return true
	}
	best := -1
	quality := float64(0)
	for _, entry := range strings.Split(raw, ",") {
		kind, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
		if err != nil {
			continue
		}
		q := float64(1)
		if rawQ, ok := params["q"]; ok {
			q, err = strconv.ParseFloat(rawQ, 64)
			if err != nil || q < 0 || q > 1 {
				continue
			}
		}
		rank := -1
		if kind == media {
			rank = 2
		} else if kind == "application/*" {
			rank = 1
		} else if kind == "*/*" {
			rank = 0
		}
		if rank > best {
			best = rank
			quality = q
		} else if rank == best && q > quality {
			quality = q
		}
	}
	return best >= 0 && quality > 0
}

// Duplicate keys can make clients interpret a different dependency closure than
// the registry validated. Reject them at every level without rewriting bytes.
func uniqueJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("manifest nesting too deep")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate manifest member")
				}
				seen[name] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing manifest data")
	}
	return nil
}
