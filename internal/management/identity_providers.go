package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/idpadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FileProvider describes a provider configured from a mounted file. It is shown
// read-only; secrets and file paths are never included.
type FileProvider struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// ActiveProvider describes what the login runtime is serving right now.
type ActiveProvider struct {
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	ProviderID string `json:"provider_id,omitempty"`
}

// IdentityAdminDeps are the runtime collaborators of the provider API.
type IdentityAdminDeps struct {
	Pool *pgxpool.Pool
	Keys *secrets.Keyring
	// Origin is the public HTTPS origin used for callback URLs ("" when unset).
	Origin        string
	Transport     http.RoundTripper // outbound OIDC transport (tests); nil = default
	Dial          directory.Dialer  // LDAP dialer (tests); nil = network
	Reload        func(ctx context.Context) error
	Active        func() []ActiveProvider
	FileProviders []FileProvider
	Now           func() time.Time
}

type idpHandlers struct {
	identityHandlers
	d IdentityAdminDeps
}

type GroupRoleMapping struct {
	GroupID string   `json:"group_id"`
	RoleIDs []string `json:"role_ids"`
}

type ClaimRoleMapping struct {
	Value   string   `json:"value"`
	RoleIDs []string `json:"role_ids"`
}

// ProviderConfig is the union of all protocol settings; unused members stay absent.
type ProviderConfig struct {
	Issuer            *string            `json:"issuer,omitempty"`
	ClientID          *string            `json:"client_id,omitempty"`
	AllowedEndpoints  []string           `json:"allowed_endpoints,omitempty"`
	GroupsClaim       *string            `json:"groups_claim,omitempty"`
	RolesClaim        *string            `json:"roles_claim,omitempty"`
	ClaimRoles        []ClaimRoleMapping `json:"claim_roles,omitempty"`
	Scopes            []string           `json:"scopes,omitempty"`
	PasswordChangeURL *string            `json:"password_change_url,omitempty"`
	NoEndSession      *bool              `json:"no_end_session,omitempty"`
	EntityID          *string            `json:"entity_id,omitempty"`
	IDPEntityID       *string            `json:"idp_entity_id,omitempty"`
	IDPMetadataXML    *string            `json:"idp_metadata_xml,omitempty"`
	SPCertificatePEM  *string            `json:"sp_certificate_pem,omitempty"`
	GroupsAttribute   *string            `json:"groups_attribute,omitempty"`
	RolesAttribute    *string            `json:"roles_attribute,omitempty"`
	EmailAttribute    *string            `json:"email_attribute,omitempty"`
	ProviderID        *string            `json:"provider_id,omitempty"`
	URL               *string            `json:"url,omitempty"`
	BindDN            *string            `json:"bind_dn,omitempty"`
	CAPEM             *string            `json:"ca_pem,omitempty"`
	UserBaseDN        *string            `json:"user_base_dn,omitempty"`
	GroupBaseDN       *string            `json:"group_base_dn,omitempty"`
	LoginAttribute    *string            `json:"login_attribute,omitempty"`
	UserIDAttribute   *string            `json:"user_id_attribute,omitempty"`
	GroupIDAttribute  *string            `json:"group_id_attribute,omitempty"`
	UserClass         *string            `json:"user_class,omitempty"`
	GroupClass        *string            `json:"group_class,omitempty"`
	MemberAttribute   *string            `json:"member_attribute,omitempty"`
	GroupRoles        []GroupRoleMapping `json:"group_roles,omitempty"`
	GroupNesting      *string            `json:"group_nesting,omitempty"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toRaw converts API config to the stored per-protocol JSON.
func (c ProviderConfig) toRaw(kind idpadmin.Kind) (json.RawMessage, error) {
	var v any
	switch kind {
	case idpadmin.OIDC:
		claims := make([]idpadmin.ClaimRole, 0, len(c.ClaimRoles))
		for _, m := range c.ClaimRoles {
			claims = append(claims, idpadmin.ClaimRole{Value: m.Value, RoleIDs: m.RoleIDs})
		}
		if len(claims) == 0 {
			claims = nil
		}
		v = idpadmin.OIDCConfig{Issuer: str(c.Issuer), ClientID: str(c.ClientID), AllowedEndpoints: c.AllowedEndpoints, GroupsClaim: str(c.GroupsClaim), RolesClaim: str(c.RolesClaim), ClaimRoles: claims, Scopes: c.Scopes, PasswordChangeURL: str(c.PasswordChangeURL), NoEndSession: c.NoEndSession != nil && *c.NoEndSession}
	case idpadmin.SAML:
		claims := make([]idpadmin.ClaimRole, 0, len(c.ClaimRoles))
		for _, m := range c.ClaimRoles {
			claims = append(claims, idpadmin.ClaimRole{Value: m.Value, RoleIDs: m.RoleIDs})
		}
		if len(claims) == 0 {
			claims = nil
		}
		v = idpadmin.SAMLConfig{EntityID: str(c.EntityID), IDPEntityID: str(c.IDPEntityID), IDPMetadataXML: str(c.IDPMetadataXML), SPCertificatePEM: str(c.SPCertificatePEM), GroupsAttribute: str(c.GroupsAttribute), EmailAttribute: str(c.EmailAttribute), RolesAttribute: str(c.RolesAttribute), ClaimRoles: claims, PasswordChangeURL: str(c.PasswordChangeURL)}
	case idpadmin.LDAP:
		roles := map[string][]string{}
		for _, m := range c.GroupRoles {
			if _, dup := roles[m.GroupID]; dup {
				return nil, idpadmin.ErrInvalid
			}
			roles[m.GroupID] = m.RoleIDs
		}
		v = idpadmin.LDAPConfig{ProviderID: str(c.ProviderID), URL: str(c.URL), BindDN: str(c.BindDN), CAPEM: str(c.CAPEM), UserBaseDN: str(c.UserBaseDN), GroupBaseDN: str(c.GroupBaseDN),
			LoginAttribute: str(c.LoginAttribute), UserIDAttribute: str(c.UserIDAttribute), GroupIDAttribute: str(c.GroupIDAttribute), UserClass: str(c.UserClass), GroupClass: str(c.GroupClass), MemberAttribute: str(c.MemberAttribute), GroupRoles: roles, GroupNesting: str(c.GroupNesting), PasswordChangeURL: str(c.PasswordChangeURL)}
	default:
		return nil, idpadmin.ErrInvalid
	}
	return json.Marshal(v)
}

func fromRaw(kind idpadmin.Kind, raw json.RawMessage) ProviderConfig {
	var c ProviderConfig
	switch kind {
	case idpadmin.OIDC:
		var o idpadmin.OIDCConfig
		_ = json.Unmarshal(raw, &o)
		c = ProviderConfig{Issuer: ptr(o.Issuer), ClientID: ptr(o.ClientID), AllowedEndpoints: o.AllowedEndpoints, GroupsClaim: ptr(o.GroupsClaim), RolesClaim: ptr(o.RolesClaim), Scopes: o.Scopes, PasswordChangeURL: ptr(o.PasswordChangeURL)}
		if o.NoEndSession {
			t := true
			c.NoEndSession = &t
		}
		for _, m := range o.ClaimRoles {
			c.ClaimRoles = append(c.ClaimRoles, ClaimRoleMapping{Value: m.Value, RoleIDs: m.RoleIDs})
		}
	case idpadmin.SAML:
		var o idpadmin.SAMLConfig
		_ = json.Unmarshal(raw, &o)
		c = ProviderConfig{EntityID: ptr(o.EntityID), IDPEntityID: ptr(o.IDPEntityID), IDPMetadataXML: ptr(o.IDPMetadataXML), SPCertificatePEM: ptr(o.SPCertificatePEM), GroupsAttribute: ptr(o.GroupsAttribute), EmailAttribute: ptr(o.EmailAttribute), RolesAttribute: ptr(o.RolesAttribute), PasswordChangeURL: ptr(o.PasswordChangeURL)}
		for _, m := range o.ClaimRoles {
			c.ClaimRoles = append(c.ClaimRoles, ClaimRoleMapping{Value: m.Value, RoleIDs: m.RoleIDs})
		}
	case idpadmin.LDAP:
		var o idpadmin.LDAPConfig
		_ = json.Unmarshal(raw, &o)
		c = ProviderConfig{ProviderID: ptr(o.ProviderID), URL: ptr(o.URL), BindDN: ptr(o.BindDN), CAPEM: ptr(o.CAPEM), UserBaseDN: ptr(o.UserBaseDN), GroupBaseDN: ptr(o.GroupBaseDN),
			LoginAttribute: ptr(o.LoginAttribute), UserIDAttribute: ptr(o.UserIDAttribute), GroupIDAttribute: ptr(o.GroupIDAttribute), UserClass: ptr(o.UserClass), GroupClass: ptr(o.GroupClass), MemberAttribute: ptr(o.MemberAttribute), GroupNesting: ptr(o.GroupNesting), PasswordChangeURL: ptr(o.PasswordChangeURL)}
		keys := make([]string, 0, len(o.GroupRoles))
		for k := range o.GroupRoles {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c.GroupRoles = append(c.GroupRoles, GroupRoleMapping{GroupID: k, RoleIDs: o.GroupRoles[k]})
		}
	}
	return c
}

type IdentityProvider struct {
	ID             string                `json:"id"`
	Kind           string                `json:"kind"`
	Name           string                `json:"name"`
	Enabled        bool                  `json:"enabled"`
	Config         ProviderConfig        `json:"config"`
	HasSecret      bool                  `json:"has_secret"`
	Revision       int64                 `json:"revision"`
	LastError      string                `json:"last_error,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
	Active         bool                  `json:"active"`
	ShadowedByFile bool                  `json:"shadowed_by_file"`
	Certificates   []idpadmin.CertStatus `json:"certificates"`
}

