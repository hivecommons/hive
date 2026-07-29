## Why

At 42 hives and growing, the operator scans every row looking for problems. This inverts the default: **surface only what is WRONG**, so the normal state of the screen is "nothing needs you".

Real misses from one night — a hive crash-looping for hours, another stuck "upgrading" indefinitely, spokes with a broken sign-in config — none of which announced themselves.

## Alert rules and thresholds

Server-side evaluation in a **new file** (`v2/pkg/hub/alerts.go`) over data the hub already has on `RegistryEntry`. Every threshold is a named constant with a rationale comment.

| Type | Severity | Threshold | Constant |
|---|---|---|---|
| `crash-loop` | critical | 3+ distinct `StartedAt` observed within 2h **and** current uptime < 15m | `alertCrashLoopMinRestarts`, `alertCrashLoopWindow`, `alertCrashLoopUptimeCeiling` |
| `offline` | critical | no heartbeat for > 30m | `alertOfflineThreshold` |
| `provision-error` | critical | SaaS provisioning status is `error` | `alertProvStatusError` |
| `stuck-upgrade` | warning | `Upgrading` past the **existing** `staleUpgradeTimeout` (10m) | `staleUpgradeTimeout` (reused, not redefined) |
| `health-check-failing` | warning | a named check reports `fail`, with the check name in the reason | `healthCheckStatusFail` |
| `token-burn` | info | > 5x fleet median, **or** claimed+online at zero for > 24h | `alertTokenBurnMedianMultiple`, `alertTokenBurnMinFleetSize`, `alertTokenBurnMinMedian`, `alertZeroTokenMinAge` |

Design notes on the thresholds:

- **`offline` is deliberately well above `maxHeartbeatAge`** (which merely flips the `Online` flag). A single missed beat or a brief pod reschedule must not page anyone.
- **`crash-loop` requires 3, not 2.** Two distinct start times is what any *single* ordinary restart looks like.
- **`crash-loop` also requires the hive to still be unsettled.** A hive that flapped this morning and has been up for six hours has recovered and stops alerting.
- **`token-burn` has both a fleet-size floor and a median floor.** Without them, a fleet median of 1 token makes anything above 5 tokens "anomalous".

## Avoiding duplication with config-drift

`feat-config-drift` answers *"how does this hive's **configuration** diverge from the fleet norm"* (version behind, branch mismatch, ACMM 0, zero agents, App missing). This PR answers *"what needs a **human right now**"* — transient, time-bounded, operational conditions.

**No drift signal is reimplemented here.** Concretely, this PR does **not** compute version-behind, branch mismatch, ACMM 0, zero agents, or GitHub App missing.

The two are built to compose rather than compete:

```go
func evaluateAlerts(state *alertState, hives []alertHive, driftAlerts []Alert, now time.Time) AlertSummary
```

`driftAlerts` is the seam. Drift-sourced signals merge through the **same** severity ordering, FirstSeen tracking, acknowledgement and counting pipeline — they get all of it for free, and no rule in this file needs rewriting when drift lands. It is `nil` today, and there is a test (`TestEvaluateAlerts_DriftAlertsMergeThroughSamePipeline`) pinning that contract, including that a merged drift alert is acknowledgeable like any other.

**Acknowledged overlap:** `offline` is a genuine overlap — drift lists "stale heartbeat" among its signals. Per the brief I **emit** the alert rather than silently dropping it, and flag it here. If drift also emits it, the merge is idempotent by `(hiveID, type)` at the ack layer, but we should pick one owner before both land. My suggestion: alerts owns `offline` (it is an operational state with a time threshold), drift keeps the configuration signals.

## Acknowledgement model

Admin-only (`POST /api/saas/admin/alert-ack`, behind `requireAdmin`), persisted to the PVC at `/data/saas/hub-alert-acks.json` with the atomic tmp+rename pattern copied from hub banners.

The interesting part is **how an ack stops being permanent**, via two independent mechanisms:

1. **Clears when the condition resolves.** Each ack stores the `ConditionFirstSeen` of the instance it silenced. `FirstSeen` is forgotten whenever a condition stops firing, so a re-occurrence carries a *different* `FirstSeen` and the stored ack no longer matches. Silencing one outage never silences the next one.
2. **Expires regardless.** `alertAckMaxAge` = 7 days, so an alert silenced last week resurfaces even if the condition never cleared. A permanent mute is how a real problem gets forgotten.

`ConditionFirstSeen` is also persisted and restored on load — without that, an ordinary hub upgrade would mint a fresh `FirstSeen` and silently void every acknowledgement.

Acking a condition that is **not currently live** is refused with `409`, so a client cannot pre-silence an alert that has not fired.

