# Hive Labels and Control Signals

This is the operator view of Hive labels: if you add or remove a label, this is what the v5 code does. It also calls out non-label controls that look like labels from the dashboard.

## How labels reach agents

Most hard gates run before an agent sees work. GitHub issue enumeration reads labels in this order: standing meta issues are excluded first, then hold labels move the item to the Hold list, then exempt labels and issue-level `needs-human` filter it out, then `project.issue_filter.require_labels` admits or rejects the issue, and only then is it actionable (`src/pkg/github/client.go:955-1045`). Pull requests use the same hold-first enumeration for the PR Hold list (`src/pkg/github/client.go:1110-1165`). That means kick prompts, dashboard actionable counts, planning-from-label, and contributor offers normally inherit the same gate.

Some sweeps bypass that enumeration and list items themselves:

- Auto-merge sweeps check generic hold/exempt labels directly, so they do not inherit the dashboard-only `hive-pause/<hive-id>` exact hold unless the path also filters paused repos (`src/pkg/github/automerge/automerge_sweep.go:402-424`, `src/pkg/github/automerge/automerge_sweep.go:512-566`, `src/pkg/github/automerge/automerge_sweep.go:805-1036`).
- The task-list sweep lists issues itself and skips exempt labels plus issue `needs-human`; it does not call the hold gate (`src/pkg/github/task_list_sweep.go:790-810`).
- The SHA-hold sweep lists primary-repo `kind/bug` issues itself and adds or removes literal `hold` based on SHA evidence (`src/pkg/github/client.go:2450-2538`).
- PR-request watching applies server-side PR holds after a PR is opened; the agent's `--label hold` flag is deliberately discarded by the wrapper (`src/pkg/github/pr_request_watcher.go:448-520`, `bin/hive-open-pr.sh:142-150`).

The "respect hold labels" text in policy templates is therefore a prompt-level backstop. The hard gate is the enumerator or sweep named in the table.

## Categories

- **Informational / display-only** changes how Hive explains an item, not whether agents can act.
- **Workflow / status** marks a process state such as reviewer outcome or rebase need.
- **Gate / permit** admits, blocks, or releases a specific workflow.
- **Hold / suppress** removes work from agent/contributor/merge lanes until cleared.
- **Contributor eligibility / routing** affects `/contribute` offers or agent lane choice.
- **Human approval / acknowledgement** records that a human accepted a direction or design.

## Reference table