type IdentityProviderInput struct {
	Kind                string         `json:"kind"`
	Name                string         `json:"name"`
	Enabled             bool           `json:"enabled"`
	Config              ProviderConfig `json:"config"`
	Secret              *string        `json:"secret,omitempty"`
	RotateSPCertificate bool           `json:"rotate_sp_certificate,omitempty"`
}

type LocalAccount struct {
	Exists      bool       `json:"exists"`
	Username    string     `json:"username,omitempty"`
	TOTPEnabled bool       `json:"totp_enabled"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

type IdentityProviderList struct {
	Items         []IdentityProvider `json:"items"`
	FileItems     []FileProvider     `json:"file_items"`
	Active        []ActiveProvider   `json:"active"`
	SSOMandatory  bool               `json:"sso_mandatory"`
	LocalUserTOTP bool               `json:"local_user_totp"`
	Origin        string             `json:"origin"`
	LocalAccount  LocalAccount       `json:"local_account"`
}

type TestCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type IdentityProviderTest struct {
	Kind             string                `json:"kind"`
	OK               bool                  `json:"ok"`
	Checks           []TestCheck           `json:"checks"`
	Warnings         []string              `json:"warnings"`
	ClockSkewSeconds *int64                `json:"clock_skew_seconds,omitempty"`
	SuggestedAllowed []string              `json:"suggested_allowed_endpoints,omitempty"`
	TLSMode          string                `json:"tls_mode,omitempty"`
	Certificates     []idpadmin.CertStatus `json:"certificates"`
}

type IdentityProviderTestRequest struct {
	ID     *string        `json:"id,omitempty"`
	Kind   string         `json:"kind"`
	Config ProviderConfig `json:"config"`
	Secret *string        `json:"secret,omitempty"`
}

type SAMLMetadataImportRequest struct {
	XML string `json:"xml"`
}

type MappingPreviewRequest struct {
	Sample *string `json:"sample,omitempty"`
	Login  *string `json:"login,omitempty"`
}

type IdentityPolicy struct {
	SSOMandatory bool `json:"sso_mandatory"`
	// LocalUserTOTP lets ordinary local users enrol their own second factor.
	// Omitted on write means unchanged; always present on read.
	LocalUserTOTP *bool `json:"local_user_totp,omitempty"`
}

type TOTPEnrollment struct {
	OtpauthURI string `json:"otpauth_uri"`
}
type TOTPConfirm struct {
	Code string `json:"code"`
}

// RegisterIdentityProviders mounts the provider administration, SSO policy and
// local (break-glass) account API. Every route requires instance Admin.
func RegisterIdentityProviders(mux *http.ServeMux, d IdentityAdminDeps) {
	h := idpHandlers{identityHandlers{feedHandlers{db: d.Pool}}, d}
	bearer := identity.NewBearer(identity.Tokens{DB: d.Pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/identity-providers":                       h.list,
		"POST /api/v1/identity-providers":                      h.create,
		"POST /api/v1/identity-providers/test":                 h.test,
		"POST /api/v1/identity-providers/saml/import-metadata": h.importSAML,
		"GET /api/v1/identity-providers/{id}":                  h.get,
		"PUT /api/v1/identity-providers/{id}":                  h.update,
		"DELETE /api/v1/identity-providers/{id}":               h.remove,
		"POST /api/v1/identity-providers/{id}/enable":          h.enable,
		"POST /api/v1/identity-providers/{id}/disable":         h.disable,
		"GET /api/v1/identity-providers/{id}/metadata":         h.metadata,
		"POST /api/v1/identity-providers/{id}/preview":         h.preview,
		"GET /api/v1/identity-policy":                          h.getPolicy,
		"PUT /api/v1/identity-policy":                          h.putPolicy,
		"GET /api/v1/local-account":                            h.localStatus,
		"POST /api/v1/local-account/unlock":                    h.localUnlock,
		"GET /api/v1/local-account/me":                         h.localMe,
		"POST /api/v1/local-account/totp":                      h.totpBegin,
		"POST /api/v1/local-account/totp/confirm":              h.totpConfirm,
		"DELETE /api/v1/local-account/totp":                    h.totpDisable,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
	RegisterLocalUsers(mux, d)
}

func (h idpHandlers) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Second)
}

func (h idpHandlers) now() time.Time {
	if h.d.Now != nil {
		return h.d.Now()
	}
	return time.Now()
}

func (h idpHandlers) store(p identity.Principal) idpadmin.Store {
	kinds := []idpadmin.Kind{}
	for _, f := range h.d.FileProviders {
		kinds = append(kinds, idpadmin.Kind(f.Kind))
	}
	return idpadmin.Store{DB: h.d.Pool, Keys: h.d.Keys, InstanceID: p.InstanceID, FileKinds: kinds}
}

func (h idpHandlers) builder(p identity.Principal) idpadmin.Builder {
	return idpadmin.Builder{Origin: h.d.Origin, Transport: h.d.Transport, DB: h.d.Pool, Keys: h.d.Keys, InstanceID: p.InstanceID, Dial: h.d.Dial}
}

func (h idpHandlers) local(p identity.Principal) *localadmin.Service {
	return &localadmin.Service{DB: h.d.Pool, Keys: h.d.Keys, InstanceID: p.InstanceID}
}

// ready answers 503 when the provider store cannot operate.
func (h idpHandlers) ready(w http.ResponseWriter, r *http.Request) bool {
	if h.d.Pool == nil || h.d.Keys == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "secret keyring required for provider administration")
		return false
	}
	return true
}

func (h idpHandlers) idpError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, idpadmin.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "identity provider not found")
	case errors.Is(err, idpadmin.ErrRevision):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "provider revision changed")
	case errors.Is(err, idpadmin.ErrConflict):
		Error(w, r, 409, api.ErrorCodeConflict, "a provider with that name already exists")
	case errors.Is(err, idpadmin.ErrLastProvider), errors.Is(err, idpadmin.ErrNoProvider), errors.Is(err, idpadmin.ErrNoBreakGlass):
		Error(w, r, 409, api.ErrorCodeConflict, err.Error())
	case errors.Is(err, idpadmin.ErrInvalid):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid provider configuration")
	default:
		// Candidate-build failures (discovery, metadata, key parsing) carry no secrets.
		var inv interface{ Unwrap() error }
		_ = inv
		Error(w, r, 422, api.ErrorCodeInvalidRequest, "configuration rejected: the provider could not be initialized with these settings; run the connectivity test for details")
	}
}

func (h idpHandlers) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid request body")
		return false
	}
	if dec.Decode(new(any)) != io.EOF {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid request body")
		return false
	}
	return true
}

func (h idpHandlers) render(p idpadmin.Provider, active map[string]bool, fileKinds map[string]bool) IdentityProvider {
	out := IdentityProvider{ID: p.ID, Kind: string(p.Kind), Name: p.Name, Enabled: p.Enabled, Config: fromRaw(p.Kind, p.Config), HasSecret: p.HasSecret, Revision: p.Revision,
		LastError: p.LastError, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Active: active[p.ID], ShadowedByFile: p.Enabled && fileKinds[string(p.Kind)], Certificates: []idpadmin.CertStatus{}}
	if p.Kind == idpadmin.SAML {
		if c, err := idpadmin.SAMLConfigOf(p.Config); err == nil {
			diag := idpadmin.Diagnostics{Now: h.d.Now}
			out.Certificates = append(out.Certificates, diag.SPCertificateStatus(c)...)
			out.Certificates = append(out.Certificates, diag.ImportSAMLMetadata([]byte(c.IDPMetadataXML)).Certificates...)
		}
	}
	return out
}

func (h idpHandlers) activeSets() (map[string]bool, map[string]bool, []ActiveProvider) {
	active, fileKinds := map[string]bool{}, map[string]bool{}
	var list []ActiveProvider
	if h.d.Active != nil {
		list = h.d.Active()
	}
	if list == nil {
		list = []ActiveProvider{}
	}
	for _, a := range list {
		if a.Source == "database" {
			active[a.ProviderID] = true
		}
	}
	for _, f := range h.d.FileProviders {
		fileKinds[f.Kind] = true
	}
	return active, fileKinds, list
}

func (h idpHandlers) event(ctx context.Context, p identity.Principal, action, resource string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	_, _ = h.d.Pool.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5)`, p.InstanceID, p.PrincipalID, action, resource, details)
}

