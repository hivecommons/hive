# GitHub write surface

Agents ask the hive to write to GitHub by dropping a JSON request file into a
relay queue. The hive checks the request, performs the write with the App
installation token, audits it, and leaves a `.result.json` next to the request.
These relays are the fixed, allowlisted and audited write surface proposed in
[#9587](https://github.com/hivecommons/hive/issues/9587). This page lists them,
along with the other places the hive writes to GitHub by itself.

Phase 1 (this page) covers:

- the inventory below;
- typed `repo` and `target` fields on the audit entries for these writes;
- a per-lane allowlist enforced by the relays;
- redaction of credential material in audited arguments and results.

Agents still have their direct write paths (`gh`, the API through the proxy,
`git push`). Removing those is a later phase.

## Relay operations

Each relay authorizes a request in this order. It stops at the first refusal.

1. File-UID forge check and ACMM mode gate (the authorizer).
2. Lane write allowlist (`write_surface.allowlist`, below).
3. Repo pause, then repo scope.
4. The relay's own content gates.
5. The GitHub call.

| Operation (`op`) | Entry point | Agent script | Audit action on success | Audited? | Typed `repo` / `target`? |
| --- | --- | --- | --- | :---: | :---: |
| `open_pr` | `pr_request_watcher.go` (`/var/run/hive-metrics/pr-requests`) | `hive-open-pr` | `agent_pr_created` | yes | yes / PR number |
| `create_issue` | `issue_request_watcher.go` kind `issue` (`.../issue-requests`) | `hive-open-issue` | `agent_issue_created` (or `agent_issue_rejected_duplicate`) | yes | yes / issue number |
| `comment` | `issue_request_watcher.go` kind `comment` | `hive-open-issue comment` | `agent_comment_created` | yes | yes / issue or PR number |
| `claim` | `issue_request_watcher.go` kind `claim` | `hive-open-issue claim` | `agent_issue_claimed` | yes | yes / issue number |
| `close_issue` | `issue_request_watcher.go` kind `close` | `hive-open-issue close` | `agent_issue_closed` (`pr_closed` when the number is a PR) | yes | yes / issue or PR number |
| `review` | `review_request_watcher.go` events `approve`, `request_changes`, `comment`, `record_verdict`, and thread replies (`.../review-requests`) | `hive-review` | `agent_pr_reviewed` | yes | yes / PR number |
| `resolve_thread` | `review_request_watcher.go` event `resolve_thread` | `hive-review` | `agent_pr_reviewed` (`state=thread_resolved`) | yes | yes / PR number |
| `merge_pr` | `merge_request_watcher.go` (`.../merge-requests`) | `hive-merge` | `pr_merged` (`path=relay`, via `MergePR`) | yes | yes / PR number |

Side effects performed on the same path are covered by the parent operation's
allowlist entry. For example, `open_pr` can also apply the `hold` label, post
the level-hold notice, and re-author the branch as a signed commit.

## Hive-internal writes (not agent requests)

The hive also writes to GitHub on its own schedule, with no agent request
behind the write. These writes are not subject to the lane allowlist. They are
listed so the surface is complete.

| Write | Entry point | Audited? | Typed `repo` / `target`? |
| --- | --- | :---: | :---: |
| Pinned advisory issue, advisory comments | `advisory.go` | yes (`hive_issue_created`, `advisory_commented`) | yes |
| Auto-merge self-approval | `client.go` (`QueuePRAutoMerge`) | yes (`agent_pr_reviewed`, agent `governor`) | yes |
| Auto-merge sweep merges | `pkg/github/automerge/automerge_sweep.go` | yes (`pr_merged`, `automerge-sweep-merged`) | yes |
| Attribution trailer reconcile | `attribution.go` (`ReconcilePRAttribution`) | yes (`pr_attribution_reconciled`) | yes |
| Task-list sweep closes | `task_list_sweep.go` | yes (`task-list-sweep-closed`) | yes |
| Supersession sweep | `pr_supersession_sweep.go` | yes (`supersession-sweep-*`) | yes |
| Duplicate-PR sweep suggestions | `duplicate_sweep.go` | yes (`duplicate-sweep-suggested`) | yes (target = surviving PR) |
| Hold-label migration | `hive_hold_migration.go` | no (migration report file) | no |
| Signed-commit reconcile (branch rewrite, explanatory comment) | `pr_signed_reconcile.go`, `pr_request_signed.go` | no (hive log only) | no |
| Review backlog issues | `review_backlog.go` | no (hive log only) | no |
| Human-decision label | `human_decision_label.go` | no | no |
| Recommendations issue | `recommendations.go` | no | no |
| Fleet report issue | `fleet_report.go` | no | no |

Rows marked "no" are follow-ups for later phases.

## Lane allowlist

`write_surface.allowlist` maps an agent (lane) name to the operations it may
ask the relays to perform:

```yaml
write_surface:
  allowlist:
    scanner: [create_issue, comment, claim]
    reviewer: [review, resolve_thread, comment]
    outreach: [open_pr, comment]
```

- **Unconfigured means unchanged.** With no `write_surface` block, or no entry
  for an agent, every operation that agent can perform today stays allowed.
  Listing one lane does not affect the others.
- **An entry narrows.** A listed agent may perform only the listed operations.
  An empty list (`muted: []`) allows nothing. `"*"` allows everything, so a lane
  can be listed without being narrowed.
- **Replicas** (`scanner-2`) with no entry of their own use their base agent's
  entry.
- Names are matched without regard to case or surrounding spaces. An unknown
  name is logged as a warning at boot (`write surface allowlist`). A misspelt
  name never grants anything.
- The allowlist is keyed on the agent name only after the authorizer has proved
  that name from the request file's owner UID. An agent cannot borrow another
  lane's allowlist by claiming its name.
- The allowlist cannot widen anything. The ACMM mode gate, repo pause and repo
  scope still apply to a listed operation.

A refused request is handled like every other policy denial:

- the request is renamed `.denied`;
- its `.result.json` carries `"ok": false` and an `error` naming the agent, the
  operation and the config key;
- an `agent_write_refused` audit entry is written with `op=` and
  `outcome=refused`.

Refusals are not counted as output activity.

## Audit fields

Audit entries for these writes carry typed fields next to the legacy `detail`
string (see [audit log format](audit-log.md)):

| JSON key | Meaning |
| --- | --- |
| `repo` | Repository written to, as the request named it (bare or `owner/repo`). |
| `target` | Issue or PR number written to. Omitted when there is none, for example a refused `open_pr`. |

The `repo=` and `number=` pairs stay in `detail`, so existing parsers keep
working. The activity collector and per-repo cost attribution
([#4836](https://github.com/hivecommons/hive/issues/4836)) read the typed field
first. They fall back to `detail` for entries written before the field existed.

## Credential redaction

Audit details include values supplied by agents, such as override reasons,
repository names and URLs. Before any audit sink sees an entry, the detail and
the typed `repo` are masked with `[REDACTED]` for:

- GitHub tokens (`ghs_`, `ghp_`, `gho_`, `ghu_`, `ghr_`, `github_pat_`);
- JWTs and AWS access keys;
- private-key blocks;
- `Bearer` values;
- the value of any `Authorization` header (`token`, `Basic`, or bare).

The masking reuses `pkg/logscrub`. A test in `pkg/github/write_surface_test.go`
feeds each of these shapes through every audited slot and asserts that none of
them survives.
