package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/idpadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/samllogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5/pgxpool"
)

// browserState is an immutable snapshot of the login providers being served.
// Handlers load it once per request, so a reload never changes behavior
// mid-request and in-flight logins finish against the providers they began with.
type browserState struct {
	client     *oidclogin.Client
	enterprise *enterpriseLogin
	active     []management.ActiveProvider
}

// fileProviders are the providers built from ANT_*_CONFIG_FILE at startup.
// They are read-only and take precedence over database providers of the same
// protocol (a database entry for that protocol is stored but shadowed).
type fileProviders struct {
	oidc       *oidclogin.Provider
	saml       samlBrowserProvider
	ldap       ldapBrowserProvider
	samlIssuer string
	ldapIssuer string
	summaries  []management.FileProvider
}

// dynamicProviders adds database-stored providers, hot reload and the local
// break-glass login on top of the file-configured runtime.
type dynamicProviders struct {
	pool       *pgxpool.Pool
	instance   string
	keys       *secrets.Keyring
	files      fileProviders
	local      *localadmin.Service
	throttle   loginThrottle
	transport  http.RoundTripper // tests only
	dial       directory.Dialer  // tests only
	mu         sync.Mutex        // serializes reloads
	originMu   sync.RWMutex
	origin     string
	live       atomic.Pointer[browserState]
	needsRetry atomic.Bool
	retrying   atomic.Bool
}

func (d *dynamicProviders) getOrigin() string {
	d.originMu.RLock()
	defer d.originMu.RUnlock()
	return d.origin
}

func (d *dynamicProviders) store() idpadmin.Store {
	kinds := []idpadmin.Kind{}
	if d.files.oidc != nil {
		kinds = append(kinds, idpadmin.OIDC)
	}
	if d.files.saml != nil {
		kinds = append(kinds, idpadmin.SAML)
	}
	if d.files.ldap != nil {
		kinds = append(kinds, idpadmin.LDAP)
	}
	return idpadmin.Store{DB: d.pool, Keys: d.keys, InstanceID: d.instance, FileKinds: kinds}
}

func (d *dynamicProviders) builder() idpadmin.Builder {
	return idpadmin.Builder{Origin: d.getOrigin(), Transport: d.transport, DB: d.pool, Keys: d.keys, InstanceID: d.instance, Dial: d.dial}
}

// localOrigin accepts the https public origin, or http only for loopback
// development; anything else yields "" (sessions need a trustworthy origin).
func localOrigin(raw string) string {
	if raw == "" {
		return ""
	}
	if o, ok := browserOrigin(raw); ok {
		return o
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); (ip == nil || !ip.IsLoopback()) && u.Hostname() != "localhost" {
		return ""
	}
	return "http://" + u.Host
}

// SetDefaultOrigin supplies the serving origin when none was configured
// (loopback development) and rebuilds the snapshot.
func (d *dynamicProviders) SetDefaultOrigin(ctx context.Context, raw string) {
	if d == nil {
		return
	}
	d.originMu.Lock()
	changed := d.origin == "" && localOrigin(raw) != ""
	if changed {
		d.origin = localOrigin(raw)
	}
	d.originMu.Unlock()
	if changed {
		_ = d.Reload(ctx)
	}
}

// Reload rebuilds the login providers from files plus enabled database rows and
// atomically publishes them. A provider that fails to build is left out (and its
// error recorded) without disturbing the others; the file providers and the
// break-glass login are unaffected. It is safe to call concurrently.
func (d *dynamicProviders) Reload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	origin := d.getOrigin()
	prev := d.live.Load()
	state := &browserState{}
	oidcProv := d.files.oidc
	ent := &enterpriseLogin{saml: d.files.saml, ldap: d.files.ldap, samlIssuer: d.files.samlIssuer, ldapIssuer: d.files.ldapIssuer, throttle: loginThrottle{}}
	state.active = []management.ActiveProvider{}
	if oidcProv != nil {
		state.active = append(state.active, management.ActiveProvider{Kind: "oidc", Source: "file"})
	}
	if ent.saml != nil {
		state.active = append(state.active, management.ActiveProvider{Kind: "saml", Source: "file"})
	}
	if ent.ldap != nil {
		state.active = append(state.active, management.ActiveProvider{Kind: "ldap", Source: "file"})
	}
	failed := false
	var firstErr error
	if d.pool != nil {
		store := d.store()
		loaded, err := store.LoadEnabled(ctx)
		if err != nil {
			// Database trouble: keep serving the previous snapshot untouched.
			if prev != nil {
				d.needsRetry.Store(true)
				return err
			}
			loaded = nil
			failed, firstErr = true, err
		}
		b := d.builder()
		for _, l := range loaded {
			kind := l.Kind
			if (kind == idpadmin.OIDC && oidcProv != nil && d.files.oidc != nil) || (kind == idpadmin.SAML && d.files.saml != nil) || (kind == idpadmin.LDAP && d.files.ldap != nil) {
				continue // shadowed by read-only file configuration
			}
			if l.LastError == "secret unavailable" {
				failed = true
				continue
			}
			var buildErr error
			buildCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			switch kind {
			case idpadmin.OIDC:
				var p *oidclogin.Provider
				if p, buildErr = b.OIDC(buildCtx, l); buildErr == nil {
					oidcProv = p
				}
			case idpadmin.SAML:
				var c idpadmin.SAMLConfig
				_ = json.Unmarshal(l.Config, &c)
				var replay *samllogin.PostgresReplay
				if replay, buildErr = samllogin.NewPostgresReplay(d.pool, d.instance, c.IDPEntityID); buildErr == nil {
					var p *samllogin.Provider
					if p, buildErr = b.SAML(l, replay); buildErr == nil {
						ent.saml, ent.samlIssuer = p, c.IDPEntityID
					}
				}
			case idpadmin.LDAP:
				var c idpadmin.LDAPConfig
				_ = json.Unmarshal(l.Config, &c)
				// The bind password is re-read from encrypted storage per operation.
				l2 := l
				clear(l2.Secret)
				l2.Secret = nil
				var p *directory.Provider
				if p, buildErr = b.LDAP(l2); buildErr == nil {
					ent.ldap, ent.ldapIssuer = p, c.ProviderID
				}
			}
			cancel()
			clear(l.Secret)
			if buildErr != nil {
				failed = true
				if firstErr == nil {
					firstErr = buildErr
				}
				store.RecordError(ctx, l.ID, "provider could not be initialized: "+errClass(buildErr))
				continue
			}
			state.active = append(state.active, management.ActiveProvider{Kind: string(kind), Source: "database", ProviderID: l.ID})
		}
	}
	client := &oidclogin.Client{Provider: oidcProv, Sessions: oidclogin.Sessions{DB: d.pool, InstanceID: d.instance}, Origin: origin, SuccessRedirect: "/console/"}
	ent.client = client
	state.client, state.enterprise = client, ent
	d.live.Store(state)
	d.needsRetry.Store(failed)
	return firstErr
}

