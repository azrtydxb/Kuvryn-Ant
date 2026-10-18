package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const (
	metadataKey   = "metadata.json"
	signingKeyKey = "signing-key.asc"
	maxUploadBody = 4 << 30
)

var protocolPattern = regexp.MustCompile(`^[1-9][0-9]{0,2}\.[0-9]{1,3}$`)

type platform struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Filename string `json:"filename"`
	Shasum   string `json:"shasum"`
}

// metadata is the deterministic per-version record sealed with the bundle.
type metadata struct {
	Protocols []string   `json:"protocols"`
	KeyID     string     `json:"key_id"`
	Platforms []platform `json:"platforms"`
}

func zipName(typ, version, os, arch string) string {
	return "terraform-provider-" + typ + "_" + version + "_" + os + "_" + arch + ".zip"
}
func sumsName(typ, version string) string {
	return "terraform-provider-" + typ + "_" + version + "_SHA256SUMS"
}

// parseZipName extracts os/arch from an exact-match zip filename.
func parseZipName(typ, version, name string) (osName, arch string, ok bool) {
	prefix := "terraform-provider-" + typ + "_" + version + "_"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".zip") {
		return "", "", false
	}
	osName, arch, ok = strings.Cut(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".zip"), "_")
	return osName, arch, ok && platformPart.MatchString(osName) && platformPart.MatchString(arch)
}

// providers routes the provider registry protocol: rest is {ns}/{type}/...
func (s *Repository) providers(w http.ResponseWriter, r *http.Request, p identity.Principal, slug, feed string, rest []string) {
	if len(rest) < 3 || len(rest) > 6 || !namePattern.MatchString(rest[0]) || !typePattern.MatchString(rest[1]) {
		failure(w, artifact.ErrNotFound)
		return
	}
	pub := artifact.Publication{FeedID: feed, Namespace: providerNamespace, Coordinate: "providers/" + rest[0] + "/" + rest[1]}
	typ := rest[1]
	switch {
	case len(rest) == 3 && rest[2] == "versions" && r.Method != "PUT":
		s.providerVersions(w, r, p, pub)
	case len(rest) == 3 && r.Method == "PUT" && validVersion(rest[2]):
		pub.Version = rest[2]
		s.providerUpload(w, r, p, pub, typ)
	case len(rest) == 6 && rest[3] == "download" && r.Method != "PUT" && validVersion(rest[2]):
		pub.Version = rest[2]
		s.providerDownload(w, r, p, slug, pub, typ, rest[4], rest[5])
	case len(rest) == 5 && rest[3] == "files" && r.Method != "PUT" && validVersion(rest[2]):
		pub.Version = rest[2]
		s.providerFile(w, r, p, pub, typ, rest[4])
	case len(rest) >= 3 && r.Method == "PUT":
		failure(w, errInvalid)
	default:
		failure(w, artifact.ErrNotFound)
	}
}

func (s *Repository) loadMetadata(ctx context.Context, p identity.Principal, pub artifact.Publication, typ string) (metadata, error) {
	pub.AssetKey = metadataKey
	_, data, e := s.readAsset(ctx, p, pub, maxMetadata)
	if e != nil {
		return metadata{}, e
	}
	var m metadata
	if json.Unmarshal(data, &m) != nil || len(m.Protocols) == 0 || len(m.Platforms) == 0 || len(m.Platforms) > maxPlatforms {
		return metadata{}, artifact.ErrUnavailable
	}
	for _, pl := range m.Platforms {
		o, a, ok := parseZipName(typ, pub.Version, pl.Filename)
		if !ok || o != pl.OS || a != pl.Arch || len(pl.Shasum) != 64 {
			return metadata{}, artifact.ErrUnavailable
		}
	}
	return m, nil
}

func (s *Repository) providerVersions(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	versions, e := s.listVersions(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, metadataKey)
	if e != nil {
		failure(w, e)
		return
	}
	typ := pub.Coordinate[strings.LastIndex(pub.Coordinate, "/")+1:]
	type plat struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	}
	type entry struct {
		Version   string   `json:"version"`
		Protocols []string `json:"protocols"`
		Platforms []plat   `json:"platforms"`
	}
	list := make([]entry, 0, len(versions))
	for _, v := range versions {
		pub.Version = v
		m, e := s.loadMetadata(ctx, p, pub, typ)
		if e != nil {
			failure(w, e)
			return
		}
		x := entry{Version: v, Protocols: m.Protocols}
		for _, pl := range m.Platforms {
			x.Platforms = append(x.Platforms, plat{pl.OS, pl.Arch})
		}
		list = append(list, x)
	}
	writeJSON(w, r, map[string]any{"versions": list})
}

