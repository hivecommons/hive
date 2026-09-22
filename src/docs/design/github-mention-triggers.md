# GitHub @-mention triggers: letting a human summon an agent from a thread

Status: **Shipped on `v6`** — all three phases, tracked on
[#7483](https://github.com/hivecommons/hive/issues/7483). Phase 1 (poller,
grammar, guards, `source=mention` kick, 👀 ack, audits) landed in
[#7582](https://github.com/hivecommons/hive/pull/7582), phase 2 (in-thread
replies through `Converse` when the summoned run completes) in
[#7597](https://github.com/hivecommons/hive/pull/7597), and phase 3 (the
webhook accelerator) in
[#7623](https://github.com/hivecommons/hive/pull/7623) — `src/pkg/mention` on
the `v6` branch. It changes no v4 or v5 trigger behaviour: that package does
not exist on either line, so a v4 or v5 hive still has no inbound GitHub
trigger.

RFC credit: this page turned the proposal in #7483 into a reviewable plan before
implementing it. Its core observation — that Hive already built the inbound
mention model for Linear and never built the GitHub equivalent — is the
organising idea below: mirror what exists, reuse every guard that already has a
runtime, and add nothing that has no runtime.

Every code reference below was checked against `v4` at `cf57dd3d3` while
writing this page. Line numbers may drift; the named symbols are the stable
handles.

---

## Problem

Every trigger in Hive today is hive-initiated. A human who wants an agent to
look at an issue or a PR has no inbound path: they wait for a cadence kick to
happen to select their item, or they ask an operator to drive the dashboard.
On Linear this is already solved — a human mentions the agent app and it wakes
with the mention as its prompt. On GitHub, where the overwhelming majority of
the fleet's work happens, there is nothing to *hear* a mention with, even
though the capability to *reply* to one already exists.

| Path | Direction | Trigger |
|---|---|---|
| Governor cadence | outbound | timer + mode thresholds (`pkg/governor`) |
| Review swarm dispatch | outbound | queue scan (`pkg/review`) |
| `issue_request_watcher.go` | outbound | an agent writes a request file |
| `pr_request_watcher.go` | outbound | an agent writes a request file |
| `review_request_watcher.go` | outbound | an agent writes a request file |
| `merge_request_watcher.go` | outbound | an agent writes a request file |
| Linear webhook | **inbound** | **a human mention or delegation** |

The last row is the only inbound trigger in the system, and it is not GitHub.

## What already exists, and what does not

**The inbound model.** `pkg/linearagent` is a complete inbound-mention path:

- the OAuth install requests `app:assignable,app:mentionable`
  (`src/pkg/linearagent/oauth.go:56`), so the app is a mentionable actor;
- `WebhookReceiver.ServeHTTP` verifies the HMAC over the raw body *before*
  parsing, applies the replay guard only to the now-authenticated body, does no
  synchronous downstream work, and hands the event off in a goroutine so the
  5-second response budget is never spent on a kick
  (`src/pkg/linearagent/webhook.go:164`);
- `SessionEvent.PromptText` picks the task text by precedence — the user's
  message on `prompted`, else the prompt context, else the mention comment,
  else the issue title (`src/pkg/linearagent/webhook.go:105`);
- `Responder.HandleSessionEvent` acks first, resolves the target agent from
  pure config, builds a kick message with a structured header and the prompt
  text, bounds it at `responderKickLimit` (10,000 runes), and delivers it
  through `agent.Manager.SendKick` (`src/pkg/linearagent/responder.go:130`,
  `responder.go:224`);
- agent resolution is `SessionAgent`: the named agent, else the only
  configured agent, else an error activity naming the missing config
  (`src/pkg/config/config.go:1688`).

**The reply half.** `AgentCapabilities.Converse` is documented in exactly the
words this feature needs — "An ADVISORY agent with Converse can reply on a
thread it was mentioned in; it still cannot file, edit or relabel anything"
(`src/pkg/agent/capabilities.go:32`). It is enforced at the MITM proxy as an
additional grant beside the mode ladder (`proxy.AllowedByModeCaps`,
`src/pkg/proxy/rules.go:152,170`), and the issue-request watcher already carries a
`Kind: "comment"` request that posts a comment as the App bot
(`src/pkg/github/issue_request_watcher.go:70`). Outbound comment text is
canary-scanned and secret-scrubbed on the way out
(`Client.CreateIssueComment`, `src/pkg/github/client.go:1236`).

**A GitHub webhook receiver.** The hub verifies `X-Hub-Signature-256` fail-closed
and dispatches on `X-GitHub-Event` (`src/pkg/hub/webhook.go:53`), today for
`installation` events only. The spoke has no GitHub webhook endpoint.

**A trap to name.** `ChannelConfig` once accepted `webhook`, `discord`,
`schedule` and `bead` trigger types that were declarative only: the
`pkg/channels` runtime meant to serve them was never wired into the binary,
declaring one validated cleanly while suppressing governor kicks, and the agent
sat permanently dormant with no diagnostics. The types were removed and
`ValidateChannels` now rejects them (`src/pkg/config/config.go:5538`, #5591).
A mention trigger is a new channel type, and it must land with its runtime in
the same PR — never as a config key first.

## Proposed design

### Transport: poll first, webhook as an accelerator

The Linear path is webhook-only because Linear pushes and demands a 5-second
answer. GitHub is different in a way that matters for this fleet: most spokes
run on pull-only clusters the hub cannot write to (see
[wrapped master delivery](master-delivery-wrapped.md)), and such a spoke has no
public URL for GitHub to deliver to either. A webhook-only design would work
on the hosted hub and nowhere else.

So the primary transport is **polling from the App installation**, which every
spoke already does for everything else:

1. On each governor eval tick, for each managed repo, list issue comments and
   review comments created since the repo's watermark
   (`Issues.ListComments` / `PullRequests.ListComments` with `since`), plus
   issues opened since it. One or two calls per repo per tick; the watermark is
   the newest `updated_at` seen, persisted with the other `/data` ledgers.
2. Filter to bodies that mention the App login and pass the guards below.
3. Hand each surviving mention to the same responder shape Linear uses.

Detection latency is one eval tick (minutes), which is the same latency a
human already experiences for a cadence kick and is fine for "have a look at
this". A **webhook receiver on the spoke** — the hub's verifier with
`issue_comment`, `pull_request_review_comment` and `issues` added to its event
switch, mounted PUBLIC beside `/api/linear/webhook`
(`src/pkg/dashboard/api_linear_agent.go:40`) — is phase 3, for spokes that have
a reachable dashboard, and it feeds the *same* handler: a webhook delivery
simply advances the poller's watermark early. Both transports dedupe on the
comment (or issue) node id, so a mention seen twice is handled once.

### The mention grammar

A mention is a comment or issue body containing `@<app-login>` where
`<app-login>` is the installation's bot login, the value the hive already knows
as `appBotLogin` (`isHiveAppReviewAuthor`, `src/pkg/github/automerge/automerge_sweep.go:1207`).
The text after the mention is the request. Two optional forms route it:

```
@hive have a look at the flaky test in this PR
@hive ask scanner is this a duplicate of #1234?
@hive review this
```

- `ask <agent>` names a configured agent explicitly.
- A bare verb (`review`, `triage`, …) is not a routing keyword; it is prompt
  text. Routing is by agent, not by intent, because intent classification is
  what the *agent* is for.
- With no selector, resolution follows the Linear rule verbatim: the agent
  named in config (`github.mentions.default_agent`), else the only configured
  agent that is enabled, governor-kickable and holds `Converse`, else the
  mention is declined with a reply naming the configured agents (when the
  hive may reply) and an audit entry either way.

### The kick

The mention becomes a kick built the way `buildKickMessage` builds Linear's
(`src/pkg/linearagent/responder.go:225`): a structured header the agent can
act on, then the human's text, bounded at the same 10,000-rune limit.

```
You were mentioned by @alice on hivecommons/hive#7483 (comment 2345678901)
— https://github.com/hivecommons/hive/issues/7483#issuecomment-2345678901

Reply on that thread when you are done; a reply is the only artifact this
kick asks for.

---
is this a duplicate of #1234?
```

The header carries repo, item number, item kind (issue / PR), comment id and
URL as structured context — the same shape the review swarm already hands its
reviewers (`pkg/review`). The body is the human's text with the leading
mention and any `ask <agent>` selector stripped, after the ioscan pass below.
Delivery is `agent.Manager.SendKick` (`src/pkg/agent/manager_kick.go:110`),
recorded in the kick history with `source=mention` so the dashboard's kick
outcome classifier (#7421) and the agent card can say where the turn came
from.

### The reply

The agent replies the way it already can: by writing an issue-request file of
`Kind: "comment"` (or a review-request with `Event: "comment"` on a PR), which
the watchers post as the App bot — audited under `agent_comment_created`
(`src/pkg/github/attribution.go:53`), canary-gated and secret-scrubbed
(`src/pkg/github/client.go:1236`), gated by `Converse` at the proxy. **Nothing
new is added to the write path.** An ADVISORY agent with `Converse` can answer
a mention; without `Converse` its kick still runs, but the only thing it can
do with the answer is leave it in its own output — which is the correct,
already-documented boundary.

The one new outbound write is the **acknowledgement**: an 👀 reaction on the
mentioning comment (`Reactions.CreateIssueCommentReaction`; the hive already
uses reactions for fleet-report dedupe, `src/pkg/github/fleet_report.go:42`).
It is the GitHub analogue of Linear's `thought` ack — cheap, non-textual, and
tells the human the mention was heard without a comment that would itself be a
target for the loop guard. It is posted only after every guard has passed, so
a declined mention gets no reaction (see *No oracle* below).

## Guards

Mentions are attacker-reachable in a way cadence kicks are not: anyone who can
comment on a public repo can type the App's name. Each guard below maps to a
mechanism that already exists; none is new policy.

1. **Who may summon — fail closed.** A mention is honoured only from a login
   the hive already trusts, using the dashboard's own role list
   (`DashboardConfig.AuthorizedRole`, `src/pkg/config/config.go:4218`) at
   `read-write` or above by default — the same lookup `trustedMergerFunc`
   uses for the merge queue (`src/cmd/hive/merge_eligibility.go:50`). An explicit
   `github.mentions.summoners` list widens it. No configuration means the
   feature is **off**, never "any commenter"; that is the rule the Discord
   design already set for its channel list.
2. **No oracle.** A mention from a login that fails guard 1 is dropped
   silently — no reaction, no reply — and audited. Replying "you are not
   allowed" would let anyone enumerate the allowlist by typing the App's
   name.
3. **Rate limits, three keys.** Per user, per repo, and per thread. The
   per-thread cap reuses `classification.review_bots.max_attempts_per_thread`
   semantics exactly: the counter *is* the thread's list of App-authored
   replies, no state file (`src/pkg/github/review_threads.go:131`,
   `review_request_watcher.go:374`). Per user and per repo are sliding
   windows in memory, sized in config with conservative defaults (a user gets
   a handful of summons an hour; a repo a few dozen), and a global per-tick
   budget bounds the worst case at one API call per accepted mention.
4. **Loop prevention.** A mention is ignored when its author is the App login
   itself, any `classification.review_bots.logins` entry
   (`src/pkg/config/review_bots.go:32`), or any login ending in `[bot]`.
   Without this two hives, or a hive and a review bot, would mention each
   other forever.
5. **Untrusted input.** The mention body is text a stranger wrote that becomes
   a prompt. It goes through `ioscan.EnforceInput` before it reaches the kick
   (`src/pkg/ioscan/enforce.go:24`), the way the scheduler already treats
   issue titles and labels (`Scheduler.enforceIssueText`,
   `src/pkg/scheduler/ioscan_enforce.go:122`,
   [ADR-0008](../adr/0008-ioscan-untrusted-input.md)): benign text passes
   byte-for-byte, blocked content is replaced with the visible
   `[ioscan: content withheld — …]` marker and audited. Outbound replies are
   canary-gated by the existing comment path (kubestellar/hive#4960).
6. **Never escalate.** A mention is an *input*. It produces a `SendKick` and
   nothing else: it cannot change the agent's mode, cannot apply a label,
   cannot queue a merge, cannot bypass a hold. Authority stays on the ladder
   plus capabilities, checked at the proxy as it is for every other turn. The
   issue's out-of-scope list — auto-merge on mention, mode escalation by
   mention — is not a phase-later item; it is a boundary.
7. **Edits and replays.** Only the `created` action of a comment or issue is a
   trigger; `edited` is not, so a mention cannot be re-armed by editing an old
   comment. Bodies are bounded (the hub's 256 KiB webhook limit is the model)
   and deduped on node id across restarts.

## Configuration shape

```yaml
github:
  mentions:
    enabled: true            # absent/false → the poller does not run
    default_agent: scanner   # like linear.session_agent; optional when exactly one agent qualifies
    summoners: []            # extra logins; dashboard role read-write+ is always accepted
    min_role: read-write     # the dashboard role floor for summoners
    per_user_per_hour: 6
    per_repo_per_hour: 30
    ack_reaction: eyes       # "" disables the acknowledgement reaction
    webhook_enabled: false   # optional latency accelerator; polling remains authoritative
    webhook_secret_env: GITHUB_MENTION_WEBHOOK_SECRET
    webhook_min_gap: 30s     # per-repo coalescing floor for webhook-triggered polls
```

`classification.review_bots.logins` and `.max_attempts_per_thread` are read
as they are today; the mention path adds no second copy of either.

An agent opts in with a channel, and — because of #5591 — the channel type
ships with its runtime in the same change:

```yaml
agents:
  scanner:
    capabilities: { converse: true }
    channels:
      - type: kick
      - type: mention          # new in v6; ValidateChannels accepts it only once the poller exists
```

## Audit and observability

- `agent_mention_kicked` — one entry per accepted mention: repo, number, comment
  id, author, resolved agent. Rides the same `recordCreationAudit` convention
  as the watchers (`src/pkg/github/attribution.go`).
- `agent_mention_declined` — one entry per dropped mention with the guard that
  dropped it (`unauthorized`, `rate-limited`, `loop`, `no-agent`, `ioscan`),
  never the body.
- Kick history rows carry `source=mention`, so the dashboard's agent card and
  the kick outcome classifier (#7421) can say the turn was summoned, and the
  per-agent "last kick" no longer implies a cadence fired.
- A dashboard counter of mentions accepted / declined per repo, beside the
  review-thread counters, so an operator can see the feature is alive — the
  exact diagnostic the removed channel types lacked.

## Phases

1. **Poller, grammar, guards, kick — no reply.** The agent is summoned and
   the outcome is in its kick log and the audit trail. Advisory-safe by
   construction: nothing is written to GitHub except the 👀 reaction.
2. **Reply through `Converse`.** No new code on the write path; this phase is
   the kick text telling the agent to answer on the thread, the `Converse`
   capability documented for the mention shape, and a dashboard note that a
   mention-kicked agent without `Converse` cannot answer.
3. **Webhook accelerator.** The hub's verifier gains the three event types and
   a spoke-side mount; deliveries advance the poller's watermark. Hub-to-spoke
   relay for pull-only clusters is a later question that the master-delivery
   design already frames.

## Non-goals

From the issue, restated as boundaries this design must not cross:

- Auto-merge on mention.
- Mode escalation by mention.
- Any change to v4/v5 trigger behaviour. This page lives on `v4` because that
  is where design records live; the work targets v6.

## Open questions for maintainers

**All three were decided before phase 1 landed, each the way this page
proposed.** The questions are kept below as the record of what was weighed;
the decision follows each one, with the code on `v6` that now carries it.

1. **Role floor.** Is `read-write` the right default floor for summoning, or
   should the default be `merger` with `read-write` as an opt-down? The
   argument for `read-write`: a summon produces at most a comment. The argument
   for `merger`: a summon spends tokens on someone else's request.
   **Decided: `read-write`, operator-overridable** — `DefaultMentionMinRole =
   RoleReadWrite` with `GitHubMentionsConfig.MinRoleEffective()`,
   `src/pkg/config/config.go` on `v6`.
2. **PR review comments.** `pull_request_review_comment` mentions land inside a
   review thread; should the reply go in-thread (the #7360 reply path, capped
   by `max_attempts_per_thread`) or as a PR-level comment? In-thread is more
   useful and already guarded; it is proposed here.
   **Decided: in-thread** — the phase 2 responder replies on the summoning
   thread under a per-thread lock (`Responder.lockThread`,
   `src/pkg/mention/responder.go` on `v6`).
3. **Multi-agent selectors.** `ask <agent>` names one agent. Is fan-out
   (`ask scanner,quality`) wanted, or is one summon one agent? One is proposed:
   fan-out multiplies token spend under a stranger's control.
   **Decided: one summon, one agent** — the parsed mention carries a single
   `Agent string` (`src/pkg/mention/grammar.go` on `v6`); there is no fan-out
   syntax.

## References

- `src/pkg/linearagent/webhook.go` — the inbound mention model to mirror.
- `src/pkg/linearagent/responder.go` — ack-first, resolve, kick, track.
- `src/pkg/linearagent/oauth.go:56` — `app:mentionable`.
- `src/pkg/agent/capabilities.go:32` — `Converse`, documented for mentions.
- `src/pkg/proxy/rules.go:152,170` — where `Converse` is enforced.
- `src/pkg/config/config.go:1716` — `linear.session_agent` resolution rule.
- `src/pkg/config/config.go:5538` — the removed declarative channel types (#5591).
- `src/pkg/config/review_bots.go:32` — `classification.review_bots`, the loop
  list and the per-thread cap.
- `src/pkg/github/review_request_watcher.go`, `review_threads.go` — the
  App-authored in-thread reply path and its attempt counter.
- `src/pkg/github/issue_request_watcher.go:70` — `Kind: "comment"`.
- `src/pkg/github/client.go:1236` — canary-gated, scrubbed comment posting.
- `src/pkg/hub/webhook.go:53` — the fail-closed GitHub webhook verifier.
- `src/pkg/ioscan/enforce.go:24`, `src/pkg/scheduler/ioscan_enforce.go:122`,
  [ADR-0008](../adr/0008-ioscan-untrusted-input.md) — untrusted kick input.
- `src/cmd/hive/merge_eligibility.go:50` — `trustedMergerFunc`, the role-list lookup to
  reuse for summoners.

## GitHub Actions trigger transport

Issue #8206 adds a second producer for the same mention path: GitHub Actions can post an `@hive` comment that the poller already understands. This is transport A, the comment-relay transport. It deliberately adds no inbound spoke port and no new authorization path.

The repository-local composite action is `hivecommons/hive/.github/actions/hive@v6`. It accepts `command`, optional `issue`, optional `prompt`, and `token`, then posts:

```text
@hive <command> <prompt>

<!-- hive:source=action run_id=<run_id> run_attempt=<run_attempt> workflow=<workflow> actor=<actor> -->
```

The marker is machine-readable only. The mention handler strips it before ioscan, before building the agent kick, and before any human-visible echo. When the marker is present, the kick source is recorded with the `action` source label instead of `mention`, and the poller dedupes on `run_id` plus `run_attempt` in the same persistent store that tracks seen comment ids.

Action-originated comments keep the existing mention guards and add transport-specific deltas:

- the marker actor maps through `github.actions.identity_map` or directly to a known dashboard identity, then the usual role floor applies;
- bot actors are refused unless the repo is already one of the hive's governed repositories and the command is explicitly listed in `github.actions.allowed_commands`;
- the repo allowlist is the existing governed-repository list used by the mention poller;
- free text from `with: prompt` is scanned with `ioscan.EnforceInput` after the marker is removed;
- action kicks stay in plan/review behavior by default; apply-like `kick` commands require an owner identity and `github.actions.allow_apply: true`.

Transport B, hub OIDC dispatch, is the direct hub path for workflows that can reach the hub. The composite action requests a GitHub Actions OIDC token with `permissions: id-token: write`, an audience pinned to `github.actions.oidc.audience`, and posts to `POST /api/contribute/actions/dispatch` with `{command,prompt,issue,run_id,run_attempt}`. The endpoint lives under `/api/contribute/` so hosted spokes inherit the same hub gateway identity rules.

The hub verifies the token against the GitHub Actions issuer (`https://token.actions.githubusercontent.com`) and JWKS, rejects wrong `aud`, `iss`, expired/not-yet-valid tokens, and then maps the verified claims into the same action guard path as transport A: `repository` must be governed, `actor` maps through `github.actions.identity_map` or an existing dashboard identity, bot actors remain constrained by `github.actions.allowed_commands`, and apply-like `kick` commands still require owner plus `github.actions.allow_apply: true`. The prompt is scanned with `ioscan.EnforceInput` before kick text is built. Dedupe shares transport A's store and key shape (`repo + run_id + run_attempt`). Audit entries use `source=action` and add `transport=comment` or `transport=oidc`, plus OIDC `workflow` and `ref`, so operators can distinguish the relay path without creating a new authorization surface.
