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

There is no per-agent scope store yet, so the caller passes the agent's
effective layers and repos.

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
