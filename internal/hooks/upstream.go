package hooks

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Change is what an upstream notification says changed: which repository or
// package, optionally which tag or version, and which hosts the notifying
// registry is known by. It carries no credentials and nothing from the payload
// beyond identifiers.
type Change struct {
	Provider string
	Event    string
	// Names are candidate identifiers of the package or repository, most
	// specific first. They are matched as substrings of cached metadata paths.
	Names   []string
	Version string
	// Kind is "image" for container registries and "package" otherwise.
	Kind string
	// Origins are the registry hostnames the provider implies (Docker Hub, GHCR,
	// the npm registry) or the payload names (GitLab project URL). Empty for
	// Artifactory and Nexus, whose sources must be named in the action.
	Origins []string
	// Ignored is set when the event carries no change worth acting on.
	Ignored string
}

// ErrNotRecognized means the payload matches no supported provider.
var ErrNotRecognized = errors.New("payload is not a recognised upstream notification")

var (
	dockerHubHosts = []string{"registry-1.docker.io", "index.docker.io", "docker.io", "registry.hub.docker.com", "hub.docker.com"}
	githubHosts    = []string{"github.com", "api.github.com", "codeload.github.com"}
	npmHosts       = []string{"registry.npmjs.org", "registry.npmjs.com"}
	githubPackages = map[string]string{
		"container": "ghcr.io", "docker": "ghcr.io",
		"npm": "npm.pkg.github.com", "maven": "maven.pkg.github.com",
		"nuget": "nuget.pkg.github.com", "rubygems": "rubygems.pkg.github.com",
	}
)

