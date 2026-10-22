# Evidence program

Hive's evidence cards describe recorded outcomes, not proof of effectiveness.
See [review evidence](review-evidence.md) for bundle storage, hashing, signing,
and export, and the [pre-registration](research/preregistration-2026-10.md)
for study hypotheses and exclusions.

## Control arm

Issue [#13532](https://github.com/hivecommons/hive/issues/13532) introduces a
within-repository counterfactual. Its prospective design is specified in the
[control-arm amendment](research/control-arm-amendment-2026-10-22.md).

### Reporting contract

An optional bundle field, `control_window`, records assignment **at PR open
time**, not the state at review, merge, export, or dashboard-render time:

- `on`: Hive dispatch enabled during the assigned window (treatment).
- `off`: Hive dispatch disabled during the scheduled window (control).
- Absent: unassigned or unknown. Legacy records are **not** implicitly `on`.

All heads of a PR must keep the same assignment. Folding preserves a known
assignment if a later head lacks it; conflicting or invalid assignments make
that PR unassigned for control reporting. A tag is covered by the bundle's
hash/signature like any other field. Never hand-edit signed bundles to create
a retrospective control population.

`GET /api/evidence/summary` includes `control_arm` after applying its usual
repository, author-kind and PR-open-time filters and review-outcome hints.
Each PR counts once across heads. `control_arm.by_repo` reports separate on/off
arms for each repository; repositories are never pooled into a causal contrast.
The outcome card displays these comparisons below its author-kind chart.

Each arm reports opened, merged, closed-without-merge, still-open and unknown
counts, plus these metrics with their own sample sizes:

| Metric | Denominator / observation |
| --- | --- |
| Merged, closed, open rates | PRs with a recorded outcome; unknowns excluded |
| Revert and issue-reopen rates (14 days) | Merged PRs, using the existing change-failure link rules |
| CI pass rate | PRs with captured settled green/red CI; absent or pending CI excluded |
| Median open-to-merge hours | Merges with forge open and merge timestamps; review-start time is not substituted |

`delta` is **on minus off**, in rate units (UI: percentage points), or hours
for the median. Each metric's delta is JSON `null` unless **both metric
sample sizes are at least 20**. Large arm totals do not override missing CI,
unknown outcomes, or absent timestamps. Raw values and counts remain visible.
An absent metric is unknown, not zero. The API also discloses the total number
of unassigned/conflicting PRs in the filtered window.

Opened counts have unequal exposure under the proposed three-on/one-off
schedule, so they are not compared as throughput deltas. A future collector
must supply exposure and complete forge coverage before throughput claims.
Revert/reopen rates are provisional until every merge has 14 days of follow-up;
missing links can still undercount failures. CI is the latest captured snapshot,
not necessarily CI at merge. These are descriptive, not causal, comparisons.

### Rollout status

This is the **reporting foundation**, not activation of an experiment. Current
runtime producers do not yet assign `control_window`; ordinary deployments
therefore show unassigned counts and no control comparison. There is no new
configuration switch to enable, and no change to dispatch, review, write gates
or auto-merge behaviour.

Remaining work on #13532:

1. Owner-only, default-off per-repo opt-in and a deterministic governor schedule
   (one off-week in four), with durable exposure/window history.
2. Independent forge observation of **all** PRs in both windows, including
   human PRs never reviewed by Hive. Persist prospective assignments on every
   evidence row, preserving assignment across heads and schedule changes.
   Review-only collection would selectively lose the control population.
3. Held-PR lifecycle and human-overlap tracking on already-Holdgated repos,
   reusing the duplicate-sweep matcher. Keep this observational stratum
   separate from the scheduled off-window experiment.
4. Exposure-normalized throughput, mature 14-day outcomes and study intervals;
   hub/fleet reporting of the same arm contract.

Shadow-gate observations remain separate: a gate's “would have held” result
is not evidence that Hive dispatch was off.
