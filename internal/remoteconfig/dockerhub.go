package remoteconfig

import (
	"net/url"
	"strings"
)

// IsDockerHub reports whether an origin is Docker Hub's registry endpoint.
// Docker Hub stores official images under the library/ namespace, which clients
// omit ("alpine" means "library/alpine").
func IsDockerHub(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "registry-1.docker.io", "docker.io", "index.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

// DockerHubDisplayName shows an official image without its library/ prefix.
func DockerHubDisplayName(coordinate string) string {
	if rest, ok := strings.CutPrefix(coordinate, "library/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return rest
	}
	return coordinate
}
