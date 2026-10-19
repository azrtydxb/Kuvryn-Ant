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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Feed struct {
	ID            string       `json:"id"`
	Slug          string       `json:"slug"`
	Format        string       `json:"format"`
	Revision      int64        `json:"revision"`
	AnonymousRead bool         `json:"anonymous_read"`
	Purpose       string       `json:"purpose"`
	CachePolicy   *CachePolicy `json:"cache_policy,omitempty"`
}
type FeedPage struct {
	Items []Feed       `json:"items"`
	Page  api.PageInfo `json:"page"`
}
type CreateFeed struct {
	Slug          string            `json:"slug"`
	Format        string            `json:"format"`
	AnonymousRead bool              `json:"anonymous_read"`
	Purpose       string            `json:"purpose,omitempty"`
	CachePolicy   *CachePolicyInput `json:"cache_policy,omitempty"`
}
type PatchFeed struct {
	Slug          *string           `json:"slug,omitempty"`
	AnonymousRead *bool             `json:"anonymous_read,omitempty"`
	Purpose       *string           `json:"purpose,omitempty"`
	CachePolicy   *CachePolicyInput `json:"cache_policy,omitempty"`
}
type feedHandlers struct {
	db    *pgxpool.Pool
	codec CursorCodec
}

// RegisterFeeds registers only authenticated metadata management. Creating a
// format configuration is not a claim that its protocol adapter is available.
func RegisterFeeds(mux *http.ServeMux, pool *pgxpool.Pool, codec CursorCodec) {
	f := feedHandlers{db: pool, codec: codec}
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	mux.Handle("GET /api/v1/feeds", bearer.Wrap(http.HandlerFunc(f.list)))
	mux.Handle("POST /api/v1/feeds", bearer.Wrap(http.HandlerFunc(f.create)))
	mux.Handle("GET /api/v1/feeds/{id}", bearer.Wrap(http.HandlerFunc(f.get)))
	mux.Handle("PATCH /api/v1/feeds/{id}", bearer.Wrap(http.HandlerFunc(f.patch)))
}
func (f feedHandlers) policy(w http.ResponseWriter, r *http.Request) (identity.Principal, identity.Policy, bool) {
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok {
		Error(w, r, 401, api.ErrorCodeUnauthenticated, "authentication required")
		return p, identity.Policy{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	policy, err := (identity.Roles{DB: f.db}).Resolve(ctx, p)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "authorization unavailable")
		return p, policy, false
	}
	return p, policy, true
}
func feedAllowed(policy identity.Policy, p identity.Principal, id string, action identity.Action) bool {
	return policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: id, Kind: identity.FeedResource}, action)
}
func feedID(raw string) (string, bool) {
	var id pgtype.UUID
	if id.Scan(raw) != nil || !id.Valid {
		return "", false
	}
	return id.String(), true
}
func (f feedHandlers) list(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	purpose := ""
	if vs, present := values["purpose"]; present {
		if len(vs) != 1 || !validPurpose(vs[0]) {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "purpose must be artifacts or cache")
			return
		}
		purpose = vs[0]
		values.Del("purpose")
	}
	q, err := ParsePage(values)
	if err != nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page parameters")
		return
	}
	p, policy, ok := f.policy(w, r)
	if !ok {
		return
	}
	scopeBytes, _ := json.Marshal([]string{"feeds", p.InstanceID, p.PrincipalID, p.TokenID, q.Prefix, purpose})
	scope := string(scopeBytes)
	after := ""
	if q.Cursor != "" {
		after, err = f.codec.Decode(scope, q.Cursor)
		if _, valid := feedID(after); err != nil || !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid page cursor")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Prefix is a literal string, never a SQL LIKE pattern. Keyset ordering does
	// not shift when earlier rows are inserted/deleted. Permission-filter before
	// producing either data or a next-page marker.
	rows, err := f.db.Query(ctx, `SELECT `+feedColumns+` FROM feeds WHERE instance_id=$1 AND ($2='' OR id>NULLIF($2,'')::uuid) AND starts_with(slug,$3) AND ($4='' OR purpose=$4) ORDER BY id`, p.InstanceID, after, q.Prefix, purpose)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "feed listing unavailable")
		return
	}
	defer rows.Close()
	page := FeedPage{Items: []Feed{}}
	for rows.Next() {
		var item Feed
		if err = scanFeed(rows, &item); err != nil {
			break
		}
		if !feedAllowed(policy, p, item.ID, identity.Read) {
			continue
		}
		if len(page.Items) == q.Limit {
			token, e := f.codec.Encode(scope, page.Items[len(page.Items)-1].ID)
			if e != nil {
				Error(w, r, 500, api.ErrorCodeInternalError, "cannot create page cursor")
				return
			}
			page.Page.HasMore = true
			page.Page.NextCursor = &token
			break
		}
		page.Items = append(page.Items, item)
	}
	if err != nil || rows.Err() != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "feed listing unavailable")
		return
	}
	JSON(w, 200, page)
}
func (f feedHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	p, policy, ok := f.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, id, identity.Read) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var item Feed
	err := scanFeed(f.db.QueryRow(ctx, `SELECT `+feedColumns+` FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, id), &item)
	if errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "feed lookup unavailable")
		return
	}
	writeFeed(w, 200, item)
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var formats = map[string]bool{"apk": true, "ansible": true, "apt": true, "cocoapods": true, "composer": true, "conan": true, "conda": true, "oci": true, "git-lfs": true, "go": true, "helm": true, "huggingface": true, "maven": true, "npm": true, "nuget": true, "oci-artifacts": true, "p2": true, "pub": true, "pypi": true, "cran": true, "raw": true, "rubygems": true, "cargo": true, "swift": true, "terraform": true, "rpm": true}

func strictJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	// Reject duplicate and null fields rather than silently choosing a value.
	var fields map[string]json.RawMessage
	fields, err = readObject(dec)
	if err == nil {
		// encoding/json otherwise matches struct fields case-insensitively.
		// The wire contract uses exact property names, including duplicates.
		for key := range fields {
			valid := key == "slug" || key == "anonymous_read" || key == "purpose" || key == "cache_policy"
			if _, create := dst.(*CreateFeed); create && key == "format" {
				valid = true
			}
			if !valid {
				err = errors.New("unknown JSON property")
				break
			}
		}
	}
	if err == nil {
		var extra any
		if err = dec.Decode(&extra); err == io.EOF {
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
func readObject(dec *json.Decoder) (map[string]json.RawMessage, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("object required")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("duplicate object key")
		}
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("null field")
		}
		fields[name] = value
	}
	if _, err = dec.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}
func mutationKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || len(values[0]) < 1 || len(values[0]) > 256 || strings.TrimSpace(values[0]) != values[0] {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "Idempotency-Key required (1–256 bytes)")
		return "", false
	}
	return values[0], true
}
func (f feedHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body CreateFeed
	if !strictJSON(w, r, &body) {
		return
	}
	if !slugPattern.MatchString(body.Slug) || !formats[body.Format] {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed configuration")
		return
	}
	if body.Purpose == "" {
		body.Purpose = PurposeArtifacts
	}
	if rejectCacheInput(w, r, validateCacheInput(body.Purpose, body.Format, body.CachePolicy)) {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := f.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, "", identity.Configure) {
		Error(w, r, 403, api.ErrorCodeForbidden, "feed configuration denied")
		return
	}
	canonical, _ := json.Marshal(body)
	result, err := database.ExecuteMutation(r.Context(), f.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "feed.create", Resource: "feeds", Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var item Feed
		idle, maxBytes := cachePolicyValues(body.CachePolicy, DefaultCacheMaxIdleSeconds, nil)
		err := scanFeed(tx.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format,anonymous_read,purpose,cache_max_idle_seconds,cache_max_bytes) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+feedColumns, p.InstanceID, body.Slug, body.Format, body.AnonymousRead, body.Purpose, idle, maxBytes), &item)
		if err != nil {
			return nil, feedConflict(err)
		}
		if err = feedEvent(ctx, tx, p, item, "feed.create"); err != nil {
			return nil, err
		}
		if item.Purpose == PurposeCache {
			if err = feedPurposeAudit(ctx, tx, p, item, ""); err != nil {
				return nil, err
			}
		}
		return json.Marshal(item)
	})
	if err != nil {
		mutationError(w, r, err)
		return
	}
	var item Feed
	if json.Unmarshal(result.JSON, &item) != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "invalid mutation result")
		return
	}
	// A stored create result refers to a concrete feed. Collection-level
	// creation permission cannot override a later deny on that resource.
	if !feedAllowed(policy, p, item.ID, identity.Configure) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	w.Header().Set("Location", "/api/v1/feeds/"+item.ID)
	writeFeed(w, 201, item)
}
func revisionHeader(r *http.Request) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) != 1 {
		return 0, false
	}
	v := values[0]
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
		return 0, false
	}
	revision, err := strconv.ParseInt(v[1:len(v)-1], 10, 64)
	return revision, err == nil && revision > 0 && v == strconv.Quote(strconv.FormatInt(revision, 10))
}
func (f feedHandlers) patch(w http.ResponseWriter, r *http.Request) {
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	var body PatchFeed
	if !strictJSON(w, r, &body) {
		return
	}
	if (body.Slug == nil && body.AnonymousRead == nil && body.Purpose == nil && body.CachePolicy == nil) || (body.Slug != nil && !slugPattern.MatchString(*body.Slug)) || (body.Purpose != nil && !validPurpose(*body.Purpose)) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed update")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	p, policy, ok := f.policy(w, r)
	if !ok {
		return
	}
	if !feedAllowed(policy, p, id, identity.Configure) {
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	}
	if body.Purpose != nil && !purposeChangeAllowed(policy, p, id) {
		Error(w, r, 403, api.ErrorCodeForbidden, "changing the feed purpose requires feed administration")
		return
	}
	canonical, _ := json.Marshal(struct {
		Body     PatchFeed
		Revision int64
	}{body, revision})
	var pr *problem
	result, err := database.ExecuteMutation(r.Context(), f.db, database.MutationRequest{InstanceID: p.InstanceID, PrincipalID: p.PrincipalID, Operation: "feed.patch", Resource: id, Key: key, Fingerprint: sha256.Sum256(canonical), NonSecretResult: true}, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		var item Feed
		err := scanFeed(tx.QueryRow(ctx, `SELECT `+feedColumns+` FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id), &item)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, database.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if body.Slug != nil {
			item.Slug = *body.Slug
		}
		if body.AnonymousRead != nil {
			item.AnonymousRead = *body.AnonymousRead
		}
		fromPurpose := item.Purpose
		idle, maxBytes := DefaultCacheMaxIdleSeconds, (*int64)(nil)
		if item.CachePolicy != nil {
			idle, maxBytes = item.CachePolicy.MaxIdleSeconds, item.CachePolicy.MaxBytes
		}
		if body.Purpose != nil {
			item.Purpose = *body.Purpose
		}
		if msg := validateCacheInput(item.Purpose, item.Format, body.CachePolicy); msg != "" {
			pr = &problem{400, api.ErrorCodeInvalidRequest, msg}
			return nil, database.ErrConflict
		}
		if item.Purpose == PurposeCache && fromPurpose != PurposeCache {
			reason, e := checkPurposeChange(ctx, tx, p.InstanceID, id)
			if e != nil {
				return nil, e
			}
			if reason != "" {
				pr = &problem{409, api.ErrorCodeConflict, "feed cannot become a cache feed: " + reason}
				return nil, database.ErrConflict
			}
		}
		idle, maxBytes = cachePolicyValues(body.CachePolicy, idle, maxBytes)
		item.Revision, err = database.UpdateFeed(ctx, tx, database.FeedUpdate{InstanceID: p.InstanceID, ID: id, Slug: item.Slug, AnonymousRead: item.AnonymousRead, ExpectedRevision: revision})
		if err != nil {
			return nil, feedConflict(err)
		}
		if err = applyCacheColumns(ctx, tx, p.InstanceID, id, item.Purpose, idle, maxBytes); err != nil {
			return nil, err
		}
		item.CachePolicy = nil
		if item.Purpose == PurposeCache {
			item.CachePolicy = &CachePolicy{MaxIdleSeconds: idle, MaxBytes: maxBytes}
		}
		if err = feedEvent(ctx, tx, p, item, "feed.patch"); err != nil {
			return nil, err
		}
		if item.Purpose != fromPurpose || body.CachePolicy != nil {
			if err = feedPurposeAudit(ctx, tx, p, item, fromPurpose); err != nil {
				return nil, err
			}
		}
		return json.Marshal(item)
	})
	if err != nil {
		if pr != nil && errors.Is(err, database.ErrConflict) {
			writeProblem(w, r, pr)
			return
		}
		mutationError(w, r, err)
		return
	}
	var item Feed
	if json.Unmarshal(result.JSON, &item) != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "invalid mutation result")
		return
	}
	writeFeed(w, 200, item)
}
func feedEvent(ctx context.Context, tx pgx.Tx, p identity.Principal, item Feed, action string) error {
	var audit int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,$3,$4) RETURNING id`, p.InstanceID, p.PrincipalID, action, item.ID).Scan(&audit); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('feed_id',$3::text,'revision',$4::bigint))`, audit, action, item.ID, item.Revision)
	return err
}
func feedConflict(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return database.ErrConflict
	}
	return err
}
func mutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
	case errors.Is(err, database.ErrRevisionConflict):
		Error(w, r, 412, api.ErrorCodePreconditionFailed, "feed revision changed")
	case errors.Is(err, database.ErrConflict), errors.Is(err, database.ErrFingerprintConflict):
		Error(w, r, 409, api.ErrorCodeConflict, "feed or idempotency key conflict")
	default:
		Error(w, r, 503, api.ErrorCodeUnavailable, "feed mutation unavailable")
	}
}
func writeFeed(w http.ResponseWriter, status int, item Feed) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(item.Revision, 10)))
	JSON(w, status, item)
}
