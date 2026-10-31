# `hive-merge` — merge a PR as the App bot

`bin/hive-merge.sh` is how an agent merges a pull request. Agents call it
**instead of the GitHub MCP `merge_pull_request` tool** (or a GraphQL
`mergePullRequest` mutation). GitHub rejects that mutation for App
installation tokens with "Resource not accessible by integration" even when
the token holds `contents:write` + `pull_requests:write`. The hive merges the
PR over REST with the App token instead, which succeeds — so the merge is
authored by the App bot (`<slug>[bot]`) and uses the transport that actually
works for an App token.

It does not merge the PR itself. It writes a request file that the hive's
merge-request watcher picks up and acts on.

## Why it exists

Two reasons.

**Transport.** The MCP/GraphQL merge path is rejected outright for App
installation tokens. Routing through the hive's REST-based merge path is not
optional convenience — it's the only path that works.

**Authorization + forge resistance.** The script runs **as the agent**, in
that agent's tmux session under that agent's UID, so the request file it
writes is owned by that UID. The watcher re-derives the requesting agent from
the **file's owner**, not from anything written inside the file. `hive-merge`
**adds no privilege**: the watcher enforces the same per-agent forge-resistance
check and a `CanMerge` ACMM gate (`AuthorizeMerge`) that a direct merge would
need — an agent whose mode allows opening PRs but not merging them still
cannot merge through this path.

Auto-merge is effective only at L6. Below L6, stored per-repo values resolve
to off; at L6, `project.repo_policies[].auto_merge: false` is also enforced
here. A repo with that switch off may still receive Hive-opened PRs, but the
relay refuses to merge them and reports that auto-merge is disabled for the
repository. The same repo policy is enforced by the App self-merge sweep and by
the proxy's direct REST/GraphQL merge denial path, so agents cannot bypass it
by calling GitHub directly.

On top of `AuthorizeMerge`, the watcher's authorizer is wrapped with a second,
target-specific check (the F4 target-binding, CWE-863) before any merge is
attempted:

- the request must carry a non-empty `expect_sha` — an empty expected SHA
  would mean "merge whatever HEAD is now," which is a TOCTOU hole: a commit
  pushed after the PR was judged eligible could get merged unseen;
- the `(repo, number)` pair must currently appear in the governor's
  merge-eligible list **at that exact head SHA** — so an agent cannot request
  landing an arbitrary reachable PR (including its own) merely because its
  checks happen to be green; it must be a PR the hive's governor already
  deemed eligible, at the commit it reviewed.

Both the agent/UID/`CanMerge` check and this target-binding check must pass
before the merge is attempted.

**The hive does not force-merge.** GitHub still enforces whatever write
permissions, branch protection, rulesets, required reviews, allowed merge
methods, and required checks apply to the App token. A PR GitHub refuses fails
here, and the result file records why.

