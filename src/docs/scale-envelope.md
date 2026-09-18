# Large-spoke scale envelope

How Hive behaves as a **single spoke's backlog** grows — how many open issues and
PRs one spoke can carry before behaviour changes, what changes, and which knobs
control it.

This page is about *one big spoke*. The roadmap already covers *many spokes*
(constellation foundations, [#5691](https://github.com/hivecommons/hive/issues/5691))
and *shared provider capacity*
([#5698](https://github.com/hivecommons/hive/issues/5698)); neither answers "what
happens at 300 open PRs in one hive", which is the question adopters actually hit
first. [#7392](https://github.com/hivecommons/hive/issues/7392) asked for that
dimension to be stated rather than discovered.

## The design rule

> **Overflow must paginate, prioritize, or shed load — never fail closed on the
> work that would shrink the overflow.**

A cap that stops the hive from doing the work that drains the backlog is
self-reinforcing: the backlog causes the block, and the block prevents the
backlog from shrinking. That is not a hypothetical — it is
[#7385](https://github.com/hivecommons/hive/issues/7385), where a render cap
overflowing produced a **STAND DOWN**, which is a *correctness* signal ("someone
else may be working here"), not a *size* signal.

Two corollaries, both already true of the shipped caps and worth holding future
ones to:

1. **A truncated list must say it is truncated.** Every kick list that is cut
   ends with an explicit "… and N more" line
   (`prListOverflowLine`, `src/pkg/scheduler/scheduler.go:915`). A silent slice
   reads to the agent as "this is the complete list".
2. **Truncation must drop the least important tail, not an arbitrary one.**
   This holds today and is easy to assume backwards:
   - Issues are sorted **oldest first** before any cap is applied
     (`src/pkg/github/client.go:669-671`, descending `AgeMinutes`).
   - PRs are sorted by **review class then oldest-first within class**
     (`SortPullRequestsForReview`, `src/pkg/github/review_priority.go:176-192`;
     fixes → refactors/docs → tests).

   So a cap drops the *newest, lowest-priority* tail, and those items return on
   later kicks as the list drains. Lowering a cap does not hide the most urgent
   work.

## What is bounded and what is not

The important structural fact: **caps in Hive are render-side, not
enumeration-side.**

Enumeration is deliberately complete. `fetchIssues` and `fetchPRs` page through
every open item in every watched repo with no page ceiling
(`src/pkg/github/client.go:718-738` and `788-808`, `PerPage: 100` looping until
`resp.NextPage == 0`). The governor therefore always sees true queue depth —
which is what makes `governor-thresholds.md` scaling meaningful — and the caps
below only decide how much of that is *rendered into a kick prompt*.

Consequences worth stating plainly:

- **API and wall-clock cost of a poll grows linearly with backlog**, at roughly
  one request per 100 open items per repo per enumeration. On a ~300-PR, 16-repo
  spoke this is tens of requests per cycle. We have not published a measured
  rate-limit headroom figure for that shape; treat it as unmeasured rather than
  as known-safe.
- **Prompt size does not grow with backlog** once the render caps apply — that
  is exactly what [#7368](https://github.com/hivecommons/hive/issues/7368)
  fixed.

## Why prompt size is a hard constraint, not a cosmetic one

A kick is **typed into a terminal**, so prompt size converts directly into
delivery *time*. The measured worst case before the PR cap existed: a spoke with
302 open PRs produced a **69.5 KiB** kick (~36 KiB of it the PR list), against
the budget documented in
`src/pkg/dashboard/prompt_history.go:47-80` (the figure is stated at line 65) — **~25 KiB worst case, ~8 KiB
typical**. Delivery of that prompt took ~3 minutes, and a long delivery window is
what made the restart-during-delivery loop of
[#7363](https://github.com/hivecommons/hive/issues/7363) reachable in practice.

That is the general shape of scale failure here: an unbounded list does not fail
by being big, it fails by widening a timing window somewhere else.

## Inventory of cap-triggered behaviours

Every entry below was read in the tree at the cited symbol. "Tunable" means an
operator can change it without rebuilding.

| Cap | Where | Default | Tunable | Behaviour when exceeded |
| --- | --- | --- | --- | --- |
| Issue list per kick | `governor.kick_limits.max_issues` → `Scheduler.issueCap()`, `src/pkg/scheduler/scheduler.go:895-900` | 100 | **Yes** (`hive.yaml`, or Settings → Repos) | Truncate oldest-first; list is cut silently in `formatIssueListWithPolicy` (`scheduler.go:397`) and with an explicit note in the scanner/work lists |
| PR list per kick (actionable, stale drafts, merge-eligible, CI-failing, repair queue) | `governor.kick_limits.max_prs` → `Scheduler.prCap()`, `src/pkg/scheduler/scheduler.go:905-910` | 50 | **Yes** (same block/UI) | Truncate in review-priority order + "… and N more open PRs not listed (cap N per kick; they return on later kicks as this list drains)" |
| Ceiling on either cap | `config.MaxKickListCap`, `src/pkg/config/kick_limits.go:31-32` | 500 | No | A configured value above 500 is **pinned to 500**, not rejected — so a bad `hive.yaml` degrades to a safe cap instead of failing startup (`clampKickListCap`, `kick_limits.go:39-47`). Non-positive means "use the default"; there is deliberately **no uncapped setting** |
| Held-PR snapshot, per repo | `maxHeldPRsPerRepoPerKick`, `src/pkg/scheduler/scheduler.go:1545` | 100 | **No** (hardcoded) | **Fails closed, by design**: the overflowing repo gets "STAND DOWN for `<repo>` this kick". Scoped per repo since [#7391](https://github.com/hivecommons/hive/pull/7391) so one crowded repo no longer blanks the snapshot for the other 15 |
| Red-PR fix-before-new detail | `redPRFixMaxDetailed`, `src/pkg/scheduler/scheduler.go:1279` | 5 | **No** (hardcoded) | Degrade to summary: "… and N more (full list: `<path>`)" |
| Review-thread fix-before-new detail | same constant, used at `src/pkg/scheduler/scheduler.go:1353-1355` | 5 | **No** (hardcoded) | Degrade to summary: "… and N more PRs (full list: `<path>`)" |
| CI-evidence excerpt per PR | `redPRFixExcerptRunes`, `src/pkg/scheduler/scheduler.go:1280` | 400 runes | **No** | Truncate with "…" |
| Skill text injected per kick | `maxSkillsInjectionBytes`, `src/pkg/scheduler/scheduler.go:2257` | 8192 B | **No** | Shed load: skills are **dropped whole**, never truncated mid-body, and the drop is logged |
| Issues primed into `AGENTS.md` context | `maxIssuesToPrime`, `src/pkg/scheduler/scheduler.go:2150` | 5 | **No** | Truncate |
| Auto-merge sweep merges per pass | `DefaultAutoMergeSweepMaxMerges`, `src/pkg/github/automerge_sweep.go:18` | 3 | **Yes** (`auto_merge.max_merges`) | Shed load: remaining merges wait for the next pass |
| Task-list sweep closures per tick | `DefaultTaskListSweepMaxCloses`, `src/pkg/github/task_list_sweep.go:19` | 5 | Partly (`MaxCloses` option) | Shed load: remaining closures wait for the next tick |
| Prompt-history on disk | `promptHistoryMaxSizeMB` × backups, `src/pkg/dashboard/prompt_history.go:80` | 96 MiB worst case | **No** | Shed oldest: rotate + gzip, oldest prompts age out first |
| Issue/PR enumeration | `fetchIssues` / `fetchPRs`, `src/pkg/github/client.go:693-713`, `775-795` | **no cap** | n/a | Pages to completion; cost grows with backlog |

### The inconsistency this inventory exposes

Two caps are operator-tunable (`governor.kick_limits.max_issues` / `max_prs`),
and the rest are hardcoded constants — including
`maxHeldPRsPerRepoPerKick`, which is the **only** cap in the table whose overflow
behaviour is *fail closed*. The cap most likely to stop work at scale is the one
an operator cannot adjust.

That is not obviously wrong: a fail-closed disjointness guarantee is a poor thing
to let an operator weaken, and `#7391`'s per-repo scoping removed most of the
practical pressure to raise it. But the split is currently accidental rather than
decided. A reasonable policy to adopt:

- **Truncating / load-shedding caps** (prompt size, evidence detail, sweep rate)
  are presentation or pacing choices and should be tunable, with a ceiling —
  the `MaxKickListCap = 500` pattern, where the ceiling exists specifically so
  the control cannot be used to recreate the failure it prevents.
- **Fail-closed caps** (safety/correctness gates) stay hardcoded, and instead
  carry the burden of being **scoped as narrowly as the guarantee allows** —
  which is exactly the change `#7391` made.

Deciding that explicitly is tracked on
[#7392](https://github.com/hivecommons/hive/issues/7392); this page records the
current state, not an accepted policy.

## The envelope today

Stated honestly, with the evidence we have:

| Backlog per spoke | Status |
| --- | --- |
| Up to ~100 open PRs / ~100 open issues | Inside every default cap. No list truncates, no degraded mode. |
| ~100–300 open PRs | **Exercised in production** (projectbluefin, ~304 open PRs across 16 repos, 2026-09-17). Kick lists truncate at the caps and say so; the governor still sees full depth. This is where the [#7368](https://github.com/hivecommons/hive/issues/7368) / [#7363](https://github.com/hivecommons/hive/issues/7363) / [#7385](https://github.com/hivecommons/hive/issues/7385) / [#7386](https://github.com/hivecommons/hive/issues/7386) cluster was found and fixed. |
| >300 open PRs, or >100 held PRs **in a single repo** | Untested. The per-repo held-PR stand-down (`maxHeldPRsPerRepoPerKick`) is the first thing expected to bite, and it is not operator-tunable. |
| Any size, per-repo enumeration cost | Unmeasured against GitHub rate limits at this shape. |

No number here is a support commitment; it is a record of what has been run and
what has not.

## For contributors adding a cap

If your PR introduces a cap, threshold, or list limit, state in the PR body:

1. What happens when it is exceeded — truncate, paginate, shed load, or fail
   closed.
2. If it truncates, that the ordering puts the least important items in the
   dropped tail, and that the output says the list is partial.
3. If it fails closed, why the guarantee needs it, and the narrowest scope it can
   hold (per repo, per agent — not per hive, if per repo will do).
4. Whether an operator can change it, and if so what the ceiling is.

## Related

- [Operator reference](operator-reference.md) — the `governor.kick_limits` entry.
- [Governor mode thresholds](governor-thresholds.md) — how *cadence* scales with
  repo count, the other half of behaviour-at-scale.
- Evidence base: [#7368](https://github.com/hivecommons/hive/issues/7368)
  (uncapped PR list) / [#7379](https://github.com/hivecommons/hive/pull/7379) /
  [#7395](https://github.com/hivecommons/hive/pull/7395),
  [#7363](https://github.com/hivecommons/hive/issues/7363) (restart/kick loop) /
  [#7378](https://github.com/hivecommons/hive/pull/7378),
  [#7385](https://github.com/hivecommons/hive/issues/7385) (stand-down on render
  overflow) / [#7391](https://github.com/hivecommons/hive/pull/7391),
  [#7386](https://github.com/hivecommons/hive/issues/7386) (fork-unaware repair
  queue).
