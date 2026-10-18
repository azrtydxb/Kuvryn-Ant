package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/samllogin"
	"github.com/jackc/pgx/v5/pgxpool"
)

type samlBrowserProvider interface {
	Begin(string) (*url.URL, samllogin.Pending, error)
	Consume(context.Context, []byte, samllogin.Pending) (samllogin.Identity, error)
	Metadata() ([]byte, error)
}
type ldapBrowserProvider interface {
	Authenticate(context.Context, string, []byte) (directory.ExternalIdentity, error)
}
type enterpriseLogin struct {
	client                 *oidclogin.Client
	saml                   samlBrowserProvider
	ldap                   ldapBrowserProvider
	samlIssuer, ldapIssuer string
	throttle               loginThrottle
	summaries              []management.FileProvider
}
type samlBrowserConfig struct {
	Origin          string `json:"origin"`
	EntityID        string `json:"entity_id"`
	IDPEntityID     string `json:"idp_entity_id"`
	MetadataFile    string `json:"metadata_file"`
	KeyFile         string `json:"key_file"`
	CertificateFile string `json:"certificate_file"`
}
type ldapBrowserConfig struct {
	Origin           string `json:"origin"`
	ProviderID       string `json:"provider_id"`
	URL              string `json:"url"`
	BindDN           string `json:"bind_dn"`
	BindPasswordFile string `json:"bind_password_file"`
	CAFile           string `json:"ca_file"`
	UserBaseDN       string `json:"user_base_dn"`
	GroupBaseDN      string `json:"group_base_dn"`
	LoginAttribute   string `json:"login_attribute"`
	UserIDAttribute  string `json:"user_id_attribute"`
	GroupIDAttribute string `json:"group_id_attribute"`
	UserClass        string `json:"user_class"`
	GroupClass       string `json:"group_class"`
	MemberAttribute  string `json:"member_attribute"`
}

