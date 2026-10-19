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
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/feedgraph"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Source struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Format   string `json:"format"`
	Revision int64  `json:"revision"`
}
type SourcePage struct {
	Items []Source     `json:"items"`
	Page  api.PageInfo `json:"page"`
}
type CreateSource struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Format string `json:"format"`
}
type PatchSource struct {
	Name *string `json:"name,omitempty"`
}
type Attachment struct {
	Source   Source `json:"source"`
	Priority int32  `json:"priority"`
}
type AttachmentPage struct {
	Items        []Attachment `json:"items"`
	Page         api.PageInfo `json:"page"`
	FeedRevision int64        `json:"feed_revision"`
}
type AttachmentMutation struct {
	FeedID       string `json:"feed_id"`
	SourceID     string `json:"source_id"`
	FeedRevision int64  `json:"feed_revision"`
	Priority     *int32 `json:"priority,omitempty"`
}
type attachmentInput struct {
	Priority *int32 `json:"priority"`
}
type sourceHandlers struct{ feedHandlers }

// RegisterSources manages metadata only. Format and kind are immutable; remote
// credentials/configuration are not accepted by these endpoints. Attachments
// share the feed's revision and require configuration rights on both resources.
func RegisterSources(mux *http.ServeMux, pool *pgxpool.Pool, codec CursorCodec) {
	s := sourceHandlers{feedHandlers{db: pool, codec: codec}}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/sources": s.listSources, "POST /api/v1/sources": s.createSource,
		"GET /api/v1/sources/{id}": s.getSource, "PATCH /api/v1/sources/{id}": s.patchSource,
		"GET /api/v1/feeds/{id}/sources":                s.listAttachments,
		"PUT /api/v1/feeds/{id}/sources/{source_id}":    s.changeAttachment,
		"DELETE /api/v1/feeds/{id}/sources/{source_id}": s.changeAttachment,
	} {
		mux.Handle(pattern, bearer.Wrap(handler))
	}
}
func sourceAllowed(policy identity.Policy, p identity.Principal, id string, action identity.Action) bool {
	return policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, SourceID: id, Kind: identity.SourceResource}, action)
}
func validSourceName(name string) bool {
	return len(name) > 0 && len(name) <= 200 && utf8.ValidString(name) && strings.TrimSpace(name) == name && !strings.ContainsFunc(name, unicode.IsControl)
}
func validSourceKind(kind string) bool {
	return kind == "local" || kind == "remote" || kind == "replica"
}
func sourceJSON(w http.ResponseWriter, r *http.Request, dst any, keys ...string) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	fields, err := readObject(decoder)
	if err == nil {
		allowed := map[string]bool{}
		for _, key := range keys {
			allowed[key] = true
		}
		for key := range fields {
			if !allowed[key] {
				err = errors.New("unknown property")
				break
			}
		}
	}
	if err == nil {
		var extra any
		if err = decoder.Decode(&extra); err == io.EOF {
			encoded, _ := json.Marshal(fields)
			typed := json.NewDecoder(bytes.NewReader(encoded))
			typed.DisallowUnknownFields()
			if err = typed.Decode(dst); err == nil {
				return true
			}
		}
	}
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		Error(w, r, 413, api.ErrorCodeInvalidRequest, "request body too large")
	} else {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid JSON body")
	}
	return false
}
func sourceMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "resource not found")
	case errors.Is(err, database.ErrRevisionConflict):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "resource revision changed")
	case errors.Is(err, database.ErrConflict), errors.Is(err, database.ErrFingerprintConflict):
		Error(w, r, 409, api.ErrorCodeConflict, "configuration or idempotency conflict")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "source management unavailable")
	}
}
func writeSource(w http.ResponseWriter, status int, item Source) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(item.Revision, 10)))
	JSON(w, status, item)
}
func sourceEvent(ctx context.Context, tx pgx.Tx, p identity.Principal, item Source, action string) error {
	var audit int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,$3,$4) RETURNING id`, p.InstanceID, p.PrincipalID, action, item.ID).Scan(&audit); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('source_id',$3::text,'revision',$4::bigint))`, audit, action, item.ID, item.Revision)
	return err
}
func (s sourceHandlers) listSources(w http.ResponseWriter, r *http.Request) {
	q, err := ParsePage(r.URL.Query())
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page parameters")
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	encoded, _ := json.Marshal([]string{"sources", p.InstanceID, p.PrincipalID, p.TokenID, q.Prefix})
	scope := string(encoded)
	after := ""
	if q.Cursor != "" {
		after, err = s.codec.Decode(scope, q.Cursor)
		if _, valid := feedID(after); err != nil || !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, `SELECT id,name,kind,format,revision FROM sources WHERE instance_id=$1 AND ($2='' OR id>NULLIF($2,'')::uuid) AND starts_with(name,$3) ORDER BY id`, p.InstanceID, after, q.Prefix)
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	defer rows.Close()
	page := SourcePage{Items: []Source{}}
	for rows.Next() {
		var item Source
		if err = rows.Scan(&item.ID, &item.Name, &item.Kind, &item.Format, &item.Revision); err != nil {
			break
		}
		if !sourceAllowed(policy, p, item.ID, identity.Read) {
			continue
		}
		if len(page.Items) == q.Limit {
			cursor, e := s.codec.Encode(scope, page.Items[len(page.Items)-1].ID)
			if e != nil {
				sourceMutationError(w, r, e)
				return
			}
			page.Page.HasMore = true
			page.Page.NextCursor = &cursor
			break
		}
		page.Items = append(page.Items, item)
	}
	if err != nil || rows.Err() != nil {
		sourceMutationError(w, r, errors.Join(err, rows.Err()))
		return
	}
	JSON(w, 200, page)
}
func (s sourceHandlers) getSource(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source ID")
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !sourceAllowed(policy, p, id, identity.Read) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var item Source
	err := s.db.QueryRow(ctx, `SELECT id,name,kind,format,revision FROM sources WHERE instance_id=$1 AND id=$2`, p.InstanceID, id).Scan(&item.ID, &item.Name, &item.Kind, &item.Format, &item.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		err = database.ErrNotFound
	}
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	writeSource(w, 200, item)
}
func (s sourceHandlers) createSource(w http.ResponseWriter, r *http.Request) {
	var body CreateSource
	if !sourceJSON(w, r, &body, "name", "kind", "format") {
		return
	}
	if !validSourceName(body.Name) || !validSourceKind(body.Kind) || !formats[body.Format] {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source configuration")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !sourceAllowed(policy, p, "", identity.Configure) {
		Error(w, r, 403, api.ErrorCodeForbidden, "source configuration denied")
		return
	}
	canonical, _ := json.Marshal(body)
	result, err := database.ExecuteMutation(r.Context(), s.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "source.create", Resource: "sources", Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var item Source
		err := tx.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,$3,$4) RETURNING id,name,kind,format,revision`, p.InstanceID, body.Name, body.Kind, body.Format).Scan(&item.ID, &item.Name, &item.Kind, &item.Format, &item.Revision)
		if err != nil {
			return nil, feedConflict(err)
		}
		if item.Kind == "local" {
			if _, err = tx.Exec(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2)`, p.InstanceID, "source:"+item.ID); err != nil {
				return nil, err
			}
		}
		if err = sourceEvent(ctx, tx, p, item, "source.create"); err != nil {
			return nil, err
		}
		return json.Marshal(item)
	})
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	var item Source
	if json.Unmarshal(result.JSON, &item) != nil {
		sourceMutationError(w, r, errors.New("invalid result"))
		return
	}
	if !sourceAllowed(policy, p, item.ID, identity.Configure) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	w.Header().Set("Location", "/api/v1/sources/"+item.ID)
	writeSource(w, 201, item)
}
func (s sourceHandlers) patchSource(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var body PatchSource
	if !sourceJSON(w, r, &body, "name") {
		return
	}
	if body.Name == nil || !validSourceName(*body.Name) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid source update")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !sourceAllowed(policy, p, id, identity.Configure) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	canonical, _ := json.Marshal(struct {
		Body     PatchSource
		Revision int64
	}{body, revision})
	result, err := database.ExecuteMutation(r.Context(), s.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "source.patch", Resource: id, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var item Source
		err := tx.QueryRow(ctx, `SELECT id,name,kind,format,revision FROM sources WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id).Scan(&item.ID, &item.Name, &item.Kind, &item.Format, &item.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if item.Revision != revision {
			return nil, database.ErrRevisionConflict
		}
		err = tx.QueryRow(ctx, `UPDATE sources SET name=$3,revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING name,revision`, p.InstanceID, id, *body.Name).Scan(&item.Name, &item.Revision)
		if err != nil {
			return nil, feedConflict(err)
		}
		if err = sourceEvent(ctx, tx, p, item, "source.patch"); err != nil {
			return nil, err
		}
		return json.Marshal(item)
	})
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	var item Source
	if json.Unmarshal(result.JSON, &item) != nil {
		sourceMutationError(w, r, errors.New("invalid result"))
		return
	}
	writeSource(w, 200, item)
}

func (s sourceHandlers) listAttachments(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	q, err := ParsePage(r.URL.Query())
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page parameters")
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, id, identity.Read) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	page := AttachmentPage{Items: []Attachment{}}
	err = tx.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, id).Scan(&page.FeedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		err = database.ErrNotFound
	}
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	encoded, _ := json.Marshal([]string{"feed_sources", p.InstanceID, p.PrincipalID, p.TokenID, id, strconv.FormatInt(page.FeedRevision, 10), q.Prefix})
	scope := string(encoded)
	afterPriority := int64(-1)
	if q.Cursor != "" {
		after, e := s.codec.Decode(scope, q.Cursor)
		if e == nil {
			afterPriority, e = strconv.ParseInt(after, 10, 32)
		}
		if e != nil || afterPriority < 0 || afterPriority > 1000000 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid or stale page cursor")
			return
		}
	}
	rows, err := tx.Query(ctx, `SELECT s.id,s.name,s.kind,s.format,s.revision,fs.priority FROM feed_sources fs JOIN sources s ON s.instance_id=fs.instance_id AND s.id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND fs.priority>$3 AND starts_with(s.name,$4) ORDER BY fs.priority`, p.InstanceID, id, afterPriority, q.Prefix)
	if err != nil {
		sourceMutationError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var item Attachment
		if err = rows.Scan(&item.Source.ID, &item.Source.Name, &item.Source.Kind, &item.Source.Format, &item.Source.Revision, &item.Priority); err != nil {
			break
		}
		if !sourceAllowed(policy, p, item.Source.ID, identity.Read) {
			continue
		}
		if len(page.Items) == q.Limit {
			cursor, e := s.codec.Encode(scope, strconv.FormatInt(int64(page.Items[len(page.Items)-1].Priority), 10))
			if e != nil {
				sourceMutationError(w, r, e)
				return
			}
			page.Page.HasMore = true
			page.Page.NextCursor = &cursor
			break
		}
		page.Items = append(page.Items, item)
	}
	if err != nil || rows.Err() != nil {
		sourceMutationError(w, r, errors.Join(err, rows.Err()))
		return
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		sourceMutationError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(page.FeedRevision, 10)))
	JSON(w, 200, page)
}
func (s sourceHandlers) changeAttachment(w http.ResponseWriter, r *http.Request) {
	feed, valid := feedID(r.PathValue("id"))
	source, sourceValid := feedID(r.PathValue("source_id"))
	if !valid || !sourceValid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid resource ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match feed revision required")
		return
	}
	var body attachmentInput
	if r.Method == http.MethodPut {
		if !sourceJSON(w, r, &body, "priority") {
			return
		}
		if body.Priority == nil || *body.Priority < 0 || *body.Priority > 1000000 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "priority must be between 0 and 1000000")
			return
		}
	} else if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if len(body) != 0 || err != nil {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "DELETE body must be empty")
			return
		}
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := s.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, feed, identity.Configure) || !sourceAllowed(policy, p, source, identity.Configure) {
		sourceMutationError(w, r, database.ErrNotFound)
		return
	}
	operation := "feed.source.attach"
	if r.Method == http.MethodDelete {
		operation = "feed.source.detach"
	}
	canonical, _ := json.Marshal(struct {
		Revision int64
		Priority *int32
	}{revision, body.Priority})
	var applyErr error
	result, err := database.ExecuteMutation(r.Context(), s.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: operation, Resource: feed + "/" + source, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var feedFormat, feedPurpose string
		var current int64
		err := tx.QueryRow(ctx, `SELECT format,revision,purpose FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, feed).Scan(&feedFormat, &current, &feedPurpose)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		var sourceFormat string
		err = tx.QueryRow(ctx, `SELECT format FROM sources WHERE instance_id=$1 AND id=$2 FOR SHARE`, p.InstanceID, source).Scan(&sourceFormat)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if r.Method == http.MethodPut {
			if feedFormat != sourceFormat {
				return nil, database.ErrConflict
			}
			if reason, e := checkCacheSourcePlacement(ctx, tx, p.InstanceID, feed, feedPurpose, source); e != nil {
				return nil, e
			} else if reason != "" {
				applyErr = conflictConfig(reason)
				return nil, applyErr
			}
			_, err = tx.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,$4) ON CONFLICT(feed_id,source_id) DO UPDATE SET priority=EXCLUDED.priority`, p.InstanceID, feed, source, *body.Priority)
		} else {
			var referenced bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM publish_policies p WHERE p.instance_id=$1 AND p.feed_id=$2 AND (p.default_source_id=$3 OR EXISTS(SELECT 1 FROM jsonb_array_elements(p.rules) rule WHERE rule->>'Target'=$3::text)))`, p.InstanceID, feed, source).Scan(&referenced); err != nil {
				return nil, err
			}
			if referenced {
				return nil, database.ErrConflict
			}
			var tag pgconn.CommandTag
			tag, err = tx.Exec(ctx, `DELETE FROM feed_sources WHERE instance_id=$1 AND feed_id=$2 AND source_id=$3`, p.InstanceID, feed, source)
			if err == nil && tag.RowsAffected() != 1 {
				return nil, database.ErrNotFound
			}
		}
		if err != nil {
			return nil, feedConflict(err)
		}
		if r.Method == http.MethodPut {
			// An attached remote source may point back at this instance.
			if loop := feedgraph.Check(ctx, tx, p.InstanceID); loop != nil {
				applyErr = loop
				return nil, loop
			}
		} else if invalid := (artifact.Policies{DB: s.db, InstanceID: p.InstanceID}).ValidateTx(ctx, tx, feed); invalid != nil {
			var config *routing.ConfigError
			if !errors.As(invalid, &config) && !errors.Is(invalid, artifact.ErrInput) {
				return nil, invalid
			}
			// Bound templates may still target the source being detached.
			applyErr = conflictConfig("source is referenced by a policy template bound to this feed; unbind or edit the template first")
			return nil, applyErr
		}
		item := AttachmentMutation{FeedID: feed, SourceID: source, Priority: body.Priority}
		if err = tx.QueryRow(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING revision`, p.InstanceID, feed).Scan(&item.FeedRevision); err != nil {
			return nil, err
		}
		var audit int64
		if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,$3,$4) RETURNING id`, p.InstanceID, p.PrincipalID, operation, feed).Scan(&audit); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,$3)`, audit, operation, payload); err != nil {
			return nil, err
		}
		return payload, nil
	})
	if err != nil {
		if applyErr != nil && rejection(w, r, applyErr) {
			return
		}
		sourceMutationError(w, r, err)
		return
	}
	var item AttachmentMutation
	if json.Unmarshal(result.JSON, &item) != nil {
		sourceMutationError(w, r, errors.New("invalid result"))
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(item.FeedRevision, 10)))
	JSON(w, 200, item)
}
