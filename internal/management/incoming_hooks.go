package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/hooks"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IncomingHook is the management view of an incoming webhook. The URL token of
// a url_token hook is never stored or returned again: Token is present only in
// the response of the call that created or regenerated it.
type IncomingHook struct {
	ID                string       `json:"id"`
	Slug              string       `json:"slug"`
	Description       string       `json:"description"`
	Enabled           bool         `json:"enabled"`
	AuthMode          string       `json:"auth_mode"`
	HMACFormat        string       `json:"hmac_format,omitempty"`
	SigningSecretID   string       `json:"signing_secret_id,omitempty"`
	SigningSecretName string       `json:"signing_secret_name,omitempty"`
	BearerScope       string       `json:"bearer_scope,omitempty"`
	TokenHint         string       `json:"token_hint,omitempty"`
	Token             string       `json:"token,omitempty"`
	Action            hooks.Action `json:"action"`
	ActionSummary     string       `json:"action_summary"`
	RatePerMinute     int          `json:"rate_per_minute"`
	Path              string       `json:"path"`
	URL               string       `json:"url,omitempty"`
	Revision          int64        `json:"revision"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
	LastReceivedAt    *time.Time   `json:"last_received_at,omitempty"`
	Received7d        int          `json:"received_7d"`
	Succeeded7d       int          `json:"succeeded_7d"`
	Failed7d          int          `json:"failed_7d"`
	SuccessRate       *float64     `json:"success_rate,omitempty"`
}

// IncomingHookDelivery is one logged call of an incoming hook.
type IncomingHookDelivery struct {
	ID           string         `json:"id"`
	ReceivedAt   time.Time      `json:"received_at"`
	SourceIP     string         `json:"source_ip,omitempty"`
	AuthResult   string         `json:"auth_result"`
	State        string         `json:"state"`
	Attempts     int            `json:"attempts"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
	ActionResult string         `json:"action_result,omitempty"`
	Detail       map[string]any `json:"detail"`
	Error        string         `json:"error,omitempty"`
	PayloadSize  int            `json:"payload_size"`
	Preview      string         `json:"preview"`
	DeliveryRef  string         `json:"delivery_ref,omitempty"`
	RedeliveryOf string         `json:"redelivery_of,omitempty"`
}

// IncomingHookFormat describes one signature format.
type IncomingHookFormat struct {
	Format string `json:"format"`
	Header string `json:"header"`
}

// IncomingHookConfig is what the console needs to build hook URLs and forms.
type IncomingHookConfig struct {
	PublicURL        string               `json:"public_url"`
	HookBaseURL      string               `json:"hook_base_url"`
	MaxBodyBytes     int64                `json:"max_body_bytes"`
	LogRetentionDays int                  `json:"log_retention_days"`
	HMACFormats      []IncomingHookFormat `json:"hmac_formats"`
}

type incomingHandlers struct {
	identityHandlers
	svc  *hooks.Service
	opts WebhookOptions
}

