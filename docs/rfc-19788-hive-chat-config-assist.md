# RFC #19788: Hive Chat config assist (model-proposed, owner-confirmed config changes)

Status: accepted ([#19835](https://github.com/hivecommons/hive/pull/19835)). Phase 1 ([#19979](https://github.com/hivecommons/hive/issues/19979)), phase 2 ([#19977](https://github.com/hivecommons/hive/issues/19977)), phase 3a ([#19978](https://github.com/hivecommons/hive/issues/19978), proposals with "Open in agent config") and phase 3b ([#19985](https://github.com/hivecommons/hive/issues/19985), direct Apply and Undo) are implemented.
Target: Hive v5
Scope: dashboard Hive Chat panel, the `chat:` config block, agent config write endpoints, audit log
Out of scope: a write-capable MCP server, writes from Slack/Discord/email chat surfaces, secrets, governance and write-surface settings, hub-level settings
Refs: [#19788](https://github.com/hivecommons/hive/issues/19788), [#19177](https://github.com/hivecommons/hive/issues/19177) (Ask AI help, [design record](../src/docs/design/dashboard-ask-ai-help.md)), [#19740](https://github.com/hivecommons/hive/issues/19740) (overlay reload drops hand-edited blocks)

> **This design was accepted on [#19788](https://github.com/hivecommons/hive/issues/19788)**
> after review in [#19835](https://github.com/hivecommons/hive/pull/19835). It is
> implemented one phase per issue; the status line above says which phases have
> shipped. [Decisions for maintainers](#decisions-for-maintainers) D2 and D4
> are still open (D6 was settled by shipping 3b) and are settled when their phase is picked up. Phase 3a keeps
> the proposal TTL, rate and caps as code constants (the values in
> [§3](#3-the-proposal-turn)); the `config_assist.model` and `proposal_ttl` knobs
> wait on D2.

Code references are to `v5` at `302fcc6e0` and name a file plus the function or
type. Treat them as pointers, not line-exact citations.

## Summary

- **The model never writes.** It can only produce a *proposal*: one agent, one
  config section, one field change. The server renders the proposal as a diff
  card from its own stored copy, never from model text. Only an owner's
  **click on the card** moves it forward. Typing "yes" in chat does nothing.
- **Phase 3a needs no new write path.** The card's button opens the existing
  agent config dialog with the proposed value filled in, and the owner clicks
  the dialog's own **Save**. Direct **Apply** from the card (phase 3b) comes
  only after the endpoint fixes in phase 2.
- **Who confirms:** an owner, one change at a time. Requesting a proposal and
  applying it are both owner-only.
- **Which settings:** a small allowlist: `models.model`, `models.reasoning_effort`,
  this agent's cadence in one existing governor mode, and `general.enabled`
  set to `false` only (disable). Everything else is excluded by name.
- **Audit:** `chat_config_proposed` (with before and after values),
  `chat_config_rejected`, `chat_config_declined`, and `via=chat proposal=<id>`
  plus before/after on the existing `config_agent_<section>` line.
- **Off by default:** a new `chat.config_assist.enabled` (`*bool`,
  nil = **false**, the opposite of `grounded_answers`). Even with the flag on,
  the feature stays inert until a model backend is wired.

## What exists today

- `chat.grounded_answers` (`src/pkg/config/chat.go`, `ChatGroundedAnswersConfig`)
  is on by default. It answers free-text chat questions with one model turn
  over a capped snapshot of hive state built by `chatBuildSnapshot`
  (`src/pkg/dashboard/api_chat_grounded.go`).
- `chatGroundedAnswer` runs that turn through `deps.ChatGrounder.RunDeciderTurn`
  with `chatGroundedTimeout` and `chatGroundedMaxInflight = 2`. That cap
  limits concurrent turns, not how often they run.
- The agent config dialog writes through owner-only section endpoints in
  `src/pkg/dashboard/api_agent_config.go`: `handleAgentConfigGeneral`,
  `handleAgentConfigCadences`, `handleAgentConfigModels` and the rest. All of
  them call `requireOwnerRole` (`src/pkg/dashboard/api_agent_control.go`),
  which needs a verified owner role. Hub-proxied identities fail closed
  without the server-side verification marker.

What those endpoints do **not** guarantee today matters for this design. Each
gap is addressed below:

| Gap at `302fcc6e0` | Where | Addressed in |
|---|---|---|
| `models.model` is only sanitized. Nothing checks it against the list the dialog offers. | `handleAgentConfigModels` | [§3](#3-the-proposal-turn) (new check), phase 3a |
| The models check runs against *effective* backend and model, computed after `reconcileLaunchCmdForBackend` / `canonicalizeModelForBackend` and from live `ModelOverride` / `BackendOverride`. | `handleAgentConfigModels` | [§3](#3-the-proposal-turn), phase 2 |
| Cadences skip unknown modes (`continue`), and each mode is written into memory before later modes are validated, so a 400 can leave earlier modes changed but not saved. | `handleAgentConfigCadences` | phase 2 |
| A `saveConfig` error is logged, and the handler still returns 200 (models also raises a system alert). | general, cadences, models handlers | phase 2 |
| The handlers change `s.deps.Config.Agents` / `Governor.Modes` with no dashboard-level lock. Only `Config.Save` takes `saveMu` (`src/pkg/config/config.go`). | all section handlers | phase 2 |
| `config_agent_general` / `config_agent_cadences` record only `section=...`, with no before or after values. | same handlers | [§5](#5-audit-trail) |
| No dashboard handler writes `chat.*`, and `loadWithDashboardOverlay` does not adopt a `chat` block from the overlay. | `src/pkg/config/config.go` | phase 1 |

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
    proposal_ttl: 10m      # how long a proposal card stays usable
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
- **The Settings toggle is a new write path.** No handler writes `chat.*`
  today, and an overlay `chat` block is dropped on reload, the same failure
  as `ask` in [#19740](https://github.com/hivecommons/hive/issues/19740). So
  phase 1 either waits for the generic #19740 fix or adds `chat` to the blocks
  `loadWithDashboardOverlay` adopts. Either way it ships a regression test
  that toggles the flag, reloads, persists, and reads it back. The toggle
  handler calls `requireOwnerRole` and writes a `config_chat_assist` audit line
  (with before and after values), which is listed in `auditSensitiveActions`.
- **The `chat:` block itself is excluded from proposals.** Chat cannot turn
  config assist on or off, or change its own model.
- **NUX** (the "encourage it being configured" part of the issue): when the
  flag is off and an **owner** asks chat something config-shaped, chat answers
  as it does today and adds one dismissible hint pointing to
  Settings → Chat → Config assist. Non-owners never see the hint. Chat never
  asks for an API key; model credentials stay where they are configured today
  (backends / governor reviewer endpoint).

### 2. Which settings chat may change

The allowlist lives in code as a table. Each row names a section endpoint, the
fields of that endpoint's body that a proposal may set, the values allowed,
and the side effects that the card must show:

| Section | Endpoint | Field and allowed values | Side effect shown on the card |
|---|---|---|---|
| `models` | `PUT /api/config/agent/{name}/models` | `model`: exactly one of the models offered for the agent's **effective** backend, as listed in the proposal snapshot | the agent restarts |
| `models` | same | `reasoning_effort`: one of the efforts valid for the effective backend/model | the agent restarts |
| `cadences` | `PUT /api/config/agent/{name}/cadences` | this agent's cadence in **one** existing governor mode. Body: `{"<mode>": <seconds\|interval\|schedule>}` with exactly one key. Unknown modes are refused by the proposal check. | which mode changes, and whether it is the active mode |
| `general` | `PUT /api/config/agent/{name}/general` | `enabled: false` only (disable) | the agent stops |

Phase 1 leaves out `backend` (it rewrites `launchCmd` through
`reconcileLaunchCmdForBackend` and restarts the agent), `enabled: true`
(starting an agent starts spend) and `onDemand` (`onDemandTransition` starts
or stops the agent). Each can come back by decision under
[D1](#decisions-for-maintainers).

The **model list check is new**. The endpoint does not do it today. The
proposal check restricts `model` to the exact list the snapshot showed the
model. It does not rely on the endpoint to catch a bad value.

**Explicitly excluded**, each needing its own decision:

- agent sections `hooks`, `restrictions`, `tools`, `connections` (they carry
  credentials), `channels`, `prompt`, `pipeline`, `stats`;
- auto-merge (`config_auto_merge`), review and approval gates (`review`),
  the release sentinel (`src/pkg/config/release_sentinel.go`), and governor
  modes themselves (creating, deleting or switching modes, or their
  thresholds), plus merge-train settings;
- roles and RBAC, auth/OIDC/keys;
- the write-surface allowlist, hub settings, reference repos and variables;
- the whole `chat:` block;
- anything that adds or removes repos or agents.

The excluded settings are the ones that widen what an agent may do, remove a
gate, hold secrets, or change chat's own behaviour. The allowed ones change
*when* an agent runs, *on which model*, or stop it. Every allowed change can
be reversed with an equally small change, which [§5](#5-audit-trail) makes
concrete.

The issue's motivating example is setting up a new spoke, which touches repos,
connections and hub registration. That is deliberately *not* phase 1 (see
[D4](#decisions-for-maintainers)). Phase 1 can still *explain* those settings
through the existing read-only grounded path.

### 3. The proposal turn

1. A chat message from an owner, with `config_assist` on and a runner wired,
   is routed to a new `chatRouteConfig` kind when it reads as a config change
   ("switch reviewer to opus", "run the triage agent every 30 minutes").
   Messages from non-owners stay on the read-only grounded path, unchanged.
2. **Prompt-injection boundary.** The grounded snapshot carries untrusted
   issue and PR titles and agent "doing" lines. The config turn is built
   separately:
   - it does **not** call or reuse `chatBuildSnapshot`;
   - it gets **no** earlier chat history and no grounded answers;
   - its input is only the allowlist table, the current allowlisted values
     for each agent (including live `ModelOverride` / `BackendOverride`), the
     model and effort lists valid for each agent's effective backend, and the
     owner's current message;
   - no connection auth, tokens, prompts, hooks, issue or PR text is included.
3. One turn runs with the same timeout and in-flight cap as grounded answers.
   The model must reply with a JSON object
   `{"agent": ..., "section": ..., "set": {field: value}, "why": ...}` or with
   `{"none": "<reason>"}`. Free text is not acted on.
4. The server checks the reply and drops it, with a `chat_config_rejected`
   line, if any check fails:
   - the agent exists;
   - the section and every key in `set` are in the allowlist, and `set` has
     exactly one field (one mode for `cadences`);
   - the value is in the allowed set for that row (for `model` and
     `reasoning_effort`, the exact lists shown in the snapshot; for `enabled`,
     only `false`);
   - the merged body passes the endpoint's validation helper (phase 2). That
     helper returns the **normalised body plus the effective backend and
     model**, so the endpoint and the proposal check validate the same values.
     The allowlist and value-list checks are **stricter than the endpoint** and
     are new; they are not presented as reused.
5. A valid proposal is stored with a random id, the requesting user (the
   identity `auditFromRequest` records), the **before and after values**, a
   hash of the agent's current section values **including live overrides**,
   and an expiry (`proposal_ttl`).
6. The reply is a card rendered **from the stored proposal**: agent, field,
   before → after, the side-effect line from the allowlist table, and the
   model's `why`. `why` is shown as plain text, truncated, with no markdown,
   links or buttons. No part of the card's structure, values or buttons comes
   from model text.

Each proposal changes one field of one agent. When the request needs more,
the model names the first change and says what is left. The owner then asks
again.

**Rate limits and storage.** Proposals live in a bounded in-memory store:

- a per-user proposal rate (proposed: 6 per minute);
- at most 3 outstanding proposals per user and 20 in total. A new proposal
  past the cap is refused with a message, not evicting someone else's;
- expired proposals are swept;
- a restart drops every pending proposal. Pending cards then show "proposal
  expired (server restarted); ask again". A hub-proxied request that lands on
  a different instance gets the same message.

Pending proposals are visible only to the owner who requested them.

### 4. How a change is confirmed, and by whom

Confirmation is **a click on the card only**. Typing "yes", "apply" or
anything else in chat never applies or advances a proposal. Nothing in the
model's reply can submit, pre-click or auto-focus a button. Both requesting
and confirming are owner-only ([D3](#decisions-for-maintainers)).

**Phase 3a: "Open in agent config" (no new write path).** The card's only
action button opens the existing agent config dialog for that agent and
section, with the proposed value filled in and the changed field highlighted.
The owner reviews it in a UI the model cannot touch and clicks the dialog's
own **Save**, which calls the endpoint exactly as a hand edit does. The card
also offers **Dismiss**. Phase 3a needs no proposal header, no binding check
and no endpoint change. Its audit is the `chat_config_proposed` /
`chat_config_declined` lines plus the existing dialog save, and the dialog
passes `via=chat proposal=<id>` as a query parameter that only adds detail to
the audit line.

**Phase 3b: direct Apply (after phase 2).** The card gets an **Apply** button
that sends the stored body to the existing section endpoint with the owner's
session and an `X-Hive-Chat-Proposal: <id>` header.

- **Lock.** Phase 2 adds a dashboard-level `agentConfigMu` that every agent
  config section handler holds from its first read of current values through
  mutation and `saveConfig`, dialog saves included. The lock order is
  `agentConfigMu` then `saveMu`. With the header present, the binding check
  runs under the same lock, so two Applies, or an Apply racing a dialog save,
  cannot both pass the hash compare.
- **Binding check.** Under the lock the proposal must exist, be unexpired,
  belong to the same user, have a body equal to the stored one, and match the
  current section hash, live overrides included. Otherwise the endpoint
  returns `409` and the card asks for a fresh proposal.
- **Atomic, single-use consumption.** The proposal is marked *in flight* under
  the lock, so a double click or a second tab gets `409`. It is consumed only
  after `saveConfig` succeeds. A `400` or a persist failure returns it to
  pending until expiry, and the card shows the error.
- **"Applied" means saved.** The card shows "applied" only on a 2xx response
  from the phase-2 endpoint, which returns non-2xx when `saveConfig` fails. A
  failed apply writes `chat_config_apply_failed`. A failed restart after a
  successful save (the models handler's `restart_error`) is shown as "saved;
  restart failed".
- The header is **only** a binding check. A request without it is a normal
  dialog save. Chat has no server-side code path that calls the endpoint
  itself.

**Undo.** An applied card offers **Undo**. Undo builds a reverse proposal from
the stored *before* value (or from the before value in the audit line, after
a restart). That reverse proposal goes through the same checks, the same owner
click and the same endpoint. It fails with `409` if the section changed since.

### 5. Audit trail

| Action | When | Detail |
|---|---|---|
| `chat_config_proposed` | valid proposal stored | proposal id, agent, section, field, **before and after values**, question truncated to `chatGroundedMaxQuestion` |
| `chat_config_rejected` | model reply failed a check, or rate/cap refused | agent/section if parseable, reason (not the raw reply) |
| `chat_config_declined` | owner dismissed the card | proposal id |
| `config_agent_<section>` (existing) | the dialog save (3a) or Apply (3b) | existing detail plus `via=chat`, `proposal=<id>`, and before/after values for the changed field |
| `chat_config_apply_failed` | Apply got non-2xx (3b) | proposal id, status, reason |
| `chat_config_undo` | owner applied an Undo | proposal id of the undo, id of the proposal it reverses |
| `config_chat_assist` | Settings toggle saved | before and after `enabled` |

Phase 2 also adds before/after values to the plain `config_agent_general` and
`config_agent_cadences` lines, which today record only `section=...`, so a
change made outside chat can be undone from the log too.

Every new action name goes into `auditSensitiveActions`
(`src/pkg/dashboard/audit.go`), so the Audit Log risk chip counts them. All of
them go through `auditFromRequest`, so they are attributed to the real user,
never to `system`.

### 6. Why not a write-capable MCP server

The issue floats "a write-allowed MCP server that is exposed to this". This
RFC recommends against it for this feature:

- Chat context carries untrusted text: issue titles, PR titles, agent
  "doing" lines. Prompt injection in that text could make a model with a
  write tool change config without an owner in the loop. A proposal-only
  model with an isolated input ([§3](#3-the-proposal-turn) step 2) has
  nothing to call and does not see that text.
- An MCP surface would need its own role checks, validation and audit, which
  duplicates the owner-only endpoints that already exist and lets the two
  drift apart.
- The confirm-then-call-the-endpoint design costs one click per change.

If a write MCP surface is wanted for other reasons (agents configuring hive),
that is a separate RFC.

## Phases

Each phase is its own issue, behind `chat.config_assist.enabled` (default off)
where it adds chat behaviour. Phase 2 changes existing endpoint behaviour, and
each change there ships with its own test.

1. **Config, Settings toggle and NUX.** The `chat.config_assist` block,
   accessor, owner-only and audited Settings toggle, `chat` overlay adoption
   (or the #19740 fix) with reload and persist regression tests, and the
   owner-only hint. No proposals yet.
2. **Endpoint hardening (behaviour changes, each tested).**
   - Extract `validate<Section>Body` helpers that validate **all** input
     before any mutation and return the normalised body plus the effective
     backend/model, live overrides included.
   - Cadences: validate all modes, then mutate, so a 400 leaves memory
     unchanged. Unknown modes stay skipped for dialog compatibility, and the
     proposal check refuses them.
   - Return non-2xx when `saveConfig` fails in the general, cadences and
     models handlers.
   - Add `agentConfigMu` across read, mutate and save.
   - Add before/after values to the `config_agent_general` and
     `config_agent_cadences` audit lines.
3. **3a. Proposals with "Open in agent config".** Routing, the isolated
   snapshot, the turn, the proposal checks, the bounded store and rate limits,
   the card, the pre-filled dialog, and the audit actions. Depends on phase 1
   only.
   **3b. Direct Apply and Undo.** The `X-Hive-Chat-Proposal` binding check
   under `agentConfigMu`, atomic consumption, `chat_config_apply_failed`, and
   Undo. Depends on phase 2.
4. **Further fields, sections or spoke setup:** only after a separate decision
   per item, starting with whatever a maintainer picks under D1 or D4.

## Decisions for maintainers

- **D1. Allowlist for phase 1.** Proposed, following review: `model`,
  `reasoning_effort`, one-mode cadences, and disable-only `enabled`. Should
  `backend`, `enabled: true` or `onDemand` be added later, and in which phase?
- **D2. Model knob.** Should there be a separate `chat.config_assist.model`
  that falls back to `grounded_answers.model`, as proposed, or should it reuse
  `grounded_answers.model` with no new knob?
- **D3. Who may request and apply.** Proposed: owner-only for both in all
  phases. Letting a merger request proposals would add a cross-user hand-off
  that the binding check does not cover.
- **D4. Spoke setup.** The issue's motivating case. Should a later phase add
  a guided, multi-step "new spoke" flow where every step is still a separate
  owner-confirmed write? Or should spoke setup stay in the existing wizard
  with chat only explaining it?
- **D5. Other chat surfaces.** Proposed and agreed in review: dashboard only.
  Slack/Discord/email chat stay read-only because they have no owner-verified
  session to confirm with.
- **D6. 3b at all?** Is phase 3a ("Open in agent config") enough on its own,
  so that 3b is dropped or deferred indefinitely? Settled: 3b shipped in
  [#19985](https://github.com/hivecommons/hive/issues/19985).

## Not in scope

- Automatic or batched config changes, and any change without a per-change
  owner click on a server-rendered card.
- Secrets, credentials, connection auth, prompt templates, hooks, tool and
  write-surface permissions, gates and roles.
- Changes to the grounded-answers path or its default.
- The Ask AI help panel (#19177), which answers "where is X in the dashboard"
  and takes no action.
