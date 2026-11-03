# v6 operator controls

## Dashboard Runs section

The dashboard's **Runs** section shows the active long-running run projection
from `GET /api/runs` and the `/api/status` SSE stream. Each card names the run,
current `spec → plan → implement` stage, who or what it is waiting on, how long
it has been in that wait state, and any plan/receipt artifacts the run exposed.
Owner users see inline **Approve** and **Reject** buttons for runs waiting on a
human plan gate; non-owner users see the same state without mutation controls.

Click **detail** on a run to fetch `GET /api/runs/{key}` and inspect the full
stage/artifact chain plus matching audit or approval entries. Agent cards also
show the current run held by that agent, and the Governor/Health surfaces call
out runs blocked on a human or stalled beyond the default 60-minute wait
threshold. If an older spoke omits the `runs` field entirely, the dashboard
renders `runs: unknown` instead of substituting zero.

## Per-repo merge strategy (`merge_strategy`)

`merge_strategy` decides **how** Hive merges into a repository, never
**whether** it does: `auto_merge`, the ACMM gate, holds, pauses and every
merge path's own checks apply exactly as before. It is set per repository,
next to the per-repo `auto_merge` switch:

```yaml
project:
  repo_policies:
    - repo: atomic-image-builder
      merge_strategy: hive-serialized   # or: direct (the default when unset)
```

| Strategy | Behaviour |
|---|---|
| `direct` (default) | Each merge path (the self-merge sweep, the `lgtm` queue, the [`hive-merge`](../hive-merge.md) relay) merges an eligible pull request on its own, exactly as Hive always has. Deciding a repository is `direct` needs no GitHub call. |
| `hive-serialized` | The **serialized lane**: per target branch at most one pull request — the one at the front — is brought up to date, has its required checks run on its exact head, is re-checked live and is merged with its head pinned. Every other eligible pull request waits in order with the reason `deferred: not at the front of the lane`. Meant for personal-account repositories, where GitHub's merge queue is not available. |

An unrecognised value is a configuration error for that repository: Hive does
not merge there and does not fall back to `direct`. Anyone who may switch a
repository's auto-merge off (owner or repo write) may switch it to
`hive-serialized`; only an owner may switch it back to `direct`. The dashboard
repository card shows the strategy and offers the change to permitted users;
the same API is `GET`/`POST /api/repos/merge-strategy` (see
[api-reference.md](../api-reference.md)).

`merge_strategy` is unrelated to `knowledge.primer.merge_strategy`, which
controls how knowledge primers are merged. The two only share a name.

### Front timeout (`merge_front_timeout`)

The serialized lane's front timeout is 60 minutes unless configured. Set it
hive-wide under `project`, and override it per repository next to
`merge_strategy`; the value is a Go duration (`90m`, `2h`). An invalid or
non-positive value is a configuration error.

```yaml
project:
  merge_front_timeout: 90m            # hive-wide; unset means 60m
  repo_policies:
    - repo: exult
      merge_strategy: hive-serialized
      merge_front_timeout: 3h         # this repository only
```

When the front's timeout runs out while every required check on the head it
is evaluating is still queued or in progress (none failed, none missing), the
lane is waiting on CI capacity rather than on the pull request: rotating the
front would only start another CI run for the next pull request. The lane then
extends the deadline by one more timeout and records a `front-extend` decision
(`front timeout extended by 1h0m0s (1 of 3): required checks still queued or
in progress; last known state: …`). It does so at most three times, so a front
waits at most 4x the timeout after its last branch update; after that it
leaves with the usual `front timeout after …` reason.

### What the serialized lane guarantees

For every `hive-serialized` repository:

1. At most one pull request per target branch is in final validation, across
   all merge paths and across restarts (the front record is durable under
   `/data/mergelane/`; a restart repeats an interrupted validation in full).
2. No pull request merges unless its head contained the target branch tip the
   final re-check read.