| Label or signal | Applies to | Consumer | Effect | Gate or signal | Who applies | When checked | Cleared / overridden | Config knobs |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `hold`, `on-hold`, `hold/review` | Issues, PRs | GitHub enumeration and auto-merge sweeps | Any label containing a configured hold substring is held; held issues/PRs leave agent kicks and PR merge lanes. Held red PRs still route back to their owning agent for CI repair, with instructions not to remove the hold. | Hard hold | Human, Hive level gate, #5117 gate, holdguard, SHA-hold | Enumeration and merge sweeps | Remove the matching label; some Hive sweeps remove literal `hold` only | Built-in `HoldLabels`; dashboard exact hold via `Client.SetHoldLabels` (`src/pkg/github/client.go:750-755`, `src/pkg/github/client.go:1886-1959`, `src/pkg/scheduler/scheduler.go:1260-1280`) |
| `hive-pause/<hive-id>` | Issues, PRs | GitHub enumeration/dashboard hold toggle | Exact, hive-scoped dashboard hold. It is not provenance and intentionally avoids the substring `hold`. | Hard hold for enumeration | Dashboard operator | Enumeration | Dashboard Release removes labels causing the hold, including this one | Canonicalized from hive id (`src/pkg/github/client.go:1890-1925`, `src/pkg/github/client.go:1928-1943`, `src/docs/dashboard.md:80-96`) |
| `hive/<hive-id>` | Issues | Provenance/migration fallback only | Marks hive provenance. It is no longer a hold label except a temporary failed-migration fallback during upgrade. | Informational | Hive | Display/migration | Remove if unwanted; do not use as a hold | Hive id (`src/pkg/github/client.go:1945-1953`) |
| `hold` from ACMM level gate | PRs | PR-request watcher, self-authored auto-merge release | Non-outreach agent PRs at L3-L5 get literal `hold`; `outreach` PRs are held at every level. The watcher, not the policy prompt, applies it. | Hard merge gate | Hive App | PR creation, later self-authored auto-merge release | Auto-release only when current policy no longer requires the level hold, latest hold event was by the App, and the release path runs; otherwise a human removes it | Hive-wide ACMM and agent (`src/pkg/github/pr_request_watcher.go:448-520`, `src/pkg/github/pr_level_hold.go:16-124`) |
| `hold` from #5117 self-authorization | PRs | PR-request watcher and self-authorization release | A PR whose only rationale is unacknowledged hive-filed issues gets literal `hold`. Acknowledging the issue later does not remove an existing PR hold by itself. | Hard merge gate | Hive App | PR creation; evaluated only when no level hold applies | Acknowledge the issue and remove the PR `hold`; disabled policy release can remove Hive's own hold | `github.self_authorization_hold`, per-repo `project.repo_policies[].self_authorization_hold`, `HIVE_SELF_AUTHORIZATION_HOLD` (`src/pkg/github/pr_self_authorization.go:14-226`, `src/pkg/github/pr_request_watcher.go:448-520`) |
| `hold` from holdguard | PRs | Holdguard ledger | If the head SHA changes while held, lifting the hold causes Hive to comment and re-apply literal `hold`; the next human removal is the fresh approval. | Hard merge gate | Hive | Governor holdguard pass | Human removes the re-applied hold | Built-in `holdguard.ReHoldLabel` (`src/pkg/holdguard/holdguard.go:1-43`) |
| `hold` from SHA-hold | Issues | SHA-hold sweep | Human-filed primary-repo `kind/bug` without a 7-40 hex SHA gets `hold`; once a SHA appears in body/comments, Hive removes literal `hold` regardless of who applied it. | Hard issue hold | Hive | Eval tick SHA sweep | Add SHA evidence; sweep removes `hold` | Primary repo and SHA-hold config (`src/pkg/github/client.go:2450-2538`) |
| `do-not-merge` and `do-not-merge*` | Issues, PRs | Exempt filter, merge sweeps, task-list sweep | Permanently exempt; exact match is case-insensitive but prefix matching follows Go `strings.HasPrefix` on the original label. | Hard suppress | Human | Enumeration/sweeps | Human removes | Built-in `PermanentExemptLabels` (`src/pkg/github/client.go:750-755`, `src/pkg/github/client.go:2030-2048`) |
| `governor.labels.exempt` entries | Issues, PRs | Exempt filter | Same exempt behavior as `do-not-merge`; wins over required-label admission. Defaults include `nightly-tests`, `LFX`, `meta-tracker`, `auto-qa-tuning-report`, `adopters`, `changes-requested`, `waiting-on-author`. | Hard suppress | Operator/dashboard | Enumeration/sweeps | Remove label or config entry | `governor.labels.exempt` (`src/pkg/config/config.go:3388-3396`, `src/pkg/config/config.go:5800-5808`) |
| `needs-human` | Issues | Enumeration and task-list sweep | Issue is filtered, not held: no agent kick or contributor offer; task-list sweep can add it when a merged PR leaves human remainder work. | Hard suppress | Hive or human | Enumeration/task-list sweep | Human removes | Fixed label (`src/pkg/github/client.go:1008-1024`, `src/pkg/github/task_list_sweep.go:600-639`, `src/pkg/github/task_list_sweep.go:790-810`) |
| `needs-human` | PRs | Escalation ledger and reviewer lane | Applied when red-CI fix budget is exhausted; stops automated fix dispatch and sends the PR to the human/reviewer lane. It is not a PR enumeration filter. | Hard fix-dispatch gate | Hive | Escalation pass/kick building | Human removes after root cause or reviewer un-escalates; budget resets after grace | Fixed label (`src/pkg/escalation/escalation.go:250-310`, `src/pkg/escalation/escalation.go:760-790`) |
| `hive/advisory` | Issues | Standing meta classifier | Hive advisory report is never actionable and never appears on the Hold list. | Hard exclusion | Hive | Before holds/exempts | Remove label, but exact advisory title still excludes | Fixed label/name (`src/pkg/github/standing_issues.go:1-57`, `src/pkg/github/client.go:983-1001`) |
| `approved-direction` | Agent-filed issues | #5117 gate, ranking, kick tag | A PR opened after the label is present avoids #5117 hold; the issue ranks ahead of unacknowledged hive-filed backlog. | Gate input + soft ranking | Human | PR creation and issue ranking | Remove label; human assignee/comment can still acknowledge for #5117 | Fixed `HumanAckLabel` (`src/pkg/github/pr_self_authorization.go:14-226`, `src/pkg/github/client.go:2683-2765`) |
| Human assignee | Issues | #5117 gate and ranking | Human assignee acknowledges hive-filed direction and moves issue into rank tier 2. | Non-label approval | Human | PR creation/ranking | Unassign | N/A (`src/pkg/github/pr_self_authorization.go:180-226`, `src/pkg/github/client.go:2720-2765`) |
| Human comment | Issues | #5117 gate | Any human comment in the first 100 issue comments acknowledges for #5117, but does not change ranking tier by itself. | Non-label approval | Human | PR creation | N/A | `selfAuthCommentPageSize` (`src/pkg/github/pr_self_authorization.go:14-18`, `src/pkg/github/pr_self_authorization.go:180-226`) |
| `hive: reporter-confirmed` label/body text | Issues | PR claim validation | Allows a human-filed bug to keep `Closes` instead of being rewritten to `Refs` for reporter confirmation. | Permit | Human/reporter | PR request claim validation | Remove label/text | Fixed phrase (`src/pkg/github/pr_request_claims.go:266-383`) |
| `design-approved` | Issues | Planning design gate | Approves a requested architect design; counts after a design exists. | Planning gate | Human | Planning label sweep | Remove/re-apply design labels as needed | `planning.design_approved_label` (`src/pkg/planning/design.go:68-125`, `src/pkg/planning/design.go:320-345`) |
| Lane-name label or `agent/<role>` segment | Issues | Classifier/scheduler | Routes issue to a lane after title prefix and before keyword routing. Scanner sees every issue; other agents see their lane. | Soft routing | Hive/human | Classification before kicks | Change/remove label | `agents.*.lane_keywords` (`src/pkg/classify/classifier.go:249-276`, `src/pkg/scheduler/scheduler.go:2369-2405`) |
| `agent/<role>` | Issues, PRs | Provenance, ownership, dashboard bands | Marks the filing/owning agent; PR ownership reads the suffix as a lane, while issue labels may be produced from display name. | Informational + routing | Hive | Issue/PR creation and display | Remove if wrong | Agent identity/display settings (`bin/gh-wrapper.sh:1068-1108`, `src/pkg/scheduler/scheduler.go:2740-2760`) |
| Priority labels (`triage/accepted`, `ai-fix-requested`, `approved-direction`, `kind/bug`, `bug`, `priority/critical-urgent`, `priority/important-soon`, `help wanted`, `good first issue`) | Human-filed issues | Ranking | Human-filed issues with these labels go to tier 0 in kick lists. | Soft ordering | Human/Hive | Ranking | Remove label | `HIVE_ACTIONABLE_PRIORITY_LABELS` (`src/pkg/github/client.go:2683-2765`) |
| `auto-qa`, `auto-qa-finding`, `kind/security`, `kind/regression` | Issues | Classifier | `auto-qa`/`auto-qa-finding` make Simple; `kind/security`/`kind/regression` make Complex. | Soft classification | Hive/human | Classification | Remove label | Classifier tables (`src/pkg/classify/classifier.go:279-303`) |
| `run/spec`, `run/fix` | Issues | Runs triage | Forces spec or direct-fix triage when runs triage is enabled. | Gate when enabled | Human/Hive | Triage | Remove label | `runs.triage.enabled` (`src/pkg/classify/triage.go:1-55`) |
| `runs.triage.spec_labels` / `fix_labels` (v6/edge Spek) | Issues | Spek/runs triage | Configured exact labels route to spec or fix; mark this v6/edge when documenting Spek behavior. | Gate when enabled | Operator/human | Triage | Remove label/config | `runs.triage.spec_labels`, `runs.triage.fix_labels` (`src/pkg/classify/triage.go:35-55`) |
| `tracker`, `meta-tracker`, `tracking`, `epic` (last segment) | Issues | Tracker detector, PR claim rewrite, contributor queue | Marks coordination-only/tracker issues; agents see tracker tags, contributors do not get them. | Soft for agents; hard for contributors | Human/Hive | Enumeration/contributor admission | Remove label or close tracker | Tracker detector (`src/pkg/github/client.go:2090-2156`) |
| `hive-plan`, `hive-design` | Issues | Planning label sweep | `hive-plan` mints/decomposes an epic; `hive-design` asks architect for design first. Only works when planning-from-label is enabled and ACMM >= 5. | Gate when enabled | Human | Planning sweep | Remove label or process plan/design | `planning.plan_from_label`, `planning.plan_labels`, `planning.design_labels` (`src/pkg/config/config.go:400-460`, `src/pkg/planning/issue.go:483-510`) |
| `lgtm` | PRs | Queued auto-merge sweep | Queue label for human/owner merge action; the sweep also requires its normal authorization and skips held/exempt PRs. Adding by hand is not enough if the App approval/authorization is absent. | Permit | Dashboard/merger | Auto-merge sweep | Remove label; head changes can de-queue | `governor.labels.automerge`, default `lgtm` (`src/pkg/config/config.go:3388-3396`, `src/pkg/config/config.go:4952-4957`, `src/pkg/github/automerge/automerge_sweep.go:966-1036`) |
| `reviewer-passed` | PRs | Reviewer lane/escalation reconciliation | Marks one reviewer pass/de-escalation; with no `needs-human`, un-escalates. | Workflow | Reviewer lane/Hive | Reviewer/escalation pass | Remove only if intentionally re-reviewing | Fixed label (`src/pkg/escalation/escalation.go:760-790`) |
| `reviewer-recommend-close` | PRs | Reviewer lane | Reviewer recommends closing rather than continuing automated repair. | Workflow | Reviewer lane | Reviewer pass | Human decides/clears | Fixed label (`src/pkg/scheduler/reviewer_lane.go:296-305`) |
| `review.human_decision_label` | PRs | Human-decision mirror | Mirrors review `requires_human` verdict to an existing repo label. It gates nothing and Hive does not create/remove it. | Informational | Hive if label exists | Review verdict | Human removes | `review.human_decision_label` (`src/pkg/github/human_decision_label.go:1-36`, `src/pkg/config/config.go:7186-7203`) |
| `needs-rebase` | PRs | Scanner/kick annotation | Fills mergeability annotation in kick data. | Informational | Scanner/Hive | PR status processing | Remove after rebase | Fixed label (`src/pkg/scheduler/scheduler.go:2715-2730`) |
| `hive/covered-by-pr` | Issues | PR-claim label sync and dashboard | Open PR is verified as related; issue remains actionable. Labels are synced only for actionable issues, so stale labels can remain on held/exempt/filtered issues. | Display-only | Hive | Claim sync after enumeration | Hive removes when actionable issue no longer has open PR evidence | Fixed label (`src/pkg/github/prclaims.go:1380-1435`) |
| `hive/likely-done` | Issues | PR-claim label sync and dashboard | Merged PR is verified as related while issue remains open; issue remains actionable until GitHub/operator closes or confirms. | Display-only | Hive | Claim sync after enumeration | Hive removes when actionable evidence no longer says likely done | Fixed label (`src/pkg/github/prclaims.go:1380-1435`) |
| `hive/already-done` | Issues | Contributor queue and dashboard | Contributor already-done verdict/confirmation; default contributor skip label and done band. Spoke agents can still receive the issue unless another gate also filters it. | Contributor hard skip; dashboard display | Hive/contributor flow | Contributor admission | Human removes or config changes; optional close behavior | `hub.contribute_already_done_label`, `contribute_close_already_done` (`src/pkg/config/config.go:4442-4572`, `src/pkg/config/config.go:4585-4685`) |
| `blocked` | Issues | Contributor queue and dashboard issue bands | Always included in contributor skip patterns and default waiting band. Does not by itself stop spoke-agent enumeration unless also exempt/held/filtered. | Contributor hard skip; display | Human | Contributor admission/dashboard render | Remove label | `hub.contribute_skip_labels`, dashboard bands (`src/pkg/config/config.go:4585-4685`, `src/docs/dashboard.md:105-140`) |
| `needs-decision` | Issues | Contributor relay and dashboard issue bands | Relay can apply it when a maintainer decision is needed; contributor queue skips it. | Contributor hard skip; display | Relay/Hive/human | Contributor admission/dashboard render | Human removes; empty config disables relay application | `hub.contribute_needs_decision_label` (`src/pkg/config/config.go:4397-4408`, `src/pkg/config/config.go:4645-4685`) |
| `needs-triage`, `discussion`, `question`, `tracking`, `epic` | Issues | Contributor queue | Default contributor skip labels/patterns. | Contributor hard skip | Human/Hive | Contributor admission | Remove label or config | `hub.contribute_skip_labels`, `HIVE_CONTRIBUTE_SKIP_LABELS` (`src/pkg/config/config.go:4585-4685`) |
| Contributor allow/deny label filters | Issues | Contributor queue | Hive-wide label filters can run in deny mode (skip if any label matches) or allow mode (offer only if at least one label matches); per-repo filters layer on top and can only narrow offers. Patterns use Hive wildcard/substring matching, not GitHub hold matching. | Contributor hard gate | Operator | Contributor admission | Edit filter mode/list or per-repo filter | `hub.contribute_labels_mode`, `contribute_deny_labels`, `contribute_allow_labels`, `contribute_repo_filters` (`src/pkg/config/config.go:4388-4420`, `src/pkg/config/config.go:6940-6994`, `src/pkg/config/config.go:7008-7055`) |
| `1-triage`, `2-discussing`, stage labels | Issues | Dashboard repo-card bands/legend | Stage vocabulary is display-only unless another consumer names the same label. `2-discussing` ships in the default waiting band; `1-triage` is just a visible repo taxonomy label unless configured elsewhere. | Display-only | Human/repo automation | Dashboard render | Remove label or change band config | `dashboard.issue_bands` (`src/docs/dashboard.md:105-140`) |
| `claimed`, `preempted:<login>`, `hive/claimed-by-<agent>` | Issues | Claims/dashboard | Shows in-progress state; Go claim gates use comments/assignees/ledger, not these labels. | Display-only | Hive/claim flows | Dashboard render | Cleared by claim flow or human | `governor.claims.*` (`src/docs/dashboard.md:105-140`) |
| `from-review` | Issues | Review backlog filing | Marks issues filed from review findings. No code path uses it as a gate. | Informational | Hive | Issue creation/display | Remove label | Fixed label (`src/pkg/github/review_backlog.go:1-40`) |
| `hive: churn-triaged` | Issues | Contributor churn guard | Marks churn triage acknowledgement for contributor flow. | Workflow signal | Human/Hive | Contributor/churn checks | Remove label to re-triage | Fixed phrase in contributor flow docs/config (`src/pkg/dashboard/contribute_admission.go:302-338`) |
| `good first issue`, `help wanted`, `bug`, `enhancement` | Issues | Contributor opportunistic ordering | Small ordering boost/visibility in contributor panels; `good first issue`, `help wanted`, and `bug` also appear in default actionable priority labels for human-filed issues. | Soft ordering | Human/Hive | Contributor queue/ranking | Remove label | Contributor/ranking config (`src/pkg/github/client.go:2683-2765`) |

