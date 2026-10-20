package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Register mounts the public receiver on /hook/<slug> and /hook/<slug>/<token>.
// Every failure before a call is accepted answers 404 with the same body, so a
// caller learns nothing about which slugs exist or why a request was refused.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /hook/{slug}", s.serve)
	mux.HandleFunc("POST /hook/{slug}/{token}", s.serve)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// perIPPerMinute bounds calls per client address across all hooks. It is
// uniform for known and unknown slugs, so it does not reveal which exist.
const perIPPerMinute = 600

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := identity.ClientKey(r)
	if !s.ip.allow(ip, perIPPerMinute) {
		s.record("", "rate_limited")
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	// The size cap applies before anything is read and before the slug is looked
	// up, so it is uniform and says nothing about whether the slug exists.
	if r.ContentLength > s.cfg.MaxBody {
		s.record("", AuthTooLarge)
		w.Header().Set("Connection", "close")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	slug, token := r.PathValue("slug"), r.PathValue("token")
	if !ValidSlug(slug) {
		s.record("", "unauthorized")
		notFound(w)
		return
	}
	hook, err := s.lookup(ctx, slug)
	if errors.Is(err, ErrNotFound) {
		// Do comparable work so unknown slugs are not measurably faster.
		_ = tokenMatches(token, make([]byte, 32))
		s.record("", "unauthorized")
		notFound(w)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	reject := func(result string, body []byte) {
		s.record(hook.Slug, "unauthorized")
		if s.rejec.allow(hook.ID, 60) {
			s.logRejected(ctx, hook, ip, result, body, r.Header)
		}
		notFound(w)
	}
	if !hook.Enabled {
		s.record(hook.Slug, "unauthorized")
		notFound(w)
		return
	}
	// Modes that authenticate without the body do so first, so a bad credential
	// never causes the body to be read.
	switch hook.AuthMode {
	case AuthURLToken:
		if token == "" {
			_ = tokenMatches("", hook.TokenHash)
			reject(AuthMissing, nil)
			return
		}
		if !tokenMatches(token, hook.TokenHash) {
			reject(AuthInvalid, nil)
			return
		}
	case AuthBearer:
		if token != "" {
			reject(AuthInvalid, nil)
			return
		}
		raw, ok := bearerToken(r)
		if !ok {
			reject(AuthMissing, nil)
			return
		}
		if !s.bearerAllowed(ctx, hook, raw) {
			reject(AuthInvalid, nil)
			return
		}
	case AuthHMAC:
		if token != "" {
			reject(AuthInvalid, nil)
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBody))
	if err != nil {
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			s.record(hook.Slug, AuthTooLarge)
			if s.rejec.allow(hook.ID, 60) {
				s.logRejected(ctx, hook, ip, AuthTooLarge, nil, r.Header)
			}
			w.Header().Set("Connection", "close")
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	if hook.AuthMode == AuthHMAC {
		keys, kerr := s.signingKeys(ctx, hook.SigningSecretID)
		if kerr != nil {
			reject(AuthInvalid, body)
			return
		}
		valid := VerifyHMAC(hook.HMACFormat, r.Header, body, keys)
		clearKeys(keys)
		if !valid {
			if strings.TrimSpace(r.Header.Get(HMACFormats[hook.HMACFormat])) == "" {
				reject(AuthMissing, body)
			} else {
				reject(AuthInvalid, body)
			}
			return
		}
	}
	// Authenticated from here on: limits and replay checks may be explicit.
	if !s.hooks.allow(hook.ID, hook.RatePerMinute) {
		s.record(hook.Slug, "rate_limited")
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	ref := deliveryRef(r.Header)
	if ref != "" {
		tag, err := s.cfg.DB.Exec(ctx, `INSERT INTO incoming_hook_replay(hook_id,delivery_ref) VALUES($1,$2) ON CONFLICT DO NOTHING`, hook.ID, ref)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		if tag.RowsAffected() == 0 {
			s.record(hook.Slug, "duplicate")
			if s.rejec.allow(hook.ID, 60) {
				s.logRejected(ctx, hook, ip, AuthDuplicate, body, r.Header)
			}
			writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate delivery"})
			return
		}
	}
	id, err := s.enqueue(ctx, s.cfg.DB, newDelivery{hook: hook, ip: ip, auth: AuthOK, body: body, headers: pickHeaders(r.Header), ref: ref})
	if err != nil {
		if ref != "" {
			// The delivery was not recorded, so the sender may retry it.
			_, _ = s.cfg.DB.Exec(context.WithoutCancel(ctx), `DELETE FROM incoming_hook_replay WHERE hook_id=$1 AND delivery_ref=$2`, hook.ID, ref)
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	s.record(hook.Slug, "accepted")
	s.Notify()
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "accepted"})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:]), true
	}
	return "", false
}

// bearerAllowed reports whether raw is a valid Ant token of this instance that
// holds the hook's required action on the instance.
func (s *Service) bearerAllowed(ctx context.Context, hook *Hook, raw string) bool {
	p, err := s.tokens.Authenticate(ctx, raw)
	if err != nil || p.InstanceID != s.cfg.InstanceID {
		return false
	}
	policy, err := s.roles.Resolve(ctx, p)
	if err != nil {
		return false
	}
	var action identity.Action
	switch hook.BearerScope {
	case ScopeConfigure:
		action = identity.Configure
	case ScopeReplicate:
		action = identity.Replicate
	case ScopeAdmin:
		action = identity.Admin
	default:
		return false
	}
	return policy.Authorize(p, identity.Resource{InstanceID: s.cfg.InstanceID, Kind: identity.InstanceResource}, action)
}

type newDelivery struct {
	hook         *Hook
	ip, auth     string
	body         []byte
	headers      map[string]string
	ref          string
	redeliveryOf string
}

func (s *Service) enqueue(ctx context.Context, q Querier, d newDelivery) (string, error) {
	headers, _ := json.Marshal(d.headers)
	var ref, of any
	if d.ref != "" {
		ref = d.ref
	}
	if d.redeliveryOf != "" {
		of = d.redeliveryOf
	}
	var id string
	err := q.QueryRow(ctx, `INSERT INTO incoming_hook_deliveries(instance_id,hook_id,source_ip,auth_result,state,payload,payload_size,headers,preview,delivery_ref,redelivery_of)
 VALUES($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9,$10) RETURNING id::text`,
		s.cfg.InstanceID, d.hook.ID, d.ip, d.auth, d.body, len(d.body), headers, Preview(d.body), ref, of).Scan(&id)
	return id, err
}

// logRejected records a refused call of a known hook. The payload is never
// stored; the preview is short, redacted, and only present when the body was read.
func (s *Service) logRejected(ctx context.Context, hook *Hook, ip, result string, body []byte, h http.Header) {
	preview := ""
	if len(body) > 0 {
		preview = Preview(body[:min(len(body), 512)])
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, _ = s.cfg.DB.Exec(ctx, `INSERT INTO incoming_hook_deliveries(instance_id,hook_id,source_ip,auth_result,state,action_result,payload_size,preview,finished_at)
 VALUES($1,$2,$3,$4,'rejected',$4,$5,$6,now())`, s.cfg.InstanceID, hook.ID, ip, result, len(body), preview)
}
