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
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
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
	// dyn is non-nil when database-stored providers, hot reload and the local
	// break-glass login are available. Without it the static fields apply.
	dyn *dynamicProviders
}

// cur returns the providers being served now: the hot-reloadable snapshot when
// present, otherwise the static startup configuration.
func (b *browserRuntime) cur() (*oidclogin.Client, *enterpriseLogin) {
	if b.dyn != nil {
		if s := b.dyn.state(); s != nil {
			return s.client, s.enterprise
		}
	}
	return b.client, b.enterprise
}

// Reload rebuilds providers from configuration without a restart.
func (b *browserRuntime) Reload(ctx context.Context) error {
	if b == nil || b.dyn == nil {
		return nil
	}
	return b.dyn.Reload(ctx)
}

func (b *browserRuntime) Active() []management.ActiveProvider {
	if b == nil || b.dyn == nil {
		return []management.ActiveProvider{}
	}
	return b.dyn.Active()
}

func (b *browserRuntime) FileProviders() []management.FileProvider {
	if b == nil || b.dyn == nil {
		return nil
	}
	return b.dyn.files.summaries
}

func (b *browserRuntime) Origin() string {
	if b == nil || b.dyn == nil {
		return ""
	}
	return b.dyn.getOrigin()
}

func (b *browserRuntime) SetDefaultOrigin(ctx context.Context, raw string) {
	if b != nil {
		b.dyn.SetDefaultOrigin(ctx, raw)
	}
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
	hasFiles := path != "" || samlPath != "" || ldapPath != ""
	if !hasFiles && (pool == nil || instanceID == "") {
		return nil, nil
	}
	if pool == nil || instanceID == "" {
		return nil, errBrowserConfig
	}
	var origin string
	var provider *oidclogin.Provider
	var summaries []management.FileProvider
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
		summaries = append(summaries, management.FileProvider{Kind: "oidc", Label: cfg.Issuer, Detail: "client " + cfg.ClientID})
	}
	enterprise, origin, err := openEnterprise(pool, instanceID, origin, samlPath, ldapPath)
	if err != nil {
		return nil, err
	}
	summaries = append(summaries, enterprise.summaries...)
	if hasFiles {
		if runtimeConfig.PublicURL != "" {
			public, ok := browserOrigin(runtimeConfig.PublicURL)
			configured, configuredOK := browserOrigin(origin)
			if !ok || !configuredOK || public != configured {
				return nil, errBrowserConfig
			}
		}
	} else {
		origin = localOrigin(runtimeConfig.PublicURL)
	}
	client := &oidclogin.Client{Provider: provider, Sessions: oidclogin.Sessions{DB: pool, InstanceID: instanceID}, Origin: origin, SuccessRedirect: "/console/"}
	enterprise.client = client
	rt := &browserRuntime{client: client, enterprise: enterprise}
	rt.dyn = &dynamicProviders{
		pool: pool, instance: instanceID, keys: runtimeConfig.SecretKeys, origin: origin,
		files: fileProviders{oidc: provider, saml: enterprise.saml, ldap: enterprise.ldap, samlIssuer: enterprise.samlIssuer, ldapIssuer: enterprise.ldapIssuer, summaries: summaries},
		local: &localadmin.Service{DB: pool, Keys: runtimeConfig.SecretKeys, InstanceID: instanceID},
	}
	// A failed database provider must not stop startup; it is recorded and retried.
	_ = rt.dyn.Reload(ctx)
	return rt, nil
}
func (b *browserRuntime) Register(mux *http.ServeMux) {
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "authentication route not found")
	})
	mux.HandleFunc("GET /auth/providers", func(w http.ResponseWriter, r *http.Request) {
		providers := make([]string, 0, 3)
		if b != nil {
			client, enterprise := b.cur()
			if client.Provider != nil {
				providers = append(providers, "oidc")
			}
			if enterprise != nil && enterprise.saml != nil {
				providers = append(providers, "saml")
			}
			if enterprise != nil && enterprise.ldap != nil {
				providers = append(providers, "ldap")
			}
		}
		browserNoStore(w)
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"providers": providers}
		if b != nil && b.dyn != nil {
			mandatory, _ := localadmin.SSOMandatory(r.Context(), b.dyn.pool, b.dyn.instance)
			body["local"] = true
			body["sso_mandatory"] = mandatory
			if labels := b.dyn.labels(r.Context(), providers); len(labels) > 0 {
				body["labels"] = labels
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	if b == nil {
		return
	}
	if b.dyn == nil {
		if b.client.Provider != nil {
			mux.HandleFunc("/auth/oidc/login", b.client.Login)
			mux.HandleFunc("/auth/oidc/callback", b.client.Callback)
		}
		b.enterprise.Register(mux)
		mux.HandleFunc("/auth/session", b.client.SessionInfo)
		mux.HandleFunc("/auth/logout", b.client.Logout)
		return
	}
	// Dynamic runtime: every route is registered once and dispatches to the
	// current snapshot, so providers can appear and disappear without restart.
	notFound := func(w http.ResponseWriter, r *http.Request) {
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "authentication route not found")
	}
	oidc := func(h func(*oidclogin.Client) http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, _ := b.cur()
			if c.Provider == nil {
				notFound(w, r)
				return
			}
			h(c)(w, r)
		}
	}
	ent := func(has func(*enterpriseLogin) bool, h func(*enterpriseLogin) http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, e := b.cur()
			if e == nil || !has(e) {
				notFound(w, r)
				return
			}
			h(e)(w, r)
		}
	}
	hasSAML := func(e *enterpriseLogin) bool { return e.saml != nil }
	hasLDAP := func(e *enterpriseLogin) bool { return e.ldap != nil }
	mux.HandleFunc("/auth/oidc/login", oidc(func(c *oidclogin.Client) http.HandlerFunc { return c.Login }))
	mux.HandleFunc("/auth/oidc/callback", oidc(func(c *oidclogin.Client) http.HandlerFunc { return c.Callback }))
	mux.HandleFunc("/auth/saml/login", ent(hasSAML, func(e *enterpriseLogin) http.HandlerFunc { return e.samlLogin }))
	mux.HandleFunc("/auth/saml/callback", ent(hasSAML, func(e *enterpriseLogin) http.HandlerFunc { return e.samlCallback }))
	mux.HandleFunc("GET /auth/saml/metadata", ent(hasSAML, func(e *enterpriseLogin) http.HandlerFunc { return e.samlMetadata }))
	mux.HandleFunc("/auth/ldap/login", ent(hasLDAP, func(e *enterpriseLogin) http.HandlerFunc { return e.ldapLogin }))
	mux.HandleFunc("/auth/local/login", b.localLogin)
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) { c, _ := b.cur(); c.SessionInfo(w, r) })
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) { c, _ := b.cur(); c.Logout(w, r) })
}
func (b *browserRuntime) Middleware(next http.Handler) http.Handler {
	if b == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && len(r.Header.Values("Authorization")) == 0 {
			client, _ := b.cur()
			session, err := client.AuthenticateRequest(r)
			if err == nil && session.InstanceID == client.Sessions.InstanceID && session.PrincipalID != "" {
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
	client, _ := b.cur()
	if b.dyn != nil {
		b.dyn.retry(ctx)
	}
	if err := client.Sessions.Cleanup(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, query := range []string{
		`DELETE FROM enterprise_login_states WHERE ctid IN (SELECT ctid FROM enterprise_login_states WHERE instance_id=$1 AND expires_at<=clock_timestamp() LIMIT 500 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM saml_replay_claims WHERE ctid IN (SELECT ctid FROM saml_replay_claims WHERE instance_id=$1 AND expires_at<=clock_timestamp() LIMIT 500 FOR UPDATE SKIP LOCKED)`,
	} {
		if _, err := client.Sessions.DB.Exec(ctx, query, client.Sessions.InstanceID); err != nil {
			return oidclogin.ErrUnavailable
		}
	}
	return nil
}
