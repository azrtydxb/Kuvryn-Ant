package main

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// directoryVerifier adapts the configured LDAP/AD provider to the package
// protocols' basic-directory method. Authorization still comes only from the
// existing explicit identity bindings and role memberships.
type directoryVerifier struct {
	provider ldapBrowserProvider
	issuer   string
}

func (d directoryVerifier) VerifyPassword(ctx context.Context, username string, password []byte) (string, string, error) {
	id, err := d.provider.Authenticate(ctx, username, password)
	switch {
	case err == nil && id.ProviderID == d.issuer && id.Subject != "":
		return id.ProviderID, id.Subject, nil
	case errors.Is(err, directory.ErrUnavailable), errors.Is(err, directory.ErrGroups):
		return "", "", identity.ErrUnavailable
	default:
		return "", "", identity.ErrUnauthenticated
	}
}

// trustedProxies parses ANT_TRUSTED_PROXY_CIDRS (comma separated). Without it
// only the TCP peer is used for anonymous network restrictions.
func trustedProxies() []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(os.Getenv("ANT_TRUSTED_PROXY_CIDRS"), ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// newClientAuth builds the shared package-protocol authentication middleware.
// It returns nil when this process does not serve package protocols.
func newClientAuth(pool *pgxpool.Pool, artifacts *artifactRuntime, browser *browserRuntime) (*identity.ClientAuth, *identity.GitHubOIDC) {
	if artifacts == nil || pool == nil {
		return nil, nil
	}
	github := &identity.GitHubOIDC{DB: pool, InstanceID: artifacts.instanceID}
	auth := &identity.ClientAuth{DB: pool, InstanceID: artifacts.instanceID, GitHub: github, ResolveOCI: artifacts.registry.ClientAuthTarget, TrustedProxies: trustedProxies()}
	if browser != nil && browser.enterprise != nil && browser.enterprise.ldap != nil {
		auth.Directory = directoryVerifier{provider: browser.enterprise.ldap, issuer: browser.enterprise.ldapIssuer}
	}
	return auth, github
}
