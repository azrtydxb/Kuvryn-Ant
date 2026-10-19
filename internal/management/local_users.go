package management

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
)

// LocalUser is a username/password sign-in account.
type LocalUser struct {
	Username    string     `json:"username"`
	PrincipalID string     `json:"principal_id"`
	BreakGlass  bool       `json:"break_glass"`
	TOTPEnabled bool       `json:"totp_enabled"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

type LocalUserList struct {
	Items []LocalUser `json:"items"`
}

type LocalUserInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LocalUserPassword struct {
	Password string `json:"password"`
}

// RegisterLocalUsers mounts local (username/password) account administration.
// Every route requires instance Admin. Role assignment uses the principal roles
// API; passwords are never stored in, or returned by, the idempotency store.
func RegisterLocalUsers(mux *http.ServeMux, d IdentityAdminDeps) {
	h := idpHandlers{identityHandlers{feedHandlers{db: d.Pool}}, d}
	bearer := identity.NewBearer(identity.Tokens{DB: d.Pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/local-accounts":                     h.localUsers,
		"POST /api/v1/local-accounts":                    h.localUserCreate,
		"PUT /api/v1/local-accounts/{username}/password": h.localUserPassword,
		"DELETE /api/v1/local-accounts/{username}":       h.localUserDelete,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

func (h idpHandlers) localUserError(err error) error {
	switch {
	case errors.Is(err, localadmin.ErrUsername), errors.Is(err, localadmin.ErrPasswordPolicy):
		return reqError{400, api.ErrorCodeInvalidRequest, err.Error()}
	case errors.Is(err, localadmin.ErrExists):
		return reqError{409, api.ErrorCodeConflict, err.Error()}
	case errors.Is(err, localadmin.ErrNotFound):
		return reqError{404, api.ErrorCodeNotFound, "local account not found"}
	case errors.Is(err, localadmin.ErrReserved):
		return reqError{409, api.ErrorCodeConflict, err.Error()}
	default:
		return reqError{503, api.ErrorCodeUnavailable, "local accounts unavailable"}
	}
}

func (h idpHandlers) localUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	users, err := h.local(p).ListUsers(ctx)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "local accounts unavailable")
		return
	}
	out := LocalUserList{Items: make([]LocalUser, 0, len(users))}
	for _, u := range users {
		out.Items = append(out.Items, LocalUser(u))
	}
	JSON(w, 200, out)
}

func (h idpHandlers) localUserCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	var in LocalUserInput
	if !h.decode(w, r, &in) {
		return
	}
	// Only a digest of the password takes part in the request fingerprint.
	sum := sha256.Sum256([]byte(in.Password))
	out, ok := h.idem(w, r, p, "local_account.create", "local-accounts", struct {
		Username string
		Password [32]byte
	}{in.Username, sum}, func(ctx context.Context) (any, error) {
		u, err := h.local(p).CreateUser(ctx, p.PrincipalID, in.Username, in.Password, false)
		if err != nil {
			return nil, h.localUserError(err)
		}
		return LocalUser(u), nil
	})
	in.Password = ""
	if ok {
		replayWrite(w, r, out, 201)
	}
}

func (h idpHandlers) localUserPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	var in LocalUserPassword
	if !h.decode(w, r, &in) {
		return
	}
	username := r.PathValue("username")
	sum := sha256.Sum256([]byte(in.Password))
	out, ok := h.idem(w, r, p, "local_account.password", username, struct{ Password [32]byte }{sum}, func(ctx context.Context) (any, error) {
		if err := h.local(p).SetUserPassword(ctx, p.PrincipalID, username, in.Password); err != nil {
			return nil, h.localUserError(err)
		}
		return struct{}{}, nil
	})
	in.Password = ""
	if ok {
		replayWrite(w, r, out, 204)
	}
}

func (h idpHandlers) localUserDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok || !h.ready(w, r) {
		return
	}
	username := r.PathValue("username")
	out, ok := h.idem(w, r, p, "local_account.delete", username, struct{}{}, func(ctx context.Context) (any, error) {
		if err := h.local(p).DeleteUser(ctx, p.PrincipalID, username); err != nil {
			return nil, h.localUserError(err)
		}
		return struct{}{}, nil
	})
	if ok {
		replayWrite(w, r, out, 204)
	}
}
