package config

import (
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
)

// RedactedMarker replaces every secret value in diagnostic output.
const RedactedMarker = "[REDACTED]"

// Redacted returns the effective configuration for diagnostics. It is an
// allowlist: only non-secret scalar settings are copied. Secret-bearing values
// (database password, root key, credentials) are replaced by RedactedMarker or
// reduced to whether they are set. File paths are reported, never file contents.
func (c Config) Redacted() map[string]any {
	db := map[string]any{}
	if c.Database != nil && c.Database.ConnConfig != nil {
		cc := c.Database.ConnConfig
		db["host"] = cc.Host
		db["port"] = cc.Port
		db["database"] = cc.Database
		db["user"] = cc.User
		db["password"] = RedactedMarker
		db["tls"] = cc.TLSConfig != nil
		db["max_conns"] = c.Database.MaxConns
	}
	st := c.Storage
	backend := st.Backend
	if backend == "" {
		backend = "disk"
	}
	storage := map[string]any{"backend": backend, "directory": st.Directory}
	if backend == "s3" {
		storage["endpoint"] = st.Endpoint
		storage["region"] = st.Region
		storage["bucket"] = st.Bucket
		storage["prefix"] = st.Prefix
		storage["path_style"] = st.PathStyle
		storage["allow_insecure_http"] = st.AllowInsecureHTTP
		storage["credentials_file"] = st.CredentialsFile
		storage["credentials"] = RedactedMarker
		storage["ca_file"] = st.CAFile
	}
	cas := make([]string, 0, len(c.UpstreamCAs))
	for ref := range c.UpstreamCAs {
		cas = append(cas, ref)
	}
	sort.Strings(cas)
	return map[string]any{
		"listen":           c.Listen,
		"public_url":       c.PublicURL,
		"instance_id":      c.InstanceID,
		"database":         db,
		"storage":          storage,
		"root_key":         RedactedMarker,
		"shutdown_timeout": c.ShutdownTimeout.String(),
		"max_requests":     c.MaxRequests,
		"upload_slots":     c.UploadSlots,
		"upstream_cas":     cas,
		"oci_mirror_feed":  c.OCIMirror.DefaultFeed,
		"retention": map[string]any{
			"interval": c.Retention.Interval.String(), "grace": c.Retention.Grace.String(), "dry_run": c.Retention.DryRun,
			"batch_size": c.Retention.BatchSize, "integrity_batch": c.Retention.IntegrityBatch, "integrity_max_bytes": c.Retention.IntegrityMaxBytes,
		},
		"audit_retention": map[string]any{
			"retain": c.AuditRetention.Retain.String(), "purge_interval": c.AuditRetention.Interval.String(), "batch_size": c.AuditRetention.BatchSize,
		},
		"tracing": map[string]any{
			"enabled": c.Tracing.Enabled, "endpoint": c.Tracing.Endpoint, "protocol": c.Tracing.Protocol,
			"service_name": c.Tracing.ServiceName, "sample_ratio": strconv.FormatFloat(c.Tracing.SampleRatio, 'g', -1, 64),
		},
	}
}

// SecretValues lists literal secret strings (and common encodings) that must
// never appear in diagnostic output, for defense in depth scrubbing of logs.
func (c Config) SecretValues() []string {
	var out []string
	if c.Database != nil && c.Database.ConnConfig != nil && c.Database.ConnConfig.Password != "" {
		out = append(out, c.Database.ConnConfig.Password)
	}
	if len(c.RootKey) > 0 {
		out = append(out, string(c.RootKey), hex.EncodeToString(c.RootKey), base64.StdEncoding.EncodeToString(c.RootKey), base64.RawStdEncoding.EncodeToString(c.RootKey), base64.URLEncoding.EncodeToString(c.RootKey), base64.RawURLEncoding.EncodeToString(c.RootKey))
	}
	return out
}
