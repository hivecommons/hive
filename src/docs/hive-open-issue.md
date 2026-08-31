# `hive-open-issue` — create an issue, comment, or claim as the App bot

`bin/hive-open-issue.sh` is how an agent creates an issue, posts a comment, or
claims an issue. Agents call it **instead of `gh issue create` /
`gh issue comment`**.

It does not perform the GitHub write itself. It writes a request file that the
hive's issue-request watcher executes with the App installation token —
server-side, retried with backoff, and deduplicated by exact open-issue title.

## Why it exists

The direct path rode the agent's own shell tool, and that path lost work
silently. Root-caused live on 2026-08-21 on a hosted hive: the sec-check
agent's `gh issue create` timed out mid-flight — repeatedly — and the finding
it was recording survived only as a bead, not as the intended GitHub issue.
One GHE secondary-rate-limit stall, a network blip, or a mangled multiline
command was enough to lose a finding with no visible failure.

Routing through this script makes the agent's job "record the request" — a
local file write that takes milliseconds and cannot fail on network — and the
hive owns delivery: retried, backed off, and immune to the agent's own shell
timing out.

**Authorization + forge resistance.** The script runs **as the agent**, in
that agent's tmux session under that agent's UID, so the request file it
writes is owned by that UID. The watcher re-derives the requesting agent from
the **file's owner**, not from anything written inside the file. The watcher
enforces the same per-agent mode gate (`CanCreateIssues`, mode ≥
`ISSUES_ONLY`) and UID forge-resistance as the direct `gh` path would need —
this shim **adds no privilege**. The same `CanCreateIssues` gate covers all
three kinds (issue, comment, claim): commenting and claiming an issue are
both issue-writes under the same tier.

## Usage

Three shapes, selected by an optional leading positional keyword
(`comment` or `claim`; the default with no keyword is `issue`):

```sh
hive-open-issue --repo <owner/repo> --title "<t>" [--body "<b>"|--body-file f] [--label a,b]
hive-open-issue comment --repo <owner/repo> <number|url> --body "<b>"
hive-open-issue claim   --repo <owner/repo> <number|url>
```

| Flag | Aliases | Applies to | Notes |
| --- | --- | --- | --- |
| `--repo` | `-R` | all | required |
| `--title` | `-t` | issue | required for `issue` |
| `--body` | `-b` | issue, comment | required for `issue` and `comment`; not used by `claim` |
| `--body-file` | `-F` | issue, comment | reads body from a file; `-` reads stdin |
| `--label` | `-l` | issue | repeatable |
| `--number` | — | comment, claim | the issue/PR number; a bare positional number or a `.../issues/N` or `.../pull/N` URL is also accepted |

Both `--flag value` and `--flag=value` forms work. Flags `gh` accepts but this
path does not need — `--assignee`/`-a`, `--milestone`/`-m`, `--project`/`-p`,
`--template`/`-T`, `--web`/`-w`, `--editor`/`-e` — are **accepted and
ignored** (the value-taking ones correctly consume their following argument so
it isn't misread as the issue number).

### `issue` (default)

`--repo`, `--title`, and `--body` are all required — an empty body is treated
as an agent bug, not a valid issue, and the script exits `2` rather than
letting the watcher quarantine it later. This is a deliberate, pinned contract
(`bin/test_hive_open_issue.sh`), not an oversight. `--label` may be repeated;
labels are always sent as a JSON array (empty if none given).

### `comment`

`--repo`, a number or URL, and `--body` are all required, or the script exits
`2`.

### `claim`

`--repo` and a number or URL are required; no body or title needed. A claim
records that this agent is starting work on an issue. Because App bots cannot
be GitHub assignees, the watcher applies a `hive/claimed-by-<agent>` label
instead — the visible, auditable ownership signal — and audits it as
`agent_issue_claimed`.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The
issue/comment/claim has not happened yet at that point.** It executes on the
next watcher tick (polling every 10 seconds); poll the `.result.json` written
next to the request file for the resulting number/URL.

## Retries and what happens when a request can't be fulfilled

Unlike the merge and PR-open watchers' fixed attempt caps, the issue-request
watcher backs off **exponentially per request**: starting at 30 seconds and
doubling up to a 15-minute ceiling. A request that still hasn't succeeded
after **24 hours** is given up on and quarantined (renamed `.failed`), so a
persistently failing request cannot hammer the forge indefinitely and the
queue directory cannot grow without bound.

A request that is structurally invalid — missing required fields for its
kind, or an unrecognized kind — is rejected before authorization or any API
call and quarantined immediately (renamed `.bad`); it is never retried, since
no amount of retrying changes a shape that can never succeed. Likewise,
invalid JSON in the request file is quarantined `.bad`. An authorization
denial (forge-resistance failure or `CanCreateIssues` gate failure) is
quarantined `.denied` immediately, also without retry — policy won't change
on the next tick.

### Idempotency

Issue creation is deduplicated by exact (whitespace-trimmed) title against
open issues in the target repo (scanning up to the 3 most recent pages). If a
matching open issue already exists, the watcher reuses it instead of creating
a duplicate — this is what makes the retry loop safe: a create that actually
succeeded server-side but crashed before the request file was consumed (or an
agent-side "timed out but maybe it worked" ambiguity) never produces a second
issue. The result file's `already_existed` field reports which case
happened.

## Where things live

| Path | What |
| --- | --- |
| `/var/run/hive-metrics/issue-requests` | request files the watcher consumes |
| `/var/run/hive/uid-map.json` | UID → agent-name map, used for a nicer log line |

The UID map is **informational only** here. The watcher re-derives ownership
from the file's UID regardless.

## Related

- [`hive-open-pr`](hive-open-pr.md) — the equivalent relay for opening a PR;
  same request-file mechanism and authorship model
- [`hive-merge`](hive-merge.md) — the equivalent relay for merging a PR
- [Security threat model](security-threat-model.md) — forge resistance and the
  UID-ownership anchor
- [Agent configuration](agent-configuration.md) — ACMM levels and the
  `CanCreateIssues` gate that governs whether an agent may create issues,
  comment, or claim at all
- [Audit log](audit-log.md) — issue creation, comments, and claims are
  recorded there with the requesting agent
