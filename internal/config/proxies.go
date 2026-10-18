package config

// trustedProxyList returns ANT_TRUSTED_PROXIES, falling back to the earlier
// name ANT_TRUSTED_PROXY_CIDRS so existing deployments keep working.
func trustedProxyList(getenv func(string) string) string {
	if v := getenv("ANT_TRUSTED_PROXIES"); v != "" {
		return v
	}
	return getenv("ANT_TRUSTED_PROXY_CIDRS")
}
