# DCO provider unavailable on large forward merges

`native-queue-policy` requires the latest completed **DCO** check from App
**1861** to succeed on the exact current PR head. When that provider reports
that it could not retrieve the complete PR commit list, admission is blocked
with **verdict UNKNOWN**. The Actions job remains failed and its summary
identifies provider unavailability. This is an operator/provider investigation;
it does not request a source change from the PR author.

An ordinary negative DCO verdict still requires sign-off remediation. The
UNKNOWN classification applies only to the provider's explicit incomplete-list
diagnostic, after the App, head and completed status have been checked. A
changed or unrecognized provider message continues to fail closed as the
original check conclusion. No diagnostic text supplies passing evidence.

## Observed evidence and provider path

Investigation for [#16404](https://github.com/hivecommons/hive/issues/16404):

| Evidence | Observation |
| --- | --- |
| [Check run 125732155592](https://github.com/hivecommons/hive/runs/125732155592) | App 1861, head `1989d20aa8d0d92cce761786ac05825aef1d9d3a`, completed `failure`; summary explicitly says the DCO verdict is unknown because GraphQL fallback did not complete. |
| [Check run 126045650867](https://github.com/hivecommons/hive/runs/126045650867) | Same diagnostic on head `66143614dbed95c6239f225719f4ec7e374f947b`; #16378 had 305 commits when inspected on 2026-11-03. |
| [Provider source](https://github.com/dcoapp/app/blob/828cc0b29a141821ddecd674bca0d8e634234c18/index.js) | `listPullRequestCommits` escalates exactly 250 REST commits to `fetchCompletePullRequestCommits`. It pages GraphQL by 100, checks stable totals, uniqueness, cursor progress and completeness, then maps the records before running the DCO evaluator. |
| [Provider issue #327](https://github.com/dcoapp/app/issues/327) | Reports the same incomplete-list failure. |
| [Provider PR #339](https://github.com/dcoapp/app/pull/339) | Proposed repair reduces nested `parents(first: 100)` to `parents(first: 2)` and exposes the underlying GraphQL error. Open and unmerged when inspected on 2026-11-03; deployment is not established. |

250 is the REST endpoint's enumeration ceiling, not a Git ancestry limit.
The provider already attempts a full-history fallback. Its error summary hides
the actual exception and can report HTTP status `unknown` and request ID
`unavailable`, including for locally detected completeness/mapping errors.
Those fields alone do not prove an API permission, timeout or query-limit
failure. The nested parent selection is the upstream repair candidate; only
provider logs or a live query comparison can establish the cause for this
installation. A repository token's successful query would not prove that the
App installation token can perform the same query.

## Read-only full-history reproducer

From a checkout, with Node.js, `gh` and an existing `GH_TOKEN` that can read the
repository:

```sh
node .github/scripts/diagnose-dco-provider.cjs hivecommons hive 16378
node .github/scripts/diagnose-dco-provider.cjs hivecommons hive 16378 --parents=2
```

The default reproduces the provider's commit fields and 100-parent selection;
the second run compares the proposed smaller parent selection. Each reads the
REST list, then **all** GraphQL pages, verifies unique commit IDs against the
reported total and REST list, and checks head/base/count stability. The CLI
prints only enumeration metadata; it does not evaluate sign-offs, publish
checks/statuses, modify the PR, or certify DCO. It exits nonzero on incomplete
history, partial GraphQL errors, duplicate IDs or a moved PR. Capture both
outputs and errors with the head SHA for the provider operator. Do not compare
runs across different heads as if they were one experiment.

GraphQL queries use a read-only `query` over HTTP POST. Contributor environments
that disallow `gh api` POST cannot run this reproducer; use an operator's normal
read-capable environment. Do not circumvent that gate. The investigation above
verified public check evidence and provider source, not a successful live
full-history GraphQL run from such a restricted environment.

The companion test fixture has 251 unique commits with an unsigned final
commit beyond the REST ceiling. It verifies that the final record survives
enumeration, and that truncation, partial errors and changed history stay
UNKNOWN. Mocked tests do not establish that GitHub accepts the provider query
or that the hosted provider has deployed a fix.

## Operator recovery

1. Record the current PR head and the latest App-1861 DCO run, including its
   output, timestamps and check-run ID:

   ```sh
   GH_TOKEN="$GH_TOKEN" gh api repos/hivecommons/hive/pulls/16378 --jq '{head: .head.sha, base: .base.sha, commits}'
   # Replace HEAD_SHA with the exact head just read.
   GH_TOKEN="$GH_TOKEN" gh api 'repos/hivecommons/hive/commits/HEAD_SHA/check-runs?per_page=100' --paginate --jq '.check_runs[] | select(.name == "DCO") | {id, head_sha, app: .app.id, status, conclusion, output}'
   ```

2. Have the DCO App operator inspect logs for that delivery/time/head. The
   provider logs the original exception as `failed to evaluate DCO check`.
   Capture the underlying GraphQL errors, request ID, page/cursor and count, or
   mapping exception. Confirm installation access and the deployed provider
   revision. Track #327/#339 with that evidence; this repository cannot deploy
   a change to the hosted App. For a self-hosted provider, its operator must
   review and deploy a repair preserving complete enumeration and the existing
   DCO evaluator. Follow the provider's [administration guide](https://github.com/dcoapp/app/blob/828cc0b29a141821ddecd674bca0d8e634234c18/docs/administration.md).
3. After the cause is repaired, an authorized operator can use GitHub's **DCO
   check re-run** UI on the current PR head. The provider supports
   `check_run.rerequested` and resolves the PR again, rejecting superseded heads.
   This does not require granting Hive `checks:write`. Do not use **Set DCO to
   pass**, copied statuses, blind retry loops or a policy override.
4. Verify a fresh App-1861 DCO `success` on the still-current head. Then rerun
   **Native queue policy** through the Actions UI; it reads current evidence
   again. If either head or evidence changed, investigate that state instead.

Preserve the true merge ancestry of #16378. Splitting/rebasing the forward
merge, adding empty commits, accepting the lowercase prow `dco` status, or
weakening protection does not repair this provider failure. Until authoritative
current-head DCO success exists, the correct admission outcome remains blocked.
