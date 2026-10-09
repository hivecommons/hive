# Scoped knowledge table of contents

Agents should know what knowledge exists without every entry being pasted into
their prompt. The table of contents (TOC) lists the entries visible under a
scope as compact rows, and a full-read endpoint returns the one entry an agent
chooses. Both honour the [lifecycle](knowledge-lifecycle.md) default: only
`approved` entries are listed or readable unless an operator opts in.

## Table of contents

`GET /api/knowledge/toc` returns rows with no bodies:

| Field | Meaning |
|---|---|
| `id` | Entry slug; pass it to the full-read endpoint |
| `title`, `type`, `layer` | Display title (capped at 100 characters), fact type, wiki layer |
| `repo` | Repository from a `repo:<owner/name>` tag, empty for org-wide entries |
| `tags` | Up to 6 tags |
| `status` | Lifecycle state (`approved` unless included explicitly) |
| `confidence` | Present only when the entry has a scored confidence |
| `updated`, `size_bytes` | Freshness (file modification time) and body size, for file-backed stores |
| `source` | Source URL or label (capped at 160 characters) for citation |
| `superseded_by` | Replacement entry, when there is one |

The response also carries `total` (in-scope entries before the cap),
`returned` and `truncated`. Entries are ordered by layer precedence (personal,
project, org, community), then most recently updated, then id. A slug present in
several layers is listed once, from the highest-precedence layer.

### Scope

All parameters are optional, comma-separated allow-lists:

- `layers` — e.g. `project,org`.
- `repos` — an entry tagged `repo:<name>` is hidden unless the name is listed;
  entries without a repo tag are org-wide and always pass.
- `types`, `tags` — fact type, and any-match tag filters.
- `include_states` — operator opt-in for `draft`, `deprecated`, `superseded` or
  `all`.

An agent-identified request adds `agent=<name>`, which applies that agent's
[per-agent scope](#per-agent-scopes) on top of the parameters above.

### Per-agent scopes

`knowledge.agent_scopes` in `hive.yaml` binds an agent to the knowledge it may
see:

```yaml
knowledge:
  agent_scopes:
    scanner:
      layers: [project, org]
      repos: [hivecommons/hive]
      types: [pattern, decision]
      tags: [ci]
    architect:
      include_states: [deprecated]
```

Each field is an allow-list with the same meaning as the matching query
parameter. An agent with no entry, or an empty field, is unrestricted, so
nothing changes until an operator adds a scope. A replica without its own
entry uses its base agent's scope.

The scope applies to the agent's kick primer (including related facts pulled
in through the knowledge graph) and to `GET /api/knowledge/toc` and
`GET /api/knowledge/entry/{id}` requests that pass `agent=<name>`. It is
intersected with the request scope: an entry is returned only when both admit
it, so a request can narrow an agent's scope but never widen it, and the entry
endpoint answers `404` for anything outside it.

`include_states` on an agent scope works the same way. Approved entries are
always admitted; a draft, deprecated or superseded entry reaches the agent
only when the agent scope lists that state (or leaves `include_states` empty)
**and** the request's `include_states` (or, for kicks, the primer's) lists it
too. An agent scope therefore never admits a state the operator did not ask
for.

Config load rejects unknown layer names (`personal`, `project`, `org`,
`community`) and unknown state names (`draft`, `approved`, `deprecated`,
`superseded`, `all`). The scopes can also be read and replaced with
`GET`/`PUT /api/config/knowledge/agent-scopes` (owner only for `PUT`); a `PUT`
is validated as a whole, saved to `hive.yaml` and audited as
`config_knowledge_agent_scopes`.

### Prompt-size caps

- `limit` defaults to 50 and is clamped to 200.
- `format=prompt` adds a `prompt` field: a markdown list that never exceeds
  `max_chars` (default 4000) and ends with `(N more entries not listed)` when
  rows were dropped, so the agent knows to narrow its scope.

## Full read

`GET /api/knowledge/entry/{id}` returns `markdown` (YAML front-matter plus the
complete body) and the structured `fact`. It takes the same scope and
`include_states` parameters as the TOC and answers `404` for an entry the TOC
would not list, so an id cannot be guessed past the scope or lifecycle filter.

## MCP

The public knowledge MCP endpoint exposes `knowledge_toc` (`type`, `repo`,
`limit`) next to `knowledge_get`, which reads one entry in full. It lists public
facts only, never includes source URLs, and uses the same caps. See
[Public knowledge MCP endpoint](public-knowledge-mcp.md).
