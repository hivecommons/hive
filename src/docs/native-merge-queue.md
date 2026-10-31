# Native GitHub merge queues

## Composable capabilities, not three merge engines

1. **Conventional direct merge:** coordinated, per-repo/base preparation
   ([#15836](https://github.com/hivecommons/hive/pull/15836)) reduces competing
   refresh writers and separates ready seats from pending preparation. It is
   independently useful without a native queue. Check that PR's merge and the
   deployed version before assuming it is available on a particular hive.
2. **Native GitHub queue:** the admission hand-off shipped in
   [#15308](https://github.com/hivecommons/hive/pull/15308). This page describes
   the accompanying CI/policy integration. It is per-repository opt-in;
   merged code, deployed code, Hive's enabled flag, and GitHub's queue rule
   are four different states. Hive controls admission and repair; GitHub is
   the sole integration/landing writer.
3. **Integration-aware multi-agent orchestration:** planned coordination
   layered on these capabilities, not implemented or selectable here.

The existing Features settings panel shows native-queue configuration and
links here. Configuration remains in `hive.yaml`; the panel does not change
GitHub rules or activate a queue. Hive's visual train is not GitHub's FIFO
queue. A pending native group can still block later landings, and a queue's
merge limits do **not** combine CI builds or eliminate head-of-line blocking.

## Opting in

The hand-off is **off by default** and opt-in per repository. A hive that
never sets `auto_merge.native_merge_queue` sees no behaviour change: no lane
probes a queue, enqueues, withdraws an entry or lists a paused repo's PRs, and
a queue-required base still parks at `merge-queue-required`.

```yaml
auto_merge:
  native_merge_queue:
    enabled: true                 # default false
    repos: [hivecommons/hive]     # owner/repo allow-list; empty = every watched repo
    bases: [hivecommons/hive#v5]  # exact repo#base pairs; branch case is significant
    skip_current_base_gate: false # default false — see "Current-base gate" below
    probe_ttl: 30m                # default 30m — see "API cost" below
```

| Key | Default | Meaning |
| --- | --- | --- |
| `auto_merge.native_merge_queue.enabled` | `false` | Turns the native-queue hand-off on. |
| `auto_merge.native_merge_queue.repos` | unset (every watched repo once enabled) | `owner/repo` allow-list, matched case-insensitively. Repos outside it keep the pre-queue behaviour. |
| `auto_merge.native_merge_queue.bases` | unset (all queue bases in opted-in repos) | Additional exact `owner/repo#base` allow-list. Branch names are case-sensitive. Unlisted bases never enqueue or bypass current-base refresh. |
| `auto_merge.native_merge_queue.skip_current_base_gate` | `false` | Lets a queue-enabled base bypass the `auto_merge.require_current_base` gate and the `behind` update-branch. Off keeps both gates in force on queue bases. |
| `auto_merge.native_merge_queue.probe_ttl` | `30m` | How long one base branch's `isMergeQueueEnabled` answer is reused before it is re-read. |

Owners can configure the same policy at **Settings → Features → Auto-Merge →
Native GitHub merge queue**, then click **Save**. The enable switch and
“Queue owns freshness” switch are both **OFF by default**. Repositories,
exact repository/base pairs, and the queue-detection TTL are editable there.
Settings refuses enabling without both explicit scope lists, rejects malformed
or wildcard scopes, mismatched repositories and negative/invalid durations,
and preserves omitted fields on partial updates. `0` TTL uses the 30-minute
default. Existing YAML configurations retain their original unscoped semantics
when the optional lists are absent; the Settings enable path deliberately
requires explicit scope.

Saving changes the Hive policy only: it does **not** create a GitHub queue,
change branch protection, set the external freshness variable, deploy code or
attest that prerequisites are satisfied. Follow the pilot rollout below first.
Disabling does not drain existing GitHub queue entries: drain or withdraw them
before rollback. Trust/review/hold/CI/soak and attribution gates remain intact.

## Activity Tracker: observed operation, not a second merge engine

When enabled, **Activity Tracker → Forge-owned merge queues → GitHub merge
queue** is a separate region beside the preparation rail, outside actual
pipeline rows. It displays provider-observed entry positions, exact PR heads,
provider states and enqueue ages. Readiness/preparation, native admission,
candidate testing and an actual merged receipt are different states.
Linked issue/PR trees are not reparented or duplicated into pipeline rows;
references say whether that exact repository/PR/head/base is on the page,
outside its filters/page, or absent/changed. Repository filtering applies to
the native rail; its references are not silently dropped by row pagination.

Candidate cards identify actual `merge_group` synthetic refs and SHAs from
authenticated events or Actions runs. **Membership is unknown** unless a signed
event base, the live queue chain, every exact member head's ancestry and a final
unchanged synthetic-ref read all agree. FIFO positions and PR numbers embedded
in ref names are never membership proof. Several observed candidates can appear
in parallel. The CI drilldown reads only the candidate SHA, including required
check names, status, conclusion, producer binding and available job links; it
never substitutes green PR-head checks. Classic protection and applicable rules
must both be readable before required policy is known. Missing/pending/skipped
checks or unreadable policy are never portrayed as passing.

The provider-neutral `forgestate.QueueObservation` contract is read-only and carries
no admission or merge authority. The GitHub adapter feeds an in-memory cache
from the existing governor cycle, at most once per three minutes, rotating
across two scopes per pass with one 45-second background budget. Each scope
reads at most 100 entries and 100 workflow runs; at most four recent candidate
cards and eight member ancestry checks per card are observed. Larger/incomplete
cohorts explicitly report partial coverage rather than implied completeness.
There are no per-render or per-PR observation pollers; OFF performs no
observation API reads. Evidence older than six minutes is explicitly stale;
failed refreshes retain older evidence with unavailable/stale diagnostics.
More than two configured scopes can therefore be stale while rotation catches
up. Observations need App installation read access to queues, Actions,
checks/statuses, contents and branch policy (including Administration read for
classic protection). Verify those permissions explicitly; enqueue permissions
alone do not establish this visibility. Unavailable authentication
or permissions are reported, not silently substituted with configured checks.

Authenticated `merge_group` deliveries retain candidate identities and
`destroyed` receipts. Complete consecutive entry snapshots can establish an entry's
removal, **not its reason**. Only an authenticated merged-PR delivery for a
previously observed exact queued/member head records a merged receipt and its
actual merge SHA. Failed required CI is shown on that candidate; moved/expired
refs do not prove failure, rebuild or merge. No rebuild reason, percentage or ETA
is invented. The last 32 lifecycle receipts and 16 event hints are memory-bounded,
not an exhaustive durable audit. At most 32 scope snapshots are retained;
evicted or not-yet-observed configured scopes report unknown. Missed deliveries and restart gaps remain
explicit. Configure delivery of `merge_group` and `pull_request` events to the
existing authenticated webhook endpoint for full event evidence; discovery
without a signed group base intentionally leaves member mapping unknown.

## How it works

Automerge sweeps detect a PR's target queue with GraphQL
`PullRequest.isMergeQueueEnabled`. Once the lane's existing approvals, trust,
holds, review, CI and soak gates allow a merge, Hive calls
`enqueuePullRequest` with `expectedHeadOid` set to the validated head. GitHub
runs the merge group's checks and performs the eventual merge. Branches without
a queue keep the direct REST merge path.

Hive uses its GitHub App installation transport. Static tokens (including a
workflow's `GITHUB_TOKEN`) retain the existing direct-merge behavior and
`merge-queue-required` refusal, since workflow-token events cannot reliably
start `merge_group` workflows. The App installation must grant the existing
merge permissions (`contents: write` and `pull_requests: write`). Permission
or GraphQL failures are surfaced as queue failures; Hive never falls back to
bypassing a detected queue. This change does not request wider App permissions.

Already queued PRs are left in place. The sweep reason reports
`native-merge-queued: queued (position N, STATE)` and maps to
`stage/100-merge-queue`; enqueueing does not emit a completed-merge event or
close an issue. The stage observation carries the reason to the dashboard.

When the owning lane rejects a PR because it lost merge eligibility — a hold
or `do-not-merge`/`needs-human` label, a removed merger label, a missing,
untrusted or stale queue approval, changes requested, a draft or closed PR, a
release in progress on the repository, or a human-merge-path hold — Hive rechecks the entry's enqueuer and withdraws it only if it belongs to
this App (a `Bot` enqueuer whose login matches the App's slug). Every other
reason, including transient refusals such as `behind`, `not-mergeable`, pending
checks, a moved head, API read errors, author-routing skips and Hive's own
native-queue outcomes, leaves the entry for GitHub's queue to settle. Entries
from a person or another App are preserved.
The queued sweep also withdraws this App's entries on opted-in repositories
that are paused or have automerge disabled. Every enqueue and dequeue uses the
existing mutation boundary, including its write denials. Reconciliation is
periodic/event-driven, so a gate change can race GitHub's eventual merge.

A queue-required base reports `mergeable_state: blocked` (or `behind`) until
the PR merges through the queue, so on a queue-enabled base the queued lane
does not treat those states as `not-mergeable`. A failed queue read keeps the
non-queue behavior.

## Current-base gate

The `auto_merge.require_current_base` gate and the `behind` update-branch stay
in force on a queue-enabled base exactly as on any other base: a behind head
is updated (or parked) before it is enqueued. The merge group does test the
merged result itself, and a pushed head drops the PR from the queue, so an
operator who prefers to let the queue own freshness can set
`auto_merge.native_merge_queue.skip_current_base_gate: true`. That bypass is
explicit and hive-wide for opted-in repos; it is never implied by the presence
of a queue.

## API cost

Each `isMergeQueueEnabled` probe is a GraphQL read. The answer is cached per
`owner/repo#base-branch` for `auto_merge.native_merge_queue.probe_ttl`
(default 30 minutes), so a sweep over many PRs on one base costs one probe,
not one per PR; a read failure is not cached. The per-held-PR entry
reconciliation, the extra `GET /pulls/{n}` the queued lane needs to run it
on a prefiltered PR, and the 10-minute paused-repo PR listing all run only on
repos that opted in through `auto_merge.native_merge_queue`.

Queue rules choose the merge method. Where Hive elected a merge commit to
preserve mixed authorship and DCO attribution, the queue must use `MERGE`;
otherwise Hive reports `native-merge-queue-method` rather than enqueueing.
Like a direct merge GitHub refuses because the merge method is disabled, the
PR parks at `stage/085-needs-decision` for a person, with the `merge-method`
explanation: change the queue's merge method to `MERGE`, or merge it by hand.
The refusal is remembered for 30 minutes per head, so the queue's method is
not re-read every sweep; a new head or the recheck window reads it again.
Read and enqueue failures (`native-merge-queue-read`, `-enqueue`) stay at
`stage/100-merge-queue`; a failure to read the queue's merge method is a
read failure too, not a refusal, and is reported as `native-merge-queue-read`.

Required workflows must subscribe to `merge_group` in addition to their PR
triggers. `v2-ci.yml`, `v2-tests.yml` and `go-security-analysis.yml` subscribe
to `merge_group: {types: [checks_requested]}`. Their default checkout tests
the synthetic `github.sha`, not a member's head. Group events always select
the full suite/scans, including coverage; the PR-only affected-package and
out-of-scope shortcuts do not apply. Distinct groups use independent run IDs
and cannot cancel each other. Failure issue creation/publishing stays scoped
to its existing push/schedule events; candidate test jobs have read-only
tokens. The coverage reporter's write permissions are job-scoped, not
inherited by group test jobs.

## Required checks and provider boundaries

The existing eleven v5 checks cannot simply be required under their old
names on a group SHA. Probot DCO (App **1861**) checks PR commits; GitHub
Actions (App **15368**) cannot legitimately impersonate that provider on a
synthetic commit. PR verifier/changelog/attribution checks also need real PR
metadata, not a fabricated last-member PR event.

`native-queue-policy.yml` owns the **new** `native-queue-policy` check:

- On `pull_request_target`, it executes only the trusted base's validator
  and waits up to 22.5 minutes for the five existing policy checks on that
  exact PR head. No candidate code is executed; tokens are read-only.
- On a group it executes the event's trusted `base_sha` validator. It reads
  the live queue prefix ending at the event's exact synthetic head,
  verifies contiguous membership and the base/head commit chain, and checks
  **every** member, not the PR number encoded in the temporary branch name.
  Each current member head must be an ancestor of the synthetic head.
- Every member must have all eleven original check results on its exact
  head from the expected App. Actions results additionally bind to their
  actual workflow file, event, head, matching check-suite ID and exact job's
  `check_run_url` through the Actions API. A copied `details_url` or check
  name is not proof of membership in that workflow. Only technical PR jobs may
  carry their existing path-scoped `skipped` result; the five policy proofs
  require `success`. No missing, neutral, failed or unavailable evidence is
  treated as success. The verifier retains its existing advisory behavior;
  this bridge does not turn its underlying implementation into a strict
  content validator. Dependabot runs that same trusted PR-target verifier
  rather than skipping it; skipped policy evidence is never exempted by actor.
- Draft/closed/held PRs, changes requested, dismissed approvals and approvals
  on an old head fail closed. Membership and mutable PR/review state are
  re-read before success, and the synthetic ref must still name this group.
  Missing/pending evidence has a bounded wait; API errors, inconsistent
  membership and untrusted provenance fail the check. A later owner rerun
  must read current evidence again; it cannot bless a vanished group.

Hive's per-head author trust, internal reviewer verdicts, admission approvals,
soak and mutation boundary remain authoritative **before enqueue**. The
bridge does not recreate Hive's private approval store or invent another
reviewer. Admission loss is also reconciled by Hive's App-owned withdrawal;
[#15781](https://github.com/hivecommons/hive/pull/15781) is a rollout
dependency for release-freeze and human-merge-path withdrawal. It is not
included in this implementation. A label/approval/config change after the
final reads can race GitHub landing; neither this check nor periodic
withdrawal is an atomic lock spanning two systems. An urgent stop must
remove entries/disable the GitHub queue, not merely wait for a Hive sweep.

### Equivalent check mapping (manual migration)

Do **not** change live protection until the rollout below. The intended v5
required set is seven contexts, all bound to GitHub Actions App **15368**:

| Required on PR **and** group | What it enforces |
| --- | --- |
| `native-queue-policy` | PR-head `gate` (`docker.yml`), external `DCO`, `changelog-fragment-guard`, `verify PR contents`, `Sign-off survives the squash`; on groups, all eleven original member-head proofs plus live membership/holds/reviews |
| `build-and-test` | Real synthetic-group build/validation (`v2-ci.yml`) |
| `test` | Real synthetic-group aggregate; fails on missing/failed shards (`v2-tests.yml`) |
| `coverage` | Real synthetic-group shard profiles and unchanged package floors (`v2-tests.yml`) |
| `golangci-lint` | Real synthetic-group lint (`go-security-analysis.yml`) |
| `gosec (insecure patterns)` | Real synthetic-group security analysis |
| `govulncheck (known vulnerabilities)` | Real synthetic-group vulnerability analysis |

The five replaced names **continue to run on PR heads** and are transitively
required by the new owned check; DCO has not been removed as a logical
requirement or relabeled as an Actions result. Do not retain those five as
direct group requirements without a supported provider implementation: that
would deadlock the queue. Do not add fake success statuses to get past it.
Do not bind `native-queue-policy` to Probot. Any `auto_merge.required_checks`
override must agree with the new required set; original member evidence is
still checked by the bridge. Non-pilot branches retain their current rules.

## Pilot rollout

No deployment, repository-variable update, branch-rule mutation or runtime
enablement is performed by this code. An owner must do the following, in order:

1. Land and deploy #15836 first, and land #15781 plus this integration with
   their latest-head CI green. Keep native queue disabled during preparation.
   Do not cherry-pick or silently bundle those other branches. A Go fixture
   repair on #15836 is independent of this integration.
2. Confirm the trusted base contains the new workflows and validator. Obtain
   a real green `native-queue-policy` PR-target run on a disposable v5 PR,
   including DCO from App 1861 and all original PR checks. This initial
   integration PR cannot demonstrate its new PR-target base workflow until
   that workflow exists on v5. Test a multi-member group in a controlled
   queue pilot before expanding beyond one member; mocked fixtures are not
   proof of GitHub's live membership/commit-chain representation.
3. In repository **Settings → Branches → v5 protection → required status
   checks**, first add `native-queue-policy` (GitHub Actions) **alongside**
   all eleven old checks. Confirm an ordinary PR passes all twelve. Preserve
   every other protection, including `strict=false` and current review
   policy; do not broaden bypass actors. Ensure the owner/reviewer workflow
   fence protects changes to these trusted workflows/scripts.
4. Configure the external writer's repository Actions variable, initially
   inert unless a real queue is detected:

   ```sh
   unset GITHUB_TOKEN && gh variable set HIVE_NATIVE_QUEUE_FRESHNESS \
     --repo hivecommons/hive --body 'hivecommons/hive#v5'
   ```

   This is a space-separated, **case-sensitive** `owner/repo#base` allow-list.
   Set Hive's repository allow-list to `[hivecommons/hive]` and explicitly
   set `skip_current_base_gate: true`, leaving `enabled: false` for now.
   The workflow variable alone cannot enable a queue or Hive admission.
5. In **Settings → Branches → v5 protection**, in a single saved rule edit,
   enable **Require merge queue** and replace the old eleven-check binding
   with the seven-context mapping above, each with expected source
   **GitHub Actions**. The new owned gate is already verified and required
   before this edit; never remove old bindings first in a separate save.
   If the UI/plan cannot express this, stop rather than leave an interval
   with incomplete requirements. Use **MERGE** (merge commits), build
   concurrency **1**, minimum merge group size **1**, maximum **1** for
   the first pilot, and a check timeout exceeding the 25-minute policy job
   budget plus runner wait. Keep “only merge non-failing pull requests”
   enabled. Build concurrency counts groups, not individual runner jobs.
   Do not change rules for v6 or any other repo/base.
6. Only after that GitHub rule is saved, enable Hive:

   ```yaml
   auto_merge:
     native_merge_queue:
       enabled: true
       repos: [hivecommons/hive]
       bases: [hivecommons/hive#v5]
       skip_current_base_gate: true
       probe_ttl: 30m
   ```

   Hive probes actual queues per base; only v5 has a queue in this pilot.
   Other bases keep direct merge and coordinated refresh because they are
   outside the explicit base allow-list. Do **not** enable a v6 queue until
   its separate rollout is ready. Allow for probe-cache expiry
   (or a controlled owner restart) before diagnosing stale queue detection.
7. Enqueue one signed, reviewed PR through the App. Record its expected head,
   enqueuer, synthetic SHA, all seven group checks, merge method, resulting
   commit and issue closure **after actual merge**. Exercise an intentionally
   failing check, hold/review withdrawal, and the external updater on both
   v5 and a non-queue branch. A queued membership observation is not merge
   evidence. Expand concurrency/member count only after the controlled
   multi-member test validates every member head and earlier-member failure.

The safe initial method is **MERGE**, preserving existing commit authors and
their DCO trailers. A repository-wide squash preference does not override
the mixed-authorship guard. Keep existing `merge-method/merge` attribution
decisions; do not rewrite sign-offs or drop authors merely to make admission
green. A queue configured for SQUASH/REBASE can still park mixed-authorship
PRs at `stage/085-needs-decision`.

### One freshness owner

`pr-auto-update.sh` normally retains its push file-intersection and
shared-incident recovery behavior. Only an exact allow-listed repo/base
**and** a successful live `isMergeQueueEnabled` probe suppress author-head
updates, including incident recovery. This covers waiting PRs, not just
members already enqueued. A queue-read error in an opted-in repo stops the
writer rather than guessing “direct mode”. Outside that allow-list, or on
a positively detected non-queue base, behavior is unchanged.

The variable and Hive's explicit `skip_current_base_gate` must be activated
together as above. #15836 owns conventional direct-mode preparation;
GitHub owns opted-in queue freshness. Existing return-to-author repair
handles rejected/broken heads; there is no second fixer or merge engine.

## Diagnostics and rollback

- Inspect the PR's exact-head checks and the **group SHA** checks separately.
  A green member head does not establish green integration evidence.
  `native-queue-policy` logs the failing provider, provenance, membership,
  hold or review condition. An unknown/changed GitHub queue schema fails
  closed and needs investigation; it is not permission to bypass the gate.
- A group waiting on `DCO` or another old policy name indicates an incomplete
  binding migration. A missing build/test/security context indicates workflow
  trigger, permission, runner or checkout failure. Read the actual run;
  do not infer success from absence.
- A pending GitHub-hosted policy job can delay the queue independently of
  self-hosted build capacity. The policy's minimal read token must read
  queues, PRs/reviews, check runs and Actions provenance; permission failures
  stop validation. Workflow-file push permissions are also required to land
  this integration.
- To roll back, first disable new Hive admissions and remove/stop GitHub queue
  entries through the owner UI (disabling Hive alone does not stop entries
  already owned by GitHub). In one v5 rule edit restore the original eleven
  required check bindings and remove **Require merge queue**; preserve all
  other protections. Remove `HIVE_NATIVE_QUEUE_FRESHNESS`, restore
  `skip_current_base_gate: false`, and leave native admission off. The
  external updater resumes only after actual queue detection says false;
  clear/expire Hive's probe cache. Conventional coordinated refresh remains
  available. No history rewrite is needed.

Workflow-trigger diagnosis is not automatic in the Hive runtime.
See GitHub's [merge queue guide](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
and [GraphQL pull request reference](https://docs.github.com/en/graphql/reference/pulls).
