# Duplicate PR sweep

A periodic, cross-PR pass that clusters open pull requests by changed-file set
and **suggests** which one to keep. Off by default.

It exists because nothing in hive could see two PRs at once. The duplicate-PR
guard (`ApplyDuplicatePRGuard`, `src/pkg/github/prclaims.go:1102`) runs at
PR-open time and only for hive's own agents, so a duplicate that already exists
between two PRs — the common case on a large human queue — was invisible. On a
spoke with hundreds of open PRs and no auto-merge, collapsing duplicates is the
one lever that actually shrinks the queue, and no agent had the view to pull it
(hivecommons/hive#7469).

## Why it is not the review swarm

The review swarm judges **one** change at a time and is deliberately denied the
PR body and the author's rationale. Duplicate detection is a comparison
*between* PRs using exactly that intent signal, so the reviewer is the wrong
instrument for it — not a weak one, the wrong kind. Nothing here changes the
reviewer's permissions: it still has no GitHub write access, and the sweep's
comment is written by the hive itself with the App token it already holds, on
the same canary-gated, mention-neutralized, secret-scrubbed comment path every
other hive-authored comment uses.

## Why the output is a suggestion and never an action

Changed-file identity is a **candidate generator**, not a verdict. The same
clustering that correctly collapses three PRs binding the same workflow inputs
also groups:

- three dependency-bump PRs for three different images that share one manifest, and
- three unrelated fixes to one busy file.

There is no threshold that separates those from a true duplicate using the file
set alone. So the sweep has no code path that closes, labels, approves,
requests changes on, or merges anything, and the comment it writes says so in
its own body.

## Grading

| Tier | Meaning |
| --- | --- |
| `identical-diff` | Every PR in the cluster changes the same files **and** produces a byte-identical patch. The strongest corroboration available without reading intent. |
| `same-files` | The changed-file sets match but the patches differ. This is where the known false positives live, and the rendered comment says so explicitly. |

An **unknown** diff hash on any member downgrades the whole cluster: "we could
not check" must never render as "we checked and they agree".

## Survivor selection

- **Ordinary cluster** — the earliest PR survives. The first author to propose
  a change should not be asked to stand down for someone who arrived later.
  Suggestions are posted on the **later** PRs.
- **Bot regeneration series** (every member authored by the same `[bot]`
  account, or a login listed in `bot_authors`) — the **newest** survives, since
  each run supersedes the last by construction, and a **single summary** is
  posted on that newest PR rather than one comment on each. A bot re-opening
  the same PR daily would otherwise turn this sweep into the noise source it is
  meant to reduce.

Drafts are excluded entirely: a draft is not competing for the queue slot the
sweep exists to free.

## Configuration

Two separate opt-ins, both off by default, because they authorize different
things:

```yaml
duplicate_sweep:
  enabled: true          # report-only: cluster, log and audit. No writes.
  post_comments: true    # the separate grant that lets it comment.
  max_comments: 5        # per pass, across all repos
  max_prs_per_repo: 100  # the sweep's API budget
  bot_authors: [churn-updater]  # extra logins to treat as a regenerating bot
```

`enabled` alone gives a dry run an operator can read in the log before the hive
says anything on a contributor's PR. `post_comments` has no effect without it.

The sweep runs at most once per hour (`duplicateSweepInterval`,
`src/cmd/hive/main.go`) and its comments are idempotent: each cluster carries an
invisible marker, so a later pass edits its own prior suggestion in place — and
elides the write entirely when the body is unchanged — rather than stacking a
fresh comment every hour.

## Fail-closed cases

- A PR whose file list GitHub **truncated** is excluded rather than
  fingerprinted from what came back. A short list fingerprints as a different,
  smaller set that could coincide with another truncated list, inventing a
  duplicate that does not exist.
- A per-repo API failure is logged and the remaining repos are still swept; a
  partial suggestion set is strictly better than none and nothing here is
  destructive.
- File sets are compared **within** a repo only. Two repos sharing
  `.github/workflows/ci.yml` is a coincidence, not duplicated work.

Source: `src/pkg/dupsweep` (clustering and rendering, no GitHub dependency),
`src/pkg/github/duplicate_sweep.go` (the scan and the comment path),
`runDuplicateSweepIfDue` in `src/cmd/hive/main.go` (the cadence).
