package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LoadCAPool reads a PEM trust bundle that the operator mounted. Callers pass a
// path resolved from an operator-configured reference, never a request value.
func LoadCAPool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 1<<20 {
		return nil, fmt.Errorf("%w: CA bundle is not readable", ErrInput)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%w: CA bundle holds no certificate", ErrInput)
	}
	return pool, nil
}

// migrationClient builds an HTTP client that trusts rootCAs (when set) and
// never follows a redirect to another host, so credentials cannot be forwarded.
func migrationClient(base *url.URL, override *http.Client, rootCAs *x509.CertPool) *http.Client {
	c := override
	if c == nil {
		tr := &http.Transport{ResponseHeaderTimeout: 30 * time.Second}
		if rootCAs != nil {
			tr.TLSClientConfig = &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12}
		}
		c = &http.Client{Timeout: 0, Transport: tr}
	}
	cc := *c
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != base.Host || len(via) > 5 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &cc
}

// ArtifactoryConfig addresses a JFrog Artifactory instance. Credentials are
// supplied for one operation and are never persisted or logged.
type ArtifactoryConfig struct {
	// BaseURL is the instance URL; "/artifactory" is appended when the URL has no path.
	BaseURL  string
	Username string
	// Password is the account password or API key (basic authentication).
	Password string
	// Token, when set, is sent as a bearer access token instead.
	Token string
	// RootCAs, when set, replaces the system trust store for this instance.
	RootCAs *x509.CertPool
	// HTTPClient overrides the default client (tests).
	HTTPClient *http.Client
}

// Artifactory implements Manager over the Artifactory REST API.
type Artifactory struct {
	base *url.URL
	cfg  ArtifactoryConfig
	http *http.Client

	mu      sync.Mutex
	formats map[string]string // repository key -> package type, from Repositories
}

var artifactoryFormats = map[string]string{
	"docker": "oci", "oci": "oci", "helmoci": "oci", "npm": "npm", "pypi": "pypi", "maven": "maven", "gradle": "maven",
	"go": "go", "cargo": "cargo", "nuget": "nuget", "helm": "helm", "gems": "rubygems", "composer": "composer",
	"conda": "conda", "conan": "conan", "cran": "cran", "pub": "pub", "cocoapods": "cocoapods", "swift": "swift",
	"terraform": "terraform", "ansible": "ansible", "debian": "apt", "rpm": "rpm", "alpine": "apk",
	"gitlfs": "git-lfs", "huggingfaceml": "huggingface", "generic": "raw", "p2": "p2",
}

// NewArtifactory validates the base URL and builds a client.
func NewArtifactory(cfg ArtifactoryConfig) (*Artifactory, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: invalid Artifactory base URL", ErrInput)
	}
	if u.Path == "" {
		u.Path = "/artifactory"
	}
	c := migrationClient(u, cfg.HTTPClient, cfg.RootCAs)
	cfg.HTTPClient, cfg.RootCAs = nil, nil
	return &Artifactory{base: u, cfg: cfg, http: c, formats: map[string]string{}}, nil
}

func (a *Artifactory) Name() string { return "artifactory" }

func (a *Artifactory) AntFormat(f string) (string, bool) {
	v, ok := artifactoryFormats[strings.ToLower(f)]
	return v, ok
}

// CopyableFormats lists the formats whose content this manager can copy.
func (a *Artifactory) CopyableFormats() map[string]bool {
	return map[string]bool{"raw": true, "maven": true, "oci": true}
}