## Matching rules

- **GitHub generic holds** are case-insensitive substring matches against `hold`, `on-hold`, `hold/review`, plus configured non-`hive-pause/` extra holds. A label such as `threshold` contains `hold` and therefore holds (`src/pkg/github/client.go:1886-1925`).
- **`hive-pause/<hive-id>`** is case-insensitive exact match, not substring, so it does not collide with `hive/<hive-id>` provenance (`src/pkg/github/client.go:1890-1925`).
- **Exempt labels** use case-insensitive equality or prefix for permanent/configured labels, but the prefix check uses the original label string (`src/pkg/github/client.go:2030-2048`).
- **Contributor skip labels** are lowercased and matched with `path.Match` glob syntax; invalid globs fall back to exact case-insensitive matching (`src/pkg/config/config.go:4658-4708`).
- **Lane routing** lowercases labels and matches lane/routing tokens by segment, so `agent/scanner` can route to scanner; PR `agent/` ownership paths are stricter and should be treated as case-sensitive operationally (`src/pkg/classify/classifier.go:249-276`).
- **Planning and triage labels** are exact label names after normalization; defaults are prefixed (`hive-plan`, `hive-design`) so ordinary `plan` or capitalized `Epic` taxonomy does not trigger planning (`src/pkg/config/config.go:400-460`, `src/pkg/classify/triage.go:66-84`).
- **Linear holds** use GitHub-like case-insensitive substring matching with defaults plus `work_source.linear.hold_labels` (`src/pkg/worksource/linear.go:344-355`).
- **Jira holds** use exact, case-sensitive equality against configured `work_source.jira.hold_labels`; there is no built-in Jira default (`src/pkg/worksource/jira.go:50-68`, `src/pkg/worksource/jira.go:302-330`).
- **GitHub Projects** work source carries labels through but does not implement a hold-label gate (`src/pkg/worksource/github_projects.go:250-285`).

