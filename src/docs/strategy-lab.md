# Experiment Lab (Nous) operator guide

The Experiment Lab (formerly the Strategy Lab) — internally and in the
API called **Nous** — is a controlled experiment framework for your
repositories. It records a baseline of hive behavior, designs experiments
against repo code (tests, CI config, automation scripts), and measures each
one against that baseline before anything is applied for real.

**The lab is repo-only.** Experiments on the hive's own runtime configuration
(cadences, model assignments, thresholds) are out of scope and are not offered
by the lab's UI or API.

**There is no `nous:` block in `hive.yaml` or `hive.yaml.example`.** Despite
what `src/README.md` used to say, Nous has no YAML config surface — it is
configured entirely at runtime through the dashboard (Experiment Lab panel, ACMM
level 4+) or directly through the `/api/nous/*` endpoints. This page documents
that runtime configuration and the experiment lifecycle, verified against
`pkg/dashboard/api.go`, `pkg/dashboard/deps.go`, `cmd/hive/main.go`, and the
dashboard's `renderNousConfig()`/`renderNous()` UI code.

## Visibility

The Experiment Lab section of the dashboard only appears at **ACMM level 4 and
above** and when `dashboard.experiment_lab: true` is set (the pre-rename
`dashboard.strategy_lab` key is a deprecated alias for one release) (`ACMM_NOUS_MIN_LEVEL` in `pkg/dashboard/static/index.html`). Below
that level the panel and sidebar summary are hidden entirely, though the
`/api/nous/*` endpoints still respond. On a hive with no snapshots yet, the
panel shows an explainer card instead of empty stats.

## State and storage

Nous state lives in three places, none of which is `hive.yaml`:

