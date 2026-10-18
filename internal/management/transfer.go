package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/transfer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TransferDeps carries the collaborators of the import/export and migration API.
// Stores may be nil (artifact serving inactive); content is then never written.
type TransferDeps struct {
	Pool   *pgxpool.Pool
	Stores transfer.Stores
}

type transferHandlers struct {
	identityHandlers
	d TransferDeps
}

const (
	maxTransferBody     = 64 << 20
	transferTimeout     = 5 * time.Minute
	maxMigrationAssets  = 100000
	transferRunsPerPage = 50
)

// RegisterTransfer mounts the import/export and migration API. Every route
// requires a bearer token with instance Admin.
func RegisterTransfer(mux *http.ServeMux, d TransferDeps) {
	h := transferHandlers{identityHandlers{feedHandlers{db: d.Pool}}, d}
	bearer := identity.NewBearer(identity.Tokens{DB: d.Pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"POST /api/v1/transfer/export":    h.export,
		"POST /api/v1/transfer/import":    h.importDoc,
		"GET /api/v1/transfer/runs":       h.runs,
		"POST /api/v1/migrations/nexus":   h.migrateNexus,
		"POST /api/v1/migrations/compare": h.compare,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTransferBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err = dec.Decode(dst); err == nil {
		var extra any
		if err = dec.Decode(&extra); err == io.EOF {
			return true
		}
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		Error(w, r, 413, api.ErrorCodeInvalidRequest, "request body too large")
	} else {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid JSON body")
	}
	return false
}

func transferError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, transfer.ErrInput):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		Error(w, r, 504, api.ErrorCodeUnavailable, "operation timed out")
	default:
		Error(w, r, 502, api.ErrorCodeUnavailable, "operation failed")
	}
}

// record stores the run history and an audit event. It never stores the report
// itself, only counts, so it cannot hold credentials or content.
func (h transferHandlers) record(ctx context.Context, p identity.Principal, kind string, dry bool, summary any) {
	enc, err := json.Marshal(summary)
	if err != nil || len(enc) > 60000 {
		enc = []byte("{}")
	}
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `INSERT INTO transfer_runs(instance_id,principal_id,kind,dry_run,summary) VALUES($1,$2,$3,$4,$5)`, p.InstanceID, p.PrincipalID, kind, dry, enc); err != nil {
		return
	}
	if !dry {
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,details) VALUES($1,$2,$3,$4)`, p.InstanceID, p.PrincipalID, "transfer."+kind, enc); err != nil {
			return
		}
	}
	_ = tx.Commit(ctx)
}

type ExportRequest struct {
	Feeds          []string `json:"feeds,omitempty"`
	Namespaces     []string `json:"namespaces,omitempty"`
	IncludeContent bool     `json:"include_content,omitempty"`
}

// export returns the definition document. Content is inventory only (digests,
// sizes, metadata); bytes are exported with `ant export`.
func (h transferHandlers) export(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in ExportRequest
	if !decodeBody(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), transferTimeout)
	defer cancel()
	doc, err := transfer.Build(ctx, h.d.Pool, p.InstanceID, transfer.ExportOptions{Selection: transfer.Selection{Feeds: in.Feeds, Namespaces: in.Namespaces}, Content: in.IncludeContent})
	if err != nil {
		transferError(w, r, err)
		return
	}
	h.record(ctx, p, "export", false, map[string]int{"feeds": len(doc.Feeds), "sources": len(doc.Sources), "components": len(doc.Components), "assets": doc.CountAssets()})
	JSON(w, 200, doc)
}

type ImportRequest struct {
	Document transfer.Document `json:"document"`
	// DryRun defaults to true: an apply must be requested explicitly.
	DryRun *bool `json:"dry_run,omitempty"`
}

func (h transferHandlers) stores() transfer.Stores {
	if h.d.Stores == nil {
		return nil
	}
	return h.d.Stores
}

// importDoc imports definitions; assets are only imported when their content is
// already in the target store (bundle content is imported with `ant import`).
func (h transferHandlers) importDoc(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in ImportRequest
	if !decodeBody(w, r, &in) {
		return
	}
	dry := in.DryRun == nil || *in.DryRun
	ctx, cancel := context.WithTimeout(r.Context(), transferTimeout)
	defer cancel()
	rep, err := transfer.Import(ctx, h.d.Pool, h.stores(), p.InstanceID, in.Document, nil, transfer.ImportOptions{DryRun: dry})
	if err != nil {
		transferError(w, r, err)
		return
	}
	h.record(ctx, p, "import", dry, rep.Summary)
	JSON(w, 200, rep)
}

type NexusRequest struct {
	URL          string   `json:"url"`
	Username     string   `json:"username,omitempty"`
	Password     string   `json:"password,omitempty"`
	Token        string   `json:"token,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	CountAssets  bool     `json:"count_assets,omitempty"`
	// DryRun defaults to true. Applying maps definitions only; copy content with
	// `ant migrate nexus --copy-content --apply`.
	DryRun *bool `json:"dry_run,omitempty"`
}

func (n NexusRequest) client() (*transfer.Nexus, error) {
	return transfer.NewNexus(transfer.NexusConfig{BaseURL: n.URL, Username: n.Username, Password: n.Password, Token: n.Token})
}

func (h transferHandlers) migrateNexus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in NexusRequest
	if !decodeBody(w, r, &in) {
		return
	}
	nx, err := in.client()
	if err != nil {
		transferError(w, r, err)
		return
	}
	dry := in.DryRun == nil || *in.DryRun
	ctx, cancel := context.WithTimeout(r.Context(), transferTimeout)
	defer cancel()
	rep, err := transfer.Migrate(ctx, h.d.Pool, h.stores(), p.InstanceID, nx, transfer.MapOptions{Repositories: in.Repositories, CountAssets: in.CountAssets, MaxAssets: maxMigrationAssets}, transfer.ImportOptions{DryRun: dry})
	if err != nil {
		transferError(w, r, err)
		return
	}
	h.record(ctx, p, "migration", dry, rep.Summary)
	JSON(w, 200, rep)
}

type CompareRequest struct {
	NexusRequest
	Items []transfer.CompareItem `json:"items"`
}

func (h transferHandlers) compare(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	var in CompareRequest
	if !decodeBody(w, r, &in) {
		return
	}
	nx, err := in.client()
	if err != nil {
		transferError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), transferTimeout)
	defer cancel()
	rep, err := transfer.Compare(ctx, h.d.Pool, p.InstanceID, nx, in.Items)
	if err != nil {
		transferError(w, r, err)
		return
	}
	h.record(ctx, p, "compare", true, rep.Counts)
	JSON(w, 200, rep)
}

type TransferRun struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	DryRun    bool            `json:"dry_run"`
	Summary   json.RawMessage `json:"summary"`
	CreatedAt time.Time       `json:"created_at"`
}
type TransferRunPage struct {
	Items []TransferRun `json:"items"`
}

func (h transferHandlers) runs(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.d.Pool.Query(ctx, `SELECT id::text,kind,dry_run,summary,created_at FROM transfer_runs WHERE instance_id=$1 ORDER BY created_at DESC,id LIMIT $2`, p.InstanceID, transferRunsPerPage)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := TransferRunPage{Items: []TransferRun{}}
	for rows.Next() {
		var t TransferRun
		var sum []byte
		if err = rows.Scan(&t.ID, &t.Kind, &t.DryRun, &sum, &t.CreatedAt); err != nil {
			identityError(w, r, err)
			return
		}
		t.Summary = bytes.Clone(sum)
		out.Items = append(out.Items, t)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	JSON(w, 200, out)
}
