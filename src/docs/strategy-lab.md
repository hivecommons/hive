# Experiment Lab (Nous) operator guide

The Experiment Lab (formerly the Strategy Lab) — internally and in the
API called **Nous** — is a controlled experiment framework for your
repositories. It records a baseline of hive behavior, designs experiments
against repo code (tests, CI config, automation scripts), and measures each
one against that baseline before anything is applied for real.

**The lab is repo-only.** Experiments on the hive's own runtime configuration
(cadences, model assignments, thresholds) are out of scope and are not offered
by the lab's UI or API. See [Hive-self background experiments](nous-hive-self-experiments.md)
for the background-only experiment mode that tests Hive itself across opt-in cohorts.

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
| Experiment ledger | `/var/run/nous/governor/ledger.json` | the external [experiment loop](#the-experiment-loop) only, read at boot (the path is historical; it holds repo experiments) |
| Learned principles | `/var/run/nous/governor/principles.json` | the external [experiment loop](#the-experiment-loop) only, read at boot |
| Hive state snapshots | `/data/nous/snapshots/*.json` | one file per evaluation cycle, via `NousState.RecordSnapshot` → `pkg/hivestate`; see [Hive state time series](#hive-state-time-series) |
| Experiment campaigns | `/data/nous/campaigns.json` | `pkg/nous` campaign store (file version 2), written by `POST/PATCH /api/nous/campaigns` (including restarts) and by the legacy `PUT /api/nous/config/goals` / `POST /api/nous/campaign/start`; see [Campaigns and the per-campaign baseline](#campaigns-and-the-per-campaign-baseline) |
| Mode, output mode, repo allow-list, fast-fail bounds, schedule | in-memory `NousState.Config`, set via `PUT /api/nous/config/*` | dashboard ⚙ *Experiment Lab Configuration* dialog (lab-wide guardrails only) or direct API calls |

### Observation finding recovery

Observation detectors keep their issue lifecycle in
`/data/nous/observations.json`, keyed by finding fingerprint. Each record stores
the issue number and URL, `opened_at`, `last_seen_at`, and (while the finding is
absent) `recovered_since`, plus the before/after values and last evidence used
in the recovery comment.

Every evaluation reconciles the currently active findings with this ledger. An
active finding clears `recovered_since`; an absent finding starts the recovery
window. After **seven consecutive days** without the finding, the spoke sends
the hub relay an `action: "recover"` request. The hub searches for the open
issue marker, comments the before/after values and evidence, and closes the
issue. A missing or already-closed fingerprint is an idempotent `ok: false`
no-op, not an evaluation error.

After a successful recovery, the ledger records `closed_at`. If the same
fingerprint fires again, it starts a fresh lifecycle and the hub's open-only
search creates a new issue rather than reopening the old one. The spoke never
writes recovery comments or closes issues directly; the hub relay is the
authoritative write path.

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

## Campaigns and the per-campaign baseline

A **campaign** (shown in the UI as an *experiment card*) is one research
question plus everything measured while it was running
([#12501](https://github.com/hivecommons/hive/issues/12501),
[#12532](https://github.com/hivecommons/hive/issues/12532)). The hive-state
snapshots above are question-independent observations of the hive — queue
depth, SLA rate, kicks, tokens — and are never discarded, but an experiment's
*baseline* is not question-independent: it must be a window not perturbed by
another campaign's interventions, and hypotheses and judged results are
answers to the question that was asked. So:

- **Experiments are defined only via + New experiment.** Submitting the
  inline form (see [Defining experiments](#defining-experiments)) creates
  the card immediately, with its own baseline starting at **0 / 672** and a
  stage line saying what happens next (see
  [Experiment stages](#experiment-stages)). A lab with no card shows
  *"Add an experiment: enter a research question and pick the repos it may
  touch."* — never a bare "no experiments yet".
- **Several experiments can run at once** — see
  [Running several experiments at once](#running-several-experiments-at-once).
- **The ⚙ dialog holds lab-wide guardrails only** — Trust Ladder, Output
  Mode + labels, the repo allow-list, fast-fail bounds, schedule and
  principle settings. It no longer shows a research question or a *Start a
  new experiment* button. The legacy `PUT /api/nous/config/goals` (a
  materially different `repoGoal` archives the oldest running campaign and
  starts a new one inheriting its scope) and `POST /api/nous/campaign/start`
  endpoints are kept for API compatibility but the dashboard no longer calls
  them; to change a question or scope use **Edit** on the card — see
  [Editing an experiment](#editing-an-experiment).
- **Baseline, hypotheses and results are scoped per campaign.** Each card's
  baseline counts snapshot files at or after its `startedAt`, capped at the
  target; ledger rows are attributed to the campaign whose window contains
  their start time and whose scope includes the row's repo (oldest match
  wins). `status.campaign`, `baseline.snapshots`, `baseline_pct` and the
  *Baseline snapshots* tile follow the **oldest running** campaign for
  compatibility. There is no lab-wide baseline bar any more: the per-card
  bars are the real baselines. Principles are durable learnings and stay
  global.
- **The observation history stays visible.** `snapshots` / `snapshotCount`
  keep reporting every snapshot file, and `GET /api/nous/status` publishes
  `observation` (`{snapshots, since}`), which the panel shows once, as the
  first line of the muted *Data collection* note under the cards:
  *"Observation history: N snapshots since <date>"*, followed by what each
  snapshot records, the cadence (one per governor evaluation cycle, 4/hour)
  and the retention policy.
- **Archived campaigns are read-only.** They render under *Previous
  experiments* with their frozen outcome (snapshot count, hypotheses, judged
  results).
- **Migration.** A hive upgraded from before campaigns has no
  `campaigns.json`; on first use the dashboard creates campaign 1 with an
  empty question whose `started_at` is the earliest snapshot on disk, so all
  existing history is campaign 1. A `campaigns.json` written by the
  single-campaign store (file version 1) is upgraded in place: its active
  campaign becomes the first card (hive-wide scope) and its archived ones
  become *Previous experiments*. The running campaign also remembers the
  question across a restart, which the in-memory `goals` section does not.

`status.campaigns` lists **every** campaign in id order as `{id, question,
hypothesis, scope: {repos, intervention, tags, hiveWide}, status, startedAt,
endedAt, snapshots, target, targetExplainer, percent, complete, experiments,
principles, results, stage, stageLabel, stageDetail, nextAction, version,
restartedFrom?}`. `version` starts at 1 and `restartedFrom` names the
archived campaign this one was restarted from (see
[Editing an experiment](#editing-an-experiment)).
`target` is `NousBaselineTarget` = 4 snapshots/hour × 24 h × 7 days = **672**
and `targetExplainer` says so in words — *"≈ 7 days of observations
(4/hour)"* — which the cards show next to an ⓘ. Campaigns are only enabled
when a snapshot directory is configured; without one the baseline stays
global and no `campaign`/`campaigns` block is published.

### Running several experiments at once

Experiments are first-class concurrent entities
([#12532](https://github.com/hivecommons/hive/issues/12532)). The lab
renders them exactly like the dashboard's Inception section renders its
campaigns — one row per experiment with the question as title, scope pills,
a status pill, the baseline bar, a stage line, and a row of compact icon
actions **✎ Edit · ⏸ Pause / ▶ Resume · 🗄 Archive** (each with an
`aria-label` and tooltip, ≥ 28 px hit area). **Generate proposals** stays a
labelled primary button because it is the one call to action. A muted
*"vN · restarted from #M"* line appears on restarted experiments and a muted
*"Scope includes repos no longer in the allow-list: …"* warning appears when
the ⚙ allow-list was narrowed after the experiment was scoped (the
experiment keeps its scope).

### Defining experiments

**+ New experiment** is the single place to define an experiment. It opens
an inline form with the research question, an optional hypothesis, and the
scope pills. The pills offered are the **allow-list** from the ⚙ dialog's
*Repos experiments may target* section (`repoTargets`; every project repo
when the allow-list is empty); no pill selected means *hive-wide*. The ⚙
dialog's copy says so: *"Experiments can only choose repos from this list;
none selected = any repo."*

### Editing an experiment

**✎ Edit** on a card opens the same form pre-filled. What happens on save
depends on what changed:

- **Hypothesis or tags only** — saved in place via
  `PATCH /api/nous/campaigns/{id}`; id, baseline and proposals are kept.
- **Research question or scope (repo set)** — the baseline and anything
  proposed so far no longer answer the edited experiment, so the UI shows an
  in-app confirm (never `window.confirm`): *"Changing the question or scope
  restarts this experiment: baseline resets to 0/672 (≈7 days) and
  hypotheses/proposals so far are archived with the old version. Continue?"*
  with **Restart experiment** / **Keep as is**. Confirming sends the same
  `PATCH` with `restart: true`: the current campaign is archived as version
  N with its frozen outcome, and a new campaign is created as version N+1
  with the edited fields, `restartedFrom` pointing at the old id and a fresh
  0 / 672 baseline. The scope-conflict guard below runs before anything is
  written. The same applies to paused experiments; archived ones are
  read-only.

Without `restart: true` a material edit is refused with **409**
`{code: "restart_required", campaign: <current>}` so a stale client can never
silently reset a baseline.

| Call | Effect |
|---|---|
| `GET /api/nous/campaigns` | Every campaign with per-campaign baseline, results, scope, stage and version lineage. |
| `POST /api/nous/campaigns` `{question, hypothesis?, scope?}` | Starts a new experiment **alongside** the running ones (201). |
| `PATCH /api/nous/campaigns/{id}` `{action: pause\|resume\|archive}` | Pauses (releases the scope), resumes or archives (read-only) a campaign. |
| `PATCH /api/nous/campaigns/{id}` `{question?, hypothesis?, scope?}` | Edits in place when nothing material changed (`{status: "updated", campaign}`); otherwise **409** `restart_required`. |
| `PATCH /api/nous/campaigns/{id}` `{question?, hypothesis?, scope?, restart: true}` | Archives the campaign as version N and starts version N+1 with the edited fields: `{status: "restarted", campaign: <new>, archived: <old>}`. |

**Confound guard.** Two *running* experiments may not share a repo target,
and two may not both be hive-wide: concurrent interventions on the same scope
confound each other's outcomes — neither baseline is a clean window any more
and a judged delta cannot be attributed to either question. `POST` and
`PATCH` (resume, widening the scope, or a restart) return **409** with
`{code: "scope_conflict", shared: [repos], conflict: <campaign>}`; the UI
renders an in-app warning on the form (never a browser `alert`) that names
the other experiment and offers **Pause #N and start** or **Narrow scope**.
A paused experiment holds no scope, so pausing is the quick way to hand a
repo to another question. Because a lab migrated from the single-campaign
store has one hive-wide card, the second experiment conflicts until that
card is narrowed or paused — the warning says so.

### Experiment stages

Each card carries a server-derived `stage` so the operator never has to work
out what the lab is waiting for (a user with a question and a 100 % baseline
once saw *"No defined experiments yet — switch to Suggest…"*; that copy is
gone):

| `stage` | Card says | What moves it on |
|---|---|---|
| `baseline` | *collecting baseline N / 672* | snapshots accumulating, one per evaluation cycle |
| `ready` + `nextAction: generate_proposals` | *ready — generate proposals* with a single **Generate proposals** button | pressing it sets the trust-ladder mode to `suggest` (`PUT /api/nous/mode`) — the mode knob stays as the safety gate but is no longer something to discover |
| `ready` (mode already `suggest`/`evolve`) | *ready — generating proposals* | the external experiment loop writing a proposal to the ledger |
| `ready` + `experimentLoopIdle: true` (mode `suggest`/`evolve`) | *ready — the experiment loop is not running on this hive*, with a link to these docs | deploying the [experiment loop](#the-experiment-loop); the card returns to *generating proposals* as soon as the loop writes under `/var/run/nous/governor` |
| `proposed` | *proposals awaiting your review* | Apply / Reject under Pending Proposal |
| `running` | *running* | the experiment's phases completing |
| `concluded` | *concluded* | archive, or let the lab propose more |
| `paused` | *paused* | Resume |

Note that the hive process itself designs no proposals: `PUT /api/nous/mode`
records the operator's trust level, and proposals, ledger rows and principles
arrive from the experiment loop that writes the files in
[State and storage](#state-and-storage). On a hive without that loop a ready
card in Suggest/Evolve mode says so after `NousLoopIdleCycles` evaluation
cycles instead of waiting indefinitely — see
[The experiment loop](#the-experiment-loop)
([#12541](https://github.com/hivecommons/hive/issues/12541) tracks an
in-process designer). `GET /api/nous/status` reports the
**live** mode (an earlier bug kept echoing the boot-time `observe`).

**Principles** (the *Principles* tile, ⓘ) are durable lessons the lab distils
from judged experiments into `principles.json`. A new lab legitimately shows
0; none are needed to start an experiment, and the count grows as experiments
conclude.

### The experiment loop

**The experiment loop is the sole producer of proposals, ledger rows and
principles.** The hive binary contains no proposal designer: `PUT
/api/nous/mode` only records the trust level, and nothing in-process consumes
it. The loop is a separate component made of:

- the external Nous strategy-evolution framework, cloned at a reviewed pin
  into `/opt/nous` by `bin/nous-install.sh`;
- `bin/nous-runner.sh`, which runs one `run_campaign.py` iteration per call
  for the scope and mode in `/etc/hive/nous-campaign.yaml` and keeps its work
  directory under `/var/run/nous/<scope>/` (`/var/run/nous/governor/state.json`
  for the governor scope);
- `bin/nous-hive-gate.py`, which posts design/findings gates to
  `PUT /api/nous/gate-decision` and polls `GET /api/nous/gate-response`;
- `bin/nous-sync.py`, which post-processes principles and recommendations.

**File contract.** The hive reads exactly two files written by the loop, from
`/var/run/nous/governor/` (`loadNousStateFromPaths` in `cmd/hive/main.go`),
**once at boot** — restart the hive to pick up new rows:

| File | Shape the hive parses |
|---|---|
| `ledger.json` | `{"iterations": [ {…}, … ]}`; each row's `type`/`status` (`pending`, `active`, `completed`, `judged`, …), `id`, `title`, `hypothesis`, `repo` and a start time (`started_at`, `ts`, …) drive the experiment rows and card stages; `dry_run` rows are ignored |
| `principles.json` | `{"principles": [ {"id", "statement", "confidence": "high"\|"medium"\|"low", "category"} ]}` |

Note that `bin/nous-sync.py` writes `principles.json` and `ledger.jsonl` at
the top of `NOUS_RUN_DIR` (default `/var/run/nous`), not under
`/var/run/nous/governor/`, so its output is not what the hive reads unless
it is placed at the paths above.

**Liveness.** On every `GET /api/nous/status` the hive takes the newest
modification time of the regular files directly under
`/var/run/nous/governor/` and reports it as the `experimentLoop` block
(`dir`, `lastWriteAt`, `running`, `idleAfterSeconds`). The loop counts as
running when that write is at most `NousLoopIdleCycles` (24) governor
evaluation cycles old — `24 × governor.eval_interval_s`, 2 h at the default
300 s. When it is not, every ready card in Suggest/Evolve mode carries
`experimentLoopIdle: true` and reads *ready — the experiment loop is not
running on this hive*. Only writes under `/var/run/nous/governor/` count,
because that is the only loop output the hive reads: a loop that writes
solely elsewhere (for example the repo-scope work directory
`/var/run/nous/repo/`) produces nothing the lab can show and is reported as
not running.

**Deploying it.** The container image copies the four scripts to
`/tmp/hive/bin/`, the example campaign files
(`examples/kubestellar/nous-*.yaml`) to `/etc/hive/`, and creates
`/var/run/nous/governor`, `/var/run/nous/repo`, `/data/nous/snapshots` and
`/opt/nous` (`src/Dockerfile`). It does **not** run `nous-install.sh` or
schedule the runner. The only driver in this repository is an agent policy
that runs `bash /tmp/hive/bin/nous-runner.sh` on every kick — see the
strategist example in `examples/kubestellar/agents/strategist.md`. Beyond
that, deploying the loop on a stock install (installing the framework,
choosing which agent runs it and how often) is **not documented in this
repository**; anything that honours the file contract above works.

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

## Progress and results on the dashboard

The Experiment Lab card surfaces per-experiment progress from
`GET /api/nous/status`:

- **Per-card baselines** — each experiment card carries its own
  `N / 672` bar (see [Campaigns](#campaigns-and-the-per-campaign-baseline)).
  There is no lab-wide baseline bar: the whole-hive snapshot count is a total
  of collections regardless of experiment, not progress, so it is reported
  once in the muted **Data collection** note under the cards — *"Observation
  history: N snapshots since <date>"*, what each snapshot records (queue
  issues, queue PRs, queue on hold, SLA violations, agents kicked, agent
  states, total tokens), the cadence (one snapshot per governor evaluation
  cycle, 4/hour) and the retention policy (24 h raw snapshots, then hourly
  buckets for 30 days). `status.baseline` is still published for API
  compatibility.
- **Proposals and runs** rows are computed server-side for pending, approved,
  running, and judged experiments and render under the cards only when there
  are any — there is no empty "Experiment Progress" box; the cards' stage
  lines carry the empty state. Each row reports the current phase
  (`baseline`, `experiment`, `cooldown`, or `judging`/`judged`), phase end
  time, elapsed/remaining time, snapshot counts per phase, and the next output
  ETA. The metrics list compares baseline-window values with experiment-window
  values as data becomes available.
- **Results** lists judged experiments with their verdict/delta and any output
  links Nous recorded on the experiment ledger entry, including pull requests,
  issues, and learned principle entries. Output modes that open issues or PRs
  should record those URLs into the ledger so the dashboard can link them
  without making extra GitHub calls.

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
`principles`) plus `activeCampaign` (the per-campaign baseline block) and,
when the in-memory `goals` section is empty, `goals.repoGoal` restored from
the active campaign. Each section is written independently with its own `PUT`
endpoint, all requiring owner role and all triggering `refreshAndPersist()`:

| Endpoint | Section | Fields (dashboard defaults shown) |
|---|---|---|
| `PUT /api/nous/config/goals` | `goals` | **Legacy — the dashboard no longer exposes this.** `repoGoal` is the free-text research question of the oldest running campaign. A legacy `governorGoal` field is accepted but ignored. A materially different question archives the oldest running campaign and starts a new one; the response carries `newCampaign` and the active `campaign`. Use `PATCH /api/nous/campaigns/{id}` instead. |
| `POST /api/nous/campaign/start` | — | **Legacy — the dashboard no longer exposes this.** Explicitly archive the active campaign and start a new experiment (`{"question": "..."}` optional; defaults to the active question). Baseline and hypotheses restart at 0; the previous campaign is kept read-only with its results. Owner role required. |
| `PUT /api/nous/config/output` | `output` | Output mode — `state-only` (files + dashboard only), `issue` (opens a GitHub issue per proposal), or `issue+pr` (issue and PR pair); plus `autoHold` and `autoDoNotMerge` booleans that apply the `hold` / `do-not-merge` labels to anything Nous creates. |
| `PUT /api/nous/config/repos` | `repos` | The **allow-list** — shown in the dialog as *Repos experiments may target*. Experiments can only choose scope repos from this list; an empty list means any project repo. Narrowing it later does not change existing experiments' scope; their cards show a muted warning instead. |
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
  counts, the `researchQuestion`, baseline progress, per-experiment progress,
  result links, and any pending proposal or active experiment. `scope` is
  always `"repo"`.

If `deps.Nous` is `nil` (nous state failed to initialize), `status` returns
`{"status": "not_configured"}`, `ledger`/`principles` return empty arrays, and
mutating endpoints return `404 nous not configured`. The dashboard treats
`not_configured` as "show the explainer card," not an error.

## Worked example: enabling Nous on a hive

1. Confirm the hive is at ACMM level 4 or above (`HIVE_LEVEL` or the config
   pack), so the Experiment Lab section is visible in the dashboard sidebar.
2. Leave mode at the default `observe` and let the hive run — no action is
   needed here. Snapshots accumulate automatically once per evaluation cycle.
3. Optionally open the panel's ⚙ gear to set lab-wide guardrails — output
   mode and labels, and the repo allow-list (*Repos experiments may
   target*). Then press **+ New experiment** and enter the research question
   describing what you want Nous to optimize, an optional hypothesis, and the
   repos it may touch (`POST /api/nous/campaigns`).
4. The question appears as an experiment card at 0 / 672. When its baseline
   is complete the card reads *ready — generate proposals*; press **Generate
   proposals** (equivalent to `PUT /api/nous/mode {"mode": "suggest"}`).
   Proposals appear only if the external
   [experiment loop](#the-experiment-loop) runs on this hive; if the card
   instead reads *ready — the experiment loop is not running on this hive*,
   deploy the loop first. Review each proposal that appears under "Pending
   Proposal" and Apply or Reject it. Add further questions with **+ New experiment**, each scoped
   to its own repos.
5. Only move to `evolve` once you're comfortable with the proposals Nous has
   made in `suggest` mode — `evolve` runs approved-pattern experiments without
   per-experiment approval.

## What does not exist today

- No in-process proposal designer — proposals, ledger rows and principles
  come only from the external [experiment loop](#the-experiment-loop)
  ([#12541](https://github.com/hivecommons/hive/issues/12541)).
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