**The hive verifies CI itself, and absent is not passing.** Before any merge is
attempted the watcher requires *positive* CI confirmation on the pinned head SHA
(#6173). This gate applies equally to protected and unprotected base branches:
a red PR, or one whose workflows all died at startup with zero jobs (and
therefore zero check runs and an empty status rollup), does not merge merely
because GitHub branch protection is absent. The watcher computes its own
verdict:

- **green** - at least one commit status or check run exists on the SHA, every
  gating one has succeeded (`neutral`/`skipped` count as success, the
  non-required ignore list still applies), every config-declared required
  check has reported, and no workflow run on the SHA failed without producing
  a job. The merge proceeds.
- **pending** - a gating check is still queued/in progress, a required check
  has not been created yet, or a workflow run is in flight without a job. The
  request is parked (`ci_waits` in the result file, no attempt consumed) and
  re-evaluated with a per-PR+SHA backoff: every tick (10 s) for the first
  minute, then once a minute, then every five minutes. After
  `mergeRequestMaxCIWaits` polls or one hour pending, whichever comes first,
  the wait becomes a failed attempt.
- **never-created** - a required check has no status or check run on the
  head, nothing else on the head (statuses, check runs of any kind, workflow
  runs) is queued or in progress, and the head is older than 30 minutes
  (`mergeCIMissingRequiredGrace`). The check will never appear, typically
  because the PR was opened with a token that cannot trigger workflows. The
  request is quarantined (`.exhausted`) on the first such verdict, an operator
  alert is raised, and the fix loop is **not** re-engaged: no code push can
  create the check.
- **red** - a gating check failed, or a workflow run concluded failure with
  zero jobs. Refused; counts as a failed attempt and, at the retry limit, is
  classified and re-engaged into the fix loop like a branch-protection
  refusal.
- **unverified** - zero statuses, zero check runs, zero workflow runs. GitHub
  has no verdict, so neither does the hive. Refused; quarantined at the retry
  limit with the reason in the result file. Repos with genuinely no CI (docs-
  only, config-only) can be opted out per-repo with `auto_merge.no_ci_ok`
  (#6281): listing the repo (`owner/repo` or bare name) downgrades this — and
  ONLY this — verdict to green. Red and pending are never downgraded, and the
  default stays refuse.

An API error while gathering that evidence is a failed attempt, never a pass.

**Wait state survives restarts and re-requests.** The attempt count, `ci_waits`,
the pending-poll backoff and an "exhausted" record are kept per PR and head SHA
in `/data/merge-request-state.json` (the PVC), not only in the tmpfs result
files. A new `hive-merge` request for a PR whose previous request was exhausted
on the same head SHA is refused within 24 hours: the request is moved to
`.exhausted` and its result file repeats the recorded reason instead of starting
another poll cycle. Pushing a new commit (a new head SHA) starts fresh.

**Fork PR workflow approval remains manual.** When a cross-repo pull request's
workflow runs conclude `action_required`, the watcher refuses the merge with a
reason that says the fork PR workflow runs are awaiting maintainer approval.
The operator alert links to the repository Actions settings and tells the owner
to approve the runs manually or relax "Approval for running fork pull request
workflows". The hive does not call GitHub's workflow-run approval API.
Two refinements (#13818): a held run whose workflow already has a successful
run on the same head under another event (a `GITHUB_TOKEN`-opened PR whose
opener dispatches the same workflows by `workflow_dispatch`) is listed as
ignored and does not block — the verdict is on the head through the twin's
check runs; and when the PR is not from a fork the reason says so, because
GitHub also holds `pull_request` runs for same-repo PRs opened by a bot token
and the approval setting may already be at its minimum.

"Merge blocked" operator alerts retire themselves. Besides clearing on the next
successful App merge in that repo, the watcher re-checks live alerts every
five minutes (`mergeAlertRevalidateInterval` in
`src/pkg/github/merge_failure_alert.go`): an alert is dropped once the blocked
PR is no longer open, and a fork-approval alert is dropped as soon as the PR
head has no workflow runs still `action_required` — i.e. the operator approved
them or relaxed the repo/org setting. API errors keep the alert in place.

**Unprotected base branches are allowed.** The watcher no longer makes a
separate branch-protection lookup and no longer refuses solely because the base
branch is unprotected. It merges into any protected or unprotected branch the
GitHub App can write, provided the hive's positive CI-evidence gate passes and
GitHub accepts the merge. The legacy `auto_merge.allow_unprotected_base` key is
accepted as a deprecated no-op so existing configs keep loading; it is no
longer needed.

**No-CI repositories require an explicit repo opt-in.** Repositories with no CI
by design can be listed under `auto_merge.no_ci_ok`. That opt-in only downgrades
the **unverified** verdict (zero statuses, zero check runs, zero workflow runs)
to green for that listed repo, and the watcher logs that
`auto_merge.no_ci_ok` enabled it. Any non-zero failing CI evidence still
refuses, and pending evidence still waits.

**Paths a person must merge.** `auto_merge.human_merge_paths` is a per-repo map
of `owner/repo` to glob patterns (same syntax as `intent.guardrail_path_patterns`),
unset by default. When a merge request touches a listed path, `hive-merge`
refuses it, adds `hold`, and posts one `<!-- hive-human-merge-path -->` comment
naming the path(s); a person must merge it. The same key guards the automerge
sweep lanes whatever the intent tier. If the changed-file list for a configured
repo cannot be fetched completely, the merge is withheld (fail closed). See
[operator-reference.md](operator-reference.md).

**Contributor PRs are held to the contributor-lane policy.** A merge request
for a PR whose author is not the hive App, a configured trusted bot, or a
trusted-author role is evaluated against the same `auto_merge.contributor_lane`
gates the sweep applies — verified claim tier within `require_tiers`, the
contributor `soak`, `min_head_age`, reviewer verdict, control labels (`hold`,
sentinel alert, parked-on-decision), human-merge paths and head-SHA pinning —
before `hive-merge` merges it. A refusal is terminal for that request (`.denied`),
names the failing gate in the result the agent sees (for example
`contributor-lane:claim-tier-untrusted (author tier "untrusted", require_tiers
merger, trusted)` or `contributor-lane:soaking, 27m0s remaining`), and is audited
as `merge_request_refused`. An agent cannot use the relay to land a contributor
PR the lane would not merge; the lane still merges it on its own tick once the
gates clear. Hive-authored PRs keep the relay's existing checks.

**Actionable merge failures alert the operator.** Failures that require an
operator configuration change raise one dashboard system alert per repo+reason,
not one per PR or retry tick. The alert text names the fix: install/grant the
App Contents and Pull requests write permissions, adjust review/ruleset bypass
or approve the PR, make a missing required check report, or enable/use an
allowed merge method. Conflicts re-engage the fix loop instead of alerting, and
rate limits are treated as transient. A later successful merge in that repo
clears its merge-failure alerts automatically.

```yaml
auto_merge:
  no_ci_ok:
    - your-org/docs-only-repo
```

### Self-merge review gate

Immediately before an automerge lane merges a PR — the self-authored sweep for
the App's own and trusted-bot PRs, and the trusted-author tier for a human's
(after the head SHA is re-verified) — it reads the swarm review verdict for that
head from the same `review-verdicts.json` the merge-eligible lane uses and
classifies it as `no-review-bundle`, `awaiting-review-approval`,
`changes-requested:<perspectives>`, `requires-human` or `approved`
([#12592](https://github.com/hivecommons/hive/issues/12592)). The class
follows the normalized **aggregate** verdict — the same one the contributor
lane and `stage/085-needs-decision` route on — so a `requires_human` head
reads as `requires-human` to the merge gate even when one perspective asked
for changes; the perspective list only names who asked. The mode is
`governor.review_gate.mode: off | shadow | enforce` (default `shadow`), raised
to `enforce` by `review.require_approval` and to at least `shadow` by
`review.shadow`; all three are re-read on every evaluation, so a config reload
takes effect without a restart.

- **shadow** (default): the merge proceeds as before; the evidence bundle for
  the head (started on the spot when none exists) records
  `shadow.review_gate` with `lane: self-merge` (the App's own PRs),
  `trusted-bot` or `trusted-author`, `would_have_held` and the reason, so the Evidence card's Shadow gates panel can show what the gate
  would have held.
- **enforce**: anything but `approved` holds the merge with reason
  `review-gate:<outcome>`; the PR is retried on a later sweep. A trusted
  merger's GitHub **Approve** review on the exact head outranks every
  non-approved swarm outcome — `requires-human`, `awaiting-review-approval`,
  `no-review-bundle` and `changes-requested:<perspectives>` alike: the person
  has read the reviewer's objection and decided the head lands as-is. The
  outcome becomes `human-approved` and the merge proceeds.
- **off**: no evaluation.

**A decision is head-scoped — except across the hive's own base refresh.**
A recorded operator decision (dashboard Approve, `/hive decision …`, a
hand-applied return label) names the exact head the person saw; a new head
is a new tree nobody has decided on, so the record is superseded and the
reviewer's `decision_requested` question stands again. The one head change
that does not supersede it is the hive's own `require_current_base` refresh:
a merge commit authored by the App whose parents are the decided head and a
commit on the base branch, and whose three-dot diff against the base is
file-for-file (name, status, blob, line counts, patch) identical to the
decided head's. Up to five such refreshes may stack before any gate reads
the PR: the carry walks the first-parent chain back from the new head and
every hop must itself pass the same check, so two App merge-from-base
commits in a row still carry while a content commit anywhere in the chain
does not ([#15766](https://github.com/hivecommons/hive/issues/15766)).
Before any gate reads the new head — the reviewer-verdict
stage route, the automerge review gate, operator admission and the hold
guard — the decision is re-recorded on it with `carried_from_head` naming
the original, and one INFO line `decision carried over base refresh` is
logged; the reviewer route then reads `decision_answered=true` instead of
parking the PR at `stage/085-needs-decision` a second time
([#15751](https://github.com/hivecommons/hive/issues/15751), seen on #15649). A content push,
a merge by anyone but the App, a rebase, a refresh whose diff differs, and
GitHub review approvals (which are never moved between heads) all keep the
record where it was. The answer for a (PR, head) pair is memoised, so the
check costs one commit read and two compares per new head.

Each evaluation writes one audit line, action `review_evaluated`, detail
`lane=<self-merge|trusted-bot|trusted-author> would_hold=<bool> reason=<code> enforce=<bool> head=<sha>`,
plus `human_approver=<login> overrode=<swarm outcome>` when a human's
approval cleared the gate, so the audit trail records who overrode what.
The gate covers every sweep lane because the swarm reviews every PR
regardless of author ([#12974](https://github.com/hivecommons/hive/issues/12974));
until then it covered the `self-authored` lane only, and a trusted human's PR
(hivecommons/hive#12966) could merge through the trusted-author tier with no
swarm verdict at all. The contributor lane has its own
`auto_merge.contributor_lane.require_reviewer_verdict` (default on). The merge
and the CI snapshot are recorded in the evidence bundle in every mode,
including when the bundle is created at merge time.

### Trusted bot authors

The self-authored automerge sweep merges the App's own open, CI-green PRs. It
also merges PRs from `auto_merge.trusted_bot_authors` through the identical
gates (required checks green, mergeable, no hold/exempt label, intent tier,
approval desk, head SHA re-verified at merge time). The default is
`dependabot[bot]` only; set an explicit empty list to keep the sweep App-only,
or add other dependency bots you trust. Each merge is recorded with
`lane=trusted-bot` so audits can tell it from `lane=self-authored`. Changing
ACMM level at runtime restarts the relay generation when the L6 self-merge gate
changes, so the sweep starts or stops under the new level immediately. A
promotion into L6 never auto-releases existing level-applied `hold` labels; use
`release_level_holds: true` on `PUT /api/packs/level` for a deliberate one-off
release. Hub, config-driven, and ordinary dashboard promotions leave those holds
for human removal. On a dashboard promotion into L6, Hive shows an informational
modal with the held App-authored PRs and per-repo auto-merge switches.
Switching to L6 turns auto-merge on for all active repositories; the owner can
then switch off repositories that should not participate before manually
removing `hold`.

```yaml
auto_merge:
  trusted_bot_authors:
    - dependabot[bot]
    - renovate[bot]
```

The same list is editable from the dashboard: **Settings → Features → Auto
merge → Trusted bot authors** shows a toggle per bot. Known dependency bots
(`dependabot`, `renovate`, `mergeraptor`, `pre-commit-ci`, `github-actions`)
are listed first, bots currently authoring open PRs in your repos are
discovered from the last scan and listed automatically, and any other login
can be typed in. Saving writes `auto_merge.trusted_bot_authors` in full;
turning every bot off writes an explicit empty list (App-only sweep). The
underlying endpoint is `GET/PUT /api/config/auto-merge` (owner-only), whose
response carries `bot_authors: [{login, source: known|discovered|custom,
trusted}]`.

### Event-driven sweeps on the ForgeState cache

With `forgestate.enabled: true` the three sweep lanes (self-authored,
trusted-author, contributor lane) stop listing `/pulls` per repo on the 10 s
ticker. They read one shared open-PR set from the ForgeState cache
(`pkg/forgestate`, [#13373](https://github.com/hivecommons/hive/issues/13373))
and run when a `check_suite`, `check_run`, `status`, `pull_request_review` or
`pull_request` webhook arrives for a PR: that delivery patches the cache and
triggers a pass that evaluates only the affected PR(s). A periodic floor pass
over the whole cached set runs every 5 minutes as the fallback when no events
arrive (`automergeSweepFloorInterval`). Before any merge the lane re-reads the
PR through the cache with a 60 s freshness bound
(`automergeMergeFreshnessBound`): an item older than that is refreshed once
from GitHub; a repo whose snapshot is marked stale is skipped at enumeration
and its PRs are never merged (`forgestate-stale`); a head SHA that moved since
evaluation defers the PR to the next pass. The review gate, required checks,
hold/exempt labels, intent tier and the existing head-SHA re-verify at merge
time are unchanged ([#13377](https://github.com/hivecommons/hive/issues/13377)).
With `forgestate.enabled` off (the default) the lanes list `/pulls` on the
ticker as before.

### github-actions[bot] PR takeover

A PR that `github-actions[bot]` opens with a workflow `GITHUB_TOKEN` never
triggers `pull_request` workflows (GitHub's anti-recursion rule), so a required
check those workflows produce stays `required-check-missing` forever. When
`github-actions[bot]` is in `auto_merge.trusted_bot_authors`, the sweep takes
such a PR over instead of polling it: once the head is older than both
`auto_merge.min_head_age` and 30 minutes with no `pull_request` run at all, the
App pushes the same change (the bot head's tree on the merge base) as one
DCO-signed-off commit to `hive/takeover-<n>`, opens a PR with the original
title and body plus `Supersedes #<n>`, comments `Superseded by #N` on the bot
PR and closes it. The App PR triggers workflows normally and merges through the
self-authored lane. Fork heads are never taken over, and each bot head SHA gets
one attempt whatever the outcome. No GitHub App secrets are needed in the
workflow that opened the bot PR.

```yaml
auto_merge:
  trusted_bot_authors:
    - dependabot[bot]
    - github-actions[bot]
  bot_takeover:
    enabled: true   # default; set false to leave such PRs held
```

### Breaking-titled or `needs-owner-merge` PRs wait for a human

A PR whose trimmed title starts with `⚠️` (U+26A0, with or without the VS16
selector — the PR-title convention for a breaking change) or that carries a
`needs-owner-merge` label is never merged by hive. One shared predicate,
`automerge.RequiresHumanMerge(pr)`, is evaluated once per PR where each lane
already computes eligibility (the App self-merge, label-queued, trusted-author
and contributor prefilters, and the refetch just before a merge), so the merge
train — which draws its candidates from the same prefilters — never fronts or
carries such a PR either. The prefixes and labels are configurable via
`auto_merge.human_merge_title_prefixes` and `auto_merge.human_merge_labels`
(see the operator reference); an empty list turns that half of the rule off.

When the PR is otherwise merge-ready (green checks, approvals satisfied) the
stage tracker parks it at **090 Ready for human merge** with the chip
*human merge* and the reason *breaking change — owner merges*; the owner
merges it with **Merge now as maintainer** (the existing gated operator
path; **Approve and let hive merge** is greyed, since the lanes never
merge a breaking change) or on GitHub. Hive posts one comment, through the writeback
gate, saying it will wait for a human. The rule exists because a ⚠️-titled
PR was auto-merged by the trusted-author lane while its owner was still
deciding whether to ship it.

### Final base preparation: one refresh owner, no ready-work blockade

For repositories governed by `auto_merge.require_current_base` (including
the inherited default), automatic base refreshes share **one preparation
owner per canonical repository and destination branch**, across App,
trusted-author and contributor lanes. Repository names are case-insensitive;
branch names remain case-sensitive. Unrelated destinations progress independently.

A behind head enters final preparation only after its existing CI, review
and lane admission gates pass, **without first requiring an up-to-date base**.
The owner refreshes with an expected-head SHA and then waits for CI and review
on the new head. Pending runner jobs (including queued sign-off checks) are
pending, not failures. Preparation is outside ready merge-train membership:
`base-refresh:preparing` and `base-refresh:queued-behind#N` remain at CI / Review,
never ready-to-merge. Waiting heads may continue development and review.
The adopted stale-red recovery path can also admit its existing, proven
base-fixes-failure repair; that admission never authorizes a merge.
Explicit trusted-human merge-forward repair shares the same owner and
write-ahead identity. Its conflict-repair allowance lasts only through the
existing mutation-reconciliation TTL; a failed/conflicting result returns to
the normal repair path, never a ready seat.

**Ownership serializes refreshes, not ready merges.** A successor that is
already current-base, CI-green and reviewed passes every normal merge gate
without waiting for a preparing owner. A behind successor cannot skip testing.
Train sibling refreshes and client-side follow-up/relay/supersession updates
defer to preparation on these repositories; opted-out repositories retain
their previous behavior.

Ownership and first-admission FIFO tickets live in `/data/base-preparation/`,
with per-destination file locking and atomic durable snapshots. An update
identity (PR/head/base SHA) is recorded before the request, across lanes,
concurrent sweeps and restarts. An unchanged unresolved pair waits; after the
existing mutation-reconciliation TTL it can retry only with an authoritative,
still-identical head and the API's expected-head guard. Accepted updates must
complete their normal current-head gates; no approval is transferred.
Release occurs on merge, closed/missing/retargeted/draft/conflicting/held heads,
failed checks, rejected review, or lost admission. Expected pending CI/review
keeps ownership, bounded by the existing review-pending dwell (its default when
stall detection is disabled), so a genuinely hung owner yields to successors.
Waiting tickets are removed when their head changes or stops qualifying.
Admission provenance prevents a different lane's non-admission from revoking
the owning lane's reservation or FIFO ticket. An old waiting ticket reserves
order only: its head must qualify afresh in its lane before becoming owner.

Audit logs use `automerge base preparation selected`, `refresh claimed`,
`waiting` and `released`, with repository, base, PR, head/base SHA where relevant,
and release/skip reason. `automerge update-branch` and the sweep's
`updated_branches` count actual successful refresh effects; queued/preparing
reasons do not consume the branch-update budget. Ready-only train seating and
reviewer dispatch scheduling are separate policies.

The review planner observes the durable preparation owner once per destination
per pass. Only its exact live, CI-green, mergeable head with a verdict still
owed receives `base-preparation` review priority; it does not need a ready train
seat. A refreshed head must pass the existing App-authored base-refresh proof:
the attempted head is preserved, the merged base lies on the destination branch,
and the PR's content is unchanged. The actual merged base can be newer than the
attempt's recorded base SHA; this remains valid after a restart without rewriting
ownership. An author's content push or amended merge cannot inherit priority.
Closed, held, retargeted, released and expired owners receive no promotion.
Observation is read-only and performs GitHub reads outside preparation locks.
Busy valid reviews are never interrupted. After a confirmed preparation review
handoff, other non-pending heads promoted by the existing three-cycle starvation
rule go first as `base-preparation-fairness`; their confirmed service restores
preparation preference. Unrelated deliveries cannot clear the owed yield. This
uses the existing dispatch history, survives restarts, and prevents successive
owners from indefinitely starving aged work. Without an active eligible owner,
the existing review order is unchanged. Priority is not review approval or merge
eligibility: normal label, model, current-head verdict and merge gates still run.
`review swarm kick planned` exposes `priority=base-preparation`; the normal
waiting reasons continue to expose reviewer/parallel-slot exhaustion.
`automerge base preparation observation skipped` with
`reason=unverified-app-base-refresh` records a failed refreshed-head proof.

Human pushes, direct merges and external automation can still move a base.
Such a move is coalesced until the preparation owner qualifies again; it must
then refresh and re-pass current-head gates if still behind. This scheduler
cannot stop external writers and does not relax strict current-base checks.
Deploying the changed hive binary activates the policy; no agent, memory,
review, CI, hold or authorization setting needs changing.

### Merge train: ready PRs lead

Every lane groups its candidates on one base whose changed files overlap
into a **merge train** (`auto_merge.merge_train.*`, [#15117](https://github.com/hivecommons/hive/issues/15117))
and merges one front at a time; the siblings park at stage/080 with lane
reason `merge-train:queued-behind#<front>:<pos>/<size>:…` and, the moment
the front merges, are update-branched (`behind`) or handed to the rebase
resolver (`dirty`) so the next front is evaluated on a current base. Hive's
own repo also runs the current-base gate (`require_current_base`), which
holds a behind head until the update lands. On current-base-gated repositories
the separate preparation owner above refreshes the next admitted head, rather
than refreshing every waiting sibling when a front merges.

The train's order is **readiness first**. Each pass the lane sorts the
members by what it last saw of their heads: a head at the merge gate —
green, approved, soaking or waiting on a sweep — leads; a head the lane has
not evaluated since it was pushed comes next (so it is evaluated and
sorted); a head still at CI / Review trails; a blocked head (red required
check, terminal hold) trails everything and never fronts. Within a tier the
old tiebreak holds (clean-mergeable first, then oldest, then number). The
order is recomputed every pass, so a member that turns green moves up and
one that turns red moves back. The front of a train is therefore always a
ready member when the train has one; a train whose every member is still
short of the gate has a *provisional* front, and the Activity Monitor says
so rather than reading "1st in the merge queue".

This replaced the age-first order after hive's own #15741, still at CI /
Review, led #15748 and #15755 — both green and queued at 080 — and nothing
merged until its checks finished. In the Activity Monitor only members at
080 and beyond ride a train chip in the *Merge queue* column; a member still
at CI / Review keeps its chevron in its own column with a muted
"train · joins behind #N when green" note.

**Membership is ready-only** ([#15825](https://github.com/hivecommons/hive/issues/15825)).
The rule: *items that have not passed the review or CI is not green should
not be (a) ready for merge, (b) in a merge train.* A PR takes a seat only
while its **current head** is green, has every required perspective
verdict recorded, is mergeable, not conflicting and not held. A PR that
shares files with a train but is short of that is a *candidate*: the
tracker keeps the overlap note, but it holds no position and is never the
front. A member that regresses — new push (head moved), CI pending or red,
a verdict owed again, a conflict with the base — is **ejected** on the
next evaluation (one INFO line `merge train: member ejected pr=N
why=head-moved|ci-pending|ci-red|verdict-owed|conflicting`), the next
ready member fronts and merges, and the ejected PR re-enters at the back
once it is ready again. Its tracker row drops back to the CI / Review
column (never the *Merge queue* column) with a caption that says what is
owed ("left the merge train — new push, CI re-running after push") and a
gray lineage pill ("left merge train · pushed 3m ago"). Among ready
members the order is the readiness tiers above, then age. The one
exception is a member the train itself made dirty by merging the front
ahead of it: it keeps its seat (trailing every clean member) while the
resolver rebases it, so the rebase keeps its place in line. Its row still
shows in the CI / Review column while the rebase runs ("Rebasing · attempt
1/2"): the rebased head has no CI or verdicts yet. The tracker's train
header counts seated members only — a candidate or an ejected row never
adds to "merge train · N PRs" — and a seated row always reads its seat
("Merge train · 2nd of 2 · behind #15771"); "checking now" is said only
of the PR the sweep is evaluating at that moment.

### Trusted-author auto-merge

`auto_merge.trusted_authors` is a third, default-off sweep tier for
human-authored PRs. It exists for hives whose maintainers want Hive to land a
maintainer's own green PR without waiting for Prow/tide's distinct
`lgtm`+`approved` path. It is not a privilege escalation: the PR author must be
in the hive's `dashboard.authorized_users` allowlist at the configured role
(`merger` by default, or `owner`), and by default GitHub must also report that
the author has write/maintain/admin permission on that repository. If that
GitHub permission lookup errors, the PR does not merge.

The tier uses the same merge mechanics as the self-authored sweep: required CI
must be green, the PR must be open, non-draft, mergeable, SHA-pinned at the
evaluated head, and the merge method follows `hive-merge` semantics (forward
merges use merge commits; other PRs squash). It refuses excluded labels
(`hold`, `do-not-merge`, and `needs-human` by default), existing hold/exempt
labels, fork PRs whose author is not a repo member, and any PR with an
outstanding `CHANGES_REQUESTED` review. Each merge records the tier as
`trusted-author` and leaves an audit comment naming the author role, GitHub
permission, and green head SHA.

```yaml
auto_merge:
  trusted_authors:
    enabled: false
    repos: []                    # optional allow-list; empty = all managed repos
    require_role: merger          # merger or owner
    require_github_permission: true
    exclude_labels:
      - hold
      - do-not-merge
      - needs-human
```

Operators can set `require_github_permission: false` only when the
`authorized_users` role list is the desired authority, but fork PRs still need a
successful GitHub permission check so non-member forks do not merge through
this tier.

The same block is editable from the dashboard: **Settings → Features → Auto
Merge → Trusted-author auto-merge** writes `auto_merge.trusted_authors` through
owner-only `GET/PUT /api/config/auto-merge`. The form exposes the enabled
switch, repository allow-list (select none for all watched repos), required
role (`merger` or `owner`), GitHub permission requirement, and excluded labels
so hosted spokes can opt in without editing `hive.yaml`.

### Contributor-lane auto-merge

`auto_merge.contributor_lane` is a fourth sweep tier, **default on**, for PRs
that this hive's own contributor lane produced. When Hive claims an issue for
a contributor it posts an App-authored `<!-- hive:claim who=<login>
kind=contributor hive=<hive-id> tier=<trust-tier> verified=<true|false> ... -->`
marker on the issue; the contributor's
PR then closes that issue with `Fixes #N`. Before this tier existed such a PR
was skipped by every lane (`untrusted-author-role` from the trusted-author tier,
`not-app-authored` from the App tier) and waited indefinitely for a human
`/lgtm` + `/approve`, even though the hive had already vetted and reviewed the
work. The maintainer position is that work the hive trusted enough to hand out
should land unattended once it is green, so the tier ships enabled; set
`enabled: false` to restore the wait-for-human behaviour.

The claim marker on its own proves only that the relay leased the issue to a
connection: relay registration (`POST /api/contribute/register`) is
self-asserted and every new profile starts as `newcomer`. So the tier merges
only for contributors the operator vetted: the marker's `tier=` must be in
`require_tiers` (default `trusted`, `merger`; only operator-granted tiers are
accepted, never the auto-promoted `newcomer`/`contributor`), and `verified=true`
(the contributor proved the GitHub login with `POST
/api/contribute/reissue-token`). Markers written before these fields existed
read as `newcomer`/unverified and are refused.

The marker is not the only statement of trust the hive holds. The operator
sets each contributor's trust tier on their profile in the hive's contributor
registry (**Contribute → Admin**, `POST /api/contribute/admin/contributors/
{id}/trust`), and that tier is what the claim marker copies when the relay
leases an issue. So the tier also admits a PR directly from the registry
(`registry_tiers_sufficient`, default `true`): if the PR author's profile
carries a tier in `require_tiers`, no linked issue or marker is needed. Live
on hive's own hive a contributor the operator had marked `trusted` was still
skipped with `contributor-lane:no-linked-issue` because the PR body said
`Part of #N` rather than `Fixes #N`, and with `contributor-lane:no-claim` when
the issue had been claimed outside the relay; the registry path admits both.
Every other gate below still applies to a registry-admitted PR. Set
`registry_tiers_sufficient: false` to require the marker for everyone.

A registry-admitted author also gets their CI started. GitHub's fork-approval
policy holds a first-time contributor's `pull_request` runs in
`action_required`, and the hive otherwise never approves a human-authored
lane's held runs (the `hive-ok-to-test` workflow approves same-repo heads
only). hivecommons/docs#481 by a `trusted` registry contributor sat 17 hours
at `required-check-missing:Audit production dependencies` until an operator
clicked "Approve and run". When the registry path admitted the author and the
lane stops at `required-check-missing:<ctx>`, it now approves the head's held
runs once (`POST /repos/{owner}/{repo}/actions/runs/{id}/approve`, fork head
included), posts one `<!-- hive:contributor-held-runs-approved … -->` audit
comment naming the author and tier, and logs `contributor-lane approved held
runs` with the run ids (`approve_held_runs_for_registry_trusted`, default
`true`). Claim-marker, auto-promotion and association admissions are never
approved for, nor is a PR carrying the sentinel or an `exclude_labels` label,
nor one touching `.github/workflows/**`. The App needs **Actions: Read and
write** for the call; without it the approve fails, is logged once per head,
and the PR waits for a person as before.

A PR merges through this tier only when all of the following hold:

- The PR author is admitted by one of three paths:
  - **Adopted by the hive**: the stage store records an in-place adoption of
    the PR (`stage/074-adopted-by-hive`) and the head being evaluated is
    exactly the head the hive adopted or pushed (`AdoptedHead`). The hive
    owns that head, so the original author's tier, marker and GitHub
    association are not consulted — hive owner decision (hivecommons/hive,
    2026-10-27): "If items are adopted, then the hive agents own it and they
    should be eligible for auto merge." The sweep logs `contributor-lane
    admitted reason=contributor-lane:adopted-by-hive adopted_head=<sha>` and
    the merge log carries `admitted_by=contributor-lane:adopted-by-hive`. A
    contributor push over the adopted head yields the adoption (074 → 072,
    `AdoptedHead` cleared), so the next pass falls through to the two paths
    below; a superseding adoption (`hive/adopt-<n>`) is never admitted on the
    original PR — its App PR merges through the self-authored lane and closes
    the original. Off with `governor.labels.stage.adoption.auto_merge: false`
    (default on). Every other gate below still applies.
  - **Registry**: the author's contributor-registry profile has a trust tier in
    `require_tiers`. The sweep logs `contributor-lane admitted
    reason=contributor-lane:registry-trusted registry_tier=<tier>` and the
    merge log carries `admitted_by=contributor-lane:registry-trusted`. A
    profile at `newcomer`/`contributor`/`revoked`, no profile at all, or
    `registry_tiers_sufficient: false` falls through to the marker path with
    that path's skip reasons unchanged.
  - **Claim marker**: the PR body closes a same-repo issue
    (`Fixes`/`Closes`/`Resolves #N`) whose App-authored `hive:claim` marker has
    `kind=contributor`, `who=` equal to the PR author, and `hive=` equal to this
    hive's `hive_id`. Markers from other commenters, other kinds, other authors
    or other hives are rejected with distinct skip reasons
    (`contributor-lane:no-claim`, `contributor-lane:claim-author-mismatch`,
    `contributor-lane:claim-other-hive`); a body with no closing keyword skips
    with `contributor-lane:no-linked-issue`. The marker's `tier=` must be in
    `require_tiers` (`contributor-lane:claim-tier-untrusted`) and it must say
    `verified=true` (`contributor-lane:claim-unverified`).
  - **Auto-promotion**: when the marker path fails on tier, the conservative
    clean-history ratchet (`auto_merge.contributor_tier.auto_promote_after_merged`,
    default 5; negative disables) admits the author if their last N merged
    PRs have no revert or sentinel-alert evidence — none is itself a revert,
    none carries the sentinel label, none was undone by a later merged
    `Revert "<title>"` / "This reverts commit <sha>" PR — and the author is
    not `revoked` in the registry. By default the merges are counted across
    **every repository the hive manages**
    (`auto_merge.contributor_lane.auto_promotion_scope: hive`): trust is a
    property of the person, and the small satellite repos of a multi-repo
    hive never reach the threshold on their own (hivecommons/docs#481 sat 17h
    at `contributor-lane:claim-tier-untrusted` while the same author had nine
    clean merges in hivecommons/hive; #14517). The PR's own repository is
    walked first; only when it is short does one `search/issues` query
    (`is:pr is:merged author:<login> repo:… repo:…`) find which other hive
    repos hold the author's merges, and only those are walked for revert
    evidence — a revert or sentinel hit in any repository blocks. Promotion
    is a property of the author in the hive, not of a PR, so the sweep
    announces it **once per (hive, author)**: the first PR that earns it
    gets a `<!-- hive:trust-promoted login=… merged=N scope=hive
    repos=hive:4,docs:1 -->` comment ("@login is now trusted for
    contributor-lane merging in this hive … Applies to their open and
    future PRs here"), and the author's other open PRs, later PRs and PRs
    after a restart post nothing — the "announced" fact lives in the stage
    store (`trust-promoted:<login>`), with the PR's own marker as a
    fallback when the store was rebuilt. Revert or sentinel evidence that
    withdraws the promotion forgets the announcement, so regaining it is
    announced once more (#15443). Every admission still logs
    `contributor-lane admitted reason=contributor-lane:auto-promoted
    merged_prs=5 scope=hive repos=hive:4,docs:1`. `auto_promotion_scope:
    repo` restores the per-repository count.
- When neither admission path above vouches for the author, GitHub's
  `author_association` for the PR author must be `OWNER`, `MEMBER`,
  `COLLABORATOR` or `CONTRIBUTOR`; `NONE`, `FIRST_TIMER`,
  `FIRST_TIME_CONTRIBUTOR` or a missing value then skip with
  `contributor-lane:author-association`, outranking the marker path's own
  reason. A verified claim marker or a registry tier is this hive's own
  statement of trust and admits the PR regardless of the association —
  GitHub reports `FIRST_TIME_CONTRIBUTOR` for every relay contributor's
  first PR to a repo, which left admitted, green, reviewer-approved PRs
  skipping every cycle (#12344; #12195, #12290). An unmarked, unregistered
  first-timer is still never admitted.
- The diff does not touch `.github/workflows/**` or `.github/actions/**`
  (`contributor-lane:workflow-path`) — **or** a hive owner / trusted merger
  has explicitly approved the current head
  (`owner_approval_releases_workflow_fence`, default `true`,
  [#14664](https://github.com/hivecommons/hive/issues/14664)). Explicit
  approval is a GitHub pull-request review in state `APPROVED` whose commit
  is the current head by a login the hive's merger authorizer trusts (the
  same bar that releases a `requires_human` verdict), or a dashboard
  approval (`/hive decision approve`, which posts `/lgtm` + `/approve`
  attributed to the maintainer and bound to the head) or a trusted merger's
  own `/hive decision approve` comment posted after the hive first observed
  the head. An unmarked approval is bound to the PR `updated_at` pinned on
  the hive's first sweep of the head, not to the push: an approval typed
  between the push and that sweep is judged stale and must be typed again
  (fail-closed by design,
  [#14754](https://github.com/hivecommons/hive/issues/14754)). Approval on an
  earlier head never counts: a new push re-arms the fence and the 085 note
  reads "awaiting owner approval … re-approve after a push". When released
  the lane logs `workflow-path fence released by owner approval login=…
  head=…`, leaves one `<!-- hive:workflow-approval head=… by=… -->` audit
  comment per head, and the PR goes through every other gate below like any
  other. Merging such a PR needs the App's **Workflows: Read and write**;
  when GitHub refuses the merge for want of it the lane marks the PR
  `hive:unauthorized` (step `merge-workflow-path`), stops retrying that
  head, and the approved head parks at `090-ready-for-human-merge` for a
  person's **Merge** click as before. With the toggle off, or with no
  approval, a workflow-path PR is never merged by the hive.
- The diff does not delete a test file or remove 25+ test lines while
  deleting at least 3× what it adds (`contributor-lane:tests-removed`).
- Required CI is green on the current head, using the same merge-CI gate as
  the App self-merge tier.
- A reviewer agent is configured (otherwise `contributor-lane:no-reviewer`:
  the tier never merges unreviewed), and `review-verdicts.json` records an
  approving aggregate verdict for the current head
  (`require_reviewer_verdict`, default `true`). Missing or failing verdicts
  skip with `contributor-lane:no-verdict`.
- Sentinel has no blocking finding (`contributor-lane:sentinel-blocked`), the
  reporter-trust gate has not held the PR (`contributor-lane:reporter-trust-hold`),
  and no reviewer has an outstanding `CHANGES_REQUESTED`.
- No excluded label (`hold`, `do-not-merge*`, `needs-human`, `blocked`,
  `needs-rebase` by default), not draft, mergeable, no human-merge path.
- The head SHA has stayed green for `soak_minutes` (`0`/unset = default 30;
  the shortest real soak is `1`). The soak is measured from when the head
  went green — the latest `completed_at` over its check runs, read once per
  (repo, PR, head) and then pinned in process — so a pod restart or upgrade
  does not restart it; a new push restarts
  the soak. The skip reason is the stable `contributor-lane:soaking`; the
  soak head, start and window are recorded once on the stage record, so the
  stage tracker computes "N left" from that deadline at request time instead
  of the hive re-stamping its note every pass. The window is also editable
  from the Activity Monitor's ⚙ → Toasts & timing ("Pre-merge soak"), which
  writes `soak_minutes` through `PUT /api/config/auto-merge`; the lane reads
  the knob on every pass, so a running soak follows the new window.

Merges squash through the same effect-claimed `MergePR` path as the other tiers,
log `automerge merged tier=contributor-lane admitted_by=<path>`, record the
sweep audit, and leave one comment naming the admission path: `Merged by hive
contributor-lane auto-merge: CI green for ≥120m, reviewer verdict approve,
claim by @author on #N verified.` for the marker path, or `… @author holds the
trusted tier in this hive's contributor registry.` for the registry path.

```yaml
auto_merge:
  contributor_lane:
    enabled: true                 # default on; false restores the human /lgtm wait
    soak_minutes: 30
    require_reviewer_verdict: true
    repos: []                     # optional allow-list; empty = all watched repos
    exclude_labels:
      - hold
      - do-not-merge
      - needs-human
      - blocked
      - needs-rebase
    require_tiers:                # operator-granted tiers only (trusted, merger, advisor)
      - trusted
      - merger
    registry_tiers_sufficient: true   # a require_tiers tier on the author's registry profile admits without a claim marker
    auto_promotion_scope: hive        # count clean merged PRs across every hive repo (repo = this repository only)
    approve_held_runs_for_registry_trusted: true   # approve action_required runs once per head for registry-admitted authors
    owner_approval_releases_workflow_fence: true   # a trusted merger's approval on the current head lets the lane merge .github/workflows/** PRs
  contributor_tier:
    auto_promote_after_merged: 5      # clean merged PRs before the lane trusts an author on its own; negative disables
```

The block is editable from **Settings → Features → Auto Merge →
Contributor-lane auto-merge** (enable switch, soak minutes, reviewer-verdict
switch, required tiers, auto-promotion scope, excluded labels, repository
allow-list) through the
same owner-only `GET/PUT /api/config/auto-merge` overlay.

**Operator workflow.** To let a known contributor's relay PRs merge
unattended (for example `mrbobbytables`):

1. The contributor proves their GitHub login once:
   `curl -X POST -H "Authorization: Bearer <their GitHub token>"
   <hub>/api/contribute/reissue-token`.
2. Grant them `trusted` (or `merger`) in **Contributors**.
3. Issues the relay leases to them from then on carry `tier=trusted
   verified=true`, and their green, reviewed, soaked PRs merge through this
   tier. Every other contributor's PRs keep waiting for a human `/lgtm`.

A claim keeps the tier the contributor held when it was taken, so grant the
tier before the issue is leased.

**No decision during CI/Review; the operator has the last word on
untrusted merges.** A contributor PR is not parked at
`stage/085-needs-decision` on a reviewer decision marker alone. While it is
in CI/Review the hive runs CI and the review swarm to completion and the
row always names its mover; the only early exit is a concrete security /
trust finding (`review.IsSecuritySensitiveFinding`: high-risk marker,
security perspective hold, security-class question, finding on a sensitive
path — `sentinel.sensitive_paths` plus `security.sensitive_paths` — or
insecure behaviour flagged), recorded as `decision_finding` and printed as
`the review found: …`. Anything else goes back to the clanker or is
advisory and the PR keeps moving. At the merge gate a contributor the lane
cannot vouch for parks at `stage/090-ready-for-human-merge`
(`contributor-lane:claim-tier-untrusted`, `untrusted-author-role`, the
human-merge rule) and waits for the operator's merge — the hive never
auto-merges an untrusted author's PR, however green, approved and soaked.
See [review-swarm.md](review-swarm.md) ("Contributor and human-authored
PRs: no decision in CI/Review").

### Issue-author credit at merge time

Every merge the hive performs — the self-authored, trusted-author,
contributor-lane and queue sweeps as well as `hive-merge` relay requests —
credits the humans who opened the issues the PR closes. The merge reads the
closing references the PR already carries (`Fixes|Closes|Resolves #N` in the
body) and resolves each referenced issue's author with at most one
`GET /issues/{n}` per newly seen issue, cached for the life of the process
(attributed as `hive:merge_credit` in the API-quota ledger). Bots and Apps
(`type=Bot`, `[bot]`/`-bot` suffixes, `github-actions*`), the PR author
themself, and issues that are really PRs are skipped; the remaining logins are
de-duplicated and appended as `Co-authored-by: <login>
<id+login@users.noreply.github.com>` trailers to the squash commit message. A
`merge-method/merge` PR gets the trailer on the merge commit instead; a rebase
merge has no commit to carry it and is left alone.

`Co-authored-by` is the trailer GitHub counts toward the contributor graph, so
a reporter whose issue was fixed by someone else's clanker shows up on the
repository's contributors page rather than only in the issue timeline. It is
purely additive: the branch's own commit subjects, bodies and `Signed-off-by`
lines are reproduced unchanged ahead of the new trailers, so the squash
sign-off gate and `check-squash-signoff-attribution.sh` see exactly what they
saw before plus the credit. This is the single mechanism: a hive-opened PR
already carries the trailer on its commits (`issue-coauthor.sh`,
`hive-open-pr`), and the merge recognises an existing trailer for the same
login (by noreply address or login) and does not add a duplicate. The credited
logins are recorded on the merge audit entry (`issue_authors=`) and in the PR's
review-evidence `merge.issue_authors`, so dashboards and DIBS statistics can
report them. If the branch's commits cannot be read at merge time, the hive
falls back to GitHub's default message rather than risk dropping a sign-off.
Switch it off with `auto_merge.credit_issue_authors: false` (#14581).

### Issues close only after the merge

The hive never closes an issue for a pull request that has not merged. The
only PR-driven close path is close-on-merge (`CloseOnMergeLedgerSweep`,
`close_on_merge.go`), which reads the PR back from the forge and returns
`pr_not_merged` without touching the issue unless `merged_at` is set (or
`merged` is true); opening a PR, admitting it to the merge train or the
pre-merge soak, and a `Fixes #N` in its body change nothing on the issue. On
GitHub's default branch GitHub itself closes the issue in the same instant
the PR merges, attributed to whoever pressed merge; on a release line the
hive's sweep does it on the next pass, with a "Fixed by #N (merged …)"
comment. Every such close goes through the central `CloseIssue` (which
refuses tracking issues and honours the reporter-confirmation gate) and is
counted by the direct-write ratchet, so no other code path can grow a close
of its own.

The tracker shows the consequence honestly. A follow-up or duplicate PR whose
body names an issue an *earlier* PR already delivered — `Refs #15422` on
#15425 after #15426 merged, `extending #15058` on #15431 after #15068 merged —
sits under the struck-through issue with the chip **closed by #N**, linking
to the merged PR GitHub records as the closer
(`closedByPullRequestsReferences`, with the hive's merged ledger as the
fallback). Only a merged PR is ever named: an open PR with `Fixes #N` is
listed under the same relation and has closed nothing. When no merged closer
can be found the chip reads **closed · this PR unmerged**, never a bare
`closed` that could be read as the issue having been closed before its fix
merged.

### Stage labels

Every sweep cycle also writes the hive-owned `stage/NNN-*` label for each PR
it evaluated, derived from the same per-lane reason it logs
(`contributor-lane:no-verdict` → `stage/050-review-pending`, unless the
swarm's known verdict is `requires_human`, which the lane also reports as
no-verdict → `stage/085-needs-decision` with reason
`reviewer-verdict:requires_human`,
`contributor-lane:soaking(...)` → `stage/095-pre-merge-soak` (admitted, soak window
running; promoted to `stage/100-merge-queue` once the soak is complete), approve verdict +
green CI but no lane decision yet → `stage/080-awaiting-merge-admission`
(a stall there is logged on the PR with the lane's last skip reason), red CI on a
human-authored PR → `stage/071-needs-author`, on a clanker PR →
`stage/072-returned-to-contributor`, a trusted human's changes-requested
review or `/hive decision <text>` on a hive-authored PR's current head →
`stage/070-fixing` with reason `human-changes-requested` and one fix kick
carrying the text ([#13041](https://github.com/hivecommons/hive/issues/13041)),
hold / sentinel alert → `stage/085-needs-decision`, hive-authored PR whose fix
dispatch is refused with `fix-attempts-exhausted` → `stage/085-needs-decision`
with reason `fixer-exhausted:<N>` and a once-per-head decision comment (re-author /
hand to a person / close as not planned; a stale `decision-answered` cannot pull it
back to 070 while the head is unchanged, [#15744](https://github.com/hivecommons/hive/issues/15744)), untrusted author with an approve verdict at the
current head, green checks, DCO and soak done →
`stage/090-ready-for-human-merge` (only a maintainer's `/lgtm` `/approve` is
missing; the dashboard derives a compact **human-merge reason code** from the
090 record's lane reason — `prstage.HumanMergeReason` — so the gate cell says
why a person merges: `workflow-files` (`contributor-lane:workflow-path`),
`tests-removed`, `manual-merge-policy` (`human-merge-path`), `branch-protection`,
`merge-method`, `auto-merge-off` (`repo-auto-merge-disabled`), `untrusted-author`
(every trust reason: `untrusted-author-role`, `claim-tier-untrusted`, `no-claim`,
`no-linked-issue`, `claim-unverified`, `claim-author-mismatch`, `claim-other-hive`,
`author-association`), `unauthorized` (the App's 403, #14595), `reviewer-deferred`
(`reviewer-verdict:requires_human` a trusted merger approved), and `policy` for
any other lane reason, quoted in the tooltip; #14658), merged → no stage), plus one `role/*` label saying
which admission tier the lane actually used, one `author/*` label saying who
wrote the PR (`human`, `clanker` = a contributor's own agent, `hive` = this repo's
own; #12581) and `stage/stalled` when nobody
but the hive has touched the PR for the stage's dwell (never while checks run
or the reviewer verdict is pending). Backward moves are a one-line stage
comment and dashboard history, not a label. Writes happen only when
the planned set differs from the labels already on the PR, under the
`hive:stage_labels` REST caller, and the whole family is switched by
`governor.labels.stage.enabled`. The mapping table, numbering rules, dwell
thresholds, the mermaid state machine and the off-happy-path flow
(072 → nudge → 074 → takeover) are in
[labels-and-control-signals.md](labels-and-control-signals/stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine);
the code is `src/pkg/stagelabels` and
`src/pkg/github/automerge/automerge_stage_labels.go`. Adoption
(`stage/074-adopted-by-hive`, `automerge_contributor_adopt.go`) reuses this
sweep's own red-PR route: the superseding App PR it opens from `hive/adopt-<n>`
is red with the contributor's failures, lands in `ci-failing.json` like any
hive-authored red PR, and the scanner's FIX-BEFORE-NEW kick rebases and fixes
it on that branch (never a replacement PR), after which it follows the normal
`050 → 100` path. When adoption is in-place on the contributor PR's own
branch, the hive owns the head it pushed: once that head is green, approved
by the reviewer, DCO-clean and soaked, the contributor lane admits it under
the hive's own authority (`contributor-lane:adopted-by-hive`, see the
admission paths above) rather than waiting for the original author's trust
tier or a maintainer `/lgtm`; a changed head stays adopted only with hive-controlled evidence
(GitHub-linked App commit metadata; the stage store only remembers SHAs that
already passed that check);
forgeable git author/committer names and emails are treated as contributor
pushes and release adoption once repo evidence exists
(`governor.labels.stage.adoption.strict_attribution`, default on). Push-capable
App-authored agents commit with the App bot noreply email (keyed by the bot's
GitHub user id, not the App id) so their fixer pushes
normally provide the GitHub linkage, while their commit name/sign-off still
shows the lane. An in-place adoption does not sit at 074 forever: the sweep
re-evaluates every adopted record each pass, and when the hive cannot finish
on the contributor branch because a blocking review-swarm finding was left
on a head that sat idle for the 074 dwell, it converts to a supersession
(`reason=in-place-stalled`) and the superseding PR takes the same red-PR /
review-fix route. A hive commit the squash sign-off gate strips
(`hive-signoff-attribution`) now re-homes immediately if the lane cannot
elect a merge commit: an App-authored superseder changes the squash
attribution while preserving the contributor's commits and sign-offs.
All DCO and merge gates still run on that PR. Loss of push access also
re-homes immediately (`reason=push-rehome`). A pushable `needs-rebase` or
conflicting branch first gets the normal fixer window; if its head remains
unchanged past the 074 dwell, the same sweep supersedes it
(`reason=rebase-rehome`) rather than parking at 085. Previously escalated
adoptions are re-evaluated for these recoveries too (#13975). A red adopted
head whose failing check is green on the base branch tip — the shared-CI
flake or red base it fell to was fixed since, but the head never ran CI
again — is recovered before anyone is asked: the sweep merges the base into
the head when it is behind (GitHub update-branch, which works on a fork the
maintainers may edit) or re-runs the failed workflow jobs when it is current,
at most once per (head, base tip) and three times per adoption, logged as
`adopted PR recovery` with `reason=update-branch`, `rerun-failed-jobs` or
`skipped:<why>`; an escalation already in place for that head is released
(hivecommons/hive#14500). An adoption with nothing the hive can
push past the dwell — after that recovery was tried and the head is still
red — goes to `stage/085-needs-decision` as
`adoption-stalled:<lane reason>` with one comment naming the blocker, so the
dashboard shows a person's item, not a silent 074; "waiting on maintainer
lgtm" is only ever the state at 080+ (hivecommons/hive#12858). The dwell runs
from the head's last hive push, a red optional check alone is not a blocker,
and the escalation releases back to 074 when its reason clears
(hivecommons/hive#13040). The issues a PR covers
follow it: once the sweep stages a PR at 050 or later, the enumeration cycle
writes the same stage on each linked issue (`stage/040-covered-by-pr` until
then), so the issue list and the PR list read the same
([#12436](https://github.com/hivecommons/hive/issues/12436)).

An adoption pair is one logical PR everywhere a person might otherwise be
asked to pick between PRs (hivecommons/hive#15495). A clanker- or
hive-authored original closes in the adopting pass by policy
(`governor.labels.stage.adoption.close_superseded: on_adopt`, the default;
`on_merge` keeps it open until the copy merges): the write-back gate admits
the close as `superseded-agent-pr` without the repo opt-in label the
`contributor_adopt` kind otherwise wants, because the copy carries the same
commits and sign-off (hivecommons/hive#15526 sat open beside hive's #15574
as "Adopted → #15574 · superseded" with the gate saying
`needs-opt-in-label`). A human-authored original still needs that opt-in
label to close in the adopting pass; without it the gate keeps it open after
supersession (`needs-opt-in-label`, or a fork the hive cannot edit) and it
sits at 072 "superseded by #N" until the copy merges. Either way the
superseded original draws in the Activity Monitor as one muted,
chevron-less line under the issue (greyed title, no stage strip) whose
trailing note reads "superseded · hive adopted → #M" — or "superseded ·
closed · hive adopted → #M" once it is closed — on a single line (the title
ellipsizes first); the hover carries the long form ("Hive re-authored this
PR as #M with the same commits and sign-off. This PR closes as superseded
when #M merges." / "…is closed as superseded."). The tracker marks it
`superseded_by: M`; the line stays only while #M is open and listed and
drops once #M merges or closes. The issue's caption, open-PR count, parked-PR check and "Let hive decide — all N
PRs" fan-out all see only the hive's copy: the original's review verdict never
gates the issue, and the copy's body carries every `Fixes #N` / `Refs #N` line
of the original (an annotated `Refs #N — partial` travels as the bare
reference, so the copy keeps the issue link). The original counts again only
when the copy is closed without merging (`adopted_by_state` closed /
closed-twice / copy-failed).

A clanker PR entering `stage/072-returned-to-contributor` also triggers the
contributor nudge: the sweep posts one `<!-- hive:contributor-nudge head=…
-->` comment per head naming the failing checks / review asks (re-nudged
once at the stall threshold, cap 2) and hands the diagnosis to the contribute
hub, which — when the author's relay heartbeat is fresh — pushes a `fix-pr`
interrupt task into their relay session (parallel sub-session or preempt,
per the relay's declared capability) under a per-contributor fix budget;
queued fixes are served before new work ("contributors should clean up their
mess before taking on new stuff"). Code:
`src/pkg/github/automerge/automerge_contributor_nudge.go`, `src/pkg/fixqueue`
and `src/pkg/dashboard/contribute_fixqueue.go`.

One exception: a PR the merge train holds because **another open PR already
fixes the same issue** (lane reason `merge-train:duplicate-of#<winner>:…`)
is parked with its author — 072 for a relay contributor, 071 for a person,
070 for the App's own — as *superseded*, not at the merge gate as "Approved;
waiting its turn", and the 07x pass neither nudges nor adopts it: there is
nothing to fix, and adopting it would land the issue twice. Its one notice
is the duplicate-fix sweep's gated `<!-- hive:duplicate-fix -->` comment;
the tracker pill reads *Duplicate of #winner · needs a pick*, and *Let hive
decide* on either row closes it as superseded
([#15469](https://github.com/hivecommons/hive/issues/15469)). Upstream of
the train, an open PR's webhook that covers an issue also releases any
contributor task already dispatched on it (see
[operator-reference.md § Open-PR dispatch guard](operator-reference.md#open-pr-dispatch-guard-and-duplicate-fix-sweep-scanner)),
so the hive does not open the second PR in the first place.

The duplicate pick above sees one lane at a time. When the two PRs sit in
**different lanes** — the hive's own PR in the self lane and a trusted
human's PR in the trusted lane both closing one issue, as #15783 and
#15793 did for #15773 — neither lane's winner rule saw the other and both
marched toward merge. Every lane therefore also runs a **same-issue claim
gate** before its trains are grouped: a candidate whose closing issue any
other open PR of the repository also claims (`Fixes`/`Closes`/`Resolves`;
a `Refs` line is not a claim; an adoption copy and the original it
`Supersedes` are one change, not a pair) is held with lane reason
`merge-train:claim-conflict#<other>:<issue>/0:0/0:` and the log line
`same-issue claim conflict undecided: #A vs #B` — in every lane, so
**neither PR is admitted** while the conflict stands. The hive does not
pick: the stage engine parks the held PR at 085, the tracker chevron reads
*held · duplicate of #B · needs decision*, the row's options name the
concrete PRs (*Keep #A — admit it to the merge queue*, *Close #A as
superseded by #B*), and *Let hive decide* only recommends ("Hold both: #A
and #B claim issue #N. Keep #B, close #A — or keep #A, close #B"). A
recorded trusted decision on a PR's head (the tracker's Approve, or any
`/hive decision` on it — the downstream gates still judge the merge) is
what lifts the gate for that PR; closing the other ends it
([#15810](https://github.com/hivecommons/hive/issues/15810)).

### Hive-authored superseding PRs auto-merge

Owner rule (@clubanderson): *"if a pr is superseded by a pr that a hive
agent creates, it should auto merge"* — *"the only exception would be if
this is sentinel or some other emergent problem with the pr."*

A **hive superseding PR** is a PR the hive App itself authored
(`@hivecommons-hive[bot]`) whose body declares `Supersedes #N` — the PR a
takeover, close-first adoption or re-author lead opens to carry a
contributor's or bot's change. `github.IsHiveSupersedingPR` /
`github.HiveSupersedingPR` detect it; `PullRequest.Supersedes` carries the
number. Twice in one morning such PRs (#15649, #15646) parked at
`stage/085-needs-decision` on a reviewer's security-sensitive
`requires_human` and the owner had to click approve for work the hive had
already decided to carry. Under this rule they do not:

- **Reviewer findings go to the fixer.** On a hive superseding PR the
  aggregate (`review.hiveSupersedingFixRoute`) turns a `requires_human` —
  a risk-class decision question, a security-sensitive finding, an
  unresolved escalation — into `changes_requested` with the reason
  `hive-authored superseding PR: reviewer requires_human routed to the fixer; auto-merges once CI and review are clean`; the fixer
  addresses it and the swarm re-reviews, exactly like any other finding.
  A `reject` is not converted: it is an emergent problem with the PR. The
  stage engine captions the head *"hive-authored superseding PR —
  auto-merging once CI/review are clean"* (lane reason
  `hive-superseder:fixer`) rather than "needs your decision", and the
  reviewer prompts say so (`hiveSupersedingGuidance`).
- **Green and clean merges with no decision.** Once CI is green and the
  review has no open findings the PR follows the hive-authored admission
  path (`stage/080` → merge). The superseded PR is closed as superseded by
  the existing writeback (`Superseded by #N`) once it merges, as before.
- **The only exceptions are the hard stops**, computed in one place
  (`automerge.hiveSupersederHardStop`) and named in the 085 caption
  (`hive-superseder-hard-stop:<token>`):
  - `sentinel-unacknowledged` — the sentinel label is on and nobody has
    acknowledged its finding (`sentinel_acknowledged` in the review
    evidence bundle; an acknowledged sentinel, as on #15650, is not a
    stop);
  - `hold` — a `hold` / decision-class label or a repo/global hold;
  - `fix-attempts-exhausted` — the fixer spent its cycle cap on the
    reviewer's findings; reported as `ci-red-after-fixes` when CI is
    still red, `base-conflict-unresolved` when the head still conflicts;
  - `base-conflict-unresolved` — the hive's rebase-resolve episode
    already failed twice.

  A pending check, a first conflict or an ordinary reviewer finding is
  never a stop. The tracker row reads *"The hive's superseding PR would
  auto-merge (owner rule), but <stop>"*, and *Let hive decide* answers
  recommend-only, naming the stop (`delegatedecision.RuleHiveSuperseder`);
  without a stop it releases the PR under the same rule.

`auto_merge.hive_superseding.auto_merge: false` turns the rule off and
restores the previous behaviour (a `requires_human` parks the superseding
PR at 085 like any hive-authored PR).

### Empty-diff PRs of the hive's own

A PR of the hive's own (the App's, or an adoption it opened) whose diff
against its base is **empty** — GitHub reports zero changed files, and the
`base...head` compare agrees — carries nothing the base does not already
have. This is what an adoption looks like once the original's fix landed
through another PR. The hive closes it itself, through the writeback gate,
with the sweep's edited-in-place comment naming the PR that carries the
change (`<!-- hive-redundant-of: #N -->`, the marker the reviewer's verdict
set) and the `hive/superseded` label, and raises **no** human decision. The
same guards as the superseded rule hold: `governor.auto_close_superseded` on,
ACMM L6, not held / keep-open / draft / parked by a person, and a head at
least 15 minutes old so a branch mid-push is not read as empty. A *person's*
empty PR is never closed by the hive; it gets the decision prompt as before.
Code: `src/pkg/github/pr_empty_diff_hive_autoclose.go`, hooked from the
self-authored sweep and from the 085 route
([#15499](https://github.com/hivecommons/hive/pull/15499)).

Three things made #15499 sit a day at 085 instead:

- The adoption's dispatch records carried no `author_agent`, so the relay
  judged the App's own PR non-hive and its decision question went down the
  **contributor** lane (`contributor-lane:decision-requested`) to a person.
  Dispatch records now carry `hive_authored`, and the relay falls back to
  the PR author's login against the hive identity; an adoption routes
  through the self lane.
- Every merge-from-base pushed by the hive's own `update-branch` was a new
  head with no verdict, so the swarm re-reviewed and re-asked the same
  question (six heads, ~16 "Decision needed" reviews and 085 comments). A
  PR with no diff against its base now re-stamps its prior verdict on each
  head (`review.delta_rereview`); a decision question is identified by its
  `hive-redundant-of` PR (else its normalised words), and a second ask with
  the same identity refreshes the standing review in place and posts no
  second 085 entry comment.
- The tracker's *Decision needed* popover read the verdict at the current
  head only, so during a re-review it fell back to "a reviewer handed this
  PR to a person without a specific question". A row parked on a reviewer
  decision now quotes the newest decision-requested verdict on any head,
  shows a *redundant of #N* chip, and leads with **Close as superseded**.

**Already landed, by the reviewer's word.** #15647 and #15648 (adoptions of
#15324 / #15343) still parked at 085 with the question "should decide
whether to close this adoption … as already landed, since v5 already
contains the identical change via #15506": the empty-diff closer ran, but
its 15-minute freshness guard read the PR's `updated_at`, which the hive's
own labels and comments bump every pass, so the head was "too fresh"
forever. "Already landed / redundant / no-op" is the hive's own decision
about its own PR, not a person's. The 085 route now takes the
**already-landed** entry when the verdict for the head says so — the
`hive-redundant-of` marker, or wording that names the landing PR
explicitly ("already landed … via #N", "identical change merged in #N";
a question that merely mentions a PR is not a claim, and two candidate
numbers are ambiguous and name none). On that entry there is no
freshness guard (a reviewer confirmed this head), a head whose files
are each byte-identical on the base tip counts as empty when GitHub
still reports a diff, and the landing PR is taken from the marker or
the words; with no number the compare alone must prove redundancy. The
close is logged once with reason `self-authored:already-landed`
(tracker caption "already landed via #N — closing as superseded"),
and settles the adoption's lineage: each original the copy declared
`Supersedes #M` whose record names this copy is told, marked superseded
on the stage store (so the orphan recovery does not re-adopt a head the
base already has) and closed, and issues the copy would have closed are
closed with the redundant-of note. Only when the verdict names no PR
*and* the compare is not empty does the PR fall through to 085 as before
([#15647](https://github.com/hivecommons/hive/pull/15647)).

**Already landed, any author.** That entry was the hive's own: #15544, a
trusted contributor's PR for #15543, was +0/−0 against `v5` because
#15545 (merged) carried the identical change, and the reviewer's "should
decide whether #15544 can be closed as a no-op now that #15545 (merged)
carries this change" still parked it at 085 for nine hours. Whether a
change is already on the base is a measurement, not a trust call, so the
already-landed entry now takes any author's PR when the evidence is
unambiguous: the diff against the current base is empty (no files, or
every file byte-identical on the base tip) **and** the landing is proven
— the PR the reviewer named is merged, the issue this PR closes was
closed by a merged PR, or the compare lists no commits (the head is
contained in the base). "#N (merged)" in the reviewer's words names the
landing PR like "via #N" does. The hive comments "already landed via #N
— closing as superseded; thank you @author", closes the PR through the
supersession-close write-back gate, and logs
`already-landed:<author_kind>` (the caption is the same "already landed
via #N — closing as superseded"). A trusted author is treated exactly
like anyone else. A PR with a real diff, an empty PR whose landing is not
proven, a held or `keep-open` PR, or one a person parked keeps the
reviewer's question as before. The "Let hive decide" preview for such a
PR gives the evidence as its reason — "diff is empty — the change already
landed via #N (merged)" — never the author's standing
([#15732](https://github.com/hivecommons/hive/issues/15732)).

## Usage

```sh
hive-merge --repo <owner/repo> --number <N> [--method squash|merge|rebase] \
           [--expect-sha <sha>] [--update-branch]
```

| Flag | Required | Default |
| --- | :---: | --- |
| `--repo` | yes | — |
| `--number` (alias `--pr`) | yes | — |
| `--method` | no | `squash` |
| `--expect-sha` | no | auto-resolved (see below) |
| `--update-branch` | no | off |

Both `--flag value` and `--flag=value` forms work. `--squash`, `--merge`, and
`--rebase` are accepted as method shorthands. `--admin` is accepted and
**ignored** — the hive never admin-bypasses branch protection.

`--repo` and `--number` must resolve, or the script exits `2`. `--number` must
be an integer, or the script exits `2`.

### `--expect-sha` is auto-resolved, not optional in effect

The merge-request watcher **requires** a non-empty `expect_sha` (the F4 TOCTOU
guard above) — a request with none is denied outright. Rather than push that
burden onto every call site, the script resolves the PR's *current* head SHA
itself when `--expect-sha` is not given, using the cached App token at
`/var/run/hive-metrics/gh-app-token.cache`, and pins it into the request. The
head is captured at request time, closing the TOCTOU window exactly as
intended, so existing call sites that never passed `--expect-sha` keep
working unchanged.

If the SHA cannot be resolved (no token cache, `gh` unavailable, or the
lookup fails), the script prints an error naming the token-cache path and
**exits `3`** rather than writing a request with no pinned head.

### `--update-branch`

When set, the watcher syncs the PR branch with its base before attempting the
merge (resolves the common "behind main" case). A failure to update the
branch is not fatal — the merge attempt still proceeds and surfaces the real
blocker.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The PR has
not been merged yet at that point.** It merges on the next watcher tick
(polling every 10 seconds).

To confirm, poll the `.result.json` written next to the request file, or
simply check whether the PR is merged.

## Retries and what happens when a merge can't land

The watcher retries a failed merge attempt up to **3 attempts total**,
tracked across ticks via the prior `.result.json`. What happens after the
final attempt depends on why it failed:

- **A required check is failing or pending** (classified from GitHub's own
  branch-protection error text, e.g. "required status check ... has not
  succeeded") — the request is quarantined (renamed `.exhausted`), but the
  hive re-engages its fix loop for that PR instead of abandoning it, subject
  to a per-red-SHA re-dispatch cap.
- **The blocker is unfixable by pushing code** — a true merge conflict or a
  permission error (matched against GitHub's own wording, e.g. "merge
  conflict", "not accessible by integration", "403", "must be a member") —
  the request is quarantined (`.exhausted`) and not retried further; the
  result file records the last error.

An authorization denial (forge-resistance failure, `CanMerge` gate failure,
or F4 target-binding failure) is not retried at all — the request file is
renamed `.denied` immediately and the result file records the reason.

A malformed request file (invalid JSON) is renamed `.bad`.

## Where things live

| Path | What |
| --- | --- |
| `/var/run/hive-metrics/merge-requests` | request files the watcher consumes |
| `/var/run/hive-metrics/gh-app-token.cache` | cached App token, used to auto-resolve `--expect-sha` |
| `/var/run/hive/uid-map.json` | UID → agent-name map, used for a nicer log line |
| `/data/stage-labels.json` | persisted stage timing, last-activity clock and transition history for `stage/stalled` and the review-queue stage history |

The UID map is **informational only** here. The watcher re-derives ownership
from the file's UID regardless.

## Related

- [`hive-open-pr`](hive-open-pr.md) — the equivalent relay for opening a PR;
  same request-file mechanism and authorship model
- [Security threat model](security-threat-model.md) — forge resistance and the
  UID-ownership anchor
- [Agent configuration](agent-configuration.md) — ACMM levels and the
  merge gate (`CanMerge`, `ModeIssuesPRsMerge`) that governs whether an agent
  may merge a PR at all
- [Audit log](audit-log.md) — merges are recorded there with the requesting
  agent
