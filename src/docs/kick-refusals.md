# Kick refusals

A hive owner who sees `kick_refusal` in the dashboard, or a toast that says
"Kick failed", is looking at a **kick refusal**. This page says what that
means, why it happens, and what to do about it.

## What a kick is

A **kick** is the message the hive sends an agent to start a work pass: the
governor builds it from the current queue (issues, pull requests, knowledge)
and types it into the agent's CLI, or starts a sandboxed run for it. The
governor sends kicks on each agent's cadence; an owner can also send one with
the **kick** button on an agent card (see
[Agents are stuck, paused, or need CLI login](troubleshooting.md#agents-are-stuck-paused-or-need-cli-login)).

## Why a hive refuses a kick

A kick is refused in one of two places:

- **The hive refuses to deliver it.** Before typing anything, the hive checks
  that the agent can take a kick right now. If it is paused, failed to start,
  has no live session, is backing off a failing model provider, or was just
  restarted, the kick is not delivered and the caller gets an error naming the
  reason. These show up as a **Kick failed** toast, an HTTP `400` from
  `POST /api/kick/{agent}`, or a `failed` status from
  `GET /api/kick/{agent}/status`.
- **The agent's model refuses to act on it.** The kick was delivered, but the
  model answered that it will not do the work (usually because it read the
  kick as a prompt-injection attempt). The hive spots the refusal in the
  agent's terminal output and flags the agent. This is what the **`kick_refusal`**
  health check reports.

Either way the cadence slot is spent, and the governor tries again at the next
interval. A refusal does not change configuration, so nothing is lost.

## Every refusal reason

Each reason has its own section below, so a link can point straight at it. The
dashboard links here with the same anchor names (`#paused`, `#provider_backoff`,
and so on).

| Reason (anchor) | Meaning | What to do |
|---|---|---|
| [`kick_refusal`](#kick_refusal) | The deep-health check that lists every agent currently flagged as having refused a kick. | Open each listed agent and follow its own reason below. |
| [`agent_declined`](#agent_declined) | The agent's model replied that it will not act on the kick. | Read the quoted line, then see the section. Often transient; persistent refusals need a prompt or model change. |
| [`startup_deferred`](#startup_deferred) | The agent was still starting, so the kick was held until startup finishes. | Normally nothing: it is delivered when the CLI is ready. |
| [`paused`](#paused) | The agent is paused, by an owner, a budget guard, or a safety trigger. | Resume it from the agent card, after checking why it was paused. |
| [`failed_start`](#failed_start) | The agent failed to start, or is blocked after repeated failed starts. | Read the start error on the agent card; fix it, then restart the agent. |
| [`not_running`](#not_running) | The agent is stopped or has never been started. | Start it from the agent card, or check why it is stopped. |
| [`provider_backoff`](#provider_backoff) | The model provider is returning errors (quota, rate limit, outage), so kicks wait out a backoff. | Wait for the backoff, or fix the provider credential or quota. |
| [`restart_hold`](#restart_hold) | The agent was just restarted mid-turn; kicks are held briefly so a second kick does not restart it again. | Wait about a minute. |
| [`restart_cancelled`](#restart_cancelled) | The agent restarted while the kick was waiting for the CLI prompt, so the kick was dropped. | Normally nothing: the next kick is delivered. Investigate only if restarts repeat. |
| [`delivery_busy`](#delivery_busy) | A kick is already being delivered to this agent. | Nothing: do not click again. |
| [`no_session`](#no_session) | The agent has no live terminal session, or its CLI is not at a prompt and did not recover. | Restart the agent from its card, then inspect the session. |
| [`sandbox_blocked`](#sandbox_blocked) | A sandboxed agent cannot run the kick: missing or invalid sandbox settings, or a live interactive session in the way. | Fix the named sandbox setting, or stop the interactive session. |
| [`unknown_agent`](#unknown_agent) | The kick named an agent that is not configured. | Check the agent name in your configuration. |

### kick_refusal

The `kick_refusal` entry in the deep health report (`GET /api/health/deep`, and
the navbar health chip's hover list) has status `warn` and lists the agents
whose `KickRefused` flag is set, for example `scanner, reviewer refused kick`.
The per-agent detail reads `refused kick: <reason>`, where the reason is either
the line the model wrote ([`agent_declined`](#agent_declined)) or
`deferred until startup completes` ([`startup_deferred`](#startup_deferred)).

The flag clears on its own after the next delivered kick or a restart of the
agent. A warning that appears once and clears is normal. A warning that stays
on the same agent across several cadence intervals is a problem.

### agent_declined

The model, not the hive, refused. The hive watches each agent's terminal output
for first-person refusals such as "I'm declining to execute", or a mention of a
prompt-injection attempt, and records the line (shortened to 200 characters).
It ignores bullet, table, heading, quote and issue-reference lines, because
those are the kick text echoed back, not the agent's own words.

In the logs this is the warning `agent refused kick`, with the matching
`pattern` and the `line` the agent wrote.

- **Normal:** a one-off refusal on a security-review kick, or a first refusal
  on a kick that began with the hive's exfiltration canary marker. When that
  happens the hive re-delivers the kick **once** without the canary, so you
  will see an `agent refused kick` warning followed by a second kick.
- **Problem:** the same agent refuses every kick. The model has decided the
  work is not something it will do. Change the agent's model or backend,
  simplify its custom prompt or knowledge entries, or check that no untrusted
  text is being pasted into the kick.

### startup_deferred

The agent's CLI was still launching when the kick arrived. The hive stored the
kick and sets the refusal reason `deferred until startup completes`; it types
the kick into the fresh pane as soon as the CLI is ready.

- **Normal:** it appears for a few seconds to a couple of minutes after a
  start or restart.
- **Problem:** it stays for many minutes. The CLI is not coming up; follow
  [`no_session`](#no_session).

### paused

Error text: `agent <name> cannot be kicked: it is paused`, followed by the
trigger in parentheses and the reason when there is one (for example
`(by budget)`). A paused agent is never kicked, by the governor or by hand.

- **Normal:** you paused it on purpose.
- **Problem:** you did not. The trigger names what paused it. Fix that cause
  (for example the spend guard or repeated crashes) before resuming, or it will
  pause again.

### failed_start

Error text: `it failed to start: <error>` or `it is blocked after repeated
failed starts: <description>`. The hive stops retrying a start that keeps
failing, so a person has to fix it.

- **What to do:** read the error on the agent card. The usual causes are a
  missing CLI login, a bad model ID, or an unreachable backend. Fix it, then
  restart the agent. Restarting without a fix returns the same state.

### not_running

Error text: `it is stopped` or `it has not been started yet`. An owner stopped
the agent, or it has not been started since the hive booted.

- **Normal:** the agent is disabled or stopped on purpose.
- **What to do otherwise:** start it from the agent card. If it stops again by
  itself, see [`failed_start`](#failed_start).

### provider_backoff

Error text: `agent <name> blocked: inference (<class>): <line>; next provider
probe in <duration>`. The agent's last output showed a model provider error
(quota exhausted, rate limit, authentication failure, outage). The hive stops
kicking it for a backoff period instead of burning cadence slots on a provider
that is failing. The backoff starts at `HIVE_PROVIDER_ERROR_BACKOFF_BASE`
(default `2m`) and doubles up to `HIVE_PROVIDER_ERROR_BACKOFF_MAX` (default
`30m`); see [environment variables](env-vars.md).

- **Normal:** a short rate-limit or provider blip that clears by itself.
- **Problem:** the same class persists. Check the provider's quota and billing,
  the agent's credential, and the provider's status page.

### restart_hold

Error text begins `kick held after restart:`. When a restart destroys a turn
that was producing output, the hive holds new kicks for that agent for 60
seconds, so the restart is not immediately followed by a kick that gets
restarted too.

- **What to do:** wait. If an owner clicked **kick** during the hold, click
  again after the time named in the message.
- **Problem:** repeated holds mean the agent keeps restarting mid-turn. Look
  at its restart reason on the card and in the logs (`audit: kicks held after
  restart interrupted a producing turn`).

### restart_cancelled

Error text begins `kick cancelled by restart:`, or the kick status reads
`cancelled: agent restarted`. The kick was waiting for the CLI's input prompt
when the agent restarted, so it was dropped instead of being typed into the
wrong session.

- **Normal:** around a deploy, upgrade or manual restart.
- **Problem:** it keeps happening without a restart you caused. Treat it as a
  crash loop.

### delivery_busy

Error text: `agent turn in progress: agent <name> kick delivery already in
progress`. The hive delivers at most one kick to an agent at a time, so a
second click or a second trigger is harmless and is not sent twice. The
dashboard shows "A kick is already being delivered".

- **What to do:** nothing.

### no_session

Error text: `tmux session <name> not found`, `agent <name> CLI did not become
ready after restart`, or `failed to restart crashed agent`. The agent's CLI is
gone, sits at a bare shell, or is stuck on a consent screen, and the hive's
attempt to restart it before the kick did not work.

- **What to do:** use the `↻ restart` action on the agent card, then follow
  [Inspecting the session](troubleshooting.md#inspecting-the-session) if it
  does not come back. A CLI that needs a fresh login is the most common cause.

### sandbox_blocked

Applies to agents that run each kick in a sandbox (podman, Kubernetes Job,
OpenShell or Substrate). Error text names the setting, for example `sandbox
image is not configured`, `sandbox execution requires a primary repo`,
`sandbox job runtime requires job.workspace_claim` (not needed with
`workspace_transport: copy`), or `agent has a live
interactive session; sandbox kicks are blocked until it stops`.

- **What to do:** set the named field in the agent's sandbox configuration
  (see [Sandbox isolation](sandbox-isolation.md)), or stop the leftover
  interactive session. Switching an agent into sandbox mode stops its terminal
  session on its own.

A Kubernetes Job kick on `workspace_transport: copy` also fails closed, before
it creates anything, with `kubejob: workspace_transport: copy is refused
because HIVE_REQUIRE_HARDENED is set` (copy mode skips the relay containment
preflight; use the claim transport on a hardened hive) or `kubejob:
workspace_transport: copy is refused because hive's own pod runs under
runtimeClassName "<class>" and job.runtime_class_name is empty` (set
`job.runtime_class_name`, usually to the same class). See
[Sandbox isolation](sandbox-isolation.md).

### unknown_agent

Error text: `agent <name> not found`. The name does not match a configured
agent, often after a rename or a config reload.

- **What to do:** check the agent list in the dashboard or `hive.yaml`.

## What a kick came to

A delivered kick is not a finished piece of work. The hive keeps a durable,
bounded record of the last ten kicks of each agent (`GET /api/kick-outcomes`):
who or what triggered each one (a dashboard user's login, the governor, a
workflow hand-off), when it started and ended, and what it came to:

| Outcome | Derived from |
|---|---|
| `opened_pr` | the relay opened a pull request for the agent during the kick (or within 15 minutes after its turn ended); the record keeps the PR number and URL |
| `pushed_commit` | the relay pushed a branch for the agent; the record keeps the branch and sha |
| `commented` | the relay posted a comment for the agent |
| `stood_down` | the agent ended its turn on a stand-down line; the record quotes that line |
| `refused` | the hive refused to deliver the kick (including a governor kick withheld during `provider_backoff`, recorded without the provider's error line), or the agent's model declined it; the record keeps the reason code from this page and the quoted text |
| `still_running` | the kick's turn has not ended and no signal has arrived yet |
| `ended_no_visible_action` | the turn ended, or can no longer be told apart from one that ended, and no signal above speaks for it |

The outcome is derived only from those signals when it is read, so a pull
request the relay opens a few minutes after the turn ends upgrades the outcome.
Where the hive cannot tell, it says `ended_no_visible_action`; it never
guesses. Refusals are recorded only for the reasons on this page.

The quoted text is what an agent (or the hive) wrote, so it is untrusted: the
hive redacts token-shaped strings, caps it at 200 characters and escapes it
when shown, and never puts it into an agent prompt. Agents cannot query the
record. Owner and read-write sessions see the detail; a read-only session sees
only the one-line summary. Log lines about the record hold ids and kinds only.

## Where to see refusals

- **Toast.** Clicking **kick** on an agent card shows "Kick failed: ..." with a
  **What does this mean?** button that opens the matching section above.
- **Health chip.** The navbar health chip's hover list shows `kick_refusal`
  under warnings with a link to [`kick_refusal`](#kick_refusal).
- **API.** `POST /api/kick/{agent}` and `GET /api/kick/{agent}/status` return a
  `docs_url` field next to `error` when the failure matches a reason on this
  page; `GET /api/health/deep` carries the same field on the `kick_refusal`
  check. Clients that do not know the field can ignore it.
- **Logs.** Search the hive logs for `agent refused kick`, `agent kick deferred
  until startup completes` and `audit: kicks held after restart`. See
  [Find the running service logs](troubleshooting.md#find-the-running-service-logs).
- **Past kicks.** The **past kicks** button on an agent card opens the archived
  output of earlier kicks; see [Per-kick log history](agent-logging.md#per-kick-log-history).

## Normal and a problem

A refusal is normal when it appears once and clears: during startup, right
after a restart, behind a short provider blip, on a deliberately paused agent,
or when you double-click **kick**.

It is a problem when the same agent shows the same reason across several
cadence intervals, when `failed_start` or `no_session` will not clear, or when
`agent_declined` follows every kick. Each of those means the agent is
effectively off while the dashboard may still say it is scheduled.

## Related

- [Troubleshooting](troubleshooting.md)
- [Agent configuration](agent-configuration.md)
- [Sandbox isolation](sandbox-isolation.md)
- [Health checks](health-checks.md)
- [Architecture: the governor loop](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick)
