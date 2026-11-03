# Pre-registration: does Hive improve outcomes, and at what cost? (2026-10)

**Status:** registered 2026-10-15. This file is never edited after registration.
Amendments go in a new, dated file in this directory that references this one.

**Registered by:** Andy Anderson (Hive owner/operator).
**Collaborators:** Tamar Eilam, Fabio Oliveira (IBM Research).
**Tracking:** RFC hivecommons/hive#12062 and its child issues.
**Run plans (navigation only, not amendments):** [`ablation-plan-2026-10.md`](ablation-plan-2026-10.md) for §Experimental arms.

## Why pre-register

Tamar Eilam's assessment *Issues and Failures with AI Coding and Hive*
(2026-10-14) classifies 19 AI-coding failure modes and rates Hive's protection
against each. Its central caveat is that **none of the ratings is backed by
measured effectiveness**. We intend to publish. Writing the hypotheses, metrics
and exclusion rules down *before* looking at the numbers is the only way the
result is credible whichever way it goes — and we already expect to lose on at
least one hypothesis.

## Guardrail

Nothing in this study changes Hive's merge behaviour. Auto-merge remains
"CI green + review-swarm eligibility" on every hive. New gates run only in
**shadow mode** (record what they *would* have held; never block). Experimental
arms are opt-in per hive, 30 days, reversible.

## Hypotheses

Each hypothesis names its direction, its metric and what we expect.

| ID | Hypothesis | Metric | Expectation |
|---|---|---|---|
| H1 | Hive-managed repos have a lower revert/follow-up-fix rate on agent-authored PRs than the naive-agent baseline reported in the literature (He & Miller; Faros via Farrag) and than human-authored PRs in the same repos. | `reverted_14d / merged` per author kind (revert / follow-up recorder, #12067) | **Expect to hold** for revert rate; uncertain vs humans. |
| H2 | Cyclomatic complexity and static-analysis warnings on Hive-managed default branches rise at a rate indistinguishable from He & Miller's +41.6% finding for naive Cursor adoption. | monthly complexity index and warning count per repo (#12072) | **Expect to hold — i.e. we expect to lose.** Nothing in Hive addresses D1 today. A positive result here is the motivation for complexity ratchets, not a refutation of the paper. |
| H3 | A reviewer from a different model family than the PR author has a lower false-approve rate than a same-family reviewer. | false-approve rate per (author family, reviewer family) from `/api/reviewer/accuracy` and evidence records (#12065) | Expect to hold; today untestable (one reviewer family, author model not stamped). |
| H4 | Under load the governor throttle keeps the fleet's issue open:close ratio at or below 1.5 per day. | issues opened / issues closed per day, from hub audit; baseline incident #9350 = 119:31 (3.8) | Expect to hold. |
| H5 | Higher ACMM level increases per-PR wait time (time from open to merge) because the queue, not the human, becomes the bottleneck. | median time-to-merge per ACMM level, within repo across level transitions (`/api/pr-throughput`) | **Expect to hold — a negative result for Hive.** Reported either way. |
| H6 | Review-swarm confidence is calibrated: PRs in confidence bucket 5 have a lower bad-outcome rate than bucket 0–2. | `/api/reviewer/accuracy` confidence buckets | Expect to hold (Bluefin, pre-registration glance: bucket 5 = 12% bad, bucket 0 = 19%; this glance is disclosed and is why H6 is listed as confirmatory, not exploratory). |
| H7 | Shadow review-gating would have held a material share of PRs that merged and did *not* produce a bad outcome (i.e. gating by default would cost throughput for little safety). | `would_have_held` vs actual outcome (#12068) | Expect to hold; informs whether gating ever becomes default. |

## Metrics and sources

- **Revert / follow-up rate** — proxy for DORA change-failure rate. Source: revert / follow-up recorder writing `revert_of` / `followup_of` into `pkg/evidence` records. Labelled as a proxy everywhere it is shown.
- **Reviewer accuracy and calibration** — `/api/reviewer/accuracy?days=N`; false-approve = approved then bad outcome, false-block = blocked then merged cleanly by humans.
- **Author kind and model** — `evidence.author.kind` (human / agent / bot) and, once #12065 lands, `evidence.author.model`.
- **Throughput and wait** — `/api/pr-throughput` (opened, merged, closed, median time-to-merge, actor matrix).
- **Complexity / warnings** — monthly report-only snapshot per repo (#12072); backfilled from git history at monthly tags.
- **Boundary enforcement** — per-tier attempted vs blocked agent writes (#12069); sentinel hits from evidence records.
- **Work balance** — issues opened by human vs agent, issues closed, governor throttle state, from the hub audit log.
- **ACMM level over time** — hub timeline / audit.

## Populations and exclusion rules

Included hives/repos must have ≥ 10 merged PRs in the analysis window and a real
project (not an available/placeholder slot).

| Population | Role | Notes |
|---|---|---|
| Project Bluefin (19 repos, L5) | **primary case** | External maintainers decide merges; clear pre-Hive baseline; evidence store already holds 301 human-, 156 bot-, 29 agent-authored PRs. |
| KubeStellar Console (L6) | ACMM-as-dose series | AI-assisted from day one — no "before AI"; has recorded L0→L6 transitions. |
| hivecommons/hive | breadth, flagged | Hive developing Hive is circular; reported separately, never pooled. |
| tunaos (43 repos), Frostyard, Open Horizon | breadth | Included if they meet the ≥ 10 merged PR rule. |
| Available / placeholder hives | **excluded** | No project, no PRs. |
| External control arm (3–5 repos using agents with no harness) | control, if recruited | If not recruited by study start, the study is observational and says so. |

## Analysis plan

- Windows: 30 / 90 days rolling, plus monthly series for complexity.
- H1, H6, H7: rates with Wilson 95% intervals; human vs agent compared within repo.
- H2: slope of complexity per month before vs after Hive adoption (Bluefin) and per ACMM level (Console); compared with He & Miller's reported effect size.
- H3: false-approve rate per (author family, reviewer family) cell; cross- vs same-family difference with interval; requires the opt-in cross-family arm.
- H4: daily open:close ratio, with throttle-active days marked.
- H5: median time-to-merge per ACMM level within repo; difference-in-differences across the transition date.
- Instrumentation drift: any metric whose logging changed during the window is footnoted with the change date (example already known: hub audit `mode=daily` was logged while effective upgrade behaviour was instant, 2026-10-14).

## Experimental arms (opt-in, 30 days, reversible)

1. **Cross-family reviewer** — consenting hives (Bluefin, Console, hive) run at least one review perspective on a different model family from the author. Adds per-review cost; adds no human step; auto-merge unchanged.
2. **Shadow gates** — review-gated merge and blocking intent verification in shadow mode on the same hives. Records only; blocks nothing.

Assignment by hash of hive id where more than one consenting hive exists.

## Known confounders (disclosed)

- Most adopters are close to the Hive authors.
- Model upgrades occur during the window and are not controlled.
- Hive's own repo is both subject and instrument.
- Audit log and evidence schema have changed over time; legacy records lack reviewer model and CI capture.
- Acceptance rate at L5/L6 is partly the gate approving itself; human-reviewed and auto-merged PRs are reported separately and acceptance rate is not used as an outcome.

## What we will report regardless of result

H2 and H5 are expected to come out against Hive. They are reported with the
same prominence as any result in Hive's favour. The limitations above are
published as written.
