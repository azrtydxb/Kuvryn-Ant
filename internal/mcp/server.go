// Package mcp implements an MCP (Model Context Protocol) server over the
// Streamable HTTP transport (JSON-RPC 2.0, stateless, JSON responses).
//
// The server holds no business logic or SQL of its own for management
// operations. Every tool dispatches in-process to the management API handlers
// with the caller's own credentials, so authentication, authorization,
// idempotency, validation and audit events are exactly those of the API.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Path is the mount point of the MCP endpoint.
const Path = "/mcp"

const (
	latestProtocol = "2025-06-18"
	maxBody        = 1 << 20
	maxBatch       = 16
)

var supportedProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Result/Error are only present when the client is answering the server.
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Auditor records one MCP-level audit event. Management mutations additionally
// emit their normal audit events through the API handlers.
type Auditor func(ctx context.Context, p identity.Principal, e AuditEvent) error

// AuditEvent describes a tool call or resource read. It never contains
// arguments or results.
type AuditEvent struct {
	Action      string // "mcp.tool.call" or "mcp.resource.read"
	Name        string // tool name or resource kind
	ResourceID  string // optional UUID the call targeted
	Outcome     string // ok, error, confirmation_required, invalid
	Destructive bool
	Status      int // HTTP status of the underlying API call, 0 if none
}

// Server is the MCP protocol endpoint.
type Server struct {
	api    http.Handler
	audit  Auditor
	logger *slog.Logger
	tools  map[string]*tool
	order  []*tool
}

// New creates a server dispatching to the given management handler (normally
// the service mux). A nil auditor disables the MCP-level audit event.
func New(api http.Handler, audit Auditor, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{api: api, audit: audit, logger: logger, tools: map[string]*tool{}}
	for _, t := range catalog() {
		s.tools[t.name] = t
		s.order = append(s.order, t)
	}
	return s
}

// Register mounts the endpoint on mux behind the same Bearer authentication as
// the management API. Tools call back into mux itself, so it must be the
// complete service mux.
func Register(mux *http.ServeMux, pool *pgxpool.Pool, logger *slog.Logger) {
	s := New(mux, DBAuditor(pool), logger)
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	mux.Handle(Path, bearer.Wrap(s))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !originAllowed(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		// No server-initiated stream and no sessions to terminate.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedProtocols[v] {
		http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	p, ok := identity.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		writeRPC(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "request body too large or unreadable"}})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(raw, &batch) != nil {
			writeRPC(w, http.StatusBadRequest, parseError())
			return
		}
		if len(batch) == 0 || len(batch) > maxBatch {
			writeRPC(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "invalid batch size"}})
			return
		}
		var out []rpcResponse
		for _, item := range batch {
			if resp, ok := s.handleOne(r, p, item); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, out)
		return
	}
	resp, reply := s.handleOne(r, p, raw)
	if !reply {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

func parseError() rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}}
}

func writeRPC(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// originAllowed rejects cross-origin browser requests (DNS rebinding defence).
// Non-browser MCP clients send no Origin header.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) handleOne(r *http.Request, p identity.Principal, raw []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&req); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return parseError(), true
		}
		return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "invalid request"}}, true
	}
	isNotification := len(req.ID) == 0
	if req.Method == "" && (len(req.Result) > 0 || len(req.Error) > 0) {
		return rpcResponse{}, false // client response to a server request; none are issued
	}
	if req.JSONRPC != "2.0" || req.Method == "" || (!isNotification && !validID(req.ID)) {
		return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "invalid request"}}, true
	}
	if isNotification {
		return rpcResponse{}, false // notifications/initialized, notifications/cancelled, ...
	}
	result, rerr := s.dispatch(r, p, req.Method, req.Params)
	if rerr != nil {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

func validID(id json.RawMessage) bool {
	t := bytes.TrimSpace(id)
	return len(t) > 0 && (t[0] == '"' || t[0] == '-' || (t[0] >= '0' && t[0] <= '9'))
}

func (s *Server) dispatch(r *http.Request, p identity.Principal, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var in struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(params) > 0 && json.Unmarshal(params, &in) != nil {
			return nil, &rpcError{codeInvalidParams, "invalid params"}
		}
		version := latestProtocol
		if supportedProtocols[in.ProtocolVersion] {
			version = in.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false},
			},
			"serverInfo":   map[string]any{"name": "kuvryn-ant", "title": "Kuvryn Ant", "version": "1"},
			"instructions": "Kuvryn Ant artifact platform. Tools enforce the same permissions and audit trail as the management API using your token. Destructive tools require confirm: true.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]any, 0, len(s.order))
		for _, t := range s.order {
			list = append(list, t.descriptor())
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		return s.callTool(r, p, params)
	case "resources/list":
		return s.listResources(), nil
	case "resources/templates/list":
		return s.listTemplates(), nil
	case "resources/read":
		return s.readResource(r, p, params)
	default:
		return nil, &rpcError{codeMethodNotFound, "method not found"}
	}
}
