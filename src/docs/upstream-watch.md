# Upstream watch

A repo that began as a fork drifts from its upstream until git-level sync stops
being viable. The `upstream_watch` block declares, per repo, which upstream to
follow and how much of it to surface. It is **off by default**.

When it is on, the hive polls each configured upstream and opens a labelled
issue in the fork for every merged PR or release that still applies, so the
existing fixer agents pick the port up like any other issue. It opens issues
only, never PRs. Tracking issues:
[#9963](https://github.com/hivecommons/hive/issues/9963),
[#9967](https://github.com/hivecommons/hive/issues/9967).

## Configuration

```yaml
upstream_watch:
  enabled: true
  interval: 6h                      # default 6h
  repos:
    myorg/forked-thing:             # must be in project.repos (bare or org-qualified)
      upstream: origin-owner/thing  # optional
      sources: [releases, prs]      # default: both
      pr_labels: [bug, security]    # empty = all merged PRs
      max_issues_per_run: 5         # omit = no cap
      label: upstream/port          # default upstream/port
```

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | Master switch. |
| `interval` | `6h` | Poll cadence; must not be negative. |
| `repos.<repo>.upstream` | fork parent | `owner/repo`. If omitted it is resolved from the GitHub fork parent (`parent.full_name`) at runtime; an explicit value always wins. |
| `repos.<repo>.sources` | `[releases, prs]` | Known values: `releases`, `prs`. |
| `repos.<repo>.pr_labels` | all merged PRs | Only PRs carrying one of these labels. |
| `repos.<repo>.max_issues_per_run` | no cap | Must be positive when set. |
| `repos.<repo>.label` | `upstream/port` | Label on filed issues. |

Loading fails if a `repos` key is not in `project.repos`, `upstream` is not
`owner/repo`, a source is unknown, or `max_issues_per_run` is negative.

## The loop

The governor's eval tick runs the watch at most once per `interval`. For each
repo under `upstream_watch.repos`, in key order, one pass:

1. Lists upstream PRs merged (`merged_at`) and non-draft releases published
   (`published_at`) strictly after the repo's watermark, filtered by
   `sources` and `pr_labels`, oldest first.
2. Classifies each item as `security`, `bugfix`, `feature` or `chore` from
   its labels and conventional-commit title prefix, and estimates the port
   difficulty (`easy`, `moderate`, `hard`) from files and lines changed.
3. Checks applicability without a checkout: through the GitHub contents API,
   at least one touched file must exist on the fork's default branch. An item
   with none is recorded as `skipped`. Releases carry no file list and are
   always applicable.
4. Files an issue for the rest, titled `upstream: <upstream title>`, with the
   upstream link, a summary of the upstream body, the class, the affected
   files, the difficulty estimate and a hidden marker such as
   `<!-- upstream-ref: owner/repo#123 -->` (`owner/repo@<tag>` for a
   release). The configured `label` (default `upstream/port`) is applied.
   Mentions copied from upstream are neutralised so nobody upstream is pinged.
5. Advances the watermark past every item that was filed, skipped or
   deduplicated.

`max_issues_per_run` stops a repo's pass after that many issues; the remaining
items stay behind the watermark and are picked up next pass. A GitHub error
stops the repo's pass the same way, after saving what was already handled.
Cancelling the hive's context stops the loop between items with the state
saved.

## Dedupe and dismissal

Each upstream item has a ref: `upstream#<pr>` or `release:<tag>`.

- **State index first.** A ref with any recorded outcome is never filed again.
- **Marker search second.** Before filing, the fork is searched for the hidden
  marker. A match is recorded as filed against that issue instead of opening a
  duplicate, which covers a lost or restored state file.
- **Dismissal.** If the matching fork issue was closed as *not planned* or
  carries the `upstream/dismissed` label, the ref is recorded as `dismissed`
  and never resurfaced.

## State file

The watermark, last-run time and per-ref outcomes (`filed`, `skipped`,
`ported`, `dismissed`, with the fork issue number) are kept in
`/data/upstream-watch.json` on the PVC, written atomically, so a restart never
re-scans an upstream from zero. A corrupt file stops the watch rather than
refiling everything.

Sources: `src/pkg/config/upstream_watch.go`, `src/pkg/upstreamwatch/`,
`src/cmd/hive/upstream_watch.go`.
