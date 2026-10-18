package management

// Console administration reads: token list, identity bindings and audit log
// read/export. All require instance administration.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// ---- Tokens ----

type TokenRecord struct {
	ID          string           `json:"id"`
	PrincipalID string           `json:"principal_id"`
	Scopes      []identity.Scope `json:"scopes"`
	Status      string           `json:"status"`
	CreatedAt   time.Time        `json:"created_at"`
	ExpiresAt   time.Time        `json:"expires_at"`
	RevokedAt   *time.Time       `json:"revoked_at,omitempty"`
	LastUsedAt  *time.Time       `json:"last_used_at,omitempty"`
}
type TokenPage struct {
	Items []TokenRecord `json:"items"`
	Page  api.PageInfo  `json:"page"`
}

const tokenStatusSQL = `CASE WHEN revoked_at IS NOT NULL THEN 'revoked' WHEN expires_at<=clock_timestamp() THEN 'expired' ELSE 'active' END`

// listTokens never selects the verifier column, so secrets cannot leak.
func (h consoleHandlers) listTokens(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "principal", "status", "limit", "cursor") {
		return
	}
	values := r.URL.Query()
	principal, status := values.Get("principal"), values.Get("status")
	if principal != "" {
		var ok bool
		if principal, ok = feedID(principal); !ok {
			identityInput(w, r)
			return
		}
	}
	if status != "" && status != "active" && status != "expired" && status != "revoked" {
		identityInput(w, r)
		return
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	scope := scopeHash("tokens", p.InstanceID, p.PrincipalID, p.TokenID, principal, status)
	after := ""
	if c := values.Get("cursor"); c != "" {
		var err error
		after, err = h.codec.Decode(scope, c)
		if _, valid := feedID(after); err != nil || !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT id::text,principal_id::text,scopes,created_at,expires_at,revoked_at,last_used_at,`+tokenStatusSQL+`
 FROM service_tokens WHERE instance_id=$1 AND ($2='' OR id>NULLIF($2,'')::uuid) AND ($3='' OR principal_id=NULLIF($3,'')::uuid)
 AND ($4='' OR $4=`+tokenStatusSQL+`) ORDER BY id LIMIT $5`, p.InstanceID, after, principal, status, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	page := TokenPage{Items: []TokenRecord{}}
	for rows.Next() {
		var t TokenRecord
		var scopes []byte
		if err = rows.Scan(&t.ID, &t.PrincipalID, &scopes, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt, &t.LastUsedAt, &t.Status); err != nil || json.Unmarshal(scopes, &t.Scopes) != nil {
			identityError(w, r, identity.ErrUnavailable)
			return
		}
		page.Items = append(page.Items, t)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		token, e := h.codec.Encode(scope, page.Items[limit-1].ID)
		if e != nil {
			Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
			return
		}
		page.Page.HasMore, page.Page.NextCursor = true, &token
	}
	JSON(w, 200, page)
}

// ---- Identity bindings ----

// bindingIDSQL derives a stable, length-prefixed (unambiguous) identifier from
// the (issuer, subject) primary key, so arbitrary subjects never appear in URLs.
const bindingIDSQL = `encode(sha256(convert_to(length(issuer)::text||':'||issuer||subject,'UTF8')),'hex')`

type IdentityBindingRecord struct {
	ID          string `json:"id"`
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	PrincipalID string `json:"principal_id"`
}
type IdentityBindingPage struct {
	Items []IdentityBindingRecord `json:"items"`
	Page  api.PageInfo            `json:"page"`
}

func (h consoleHandlers) listBindings(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "principal", "limit", "cursor") {
		return
	}
	values := r.URL.Query()
	principal := values.Get("principal")
	if principal != "" {
		var ok bool
		if principal, ok = feedID(principal); !ok {
			identityInput(w, r)
			return
		}
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	scope := scopeHash("identity-bindings", p.InstanceID, p.PrincipalID, p.TokenID, principal)
	after := ""
	if c := values.Get("cursor"); c != "" {
		var err error
		after, err = h.codec.Decode(scope, c)
		if err != nil || len(after) != 64 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT * FROM (SELECT `+bindingIDSQL+` AS id,issuer,subject,principal_id::text FROM oidc_identity_bindings WHERE instance_id=$1 AND ($3='' OR principal_id=NULLIF($3,'')::uuid)) b WHERE $2='' OR id>$2 ORDER BY id LIMIT $4`, p.InstanceID, after, principal, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	page := IdentityBindingPage{Items: []IdentityBindingRecord{}}
	for rows.Next() {
		var b IdentityBindingRecord
		if err = rows.Scan(&b.ID, &b.Issuer, &b.Subject, &b.PrincipalID); err != nil {
			identityError(w, r, err)
			return
		}
		page.Items = append(page.Items, b)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		token, e := h.codec.Encode(scope, page.Items[limit-1].ID)
		if e != nil {
			Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
			return
		}
		page.Page.HasMore, page.Page.NextCursor = true, &token
	}
	JSON(w, 200, page)
}

type IdentityBindingDeleted struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// deleteBinding removes the binding; browser sessions that depend on it are
// removed by the database (ON DELETE CASCADE), ending those logins.
func (h consoleHandlers) deleteBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		identityInput(w, r)
		return
	}
	h.mutate(w, r, p, "identity.unbind", id, struct{ ID string }{id}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var principal string
		err := tx.QueryRow(ctx, `DELETE FROM oidc_identity_bindings WHERE instance_id=$1 AND `+bindingIDSQL+`=$2 RETURNING principal_id::text`, p.InstanceID, id).Scan(&principal)
		if err != nil {
			return nil, err
		}
		// The issuer/subject pair is intentionally absent from events.
		return IdentityBindingDeleted{ID: id, Deleted: true}, identityEvent(ctx, tx, p, principal, "identity.unbind", 0)
	})
}