func (s *Repository) providerDownload(w http.ResponseWriter, r *http.Request, p identity.Principal, slug string, pub artifact.Publication, typ, osName, arch string) {
	if !platformPart.MatchString(osName) || !platformPart.MatchString(arch) {
		failure(w, errInvalid)
		return
	}
	base := s.feedURL(slug)
	if base == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m, e := s.loadMetadata(ctx, p, pub, typ)
	if e != nil {
		failure(w, e)
		return
	}
	var pl *platform
	for i := range m.Platforms {
		if m.Platforms[i].OS == osName && m.Platforms[i].Arch == arch {
			pl = &m.Platforms[i]
		}
	}
	if pl == nil {
		failure(w, artifact.ErrNotFound)
		return
	}
	// Re-verify the sealed closure on every resolution so that a substituted
	// asset (zip, SHA256SUMS, signature or key) is refused, never advertised.
	zip := pub
	zip.AssetKey = pl.Filename
	zrec, e := s.service.Lookup(ctx, p, zip)
	if e != nil {
		failure(w, e)
		return
	}
	if string(zrec.Info.Digest) != pl.Shasum {
		failure(w, blob.ErrIntegrity)
		return
	}
	sums, sig, key, e := s.loadSigning(ctx, p, pub, typ)
	if e != nil {
		failure(w, e)
		return
	}
	table, e := parseSums(sums)
	if e != nil || table[pl.Filename] != pl.Shasum || key.keyID != m.KeyID {
		failure(w, blob.ErrIntegrity)
		return
	}
	if _, e = verifySignature(key, sums, sig); e != nil {
		failure(w, blob.ErrIntegrity)
		return
	}
	files := base + "/v1/providers/" + pub.Coordinate[len("providers/"):] + "/" + pub.Version + "/files/"
	writeJSON(w, r, map[string]any{
		"protocols":             m.Protocols,
		"os":                    osName,
		"arch":                  arch,
		"filename":              pl.Filename,
		"download_url":          files + pl.Filename,
		"shasums_url":           files + sumsName(typ, pub.Version),
		"shasums_signature_url": files + sumsName(typ, pub.Version) + ".sig",
		"shasum":                pl.Shasum,
		"signing_keys": map[string]any{"gpg_public_keys": []map[string]string{
			{"key_id": key.keyID, "ascii_armor": key.armored},
		}},
	})
}

func (s *Repository) loadSigning(ctx context.Context, p identity.Principal, pub artifact.Publication, typ string) (sums, sig []byte, key signingKey, err error) {
	pub.AssetKey = sumsName(typ, pub.Version)
	if _, sums, err = s.readAsset(ctx, p, pub, maxSmallPart); err != nil {
		return
	}
	pub.AssetKey += ".sig"
	if _, sig, err = s.readAsset(ctx, p, pub, maxSmallPart); err != nil {
		return
	}
	pub.AssetKey = signingKeyKey
	var raw []byte
	if _, raw, err = s.readAsset(ctx, p, pub, maxSmallPart); err != nil {
		return
	}
	if key, err = parseKey(raw); err != nil {
		err = artifact.ErrUnavailable
	}
	return
}

func (s *Repository) providerFile(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, typ, name string) {
	switch {
	case name == sumsName(typ, pub.Version):
		pub.MediaType = "text/plain"
	case name == sumsName(typ, pub.Version)+".sig":
		pub.MediaType = "application/pgp-signature"
	default:
		if _, _, ok := parseZipName(typ, pub.Version, name); !ok {
			failure(w, artifact.ErrNotFound)
			return
		}
	}
	pub.AssetKey = name
	s.serveAsset(w, r, p, pub)
}

type upload struct {
	sums, sig, key, protocols []byte
	zips                      map[string]*spooled
}

func (u *upload) close() {
	for _, z := range u.zips {
		z.close()
	}
}

func readSmall(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(contextReader{ctx, r}, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errInvalid
	}
	return b, nil
}

func readUpload(ctx context.Context, mr *multipart.Reader) (*upload, error) {
	u := &upload{zips: map[string]*spooled{}}
	ok := false
	defer func() {
		if !ok {
			u.close()
		}
	}()
	seen := map[string]bool{}
	for {
		part, e := mr.NextPart()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, errInvalid
		}
		name := part.FormName()
		if name == "zip" {
			fn := part.FileName()
			if fn == "" || u.zips[fn] != nil || len(u.zips) >= maxPlatforms {
				return nil, errInvalid
			}
			z, e := spool(ctx, part, maxProviderZip)
			if e != nil {
				return nil, errInvalid
			}
			u.zips[fn] = z
			continue
		}
		if seen[name] {
			return nil, errInvalid
		}
		seen[name] = true
		limit := int64(maxSmallPart)
		if name == "protocols" {
			limit = 64
		}
		b, e := readSmall(ctx, part, limit)
		if e != nil {
			return nil, e
		}
		switch name {
		case "shasums":
			u.sums = b
		case "shasums_sig":
			u.sig = b
		case "signing_key":
			u.key = b
		case "protocols":
			u.protocols = b
		default:
			return nil, errInvalid
		}
	}
	ok = true
	return u, nil
}