// errClass reduces a build error to a fixed, non-sensitive category.
func errClass(err error) string {
	switch {
	case errors.Is(err, idpadmin.ErrInvalid), errors.Is(err, samllogin.ErrInvalid), errors.Is(err, directory.ErrConfig):
		return "invalid configuration"
	case errors.Is(err, oidclogin.ErrUnavailable):
		return "provider unreachable or discovery rejected"
	}
	return "initialization failed"
}

func (d *dynamicProviders) state() *browserState { return d.live.Load() }

// Active reports what is being served now.
func (d *dynamicProviders) Active() []management.ActiveProvider {
	if s := d.state(); s != nil {
		return append([]management.ActiveProvider{}, s.active...)
	}
	return []management.ActiveProvider{}
}

// retry reloads in the background after a failed build (an IdP that was down at
// startup, for example), without blocking the caller.
func (d *dynamicProviders) retry(parent context.Context) {
	if !d.needsRetry.Load() || !d.retrying.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.retrying.Store(false)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 40*time.Second)
		defer cancel()
		_ = d.Reload(ctx)
	}()
}

var errLocalLogin = errors.New("local login failed")

// localLogin is the break-glass sign-in. It is registered whether or not any
// SSO provider is configured or healthy, so a broken SSO configuration can
// never lock administrators out. Failures return one generic response.
func (b *browserRuntime) localLogin(w http.ResponseWriter, r *http.Request) {
	d := b.dyn
	browserNoStore(w)
	s := d.state()
	if s == nil || s.client == nil || s.client.Origin == "" {
		http.Error(w, "browser authentication failed", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !d.throttle.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" || len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != s.client.Origin {
		s.enterprise.fail(w, r, "local")
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		s.enterprise.fail(w, r, "local")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.enterprise.fail(w, r, "local")
		return
	}
	defer clear(raw)
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTPCode string `json:"totp_code"`
	}
	fields := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &fields) != nil || len(fields) == 0 || len(fields) > 3 {
		s.enterprise.fail(w, r, "local")
		return
	}
	for k := range fields {
		if k != "username" && k != "password" && k != "totp_code" {
			s.enterprise.fail(w, r, "local")
			return
		}
	}
	if json.Unmarshal(raw, &in) != nil || in.Username == "" || in.Password == "" {
		s.enterprise.fail(w, r, "local")
		return
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	res, err := d.local.Login(r.Context(), in.Username, in.Password, strings.TrimSpace(in.TOTPCode), peer)
	in.Password = ""
	switch {
	case errors.Is(err, localadmin.ErrMFARequired):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"mfa_required":true}`))
		return
	case errors.Is(err, localadmin.ErrUnavailable):
		http.Error(w, "browser authentication unavailable", http.StatusServiceUnavailable)
		return
	case err != nil:
		// The attempt was audited by the local administrator service.
		http.Error(w, "browser authentication failed", http.StatusUnauthorized)
		return
	}
	s.enterprise.issue(w, r, "local", localadmin.Issuer, res.Subject, loginExtras{})
}

// labels returns the administrator-chosen display names of the enabled stored
// providers whose protocol is currently active, keyed by protocol ("oidc",
// "saml", "ldap"). Names are not secret; they let the sign-in page say "Sign in
// with Zitadel". A lookup failure just yields no labels.
func (d *dynamicProviders) labels(ctx context.Context, active []string) map[string]string {
	out := map[string]string{}
	if d == nil || d.pool == nil || len(active) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := d.pool.Query(ctx, `SELECT kind,name FROM identity_providers WHERE instance_id=$1 AND enabled ORDER BY kind`, d.instance)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name string
		if rows.Scan(&kind, &name) != nil {
			return map[string]string{}
		}
		for _, a := range active {
			if a == kind {
				out[kind] = name
			}
		}
	}
	return out
}
