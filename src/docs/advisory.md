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
    update_interval_s: 0  # 0 = default (~60s cycle); 30–3600 to slow the refresh
    target: github        # github (default) | linear
    linear_issue: ""      # required when target is linear, e.g. ONB-123
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

### Evidence provenance

The digest footer stamps one commit — `Analyzed at owner/repo@<sha>` — across
every finding it renders. That stamp describes when the digest was *built*, not
when each finding's evidence was *computed*, and the two drift apart: findings
persist as open beads and are re-rendered verbatim every cycle, so a finding
whose evidence was gathered several commits ago is republished under today's
commit without anything re-checking it.

That drift is not just noise. A finding that was accurate when computed, then
fixed in the target repo, kept appearing under a commit at which it no longer
reproduced — and a re-verifying agent that checked it against the stamped commit
concluded the evidence had been fabricated when it was merely stale (#5130).

So the digest now tracks the commit a finding's evidence came from, and captions
any finding that was **not** computed at the analyzed commit:

> - **[coverage-gap]** contrib/aib has no test coverage ⚠️ _(evidence computed at
>   `c9546a8`, not re-verified at the analyzed commit)_ _quality_

The digest cannot re-run a finding's own evidence — that evidence is arbitrary
(a `grep`, a workflow-file read, a coverage run), and a coverage-specific refresh
would still republish the rest. What it can do is stop asserting a freshness it
never checked, so the caption says exactly that and the footer no longer implies
otherwise.

A finding's provenance commit is read from, in order:

1. `provenance_sha` in the finding's advisory JSONL, or the same key in the
   bead's metadata. This is the **explicit** form, and the one to prefer.
2. The finding's own prose, when it already names its provenance — wordings like
   `revision <sha>`, `commit <sha>`, `computed at <sha>` or `as of <sha>` are
   recognised. A bare hex run with no such keyword is never read as a commit:
   log ids and digests appear in finding text far too often.

A finding that names no provenance at all is left **unmarked**. Silence about
provenance is not a freshness claim in either direction, so the digest neither
captions it nor implies it was verified.

#### Provenance and the staleness clock

Provenance also fixes a hole in [staleness auto-close](#staleness-auto-close).
The re-report of a finding is what refreshes its `last_seen_at`, on the reasoning
that an agent only re-files a finding while its condition holds. But agents
re-report from **cached prior findings**, not from re-verification — so a
finding that had already been fixed kept re-stamping itself and survived every
prune window.

A re-report that carries the **same** `provenance_sha` the bead already records
is therefore a restatement of evidence computed once, not fresh confirmation
that the condition still holds. It no longer refreshes `last_seen_at`, so the
staleness clock keeps running and `staleness_days` retires the finding on the
normal schedule. A re-report computed at a *different* commit is a genuine
re-check: it refreshes the stamp and records its new provenance.

Two deliberate limits keep this from retiring findings that still hold:

- Only an **explicit** `provenance_sha` gates the refresh, never the prose-
  inferred one. Misreading "regressed in commit `<sha>`" as provenance would age
  out a live finding, so inference is trusted for captions only.
- A finding that records no provenance behaves exactly as it did before — every
  re-report still counts as a confirmation. Nothing starts ageing out merely
  because its producer does not report a commit.

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

`max_findings` bounds the **Recently Resolved** changelog too. It is a
changelog, not work: a reader opens the digest to learn what still needs doing,
and there is no length at which a long list of healed findings serves that
better than the open ones do. Left outside the cap it crowds them out — the live
digest of 2026-09-03 rendered 10 open findings in 4,937 characters, under a note
saying 286 more existed, and 100 resolved ones in 22,138, so 82% of the comment
was work already done.

Withheld resolved entries are announced the same way withheld findings are, as
the last line of the section:

> - *…plus 20 more resolved in the last 48h, collapsed so the open findings
>   above stay readable*

`show_all: true` lifts the cap off the changelog as well. A hard ceiling of 100
resolved entries still applies above it — an owner asking to see every finding
is not asking for an unbounded changelog, and past GitHub's character limit the
digest is truncated from the bottom, which is where this section and the
analyzed-commit footer under it live.

For the same reason, a finding whose file no longer exists at the analyzed
commit — the ones captioned *"file path not found at analyzed commit — finding
may be outdated"* — does not take a slot ahead of a live finding of the **same
severity**. Freshness only breaks ties within a severity band: a path that moved
does not prove the problem is gone, so a stale critical still outranks a live
low. Stale findings are set aside rather than dropped, and backfill any slot no
live finding of that severity claims, so the digest never renders short.

This ordering requires a pinned `AnalyzedSnapshot`, which the governor resolves
once per evaluation cycle (so the dashboard and status digests are snapshot-pinned
too, not only the posted comment). Without one (no GitHub client, or the commit could not be
resolved) the ranking falls back to severity-then-recency and no path is
checked. Path lookups are cached per commit and stop as soon as the cap is
filled, so ranking the full finding set costs roughly what verifying only the
rendered ones used to.

## The digest stopped updating

The digest is posted to one pinned issue per repo, titled **🐝 Hive Advisory
Report**. By default it refreshes every governor eval cycle (~60s); an operator
can slow that deliberately (see below), which is a configured cadence, not a
stall.

Three things can stop it, and all three now surface rather than fail
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

## How often the digest updates

`governor.advisory.update_interval_s` — default `0`, meaning the digest is
checked and refreshed every governor eval cycle (~60s), exactly the behavior
before the knob existed. Set it between `30` and `3600` seconds to slow the
refresh — useful to cut GitHub API writes, notification churn on watched
repos, and audit-log noise. Also editable from **Governor Config → Advisory →
When it updates**; dashboard edits apply live from the next cycle, no restart.

Details worth knowing:

- The throttle paces only the GitHub comment write. The digest itself, the
  dashboard view, and the status broadcast still refresh every cycle.
- The window advances only on a **successful** post, so a failed write is
  retried on the very next cycle rather than waiting out the interval.
- A brand-new finding on a quiet hive posts immediately; only subsequent
  refreshes wait for the interval.
- Byte-identical digests are skipped regardless of this setting, with a
  periodic forced full rewrite to heal hand-edited comments; that forced
  rewrite is counted in post attempts, so its spacing stretches proportionally
  at longer intervals.
- The maximum (1 hour) is deliberately capped under the hub's 90-minute
  wedged-digest alarm, so a healthy slow cadence can never be flagged as a
  stalled digest.
- Values outside the band are rejected by the dashboard API and clamped (with
  a one-time warning in the hive log) when hand-edited into `hive.yaml`.

## Where the digest is posted

`governor.advisory.target` — default `github`. Chooses which tracker hosts the
digest comment. Also editable from **Governor Config → Advisory**.

- `github` (default, and the only behavior before the key existed): the pinned
  comment on the advisory issue of the primary repo, exactly as described at
  the top of this page. Leaving the key out means this; nothing about the
  GitHub path changes when the key is absent.
- `linear`: the digest is maintained as **one comment on a designated Linear
  issue**, named by `governor.advisory.linear_issue` (an identifier such as
  `ONB-123`). The comment body is identical to what the GitHub comment would
  contain, and it is rewritten in place every cycle: the hive finds its own
  comment by the fixed footer it stamps on it (`hive-advisory-digest`) and
  updates that comment rather than adding a new one, skipping the write when
  the body is unchanged. Authentication reuses the work source's key
  (`governor.work_source.linear.api_key`, the same bare-token header the
  issue enumerator sends), so a Linear-sourced hive needs no extra credential.
  The `update_interval_s` throttle and the hub's staleness signal apply to
  this route exactly as to GitHub.

This is meant for hives whose work source is Linear
(`governor.work_source.type: linear`) and whose owners live in Linear rather
than GitHub. It is a choice, not an inference: a Linear work source does
**not** move the digest by itself, because a living GitHub issue that is never
closed is a perfectly good digest home and many operators prefer it.

The Linear route fails closed. If `target` is `linear` and `linear_issue` is
empty, or the API key is missing, or the issue cannot be found, the hive logs
an error naming the missing key (`governor.advisory.linear_issue is required
when governor.advisory.target is linear — digest not posted`), records the
cycle as a failed post so the hub's stale-advisory pill trips, and does
**not** fall back to the GitHub issue. The dashboard API rejects a `linear`
target with no issue up front, and rejects any target other than `github` or
`linear`; a hand-edited unknown value in `hive.yaml` is reported the same way
at post time.

## Related

- [Advisory digest staleness](advisory-staleness.md) — the other side of the
  section above: which of these failures light the hub's stale-advisory pill,
  which are deliberately suppressed, and how to measure the suppressed ones.
- [`bd` beads CLI](beads-cli.md) — inspecting advisory beads directly, including
  their `close_reason`.
- [Governor mode thresholds](governor-thresholds.md) — the eval cycle the digest
  is built on.
