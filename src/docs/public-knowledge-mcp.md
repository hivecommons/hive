# Public knowledge MCP endpoint

**MCP means Model Context Protocol:** it lets an AI app call tools supplied by another service. This connection searches a hive's public notes without credentials; it cannot change them.

## Enable, connect, check, and disconnect

1. **If you only want to read:** ask the owner for the public knowledge URL. You do not need a Hive account or owner controls. If you own the hive, first use [Owner switch](#owner-switch) to enable sharing and choose any tag filter.
2. Choose one example under [Client configuration](#client-configuration). Replace `https://hive.example.org/mcp/knowledge` with the owner's URL. `hive.example.org` is a placeholder, and `bluefin-hive` is an arbitrary name stored in your own client, not a required hive identifier.
3. Ask the client to call `knowledge_search` with `{"query":"bluetooth"}`, or use the curl example. A successful tool result contains `query`, `count`, and `results`. For example, `{"query":"bluetooth","count":0,"results":[]}` is a successful search with no matching public notes, not a connection failure. Matches contain the public fact fields listed below. MCP clients may wrap that JSON in a tool-result content block.
4. A disabled endpoint returns **404**. Confirm the URL and owner setting before troubleshooting credentials; this endpoint needs none.
5. To stop using the connection, remove `bluefin-hive` from the client configuration you added (in Claude Code, `claude mcp remove bluefin-hive`). To stop publishing for everyone, the **owner** turns the sharing switch off; this takes effect without a restart.

## Publication reference

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

Owners can use the dashboard instead of editing environment variables:

- **Knowledge** section header: `Public read-only MCP: Off/On`.
- **Settings → Knowledge → Knowledge sharing**: the same control, plus tags
  and a copyable endpoint URL.

The dashboard setting is persisted in `hive.yaml` and takes precedence over
the environment variables below. If no dashboard setting has been saved, the
legacy env-only behavior remains unchanged.

| Variable | Effect |
|---|---|
| `HIVE_PUBLIC_KNOWLEDGE=1` (`true`/`yes`/`on`) | Enables `POST /mcp/knowledge`. Unset or anything else → the path returns **404**, even to the owner. |
| `HIVE_PUBLIC_KNOWLEDGE_TAGS=linux,troubleshooting` | Optional. Only facts carrying at least one listed tag are served. |

Both the dashboard setting and env fallback are resolved on every request, so
the surface can be closed instantly without restarting the spoke.

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

A *slug* is a fact's lookup name. An *endpoint* is the URL the app calls; this public endpoint is separate from Hive's authenticated knowledge API.

## What is *not* possible

There is no write method on this surface. The endpoint serves the MCP
handshake (`initialize`, `ping`, `notifications/*`), `tools/list`, and
`tools/call` for four tools. Nothing in the handler reaches
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
| `knowledge_toc` | `type`, `repo`, `limit` (all optional; default 50, max 200) | A compact table of contents of public facts — `id`, `title`, `type`, `layer`, `repo`, `tags`, `status`, `updated`, `size_bytes` — with `total`/`returned`/`truncated`, no bodies and no source URLs. Read a chosen `id` with `knowledge_get` |
| `knowledge_get` | `slug` (required) | One public fact as JSON, or an `isError` "fact not found" |
| `knowledge_export` | none | The whole public base as one Markdown document, grouped by type; `_meta.etag` and `_meta.facts` |

## Client configuration

The endpoint speaks MCP Streamable HTTP with plain JSON responses: clients send JSON-RPC requests over HTTP. It does not open an SSE (server-sent events) stream.

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
itself: the spoke re-checks the dashboard setting (or
`HIVE_PUBLIC_KNOWLEDGE` when no setting exists) and 404s when it is off.

Here a *spoke* is the individual hive behind the shared hub, and a *distro* is an operating-system distribution whose assistant might consume its public notes. Reverse-proxy details matter to the hosting operator, not to an anonymous client connecting with the public URL.

## Related

- [Environment variable reference](env-vars.md)
- [Knowledge curator](knowledge-curator.md) — how facts are produced and promoted
- [Knowledge system design](design/knowledge-system.md)
