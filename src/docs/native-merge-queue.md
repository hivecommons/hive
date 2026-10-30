# Native GitHub merge queues

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
    skip_current_base_gate: false # default false — see "Current-base gate" below
    probe_ttl: 30m                # default 30m — see "API cost" below
```

| Key | Default | Meaning |
| --- | --- | --- |
| `auto_merge.native_merge_queue.enabled` | `false` | Turns the native-queue hand-off on. |
| `auto_merge.native_merge_queue.repos` | unset (every watched repo once enabled) | `owner/repo` allow-list, matched case-insensitively. Repos outside it keep the pre-queue behaviour. |
| `auto_merge.native_merge_queue.skip_current_base_gate` | `false` | Lets a queue-enabled base bypass the `auto_merge.require_current_base` gate and the `behind` update-branch. Off keeps both gates in force on queue bases. |
| `auto_merge.native_merge_queue.probe_ttl` | `30m` | How long one base branch's `isMergeQueueEnabled` answer is reused before it is re-read. |

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
triggers. Workflow-trigger diagnosis is not automatic in this implementation.
See GitHub's [merge queue guide](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
and [GraphQL pull request reference](https://docs.github.com/en/graphql/reference/pulls).