An acknowledged alert is still returned and rendered (muted, behind a "N acknowledged" toggle) but excluded from `Total` and the severity/type counts — it stops dominating the view without disappearing.

## UI

"Attention needed" panel above My Hives:

- **Clean fleet** → collapses to one quiet line (`✓ Nothing needs your attention`) rather than vanishing, so the operator can see the check actually ran.
- **Dirty fleet** → severity pills, and type chips that drill the hive list down to the affected hives.
- Composes with the existing status chips as an **AND** (chips narrow by state, the alert filter narrows to hives carrying that alert) and respects the existing `isPlaceholderHive()` assigned/unassigned split.
- The alert state joins `renderHives`' signature string — without that, toggling a chip while hive data is unchanged would be a silent no-op (the same trap the existing status filters document).
- The empty-state escape hatch now calls `clearAllHiveFilters()`, since an alert drill-down can empty the list too and a button that only cleared the status chips would strand the operator.

## Placeholder scoping

`isPlaceholderEntry()` is a Go mirror of the dashboard's `isPlaceholderHive()` — `provStatus === 'available'` authoritative, `available-` org prefix as fallback — so server and client can never disagree about what counts as a placeholder.

Unassigned placeholders are **excluded** from rules that only apply to claimed hives (token burn / zero tokens), but **still alert** on genuinely broken operational states (`offline`, `provision-error`). A pool slot that has stopped heartbeating is the admin's problem; one that is merely idle is not. Both directions are tested.

## Conventions

- New file for the logic; hooks into existing code are **3 lines in `server.go`** (struct field, constructor, loader) and **2 in `saas.go`** (one route, one payload key) — kept tight for the concurrent `saas.go` edits.
- Dashboard changes are inside the `dashboardHTML` raw-string const, not `static/index.html`.
- `Health` is `map[string]any` from the wire and is parsed **fully defensively**: two-value type assertions throughout, nil-guarded iteration, and **both** real shapes handled — the heartbeat's `checks[]` array (built by `HealthSummary()`) and the map-of-name→object shape `/api/health/deep` builds. Names are sorted so the reason string does not churn between polls (Go map order is randomised).
- `alertState` carries its **own leaf mutex**, taken and released within a single function and never held while acquiring `s.mu` — per the startup-deadlock incident. The evaluator runs entirely off the `s.mu` critical path on an already-copied snapshot.
- Every user/spoke-controlled string is escaped with `esc()` on render (hive names, reasons embedding check names and provisioning error text, acking usernames).
- No literal `<body>` in any comment or string.
- `AckAt` is a **pointer** because `omitempty` does not elide a zero `time.Time` — as a value it serialised a bogus `0001-01-01T00:00:00Z` onto every unacknowledged alert. Caught by an actual payload inspection, and pinned by a test.

## Verified vs assumed

**Verified:**
- `go build ./...`, `go vet ./pkg/hub/`, `go test -timeout 480s ./pkg/hub/` — all pass. Full package suite green (~87s).
- **95.9% statement coverage** on `alerts.go`.
- The dashboard JS **parses** — extracted every `<script>` block from `dashboardHTML` and ran `node --check` on it, before and after the edits.
- The **actual serialised payload**, by marshalling `fleetAlerts()` output and asserting on real key names — this is what caught the `ackAt` zero-time bug.
- The **ack endpoint end-to-end** through `httptest`: success, persistence to disk, and every error path (unknown type → 400, missing fields → 400, malformed body → 400, non-live condition → 409, clear → 200).
- The **`Health.checks` shape**, read from `HealthSummary()` in `pkg/dashboard/server.go` and cross-checked against how the existing `healthBadge()` JS already consumes it. The brief described `checks[]` as an array; I confirmed that is right for the heartbeat, and additionally found the *different* map shape in `handleHealthDeep` (a different endpoint, not what the hub receives) — both are handled rather than assuming one.
- **DCO signed**, no `Co-Authored-By`.
- Rebased on latest `origin/v2` at push time.

**Assumed / not verified:**
- **Not rendered in a browser.** The panel's visual layout, the collapsed clean state, and the chip interactions are verified only by JS syntax check and by reading the surrounding code — not by loading the hub dashboard.
- **Threshold values are judgement calls**, not tuned against production data. 30m offline, 3 restarts / 2h, 5x median are defensible starting points; they may need adjustment once seen against the real 42-hive fleet. All are one-line constant edits.
- **`_isAdmin` timing**: the ack buttons render behind the existing `_isAdmin` flag, following the same pattern as other admin-gated UI in this file. The server is the real gate (`requireAdmin`), so a stale client flag is cosmetic, not a security issue.
- **The `offline` overlap with config-drift** is flagged above but not resolved — that needs a decision once both branches are up.
- Expect to rebase; `saas.go` has six concurrent editors.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
