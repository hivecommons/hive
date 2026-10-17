# Hive-self background experiment mode (Nous)

This guide documents the **Hive-self background experiment mode** within the Nous experiment framework: what it is, the outcome questions it answers, how it differs fundamentally from repository-scoped experiments run in the Experiment Lab, its opt-in background-only boundary, and its guardrails and auto-revert mechanisms.

> [!IMPORTANT]
> **Delivery status:** This guide describes the Hive-self design, not a fully delivered runner. The opt-in and heartbeat feature-flag delivery seam is tracked in [PR #12421](https://github.com/hivecommons/hive/pull/12421) and is not yet available on `v5`. Cohort scheduling and fast-fail evaluation are separate work; image-channel switching is not implemented by this seam.

---

## What is Hive-self experiment mode?

While the [Experiment Lab](strategy-lab.md) focuses exclusively on repo-scoped experiments (optimizing repository code, tests, CI pipelines, and automation scripts), **Hive-self experiment mode** is a background-only experimentation runner designed to evaluate and improve **Hive the product itself**.

Instead of testing repository code modifications within a single project, Hive-self experiments test code and behavioral changes to Hive's own agent system, scheduler, prompts, cadences, and model assignments. Hypotheses originate from cross-hive recurring observation patterns, and evaluation occurs across consenting hives grouped into matched cohorts.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                       NOUS EXPERIMENTATION MODES                            │
├──────────────────────────────────────┬──────────────────────────────────────┤
│      REPO-SCOPED (Experiment Lab)    │       HIVE-SELF (Background Mode)    │
├──────────────────────────────────────┼──────────────────────────────────────┤
│ Target: Repository code & CI         │ Target: Hive itself (code, flags)    │
│ Scope: Single repo / spoke           │ Scope: Cross-hive cohorts (opt-in)   │
│ UI: Spoke Dashboard Experiment Lab   │ UI: Never on spoke; Hub owner view   │
│ Gates: Spoke operator approval       │ Gates: Fast-fail bounds, auto-revert │
│ Reports: Local repo PRs / issues     │ Reports: hivecommons/hive self-report│
└──────────────────────────────────────┴──────────────────────────────────────┘
```

---

## Core differences: Repo-scoped vs. Hive-self experiments

The boundaries between repo experiments and Hive-self experiments are strictly enforced by architecture:

| Dimension | Repo-scoped Experiment ([Experiment Lab](strategy-lab.md)) | Hive-self Background Experiment Mode |
|---|---|---|
| **Subject** | Repository files, CI workflows, build scripts, tests | Hive product behavior, agent cadences, model routing, feature flags; image-channel trials remain future work |
| **Visibility** | Spoke dashboard (Experiment Lab panel at ACMM L4+) | **Hidden from spoke dashboard**; zero spoke UI presence |
| **Execution Surface** | Foregrounds in operator UI; proposals reviewed in dashboard | **Background-only scheduler**; runs silently on opt-in worker hives |
| **Population** | Single local repository | Matched cohorts of consenting hives (matched on ACMM level, repository count, workload density) |
| **Control / Delivery** | Local `/api/nous/*` endpoints, dashboard config | Planned hub heartbeat-delivered feature flags; no release channel assignments |
| **Output / Reporting** | Local repo pull requests, issues, or state-only ledger | Aggregated fleet scorecard, Hub owner evidence view, and upstream reports to `hivecommons/hive` |
| **User Role** | Repository operator directly reviews and applies proposals | Spoke owner opts in to compute contribution; no day-to-day management |

> [!IMPORTANT]
> **Spoke dashboard boundary:** Users only ever see repository-level experiments in the Experiment Lab. Hive-self experiments never surface on spoke dashboards, spoke experiment listings, or local `/api/nous/status` responses.

---

## User and background boundary

The Hive-self runner respects strict operational and privacy boundaries:

1. **Strictly opt-in:** A hive never participates in Hive-self experiments without explicit operator consent. The feature-flag delivery seam in [PR #12421](https://github.com/hivecommons/hive/pull/12421) uses `HIVE_SELF_EXPERIMENTS_OPT_IN=true` in the spoke process environment as its local consent mechanism (default off; the hub cannot opt a spoke in). This variable is not available on `v5` until that implementation lands. Opting in permits assignments; it does not itself start an experiment.
2. **Hidden from spoke dashboards:** Spoke dashboards show only repo experiments. The Experiment Lab panel, sidebar items, and local status endpoints filter out Hive-self execution state.
3. **Never on spoke dashboards or spoke repos:** Hive-self experiments never open issues or pull requests on the spoke's managed repositories.
4. **Delivered via heartbeat (planned):** The feature-flag delivery seam carries assignments through the hub heartbeat control plane. It does not assign `candidate` or `stable` image channels.
5. **Reported upstream to `hivecommons/hive`:** Aggregated results, observations, and telemetry are transmitted to `hivecommons/hive` using deduplicated self-reporting rather than local noise. Cross-hive trends and effectiveness scorecards are visible exclusively in the hub owner evidence view.

---

## Outcome questions answered by the runner

Beyond low-level parameter tuning, the Hive-self background runner is designed to answer fundamental, outcome-level questions per agent, per hive, trended over time and compared across the fleet:

| Outcome Question | Snapshot Metrics Used | Example Experiment |
|---|---|---|
| **Is quality improving?** | Test coverage trend, regression escapes, lint/vet passes | Quality agent on/off, stricter test verification gates |
| **Is security improving?** | Open Dependabot/govulncheck alerts, time-to-remediate CVEs | Security agent cadence (daily vs continuous), automated patch depth |
| **Are reviews useful and accurate?** | Review verdict accuracy vs human merge decision, false positive rate | 2 vs 3 review perspectives; excluding author model family from review swarm |
| **Is auto-merge efficient?** | Time from green checks to merge, required-checks latency, merge failure rate | Required-checks-only sweep vs full-matrix evaluation gating |
| **Is outreach effective?** | New external contributors engaged, response rate, PR conversion rate | Outreach agent on/off, intro message variants, antispam cooldown windows |
| **Is each agent effective?** | Per-agent output per kick, kicks without output, token efficiency per merged PR | Per-agent cadence tuning, prompt optimization, model backend swaps |
| **Are users satisfied?** | Net Promoter Score (NPS) card responses, human override frequency, outreach replies | NPS prompt timing and frequency, tone/verbosity adjustments |

### The User Satisfaction signal

Historically, the Fleet quadrant's Satisfaction axis had no direct signal (`quadrant_axes.go:satisfactionCriteria` returned `nil`). Hive-self experiments incorporate user satisfaction by pairing:
1. **Direct NPS signal:** Feedback ratings and comments collected via the opt-in dashboard NPS prompt (see [NPS feedback prompt](nps.md)).
2. **Indirect satisfaction indicators:** Human override frequency (reverting an agent action, closing a bot PR without merge) and external developer outreach sentiment.

Paired with the agent-effectiveness scorecard, this answers the dual criteria: **"Is the agent effective, and is it wanted?"**

---

## Cohorts, windows, and flags

### Cohort matching
Hive-self experiments run across pools of opt-in hives. To ensure statistical validity, hives are partitioned into balanced cohorts matching key characteristics:
- **ACMM Maturity Level:** Ensuring L3 hives are not compared directly against L6 hives.
- **Backlog & Activity Density:** Queue depth, issue creation frequency, and PR arrival rates.
- **Repository Characteristics:** Monorepo vs multi-repo, languages, and CI run durations.

### Experiment windows
Every Hive-self experiment runs through three distinct temporal phases to avoid sample bias and seasonal noise:
- **Baseline Window (`baseline_hours`):** Pre-experiment observation window (typically 4–48 hours) to capture the control group's baseline metrics.
- **Experiment Window (`experiment_duration_hours`):** Active intervention window where the candidate code, prompt, or feature flag is enabled.
- **Cooldown Window (`cooldown_hours`):** Mandatory cooldown pause (typically 2–24 hours) after reverting or terminating the experiment, ensuring lingering background tasks or CI queues do not contaminate subsequent trials.

### Feature-flag gating and release-channel boundary
The delivery seam proposed in [PR #12421](https://github.com/hivecommons/hive/pull/12421) provides heartbeat-delivered **feature flags only**, gated by explicit local opt-in:
- Background runtime consumers can use the assigned flag to gate behavior without editing local `hive.yaml` configuration files. The seam does not itself implement cohort scheduling or fast-fail evaluation.
- Withdrawal, an explicit disable, or local opt-out clears the previous assignment when the heartbeat response is applied.
- Experiments do **not** automatically assign or switch `candidate` vs `stable` image channels. Channel switching remains deferred to the separate image-upgrade protocol; do not expect image-channel A/B trials or image rollback from feature-flag delivery.

---

## Guardrails and auto-revert

Hive-self experiments operate in production environments, making safety and autonomy boundaries paramount:

1. **Pre-registered thresholds:** Hypotheses, minimum detectable effects (MDE), and metric thresholds are pre-registered before experiment execution starts.
2. **Fast-fail triggers:** The background scheduler continuously monitors health indicators during the active window. An experiment immediately fast-fails and triggers auto-revert if:
   - **SLA Spike:** Task queue MTTR or actionable backlog exceeds pre-registered safety limits (e.g. `mttr_max_minutes` exceeded by >20%).
   - **Token Blow-up:** Token burn rate spikes beyond allowable variance (`budget_burn_rate_max_pct`).
   - **Error Streaks:** Repeated agent crashes or failed GitHub API calls occur in succession.
3. **Feature-flag auto-revert (design):** The scheduler is intended to disable or withdraw the assigned flag on fast-fail detection. The delivery seam applies that change on the next heartbeat response, not instantaneously. Fast-fail evaluation is separate scheduler work; this mechanism does not roll back an image or release channel.
4. **Isolated blast radius:** Because Hive-self experiments cannot write to spoke repository code or merge unvetted changes, an experiment failure cannot corrupt customer repositories.

---

## Where to see results

- **Spoke operators:** Spoke operators see only normal repository operations and local Experiment Lab studies on their own code.
- **Hub owners:** Hub administrators review aggregated results, fleet scorecards, and trend lines in the hub owner-only evidence view (`/fleet/evidence`).
- **Hive maintainers:** Completed experiments and validated principles publish summary observation findings upstream to [hivecommons/hive](https://github.com/hivecommons/hive) issues and discussions.