| What | Where | Written by |
|---|---|---|
| Experiment ledger | `/var/run/nous/governor/ledger.json` | experiment loop, read at boot (the path is historical; it holds repo experiments) |
| Learned principles | `/var/run/nous/governor/principles.json` | experiment loop, read at boot |
| Hive state snapshots | `/data/nous/snapshots/*.json` | one file per evaluation cycle, via `NousState.RecordSnapshot` → `pkg/hivestate`; see [Hive state time series](#hive-state-time-series) |
| Mode, goals, output mode, repo targets, fast-fail bounds, schedule | in-memory `NousState.Config`, set via `PUT /api/nous/config/*` | dashboard config dialog or direct API calls |

The in-memory `Config` map (goals/repos/output/fast_fail/schedule/
principles sections) is **not currently persisted back to disk
across a process restart** — it is rebuilt from defaults each time the hive
boots, while `loadNousState()` in `cmd/hive/main.go` reseeds only the ledger,
principles, and snapshot count from the files above. Re-apply dashboard
configuration after a restart if you rely on non-default values.

## Hive state time series

The per-cycle snapshots are recorded on every governor evaluation cycle
regardless of Nous mode or scope, and are read by `pkg/hivestate` as a
general hive-state time series ([#12339](https://github.com/hivecommons/hive/issues/12339)):

- **Retention.** Raw `<epoch-ms>.json` snapshots are kept for 24 h; older ones
  are rolled into hourly `hour-<epoch-ms>.json` buckets (every queue-depth
  sample of the hour is kept, so quantiles stay exact), and anything older
  than 30 days is deleted. Compaction runs at most once an hour from the
  snapshot write path, so the directory is bounded (~720 raw files plus at
  most 720 hourly buckets at the default 120 s cadence).
- **Trends.** `hivestate.Store.Trends()` summarizes the last 24 h and 7 d:
  median and p90 of `queue_issues`, `queue_prs` and `queue_hold`;
  `sla_violation_rate` (fraction of samples with at least one SLA violation);
  `agent_idle_ratio` (idle ÷ idle + running agent-samples; stopped and paused
  agents are excluded); `kicks_per_day`; `tokens_per_day` (sum of positive
  `total_tokens` deltas); plus `samples` and `coverage_hours`. Per-day rates
  are scaled by the hours the samples actually cover and stay `0` below one
  hour. The summary is cached for five minutes.
- **Consumers.** The trends ride on fleet self-reports as `hive_state` (see
  [Fleet self-reporting](fleet-report.md)) and on the hub heartbeat's
  `evidence.hive_state` (see [Evidence summary heartbeat](fleet-health.md#evidence-summary-heartbeat-12114)).

Because compaction turns many raw files into one bucket, the dashboard's
`snapshots` / `baseline_pct` count is a file count, not a sample count.

## Experiment lifecycle

1. **Collecting** — a fresh hive starts in this phase. Every evaluation cycle
   writes a baseline snapshot
   (`/data/nous/snapshots/<epoch-ms>.json`) with queue depth, MTTR, SLA
   violations, agents kicked, and token totals. The `NousBaselineTarget`
   constant (672 snapshots) is treated as "enough baseline" by the dashboard
   progress bar and the `baseline_pct` status field.
2. **Observing** — once at least one snapshot exists, phase auto-advances to
   `observing` (`refreshStatus()` in `pkg/dashboard/deps.go`).
3. **Framing → Design → Design Review → Executing → Analysis → Findings
   Review → Extraction** — the full phase sequence
   (`NOUS_PHASES` in the dashboard JS) that a live experiment iteration walks
   through. Phase and iteration are reported through `GET /api/nous/phase` and
   the `phases.repo` block of `GET /api/nous/status`. Any other scope block the
   experiment loop publishes into `NousState.Status` is dropped from the
   response.
4. **Gate decision** — a phase can pause for an operator decision. The
   dashboard surfaces this via `GET /api/nous/gate-pending`; an operator (or
   an owner-role automation) responds with
   `PUT /api/nous/gate-decision {"decision": "...", "reason": "..."}`. The
   decision is recorded in `NousState.GateResponse` and audited as
   `nous_gate_decision`. `POST /api/nous/gate-respond` accepts a free-form
   body for the same purpose and is audited as `nous_gate_respond`.
5. **Approve / Reject / Abort** — while in **Suggest** mode, a pending
   proposal (`NousState.Status["pending"]`) shows Apply/Reject buttons in the
   dashboard, backed by `POST /api/nous/approve` and `POST /api/nous/abort`.
   A running experiment can be stopped early with the same Abort action,
   which withdraws the proposed repo change.

## Trust ladder (mode)

Set with `PUT /api/nous/mode {"mode": "observe|suggest|evolve"}` (owner role
required). The dashboard's trust-ladder widget documents the three modes:

| Mode | Behavior |
|---|---|
| `observe` | Data collection only — nothing is proposed or changed. |
| `suggest` | Nous designs experiments but every proposal requires operator approval via `nous_approve`/`nous_abort` before it runs. |
| `evolve` | Nous runs approved-pattern experiments autonomously and distills durable results into principles without per-experiment approval. |

## Scope

The lab is **repo-only**: experiments propose repo code changes (tests, CI
config, automation scripts) and always require approval regardless of trust
level. There is no scope selector in the dashboard, and `GET /api/nous/status`
always reports `"scope": "repo"`.

`PUT /api/nous/scope` is kept only for backward compatibility (owner role
required). `{"scope": "repo"}` returns 200; any other value — including the
former `governor` and `both` — returns `400 scope must be "repo": the
Experiment Lab is repo-only` and leaves state untouched.

## Configuring goals, output, and bounds

`GET /api/nous/config` returns the current in-memory config as JSON
(`campaign`, `repoCampaign`, `output`, `projectRepos`, `repoTargets`,
`principles`). Each section is written independently with its own `PUT`
endpoint, all requiring owner role and all triggering `refreshAndPersist()`:

| Endpoint | Section | Fields (dashboard defaults shown) |
|---|---|---|
| `PUT /api/nous/config/goals` | `goals` | `repoGoal` — the free-text research question (e.g. "Can we cut CI time by 20% by sharding the slowest test package?"). A legacy `governorGoal` field is accepted but ignored. |
| `PUT /api/nous/config/output` | `output` | Output mode — `state-only` (files + dashboard only), `issue` (opens a GitHub issue per proposal), or `issue+pr` (issue and PR pair); plus `autoHold` and `autoDoNotMerge` booleans that apply the `hold` / `do-not-merge` labels to anything Nous creates. |
| `PUT /api/nous/config/repos` | `repos` | Which repos from `projectRepos` are eligible experiment targets. |
| `PUT /api/nous/config/fast-fail` | `fast_fail` | `queue_depth_max` (default 30), `mttr_max_minutes` (default 180), `budget_burn_rate_max_pct` (default 110) — see below. |
| `PUT /api/nous/config/schedule` | `schedule` | `experiment_duration_hours` (default 4), `baseline_hours` (default 4), `cooldown_hours` (default 2). |
| `PUT /api/nous/config/controllables` | `controllables` | Legacy endpoint, no longer used by the dashboard; it still stores whatever body it receives. |
| `PUT /api/nous/config/principles` | `principles` | Principle-store settings (confidence threshold, decay rate) surfaced alongside the accumulated principle list. |
| `DELETE /api/nous/principles/{id}` | — | Invalidates (removes) a single learned principle. Owner role required. |

The config dialog no longer shows the controllable-knob or invariant lists;
both described runtime-configuration experiments, which the lab does not run.

## Fast-fail bounds

Fast-fail bounds are the safety net for a running experiment. If any bound is
exceeded during an active experiment, Nous aborts immediately and reverts to
the pre-experiment configuration:

- **Queue Depth Max** — actionable issue count ceiling (default 30, range
  5–100 in the dashboard form).
- **MTTR Max (minutes)** — mean time to resolution ceiling (default 180,
  range 30–1440).
- **Burn Rate Max %** — token burn rate as a percentage of the normal rate
  (default 110, range 80–200); values above 100% permit some
  higher-than-normal spend before aborting.

## Schedule

- **Duration** — how long each experiment runs before results are collected
  (default 4h, range 1–48h).
- **Baseline** — how many hours of baseline data are collected before the
  first experiment (default 4h, range 1–48h).
- **Cooldown** — mandatory pause between experiments so results don't
  confound each other (default 2h, range 0.5–24h).

## Principle store

Experiments that consistently improve outcomes get distilled into
**principles** — short natural-language statements with a confidence score
(0.0–1.0). Principles are listed by `GET /api/nous/principles` and shown
sorted by confidence in both the main panel and the config dialog. A
confidence threshold and a weekly decay rate govern when a principle is
considered strong enough to recommend applying permanently, and how quickly
an unreinforced principle loses confidence over time.

## Relationship to the ledger and principles endpoints

- **`GET /api/nous/ledger`** returns the full experiment history — dry runs,
  active runs, completed runs, aborts, and analysis entries — used both for
  the "Would Have Tested" / "Recent Proposals" list (filtered to `dry_run`
  entries) and the experiment history timeline in the dashboard.
- **`GET /api/nous/principles`** returns the current principle list
  independently of the ledger; principles persist (via
  `/var/run/nous/governor/principles.json`) even as ledger entries age out of
  the recent-history view.
- **`GET /api/nous/status`** is the aggregate the dashboard polls to render
  everything above in one call — mode, phase, repo phase detail, snapshot
  counts, the `researchQuestion`, and any pending proposal or active
  experiment. `scope` is always `"repo"`.

If `deps.Nous` is `nil` (nous state failed to initialize), `status` returns
`{"status": "not_configured"}`, `ledger`/`principles` return empty arrays, and
mutating endpoints return `404 nous not configured`. The dashboard treats
`not_configured` as "show the explainer card," not an error.

## Worked example: enabling Nous on a hive

1. Confirm the hive is at ACMM level 4 or above (`HIVE_LEVEL` or the config
   pack), so the Experiment Lab section is visible in the dashboard sidebar.
2. Leave mode at the default `observe` and let the hive run — no action is
   needed here. Snapshots accumulate automatically once per evaluation cycle.
3. Open the Experiment Lab panel's gear icon to set the research question
   (`PUT /api/nous/config/goals`) describing what you want Nous to optimize
   in your repositories.
4. Once the baseline progress bar reports sufficient coverage, switch mode to
   `suggest` (`PUT /api/nous/mode {"mode": "suggest"}`). Review each proposal
   that appears under "Pending Proposal" and Apply or Reject it.
5. Only move to `evolve` once you're comfortable with the proposals Nous has
   made in `suggest` mode — `evolve` runs approved-pattern experiments without
   per-experiment approval.

## What does not exist today

- No `nous:` key in `hive.yaml` or `hive.yaml.example` — do not add one to an
  operator's config expecting it to be read; it is ignored.
- No persistence of the `goals`/`output`/`fast_fail`/`schedule` config
  sections across a process restart beyond the ledger,
  principles, and snapshot count already described above.
- No environment-variable override for the confidence threshold or decay
  rate shown in the dashboard's Principle Store section — those fields exist
  in the UI but are not wired to `NOUS_CONFIDENCE_THRESHOLD` /
  `NOUS_DECAY_RATE_PER_WEEK` on the backend as of this writing; the dashboard
  falls back to its hardcoded defaults (0.8 / 0.05) if the config section
  doesn't provide them.

If any of the above changes, update this page (and the config/output/schedule
table) in the same change that lands the feature.
