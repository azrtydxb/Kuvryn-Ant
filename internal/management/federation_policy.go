package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation"
	"github.com/jackc/pgx/v5"
)

// Federation policy management (docs/planning/phase-2-federation.md §6, §12.1,
// P2-M2). Reads need read on the instance. Drafts and publishing need
// instance admin on the authority; feed-key bindings, local settings and
// principal adoption need instance admin on the member itself (they never
// need the authority). Every mutation requires Idempotency-Key and replays.

const maxPolicyBody = 1 << 20

// policyJSON decodes a JSON body (up to 1 MiB) strictly: unknown properties
// anywhere are rejected.
func policyJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		Error(w, r, 415, api.ErrorCodeInvalidRequest, "application/json required")
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolicyBody))
	if err != nil {
		Error(w, r, 413, api.ErrorCodeInvalidRequest, "request body too large")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil || dec.More() {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid JSON body")
		return false
	}
	return true
}

func (h federationHandlers) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	v, err := svc.Policy(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, v)
}

func (h federationHandlers) getDraft(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	d, err := svc.Draft(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(d.Revision, 10)))
	JSON(w, 200, d)
}

func (h federationHandlers) putDraft(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	var expected int64
	if len(r.Header.Values("If-Match")) > 0 {
		if expected, ok = revisionHeader(r); !ok {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid If-Match revision")
			return
		}
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in struct {
		Policy *federation.PolicyBody `json:"policy"`
	}
	if !policyJSON(w, r, &in) {
		return
	}
	if in.Policy == nil {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "policy required")
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.policy.draft", "draft", struct {
		Policy   *federation.PolicyBody
		Expected int64
	}{in.Policy, expected}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return svc.SavePolicyDraftTx(ctx, tx, p.PrincipalID, *in.Policy, expected)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
		return
	case storeErr != nil:
		storeError(w, r, storeErr)
		return
	}
	var rev struct {
		Revision int64 `json:"revision"`
	}
	_ = json.Unmarshal(out.JSON, &rev)
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(rev.Revision, 10)))
	JSON(w, 200, out.JSON)
}

// PolicyPublishResult is returned by publish.
type PolicyPublishResult struct {
	Generation int64                   `json:"generation"`
	Hash       string                  `json:"hash"`
	Report     federation.PolicyReport `json:"report"`
}

func (h federationHandlers) publishPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in struct {
		BaseGeneration *int64                 `json:"base_generation"`
		Confirm        bool                   `json:"confirm"`
		Policy         *federation.PolicyBody `json:"policy"`
	}
	if !policyJSON(w, r, &in) {
		return
	}
	if in.BaseGeneration == nil || *in.BaseGeneration < 0 {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "base_generation (the applied policy generation you reviewed) is required")
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "publishing federation policy requires confirm: true")
		return
	}
	var doc *federation.PolicyDocument
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.policy.publish", "policy", struct {
		Base   int64
		Policy *federation.PolicyBody
	}{*in.BaseGeneration, in.Policy}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		d, report, err := svc.PublishPolicyTx(ctx, tx, p.PrincipalID, federation.PublishInput{BaseGeneration: *in.BaseGeneration, Body: in.Policy})
		if err != nil {
			return nil, err
		}
		doc = d
		return PolicyPublishResult{Generation: d.Generation, Hash: federation.PolicyHash(d), Report: report}, nil
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
		return
	case storeErr != nil:
		storeError(w, r, storeErr)
		return
	}
	if !out.Replayed {
		svc.AfterPolicyPublish(r.Context(), doc)
	}
	JSON(w, 200, out.JSON)
}

func (h federationHandlers) policyDiff(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		if (k != "from" && k != "to") || len(v) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "unknown or repeated query parameter")
			return
		}
	}
	from, to, toDraft := int64(-1), int64(0), false
	if v := q.Get("from"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "from must be a generation (0 for empty)")
			return
		}
		from = n
	}
	switch v := q.Get("to"); v {
	case "":
	case "draft":
		toDraft = true
	default:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "to must be a generation or draft")
			return
		}
		to = n
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	d, err := svc.Diff(ctx, from, to, toDraft)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, d)
}

func (h federationHandlers) feedBindings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	items, err := svc.FeedBindings(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": items})
}

func (h federationHandlers) bindFeed(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in struct {
		FeedKey string `json:"feed_key"`
		FeedID  string `json:"feed_id"`
	}
	if !sourceJSON(w, r, &in, "feed_key", "feed_id") {
		return
	}
	feed, valid := feedID(in.FeedID)
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "feed_id (uuid) and feed_key required")
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.feed.bind", feed, in, func(ctx context.Context, tx pgx.Tx) (any, error) {
		b, _, err := svc.BindFeedTx(ctx, tx, p.PrincipalID, in.FeedKey, feed)
		return b, err
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		JSON(w, 200, out.JSON)
	}
}

func (h federationHandlers) unbindFeed(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	feedKey := r.PathValue("fed") + "/" + r.PathValue("slug")
	_, callErr, storeErr := idempotent(r, h.db, p, key, "federation.feed.unbind", feedKey, feedKey, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return struct{}{}, svc.UnbindFeedTx(ctx, tx, p.PrincipalID, feedKey)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		w.WriteHeader(204)
	}
}

func (h federationHandlers) principals(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	items, err := svc.Principals(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, map[string]any{"items": items})
}

func (h federationHandlers) adoptPrincipal(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid principal ID")
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !sourceJSON(w, r, &in, "confirm") {
		return
	}
	if !in.Confirm {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "adopting a local principal into federation management requires confirm: true")
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.principal.adopt", id, struct{}{}, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return svc.AdoptPrincipalTx(ctx, tx, p.PrincipalID, id)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		JSON(w, 200, out.JSON)
	}
}

func (h federationHandlers) getSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.reader(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r, 10*time.Second)
	defer cancel()
	st, err := svc.Settings(ctx)
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, st)
}

func (h federationHandlers) putSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	key, ok := mutationKey(w, r)
	if !ok {
		return
	}
	var in federation.Settings
	if !policyJSON(w, r, &in) {
		return
	}
	out, callErr, storeErr := idempotent(r, h.db, p, key, "federation.settings", "settings", in, func(ctx context.Context, tx pgx.Tx) (any, error) {
		return svc.SetSettingsTx(ctx, tx, p.PrincipalID, in)
	})
	switch {
	case callErr != nil:
		federationError(w, r, callErr)
	case storeErr != nil:
		storeError(w, r, storeErr)
	default:
		JSON(w, 200, out.JSON)
	}
}

// accessCheck asserts the caller to another member, which answers with its
// own decision. Any authenticated federated principal may check itself.
func (h federationHandlers) accessCheck(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.policy(w, r)
	if !ok {
		return
	}
	svc, ok := h.service(w, r, p)
	if !ok {
		return
	}
	var in federation.AccessCheckInput
	if !sourceJSON(w, r, &in, "member_id", "kind", "feed_key", "namespace", "action") {
		return
	}
	member, valid := feedID(in.Member)
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "member_id (uuid) required")
		return
	}
	in.Member = member
	ctx, cancel := h.ctx(r, 15*time.Second)
	defer cancel()
	out, err := svc.CheckAccess(ctx, p, in)
	var rej *federation.RejectedError
	if errors.As(err, &rej) {
		Error(w, r, 409, api.ErrorCodeDocumentRejected, "member refused the assertion: "+rej.Reason)
		return
	}
	if err != nil {
		federationError(w, r, err)
		return
	}
	JSON(w, 200, out)
}