## Config that changes label behavior

- `github.self_authorization_hold`, `project.repo_policies[].self_authorization_hold`, and `HIVE_SELF_AUTHORIZATION_HOLD` change #5117 holds.
- Hive-wide ACMM level controls level holds; per-repo ACMM overrides do not make PRs skip the level hold.
- `governor.labels.exempt`, `governor.labels.automerge`, and `project.issue_filter.require_labels` decide exempt/admit/queue behavior.
- `planning.plan_from_label`, `planning.plan_labels`, `planning.design_labels`, and `planning.design_approved_label` control planning labels, with the L5+ planning floor.
- `runs.triage.enabled`, `runs.triage.spec_labels`, and `runs.triage.fix_labels` control run triage labels.
- `governor.claims.*`, `review.human_decision_label`, `hub.contribute_*`, `dashboard.issue_bands.*`, and `HIVE_ACTIONABLE_PRIORITY_LABELS` control the display/contributor/ranking labels above, including contributor label allow/deny filters and per-repo narrowing.
- `work_source.linear.hold_labels` and `work_source.jira.hold_labels` are non-GitHub label-equivalent gates with the different matching rules described above.
- Not labels: `project.paused_repos` pauses an entire repo; contributor queue holds can park `owner/repo#N` without changing GitHub labels.

