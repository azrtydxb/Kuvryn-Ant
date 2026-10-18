# internal/mcp

Embedded MCP (Model Context Protocol) server, spec section 6.11. Streamable HTTP transport at `POST /mcp` (JSON-RPC 2.0, stateless, JSON responses; `GET` returns 405, no SSE stream, no sessions). Protocol versions 2025-06-18, 2025-03-26 and 2024-11-05. Implemented with the standard library only.

## Security model

- Authentication is the management API's Bearer middleware: the same scoped API tokens and service accounts.
- No tool contains SQL or its own authorization. Each tool dispatches in-process to the management API handlers with the caller's `Authorization` header, so authorization decisions, idempotency, validation and audit events are exactly those of the HTTP API. A token that the API denies is denied identically through MCP.
- Every `tools/call` and `resources/read` also writes an `mcp.tool.call` / `mcp.resource.read` audit event (tool name, outcome, HTTP status, token ID, target UUID; never arguments or results). If the audit write fails the result is withheld (fail closed).
- Destructive tools (`detach_source`, `disable_webhook`, `delete_package_version`, `revoke_service_token`, `purge_source_cache`, `start_retention_run`, `start_storage_job`) carry `destructiveHint: true` and refuse to run unless `confirm: true` is passed. Refusals are audited with outcome `confirmation_required`.
- Errors: protocol and argument errors are JSON-RPC errors (-32700, -32600, -32601, -32602, -32603). API failures (denied, not found, conflict) are tool results with `isError: true` carrying only the API's stable `code`, `message` and `request_id`. Raw bodies of failed calls and internal error text are never forwarded.
- Cross-origin browser requests (an `Origin` that differs from `Host`) are rejected.

## Tools

Read-only: `list_feeds`, `get_feed`, `list_sources`, `get_source`, `list_feed_sources`, `get_publish_policy`, `search_packages`, `list_package_versions`, `list_package_tags`, `list_package_assets`, `get_asset`, `lookup_checksum`, `get_health`, `get_cache_insight`, `list_webhooks`.

Admin: `create_feed`, `attach_source`, `set_publish_policy`, `rotate_webhook_secret` (generates and returns a secret once when none is supplied), and the destructive `detach_source`, `disable_webhook`.

Operations (all call the same handlers as the console): replication `list_replication_peers`, `get_replication_peer`, `list_replication_jobs`, `get_replication_job`, `get_replication_job_status`, `get_replication_status`, `preview_replication_deletions`, `run_replication_job`; retention `list_retention_policies`, `list_retention_runs`, `get_retention_run`, `list_integrity_findings`, `start_retention_run` (dry run by default); storage `get_storage_usage`, `get_storage_migration`, `list_storage_jobs`, `get_storage_job`, `start_storage_job` (dry run by default), `storage_job_action`; identity `list_service_tokens`, `revoke_service_token`, `list_identity_bindings`; `query_audit`, `purge_source_cache`, `resolve_coordinate`, `test_publish_policy`, `list_feed_stats`, `list_source_stats`, `list_source_health`, `get_traffic_metrics`, `get_settings`, `get_diagnostics`, `get_delete_policy`, `submit_security_result`, `delete_package_version`.

Mutating tools accept an optional `idempotency_key` (replay-safe retries) and `revision` (otherwise the current revision is read first and sent as `If-Match`).

Resources: `ant://feeds`, `ant://feeds/{feed_id}`, `ant://sources`, `ant://sources/{source_id}`, `ant://health`.

## Client configuration

Create a token with the minimum scopes needed (a read-only token yields a read-only MCP server). Example for Claude Code or any client supporting Streamable HTTP:

```json
{
  "mcpServers": {
    "kuvryn-ant": {
      "type": "http",
      "url": "https://ant.example.com/mcp",
      "headers": { "Authorization": "Bearer ${ANT_TOKEN}" }
    }
  }
}
```

Smoke test:

```sh
curl -s https://ant.example.com/mcp -H "Authorization: Bearer $ANT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## Tests

`go test ./internal/mcp/...` runs protocol unit tests. `scripts/test-postgres.sh ./internal/mcp/...` adds the database integration tests (authorization parity with the API, confirmation enforcement, audit emission).