// ---- Audit ----

type AuditEvent struct {
	ID          int64           `json:"id"`
	CreatedAt   time.Time       `json:"created_at"`
	PrincipalID *string         `json:"principal_id,omitempty"`
	Action      string          `json:"action"`
	ResourceID  *string         `json:"resource_id,omitempty"`
	Result      string          `json:"result"`
	Details     json.RawMessage `json:"details"`
}
type AuditPage struct {
	Items []AuditEvent `json:"items"`
	Page  api.PageInfo `json:"page"`
}
type auditFilter struct {
	Principal, Action, Result string
	From, To                  *time.Time
}

func (f auditFilter) scope(p identity.Principal, kind string) string {
	var from, to string
	if f.From != nil {
		from = f.From.UTC().Format(time.RFC3339Nano)
	}
	if f.To != nil {
		to = f.To.UTC().Format(time.RFC3339Nano)
	}
	return scopeHash(kind, p.InstanceID, p.PrincipalID, p.TokenID, f.Principal, f.Action, f.Result, from, to)
}

func parseAuditFilter(w http.ResponseWriter, r *http.Request) (auditFilter, bool) {
	values := r.URL.Query()
	f := auditFilter{Principal: values.Get("principal"), Action: values.Get("action"), Result: values.Get("result")}
	bad := func() (auditFilter, bool) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid audit filter")
		return f, false
	}
	if f.Principal != "" {
		var ok bool
		if f.Principal, ok = feedID(f.Principal); !ok {
			return bad()
		}
	}
	if len(f.Action) > 128 || !validText(f.Action, 128) || (f.Result != "" && f.Result != "success" && f.Result != "denied" && f.Result != "failure") {
		return bad()
	}
	for name, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if v := values.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return bad()
			}
			*dst = &t
		}
	}
	if f.From != nil && f.To != nil && !f.To.After(*f.From) {
		return bad()
	}
	return f, true
}