3. No pull request merges unless every required check finished successfully on
   the exact head that merges. Missing, queued or running never counts as
   passing; an unknown or empty required-check set means no merge.

**Administration: Read-only is optional for `hive-serialized`.** Without it
the App cannot read classic branch protection (GitHub answers 403); the lane
then uses the required checks the branch's rulesets list, and GitHub still
enforces any classic rule itself at merge time. The lane panel and the ACMM
merge-queue row show the reason when the required-check set is unknown, for
example a 403 on a branch whose rulesets require no checks. Without Administration,
classic-protection `require_code_owner_reviews` is likewise invisible (the
403 reads as "not required"), so only ruleset code-owner rules are detected.
4. The merge call pins the validated head.
5. Immediately before the merge Hive re-reads the head, the branch tip,
   open/draft state, the target branch, mergeability, the required-check set
   and its results, hold/do-not-merge/exempt/pause labels, the strategy,
   auto-merge permission and the calling path's own authorization.
6. Any change, error or ambiguous answer means no merge (fail closed).
7. One validation never covers two merges.
8. No existing gate on any merge path is weakened or skipped, and setting the
   strategy never turns merging on.
9. Repositories left on `direct` behave exactly as before.
10. Hive never rebases, force-pushes or otherwise rewrites a pull request
    branch; it only merges the target branch into it, pinned to the head it
    evaluated.

A front pull request whose required check fails on the head the lane is
evaluating leaves the front at once with that reason (for example
`required check "test" finished as failure on head 4d9ac7c`), and the next
one starts. This holds even when a merge path (the self-merge sweep or the
`lgtm` queue sweep) rejects the front before the lane, for example as not
mergeable once GitHub reports it `blocked`: the path still hands its current
front to the lane, which re-evaluates it without merging and lets it leave
with its own reason. The front timeout (`merge_front_timeout`, 60 minutes by
default, restarted on every branch update, and extended up to 4x while the
required checks are only queued or in progress) is the fallback for a front
whose checks never finish; its reason says what the lane last knew, for
example that the head after the branch update was never evaluated, or the
last recorded waiting reason. A front whose merge call GitHub definitively
refuses (HTTP 403, 405 or 422, for example a merge method the repository
does not allow) also leaves at once with the reason `merge refused by
GitHub: ...`, since nothing was applied; a 5xx or transport error on the
merge call is not definitive, so that front keeps waiting and re-validates
until the front timeout.

A front pull request that is closed, merged outside the lane, turned into a
draft or retargeted to another branch leaves on the next automerge sweep
pass. The merge paths list open pull requests only, so none of them calls in
for it again; instead every pass reads each front of a `hive-serialized`
repository once and releases a front in one of those states with a reason
such as `front check: pull request is closed` or `front check: pull request
is already merged`, and the next waiting pull request is promoted. An open
front, including one waiting on its required checks, is left as it is.

A front that GitHub still reports as `blocked` after its head contains the
current target tip and all required checks pass also leaves immediately. The
lane re-reads the PR after observing passing checks, so a block in the earlier
snapshot alone cannot evict the front. If the final live re-check still finds
the block, the lane records that no remaining lane action can satisfy it and promotes
the next pull request. Pending, missing or unreadable required checks still
wait; an eligible pull request that returns joins the back of the lane.

The lane's branch updates and merges pass the same writeback containment as
every other write (`contributor_adopt`: L6 plus the opt-in label for an item
the hive did not author), except on the explicitly requested paths (the `lgtm`
queue, the `/hive merge` relay and the dashboard), which only an operator
pause stops. A pull request that only an automerge sweep lane would merge,
and whose writes containment refuses (for example a contributor PR without
`hive-review` at L6), takes no lane place: the sweep logs
`the sweep merge path cannot update or merge this pull request, so it takes
no lane place: …` and the next eligible pull request goes ahead. A front whose
sweep writes become refused leaves on that path's next call, and stays out
until containment allows it again.

### The remaining window, and GitHub's up-to-date rule

