# Hive operator reference

[Contents](../operator-reference.md)

## Fleet breaker

The dashboard top bar includes a fleet breaker: a circular pause/play control plus a status pill. The `GET /api/breaker` state is readable by authenticated viewers, but engaging or releasing the breaker is owner-only (`POST /api/breaker/engage`, `POST /api/breaker/release`).

Engage pauses every agent that is currently running and is not `on_demand`. Agents already paused before engage are skipped and keep their original pause reason. Release resumes only the exact agents the breaker paused and still owns (`PausedTrigger == fleet-breaker`); if an operator manually re-pauses an agent while the breaker is engaged, release leaves it paused. The breaker state is persisted so a crash/restart can still release the same captured set.

Fixes #2953.

## Self-reported issues

When a hive notices an unusual error in itself — a restart storm, repeated
kick failures, a recovered panic, degraded model discovery, inference
auth failures or advisory post failures — it files an issue on
`hivecommons/hive` titled `[selfreport] <class>: <short message>`, or adds
itself to an open issue that already describes the problem (fingerprint
match, `hive/selfreport` + class match, or keyword match on a human-filed
issue). Joining posts one "Also seen on **<hive>**" comment per 24h and
bumps the `Affected hives (N)` block in the body, where each hive has its own
line — `- <hive> (instance <id>) — owner(s): @login — seen N, last <time>` —
naming the GitHub login(s) of the hive owner (`dashboard.authorized_users`
owner-role entries plus the device-flow operator) so a maintainer can see who
is affected. Each report also names the repositories the hive watches
(`project.repos`) and their owners, and the body block keeps an
`Affected repos (N)` / `Affected repo owners (N)` union across reporting
hives (hivecommons/hive#12338). Every selfreport carries
`origin/selfreport` (the provenance axis every hive-created issue records,
see [labels-and-control-signals.md](../labels-and-control-signals/reference.md#origin-sources))
next to the deprecated `hive/selfreport`.

```yaml
governor:
  fleet_report:
    enabled: true            # unset = on for hub-managed hives, off (dry-run) for self-hosted
    repo: hivecommons/hive
    max_per_day: 5
    include_repos: true      # false: never name project.repos / repo owners (set this if any watched repo is private)
    include_owners: true     # false: render "(anonymous)" instead of the hive owner's GitHub login(s)
```

Rate limits: `max_per_day` new issues per hive per 24h, at least 1h between
new filings of different classes, no re-filing of a fingerprint closed
upstream while the hive keeps observing the condition (7-day cooldown from
the last observation), deferred while the GitHub API budget is not normal. Issues
are never auto-closed; the hive posts one "cleared" comment when the
condition stops. Hub-managed hives that cannot write to the report repo relay
through the hub (`POST /api/selfreport/ingest`, heartbeat bearer). The
dashboard shows "Self-reports: N open (last: <class> → #NNN)" in the Fleet
Report panel and the toggle/repo/cap under Settings → Governor →
"Self-Reported Issues". Full details: [fleet-report.md](../fleet-report.md).

## Agent CLI session recycling

Each agent's CLI (Copilot, Claude, …) is a long-lived Node/V8 process whose
heap grows with every turn and never shrinks while idle. The health tick
samples the resident memory of the whole process tree under each agent's
tmux pane and recycles a session that is over budget — but only while the
agent is idle at its input prompt with no kick pending, so a turn is never
interrupted. A recycle is an ordinary restart (`last_restart_reason: session
recycle`): claims, queues and the kick log are preserved exactly as on any
other restart, and the governor re-kicks on the next cadence. After a recycle
the same agent is left alone for ten minutes even if it is over budget again.

```yaml
agents:
  session_recycle:        # fleet default; on unless enabled: false
    max_rss_mb: 1536      # recycle an idle session whose process tree exceeds this (MiB)
    max_age: 6h           # …or whose CLI session is older than this
    min_idle: 2m          # pane + transcript must be silent this long first
  scanner:
    session_recycle:      # optional per-agent override; absent fields inherit the fleet value
      max_rss_mb: 2048
```

The sampled value is shown as **Session RSS** (with the session age and a
`↻N` recycle count) in the agent detail panel. The log line is
`agent session recycled` with `rss_mb`, `age` and `reason`.

## GitHub API quota

Agents reach `api.github.com` through the Hive GitHub proxy, which injects the
agent-scoped App installation token and records charged requests. The App
installation quota is shared with the daemon's scan, merge, and maintenance
loops, so the proxy enforces two safeguards:

- `agents.github_api_hourly_cap` (default `300`) limits each agent to that many
  charged GitHub API responses in a sliding one-hour window. A charged response
  is any proxied response other than `304 Not Modified`; the proxy's own token
  minting is not agent-attributed and is not counted. Set the value to `0` to
  disable the fleet default, or set `agents.<name>.github_api_hourly_cap` to
  override one agent (`0` disables that agent's cap).
- `github.agent_reserve_floor` (default `400`) protects the shared core bucket.
  When the latest rate-limit reading shows `core.remaining` below the floor,
  agent read requests receive the same proxy `429` even if the agent has not
  reached its personal cap. This keeps daemon merge/scan work from being
  starved by agent polling. Only readings from the hive's own bucket update
  that snapshot: responses whose `X-RateLimit-Limit` is 60 (GitHub's per-IP
  unauthenticated bucket, e.g. an agent's `gh` call without a token) or whose
  limit differs from the daemon's are ignored, so they cannot trip the floor
  or the API budget mode.

The proxy returns JSON shaped for the GitHub CLI:

```json
{"message":"hive: agent GitHub API hourly cap reached (N/cap); stop polling and continue with local work","documentation_url":"…docs/operator-reference.md#github-api-quota"}
```

It also sets `Retry-After` to when the one-hour window (or observed reset) can
make progress, logs once at 80% of each agent window, and sends one
stop-polling nudge per agent window. Hive-authored write relays are not
agent-attributed and are not capped by this path.

Open-PR scans use `github.graphql_pr_batch` by default to move mergeability,
review-decision, linked-issue and CI-rollup reads into GitHub's separate
GraphQL bucket. `/api/gh-rate-limits` reports these calls as caller
`hive:pr_batch` on endpoint `/graphql` and exposes `graphql_pr_batch` counters
for repositories, PRs, pages, REST fallbacks, errors, and the last GraphQL
query cost. Per-PR REST reads still happen when the batch is disabled, a repo's
GraphQL query fails, or GitHub returns `mergeable: UNKNOWN` for that PR.

### Webhook-driven PR cache invalidation

See [GitHub webhooks: cadence, patch and reconcile](../webhooks.md) for the
cadence keys, the fallback to polling when deliveries degrade, and the
accuracy guarantees of the ForgeState contract.

A spoke accepts GitHub App webhooks on the public `POST /api/webhook/github`,
either delivered directly by GitHub or relayed by the hub (the hub forwards the
original signed body to the hive that manages the repository). The receiver
fails closed: it rejects every delivery until `GITHUB_WEBHOOK_SECRET` is set to
the App's webhook secret, and it verifies `X-Hub-Signature-256` over the raw
body exactly as the hub does. Deliveries for repositories the hive does not
manage are ignored.

#### What the operator configures

1. **Secret.** Set `GITHUB_WEBHOOK_SECRET` in the spoke's environment (a
   Secret mount on Kubernetes; never in `hive.yaml`) to the App's webhook
   secret. On a hosted spoke this is the same value as the hub's
   `webhook-secret.key`. Without it the receiver answers 503 and the loop
   stays on the polling cadence.
2. **GitHub App webhook.** Point the App's webhook URL at the hub
   (`POST /api/github/webhook`) for hosted spokes, or at the spoke's own
   `/api/webhook/github` for a stand-alone hive (a repository or organization
   webhook with the same secret works too). Subscribe the App to these events:
   `pull_request`, `pull_request_review`, `pull_request_review_comment`,
   `issue_comment`, `issues`, `check_suite`, `check_run`, `status`, `push`,
   and, with `governor.work_source.type: github_projects`, `projects_v2_item`
   (organization-scoped; see [Work sources](../work-sources.md#type-github_projects--github-projects-projects-v2)).
3. **Hub relay.** The hub verifies the HMAC and relays the body byte-for-byte,
   with the original signature and `X-GitHub-Delivery`, to the spoke whose
   reported dashboard URL manages the repository. Nothing to configure on the
   spoke beyond the shared secret.
4. **Pacing (optional).** `governor.eval_interval_webhook_s` (default `900`)
   is the base tick while webhooks are healthy; `governor.webhook_wake_debounce_s`
   (default `10`) and `governor.webhook_wake_min_gap_s` (default `30`) pace the
   early cycles described below. `review.event_driven` additionally lets
   `pull_request` deliveries dispatch reviews early.

Verify: `/api/status.webhooks.events_1h` climbs after a push or comment,
`/api/status.evalMode` reads `webhook`, and the spoke log shows
`github webhook wake: running an early eval cycle` with the delivery's reason.

Through the hub, every enrolled hive that tracks the delivery's repository
and has a dashboard URL receives the relay, so multiple hives (for example
v5 and v6 spokes) tracking one repo all stay on webhook mode.

#### What a delivery does

- `pull_request`, `pull_request_review`, `pull_request_review_comment`,
  `check_suite`, `check_run`, `status`, `issue_comment` (on PRs only) and
  `push` invalidate the cached PR detail and the GraphQL batch
  check-run/review entries for the affected PR and mark it dirty. Check and
  status events are matched by PR number and head SHA (fork PRs arrive
  without a PR list); `push` invalidates every open PR whose base or head
  branch is the pushed branch. `issues` `opened`/`labeled`/`unlabeled`
  mark the issue dirty and wake evaluation; `reopened` wakes evaluation too.
  To select the scanner for newly seen actionable issues, configure a
  top-level [CEL trigger](../cel-triggers.md#run-the-scanner-for-newly-seen-github-issues)
  matching `event.kind == "issue.opened"`. Agent Channels is not a webhook
  installer and only supports the governor `kick` runtime.
- `pull_request` `opened`/`synchronize`/`reopened` also hands the PR to the
  [sentinel](sweeps-and-operations.md#suspicious-activity-alerts-sentinel), which evaluates it now
  instead of waiting for its reconcile cadence
  ([#13379](https://github.com/hivecommons/hive/issues/13379)).
- `projects_v2_item` (`created`/`edited`/`deleted`/`reordered`/`archived`/
  `restored`/`converted`) patches the GitHub Projects work-source cache: the
  delivery names the item and its project, the hive re-reads that one item
  (one GraphQL call) and upserts it, or drops it on `deleted`/archived. The
  payload carries no repository, so the spoke receiver routes it ahead of the
  repository-keyed events and the response carries `projects_patch`. The hub
  relay forwards repository-keyed deliveries only, so a hosted spoke using a
  Projects board needs the organization webhook pointed at its own
  `/api/webhook/github` for this event.
- **Every invalidating delivery wakes the governor loop.** The wake is
  debounced for `governor.webhook_wake_debounce_s` so a push and its check
  fan-out coalesce into one cycle, and webhook-triggered cycles are at least
  `governor.webhook_wake_min_gap_s` apart so a delivery storm stretches the
  batch instead of spinning cycles. **The wake cycle does not re-list
  repositories.** It plans from the governor's cached actionable snapshot
  (the one the last cadence tick enumerated, patched by the targeted
  re-reads described below), so a wake costs the targeted re-read plus the
  dirty-PR detail refreshes and nothing else: repositories with no dirty PR
  reuse their previous GraphQL PR batch (`graphql_pr_batch.webhook_skips`),
  clean PRs are served from the PR detail cache past
  `github.pr_detail_ttl_s`, and only the dirty PRs are re-read. The log line
  is `github webhook wake: running an early eval cycle` (`reason`,
  `deliveries`, `coalesced`, `source=cached`), followed by
  `eval cycle planning from cached snapshot`.
- Webhooks are **healthy** while the last delivery for a managed repository is
  newer than `2 × governor.eval_interval_s`. The first healthy window puts
  the loop in **webhook mode**: the governor's base eval interval rises to
  `governor.eval_interval_webhook_s` (default `900`, capped by
  `governor.eval_interval_max_s`) — safe because the wakes above carry the
  latency — and `/api/status.evalMode` reads `webhook`,
  `/api/status.webhooks.mode` reads `webhook`. The API budget stretch still
  applies; the larger interval wins.
- **Fallback to polling** ([webhooks.md](../webhooks.md#fallback-when-webhooks-degrade)).
  Webhook mode degrades when the window lapses *while GitHub was active*
  (a cadence pass listed an item updated after the newest delivery) or when
  deliveries reach the receiver but fail verification; a quiet repository
  with no deliveries and no activity keeps the stretched tick. The loop
  re-checks as the window closes (not just at the next stretched tick),
  resets its ticker to `governor.eval_interval_s`, logs one INFO
  `webhooks degraded; polling cadence restored` (with `reason`), and
  `evalMode` reads `poll`, `webhooks.mode` reads `polling` with `since` and
  `mode_reason`. The way back has hysteresis: deliveries must keep arriving
  for `2` consecutive healthy windows (`webhooks.healthy_windows` counts
  them) before one INFO `webhooks healthy; webhook cadence engaged` restores
  the webhook cadence.
- `pull_request` `opened`, `reopened`, `synchronize`, `ready_for_review` and
  `review_requested` deliveries also queue an early review dispatch when
  `review.event_driven` is on; see
  [Event-driven dispatch](../review-swarm.md#event-driven-dispatch).
- **A failed check reacts within seconds, not at the next sweep.** A
  `check_run` (`completed`, conclusion `failure`/`timed_out`), `check_suite`
  (`failure`/`timed_out`) or `status` (`failure`/`error`) delivery is
  resolved to the open PR(s) at that head from the PR detail cache — one
  cached lookup, no extra `GET /pulls` fan-out; a head no cached PR matches
  is counted under `/api/status.ci_failure_signals.unmatched` and left to
  the sweep's dirty mark. Each match becomes a **CI failure signal**
  `{repo, number, head, check, details_url, at}`, deduplicated per head +
  check and kept for two hours. A **hive-authored** PR is attributed to the
  agent whose relay opened it (`agent_pr_created` in the audit log; scanner
  when unattributed) and that agent is kicked at once with its
  FIX-BEFORE-NEW section naming the check and the log URL — the same
  section every regular kick carries, into which the signal is also folded
  until `ci-failing.json` catches up, so nothing is lost if the kick is
  gated. A **contributor** PR nudges the automerge lanes for a targeted pass
  over that PR now (the contributor lane / `stage/072` path), with or
  without the ForgeState cache. Guards, in this order: `governor.ci_failure_webhook_kick`
  (default `true`; `false` records signals only), the repo pause
  (`project.paused_repos`), the agent's config/runtime pause (the fleet
  breaker pauses every agent), governor kick eligibility, and one
  webhook-triggered kick or nudge per PR head per
  `governor.ci_failure_kick_cooldown_s` (default `600`). The activity log
  records `ci_failure_kick` — "CI failed on #N (check) — kicked agent X" /
  "nudged contributor lane". **Agents never watch or poll CI themselves**:
  detection is out-of-band via this webhook path, or the sweep as fallback.
- **A recorded review verdict also wakes the governor loop.** When the
  relay records a verdict bound to a review dispatch, the head that wrote it
  is free and the PR may be ready for its next head or its decision kick, so
  the loop runs a cached-snapshot cycle now instead of waiting for an
  unrelated delivery or `governor.eval_interval_webhook_s`. Advisory verdicts
  do not wake it. The first verdict wakes at once; later ones inside
  `governor.webhook_wake_min_gap_s` fold into one wake at the end of the gap.
  `governor.webhook_wake_debounce_s` does not apply to this wake (its
  debounce is fixed at 0, so a verdict is never held back), and the spoke logs `review verdict wake: running an early eval cycle from
  the cached snapshot`
  ([#14705](https://github.com/hivecommons/hive/issues/14705)).
- Every status publish (cadence or wake) invalidates the `/api/stage-tracker`
  cache and sends a `stage-tracker` event on `/api/events`, so the dashboard's
  activity monitor refetches at once instead of waiting out
  `dashboard.stage_tracker.cache_ttl_s` plus its poll. Invalidation only
  marks the cached body stale: the publish, and every webhook below, returns
  at once even while a tracker rebuild is running. The next poll starts
  **one** rebuild outside any lock; polls that land during it get the
  previous body with `source: stale` and `max-age=0` rather than queueing,
  so a slow build (one sentinel evidence read per open PR, on NFS in hosted
  hives) can never back up webhook handlers, the governor's publish, or an
  unbounded line of dashboard requests (hivecommons/hive#14681).
- **The activity monitor row moves without waiting for a cycle.** A delivery
  that names a PR or issue this hive tracks (`pull_request*`,
  `check_suite`/`check_run`, `status`, `push` to a PR branch, `issue_comment`,
  `issues` — any action) also drops the `/api/stage-tracker` cache at once
  and queues a **targeted re-read of just that item**: one `GET` of the PR
  or issue, debounced by `dashboard.stage_tracker.webhook_refresh_debounce_ms`
  (default `2000`) and coalesced per item, so a check fan-out of twenty
  `check_run` deliveries costs one request. The re-read patches that row of
  the status snapshot in place — labels, title, draft flag, last activity;
  a closed or merged item leaves — then drops the tracker cache again and
  sends the `stage-tracker` SSE nudge. The same re-read is handed to the
  governor, which upserts the item into its cached actionable snapshot —
  a new issue or PR appears, a closed or merged one leaves, a label change
  moves it between the actionable / held / sidelined buckets — so the wake
  cycle that follows plans against the patched item. No repository is
  re-enumerated. The read goes through the paced REST transport and is
  skipped outright while the API budget is `critical`; it refreshes the PR
  detail cache, so the following cycle does not pay for it again. The
  re-read is also remembered for
  `dashboard.stage_tracker.webhook_refresh_replay_window_s` (default
  `1800`) and re-applied to every status snapshot published meanwhile: an
  eval cycle that enumerated the item *before* the close still publishes
  from that enumeration minutes later, and without the replay its publish
  put the closed row back until the next full enumeration.
  `dashboard.stage_tracker.webhook_refresh: false` turns the re-read off
  (deliveries then still invalidate caches and wake the loop, and wake
  cycles fall back to a full enumeration because nothing else patches the
  snapshot). Independently of the re-read, the one payload fact the tracker
  trusts without a confirming read is a closed or merged `state` on the
  issue or pull request the delivery embeds: on *any* action (`labeled`,
  `unlabeled`, `created`, not only `closed`) that row leaves at once, so a
  refused or failed re-read cannot leave a closed item listed open. As a
  backstop for deliveries that never arrived, on each status publish it
  also lists each repo's items closed since its last look every
  `dashboard.stage_tracker.closed_sweep_interval_s` (default `300`; one
  list call per repo; negative disables) and evicts any still shown.

#### Polling cadence vs webhook patch vs reconcile

The full model — the three writers, the accuracy guarantees of #13373, every
cadence key with its default, and the webhook-outage fallback — is
[webhooks.md](../webhooks.md). In brief: inbound webhooks cost nothing against
the GitHub rate limit; outbound API calls do. The governor therefore keeps three distinct paths and never lets a
delivery trigger a full re-list:

| Path | Trigger | GitHub cost | What it does |
| --- | --- | --- | --- |
| **Reconcile** | cadence tick — `governor.eval_interval_webhook_s` (default `900`) while webhooks are healthy, `governor.eval_interval_s` otherwise | full: lists issues and PRs for every tracked repo | Authoritative. Rebuilds the actionable snapshot from scratch and **overwrites** every webhook patch. If the pass fails outright (every repo refused: rate limit, 429, 5xx) the previous snapshot — patches included — stays published, marked stale for every repo (`stale_repos`, repo card `countsStale`), and the next tick retries; a repo refused on its own is served from its last-good enumeration the same way. |
| **Webhook patch** | one delivery naming an issue/PR | one `GET` per item, debounced and coalesced | Upserts, moves or removes that single item in both the activity-monitor row and the governor's cached snapshot. Counted in `/api/status.webhookPatches`. |
| **Cached cycle** | webhook wake, review wake, continuous-agent wake | dirty-PR detail refreshes only | Runs the governor's decision pass (kicks, automerge, stage moves) from the patched snapshot without listing. `source=cached` in the wake log; counted in `/api/status.cachedCycles`. Before the first reconcile has produced a snapshot a wake enumerates instead. |

Cadence ticks reconcile; webhooks keep the picture current in between. The
priority order is accuracy of issue/PR state, then latency, then API thrift,
which fixes four rules for the patch path:

- **The payload is a hint, never state.** A delivery only names the item;
  the GET decides what the row says. A `closed` delivery whose re-read finds
  the item open leaves it open.
- **Upserts are monotonic.** A re-read whose `updated_at` is older than the
  copy already held (two deliveries' GETs landing out of order) is dropped.
  Each applied read is recorded in the snapshot's `item_patches` (fetch time,
  `updated_at`, head SHA, `X-GitHub-Delivery` id) for audit.
- **Closed/merged removes at once; reopened re-adds** — both after the
  confirming GET.
- **Irreversible actions are freshness-bounded.** Before an issue close or a
  PR merge planned from the snapshot (stage sync, close-on-merge, automerge
  sweep), the governor refuses outright while that repo's share is stale and
  otherwise re-reads the item once if its copy is older than 60 s
  (`DecisionFreshnessBound`), proceeding only if it is still open. Refused
  actions log `irreversible action deferred` and retry next pass.

Startup runs a full enumeration before the first decision pass; a cold cache
whose GitHub share failed is published stale, never as "0 items".
`/api/status.staleSince` (and per-repo `countsStaleSince`) carries the
stale-as-of time and the Overview shows a `stale · as of HH:MM` badge.
Raising `eval_interval_webhook_s` lowers list-call volume further at the cost
of a longer window before a missed delivery is corrected by reconciliation.

##### Reading from the ForgeState cache (`forgestate.enabled: true`)

With the per-repo state cache on (`forgestate:` block in `hive.yaml`,
default off; [#13373](https://github.com/hivecommons/hive/issues/13373)) the
governor work source stops listing GitHub on the cadence tick too
([#13375](https://github.com/hivecommons/hive/issues/13375)). The cache's own
reconcile loop (`forgestate.reconcile_interval_s`, default `900`, shortened
to one minute while a repo is stale or its drift is high) is the **only**
full list; every governor pass — cadence tick, webhook wake, review wake —
takes each repo's open issues and PRs from the cached snapshot and classifies
them exactly as before. What changes:

- **Startup gate.** No decision pass runs before every repo has completed
  its first successful reconcile. The loop logs `forgestate: waiting for the
  first reconcile before the first decision pass` once a minute while it
  waits and `first reconcile complete` when it is released. A repo whose
  cache is not ready in a later pass is served from its last-good
  enumeration and marked stale, like a transient GitHub refusal.
- **Stale is inherited.** A repo whose last reconcile failed keeps its
  last-good items and appears in `stale_repos` with `stale_since` set to the
  last good reconcile, so the same close/merge guard applies.
- **Freshness bound before a kick.** Every GitHub ref in an assembled kick
  goes through the same 60 s bound as close and merge: a copy younger than
  the bound is kicked as-is, an older one is confirmed with one targeted
  `GET` (dropped from the kick if it closed or merged meanwhile), and a ref
  in a stale repo is withheld (`kick ref withheld: snapshot not fresh enough
  to act on`). A kick whose refs all fail is not sent.
- **Single-item reads feed the cache.** The webhook patch `GET` and the
  freshness confirmation `GET` are observed into the cache (as a webhook
  patch carrying the `X-GitHub-Delivery` id, or as a read-through), and the
  hive's own label add/remove, issue close and PR merge are written through
  immediately, so the next pass sees them without waiting for a webhook.
- **`item_patches` covers every cached item.** With the cache, the pass's
  `generated_at` says nothing about an item's age, so the fetch time of
  every item served from the cache is recorded there; the closed attributed
  PR history scan is reused for one reconcile interval rather than re-run
  on every wake.
- **Automerge lanes read the same cache.** The self-authored, trusted-author
  and contributor-lane sweeps take their open-PR set from the cached
  snapshot and run event-driven with a 5-minute floor pass; see
  `hive-merge.md` → "Event-driven sweeps on the ForgeState cache"
  ([#13377](https://github.com/hivecommons/hive/issues/13377)).

Repos added to the config after boot list live until the next restart
rebuilds the store with them. Enabling the cache changes no classification:
only the two list calls per repo move behind it.

##### Rate-limit pacing priority

After a GitHub rate-limit refusal (a secondary-limit 403 or a primary-limit
reset) the shared transport enters a slow-start caution window and paces
requests about 2s apart. Pacing is prioritised so periodic housekeeping cannot
starve the governor:

- the reconcile pass (`EnumerateActionable`) runs **high** priority and is
  bounded only by its own deadline;
- ordinary requests may queue at most 30s ahead;
- housekeeping (self-authored and queued automerge sweeps, the review-request
  watcher, advisory digest lookups, the dashboard metrics and fleet-stats
  collectors) runs **low** priority: a request that would queue more than 5s
  ahead is refused immediately (`github pacing: request shed`) instead of
  taking a slot, and a tick whose first slot is already further out than the
  tick could wait is skipped outright (`low-priority work shed`, logged at
  most once a minute per caller) and retried on its next tick;
- the self-authored automerge sweep backs its tick off to at least 60s while
  the window is engaged and resumes its adaptive interval when it ends;
- GitHub App installation-token mints (and the other App-JWT endpoints) do
  not go through the pacer at all: they are a separate rate-limit bucket, and
  a mint that queued behind paced traffic failed every request that needed
  the token.

##### Event-driven readers (ForgeState phase 2d)

- **The review-request watcher runs on events.** The relay that submits
  `hive-review` requests (`review_request_watcher.go`) no longer polls its
  drop box every 10 s. A request file drop wakes it through a directory
  notification, and `issue_comment`, `pull_request_review`,
  `pull_request_review_comment` and `issues` `labeled`/`unlabeled`
  deliveries wake it for a pass (`/api/status.webhooks` deliveries report
  `woke_review_watcher`). While webhooks are healthy the unprompted scan is a
  five-minute reconcile (`ReviewRequestWebhookFallbackInterval`); when
  webhooks go stale, or the drop box cannot be watched, the watcher returns
  to the legacy 10 s poll. A `--revise` request for a repo outside
  `review.revise_repos` is a policy denial (`.denied`, one WARN), not a
  retried failure.
- **The advisory issue is resolved from the ForgeState cache.** When a
  `forgestate` store is bound to the GitHub client (`forgestate.enabled`,
  hivecommons/hive#13373), `EnsureAdvisoryIssue` answers from the cache: the
  memoized advisory issue is read through the cache within its freshness
  bound (zero GitHub calls on a healthy cycle), a stale or missing cached
  copy costs one targeted `GET` of that issue, and a closed advisory issue
  is reopened in place and written through. The search → label list →
  title-scan path runs only when the cache cannot answer (repo not yet
  reconciled, or no labelled advisory issue to confirm).

`/api/status` and `/api/gh-rate-limits` expose
`webhooks: {healthy, last_event_at, events_1h, invalidations_1h,
targeted_refreshes_1h, tracker_invalidations_1h, mode, since, mode_reason,
healthy_windows}` (the middle two count the single-item re-reads and the
tracker cache drops webhooks caused; `mode` is `webhook` | `polling` and
`since` when it began — see [webhooks.md](../webhooks.md));
`/api/status` adds `evalMode` (`webhook` | `poll`), `evalIntervalS`,
`lastWakeReason`, `lastWakeAt` and `webhookWakes`;
`graphql_pr_batch.webhook_skips` counts batch queries skipped because nothing
changed.

```yaml
governor:
  eval_interval_s: 300
  eval_interval_webhook_s: 900
  webhook_wake_debounce_s: 10
  webhook_wake_min_gap_s: 30
dashboard:
  stage_tracker:
    webhook_refresh: true          # re-read the PR/issue a delivery names
    webhook_refresh_debounce_ms: 2000
    webhook_refresh_replay_window_s: 1800   # re-apply re-reads to snapshots published meanwhile
    closed_sweep_interval_s: 300   # list items closed since the last look and evict rows still shown; -1 disables
    waiting_push_stale_h: 4        # hours without a push on a CI / Review row before the pill's age turns muted-amber
    toast_info_s: 15               # how long the tracker's result / waiting-on-you toasts stay (3-120 s); the merge banner stays twice as long
    toast_error_s: 30              # how long a failed row action's toast stays (3-120 s); editable from the Activity Monitor's ⚙ → Toasts & timing
    merged_hold_s: 8               # ceiling on the "Merging · merge landed" bridge; celebration fires on the merged fact (min 3)
```

## `HIVE_GITHUB_TOKEN` permissions

`github.token: ${HIVE_GITHUB_TOKEN}` creates the main GitHub client when a GitHub
App is not configured. The code reads issues, PRs, commits, contents, checks,
commit statuses, contributors, and search results; it can also create/update
issues and comments, add/remove/create labels, open PRs, approve PRs for the
merge queue, and squash-merge queued PRs.

Minimum practical PAT permissions for full PAT-authenticated operation:

- **Classic PAT:** `repo` for private repositories (`public_repo` is enough only
  when every managed repo is public and no private org data is needed).
- **Fine-grained PAT:** select the managed repositories and grant:
  - Contents: read/write (branch pushes by agents and content reads)
  - Pull requests: read/write (list, open, approve, merge)
  - Issues: read/write (list, comments, labels, advisory issue)
  - Checks: read-only and Commit statuses: read-only (CI/merge gating)
  - Administration: read-only (branch-protection verification before merge requests)
  - Metadata: read-only (required by GitHub)

If you use a GitHub App, configure equivalent repository permissions on the App
installation instead of broadening the PAT. A `repo-moved` GitHub App verdict
means the App/key/installation are healthy, but the hive still names the old
repository owner after an org transfer; update the configured project org rather
than trying to add the moved repository back to the old installation.

### GitHub App permissions the hive needs, and what each one unblocks

The App's repository permissions decide which steps the hive can take itself.
A step the App cannot take is not a question for a person to *decide*; it is
a **capability gap** the hive records as a structured blocker
(`<!-- hive:blocker kind=capability:… -->`, see
[labels-and-control-signals.md](../labels-and-control-signals/glossary-and-operations.md#blockers-hiveblocker-marker)),
parks the item on, and surfaces at ⚙ → Security as *GitHub App permissions:
missing `<permission>` · N items blocked* ([#15099](https://github.com/hivecommons/hive/issues/15099)).

| Permission | Level | Without it the hive cannot… | Blocker kind |
|---|---|---|---|
| Contents | read/write | push branches, read files | `capability:repo-write` |
| Pull requests | read/write | open, approve, merge PRs | — (nothing starts) |
| Issues | read/write | comment, label, file issues | — (nothing starts) |
| Workflows | write | push any change under `.github/workflows/*` (GitHub refuses the push with *refusing to allow a GitHub App to create or update workflow*) | `capability:workflows-permission` |
| Actions | write | approve workflow runs from forks (the 085 *Awaiting human: approve workflow runs* pill) | — (`hive:unauthorized`) |
| Checks / Commit statuses | read | gate merges on CI | — |
| Administration | read | verify branch protection before a merge request | `capability:branch-protection` when a rule the App cannot satisfy stands |
| Metadata | read | anything (required by GitHub) | — |

Granting a permission on an existing installation needs the installation
owner to accept the new permission request in the App settings page the
dashboard links to (`githubAppInstallURL`). The hive re-checks on its own on
the next stage pass; *Re-check permissions* on a blocked row
(`POST /api/repos/{owner}/{repo}/items/{number}/blocker/recheck`) probes the
installation now and releases every item blocked on that permission when it is
there. `GET /api/status` carries the same picture as
`capabilities: {missing: ["workflows"], blocked_items: N, by_permission: {workflows: N}}`.

#### Onboarding errors point to the Getting Started guide

Every setup problem the dashboard's **🧭 Guide** (top navbar; also *Getting
Started* in the sidebar) has a step for is classified server-side and rendered
with a tip — *New hive? Open the 🧭 Guide → step ‘Install the Forge App
(GitHub App)’ for the install URL and permissions checklist* — whose link opens
the guide at that step. The classification rides the status payload as
`onboardingIssues: [{kind, guideStep, message}]`, on system alerts as
`onboarding: true` + `guideStep`, and on the embedded `deepHealth` checks
(`github_auth`, `repo_target`) as `onboarding`/`guideStep`. Kinds:
`github-app-not-installed`, `github-app-wrong-installation`,
`github-app-permissions`, `github-app-credentials`, `github-base-url`
(→ step `github-app`), `repo-target`, `no-repos` (→ `repos`), `no-agents`
(→ `add-agent`), `proxy-token` (→ `welcome`). Error text is unchanged; the tip
is appended. While any onboarding issue is active the Guide chip stays visible
even if the checklist was dismissed earlier. The guide's GitHub App step shows
the concrete install URL for the configured forge host (`githubAppInstallURL`,
GHES-aware; on an enterprise host with no `github_app_slug` it says what to
configure instead of linking to github.com). The guide, its entry points and
the ⚙️ Governor Config gear require a non-`read` role from `GET /api/role` —
a read-only viewer sees none of them and must be granted access via Manage
Access.

## Runtime diagnostics (`debug`, `GOMEMLIMIT`, `mem:` log line)

Added for [#14681](https://github.com/hivecommons/hive/issues/14681) (hive
process RSS growing hundreds of MiB/min with no pprof to name the culprit).

| Name | Source | Default | Purpose |
|---|---|---|---|
| `debug.pprof_addr` | `hive.yaml` | `127.0.0.1:6060` | `net/http/pprof` listener. **Loopback only**: a non-loopback host (`0.0.0.0`, pod IP) is rejected at startup with a warning and the default is used instead. `off` disables the listener. Profiles are under `/debug/pprof/`; `/debug/runtime` prints the same counters as the `mem:` log line. |
| `GOMEMLIMIT` | env | 85% of the cgroup memory limit | Go soft memory limit. When the env var is unset the hive reads `/sys/fs/cgroup/memory.max` (v2) or `memory/memory.limit_in_bytes` (v1) and sets `MemoryLimitPercent` (85%) of it via `debug.SetMemoryLimit`; an unlimited cgroup leaves the runtime default. Set the env var to override. Logged at startup as `runtime: memory limit` with `source=env|cgroup|none`. |
| `mem: runtime snapshot` | log (INFO, every 60 s) | always on | `heap_inuse`, `heap_live`, `heap_objects`, `total_sys`, `next_gc`, `gc_cycles`, `goroutines`, `threads` (OS threads from `/proc/self/status`), `gomemlimit`. Read from `runtime/metrics`, so no stop-the-world. |

The same snapshot is embedded in `GET /api/health` as `runtime` so the
dashboard health probe and `kubectl exec … curl` see goroutine count and heap
without the pprof port. Taking profiles on a pod:

```bash
kubectl -n <ns> port-forward deploy/hive 6060:6060 &
go tool pprof -top  http://127.0.0.1:6060/debug/pprof/heap          # inuse_space
curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head  # parked goroutines
```

## `cmd/hive` flags and environment

### Startup

| Name | Source | Purpose |
|---|---|---|
| `--config` | flag | Path to `hive.yaml`; default `/etc/hive/hive.yaml` unless `HIVE_CONFIG` is set. |
| `HIVE_CONFIG` | env | Sets the **default** of `--config`. An explicit `--config` outranks it — and the image ships one (`CMD ["--config", "/etc/hive/hive.yaml"]`), so `entrypoint.sh` appends `--config "$HIVE_CONFIG"` to the launch argv when the variable is set. Without that append the variable is inert in the container ([#4973](https://github.com/hivecommons/hive/issues/4973)). |
| `HIVE_MODE=hub` | env | Starts the hub server instead of a spoke dashboard. |
| `HIVE_HUB_PORT` | env | Hub listen port in hub mode; default `3001`. |
| `HIVE_SINGLETON_LOCK` | env | Internal/escape hatch for the process singleton lock path; value `off` disables the guard. |

### GitHub and dashboard auth

| Name | Purpose |
|---|---|
| `HIVE_GITHUB_TOKEN` | Main PAT fallback when `github.token` is empty; also used for token identity/fleet stats fallback. Wrong-scope tokens fail with generic 403s at request time — see [github-app-setup.md](../github-app-setup.md#personal-access-token-pat-scopes) for the required scopes per ACMM tier. |
| `GH_APP_KEY_FILE` | GitHub App private-key path fallback when `github.key_file` is empty. |
| `HIVE_DASHBOARD_TOKEN` | Shared dashboard/API token fallback for `dashboard.auth_token`. Any non-empty string is accepted with no strength check — generate one with `openssl rand -hex 32`; see [env-vars.md](../env-vars.md#generating-and-rotating-hive_dashboard_token). |

### Hosted-spoke and fleet metadata

| Name | Purpose |
|---|---|
| `HIVE_HUB_URL` | Hub URL override for heartbeat/spoke registration. |
| `HIVE_CLUSTER_ID` | Hosted cluster ID override. |
| `HIVE_ID` | Hive ID override. |
| `HIVE_LEVEL` | ACMM level bootstrap/override used by hosted flows. |
| `HIVE_COVERAGE_BADGE_URL` | Source of the coverage percentage on the ci-maintainer card: an `http(s)` badge URL (shields JSON or SVG) or `repo://<ref>/<path>` read from the primary repo via the App client. See [env-vars.md](../env-vars.md). |

### Dashboard metrics

See [Monitoring a hive with Prometheus and Grafana](../monitoring.md) for setup, metric families and scraping.

| Name | Purpose |
|---|---|
| `HIVE_METRICS_ENABLED` | Registers Prometheus `/metrics` when set to `1`, `true`, `yes`, or `on`; off by default (route not registered — scrapes 404) because it exposes estimated cost data. |
| `HIVE_METRICS_TOKEN` | **Required whenever metrics are enabled** ([#3804](https://github.com/hivecommons/hive/pull/3804)): scrapers must send `Authorization: Bearer <token>` (configure Prometheus `bearer_token`). Enabled-but-tokenless serves 403 with an error naming both variables, and the hive logs a startup warning; the cost/agent series are never served unauthenticated. See [ServiceMonitor setup](../monitoring.md#adding-the-monitoring-component-to-an-overlay) and [alert runbooks](../monitoring.md#alerts-and-runbooks) for configuring scraper access and on-call alerts. |

> **Note on `tokens_24h`:** despite its name, the per-spoke heartbeat field
> `tokens_24h` (stored on the hub as `totalTokens24h`) is a **cumulative total**,
> not a rolling 24-hour window. Read it as lifetime token consumption for the
> spoke. See [token-tracking.md](../token-tracking.md) for the full heartbeat and
> `/api/saas/usage` rollup details.

### Knowledge: Context7 content caps

| Name | Default | Purpose |
|---|---|---|
| `HIVE_CONTEXT7_MAX_SNIPPETS` | `40` | Snippets kept from one Context7 llms.txt fetch (document import or `context7` connector). |
| `HIVE_CONTEXT7_MAX_SNIPPET_BYTES` | `4096` | A single snippet larger than this is dropped whole. |
| `HIVE_CONTEXT7_MAX_TOTAL_BYTES` | `16384` | Total content kept per fetch, marker included; values under `1024` are raised to `1024`. |

Snippets are dropped whole (never clipped) by relevance to the topic, the
stored content ends with a `… truncated: N snippets / M bytes omitted (size
cap)` line, and the fetch logs the library. Unparseable or non-positive values
fall back to the default. The kick primer's own byte cap is the yaml key
`knowledge.primer.max_bytes` (default `4096`); see
[knowledge-curator.md](../knowledge-curator.md#kick-primer-size-cap-knowledgeprimermax_bytes).

### Inference endpoint fallbacks

| Name | Default | Purpose |
|---|---|---|
| `HIVE_VLLM_ENDPOINT` | unset | Comma-separated vLLM endpoint list; set only to a cluster-reachable endpoint. Unset disables the built-in vLLM gateway route. |
| `HIVE_LLMD_ENDPOINT` | `http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000` | Comma-separated llm-d endpoint list. |