// RegisterIncomingHooks mounts the administration API of incoming webhooks.
// The public receiver itself (/hook/...) is hooks.Service.Register.
func RegisterIncomingHooks(mux *http.ServeMux, pool *pgxpool.Pool, svc *hooks.Service, options ...WebhookOptions) {
	h := incomingHandlers{identityHandlers: identityHandlers{feedHandlers{db: pool}}, svc: svc}
	if len(options) > 0 {
		h.opts = options[0]
	}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/incoming-hook-config":                                    h.config,
		"GET /api/v1/incoming-hooks":                                          h.list,
		"POST /api/v1/incoming-hooks":                                         h.create,
		"GET /api/v1/incoming-hooks/{id}":                                     h.get,
		"PATCH /api/v1/incoming-hooks/{id}":                                   h.patch,
		"DELETE /api/v1/incoming-hooks/{id}":                                  h.delete,
		"POST /api/v1/incoming-hooks/{id}/token":                              h.regenerateToken,
		"GET /api/v1/incoming-hooks/{id}/deliveries":                          h.deliveries,
		"POST /api/v1/incoming-hooks/{id}/test":                               h.test,
		"POST /api/v1/incoming-hooks/{id}/deliveries/{delivery_id}/redeliver": h.redeliver,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

func (h incomingHandlers) view(v hooks.HookView, token string) IncomingHook {
	out := IncomingHook{ID: v.ID, Slug: v.Slug, Description: v.Description, Enabled: v.Enabled, AuthMode: v.AuthMode, HMACFormat: v.HMACFormat,
		SigningSecretID: v.SigningSecretID, SigningSecretName: v.SecretName, BearerScope: v.BearerScope, Action: v.Action, ActionSummary: hooks.Summary(v.Action),
		RatePerMinute: v.RatePerMinute, Path: "/hook/" + v.Slug, Revision: v.Revision, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		LastReceivedAt: v.LastReceivedAt, Received7d: v.Received7d, Succeeded7d: v.Succeeded7d, Failed7d: v.Failed7d, Token: token}
	if v.AuthMode == hooks.AuthURLToken {
		out.TokenHint = v.TokenHint
	}
	if base := h.svc.PublicURL(); base != "" {
		out.URL = base + out.Path
	}
	if done := v.Succeeded7d + v.Failed7d; done > 0 {
		rate := float64(v.Succeeded7d) / float64(done)
		out.SuccessRate = &rate
	}
	return out
}

func hookInput(w http.ResponseWriter, r *http.Request, msg string) {
	if msg == "" {
		msg = "invalid incoming hook configuration"
	}
	Error(w, r, 400, api.ErrorCodeInvalidRequest, msg)
}

// hookError maps hooks errors to API errors; ok reports whether it handled err.
func hookError(w http.ResponseWriter, r *http.Request, err error) bool {
	var ve hooks.ValidationError
	switch {
	case err == nil:
		return false
	case errors.As(err, &ve):
		hookInput(w, r, string(ve))
	case errors.Is(err, hooks.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "incoming hook not found")
	default:
		return false
	}
	return true
}

func (h incomingHandlers) config(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	cfg := IncomingHookConfig{PublicURL: h.svc.PublicURL(), MaxBodyBytes: h.svc.MaxBody(), LogRetentionDays: h.svc.LogRetentionDays(), HMACFormats: []IncomingHookFormat{}}
	if cfg.PublicURL != "" {
		cfg.HookBaseURL = cfg.PublicURL + "/hook/"
	}
	for _, f := range []string{hooks.FormatGitHub, hooks.FormatGitLab, hooks.FormatAnt, hooks.FormatNPM, hooks.FormatArtifactory, hooks.FormatNexus} {
		cfg.HMACFormats = append(cfg.HMACFormats, IncomingHookFormat{Format: f, Header: hooks.HMACFormats[f]})
	}
	JSON(w, 200, cfg)
}

func (h incomingHandlers) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "limit") || len(values) != 1 {
			hookInput(w, r, "")
			return
		}
	}
	after := q.Get("after")
	if after != "" && !hooks.ValidSlug(after) {
		hookInput(w, r, "after must be a slug")
		return
	}
	limit := 50
	if q.Has("limit") {
		var err error
		if limit, err = strconv.Atoi(q.Get("limit")); err != nil || limit < 1 || limit > 200 {
			hookInput(w, r, "")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := hooks.ListHooks(ctx, h.db, p.InstanceID, after, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].Slug
	}
	items := make([]IncomingHook, 0, len(rows))
	for _, v := range rows {
		items = append(items, h.view(v, ""))
	}
	JSON(w, 200, struct {
		Items []IncomingHook `json:"items"`
		Next  string         `json:"next_after,omitempty"`
	}{items, next})
}

func (h incomingHandlers) get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		hookInput(w, r, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	v, err := hooks.ViewHook(ctx, h.db, p.InstanceID, id)
	if hookError(w, r, err) {
		return
	}
	if err != nil {
		identityError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(v.Revision, 10)))
	JSON(w, 200, h.view(v, ""))
}

// newURLToken returns a 256-bit random path suffix. Only its SHA-256 is stored.
func newURLToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	defer clear(raw)
	return "hk_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

