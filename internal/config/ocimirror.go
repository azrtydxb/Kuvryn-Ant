package config

import (
	"errors"
	"regexp"
	"strings"
)

// OCIMirror configures serving OCI feeds without the feed slug prefix so Ant
// can act as a pull-through mirror (dockerd registry-mirrors, buildkit,
// containerd registries.yaml).
//
//	ANT_OCI_DEFAULT_FEED  feed slug served for unprefixed /v2/<name>/... paths
//	ANT_OCI_MIRROR_MAP    comma list host=slug, e.g. docker.io=dockerhub,ghcr.io=ghcr;
//	                      selects the feed from containerd's ?ns= parameter
type OCIMirror struct {
	DefaultFeed string
	Namespaces  map[string]string
}

var (
	mirrorSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	mirrorHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
)

func loadOCIMirror(getenv func(string) string) (OCIMirror, error) {
	m := OCIMirror{DefaultFeed: strings.TrimSpace(getenv("ANT_OCI_DEFAULT_FEED"))}
	if m.DefaultFeed != "" && !mirrorSlug.MatchString(m.DefaultFeed) {
		return OCIMirror{}, errors.New("ANT_OCI_DEFAULT_FEED must be a feed slug")
	}
	if raw := strings.TrimSpace(getenv("ANT_OCI_MIRROR_MAP")); raw != "" {
		m.Namespaces = map[string]string{}
		for _, item := range strings.Split(raw, ",") {
			host, slug, ok := strings.Cut(strings.TrimSpace(item), "=")
			host, slug = strings.ToLower(strings.TrimSpace(host)), strings.TrimSpace(slug)
			if !ok || !mirrorHost.MatchString(host) || !mirrorSlug.MatchString(slug) {
				return OCIMirror{}, errors.New("ANT_OCI_MIRROR_MAP must be host=feed-slug pairs separated by commas")
			}
			if _, dup := m.Namespaces[host]; dup || len(m.Namespaces) >= 64 {
				return OCIMirror{}, errors.New("ANT_OCI_MIRROR_MAP has a duplicate host or too many entries")
			}
			m.Namespaces[host] = slug
		}
	}
	return m, nil
}
