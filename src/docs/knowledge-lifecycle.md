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
  states.
- **`GET /api/knowledge/search`** returns approved facts only unless the
  operator passes `include_states`, e.g. `include_states=deprecated,superseded`
  or `include_states=all`.
- **Public knowledge MCP** (`/mcp/knowledge`) only ever publishes approved
  facts.
- **`GET /api/knowledge/toc`** and **`GET /api/knowledge/entry/{id}`** apply
  the same default and `include_states` opt-in; see
  [Scoped knowledge table of contents](knowledge-toc.md).
- `GET /api/knowledge` (the dashboard inventory) still lists every fact, and
  vault facts report their `state`, so operators can review and restore them.

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
  layers, git sources and the reserved automation channel return 404.
- Every change writes an audit entry: `knowledge_set_state`, or
  `knowledge_supersede` for supersession. The entry records the actor, the
  entry id and channel, the previous and new state, `superseded_by`, and the
  `reason` (whitespace collapsed, capped at 500 characters).
- The response returns the `channel`, `previous_state`, the new `state`,
  `superseded_by` and the updated `fact`.

Restoring a fact to `approved` clears its `superseded_by` link. It then
reaches primers and the table of contents again.