func (h idpHandlers) reload(ctx context.Context, p identity.Principal) {
	if h.d.Reload == nil {
		return
	}
	if err := h.d.Reload(ctx); err != nil {
		h.event(ctx, p, "identity_provider.reload_failed", "", nil)
	}
}

// idem runs a provider/policy/local-account mutation under the shared
// Idempotency-Key store. The stores below own their transactions and the
// candidate checks do network I/O, so apply runs while the store holds the
// per-key advisory lock: concurrent identical requests execute once and a
// replay returns the stored non-secret result. Secrets are never part of the
// result; first-time responses that carry one are written by the caller.
func (h idpHandlers) idem(w http.ResponseWriter, r *http.Request, p identity.Principal, operation, resource string, input any, apply func(context.Context) (any, error)) (idempotentOutcome, bool) {
	key, ok := mutationKey(w, r)
	if !ok {
		return idempotentOutcome{}, false
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, operation, resource, input, func(ctx context.Context, _ pgx.Tx) (any, error) { return apply(ctx) })
	if callErr != nil {
		var re reqError
		if errors.As(callErr, &re) {
			Error(w, r, re.status, re.code, re.message)
		} else {
			h.idpError(w, r, callErr)
		}
		return out, false
	}
	if storeErr != nil {
		storeError(w, r, storeErr)
		return out, false
	}
	return out, true
}

