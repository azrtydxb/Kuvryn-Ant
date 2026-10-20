package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/accountlinks"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// meHandlers serve the signed-in user's own account: who they are, how their
// password is changed, and their other browser sessions. b may be nil (no
// browser runtime): then only bearer-token callers exist.
type meHandlers struct {
	pool *pgxpool.Pool
	b    *browserRuntime
	mu   sync.Mutex
	hits map[string]loginAttempt
}

type meRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type meResponse struct {
	PrincipalID      string              `json:"principal_id"`
	Kind             string              `json:"kind"`
	DisplayName      string              `json:"display_name"`
	Subject          string              `json:"subject"`
	Email            string              `json:"email,omitempty"`
	Picture          string              `json:"picture,omitempty"`
	AuthMethod       accountlinks.Method `json:"auth_method"`
	IdP              string              `json:"idp,omitempty"`
	IdPID            string              `json:"idp_id,omitempty"`
	Roles            []meRole            `json:"roles"`
	PasswordChange   accountlinks.Change `json:"password_change"`
	SessionExpiresAt *time.Time          `json:"session_expires_at,omitempty"`
	ActiveSessions   int                 `json:"active_sessions"`
}

// registerMe mounts /api/v1/me. Credentials are the same as every other
// management route: a bearer token, or a browser session whose Origin and CSRF
// the browser middleware already enforced for mutations.
func registerMe(mux *http.ServeMux, pool *pgxpool.Pool, b *browserRuntime) {
	h := &meHandlers{pool: pool, b: b, hits: map[string]loginAttempt{}}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/me":                    h.me,
		"POST /api/v1/me/password":          h.password,
		"DELETE /api/v1/me/sessions/others": h.revokeOthers,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

// sessionCookie returns the browser session cookie when the request is a
// browser session (no Authorization header, no token principal).
func sessionCookie(r *http.Request, p identity.Principal) string {
	if p.TokenID != "" || len(r.Header.Values("Authorization")) > 0 {
		return ""
	}
	c, err := r.Cookie(oidclogin.SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// classify maps a session issuer to the sign-in method and the operator
// configuration of its identity provider. Nothing here comes from the request.
func (h *meHandlers) classify(ctx context.Context, issuer string) (accountlinks.Method, string, string, accountlinks.Provider) {
	if issuer == localadmin.Issuer {
		return accountlinks.MethodLocal, "Local password", "", accountlinks.Provider{}
	}
	prov := accountlinks.Provider{Issuer: issuer}
	if h.pool != nil && h.b != nil && h.b.dyn != nil {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var id, kind, name string
		var raw []byte
		err := h.pool.QueryRow(qctx, `SELECT id::text,kind,name,config FROM identity_providers WHERE instance_id=$1 AND ((kind='oidc' AND config->>'issuer'=$2) OR (kind='saml' AND config->>'idp_entity_id'=$2) OR (kind='ldap' AND config->>'provider_id'=$2)) ORDER BY enabled DESC LIMIT 1`, h.b.dyn.instance, issuer).Scan(&id, &kind, &name, &raw)
		if err == nil {
			var cfg struct {
				Issuer            string   `json:"issuer"`
				AllowedEndpoints  []string `json:"allowed_endpoints"`
				PasswordChangeURL string   `json:"password_change_url"`
			}
			_ = json.Unmarshal(raw, &cfg)
			prov.Name, prov.Configured, prov.Endpoints = name, cfg.PasswordChangeURL, cfg.AllowedEndpoints
			switch kind {
			case "saml":
				return accountlinks.MethodSAML, name, id, prov
			case "ldap":
				return accountlinks.MethodLDAP, name, id, prov
			}
			return accountlinks.MethodOIDC, name, id, prov
		}
	}
	// File-configured providers have no stored name; label them by host.
	label := issuer
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		label = u.Host
	}
	prov.Name = label
	if h.b != nil {
		client, ent := h.b.cur()
		switch {
		case client != nil && client.Provider != nil && client.Provider.Issuer() == issuer:
			return accountlinks.MethodOIDC, label, "", prov
		case ent != nil && ent.samlIssuer == issuer:
			return accountlinks.MethodSAML, label, "", prov
		case ent != nil && ent.ldapIssuer == issuer:
			return accountlinks.MethodLDAP, label, "", prov
		}
	}
	// Unknown issuer (provider removed since sign-in): no link, no claim.
	return accountlinks.MethodOIDC, label, "", prov
}

func (h *meHandlers) me(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok {
		management.Error(w, r, 401, api.ErrorCodeUnauthenticated, "authentication required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := meResponse{PrincipalID: p.PrincipalID, Roles: []meRole{}}
	if h.pool.QueryRow(ctx, `SELECT kind,subject FROM principals WHERE instance_id=$1 AND id=$2`, p.InstanceID, p.PrincipalID).Scan(&out.Kind, &out.Subject) != nil {
		management.Error(w, r, 503, api.ErrorCodeUnavailable, "account unavailable")
		return
	}
	out.DisplayName = out.Subject
	rows, err := h.pool.Query(ctx, `SELECT r.id::text,r.name FROM principal_roles pr JOIN roles r ON (r.instance_id,r.id)=(pr.instance_id,pr.role_id) WHERE pr.instance_id=$1 AND pr.principal_id=$2 ORDER BY r.name`, p.InstanceID, p.PrincipalID)
	if err != nil {
		management.Error(w, r, 503, api.ErrorCodeUnavailable, "account unavailable")
		return
	}
	for rows.Next() {
		var role meRole
		if rows.Scan(&role.ID, &role.Name) == nil {
			out.Roles = append(out.Roles, role)
		}
	}
	rows.Close()
	raw := sessionCookie(r, p)
	var details oidclogin.Details
	var derr error = errors.New("no browser session")
	if raw != "" && h.b != nil {
		client, _ := h.b.cur()
		details, derr = client.Sessions.Describe(ctx, raw)
	}
	if derr != nil {
		out.AuthMethod = accountlinks.MethodToken
		out.PasswordChange = accountlinks.For(accountlinks.MethodToken, accountlinks.Provider{})
		management.JSON(w, 200, out)
		return
	}
	method, name, id, prov := h.classify(ctx, details.Issuer)
	out.AuthMethod, out.IdP, out.IdPID = method, name, id
	if method == accountlinks.MethodLocal {
		out.IdP = ""
	}
	out.Email, out.Picture = details.Email, details.Picture
	if details.Name != "" {
		out.DisplayName = details.Name
	} else if details.Email != "" {
		out.DisplayName = details.Email
	}
	exp := details.ExpiresAt
	out.SessionExpiresAt = &exp
	out.PasswordChange = accountlinks.For(method, prov)
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2 AND expires_at>clock_timestamp() AND last_seen_at+idle_seconds*interval '1 second'>clock_timestamp()`, p.InstanceID, p.PrincipalID).Scan(&out.ActiveSessions)
	management.JSON(w, 200, out)
}

// allow bounds password-change attempts per principal (10 per minute) on top
// of the per-account lockout that wrong passwords feed.
func (h *meHandlers) allow(principal string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	a, ok := h.hits[principal]
	if !ok || now.Sub(a.start) >= time.Minute {
		if len(h.hits) >= 4096 {
			for k, v := range h.hits {
				if now.Sub(v.start) >= time.Minute {
					delete(h.hits, k)
				}
			}
		}
		a = loginAttempt{start: now}
	}
	if a.count >= 10 {
		return false
	}
	a.count++
	h.hits[principal] = a
	return true
}

func (h *meHandlers) password(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.PrincipalFromContext(r.Context())
	raw := ""
	if ok {
		raw = sessionCookie(r, p)
	}
	if !ok || raw == "" || h.b == nil || h.b.dyn == nil || h.b.dyn.local == nil {
		management.Error(w, r, 403, api.ErrorCodeForbidden, "password change is available to signed-in local users only")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client, _ := h.b.cur()
	details, err := client.Sessions.Describe(ctx, raw)
	if err != nil || details.Issuer != localadmin.Issuer {
		management.Error(w, r, 403, api.ErrorCodeForbidden, "your password is managed by your identity provider")
		return
	}
	if !h.allow(p.PrincipalID) {
		w.Header().Set("Retry-After", "60")
		management.Error(w, r, 429, api.ErrorCodeRateLimited, "too many password change attempts")
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		management.Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || in.Current == "" || in.New == "" {
		management.Error(w, r, 400, api.ErrorCodeInvalidRequest, "current_password and new_password are required")
		return
	}
	_, err = h.b.dyn.local.ChangeOwnPassword(ctx, p.PrincipalID, in.Current, in.New, oidclogin.VerifierHash(raw))
	in.Current, in.New = "", ""
	switch {
	case err == nil:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, localadmin.ErrWrongPassword):
		management.Error(w, r, 403, api.ErrorCodeForbidden, "current password is incorrect")
	case errors.Is(err, localadmin.ErrLocked):
		w.Header().Set("Retry-After", "900")
		management.Error(w, r, 429, api.ErrorCodeRateLimited, "account temporarily locked after repeated failures")
	case errors.Is(err, localadmin.ErrPasswordPolicy), errors.Is(err, localadmin.ErrSamePassword):
		management.Error(w, r, 400, api.ErrorCodeInvalidRequest, err.Error())
	case errors.Is(err, localadmin.ErrNotFound):
		management.Error(w, r, 403, api.ErrorCodeForbidden, "this account has no local password")
	default:
		management.Error(w, r, 503, api.ErrorCodeUnavailable, "password change unavailable")
	}
}

func (h *meHandlers) revokeOthers(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.PrincipalFromContext(r.Context())
	raw := ""
	if ok {
		raw = sessionCookie(r, p)
	}
	if !ok || raw == "" || h.b == nil {
		management.Error(w, r, 403, api.ErrorCodeForbidden, "available to browser sessions only")
		return
	}
	client, _ := h.b.cur()
	n, err := client.Sessions.RevokeOthers(r.Context(), p.PrincipalID, raw)
	if err != nil {
		management.Error(w, r, 503, api.ErrorCodeUnavailable, "sessions unavailable")
		return
	}
	management.JSON(w, 200, map[string]int{"revoked": n})
}