const auditSelect = `SELECT id,created_at,principal_id::text,action,resource_id::text,result,details FROM audit_events
 WHERE instance_id=$1 AND ($2::bigint=0 OR id<$2) AND ($3='' OR principal_id=NULLIF($3,'')::uuid) AND ($4='' OR action=$4) AND ($5='' OR result=$5)
 AND ($6::timestamptz IS NULL OR created_at>=$6) AND ($7::timestamptz IS NULL OR created_at<$7) ORDER BY id DESC LIMIT $8`

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryAudit(ctx context.Context, db queryer, instance string, before int64, f auditFilter, limit int) ([]AuditEvent, error) {
	rows, err := db.Query(ctx, auditSelect, instance, before, f.Principal, f.Action, f.Result, f.From, f.To, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err = rows.Scan(&e.ID, &e.CreatedAt, &e.PrincipalID, &e.Action, &e.ResourceID, &e.Result, &e.Details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (h consoleHandlers) listAudit(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "principal", "action", "result", "from", "to", "limit", "cursor") {
		return
	}
	filter, ok := parseAuditFilter(w, r)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	scope := filter.scope(p, "audit")
	var before int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		after, err := h.codec.Decode(scope, c)
		if err == nil {
			before, err = strconv.ParseInt(after, 10, 64)
		}
		if err != nil || before < 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := queryAudit(ctx, h.db, p.InstanceID, before, filter, limit+1)
	if err != nil {
		identityError(w, r, err)
		return
	}
	page := AuditPage{Items: items}
	if page.Items == nil {
		page.Items = []AuditEvent{}
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		token, e := h.codec.Encode(scope, strconv.FormatInt(page.Items[limit-1].ID, 10))
		if e != nil {
			Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
			return
		}
		page.Page.HasMore, page.Page.NextCursor = true, &token
	}
	JSON(w, 200, page)
}

// csvSafe neutralizes spreadsheet formula injection in exported text cells.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// exportAudit streams every matching event, newest first, in keyset batches so
// memory stays bounded. The export itself is recorded in the audit log first.
func (h consoleHandlers) exportAudit(w http.ResponseWriter, r *http.Request) {
	if !singleParams(w, r, "principal", "action", "result", "from", "to", "format") {
		return
	}
	filter, ok := parseAuditFilter(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "ndjson"
	}
	if format != "csv" && format != "ndjson" {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "format must be csv or ndjson")
		return
	}
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	details, _ := json.Marshal(map[string]any{"format": format, "principal": filter.Principal, "action": filter.Action, "result": filter.Result})
	if _, err := h.db.Exec(r.Context(), `INSERT INTO audit_events(instance_id,principal_id,action,details) VALUES($1,$2,'audit.export',$3)`, p.InstanceID, p.PrincipalID, details); err != nil {
		identityError(w, r, err)
		return
	}
	name := "audit-" + time.Now().UTC().Format("20060102T150405Z")
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		name += ".csv"
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
		name += ".ndjson"
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	cw := csv.NewWriter(w)
	if format == "csv" {
		_ = cw.Write([]string{"id", "created_at", "principal_id", "action", "resource_id", "result", "details"})
	}
	enc := json.NewEncoder(w)
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	var before int64
	for {
		// The server write timeout is per response; extend it for each batch.
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		items, err := queryAudit(ctx, h.db, p.InstanceID, before, filter, 500)
		cancel()
		if err != nil || len(items) == 0 {
			// Headers are already sent; a failed batch truncates the stream.
			break
		}
		for _, e := range items {
			if format == "csv" {
				_ = cw.Write([]string{strconv.FormatInt(e.ID, 10), e.CreatedAt.UTC().Format(time.RFC3339Nano), str(e.PrincipalID), csvSafe(e.Action), str(e.ResourceID), e.Result, csvSafe(string(e.Details))})
			} else if enc.Encode(e) != nil {
				return
			}
		}
		cw.Flush()
		if cw.Error() != nil {
			return
		}
		_ = rc.Flush()
		before = items[len(items)-1].ID
		if len(items) < 500 {
			break
		}
	}
	cw.Flush()
	_ = rc.Flush()
}
