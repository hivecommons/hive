# Experiment Lab (Nous) operator guide

The Experiment Lab — formerly the Experiment Lab, internally and in the API
called **Nous** — is a controlled experiment framework for the governor. It records a baseline of hive behavior,
designs experiments against governor (and optionally repo) parameters, and
measures each one against that baseline before anything is applied for real.

**There is no `nous:` block in `hive.yaml` or `hive.yaml.example`.** Despite
what `src/README.md` used to say, Nous has no YAML config surface beyond the
`data.nous_dir` storage root — it is configured at runtime through the
dashboard (Experiment Lab panel, ACMM level 4+) or directly through the
`/api/nous/*` endpoints. This page documents
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

Once enabled, the section only appears at **ACMM level 4 and
above** (`ACMM_NOUS_MIN_LEVEL` in `pkg/dashboard/static/index.html`). Below
that level the panel and sidebar summary are hidden entirely, though the
`/api/nous/*` endpoints still respond. On a hive with no snapshots yet, the
panel shows an explainer card instead of empty stats.

## State and storage

All Nous state lives under one durable root on the PVC — `/data/nous` by
default, configurable with `data.nous_dir` and overridable at runtime with
`HIVE_NOUS_DATA_DIR` (RFC #11585 part 1, #11694). The only other Nous key in
`hive.yaml` is `nous.gate_poll_seconds` (see [Gate protocol](#gate-protocol)):

| What | Where (relative to `data.nous_dir`) | Written by |
|---|---|---|
| Experiment ledger | `governor/ledger.json` | the experimentation agent (Nous); read live — see [Live ledger and principles](#live-ledger-and-principles) |
| Learned principles | `governor/principles.json` | the experimentation agent (Nous); read live |
| Baseline snapshots | `snapshots/*.json` | one file per evaluation cycle, via `NousState.RecordSnapshot` |
| Pending gate question, owner decision, decision history | `gates/pending.json`, `gates/decision.json`, `gates/history.jsonl` | `bin/nous-hive-gate.py` writes `pending.json`; `PUT /api/nous/gate-decision` writes the other two (see [Gate protocol](#gate-protocol)) |
| Mode, scope, goals, output mode, fast-fail bounds, schedule, controllable knobs | `panel-config.json` (mirrors `NousState.Mode`/`Scope`/`Config`) | every `PUT /api/nous/mode`, `PUT /api/nous/scope` and `PUT /api/nous/config/*`; re-applied at boot |

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

## Experiment lifecycle

1. **Collecting** — a fresh hive starts in this phase. Every evaluation cycle
   that has a governor state writes a baseline snapshot
   (`/data/nous/snapshots/<epoch-ms>.json`) with queue depth, MTTR, SLA
   violations, agents kicked, and token totals. The `NousBaselineTarget`
   constant (672 snapshots) is treated as "enough baseline". The `baseline_pct` status
   field is `snapshotCount / NousBaselineTarget` capped at 100; the dashboard
   progress bar computes its own clamped value from `snapshotCount` and
   `snapshotTarget` rather than reading `baseline_pct`.
2. **Observing** — once at least one snapshot exists, phase auto-advances to
   `observing` (`refreshStatus()` in `pkg/dashboard/deps.go`).
3. **Framing → Design → Design Review → Executing → Analysis → Findings
   Review → Extraction** — the full per-scope phase sequence
   (`NOUS_PHASES` in the dashboard JS) that a live experiment iteration walks
   through. Phase and iteration are reported per scope (`governor`, `repo`)
   through `GET /api/nous/phase` and the `phases` field the governor process
   publishes into `NousState.Status`.
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
   A running experiment can be stopped early with the same Abort action,
   which reverts the governor to its pre-experiment configuration.

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
nothing. `suggest` mode — and `repo` scope in any mode — always waits for the
owner.

**Notification.** The owner is pinged through the configured notifier
(ntfy/Slack/Discord) once per gate id with the question; the `notified`
marker is written *before* the ping so a crash between the two drops a ping
rather than duplicating it.

## Trust ladder (mode)

Set with `PUT /api/nous/mode {"mode": "observe|suggest|evolve"}` (owner role
required). The dashboard's trust-ladder widget documents the three modes:

| Mode | Behavior |
|---|---|
| `observe` | Data collection only — governor configuration is never changed. |
| `suggest` | Nous designs experiments but every proposal requires operator approval via `nous_approve`/`nous_abort` before it runs. |
| `evolve` | Nous runs approved-pattern experiments autonomously and distills durable results into principles without per-experiment approval. |

## Scope

Set with `PUT /api/nous/scope {"scope": "governor|repo|both"}` (owner role
required):

- `governor` — experiments adjust governor parameters (cadences, model
  assignments, thresholds).
- `repo` — experiments propose repo code changes (tests, CI config,
  automation scripts). Repo-scope experiments always require approval
  regardless of trust level, per the dashboard's own scope description.
- `both` — alternates between governor- and repo-scope experiments.

## Configuring goals, output, and bounds

`GET /api/nous/config` returns the current in-memory config as JSON
(`campaign`, `repoCampaign`, `output`, `projectRepos`, `repoTargets`,
`principles`). Each section is written independently with its own `PUT`
endpoint, all requiring owner role and all triggering `refreshAndPersist()`:

| Endpoint | Section | Fields (dashboard defaults shown) |
|---|---|---|
| `PUT /api/nous/config/goals` | `goals` | Governor and repo research questions — free-text hypotheses (e.g. "Can we reduce MTTR by 20% by adjusting agent cadences during busy mode?"). |
| `PUT /api/nous/config/output` | `output` | Output mode — `state-only` (files + dashboard only), `issue` (opens a GitHub issue per proposal), `issue+pr` (issue and PR pair), or `spektacular-run` (approving a proposal admits its org-qualified `owner/repo#number` target as a Spektacular run at `spec`; a repeated approve returns the admitted run instead of admitting it again — see [Spektacular](spektacular.md)); plus `autoHold` and `autoDoNotMerge` booleans that apply the `hold` / `do-not-merge` labels to anything Nous creates. |
| `PUT /api/nous/config/repos` | `repos` | Which repos from `projectRepos` are eligible targets for repo-scope experiments. |
| `PUT /api/nous/config/fast-fail` | `fast_fail` | `queue_depth_max` (default 30), `mttr_max_minutes` (default 180), `budget_burn_rate_max_pct` (default 110) — see below. |
| `PUT /api/nous/config/schedule` | `schedule` | `experiment_duration_hours` (default 4), `baseline_hours` (default 4), `cooldown_hours` (default 2). |
| `PUT /api/nous/config/controllables` | `controllables` | Which governor knobs (cadences, models, thresholds) Nous is allowed to vary, each with an enabled flag and a type-appropriate range/enum. |
| `PUT /api/nous/config/principles` | `principles` | Principle-store settings (confidence threshold, decay rate) surfaced alongside the accumulated principle list. |
| `DELETE /api/nous/principles/{id}` | — | Invalidates (removes) a single learned principle. Owner role required. |

**Invariants** (governor parameters Nous will never touch, e.g. safety-
critical thresholds) are shown read-only in the config dialog and are not
settable through this API — they come from the campaign definition the
governor experiment loop enforces, not from an operator-editable section.

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
  everything above in one call — mode, scope, phase, per-scope phase detail,
  snapshot counts, and any pending proposal or active experiment.

If `deps.Nous` is `nil` (nous state failed to initialize), `status` returns
`{"status": "not_configured"}`, `ledger`/`principles` return empty arrays, and
mutating endpoints return `404 nous not configured`. The dashboard treats
`not_configured` as "show the explainer card," not an error.

## Worked example: enabling Nous on a hive

1. Confirm the hive is at ACMM level 4 or above (`HIVE_LEVEL` or the config
   pack), so the Experiment Lab section is visible in the dashboard sidebar.
2. Leave mode at the default `observe` and let the hive run — no action is
   needed here. Snapshots accumulate automatically once per evaluation cycle.
3. Open the Experiment Lab panel's gear icon to set a governor research
   question (`PUT /api/nous/config/goals`) describing what you want Nous to
   optimize.
4. Once the baseline progress bar reports sufficient coverage, switch mode to
   `suggest` (`PUT /api/nous/mode {"mode": "suggest"}`). Review each proposal
   that appears under "Pending Proposal" and Apply or Reject it.
5. Only move to `evolve` once you're comfortable with the proposals Nous has
   made in `suggest` mode — `evolve` runs approved-pattern experiments without
   per-experiment approval.

## What does not exist today

- The `nous:` block in `hive.yaml` carries only `gate_poll_seconds`. Goals,
  bounds, schedule and the trust ladder are **not** read from YAML — they live
  in `panel-config.json` under `data.nous_dir` and are set through the API.
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

**Not in this part.** Promotion of a winning configuration (part 6), dashboard
campaign cards driven by the contract, and any pack inclusion of the role.

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