func (h idpHandlers) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	providers, err := h.store(p).List(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "provider list unavailable")
		return
	}
	mandatory, _ := h.store(p).SSOMandatory(ctx)
	localTOTP, _ := h.store(p).LocalUserTOTP(ctx)
	active, fileKinds, activeList := h.activeSets()
	out := IdentityProviderList{Items: []IdentityProvider{}, FileItems: h.d.FileProviders, Active: activeList, SSOMandatory: mandatory, LocalUserTOTP: localTOTP, Origin: h.d.Origin}
	if out.FileItems == nil {
		out.FileItems = []FileProvider{}
	}
	for _, pr := range providers {
		out.Items = append(out.Items, h.render(pr, active, fileKinds))
	}
	if st, err := h.local(p).Status(ctx); err == nil {
		out.LocalAccount = LocalAccount{Exists: st.Exists, Username: st.Username, TOTPEnabled: st.TOTPEnabled, LockedUntil: st.LockedUntil, LastLoginAt: st.LastLoginAt}
	}
	JSON(w, 200, out)
}

func (h idpHandlers) one(w http.ResponseWriter, r *http.Request, p identity.Principal, pr idpadmin.Provider, status int) {
	active, fileKinds, _ := h.activeSets()
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(pr.Revision, 10)))
	JSON(w, status, h.render(pr, active, fileKinds))
}

func (h idpHandlers) get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	pr, err := h.store(p).Get(ctx, r.PathValue("id"))
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	h.one(w, r, p, pr, 200)
}

