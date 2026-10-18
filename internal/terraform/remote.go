package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Pull-through for Terraform registries (registry.terraform.io). The origin is
// the registry root; `v1/modules/...` and `v1/providers/...` map one to one.
//
// Providers. `versions` and the per-platform `download` document are read
// through the runtime's metadata cache and validated. The feed's `download`
// answer keeps `shasum` and `signing_keys` exactly as the registry sent them and
// points `download_url`, `shasums_url` and `shasums_signature_url` at the feed's
// `files/<name>` routes, so Terraform verifies the zip, SHA256SUMS and signature
// itself against the upstream publisher key (for example HashiCorp's). Ant also
// checks what it caches: the zip against `shasum`, SHA256SUMS against the zip
// line, and the detached signature against the supplied key when it parses as a
// single public key. Release files normally live on another host
// (releases.hashicorp.com): add it to the source's redirect origins.
//
// Modules. `versions` is read through the metadata cache. The registry answers
// `download` with a 204 and an `X-Terraform-Get` header that the runtime does not
// expose, so the feed resolves the module detail document (`source` GitHub
// repository and `tag`) to the GitHub tarball (`codeload.github.com`), caches the
// tar.gz once (checked as a bounded, safe tar.gz; the registry publishes no
// digest) and answers `download` with its own archive URL plus the `//*` subdir
// that unwraps the GitHub top-level directory. Registries whose modules are not
// GitHub repositories, and zip archives, are not supported. Add
// `https://codeload.github.com` to the source's redirect origins.

var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)

const remoteGetKey = "remote-get"

// githubArchives is the host serving GitHub tarballs (a variable for tests).
var githubArchives = "https://codeload.github.com"

// remoteVersionList fetches the upstream version document of a provider or module.
func (s *Repository) remoteDoc(ctx context.Context, p identity.Principal, pub artifact.Publication, upstreamPath, source string) (artifact.Document, error) {
	return s.service.RemoteMetadata(ctx, p, artifact.Publication{FeedID: pub.FeedID, Namespace: pub.Namespace, Coordinate: pub.Coordinate}, artifact.Origin{Path: upstreamPath, Representation: "application/json", SourceID: source})
}

// serveRemoteModuleVersions answers a module versions request from upstream.
func (s *Repository) serveRemoteModuleVersions(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, rest []string) (bool, error) {
	doc, err := s.remoteDoc(r.Context(), p, pub, "v1/"+pub.Coordinate+"/versions", "")
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	var in struct {
		Modules []struct {
			Versions []struct{ Version string } `json:"versions"`
		} `json:"modules"`
	}
	if json.Unmarshal(doc.Body, &in) != nil || len(in.Modules) == 0 || len(in.Modules[0].Versions) > 4096 {
		failure(w, artifact.ErrUnavailable)
		return true, nil
	}
	type entry struct {
		Version string `json:"version"`
	}
	list := []entry{}
	for _, v := range in.Modules[0].Versions {
		if validVersion(v.Version) {
			list = append(list, entry{v.Version})
		}
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	type module struct {
		Source   string  `json:"source"`
		Versions []entry `json:"versions"`
	}
	writeJSON(w, r, map[string][]module{"modules": {{Source: rest[0] + "/" + rest[1] + "/" + rest[2], Versions: list}}})
	return true, nil
}

// parseGet maps an upstream X-Terraform-Get value to a fetchable https URL and
// the go-getter subdirectory that must be applied after extraction.
func parseGet(v string) (target, subdir string, ok bool) {
	if strings.HasPrefix(v, "git::") {
		u, err := url.Parse(strings.TrimPrefix(v, "git::"))
		if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
			return "", "", false
		}
		repo := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		ref := u.Query().Get("ref")
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || ref == "" || !refPattern.MatchString(ref) || strings.Contains(ref, "..") || !namePart(parts[0]) || !namePart(parts[1]) {
			return "", "", false
		}
		return githubArchives + "/" + parts[0] + "/" + parts[1] + "/tar.gz/" + artifact.EscapePath(ref), "*", true
	}
	if i := strings.Index(v, "://"); i >= 0 {
		rest := v[i+3:]
		query := ""
		if q := strings.Index(rest, "?"); q >= 0 {
			rest, query = rest[:q], rest[q:]
		}
		if j := strings.Index(rest, "//"); j >= 0 {
			subdir, rest = rest[j+2:], rest[:j]
		}
		u, err := url.Parse(v[:i+3] + rest + query)
		if err != nil || u.Scheme != "https" {
			return "", "", false
		}
		q := u.Query()
		if a := q.Get("archive"); a != "" && a != "tar.gz" && a != "tgz" {
			return "", "", false
		}
		q.Del("archive")
		u.RawQuery = q.Encode()
		v = u.String()
	}
	if subdir != "" && subdir != "*" && !safeName(subdir) {
		return "", "", false
	}
	return v, subdir, true
}

