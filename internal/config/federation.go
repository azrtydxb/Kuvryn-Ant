package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// Routing abuse limits (ANT_FEDERATION_ROUTE_*); zero keeps the built-in
	// defaults in internal/federation/route_limits.go.
	RouteMaxStreams, RouteStreamsPerPrincipal, RouteStreamsPerClient, RoutePrincipalRate int
	RouteMinTransferRate                                                                 int64
	RouteTransferGrace, RouteTransferTimeout                                             time.Duration
}

// routeInt parses an optional non-negative integer variable.
func routeInt(getenv func(string) string, name string, max int64) (int64, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > max {
		return 0, fmt.Errorf("%s must be an integer between 0 and %d (0 keeps the default)", name, max)
	}
	return n, nil
}

func routeDuration(getenv func(string) string, name string) (time.Duration, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration such as 30s (0 keeps the default)", name)
	}
	return d, nil
}

func loadFederationRoute(getenv func(string) string, f *Federation) error {
	var err error
	ints := []struct {
		name string
		max  int64
		set  func(int64)
	}{
		{"ANT_FEDERATION_ROUTE_MAX_STREAMS", 1 << 16, func(n int64) { f.RouteMaxStreams = int(n) }},
		{"ANT_FEDERATION_ROUTE_STREAMS_PER_PRINCIPAL", 1 << 16, func(n int64) { f.RouteStreamsPerPrincipal = int(n) }},
		{"ANT_FEDERATION_ROUTE_STREAMS_PER_CLIENT", 1 << 16, func(n int64) { f.RouteStreamsPerClient = int(n) }},
		{"ANT_FEDERATION_ROUTE_PRINCIPAL_RATE", 1 << 20, func(n int64) { f.RoutePrincipalRate = int(n) }},
		{"ANT_FEDERATION_ROUTE_MIN_TRANSFER_RATE", 1 << 40, func(n int64) { f.RouteMinTransferRate = n }},
	}
	for _, i := range ints {
		n, e := routeInt(getenv, i.name, i.max)
		if e != nil {
			return e
		}
		i.set(n)
	}
	if f.RouteTransferGrace, err = routeDuration(getenv, "ANT_FEDERATION_ROUTE_TRANSFER_GRACE"); err != nil {
		return err
	}
	f.RouteTransferTimeout, err = routeDuration(getenv, "ANT_FEDERATION_ROUTE_TRANSFER_TIMEOUT")
	return err
}

func loadFederation(getenv func(string) string) (Federation, error) {
	f := Federation{Listen: getenv("ANT_FEDERATION_LISTEN"), PublicURL: getenv("ANT_FEDERATION_PUBLIC_URL"), JoinDir: getenv("ANT_FEDERATION_JOIN_DIR")}
	if err := loadFederationRoute(getenv, &f); err != nil {
		return Federation{}, err
	}
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