// providerUpload is the admin ingestion endpoint. One multipart request carries
// every platform zip plus SHA256SUMS, its detached signature and the public key;
// all of it is validated and then published as one atomic bundle.
func (s *Repository) providerUpload(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, typ string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	source, e := s.prepare(ctx, p, pub.FeedID, pub.Namespace, pub.Coordinate, metadataKey)
	if e != nil {
		failure(w, e)
		return
	}
	mediatype := r.Header.Get("Content-Type")
	if !strings.HasPrefix(mediatype, "multipart/form-data") {
		failure(w, errInvalid)
		return
	}
	mr, e := r.MultipartReader()
	if e != nil {
		failure(w, errInvalid)
		return
	}
	if !s.acquire() {
		failure(w, artifact.ErrUnavailable)
		return
	}
	defer s.release()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	u, e := readUpload(ctx, mr)
	if e != nil {
		failure(w, e)
		return
	}
	defer u.close()
	bundle, bodies, e := buildBundle(ctx, u, pub, typ, source)
	if e != nil {
		failure(w, e)
		return
	}
	if _, e = s.service.PublishBundle(ctx, p, bundle, bodies); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// buildBundle validates the whole closure and returns the publications.
func buildBundle(ctx context.Context, u *upload, base artifact.Publication, typ, source string) ([]artifact.Publication, []io.Reader, error) {
	if len(u.zips) == 0 || len(u.sums) == 0 || len(u.sig) == 0 || len(u.key) == 0 {
		return nil, nil, errInvalid
	}
	protocols := []string{"5.0"}
	if len(u.protocols) > 0 {
		protocols = nil
		for _, x := range strings.Split(string(u.protocols), ",") {
			x = strings.TrimSpace(x)
			if !protocolPattern.MatchString(x) || len(protocols) >= 4 {
				return nil, nil, errInvalid
			}
			protocols = append(protocols, x)
		}
	}
	key, e := parseKey(u.key)
	if e != nil {
		return nil, nil, e
	}
	sig, e := verifySignature(key, u.sums, u.sig)
	if e != nil {
		return nil, nil, e
	}
	table, e := parseSums(u.sums)
	if e != nil {
		return nil, nil, e
	}
	// Exact closure: SHA256SUMS lists every uploaded zip and nothing else.
	if len(table) != len(u.zips) {
		return nil, nil, errInvalid
	}
	names := make([]string, 0, len(u.zips))
	for n := range u.zips {
		names = append(names, n)
	}
	sort.Strings(names)
	meta := metadata{Protocols: protocols, KeyID: key.keyID}
	seenPlatform := map[string]bool{}
	var pubs []artifact.Publication
	var bodies []io.Reader
	var refs []artifact.Reference
	add := func(key, media string, info blob.Info, body io.Reader) {
		x := base
		x.AssetKey, x.MediaType, x.Info, x.ExpectedSource = key, media, info, source
		pubs = append(pubs, x)
		bodies = append(bodies, body)
		refs = append(refs, artifact.Reference{Version: base.Version, AssetKey: key, Info: info})
	}
	for _, n := range names {
		z := u.zips[n]
		o, a, ok := parseZipName(typ, base.Version, n)
		if !ok || seenPlatform[o+"_"+a] || table[n] != z.info.digest {
			return nil, nil, errInvalid
		}
		seenPlatform[o+"_"+a] = true
		if e := inspectProviderZip(z.f, z.info.size, typ); e != nil {
			return nil, nil, e
		}
		if _, e := z.f.Seek(0, 0); e != nil {
			return nil, nil, e
		}
		meta.Platforms = append(meta.Platforms, platform{OS: o, Arch: a, Filename: n, Shasum: z.info.digest})
		add(n, "application/zip", blob.Info{Size: z.info.size, Digest: blob.Digest(z.info.digest)}, contextReader{ctx, z.f})
	}
	add(sumsName(typ, base.Version), "text/plain", info(u.sums), bytes.NewReader(u.sums))
	add(sumsName(typ, base.Version)+".sig", "application/pgp-signature", info(sig), bytes.NewReader(sig))
	add(signingKeyKey, "application/pgp-keys", info([]byte(key.armored)), strings.NewReader(key.armored))
	mj, e := json.Marshal(meta)
	if e != nil || len(mj) > maxMetadata {
		return nil, nil, errInvalid
	}
	x := base
	x.AssetKey, x.MediaType, x.Info, x.ExpectedSource, x.References = metadataKey, "application/json", info(mj), source, refs
	pubs = append(pubs, x)
	bodies = append(bodies, bytes.NewReader(mj))
	return pubs, bodies, nil
}
