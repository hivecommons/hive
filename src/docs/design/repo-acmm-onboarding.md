# Automatic repo ACMM onboarding reconciler

Status: Declined (2026-09-09) — kept as a record of the decision.

> **Decision.** The maintainers closed RFC #6235 without adopting it, and RFC
> #6111 (per-repo ACMM levels under a hive-wide ceiling) is on hold: the hive
> keeps a single hive-wide ACMM level rather than a level per repository, so a
> reconciler that drives each repo toward its own target level has no policy
> to enforce. Nothing below is scheduled for implementation; reopen #6235
> before building on it.
>
> **Per-repo ACMM may still be reachable another way.** Declining per-repo
> *levels* is not declining per-repo *behaviour*. The hive already has
> per-repo primitives that compose without a second level model: off-repo
> criterion waivers (#6264, backported in #6395) let a repo declare a
> capability satisfied elsewhere; per-repo agent scope (#6215) limits which
> agents act on which repos; per-repo pause (#6211) removes a repo from
> autonomous dispatch. A future proposal that achieves per-repo maturity
> outcomes by composing primitives like these — keeping one hive-wide level as
> the ceiling and the single source of truth — would be reviewed on its own
> merits; see the discussion on #6111.

RFC credit: this design turns the adopter RFC in
[#6235](https://github.com/hivecommons/hive/issues/6235) into a reviewable plan
without implementing it. The RFC's key warning is preserved here: automation
must improve repositories, not merely game file-existence criteria.

Every code reference below was checked against `v4` while writing this page.
Line numbers may drift; the named symbols are the stable handles.

---

## Problem

A Hive has a configured ACMM level, and each repo can be evaluated against ACMM
criteria, but nothing reconciles a repo's measured level toward a per-repo
target. The result is a governance gap: a high-level Hive can run high-trust
agents against a repo that still lacks lower-level prerequisites, and the system
will display the gap without turning it into managed onboarding work.

The measurement side already exists. `ACMMCriterion` describes criteria with an
ID, level, category, and file patterns (`src/pkg/dashboard/acmm_criteria.go:4`),
and `universalCriteria` contains the repo-universal checks
(`src/pkg/dashboard/acmm_criteria.go:24`). The evaluator loops all configured
repos, calls `checkCriterion` for each criterion, stores per-repo results, and
returns a `RepoEvaluation` with `CodebaseLevel` and level details
(`src/pkg/dashboard/api_acmm_eval.go:479`, `src/pkg/dashboard/api_acmm_eval.go:493`).
`checkCriterion` passes when any listed pattern exists
(`src/pkg/dashboard/api_acmm_eval.go:578`). That existence-only behavior is
useful for measurement but dangerous as an automation target.

The hive-wide readiness side also exists. `pkg/acmmadvisor` is explicitly
advisory only and never changes the applied ACMM level
(`src/pkg/acmmadvisor/acmmadvisor.go:1`, `src/pkg/acmmadvisor/acmmadvisor.go:10`).
It recommends at most one level at a time through `levelStep = 1`
(`src/pkg/acmmadvisor/acmmadvisor.go:38`) and computes the next target as
`level + levelStep` (`src/pkg/acmmadvisor/acmmadvisor.go:220`). The dashboard
wires the same signal path to `GET /api/acmm-recommendation`
(`src/pkg/dashboard/api.go:252`) and keeps human approval on level changes via
`handlePackSetLevel` (`src/pkg/dashboard/api_packs.go:567`).

There is also a current per-repo checkout-root mechanism, but not the exact
`governor.acmm.repo_roots` spelling from the RFC. On `v4`,
`ProjectConfig.CheckoutRootFor` supplies a host-local checkout root for one
monitored repo (`src/pkg/config/config.go:562`), and the scheduler resolves
per-repo roots through `agentsRepoRoot` (`src/pkg/scheduler/scheduler.go:1480`).
The onboarding reconciler should follow the current config structure rather than
revive stale field names.

## Proposed design

Add a repo onboarding reconciler that computes:

```text
repo current codebase level → repo target level → ordered failing criteria → classified work
```

### Per-repo target level

Introduce a first-class target map under the ACMM governor config:

```yaml
governor:
  acmm:
    repo_targets:
      docs-site:
        level: 2
        reason: "documentation-only repo; e2e gate does not apply"
      hive:
        level: inherit
```

`inherit` is the default and means "use the Hive's current ACMM level". Explicit
lower targets are allowed but require a reason, so divergence is visible rather
than an informal escape hatch. This is related to #6111, which proposes a
per-repo ACMM ceiling: a ceiling limits how high automation may go, while a
target says what onboarding should try to reach. If both exist, the effective
onboarding target should be `min(repo_target, repo_ceiling)` unless maintainers
choose a different policy in #6111.

### Gap to work

For each repo, reuse the existing evaluator to find the current codebase level
and failing criteria up to the target. Order work by level, lowest first, so L0
prerequisites precede L2+ conveniences. Emit work through the governor's normal
work paths rather than creating a special ACMM queue, unless capacity data proves
normal work starvation.

Each gap is classified:

- `agent-actionable`: an agent can make a substantive change in a PR.
- `needs-human`: repo or org settings, permissions, credentials, or engineering
  decisions block agent-only work.
- `waived`: maintainers explicitly decided the criterion does not apply, with a
  reason and review date.

The dashboard should show one standing onboarding summary per repo. It should
not file a new issue for every criterion on every run; persistent human work
belongs in the summary until resolved, waived, or reclassified.

### Anti-gaming requirements

The reconciler must not be a scoring machine.

- Onboarding changes land as PRs, never direct commits, and never self-approved.
- Work prompts must require substantive artifacts, not empty files satisfying
  `checkCriterion`'s existence rule.
- Cheaply gameable criteria should gain substance checks in follow-up PRs. That
  work composes with #6264, which merged off-repo criteria on `v5`; those richer
  checks should be forward-ported or adapted before raising trust in automated
  onboarding results.
- Level changes are proposed, not applied. The reconciler may say "repo appears
  ready for target L3"; it must not mutate the target or Hive level by itself.
- Waivers require a reason and should be visible in the same standing summary as
  unmet work, so they do not become invisible score inflation.

The design also follows the per-repo scoping theme from #6208: evaluate, plan,
and report per repo first; aggregate views should not hide a single low-maturity
repo behind healthier siblings.

## Configuration shape

```yaml
governor:
  acmm:
    repo_targets:
      default: inherit
      repos:
        hive:
          level: inherit
        docs-site:
          level: 2
          reason: "docs-only repo; L5 runtime criteria do not apply"
    waivers:
      docs-site:
        acmm:prereq-e2e:
          reason: "static documentation site; no browser app surface"
          review_after: "2026-12-01"
```

The exact YAML can be simplified during implementation, but it needs three
invariants: default inherit, explicit per-repo override, and waiver reasons.

## Security and abuse considerations

ACMM level affects the trust granted to agents. Treat onboarding as a governance
control, not a cosmetic dashboard feature.

- PR-only changes protect repos from direct automated score inflation.
- Human review is mandatory before merged artifacts change the measured level.
- `needs-human` prevents infinite re-filing and keeps permission-sensitive work
  out of agent prompts.
- Waivers are auditable and reviewable; they must not silently raise a repo's
  score.
- Substance checks should be prioritized for criteria that can be satisfied by
  empty placeholder files.
- The reconciler should record why a gap was classified and which criterion
  evidence was observed, so maintainers can challenge bad classifications.

## Phased implementation plan

1. **Smallest first PR:** add config parsing and read-only dashboard reporting
   for per-repo targets, inherited defaults, effective target, current codebase
   level, and failing criteria. No issue filing and no agent dispatch.
2. Add classification metadata and one standing summary per repo. Include manual
   transitions among `agent-actionable`, `needs-human`, and `waived`, with waiver
   reasons.
3. Add PR-generating onboarding work for a narrow set of hard-to-game L0
   criteria, using normal review paths and no direct commits.
4. Add substance checks for the easiest-to-game criteria before expanding
   automatic work generation to higher levels.
5. Integrate #6111's per-repo ceiling and #6264's off-repo criteria once their
   target branches converge with `v4`; keep #6208's per-repo scope as the report
   and dispatch boundary.

## Open questions from the RFC

1. **Should a repo target be capped by the Hive level?** Recommended answer:
   allow targets above the Hive level for measurement and planning, but cap
   autonomous dispatch by the effective runtime ceiling from #6111 when present.
2. **Reuse existing agent assignment or create a queue?** Recommended answer:
   reuse existing work paths at first. Add a separate queue only if measurements
   show onboarding work starves normal maintenance or vice versa.
3. **Should `levelStep = 1` apply per repo?** Recommended answer: yes for work
   generation. Reports can show the full gap, but generated PRs should advance
   one level at a time to preserve reviewable trust increments.
4. **Should reports land in the dashboard ACMM view?** Recommended answer: yes.
   The existing ACMM evaluation already reports per-repo codebase levels; the
   reconciler should add target, gap, classification, waiver, and summary state
   there rather than creating a disconnected view.

## Non-goals

This design does not implement the reconciler, alter ACMM scoring, add substance
checks, or change any Hive or repo level. It records the proposed control loop
and the anti-gaming constraints that must shape future code PRs.
