package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Federation configures the dedicated federation listener (Phase 2, D1). The
// listener binds only when Listen is set; federation stays inert until an
// administrator founds or joins a federation.
type Federation struct {
	// Listen is the mTLS listener address (ANT_FEDERATION_LISTEN), e.g. :8444.
	Listen string
	// PublicURL is the https URL other members use to reach the listener
	// (ANT_FEDERATION_PUBLIC_URL); required to found or join.
	PublicURL string
	// JoinDir optionally holds authority_url, ca_sha256 and invitation files
	// (ANT_FEDERATION_JOIN_DIR, typically a mounted Secret). When present and
	// the instance is not yet federated, it joins automatically at startup.
	JoinDir string
}

func loadFederation(getenv func(string) string) (Federation, error) {
	f := Federation{Listen: getenv("ANT_FEDERATION_LISTEN"), PublicURL: getenv("ANT_FEDERATION_PUBLIC_URL"), JoinDir: getenv("ANT_FEDERATION_JOIN_DIR")}
	if f.Listen != "" {
		_, port, err := net.SplitHostPort(f.Listen)
		n, perr := strconv.Atoi(port)
		if err != nil || perr != nil || n < 0 || n > 65535 {
			return Federation{}, errors.New("ANT_FEDERATION_LISTEN must be host:port")
		}
	}
	if f.PublicURL != "" {
		u, err := url.Parse(f.PublicURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return Federation{}, errors.New("ANT_FEDERATION_PUBLIC_URL must be an https origin (scheme, host and optional port only)")
		}
		f.PublicURL = strings.TrimSuffix(f.PublicURL, "/")
		if f.Listen == "" {
			return Federation{}, errors.New("ANT_FEDERATION_PUBLIC_URL requires ANT_FEDERATION_LISTEN")
		}
	}
	if f.JoinDir != "" {
		info, err := os.Stat(f.JoinDir)
		if err != nil || !info.IsDir() || !filepath.IsAbs(f.JoinDir) {
			return Federation{}, errors.New("ANT_FEDERATION_JOIN_DIR must be an absolute directory")
		}
		if f.PublicURL == "" {
			return Federation{}, errors.New("ANT_FEDERATION_JOIN_DIR requires ANT_FEDERATION_PUBLIC_URL")
		}
	}
	return f, nil
}
