# Hive Labels and Control Signals: Decision glossary, blockers, matching rules and examples

Part of [Hive Labels and Control Signals](../labels-and-control-signals.md).

## Decision glossary

Every rule, verdict and reason the owner-delegate decider ("Let hive decide") and the lane engine can show an operator has one plain-words entry here, kept in one place — `src/pkg/delegatedecision/decision_glossary.go`, mirrored into the dashboard as `STAGE_TRACKER_DECISION_GLOSSARY` (a test fails when the two drift) — and used everywhere the hive talks to a person about a decision: the Activity Monitor result toast, the needs-decision popover, the `/hive decision` comment the delegate posts, and tooltips. A rule slug is never shown bare: where the slug appears for the record (the comment's attribution line, the audit log) the plain words sit next to it. The owner's case that forced this was [#14822](https://github.com/hivecommons/hive/pull/14822): a toast reading "hive already recorded this decision (rule `contributor-offline`)" and a PR comment saying "supersede" told him neither why the hive could not decide nor what supersede meant.

The plain-words comment the delegate posts has four lines — **Hive decided** (the action in plain words), **Why** (the rule in plain words), **What happens next** (the expected effect and by when, `decisions.delegate.effect_timeout`), **If it doesn't happen** (what the owner can do) — above the attribution line and the machine marker (`<!-- hive:delegated-decision … -->`, unchanged). When the effect has not shown by the deadline the delegate posts one follow-up (marker `<!-- hive:decision-stalled by=<hive-id> rule=<rule> -->`, never a second decision and never parsed as one) with **What should have happened / Why it hasn't / What you can do**, and the toast for a repeat click says the same (see [Dashboard sections → Let hive decide](../dashboard-sections/activity-monitor.md#let-hive-decide)).

**Rules (why the hive decided the way it did)**

| Token | Plain words | Detail |
|---|---|---|
| `already-fixed` | the issue was already fixed by a merged PR; closed as superseded | The issue this PR fixes is already closed and a merged PR closed it (or a merged PR declares it supersedes this one), so nothing is left to deliver. The hive closes the PR as superseded with a note naming the merging PR; it never adopts it. |
| `capability-gap` | the GitHub App lacks a permission; deciding cannot clear it | The blocker is a missing GitHub App permission (for example `workflows`). Approving a direction would only re-apply needs-human, so the park stays until the owner grants the permission or a person applies the change. |
| `ci-ratchet` | a CI hygiene gate, landed as a ratchet | A new CI hygiene gate lands as a ratchet: it records today's baseline and fails only on increases, so existing work is never blocked. |
| `contributor-offline` | the author hasn't been reachable since the nudge | The PR's author (or their clanker or hive) has been offline since the hive nudged them, so the hive takes the work over instead of waiting. The decision itself names which: the author's clanker, a connected hive, or the author. |
| `dedupe` | an older issue already tracks this | An older issue already tracks the same work; one thread keeps it moving. |
| `proposal-approve-evidence` | a proposal backed by an incident or a repeated class; approved | The hive-filed proposal names an incident it would have prevented, or prior instances of the same class fixed one at a time, so its cost is justified and it is approved as proposed. |
| `proposal-not-now` | a hive-filed proposal with no incident or recurrence behind it; declined for now | The proposal shows neither an incident it would have prevented nor a repeated class it would end, so its value is speculative. The hive declines it for now and closes it as not planned; re-propose with evidence. |
| `selfreport-operator-action` | a spoke self-report only its operator can fix; not code work | The issue is a fleet self-report (expired login, credentials, disk, network, App setup) whose remedy is on the reporting spoke. The hive tells that spoke's owner what to do and keeps it out of the actionable set; no agent is sent to code. |
| `default-approve` | no special rule applies; the hive took its own recommendation | No owner rule singles this issue out, so the hive went with its own recommendation — the reversible choice that keeps work moving. |
| `default-release` | no special rule applies; a green branch is approved | No owner rule holds this PR back; its branch is green and a merge is reversible, so it is approved. |
| `design-proposal` | a proposal with clear acceptance criteria, accepted as written | The proposal states concrete acceptance criteria, so it is accepted as written; anything not stated keeps today's behaviour. |
| `fix-then-merge` | the branch isn't mergeable yet | The branch cannot merge as it is (conflicts, red CI or a missing sign-off); the hive told the author agent exactly what to fix. |
| `flaky-test` | a flaky test, quarantined with a retry | The failure is a flaky test: it is quarantined with a retry and a separate deflake task is filed, so it blocks nothing. |
| `hive-policy` | this changes the hive's own rules | This changes the hive's own settings, governance or security posture; the hive only recommends and the owner confirms. |
| `llm-delegate` | decided by the hive's model under the owner's rules | No mechanical rule applied, so the hive's model decided under the owner's standing rules; the same safety rails applied. |
| `mergeable-as-is` | mergeable and green as it is; handed to the merge queue | The head is mergeable with green CI, so no adoption is needed: the hive approves it and the merge queue takes it as it is. |
| `one-pr-per-issue` | two PRs fix the same issue; the merge queue keeps one | Two open PRs declare the same closing issue. The merge queue carries one PR per issue — a green head first, then the earlier-opened — and the other closes as superseded with credit. |
| `owner-only`, `owner_only` | only the hive owner can do this | This needs a secret, token or organization permission that only the hive owner holds; the hive cannot do it for you. |
| `prefer-contributor` | a contributor's PR and a hive PR do the same work | A contributor's PR and a hive-authored PR cover the same work; the hive prefers the contributor's when it is mergeable and current. |
| `rail-hold` | the item is on hold | The item carries the hold label; the hive never acts on a held item until a person releases the hold. |
| `rebase-then-merge` | only conflicts block it; a rebase then merge | Conflicts are the only blocker. The unattended rebase-then-merge path lands with hivecommons/hive#15104; until then the ladder records this rung and moves on. |
| `reporter-trust` | the hold on an unknown reporter is cleared because the fix is sound | The PR was held until the reporter's trust was known; its fix is green and mergeable, so the hold is cleared. |
| `return-to-contributor` | the author's clanker or hive is connected; returned to them with the fix list | The author's clanker or hive is connected (or has picked the rework up), so the hive hands the PR back to them with the exact fix list instead of adopting it. |
| `review-backlog` | the PR closed without merging, so the issue stays open | The fixing PR closed unmerged; the issue stays open and is re-triaged rather than auto-closed. |
| `sentinel-trusted` | security-sensitive files changed, but the author is trusted | Security-sensitive files changed; because the author is trusted the hive approved, but a person must still remove the sentinel label before it merges. |
| `sentinel-untrusted` | security-sensitive files changed; a maintainer must approve | Security-sensitive files changed in a PR from an author without write access; a maintainer must review and approve it. |
| `small-low-risk` | a small, low-risk change on a green branch | The change is small and low-risk (docs, CI pins, text, workflow hygiene) and CI is green, so it is approved. |

**Actions and verdicts (what the hive decided)**

| Token | Plain words | Detail |
|---|---|---|
| `accept-proposal` | accepted the proposal as written | The proposal is accepted as written with its stated defaults. |
| `accept-ratchet` | accepted as a ratchet | Accepted as a ratchet: baseline recorded now, failures only on increases. |
| `adopt` | hive takes over finishing this PR | Adopt means the hive takes over finishing this PR: it rebases, fixes CI and merges it, keeping the author's commits and sign-off. |
| `adopt-supersede`, `adopted`, `adoption/supersede`, `supersede`, `supersede-now` | close this PR and open a fresh hive-authored PR with the same commits | Supersede means the hive closes this PR and opens a fresh hive-authored PR carrying the same commits and sign-off, then finishes it itself. |
| `approve`, `approve-review`, `approved` | approved for merge | The hive approved it; the merge queue merges it once every check passes. |
| `close-as-duplicate`, `close-duplicate` | closed as a duplicate | Closed because an older item already tracks the same work. |
| `close-superseded`, `closed`, `superseded` | closed because another PR carries the work | Closed because another PR carries the same work; the author is credited there. |
| `comment-decision` | recorded the decision on GitHub | The hive posted the decision as a `/hive decision` comment, the way a maintainer would. |
| `directed`, `direction` | told the author agent what to change | The hive recorded direction for the author agent, which pushes a new commit with that change. |
| `labels-add` | added labels | The hive added labels to the item. |
| `labels-remove` | removed labels | The hive removed labels from the item. |
| `link` | cross-linked a related item | The hive left a comment linking a related issue or PR. |
| `not-now`, `close-not-now` | declined for now; closed as not planned | The hive declined the proposal for now and closed it as not planned, with the reason and what evidence would reopen it. |
| `operator-action` | told the spoke owner what to do; nothing for an agent here | The hive named the spoke owner and the one action they take (for example re-authenticate the provider). The item stays parked, gets no approved-direction, and clears when the report stops recurring. |
| `quarantine-flaky` | quarantined the flaky test | The flaky test is quarantined with a retry and a deflake task is filed. |
| `recommend-only`, `recommended` | recommended only; a person confirms | The hive only recommended; the item stays parked until a person confirms. |
| `refused` | did not decide | The hive did not decide this item; its safety rails stopped it (for example, the item is on hold). |

**Reasons (why a decision has not taken effect)**

| Token | Plain words | Detail |
|---|---|---|
| `adoption-disabled` | adoption is switched off on this hive | Stage adoption is disabled on this hive (governor.labels.stage.adoption), so a request to take a PR over is never carried out. |
| `adoption-executing` | the adoption engine is carrying it out | The decision is recorded and the adoption engine picks it up on its next stage pass (a few minutes): it pushes the commits to a hive branch and opens the hive's copy. Nothing to do yet. |
| `adoption-stalled:contributor-lane:workflow-path`, `capability:workflows-permission`, `contributor-lane:workflow-path`, `workflow-path` | the PR edits .github/workflows; the hive's App lacks the `workflows` permission, so it cannot adopt it | The PR changes files under .github/workflows. The hive's GitHub App has no `workflows` permission, so the adoption fence refuses to publish those edits from the hive. Grant the App the `workflows` permission and re-run the decision, or have a person rebase and merge it on GitHub, or close it as superseded. |
| `adoption-undone` | the hive's copy was closed without merging and the adoption was undone | The hive opened its copy, but the copy was closed without merging and a fresh one could not be opened, so the adoption was undone and the PR is back in its normal flow. Re-run the decision to adopt it again, or act on GitHub. |
| `author-association`, `contributor-lane:author-association`, `human-merge`, `ready-for-human-merge`, `untrusted-author` | no merge lane vouches for this PR's author; a maintainer merges | The PR is ready, but its author is not a collaborator and holds no trusted or merger tier, so no merge lane vouches for them and the hive never merges it unattended. A maintainer approves it (Approve and let hive merge — hive merges on the next tick once CI is green — or Merge now as maintainer), or trusts the author under 👥 Contributors. |
| `author-online` | the author's clanker or hive is connected, so hive won't take the PR over | Adoption is for authors whose clanker or hive has gone offline; this author's is connected, so the hive leaves the PR with them. |
| `blocker`, `capability`, `human-only` | the hive can't do this itself | The item is blocked on something the hive cannot do: a permission the GitHub App lacks, or a step only a person may take. No decision clears it; grant the permission (and re-check) or do the step by hand (hivecommons/hive#15099). |
| `behind` | the branch is behind the base branch | The branch is behind the base branch and must be updated before it can merge. |
| `changes-requested`, `changes_requested`, `contributor-lane:changes-requested`, `human-changes-requested`, `request_changes`, `reviewer-verdict:request_changes` | a reviewer asked for changes; inspect whether the objection is from the swarm or a human | A new commit can address the objection. A trusted human's GitHub APPROVED review on the exact head can override any non-approved **swarm** outcome at the merge review gate, including `changes_requested`/`reject`; for swarm-review purposes, `/hive decision approve` only answers `requires_human`. This does not bypass an independent human change request or any other merge queue. |
| `checks-failing`, `ci-failing`, `ci-red`, `red` | CI is failing on this branch | Required checks are failing on this branch; nothing merges until they are green. |
| `conflict`, `conflicting`, `dirty` | the branch conflicts with the base branch | The branch has merge conflicts with the base branch; it needs a rebase before anything can merge. |
| `contributor-lane:sentinel-blocked`, `sentinel`, `sentinel-alert`, `sentinel-blocked` | security-sensitive files changed; a maintainer must approve | Security-sensitive files changed. The hive never merges until a maintainer reviews and removes the sentinel label. |
| `dco`, `dco-missing` | a commit is missing its sign-off | A commit on the branch is missing its Developer Certificate of Origin sign-off; the author must sign it off. |
| `effect-unknown` | the hive did not say why | The hive could not tell from the lane reason why the decision has not taken effect. |
| `hold` | on hold; the hive does nothing until it is released | The item is on hold. The hive does nothing with it until a person releases the hold. |
| `label:needs-human`, `needs-human`, `requires_human` | waiting for human input; inspect whether it is a label or a review verdict | For an issue's `needs-human` label, an eligible maintainer command clears it on the ordinary un-park path regardless of provenance; keep `hold` if work must remain stopped. A PR's `requires_human` swarm verdict is answered by exact-head approval as described in the review reference; it is not itself a label to remove. Other gates and label-driven repository policy remain authoritative. |
| `no-effect` | the decision hasn't taken effect | The hive recorded this decision, but what it was supposed to cause has not happened within the expected time. |
| `no-new-commit` | no new commit has been pushed | The hive gave direction, but no new commit has been pushed yet. |
| `not-closed-yet` | the close didn't land | The hive decided to close it, but it is still open; the close call did not land. |
| `not-merged-yet` | the merge queue hasn't merged it | The hive approved it but the merge queue has not merged it; its lane reason says what still blocks it. |
| `self-decision-read-as-changes` | hive's own decision comment was read as a reviewer asking for changes | The merge queue read the hive's own `/hive decision` comment as a reviewer asking for changes, so the decision blocked the PR it meant to advance (hivecommons/hive#14822). A new commit or a maintainer merge clears it. |
| `still-parked` | the item is still waiting on a person | The item is still parked for a person's decision; the recorded decision did not move it. |

A `/hive decision …` comment the hive itself wrote as the owner's delegate (it carries the delegated-decision marker) is never read as a maintainer's command: not as a change request on the PR, not as an approval. [#14822](https://github.com/hivecommons/hive/pull/14822) fixes the parse of `/hive decision approve <rationale>`; the marker guard covers every other decision text the delegate can post, whichever login it was posted under.

## Blockers (`hive:blocker` marker)

`needs-human` says *a person is needed*; it does not say *what for*. When the reason is a **capability gap** — the GitHub App cannot do the next step at all — a direction decision answers a question nobody asked, and "Let hive decide" would un-park the issue only for the next sweep to re-park it with the same text (hivecommons/hive#14976 → #15099). So an agent that parks an item because the hive *cannot* act records a structured blocker next to the label:

```
<!-- hive:blocker kind=capability:workflows-permission -->
```

| Kind | Meaning | Clears when |
|---|---|---|
| `capability:workflows-permission` | the App token cannot push `.github/workflows/*` (no `workflows: write`) | the permission is granted and re-checked |
| `capability:repo-write` | the App has no `contents: write` on the repo | the permission is granted and re-checked |
| `capability:branch-protection` | a protection rule the App cannot satisfy or bypass | a person changes the rule |
| `capability:secret` | a secret or token only the owner holds | a person sets it |
| `human-only:cla` | a CLA / legal sign-off only a person can give | a person signs |
| `human-only:re-asked` | the same needs-human reason came back after a delegated un-park (loop guard) | the reason text changes |

Where it comes from:

- **Agents** put the marker in the body of the needs-human report (the task-list sweep copies a merged PR's remainder into its progress comment and adds the marker when the prose names a capability). `bin/gh-wrapper.sh` adds the marker itself when a needs-human comment says "the App cannot … (no `workflows` permission)" and no marker is present, so a report written in plain prose still lands structured.
- **The hive** reads it at every comment path — the `issue_comment` webhook, the task-list sweep, the stage pass — into `pkg/blocker`'s ledger (`/data/item-blockers.json`), inferring the kind from the prose when the marker is missing.

What it changes:

- The issue stays 045 with the reason `blocked: App lacks workflows permission`; the tracker pill reads **Blocked · App lacks workflows permission** and its popover says in plain words what the hive cannot do and lists the ways out (grant the permission, re-check, prepare the patch for a person where an identity exists, mark partially addressed and close, keep waiting).
- **"Let hive decide" returns `no-effect`** (reason `blocker`) instead of applying `approved-direction`; it is shown disabled with that sentence, not greyed silently.
- **Loop guard**: a delegated un-park records the reason it cleared. If the same reason is re-applied afterwards, the item is parked with a blocker (`human-only:re-asked` when no capability is named) and "Let hive decide" stays disabled on it until the reason changes.
- **Label consistency**: `stage/045-needs-decision` and `approved-direction` cannot coexist. `needs-human` is always the newer signal next to `approved-direction` (the un-park removes `needs-human` when it applies the ack), so the stage pass keeps 045 and clears `approved-direction` — a person or sweep that re-parked after a delegated decision is never silently un-parked again.
- `/api/status.capabilities` lists the missing permissions and the count of items blocked on them; ⚙ → Security shows the same line with a link to the App settings page. **Re-check permissions** on a row probes the installation and clears the blocker when the permission is there.

## How labels reach agents

Most hard gates run before an agent sees work. Enumeration first excludes standing meta issues, then moves held items to the Hold list. It next excludes exempt/tracking work and issues carrying `blocked`, reporter-confirmation or hard-suppress labels. The blocked test is independent of the hold/exempt tests: a bare `blocked` label is sufficient to stop issue admission.

The issue-level hard-suppress set includes `needs-human`, `needs-direction`, `needs-decision`, `needs-spec` and decision/prior-art stage parks (`hardSuppressIssueLabels` in `src/pkg/github/labels.go`). The configured `project.issue_filter.require_labels` then admits or rejects remaining work (`src/pkg/github/client.go:1690-1899`). Reporter-trust triage also applies when enabled; the exact ordering and visibility effects are recorded in its reference entry.

Pull requests use the same hold-first enumeration for the PR Hold list (`src/pkg/github/client.go:1975-2165`). Kick prompts, dashboard actionable counts, planning-from-label and contributor offers normally inherit enumeration's admission gates. The activity monitor lists excluded items too; that is visibility, not admission.

Some sweeps list items themselves. They still use the same hold predicate (`Client.IsHeldLabels` / `Client.isHeld`, `src/pkg/github/client.go:3434-3442`, `src/pkg/github/client.go:4399-4401`), including generic hold substrings and the exact dashboard `hive-pause/<hive-id>`:

- Auto-merge sweeps use `Transport.IsHeldLabels`. The queued (`lgtm`) sweep skips a held PR before fetching it; the self-authored sweep skips held PRs and `needs-human` before merge (`src/pkg/github/automerge/automerge_sweep.go:23-38`, `src/pkg/github/automerge/automerge_sweep.go:959-964`, `src/pkg/github/automerge/automerge_sweep.go:966-1004`, `src/pkg/github/automerge/automerge_sweep.go:1029-1046`).
- The task-list sweep skips held issues before its exempt / issue `needs-human` gate. It does not comment on, relabel or close them (`src/pkg/github/task_list_sweep.go:848-854`).
- The SHA-hold sweep lists primary-repo `kind/bug` issues itself. Without a SHA it adds literal `hold` and a marker notice; trusted reporter associations instead get soft `issues.sha_hold_trusted_label` (default `needs-sha`, #12002). It removes only its own current hold: the newest label event must be a `labeled` by the App bot with an App-authored SHA-hold notice at or after it (`EnforceSHAHold` / `shaHoldIsOwn` in `src/pkg/github/client.go`).
- PR-request watching applies server-side holds after creation; the wrapper discards an agent's `--label hold` (`src/pkg/github/pr_request_watcher.go:452-524`, `bin/hive-open-pr.sh:142-150`).

Policy templates also say to respect `hold`, `on-hold`, `hold/review`, `hive-pause/<hive-id>`, any label containing `hold`, and exempt `do-not-merge`. That instruction is a prompt-level backstop; enumeration and sweep checks are the hard enforcement.

## Matching rules

- **GitHub generic holds** are case-insensitive substring matches against `hold`, `on-hold`, `hold/review`, plus configured non-`hive-pause/` extra holds. A label such as `threshold` contains `hold` and therefore holds (`src/pkg/github/client.go:1899-1937`).
- **`hive-pause/<hive-id>`** is case-insensitive exact match, not substring, so it never collided with the retired `hive/<hive-id>` provenance label (`src/pkg/github/client.go:1903-1937`).
- **Exempt labels** use case-insensitive equality or prefix for permanent/configured labels, but the prefix check uses the original label string (`src/pkg/github/client.go:2035-2053`).
- **Contributor skip labels** are lowercased and matched with `path.Match` glob syntax; invalid globs fall back to exact case-insensitive matching (`src/pkg/config/hub_config.go:427-488`).
- **Lane routing** lowercases labels and matches lane/routing tokens by segment, so `agent/scanner` can route to scanner; PR `agent/` ownership paths are stricter and should be treated as case-sensitive operationally. The wrapper writes `agent/<lane>` from `HIVE_AGENT`, so the label always carries a routable token (`src/pkg/classify/classifier.go:249-276`, `bin/gh-wrapper.sh:1068-1113`).
- **Planning and triage labels** are exact label names after normalization; defaults are prefixed (`hive-plan`, `hive-design`) so ordinary `plan` or capitalized `Epic` taxonomy does not trigger planning (`src/pkg/config/planning_config.go:35-56`, `src/pkg/classify/triage.go:66-84`).
- **Linear holds** use GitHub-like case-insensitive substring matching with defaults plus `work_source.linear.hold_labels` (`src/pkg/worksource/linear.go:344-355`).
- **Jira holds** use exact, case-sensitive equality against configured `work_source.jira.hold_labels`; there is no built-in Jira default (`src/pkg/worksource/jira.go:50-68`, `src/pkg/worksource/jira.go:302-330`).
- **GitHub Projects** work source carries labels through but does not implement a hold-label gate (`src/pkg/worksource/github_projects.go:435-492`).

## Config that changes label behavior

- `github.self_authorization_hold`, `project.repo_policies[].self_authorization_hold`, and `HIVE_SELF_AUTHORIZATION_HOLD` change #5117 holds. When all are unset, the default follows the live ACMM level: on through L5, off at L6 Autonomous.
- Hive-wide ACMM level controls level holds; per-repo ACMM overrides do not make PRs skip the level hold.
- `governor.labels.exempt`, `governor.labels.automerge`, and `project.issue_filter.require_labels` decide exempt/admit/queue behavior.
- `project.issue_filter.reporter_trust.*` (admission by reporter), `github.reporter_trust_hold`, `project.repo_policies[].reporter_trust_hold`, and `HIVE_REPORTER_TRUST_HOLD` (the matching merge hold) are the #9665 reporter-trust gate. Off unless `reporter_trust.enabled: true`; the hold follows that switch unless set on its own. Both are edited under **Settings → Labels** (who is trusted, which triage label) and **Settings → Repos** (the hold).
- `planning.plan_from_label`, `planning.plan_labels`, `planning.design_labels`, and `planning.design_approved_label` control planning labels, with the L5+ planning floor.
- `runs.triage.enabled`, `runs.triage.spec_labels`, and `runs.triage.fix_labels` control run triage labels.
- `governor.question_autoclose.*` (`HIVE_QUESTION_AUTOCLOSE`, `HIVE_QUESTION_AUTOCLOSE_HOURS`) closes answered `question` issues; see [Question auto-close](#question-auto-close).
- `governor.claims.*`, `review.human_decision_label`, `hub.contribute_*`, `dashboard.issue_bands.*`, and `HIVE_ACTIONABLE_PRIORITY_LABELS` control the display/contributor/ranking labels above, including contributor label allow/deny filters and per-repo narrowing.
- `writeback.unsolicited.*`, `writeback.per_repo`, and `writeback.kinds.<kind>` decide whether hive may put an unsolicited label or comment on a PR it did not open at all: each kind has an ACMM floor and, by default, requires the review opt-in label (`review.require_labels`, fallback `hive-review`) on the PR. Below the gate `stage/*`, `sentinel-alert`, `needs-human`, `has-open-pr` and similar labels are tracked internally and shown in the dashboard only. See [What hive writes to GitHub, and when](../operator-reference.md#what-hive-writes-to-github-and-when).
- `work_source.linear.hold_labels` and `work_source.jira.hold_labels` are non-GitHub label-equivalent gates with the different matching rules described above.
- Not labels: `project.paused_repos` pauses an entire repo; contributor queue holds can park `owner/repo#N` without changing GitHub labels.

## Question auto-close

Off by default ([#9584](https://github.com/hivecommons/hive/issues/9584)). When on, a question issue the hive has answered is closed after a short window unless the person who asked objects, so answered questions stop inflating the open-issue count and being rescanned every sweep.

```yaml
governor:
  question_autoclose:
    enabled: true          # or HIVE_QUESTION_AUTOCLOSE=true
    hours: 4               # or HIVE_QUESTION_AUTOCLOSE_HOURS; default 4
    labels: [question, kind/question]   # default; what marks a question
    human_label: needs-human            # default; added when the author objects
```

How it works:

1. The scanner kick gains a short answer contract: an issue that only asks a question gets the `question` label and ONE answer comment, which ends with a hidden `<!-- hive-question-answer -->` marker and the line "If this doesn't answer your question, react 👎 to this comment and the issue will stay open." The footer text is built by Hive and passed through the mention sanitizer.
2. Hive watches question-labelled issues from the normal issue pass. When the last comment on one is a marked answer posted by the hive's own identity (the GitHub App bot login `<app-slug>[bot]` or `project.ai_author`), it schedules a close at answer time + `hours`. The schedule lives in `/data/question-autoclose.json`, so a restart keeps the original deadline. The marker is plain text, so a marked comment from any other account (a person or another bot pasting it) is ignored and never starts the clock; with no hive identity known, nothing is auto-closed.
3. At the deadline Hive re-reads the issue. A 👎 from the issue author on the answer keeps it open and adds `human_label`. Otherwise the issue is closed with `state_reason: completed` and no extra comment, so the answer stays the last thing Hive said.

A schedule is cancelled (never acted on) when anyone comments after the answer, the issue is closed by someone else, the question label is removed, or a bug, enhancement, hold or `human_label` label appears. Bugs (`bug`, `kind/bug`, human-filed bug reports), enhancements/features and held issues are never auto-closed. A cancelled or finished answer is remembered for 30 days so the same answer is never scheduled twice; a new answer after a follow-up starts a new window.

### Dashboard

The Governor dialog's Features tab has a "Question Auto-Close" section: a switch for `enabled` and a field for `hours` (`GET`/`PUT /api/config/governor/question-autoclose`, owner-only, same pointer/only-what-you-send contract as the other governor-config sections). Turning it on there shows a read-only live-schedule table underneath — one row per answered issue currently waiting out its objection window (repo/issue, answered-at, closes-at), backed by `GET /api/config/governor/question-autoclose/schedule`. The table is read-only: `labels`/`human_label` still need `hive.yaml`.

### Hive activity monitor

The dashboard renders every open issue and PR's stage as one unified tracker strip in the **Hive activity monitor** section (`GET /api/stage-tracker`): `Open → 010 → 020 → 030/040 (PR in progress) → 050/060 (CI / Review) → 080/100 (Merge gate) → merged`, with 060 lit as "Awaiting merge gate" (what holds it), 080 as "Ready for auto merge" (next sweep countdown), 095 as "Soaking" (time left) and 100 as "Merging" ([dashboard-sections.md](../dashboard-sections/activity-monitor.md#hive-activity-monitor) has the legend), 070/072/074 as the lit *CI / Review* step named after the side stage, 085/090 as *Merge gate* lit "Needs human decision" / "Ready for human merge", and an issue parked at 045 lit at *Actionable* (the issue side) as "Needs human decision"; an issue wearing a hold-class label is "Open · on hold" whatever else it carries, `needs-human` included ([#13012](https://github.com/hivecommons/hive/issues/13012)). 070 "Fixing" is the author's move (a `needs-rebase` chip says what is being fixed — needs-rebase always lands at 070/072, never at 050/060). An issue with exactly one covering PR is a single entry whose strip is driven by the PR's stage from 030 on; with two or more covering PRs the issue's own strip stops at 020 Claimed and each PR nests beneath as an indented sub-row (`[author/*-coloured icon] #N title` plus its own strip from *PR in progress* on), and PRs linked to a listed issue have no top-level row (`nested_under`); a PR with no linked issue is one row named once on the left, and a PR is never listed as an issue (the PR record wins over any issues-feed copy of the same number). `stage/stalled` and the store's regressed marker (the retired `stage/regressed` label's successor) appear as chips and colour the `since` value; the stage store's per-item transition history (last eight hops) is in the row tooltip. See [dashboard.md](../dashboard.md#hive-activity-monitor).

## Lifecycle examples

### Strategist-filed direction to a PR

1. Strategist files an issue with `[strategist]`/`agent/strategist`. It is actionable but lower-ranked as hive-filed.
2. A maintainer adds `approved-direction`, assigns a human, or comments. Label or assignee also improves ranking; a comment only satisfies #5117.
3. An agent opens a PR citing the issue. If the acknowledgement existed before PR creation, no #5117 hold is applied. At L3-L5, the separate level hold can still apply.
4. If acknowledgement is added after a PR already has `hold`, remove the PR hold too.

### Holding an issue

1. Add `hold`/`on-hold`/`hold/review`, or use dashboard `⏸ Hold` to add `hive-pause/<hive-id>`.
2. Enumeration moves it to the Hold list. It leaves agent kicks and the contributor queue.
3. Caveat: the task-list and SHA sweeps do their own listing but honour the same hold predicate; SHA-hold lifts only the literal `hold` it applied itself.
4. Hard-suppress reconcile (#12002): every `issues.hard_suppress_reconcile_interval` (default `30m`, unstick step of the eval loop), Hive lists open `hold`/`blocked` issues in its repos and lifts a label only when the newest event for it is a `labeled` by the App bot (for `hold`, also paired with the `<!-- hive:sha-hold -->` notice) **and** a cross-referencing PR merged after that event, in one of the hive's repos and authored by the App bot or `project.ai_author` (a mention in anyone else's PR is not a fix). It posts one note carrying `<!-- hive:hard-suppress-lifted -->`. Disable with `issues.lift_hive_hard_suppress: false`.
4. Remove the hold label or click `▶ Release`; it is reconsidered on the next enumeration.
5. Every open PR in the same repository whose body closes the issue (`Fixes #N`, `Closes #N`, …) is held with it: each merge lane (self-authored, trusted-author, queued, contributor) reads the issue once the PR's head is otherwise ready to merge and refuses with `linked-issue:hold:#N:<label>` (hold family: the configured hold labels, `hive-pause/<hive-id>`, `do-not-merge*`) or `linked-issue:needs-decision:#N:<label>` (`stage/045-needs-decision`, the retired `needs-decision` / `needs-direction` / issue-side `stage/085-needs-decision`, `needs-human`). The PR stages `085-needs-decision` and the entry comment names the issue and the label. Closed issues and other repositories' issues never hold a PR, and an open pull request the body closes is refused separately with `closing-keyword-targets-pr:#N` (`070`/`071`/`072`, [#15025](https://github.com/hivecommons/hive/issues/15025)); an issue read that fails (other than 404/410) withholds the merge for that tick (`linked-issue-check`). Nothing is remembered: removing the label from the issue lets the next sweep merge (`src/pkg/github/automerge/automerge_linked_issue.go`).

### Holding a PR

1. The PR receives `hold` from a human, the level gate, #5117, holdguard, or SHA-related policy, or receives `needs-human` from a human/escalation workflow.
2. It is not merge-eligible or auto-merged; self-authored and queued auto-merge sweeps treat `needs-human` like a live hold label. If CI is red and it is not a held outreach PR or escalated, its owning agent may still be asked to fix CI without removing the hold.
3. If the branch moves while held and then the hold is lifted, holdguard re-adds `hold`; removing it again is the fresh approval. If a human lifted the gate after the current head arrived (for example a Renovate rebase earlier during the hold), proven by GitHub's server-stamped force-push event or by the hive having seen that head while the PR was still held (never the commit date or SHA-keyed check-suite/status times), that lift already covers the current head and holdguard does not re-hold (#13603).

### Escalation

1. Red CI across the distinct-SHA budget applies PR `needs-human`; automated fix dispatch stops.
2. Reviewer lane may add `reviewer-passed` and remove/un-escalate `needs-human`, or add `reviewer-recommend-close` or `reviewer-handoff`.
3. A human can remove `needs-human` after addressing the root cause; the ledger resets after the grace period.

## Non-label equivalents

- **Human comments and assignees** can acknowledge hive-filed directions for #5117; assignees also affect ranking.
- **Hidden comment markers** carry state that no label does. `<!-- hive-claim -->` records an issue claim, `<!-- hive-finding: HASH -->` dedupes a published finding, `<!-- hive-pr-overlap -->` marks an overlap notice, `<!-- hive-question-answer -->` starts the question auto-close clock, and `<!-- hive-shared-ci-<n> -->` is stamped on the single comment a fix lane leaves on a PR it defers to shared incident `#<n>` (`DEFER_TO_INCIDENT` from `bin/hive-baseline-check.sh`), so "every PR incident `#<n>` broke" stays greppable after the fix lands ([#10441](https://github.com/hivecommons/hive/issues/10441)). One marker per PR per incident; it is never edited or removed while the incident is open. While incident `#<n>` is open, the governor records it as `deferred_incident` on the PR's `ci-failing.json` row and the kick builders keep that PR out of every CI-FAILING / FIX-BEFORE-NEW repair list, naming it only as deferred; once the incident closes the PR is listed for repair again ([#10528](https://github.com/hivecommons/hive/issues/10528)). When the incident closes (not as "not planned"), the shared-CI rerun sweep re-runs the failed jobs of each still-open PR carrying that marker — the newest failed or timed-out run per workflow on the PR head — using the hive's own credential, then stamps `<!-- hive-shared-ci-rerun-<n> -->` on the PR so it is re-run at most once per incident; disable with `shared_ci_rerun.enabled: false` ([#12253](https://github.com/hivecommons/hive/issues/12253)).
- **`<!-- hive-ci-external head=<sha> check=<encoded name> run=<check run id> -->`** is stamped by a fix lane on the one comment it leaves when `bin/hive-baseline-check.sh` returns `DEFER_EXTERNAL`: the repository syncs from an upstream project (its GitHub fork parent, or `--upstream`) and the same check is red on the upstream default branch, so no change here can fix it. The helper prints the exact string as `external_marker`. While every failing check run on the PR's current head carries a marker for that head and run, the governor records `deferred_external` on the PR's `ci-failing.json` row and the kick builders name the PR only as deferred under `DEFERRED AS FAILING UPSTREAM`; a push, a newly failing check or a re-run of the check (a new run id — e.g. it went green and failed again at the same head) makes the markers stale and the PR is listed for repair again ([#11609](https://github.com/hivecommons/hive/issues/11609)).
- **`<!-- hive-ci-transient head=<sha> check=<encoded name> run=<check run id> -->`** is stamped by an agent on its triage comment when it diagnoses a red check on a PR as transient or infrastructure-related (a flaky job, a runner outage, a race against a cache the default branch was still publishing) rather than caused by the diff, and cannot re-run the check itself. The fields and pinning match `hive-ci-external`; `bin/hive-baseline-check.sh --json` with the PR number prints the exact string as `transient_marker` whenever the PR's newest run of the check is red, on any verdict, and the agent copies it verbatim. While every failing check run on the PR's current head carries a marker for that head and run, the PR's `transient_ci_triaged` is set and the dependency-bot red-CI hand-off still adds `needs-human` after its grace period (nothing re-runs a single PR's check automatically), but its comment says Hive's triage found the failure was not caused by the bump and asks for a re-run, instead of telling the owner to fix, pin or close the bump. A push or a re-run of the check (a new run id) makes the marker stale ([#12977](https://github.com/hivecommons/hive/issues/12977)).
- **Paused repos** (`project.paused_repos`) stop whole-repo write/merge/enumeration paths without labels.
- **Contributor queue holds** such as active leases, cooldowns, dependencies, and quota guard holds suppress contributor offers without touching GitHub labels.
- **Jira/Linear labels** map only where the work-source adapter supports them: Linear skips held work using substring hold labels; Jira skips exact configured hold labels; GitHub Projects currently imports labels but does not use a hold gate.

## Repo-card legend vocabulary

The repository-card legend is a UI vocabulary, not a second scheduler. Unclaimed, claimed, needs-triage, needs-human, and confirm-and-close issue bands are computed client-side from labels/assignees, the snapshot's `human_acknowledged` flag (the cheap half of the #5117 acknowledgment: `approved-direction` or a human assignee, no comment scan), and `dashboard.issue_bands`; PR bands are computed client-side from held/needs-human labels, merge verdicts, CI, review links, draft state, and the same waiting/stale display config. Non-winning states stay as badges. State glyphs such as `⛔`, `❓`, `👤`, `✓`, role badges, stale `🕒`, PR `✓`/`◐`/`⚠`, held `⏸`, failing CI `✗ CI`, conflicts `⑂`, reviewed `💬`, auto-merge `🔀`, and review-class badges explain the same data the table above names. A PR carrying the configured sentinel alert label (`sentinel.label`, default `sentinel-alert`, case-insensitive) is drawn as a red `sentinel` pill that wins over the held, needs-human and merge colours; the legend explains that Hive will not merge it until a maintainer removes the label and that trusted-author PRs, which normally get only a sentinel comment, are usually not red. Changing a band label changes the repo card description only; it does not make an item held, exempt, mergeable, or actionable.

### Hold sources in the status snapshot

Every held issue and pull request row in the status snapshot (`hold.items[]` and `prs.held[]`) carries `hold_sources`: the set of sources currently requiring the hold, drawn from `level_gate`, `outreach`, `self_authorization`, `reporter_trust`, `sha_hold`, `holdguard`, `human_merge_path`, `standby_donation`, `dashboard_pause`, `person` and `unknown` (#11564, spec #11507). Detection is display-only and read-only towards GitHub. This hive's own pause label `hive-pause/<hive-id>` (another hive's pause label is not a source here) and the reporter-trust extra hold label are read from the row's labels; the sentinel label is not a hold source. Holds that leave no marker on GitHub — the holdguard re-hold, the donated-standby hold and the SHA hold — are recorded in Hive's local record `/data/hold-sources.json` when Hive applies them. For a held pull request Hive also reads its comments (the level-hold, reporter-trust, #5117 self-authorization and human-merge-path notices, accepted only from the Hive App) and hold-label events (`person` when a person made the latest one, `unknown` when nothing attributes the hold); that check is cached in the same record against the PR's `updated_at`, so a refresh in which no held PR changed makes no GitHub API calls for hold sources. A held issue is never checked against GitHub: a hold the local record does not attribute to a Hive source counts as `person`, except the sentinel label, which is not a hold source and leaves the issue to the sentinel rule. The your-turn ownership rules read these sources as recorded, with no fallback.

## Documented inconsistencies and follow-ups

Issue #8924 recorded the sharp edges of this page's first version; #8927 aligned the code: auto-merge sweeps, the task-list sweep, and the SHA-hold sweep now gate on the enumeration hold predicate (so `hive-pause/<hive-id>` holds everywhere), SHA-hold lifts only its own `hold`, policy templates name the enforced hold set, and `agent/<role>` is always the lane name. Two behaviours remain deliberate rather than fixed:

- Level-hold and #5117 disabled-policy auto-release are different. The self-authored auto-merge sweep may still release its own #5117 self-authorization holds, but ACMM level-applied holds are never auto-released by a sweep or level change. They are released by a human removing `hold`, or by a deliberate one-off `PUT /api/packs/level` with `release_level_holds: true`, which touches only Hive App level-hold notices whose latest `hold` event was by the App.
- `hive-open-pr` discards every `--label` value, including `hold`, because the PR-request watcher applies holds server-side (`bin/hive-open-pr.sh:142-150`, `src/docs/hive-open-pr.md`).
