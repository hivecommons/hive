# Fleet health: the verdict and remediation hints

The hub's fleet page (`/fleet`) shows one health verdict per hive: a colored
dot, a small "WHY" chip next to the hive name that states the reason in one
phrase, and — for a non-green verdict that matches a known failure signature —
a one-line remediation hint that says what to do and where to do it.

This page documents what each verdict state means, how the verdict is
computed, every remediation hint an operator can see, and how to go from a
symptom on the fleet page to a fix.

The verdict answers one question:

> **Does this hive have recent output back to its work source, for the ACMM
> level it is at?**

"Recent" is a 12-hour window, owned by the hub — a spoke cannot widen it.
The verdict is computed on the hub at read time from signals the spoke
already reports in its heartbeat; it costs the spoke no extra work and no
extra GitHub API calls.

This is a different check from
[dashboard route and health checks](health-checks.md), which covers HTTP
probes of the dashboard URL. A hive can pass every route probe and still be
red here, because the verdict measures *output*, not *reachability*.

## The four states

| State | Dot on `/fleet` | Meaning |
|---|---|---|
| **green** | ok | The hive has recent output for its level, or legitimately has nothing to produce (empty queue, or no agent is configured/on duty for the output its level is judged on). A green verdict **never** carries a remediation hint — a healthy hive gets no instruction. |
| **amber** | warning | The hive is working or intentionally quiet, but something needs an operator: all agents paused with work queued, output parked behind hold labels awaiting human review, an agent looping on a consent screen, an agent that will never be kicked, or a spoke lagging its release channel. |
| **red** | problem | Output is absent or impossible and the hive is expected to produce. The chip names the most fundamental cause it can prove (see [precedence](#precedence-the-most-fundamental-cause-wins)). |
| **unknown** | dashed ring | The hub cannot see enough to claim health: the hive is not reporting (offline), or the spoke is too old to send the signals the verdict needs. Unknown is never softened to green. |

Two display states on `/fleet` are more specific than the verdict and take
over the row when they apply:

- **parked** — every agent is deliberately paused or off-schedule and the
  hive has no queued reason to run; shown as a parked chip instead of a
  plain ok dot. A broken GitHub App still outranks parked: auth is broken
  whether or not the hive is in use.
- **offline** — not reporting; the dashed offline dot replaces the WHY chip.

## What "output" means at each ACMM level

The verdict is banded by the hive's ACMM level, because different levels are
*supposed* to produce different things. Absence of output a level does not
produce is never a fault.

| Level | Judged on | Healthy when |
|---|---|---|
| L1 (Inception) | nothing | Always green while reporting — no output is expected, and even precondition gaps (App, logins, cadences) are not faulted at L1. |
| L2 (Advisory) | the advisory digest | The advisory issue is fresh or aging. A spoke-reported posting error is red regardless of timestamps — the spoke proved the digest is wedged. |
| L3–L5 | writes to the work source: issue/PR creates, comments, and reviews | Any write in the last 12 hours. Merges are the human's job at these levels, so an unmerged PR is never a fault. Comments and reviews count: an agent triaging a backlog is producing real output. |
| L6 | merges | A merge in the last 12 hours. A create that never merges, with work still queued, is exactly the failure this level exists to catch. |

Three gates soften "no output" into green before anything goes red:

- **The queue gate.** If the actionable backlog (open issues + PRs the hive
  could work) is zero, "no output" means *idle*, not broken: the chip reads
  "queue empty — idle".
- **No writers on duty.** If no on-duty agent holds the write grant the level
  is judged on (for example, a quiet-mode hive whose only running agent
  cannot open issues or PRs), the hive is quiet by configuration, not
  failing. When grant-holding agents exist but are paused or off-schedule,
  the chip names them: "no output expected — create-capable agent(s) off:
  …", so you don't have to hunt the agent rows for the correlation.
- **Hold-stale (amber, not red).** At L3–L5, when the write stream is stale
  but work is parked behind hold labels awaiting human review, the agents
  produced and then correctly stood down — the next move is a person's.
  The chip reads "awaiting human review — N held for approval" and the row
  goes amber instead of red.

## Precedence: the most fundamental cause wins

When output is absent, three preconditions explain why it was *impossible*:
the GitHub App must be green, at least one agent must be logged in, and at
least one agent must be running at cadence. The verdict checks these in a
fixed order and returns on the first match, so a deeper cause can never be
shadowed by a shallower one:

1. **Inference gateway failing** — beats inferred App breakage. Runtime proxy
   failures and Gateway Test report DNS/connect/5xx/auth/budget classes; the
   chip says, for example, "inference gateway 'litellm' unreachable (dns)"
   or "inference gateway 'litellm' rejected key (401)" and points at
   Settings → Model Gateways.
2. **GitHub App broken** — applies when an actual App/auth problem is reported
   or no gateway fault is present. The chip names the specific failure when the
   spoke reported one (for example "GitHub App: repo-not-covered", which needs
   a different remedy than a bad key).
3. **Provider spending limit reached** — the model provider is refusing
   calls; shown with the refused-call count when known.
4. **Budget exhausted / budget misconfigured** — the hive's own token budget
   closed the gate (see [the budget signals](#budget-exhausted-vs-budget-misconfigured)).
5. **Blocked agents** — all blocked agents share one cause when possible:
   "N agent(s) out of provider quota", "N agent(s) stuck at login —
   re-login needed", "N agent(s) down — restart needed", "N agent(s) idle
   with work queued", "agent restarts: NAME ×N/24h (reason)", or the generic
   "N agent(s) blocked". Agent restart storms use the hub threshold
   `HIVE_HUB_AGENT_RESTART_PROBLEM_THRESHOLD` (default `5`).
6. **No agents running** (red) or **all agents paused** (amber — pausing is
   operator choice, not an outage, but queued work will not move until
   someone resumes them).
7. **Repo Issues disabled** — the repo's Issues tab is off (the GitHub
   default on forks), so the advisory digest and every agent-filed issue
   have nowhere to go. Nothing about the App, key, or agents is wrong; the
   fix is the repo setting.
7. **Banded output freshness** — only now, with every precondition green,
   can "no write in 30h (7 queued)" or "advisory stale" appear.

Because the precondition reds return early, a budget-exhausted hive shows
"budget exhausted", never the downstream "no write in Nh" — the remedies are
entirely different.

### Detector layering on top of the base verdict

Three detector signals refine the base verdict without ever masking a harder
fault:

- **Error-streak (red, re-explains).** When the base verdict is the
  *generic* stale-output red and an agent has 3 or more consecutive failed
  model calls, the chip is rewritten to "agent NAME model calls failing
  (N consecutive) — pin a working model". It only ever re-explains the
  generic red: precondition reds (App, provider, budget, login) keep their
  reason, because they gate the output stream upstream of the model calls
  and already name a more fundamental fix.
- **Consent-wedge and no-cadence (green → amber).** These demote a green
  verdict only. Output may still look acceptable, but a named agent is
  looping on a consent screen, or is enabled yet will never be kicked, and
  only an operator can fix either. They never touch a red or amber verdict —
  a harder fault stays the headline.
- **Channel-lag (green → amber).** A spoke tracking the `stable` release
  channel that is 2 or more commits behind the channel head, and not
  currently upgrading, ambers with "spoke lags its channel — N commits
  behind stable". One commit behind is a rollout in flight and is ignored,
  as is any hive where the channel signals are absent. See
  [release channels](release-channels.md).

L1 (Inception) hives and non-reporting hives are exempt from all detector
layering: no output is expected there, so a configuration gap is not a
health fault.

## Remediation hints: the signature → action table

Every non-green verdict whose cause matches a known signature carries a
one-line hint, rendered under the WHY chip as "→ *action*", with an
**open** link when the hub can build a direct URL. A hint is only shown when
it is *specifically* true: causes without a precise fix (provider limit,
agents down, advisory stale, …) deliberately get none rather than a guess.

| Cause | WHY chip you see | Verdict | Hint (action) | Where to do it |
|---|---|---|---|---|
| Inference gateway | "inference gateway 'litellm' unreachable (dns)" / "rejected key (401)" | red | Fix or retest the failing gateway (Settings → Model Gateways) | spoke dashboard settings |
| App broken | "GitHub App broken" or "GitHub App: \<state\>" | red | Install or repair the GitHub App for this repo | GitHub App settings (link resolved per cluster/forge) |
| Login stuck | "N agent(s) stuck at login — re-login needed" | red | Copilot device-flow login on the spoke dashboard | spoke dashboard `/login` |
| Budget exhausted | "budget exhausted — spend X of Y, kicks suppressed" | red | Raise or reset the budget limit (Settings → Budget) | spoke dashboard settings |
| Budget misconfigured | "budget limit misconfigured (N tokens) — agents halted, window reset will not help" | red | Raise or reset the budget limit (Settings → Budget) | spoke dashboard settings |
| Error streak | "agent NAME model calls failing (N consecutive) — pin a working model" | red | Pin a working model on the agent card | spoke dashboard agent card |
| Consent wedge | "agent(s) NAME stuck at Copilot consent — restarting in a loop" | amber | Complete the Copilot consent flow on the live pod dashboard | spoke dashboard |
| No cadence | "agent(s) NAME enabled but never kicked — set cadences" | amber | Set cadences on the agent card | spoke dashboard agent card |
| Hold stale | "awaiting human review — N held for approval" | amber | Review the needs-human queue | the repo's PR list (direct link) |
| Channel lag | "spoke lags its channel — N commits behind stable" | amber | Check auto-upgrade / force rollout | hub fleet version controls |

## Detector semantics

These are the signals behind the table above — what each one actually
measures on the spoke, and why a recovered hive clears its own alarm.

### Budget exhausted vs. budget misconfigured

`budget exhausted` means the governor is actively suppressing agent kicks
because the current budget window's tokens ran out. The chip shows "spend X
of Y" when the spoke reported the numbers, so you can tell at a glance
whether the hive is one rolled window away from healing or needs a bigger
limit.

`budget misconfigured` is separated out because the remedy is unrelated: a
positive limit below 100,000 tokens cannot fund even one model call, so the
gate closed on the first kick and *nothing was ever spent* — waiting for the
window to roll changes nothing, and the operator must fix the number. This
is almost always a unit mistake (a limit of "50" that meant 50M). A limit of
zero is exempt: it is the documented way to disable budget tracking
entirely.

### Agent error streaks

An agent whose CLI keeps running turns while every model call inside them
dies produces no output and no token usage, yet its session stays up and
reads green. The spoke detects this from its own chat recordings: each kick
appends a user turn, and a successful call appends an assistant reply
carrying real token usage — so a run of trailing turns with no usage-bearing
reply *is* the consecutive-failure streak, attributed per agent.

- The newest unanswered turn is ignored for its first 10 minutes (a kick
  delivered moments ago legitimately has no reply yet). A reply that
  explicitly recorded zero usage is a completed failure and counts
  regardless of age.
- A streak whose newest failure is older than 24 hours is history, not a
  live fault, and is not reported.
- The hub only rewrites the chip at 3 or more consecutive failures: one
  failure is noise, three in a row is a pattern.

The fix is to pin a working model on the agent's card in the spoke
dashboard. Note that when the agent is in automatic model selection, editing
config files cannot outrun the auto-picker — pin in the dashboard.

### Consent wedge

A Copilot CLI parked on an interactive consent screen looks alive to the
kick path's prompt check, so the kick path restarts the agent before sending
— which lands right back on the same consent screen. The result is a restart
loop (roughly once a minute) during which the agent reads green while doing
no work.

The spoke reports an agent as consent-wedged when its kick path hit a
consent-screen restart **within the last hour**. A live loop refreshes the
stamp on every kick attempt; once you complete the consent flow on the
spoke dashboard, no new restarts occur and the agent ages out of the list
by itself.

### No cadence

An agent that is enabled and governor-kickable but appears in **no** mode's
cadence map is never timer-kicked; if it has also never been kicked by any
other path, it will sit green and idle forever until someone sets a cadence.
The detector names exactly that: enabled + kickable + no cadence configured
in any mode + never kicked. An explicit "off" or "pause" cadence entry
counts as configured — that is operator choice, not omission — and on-demand
or event-driven agents are excluded. Fix: set cadences on the agent card.
See [agent configuration](agent-configuration.md).

### Carry-forward: why a recovered hive clears its own alarm

The three detector fields (error streaks, consent-wedged list, no-cadence
list) distinguish **"not measured"** from **"measured and clear"**:

- A spoke that is too old to report them, or that just restarted and whose
  collectors have not warmed up yet, sends *nothing* (null). The hub
  **carries the last real measurement forward** — blanking it would hide a
  live wedge for exactly the window an operator is most likely to be
  looking.
- A spoke that measured and found nothing wrong sends an explicit **empty**
  value (`{}` / `[]`), which **overwrites** the carried value on the hub.

The practical consequence: you never need to clear these alarms by hand.
Complete the consent flow, set the cadence, or pin a working model, and the
spoke's next heartbeat carries a measured all-clear that clears its own
signal. Conversely, an alarm that persists across a spoke restart is not a
stale artifact — it is the last real measurement, held until the spoke
measures again.

The scalar quadrant signals (budget spend/limit/exhausted, hold totals)
follow the same nil-means-not-measured rule: an old spoke that never sends
them is simply not judged on them.

## Output-freshness telemetry

For L3-L6 hives, newer spokes add optional heartbeat fields that explain stale write/merge streams: the most recent write-capable kick, the last kick disposition or skip reason, and how many queued items were deliberately deemed not writable. The hub treats missing fields as an old spoke and keeps the legacy red `no write in Nd (M queued)` behavior. When the fields are present, the verdict can distinguish a broken pipeline (recent write-capable kicks but no writes) from quiet-by-design states such as no due agents, budget-suppressed kicks, advisory-only operation, or work that agents intentionally declined as not writable.

## Troubleshooting: symptom → hint → fix

| Symptom on `/fleet` | What it means | Fix, and where |
|---|---|---|
| "GitHub App broken" / "GitHub App: repo-not-covered" | The hive cannot authenticate to its repo, or the App is installed but this repo is not selected | Install or repair the App; for repo-not-covered, add the repo to the App installation. Follow the row's **open** link, or see [GitHub App setup](github-app-setup.md) |
| "GitHub App: repo-moved" | The App installation is healthy, but it covers this hive's repositories under a **different account** — they were transferred to another org | Point the hive's configured organization at the account named in the message. Do **not** add the repo to the old org's installation: it has left that account, so there is nothing there to add |
| "provider spending limit reached — N refused calls" | The model provider is refusing calls on billing/limit grounds | Raise the provider's spending limit or wait for it to reset (provider console — no hint link) |
| "budget exhausted — spend X of Y, kicks suppressed" | The hive spent its window's tokens; the governor halted kicks | Raise the limit in the spoke dashboard (Settings → Budget), or wait for the window to roll |
| "budget limit misconfigured (N tokens) …" | The limit is too small to fund one model call — likely a unit mistake | Fix the number in Settings → Budget. Window reset will **not** help |
| "N agent(s) stuck at login — re-login needed" | Every blocked agent is wedged at a login prompt | Run the Copilot device-flow login on the spoke dashboard `/login` |
| "N agent(s) out of provider quota" | Agents hit per-account model quota | Wait for quota reset or change the account/model on the agent cards |
| "N agent(s) down — restart needed" | Agent sessions failed or died | Restart the agents from the spoke dashboard |
| "N agent(s) idle with work queued" | Sessions are alive but agents sat past the idle threshold with work available | Kick the agents or review their schedules — this is not a restart problem |
| "agent restarts: NAME ×N/24h (reason)" | One agent has been relaunched at least the configured threshold within the recent window | Use the `/fleet` reset button next to the restart chip before troubleshooting; the hub records a reset marker and asks the spoke to zero its counter on the next heartbeat so recurrence is visible |
| "no agents running" | Agents are expected on but none are running | Start/resume agents on the spoke dashboard |
| "all agents paused — resume to produce output" (amber) | Every agent is operator-paused while work is queued | Resume agents when you want the queue to move — deliberate pause is respected, not faulted |
| "repo Issues disabled — advisory/issues have nowhere to go" | The repo's Issues tab is off (common on forks) | Enable Issues in the repo's settings on your forge |
| "advisory stale" / "advisory posting failing" | The L2 output — the advisory digest — is old, or the spoke reported a posting error | See [advisory digest staleness](advisory-staleness.md) |
| "pipeline broken — write-capable kick … but no writes" | Kicks are reaching write-capable agents, but no work-source writes are appearing | Check the kicked agent logs and write permissions on the spoke dashboard |
| "nothing to write — governor idle since … because …" | The governor is intentionally idle (for example no due agents), so stale writes are not a broken pipeline | Review schedules only if you expected an agent to be due |
| "advisory-only — …" | The hive is in an advisory-only band/path, so writes are not expected from that activity | Nothing, unless you intended L3+ write behavior |
| "nothing writable — N queued deemed not writable" | Agents classified queued items as held or not writable and stood down | Review the queued/held items; no restart is implied |
| "no write in Nh (M queued)" / "no create output" | Older spoke or no freshness telemetry: preconditions are green but the write stream appears stalled | Check agent activity and logs on the spoke dashboard; if an error-streak chip appears instead, follow that hint first |
| "no merge in Nh (M queued)" (L6) | PRs are being created but nothing merges | Check merge-agent grants and required checks on the queued PRs |
| "agent NAME model calls failing (N consecutive) — pin a working model" | The agent's turns run but every model call dies | Pin a working model on the agent card in the spoke dashboard |
| "agent(s) NAME stuck at Copilot consent — restarting in a loop" | The kick path keeps restarting an agent parked on a consent screen | Complete the consent flow on the spoke dashboard; the alarm clears itself within the hour |
| "agent(s) NAME enabled but never kicked — set cadences" | The agent has no cadence in any mode and has never been kicked | Set cadences on the agent card |
| "awaiting human review — N held for approval" | Output is parked behind hold labels; the next move is a person's | Review the held PRs (the hint links straight to the repo's PR list) |
| "spoke lags its channel — N commits behind stable" | The channel moved and the spoke did not follow | Check auto-upgrade / force a rollout from the hub's fleet version controls; see [release channels](release-channels.md) |
| "not reporting (offline)" (unknown) | No recent heartbeat | Check the spoke's pod/container and network path to the hub; see [troubleshooting](troubleshooting.md) |
| "spoke too old to report health" (unknown) | The spoke predates the health signals | Upgrade the spoke image |
| "queue empty — idle" (green) | Nothing to work on — this is healthy | Nothing. Add work sources if you expected a backlog |
| "no output expected — …-capable agent(s) off: NAMES" (green) | The agents that could produce this level's output are paused/off-schedule | Nothing, unless you want output — then resume the named agents |

## Where the verdict comes from (for the curious)

The verdict is computed in `src/pkg/hub/health_verdict.go` (base verdict and
ACMM banding) and `src/pkg/hub/remediation.go` (detector layering and the
signature → action map), from fields the spoke reports in its heartbeat
(`src/pkg/hub/heartbeat.go`). The spoke-side detectors live in
`src/pkg/tokens/error_streak.go` (error streaks, from chat recordings),
`src/pkg/agent/consent_wedge.go` (consent-screen restart tracking), and
`src/pkg/governor/no_cadence.go` (cadence-less agents). The `/fleet`
rendering — dot, WHY chip, and hint line — is in
`src/pkg/hub/static/my-hives.html`.
