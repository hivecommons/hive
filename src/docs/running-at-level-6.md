# Running a hive at ACMM Level 6

For people who operate a hive, Level 6 means unattended merging, not just faster agents. Protect every destination branch with real required tests and coverage checks; aim for **90% or better coverage before enabling automatic merging**. Stage repositories individually, keep work creation below CI/merge capacity, and prove your emergency stops work before you need them. Human-authored PRs can merge too. A reviewer comment, a spending limit, and paused agents are not substitutes for a merge stop.

This guide walks through preparation, a weekly routine, and recovery. Start with the merge-path table, configure the five areas below, then use the single pre-switch checklist. After a bad merge, stop further harm first, revert and verify, then demonstrate that a strengthened required check rejects the original change.

**Last checked:** 2026-10-04 against branch `v5`, commit `18e4a36e6d81c9e9fcefc6a3dcab8f93b25355e9`. Source links name files and symbols rather than fragile line numbers. **DOCUMENTED NOT EXECUTED:** this is a code-checked guide, not a live deployment trial. **JUDGMENT CALL** consistently marks operating choices that the product does not establish. Hive does not enforce those choices unless the practice explicitly names an enforcement mechanism.

**How directions are given.** Every step says where to do it in the dashboard first and gives the config-file setting second. Nothing in this guide needs an API call. Where the dashboard has no control for something, the step says so.

**Policy reconciliation, 2026-10-28:** the review-gate and issue/PR command
sections were checked against the current v5 implementation for this pass.
They supersede older scanner-only/PR-command summaries. The live-deployment
limitations of the original validation baseline above still apply.

## Read this before enabling unattended merges

**ACMM** is the **AI Codebase Maturity Model**: levels determine permitted automation. Level 6 (L6) is Autonomous, not a release-branch name. An **agent** is a configured AI worker; a **pack** supplies a level's roster, modes and defaults. The **governor** schedules workers according to workload modes; a **cadence** is an agent's wake interval in one mode. These definitions and the shipped roster are grounded in the [Level 6 pack](../pkg/config/packs/level-6.yaml). Only `scanner` has `ISSUES_PRS_MERGE`; other delivery agents create PRs but the App can merge them independently.

**Auto-merge** here means Hive initiating a merge without a person doing it, not GitHub's similarly named feature. A **hold** is a label-based exclusion from work/merging, including literal `hold` and the dashboard's `hive-pause/<hive-id>` item hold. See [labels and control signals](labels-and-control-signals.md); the enforcing predicates are `HasHoldLabel`, `IsHeldLabels` and `HasExemptLabel` in [GitHub client](../pkg/github/client.go).

### Which PRs merge without a person?

All answers assume the repository participates in automatic merging, the PR is ready, unheld and non-exempt, GitHub allows the merge, and the path's checks pass. The common per-repository gate is `RepoAutoMergeEnabled` in [repo policy](../pkg/config/repo_policy.go); below L6 it returns false.

| Author | Direct answer and deciding path |
| --- | --- |
| Hive's agents | **Yes.** The App self-authored sweep needs no human queue approval. It checks author, labels, GitHub mergeability, CI, intent policy and the head again before merging. See `trySweepSelfAuthoredPR` in [sweep](../pkg/github/automerge/automerge_sweep.go). Outreach is an exception below. |
| Dependency bot | **Yes**, for exact logins in `auto_merge.trusted_bot_authors` (unset means `dependabot[bot]`). This is the same sweep with the same gates. Others can enter the scanner/queued paths; removing a bot from this list is not a universal author denylist. See `TrustedBotAuthorSet` in [config](../pkg/config/config.go) and `sweepLaneForAuthor` in [sweep](../pkg/github/automerge/automerge_sweep.go). |
| Contributor relay | **Yes.** ClankeR is the external contributor relay, not a separate merge exception. Its PRs can enter the scanner's current merge-eligible list, without a human `lgtm`. The contributor-lane sweep can also merge green, reviewed PRs for operator-trusted contributors, or contributors auto-promoted by `auto_merge.contributor_tier.auto_promote_after_merged` after a clean merged-PR history. The human queue is another route, not a requirement at L6. See `classifyMergeEligibility`, `bindMergeAuthz` in [eligibility](../cmd/hive/merge_eligibility.go) and `trySweepContributorLanePR` in [contributor lane automerge](../pkg/github/automerge/automerge_contributor_lane.go). |
| Person by hand, including an unknown person | **Yes.** The scanner eligibility classifier is not filtered by author. An unknown PR author is not itself a merge hold. Reporter trust concerns the issues motivating work, not a blanket PR-author restriction. See [eligibility](../cmd/hive/merge_eligibility.go) and [reporter-trust hold](../pkg/github/pr_reporter_trust.go). |

The third route is the human-approved queue (normally `lgtm`): an authorized merger/owner queues **someone else's** PR; the sweep rechecks that authorization. See `handleQueuePRAutoMerge` in [dashboard API](../pkg/dashboard/api.go) and `trySweepQueuedPR` in [sweep](../pkg/github/automerge/automerge_sweep.go). A bare `lgtm` from an arbitrary account is not authorized queueing.

### What waits, and what releases it?