## Lifecycle examples

### Strategist-filed direction to a PR

1. Strategist files an issue with `[strategist]`/`agent/strategist`. It is actionable but lower-ranked as hive-filed.
2. A maintainer adds `approved-direction`, assigns a human, or comments. Label or assignee also improves ranking; a comment only satisfies #5117.
3. An agent opens a PR citing the issue. If the acknowledgement existed before PR creation, no #5117 hold is applied. At L3-L5, the separate level hold can still apply.
4. If acknowledgement is added after a PR already has `hold`, remove the PR hold too.

### Holding an issue

1. Add `hold`/`on-hold`/`hold/review`, or use dashboard `⏸ Hold` to add `hive-pause/<hive-id>`.
2. Enumeration moves it to the Hold list. It leaves agent kicks and the contributor queue.
3. Caveats: task-list and SHA sweeps do their own listing; SHA-hold can remove literal `hold` when SHA evidence appears.
4. Remove the hold label or click `▶ Release`; it is reconsidered on the next enumeration.

### Holding a PR

1. The PR receives `hold` from a human, the level gate, #5117, holdguard, or SHA-related policy.
2. It is not merge-eligible or auto-merged. If CI is red and it is not outreach/escalated, its owning agent may still be asked to fix CI without removing the hold.
3. If the branch moves while held and then the hold is lifted, holdguard re-adds `hold`; removing it again is the fresh approval.

