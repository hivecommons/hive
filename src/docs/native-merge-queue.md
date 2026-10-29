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

When a lane rejects an entry after a hold, withdrawn approval, changes request,
or stale approval, Hive rechecks the entry's enqueuer and withdraws it only if
it belongs to this App. Entries from a person or another App are preserved.
The queued sweep also withdraws this App's entries on paused repositories or
repositories with automerge disabled. Every enqueue and dequeue uses the
existing mutation boundary, including its write denials. Reconciliation is
periodic/event-driven, so a gate change can race GitHub's eventual merge.

Queue rules choose the merge method. Where Hive elected a merge commit to
preserve mixed authorship and DCO attribution, the queue must use `MERGE`;
otherwise Hive reports a queue-method failure rather than enqueueing.

Required workflows must subscribe to `merge_group` in addition to their PR
triggers. Workflow-trigger diagnosis is not automatic in this implementation.
See GitHub's [merge queue guide](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)
and [GraphQL pull request reference](https://docs.github.com/en/graphql/reference/pulls).
