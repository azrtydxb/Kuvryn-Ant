package transfer

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// NexusConfig addresses a Sonatype Nexus Repository 3 instance. Credentials are
// supplied by the caller for one operation and are never persisted or logged.
type NexusConfig struct {
	BaseURL  string
	Username string
	Password string
	// Token, when set, is sent as a bearer token instead of basic credentials.
	Token string
	// RootCAs, when set, replaces the system trust store for this instance.
	RootCAs *x509.CertPool
	// HTTPClient overrides the default client (tests).
	HTTPClient *http.Client
}

// Nexus implements Manager over the Nexus 3 REST API.
type Nexus struct {
	base *url.URL
	cfg  NexusConfig
	http *http.Client
}

var nexusFormats = map[string]string{
	"maven2": "maven", "npm": "npm", "nuget": "nuget", "pypi": "pypi", "rubygems": "rubygems",
	"docker": "oci", "helm": "helm", "raw": "raw", "yum": "rpm", "apt": "apt", "r": "cran",
	"conan": "conan", "conda": "conda", "go": "go", "cargo": "cargo", "p2": "p2",
	"gitlfs": "git-lfs", "huggingface": "huggingface", "cocoapods": "cocoapods", "composer": "composer",
}

// NewNexus validates the base URL and builds a client. Redirects to another host
// are refused so credentials cannot be forwarded.
func NewNexus(cfg NexusConfig) (*Nexus, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: invalid Nexus base URL", ErrInput)
	}
	c := migrationClient(u, cfg.HTTPClient, cfg.RootCAs)
	cfg.RootCAs = nil
	cfg.HTTPClient = nil
	return &Nexus{base: u, cfg: cfg, http: c}, nil
}

func (n *Nexus) Name() string { return "nexus" }

func (n *Nexus) AntFormat(f string) (string, bool) { v, ok := nexusFormats[f]; return v, ok }

func (n *Nexus) do(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != n.base.Host || u.Scheme != n.base.Scheme {
		return nil, fmt.Errorf("%w: refusing to send credentials to a different origin", ErrInput)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case n.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.cfg.Token)
	case n.cfg.Username != "":
		req.SetBasicAuth(n.cfg.Username, n.cfg.Password)
	}
	req.Header.Set("Accept", "application/json, */*")
	return n.http.Do(req)
}

func (n *Nexus) api(path string, q url.Values) string {
	u := *n.base
	u.Path = strings.TrimRight(u.Path, "/") + "/service/rest/v1/" + path
	u.RawQuery = q.Encode()
	return u.String()
}