// Mounted configuration and login JSON use exact keys, reject duplicate/null
// members and case aliases, and never include raw decoder errors in responses.
func exactObject(data []byte, dst any) error {
	fields := map[string]bool{}
	typ := reflect.TypeOf(dst).Elem()
	for i := 0; i < typ.NumField(); i++ {
		fields[typ.Field(i).Tag.Get("json")] = true
	}
	d := json.NewDecoder(bytes.NewReader(data))
	v, e := d.Token()
	if e != nil || v != json.Delim('{') {
		return errBrowserConfig
	}
	seen := map[string]bool{}
	for d.More() {
		v, e = d.Token()
		if e != nil {
			return errBrowserConfig
		}
		k, ok := v.(string)
		if !ok || !fields[k] || seen[k] {
			return errBrowserConfig
		}
		seen[k] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil || bytes.Equal(raw, []byte("null")) {
			return errBrowserConfig
		}
	}
	if _, e = d.Token(); e != nil {
		return errBrowserConfig
	}
	if d.Decode(new(any)) != io.EOF {
		return errBrowserConfig
	}
	if json.Unmarshal(data, dst) != nil {
		return errBrowserConfig
	}
	return nil
}
func enterpriseConfig(path string, dst any) error {
	b, e := readBrowserFile(path)
	if e != nil {
		return errBrowserConfig
	}
	return exactObject(b, dst)
}
func validBrowserOrigin(origin string) bool {
	u, e := url.Parse(origin)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && origin == "https://"+u.Host
}
func openEnterprise(pool *pgxpool.Pool, instance, origin, samlPath, ldapPath string) (*enterpriseLogin, string, error) {
	e := &enterpriseLogin{}
	mergeOrigin := func(next string) bool {
		if !validBrowserOrigin(next) || origin != "" && origin != next {
			return false
		}
		origin = next
		return true
	}
	if samlPath != "" {
		var c samlBrowserConfig
		if enterpriseConfig(samlPath, &c) != nil || !mergeOrigin(c.Origin) {
			return nil, "", errBrowserConfig
		}
		metadata, err := readBrowserFile(c.MetadataFile)
		if err != nil {
			return nil, "", errBrowserConfig
		}
		certPEM, err := readBrowserFile(c.CertificateFile)
		if err != nil {
			return nil, "", errBrowserConfig
		}
		keyPEM, err := readBrowserFile(c.KeyFile)
		if err != nil {
			return nil, "", errBrowserConfig
		}
		defer clear(keyPEM)
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil || len(pair.Certificate) == 0 {
			return nil, "", errBrowserConfig
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, "", errBrowserConfig
		}
		replay, err := samllogin.NewPostgresReplay(pool, instance, c.IDPEntityID)
		if err != nil {
			return nil, "", errBrowserConfig
		}
		signer, ok := pair.PrivateKey.(crypto.Signer)
		if !ok {
			return nil, "", errBrowserConfig
		}
		// The SAML library validates the RSA private key, certificate and imported metadata.
		p, err := samllogin.New(samllogin.Config{EntityID: c.EntityID, IDPEntityID: c.IDPEntityID, ACSURL: c.Origin + "/auth/saml/callback", MetadataURL: c.Origin + "/auth/saml/metadata", Metadata: metadata, Key: signer, Certificate: cert, Replay: replay})
		if err != nil {
			return nil, "", errBrowserConfig
		}
		e.saml = p
		e.samlIssuer = c.IDPEntityID
		e.summaries = append(e.summaries, management.FileProvider{Kind: "saml", Label: c.IDPEntityID, Detail: "SP entity " + c.EntityID + "; certificate expires " + cert.NotAfter.UTC().Format("2006-01-02")})
	}
	if ldapPath != "" {
		var c ldapBrowserConfig
		if enterpriseConfig(ldapPath, &c) != nil || !mergeOrigin(c.Origin) {
			return nil, "", errBrowserConfig
		}
		id, err := url.Parse(c.ProviderID)
		if err != nil || id.Scheme != "https" || id.Host == "" || id.User != nil || id.Fragment != "" || id.RawQuery != "" || len(c.ProviderID) > 256 {
			return nil, "", errBrowserConfig
		}
		var ca []byte
		if c.CAFile != "" {
			ca, err = readBrowserFile(c.CAFile)
			if err != nil {
				return nil, "", errBrowserConfig
			}
		}
		secret := func(ctx context.Context) ([]byte, time.Time, error) {
			if ctx.Err() != nil {
				return nil, time.Time{}, ctx.Err()
			}
			b, err := readBrowserFile(c.BindPasswordFile)
			if err != nil {
				return nil, time.Time{}, errBrowserConfig
			}
			b = bytes.TrimSuffix(bytes.TrimSuffix(b, []byte("\n")), []byte("\r"))
			if len(b) == 0 || len(b) > 4096 {
				clear(b)
				return nil, time.Time{}, errBrowserConfig
			}
			return b, time.Now().Add(time.Minute), nil
		}
		check, _, err := secret(context.Background())
		clear(check)
		if err != nil {
			return nil, "", errBrowserConfig
		}
		p, err := directory.New(directory.Config{ProviderID: c.ProviderID, URL: c.URL, BindDN: c.BindDN, UserBaseDN: c.UserBaseDN, GroupBaseDN: c.GroupBaseDN, LoginAttribute: c.LoginAttribute, UserIDAttribute: c.UserIDAttribute, GroupIDAttribute: c.GroupIDAttribute, UserClass: c.UserClass, GroupClass: c.GroupClass, MemberAttribute: c.MemberAttribute, CAPEM: ca}, secret, nil)
		if err != nil {
			return nil, "", errBrowserConfig
		}
		e.ldap = p
		e.ldapIssuer = c.ProviderID
		e.summaries = append(e.summaries, management.FileProvider{Kind: "ldap", Label: c.ProviderID, Detail: c.URL})
	}
	return e, origin, nil
}

