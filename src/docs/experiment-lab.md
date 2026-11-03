# Experiment Lab (Nous) operator guide

The Experiment Lab (formerly Strategy Lab; internally and in the API
called **Nous**) is a controlled experiment framework for your
repositories. It records a baseline of hive behavior, designs experiments
against repo code (tests, CI config, automation scripts), and measures each
one against that baseline before anything is applied for real.

**The lab is repo-only.** Experiments on the hive's own runtime configuration
(cadences, model assignments, thresholds) are out of scope and are not offered
by the lab's UI or API (#12207). See [Hive-self background experiments](nous-hive-self-experiments.md)
for the background-only experiment mode that tests Hive itself across opt-in cohorts.

The `nous:` block in `hive.yaml` carries only runtime knobs — gate poll
cadence, dispatcher, dependency fetch allowlist and the optional artifact
repository — and every one of them is editable from **Settings → Features →
Nous / Experiment Lab** (see [Settings](#settings)). Goals, bounds, schedule
and the trust ladder are configured at runtime through the dashboard
(Experiment Lab panel, ACMM level 3+) or directly through the `/api/nous/*`
endpoints. This page documents
that runtime configuration and the experiment lifecycle, verified against
`pkg/dashboard/api.go`, `pkg/dashboard/deps.go`, `cmd/hive/main.go`, and the
dashboard's `renderNousConfig()`/`renderNous()` UI code.

## Visibility

The Experiment Lab section is hidden by default. Set `dashboard.experiment_lab:
true` in `hive.yaml` to show it. The pre-rename key `dashboard.strategy_lab`
is accepted as an alias for one release (#11695): it still enables the panel,
logs a one-line deprecation at config load, and is folded into
`dashboard.experiment_lab` so the next config save writes the documented key.
The `/api/nous/*` endpoints, `NousState` and the `nous-section` dashboard
anchor keep their Nous names; only the user-facing label changed.

Once enabled, the **Experiment Lab (nous)** sidebar entry and section appear
on the next dashboard status refresh — no page reload — at **ACMM level 3 and
above** (`ACMM_NOUS_MIN_LEVEL` in `pkg/dashboard/static/index.html`; it was
L4 before #12076, which also made the Settings toggle say so). Both gates must
pass: the flag on and the level at L3+. Below L3 the panel and sidebar summary
are hidden, though the `/api/nous/*` endpoints still respond. The sidebar label
follows the Inception convention ("Inception (spektacular)"): section name plus
the lowercase codename of the planner behind it. On a hive with no snapshots
yet, the panel shows an explainer card instead of empty stats.

### Section header

The section header reads **🧪 Experiment Lab** followed by the research
question, when one is set, as saved from the ⚙️ dialog's **Goal Definition**
section (`PUT /api/nous/config/goals`). The question is shown in full after a
muted *Research question:* label and wraps to the remaining header width (a pasted
`research_question:` YAML key or `>`/`|` folded-scalar marker is stripped, #12498);
the collapsed-section summary shows the lab mode and experiment count instead.
When no research question is set the header shows nothing beside the title. The header does **not** show mode: the `OBSERVING ·
GOVERNOR` mode × scope pill from the first Nous integration (#330, #338) was
removed in #12089 because the segmented **Observe | Suggest | Evolve** control
inside the card already carries that state, and the scope selector was removed
in #12207 when the lab became repo-only.

## State and storage

All Nous state lives under one durable root on the PVC — `/data/nous` by
default, configurable with `data.nous_dir` and overridable at runtime with
`HIVE_NOUS_DATA_DIR` (RFC #11585 part 1, #11694). The other Nous keys in
`hive.yaml` live under `nous:` and are listed in [Settings](#settings):

| What | Where (relative to `data.nous_dir`) | Written by |
|---|---|---|
| Experiment ledger | `governor/ledger.json` | the experimentation agent (Nous); read live — see [Live ledger and principles](#live-ledger-and-principles) |
| Learned principles | `governor/principles.json` | the experimentation agent (Nous); read live |
| Hive state snapshots | `snapshots/*.json` | one file per evaluation cycle, via `NousState.RecordSnapshot` → `pkg/hivestate`; see [Hive state time series](#hive-state-time-series) |
| Experiment campaigns | `campaigns.json` | `pkg/nous` campaign store (file version 2), written by `POST/PATCH /api/nous/campaigns` (including restarts) and by the legacy `PUT /api/nous/config/goals` / `POST /api/nous/campaign/start`; see [Campaigns and the per-campaign baseline](#campaigns-and-the-per-campaign-baseline) |
| Pending gate question, owner decision, decision history | `gates/pending.json`, `gates/decision.json`, `gates/history.jsonl` | `bin/nous-hive-gate.py` writes `pending.json`; `PUT /api/nous/gate-decision` writes the other two (see [Gate protocol](#gate-protocol)) |
| Mode, goals, output labels, repo allow-list, fast-fail bounds, schedule | `panel-config.json` (mirrors `NousState.Mode`/`Config`) | every `PUT /api/nous/mode` and `PUT /api/nous/config/*`; re-applied at boot |

Panel configuration therefore **survives a process restart**: `loadNousState()`
in `cmd/hive/main.go` seeds the snapshot count from the files above, performs
the first live load of the ledger and principles, and then overlays
`panel-config.json` onto the defaults. A corrupt `panel-config.json` is logged
and ignored (defaults apply) rather than blocking boot.

### Live ledger and principles

The ledger and principles are **not** a boot-time snapshot (RFC #11585 part
4, #11697). Every `GET /api/nous/status`, `GET /api/nous/ledger` and
`GET /api/nous/principles` asks `pkg/nousstore`'s `KnowledgeReader` for the
current content. The reader stats the two files (one `stat` each, no read)
and re-parses only a file whose size or mtime changed, so the panel's 30s
poll picks up a round the agent just finished without a restart and without
re-reading unchanged files. Rules:

- A missing file is empty. A file that exists but does not parse (the agent
  is mid-write) is logged and the **last good copy** keeps being served; the
  repaired file is picked up on the next read.
- When the durable `governor/` directory has neither file, the legacy
  `/var/run/nous/governor` is read instead; the reader switches to the
  durable directory as soon as a file appears there.
- `NousState.KnowledgeStore` is the seam (`SyncKnowledge()`); with it unset
  (older wiring, tests) the handlers serve whatever is in `NousState.Ledger`
  / `Principles`, exactly as before.

### Ledger and principle schema: `type` and `status`

Nous's own `ledger.schema.json` has no `type` or `status` — a row is "one
record per completed iteration" whose result is `h_main_result` — while its
`principles.schema.json` has `status` (`active`/`updated`/`pruned`) and
`category` (`domain`/`meta`). The panel, written against the earlier
governor experiment loop, grouped ledger rows by a `type` whose values mixed
kinds (`dry_run`, `analysis`) with states (`active`, `completed`, `aborted`).
`pkg/nousstore` now defines **one schema both sides use**, applied on read:

| Entry | Field | Values | Default when absent |
|---|---|---|---|
| Ledger | `type` | `baseline`, `experiment`, `dry_run`, `analysis`, `shadow_analysis` | `baseline` when Nous's `family` or `candidate_id` is `baseline`, else `experiment` |
| Ledger | `status` | `pending`, `active`, `completed`, `aborted` | `completed`; a legacy row whose `type` was `active`/`completed`/`aborted` becomes `type: experiment` with that `status` |
| Ledger | `outcome` | free text | lowercase `h_main_result` (`confirmed`, `refuted`, `partially_confirmed`) when the row has one |
| Principle | `type` | `domain`, `meta` | Nous's `category` when it is one of those, else `domain`; the raw category is still served as `source` |
| Principle | `status` | `active`, `updated`, `pruned` | `active` |

Existing files load unchanged. A row that carries an **unknown** `type` or
`status` is logged (`nous: ledger/principles entry off schema`), served with
the default, and never hides the rest of the file. A writer that emits the
normalized shape round-trips through the reader unchanged (pinned by
`TestKnowledgeRoundTrip`). In the panel, each principle and each entry in
the **recent rounds** list under the history timeline shows its `type` and
`status` as chips (`status` uses the design system's tones: pending = warn,
active = info, completed = ok, aborted = error, pruned = neutral); the
timeline bars are keyed by `status` (and by `type` for analyses). The
"Would Have Tested" list still filters on `type: dry_run`.

**Migration from the previous layout.** Before #11694 the ledger and
principles were read from `/var/run/nous/governor`, a tmpfs that is wiped on
every container restart. On each boot the hive now copies any
`ledger.json`/`principles.json` found there into `<nous_dir>/governor/` when
the durable copy is missing or older, and never copies backwards or deletes.
If the durable copy cannot be written, the loader falls back to reading the
legacy directory for that boot and logs a warning. The legacy path is still
created in the image so an older governor loop keeps working; its output is
simply carried forward on the next restart.

**Backups.** The owner-triggered spoke backup (`POST /api/backup`) captures
`<data-dir>/nous/` **except** `snapshots/` (hundreds of MB of regenerable
baseline samples), and `hive-backup restore` puts it back under
`<data-dir>/nous/`. See [backup-restore.md](backup-restore.md#owner-triggered-spoke-backup).
The nightly hub DR backup is unchanged and still excludes `nous/` entirely.
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

### Observation detectors

`hivestate.Observe` runs pure detectors over the snapshot series and returns
`Observation` values (kind, title, evidence, suspected cause, next step, a
daily series, and the fingerprint `<hive id>:<kind>`) ([#12652](https://github.com/hivecommons/hive/issues/12652)).
It writes nothing. Every detector returns nothing on an empty series or when
the last 7 d window covers under 5 days; week-over-week detectors also need
5 days of coverage in the prior 7 d.

- **`queue-growth`.** p90 queue depth (issues + PRs) over the last 7 d is at
  least 50 % above the prior 7 d.
- **`hold-growth`.** The daily median `queue_hold` rose on each of the last 3
  days, with samples on all four days involved.
- **`sla-violations`.** `sla_violation_rate` over 7 d is above 0.25, or at
  least 1.5× and 0.05 above the prior 7 d.
- **`agent-idle` / `agent-thrashing`.** `agent_idle_ratio` over 7 d is above
  0.90; if `kicks_per_day` is also at least 500 the finding is
  `agent-thrashing` instead.
- **`token-efficiency`.** `tokens_per_day` is at least 50 % above the prior
  7 d while median queue depth (issues + PRs) stays within 20 % (at least one
  item); queue depth stands in for throughput because buckets carry no
  merged-PR counts.
- **`mode-stuck`.** One governor mode, `surge` or `idle`, accounts for at least
  95 % of the mode-bearing samples over 7 d and is the majority mode on every
  day with samples. Buckets written before `mode_samples` existed carry no mode
  info and never fire it.

### Filing observations

Filing detector findings upstream is opt-in and throttled ([#12654](https://github.com/hivecommons/hive/issues/12654)):

- **Off by default.** Nothing runs unless `nous.observations.enabled` is
  `true`. When it is, the detectors run once per eval cycle, right after the
  snapshot is recorded.
- **Weekly budget.** At most `nous.observations.max_per_week` (default `5`,
  minimum `1`) new observations are filed per rolling 7 days. The ledger is
  `observations-budget.json` under `data.nous_dir`, so a restart does not
  reset it. A fingerprint already filed in the window is not filed again.
  Observations over budget are logged (`weekly budget exhausted`) and
  dropped; that is not an error.
- **Upgrade window.** While a hub-instructed self-upgrade is draining — the
  same signal the governor uses to withhold new kicks — the detectors still
  run but nothing is filed and no budget is spent.
- **Sink.** Findings go through the hub self-report relay
  ([#12653](https://github.com/hivecommons/hive/issues/12653)). A build
  without that relay files nothing.

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
  dialog (see [Defining experiments](#defining-experiments)) creates
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
a modal dialog titled *New experiment* with the research question, an
optional hypothesis, and the scope pills; Esc, Cancel or a click on the
backdrop closes it and focus returns to the button that opened it. Only one
add/edit dialog is open at a time: pressing **+ New experiment** or ✎ while
a draft is open is blocked (the open dialog is re-focused and a toast says
*"Finish or cancel the open experiment form first"*), so a draft is never
silently replaced. The pills offered are the **allow-list** from the ⚙ dialog's
*Repos experiments may target* section (`repoTargets`; every project repo
when the allow-list is empty); no pill selected means *hive-wide*. The ⚙
dialog's copy says so: *"Experiments can only choose repos from this list;
none selected = any repo."*

### Editing an experiment

**✎ Edit** on a card opens the same dialog pre-filled, titled *Edit
experiment #N*, with the help text *"Changing the question or scope restarts
it; the hypothesis may change freely."* under the title. What happens on
save depends on what changed:

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

## Experiment lifecycle

1. **Collecting** — a fresh hive starts in this phase. Every evaluation cycle
   writes a baseline snapshot
   (`/data/nous/snapshots/<epoch-ms>.json`) with queue depth, MTTR, SLA
   violations, agents kicked, and token totals. The `NousBaselineTarget`
   constant (672 snapshots) is treated as "enough baseline". The `baseline_pct` status
   field is `snapshotCount / NousBaselineTarget` capped at 100; the dashboard
   progress bar computes its own clamped value from `snapshotCount` and
   `snapshotTarget` rather than reading `baseline_pct`.
2. **Observing** — once at least one snapshot exists, phase auto-advances to
   `observing` (`refreshStatus()` in `pkg/dashboard/deps.go`).
3. **Framing → Design → Design Review → Executing → Analysis → Findings
   Review → Extraction** — the full phase sequence
   (`NOUS_PHASES` in the dashboard JS) that a live experiment iteration walks
   through. Phase and iteration are reported through `GET /api/nous/phase` and
   the `phases.repo` block of `GET /api/nous/status`. Any other scope block the
   experiment loop publishes into `NousState.Status` is dropped from the
   response.
4. **Gate decision** — the design and findings phases pause for an owner
   decision. The experiment agent publishes the question to
   `<nous_dir>/gates/pending.json`; the hive mirrors it into
   `GET /api/nous/gate-pending` and pings the owner once. An owner responds
   with `PUT /api/nous/gate-decision {"decision": "approve|reject|abort",
   "reason": "..."}`; the decision is recorded on disk for the agent, kept in
   `NousState.GateResponse`, and audited as `nous_gate_decision`. See
   [Gate protocol](#gate-protocol). `POST /api/nous/gate-respond` accepts a
   free-form body for the same purpose and is audited as `nous_gate_respond`.
5. **Approve / Reject / Abort** — while in **Suggest** mode, a pending
   proposal (`NousState.Status["pending"]`) shows Apply/Reject buttons in the
   dashboard, backed by `POST /api/nous/approve` and `POST /api/nous/abort`.
   Approve only records the decision (audited `nous_approve`); it never opens
   an issue or admits a run. A running experiment can be stopped early with
   the same Abort action, which withdraws the proposed repo change.
6. **Verdict and Promote** — when the campaign has written its final
   recommendation, the panel shows a verdict card. Acting on it is a separate,
   manual, owner-only step: see
   [Verdict card and manual Promote](#verdict-card-and-manual-promote).

## Gate protocol

A Nous gate is the point where an experiment stops and asks "may I run this
design?" or "do these findings hold?". Only an owner may answer, and the
answer must reach the agent that asked (RFC #11585 part 3, #11696).

Before #11696 the agent-side bridge (`bin/nous-hive-gate.py`) tried to *PUT*
its question to `/api/nous/gate-decision`. That endpoint is owner-only and
the agent holds no owner credential, so the write always failed, nothing ever
appeared in `GET /api/nous/gate-pending`, and every gate silently timed out as
a reject. The fix keeps the HTTP API unchanged and moves the agent→hive leg
onto the durable Nous root from part 1:

```
agent (nous-hive-gate.py)              hive (pkg/dashboard + pkg/nousstore)            owner
        │  write gates/pending.json              │                                       │
        │ ──────────────────────────────────────▶│  poll (nous.gate_poll_seconds) or     │
        │                                        │  on GET /api/nous/gate-pending        │
        │                                        │  → NousState.GatePending, ping once ─▶│
        │                                        │◀── PUT /api/nous/gate-decision ───────│
        │                                        │  append gates/history.jsonl           │
        │                                        │  write  gates/decision.json           │
        │                                        │  remove gates/pending.json            │
        │  poll gates/decision.json (gate_id)    │                                       │
        │◀───────────────────────────────────────│                                       │
        ▼ approve | reject | abort
```

**Files** (all under `<nous_dir>/gates/`, written atomically via `.tmp` +
rename, included in the owner spoke backup):

| File | Written by | Contents |
|---|---|---|
| `pending.json` | agent | `id`, `gate` (`design`/`findings`), `question`, `scope`, optional `summary`, `artifact`, `reviews`, `requested_at` |
| `decision.json` | hive | `gate_id`, `decision`, `reason`, `decided_by`, `decided_at` — replaced on every decision |
| `history.jsonl` | hive | one `decision.json` record per line, append-only |
| `notified` | hive | id of the last gate the owner was pinged for, so a restart does not re-ping |

**Decision vocabulary.** `decision` must be `approve`, `reject` or `abort`;
anything else is `400`. `reason` (alias `note`) is optional free text. An
optional `gate_id` must match the pending gate or the request is `409` — a
stale answer can never approve a newer gate. With no pending gate the request
is `409` too. The agent ignores a `decision.json` whose `gate_id` is not the
one it published.

**Timing.** The hive checks for a new `pending.json` every
`nous.gate_poll_seconds` (`15` by default, `HIVE_NOUS_GATE_POLL_INTERVAL`
overrides) *and* on every `GET /api/nous/gate-pending`, so the Experiment Lab
page is never behind the file. The agent polls `decision.json` every
`NOUS_GATE_POLL_INTERVAL_SEC` (5) for up to `NOUS_GATE_TIMEOUT_SEC` (1800);
on timeout it withdraws `pending.json` and treats the gate as `reject`.

**Modes.** In `observe` and `evolve` mode the bridge auto-approves and writes
nothing. `suggest` mode always waits for the owner, and because the lab is
repo-only every proposal is a repo change that gates on approval.

**Notification.** The owner is pinged through the configured notifier
(ntfy/Slack/Discord) once per gate id with the question; the `notified`
marker is written *before* the ping so a crash between the two drops a ping
rather than duplicating it.

## Trust ladder (mode)

Set with `PUT /api/nous/mode {"mode": "observe|suggest|evolve"}`. The
dashboard's trust-ladder widget documents the three modes:

| Mode | Behavior |
|---|---|
| `observe` | Data collection only — nothing is proposed or changed. |
| `suggest` | Nous designs experiments but every proposal requires operator approval via `nous_approve`/`nous_abort` before it runs. |
| `evolve` | Nous runs approved-pattern experiments autonomously and distills durable results into principles without per-experiment approval. |

**Who can change it.** Mode is owner-only: the endpoint runs
`requireOwnerRole`, i.e. the request must carry a *verified* owner identity —
a hub-proxied login whose `dashboard.authorized_users` entry is `:owner`, a
device-flow session with the live owner role, or the shared dashboard token
on a self-hosted spoke. Anything else is a `403 {"error":"owner access
required"}`. The dashboard mirrors this: the **Observe | Suggest | Evolve**
buttons in the Experiment Lab section and the Trust Ladder steps in the ⚙️
configuration dialog are the same control (one `nousSetMode` handler); they
are disabled with an
"Owner access required" tooltip for read/read-write/merger viewers, and any
server rejection (400/401/403) is surfaced as an error toast rather than
swallowed (#12039).

**What each transition requires.**

| Transition | Requirement |
|---|---|
| `observe → suggest` | Nothing beyond owner role. Recommended only once the baseline progress bar reports sufficient coverage (`baseline_pct`); the panel shows "Ready to upgrade to Suggest" at that point. |
| `suggest → evolve` | Owner role. Intended only after you have reviewed proposals in `suggest`. |
| any downgrade (`evolve → suggest`, `→ observe`) | Owner role plus an in-app confirmation ("Lower the trust level?"); the dashboard sends `force: true` because a downgrade stops any active experiment. |

Every accepted change is audited (`nous_set_mode` with the new value), mirrored into `GET /api/nous/status` immediately, and persisted to
`panel-config.json` so it survives a restart. Before #12039 the handler
updated the persisted/in-memory field but not the served status, so the UI
appeared not to react until the next restart.

## Scope

The lab is **repo-only**: experiments propose repo code changes (tests, CI
config, automation scripts) and always require approval regardless of trust
level. There is no scope selector in the dashboard, and `GET /api/nous/status`
always reports `"scope": "repo"`.

`PUT /api/nous/scope` is kept only for backward compatibility (owner role
required, see [Who can change it](#trust-ladder-mode)). `{"scope": "repo"}`
returns 200; any other value — including the former `governor` and `both` —
returns `400 scope must be "repo": the Experiment Lab is repo-only` and leaves
state untouched.

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
| `PUT /api/nous/config/output` | `output` | `autoHold` and `autoDoNotMerge` booleans that apply the `hold` / `do-not-merge` labels to anything a promotion creates. There is **no output mode**: Nous writes its verdict to files and the dashboard reads it; opening an issue or admitting a Spektacular run is the owner's manual Promote action on the [verdict card](#verdict-card-and-manual-promote). (Before #11699 this section carried a `mode` of `state-only` / `issue` / `issue+pr` / `spektacular-run` that Approve acted on automatically; a stored `mode` is ignored.) |
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

- **`GET /api/nous/ledger`** returns the full experiment history, read live
  from `<nous_dir>/governor/ledger.json`, every entry carrying `type`,
  `status` and (when recorded) `outcome` per the
  [schema above](#ledger-and-principle-schema-type-and-status). It feeds the
  "Would Have Tested" / "Recent Proposals" list (filtered to
  `type: dry_run`), the history timeline and the recent-rounds list.
- **`GET /api/nous/principles`** returns the current principle list, read
  live from `<nous_dir>/governor/principles.json`, independently of the
  ledger; each carries `id`, `text`, `confidence` (0–1), `source` (raw
  category), `type` and `status`.
- **`GET /api/nous/status`** is the aggregate the dashboard polls to render
  everything above in one call — mode, phase, repo phase detail, snapshot
  counts, baseline progress, per-experiment progress, result links, and any
  pending proposal or active experiment. `scope` is always `"repo"`. It also carries `researchQuestion`, the research question from
  the saved `goals` (empty when none is set), which the
  [section header](#section-header) displays.

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
   labels and the repo allow-list (*Repos experiments may target*). Then press **+ New experiment** and enter the research question
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
   When the campaign reaches a verdict, the verdict card appears; promote it
   to an issue or a Spektacular run yourself if you agree with it.
5. Only move to `evolve` once you're comfortable with the proposals Nous has
   made in `suggest` mode — `evolve` runs approved-pattern experiments without
   per-experiment approval.

## Settings

Every `nous.*` key in `hive.yaml` has a control in **Settings → Features →
Nous / Experiment Lab** (#12026; the same pattern as the Autonomy policy
group, #11855). Saving goes through the existing
`PUT /api/config/governor/features` path, is owner-only, and is validated
with the same rules `hive.yaml` loading applies (`NousConfig.Validate`), so a
value the dialog accepts is exactly a value the hive will load. Typing
"nous", "experiment", "gate" or "artifact" into the settings search finds the
group. The panel's visibility toggle, `dashboard.experiment_lab`, stays on the
Dashboard tab.

| Control | Key | Env override | Notes |
|---|---|---|---|
| Dispatcher | `nous.dispatcher` | `HIVE_NOUS_DISPATCHER` | Select: `inline` (default) or `sdk`. Empty stores the default. The dialog notes when the env var overrides the saved value. |
| Gate poll (seconds) | `nous.gate_poll_seconds` | `HIVE_NOUS_GATE_POLL_INTERVAL` | `0` means the 15s default; negative is rejected. |
| Dependency fetch allowlist | `nous.fetch_allowlist` | `HIVE_NOUS_FETCH_ALLOWLIST` | Comma-separated exact lowercase hostnames. **Saving an empty field writes an empty list, which denies every fetch**; the field's help text says so and shows whether the key is currently absent (default `github.com`) or empty. Enter `github.com` to restore the default host. |
| Repository | `nous.artifact_repo.repo` | `HIVE_NOUS_ARTIFACT_REPO` | `owner/name`; empty keeps the mirror off. |
| Branch | `nous.artifact_repo.branch` | `HIVE_NOUS_ARTIFACT_BRANCH` | Empty resolves the repository's default branch. Ref-unsafe names are rejected even while the repo is empty. |
| Path | `nous.artifact_repo.path` | `HIVE_NOUS_ARTIFACT_PATH` | Relative, no `..`; empty means `campaigns`. |
| Sync interval (seconds) | `nous.artifact_repo.sync_interval_seconds` | `HIVE_NOUS_ARTIFACT_SYNC_INTERVAL` | `0` means the 300s default; the 30s floor is applied at resolve time. |
| Observations enabled | `nous.observations.enabled` | — | Off by default. See [Filing observations](#filing-observations). |
| Observations max per week | `nous.observations.max_per_week` | — | Default `5`; values below `1` are rejected. |

`agents.<name>.engine` is exposed in the agent configuration dialog (General
tab, "Experiment engine", next to Role) and validated with
`ValidateExperimentEngine`: it is accepted only on an agent whose role (after
the same save) is `experimenter`.

## Resetting data

The Experiment Lab Configuration dialog (Settings → Experiment Lab → gear
icon) ends with a **Data** section that shows the current snapshot /
principle / experiment counts and three owner-only buttons. Each one calls
`POST /api/nous/reset` with a JSON body of the form
`{"scope": "<scope>", "confirm": "RESET"}`:

| Button | `scope` | What is removed | What else happens |
|---|---|---|---|
| Reset baseline | `baseline` | `snapshots/*` | snapshot counter and `baseline_pct` return to 0, phase to `collecting`, mode forced back to `observe` |
| Clear knowledge | `knowledge` | contents of `governor/ledger.json` and `governor/principles.json` (rewritten as empty arrays) | the live `KnowledgeReader` cache is invalidated, so the next status read reports 0 principles / 0 experiments |
| Reset everything | `all` | both of the above plus `gates/*` | pending gate question and decision are cleared; mode forced back to `observe` |

`panel-config.json` is **never** touched: mode, scope, goals, bounds and
schedule are settings, not data. Only the mode is rewritten (to `observe`)
after a `baseline` or `all` reset, because the baseline the other modes
depend on is gone.

Rules enforced by the handler:

- Owner role required (403 otherwise); every call is audited as
  `nous_reset` with the scope and the removed counts, or `nous_reset_failed`
  with the error.
- `confirm` must be exactly `RESET` and `scope` one of the three values
  above, otherwise 400. The dashboard asks for a styled confirmation and, for
  "Reset everything", makes you type `RESET`.
- If the ledger contains an `active` experiment, `baseline` and `knowledge`
  return 409 — abort the experiment first (`POST /api/nous/abort`). `all`
  is always allowed because it removes the ledger as well.
- A missing `data.nous_dir` (or a missing sub-directory) is a no-op success.

The response is `{"ok": true, "scope": ..., "removed_snapshots": N,
"removed_ledger_entries": N, "removed_principles": N, "mode": "observe"}`.

Deletion is atomic per directory: `pkg/nousstore` renames the directory to
`<nous_dir>/.trash/<timestamp>/<name>` and then removes it, so a crash
mid-way leaves either the original or a complete trash copy, never a
half-deleted tree. The newest trash entry is kept (older ones are pruned on
the next reset) and `.trash` is excluded from spoke backups.

If the snapshot count exceeds ten times the baseline target the Data section
shows a hint that older snapshots are not pruned automatically — see the
retention issue #12030.

## What does not exist today

- No in-process proposal designer — proposals, ledger rows and principles
  come only from the external [experiment loop](#the-experiment-loop)
  ([#12541](https://github.com/hivecommons/hive/issues/12541)).
- Goals, bounds, schedule and the trust ladder are **not** read from YAML —
  they live in `panel-config.json` under `data.nous_dir` and are set through
  the API, not through the Settings dialog.
- No environment-variable override for the confidence threshold or decay
  rate shown in the dashboard's Principle Store section — those fields exist
  in the UI but are not wired to `NOUS_CONFIDENCE_THRESHOLD` /
  `NOUS_DECAY_RATE_PER_WEEK` on the backend as of this writing; the dashboard
  falls back to its hardcoded defaults (0.8 / 0.05) if the config section
  doesn't provide them.

If any of the above changes, update this page (and the config/output/schedule
table) in the same change that lands the feature.

## Experimenter role and engine contract

RFC #11585 part 5 (#11698) adds the `experimenter` agent role and makes Nous
the default engine running through the `inline` dispatcher. The strategist
is unchanged, and nothing an experiment produces is promoted automatically.

**Role.** An agent named `experimenter`, or any agent with `role: experimenter`,
runs Experiment Lab campaigns. It gets the usual catalog defaults (⚗️, lane
keywords `experiment`/`campaign`/`hypothesis`/`benchmark`, chat alias `ex`),
the embedded policy template `experimenter.md`, and one extra field:

```yaml
agents:
  experimenter:
    role: experimenter
    engine: nous     # default; the only engine today
    interval: 24h
```

`engine` is only valid on this role — setting it on any other agent is a
load-time error rather than a silent no-op. The role is **not** added to any
ACMM pack; opt in by hand.

**Engine contract.** `pkg/nousengine` defines the `Engine` interface every
engine implements: `Invocation` (the command the agent runs), `Layout`
(where the ledger, principles, verdict, gates and other artifacts live),
`Progress`, `Principles` and `Verdict`. Nous is the registered default; an
empty engine name resolves to it. A `Fake` engine exists for tests only and
cannot be selected from config. The verdict vocabulary is
`deploy` / `deploy_with_caveats` / `fall_back_to_baseline`, and a verdict
carries `measured_against_baseline` so a score produced against a zero
baseline can never read as "better".

**Dispatcher.** `nous.dispatcher` (env `HIVE_NOUS_DISPATCHER`) selects how
Nous drives model calls: `inline` (default) runs the campaign loop
in-process; `sdk` is the legacy agent-SDK mode and remains selectable.
Precedence is env > yaml > default, and an unknown value falls through to the
next tier. Existing configs without the key resolve to `inline`. Inside
`bin/nous-runner.sh` a `NOUS_AGENT` already present in the agent's environment
still wins, so operators who pinned a mode before the hive owned the setting
keep it.

**What the agent sees.** Only experimenter-role agents receive
`HIVE_NOUS_ENGINE`, `HIVE_NOUS_DISPATCHER` and `HIVE_NOUS_DATA_DIR` at launch,
resolved by the hive with the same precedence its own readers use, so the
agent and the dashboard never disagree about where a campaign's files live.
Every other role launches exactly as before.

**Not in this part.** Dashboard campaign cards driven by the contract and any
pack inclusion of the role. Promotion of a winning configuration is part 6
([below](#verdict-card-and-manual-promote)).

## Pinned campaign dependencies

RFC #11585 part 7 (#11700) gives a campaign a controlled way to get the
things it needs that are not in the hive's own repo — a simulator such as
BLIS, workload traces, model configs — so results are reproducible and
nobody has to trust what the model decides to download.

**Declaring dependencies.** A campaign lists them under
`campaign.dependencies` (or top-level `dependencies`). Every entry is pinned:
repos by a full 40-hex commit, files by URL plus sha256. Short SHAs, tags and
branch names are refused.

```yaml
campaign:
  name: blis-routing-v1
  dependencies:
    - repo: inference-sim/inference-sim      # owner/name on github.com, or a full https URL
      commit: 8de28905b87474b870c56e1ecbd3c970532683b8
      role: target                           # becomes NOUS_TARGET_REPO_PATH / target_system.repo_path
    - url: https://huggingface.co/datasets/<org>/<name>/resolve/<rev>/trace.jsonl
      sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
      role: data                             # linked into worktrees via worktree_extras
```

`role` is `target` (at most one, repos only), `data` or `tool`; it defaults
to `target` for the first repo, `data` for files and `tool` for further
repos. `name` defaults to the repo or file name and becomes the
`NOUS_DEP_<NAME>` variable below.

**The setup step.** `bin/nous-runner.sh` runs `hive nous-setup` before
`run_campaign.py`. The step (`pkg/nousdeps`):

1. validates every declaration and checks every host against the allowlist
   **before fetching anything** — one bad entry ends the campaign with no
   bytes moved;
2. fetches repos with `git fetch --depth 1 <commit>` and files over https
   (redirect hops are checked against the allowlist too; downloads are
   capped at 2 GiB), into a cache under `<nous_dir>/cache/deps/` —
   `repos/<host>/<owner>/<name>/<commit>` and `files/<sha256>/<basename>` —
   or under `$HIVE_AGENT_CACHE_DIR/deps` when that is set, so a repeat
   campaign re-uses verified content instead of re-downloading;
3. verifies the checked-out commit and the file hash against the pin, and
   deletes anything that does not match;
4. records what it resolved, plus toolchain versions (installed Nous commit,
   Python, Go, git), in `<nous_dir>/<run-id>/dependencies.json`, and writes
   `dependencies.env` next to it with `NOUS_DEP_<NAME>=<path>` for every
   dependency and `NOUS_TARGET_REPO_PATH` for the target. The runner sources
   that file so the campaign can reference the paths.

A refusal or verification failure leaves no record and exits non-zero; the
runner then refuses to start the campaign. A campaign that declares no
dependencies still gets a record with the toolchain versions, so every run
says exactly which Nous it ran. Once setup finishes the agent needs no new
network access; the runner itself performs no fetch of any kind.

**Allowlist.** `nous.fetch_allowlist` is a list of exact lowercase DNS
hostnames (no wildcards, schemes, ports or IP literals — the same shape as
`runs.spektacular.recheck.egress_allowlist`). The default is `github.com`.
`HIVE_NOUS_FETCH_ALLOWLIST` (comma-separated) overrides it; precedence is
env > yaml > default. An explicitly empty yaml list denies every fetch. The
hive resolves the list once and exports it to experimenter agents under the
same variable, so `hive nous-setup` enforces what the operator configured.
`GET /api/nous/status` reports it under `dependencies.fetch_allowlist`
together with the cache directory and record path.

**Guards.** `pkg/nousdeps` guard tests and section 8 of
`src/deploy/test_supply_chain_pins.sh` fail CI if the Nous image or install
pin stops being a full commit, if `bin/nous-runner.sh` stops running
`hive nous-setup` before `run_campaign.py`, or if a bare
`curl`/`wget`/`pip install`/`git clone` appears in the runner.

**Not in this part.** Rewriting `target_system.repo_path` /
`worktree_extras` inside the campaign file (campaigns reference the exported
`NOUS_TARGET_REPO_PATH` / `NOUS_DEP_*` paths instead), a declared build step
for fetched repos, per-campaign narrowing of the hive-wide allowlist, cache
size limits and cleanup, hash-pinning the Nous Python packages themselves
(upstream ships `uv.lock` only; the commit pin stands), and enforcing the
allowlist at the network layer in Docker mode.

## Verdict card and manual Promote

RFC #11585 part 6 (#11699). When a campaign has produced its `Verdict`
(Nous: `deployment_recommendation` in `<nous_dir>/<campaign>/meta_findings.json`,
read through the engine contract above), the Experiment Lab panel shows a
**verdict card**, and the owner may **promote** it. Nothing is promoted
automatically — the `/hive decision` on the RFC makes this binding: promotion
is a manual, owner-only, audited step, and the card is the only place it
happens.

### What the card shows

- The recommendation as a status chip: `deploy` (ok), `deploy_with_caveats`
  (warn) or `fall_back_to_baseline` (error), plus the campaign name.
- **Score vs. baseline** when the campaign measured against one. When it did
  not (`measured_against_baseline: false` — no objective, or no baseline
  measurements, RFC §4), the card shows the score alone with a
  **not measured against baseline** chip, because that score says nothing
  about whether the change beats what is running today. The same flag and
  wording are carried into any issue the verdict is promoted to.
- Summary, evidence (`citations`) and `caveats` from the engine, and the
  campaign report path when `report.md` exists beside the verdict.
- Each promotion already recorded (who, when, the issue link or the
  Spektacular run key), and one **Promote** button per target that has not
  been promoted yet. Buttons for promoted targets are disabled.

The verdict rides inside `GET /api/nous/status` as `verdict`:

```json
{
  "verdict": {
    "campaign": "governor",
    "recommendation": "deploy_with_caveats",
    "score": 0.82, "baseline": 0.75, "measured_against_baseline": true,
    "citations": ["ledger#12"], "caveats": ["only two rounds"],
    "summary": "…", "target_repo": "acme/widgets", "report": "/data/nous/governor/report.md",
    "targets": ["issue", "spektacular-run"],
    "promotions": { "issue": { "promoted_by": "alice", "promoted_at": "…", "repo": "acme/widgets", "number": 77, "url": "…" } }
  }
}
```

The key is absent while the campaign has no verdict. `NousState` and the
`/api/nous/*` paths are otherwise unchanged.

### Promote targets

`POST /api/nous/promote {"target": "issue" | "spektacular-run", "repo": "owner/name"}`
— **owner only**. The dashboard asks for confirmation in its own dialog first
(no native browser dialogs). `repo` is optional: it defaults to the campaign's
target repo, then to the hive's single configured repo; a bare name is
qualified with the project org. If none of those resolve, the request is a
`400` and the owner must name the repo.

| Target | What happens |
|---|---|
| `issue` | Opens one issue in the target repo titled `Experiment Lab verdict: <recommendation> (<campaign>)` carrying the recommendation, score (or the not-measured flag), evidence, caveats, report path and who promoted it. `output.autoHold` adds the `hold` label. Audited as `hive_issue_created` (flow `nous-promote`). |
| `spektacular-run` | Opens the verdict issue (or reuses the one an earlier `issue` promotion opened) and admits it as a Spektacular run at the `spec` stage — the same path as `POST /api/runs/spec`; the run follows the normal review pipeline and nothing merges on its own. Requires `runs.spektacular.enabled` (`409` otherwise) and the run lease registry (`503` otherwise); both are checked before any issue is opened. |

Responses: `200 {"ok": true, "status": "promoted", "promotion": {…}}`;
`409 {"status": "already_promoted", "promotion": {…}}` when the current
verdict was already promoted to that target (idempotent — the second call
opens nothing); `400` for a bad body, unknown target or unresolvable repo;
`409` with `no verdict to promote` when the campaign has none; `502` when the
forge rejects the issue; `500` when the promotion cannot be recorded.

Every call is audited: `nous_promote` with `campaign`, `target`,
`recommendation`, `repo`, `number` and `run_key` on success;
`nous_promote_failed` with the reason otherwise. Promotions are appended to
`<nous_dir>/promotions/history.jsonl` (`pkg/nousstore`) and mirrored in
`NousState.Promotions`, so they survive restarts and the card stays
consistent with what actually happened.

### What changed from the earlier output modes

Before this part, `PUT /api/nous/config/output {"mode": …}` chose an
automatic output (`issue`, `issue+pr`, `spektacular-run`) and
`POST /api/nous/approve` acted on it. That path is removed: Approve records
the decision only, the config dialog's Output section keeps just the label
toggles, and the only way a verdict reaches an issue or a run is the owner
pressing Promote.

**Not in this part.** An owner notification when a new verdict lands, the
link to an artifact PR (part 8), and verdicts for campaigns other than the
hive's own governor campaign.

## Optional GitHub artifact repository

RFC #11585 part 8 (#11701). Off by default: a hive with no
`nous.artifact_repo` behaves exactly as before and
`GET /api/nous/status` reports `artifact_repo: {"enabled": false}`.

**What it does.** When enabled, the hive mirrors each *finished* campaign
folder under `<nous_dir>` (`/data/nous`) to a GitHub repository the
operator owns, one folder per campaign, delivered as a pull request so
the owner reviews before anything lands on the configured branch. The
repository — not the pod disk — is then the durable record (RFC §5). The
`governor/` scope folder (rolling `ledger.json` / `principles.json`) is
mirrored the same way whenever it has changed and been quiet; its status
is `ledger`.

**Who pushes.** The hive itself, through its existing GitHub App
installation identity (`pkg/github`: ETag cache, request accounting,
effects boundary, GitHub-signed commits via `createCommitOnBranch`).
The experiment agent never gets a token and the owner's
`/data/gh-user-token` is never used. Every push and every failure is an
audit entry (`nous_artifact_push` / `nous_artifact_push_failed`, user
`hive`) with campaign, status, branch, commit, file counts and PR URL.

**When a campaign is pushed.** Once, at the end: either `meta_findings.json`
exists (status `completed`) or the folder has not been written for
30 minutes (status `incomplete` — failed and abandoned runs are kept, as
the RFC requires). A folder that is still being written is left alone.
A campaign whose content changes after it was mirrored (for example a
late `dependencies.json`) is pushed again to the same branch; unchanged
campaigns cost no API calls beyond the periodic scan of local disk.

**Layout in the repository.** `<path>/<campaign-id>/...` on branch
`nous/artifacts/<campaign-id>`, with a `hive-sync.json` manifest listing
every file's sha256, the files omitted and why, and how many redactions
were applied. The commit headline is deterministic:
`nous: mirror campaign <id> (<status>, <n> files)`. The PR opens against
`nous.artifact_repo.branch` or the repository's default branch; an
existing open PR for the same branch is reused, never duplicated.

**What is never pushed.** `snapshots/`, `gates/`, `cache/` and hidden
entries at the root; `.git`, `__pycache__`, `node_modules`, `venv`
directories; symlinks; and any file whose name matches the denylist
(dotfiles, `*.tmp`, `*.env`, `*.pem`/`*.key`/`*.p12`/`*.pfx`/`*.jks`,
`id_rsa`-style keys, `.netrc`/`.npmrc`/`.pypirc`, and names containing
`token`, `secret`, `credential` or `password`). Text files are passed
through the same secret scrubber the hive uses for logs before upload;
the manifest records the count. Caps: 2 MiB per file, 20 MiB and 400
files per commit; anything over is listed in the manifest as omitted,
never silently dropped.

**Budget and failure handling.** The mirror is optional work: it skips a
pass whenever the API budget governor is not in `normal` mode and says so
in `status.artifact_repo.last_skip_reason`. API errors back off
exponentially from 1 minute to 1 hour (per campaign failures do not block
other campaigns). A public destination repository is a warning in the log
and `status.artifact_repo.public: true`, not a block. The sync state is
`<nous_dir>/artifact-sync-state.json`; deleting it only causes a re-scan,
and unchanged campaigns are recognised by content fingerprint.

**Configuration.**

```yaml
nous:
  artifact_repo:
    repo: my-org/nous-artifacts   # owner/name the hive's App is installed on
    branch: main                  # optional; default: the repo's default branch
    path: campaigns               # optional path prefix inside the repo
    sync_interval_seconds: 300    # optional; minimum 30
```

`HIVE_NOUS_ARTIFACT_REPO`, `HIVE_NOUS_ARTIFACT_BRANCH`,
`HIVE_NOUS_ARTIFACT_PATH` and `HIVE_NOUS_ARTIFACT_SYNC_INTERVAL` (Go
duration) override the yaml; precedence is env > yaml > default. The App
installation needs `contents: write` and `pull_requests: write` on that
repository.

**Status and manual sync.** `GET /api/nous/status` carries
`artifact_repo` with `enabled`, `repo`, `branch`, `path`, `public`,
`last_attempt_at`, `last_sync_at`, `last_error`, `last_skip_reason`,
`next_attempt_at`, `consecutive_failures` and a per-campaign map of what
was pushed (branch, commit, PR, file count, fingerprint). The owner-only
`POST /api/nous/artifacts/sync` runs one pass immediately — it bypasses
the retry backoff but never the API budget — and returns the pass result;
it answers 404 when no artifact repository is configured.

**Not in this part.** Git LFS for large artifacts (over-cap files are
listed in the manifest instead), deleting mirrored folders when a campaign
is removed locally, per-checkpoint pushes during a running campaign, and
letting the experiment agent push on its own.
