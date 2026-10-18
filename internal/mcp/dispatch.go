package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxAPIResponse = 1 << 20

// recorder is a minimal in-memory http.ResponseWriter with a size bound.
type recorder struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *recorder) Header() http.Header { return w.header }
func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *recorder) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.body.Len()+len(b) > maxAPIResponse {
		w.overflow = true
		return len(b), nil
	}
	return w.body.Write(b)
}

type apiResp struct {
	status   int
	header   http.Header
	body     []byte
	overflow bool
}

// call carries one tools/call or resources/read execution.
type call struct {
	s          *Server
	r          *http.Request
	p          identity.Principal
	args       map[string]any
	status     int    // last API status, for the audit event
	resourceID string // UUID targeted, for the audit event
}

// api dispatches an in-process request to the management API with the
// caller's identity. The principal is authenticated again by the API's own
// middleware (Authorization is forwarded), so authorization, idempotency and
// audit behaviour are exactly those of the HTTP API.
func (c *call) api(method, path string, q url.Values, body any, hdr map[string]string) *apiResp {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(c.r.Context(), method, target, reader)
	if err != nil {
		return &apiResp{status: http.StatusInternalServerError}
	}
	req.RemoteAddr = c.r.RemoteAddr
	req.Host = c.r.Host
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a := c.r.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := &recorder{header: http.Header{}}
	c.s.api.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	c.status = rec.status
	return &apiResp{status: rec.status, header: rec.header, body: rec.body.Bytes(), overflow: rec.overflow}
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func textResult(s string) *toolResult { return &toolResult{Content: []content{{"text", s}}} }
func errResult(s string) *toolResult {
	return &toolResult{Content: []content{{"text", s}}, IsError: true}
}

// result converts an API response into a tool result. API errors are already
// sanitised by the management layer (stable code, message, request id); only
// those fields are forwarded, never raw bodies of failed calls.
func (a *apiResp) result() *toolResult {
	if a.overflow {
		return errResult("response too large; narrow the query or use a smaller limit")
	}
	if a.status >= 400 {
		var e struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(a.body, &e) != nil || e.Code == "" {
			return errResult(fmt.Sprintf("request failed with status %d", a.status))
		}
		return errResult(fmt.Sprintf("%s: %s (status %d, request_id %s)", e.Code, e.Message, a.status, e.RequestID))
	}
	if len(bytes.TrimSpace(a.body)) == 0 {
		return textResult(fmt.Sprintf(`{"status":%d}`, a.status))
	}
	return textResult(strings.TrimSpace(string(a.body)))
}

// idempotencyKey uses the client-supplied key when present so retries are
// replay-safe; otherwise a fresh key per call.
func (c *call) idempotencyKey() string {
	if k, ok := c.args["idempotency_key"].(string); ok && k != "" {
		return k
	}
	return "mcp-" + rand.Text()
}

func etag(rev int64) string { return `"` + strconv.FormatInt(rev, 10) + `"` }

// revision returns the explicit revision argument, or reads the current one
// through the API (so the same read authorization applies).
func (c *call) revision(explicit string, fetch func() (int64, *toolResult)) (int64, *toolResult) {
	if v, ok := c.args[explicit]; ok {
		n, _ := toInt(v)
		return int64(n), nil
	}
	return fetch()
}

func (c *call) feedRevision(feed string) (int64, *toolResult) {
	resp := c.api(http.MethodGet, "/api/v1/feeds/"+feed, nil, nil, nil)
	if resp.status >= 400 {
		return 0, resp.result()
	}
	var f struct {
		Revision int64 `json:"revision"`
	}
	if json.Unmarshal(resp.body, &f) != nil || f.Revision < 1 {
		return 0, errResult("cannot determine revision")
	}
	return f.Revision, nil
}

func (c *call) webhookRevision(id string) (int64, *toolResult) {
	after := ""
	for range 20 {
		q := url.Values{"limit": {"200"}}
		if after != "" {
			q.Set("after", after)
		}
		resp := c.api(http.MethodGet, "/api/v1/webhooks", q, nil, nil)
		if resp.status >= 400 {
			return 0, resp.result()
		}
		var page struct {
			Items []struct {
				ID       string `json:"id"`
				Revision int64  `json:"revision"`
			} `json:"items"`
			Next string `json:"next_after"`
		}
		if json.Unmarshal(resp.body, &page) != nil {
			return 0, errResult("cannot determine revision")
		}
		for _, it := range page.Items {
			if strings.EqualFold(it.ID, id) {
				return it.Revision, nil
			}
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	return 0, errResult("not_found: webhook not found")
}

// DBAuditor writes MCP audit events to the platform audit log. The event
// holds the tool name and outcome only, never arguments or results.
func DBAuditor(pool *pgxpool.Pool) Auditor {
	return func(ctx context.Context, p identity.Principal, e AuditEvent) error {
		details, _ := json.Marshal(map[string]any{
			"name": e.Name, "outcome": e.Outcome, "destructive": e.Destructive,
			"status": e.Status, "token_id": p.TokenID, "transport": "mcp",
		})
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,$3,NULLIF($4::text,'')::uuid,$5::jsonb)`,
			p.InstanceID, p.PrincipalID, e.Action, e.ResourceID, string(details))
		return err
	}
}
