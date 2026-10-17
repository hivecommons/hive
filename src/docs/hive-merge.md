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

Immediately before the self-authored sweep merges one of the App's own PRs
(after the head SHA is re-verified), it reads the swarm review verdict for that
head from the same `review-verdicts.json` the merge-eligible lane uses and
classifies it as `no-review-bundle`, `awaiting-review-approval`,
`changes-requested:<perspectives>` or `approved`
([#12592](https://github.com/hivecommons/hive/issues/12592)). The mode is
`governor.review_gate.mode: off | shadow | enforce` (default `shadow`), raised
to `enforce` by `review.require_approval` and to at least `shadow` by
`review.shadow`; all three are re-read on every evaluation, so a config reload
takes effect without a restart.

- **shadow** (default): the merge proceeds as before; the evidence bundle for
  the head (started on the spot when none exists) records
  `shadow.review_gate` with `lane: self-merge`, `would_have_held` and the
  reason, so the Evidence card's Shadow gates panel can show what the gate
  would have held.
- **enforce**: anything but `approved` holds the merge with reason
  `review-gate:<outcome>`; the PR is retried on a later sweep.
- **off**: no evaluation.

Each evaluation writes one audit line, action `review_evaluated`, detail
`lane=self-merge would_hold=<bool> reason=<code> enforce=<bool> head=<sha>`.
The gate applies to the `self-authored` lane only; trusted-bot PRs are
unchanged. The merge and the CI snapshot are recorded in the evidence bundle
in every mode, including when the bundle is created at merge time.

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

A PR merges through this tier only when all of the following hold:

- The PR author is admitted by one of two paths:
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
  (`contributor-lane:workflow-path`) and does not delete a test file or remove
  25+ test lines while deleting at least 3× what it adds
  (`contributor-lane:tests-removed`).
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
  the soak and the skip reason reports the time left
  (`contributor-lane:soaking(remaining=25m0s)`).

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
```

The block is editable from **Settings → Features → Auto Merge →
Contributor-lane auto-merge** (enable switch, soak minutes, reviewer-verdict
switch, required tiers, excluded labels, repository allow-list) through the
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

### Stage labels

Every sweep cycle also writes the hive-owned `stage/NNN-*` label for each PR
it evaluated, derived from the same per-lane reason it logs
(`contributor-lane:no-verdict` → `stage/050-review-pending`, unless the
swarm's known verdict is `requires_human`, which the lane also reports as
no-verdict → `stage/085-needs-decision` with reason
`reviewer-verdict:requires_human`,
`contributor-lane:soaking(...)` → `stage/100-merge-queue`, approve verdict +
green CI but no lane decision yet → `stage/080-awaiting-merge-admission`
(a stall there is logged on the PR with the lane's last skip reason), red CI on a
human-authored PR → `stage/071-needs-author`, on a clanker PR →
`stage/072-returned-to-contributor`, hold / sentinel alert →
`stage/085-needs-decision`, untrusted author with an approve verdict at the
current head, green checks, DCO and soak done →
`stage/090-ready-for-human-merge` (only a maintainer's `/lgtm` `/approve` is
missing), merged → no stage), plus one `role/*` label saying
which admission tier the lane actually used, one `author/*` label saying who
wrote the PR (`human`, `clanker` = a contributor's hive, `hive` = this repo's
own; #12581) and `stage/stalled` when nobody
but the hive has touched the PR for the stage's dwell (never while checks run
or the reviewer verdict is pending). Backward moves are a one-line stage
comment and dashboard history, not a label. Writes happen only when
the planned set differs from the labels already on the PR, under the
`hive:stage_labels` REST caller, and the whole family is switched by
`governor.labels.stage.enabled`. The mapping table, numbering rules, dwell
thresholds, the mermaid state machine and the off-happy-path flow
(072 → nudge → 074 → takeover) are in
[labels-and-control-signals.md](labels-and-control-signals.md#stage-labels-stagennn--the-pipeline-state-machine);
the code is `src/pkg/stagelabels` and
`src/pkg/github/automerge/automerge_stage_labels.go`. Adoption
(`stage/074-adopted-by-hive`, `automerge_contributor_adopt.go`) reuses this
sweep's own red-PR route: the superseding App PR it opens from `hive/adopt-<n>`
is red with the contributor's failures, lands in `ci-failing.json` like any
hive-authored red PR, and the scanner's FIX-BEFORE-NEW kick rebases and fixes
it on that branch (never a replacement PR), after which it follows the normal
`050 → 100` path. When adoption is in-place on the contributor PR's own
branch, a changed head stays adopted only with hive-controlled evidence
(GitHub-linked App commit metadata; the stage store only remembers SHAs that
already passed that check);
forgeable git author/committer names and emails are treated as contributor
pushes and release adoption once repo evidence exists
(`governor.labels.stage.adoption.strict_attribution`, default on). Push-capable
App-authored agents commit with the App bot noreply email (keyed by the bot's
GitHub user id, not the App id) so their fixer pushes
normally provide the GitHub linkage, while their commit name/sign-off still
shows the lane. The issues a PR covers
follow it: once the sweep stages a PR at 050 or later, the enumeration cycle
writes the same stage on each linked issue (`stage/040-covered-by-pr` until
then), so the issue list and the PR list read the same
([#12436](https://github.com/hivecommons/hive/issues/12436)).

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