| Item | Why it waits; release |
| --- | --- |
| Held PR or issue | Label-based hold. A human removes the hold after reviewing the actual current head/request, or resumes the dashboard item hold. Promotion does **not** release holds automatically, and the dashboard's level change has no option to release them. See [level hold](../pkg/github/pr_level_hold.go). |
| Outreach PR | `shouldHoldAgentPR` holds an outreach PR whose target repository the hive does not manage (`config.AgentOwnsRepo` false — an awesome-list, a partner's docs, another org), even at L6. Human review and removal of `hold` release it; no level switch removes this exception. An outreach PR into one of the hive's own repos is an ordinary agent PR and auto-merges on green CI. See [eligibility](../cmd/hive/merge_eligibility.go). |
| PR motivated by an untrusted reporter's issue | When reporter-trust holding is enabled, an untrusted cited issue causes literal `hold`; a maintainer reviews and removes it. Triaging the issue alone does not release the PR. See [reporter-trust hold](../pkg/github/pr_reporter_trust.go). |
| Draft | Excluded; author marks ready for review when finished. See [eligibility](../cmd/hive/merge_eligibility.go) and [sweep](../pkg/github/automerge/automerge_sweep.go). |
| GitHub-blocked PR | Missing required checks/reviews, signatures, conflicts or branch rules still bind the merge. Satisfy the actual rule, not just the Hive pill. See [merge block reasons](../pkg/github/merge_block_reason.go) and [sweep](../pkg/github/automerge/automerge_sweep.go). |
| PR escalated after failed fixes | The fix ledger parks exhausted repair work; `needs-human` is a triage signal, **not by itself a universal merge hold**. The scanner separates escalated rows; without the enforcing review gate below, a green App PR may still merge. Add `hold` if the decision must prevent merging; repair/review, clear the escalation, then release the hold. See [escalation](../pkg/escalation/escalation.go), `writeMergeEligible` in [eligibility](../cmd/hive/merge_eligibility.go), and [sweep](../pkg/github/automerge/automerge_sweep.go). |
| Behind/conflicting fork PR | A GitHub `behind` state triggers an update attempt in the sweep, not a guaranteed repair. The hive cannot assume write access to a contributor's fork. Author rebases/updates the fork, resolves conflicts and reruns CI; a fork merely older than base is not blocked unless GitHub requires freshness or reports a conflict. See [sweep](../pkg/github/automerge/automerge_sweep.go) and `UpdateBranch` in [pull requests](../pkg/github/pullrequest.go). |
| Adopted PR with unsigned commits | `no-dco` cannot be fixed by adding a later empty signed commit. If all unsigned commits are hive-authored (GitHub-linked to the App bot; git author emails never count) and the adopted branch can be pushed, the hive rewrites just its own unsigned commits with sign-off in place. If any unsigned commit is contributor-authored/unknown, or the branch cannot be pushed, adoption re-homes the work onto a hive-owned branch from the base, cherry-picks or squashes with `-s` while preserving original authorship/credit, opens a superseding PR, and closes the original. See [stage 074 adoption](labels-and-control-signals.md#off-the-happy-path-072--nudge--074--takeover). |
| Adopted PR with review changes requested | `stage/074-adopted-by-hive` means Hive owns the branch. A swarm `changes_requested` verdict must feed the fix dispatcher, not a human `/lgtm` request; for in-place adoptions the fixer may push signed commits to the contributor branch, and the PR stays in the fixer's work list every cycle until a new head lands. If the bounded attempts are exhausted, Hive parks it visibly at 085 with `fix-attempts-exhausted`. |
| Issue parked for a maintainer decision | A human with repository write/maintain/admin permission comments `/hive approve` or `/hive decision <direction>`; the un-park sweep removes decision labels, never `hold`. Plain "approved" is insufficient. See [maintainer commands](maintainer-commands.md) and [un-park implementation](../pkg/github/issue_unpark_command.go). |
| Issue parked on the target repo's own confirmation gate or a missing companion checkout | Not a maintainer decision. The hub reclassifies a `needs_decision` verdict whose reason is a repo-local gate ("AGENTS.md says offer a spec and wait for acceptance", "confirm scope before implementing", `repo_location_missing` for a registered sibling path) as idle, lifts `stage/045-needs-decision`, posts a `<!-- hive:needs-decision-released kind=repo-local-gate -->` comment explaining that the dispatch was the acceptance, and re-offers the issue once; a second such park on the same issue stays parked for a person. See [Needless parks](#needless-parks). | Usually none. If it parks twice, reply `/hive approve`, and if a sibling repo is involved add it to `project.repo_policies[].companion_repos`. |

### Needless parks

`stage/045-needs-decision` is for decisions only a person can make: security posture, data loss, irreversible or public actions, governance/licensing, spend, credentials, or a genuine A/B the issue body leaves open. Two park reasons observed on live hives are **not** decisions and the product now refuses them:

- **Repo-local confirmation gates.** A target repository's `AGENTS.md` / `CLAUDE.md` may tell a coding agent to "offer a spec and wait for acceptance", "confirm scope before implementing", or similar. An issue the hive dispatched is already accepted (triaged, approved-direction, or chosen by the hive), so that gate is satisfied by the dispatch itself. Contributor task prompts, resident-agent kicks and every role policy say so; an agent that parks anyway has its verdict reclassified to idle by the hub (`src/pkg/dashboard/contribute_gate_park.go`, classifier in `src/pkg/decisionpolicy`), the label is lifted with a marked comment, and the issue is re-offered once. The second identical park on the same issue is left standing — that is the real escalation.
- **Missing companion checkouts.** A target repository's tooling may register a sibling repository path under the agent workspace root (`<workspace>/<org>/<sibling>/…`) and say "stop and report" when it is absent. When that path's last two segments name a repository the installation can read, the agent clones it there (shallow is fine) and continues — the intended checkout, not a substitution. Operators can make this deterministic with `project.repo_policies[].companion_repos`, which puts the clone plan into every prompt for that repo (`src/pkg/config/repo_companion_checkouts.go`).

Risk terms always win: a park whose reason mentions a deploy key, data loss, spend, or a public action stays parked even if it also cites the repo's gate.

### Does the Hive reviewer stop a merge?

**Default: review observation is not a merge stop.** The reviewer role never
merges. The separate review gate defaults to `shadow`: it records what it
would hold but does not stop the App sweep.

| Setting / action | Current effect |
|---|---|
| `review.require_approval: true` | Requires same-head aggregate approval in scanner eligibility and forces the App sweep's review gate to enforce |
| `governor.review_gate.mode: enforce` | Enforces review on the App sweep's self-authored, trusted-bot and trusted-author paths; do not assume this switch alone configures scanner eligibility |
| `governor.review_gate.mode: off` | Does not weaken `review.require_approval: true`; the strictest setting wins |
| Trusted human merger's GitHub **Approve** review on the exact head | Can override **any** swarm outcome in the enforcing sweep gate, including `changes_requested` or `reject`; audit records the actor and overridden outcome |
| `/hive decision approve <rationale>` on an eligible Hive-authored PR at 085 or 070 | Records approval for that head and can release `requires_human`; not a universal override of every gate |
| Bare `/hive approve` on a PR | Acknowledgement only; does not release or authorize a merge |

Approval never bypasses author admission, holds, CI, diff/manual-merge fences,
or GitHub branch rules. New commits require current-head evidence again.
Other `/hive decision <instruction>` text on an eligible Hive-authored PR is
sent verbatim to its author/fixer, not treated as approval. A bare `yes`
counts as approval; `yes, but …` is an instruction. PR decisions require a
human with repo write/maintain/admin access **and** configured merger trust.
Issues instead park at 045 and use their issue un-park/label-approval flow.
Command eligibility also requires a recent, unedited comment (the scan window
is seven days); a stale or edited command is not fresh authorization.

**Implementation evidence (v5 readability review):** `ReviewGate.effectiveMode`
and `mergeReviewGate` in [review gate](../pkg/github/automerge/automerge_review_gate.go)
implement the strictest mode and all-swarm-outcome GitHub-review override.
`tryUnparkPullRequest` in [PR decisions](../pkg/github/pr_unpark_decision.go)
and `unparkCommand.Approval` in [command parsing](../pkg/github/issue_unpark_command.go)
implement the distinct comment paths. This supersedes older scanner-only and
“changes-requested cannot be overridden” guidance, not the other merge limits.
Dispatch fan-out has its own enabling conditions: see `classifyMergeEligibility`
in [eligibility](../cmd/hive/merge_eligibility.go) and `PlanDispatch` in
[review dispatch](../pkg/review/dispatch.go).

On Hive-authored PRs the default `review.self_authored_decision_policy: resolve`
keeps reviewer questions advisory unless they are a risk class: the test is a
denylist (`review.DecisionRiskClasses` — permissions/credentials, security,
sensitive paths, data loss/irreversible, merge/trust policy, external side
effects, licensing/legal, unverifiable — or an explicit
`<!-- hive-decision-risk: high -->` marker), so naming, sequencing, defaults,
config-shape, scope and "land now vs later" calls are the hive's own decision
and the aggregate approves while preserving the question in the audit reason
(`decision resolved by policy: <topic> — not a risk question`). A
non-risk incomplete-fix review that names concrete remediation is different:
it is normalized from `requires_human` to `changes_requested` when verdicts are
aggregated and loaded, even if the finding severity is high, so the review
gate, every stage-label observer, human-entry comments, and fixer dispatch all
see the fixer-lane verdict and the PR enters `stage/070-fixing` instead of
parking for a person. Intent-alignment cleanup with an obvious fix, such as
trimming a duplicate PR summary or changelog claim, follows the same route. Set the policy to `escalate` to restore the older behavior where every
`needs-decision` marker parks the PR at `stage/085-needs-decision`. Risk-class
questions still escalate either way; see
[review-swarm.md](review-swarm.md#configuration) for the class table.

The [policy matrix](acmm-policy-matrix.md) describes the same distinction: reviewer verdicts alone are not universal merge stops, and promotion does not automatically release level holds. Use GitHub rules or an actual hold for a safety guarantee.

### Label discipline

**Before L6, map your repository's labels to Hive's gates.** In the dashboard, start with **Settings → Labels**, **Settings → Repos**, **Governor Config → Hub**, and the repository-card legend. Compare them with the repo's GitHub labels and lifecycle/Prow rules. Where a setting has no dashboard control, use the config file with the deployment operator. Hive ships defaults, not a universal workflow: map your existing labels or opt out of conflicting behavior before enabling unattended work. [#10537](https://github.com/hivecommons/hive/issues/10537) illustrates the collision between a repo's `triage/accepted`/`needs-human` workflow and Hive's `/hive approve` transition to `approved-direction`.

Config keys to cross-check: `project.issue_filter.require_labels`, `project.issue_filter.reporter_trust.untrusted_require_labels`, `governor.labels.exempt`, `governor.labels.automerge`, `github.self_authorization_hold`, `github.reporter_trust_hold`, per-repo `project.repo_policies[].{auto_merge,self_authorization_hold,reporter_trust_hold}`, `hub.contribute_*labels`, `governor.claims.*`, `review.human_decision_label`, `dashboard.issue_bands.*`, and `work_source.{linear,jira}.hold_labels` (under `governor` in runtime YAML).

| Label / signal | Who sets it | What Hive does | Human action |
| --- | --- | --- | --- |
| `hold`, `on-hold`, `hold/review`, `hive-pause/<hive-id>` | Human, dashboard, Hive hold paths | Hard work/merge hold; held red PRs can still get CI repair without release. | Use when an item must not proceed; remove only after reviewing the current head/request. |
| `do-not-merge*`; `governor.labels.exempt` defaults (`nightly-tests`, `LFX`, `meta-tracker`, `auto-qa-tuning-report`, `adopters`, `changes-requested`, `waiting-on-author`) | Human, repo automation, operator | Suppresses enumeration and merge sweeps. | Remove the label/config exclusion only when Hive should act again. |
| Issue `needs-human`, `needs-decision`, `needs-direction`, `needs-spec` | Hive, relay, human | Suppresses issue kicks/offers. Un-park commands handle the first three, not `needs-spec`; however, a `needs-spec` applied by the clarity gate is lifted automatically by the resume sweep (see the `needs-spec` from the clarity gate row of the [reference table](labels-and-control-signals.md#reference-table) in labels-and-control-signals.md). | Use `/hive approve` or `/hive decision <text>` only for a repo using Hive's command flow; otherwise use its mapped label workflow. Commands never clear `hold`. |
| PR `needs-human` | Hive escalation/reviewer | Stops automated fix dispatch, not every merge path. | Add `hold` if merging must stop; clear escalation after repair/decision. |
| `approved-direction` | Maintainer command/label workflow | Acknowledges Hive-filed direction for the self-authorization gate and ranking. | Map it to your acceptance workflow; do not assume it releases existing PR holds. |
| `triage/accepted`; configured untrusted-reporter ready labels | Human/operator | Admits untrusted reporter work when reporter trust is enabled, subject to other filters. | Match the actual reviewed-ready label; test accepted and unreviewed issues. |
| `blocked` | Human/repo bot | Contributor exclusion and waiting display, not a spoke-agent hold alone. | Add `hold` or an enforced filter if agents must not act. |
| `help wanted`, `good first issue`, `bug`, `kind/bug`, `priority/*` | Human/repo bot/Hive | Ranking/visibility; some kind labels classify work. | Treat as prioritization, not approval. |
| `kind/security`, `kind/regression`, `auto-qa`, `auto-qa-finding`, lane labels, `agent/<role>` | Human/Hive | Classification, routing, provenance. | Verify lane keywords and ownership; these are not holds. |
| `area/*` | Human/repo bot | Helper validation; generic scheduling gates only if configured. | Maintain ownership taxonomy, not an assumed safety gate. |
| `lgtm`; `governor.labels.automerge` | Dashboard/authorized merger | Queues authorized human-approved merging; rechecks holds, CI and mergeability. | Do not substitute hand-labeling for authorization. |
| `review.human_decision_label` | Hive, if configured label exists | Mirrors a verdict; text alone does not gate the App sweep. | Choose an actual hold label/GitHub rule when review must stop merging. |
| `hive/advisory` | Hive | Standing advisory, never actionable. | Leave alone unless deliberately moving the digest. |
| `🐝 covered by PR`, `hive/likely-done`, `hive/verified-open` | Hive/agent outcomes | Progress display; verified-open suppresses stale likely-done state. | Confirm/close/remove based on real outcomes, not as a merge stop. |
| `hive/already-done` | Contributor flow | Contributor skip/done display; not a universal agent gate. | Verify the outcome before clearing/closing. |
| `claimed`, `preempted:<login>`, `hive/claimed-by-human` / `hive/claimed-by-<agent>`, `<!-- hive-claim -->` | Claim flows | Ownership/display; the ledger/comments carry claim state. | Use `governor.claims.comment: false` for noisy comments; claim labels do not replace holds. |
| `release-blocker` | Human/release workflow | Stable promotion script gate on open issues. | Clear after release blocker resolution, not to unblock ordinary L6 work. |
| `ci-flake` | CI/Hive scripts | Flake tracking, not an L6 hold itself. | Fix/quarantine the flake while keeping required checks meaningful. |
| `user-feedback` | Dashboard/hub | Feedback provenance. | Triage reporter input; apply trust/hold policy as needed. |
| `dco` / `no-dco` (Prow's `dco-signoff: yes|no` coexist on Prow-gated repos; the hive never adds or removes them) | Hive stage pass (commit check) | Merge-eligibility display on every managed repo. Where DCO is required (detected from required/head checks or configured via `governor.labels.stage.dco.required`), `no-dco` routes by author kind: `070` (hive/self-authored), `071` (human), or `072` (clanker); the 072 nudge includes the rebase command. On ungated repos it is informational only: the lane's stage is unchanged and no DCO rebase is requested. See [DCO routing](labels-and-control-signals.md#dco--no-dco--the-hive-owned-sign-off-signal). | Fix commit sign-offs (`git rebase --signoff`) rather than hand-editing labels. |
| `conflicts-with-open-pr` | PR-overlap workflow | Overlap signal. | Resolve or close conflicting work before merging; same-file overlap alone may be harmless. Two open PRs that both `Fixes` one issue are resolved by the duplicate-fix sweep without a decision: the earlier green/pending PR stays, the later hive PR is closed as `duplicate-of:<keeper>`, a person's later PR is commented on and left open ([#14799](https://github.com/hivecommons/hive/issues/14799)). |
| `has-open-pr` | Pre-dispatch guard | An open PR already links the issue with a closing keyword; the hive skipped the claim. | Nothing — the label is a tracker signal, not a park; see `scanner.skip_issues_with_open_pr` in [operator-reference.md](operator-reference.md#open-pr-dispatch-guard-and-duplicate-fix-sweep-scanner). |
| `no-changelog` | Dependabot/maintainer | Repo changelog exception, not a Hive safety gate. | Apply only under repo policy. |
| `human-only` remainder text (not a default label) | Agent/reviewer | Task-list sweep can park remaining issue work as `needs-human`. | Treat as a handoff; add PR `hold` if merging must stop. |

**Verify the mapping with canaries:** a held item stays held, an accepted issue enters the intended queue, an unreviewed issue stays out, and repo bots do not undo each other's decision labels. Generic GitHub holds use substring matching (`threshold` also holds); Linear holds use substring matching, Jira holds use exact configured labels, and GitHub Projects carries labels without an adapter hold gate. Do not assume identical enforcement across sources. Sources: [label reference](labels-and-control-signals.md), [maintainer commands](maintainer-commands.md), [GitHub predicates](../pkg/github/client.go), [issue labels](../pkg/github/labels.go), [un-park](../pkg/github/issue_unpark_command.go), [claim config](../pkg/config/claims.go), [progress labels](../pkg/github/prclaims.go), [remainder sweep](../pkg/github/task_list_sweep.go).

## Getting ready to switch

### GitHub settings

For hosted and self-hosted hives alike, GitHub repository/organization administrators configure these on GitHub, **not the Hive dashboard**. In these rows, GitHub enforces its rules; Hive does not enforce the recommended policy choice. The source describing Hive's interaction is given per row.

| Practice: what and where | L6 failure avoided; how to verify |
| --- | --- |
| **G1. Restrict App installation scope.** GitHub organization/repository Settings → GitHub Apps → installed Hive App → Configure: select only intended repositories; match Hive's `project.repos` in runtime config. | Broad installation gives credentials reach beyond intended work. Verify the installation's repository list against the dashboard repo cards. See [App setup](github-app-setup.md) and `Repositories` in [client](../pkg/github/client.go). |
| **G2. Grant the documented App permissions, not an assumed owner identity.** App administrators use App Settings → Permissions; installation administrators accept permission updates. Include the documented contents/PR/issues/checks/actions access and Workflows write if workflow edits are in scope. | Missing permission causes forbidden API calls or rejected workflow pushes, not useful automation. Verify a harmless PR and permitted workflow edit on a test repository; inspect App errors. See [App setup](github-app-setup.md) and [PR request watcher](../pkg/github/pr_request_watcher.go). |
| **G3. Protect every destination branch.** Settings → Rules → Rulesets (or Branches → protection): require build, tests, coverage and security contexts, including release branches, with no App bypass for these checks. | Unprotected bases can merge; Hive no longer refuses solely for lack of protection. Verify an intentionally red and a missing-context test PR are blocked for the App identity. See [CI gate](../pkg/github/merge_ci_gate.go). |
| **G4. Decide whether humans approve.** In the same ruleset, require approving/code-owner reviews for branches where unattended merging is unacceptable; make that rule apply to the App. **JUDGMENT CALL:** retain it for high-impact branches. Maintain CODEOWNERS so those paths have real owners: required code-owner review protects only matched paths. Verify with an owned-path canary and an unowned-path canary. | A review rule without App bypass stops unattended merges; a bypass removes this protection. Verify GitHub reports awaiting review before any App merge. Hive's reviewer approval is not a GitHub approving review. See [block reasons](../pkg/github/merge_block_reason.go). **Note:** Intent tiers do not stop guardrail-tier App PRs at L6: the App self-merge contract (`EvaluateForAppSelfMerge`, #6258) authorizes them without a person. On a single-maintainer repo, GitHub code-owner review cannot help because the owner cannot approve their own PRs; use `auto_merge.human_merge_paths` there, a per-repo map of `owner/repo` to glob patterns that holds any matching PR, in every merge lane, for a person to merge (see [operator-reference.md](operator-reference.md)). |
| **G5. Require an up-to-date base.** Enable strict required checks/"Require branches to be up to date before merging" on each protected branch. | Green on an older base does not establish green after integration. Verify a PR becomes blocked after a base push until updated/retested. The sweep attempts updates for `behind`; see [sweep](../pkg/github/automerge/automerge_sweep.go). |
| **G6. Allow the merge methods actually used.** Settings → General → Pull Requests: allow squash; also allow merge commits if managing release-line forward merges. | The ordinary method is squash; recognized forward merges use merge commits to preserve ancestry. Disallowed methods cause merge refusals. Verify landing method in a test PR and ancestry for a sync PR. See `mergeMethodFor` in [sweep](../pkg/github/automerge/automerge_sweep.go) and [pull requests](../pkg/github/pullrequest.go). |
| **G7. Keep fork-run approval deliberate.** Settings → Actions → General → Approval for running fork pull request workflows: choose the trust policy your maintainers can service. **JUDGMENT CALL:** require approval for untrusted fork authors rather than relaxing it to drain a queue. | Unapproved runs are `action_required`, not successful CI. Have a maintainer inspect the workflow/diff then approve runs; verify check evidence appears at the PR head. See `forkRunApprovalReason` in [CI gate](../pkg/github/merge_ci_gate.go). |
| **G8. Test signed-commit requirements end to end.** If your ruleset requires signatures, enable `github.app_signed_commits` in Hive runtime YAML (not a dashboard switch) and test follow-up commits too. | An unsigned follow-up can strand an otherwise green PR. Hive reconciles its own commits, not people's work. Verify GitHub's Verified badge at the latest head and branch-rule acceptance. See [signed reconciliation](../pkg/github/pr_signed_reconcile.go). |

### Test coverage of 90% or better

**T1. Make the 90% target a real CI gate.** In each repository's CI workflow, measure coverage using its native test tooling, save the report as an artifact, and fail below 90%; make that exact job a required GitHub context and declare it in Hive's required-check set below. Without both, a flattering badge or an optional red coverage job does not protect L6. Verify by opening a PR whose measured coverage falls below the floor and checking that the App cannot merge it. The project's 90% target is **only reported by the advisor**, not enforced by Hive as a coverage percentage; CI enforces your configured floor. Sources: [advisor code](../pkg/acmmadvisor/acmmadvisor.go), [commit CI](../pkg/github/commit_ci.go), [eligibility](../cmd/hive/merge_eligibility.go).

**T2. Keep an 82%-covered repository out of auto-merge while improving it.** Turn its repo-card auto-merge switch off; use `quality` work to add behavior/regression tests for uncovered risk, human-review those changes, then measure again in CI. Enable only after the 90% target and branch gates are demonstrated. This staging choice is **JUDGMENT CALL**, not a Hive enforced readiness rule. Verify increasing report coverage **and** a regression test that fails on the old broken behavior. The quality lane is available in the [pack](../pkg/config/packs/level-6.yaml); the switch is enforced by [repo policy](../pkg/config/repo_policy.go).

**T3. Cross-check the reporting input.** The coverage number to trust is the repository's CI report with its denominator/exclusions, not an agent's prose. `HIVE_COVERAGE_BADGE_URL` supplies the dashboard measurement: self-hosters set it in their deployment environment (not dashboard); hosted operators ask the hub operator to set it. Verify the badge agrees with the report. It is a reporting input, **not** a merge floor. See [coverage collector](../pkg/dashboard/metrics_collector.go) and [advisor inputs](../pkg/dashboard/api_acmm_recommendation.go).

Direct answers about checks:

- **Failed test: does Hive still merge? No** if it is a required gating check; **yes, possibly** if it is optional and GitHub allows merging.
- **Failed coverage job: does Hive still merge? No** if required; **yes, possibly** if optional. The scanner explicitly permits optional-only red when an operator-declared required set exists and GitHub reports mergeable (`onlyOptionalRed`). A report uploaded successfully without a failing floor does not block a merge.
- **No check results: does Hive merge? The App sweep can say yes** after its age/expected-check and GitHub mergeability gates; GitHub protection is essential for missing required contexts. **The merge-request path says no** to zero statuses/checks/workflow runs unless the repo is explicitly in `auto_merge.no_ci_ok`. A `neutral` or `skipped` observed check is also accepted by the shared evaluator, so keep required gate workflows executing meaningful tests. Missing required contexts block on the **merge-request path**; on the App sweep, the evaluator records `MissingRequired` but its green result does not itself reject that list. GitHub's required-check rules must supply the missing-context block there. Do not rely on the Hive declaration alone. Unapproved or failed workflows that produced no jobs are also checked on the merge-request path.

These are path-specific, not an "all CI green" promise: see [commit CI](../pkg/github/commit_ci.go), [sweep](../pkg/github/automerge/automerge_sweep.go), [CI gate](../pkg/github/merge_ci_gate.go), and [eligibility](../cmd/hive/merge_eligibility.go).

### Knowledge accumulation

A **bead** is an agent's persistent work-ledger entry. The default hourly bead synthesizer classifies closed beads into wiki facts. It is not merged-PR fact extraction; that old pipeline was never wired. Collection can run while priming is off. The **primer** selects knowledge for an agent's next kick (work prompt). See [knowledge boot](../cmd/hive/main.go), [synthesizer](../pkg/knowledge/bead_synthesizer.go), and [knowledge curator reference](knowledge-curator.md).

| Practice: what and where | L6 failure avoided; verification and enforcement |
| --- | --- |
| **K1. Enable and inspect priming.** Dashboard → **Knowledge**: turn knowledge on (`knowledge.enabled` in the config file). The panel's badge must not read **not primed** or **restart required**; if it does, the panel's banner says what still needs a restart. | Stored facts alone do not reach workers. Open an agent's card → **Prior Prompts** and check that a recent kick contains relevant facts, not just that the wiki count is growing. The toggle enforces priming enablement, not knowledge quality. See [live primer wiring](../cmd/hive/boot_knowledge_primer.go). |
| **K2. Add project-specific constraints by hand.** **JUDGMENT CALL:** record release branches, test commands, forbidden migrations, rollback steps and prior incidents in Knowledge or a managed public markdown git source; validate selected facts against the repo. | Generic accumulated notes can miss the costly local exception. Read several real kicks under an agent card's **Prior Prompts** and have a maintainer correct contradictions; Hive does not enforce "enough knowledge" or factual accuracy. Sources/import locations are in [curator reference](knowledge-curator.md); source wiring is in [knowledge boot](../cmd/hive/main.go). |
| **K3. Supply a maintained checkout if relying on AGENTS.md.** Set `project.checkouts_dir` in runtime YAML; populate `<directory>/<bare-repo-name>/AGENTS.md`. This filesystem setting is not a dashboard checkout provisioner. Self-hosters mount/update it; hosted operators need hub-operator help. | Without a real root the instructions are not injected. Verify the primary repo's root instructions appear in a kick (agent card → **Prior Prompts**) after a checkout update. Hive enforces parsing/injection when present, not freshness of your clone. See [AGENTS.md reference](agents-md.md) and `agentsRepoRoot` in [scheduler](../pkg/scheduler/kick_priming.go). |

### Hive settings

Both hosting types use the same spoke dashboard for owner-authorized changes. "Runtime YAML" below means the **effective persisted config**, not merely a seed ConfigMap; see [config layering](config-layering.md). Self-hosters manage that file and restart their service; hosted operators ask the hub operator for file/environment/restart changes they cannot perform. Never run repository tests in a live hive to verify configuration.

Choose the task first: **merge scope and checks** (H1–H6a), **human decision
boundaries** (H7–H10, H15), or **workload and coordination** (H11–H14, H16).
These are recommended practices, not a readiness test Hive applies automatically.
Each practice separates the action, its verification and its limits.

#### H1. Choose participating repos explicitly

**Action:** repo-card Auto-merge → off for every unqualified repo
(`project.repo_policies[].auto_merge`).

**Verify:** inspect every card after changing level; an off repo must reject
a test merge. The L6 action enables all active repos.

**Limit:** Hive enforces the switch, not your qualification rule.
Evidence: [repo API](../pkg/dashboard/api_repo_pause.go),
[repo policy](../pkg/config/repo_policy.go).

#### H2. Declare all critical checks by exact name

**Action:** Governor Config → Auto-merge → Required checks.
Use `auto_merge.required_checks` as a default and
`auto_merge.repos.<repo>.required_checks` for heterogeneous repos. Mirror
branch rules, including coverage; per-repo keys accept bare names or `owner/repo`.

**Verify:** test a red/missing context and a green PR on each repo/branch.
A default produced by only one repo can block every other repo.

**Limit:** when GitHub reports a known actual set that disagrees with config,
Hive warns once and uses the actual set. If the set is unknown, config stays
fail-closed. The [path-specific CI limits](#test-coverage-of-90-or-better)
still apply. Evidence: [config](../pkg/config/config.go),
[commit CI](../pkg/github/commit_ci.go).

#### H3. Leave no-CI exceptions empty

**Action — runtime YAML, no dashboard field:** `auto_merge.no_ci_ok: []`.

**Verify:** merge requests with no CI evidence must refuse.

**Limit:** Hive enforces the exception list; granting none is the
recommendation. It prevents absent evidence passing on the relay, but does
not fix the App sweep's no-results behavior.
Evidence: [CI gate](../pkg/github/merge_ci_gate.go).

#### H4. Start with one merge per sweep pass

**Action — JUDGMENT CALL:** Governor Config → Auto-merge → Max merges,
`auto_merge.max_merges: 1`; raise only after recovery drills.

**Verify:** inspect the dashboard **Audit Log** for the expected merge pace.

**Limit:** the cap bounds a sweep burst, not all parallel merge paths. Unset
uses 3. Sweep timing adapts to repositories/candidates; there is no operator-set
interval. Evidence: [sweep](../pkg/github/automerge/automerge_sweep.go).

#### H5. Keep the post-push quiet period

**Action — runtime YAML, no dashboard field:** `auto_merge.min_head_age: 3m`.
Lengthen if checks register slowly.

**Verify:** look for fresh-head pending messages, not an unconditional
three-minute delay.

**Limit:** Hive may bypass the quiet period when the config-declared required
set is already successful. Evidence: [commit CI](../pkg/github/commit_ci.go)
and [config defaults](../pkg/config/config.go).

#### H6. Narrow the bot sweep list

**Action:** Governor Config → Auto-merge bot toggles,
`auto_merge.trusted_bot_authors`. Keep only CI-qualified exact logins.
An explicit empty list disables this bot lane; unset enables dependabot.

**Verify:** inspect **Audit Log** for which bot PRs merged.

**Limit:** Hive enforces sweep membership only; scanner merging is separate.
Evidence: [config](../pkg/config/config.go),
[sweep](../pkg/github/automerge/automerge_sweep.go).

#### H6a. Install the Hive `/ok-to-test` workflow

**Action — repository maintainer:** commit
`.github/workflows/hive-ok-to-test.yml`. Run `hivectl ok-to-test-template`
to fill in the hive's App login, or copy `src/docs/templates/hive-ok-to-test.yml`.
It is self-contained, with no `uses: hivecommons/hive/...` reference, so it
works under restrictive Actions policies and on GHES.

**Verify:** for a trusted same-repo bot/App/clanker PR with `action_required`
runs, Hive posts `/ok-to-test` and the repo workflow approves the held runs.

**Permissions and limits:** the App needs Actions **read**, not write.
The workflow uses that repo's `GITHUB_TOKEN` with `actions: write`,
`issues: write`, `pull-requests: read`, `contents: read`. Prow is not needed.
If the workflow is absent, the item remains at 085 with
`workflow-approval-required:missing-workflow:<ctx>` and the exact workflow
rendered for that hive. A sentinel alert also requires human action.

#### H7. Enable reporter trust for public issues

**Action:** Settings → Labels → Reporter trust
(`project.issue_filter.reporter_trust`). Choose trusted associations/logins
deliberately, not every historic contributor. Leave `github.reporter_trust_hold`
inherited unless deliberately making it stricter.

**Verify:** an unknown reporter's issue is triage-only; a PR citing it is held.

**Limit:** default off. This governs whose issues lead to work/unattended
merges, not PR-author admission. Hive enforces configured admission and the PR
hold. Evidence: [reporter hold](../pkg/github/pr_reporter_trust.go),
[config](../pkg/config/config.go).

#### H8. Decide whether the hive approves its own proposals

**Action — JUDGMENT CALL:** when a human must agree to Hive-filed proposals,
Governor Config → Repos → `github.self_authorization_hold: true`.
Check per-repo overrides and any environment lock. Otherwise retain L6's
off-by-level default.

**Verify:** a Hive-originated proposal's PR gets `hold` and a notice, or
deliberately does not.

**Limit:** off permits proposal and implementation without human assent;
on can create a large held backlog. Hive enforces the hold, not the proposal's
wisdom. Evidence: [self-authorization](../pkg/github/pr_self_authorization.go),
[config](../pkg/config/config.go).

#### H9. Decide whether automatic large-plan approval is acceptable

**Action — JUDGMENT CALL:** if not, stay below L6 or hold the epic/children.
`plan_auto_approve` belongs to the shipped pack, **not runtime governor
config**; no per-hive dashboard/YAML override exists.

**Verify:** inspect plan/child labels and admission.

**Limit:** the plan sink reads L6's pack directly and allows child tasks
without human plan approval. Pack approval is not design correctness.
Evidence: `PlanAutoApproveForLevel` in [pack config](../pkg/config/acmm_packs.go),
`planFromLabeledIssues` in [runtime wiring](../cmd/hive/main_eval_cycle.go).

#### H10. Keep input scanning fail-closed

**Action:** Governor Config → Security → Input Defense → ioscan fail mode,
`closed` (`ioscan.fail_mode`), retaining L6's inherited default.
**ioscan** screens incoming/outgoing text for injection.

**Verify:** **Audit Log** shows `ioscan_fail_closed` for a blocked item and
no kick reached the agent.

**Limit:** Critical suspect input blocks a kick instead of only being redacted;
false positives need investigation. The fail mode is not perfect attack
detection. Evidence: [kick enforcement](../pkg/scheduler/ioscan_enforce.go),
[pack](../pkg/config/packs/level-6.yaml).

#### H11. Begin with shipped cadences

**Action:** Agent settings → Cadences; keep the
[pack's per-mode matrix](../pkg/config/packs/level-6.yaml), slowing work
creators when backlog grows rather than continuously generating issues.

**Verify:** creations versus merges and queue age fall after slowing.

**Limit:** Hive enforces intervals, not throughput balance. The pack's
2026-09 tuning lesson was 119 issues opened versus 31 closed in 24 hours.
Evidence: pack comment and [governor](../pkg/governor/governor.go).

#### H12. Set a funded token budget

**Action — JUDGMENT CALL:** Governor Config → Budget,
`governor.budget.total_tokens`, based on measured use for the displayed
`period_days` window. Set a separate monetary cap at the inference provider.

**Verify:** compare tokens used/remaining and provider dollars against that window.

**Limit:** zero disables budgeting; tiny limits can suppress almost every kick.
Hive enforces token kick suppression subject to exemptions/ignore settings,
not a dollar ceiling or merge stop.
Evidence: [governor](../pkg/governor/governor.go).

#### H13. Keep optional lanes paused until wanted

**Action:** Agent controls/Cadences: retain shipped pauses for supervisor,
strategist, outreach, telemetry and operations in all modes.

**Verify:** inspect actual pause/cadence state in every mode.

**Limit:** L6 does not mean every lane is active; more issue-filers can worsen
saturation. Hive enforces pauses, while you decide which work is wanted.
Evidence: [pack](../pkg/config/packs/level-6.yaml),
[agent pause](../crux/agent/manager_pause.go).

#### H14. Deliver alerts to someone who can act

**Action — JUDGMENT CALL:** Settings → Notifications: configure a documented
ntfy/Slack/Discord destination and name a responsible operator.

**Verify:** send the documented test notification and confirm receipt.

**Limit:** dashboard-only warnings may go unseen while merges continue.
Hive sends notifications, not an acknowledgement or service-level guarantee.
Evidence: [notifications](notifications.md), [notifier](../pkg/notify/notify.go).

#### H15. Choose an enforcing review gate deliberately

**Action — JUDGMENT CALL:** Governor Config → Security → PR Review → Require
review approval (`review.require_approval: true`).

**Verify:** test missing-review, rejected and approved heads on both scanner
and App sweep paths.

**Limit:** this gates scanner eligibility **and** enforces the sweep review
gate. Trusted human exact-head overrides and other gates still apply; see
[the current gate contract](#does-the-hive-reviewer-stop-a-merge).
Use GitHub rules or literal holds for changes that must wait for human review.

#### H16. Turn issue claims on so workers do not collide

**Action — owner:** Governor Config → Features → **Issue claims**, or
`governor.claims.enabled: true`. It is **off by default**. Set
`governor.claims.{human_ttl_s,agent_ttl_s,contributor_ttl_s}` in YAML or the
claim-lifetime inputs under the same toggle. Changes apply without a restart.
Claim your own work with `hivectl claim o/r#N` before starting.

**Verify:** claim a test issue; confirm `claimed` appears and the agent kick
is withheld. Let the claim expire or run `hivectl unclaim`; confirm work becomes
actionable again.

**Limit:** Hive enforces the claim ledger and expiry, not your discipline of
claiming before you start. Without claims, duplicate PR detection only sees
collisions after workers have opened their PRs.

<details>
<summary>Claim ranking, defaults and implementation reference</summary>

- Holders rank human > agent > contributor > external. Lower ranks back off;
  higher ranks take over with `preempted:<login>`.
- The record is a marker comment (`<!-- hive:claim … -->`) or the assignee on
  the GitHub issue itself. No organization is required: personal repositories
  work, and another hive/script can read the same claim.
- Default lifetimes: 4h person, 2h agent kick, 30m relay task.
- See [claims](../pkg/claims/claims.go),
  [issue claims](../pkg/github/issue_claims.go),
  [config](../pkg/config/claims.go) and
  [CLI commands](hivectl.md#claim--unclaim--claims--issue-claims).

</details>

### ClankeR settings

**ClankeR** is the contributor relay offering external compute to a hive. A **trust tier** is the contributor profile's server-side standing, distinct from reporter trust and dashboard access roles. Controls below are under **Governor Config → Hub** on the hive serving `/contribute`, also exposed on its contribution admin page. They are not the unrelated central hub's global settings. Hosted owners use that hive's admin UI; self-hosted owners use their own dashboard. All are owner-controlled; see [Hub config API](../pkg/dashboard/api_governor.go) and [relay admin reference](contributor-relay.md).

| Practice: what and where | Risk; verify; enforcement |
| --- | --- |
| **C1a. Exclude unqualified repositories.** **JUDGMENT CALL:** Hub → Repos for Contribute, put unqualified repos in `hub.disabled_repos`. | New repos otherwise default on, amplifying unready work. Verify each repo's ready-work count. Hive enforces repo admission, not qualification. See [contribution API/queue](../pkg/dashboard/api_contribute.go). |
| **C1b. Require a readiness-label mode.** **JUDGMENT CALL:** Hub → Label filter, `hub.contribute_labels_mode: allow`, with the non-empty list in C1c. | Deny mode can admit untriaged work; an empty allow list also means filter off. Verify unlabeled work is withheld. Hive enforces the mode at queue build, not label correctness. See [queue filters](../pkg/dashboard/api_contribute.go). |
| **C1c. Name the readiness labels.** **JUDGMENT CALL:** Hub → Label filter list, `hub.contribute_deny_labels` contains your reviewed ready labels (the legacy key name is used in allow mode too). | A wrong/empty list defeats the intended admission policy. Compare ready-work entries to reviewed GitHub labels. Hive enforces list matching, not work quality. See [queue filters](../pkg/dashboard/api_contribute.go). |
| **C1d. Exclude decision/held work independently.** **JUDGMENT CALL:** Hub → Contribute skip labels, `hub.contribute_skip_labels`, retain decision/blocked exclusions and add your manual hold labels. | Ready labels must not override a later hold/decision. Verify doubly-labelled work stays out of the queue. Hive enforces these skip filters before ordinary filters. See [contribution API](../pkg/dashboard/api_contribute.go). |
| **C2a. Name evaluated models.** **JUDGMENT CALL:** Hub → Allowed Models, `hub.contribute_allow_models`, use a non-empty list evaluated on your tasks. | Empty accepts all models. Verify accepted patterns against successful task trials. Hive uses relay-declared metadata, not independent proof of model identity. See [connection admission](../pkg/dashboard/contribute_ws.go). |
| **C2b. Enforce the model list.** Hub → Reject Unknown Models, `hub.contribute_reject_unknown_models: true` alongside C2a's non-empty list. | With rejection off the list is not a hard boundary. Verify a disallowed connection fails. Hive enforces connect-time rejection, not actual reasoning quality. See `checkModelAllowed` in [connection admission](../pkg/dashboard/contribute_ws.go). |
| **C3a. Set an effort floor from task trials.** **JUDGMENT CALL:** Hub → Effort floor, `hub.contribute_min_reasoning_effort: high`, revising from quality/cost measurements. | Empty floor does not constrain effort. Verify low effort refuses and accepted effort passes. Hive enforces the backend-normalized floor at connect time, not reasoning quality. See [effort admission](../pkg/dashboard/contribute_ws.go). |
| **C3b. Refuse unclassified effort.** Hub → Reject Unknown Effort, `hub.contribute_reject_unknown_effort: true` with C3a's floor. | Otherwise unknown/absent effort can pass. Verify an unknown-effort connection refuses. Hive enforces this conditional rejection against declared metadata. See `checkEffortAllowed` in [connection admission](../pkg/dashboard/contribute_ws.go). |
| **C4a. Limit delegated roles.** **JUDGMENT CALL:** Hub → delegated-role controls, `hub.contribute_delegatable_roles`, keep only needed ordinary lanes, not privileged roles. | Contributor execution need not imply authority over every lane. Verify role-claim refusals. Hive enforces role/tier grants, not your confidence judgment. See [trust and roles](contributor-trust-and-roles.md), [role enforcement](../pkg/dashboard/contribute_agent_roles.go). |
| **C4b. Review before granting higher trust.** **JUDGMENT CALL:** Operations → Connected clankers tier dropdown: manually grant `trusted`/`merger` only after reviewing outcomes. | Completed PR tasks establish activity, not safety. Newcomer→contributor promotion occurs after five PR tasks; trusted/merger are operator grants. Verify persisted profile tier and independently reviewed sample. Hive enforces grants, not confidence. See [trust and roles](contributor-trust-and-roles.md), [profile updates](../pkg/dashboard/contribute_ws.go). |
| **C5. Keep queueing separate from authorship.** Manage Access: grant merger/owner only to designated independent maintainers. | An author must not approve their own queued merge; this is **enforced** by the queue endpoint and queued sweep. Verify a self-queue request refuses. L6's scanner route still needs no queue approval, so this is not a general reviewer requirement. See [dashboard API](../pkg/dashboard/api.go), [sweep](../pkg/github/automerge/automerge_sweep.go). |
| **C6a. Rehearse contribution suspension.** **JUDGMENT CALL:** know Hub → Suspend contributions (`hub.contribute_suspended`). | Suspending offers is not recalling already-open PRs or necessarily killing active tasks. Verify the ready-work queue is paused; stop/revoke active relays separately and use repo merge stops as needed. Hive enforces suspension/admission, not a universal emergency brake. See [contribution API](../pkg/dashboard/api_contribute.go) and [relay reference](contributor-relay.md). |

**C6b. Rehearse individual revocation.** **JUDGMENT CALL:** owner uses Operations → Connected clankers → Revoke for an unsafe contributor, and verifies it cannot reconnect. Stop any continuing workload separately and hold/disable merging for its existing PRs; revocation is not a rollback. Undo by deliberately restoring an authorized tier. Hive enforces the revoked-profile connection gate, not incident resolution. See `handleAuthResponse` in [connection admission](../pkg/dashboard/contribute_ws.go) and [trust reference](contributor-trust-and-roles.md).

Model/effort restrictions apply at the **next connection**, queue restrictions at the **next build**. Do not infer that changing them stopped a task already running; see [relay reference](contributor-relay.md) and [connection admission](../pkg/dashboard/contribute_ws.go).

### Notification hygiene

**JUDGMENT CALL:** do not make every maintainer watch every Hive write. Comments, labels, claims, reviews, merge messages, un-park notices and work-source activities create continuous noise at L6. Keep actionable alerts assigned to an operator; mute routine chatter, not incident signals.

**GitHub Issues/PRs and Projects-backed work.** In GitHub's repository **Watch → Custom** settings, choose needed event types rather than all activity; use participation/mentions and individual thread subscriptions for focused attention. Filter mail from `notifications@github.com` by the configured App actor (for example `hivecommons-hive[bot]`, in `X-GitHub-Sender`) and inspect hidden body markers when your mail client exposes them: `<!-- hive:unpark-notice:9879 -->`, `<!-- hive-claim -->`, `<!-- hive-finding: HASH -->`, `<!-- hive-pr-overlap -->`, `<!-- hive-shared-ci-<n> -->`, `<!-- hive-question-answer -->`. Not all markers use the `hive:` prefix. Unsubscribe from ownership-only threads. Projects is a read source; Hive's GitHub writes land on backing issues/PRs.

**Linear.** In Linear's notification preferences, scope notifications for the Hive app user, delegated/assigned issues, mentions and relevant teams/projects. The agent integration posts as the app (`actor=app`) and can write thought/action/response session activities, comments/issues and PR links. In Hive **Settings → Work Source**, narrow Linear enumeration using assigned-only, team states, cycles, projects and hold labels (`work_source.linear.*`, under `governor` in runtime YAML). There is no source-neutral Hive mute for Linear session activity.

**Jira.** In **Settings → Work Source**, narrow project keys, JQL and hold labels (`work_source.jira.*`, under `governor` in runtime YAML). The v5 adapter reads Jira; generic `WorkSource` is read-only and source-neutral claim/comment/transition writes are not pluggable. If helper writes are enabled later, configure Jira's project notifications for the credential account; do not assume Hive supplies a Jira-side mute.

**Run stages and Wavefront.** These additive sources list internal stage/graph work and record receipts; they do not themselves create source-native comments/labels. Noise generally appears on the GitHub issues/PRs opened for that work, so apply the GitHub guidance.

**Hive chatter controls.** Start in **Governor Config → Advisory**, **Security → PR Review**, and **Hub**; use the runtime config with the deployment operator for controls not exposed in the dashboard. Tune `governor.advisory.update_interval_s` and `governor.advisory.target` for digest cadence/destination. Leave `review.post_comments` off unless public reviews help humans; bound repeat notifications with `review.combined_perspectives`, `review.max_perspectives_per_pr`, `review.max_reviews_per_head`, and per-repo `review.revise_repos`. Bound duplicate-sweep comments with `duplicate_sweep.post_comments`/`max_comments`; turn claim comments off with `governor.claims.comment: false`. Respect the un-park action cap and repo label-workflow mapping rather than relying on email filters to hide conflicting automation. `hive/advisory` remains excluded from actionable work.

Verify a routine claim/review no longer floods unrelated maintainers, while a test incident alert and a direct mention still reach the responsible person. Sources: [integration guide](integration-guide.md), [work-source providers](integrations/work-source-providers.md), [work sources](work-sources.md), [Linear agent](linear-agent.md), [configuration](../pkg/config/config.go), [claim config](../pkg/config/claims.go), [un-park](../pkg/github/issue_unpark_command.go).

### Readiness, staged switching and confirmation

**R1. Use readiness numbers as evidence, not permission.** While at L5, open the dashboard's **Advisory** section and read the **⬆️ Ready to level up?** card. Its checklist covers: quality agent present, coverage ≥90%, ≥12 consecutive non-red default-branch runs, merge success ≥95%, actionable issues ≤10, holds ≤5. These thresholds are **only reported**, not enforced or configurable. Verify against each repo's CI and reviewed changes, not just a hive-wide aggregate. They do not measure security, test adequacy, production outcomes or knowledge accuracy. At L6 the advisor reports ready/stay because there is no higher level, not because all readiness criteria remain satisfied. See [advisor](../pkg/acmmadvisor/acmmadvisor.go), [input collection](../pkg/dashboard/api_acmm_recommendation.go).

**R2. Stage without an all-repos merge window.** **JUDGMENT CALL:** an owner should use the following sequence; Hive enforces the pause/repo switches, not this rollout discipline. The level action reconciles roster, pack fields and cadences (operator-owned cadences survive), activates L6 merge capability, and enables auto-merge for **active** repos. It does not add strong CI, change installation permissions, validate coverage or automatically release old holds. Sources: [pack API](../pkg/dashboard/api_packs.go), [repo policy](../pkg/config/repo_policy.go).

1. Pause **all** managed repositories with repo-card Pause before applying Level 6. These pauses remove them from active repositories; do not merely disable auto-merge beforehand, because promotion re-enables active repos.
2. As owner, click the ACMM level badge in the dashboard's top bar (or **ACMM Eval → Change level**), choose Level 6 and **Apply**. No notice follows, so check the result yourself as in R3.
3. While repos remain paused, switch Auto-merge off on every repository card, then look at each card again to confirm it stayed off.
4. Resume just one qualified repo, then enable its Auto-merge as verified owner. Other repos stay paused or are resumed with Auto-merge explicitly off.
5. Verify an eligible test PR merges only in the chosen repo; verify another repo rejects merging. Review any old held PR separately before removing its hold.

**R3. Confirm intent after every switch, restart and upgrade.** On the dashboard, check three things: the ACMM level badge in the top bar shows the level you intended; each agent's card shows the mode, cadence and paused state you expect; and each repository card shows the Auto-merge and paused state you set. Then open **Audit Log** and confirm that merges are appearing, or have stopped, as you intended. If you keep settings in the config file, compare it with what the dashboard shows. Use a harmless held test PR to confirm it stays held, then a qualified canary to confirm the intended path works. Without read-back, a saved level alone can hide reconciliation or persistence failure. Hive reports state and errors; this verification discipline is **JUDGMENT CALL**. Sources: [pack API](../pkg/dashboard/api_packs.go), [sweep startup](../pkg/github/automerge/automerge_sweep.go), [config layering](config-layering.md).

Do not rely on a promotion notice: the checked dashboard defines `maybeShowLevelAutoMergeActiveModal` but the Apply flow does not call it. This differs from references promising a notice. Check state yourself; see [dashboard](../pkg/dashboard/static/index.html).

### Pre-switch checklist (complete once per rollout)

These are confirmations of the practices above, not additional product gates.

- [ ] Real weeks at L3–L5 produced reviewed PRs matching your judgment; no calendar interval is enforced, and "not surprised" matters more than elapsed time.
- [ ] GitHub installation scope/permissions and **every** merge destination's rules checked; App cannot bypass critical tests/coverage. Green CI genuinely means safe to ship, not just a passing harness.
- [ ] Each proposed repo meets the 90% coverage target with demonstrated failing gates; below-target repos remain excluded. Advisor readings and their limitations recorded.
- [ ] Per-repository opt-out decided and applied: Level 6 is not all-or-nothing. Each repository card's **Auto-merge** switch is set deliberately (off for repos that are not ready or must stay human-merged; `project.repo_policies[].auto_merge` in config), and a test PR in an opted-out repo was refused while agents kept proposing work there. Remember that re-applying Level 6 re-enables auto-merge for every **active** repo, so opted-out repos are paused during the switch (R2) or re-checked afterwards (R3).
- [ ] Reporter-trust posture explicitly decided before promotion. For public issues, trust is on with the intended association set; decide specifically whether `CONTRIBUTOR` belongs. Demonstrate admission and PR-side holding.
- [ ] Knowledge priming enabled and actual prompts inspected; release/test/rollback constraints are accurate. Any relied-on checkout instructions demonstrably reach agents.
- [ ] Hive repo scope, required contexts, proposal/plan decisions, cadence, token budget, paused lanes and alert delivery reviewed.
- [ ] Issue claims enabled (`governor.claims.enabled`) with TTLs that fit real task length; a claimed test issue was withheld from agents and released on lapse/unclaim. Humans know to `hivectl claim` before starting on an item that agents could also pick up.
- [ ] ClankeR filters, model/effort admission, trust/roles, independent queueing and suspension tested.
- [ ] Repository labels mapped to Hive gates; acceptance, hold and un-park canaries do not conflict with lifecycle/Prow automation.
- [ ] GitHub/work-source notifications narrowed; routine chatter muted without hiding direct mentions or actionable incident alerts.
- [ ] Emergency brake understood: item `hive-pause/<hive-id>`/`hold`, repo merge-off, and level step-down. Agent pause and budget exhaustion are not merge stops.
- [ ] Outreach remains held at L6; human review is still expected, not removed by a setting or promotion.
- [ ] Staged rollout and fresh read-back/canary prepared; old holds will not be batch-released blindly.

## Running week to week

**W1. Review a small sample and trends every week.** **JUDGMENT CALL:** name a responsible operator and record these readings in an operator log. Hive does not enforce a weekly review or a sample size. Without outcome review, green checks can repeatedly ship the wrong behavior. Verify corrective actions from the last review actually landed. Sources for the displayed mechanisms: [fleet health](../pkg/hub/health_verdict.go), [merge audit](../pkg/github/automerge/automerge_sweep.go), [governor budget](../pkg/governor/governor.go).

| Look at / where | Healthy | Unhealthy / response |
| --- | --- | --- |
| Merged sample / dashboard **Audit Log**, and GitHub merged PR search | Diff, required CI and actual outcome agree; inspect human/bot/relay and App lanes | Surprise scope or bypass: hold similar work and strengthen gate |
| Waiting on people / dashboard held/triage bands and GitHub hold/needs-human/needs-decision searches | Each has an owner and explicit next action | Aging ambiguous work: decide, repair, close or keep held with reason |
| Rework / dashboard **Governor** → PRs by model (review rounds, fix attempts), plus GitHub reverts and follow-up fixes linked to the merged sample | Low/stable rate and regression tests added | Repeat faults despite green CI: stop affected repo and fix blind spots |
| Health/alerts / the alert banner at the top of the dashboard and **Diagnostics**; on a hub, the `/fleet` page | Known recent merge activity and alerts acknowledged | L6's 12-hour merge-recency health is a liveness signal, **not correctness**; idle repo can be red without a defect, busy wrong repo can be green |
| Work opened versus merged / dashboard **Throughput**, and GitHub time-bounded issue/PR searches | Backlog age stable or falling; CI drains | New work consistently outruns merges: slow creators before adding compute |
| Spend / dashboard **Tokens** and **Cost**, the budget under Governor Config, and provider billing | Enough remaining tokens to drain work; expected dollar trend | Exhaustion or unexplained increase: investigate loops/cadence, not blindly raise cap |

**W2. Slow creation before accelerating everything.** When new issues/PRs outpace merges and oldest queue age rises across successive windows, lengthen creator cadences in Agent settings → Cadences; keep reviewer capacity available and repair CI bottlenecks. The pack already omits/pauses architect/guide work generation when busy/surging and keeps reviewer at 30 minutes. Verify lower arrivals and falling queue age after the change. Hive enforces cadence, **does not enforce** arrival/merge balance. Evidence: [pack's measured tuning comment](../pkg/config/packs/level-6.yaml).

**W3. Use an actual hold for one change.** Add literal `hold` on GitHub or use the dashboard item's Hold action (PR **or issue**) before a decision-sensitive change becomes eligible. Authors should open unfinished PRs as **drafts**, not ready PRs with a "please don't merge" comment. Verify held/draft status and exclusion from eligibility. Hive enforces these states; prose requests, lack of `lgtm`, reviewer text and `needs-human` alone are not universal merge stops. The hold matcher is substring-based (`threshold` also contains `hold`); provenance `hive/<id>` is not a hold. See [client predicates](../pkg/github/client.go), [item hold API](../pkg/dashboard/api_repo_hold.go), [eligibility](../cmd/hive/merge_eligibility.go), [sweep](../pkg/github/automerge/automerge_sweep.go).

## When something goes wrong

### Stops, smallest to largest

**S1. Choose the stop that closes the dangerous path, then read it back.** Do not substitute paused workers for revoked merge authority. This practice follows the distinct enforcing paths cited below. Confirm in the dashboard's **Audit Log** that no new merges appear after a repo merge stop, accounting for a request already in flight; a stop cannot undo a completed merge.

| Scope / action | What stops; what remains; can open PRs still merge? | Who, undo, verification/source |
| --- | --- | --- |
| One PR or issue: literal `hold` or item Hold | That item's Hive work/merge eligibility; other items continue. Held PR cannot merge via Hive; an issue hold is not a retroactive hold on every existing linked PR, so hold those too. | GitHub label-capable maintainer; dashboard owner/repo-write permission. Remove label/resume item after review; confirm head unchanged or re-review. [Item API](../pkg/dashboard/api_repo_hold.go), [client](../pkg/github/client.go), [hold guard](../cmd/hive/main_helpers.go). |
| One repo: Auto-merge off | All Hive merge paths for that repo; PR creation can continue. Existing PRs **cannot** merge through Hive. Repo Pause additionally removes repo work enumeration/sweep activity. | Owner or repo-write user may switch off/pause; **owner** required to restore auto-merge. Resume repo and separately restore chosen merge policy. Confirm card + refused test merge. [Repo API](../pkg/dashboard/api_repo_pause.go), [repo policy](../pkg/config/repo_policy.go). |
| One agent: Pause | That worker's execution/scheduling; other workers and App sweep remain. Existing green PRs **can** still merge. | Dashboard owner; Resume reverses manual pause. Confirm agent paused state. [Agent control](../pkg/dashboard/api_agent_control.go), [pause implementation](../crux/agent/manager_pause.go). |
| Every agent: fleet breaker, plus explicitly pause any remaining on-demand workers | Breaker pauses currently running non-on-demand agents, preserving already-paused states; it is **not a merge brake**. Existing PRs **can** merge via sweep. | Owner, using the fleet breaker button in the dashboard's top bar. Release resumes only breaker-owned pauses; manually paused agents stay paused. Confirm captured set and each agent status. [Breaker API](../pkg/dashboard/api_breaker.go), [pause implementation](../crux/agent/manager_pause.go). |
| All automatic merging: disable Auto-merge on every repo, or step down below L6 | Repo gate blocks Hive merge authority on existing and new PRs. Workers can still create work according to the remaining level/modes. Disabling only `auto_merge.self_authored` stops **only the App sweep**, not the scanner lane; restarting relays may be necessary for startup-captured sweep flags. | Owner for level change; owner/repo-write for repo off. Restore selected repo switches as owner; re-applying L6 enables active repos again. Confirm each repository card and the **Audit Log**. [Repo policy](../pkg/config/repo_policy.go), [pack API](../pkg/dashboard/api_packs.go), [sweep wiring](../cmd/hive/main_sweeps.go). |
| Contributor work: Suspend contributions, revoke/stop active relays if necessary | Stops offering/admitting contribution work as configured, not Hive's other workers or automatic merges. Existing contributor PRs **can** merge; active work must be handled separately. | Owner in contribution admin; contributor can stop their own relay. Unsuspend/re-authorize only after triage; confirm queue paused and active leases/workers accounted for. [Contribution API](../pkg/dashboard/api_contribute.go), [relay reference](contributor-relay.md). |

These controls stop **Hive**, not a separate GitHub bot or a maintainer manually merging. For an immediate repo-wide incident boundary, a GitHub administrator can also revoke/suspend the App installation's access; coordinate restoration and verify credential failures, rather than silently weakening branch rules. **JUDGMENT CALL:** use this larger boundary if Hive's runtime state cannot be trusted. No Hive setting enforces that incident policy.

### After a bad merge

**S2. Contain, reverse, prove prevention.** **JUDGMENT CALL:** use this ordered recovery procedure. Hive does not automatically enforce it. Perform changes with an authorized maintainer and keep the incident record on GitHub; verification is the artifact from each step, not "agent says fixed".

1. **Prevent further harm.** Turn the affected repo's Auto-merge off; if scope is unknown disable all repos or lower the level. Pause harmful workers/contributions too. Confirm persisted switches and stopped merge activity using S1. Do not merely exhaust a budget or pause agents.
2. Record the PR's head and merge SHA, the merge's entry in the dashboard's **Audit Log**, the required-check results on GitHub, and the deployment revision. Work out whether the sweep, the scanner, a queued merge or another actor landed it. Sources: [merge audit](../pkg/github/automerge/automerge_sweep.go), [relay merge](../pkg/github/pullrequest.go).
3. Revert through a human-reviewed PR (or use the project's authorized emergency procedure), run CI, and verify the destination branch no longer contains the bad behavior. If deployed, roll back the application too and verify the running revision plus the failing user scenario; a Git revert alone is not a deployment rollback. For Hive itself, use the [digest-verifiable rollback runbook](release-rollback.md).
4. Determine the missed protection: absent context, optional coverage, stale base, unchecked integration behavior, bypass, or incorrect scope. Record an accurate knowledge fact and correction so future workers do not repeat the assumption.
5. **Add or strengthen a required check before restoring automation.** Run the new regression/check against the bad PR's exact head (it must fail) and the reverted/fixed head (it must pass). Verify the context is required on GitHub and in Hive's declared set, with no App bypass. Then restore only the qualified repo. CI gates, not the recovery prose, enforce future merging: [commit CI](../pkg/github/commit_ci.go), [CI gate](../pkg/github/merge_ci_gate.go).

### When work is stuck

**S3. Repair the blocking system, not the symptom.** Identify the row below from the reason the dashboard or GitHub shows before changing policy; verify recovery at the same PR head or a deliberately repaired head. Hive reports/enforces the cited gates, not an obligation to make every queue item merge.

| Cause / observable sign | Action and confirmation |
| --- | --- |
| Suspected attack / an `ioscan_fail_closed` entry in **Audit Log**, and the item gets no kick | Inspect hostile text or false positive, sanitize/remove the input or hold it for a maintainer, then retry a safe prompt. Do not globally fail-open to drain the queue. Verify a safe kick and retained scan protection. [Kick input enforcement](../pkg/scheduler/ioscan_enforce.go). |
| Escalated repair / `needs-human` on the PR after repeated failed heads | Read evidence, decide repair versus close; hold the PR if it must not merge meanwhile. Adjudicator may repair/de-escalate hive PRs at L6, but is not a human authorization substitute. Verify the repaired CI and that the label is cleared before releasing a real hold. [Escalation](../pkg/escalation/escalation.go), [pack](../pkg/config/packs/level-6.yaml). |
| GitHub refused merge / a merge-blocked alert in the dashboard's alert banner, or the reason shown on the PR; a branch that is behind or in conflict | Fix the specific permission/rule/signature/check; ask fork author to update when Hive cannot push. Retry at the verified new head. Never treat "mergeable" from an old snapshot as a merge result. [Block reasons](../pkg/github/merge_block_reason.go), [sweep](../pkg/github/automerge/automerge_sweep.go). |
| Spending limit / budget exhausted or provider-limit alert, no new kicks | Compare token window to provider money/quota; wait for reset or deliberately fund a realistic limit. Verify a successful probe/kick and falling queue. Existing PR merging is separate; use repo-off if that must stop. [Governor budget](../pkg/governor/governor.go), [provider budget](../pkg/governor/budget_notify.go). |
| Checking infrastructure broken / same runner/startup failure across unrelated PRs | **JUDGMENT CALL:** stop affected repo auto-merging, repair runners/workflow plumbing, then rerun existing PRs. Do not ask agents to write unrelated tests or remove required coverage to fix a harness fault. Verify required contexts actually execute and pass, and a seeded failing case still refuses. Hive's CI gate refuses opaque failed workflows; [CI gate](../pkg/github/merge_ci_gate.go). |

**S4. Step down when confidence in unattended landing is lost.** **JUDGMENT CALL:** repeated bad merges, unavailable required CI, or inability to explain active authority warrant L5 while investigating. Owner changes the level through the level/pack control; verify as R3. Hive enforces repo merge-off below L6, so **already-open** PRs lose Hive automatic merge eligibility; it does not revert their commits or retroactively add a level hold to every old PR. Subsequent agent PRs at L3–L5 receive the level hold; cross-repo outreach stays held at all levels. Manual/other-bot merges remain outside this stop. Promotion later leaves old holds intact unless explicitly released, and enables active repos again. See [repo policy](../pkg/config/repo_policy.go), `shouldHoldAgentPR` in [eligibility](../cmd/hive/merge_eligibility.go), [level hold](../pkg/github/pr_level_hold.go), [pack API](../pkg/dashboard/api_packs.go).

Maintainer review of this guide and its promotion belong to [#10515](https://github.com/hivecommons/hive/issues/10515), not an agent review. Until that sign-off, retain the notice and the index's awaiting-review label.
