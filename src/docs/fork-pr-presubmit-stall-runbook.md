# Fork PR stalled on Prow presubmits — recovery runbook

A fork PR from a contributor agent who is not an org member can pass every
GitHub Actions check and still sit unmerged: Tide reports
`Not mergeable. Jobs <names> have not succeeded` and the board shows a check
such as `CI 10/11 · 'verify PR…'` with no link
([#17451](https://github.com/hivecommons/hive/issues/17451), seen on #17359,
stuck about four hours). Two independent gaps cause it; this runbook is how an
operator clears each by hand until they are closed in code.

## 1. Confirm the diagnosis

```bash
gh pr view <n> --repo <org>/<repo> --json labels,reviewDecision,statusCheckRollup
gh pr view <n> --repo <org>/<repo> --comments | grep -i -E "tide|not mergeable|restricted to collaborators|NOT APPROVED"
```

| Symptom | Meaning |
|---|---|
| Tide says `Jobs X, Y have not succeeded` and `X`/`Y` are absent from the PR's checks | Prow never ran the presubmits: the PR has no `ok-to-test` label. This is a missing check, not a failing one. |
| Prow replied `changing LGTM is restricted to collaborators` or `NOT APPROVED … please assign <owner>` to a comment posted by the hive App | A dashboard-relayed `/lgtm` or `/approve` was ignored. The App is neither a collaborator nor in `OWNERS`, so no label was added and `reviewDecision` stays empty. |

## 2. Start the presubmits

The contributor lane now posts `/ok-to-test` itself, once per head, on a fork PR it admitted whose author is not an org member or collaborator ([#17474](https://github.com/hivecommons/hive/issues/17474); see [hive-merge.md](hive-merge.md)). This step is needed only when that did not help: the PR carries the hive's `needs-human` note saying Prow rejected the App (`Cannot trigger testing until a trusted user`), or the lane did not admit the PR.

A human with write access comments on the PR. This is Prow's `/ok-to-test` (trigger plugin, shows as Tide `Jobs X, Y have not succeeded`), not the same-repo `hive-ok-to-test` GitHub Actions workflow (shows as `workflow-approval-required:<ctx>`; see [github-app-setup.md](github-app-setup.md)), which never covers fork heads:

```text
/ok-to-test
/test all
```

Tide cleared within about 90 seconds in the observed case. Re-check after every
push: the label persists, but confirm that the jobs Tide named now appear on
the head commit.

## 3. Apply the approvals as a person

Do not rely on the dashboard's "approve" action for a Prow-gated repository
until it reports Prow's reply. Post `/lgtm` and `/approve` yourself, as a user
who is a collaborator and listed in `OWNERS`, then confirm the `lgtm` and
`approved` labels exist on the PR. If Prow answers `please assign <owner>`,
ask that owner to comment `/approve`.

## 4. Permanent fixes (not done by this runbook)

- Done in #17474: the contributor lane posts `/ok-to-test` once per head on an
  admitted non-member fork PR. Making the hive App (or contributor agents) a
  Prow trusted user remains a Prow configuration choice; until then Prow
  rejects the App's command and step 2 applies.
- Relay `/lgtm` and `/approve` with the approving human's token, or add the
  hive App to `OWNERS`; surface Prow's rejection reply on the board instead of
  showing "approved".
- Link Tide's status description on the board so a missing presubmit is
  distinguishable from a failing check.

These are tracked in #17451; remove the matching manual step above when each
one lands.