type incomingInput struct {
	Slug            string        `json:"slug"`
	Description     string        `json:"description"`
	Enabled         *bool         `json:"enabled"`
	AuthMode        string        `json:"auth_mode"`
	HMACFormat      string        `json:"hmac_format"`
	SigningSecretID string        `json:"signing_secret_id"`
	BearerScope     string        `json:"bearer_scope"`
	Action          *hooks.Action `json:"action"`
	RatePerMinute   int           `json:"rate_per_minute"`
}

// patchIncomingInput has a pointer per field, so omitted fields stay as they are.
type patchIncomingInput struct {
	Slug            *string       `json:"slug"`
	Description     *string       `json:"description"`
	Enabled         *bool         `json:"enabled"`
	AuthMode        *string       `json:"auth_mode"`
	HMACFormat      *string       `json:"hmac_format"`
	SigningSecretID *string       `json:"signing_secret_id"`
	BearerScope     *string       `json:"bearer_scope"`
	Action          *hooks.Action `json:"action"`
	RatePerMinute   *int          `json:"rate_per_minute"`
}

var incomingKeys = []string{"slug", "description", "enabled", "auth_mode", "hmac_format", "signing_secret_id", "bearer_scope", "action", "rate_per_minute"}

// hookMutate runs apply as an idempotent audited mutation. A freshly generated
// URL token is added to the response of the first execution only; replays and
// the stored replay record carry the metadata without it.
func (h incomingHandlers) hookMutate(w http.ResponseWriter, r *http.Request, p identity.Principal, operation, resource string, input any, status int, token string, apply func(context.Context, pgx.Tx) (any, error)) {
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	canonical, _ := json.Marshal(input)
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), h.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: operation, Resource: resource, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		v, err := apply(ctx, tx)
		if err != nil {
			applyErr = err
			return nil, identityDBError(err)
		}
		return json.Marshal(v)
	})
	h.svc.Invalidate()
	if err != nil {
		if applyErr != nil && hookError(w, r, applyErr) {
			return
		}
		identityError(w, r, err)
		return
	}
	body := result.JSON
	var meta struct {
		Revision int64 `json:"revision"`
	}
	_ = json.Unmarshal(body, &meta)
	if meta.Revision > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(meta.Revision, 10)))
	}
	if token != "" && !result.Replayed {
		var item IncomingHook
		if json.Unmarshal(body, &item) == nil {
			item.Token = token
			JSON(w, status, item)
			return
		}
	}
	JSON(w, status, body)
}

func (h incomingHandlers) hookFromInput(in incomingInput, creator string) hooks.Hook {
	hk := hooks.Hook{Slug: in.Slug, Description: in.Description, Enabled: true, AuthMode: in.AuthMode, HMACFormat: in.HMACFormat, SigningSecretID: in.SigningSecretID,
		BearerScope: in.BearerScope, RatePerMinute: in.RatePerMinute, CreatedBy: creator}
	if in.Enabled != nil {
		hk.Enabled = *in.Enabled
	}
	if in.Action != nil {
		hk.Action = *in.Action
	}
	return hk
}

func (h incomingHandlers) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in incomingInput
	if !sourceJSON(w, r, &in, incomingKeys...) {
		return
	}
	hk := h.hookFromInput(in, p.PrincipalID)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := hooks.Validate(ctx, h.db, p.InstanceID, &hk, h.opts.AllowHTTP)
	cancel()
	if hookError(w, r, err) {
		return
	}
	if err != nil {
		identityError(w, r, err)
		return
	}
	token := ""
	if hk.AuthMode == hooks.AuthURLToken {
		var terr error
		if token, terr = newURLToken(); terr != nil {
			identityError(w, r, terr)
			return
		}
	}
	// The fingerprint covers the request, not the generated token.
	h.hookMutate(w, r, p, "incoming_hook.create", "incoming-hooks", struct {
		In  incomingInput
		Act hooks.Action
	}{in, hk.Action}, 201, token, func(ctx context.Context, tx pgx.Tx) (any, error) {
		id, err := hooks.InsertHook(ctx, tx, p.InstanceID, hk, token)
		if err != nil {
			return nil, err
		}
		v, err := hooks.ViewHook(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		return h.view(v, ""), identityEvent(ctx, tx, p, id, "incoming_hook.create", v.Revision)
	})
}

