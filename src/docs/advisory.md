# Advisory digest

Advisory agents (scanner, ci-maintainer, reviewer, and friends) file their
findings as **advisory beads**. Once per governor eval cycle the hive rolls the
open advisory beads of every agent into a single **advisory digest** and posts it
as a pinned comment on the advisory issue of the primary repo, rewriting the same
comment each cycle so a repo owner has exactly one place to look.

Everything on this page lives under `governor.advisory` in `hive.yaml`, and every
setting is also editable from **Governor Config → Advisory** in the dashboard
(owner role required).

```yaml
governor:
  advisory:
    max_findings: 10
    show_all: false       # true = ignore max_findings
    staleness_days: 7
    pr_autoclose: true
```

## Keeping findings current

A digest is only useful if the findings on it are findings that still hold. Two
mechanisms retire the ones that do not.

### Staleness auto-close

An advisory agent re-files a finding for as long as its condition holds. The
re-file does **not** create a second bead: the hive upserts it, refreshing the
bead's `last_seen_at` stamp instead. That makes silence meaningful — a finding no
agent has re-reported for `governor.advisory.staleness_days` days is one no agent
still sees.

Each eval cycle, before the digest is built, the hive closes every open advisory
bead whose `last_seen_at` is older than that window. Closed findings leave the
open list and appear once under **Recently Resolved** before dropping out
entirely.

- `governor.advisory.staleness_days` — default `7`, minimum `1`. Agents re-scan
  far more often than weekly, so a week is a generous margin. Shorten it on a
  hive whose advisory agents run constantly; lengthen it on one whose agents run
  rarely, or a finding may be closed between two runs of the agent that reports
  it.
- Beads filed **before** this feature existed carry no `last_seen_at` and are
  never pruned for staleness. "Not re-reported" cannot be told apart from "never
  stamped", so those beads are left alone and close through the normal paths.

The close is recorded on the bead as
`close_reason: auto-closed: finding not re-reported within staleness window`, so
every automatic close is inspectable with `bd show`.

Agents get the upsert behavior for free: `bd create --type advisory` with a title
matching an open advisory bead refreshes that bead instead of adding a second
one. No other bead type upserts — two tasks with similar titles are two tasks.

### PR-linked auto-close

When a pull request is verified merged, the hive compares its title against the
titles of open advisory findings and closes the ones that are close enough. A fix
therefore retires the finding it addresses immediately, instead of waiting out
the staleness window.

- `governor.advisory.pr_autoclose` — default `true`. Set it to `false` to rely on
  staleness (and explicit agent closes) alone.
- Matching is title similarity only, so it is deliberately best-effort: a finding
  closed in error comes straight back the next time an agent files it, because a
  re-file after a close opens a fresh bead.
- The close is recorded as
  `close_reason: auto-closed: a merged pull request addresses this finding`.

## How much the digest shows

By default the digest renders the **top 10** findings, ranked by severity
(critical → high → medium → low → minor), then by whether the finding's file
still exists at the analyzed commit, and then by recency, across all agents at
once. Ranking globally rather than per agent is deliberate: an owner cares which
findings matter most, not which agent produced them.

- `governor.advisory.max_findings` — default `10`, minimum `1`.
- `governor.advisory.show_all` — default `false`. `true` bypasses the cap
  entirely, whatever `max_findings` says. This is the only supported way to
  render an uncapped digest: `max_findings: 0` reads as "unset" and resolves
  straight back to the default on the next config load.

Nothing is dropped silently. When the cap holds findings back, the digest carries
a note directly under the finding count:

> 💡 Showing top 10 findings (by severity). 24 more exist. Set
> `governor.advisory.show_all: true` to see all.

The hub's My Hives view shows the same thing on the hive's row: a
`10 findings (top 10)` pill, whose tooltip names the full total.

The top-N cap is applied **after** near-duplicate collapsing, so a single
recurring problem re-filed under a dozen wordings cannot consume the whole
budget. A separate, unconditional cap still limits how many findings one agent
may render under one finding type in a single severity section — that one exists
to keep the comment inside GitHub's 65,536-character limit and is not
configurable.

For the same reason, a finding whose file no longer exists at the analyzed
commit — the ones captioned *"file path not found at analyzed commit — finding
may be outdated"* — does not take a slot ahead of a live finding of the **same
severity**. Freshness only breaks ties within a severity band: a path that moved
does not prove the problem is gone, so a stale critical still outranks a live
low. Stale findings are set aside rather than dropped, and backfill any slot no
live finding of that severity claims, so the digest never renders short.

This ordering requires a pinned `AnalyzedSnapshot`, which the governor resolves
once per post cycle. Without one (no GitHub client, or the commit could not be
resolved) the ranking falls back to severity-then-recency and no path is
checked. Path lookups are cached per commit and stop as soon as the cap is
filled, so ranking the full finding set costs roughly what verifying only the
rendered ones used to.

## The digest stopped updating

The digest is posted to one pinned issue per repo, titled **🐝 Hive Advisory
Report**. Three things can stop it, and all three now surface rather than fail
quietly:

- **The pinned issue could not be resolved.** The hive ensures the issue at boot;
  if that call fails (rate limit, 5xx, search-API blip) it is retried on every
  eval cycle until it succeeds. While it is unresolved and there are findings to
  publish, the spoke records a post error — `no advisory issue resolved for
  <repo>` — so the hub flags the hive's digest stale with that cause instead of
  reading it as a hive that simply does not post advisories.
- **The repo has Issues disabled.** Forks have `has_issues=false` by default —
  there is no Issues tab, so the pinned issue can neither be found nor created.
  The hive checks the flag before attempting the create and records a distinct
  post error naming the remedy: enable Issues in the repo's **Settings >
  General > Features**, or point the hive at the upstream repo. This is not an
  App-permission problem; the App banner stays down.
- **Someone closed the pinned issue.** The hive reopens the existing issue rather
  than filing a new one. Filing a new one splits the digest: the hive writes to
  the new issue while everyone subscribed to the old one watches a comment that
  never changes again — which looks exactly like a wedged digest. If a repo
  already has several `🐝 Hive Advisory Report` issues from before this behavior,
  close the stale duplicates and keep the one the hive is currently updating (the
  one with the newest digest comment).
- **The App cannot write.** A 403 on the digest post raises the App banner with
  its specific cause; the hub pill carries the same string.

To check freshness: the hive's row on the hub shows the last successful post
time and any error; on the spoke, `posted advisory digest` is logged at INFO on
every successful update, and `advisory digest not posted` at WARN when there is
nothing to post to.

## Related

- [Advisory digest staleness](advisory-staleness.md) — the other side of the
  section above: which of these failures light the hub's stale-advisory pill,
  which are deliberately suppressed, and how to measure the suppressed ones.
- [`bd` beads CLI](beads-cli.md) — inspecting advisory beads directly, including
  their `close_reason`.
- [Governor mode thresholds](governor-thresholds.md) — the eval cycle the digest
  is built on.
