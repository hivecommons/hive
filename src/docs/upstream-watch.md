# Upstream watch configuration

A repo that began as a fork drifts from its upstream until git-level sync stops
being viable. The `upstream_watch` block declares, per repo, which upstream to
follow and how much of it to surface. It is **off by default**.

This page covers the configuration only (hivecommons/hive#9966, part of
#9963). Polling upstream and filing issues in the fork are separate work.

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

Source: `src/pkg/config/upstream_watch.go`.
