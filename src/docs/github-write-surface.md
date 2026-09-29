# GitHub write surface

Agents ask the hive to write to GitHub by dropping a JSON request file into a
relay queue. The hive checks the request, performs the write with the App
installation token, audits it, and leaves a `.result.json` next to the request.
These relays are the fixed, allowlisted and audited write surface proposed in
[#9587](https://github.com/hivecommons/hive/issues/9587). This page lists them,
along with the other places the hive writes to GitHub by itself.

This page covers:

- the inventory below;
- typed `repo` and `target` fields on the audit entries for these writes,
  passed explicitly by every write site;
- audit entries for the hive's own writes, not only the agent relays;
- a per-lane allowlist enforced by the relays, editable from the dashboard;
- redaction of credential material in audited arguments and results.

Agents still have their direct write paths (`gh`, the API through the proxy,
`git push`). Removing those is a later phase; see [Not yet done](#not-yet-done).

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
behind the write. These writes are not subject to the lane allowlist. Every
one of them is audited with typed `repo` and `target` fields and redacted
detail, the same as the relay writes. The entries for the rows below the
sweeps are recorded under the `governor` agent, and only when the write
landed: a skipped, unchanged or failed write produced nothing on GitHub and
writes no entry.

| Write | Entry point | Audited? | Typed `repo` / `target`? |
| --- | --- | :---: | :---: |
| Pinned advisory issue, advisory comments | `advisory.go` | yes (`hive_issue_created`, `advisory_commented`) | yes |
| Auto-merge self-approval | `client.go` (`QueuePRAutoMerge`) | yes (`agent_pr_reviewed`, agent `governor`) | yes |
| Auto-merge sweep merges | `pkg/github/automerge/automerge_sweep.go` | yes (`pr_merged`, `automerge-sweep-merged`) | yes |
| Attribution trailer reconcile | `attribution.go` (`ReconcilePRAttribution`) | yes (`pr_attribution_reconciled`) | yes |
| Task-list sweep closes | `task_list_sweep.go` | yes (`task-list-sweep-closed`) | yes |
| Supersession sweep | `pr_supersession_sweep.go` | yes (`supersession-sweep-*`) | yes |
| Duplicate-PR sweep suggestions | `duplicate_sweep.go` | yes (`duplicate-sweep-suggested`) | yes (target = surviving PR) |
| Hold-label migration (one-time) | `hive_hold_migration.go` | yes (`hold_migration_label_added`, one per item labeled; the migration report file is still written) | yes |
| Signed-commit reconcile: branch rewrite | `pr_signed_reconcile.go` | yes (`signed_commit_reauthored`, with `branch`, `base`, `commit`, `replaced_commits`) | yes / PR number |
| Signed-commit reconcile: "cannot sign" comment | `pr_signed_reconcile.go` | yes (`signed_commit_skip_noted`, with `reason`) | yes / PR number |
| Review backlog issues | `review_backlog.go` | yes (`review_backlog_issue_filed`, one per issue, with `pr`, `perspective`, `reused`) | yes / backlog issue number |
| Review backlog summary comment | `review_backlog.go` | yes (`review_backlog_summary_posted`) | yes / PR number |
| Human-decision and review-priority labels | `human_decision_label.go` | yes (`hive_label_applied`, with `label`) | yes / PR number |
| Recommendations issue | `recommendations.go` | yes (`recommendations_posted`, `outcome=created` or `updated`) | yes / issue number |
| Fleet report issue | `fleet_report.go` | yes (`fleet_report_posted`, `outcome=created` or `commented`) | yes / issue number |
| Fleet report recovery | `fleet_report.go` | yes (`fleet_report_recovered`, with `closed`) | yes / issue number |

The open-time signed rewrite in `pr_request_signed.go` is part of `open_pr`
and is covered by that operation's `agent_pr_created` entry. Creating a
missing label on a repository (the hold label, `from-review`) is a side
effect of the labeled write and is not audited on its own.

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

### Editing it from the dashboard

The allowlist can be edited at runtime in **Settings > Security > Write
Surface**. The editor is plain text, one lane per line:

```text
scanner: create_issue, comment, claim
reviewer: review, resolve_thread, comment
muted:
```

`muted:` with nothing after it allows that lane nothing. An empty editor clears
the allowlist, which restricts nothing. Saving takes effect on the next relay
request, with no restart, and is persisted like every other dashboard setting.

The editor is backed by an owner-only API:

- `GET /api/config/write-surface` returns `allowlist`, the known `ops`, and
  `warnings` (an unknown operation from a hand-edited `hive.yaml`, or a lane
  that names no configured agent or replica).
- `PUT /api/config/write-surface` takes `{"allowlist": {...}}` and replaces the
  whole allowlist. `{"allowlist": {}}` clears it. A missing `allowlist` key, an
  unknown operation, or a lane name with anything other than letters, digits,
  `.`, `_` and `-` is refused with `400` and nothing changes. Operations are
  stored lower-cased, de-duplicated and in the order listed above; a list
  containing `"*"` is stored as `["*"]`.

Each accepted change is audited as `config_write_surface` with the number of
listed lanes and their names. The editor renders every name as text.

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

Every write site passes the repository and number it wrote to directly
(`recordWriteAudit` with a `WriteTarget`), so the typed fields are the values
the write used, not a re-parse of `detail`. The `repo=` and `number=` pairs
are still written first in `detail`, in the same order as before, so existing
parsers keep working. The activity collector and per-repo cost attribution
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

## Not yet done

These are deliberately out of scope so far. They change what an agent can do,
so they need operator sign-off first. ACMM L6 is full autonomy by design: none
of them may reduce what an L6 hive can do by default.

- **Removing direct write access from the agent sandbox.** Agents can still
  run `gh`, call the API through the proxy, and `git push`. A likely shape: the
  proxy refuses GitHub write methods for a lane that opted in (a per-lane
  `write_surface.enforce` flag, default off), and the relays become the only
  write path for that lane. Unlisted lanes and L6 hives keep direct access
  unless the operator turns enforcement on. `git push` needs its own relay
  first (below), or pushes would simply stop.
- **New operations with no relay yet:** `push_branch`, a standalone `label`,
  and `request_review`. Each would be a new relay (request file, file-UID
  authorizer, allowlist check, audit), following the existing four. A hive with
  no allowlist would allow them, like every other operation.
- **Lifecycle timeline reader.** `recordLifecycleFromAudit` still reads
  `repo=`/`number=` from `detail`. It works, because the pairs are still
  written, but it could take the typed fields directly.
