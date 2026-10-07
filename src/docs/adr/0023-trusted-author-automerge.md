# ADR-0023: Hive may merge PRs whose author could merge them

Status: Accepted

## Context

Hive already has two auto-merge routes. The queued route requires a distinct
trusted merger to queue another author's PR, and the self-authored route lets
the App land its own CI-green PRs because Prow/tide cannot accept self-approval.
That left human-authored maintainer PRs in a gap: a maintainer can merge their
own green PR in GitHub, but Hive would not do it unless a second person added
`tide`'s `lgtm` and `approved` signals.

## Decision

Add an opt-in `auto_merge.trusted_authors` tier. A PR is eligible only when the
operator enabled the tier, the PR author holds the configured
`authorized_users` role (`merger` by default, or `owner`), and GitHub reports
repo write/maintain/admin permission unless the operator explicitly disables
that API check. GitHub permission errors fail closed. Fork PRs still require a
positive permission check so non-member forks never merge through this path.

The tier reuses the self-authored sweep's merge mechanics: required CI green,
mergeable state, no hold/exempt/excluded labels, no outstanding
`CHANGES_REQUESTED` review, expected-head-SHA pinning immediately before merge,
and the same merge method selection as `hive-merge`. Each merge records
`tier=trusted-author` and comments with the author's role, GitHub permission,
and green head SHA.

## Consequences

Hive can land green maintainer-authored PRs without waiting for a second human
approval signal, but only when the author already has the right to perform that
merge. The security argument is therefore no-escalation: Hive acts as an
audited executor for a maintainer's existing repository authority, not as a way
for arbitrary PR authors to gain merge power. Operators who do not enable the
block get byte-identical behavior.