func (e *enterpriseLogin) Register(mux *http.ServeMux) {
	if e == nil {
		return
	}
	if e.saml != nil {
		mux.HandleFunc("/auth/saml/login", e.samlLogin)
		mux.HandleFunc("/auth/saml/callback", e.samlCallback)
		mux.HandleFunc("GET /auth/saml/metadata", e.samlMetadata)
	}
	if e.ldap != nil {
		mux.HandleFunc("/auth/ldap/login", e.ldapLogin)
	}
}
func (e *enterpriseLogin) samlMetadata(w http.ResponseWriter, r *http.Request) {
	b, err := e.saml.Metadata()
	if err != nil {
		e.fail(w, r, "saml")
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	w.Write(b)
}
func browserNonce() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure randomness unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func browserHash(s string) []byte { b := sha256.Sum256([]byte(s)); return b[:] }
func validBrowserNonce(s string) bool {
	b, e := base64.RawURLEncoding.DecodeString(s)
	return len(s) == 43 && e == nil && len(b) == 32
}
func enterpriseCookie(w http.ResponseWriter, name, value string, expiry time.Time, mode http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: mode, Expires: expiry, MaxAge: int(time.Until(expiry).Seconds())})
}
func singleCookie(r *http.Request, name string) string {
	value := ""
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			value = c.Value
			n++
		}
	}
	if n != 1 {
		return ""
	}
	return value
}
func browserNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
func (e *enterpriseLogin) audit(ctx context.Context, kind string, success bool) error {
	if kind == "local" && success {
		return nil // the local administrator service already audited this sign-in
	}
	action := "auth." + kind + ".failure"
	if success {
		action = "auth." + kind + ".success"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := e.client.Sessions.DB.Exec(ctx, `INSERT INTO audit_events(instance_id,action) VALUES($1,$2)`, e.client.Sessions.InstanceID, action)
	return err
}
func (e *enterpriseLogin) fail(w http.ResponseWriter, r *http.Request, kind string) {
	browserNoStore(w)
	_ = e.audit(r.Context(), kind, false)
	http.Error(w, "browser authentication failed", http.StatusUnauthorized)
}
func (e *enterpriseLogin) saveState(ctx context.Context, kind, issuer, state, browser string, pending samllogin.Pending) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Bounded opportunistic expiry cleanup; correctness never depends on cleanup.
	_, err := e.client.Sessions.DB.Exec(ctx, `DELETE FROM enterprise_login_states WHERE state_hash IN (SELECT state_hash FROM enterprise_login_states WHERE instance_id=$1 AND expires_at<clock_timestamp() LIMIT 100)`, e.client.Sessions.InstanceID)
	if err != nil {
		return err
	}
	_, err = e.client.Sessions.DB.Exec(ctx, `INSERT INTO enterprise_login_states(state_hash,browser_hash,instance_id,provider,kind,request_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, browserHash(state), browserHash(browser), e.client.Sessions.InstanceID, issuer, kind, pending.ID, pending.Expires)
	return err
}
func (e *enterpriseLogin) consumeState(ctx context.Context, kind, issuer, state, browser string) (samllogin.Pending, error) {
	var pending samllogin.Pending
	if !validBrowserNonce(state) || !validBrowserNonce(browser) {
		return pending, errBrowserConfig
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := e.client.Sessions.DB.QueryRow(ctx, `DELETE FROM enterprise_login_states WHERE state_hash=$1 AND browser_hash=$2 AND instance_id=$3 AND provider=$4 AND kind=$5 AND expires_at>clock_timestamp() RETURNING request_id,expires_at`, browserHash(state), browserHash(browser), e.client.Sessions.InstanceID, issuer, kind).Scan(&pending.ID, &pending.Expires)
	return pending, err
}
func (e *enterpriseLogin) issue(w http.ResponseWriter, r *http.Request, kind, issuer, subject string, groups []string) {
	session, raw, csrf, err := e.client.Sessions.IssueExternalGroups(r.Context(), issuer, subject, groups)
	if err != nil {
		e.fail(w, r, kind)
		return
	}
	if old := singleCookie(r, oidclogin.SessionCookie); old != "" {
		err = e.client.Sessions.Revoke(r.Context(), old)
	}
	if err == nil {
		err = e.audit(r.Context(), kind, true)
	}
	if err != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		_ = e.client.Sessions.Revoke(ctx, raw)
		e.fail(w, r, kind)
		return
	}
	enterpriseCookie(w, oidclogin.SessionCookie, raw, session.ExpiresAt, http.SameSiteLaxMode)
	enterpriseCookie(w, oidclogin.CSRFCookie, csrf, session.ExpiresAt, http.SameSiteLaxMode)
	browserNoStore(w)
	if kind == "saml" && e.client.SuccessRedirect == "/console/" {
		http.Redirect(w, r, "/console/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Session oidclogin.Session `json:"session"`
		CSRF    string            `json:"csrf_token"`
	}{session, csrf})
}
func (e *enterpriseLogin) samlLogin(w http.ResponseWriter, r *http.Request) {
	browserNoStore(w)
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !e.throttle.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		return
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		e.fail(w, r, "saml")
		return
	}
	state, browser := browserNonce(), browserNonce()
	location, pending, err := e.saml.Begin(state)
	if err != nil {
		e.fail(w, r, "saml")
		return
	}
	if err = e.saveState(r.Context(), "saml", e.samlIssuer, state, browser, pending); err != nil {
		e.fail(w, r, "saml")
		return
	}
	// SAML HTTP-POST arrives cross-site; this correlation-only cookie needs None.
	enterpriseCookie(w, "__Host-ant-saml-login", browser, pending.Expires, http.SameSiteNoneMode)
	http.Redirect(w, r, location.String(), http.StatusFound)
}
func (e *enterpriseLogin) samlCallback(w http.ResponseWriter, r *http.Request) {
	browserNoStore(w)
	if !e.throttle.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	origin, _ := url.Parse(e.client.Origin)
	if r.Method != http.MethodPost || r.Host != origin.Host || r.URL.EscapedPath() != "/auth/saml/callback" || r.URL.RawQuery != "" {
		e.fail(w, r, "saml")
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/x-www-form-urlencoded" {
		e.fail(w, r, "saml")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if r.ParseForm() != nil || len(r.PostForm["RelayState"]) != 1 || len(r.PostForm["SAMLResponse"]) != 1 || len(r.PostForm) != 2 {
		e.fail(w, r, "saml")
		return
	}
	pending, err := e.consumeState(r.Context(), "saml", e.samlIssuer, r.PostForm.Get("RelayState"), singleCookie(r, "__Host-ant-saml-login"))
	enterpriseCookie(w, "__Host-ant-saml-login", "", time.Unix(1, 0), http.SameSiteNoneMode)
	if err != nil {
		e.fail(w, r, "saml")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(r.PostForm.Get("SAMLResponse"))
	if err != nil || len(raw) > 1<<20 {
		e.fail(w, r, "saml")
		return
	}
	identity, err := e.saml.Consume(r.Context(), raw, pending)
	if err != nil || identity.Issuer != e.samlIssuer || identity.Subject == "" {
		e.fail(w, r, "saml")
		return
	}
	e.issue(w, r, "saml", identity.Issuer, identity.Subject, identity.Groups)
}
func (e *enterpriseLogin) ldapLogin(w http.ResponseWriter, r *http.Request) {
	browserNoStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !e.throttle.allow(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		e.fail(w, r, "ldap")
		return
	}
	if r.Method == http.MethodGet {
		if origin := r.Header.Get("Origin"); origin != "" && origin != e.client.Origin {
			e.fail(w, r, "ldap")
			return
		}
		state, browser := browserNonce(), browserNonce()
		expires := time.Now().Add(5 * time.Minute)
		if err := e.saveState(r.Context(), "ldap", e.ldapIssuer, state, browser, samllogin.Pending{Expires: expires}); err != nil {
			e.fail(w, r, "ldap")
			return
		}
		enterpriseCookie(w, "__Host-ant-ldap-login", browser, expires, http.SameSiteStrictMode)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": state})
		return
	}
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != e.client.Origin || len(r.Header.Values("X-CSRF-Token")) != 1 {
		e.fail(w, r, "ldap")
		return
	}
	if _, err := e.consumeState(r.Context(), "ldap", e.ldapIssuer, r.Header.Get("X-CSRF-Token"), singleCookie(r, "__Host-ant-ldap-login")); err != nil {
		e.fail(w, r, "ldap")
		return
	}
	enterpriseCookie(w, "__Host-ant-ldap-login", "", time.Unix(1, 0), http.SameSiteStrictMode)
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		e.fail(w, r, "ldap")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		e.fail(w, r, "ldap")
		return
	}
	defer clear(raw)
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if exactObject(raw, &credentials) != nil || credentials.Username == "" || len(credentials.Username) > 256 || len(credentials.Password) == 0 || len(credentials.Password) > 4096 {
		e.fail(w, r, "ldap")
		return
	}
	password := []byte(credentials.Password)
	credentials.Password = ""
	defer clear(password)
	identity, err := e.ldap.Authenticate(r.Context(), credentials.Username, password)
	if err != nil || identity.ProviderID != e.ldapIssuer || identity.Subject == "" {
		e.fail(w, r, "ldap")
		return
	}
	// RoleIDs/GroupIDs are deliberately not local grants. Only explicit local
	// identity bindings and memberships, and federated group bindings applied
	// from the signed federation policy (P2-M2), determine authorization.
	e.issue(w, r, "ldap", identity.ProviderID, identity.Subject, identity.GroupIDs)
}

type loginAttempt struct {
	start time.Time
	count int
}
type loginThrottle struct {
	mu    sync.Mutex
	peers map[string]loginAttempt
}

func (t *loginThrottle) allow(remote string) bool {
	peer, _, err := net.SplitHostPort(remote)
	if err != nil {
		peer = "unknown"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.peers == nil {
		t.peers = map[string]loginAttempt{}
	}
	old, ok := t.peers[peer]
	if !ok || now.Sub(old.start) >= time.Minute {
		if len(t.peers) >= 4096 {
			for p, a := range t.peers {
				if now.Sub(a.start) >= time.Minute {
					delete(t.peers, p)
				}
			}
			if len(t.peers) >= 4096 {
				return false
			}
		}
		old = loginAttempt{start: now}
	}
	if old.count >= 20 {
		return false
	}
	old.count++
	t.peers[peer] = old
	return true
}
