package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/jackc/pgx/v5/pgxpool"
)

type browserConfig struct {
	Origin           string   `json:"origin"`
	Key              string   `json:"key"`
	Issuer           string   `json:"issuer"`
	ClientID         string   `json:"client_id"`
	ClientSecretFile string   `json:"client_secret_file"`
	RedirectURI      string   `json:"redirect_uri"`
	AllowedEndpoints []string `json:"allowed_endpoints"`
}
type browserRuntime struct {
	client     *oidclogin.Client
	enterprise *enterpriseLogin
}

var errBrowserConfig = errors.New("invalid browser authentication configuration")

// Configuration is operator-mounted and never accepts request-derived URLs or
// inline client secrets. Errors intentionally omit configuration/file contents.
func readBrowserConfig(path string) (browserConfig, string, error) {
	var cfg browserConfig
	data, err := readBrowserFile(path)
	if err != nil {
		return cfg, "", errBrowserConfig
	}
	// Decode exact keys and reject duplicates, including case aliases that the
	// standard struct decoder otherwise accepts.
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return cfg, "", errBrowserConfig
	}
	fields := map[string]json.RawMessage{}
	allowed := map[string]bool{"origin": true, "key": true, "issuer": true, "client_id": true, "client_secret_file": true, "redirect_uri": true, "allowed_endpoints": true}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return cfg, "", errBrowserConfig
		}
		name, ok := key.(string)
		if !ok || !allowed[name] || fields[name] != nil {
			return cfg, "", errBrowserConfig
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return cfg, "", errBrowserConfig
		}
		fields[name] = value
	}
	if _, err = decoder.Token(); err != nil {
		return cfg, "", errBrowserConfig
	}
	if decoder.Decode(new(any)) != io.EOF {
		return cfg, "", errBrowserConfig
	}
	if json.Unmarshal(data, &cfg) != nil {
		return cfg, "", errBrowserConfig
	}
	origin, err := url.Parse(cfg.Origin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || cfg.Origin != "https://"+origin.Host {
		return cfg, "", errBrowserConfig
	}
	if cfg.RedirectURI != cfg.Origin+"/auth/oidc/callback" || cfg.Key == "" || cfg.ClientID == "" || cfg.Issuer == "" || len(cfg.AllowedEndpoints) == 0 || len(cfg.AllowedEndpoints) > 32 {
		return cfg, "", errBrowserConfig
	}
	secret := ""
	if cfg.ClientSecretFile != "" {
		raw, err := readBrowserFile(cfg.ClientSecretFile)
		if err != nil {
			return cfg, "", errBrowserConfig
		}
		secret = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
			return cfg, "", errBrowserConfig
		}
	}
	return cfg, secret, nil
}
func readBrowserFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errBrowserConfig
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 64<<10 {
		return nil, errBrowserConfig
	}
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, errBrowserConfig
	}
	return data, nil
}
func openBrowser(ctx context.Context, pool *pgxpool.Pool, runtimeConfig config.Config, instanceID string) (*browserRuntime, error) {
	path := os.Getenv("ANT_OIDC_CONFIG_FILE")
	samlPath, ldapPath := os.Getenv("ANT_SAML_CONFIG_FILE"), os.Getenv("ANT_LDAP_CONFIG_FILE")
	if path == "" && samlPath == "" && ldapPath == "" {
		return nil, nil
	}
	if pool == nil || instanceID == "" {
		return nil, errBrowserConfig
	}
	var origin string
	var provider *oidclogin.Provider
	if path != "" {
		cfg, secret, err := readBrowserConfig(path)
		if err != nil {
			return nil, err
		}
		origin = cfg.Origin
		provider, err = oidclogin.NewProvider(ctx, oidclogin.ProviderConfig{Key: cfg.Key, Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: secret, RedirectURI: cfg.RedirectURI, AllowedEndpoints: cfg.AllowedEndpoints})
		if err != nil {
			return nil, errors.New("cannot initialize browser authentication provider")
		}
	}
	enterprise, origin, err := openEnterprise(pool, instanceID, origin, samlPath, ldapPath)
	if err != nil {
		return nil, err
	}
	if runtimeConfig.PublicURL != "" {
		public, ok := browserOrigin(runtimeConfig.PublicURL)
		configured, configuredOK := browserOrigin(origin)
		if !ok || !configuredOK || public != configured {
			return nil, errBrowserConfig
		}
	}
	client := &oidclogin.Client{Provider: provider, Sessions: oidclogin.Sessions{DB: pool, InstanceID: instanceID}, Origin: origin, SuccessRedirect: "/console/"}
	enterprise.client = client
	return &browserRuntime{client: client, enterprise: enterprise}, nil
}
func (b *browserRuntime) Register(mux *http.ServeMux) {
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "authentication route not found")
	})
	mux.HandleFunc("GET /auth/providers", func(w http.ResponseWriter, r *http.Request) {
		providers := make([]string, 0, 3)
		if b != nil {
			if b.client.Provider != nil {
				providers = append(providers, "oidc")
			}
			if b.enterprise != nil && b.enterprise.saml != nil {
				providers = append(providers, "saml")
			}
			if b.enterprise != nil && b.enterprise.ldap != nil {
				providers = append(providers, "ldap")
			}
		}
		browserNoStore(w)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": providers})
	})
	if b == nil {
		return
	}
	if b.client.Provider != nil {
		mux.HandleFunc("/auth/oidc/login", b.client.Login)
		mux.HandleFunc("/auth/oidc/callback", b.client.Callback)
	}
	b.enterprise.Register(mux)
	mux.HandleFunc("/auth/session", b.client.SessionInfo)
	mux.HandleFunc("/auth/logout", b.client.Logout)
}
func (b *browserRuntime) Middleware(next http.Handler) http.Handler {
	if b == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && len(r.Header.Values("Authorization")) == 0 {
			session, err := b.client.AuthenticateRequest(r)
			if err == nil && session.InstanceID == b.client.Sessions.InstanceID && session.PrincipalID != "" {
				r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{InstanceID: session.InstanceID, PrincipalID: session.PrincipalID}))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// browserOrigin accepts a trusted HTTPS origin with an optional terminal slash,
// normalizes DNS case and the default port, and rejects paths/userinfo/queries.
func browserOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if host == "" {
		return "", false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, true
}

// Cleanup performs one bounded pass; invoke periodically (e.g. once per minute).
// Authentication rejects expiry independently of cleanup, including during outages.
func (b *browserRuntime) Cleanup(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if err := b.client.Sessions.Cleanup(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, query := range []string{
		`DELETE FROM enterprise_login_states WHERE ctid IN (SELECT ctid FROM enterprise_login_states WHERE instance_id=$1 AND expires_at<=clock_timestamp() LIMIT 500 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM saml_replay_claims WHERE ctid IN (SELECT ctid FROM saml_replay_claims WHERE instance_id=$1 AND expires_at<=clock_timestamp() LIMIT 500 FOR UPDATE SKIP LOCKED)`,
	} {
		if _, err := b.client.Sessions.DB.Exec(ctx, query, b.client.Sessions.InstanceID); err != nil {
			return oidclogin.ErrUnavailable
		}
	}
	return nil
}
