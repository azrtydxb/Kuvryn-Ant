package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Resources are read-only views backed by the same tools (and therefore the
// same authorization): ant://feeds, ant://feeds/{id}, ant://sources,
// ant://sources/{id}, ant://health, ant://federation, ant://federation/members,
// ant://federation/policy.

func (s *Server) listResources() any {
	return map[string]any{"resources": []any{
		map[string]any{"uri": "ant://feeds", "name": "feeds", "title": "Feeds", "description": "Feeds visible to the caller.", "mimeType": "application/json"},
		map[string]any{"uri": "ant://sources", "name": "sources", "title": "Sources", "description": "Sources visible to the caller.", "mimeType": "application/json"},
		map[string]any{"uri": "ant://health", "name": "health", "title": "Service health", "description": "Liveness, readiness and saturation.", "mimeType": "application/json"},
		map[string]any{"uri": "ant://federation", "name": "federation", "title": "Federation status", "description": "Local federation view, policy head and alerts.", "mimeType": "application/json"},
		map[string]any{"uri": "ant://federation/members", "name": "federation-members", "title": "Federation members", "description": "Members of the applied membership.", "mimeType": "application/json"},
		map[string]any{"uri": "ant://federation/policy", "name": "federation-policy", "title": "Federation policy", "description": "The applied signed federation policy.", "mimeType": "application/json"},
	}}
}

func (s *Server) listTemplates() any {
	return map[string]any{"resourceTemplates": []any{
		map[string]any{"uriTemplate": "ant://feeds/{feed_id}", "name": "feed", "title": "Feed", "mimeType": "application/json"},
		map[string]any{"uriTemplate": "ant://sources/{source_id}", "name": "source", "title": "Source", "mimeType": "application/json"},
	}}
}

func (s *Server) readResource(r *http.Request, p identity.Principal, params json.RawMessage) (any, *rpcError) {
	var in struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &in); err != nil || in.URI == "" {
		return nil, &rpcError{codeInvalidParams, "invalid params"}
	}
	u, err := url.Parse(in.URI)
	if err != nil || u.Scheme != "ant" {
		return nil, &rpcError{codeInvalidParams, "unknown resource"}
	}
	id := strings.TrimPrefix(u.Path, "/")
	var name string
	args := map[string]any{}
	switch u.Host {
	case "feeds":
		name = "list_feeds"
		if id != "" {
			name, args = "get_feed", map[string]any{"feed_id": id}
		}
	case "sources":
		name = "list_sources"
		if id != "" {
			name, args = "get_source", map[string]any{"source_id": id}
		}
	case "health":
		name = "get_health"
	case "federation":
		switch id {
		case "":
			name = "get_federation_status"
		case "members":
			name = "list_federation_members"
		case "policy":
			name = "get_federation_policy"
		default:
			return nil, &rpcError{codeInvalidParams, "unknown resource"}
		}
		id = ""
	default:
		return nil, &rpcError{codeInvalidParams, "unknown resource"}
	}
	t := s.tools[name]
	if (id != "" && u.Host != "health" && !uuidPattern.MatchString(id)) || (id != "" && u.Host == "health") || t.validate(args) != "" {
		return nil, &rpcError{codeInvalidParams, "unknown resource"}
	}
	c := &call{s: s, r: r, p: p, args: args}
	res := t.run(c)
	event := AuditEvent{Action: "mcp.resource.read", Name: name, ResourceID: c.resourceID, Status: c.status, Outcome: "ok"}
	if res.IsError {
		event.Outcome = "error"
	}
	if s.audit != nil {
		if err := s.audit(r.Context(), p, event); err != nil {
			s.logger.Error("mcp audit failed", "resource", name)
			return nil, &rpcError{codeInternal, "audit unavailable; call result withheld"}
		}
	}
	if res.IsError {
		// Surface API denial/errors as JSON-RPC errors (resources have no isError).
		return nil, &rpcError{codeInvalidParams, res.Content[0].Text}
	}
	return map[string]any{"contents": []any{map[string]any{"uri": in.URI, "mimeType": "application/json", "text": res.Content[0].Text}}}, nil
}