### Escalation

1. Red CI across the distinct-SHA budget applies PR `needs-human`; automated fix dispatch stops.
2. Reviewer lane may add `reviewer-passed` and remove/un-escalate `needs-human`, or add `reviewer-recommend-close`.
3. A human can remove `needs-human` after addressing the root cause; the ledger resets after the grace period.

## Non-label equivalents

- **Human comments and assignees** can acknowledge hive-filed directions for #5117; assignees also affect ranking.
- **Paused repos** (`project.paused_repos`) stop whole-repo write/merge/enumeration paths without labels.
- **Contributor queue holds** such as active leases, cooldowns, dependencies, and quota guard holds suppress contributor offers without touching GitHub labels.
- **Jira/Linear labels** map only where the work-source adapter supports them: Linear skips held work using substring hold labels; Jira skips exact configured hold labels; GitHub Projects currently imports labels but does not use a hold gate.

## Repo-card legend vocabulary

The repository-card legend is a UI vocabulary, not a second scheduler. Ready, in-progress, agent-filed, waiting-on-human, and likely-done bands are computed client-side from labels/assignees and `dashboard.issue_bands`; non-winning states stay as badges. State glyphs such as `⛔`, `❓`, `👤`, `✓`, role badges, stale `🕒`, PR `✓`/`◐`/`⚠`, held, reviewed `💬`, auto-merge `🔀`, and review-class badges explain the same data the table above names. Changing a band label changes the repo card description only; it does not make an item held, exempt, or actionable (`src/docs/dashboard.md:98-140`).

## Documented inconsistencies and follow-ups

The code currently has the sharp edges called out in issue #8924: auto-merge sweeps do not treat `hive-pause/<hive-id>` like a generic hold, task-list sweep skips exempt/`needs-human` but not holds, SHA-hold can remove a hold it did not add, level-hold releases depend on the self-authored sweep path, and agent policy text names fewer hold labels than the code enforces. This page documents those behaviors rather than changing them; the PR for this page files follow-up issues for code/doc cleanup that should not be mixed into this reference.