func (h incomingHandlers) patch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	var in patchIncomingInput
	if !sourceJSON(w, r, &in, incomingKeys...) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	current, err := hooks.GetHook(ctx, h.db, p.InstanceID, id)
	if err != nil {
		cancel()
		if hookError(w, r, err) {
			return
		}
		identityError(w, r, err)
		return
	}
	next := current
	if in.Slug != nil {
		next.Slug = *in.Slug
	}
	if in.Description != nil {
		next.Description = *in.Description
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.AuthMode != nil {
		next.AuthMode = *in.AuthMode
	}
	if in.HMACFormat != nil {
		next.HMACFormat = *in.HMACFormat
	}
	if in.SigningSecretID != nil {
		next.SigningSecretID = *in.SigningSecretID
	}
	if in.BearerScope != nil {
		next.BearerScope = *in.BearerScope
	}
	if in.RatePerMinute != nil {
		next.RatePerMinute = *in.RatePerMinute
	}
	if in.Action != nil {
		next.Action = *in.Action
	}
	err = hooks.Validate(ctx, h.db, p.InstanceID, &next, h.opts.AllowHTTP)
	cancel()
	if hookError(w, r, err) {
		return
	}
	if err != nil {
		identityError(w, r, err)
		return
	}
	token := ""
	if next.AuthMode == hooks.AuthURLToken && current.AuthMode != hooks.AuthURLToken {
		var terr error
		if token, terr = newURLToken(); terr != nil {
			identityError(w, r, terr)
			return
		}
	}
	h.hookMutate(w, r, p, "incoming_hook.patch", id, struct {
		In       patchIncomingInput
		Act      hooks.Action
		Revision int64
	}{in, next.Action, revision}, 200, token, func(ctx context.Context, tx pgx.Tx) (any, error) {
		locked, err := hooks.LockHook(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		if locked.Revision != revision {
			return nil, database.ErrRevisionConflict
		}
		next.ID, next.TokenHash, next.TokenHint, next.CreatedBy = id, locked.TokenHash, locked.TokenHint, locked.CreatedBy
		if err = hooks.UpdateHook(ctx, tx, p.InstanceID, next, token); err != nil {
			return nil, err
		}
		v, err := hooks.ViewHook(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		return h.view(v, ""), identityEvent(ctx, tx, p, id, "incoming_hook.patch", v.Revision)
	})
}

func (h incomingHandlers) delete(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	h.hookMutate(w, r, p, "incoming_hook.delete", id, struct{ Revision int64 }{revision}, 200, "", func(ctx context.Context, tx pgx.Tx) (any, error) {
		locked, err := hooks.LockHook(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		if locked.Revision != revision {
			return nil, database.ErrRevisionConflict
		}
		if err = hooks.DeleteHook(ctx, tx, p.InstanceID, id); err != nil {
			return nil, err
		}
		return struct {
			ID       string `json:"id"`
			Deleted  bool   `json:"deleted"`
			Revision int64  `json:"revision"`
		}{id, true, locked.Revision}, identityEvent(ctx, tx, p, id, "incoming_hook.delete", locked.Revision)
	})
}

func (h incomingHandlers) regenerateToken(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, revision, ok := identityTarget(w, r)
	if !ok {
		return
	}
	token, err := newURLToken()
	if err != nil {
		identityError(w, r, err)
		return
	}
	h.hookMutate(w, r, p, "incoming_hook.token", id, struct{ Revision int64 }{revision}, 200, token, func(ctx context.Context, tx pgx.Tx) (any, error) {
		locked, err := hooks.LockHook(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		if locked.Revision != revision {
			return nil, database.ErrRevisionConflict
		}
		if locked.AuthMode != hooks.AuthURLToken {
			return nil, hooks.ValidationError("only hooks that authenticate with a secret token in the URL have a token")
		}
		if err = hooks.UpdateHook(ctx, tx, p.InstanceID, locked, token); err != nil {
			return nil, err
		}
		v, err := hooks.ViewHook(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		return h.view(v, ""), identityEvent(ctx, tx, p, id, "incoming_hook.token", v.Revision)
	})
}

func (h incomingHandlers) deliveries(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		hookInput(w, r, "")
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "before" && key != "limit" && key != "state") || len(values) != 1 {
			hookInput(w, r, "")
			return
		}
	}
	before := ""
	if q.Has("before") {
		if before, ok = feedID(q.Get("before")); !ok {
			hookInput(w, r, "")
			return
		}
	}
	limit := 50
	if q.Has("limit") {
		var err error
		if limit, err = strconv.Atoi(q.Get("limit")); err != nil || limit < 1 || limit > 200 {
			hookInput(w, r, "")
			return
		}
	}
	state := q.Get("state")
	switch state {
	case "", hooks.StatePending, hooks.StateRunning, hooks.StateSucceeded, hooks.StateFailed, hooks.StateRejected:
	default:
		hookInput(w, r, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := hooks.GetHook(ctx, h.db, p.InstanceID, id); err != nil {
		if hookError(w, r, err) {
			return
		}
		identityError(w, r, err)
		return
	}
	rows, err := hooks.ListDeliveries(ctx, h.db, p.InstanceID, id, before, state, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].ID
	}
	items := make([]IncomingHookDelivery, 0, len(rows))
	for _, d := range rows {
		if d.Detail == nil {
			d.Detail = map[string]any{}
		}
		src := d.SourceIP
		items = append(items, IncomingHookDelivery{ID: d.ID, ReceivedAt: d.ReceivedAt, SourceIP: src, AuthResult: d.AuthResult, State: d.State, Attempts: d.Attempts, FinishedAt: d.FinishedAt,
			ActionResult: d.ActionResult, Detail: d.Detail, Error: d.Error, PayloadSize: d.PayloadSize, Preview: d.Preview, DeliveryRef: d.DeliveryRef, RedeliveryOf: d.RedeliveryOf})
	}
	JSON(w, 200, struct {
		Items []IncomingHookDelivery `json:"items"`
		Next  string                 `json:"next_before,omitempty"`
	}{items, next})
}

// IncomingHookQueued is the answer of test and redeliver.
type IncomingHookQueued struct {
	DeliveryID string `json:"delivery_id"`
}

func (h incomingHandlers) queue(w http.ResponseWriter, r *http.Request, operation string, resource func(id string) string, enqueue func(ctx context.Context, tx pgx.Tx, hook hooks.Hook) (string, error)) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := feedID(r.PathValue("id"))
	if !ok {
		hookInput(w, r, "")
		return
	}
	hook, err := hooks.GetHook(r.Context(), h.db, p.InstanceID, id)
	if hookError(w, r, err) {
		return
	}
	if err != nil {
		identityError(w, r, err)
		return
	}
	var queued string
	h.hookMutate(w, r, p, operation, resource(id), struct{ ID string }{resource(id)}, 202, "", func(ctx context.Context, tx pgx.Tx) (any, error) {
		d, err := enqueue(ctx, tx, hook)
		if err != nil {
			return nil, err
		}
		queued = d
		return IncomingHookQueued{DeliveryID: d}, identityEvent(ctx, tx, p, id, operation, 0)
	})
	if queued != "" {
		h.svc.Notify()
	}
}

func (h incomingHandlers) test(w http.ResponseWriter, r *http.Request) {
	h.queue(w, r, "incoming_hook.test", func(id string) string { return id }, func(ctx context.Context, tx pgx.Tx, hook hooks.Hook) (string, error) {
		return h.svc.SendTest(ctx, tx, hook)
	})
}

func (h incomingHandlers) redeliver(w http.ResponseWriter, r *http.Request) {
	delivery, ok := feedID(r.PathValue("delivery_id"))
	if !ok {
		hookInput(w, r, "")
		return
	}
	h.queue(w, r, "incoming_hook.redeliver", func(id string) string { return id + "/" + delivery }, func(ctx context.Context, tx pgx.Tx, hook hooks.Hook) (string, error) {
		return h.svc.Redeliver(ctx, tx, hook, delivery)
	})
}