func get(doc any, keys ...string) any {
	cur := doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func str(doc any, keys ...string) string {
	s, _ := get(doc, keys...).(string)
	return strings.TrimSpace(s)
}

// DetectProvider names the provider a payload comes from, or "".
func DetectProvider(h http.Header, doc any) string {
	switch {
	case h.Get("X-GitHub-Event") != "":
		return ProviderGitHub
	case h.Get("X-Gitlab-Event") != "":
		return ProviderGitLab
	case h.Get("X-Nexus-Webhook-ID") != "":
		return ProviderNexus
	case get(doc, "push_data") != nil && str(doc, "repository", "repo_name") != "":
		return ProviderDockerHub
	case strings.HasPrefix(str(doc, "event"), "package:") || strings.HasPrefix(str(doc, "event"), "scope:") || strings.HasPrefix(str(doc, "event"), "owner:"):
		return ProviderNPM
	case str(doc, "domain") != "" && str(doc, "event_type") != "" && get(doc, "data") != nil:
		return ProviderArtifactory
	case str(doc, "repositoryName") != "" && (get(doc, "component") != nil || get(doc, "asset") != nil):
		return ProviderNexus
	}
	return ""
}

// ParseUpstream turns a provider notification into a Change. provider may be
// ProviderAuto. Unknown shapes return ErrNotRecognized; a recognised but
// irrelevant event (a GitHub ping, an npm star) returns a Change with Ignored set.
func ParseUpstream(provider string, h http.Header, body []byte) (Change, error) {
	doc := decodePayload(body)
	if _, ok := doc.(map[string]any); !ok {
		return Change{}, ErrNotRecognized
	}
	if provider == "" || provider == ProviderAuto {
		provider = DetectProvider(h, doc)
	}
	switch provider {
	case ProviderDockerHub:
		return parseDockerHub(doc)
	case ProviderGitHub:
		return parseGitHub(h, doc)
	case ProviderGitLab:
		return parseGitLab(h, doc)
	case ProviderNPM:
		return parseNPM(doc)
	case ProviderArtifactory:
		return parseArtifactory(doc)
	case ProviderNexus:
		return parseNexus(h, doc)
	}
	return Change{}, ErrNotRecognized
}

func parseDockerHub(doc any) (Change, error) {
	repo := str(doc, "repository", "repo_name")
	if repo == "" {
		return Change{}, ErrNotRecognized
	}
	names := []string{repo}
	if !strings.Contains(repo, "/") {
		names = append(names, "library/"+repo)
	}
	return Change{Provider: ProviderDockerHub, Event: "push", Names: names, Version: str(doc, "push_data", "tag"), Kind: "image", Origins: dockerHubHosts}, nil
}

func parseGitHub(h http.Header, doc any) (Change, error) {
	event := h.Get("X-GitHub-Event")
	c := Change{Provider: ProviderGitHub, Event: event, Kind: "package"}
	switch event {
	case "ping":
		c.Ignored = "ping"
	case "release":
		c.Names = []string{str(doc, "repository", "full_name")}
		c.Version = str(doc, "release", "tag_name")
		c.Origins = githubHosts
		if c.Names[0] == "" || c.Version == "" {
			return Change{}, ErrNotRecognized
		}
	case "package", "registry_package":
		key := event
		kind := strings.ToLower(str(doc, key, "package_type"))
		name := str(doc, key, "name")
		owner := strings.ToLower(str(doc, key, "namespace"))
		if name == "" {
			return Change{}, ErrNotRecognized
		}
		c.Version = str(doc, key, "package_version", "version")
		if kind == "container" || kind == "docker" {
			c.Kind = "image"
			full := strings.ToLower(name)
			if owner != "" {
				full = owner + "/" + full
			}
			c.Names = []string{full}
			if tag := str(doc, key, "package_version", "container_metadata", "tag", "name"); tag != "" {
				c.Version = tag
			}
			c.Origins = []string{"ghcr.io"}
			return c, nil
		}
		c.Names = []string{name}
		if owner != "" {
			c.Names = append(c.Names, "@"+owner+"/"+name)
		}
		if host, ok := githubPackages[kind]; ok {
			c.Origins = []string{host}
		} else {
			for _, host := range githubPackages {
				c.Origins = append(c.Origins, host)
			}
		}
	default:
		c.Ignored = "unsupported_event"
	}
	return c, nil
}

func parseGitLab(h http.Header, doc any) (Change, error) {
	event := h.Get("X-Gitlab-Event")
	c := Change{Provider: ProviderGitLab, Event: event, Kind: "package"}
	project := str(doc, "project", "path_with_namespace")
	if project == "" {
		return Change{}, ErrNotRecognized
	}
	c.Names = []string{project}
	host := "gitlab.com"
	if u, err := url.Parse(str(doc, "project", "web_url")); err == nil && u.Hostname() != "" {
		host = strings.ToLower(u.Hostname())
	}
	c.Origins = []string{host}
	if host == "gitlab.com" {
		c.Origins = append(c.Origins, "registry.gitlab.com")
	}
	switch kind := str(doc, "object_kind"); {
	case kind == "release" || event == "Release Hook":
		c.Version = str(doc, "tag")
	case kind == "tag_push" || event == "Tag Push Hook":
		c.Version = strings.TrimPrefix(str(doc, "ref"), "refs/tags/")
	case kind == "push" || event == "Push Hook":
		c.Version = strings.TrimPrefix(str(doc, "ref"), "refs/heads/")
	default:
		c.Ignored = "unsupported_event"
	}
	return c, nil
}

func parseNPM(doc any) (Change, error) {
	event := str(doc, "event")
	if !strings.HasPrefix(event, "package:") {
		return Change{Provider: ProviderNPM, Event: event, Kind: "package", Ignored: "unsupported_event"}, nil
	}
	if event == "package:star" || event == "package:unstar" {
		return Change{Provider: ProviderNPM, Event: event, Kind: "package", Ignored: "unsupported_event"}, nil
	}
	name := str(doc, "name")
	if name == "" {
		name = str(doc, "payload", "name")
	}
	if name == "" {
		return Change{}, ErrNotRecognized
	}
	version := str(doc, "version")
	if version == "" {
		version = str(doc, "payload", "dist-tags", "latest")
	}
	names := []string{name}
	if strings.HasPrefix(name, "@") {
		names = append(names, strings.Replace(name, "/", "%2f", 1))
	}
	return Change{Provider: ProviderNPM, Event: event, Names: names, Version: version, Kind: "package", Origins: npmHosts}, nil
}

func parseArtifactory(doc any) (Change, error) {
	domain, event := str(doc, "domain"), str(doc, "event_type")
	if domain == "" || event == "" {
		return Change{}, ErrNotRecognized
	}
	c := Change{Provider: ProviderArtifactory, Event: domain + ":" + event, Kind: "package"}
	switch domain {
	case "docker":
		c.Kind = "image"
		c.Names = nonEmpty(str(doc, "data", "image_name"))
		c.Version = str(doc, "data", "tag")
		if len(c.Names) == 0 {
			c.Names = nonEmpty(strings.TrimSuffix(str(doc, "data", "path"), "/"+c.Version))
		}
	case "artifact", "artifactory_release_bundle":
		path := strings.Trim(str(doc, "data", "path"), "/")
		dir := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			dir = path[:i]
		}
		parent := dir
		if i := strings.LastIndex(dir, "/"); i >= 0 {
			parent = dir[:i]
			c.Version = dir[i+1:]
		}
		c.Names = nonEmpty(parent, dir, str(doc, "data", "name"))
	default:
		c.Ignored = "unsupported_event"
		return c, nil
	}
	if len(c.Names) == 0 {
		return Change{}, ErrNotRecognized
	}
	return c, nil
}

func parseNexus(h http.Header, doc any) (Change, error) {
	repo := str(doc, "repositoryName")
	if repo == "" {
		return Change{}, ErrNotRecognized
	}
	c := Change{Provider: ProviderNexus, Event: h.Get("X-Nexus-Webhook-ID") + ":" + str(doc, "action"), Kind: "package"}
	if comp := get(doc, "component"); comp != nil {
		group, name := str(comp, "group"), str(comp, "name")
		c.Version = str(comp, "version")
		if name == "" {
			return Change{}, ErrNotRecognized
		}
		switch {
		case group != "":
			c.Names = nonEmpty(group+":"+name, strings.ReplaceAll(group, ".", "/")+"/"+name, group+"/"+name, name)
		default:
			c.Names = nonEmpty(name)
		}
		return c, nil
	}
	asset := str(doc, "asset", "name")
	if asset == "" {
		return Change{}, ErrNotRecognized
	}
	asset = strings.Trim(asset, "/")
	if i := strings.LastIndex(asset, "/"); i >= 0 {
		c.Names = nonEmpty(asset[:i], asset)
	} else {
		c.Names = nonEmpty(asset)
	}
	return c, nil
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
