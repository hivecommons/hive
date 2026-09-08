# ACMM waivers (`.acmm.yml`)

The ACMM evaluation detects capability by looking for files. Each criterion
carries a list of paths and passes if any of them exists in the repository
(`checkCriterion`, `src/pkg/dashboard/api_acmm_eval.go`). That is cheap,
language-agnostic and needs no cooperation from the repo — which is why it
works — but it cannot tell the difference between two very different repos:

- one that **never built** the capability, and
- one that **moved the capability somewhere the file check cannot see**.

Both read as a missing file. The second repo is penalised for an architectural
choice, and the cheapest way for its maintainer to restore the score is to put
the file back — which is the wrong outcome whenever the file was removed for a
reason.

A waiver is how a repository states the second case: *this capability exists,
here is where it lives, here is why it is not in this repo*. It is a
declaration about architecture, not an exemption from the model.

## The worked example

`Danathar/sensi` deleted `.github/workflows/ai-fix.yml`. The job it removed
ran an agent in-repository against a labelled issue or an `@claude` comment,
holding `contents: write`, `issues: write`, `pull-requests: write`,
`id-token: write` and an API key. Its label gate turned out not to be an
authorisation check at all — the `ai-fix-requested` label was applied
automatically by the issue-filing bot, so the gate admitted whatever that bot
filed rather than expressing a human triage decision. The same work was
already being done by hive, at ACMM L4, across one trust boundary instead of
two. Removing the workflow was straightforwardly the right call.

Two L4 criteria name that one filename:

| Criterion | Patterns |
|---|---|
| `acmm:ai-fix-workflow` | `ai-fix.yml`, `ai-fix-requested.yml`, `claude.yml` |
| `acmm:copilot-review-apply` | `copilot-review-apply.yml`, `apply-copilot.yml`, `ai-fix.yml`, `auto-review.yml` |

So one security fix cost the repo two criteria and took L4 from 9/9 to 7/9 —
still passing (0.78 against the 0.70 threshold) but no longer full green. The
capability had not gone anywhere; it had moved to hive.

Without waivers the available responses are all bad: re-add the file that was
removed for cause, broaden the criterion for every repo in the fleet, or leave
a permanent red that trains the operator to ignore reds. The general shape
recurs wherever a capability can live outside a repository — a shared
org-level reusable workflow, a platform team's CI, a service mesh doing what a
per-repo config used to do.

## Declaring a waiver

Commit `.acmm.yml` (or `.acmm.yaml`) to the repository root:

```yaml
waivers:
  - id: acmm:ai-fix-workflow
    satisfied_by: hive
    reason: >
      The in-repo AI-fix path was removed in PR #118 — it duplicated what
      hive already does at ACMM L4, across a second trust boundary. Hive is
      the sole autonomous path. See docs/SECURITY-AI.md.
  - id: acmm:copilot-review-apply
    satisfied_by: hive
    reason: Same removal; hive applies review feedback. See docs/SECURITY-AI.md.
```

All three fields are required. `id` must name a criterion that exists in
`universalCriteria`; `satisfied_by` must name what provides the capability
instead; `reason` must say why. A declaration missing any of them is dropped
and the criterion stays red — see [What a waiver cannot do](#what-a-waiver-cannot-do).

## What a waiver cannot do

The mechanism is deliberately unable to function as a way around ACMM
compliance. Five properties, each with a test in
`src/pkg/dashboard/acmm_waivers_test.go`:

1. **A waiver can never advance a level.** Level pass/fail is computed on
   *detected* criteria alone (`ACMMLevelScore.DetectedScore`, used for
   `Passed`); the displayed ratio includes waivers. A repo that waives an
   entire level shows a full ratio and still does not pass it, and its
   codebase level does not move. Waivers close the gap between *passing* and
   *full green* — nothing more.
2. **A waiver never overrides a detection.** It is consulted only when the
   file check has already failed, so a repo that later adds the real file
   stops depending on the declaration silently, and the stale entry becomes a
   no-op rather than a lie.
3. **A waiver must name a real criterion.** IDs are validated against
   `universalCriteria`, so a typo cannot sit in the file looking effective and
   a renamed criterion re-opens as a visible gap.
4. **A waiver must justify itself.** Missing `satisfied_by` or `reason` voids
   it. Without them the declaration says only "do not check this", which is
   the thing this must not become.
5. **A waiver is permanently visible.** It is marked on the criterion row, and
   with a red asterisk on the level line — the altitude operators actually
   read. It is also in git history, under review, attributable.

## How it reads on the dashboard

A waived criterion counts as passed, so tiles, bars and counts read green.
The row carries a `⌾ waived` chip and, when expanded, the `satisfied_by` and
`reason` verbatim above the pattern list — so a reader who opens a green row
whose patterns match nothing finds the explanation in the same place they
found the contradiction.

The level line carries a red asterisk whenever any criterion at that level is
waived, with a tooltip naming them. Red on purpose: it should interrupt
someone skimming for green. When a level's displayed ratio is full but the
level still does not pass, the tooltip gives the detected count and says that
waivers cannot advance a level.

In the multi-repo aggregate a criterion is marked waived only when *no* repo
detected the real thing — one repo holding the file makes the fleet-wide claim
true on its own.

## Cost

Effectively zero for repos that do not use it. `prefetchDirectories` already
lists the repository root, so a repo with no `.acmm.yml` is settled from that
cache with no additional GitHub call. Only a repo that actually declares
waivers pays for the content fetch. This matters because a full refresh is
already up to ~29 `GetContents` calls per repo inside a 20s per-repo timeout.

## Implementation

- `src/pkg/dashboard/acmm_waivers.go` — parsing, validation, fetch.
- `src/pkg/dashboard/api_acmm_eval.go` — `CriterionResult.Waived` and the
  waiver-blind `ACMMLevelScore.Passed`.
- `src/pkg/dashboard/static/index.html` — `acmmWaiverDetail`,
  `acmmLevelWaiverMark`.
