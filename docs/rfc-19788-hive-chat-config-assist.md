# RFC #19788: Hive Chat config assist (model-proposed, owner-confirmed config changes)

Status: proposal. Open for maintainer discussion, not accepted.
Target: Hive v5
Scope: dashboard Hive Chat panel, the `chat:` config block, agent config write endpoints, audit log
Out of scope: a write-capable MCP server, writes from Slack/Discord/email chat surfaces, secrets, governance and write-surface settings, hub-level settings
Refs: [#19788](https://github.com/hivecommons/hive/issues/19788), [#19177](https://github.com/hivecommons/hive/issues/19177) (Ask AI help, [design record](../src/docs/design/dashboard-ask-ai-help.md))

> **This page is the design under discussion, not an adopted one.** The issue
> was put on `hold` by a maintainer pending "an RFC that covers which settings
> chat may change, how a change is confirmed and by whom, the audit trail, and
> how it stays off by default". This page answers those four questions and
> lists what is still open in [Decisions for maintainers](#decisions-for-maintainers).
> Nothing here has shipped, and merging this page does not authorize
> implementation tasks. The hold stays in place until a maintainer records a
> decision on the issue ([RFC process](../src/docs/rfc-process.md#what-accepted-means)).

Code references are to `v5` at `20d18ac50` and name a file plus the function or
type. Treat them as pointers, not line-exact citations.

## Summary

- **The model never writes.** It can only produce a *proposal*: one agent, one
  config section, and the new field values. The dashboard renders the proposal
  as a diff card. When the owner clicks **Apply**, the browser calls the
  **existing** section endpoint (`PUT /api/config/agent/{name}/{section}`) with
  the owner's own session. That endpoint's role check, validation, persistence
  and audit line all stay as they are. There is no new write path to the
  config.
- **Who confirms:** an owner, one change at a time. Every agent config PUT
  handler already calls `requireOwnerRole`, so a non-owner can see a proposal
  but cannot apply it. Chat cannot batch changes or apply anything
  automatically.
- **Which settings:** an allowlist that starts small: agent `general`
  (`enabled`, `onDemand`), `cadences`, and `models` (`backend`, `model`,
  `reasoning_effort`). Everything else is out of scope until it gets its own
  decision.
- **Audit:** one `chat_config_proposed` line per proposal and one
  `chat_config_declined` line when the owner dismisses it. The existing
  `config_agent_*` line covers the apply, with `via=chat` and the proposal id
  added to its detail.
- **Off by default:** a new `chat.config_assist.enabled` (`*bool`,
  nil = **false**, the opposite of `grounded_answers`). Even with the flag on,
  the feature stays inert until a model backend is wired.

## What exists today

- `chat.grounded_answers` (`src/pkg/config/chat.go`, `ChatGroundedAnswersConfig`)
  is on by default. It answers free-text chat questions with one model turn
  over a capped snapshot of hive state.
- `src/pkg/dashboard/api_chat_grounded.go` (`chatGroundedAnswer`) runs that
  turn through `deps.ChatGrounder.RunDeciderTurn`. It has a 25s timeout and
  an in-flight cap of 2, writes an audit line per answer
  (`chat_grounded_answer`), and is documented as "read-only and never
  triggers an action".
- The agent config dialog already writes through owner-only section endpoints
  in `src/pkg/dashboard/api_agent_config.go`: `handleAgentConfigGeneral`,
  `handleAgentConfigCadences`, `handleAgentConfigModels` and the rest. Each
  one validates before it mutates (for example `validateAgentGeneralInput`
  and the backend/model/effort checks in `handleAgentConfigModels`), persists
  through `Server.saveConfig` and writes a `config_agent_<section>` audit line
  through `auditFromRequest`.
- `requireOwnerRole` (`src/pkg/dashboard/api_agent_control.go`) requires a
  verified owner role, and hub-proxied identities fail closed without the
  server-side verification marker.

What is missing is a bridge from "the operator describes what they want" to
"the dialog's existing endpoint, filled in". This RFC proposes that bridge and
nothing more.

## Proposal

### 1. Configuration and the off-by-default rule

```yaml
chat:
  grounded_answers:
    enabled: true          # unchanged
    model: ""              # unchanged
  config_assist:
    enabled: false         # nil = false; must be set explicitly
    model: ""              # "" = use chat.grounded_answers.model, then the reviewer default
    proposal_ttl: 10m      # how long a proposal card stays applicable
```

- `ChatConfig.ConfigAssistEnabled()` returns true only when
  `config_assist.enabled` is explicitly `true`. Turning on `grounded_answers`
  does not turn this on.
- Even when enabled, the feature is inert if no turn runner is wired
  (`deps.ChatGrounder == nil`). In that case chat says so and links to the
  setting instead of guessing.
- The model is resolved as `config_assist.model`, then
  `grounded_answers.model`, then the reviewer default. It reuses the same
  runner, so no new credential or endpoint is introduced. See
  [D2](#decisions-for-maintainers).
- **NUX** (the "encourage it being configured" part of the issue): when the
  flag is off and an **owner** asks chat something config-shaped, chat answers
  as it does today and adds one dismissible hint pointing to
  Settings → Chat → Config assist. Non-owners never see the hint. Chat never
  asks for an API key; model credentials stay where they are configured today
  (backends / governor reviewer endpoint).

### 2. Which settings chat may change

The allowlist lives in code as a table. Each row names a section endpoint and
the fields of that endpoint's body that a proposal may set:

| Section | Endpoint | Fields a proposal may set |
|---|---|---|
| `general` | `PUT /api/config/agent/{name}/general` | `enabled`, `onDemand` |
| `cadences` | `PUT /api/config/agent/{name}/cadences` | this agent's cadence in an existing governor mode (`governor.modes.<mode>.cadences.<agent>`); unknown modes are refused, not skipped |
| `models` | `PUT /api/config/agent/{name}/models` | `backend`, `model`, `reasoning_effort` |

These sections are **explicitly excluded** and would each need their own
decision: `hooks`, `restrictions`, `tools`, `connections` (they carry
credentials), `channels`, `prompt`, `pipeline`, the write-surface allowlist,
governor and merge-train settings, auth/OIDC/keys, hub settings, reference
repos, variables, and anything that adds or removes repos or agents.

The excluded sections are the ones that widen what an agent may do, or that
hold secrets. The allowed ones change *which* agent runs, *when*, and on
*which model*. A wrong value is visible in the diff card, easy to revert, and
already validated by the endpoint.

The issue's motivating example is setting up a new spoke, which touches repos,
connections and hub registration. That is deliberately *not* phase 1 (see
[D4](#decisions-for-maintainers)). Phase 1 can still *explain* those settings
through the existing read-only grounded path.

### 3. The proposal turn

1. A chat message from an owner, with `config_assist` on and a runner wired,
   is routed to a new `chatRouteConfig` kind when it reads as a config change
   ("switch reviewer to opus", "run the triage agent every 30 minutes").
   Messages from non-owners stay on the read-only grounded path, unchanged.
2. The server builds a snapshot that holds only allowlisted values: for each
   agent, its current `general`/`cadences`/`models` fields, the backends and
   models that are valid for it (the same lists the dialog offers), and the
   allowlist table itself. No connection auth, tokens, prompts or hooks are
   included.
3. One turn runs with the same timeout and in-flight cap as grounded answers.
   The model must reply with a JSON object
   `{"agent": ..., "section": ..., "set": {field: value}, "why": ...}` or with
   `{"none": "<reason>"}`. Free text is not acted on.
4. The server checks the reply and drops it if any check fails:
   - the agent exists;
   - the section is in the allowlist;
   - every key in `set` is an allowlisted field of that section;
   - the merged body passes the **same validation function** the endpoint
     uses. To make that possible, the endpoints' validation is extracted into
     pure `validate<Section>Body(cfg, name, body)` helpers, and both the
     endpoint and the proposal check call them. No second rulebook is written.
5. A valid proposal is stored server-side with a random id, the requesting
   user, a hash of the agent's current section values and an expiry
   (`proposal_ttl`). The chat reply is a card that shows the before/after diff,
   the model's one-line `why`, and **Apply** / **Dismiss** buttons.

Each proposal changes one section of one agent. When the request needs more
than that, the model names the first change and says what is left. The owner
then asks again.

### 4. How a change is confirmed, and by whom

- **Apply** sends the exact body shown in the card to the existing section
  endpoint, with the owner's session and an `X-Hive-Chat-Proposal: <id>`
  header.
- The endpoint behaves as it does today: `requireOwnerRole`, validate, mutate,
  `saveConfig`, audit. One check is added. When the header is present, the
  proposal must exist, be unexpired, belong to the same user, have a body
  equal to the stored one, and match the section hash it was built from.
  Otherwise the request gets `409` and the card asks the owner to request a
  fresh proposal. This stops a proposal built on stale config from overwriting
  an edit made in between.
- A proposal is single-use. After Apply, Dismiss or expiry it is gone.
- The header is **only** a binding check. A request without it is a normal
  dialog save and is unaffected. Chat has no server-side code path that calls
  the endpoint itself.

### 5. Audit trail

| Action | When | Detail |
|---|---|---|
| `chat_config_proposed` | valid proposal shown | proposal id, agent, section, field names (values for `models`/`general`; cadence values), question truncated to `chatGroundedMaxQuestion` |
| `chat_config_rejected` | model reply failed validation | agent/section if parseable, reason (not the raw reply) |
| `config_agent_<section>` (existing) | owner clicked Apply | existing detail plus `via=chat`, `proposal=<id>` |
| `chat_config_declined` | owner dismissed the card | proposal id |

Every new action name goes into `auditSensitiveActions`
(`src/pkg/dashboard/audit.go`), so the Audit Log risk chip counts them. All of
them go through `auditFromRequest`, so they are attributed to the real user,
never to `system`.

### 6. Why not a write-capable MCP server

The issue floats "a write-allowed MCP server that is exposed to this". This
RFC recommends against it for this feature:

- Chat snapshots contain untrusted text: issue titles, PR titles, agent
  "doing" lines. Prompt injection in that text could make a model with a
  write tool change config without an owner in the loop. A proposal-only
  model has nothing to call.
- An MCP surface would need its own role checks, validation and audit, which
  duplicates the owner-only endpoints that already exist and lets the two
  drift apart.
- The confirm-then-call-the-endpoint design costs one click per change.

If a write MCP surface is wanted for other reasons (agents configuring hive),
that is a separate RFC.

## Phases

Each phase is its own issue, behind `chat.config_assist.enabled` (default off).

1. **Config and NUX:** the `chat.config_assist` block, accessor, Settings
   entry and owner-only hint. No proposals yet.
2. **Validation extraction:** pull `validate<Section>Body` out of the
   `general`/`cadences`/`models` handlers with no behaviour change, covered by
   the handlers' existing tests.
3. **Proposals:** routing, the allowlisted snapshot, the turn, server-side
   proposal check and storage, the diff card, the `X-Hive-Chat-Proposal`
   binding check, and the audit actions.
4. **Further sections / spoke setup:** only after a separate decision per
   section, starting with whatever a maintainer picks under D4.

## Decisions for maintainers

- **D1. Allowlist for phase 1.** Is `general` (`enabled`, `onDemand`) +
  `cadences` + `models` right? Should `enabled` be excluded, since turning an
  agent on starts spend?
- **D2. Model knob.** Should there be a separate `chat.config_assist.model`
  that falls back to `grounded_answers.model`, as proposed, or should it reuse
  `grounded_answers.model` with no new knob?
- **D3. Who may apply.** Owner only, as proposed (matching the endpoints
  today). Or should a merger also be able to *request* proposals while only an
  owner applies them?
- **D4. Spoke setup.** The issue's motivating case. Should a later phase add
  a guided, multi-step "new spoke" flow where every step is still a separate
  owner-confirmed write? Or should spoke setup stay in the existing wizard
  with chat only explaining it?
- **D5. Other chat surfaces.** Proposed: dashboard only. Slack/Discord/email
  chat stay read-only because they have no owner-verified session to confirm
  with.

## Not in scope

- Automatic or batched config changes, and any change without a per-change
  owner click.
- Secrets, credentials, connection auth, prompt templates, hooks, tool and
  write-surface permissions.
- Changes to the grounded-answers path or its default.
- The Ask AI help panel (#19177), which answers "where is X in the dashboard"
  and takes no action.
