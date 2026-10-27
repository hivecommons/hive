# GitHub webhooks: cadence, patch and reconcile

This page is the operator's model of how a hive keeps its picture of GitHub
current, and what each of the moving parts costs. It is the Phase 4 document
of the ForgeState epic
([#13373](https://github.com/hivecommons/hive/issues/13373),
[#13381](https://github.com/hivecommons/hive/issues/13381)). The receiver,
the App/hub set-up and what each delivery invalidates are in
[Webhook-driven PR cache invalidation](operator-reference.md#webhook-driven-pr-cache-invalidation);
the dashboard side is in [dashboard.md](dashboard.md#hive-activity-monitor).

The priority order throughout is **accuracy → latency → API thrift**. A
delivery is a free hint; every outbound GitHub call is budgeted. The rules
below exist so that the hive never trades the first for the third.

## Polling cadence vs webhook patch vs reconcile

Three writers feed one snapshot per repository; everything else reads it.

| Writer | Trigger | GitHub cost | Authority |
| --- | --- | --- | --- |
| **Reconcile** | the cadence tick: `governor.eval_interval_webhook_s` (default `900`) in webhook mode, `governor.eval_interval_s` (default `300`) in polling mode; the `forgestate` cache additionally runs its own `reconcile_interval_s` (default `900`) per repo | full list of open issues and PRs per repo, ETag-conditional where supported | **Authoritative.** Rebuilds the snapshot and overwrites every patch. A failed pass keeps the last-good snapshot and marks it `stale`; it never publishes an empty one. |
| **Webhook patch** | one verified delivery naming an issue or PR | one `GET` of that item (1–3 calls with labels/head), debounced and coalesced per item | **Hint + confirming fetch.** The payload only names the item; the re-read decides what the row says. Upserts are monotonic by `updated_at` / head SHA, so out-of-order deliveries cannot roll state backwards. Closed/merged removes at once; reopened re-adds. The `X-GitHub-Delivery` id is recorded for audit. |
| **Write-through** | the hive's own mutation (stage label, comment, merge, close, claim) | none beyond the mutation itself | **Immediate.** The cache reflects the mutation before the next read, without a confirming call. |

Where they live:

- Reconcile — the governor's `EnumerateActionable` pass (`src/pkg/github/client.go:1430` records the newest `updated_at` it listed, see [fallback](#fallback-when-webhooks-degrade)) and the ForgeState `Reconcile` writer (`src/pkg/forgestate/reconcile.go:18`), whose per-repo `interval` shortens while drift is high (`src/pkg/forgestate/reconcile.go:18`).
- Webhook patch — the legacy snapshot's `ApplyWebhookItem` (`src/pkg/github/webhook_item_patch.go:291`) fed by `FetchWebhookItemSnapshot` (`src/pkg/github/webhook_item_refresh.go:75`), and ForgeState's `ApplyWebhook` (`src/pkg/forgestate/webhook.go:38`) with its `supersedes` monotonic check (`src/pkg/forgestate/store.go:400`).
- Write-through — ForgeState's `WriteThrough` (`src/pkg/forgestate/writethrough.go:38`).

Between reconciles the picture is kept current by patches; a cadence tick
in webhook mode is therefore a slow safety net, not the source of latency.
Wake cycles (webhook, review, continuous-agent) plan from the patched
snapshot and never re-list; see the table in
[operator-reference.md](operator-reference.md#polling-cadence-vs-webhook-patch-vs-reconcile).

## Accuracy guarantees

These are the acceptance criteria of #13373 and the tests that hold them.

| Guarantee | How | Where |
| --- | --- | --- |
| **Closed on GitHub → gone within ≤ 10 s** | a `closed`/`merged` delivery triggers the confirming `GET`; the re-read removes the item from the activity monitor and the governor's snapshot; the debounce is `dashboard.stage_tracker.webhook_refresh_debounce_ms` (default `2000`); the re-read is remembered for `dashboard.stage_tracker.webhook_refresh_replay_window_s` (default `1800`) and re-applied to every status snapshot published meanwhile, so an eval cycle that enumerated the item open before the close cannot bring the row back | `src/pkg/forgestate/webhook.go:38`, `src/pkg/github/webhook_item_patch.go:291` |
| **Closed in the payload → gone on that delivery, read or no read** | every `issues`, `issue_comment` and `pull_request*` payload embeds the item with its `state`; a closed or merged state evicts the activity-monitor row and the governor's copy at once on *any* action (a `labeled`/`unlabeled`/`created` for an already-closed item says closed too), before and independently of the confirming `GET`, which a critical budget or the rate-limit caution window may refuse. Open-side fields (title, labels, bucket) still come only from the `GET`. Backstop: on each status publish, at most every `dashboard.stage_tracker.closed_sweep_interval_s` (default `300`) one `state=closed&since=` list per repo evicts rows GitHub reports closed (hivecommons/hive#14159, #14064) | `src/pkg/dashboard/webhook_tracker_refresh.go`, `src/pkg/dashboard/stage_tracker_closed_sweep.go`, `src/pkg/github/closed_since.go` |
| **Out-of-order deliveries are safe** | upserts compare `updated_at` (and head SHA for PRs); an older read never replaces a newer copy; a removal leaves a tombstone so a late "open" read cannot resurrect it | `src/pkg/forgestate/store.go:400` |
| **403 mid-reconcile keeps last-good + `stale`** | a refused reconcile marks the repo `stale` and keeps the previous snapshot, patches included; counts are never published as zero | `src/pkg/forgestate/reconcile.go:146`, `src/pkg/github/enumerate_lastgood.go:39` |
| **Own mutation visible before the next read** | write-through | `src/pkg/forgestate/writethrough.go:38` |
| **Freshness bound before irreversible actions** | merge, close, claim and kick re-read an item older than `DecisionFreshnessBound` (`60s`) and refuse outright while the repo's share is stale (`irreversible action deferred`) | `src/pkg/github/decision_freshness.go:15`, `src/pkg/forgestate/store.go:334` |
| **Startup gate** | the first governor decision pass waits for a full reconcile of every repo (`WaitReady`); a cold cache whose fetch failed is published stale, never empty | `src/pkg/forgestate/store.go:320` |
| **Drift detection** | each reconcile diffs cache vs API into `forgestate_drift`; above `drift_threshold` the repo's reconcile interval shortens (the dashboard warning of #13373 follows once a reader wires the metric) | `src/pkg/forgestate/reconcile.go:58`, `src/pkg/forgestate/metrics.go:13` |
| **Provenance** | tracker rows carry `fetched_at` and source (`webhook` / `reconcile`); the overview shows `stale · as of HH:MM` instead of zeros | [operator-reference.md](operator-reference.md#polling-cadence-vs-webhook-patch-vs-reconcile) |

## Configuration and defaults

| Key | Default | Meaning |
| --- | --- | --- |
| `governor.eval_interval_s` | `300` | legacy polling cadence; the base tick in polling mode |
| `governor.eval_interval_webhook_s` | `900` | the base tick in webhook mode; `0` disables webhook mode (always poll) |
| `governor.eval_interval_max_s` | `1800` | cap on either tick after the API-budget stretch |
| `governor.webhook_wake_debounce_s` | `10` | a delivery and its check fan-out coalesce into one wake cycle |
| `governor.webhook_wake_min_gap_s` | `30` | minimum spacing between webhook-triggered cycles, and between the cycles recorded review verdicts trigger |
| `forgestate.reconcile_interval_s` | `900` | ForgeState's own full-list interval per repo, shortened while drift is high |
| `forgestate.freshness_bound_s` | `60` | maximum item age before merge/close/claim/kick re-reads it |
| `forgestate.drift_threshold` | `0.05` | fraction of items a reconcile may find wrong before the interval shortens |
| `dashboard.stage_tracker.webhook_refresh_debounce_ms` | `2000` | debounce of the single-item re-read a delivery queues |
| `dashboard.stage_tracker.webhook_refresh_replay_window_s` | `1800` | how long a re-read is remembered and re-applied to every status snapshot the governor publishes, so a cycle that enumerated before the close cannot re-list the item |
| `dashboard.stage_tracker.closed_sweep_interval_s` | `300` | how often (driven by status publishes) each repo's items closed since the last look are listed (one call per repo) and rows still shown are evicted; negative disables |
| health window | `2 × governor.eval_interval_s` (10 min at the defaults) | how recent the newest verified delivery must be for webhooks to count as healthy |
| recovery hysteresis | `WebhookRecoveryHealthyWindows` = `2` | consecutive healthy windows required before the cadence returns to webhook mode after a degradation |

Defaults: `src/pkg/config/config.go:7192` (`DefaultEvalIntervalWebhookS`,
`DefaultWebhookWakeDebounceS`, `DefaultWebhookWakeMinGapS`),
`src/pkg/config/forgestate.go:8` (`DefaultForgeStateReconcileInterval`,
`DefaultForgeStateFreshnessBound`, `DefaultForgeStateDriftThreshold`),
`src/pkg/github/webhook_cache.go:14` (`DefaultWebhookHealthWindow`, replaced
by `SetWebhookHealthWindow` at `src/pkg/github/webhook_cache.go:102` once the
governor loop publishes 2 × `eval_interval_s`), and
`src/pkg/github/webhook_mode.go:34` (`WebhookRecoveryHealthyWindows`). The
effective tick is composed in `webhookAwareEvalInterval`
(`src/cmd/hive/api_budget_decision.go:82`): the API-budget stretch still
applies and the larger interval wins.

```yaml
governor:
  eval_interval_s: 300
  eval_interval_webhook_s: 900
  eval_interval_max_s: 1800
  webhook_wake_debounce_s: 10
  webhook_wake_min_gap_s: 30
forgestate:
  enabled: false
  reconcile_interval_s: 900
  freshness_bound_s: 60
  drift_threshold: 0.05
```

## Fallback when webhooks degrade

Webhook mode is only safe while deliveries actually arrive. The governor
therefore keeps a small cadence **mode** machine on top of the raw health
window (`src/pkg/github/webhook_mode.go:100`, `observe`) and recomputes it
wherever it recomputes the eval interval
(`evalIntervalAndModeForConfig`, `src/cmd/hive/api_budget_decision.go:115`),
including the re-check timer that fires as the health window closes
(`webhookHealthRecheckDelay`, `src/cmd/hive/webhook_wake.go:238`).

States and transitions:

- **polling** (boot) — no verified delivery yet. Base tick =
  `eval_interval_s`. The **first** healthy window engages webhook mode at
  once; there is no hysteresis before webhooks have ever degraded.
- **webhook** — the base tick is `eval_interval_webhook_s`. The mode
  **degrades** to polling when either:
  - the health window lapsed **and** GitHub was active meanwhile: a cadence
    pass listed an item whose `updated_at` is newer than the newest delivery
    (plus a 30 s clock-skew allowance, `webhookActivityClockSkew`,
    `src/pkg/github/webhook_mode.go:40`). The reconcile pass reports that
    newest `updated_at` through `RecordGitHubActivity`
    (`src/pkg/github/webhook_mode.go:183`). Silence alone — a quiet
    repository overnight — is **not** degradation and keeps the stretched
    tick; or
  - deliveries are failing: the receiver saw a delivery (direct or relayed
    by the hub) whose signature did not verify or whose payload did not
    decode, newer than the last good delivery and inside the window
    (`RecordWebhookDeliveryFailure`, `src/pkg/github/webhook_mode.go:199`,
    called from `src/pkg/dashboard/api_github_webhook.go:58`). A wrong
    `GITHUB_WEBHOOK_SECRET` on a spoke therefore shows up as polling mode
    with `mode_reason: deliveries failing verification`, not as a silent
    stretch to 900 s.
- **recovering** (polling after a degradation) — each observation with a
  healthy window and no newer failure extends a streak;
  `webhooks.healthy_windows` counts it (1 at the first healthy observation,
  +1 per further window of uninterrupted health). A lapse or a failure
  resets the streak to zero.
- back to **webhook** once the streak reaches
  `WebhookRecoveryHealthyWindows` (`2`): at the defaults, one delivery
  followed by ten more minutes of deliveries.

The transition is logged exactly once each way, at INFO:
`webhooks degraded; polling cadence restored` (with `reason`,
`effective_interval_s`, `recovery_healthy_windows`) and
`webhooks healthy; webhook cadence engaged`. The governor's ticker is reset
on the same observation (`eval interval changed, resetting ticker`), so the
polling cadence is in force from the next tick, not after the stretched one
would have fired.

The dashboard reads the same machine:

- `/api/status.webhooks.mode` — `webhook` | `polling`;
  `/api/status.webhooks.since` — when the current mode began;
  `mode_reason` while polling after a degradation; `healthy_windows` while
  recovering (`src/pkg/github/webhook_cache.go:307`, `snapshot`).
  `webhooks.healthy` stays the raw window (a delivery inside
  2 × `eval_interval_s`), so `healthy: false, mode: webhook` reads "quiet
  but trusted" and `healthy: true, mode: polling` reads "recovering".
- `/api/status.evalMode` (`webhook` | `poll`) and `evalIntervalS` are the
  loop's view of the same decision, and the governor card's next-kick
  estimate uses it (`src/pkg/dashboard/status_builder.go:2058`).

What stays on the raw window: the per-repo PR-detail cache TTL extension
and the "skip the repo's re-enrichment" shortcut still require a delivery
inside the window for that repository, so a quiet-but-trusted repo pays
its normal detail refreshes — the fallback only decides the *cadence*.

Operator checks:

1. `curl -s $SPOKE/api/status?fields=webhooks | jq .webhooks` — expect
   `mode: webhook` on a busy repo with the App webhook pointed at the hub
   or the spoke; `polling` with a `mode_reason` is a configuration fault to
   fix (secret, App subscription, hub relay target), `polling` without one
   means no delivery has ever arrived.
2. `grep 'webhooks degraded' hive.log` — one line per outage with the
   reason; the matching `webhooks healthy; webhook cadence engaged` closes
   it.
3. A degradation shortens the cadence back to `eval_interval_s`, so API
   spend returns to the pre-webhook level while it lasts; the budget
   stretch (`conserve`/`critical`) still applies on top.
