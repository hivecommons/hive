# Native GitHub merge queues

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
untrusted or stale queue approval, changes requested, or a draft or closed
PR — Hive rechecks the entry's enqueuer and withdraws it only if it belongs to
this App (a `Bot` enqueuer whose login matches the App's slug). Every other
reason, including transient refusals such as `behind`, `not-mergeable`, pending
checks, a moved head, API read errors, author-routing skips and Hive's own
native-queue outcomes, leaves the entry for GitHub's queue to settle. Entries
from a person or another App are preserved.
The queued sweep also withdraws this App's entries on paused repositories or
repositories with automerge disabled. Every enqueue and dequeue uses the
existing mutation boundary, including its write denials. Reconciliation is
periodic/event-driven, so a gate change can race GitHub's eventual merge.

A queue-required base reports `mergeable_state: blocked` (or `behind`) until
the PR merges through the queue, so on a queue-enabled base the queued lane
does not treat those states as `not-mergeable`. For the same reason the lanes
do not update a PR's branch on a queue-enabled base: neither the `behind`
update nor the `auto_merge.require_current_base` gate (on by default) runs
there, since the merge group already tests the PR against the current base
and a pushed head would drop the PR from the queue. A failed queue read keeps
the non-queue behavior.

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
