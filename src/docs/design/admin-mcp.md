# The operator-facing admin MCP

**Status: design only.** Nothing here is implemented yet. It belongs to the v6
dashboard-optional line ([#7563](https://github.com/hivecommons/hive/issues/7563)) and, per
that line's policy, lands on the `v6` branch only. Tracked by
[#8697](https://github.com/hivecommons/hive/issues/8697).

Read [task-mcp.md](task-mcp.md) first. This page is written as its complement and does not
repeat its reasoning.

## What this is

A `cmd/` client binary that exposes a Hive's administration surface as Model Context Protocol
tools over stdio, so an operator can administer a Hive by talking to an assistant instead of
driving the dashboard. It reaches the Hive over the dashboard REST API with a dashboard token —
the same interface and the same credential `hivectl` uses.

It is the v6 theme applied to one more place humans already are. The line's theme says every
operator interaction the dashboard offers should be reachable from a GitHub thread, a chat
workspace, an inbox, a phone; an assistant conversation is now one of those places, and it is
the only one on that list where the *reader* of the hive's state can also correlate it — a
stalled lease against a draft plan against a failing check, in one pass.

## Why a second MCP surface

`POST /api/contribute/mcp` serves **agents**. It is task-scoped, read-only, authenticated by
an HMAC lease bearer bound to an assignment tuple, and it explicitly never carries the
dashboard token: "Hub-launched agents do not receive the dashboard auth token (#8348) … The
dashboard token itself is still accepted on the endpoint, but only for the dashboard's own UI
calls; it is never placed in an agent's environment, launch flags, or MCP configuration."

This serves an **operator** — the person who already holds the dashboard token and already has
owner authority through the SPA. The axes are the inverse of task-mcp's on everything that
matters:

| | `hive-task` | admin MCP |
|---|---|---|
| Consumer | agent | operator |
| Scope | one task | whole hive |
| Direction | read-only | read and write |
| Credential | HMAC lease bearer | dashboard token |

## Shape: both transports, endpoint required

The tools, the preview-and-confirm contract, the refusal machinery, the caps and the outbound
scrubbing are all transport-independent. They live in one package; each transport is a thin
shell over it, so a tool is defined once and the two cannot drift apart.

**The endpoint is the one that must exist.** It needs no install — a URL and a token, and any
MCP client can use it, including clients that cannot spawn a local process. That is the v6
theme: reachable from where people already are, rather than asking every operator to obtain and
run a binary first. It administers the hive that serves it, so there is no hive selection: the
endpoint *is* the scope.

**The stdio binary ships beside it.** stdio is MCP's primary transport and is what an agent CLI
spawns directly, and `cmd/hivectl` and `pkg/tui` are the precedent — standalone client binaries
of this same API, already in tree. Because it runs on the operator's machine rather than on a
hive, it is also the only transport that can hold a roster and address more than one.

An earlier draft of this page argued for the binary alone, on the grounds that a client adds
nothing for an attacker on the pod network to reach. That argument is real but it loses to
reachability, and the two shapes are not exclusive. The honest accounting:

- The endpoint **is** a new authenticated write surface on the hub, which `pkg/taskmcp` is not —
  that one is read-only. It exposes no authority the dashboard REST API does not already give
  the same credential, but it is new request-handling code on the hub and should be read as
  such. Every write on both transports is gated by the preview-and-confirm contract, and the
  surface is read-only until that contract exists.
- Because it is served by the hub, the endpoint needs no authentication of its own: the existing
  `authenticate` path already resolves a dashboard token to owner. Unlike `pkg/taskmcp` it needs
  no lease minting, no per-lease scoping and no revocation record, because the credential is the
  operator's own and the scope is the whole hive by design.

In tree, both transports *use* `pkg/hivectl`'s client, `pkg/logscrub` and `pkg/ioscan` rather
than reimplementing them. That is what makes the guard invariant literally true instead of
promised: an out-of-tree client would have to carry its own copy of the safety machinery, and
carrying one's own copy is what "no surface grows its own authz" forbids.

Its scope is hive administration **broadly** — not a fixed short list of operations — with a
named set of exclusions, each recorded below with the reason it was excluded rather than as a
bare statement of scope.

## What it inherits for free, and what it does not

### Owner role — inherited

`authenticate` (`src/pkg/dashboard/server.go`) accepts `Authorization: Bearer <dashboard
token>` on a non-direct-route spoke and, on a match, sets `X-Hive-Role: owner` together with
the server-only `ownerRoleVerifiedHeader` marker. Possession of the token **is** the owner
credential on those deployments — the comment there says so, and #4134 is why. So every
`requireOwnerRole` gate is satisfied by the credential alone, and a client has nothing to
implement and no decision to make about who may act.

Two corollaries the client must honour:

- It must never send `X-Hive-User`, `X-Hive-Role`, or `X-Hive-Owner-Role-Verified`.
  `authenticate` strips inbound copies of all three before auth precisely because they are
  forgeable, so sending them cannot grant anything and marks the client as suspect.
- The token goes in the `Authorization` header only. A `?token=` query parameter is rejected
  outright by `writeQueryTokenRejected`.

### Direct-route spokes — not supported, by construction

The bearer branch is guarded by `!directRouteAuthz`. A spoke with an `authorized_users`
allowlist deliberately refuses the shared token, because it "grants no per-user identity, so
accepting it would let any holder act as an unscoped owner and defeat the per-hive allowlist."

A token-bearing admin client therefore **cannot administer a direct-route spoke at all**.
That is correct behaviour on Hive's side and a hard limit on the client's applicability, not
a gap to work around. The client is expected to detect it at connect time and say so,
distinguishing it from a merely wrong token. `hivectl` has the same limitation for the same
reason.

### Mode ladder — not a check, but the thing most worth previewing

`pkg/agentmode` is ordinal — `ADVISORY < ISSUES_ONLY < ISSUES_AND_PRS < ISSUES_PRS_MERGE` —
and every capability predicate (`CanCreateIssues`, `CanCreatePRs`, `CanMerge`, `CanPush`) is a
`>=` comparison against it, with each rung minting a GitHub App token tier.

It governs what an **agent** may do on GitHub. It is not an authorization check on an
operator write, so an admin client has nothing to consult it for when deciding whether a call
is allowed — the owner gate has already answered that.

Where it matters is as the **subject** of a write. Changing a hive's ACMM level or one
agent's mode is in scope for the admin surface, through the endpoints that already exist:

- `PUT /api/packs/level` — set the hive's level, validated server-side to 1–6.
- `POST /api/packs/{level}/apply` — apply that level's pack. `handlePackApply` answers with
  `created`, `updated`, `skipped`, `paused`, `resumed`, `tombstoned` and `governor_changes`,
  which is a complete account of what the apply did.
- `PUT /api/config/agent/{name}/general` — set one agent's `mode`, validated against the
  ladder vocabulary; an unrecognised value is rejected rather than coerced.

All three are `requireOwnerRole`, so the dashboard token satisfies them like any other write.

This is the single most consequential thing the surface can do, because it is the one write
that changes what a *fleet* may do to real repositories — and unlike a pause, its effects do
not unwind when it is reversed: pull requests opened or merged at the higher tier stay opened
and merged. So the preview obligation is heavier here than anywhere else. A capability
preview names every agent whose tier changes, states for each what it gains or loses in terms
of authority rather than tier names (`ISSUES_PRS_MERGE` means nothing to an operator in the
moment; "quality may now merge on green CI" does), names the agents the change would create,
pause, or retire, and says explicitly when a change widens what any agent may do.

Two properties keep that preview honest. It should be derived from Hive's own account of the
change — the apply response above — rather than from a client-side copy of the level
definitions, which can drift from the hive's. And the `>=` structure of the ladder means a
level change can move several agents at once in different directions, so "which agents, and
which way" is a question the preview must answer per agent, not per hive.

### Input scanning — and the gap this work closes

The v6 guard invariant reads, in part: "`ioscan` enforcement on all inbound text." Today that
is not quite true, and the exception is on the path this surface most needs.

`pkg/ioscan` is applied at specific seams, not globally. `EnforceInput` is called from the
dashboard chat handler, `enforcePlanIssueBody` on the plan-from-issue path, the chat backends
(`pkg/chat`, `pkg/dashchat`, `pkg/discord`, `pkg/matrix`), and
`pkg/scheduler/ioscan_enforce.go` for untrusted GitHub text entering a kick.

`handleKick` is **not** one of them. A kick prompt supplied in the request body reaches the
agent unscanned; its only protections are the owner gate and a 10 000-character cap. The
handler's own comment states the exposure plainly: "the kick prompt is typed verbatim into the
agent's CLI session, and agents execute shell commands with App-scoped credentials."

**This is not specific to the admin MCP — the dashboard's own Kick button goes through the same
unscanned path.** So rather than declare a deviation from a non-negotiable invariant on its
first day, this work closes the gap: `handleKick` gains input scanning on the same terms every
other seam has it, and the protection becomes a property of the endpoint rather than of any one
client. `enforcePlanIssueBody` is the model to follow — it resolves the fail-open/fail-closed
decision against the hive's ACMM level and records an audit entry when it blocks, so there stays
one story about what scanning does when it trips.

It is a behaviour change, and worth saying so in the issue rather than in a footnote: text that
previously reached an agent untouched may now be redacted, or refused outright where the hive's
level calls for fail-closed. The blast radius is bounded — the default posture redacts rather
than blocks — but every existing caller of the endpoint is affected, the dashboard included.

The client keeps the verbatim confirmation regardless, as a second layer rather than a
substitute. The operator should read what is being sent to a process that can run shell
commands, whether or not a scanner also passed it, and a scanner that fails open by design is
not a reason to stop showing a human the text.

The inverse case is worth naming, because it is the only one already right: `POST /api/chat` —
the hive advisor — *does* scan. `handleDashboardChat` calls `EnforceInput`, refuses a blocked
message with a `chat.dashboard.refused` audit entry, and submits only the sanitized text.
Routing an operator's prose through the advisor inherits a gate rather than substituting for
one, which is why the advisor is in scope.

### Secret scrubbing — inherited unevenly, so applied again outbound

Redaction on the dashboard is **per-handler**, not response middleware:
`redactTokensInLine`, `redactTokens` (applied to run titles), `redactPromptSecrets`,
`redactLiteLLMKeyMaterial`, `redactSecret`. `pkg/logscrub` holds the canonical closed
category set — `hive-canary`, `github-token`, `jwt`, `aws-access-key`, `bearer-token`,
`private-key`, `encrypted-private-key`, `pgp-private-key` — with `TokenPattern` exported as
the single source of truth and reused by `pkg/ioscan`.

Two consequences for a client whose output is read by a language model:

1. **No response can be treated as pre-sanitised.** The client re-applies the same category
   set on the way out. This is why the tool surface is a fixed endpoint allowlist and not a
   generic request passthrough: an unvetted endpoint is an unvetted redaction story.
2. **A mask must be labelled as a mask.** `logscrub/redaction.go` exists because of
   hivecommons/hive#8067, where a reviewer agent read `Authorization: ******`, concluded the
   format string had no `%s`, and filed the conclusion as a defect. `FindRedactionMarker` is a
   recognizer built for exactly that failure. A model reading admin-MCP output will make the
   same mistake unless redactions are marked as redactions rather than passed through as text.

## Conventions carried over from task-mcp

Three of task-mcp's disciplines apply unchanged, and for the same reasons:

- **Structural injection handling.** Hive-originated text — issue and plan bodies, agent
  output, audit detail — is returned only inside fixed-schema result data. Tool descriptions
  and other free-text metadata never interpolate served content.
- **Typed refusals inside the result.** A refusal is data with a machine-readable reason, not
  a transport error, as `RefusalData` / `outside_lease_scope` / `stage_mismatch` already are.
- **Hard caps.** Item counts and text sizes are bounded and truncation is disclosed, in the
  spirit of `MaxPageSize = 20` and `MaxTextBytes = 16 KiB`.

To them the admin surface adds one the read-only sibling never needed: **no write happens on
a single call.** Each write is previewed — reporting exactly what would change, on which
hive, and changing nothing — and executes only against a single-use confirmation reference
issued by a preview of that same action on that same hive. The token is owner authority, and
a model can decide faster than an operator can read; the preview is the only place a human
is guaranteed to stand between the two.

## Endpoint mapping

The whole surface. Every entry is an endpoint that exists today; nothing here asks for a new
one.

### Read

| Tool area | Endpoint(s) | Notes |
|---|---|---|
| fleet | `GET /api/status`, `GET /api/contribute/fleet` | `/api/status` may be served from the cached pre-mutation snapshot, so it can lag a write this client just made. |
| agent detail | `GET /api/agents`, `GET /api/config/agent/{name}`, `GET /api/agents/{name}/kicks` | `GET /api/config/agent/{name}` reports the last kick as `prompt`. |
| leases and claims | `GET /api/runs`, `GET /api/runs/{key}`, `GET /api/claims` | Two distinct notions of "who holds this work", and both are wanted — see below. |
| plans | `GET /api/plans`, `GET /api/plan/{epicID}` | `ListPlans` returns drafts first; the detail view is the epic plus children with execution tags and dependency edges. |
| audit | `GET /api/audit`, `GET /api/runs/audit` | `handleAuditLog` requires `RoleAtLeast(role, RoleReadWrite)`, not owner. Both read-only by construction. |
| settings | `GET /api/config`, `GET /api/config/governor`, `GET /api/packs` | Every setting a write tool can change is readable, so a preview can state a before as well as an after. |
| autonomy readiness | `GET /api/acmm/evaluation`, `GET /api/acmm-recommendation` | The hive's own assessment, rather than a client-side judgment about level fitness. |
| spend | `GET /api/cost`, `GET /api/cost/history`, `GET /api/budget/history`, `GET /api/repo-cost` | |
| contributors | `GET /api/contributors`, `GET /api/contributors/{id}` | |
| repositories | `GET /api/repos/pauses` | |
| knowledge | `GET /api/knowledge`, `GET /api/knowledge/search`, `GET /api/knowledge/{layer}/{slug}` | Reads only. Writes are excluded — see below. |
| hive advisor | `POST /api/chat`, `GET /api/chat/messages` | The one path where Hive input-scans the operator's text *for* the client rather than the reverse: `handleDashboardChat` calls `ioscan.EnforceInput` and refuses a blocked message with `chat.dashboard.refused`. |
| breaker, backup | `GET /api/breaker`, `GET /api/backup/status` | |

**Leases and claims are not the same thing, and an operator wants both.** A `taskLease`
(`contribute_leases.go`) is the hub's server-authoritative record binding an assignment to a
`{profile, task, repo, generation}` tuple with an expiry — it is what makes a reconnecting
relay's resume legal, and it is keyed per task since #7774. A `claims.Claim`
(`claims_api.go`, hive#8380) is the worker-claim ledger entry recording who is *working* an
issue, ranked human > agent > contributor > external, and it is what makes a hold visible to
humans, other agents, and other hives. `GET /api/runs` surfaces the lease view (with
`LeaseKey`, stage, generation, expiry, and the claim fields alongside); `GET /api/claims`
surfaces the ledger. Answering "why is nobody working this?" needs both.

`GET /api/runs/{key}` resolves its path value against `run.Key` **or** `run.LeaseKey`, and
falls back to the queued-run snapshot, so an operator can paste either identifier.

### Write

| Tool area | Endpoint(s) | Hive-side gate |
|---|---|---|
| nudge | `POST /api/kick/{agent}` → `GET /api/kick/{agent}/status` | `requireOwnerRole` |
| pause / resume / restart | `POST /api/pause/{agent}`, `/api/resume/{agent}`, `/api/restart/{agent}`, `/api/reset-restarts/{agent}` | `requireOwnerRole` |
| add / remove agent | `POST /api/agents`, `DELETE /api/agents/{name}` | `requireOwnerRole` |
| model / backend / effort | `POST /api/model/{agent}/{model}`, `/api/switch/{agent}/{backend}`, `/api/effort/{agent}/{effort}` | `requireOwnerRole` |
| agent interaction tier | `PUT /api/config/agent/{name}/general` | `requireOwnerRole`; `mode` validated, unrecognised values rejected not coerced |
| autonomy level | `PUT /api/packs/level`, `POST /api/packs/{level}/apply` | `requireOwnerRole`; level validated 1–6 |
| plan propose | `POST /api/plan/from-issue` | **deliberately not owner** — ioscan + ACMM L5 floor |
| plan approve / reject | `POST /api/plan/{epicID}/approve`, `.../reject` | `requireOwnerRole` |
| feature settings | `PUT /api/config/governor/features` | `requireOwnerRole`; pointer fields, nil means unchanged |
| repository pause / resume | `POST /api/repos/pause`, `/api/repos/resume` | `requireRepoPausePermission` → `canToggleRepoHold` |
| item hold | `POST /api/repos/{owner}/{repo}/items/{number}/hold` | `canToggleRepoHold` |
| spend | `PUT /api/config/governor/budget`, `POST /api/config/governor/budget/reset` | `requireOwnerRole` |
| contributors | `PUT /api/contributors/{id}/trust`, `POST /api/contributors/{id}/revoke`, `/requeue`, `PUT /api/contributors/{id}/agent-role-grants`, `DELETE /api/contributors/{id}` | `requireOwnerRole` |
| backup | `POST /api/backup` | `requireOwnerRole` |
| breaker | `POST /api/breaker/engage`, `/api/breaker/release` | `requireOwnerRole` |

**The gate is not uniform, and a client must not assume it is.** Two departures matter:

- **Plan proposal is deliberately ungated**, with a comment naming its audit item and its
  regression test (`TestF16PlanFromIssueStaysUngated`): it mints a *draft* epic whose children
  `decompose.go` withholds from `Ready()`, so proposing a plan releases no work, and gating it
  "would make planning owner-only end to end and break the contributor workflow." It has two
  other gates instead — `enforcePlanIssueBody` (ioscan; **422** on critical injection) and
  `planning.PlanningAllowedAtLevel` (**409** below L5, because the architect agent has no
  cadence until then). A client that treats every write as owner-gated will misreport both.
- **Repository pause and item holds** use `canToggleRepoHold`, which admits a verified owner
  *or* a GitHub user with live-checked write permission on that repository (cached, with a
  TTL). A token-bearing client passes on the owner branch, but the refusal text it may receive
  is not the owner gate's.

Five behaviours are worth stating at length, because a client that gets them wrong is worse
than no client.

**The nudge is asynchronous and answering 202 is the contract.** `SendKickAsync` keeps every
fast, deterministic precondition synchronous — unknown agent, paused, missing tmux session,
sandbox rejection still answer 400 — and moves the prompt wait and the typing to a background
goroutine behind an exactly-once in-flight guard. It is this way because of #5325: the
synchronous `SendKick` waited up to `inputPromptTimeout` (120 s) for the CLI's input prompt,
which exceeds a typical 60 s ingress idle timeout, so the proxy answered 504 while the wait
was still running — the prompt *was* typed, the session *did* run, and the operator had been
told it failed, so the natural retry delivered the work twice. A tool must report acceptance
as acceptance, poll `GET /api/kick/{agent}/status` for the outcome, and surface the
`in-flight` answer as "already being delivered", never as a new delivery.

**Pause and resume must be rendered from the response's `state`, never from intent.**
`pauseToggleResponse` carries `changed` specifically to distinguish a real transition from a
no-op, and the reason is in the code: "pausing an already-paused agent used to return the same
undifferentiated success as a real pause, which let a dashboard with a stale belief silently
re-pause an agent the operator was trying to START (audit showed pause-pairs seconds apart
while the agent stayed paused indefinitely)." A conversational client is exactly that
stale-belief client — a model's summary of what it *intended* is the failure mode this field
was added to defeat.

**Plan approve and reject sit at the same trust level on purpose.** Both are owner-gated, per
Audit F16 (2026-08-13): "if approve is owner-only and reject is not, a read-write member can
undo an owner's approval at will — the gate is bypassed from the other side, and the two ends
of one state machine must sit at the same trust level." Approve releases the epic's children
through `Ready()` and calls `advanceApprovedPlanLease`, which can fail with **409 Conflict**;
reject re-gates the children and resets the run lease. Neither refusal should be retried.

**A capability change is the one write whose effects do not unwind.** See the mode-ladder
section above for the preview obligation it carries.

**The feature-settings PUT can switch off the input scanning described above.** Its body is a
struct of pointer fields, so a partial update is native and omitted fields are untouched — but
`IoscanEnabled` is one of them, alongside `ClaimsEnabled`, the checkpoint gates, and the
autonomy auto-promote/demote controls. A surface that can disable a protection is reachable by
the same conversation the protection exists to bound. The mitigation is disclosure: a preview
of a change that switches off a protection says so explicitly.

## Deliberately absent, and why

Recording the reason matters as much as recording the exclusion. A bare "out of scope" reads,
a year later, as either an oversight or a prohibition, and a reader cannot tell which. Three of
these are mechanical — the operation cannot work through this credential or this transport —
and the rest are judgments that a future reader is entitled to revisit.

**These reasons are a runtime artifact, not just documentation.** An operator who asks the
surface for one of these operations is told why it is unavailable, in these terms, at the moment
they ask, and is told which kind of refusal it is: impossible, judged against, or merely switched
off. Two consequences follow. The reasons here and the reasons the binary gives must stay in
step, which is worth a test rather than a convention — the same discipline `logscrub` already
applies to its category set. And for a judgment, the refusal points the operator at this
project's issue tracker, because disagreeing with a recorded reason is a legitimate response to
reading one; for a mechanical exclusion it says what would have to change instead, since an
issue asking for it as stated could not be granted.

There is a third reason the refusal has to carry its argument, beyond courtesy to the operator.
Several of these exclusions can be approximated by combining operations that *are* available —
an assistant told only "no" may clear a hold or approve a plan to reach the effect it was denied.
A refusal that explains the line is what discourages routing around it, and the surface says
explicitly not to.

**Cannot work**

- **`POST /api/self-upgrade`.** The upgrade restarts the process serving the API the request
  arrived on, so the call severs its own connection and cannot report its outcome. The
  operator would be unable to distinguish a successful upgrade from a failed one. (It also
  needs the spoke's own dashboard token to prove itself to the hub, which is a separate
  matter.)
- **`POST /api/contribute/invite`.** `handleContributeInvite` resolves the caller's identity
  server-side via `resolveViewerUsername` and requires *that user's* contributor profile to
  hold a `trusted`, `merger` or `advisor` tier — `inviteTrustTiers`. Authority here is derived
  from who the caller is, not from the owner role, and there is no owner override. A
  credential carrying no personal identity gets 401. Every other contributor operation is an
  ordinary `requireOwnerRole` write and is in scope.
- **Fan-out across hives.** Exactly one hive is active at a time by design, so an all-hives
  mode would contradict the surface rather than extend it.

**Blast radius**

- **`POST /api/prs/{owner}/{repo}/{number}/queue-automerge`.** The only available action that
  directly causes a merge into real code, and the one a preview cannot meaningfully soften: a
  capability change previews as "these agents gain this authority", whereas this is "this pull
  request, merged, now". Worth revisiting if a preview can be made to show the diff and the
  checks being merged.
- **`PUT /api/config/agent/{name}/prompt`.** The kick hazard made permanent. Kick text reaches
  the CLI once and is confirmed verbatim for that reason; a prompt reaches it on *every*
  future kick, and Hive does not scan that path either. Verbatim confirmation is a reasonable
  guard for one string and a weak one for a standing instruction. Model, backend, effort and
  interaction tier are all in scope; only the prompt text is not.
- **`POST /api/release-channel`.** Changes which code the hive runs at its next upgrade. The
  effect is deferred and invisible at the moment of the change, so a preview can say nothing
  useful about it.
- **Knowledge writes** (`POST /api/knowledge/create`, `PUT /api/knowledge/{layer}/{slug}`,
  the document, vault, git-source and subscription families). Agents read knowledge when they
  are handed work, so a write changes fleet behaviour with nothing in any fleet or agent view
  showing that it happened — the only such write adjacent to this surface whose effect is both
  durable and unobservable through the rest of it. Knowledge **reads** are in scope.

**By construction**

- **Anything that issues, rotates, reads back or exchanges a credential**:
  `GET /api/auth/token`, the `claude-auth` / `copilot-auth` / `gh-user-auth` families,
  `POST /api/contribute/reissue-token`, `POST /api/agents/{name}/login-code`. The surface
  exists to hand Hive state to a language model, and credential material is the one class of
  content that must never reach one. There is no version of this design in which these belong.

**Shape**

- **Purely presentational endpoints** — themes, branding, sidebar layout, leaderboard styling,
  banner dismissal. They have no meaning in a conversation.
- **Any tool taking a path, URL, or method as an argument.** The allowlist is the design: Hive
  redacts per handler, so an unvetted endpoint is an unvetted redaction story.

## Known gap: attribution

`requestUser` resolves the acting user from `X-Hive-User`, falling back to `"local"`. Because
this client is forbidden from asserting an identity, every write it makes is audited as
`local` — the same as any other token-authenticated caller, including `hivectl` and the SPA on
a self-hosted spoke.

So `GET /api/audit` will show *that* an agent was paused and *when*, but not that an assistant
mediated it. An operator reconstructing a timeline cannot separate conversational actions from
dashboard ones.

**This was decided, not overlooked.** #8697 settled it: accepted as a recorded limitation, and
the write contract will not carry a client-identity record. Three reasons, in the order they
matter:

- The token holder is already the accountable party on this path — possession of the dashboard
  token *is* the identity, per `authenticate`. A client-supplied name adds nothing to
  accountability, because anyone holding the token can put whatever they like in it.
- What is actually wanted is **diagnosis**, not attribution: "was this the assistant or a
  person?". That does not need to be unforgeable, and conflating the two is how a trivially
  spoofable field ends up being treated as proof.
- It is not specific to this surface. `hivectl`, `curl` and the SPA on a self-hosted hive all
  audit as `local` too. If recording the calling client is worth doing, it is worth doing for
  all of them — which makes it its own change, recording the caller as a *claim* rather than an
  identity, not a rider on this one.

The alternative that is genuinely closed off is sending an identity header: `authenticate`
strips inbound copies before auth, and on any deployment where it did not, doing so would be
forgery.

## Relationship to `hivectl`

`src/pkg/hivectl/client.go` is already a Go client for this API: it validates the base URL
(http/https only), joins paths against a trimmed base, and attaches
`Authorization: Bearer <token>`. The **stdio** transport uses it rather than carrying a second
copy — there is no reason for two clients in one repository to disagree about how a Hive is
addressed, and a change to that (a new auth path, a base-URL quirk on a GHE deployment) should
land once. The endpoint does not need it: served by the hub, it reaches the same state
in-process.

The difference is upstream of the transport. `hivectl` is driven by an operator typing a
command; the admin MCP is driven by a model proposing one. That single difference is the
origin of every extra control in this design — the preview-and-confirm step, the verbatim
prompt confirmation, the outbound scrub, the mask labelling, and the read-only default.
