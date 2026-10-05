# Public knowledge MCP endpoint

A hive accumulates operational knowledge — gotchas, patterns, regressions,
integration notes — as its agents work. Normally that knowledge is reachable
only with a hub session, the dashboard token, or a contributor registration
token (`/api/knowledge/*`, `hivectl knowledge export`).

The **public knowledge** switch lets a hive owner open a second, strictly
**read-only** surface so *any* agent — Goose, Claude Desktop/Code, Copilot
CLI, a shell script — can consult the hive without credentials. The
motivating case is a distro's troubleshooting assistant reasoning from the
project's own accumulated fixes instead of only what its model was trained on
([#10615](https://github.com/hivecommons/hive/issues/10615)).

## Owner switch

| Variable | Effect |
|---|---|
| `HIVE_PUBLIC_KNOWLEDGE=1` (`true`/`yes`/`on`) | Enables `POST /mcp/knowledge`. Unset or anything else → the path returns **404**, even to the owner. |
| `HIVE_PUBLIC_KNOWLEDGE_TAGS=linux,troubleshooting` | Optional. Only facts carrying at least one listed tag are served. |

Both are read on every request, so the surface can be closed instantly
without restarting the spoke.

## What is exposed

Only operational fact types leave the hive:

`pattern`, `gotcha`, `regression`, `test_scaffold`, `integration`,
`coverage_rule`, `general`

Ideation and governance types (`idea`, `vision`, `constitution`,
`requirement`, `constraint`, `stakeholder`, `decision`) are never served,
and a request for one by slug answers "not found" rather than "forbidden".

Each public fact carries `slug`, `title`, `type`, `body`, `tags`, `related`
and (only when scored) `confidence`. Sources (PR, comment, author), usage
counters, confidence reasoning, and lifecycle phase are stripped.

## What is *not* possible

There is no write method on this surface. The endpoint serves the MCP
handshake (`initialize`, `ping`, `notifications/*`), `tools/list`, and
`tools/call` for three tools. Nothing in the handler reaches
`CreateFact`, `UpdateFact`, `DeleteFact`, import, promotion, or vault
mutation. Anonymous callers cannot alter a knowledge base, and every tool is
annotated `readOnlyHint: true` so MCP clients run them without confirmation.

Other hard limits: POST only (no SSE stream), 64 KiB request body, one
JSON-RPC request per POST (batch arrays are answered with `-32600`; the
endpoint speaks MCP `2025-06-18`, which dropped batching), and
`knowledge_search` returns at most 50 results per call.

A fact's `related` list is filtered to public slugs, so a public fact cannot
disclose even the name of a private one. `knowledge_get` and
`knowledge_export` return complete bodies; `knowledge_search` results do too,
re-read per hit.

## Tools

| Tool | Arguments | Returns |
|---|---|---|
| `knowledge_search` | `query` (required), `type`, `limit` (default 10, max 50) | JSON `{query, count, results[]}` |
| `knowledge_get` | `slug` (required) | One public fact as JSON, or an `isError` "fact not found" |
| `knowledge_export` | none | The whole public base as one Markdown document, grouped by type; `_meta.etag` and `_meta.facts` |

## Client configuration

The endpoint speaks MCP Streamable HTTP with plain JSON responses.

**Goose** (`~/.config/goose/config.yaml`):

```yaml
extensions:
  bluefin-hive:
    type: streamable_http
    uri: https://hive.example.org/mcp/knowledge
    enabled: true
```

**Claude Code**:

```sh
claude mcp add --transport http bluefin-hive https://hive.example.org/mcp/knowledge
```

**Copilot CLI** (`~/.copilot/mcp-config.json`):

```json
{ "mcpServers": { "bluefin-hive": { "type": "http", "url": "https://hive.example.org/mcp/knowledge" } } }
```

**curl**:

```sh
curl -s https://hive.example.org/mcp/knowledge \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge_search","arguments":{"query":"bluetooth"}}}'
```

## Hub-fronted spokes

On hosted hives the hub's nginx `auth_request` gate waves `/mcp/knowledge`
through (same mechanism as `/api/knowledge/export`, #8294) so an agent with
no browser session is not bounced to the login page. This opens nothing by
itself: the spoke re-checks `HIVE_PUBLIC_KNOWLEDGE` and 404s when it is off.

## Related

- [Environment variable reference](env-vars.md)
- [Knowledge curator](knowledge-curator.md) — how facts are produced and promoted
- [Knowledge system design](design/knowledge-system.md)
