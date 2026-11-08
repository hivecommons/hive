# Knowledge lifecycle states

Every knowledge fact has a lifecycle `state` that decides whether it may reach
agents:

| State | Meaning | Reaches agents by default |
|---|---|---|
| `approved` | Current, curated knowledge | yes |
| `draft` | Proposed, not yet reviewed | no |
| `deprecated` | No longer valid | no |
| `superseded` | Replaced by another fact (`superseded_by`) | no |

## On-disk format

Vault and git-source facts carry the state in their YAML frontmatter:

```markdown
---
title: Old deploy flow
state: superseded
superseded_by: new-deploy-flow
---
```

The replacement fact records the reverse link as `supersedes: <slug>`.

Facts written before lifecycle states existed have no `state:` key and load as
`approved`, so existing vaults keep working unchanged. A fact with a
`superseded_by:` link, or a legacy `status: deprecated` / `status: superseded`
(for example a connector tombstone), loads as `superseded` / `deprecated`.
Remote wiki layers only carry a `status`, which is mapped the same way.

## Filtering

- **Kick primers** include approved facts only. Set
  `knowledge.PrimerConfig.IncludeStates` (`include_states`) to admit other
  states. An agent whose `knowledge.agent_scopes` entry lists `draft` is also
  primed with the drafts it (or the hive owner) authored, marked `(draft)`;
  see [Per-agent scopes](knowledge-toc.md#per-agent-scopes).
- **`GET /api/knowledge/search`** returns approved facts only unless the
  operator passes `include_states`, e.g. `include_states=deprecated,superseded`
  or `include_states=all`.
- **Public knowledge MCP** (`/mcp/knowledge`) only ever publishes approved
  facts.
- **`GET /api/knowledge/toc`** and **`GET /api/knowledge/entry/{id}`** apply
  the same default and `include_states` opt-in; see
  [Scoped knowledge table of contents](knowledge-toc.md).
- `GET /api/knowledge` (the dashboard inventory) applies the same default and
  `include_states` opt-in, and vault facts report their `state`. An owner can
  list exactly one set of states with `state`, e.g.
  `state=draft&tag=retro,review-feedback` lists retro lessons derived from
  human review that await approval (`hivectl knowledge list --state draft
  --tag review-feedback`). Asking for any state other than `approved` without
  the owner role is refused with 403. The dashboard Knowledge section shows
  the owner "N lessons awaiting approval" when such drafts exist.

## Changing a fact's state

`PUT /api/knowledge/{layer}/{slug}` accepts two lifecycle fields. When
`{layer}` names a local channel (vault), they rewrite the fact's frontmatter:

- `{"state": "deprecated"}` (or `draft` / `approved`) sets the state.
- `{"superseded_by": "new-slug"}` marks the fact `superseded`, records
  `superseded_by: new-slug` on it and `supersedes: <slug>` on the replacement.
  Both facts must be in the same channel.

On a remote wiki layer `state` is forwarded as the page `status`; supersession
links are only supported in local channels.

### Changing state over the API

Operators change an entry's state by id with an audited, owner-only call. The
channel does not need to be named:

```http
PUT /api/knowledge/entry/old-deploy-flow/state
{"state": "superseded", "superseded_by": "new-deploy-flow", "reason": "deploy flow rewritten"}
```

```bash
hivectl knowledge state old-deploy-flow deprecated --reason "deploy flow removed"
```

- `state` must be `draft`, `approved`, `deprecated` or `superseded`.
  `superseded` requires `superseded_by`, and `superseded_by` is only accepted
  with `superseded`. The replacement must be in the same local channel as the
  entry. Invalid input returns 400.
- The entry must be in a local channel (vault). Entries from remote wiki
  layers, git sources and the reserved automation channel return 404. The one
  exception is a drained CLI-memory entry (`source: cli-memory`) in the
  `bead-synth-wiki` channel; see [Auto-memory drafts](#auto-memory-drafts).
- Every change writes an audit entry: `knowledge_set_state`, or
  `knowledge_supersede` for supersession. The entry records the actor, the
  entry id and channel, the previous and new state, `superseded_by`, and the
  `reason` (whitespace collapsed, capped at 500 characters).
- The response returns the `channel`, `previous_state`, the new `state`,
  `superseded_by` and the updated `fact`.

Restoring a fact to `approved` clears its `superseded_by` link. It then
reaches primers and the table of contents again.

## Auto-memory drafts

With `agents.cli_memory.drain_to_knowledge` on, each agent's coding-CLI memory
(Claude Code `MEMORY.md` and topic files, Codex `memories*/`, Gemini
`GEMINI.md`) is drained into `state: draft` entries in the `bead-synth-wiki`
vault ([#17033](https://github.com/hivecommons/hive/issues/17033)). Each entry
has `source: cli-memory` and the tags `cli-memory`, `agent:<name>`,
`cli:<name>`, `memhash:<sha256>` and, when the project path names one of the
agent's repos, `repo:<org>/<repo>`. The shared home's pre-isolation store is
drained once, on demand, with
[`hivectl knowledge drain-memory --legacy`](hivectl.md); those entries are
tagged `agent:shared`.

The text is agent-written and unreviewed: a model may have copied it from an
issue body or a fetched page. So an auto-memory draft never reaches an agent
([#17058](https://github.com/hivecommons/hive/issues/17058)):

- The kick primer and the knowledge advisor never prime it, even when the
  primer's `include_states` or the agent's `knowledge.agent_scopes` entry
  admits `draft`, and even when its author matches the kicked agent.
- An agent-identified read (`?agent=<name>` on `GET /api/knowledge/toc`,
  `GET /api/knowledge/entry/{id}` and `GET /api/knowledge/effective`) never
  returns it, whatever `include_states` the request names. The effective view
  lists it as excluded with reason `lifecycle`.
- The public knowledge MCP publishes approved facts only.
- The curator's scheduled auto-promotion never selects a page tagged
  `cli-memory`; it moves to a broader layer only through a manual promote.

Operator reads still show these drafts so they can be reviewed: pass
`include_states=draft` to `GET /api/knowledge/search` or
`GET /api/knowledge/toc` without `?agent=`.

### Reviewing them

In the dashboard's knowledge view, **🧠 Auto-memory drafts** in the sources
list shows every drained draft with the agents it was drained from. An owner
gets two actions per row, both backed by `PUT /api/knowledge/entry/{id}/state`
and audited:

- **Approve** sets `approved`. From the next kick on, the entry is primed for
  every agent whose scope includes it.
- **Discard** sets `deprecated`. It reaches no agent, and because the drain
  deduplicates by `memhash` across all states, the same memory is never drafted
  again. Deleting the file instead would let the next drain re-create it.

Outside the dashboard, list them with
`GET /api/knowledge/search?tag=cli-memory&include_states=draft` and change
their state from the CLI:

```bash
hivectl knowledge state cli-memory-1a2b3c4d5e6f7a8b approved --reason "verified"
hivectl knowledge state cli-memory-1a2b3c4d5e6f7a8b deprecated --reason "one-off"
```
