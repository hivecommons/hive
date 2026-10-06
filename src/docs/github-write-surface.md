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
- a per-lane opt-in (`write_surface.enforce`) that makes the relays the only
  write path for a lane by refusing its direct writes at the GitHub proxy and,
  for the same lanes, in the agent sandbox's `gh` wrapper;
- redaction of credential material in audited arguments and results;
- per-lane mention sanitizing of relay-posted bodies
  (`write_surface.neutralize_mentions`).

By default agents still have their direct write paths (`gh`, the API through
the proxy, `git push`). An operator removes them lane by lane; see
[Enforcing the write surface](#enforcing-the-write-surface).

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
| `label` | `issue_request_watcher.go` kind `label` (`.../issue-requests`) | `hive-open-issue label` (also reached by `gh issue edit --add-label`/`--remove-label` and `gh pr edit --add-label`/`--remove-label`, which the `gh` wrapper translates into this relay) | `agent_label_applied` | yes | yes / issue or PR number |
| `request_review` | `issue_request_watcher.go` kind `request_review` (`.../issue-requests`) | `hive-open-issue request-review` (also reached by `gh pr edit --add-reviewer`, which the `gh` wrapper translates into this relay) | `agent_review_requested` | yes | yes / PR number |
| `review` | `review_request_watcher.go` events `approve`, `request_changes`, `comment`, `record_verdict`, and thread replies (`.../review-requests`) | `hive-review` | `agent_pr_reviewed` | yes | yes / PR number |
| `resolve_thread` | `review_request_watcher.go` event `resolve_thread` | `hive-review` | `agent_pr_reviewed` (`state=thread_resolved`) | yes | yes / PR number |
| `merge_pr` | `merge_request_watcher.go` (`.../merge-requests`) | `hive-merge` | `pr_merged` (`path=relay`, via `MergePR`) | yes | yes / PR number |
| `push_branch` | `push_branch_request_watcher.go` (`.../push-requests`) | `hive-push-branch` | `agent_branch_pushed` (branch and commit in the detail) | yes | yes / none (a branch push has no number) |

Side effects performed on the same path are covered by the parent operation's
allowlist entry. For example, `open_pr` can also apply the `hold` label, post
the level-hold notice, and re-author the branch as a signed commit.

### `review` is COMMENT-only on contributor pull requests

The hive reviews every pull request it is pointed at, but it only adjudicates
its own. When the PR's author is not one of this hive's accounts
(`project.ai_author` or the App bot login), the `review` relay rewrites an
`approve` or `request_changes` request into a `COMMENT` before it reaches
GitHub, and says so in the request's `.result.json` `note`
(`review_contributor_guard.go`, hivecommons/hive#9590). An `APPROVE` or
`REQUEST_CHANGES` is a repository verdict — it satisfies branch-protection
approval counts and gates merge queues — and belongs to the humans who own the
repository. A PR whose author cannot be read is treated as a contributor's: the
relay never resolves "we could not tell" into approving someone else's work.
There is no setting to turn this off; `review.all_authors` widens what is
reviewed, never what may be approved.

### `label` refuses hive-controlled labels

The `label` operation adds and removes plain, descriptive labels
(`bug`, `area/proxy`, …) on an existing issue or PR. It refuses, in both
directions and before any GitHub call, the labels the hive itself reads back
as fact (`issue_request_label.go`):

- the merge-queue label under its configured name (`lgtm` by default) — it
  queues a merge;
- the hold labels (`hold`, `on-hold`, `hold/review`) — they block one;
- everything in the `hive/` namespace, including `hive/claimed-by-<agent>`
  (use `claim`), `hive/covered-by-pr`, `hive/likely-done` and
  `hive/verified-open`;
- the labels that record a person's decision or an escalation:
  `approved-direction`, `design-approved`, `needs-human`, `needs-decision`,
  `blocked`.

One reserved label refuses the whole request — nothing in it is applied — so a
batch can never partially land. The refusal is a policy denial like any other:
`.denied`, a result file naming the labels, and an audit entry. A lane
allowlist can narrow `label` further but can never widen it to these.

### `push_branch` publishes topic branches only

The `push_branch` operation pushes a branch from the agent's own checkout to
GitHub with the App token (`hive-push-branch --repo <owner/repo> [--branch
<name>] [--dir <path>] [--force-with-lease]`). It is the audited stand-in for
a direct `git push`, gated by the same CanPush ACMM check, and it is what
keeps pushes working for a lane whose direct sandbox writes are refused. Two
gates are its own:

- the checkout it pushes from must be **owned by the requesting agent's UID**
  (when per-agent UIDs are in play) — an agent can only publish its own
  working tree, never a peer's or the hive's;
- the repository's **default branch is refused**, whatever the allowlist
  says: agents propose changes through `open_pr` and land them through
  `merge_pr`; a push straight to the default branch would bypass review and
  the merge relay's CI gate in one move.

A rework push uses `--force-with-lease`; there is deliberately no plain
`--force`.

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
| Reporter-trust wait notice/comment and owned label cleanup | `client.go` / `reporter_trust_wait.go` (`fetchIssues` reporter-trust reject/admit paths) | yes (`reporter_trust_wait_noticed`, `reporter_trust_wait_cleared`; label adds also record `hive_label_applied` with `reason=reporter_trust_wait`) | yes / issue number |

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
    scanner: [create_issue, comment, claim, label]
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

## Enforcing the write surface

`write_surface.enforce` lists the lanes whose **direct** GitHub writes from the
agent sandbox are refused by the GitHub proxy
([#9772](https://github.com/hivecommons/hive/issues/9772)). For those lanes the
relays above are the only way to write:

```yaml
write_surface:
  enforce: [scanner, reviewer]
  allowlist:
    scanner: [create_issue, comment, claim, label]
```

- **Default off.** With no `enforce` list nothing changes: every lane keeps
  its direct access, and an ACMM L6 hive stays fully autonomous. Enforcement
  starts only when an operator lists a lane, at any ACMM level.
- **Per lane.** Listing one lane does not affect the others. A replica
  (`scanner-2`) follows its base agent; `"*"` enforces every lane. Names are
  matched without regard to case or surrounding spaces. The proxy reads the
  list live from config, so a change takes effect on the next request.
- **What is refused** for an enforced lane, whatever its ACMM mode:
  - every REST write (`POST`, `PUT`, `PATCH`, `DELETE`) to a GitHub host;
  - every GraphQL mutation (a GraphQL body that cannot be parsed is refused
    too);
  - `git push`, at its first round trip
    (`GET .../info/refs?service=git-receive-pack`) and at
    `.../git-receive-pack`. To make this possible the proxy intercepts
    `github.com` (and registered GHE hosts) for an enforced lane, not only
    `api.github.com`.
- **What still works:** every read, `git clone`/`git fetch`
  (`git-upload-pack`), GraphQL queries, the CLI device-flow login, and the
  hive's own control-plane traffic.
- **Order.** Repo pause, agent repo scope and the auto-merge policy are checked
  first, so their more specific reasons win when they also apply. The ACMM mode
  gate is not consulted for a refused request: enforcement refuses a write the
  mode would have allowed.
- **The relays are unaffected.** They run in the hive with the App token, not
  in the sandbox, so an enforced lane still pushes with `hive-push-branch` and
  opens PRs with `hive-open-pr`, subject to its `allowlist` entry. List
  `push_branch` in an enforced lane's allowlist entry (or leave it
  unrestricted) if it needs to publish branches.

A refused request gets a `403` whose body names `write_surface.enforce` and the
relay to use instead, counts as a proxy violation, and is audited as
`agent_write_refused` with the typed `repo` (when the path names one) and
`via=proxy`, `kind=` (`rest`, `graphql` or `git_push`), `method=`, `path=`
and `outcome=refused` in the detail. `via=proxy` tells it apart from a relay's
allowlist refusal, which carries `op=`. The path is redacted like every other
audited value.

`enforce` is set in `hive.yaml`; the dashboard's Write Surface editor changes
only the allowlist and leaves `enforce` as it is.

### The same refusal inside the sandbox

The proxy refusal covers traffic that goes **through** the proxy. A sandbox
whose forced egress is off can reach GitHub without it, so the `gh` wrapper
makes the same decision one layer earlier, before the write is sent:

- the hive publishes the resolved enforced lanes to
  `/var/run/hive-metrics/write-surface-enforce.json` at boot and on every
  config reload (`PublishWriteSurfaceEnforce`). "Resolved" means each
  configured agent and replica the operator's list enforces, so the sandbox
  compares a name instead of re-deriving replica inheritance; a list
  containing `"*"` is published as `"*"`;
- for a listed lane the wrapper refuses `gh api` with any method other than
  `GET`, and the write verbs (`issue`/`pr` `create`, `edit`, `comment`,
  `close`, `reopen`, `develop`, `merge`, `review`, `ready`, and
  `label create`) on the paths where they would reach GitHub directly. The
  refusal names the relay that performs the same write under audit;
- **the relay redirects are unaffected**: `gh issue comment`, `gh issue close`,
  `gh pr review`, `gh pr create` and a pure `gh issue edit --add-label` are
  already translated into relay requests before this gate, so an enforced lane
  keeps working through them. Only the direct fall-through is refused.
- **every read still works**, including `gh issue view`, `gh pr list` and
  `gh api` GETs.

The file is written by the hive into a directory no agent UID may write, and
the path is a constant in the wrapper for the same reason the contributor-mode
marker is: an environment-selected path would let an enforced agent point the
check at a file it controls. The list is parsed with bash builtins only —
every external command is reachable through the agent's own `PATH`. An absent,
unreadable or unrecognized file enforces **nothing**, which is the documented
default and leaves the proxy refusal as the enforcement the feature rests on.

## Mention sanitizing

`write_surface.neutralize_mentions` lists the lanes whose relay-posted bodies
have every GitHub `@mention` rewritten so it notifies no one:

```yaml
write_surface:
  neutralize_mentions: [scanner, outreach]
```

- **Default off.** With no list, every body is posted as the agent wrote it.
  Some prompts deliberately `@`-mention a person (the reviewer names the PR
  author so the one person who can act is notified), so sanitizing is opted
  into per lane rather than applied everywhere.
- **Per lane.** A replica follows its base agent; `"*"` covers every lane.
  Names are matched without regard to case or surrounding spaces, and the list
  is read live from config on every relay request.
- **What is rewritten:** the agent-supplied body of `comment`, `create_issue`,
  `open_pr`, `review` and review-thread replies. The rewrite is
  `advisory.NeutralizeMentions`, the same sanitizer the advisory digest,
  recommendations and duplicate-sweep posts use: outside code `@user` becomes
  `` `user` ``; inside inline code and fenced blocks only the `@` is dropped.
  Email addresses, URLs and `#123` references are left alone.
- **What is not:** the hive's attribution trailer and confidence line, which
  are appended after the rewrite, and titles, which GitHub does not scan for
  mentions.

`neutralize_mentions` is set in `hive.yaml`; the dashboard's Write Surface
editor leaves it as it is.

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
parsers keep working. The activity collector, per-repo cost attribution
([#4836](https://github.com/hivecommons/hive/issues/4836)) and the lifecycle
timeline (`recordLifecycleFromAudit`) read the typed fields first. They fall
back to `detail` for entries written before the fields existed.

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

- **Removing direct write access by default.** The per-lane opt-in has landed
  (hivecommons/hive#9772, [above](#enforcing-the-write-surface)): a lane
  listed under `write_surface.enforce` has its direct REST writes, GraphQL
  mutations and `git push` refused at the proxy, so the relays are its only
  write path. What remains is turning enforcement on without an operator
  listing each lane. That would change what every existing hive's agents can
  do, so it needs operator sign-off, and it must never apply to an L6 hive by
  default.
- **Enforcement outside the proxy.** The refusal now lives in two places: the
  GitHub proxy and, for the same lanes, the `gh` wrapper inside the sandbox
  ([above](#the-same-refusal-inside-the-sandbox)), so a sandbox that can reach
  GitHub without the proxy (forced egress disabled) no longer writes through
  `gh` unchecked. What remains uncovered is a tool that is neither `gh` nor
  proxied — a raw `curl` with a token of its own, or an MCP server talking
  straight to `api.github.com`. Closing that needs the sandbox to hold no
  GitHub-capable credential at all, which is the "removing direct write access
  by default" item above.
- **Routing the `gh` wrapper's label and reviewer edits through the relays**
  has landed (hivecommons/hive#9773): the wrapper translates a pure
  `gh issue edit --add-label`/`--remove-label` or `gh pr edit --add-reviewer`
  into a `label` / `request_review` relay request; edits the relays cannot
  express still fall through to the direct path. See the inventory above.
