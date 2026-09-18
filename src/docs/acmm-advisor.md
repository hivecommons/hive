# ACMM level-up advisor

The ACMM advisor is a pure, read-only computation that answers one question: *has this hive earned progression to the next ACMM level?* It implements the "you earn automation with test coverage" thesis from the [CNCF reference architecture](cncf-reference-architecture.md) (`src/docs/cncf-reference-architecture.md:174`) as a concrete, auditable checklist, and is exposed over the dashboard API as `GET /api/acmm-recommendation` (see the [API reference](api-reference.md)).

**This page documents what exists in `pkg/acmmadvisor` today.** It is advisory-only and is not currently wired into the dashboard UI — see [What this does NOT do](#what-this-does-not-do) below.

## What it measures

The advisor evaluates a fixed set of six signals (`Signals` struct, `src/pkg/acmmadvisor/acmmadvisor.go:124-144`):

| Signal | Meaning |
|---|---|
| `CurrentLevel` | The ACMM level the hive is applying right now (1–6). |
| `CoveragePct` | Current test-coverage percentage (0–100). |
| `GreenStreak` | Count of consecutive green CI runs with no red, measured from default-branch Actions history. See below. |
| `MergeSuccessRate` | Fraction (0.0–1.0) of recent PRs that merged cleanly. |
| `ActionableIssues` | Count of open actionable issues the hive has surfaced but not yet resolved. |
| `HoldCount` | Count of open PRs still carrying a `hold` label awaiting human review. |
| `HasQualityAgent` | Whether an active quality/coverage agent is present in the config. |

Levels run L1 (Assisted) through L6 (Fully Autonomous), matching the level pack files and the dashboard's 1–6 bound (`src/pkg/acmmadvisor/acmmadvisor.go:34-42`). The advisor never proposes a target more than one level above the current level — advancement is always incremental (`levelStep = 1`, same file).

### Where the live signals come from

The dashboard assembles `Signals` for the running hive in `buildACMMStatusInputs` (`src/pkg/dashboard/api_acmm_recommendation.go:47-106`):

- `CurrentLevel` and `HasQualityAgent` come from the live config (`detectACMMLevel`, presence of an agent named `quality` in `cfg.Agents`, via `hasQualityAgent` at `src/pkg/dashboard/api_acmm_recommendation.go:126-132`).
- `MergeSuccessRate` is read from the fleet-stats collector's cached 90-day merged/rejected counts (`mergeSuccessRateFromFleetStats`, `src/pkg/dashboard/api_acmm_recommendation.go:135-141`) — no fresh GitHub call is made on the request path.
- `ActionableIssues` and `HoldCount` come from the most recent published status snapshot (`status.Governor.Issues`, `status.Hold.Total`).
- `CoveragePct` is read from `status.AgentMetrics["ci-maintainer"]["coverage"]`, the value the coverage badge collector populates (`coverageFromAgentMetrics`, `src/pkg/dashboard/api_acmm_recommendation.go:148-166`).
- `GreenStreak` is the count of consecutive non-red completed runs on the primary repo's **default branch**, counting back from the most recent run (`Client.GreenCIStreak`, `src/pkg/github/health.go`). It is measured on the status-build path — the same pass that already fetches workflow health — and cached, so no fresh GitHub call is made on the request path. When no measurement has ever succeeded (no GitHub client, an Actions API failure, or a repo with **no CI history at all**) the signal stays at zero and reads as *unknown / not yet earned* rather than as a measured zero — a repo with no CI must never read as green. A failed refresh leaves the last real reading in place rather than clobbering it with an unknown.

All of the above is nil-safe: a freshly-booted hive with no config, no status snapshot yet, or a nil fleet-stats collector collapses to conservative zero-value signals rather than panicking or fabricating data (comment at `src/pkg/dashboard/api_acmm_recommendation.go:41-45`, confirmed by `TestHandleACMMRecommendationEmpty` in `src/pkg/dashboard/api_acmm_recommendation_test.go`).

## Thresholds by target level

The advisor computes the checklist for advancing to the level **one above** the current one. Coverage floors rise monotonically and are anchored on the project's own enforced 90% coverage gate (`coverageGate = 90.0`, `src/pkg/acmmadvisor/acmmadvisor.go:52-63`):

| Target level | Quality agent | Coverage floor | Green-CI streak | Merge success rate | Actionable-issue ceiling | Open-holds ceiling |
|---|---|---|---|---|---|---|
| L2 (Instructed) | required | — | — | — | — | — |
| L3 (Measured) | required | ≥ 40% | ≥ 3 | — | — | — |
| L4 (Adaptive) | required | ≥ 60% | ≥ 5 | ≥ 70% | — | — |
| L5 (Semi-Automated) | required | ≥ 75% | ≥ 8 | ≥ 85% | ≤ 25 | ≤ 15 |
| L6 (Fully Autonomous) | required | ≥ 90% | ≥ 12 | ≥ 95% | ≤ 10 | ≤ 5 |

Source: `criteriaForTarget` and the threshold constants in `src/pkg/acmmadvisor/acmmadvisor.go:32-104,259-338`. All thresholds are compile-time constants; there is no config field or environment variable that overrides them (see [What this does NOT do](#what-this-does-not-do)).

If a hive is already at L6, the advisor reports `stay` with `Ready: true` and a rationale stating there is nothing higher to propose (`Recommend`, `src/pkg/acmmadvisor/acmmadvisor.go:206-217`).

## Reading a recommendation

A recommendation (`Recommendation` struct, `src/pkg/acmmadvisor/acmmadvisor.go:178-197`) has:

- **`Advise`** — `"stay"` or `"raise"`. `"raise"` is only ever emitted when *every* criterion for the target level is met.
- **`CurrentLevel`** / **`TargetLevel`** — the evaluated levels. When `Advise` is `"stay"`, they are equal (except at the L6 ceiling case above, where target also equals current).
- **`Ready`** — whether every criterion for `TargetLevel` is met.
- **`Met`** / **`Unmet`** — the satisfied and unsatisfied criteria as a renderable checklist. Each `Criterion` (`src/pkg/acmmadvisor/acmmadvisor.go:161-174`) carries a human label, a comparator (`>=`, `<=`, or `is`), the required and actual values formatted for display, whether it passed, and a one-line rationale for why the criterion exists.
- **`Rationale`** — a human-readable summary, e.g. `"Stay at L2. 2 of 3 criteria for L3 not yet met: Green-CI streak, Test coverage."`

## What this does NOT do

- **It never changes the applied ACMM level.** The package comment states this explicitly: "This package is ADVISORY ONLY. It never changes the applied ACMM level... A human always approves the actual level change." (`src/pkg/acmmadvisor/acmmadvisor.go:12-17`). The only code path that changes a hive's level is `PUT /api/packs/level` → `handlePackSetLevel` → `ApplyPack`, documented in [ACMM policy matrix — Changing a hive's ACMM level](acmm-policy-matrix.md#changing-a-hives-acmm-level). Nothing in `pkg/acmmadvisor` or the `/api/acmm-recommendation` handler calls that path.
- **It is a pure function with no I/O.** `Recommend` and `RecommendFromStatus` take already-collected signals and return a value; they do not read config, call GitHub, or touch the clock. Signal *collection* does talk to GitHub, but only on the status-build path, and its results are cached — neither the API endpoint nor the status payload triggers a GitHub call of its own.
- **It never auto-applies a recommendation.** The recommendation now travels on the status payload as `acmmAdvice` (#5225) in addition to the API endpoint, but both are read-only advice. Nothing consumes them to change a level.
- **Thresholds are not configurable.** All coverage/streak/rate/backlog numbers are Go constants in `pkg/acmmadvisor`; there is no `hive.yaml` field or environment variable to adjust them.
- **It does not measure streak quality, only streak length.** `GreenStreak` resets on **any** red, including a flake unrelated to code quality, and a repo that rarely commits can hold a stale streak indefinitely because the signal is count-based rather than time-based. The scan also reads at most one page of runs (`ciStreakRunsLimit`), so a very long streak is reported as "at least N" — the cap sits above every advisor threshold, so it can only ever under-report.

## The API endpoint

`GET /api/acmm-recommendation` returns the JSON-encoded `Recommendation` computed from the hive's live signals. See the full request/response contract in the [API reference](api-reference.md#packs-and-acmm).

## Open questions

- Whether the streak should become time-based ("days since last red") rather than count-based is unresolved. A count-based streak lets a quiet repo hold a stale value; a time-based one would change the meaning of the existing `greenStreakL3..L6` thresholds, so it is deliberately not attempted here.
- Whether a "green" streak should require the run to have actually executed gates is unresolved: `skipped` runs are currently transparent to the streak (matching `ciPassRate`), so a repo whose workflows are entirely skipped on the default branch could accumulate a streak without any gate ever running.
