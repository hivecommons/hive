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
- `types`, `tags` — fact type, and any-match tag filters. A fact with no type
  of its own is listed as `general`, and `types=general` matches it. (An
  agent scope's `types` below still compares the type as stored.)
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

The kick primer makes one exception for an agent's own drafts (#17076). When
an agent scope lists `draft` (or `all`) explicitly, a kick for that agent also
carries draft entries whose `author:` (alias `owner:`) frontmatter matches the
agent name, its base agent (for replicas) or an owner login from
`dashboard.authorized_users`; they are rendered with a `(draft)` marker.
Drafts by other authors, and drafts without an author, remain excluded, and an
empty `include_states` does not opt in. TOC and entry reads are unaffected —
they keep the intersection rule above.

Config load rejects unknown layer names (`personal`, `project`, `org`,
`community`) and unknown state names (`draft`, `approved`, `deprecated`,
`superseded`, `all`). The scopes can also be read and replaced with
`GET`/`PUT /api/config/knowledge/agent-scopes` (owner only for `PUT`); a `PUT`
is validated as a whole, saved to `hive.yaml` and audited as
`config_knowledge_agent_scopes`.

### Effective knowledge

`GET /api/knowledge/effective?agent=<name>[&limit=N]` explains what an agent
receives. It resolves the agent's `knowledge.agent_scopes` entry (an agent
without one is unrestricted), intersects it with the request scope (approved
only unless `include_states` is set), and returns:

- `scope`: the resolved scope applied — `layers`, `repos`, `types`, `tags`,
  the lifecycle states both sides admit (`include_states`) and `configured`
  (whether the agent has an entry).
- `included`: the TOC entries the agent would receive (`id`, `title`, `layer`,
  `repo`, `type`, `status`, …).
- `excluded`: the entries filtered out, each with a `reason` code. The checks
  run in the order `lifecycle`, `layer`, `repo`, `type`, `tag` and the first
  failing one is reported. A lower-precedence copy of an included id is
  shadowed by layer precedence and is not listed.
- `included_total`, `excluded_total`, `included_returned`,
  `excluded_returned`, and `truncated` when either list hit `limit`.

The dashboard Knowledge section has an **Effective knowledge** panel with an
agent picker that renders both lists and each exclusion reason.

### Entry scope (`scope:` frontmatter)

`knowledge.agent_scopes` says what an *agent* may see. An entry can also say
where *it* applies, with a `scope:` block in its frontmatter
(hivecommons/hive#17045):

```yaml
---
title: Editing src/scripts/ requires re-pinning TRUSTED_SCRIPTS_TREE
type: gotcha
layer: project
scope:
  paths: [src/scripts/**, .github/scripts/integration-governance.cjs]
  labels: [area/ci]
  lanes: []
---
```

- `paths` are repo-relative globs (`**` matches any number of segments),
  `labels` are issue labels and `lanes` are base agent names; `labels` and
  `lanes` compare case-insensitively. Inline lists only — the frontmatter
  parser is line-oriented and block lists (`- item`) inside `scope:` are
  skipped.
- An entry **without** `scope:` behaves as before: keyword relevance plus the
  agent's `knowledge.agent_scopes`.
- An entry **with** `scope:` is surfaced in a kick only when at least one
  declared dimension matches the kick — its lane, a label on one of the primed
  issues, or a file path the caller knows. Dimensions are OR-ed; a dimension the
  kick cannot evaluate (file paths are not known on the kick path) simply does
  not match. Related facts pulled in by graph expansion are gated the same way.
- The primer reports how many relevant entries the kick context excluded in the
  `knowledge primed` log line (`scoped_out`) and in `PrimedKnowledge.ScopedOut`.

The full read (`GET /api/knowledge/entry/{id}`) returns the entry's `scope`
inside `fact`, so an operator can see why an entry does or does not reach a
kick. The agent-level filter (`agent_scopes`) applies first; the entry's own
scope can only narrow further.

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