No GitHub merge API can be made conditional on the target branch's tip; they
pin only the head. Between Hive's final re-read of the tip and the merge call
there is therefore a window of about one API call. Without GitHub's own
"Require branches to be up to date before merging" rule on the branch, a commit
landing in that instant can slip through (guarantee 2 then fails). Hive does
not hide this: after every merge it checks that the merge landed directly on
the validated tip and, if it did not, raises an alert naming the merged pull
request and the unexpected commit (and that commit's pull request, when it has
one). The alert is logged at error level (`merge lane: merge did not land on
the validated tip`) and recorded as an `alert` lane decision.

**Recommendation:** turn GitHub's "Require branches to be up to date before
merging" rule on for every target branch of a `hive-serialized` repository
(Settings → Rules → Rulesets, "Require status checks to pass" with "Require
branches to be up to date"). GitHub then closes the window on the server. It
costs one CI run per merge, because the lane only ever updates the pull request
at the front.

### Seeing what the lane is doing

On a `hive-serialized` repository card the dashboard shows a serialized lane
panel per target branch: the pull request at the front and its stage
(updating, waiting for checks, final re-check, merging), the waiting pull
requests in order, the last reason a pull request left the front, every
recorded no-merge or deferral reason, and the guarantee in force:

| Label | Meaning |
|---|---|
| server-enforced | A ruleset on the branch requires up-to-date branches; GitHub closes the window. |
| Hive-checked | Rulesets require checks but not up-to-date branches; only Hive's own check and the post-merge alert apply. |
| unknown | No ruleset speaks to it. Classic branch protection carries the flag, but reading it needs the Administration permission the App does not hold. It also appears when Hive could not read the branch rules at that moment (for example while rate limited); the reason is shown and no up-to-date-rule recommendation is shown in that case. |

For Hive-checked and unknown lanes the panel recommends turning the up-to-date
rule on. The same state is available to scripts, the admin MCP server and
`hivectl` through `GET /api/repos/merge-lane?repo=<repo>`; a `direct`
repository returns only its strategy and has no lane panel.

### Fork pull requests

The lane brings a pull request up to date only by merging the target branch
into its branch, pinned to the evaluated head; it never rebases or
force-pushes, and never rewrites a contributor's commits. A fork pull request
that already contains the current tip with passing required checks needs no
update and merges normally. Fork CI waiting for maintainer approval
(`action_required`) counts as not passed. The lane updates a behind fork
branch only when GitHub reports that Hive can update it
(`viewerCanUpdateBranch`, which needs "Allow edits by maintainers") **and**
`review.contributor_prs.base_sync` is on. Otherwise the pull request leaves
the front at once, without waiting for the front timeout, with the reason
`fork pull request is behind the target branch and Hive is not allowed to
update it: …` naming which condition failed. Hive posts one comment asking the
author to merge the target tip, and the next pull request starts. Once the
author has merged the tip and the required checks pass, the pull request
rejoins the lane and merges at a later turn. A pull request merged through a
`/hive queue` approval must be queued again after any new head, including a
merge of the tip: the approval covers exactly the approved commit.

### Branches with GitHub's merge queue

A target branch that has GitHub's merge queue is left alone: the serialized
lane does not run for it and Hive makes no direct merge into it. When Hive
cannot establish whether the branch has GitHub's merge queue, it does not merge
(fail closed). The lane panel lists the refusal among its reasons.

### Downgrading

A Hive without the serialized lane ignores the `merge_strategy` key
(configuration decoding ignores unknown keys), so after a downgrade every
opted-in repository silently goes back to `direct` merging. Older versions
cannot be changed to warn about this; check your repositories' strategy before
downgrading.
## Red-main circuit breaker (`red_main_threshold`)

When a required check has been red on a repository's default branch for more
than N hours, PRs opened against it will be red too, and the PR that fixes
main is the only one that matters. Every ~10 minutes the hive reads each
active repo's default branch tip (`src/pkg/github/red_main.go`): the required
checks (config, classic branch protection and rulesets) that concluded red,
the failing run, the files its annotations name, and an open
`[shared-ci] <check> failing across <owner/repo>` tracking issue. A check is
dated from the first time it was seen red since it was last green; while the
tip's run for it is still queued or in progress (a push to the red branch) it
keeps its previous state, so pushes do not restart the clock. Once it has
been red for **more than** the threshold:

- **Fix-main PRs rank first** in the review queue. A PR fixes main when it has
  the `fixes-main` label, OR closes the tracking issue (`Fixes #N`), OR its
  changed files overlap the failing check (a file the failing run's
  annotations name or one in the same directory, or a workflow under
  `.github/workflows/`). A PR whose own checks pass while main fails is
  marked *confirmed*, but passing checks alone never make a PR a fix.
- **New PR opening pauses in that repo.** `hive-open-pr` refuses the request
  with gate `red-main` and a message naming the check, the run and the fix PR,
  unless the request itself fixes main (it names the tracking issue in
  `--issues` or closes it in its title/body). If the sweep has not seen the
  tracking issue yet, the issues the request names are read on the spot, so
  a PR naming a just-filed `[shared-ci]` issue for the check opens at once.
  Existing PRs keep going and
  issue filing is unaffected; agents are told the same in their kick.
- **The repo card** shows `main red since <time> on <check>; fix: #N waiting
  on <who>`.

Everything lifts automatically on the first sweep that sees a completed green
run of the check.
A repo whose required-check set or check runs cannot be read keeps its last
observation (a read error neither trips nor clears the breaker). Trust levels,
the trusted-merger definition and the self-merge floor are unchanged.

The threshold defaults to `6h`. Set it hive-wide under `project` and override
it per repository; the value is a positive Go duration.

```yaml
project:
  red_main_threshold: 8h              # hive-wide; unset means 6h
  repo_policies:
    - repo: dakota-iso
      red_main_threshold: 2h          # this repository only
```

## Open held PR cap (`max_open_held_prs`)

Agents can open PRs faster than people review them. With a cap set, a repo
stops receiving new hive PRs while too many are already waiting on a human
([#13645](https://github.com/hivecommons/hive/issues/13645), part of
[#13617](https://github.com/hivecommons/hive/issues/13617)). The cap is per
repository, never per agent, and off unless configured.

- **What counts:** open PRs authored by the hive's GitHub App that still carry
  a hold label (the level `hold`, or any other hold Hive recognises). Human
  PRs, other bots' PRs and issues never count. A test-only or docs-only PR
  counts until it is auto-released
  ([#13606](https://github.com/hivecommons/hive/issues/13606)); once its hold
  is removed — by that release or by a person — it stops counting, as does a
  PR that is merged or closed. The count is refreshed every governor cycle
  (`src/pkg/github/open_held_prs.go`).
- **Requires GitHub App auth:** PRs are attributed to the hive by the App's
  bot login, so under a personal access token
  ([`HIVE_GITHUB_TOKEN`](runtime-and-quota.md#hive_github_token-permissions)) no PR can be
  attributed, every count stays 0 and the cap never fires. The governor then logs
  `max_open_held_prs is set but cannot be enforced` once, naming the capped
  repos ([#13739](https://github.com/hivecommons/hive/issues/13739)).
- **At the cap:** `hive-open-pr` refuses the request with gate `capacity` and
  the line `repo at capacity: review or close existing PRs first`, and
  PR-opening agents get the same line in their kick under the repo's name.
  Existing PRs keep going and issue filing is unaffected. Trust levels, the
  trusted-merger definition and the self-merge floor are unchanged.

Set it per repository under `project.repo_policies`: `0` means the default of
`12`, a positive number is the cap, and leaving the key out means no cap.

```yaml
project:
  repo_policies:
    - repo: console
      max_open_held_prs: 0            # cap at the default, 12
    - repo: docs
      max_open_held_prs: 5            # this repository only
```