func namePart(s string) bool {
	return s != "" && len(s) <= 100 && regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(s)
}

func tarballVerify(rs io.ReadSeeker) error {
	_, err := inspectTar(context.Background(), rs)
	return err
}

func drain(m *artifact.Materialized) {
	if m != nil {
		_, _ = io.Copy(io.Discard, m.Body)
		_ = m.Body.Close()
	}
}

// fillModule materializes a missing module archive and returns its subdir. The
// registry's `download` answers 204 with `X-Terraform-Get`, a header the runtime
// does not expose; the module detail document names the same GitHub repository
// and tag, which is resolved to the GitHub tarball.
func (s *Repository) fillModule(ctx context.Context, p identity.Principal, pub artifact.Publication) (string, error) {
	doc, err := s.remoteDoc(ctx, p, pub, "v1/"+pub.Coordinate+"/"+pub.Version, "")
	if err != nil {
		return "", err
	}
	var detail struct{ Source, Tag string }
	if json.Unmarshal(doc.Body, &detail) != nil || detail.Source == "" || detail.Tag == "" {
		return "", artifact.ErrUnavailable
	}
	target, subdir, ok := parseGet("git::" + detail.Source + "?ref=" + url.QueryEscape(detail.Tag))
	if !ok {
		return "", artifact.ErrUnavailable
	}
	rel, abs, err := artifact.ResolveReference("v1/"+pub.Coordinate+"/"+pub.Version, target)
	if err != nil {
		return "", artifact.ErrUnavailable
	}
	stream, _, err := s.service.PullThrough(ctx, p, pub, artifact.Origin{Path: rel, URL: abs, SourceID: doc.SourceID, Verify: tarballVerify})
	if err != nil {
		return "", err
	}
	drain(stream)
	meta, _ := json.Marshal(map[string]string{"subdir": subdir})
	gp := pub
	gp.AssetKey, gp.MediaType = remoteGetKey, "application/json"
	if err = s.service.CacheDocument(ctx, p, gp, doc.SourceID, meta); err != nil {
		return "", artifact.ErrUnavailable
	}
	return subdir, nil
}

// moduleSubdir returns the cached go-getter subdirectory of a remote archive.
func (s *Repository) moduleSubdir(ctx context.Context, p identity.Principal, pub artifact.Publication) string {
	pub.AssetKey = remoteGetKey
	_, data, err := s.readAsset(ctx, p, pub, 4096)
	var v struct{ Subdir string }
	if err != nil || json.Unmarshal(data, &v) != nil || (v.Subdir != "" && v.Subdir != "*" && !safeName(v.Subdir)) {
		return ""
	}
	return v.Subdir
}

// ----- providers -----

type remoteDownload struct {
	Protocols   []string        `json:"protocols"`
	OS          string          `json:"os"`
	Arch        string          `json:"arch"`
	Filename    string          `json:"filename"`
	DownloadURL string          `json:"download_url"`
	ShasumsURL  string          `json:"shasums_url"`
	SigURL      string          `json:"shasums_signature_url"`
	Shasum      string          `json:"shasum"`
	SigningKeys json.RawMessage `json:"signing_keys"`
}

var hexSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

func downloadPath(pub artifact.Publication, osName, arch string) string {
	return "v1/" + pub.Coordinate + "/" + pub.Version + "/download/" + osName + "/" + arch
}

func urlBase(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return path.Base(raw)
}

// remoteProviderDownload fetches and validates one platform download document.
func (s *Repository) remoteProviderDownload(ctx context.Context, p identity.Principal, pub artifact.Publication, typ, osName, arch string) (remoteDownload, artifact.Document, error) {
	doc, err := s.remoteDoc(ctx, p, pub, downloadPath(pub, osName, arch), "")
	if err != nil {
		return remoteDownload{}, doc, err
	}
	var d remoteDownload
	if json.Unmarshal(doc.Body, &d) != nil || d.OS != osName || d.Arch != arch || d.Filename != zipName(typ, pub.Version, osName, arch) || !hexSum.MatchString(d.Shasum) || d.DownloadURL == "" || urlBase(d.ShasumsURL) != sumsName(typ, pub.Version) || urlBase(d.SigURL) != sumsName(typ, pub.Version)+".sig" {
		return d, doc, artifact.ErrUnavailable
	}
	return d, doc, nil
}

type remoteVersions struct {
	Versions []struct {
		Version   string   `json:"version"`
		Protocols []string `json:"protocols"`
		Platforms []struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"platforms"`
	} `json:"versions"`
}

func (s *Repository) remoteProviderVersions(ctx context.Context, p identity.Principal, pub artifact.Publication) (remoteVersions, artifact.Document, error) {
	doc, err := s.remoteDoc(ctx, p, pub, "v1/"+pub.Coordinate+"/versions", "")
	if err != nil {
		return remoteVersions{}, doc, err
	}
	var in remoteVersions
	if json.Unmarshal(doc.Body, &in) != nil || len(in.Versions) > 4096 {
		return in, doc, artifact.ErrUnavailable
	}
	out := in.Versions[:0]
	for _, v := range in.Versions {
		if !validVersion(v.Version) {
			continue
		}
		ps := v.Platforms[:0]
		for _, pl := range v.Platforms {
			if platformPart.MatchString(pl.OS) && platformPart.MatchString(pl.Arch) {
				ps = append(ps, pl)
			}
		}
		v.Platforms = ps
		protocols := v.Protocols[:0]
		for _, x := range v.Protocols {
			if protocolPattern.MatchString(x) {
				protocols = append(protocols, x)
			}
		}
		v.Protocols = protocols
		out = append(out, v)
	}
	in.Versions = out
	return in, doc, nil
}

func (s *Repository) serveRemoteProviderVersions(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) (bool, error) {
	in, doc, err := s.remoteProviderVersions(r.Context(), p, pub)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	writeJSON(w, r, in)
	return true, nil
}

func (s *Repository) serveRemoteProviderDownload(w http.ResponseWriter, r *http.Request, p identity.Principal, slug string, pub artifact.Publication, typ, osName, arch string) (bool, error) {
	base := s.feedURL(slug)
	if base == "" {
		return false, nil
	}
	d, doc, err := s.remoteProviderDownload(r.Context(), p, pub, typ, osName, arch)
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return false, nil
	case errors.Is(err, artifact.ErrUnavailable):
		return false, err
	case err != nil:
		failure(w, err)
		return true, nil
	}
	if doc.Stale {
		for k, v := range artifact.StaleHeaders() {
			w.Header().Set(k, v)
		}
	}
	files := base + "/v1/" + pub.Coordinate + "/" + pub.Version + "/files/"
	out := map[string]any{
		"protocols":             d.Protocols,
		"os":                    osName,
		"arch":                  arch,
		"filename":              d.Filename,
		"download_url":          files + d.Filename,
		"shasums_url":           files + sumsName(typ, pub.Version),
		"shasums_signature_url": files + sumsName(typ, pub.Version) + ".sig",
		"shasum":                d.Shasum,
	}
	if len(d.SigningKeys) > 0 {
		out["signing_keys"] = d.SigningKeys
	}
	writeJSON(w, r, out)
	return true, nil
}

// signatureVerify checks the detached signature over sums with the keys the
// registry supplied. Keys that do not parse as a single public key are skipped
// (Terraform still verifies the signature itself); if any parses, one must verify.
func signatureVerify(sums []byte, keys json.RawMessage) func(io.ReadSeeker) error {
	return func(rs io.ReadSeeker) error {
		sig, err := io.ReadAll(io.LimitReader(rs, maxSmallPart+1))
		if err != nil || len(sig) == 0 || len(sig) > maxSmallPart {
			return errInvalid
		}
		var in struct {
			Keys []struct {
				ASCIIArmor string `json:"ascii_armor"`
			} `json:"gpg_public_keys"`
		}
		_ = json.Unmarshal(keys, &in)
		parsed := false
		for _, k := range in.Keys {
			key, e := parseKey([]byte(k.ASCIIArmor))
			if e != nil {
				continue
			}
			parsed = true
			if _, e = verifySignature(key, sums, sig); e == nil {
				return nil
			}
		}
		if parsed {
			return errInvalid
		}
		return nil
	}
}

// fillProviderFile materializes one missing provider file: a platform zip,
// SHA256SUMS or its signature.
func (s *Repository) fillProviderFile(ctx context.Context, p identity.Principal, pub artifact.Publication, typ, name string) error {
	sums, sig := sumsName(typ, pub.Version), sumsName(typ, pub.Version)+".sig"
	osName, arch := "", ""
	if name == sums || name == sig {
		in, _, err := s.remoteProviderVersions(ctx, p, pub)
		if err != nil {
			return err
		}
		for _, v := range in.Versions {
			if v.Version == pub.Version && len(v.Platforms) > 0 {
				osName, arch = v.Platforms[0].OS, v.Platforms[0].Arch
			}
		}
		if osName == "" {
			return artifact.ErrNotFound
		}
	} else {
		var ok bool
		if osName, arch, ok = parseZipName(typ, pub.Version, name); !ok {
			return artifact.ErrNotFound
		}
	}
	d, doc, err := s.remoteProviderDownload(ctx, p, pub, typ, osName, arch)
	if err != nil {
		return err
	}
	dp := downloadPath(pub, osName, arch)
	pub.AssetKey = name
	origin := artifact.Origin{SourceID: doc.SourceID}
	ref := ""
	switch name {
	case sums:
		ref, pub.MediaType = d.ShasumsURL, "text/plain"
		filename, shasum := d.Filename, d.Shasum
		origin.Verify = func(rs io.ReadSeeker) error {
			b, e := io.ReadAll(io.LimitReader(rs, maxSmallPart+1))
			if e != nil || len(b) > maxSmallPart {
				return errInvalid
			}
			table, e := parseSums(b)
			if e != nil || table[filename] != shasum {
				return errInvalid
			}
			return nil
		}
	case sig:
		sp := pub
		sp.AssetKey = sums
		if _, e := s.service.Lookup(ctx, p, sp); errors.Is(e, artifact.ErrNotFound) {
			if e = s.fillProviderFile(ctx, p, pub, typ, sums); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		_, data, e := s.readAsset(ctx, p, sp, maxSmallPart)
		if e != nil {
			return e
		}
		ref, pub.MediaType = d.SigURL, "application/pgp-signature"
		origin.Verify = signatureVerify(data, d.SigningKeys)
	default:
		ref, pub.MediaType = d.DownloadURL, "application/zip"
		origin.Digest = blob.Digest(d.Shasum)
	}
	rel, abs, err := artifact.ResolveReference(dp, ref)
	if err != nil {
		return artifact.ErrUnavailable
	}
	origin.Path, origin.URL = rel, abs
	stream, _, err := s.service.PullThrough(ctx, p, pub, origin)
	if err != nil {
		return err
	}
	drain(stream)
	return nil
}