// getJSON decodes a bounded JSON response. A non-2xx status is an error that
// contains the status only, never the body.
func (n *Nexus) getJSON(ctx context.Context, rawURL string, out any) (int, error) {
	resp, err := n.do(ctx, rawURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("nexus returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

type nexusRepo struct {
	Name       string `json:"name"`
	Format     string `json:"format"`
	Type       string `json:"type"`
	Attributes struct {
		Proxy struct {
			RemoteURL string `json:"remoteUrl"`
		} `json:"proxy"`
	} `json:"attributes"`
}

type nexusRepoDetail struct {
	Group struct {
		MemberNames    []string `json:"memberNames"`
		WritableMember string   `json:"writableMember"`
	} `json:"group"`
	Proxy struct {
		RemoteURL      string `json:"remoteUrl"`
		MetadataMaxAge int64  `json:"metadataMaxAge"` // minutes
	} `json:"proxy"`
	HTTPClient struct {
		Authentication *struct {
			Type string `json:"type"`
		} `json:"authentication"`
	} `json:"httpClient"`
	Cleanup struct {
		PolicyNames []string `json:"policyNames"`
	} `json:"cleanup"`
}

func (n *Nexus) Repositories(ctx context.Context) ([]SourceRepo, error) {
	var list []nexusRepo
	if _, err := n.getJSON(ctx, n.api("repositories", nil), &list); err != nil {
		return nil, err
	}
	out := make([]SourceRepo, 0, len(list))
	for _, r := range list {
		sr := SourceRepo{Name: r.Name, Format: r.Format, Type: r.Type, RemoteURL: r.Attributes.Proxy.RemoteURL}
		// Group members, cleanup and proxy cache settings are only in the
		// per-repository representation.
		fmtPath := r.Format
		if fmtPath == "maven2" {
			fmtPath = "maven"
		}
		var d nexusRepoDetail
		if _, err := n.getJSON(ctx, n.api("repositories/"+url.PathEscape(fmtPath)+"/"+url.PathEscape(r.Type)+"/"+url.PathEscape(r.Name), nil), &d); err == nil {
			sr.Members = d.Group.MemberNames
			sr.WritableMember = d.Group.WritableMember
			if d.Proxy.RemoteURL != "" {
				sr.RemoteURL = d.Proxy.RemoteURL
			}
			sr.MetadataMaxAge = d.Proxy.MetadataMaxAge * 60
			sr.Authenticated = d.HTTPClient.Authentication != nil && d.HTTPClient.Authentication.Type != ""
			sr.CleanupPolicies = d.Cleanup.PolicyNames
		}
		out = append(out, sr)
	}
	return out, nil
}

type nexusCleanup struct {
	Name                    string `json:"name"`
	CriteriaLastBlobUpdated *int   `json:"criteriaLastBlobUpdated"`
	CriteriaLastDownloaded  *int   `json:"criteriaLastDownloaded"`
	CriteriaReleaseType     string `json:"criteriaReleaseType"`
	CriteriaAssetRegex      string `json:"criteriaAssetRegex"`
	Retain                  *int   `json:"retain"`
}

func (n *Nexus) CleanupPolicies(ctx context.Context) ([]CleanupPolicy, error) {
	var list []nexusCleanup
	if _, err := n.getJSON(ctx, n.api("cleanup-policies", nil), &list); err != nil {
		return nil, err
	}
	out := make([]CleanupPolicy, 0, len(list))
	for _, c := range list {
		p := CleanupPolicy{Name: c.Name, MaxAgeDays: c.CriteriaLastBlobUpdated, MaxIdleDays: c.CriteriaLastDownloaded, Retain: c.Retain, ReleaseType: c.CriteriaReleaseType}
		if c.CriteriaAssetRegex != "" {
			p.UnmappedNote = append(p.UnmappedNote, "asset name regex criterion is not supported")
		}
		if c.CriteriaReleaseType == "PRERELEASES" {
			p.UnmappedNote = append(p.UnmappedNote, "prerelease-only criterion is not supported")
		}
		out = append(out, p)
	}
	return out, nil
}

type nexusAssets struct {
	Items []struct {
		Path        string `json:"path"`
		DownloadURL string `json:"downloadUrl"`
		ContentType string `json:"contentType"`
		FileSize    *int64 `json:"fileSize"`
		Checksum    struct {
			SHA256 string `json:"sha256"`
		} `json:"checksum"`
	} `json:"items"`
	ContinuationToken *string `json:"continuationToken"`
}

func (n *Nexus) Assets(ctx context.Context, repo string, fn func(SourceAsset) error) error {
	token := ""
	for page := 0; page < 100000; page++ {
		q := url.Values{"repository": {repo}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var res nexusAssets
		if _, err := n.getJSON(ctx, n.api("assets", q), &res); err != nil {
			return err
		}
		for _, it := range res.Items {
			a := SourceAsset{Repository: repo, Path: it.Path, DownloadURL: it.DownloadURL, SHA256: strings.ToLower(it.Checksum.SHA256), Size: -1, ContentType: it.ContentType}
			if it.FileSize != nil {
				a.Size = *it.FileSize
			}
			if err := fn(a); err != nil {
				return err
			}
		}
		if res.ContinuationToken == nil || *res.ContinuationToken == "" {
			return nil
		}
		token = *res.ContinuationToken
	}
	return fmt.Errorf("nexus asset listing did not terminate")
}

func (n *Nexus) Open(ctx context.Context, a SourceAsset) (io.ReadCloser, error) {
	raw := a.DownloadURL
	if u, err := url.Parse(raw); raw != "" && (err != nil || u.Host != n.base.Host || u.Scheme != n.base.Scheme) {
		// Nexus advertises download URLs built from its configured base URL,
		// which may differ from the address used here. Re-root on our base.
		raw = ""
		if err == nil {
			if i := strings.Index(u.EscapedPath(), "/repository/"); i >= 0 {
				r := *n.base
				r.Path = strings.TrimRight(n.base.Path, "/") + mustUnescape(u.EscapedPath()[i:])
				r.RawPath = strings.TrimRight(n.base.EscapedPath(), "/") + u.EscapedPath()[i:]
				raw = r.String()
			}
		}
	}
	if raw == "" {
		u := *n.base
		segs := strings.Split(a.Path, "/")
		for i := range segs {
			segs[i] = url.PathEscape(segs[i])
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/repository/" + url.PathEscape(a.Repository) + "/" + strings.Join(segs, "/")
		u.RawPath = u.EscapedPath()
		raw = u.String()
	}
	resp, err := n.do(ctx, raw)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, fmt.Errorf("nexus returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func mustUnescape(p string) string {
	if u, err := url.PathUnescape(p); err == nil {
		return u
	}
	return p
}
