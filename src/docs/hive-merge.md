# `hive-merge` — merge a PR as the App bot

`bin/hive-merge.sh` is how an agent merges a pull request. Agents call it
**instead of the GitHub MCP `merge_pull_request` tool** (or a GraphQL
`mergePullRequest` mutation). GitHub rejects that mutation for App
installation tokens with "Resource not accessible by integration" even when
the token holds `contents:write` + `pull_requests:write`. The hive merges the
PR over REST with the App token instead, which succeeds — so the merge is
authored by the App bot (`<slug>[bot]`) and uses the transport that actually
works for an App token.

It does not merge the PR itself. It writes a request file that the hive's
merge-request watcher picks up and acts on.

## Why it exists

Two reasons.

**Transport.** The MCP/GraphQL merge path is rejected outright for App
installation tokens. Routing through the hive's REST-based merge path is not
optional convenience — it's the only path that works.

**Authorization + forge resistance.** The script runs **as the agent**, in
that agent's tmux session under that agent's UID, so the request file it
writes is owned by that UID. The watcher re-derives the requesting agent from
the **file's owner**, not from anything written inside the file. `hive-merge`
**adds no privilege**: the watcher enforces the same per-agent forge-resistance
check and a `CanMerge` ACMM gate (`AuthorizeMerge`) that a direct merge would
need — an agent whose mode allows opening PRs but not merging them still
cannot merge through this path.

On top of `AuthorizeMerge`, the watcher's authorizer is wrapped with a second,
target-specific check (the F4 target-binding, CWE-863) before any merge is
attempted:

- the request must carry a non-empty `expect_sha` — an empty expected SHA
  would mean "merge whatever HEAD is now," which is a TOCTOU hole: a commit
  pushed after the PR was judged eligible could get merged unseen;
- the `(repo, number)` pair must currently appear in the governor's
  merge-eligible list **at that exact head SHA** — so an agent cannot request
  landing an arbitrary reachable PR (including its own) merely because its
  checks happen to be green; it must be a PR the hive's governor already
  deemed eligible, at the commit it reviewed.

Both the agent/UID/`CanMerge` check and this target-binding check must pass
before the merge is attempted.

**The hive does not force-merge.** With admin bypass disabled, GitHub still
enforces branch protection (required checks like build-gate). A PR whose
required checks aren't green fails here, and the result file records why.

## Usage

```sh
hive-merge --repo <owner/repo> --number <N> [--method squash|merge|rebase] \
           [--expect-sha <sha>] [--update-branch]
```

| Flag | Required | Default |
| --- | :---: | --- |
| `--repo` | yes | — |
| `--number` (alias `--pr`) | yes | — |
| `--method` | no | `squash` |
| `--expect-sha` | no | auto-resolved (see below) |
| `--update-branch` | no | off |

Both `--flag value` and `--flag=value` forms work. `--squash`, `--merge`, and
`--rebase` are accepted as method shorthands. `--admin` is accepted and
**ignored** — the hive never admin-bypasses branch protection.

`--repo` and `--number` must resolve, or the script exits `2`. `--number` must
be an integer, or the script exits `2`.

### `--expect-sha` is auto-resolved, not optional in effect

The merge-request watcher **requires** a non-empty `expect_sha` (the F4 TOCTOU
guard above) — a request with none is denied outright. Rather than push that
burden onto every call site, the script resolves the PR's *current* head SHA
itself when `--expect-sha` is not given, using the cached App token at
`/var/run/hive-metrics/gh-app-token.cache`, and pins it into the request. The
head is captured at request time, closing the TOCTOU window exactly as
intended, so existing call sites that never passed `--expect-sha` keep
working unchanged.

If the SHA cannot be resolved (no token cache, `gh` unavailable, or the
lookup fails), the script prints an error naming the token-cache path and
**exits `3`** rather than writing a request with no pinned head.

### `--update-branch`

When set, the watcher syncs the PR branch with its base before attempting the
merge (resolves the common "behind main" case). A failure to update the
branch is not fatal — the merge attempt still proceeds and surfaces the real
blocker.

## It is asynchronous, by design

On success the script prints the request path and returns `0`. **The PR has
not been merged yet at that point.** It merges on the next watcher tick
(polling every 10 seconds).

To confirm, poll the `.result.json` written next to the request file, or
simply check whether the PR is merged.

## Retries and what happens when a merge can't land

The watcher retries a failed merge attempt up to **3 attempts total**,
tracked across ticks via the prior `.result.json`. What happens after the
final attempt depends on why it failed:

- **A required check is failing or pending** (classified from GitHub's own
  branch-protection error text, e.g. "required status check ... has not
  succeeded") — the request is quarantined (renamed `.exhausted`), but the
  hive re-engages its fix loop for that PR instead of abandoning it, subject
  to a per-red-SHA re-dispatch cap.
- **The blocker is unfixable by pushing code** — a true merge conflict or a
  permission error (matched against GitHub's own wording, e.g. "merge
  conflict", "not accessible by integration", "403", "must be a member") —
  the request is quarantined (`.exhausted`) and not retried further; the
  result file records the last error.

An authorization denial (forge-resistance failure, `CanMerge` gate failure,
or F4 target-binding failure) is not retried at all — the request file is
renamed `.denied` immediately and the result file records the reason.

A malformed request file (invalid JSON) is renamed `.bad`.

## Where things live

| Path | What |
| --- | --- |
| `/var/run/hive-metrics/merge-requests` | request files the watcher consumes |
| `/var/run/hive-metrics/gh-app-token.cache` | cached App token, used to auto-resolve `--expect-sha` |
| `/var/run/hive/uid-map.json` | UID → agent-name map, used for a nicer log line |

The UID map is **informational only** here. The watcher re-derives ownership
from the file's UID regardless.

## Related

- [`hive-open-pr`](hive-open-pr.md) — the equivalent relay for opening a PR;
  same request-file mechanism and authorship model
- [Security threat model](security-threat-model.md) — forge resistance and the
  UID-ownership anchor
- [Agent configuration](agent-configuration.md) — ACMM levels and the
  merge gate (`CanMerge`, `ModeIssuesPRsMerge`) that governs whether an agent
  may merge a PR at all
- [Audit log](audit-log.md) — merges are recorded there with the requesting
  agent