func (a *Artifactory) do(ctx context.Context, method, rawURL string, body []byte, hdr map[string]string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != a.base.Host || u.Scheme != a.base.Scheme {
		return nil, fmt.Errorf("%w: refusing to send credentials to a different origin", ErrInput)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	switch {
	case a.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	case a.cfg.Username != "":
		req.SetBasicAuth(a.cfg.Username, a.cfg.Password)
	}
	req.Header.Set("Accept", "application/json, */*")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return a.http.Do(req)
}

// path builds a URL below the base from already-unescaped path segments joined
// by "/"; every segment is escaped.
func (a *Artifactory) path(segments string, q url.Values) string {
	u := *a.base
	parts := strings.Split(segments, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	u.Path = strings.TrimRight(a.base.Path, "/") + "/" + segments
	u.RawPath = strings.TrimRight(a.base.EscapedPath(), "/") + "/" + strings.Join(parts, "/")
	u.RawQuery = q.Encode()
	return u.String()
}

// statusError carries only the HTTP status, never a response body.
type statusError struct{ Code int }

func (e statusError) Error() string { return fmt.Sprintf("artifactory returned HTTP %d", e.Code) }

func (a *Artifactory) getJSON(ctx context.Context, rawURL string, out any) error {
	resp, err := a.do(ctx, http.MethodGet, rawURL, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError{resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

type artifactoryRepo struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	PackageType string `json:"packageType"`
}

type artifactoryRepoDetail struct {
	RClass                                        string   `json:"rclass"`
	PackageType                                   string   `json:"packageType"`
	URL                                           string   `json:"url"`
	Username                                      string   `json:"username"`
	Repositories                                  []string `json:"repositories"`
	DefaultDeploymentRepo                         string   `json:"defaultDeploymentRepo"`
	RetrievalCachePeriodSecs                      int64    `json:"retrievalCachePeriodSecs"`
	Offline                                       bool     `json:"offline"`
	Proxy                                         string   `json:"proxy"`
	ClientTLSCertificate                          string   `json:"clientTlsCertificate"`
	MaxUniqueSnapshots                            int      `json:"maxUniqueSnapshots"`
	MaxUniqueTags                                 int      `json:"maxUniqueTags"`
	ExternalDependenciesEnabled                   bool     `json:"externalDependenciesEnabled"`
	ArtifactoryRequestsCanRetrieveRemoteArtifacts bool     `json:"artifactoryRequestsCanRetrieveRemoteArtifacts"`
}

// Repositories lists repositories with their type and package type, and reads
// each repository's configuration for remote URLs and virtual members.
func (a *Artifactory) Repositories(ctx context.Context) ([]SourceRepo, error) {
	var list []artifactoryRepo
	if err := a.getJSON(ctx, a.path("api/repositories", nil), &list); err != nil {
		return nil, err
	}
	out := make([]SourceRepo, 0, len(list))
	formats := map[string]string{}
	for _, r := range list {
		pt := strings.ToLower(r.PackageType)
		formats[r.Key] = pt
		sr := SourceRepo{Name: r.Key, Format: pt}
		rtype := strings.ToLower(r.Type)
		switch rtype {
		case "local":
			sr.Type = "hosted"
		case "federated":
			sr.Type = "hosted"
			sr.Notes = append(sr.Notes, "federated repository mapped as a local source; federation members are not recreated (use Ant replication)")
		case "remote":
			sr.Type = "proxy"
		case "virtual":
			sr.Type = "group"
		default:
			sr.Type = rtype
		}
		var d artifactoryRepoDetail
		err := a.getJSON(ctx, a.path("api/repositories/"+r.Key, nil), &d)
		if err != nil {
			// A local repository needs nothing from its configuration; for the
			// others a missing configuration would mean a wrong mapping.
			if sr.Type == "proxy" || sr.Type == "group" {
				var se statusError
				if errors.As(err, &se) {
					sr.ConfigErr = fmt.Sprintf("HTTP %d; reading repository configuration needs admin permission", se.Code)
				} else {
					sr.ConfigErr = "request failed"
				}
			}
			out = append(out, sr)
			continue
		}
		switch sr.Type {
		case "proxy":
			sr.RemoteURL = d.URL
			sr.MetadataMaxAge = d.RetrievalCachePeriodSecs
			// Artifactory never returns the stored password; a username means the
			// remote repository authenticates upstream.
			sr.Authenticated = d.Username != ""
			if d.Offline {
				sr.Notes = append(sr.Notes, "remote repository is set offline in Artifactory; Ant fetches from the upstream")
			}
			if d.Proxy != "" {
				sr.Notes = append(sr.Notes, "outbound proxy "+d.Proxy+" is not mapped; configure egress for the Ant deployment")
			}
			if d.ClientTLSCertificate != "" {
				sr.Notes = append(sr.Notes, "client TLS certificate is not mapped")
			}
		case "group":
			sr.Members = d.Repositories
			sr.WritableMember = d.DefaultDeploymentRepo
		case "hosted":
			if d.MaxUniqueSnapshots > 0 {
				sr.Notes = append(sr.Notes, fmt.Sprintf("maxUniqueSnapshots=%d is not mapped to a retention policy", d.MaxUniqueSnapshots))
			}
			if d.MaxUniqueTags > 0 {
				sr.Notes = append(sr.Notes, fmt.Sprintf("maxUniqueTags=%d is not mapped to a retention policy", d.MaxUniqueTags))
			}
		}
		out = append(out, sr)
	}
	a.mu.Lock()
	a.formats = formats
	a.mu.Unlock()
	return out, nil
}

// CleanupPolicies: Artifactory keeps no cleanup policies in repository
// configuration that Ant could read through this API.
func (a *Artifactory) CleanupPolicies(context.Context) ([]CleanupPolicy, error) { return nil, nil }

var sizePattern = regexp.MustCompile(`^\s*([0-9][0-9,]*\.?[0-9]*)\s*([A-Za-z]*)\s*$`)

func parseUsedSpace(s string) int64 {
	m := sizePattern.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"": 1, "bytes": 1, "byte": 1, "b": 1, "kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "tb": 1 << 40}[strings.ToLower(m[2])]
	return int64(v * mult)
}

// Stats reads the storage summary. It needs admin permission and Artifactory
// calculates it periodically, so the numbers can lag the live content.
func (a *Artifactory) Stats(ctx context.Context) (map[string]RepoStat, error) {
	var info struct {
		Repos []struct {
			RepoKey          string `json:"repoKey"`
			FilesCount       int    `json:"filesCount"`
			UsedSpace        string `json:"usedSpace"`
			UsedSpaceInBytes *int64 `json:"usedSpaceInBytes"`
		} `json:"repositoriesSummaryList"`
	}
	if err := a.getJSON(ctx, a.path("api/storageinfo", nil), &info); err != nil {
		return nil, err
	}
	out := map[string]RepoStat{}
	for _, r := range info.Repos {
		st := RepoStat{Files: r.FilesCount}
		if r.UsedSpaceInBytes != nil {
			st.Bytes = *r.UsedSpaceInBytes
		} else {
			st.Bytes = parseUsedSpace(r.UsedSpace)
		}
		out[r.RepoKey] = st
	}
	return out, nil
}

// Identities reports users, groups and permission targets. Ant does not import
// accounts or Artifactory permission targets; they are listed with guidance.
func (a *Artifactory) Identities(ctx context.Context) ([]Unmapped, error) {
	var out []Unmapped
	names := func(path string, key string) ([]string, error) {
		var list []map[string]any
		if err := a.getJSON(ctx, a.path(path, nil), &list); err != nil {
			return nil, err
		}
		var n []string
		for _, e := range list {
			if s, ok := e[key].(string); ok {
				n = append(n, s)
			}
		}
		return n, nil
	}
	summary := func(kind string, n []string, guidance string) {
		if len(n) == 0 {
			return
		}
		sample := n
		if len(sample) > 10 {
			sample = sample[:10]
		}
		out = append(out, Unmapped{kind, fmt.Sprintf("%d %ss", len(n), kind), guidance + " Examples: " + strings.Join(sample, ", ") + "."})
	}
	users, err := names("api/security/users", "name")
	if err != nil {
		return nil, err
	}
	summary("user", users, "Accounts are not imported. Sign users in through OIDC or SAML and bind identities to Ant roles, or issue scoped service tokens for automation.")
	if groups, err := names("api/security/groups", "name"); err == nil {
		summary("group", groups, "Groups are not imported. Map directory groups to Ant roles with identity bindings.")
	}
	if perms, err := names("api/v2/security/permissions", "name"); err == nil {
		for i, p := range perms {
			if i >= 100 {
				out = append(out, Unmapped{"permission", fmt.Sprintf("%d more permission targets", len(perms)-i), "Not listed individually."})
				break
			}
			out = append(out, Unmapped{"permission", p, "Permission targets are not imported. Recreate the access as Ant roles and feed-scoped service tokens."})
		}
	}
	return out, nil
}

type aqlResult struct {
	Results []struct {
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Name   string `json:"name"`
		Size   *int64 `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"results"`
}

var aqlPage = 1000

// Assets streams the files of a repository. Docker and OCI repositories are
// read through the registry API; every other repository through AQL.
func (a *Artifactory) Assets(ctx context.Context, repo string, fn func(SourceAsset) error) error {
	a.mu.Lock()
	known := len(a.formats) > 0
	f := a.formats[repo]
	a.mu.Unlock()
	if !known {
		if _, err := a.Repositories(ctx); err != nil {
			return err
		}
		a.mu.Lock()
		f = a.formats[repo]
		a.mu.Unlock()
	}
	if af, _ := a.AntFormat(f); af == "oci" {
		return a.ociAssets(ctx, repo, fn)
	}
	key, _ := json.Marshal(repo)
	for offset := 0; offset < 100_000_000; {
		query := fmt.Sprintf(`items.find({"repo":%s,"type":"file"}).include("repo","path","name","size","sha256").sort({"$asc":["path","name"]}).offset(%d).limit(%d)`, key, offset, aqlPage)
		resp, err := a.do(ctx, http.MethodPost, a.path("api/search/aql", nil), []byte(query), map[string]string{"Content-Type": "text/plain"})
		if err != nil {
			return err
		}
		var res aqlResult
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return statusError{resp.StatusCode}
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(res.Results) == 0 {
			return nil
		}
		for _, it := range res.Results {
			if strings.HasPrefix(it.Path, ".artifactory") || strings.HasPrefix(it.Path, "_uploads") {
				continue
			}
			p := it.Name
			if it.Path != "" && it.Path != "." {
				p = it.Path + "/" + it.Name
			}
			sa := SourceAsset{Repository: repo, Path: p, SHA256: strings.ToLower(it.SHA256), Size: -1}
			if it.Size != nil {
				sa.Size = *it.Size
			}
			if err := fn(sa); err != nil {
				return err
			}
		}
		offset += len(res.Results)
	}
	return fmt.Errorf("artifactory AQL listing did not terminate")
}

// Open downloads a file, or a registry manifest or blob.
func (a *Artifactory) Open(ctx context.Context, sa SourceAsset) (io.ReadCloser, error) {
	var raw string
	hdr := map[string]string{}
	switch sa.Kind {
	case "manifest":
		raw = a.registry(sa.Repository, sa.Image+"/manifests/sha256:"+sa.SHA256)
		hdr["Accept"] = ociAccept
	case "blob":
		raw = a.registry(sa.Repository, sa.Image+"/blobs/sha256:"+sa.SHA256)
	default:
		raw = a.path(sa.Repository+"/"+sa.Path, nil)
	}
	resp, err := a.do(ctx, http.MethodGet, raw, nil, hdr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, statusError{resp.StatusCode}
	}
	return resp.Body, nil
}

const ociAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

// registry builds a Docker v2 API URL for a repository. rest is
// "_catalog", "<image>/tags/list" or "<image>/manifests/<ref>".
func (a *Artifactory) registry(repo, rest string) string {
	return a.path("api/docker/"+repo+"/v2/"+rest, nil)
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// pagedNames reads a Docker v2 list ("repositories" or "tags") following Link headers.
func (a *Artifactory) pagedNames(ctx context.Context, first, field string) ([]string, error) {
	var all []string
	next := first
	for page := 0; next != "" && page < 10000; page++ {
		resp, err := a.do(ctx, http.MethodGet, next, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return all, nil
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return nil, statusError{resp.StatusCode}
		}
		var doc map[string]json.RawMessage
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&doc)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var names []string
		_ = json.Unmarshal(doc[field], &names)
		all = append(all, names...)
		next = ""
		if m := linkNext.FindStringSubmatch(link); m != nil && len(names) > 0 {
			ref, err := url.Parse(m[1])
			if err != nil {
				return nil, err
			}
			next = a.base.ResolveReference(ref).String()
			if ref.Path != "" && !strings.HasPrefix(ref.Path, "/") {
				next = ""
			}
		}
	}
	return all, nil
}

type ociDesc struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      *int64 `json:"size"`
}

type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        *ociDesc          `json:"config"`
	Layers        []ociDesc         `json:"layers"`
	Manifests     []ociDesc         `json:"manifests"`
	Subject       *ociDesc          `json:"subject"`
	Annotations   map[string]string `json:"annotations"`
}

const maxManifest = 4 << 20

func isManifestType(m string) bool {
	switch m {
	case "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}
	return false
}

func (a *Artifactory) fetchManifest(ctx context.Context, repo, image, ref string) (body []byte, media, digest string, err error) {
	resp, err := a.do(ctx, http.MethodGet, a.registry(repo, image+"/manifests/"+ref), nil, map[string]string{"Accept": ociAccept})
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", "", statusError{resp.StatusCode}
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(body) > maxManifest {
		return nil, "", "", errors.New("manifest is larger than 4 MiB")
	}
	media = strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	sum := sha256.Sum256(body)
	return body, media, hex.EncodeToString(sum[:]), nil
}

func hexDigest(d string) (string, bool) {
	alg, v, ok := strings.Cut(d, ":")
	return v, ok && alg == "sha256" && digestPattern.MatchString(v)
}

// ociAssets enumerates the manifests and blobs of every image in a Docker
// repository: tags are resolved to manifests, image indexes to their children,
// and each image manifest to its config and layer blobs.
func (a *Artifactory) ociAssets(ctx context.Context, repo string, fn func(SourceAsset) error) error {
	images, err := a.pagedNames(ctx, a.path("api/docker/"+repo+"/v2/_catalog", map[string][]string{"n": {"1000"}}), "repositories")
	if err != nil {
		return err
	}
	for _, image := range images {
		tags, err := a.pagedNames(ctx, a.path("api/docker/"+repo+"/v2/"+image+"/tags/list", map[string][]string{"n": {"1000"}}), "tags")
		if err != nil {
			return err
		}
		emitted := map[string]bool{}
		type top struct {
			body  []byte
			media string
			tags  []string
		}
		tops := map[string]*top{}
		var order []string
		for _, tag := range tags {
			body, media, digest, err := a.fetchManifest(ctx, repo, image, tag)
			if err != nil {
				var se statusError
				if errors.As(err, &se) && se.Code == http.StatusNotFound {
					continue
				}
				return err
			}
			if !isManifestType(media) {
				if err := fn(SourceAsset{Repository: repo, Image: image, Path: image + ":" + tag, Note: "manifest type " + media + " is not supported (schema 1 images must be rebuilt)"}); err != nil {
					return err
				}
				continue
			}
			t := tops[digest]
			if t == nil {
				t = &top{body: body, media: media}
				tops[digest] = t
				order = append(order, digest)
			}
			t.tags = append(t.tags, tag)
		}
		var emit func(digest string, body []byte, media string, tags []string) error
		emit = func(digest string, body []byte, media string, tags []string) error {
			if emitted[digest] {
				return nil
			}
			emitted[digest] = true
			var m ociManifest
			if json.Unmarshal(body, &m) != nil || m.SchemaVersion != 2 {
				return fn(SourceAsset{Repository: repo, Image: image, Path: image + "@sha256:" + digest, Note: "manifest could not be parsed"})
			}
			sa := SourceAsset{Repository: repo, Image: image, Kind: "manifest", Path: image + "@sha256:" + digest, SHA256: digest, Size: int64(len(body)), ContentType: media, Tags: tags}
			if m.Subject != nil {
				if sd, ok := hexDigest(m.Subject.Digest); ok && m.Subject.Size != nil {
					at := m.ArtifactType
					if at == "" && m.Config != nil {
						at = m.Config.MediaType
					}
					ann, _ := json.Marshal(m.Annotations)
					sa.Subject = &SubjectDef{Digest: sd, Size: *m.Subject.Size, MediaType: m.Subject.MediaType, ArtifactType: at, Annotations: ann}
				}
			}
			if err := fn(sa); err != nil {
				return err
			}
			if m.Manifests != nil {
				for _, c := range m.Manifests {
					cd, ok := hexDigest(c.Digest)
					if !ok {
						continue
					}
					cb, cm, got, err := a.fetchManifest(ctx, repo, image, "sha256:"+cd)
					if err != nil {
						var se statusError
						if errors.As(err, &se) && se.Code == http.StatusNotFound {
							if err := fn(SourceAsset{Repository: repo, Image: image, Path: image + "@sha256:" + cd, Note: "index child manifest is missing in the source repository"}); err != nil {
								return err
							}
							continue
						}
						return err
					}
					if got != cd || !isManifestType(cm) {
						if err := fn(SourceAsset{Repository: repo, Image: image, Path: image + "@sha256:" + cd, Note: "index child manifest does not match its digest or type"}); err != nil {
							return err
						}
						continue
					}
					if err := emit(cd, cb, cm, nil); err != nil {
						return err
					}
				}
				return nil
			}
			descs := m.Layers
			if m.Config != nil {
				descs = append([]ociDesc{*m.Config}, descs...)
			}
			for _, d := range descs {
				bd, ok := hexDigest(d.Digest)
				if !ok || d.Size == nil {
					if err := fn(SourceAsset{Repository: repo, Image: image, Path: image + "@" + d.Digest, Note: "blob digest is not sha256 or has no size"}); err != nil {
						return err
					}
					continue
				}
				if emitted[bd] {
					continue
				}
				emitted[bd] = true
				if err := fn(SourceAsset{Repository: repo, Image: image, Kind: "blob", Path: image + "@sha256:" + bd, SHA256: bd, Size: *d.Size, ContentType: d.MediaType}); err != nil {
					return err
				}
			}
			return nil
		}
		for _, digest := range order {
			t := tops[digest]
			if err := emit(digest, t.body, t.media, t.tags); err != nil {
				return err
			}
		}
	}
	return nil
}