func (h idpHandlers) needsOrigin(w http.ResponseWriter, r *http.Request, kind idpadmin.Kind) bool {
	if kind != idpadmin.LDAP && !idpadmin.ExportableOrigin(h.d.Origin) {
		Error(w, r, 409, api.ErrorCodeConflict, "ANT_PUBLIC_URL must be an https origin before OIDC or SAML providers can be configured")
		return false
	}
	return true
}

func (h idpHandlers) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	var in IdentityProviderInput
	if !h.decode(w, r, &in) {
		return
	}
	kind := idpadmin.Kind(in.Kind)
	if !kind.Valid() || !h.needsOrigin(w, r, kind) {
		if !kind.Valid() {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid provider kind")
		}
		return
	}
	raw, err := in.Config.toRaw(kind)
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	if !h.claimRolesExist(w, r, p, raw, kind) {
		return
	}
	out, ok := h.idem(w, r, p, "identity_provider.create", "identity-providers", in, func(ctx context.Context) (any, error) {
		pr, err := h.store(p).Create(ctx, p.PrincipalID, idpadmin.Input{Kind: kind, Name: in.Name, Enabled: in.Enabled, Config: raw, Secret: in.Secret}, h.builder(p).Check)
		if err != nil {
			return nil, err
		}
		h.reload(ctx, p)
		return h.rendered(pr), nil
	})
	if ok {
		replayWrite(w, r, out, 201)
	}
}

// claimRolesExist rejects role mappings (OIDC claims, SAML attributes, LDAP
// groups) that reference unknown or federation-managed roles, so a mapping can
// only ever assign existing local roles.
func (h idpHandlers) claimRolesExist(w http.ResponseWriter, r *http.Request, p identity.Principal, raw json.RawMessage, kind idpadmin.Kind) bool {
	want := map[string]bool{}
	add := func(maps []idpadmin.ClaimRole) {
		for _, m := range maps {
			for _, id := range m.RoleIDs {
				want[id] = true
			}
		}
	}
	switch kind {
	case idpadmin.OIDC:
		var c idpadmin.OIDCConfig
		if json.Unmarshal(raw, &c) != nil {
			return true // structural validation reports this
		}
		add(c.ClaimRoles)
	case idpadmin.SAML:
		var c idpadmin.SAMLConfig
		if json.Unmarshal(raw, &c) != nil {
			return true
		}
		add(c.ClaimRoles)
	case idpadmin.LDAP:
		var c idpadmin.LDAPConfig
		if json.Unmarshal(raw, &c) != nil {
			return true
		}
		for _, ids := range c.GroupRoles {
			for _, id := range ids {
				want[id] = true
			}
		}
	default:
		return true
	}
	if len(want) == 0 {
		return true
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	var n int
	if err := h.d.Pool.QueryRow(ctx, `SELECT count(*) FROM roles WHERE instance_id=$1 AND origin='local' AND id::text=ANY($2)`, p.InstanceID, ids).Scan(&n); err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "role lookup unavailable")
		return false
	}
	if n != len(ids) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "role mappings reference unknown or non-local roles")
		return false
	}
	return true
}

func (h idpHandlers) update(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var in IdentityProviderInput
	if !h.decode(w, r, &in) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	cur, err := h.store(p).Get(ctx, r.PathValue("id"))
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	if in.Kind != "" && in.Kind != string(cur.Kind) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "provider kind cannot change")
		return
	}
	if !h.needsOrigin(w, r, cur.Kind) {
		return
	}
	raw, err := in.Config.toRaw(cur.Kind)
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	if !h.claimRolesExist(w, r, p, raw, cur.Kind) {
		return
	}
	out, ok := h.idem(w, r, p, "identity_provider.update", cur.ID, struct {
		Revision int64
		In       IdentityProviderInput
	}{revision, in}, func(ctx context.Context) (any, error) {
		pr, err := h.store(p).Update(ctx, p.PrincipalID, cur.ID, revision, idpadmin.Input{Name: in.Name, Enabled: in.Enabled, Config: raw, Secret: in.Secret, RotateSPCertificate: in.RotateSPCertificate}, h.builder(p).Check)
		if err != nil {
			return nil, err
		}
		h.reload(ctx, p)
		return h.rendered(pr), nil
	})
	if ok {
		replayWrite(w, r, out, 200)
	}
}

func (h idpHandlers) remove(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	id := r.PathValue("id")
	out, ok := h.idem(w, r, p, "identity_provider.delete", id, struct{ Revision int64 }{revision}, func(ctx context.Context) (any, error) {
		if err := h.store(p).Delete(ctx, p.PrincipalID, id, revision); err != nil {
			return nil, err
		}
		h.reload(ctx, p)
		return struct{}{}, nil
	})
	if ok {
		replayWrite(w, r, out, 204)
	}
}

func (h idpHandlers) toggle(w http.ResponseWriter, r *http.Request, enabled bool) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	cur, err := h.store(p).Get(ctx, r.PathValue("id"))
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	if enabled && !h.needsOrigin(w, r, cur.Kind) {
		return
	}
	out, ok := h.idem(w, r, p, "identity_provider.enabled", cur.ID, struct {
		Revision int64
		Enabled  bool
	}{revision, enabled}, func(ctx context.Context) (any, error) {
		pr, err := h.store(p).SetEnabled(ctx, p.PrincipalID, cur.ID, revision, enabled, h.builder(p).Check)
		if err != nil {
			return nil, err
		}
		h.reload(ctx, p)
		return h.rendered(pr), nil
	})
	if ok {
		replayWrite(w, r, out, 200)
	}
}
func (h idpHandlers) enable(w http.ResponseWriter, r *http.Request)  { h.toggle(w, r, true) }
func (h idpHandlers) disable(w http.ResponseWriter, r *http.Request) { h.toggle(w, r, false) }

func (h idpHandlers) metadata(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	l, err := h.store(p).Load(ctx, r.PathValue("id"))
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	defer clear(l.Secret)
	if l.Kind != idpadmin.SAML {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "only SAML providers have service provider metadata")
		return
	}
	if !h.needsOrigin(w, r, l.Kind) {
		return
	}
	xml, err := h.builder(p).SPMetadata(l)
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	w.Header().Set("Content-Disposition", `attachment; filename="ant-sp-metadata.xml"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(xml)
}

func (h idpHandlers) importSAML(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in SAMLMetadataImportRequest
	if !h.decode(w, r, &in) {
		return
	}
	diag := idpadmin.Diagnostics{Now: h.d.Now}
	JSON(w, 200, diag.ImportSAMLMetadata([]byte(in.XML)))
}

func check(name string, ok bool, detail string) TestCheck { return TestCheck{name, ok, detail} }

func (h idpHandlers) test(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	var in IdentityProviderTestRequest
	if !h.decode(w, r, &in) {
		return
	}
	kind := idpadmin.Kind(in.Kind)
	if !kind.Valid() {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid provider kind")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	l := idpadmin.Loaded{Provider: idpadmin.Provider{Kind: kind, Name: "test"}}
	raw, err := in.Config.toRaw(kind)
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	l.Config = raw
	if in.Secret != nil {
		l.Secret = []byte(*in.Secret)
	} else if in.ID != nil {
		stored, err := h.store(p).Load(ctx, *in.ID)
		if err != nil {
			h.idpError(w, r, err)
			return
		}
		l.Secret = stored.Secret
		if stored.Kind != kind {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "provider kind mismatch")
			return
		}
		if kind == idpadmin.SAML {
			// Keep the stored SP certificate and key for the dry-run build.
			l.Config = stored.Config
			var sc idpadmin.SAMLConfig
			_ = json.Unmarshal(raw, &sc)
			var old idpadmin.SAMLConfig
			_ = json.Unmarshal(stored.Config, &old)
			old.IDPMetadataXML, old.EntityID, old.IDPEntityID, old.GroupsAttribute, old.EmailAttribute = sc.IDPMetadataXML, sc.EntityID, sc.IDPEntityID, sc.GroupsAttribute, sc.EmailAttribute
			l.Config, _ = json.Marshal(old)
		}
		l.ID = stored.ID
	}
	defer clear(l.Secret)
	if kind == idpadmin.SAML && in.ID == nil {
		// Unsaved SAML config: use a throwaway SP key pair so the build can run.
		var sc idpadmin.SAMLConfig
		_ = json.Unmarshal(raw, &sc)
		cert, key, err := idpadmin.GenerateSPKeyPair(sc.EntityID)
		if err == nil {
			sc.SPCertificatePEM, l.Secret = string(cert), key
			l.Config, _ = json.Marshal(sc)
		}
	}
	out := IdentityProviderTest{Kind: in.Kind, Checks: []TestCheck{}, Warnings: []string{}, Certificates: []idpadmin.CertStatus{}}
	diag := idpadmin.Diagnostics{Transport: h.d.Transport, Now: h.d.Now}
	b := h.builder(p)
	switch kind {
	case idpadmin.OIDC:
		var c idpadmin.OIDCConfig
		_ = json.Unmarshal(l.Config, &c)
		rep := diag.OIDC(ctx, c.Issuer, c.AllowedEndpoints)
		out.Checks = append(out.Checks,
			check("discovery", rep.IssuerMatches && rep.Endpoints.Token != "", "discovery document fetched and issuer matches"),
			check("jwks", rep.JWKSKeys > 0, "signing keys fetched from jwks_uri"),
			check("signing_algorithm", rep.SupportsRS256, "RS256 is advertised and a key is present"),
			check("pkce", rep.SupportsPKCE, "PKCE S256 advertised"),
			check("approved_endpoints", len(rep.MissingFromAllowed) == 0 && len(c.AllowedEndpoints) > 0, "allowed_endpoints covers authorization, token and JWKS URLs"),
		)
		out.Warnings = append(out.Warnings, rep.Warnings...)
		out.Warnings = append(out.Warnings, rep.Errors...)
		out.ClockSkewSeconds = rep.ClockSkewSeconds
		out.SuggestedAllowed = rep.SuggestedEndpoints
		if rep.ClockSkewSeconds != nil {
			out.Checks = append(out.Checks, check("clock_skew", !rep.SkewWarning, "local clock within 30 seconds of the provider"))
		}
		out.OK = rep.OK && len(c.AllowedEndpoints) > 0
		if !out.OK && len(rep.Errors) == 0 {
			out.Warnings = append(out.Warnings, "allowed_endpoints must be set (use the suggested endpoints)")
		}
	case idpadmin.SAML:
		var c idpadmin.SAMLConfig
		_ = json.Unmarshal(l.Config, &c)
		meta := diag.ImportSAMLMetadata([]byte(c.IDPMetadataXML))
		out.Certificates = append(out.Certificates, meta.Certificates...)
		out.Certificates = append(out.Certificates, diag.SPCertificateStatus(c)...)
		out.Warnings = append(out.Warnings, meta.Warnings...)
		out.Warnings = append(out.Warnings, meta.Errors...)
		out.Checks = append(out.Checks, check("metadata", meta.OK, "IdP metadata parsed; endpoints and signing certificates valid"),
			check("entity_id", meta.EntityID != "" && meta.EntityID == c.IDPEntityID, "metadata entityID equals idp_entity_id"))
		buildErr := b.Check(ctx, l)
		out.Checks = append(out.Checks, check("provider_build", buildErr == nil, "service provider can be initialized from these settings"))
		out.OK = meta.OK && meta.EntityID == c.IDPEntityID && buildErr == nil
	case idpadmin.LDAP:
		dir, err := b.LDAP(l)
		if err != nil {
			out.Checks = append(out.Checks, check("configuration", false, "LDAP settings are invalid (URL scheme, DNs, attributes or CA certificate)"))
			break
		}
		d := dir.Diagnose(ctx)
		out.TLSMode = d.TLSMode
		for _, s := range d.Steps {
			out.Checks = append(out.Checks, check(s.Name, s.OK, s.Detail))
		}
		out.OK = d.OK
	}
	h.event(ctx, p, "identity_provider.test", l.ID, map[string]any{"kind": in.Kind, "ok": out.OK})
	JSON(w, 200, out)
}

type MappingPreview struct {
	Identity          idpadmin.Subject `json:"identity"`
	SignatureVerified bool             `json:"signature_verified"`
	Binding           idpadmin.Binding `json:"binding"`
	ProposedRoles     []string         `json:"proposed_roles"`
	WouldSignIn       bool             `json:"would_sign_in"`
	Problems          []string         `json:"problems"`
	Warnings          []string         `json:"warnings"`
	ClockSkewSeconds  *int64           `json:"clock_skew_seconds,omitempty"`
}

func (h idpHandlers) preview(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	var in MappingPreviewRequest
	if !h.decode(w, r, &in) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	l, err := h.store(p).Load(ctx, r.PathValue("id"))
	if err != nil {
		h.idpError(w, r, err)
		return
	}
	defer clear(l.Secret)
	pv := idpadmin.Preview{DB: h.d.Pool, InstanceID: p.InstanceID, Now: h.d.Now}
	var res idpadmin.PreviewResult
	switch l.Kind {
	case idpadmin.OIDC:
		var c idpadmin.OIDCConfig
		_ = json.Unmarshal(l.Config, &c)
		res, err = pv.OIDCToken(ctx, c, str(in.Sample))
	case idpadmin.SAML:
		var c idpadmin.SAMLConfig
		_ = json.Unmarshal(l.Config, &c)
		res, err = pv.SAMLAssertion(ctx, c, str(in.Sample))
	case idpadmin.LDAP:
		var c idpadmin.LDAPConfig
		_ = json.Unmarshal(l.Config, &c)
		dir, derr := h.builder(p).LDAP(idpadmin.Loaded{Provider: l.Provider})
		if derr != nil {
			Error(w, r, 422, api.ErrorCodeInvalidRequest, "LDAP provider cannot be initialized")
			return
		}
		res, err = pv.LDAPUser(ctx, dir, c.ProviderID, strings.TrimSpace(str(in.Login)))
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "preview unavailable")
		return
	}
	// Never audit pasted material; record only that a preview ran.
	h.event(ctx, p, "identity_provider.preview", l.ID, map[string]any{"kind": string(l.Kind), "would_sign_in": res.WouldSignIn})
	if res.ProposedRoles == nil {
		res.ProposedRoles = []string{}
	}
	JSON(w, 200, MappingPreview{res.Subject, res.Verified, res.Binding, res.ProposedRoles, res.WouldSignIn, res.Problems, res.Warnings, res.ClockSkewSeconds})
}

func (h idpHandlers) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	v, err := h.store(p).SSOMandatory(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "policy unavailable")
		return
	}
	localTOTP, err := h.store(p).LocalUserTOTP(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "policy unavailable")
		return
	}
	JSON(w, 200, IdentityPolicy{SSOMandatory: v, LocalUserTOTP: &localTOTP})
}

func (h idpHandlers) putPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	var in IdentityPolicy
	if !h.decode(w, r, &in) {
		return
	}
	out, ok := h.idem(w, r, p, "identity_policy.put", "identity-policy", in, func(ctx context.Context) (any, error) {
		if err := h.store(p).SetSSOMandatory(ctx, p.PrincipalID, in.SSOMandatory); err != nil {
			return nil, err
		}
		if in.LocalUserTOTP != nil {
			if err := h.store(p).SetLocalUserTOTP(ctx, p.PrincipalID, *in.LocalUserTOTP); err != nil {
				return nil, err
			}
		}
		current, err := h.store(p).LocalUserTOTP(ctx)
		if err != nil {
			return nil, err
		}
		return IdentityPolicy{SSOMandatory: in.SSOMandatory, LocalUserTOTP: &current}, nil
	})
	if ok {
		replayWrite(w, r, out, 200)
	}
}

func (h idpHandlers) localStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	st, err := h.local(p).Status(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "local account unavailable")
		return
	}
	JSON(w, 200, LocalAccount{Exists: st.Exists, Username: st.Username, TOTPEnabled: st.TOTPEnabled, LockedUntil: st.LockedUntil, LastLoginAt: st.LastLoginAt})
}

func (h idpHandlers) localUnlock(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	out, ok := h.idem(w, r, p, "local_account.unlock", "local-account", struct{}{}, func(ctx context.Context) (any, error) {
		if err := h.local(p).Unlock(ctx); err != nil {
			return nil, reqError{503, api.ErrorCodeUnavailable, "local account unavailable"}
		}
		h.event(ctx, p, "auth.local.unlocked", "", nil)
		return struct{}{}, nil
	})
	if ok {
		replayWrite(w, r, out, 204)
	}
}

// own authorizes second-factor changes: only the signed-in local account
// itself may manage its own factor, so one administrator cannot silently weaken
// another's. The break-glass account always may; an ordinary local user may
// when the local-user TOTP setting is on and the caller is a browser session
// (not a token). Admins reset a user's lost factor through the local-accounts API.
func (h idpHandlers) own(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, _, ok := h.policy(w, r)
	if !ok || !h.ready(w, r) {
		return p, false
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	u, found, err := h.local(p).Account(ctx, p.PrincipalID)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "local account unavailable")
		return p, false
	}
	if !found || (!u.BreakGlass && (p.TokenID != "" || p.Scopes != nil)) {
		Error(w, r, 403, api.ErrorCodeForbidden, "only the signed-in local account can manage its own second factor")
		return p, false
	}
	if !u.BreakGlass {
		enabled, err := h.store(p).LocalUserTOTP(ctx)
		if err != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "local account unavailable")
			return p, false
		}
		if !enabled {
			Error(w, r, 403, api.ErrorCodeForbidden, "second-factor enrolment is not enabled for local users")
			return p, false
		}
	}
	return p, true
}

// localMe reports the signed-in caller's own local account.
func (h idpHandlers) localMe(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.policy(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	u, found, err := h.local(p).Account(ctx, p.PrincipalID)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "local account unavailable")
		return
	}
	if !found {
		Error(w, r, 404, api.ErrorCodeNotFound, "no local account for this principal")
		return
	}
	JSON(w, 200, LocalUser(u))
}

func (h idpHandlers) totpBegin(w http.ResponseWriter, r *http.Request) {
	p, ok := h.own(w, r)
	if !ok {
		return
	}
	// The otpauth URI embeds the TOTP secret, so it is returned to the first
	// caller only; the replay store keeps nothing and a retry gets a 409.
	var uri string
	out, ok := h.idem(w, r, p, "local_account.totp.begin", "local-account/totp", struct{}{}, func(ctx context.Context) (any, error) {
		var err error
		if uri, err = h.local(p).BeginTOTP(ctx, p.PrincipalID); err != nil {
			return nil, reqError{409, api.ErrorCodeConflict, "cannot start TOTP enrollment (already enabled?)"}
		}
		return struct{}{}, nil
	})
	if !ok {
		return
	}
	if out.Replayed {
		Error(w, r, 409, api.ErrorCodeConflict, "already started, secret not retrievable")
		return
	}
	JSON(w, 200, TOTPEnrollment{uri})
}

func (h idpHandlers) totpConfirm(w http.ResponseWriter, r *http.Request) {
	p, ok := h.own(w, r)
	if !ok {
		return
	}
	var in TOTPConfirm
	if !h.decode(w, r, &in) {
		return
	}
	code := strings.TrimSpace(in.Code)
	// Only a digest of the one-time code is fingerprinted; replaying a
	// confirmed enrollment must not re-verify an expired code.
	sum := sha256.Sum256([]byte(code))
	out, ok := h.idem(w, r, p, "local_account.totp.confirm", "local-account/totp", struct{ Code [32]byte }{sum}, func(ctx context.Context) (any, error) {
		if err := h.local(p).ConfirmTOTP(ctx, p.PrincipalID, code); err != nil {
			return nil, reqError{400, api.ErrorCodeInvalidRequest, "invalid code or no enrollment in progress"}
		}
		return struct{}{}, nil
	})
	if ok {
		replayWrite(w, r, out, 204)
	}
}

func (h idpHandlers) totpDisable(w http.ResponseWriter, r *http.Request) {
	p, ok := h.own(w, r)
	if !ok {
		return
	}
	out, ok := h.idem(w, r, p, "local_account.totp.disable", "local-account/totp", struct{}{}, func(ctx context.Context) (any, error) {
		if err := h.local(p).DisableTOTP(ctx, p.PrincipalID); err != nil {
			return nil, reqError{503, api.ErrorCodeUnavailable, "local account unavailable"}
		}
		return struct{}{}, nil
	})
	if ok {
		replayWrite(w, r, out, 204)
	}
}

var _ = bytes.MinRead

// rendered is the replay-safe (secret-free) representation of a provider.
func (h idpHandlers) rendered(pr idpadmin.Provider) IdentityProvider {
	active, fileKinds, _ := h.activeSets()
	return h.render(pr, active, fileKinds)
}
