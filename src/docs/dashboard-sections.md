# Dashboard sections explained

This page explains every section of the Hive dashboard in plain words. It is written for someone running Hive for the first time.

Every section title on the dashboard has a small **?** mark next to it. Rest the pointer on it, or move to it with the Tab key, to see one sentence about the section. Click it, tap it, or press Enter on it to open this page at that section's entry.

Each entry answers the same questions in the same order:

1. What the section is. This is the same sentence the **?** mark shows.
2. What it tells you.
3. How its numbers and labels are worked out.
4. What it is good for: when to look at it, and what to do about it.
5. A short example.
6. When the section appears.
7. Which settings change it.

Hive has two release lines. **v5** is the stable line. **v6** is the newer line. Where something exists on only one line, the entry says so.

For short definitions of the names the dashboard uses, see the [dashboard glossary](dashboard-glossary.md). For how the dashboard is built, see the [spoke dashboard guide](dashboard.md).

## Words used on this page

These words have a special meaning in Hive. Each entry explains them again the first time it uses them.

- **Autonomy level:** how much Hive may do on its own, from L1 (watch only) to L6 (works and merges on its own). Hive calls this the ACMM level.
- **Tracked:** an issue or pull request that Hive has put on its work list after its filters ran.
- **Actionable:** a tracked item that Hive could work on now, because nothing is waiting on a person.
- **Held:** an item someone parked on purpose by adding a hold label, such as `hold` or `on-hold`. Hive leaves it alone.
- **Outside:** an open item that Hive's filters kept off its work list, for example because of a label.
- **Band:** a group the dashboard puts an issue or pull request in, named after what it needs next. Bands are for display only.
- **Needs-human:** waiting on a person, for example for a decision or a review. It is also the name of a label.
- **Merge-eligible:** a pull request that passed every check Hive uses before it may merge it.

## Overview

How many issues and pull requests Hive is tracking right now, and how many of them it can work on.

**What it tells you.** The top row of tiles splits all your open work into a few piles. The two charts show the same work grouped by what it needs next. The small lines inside each tile show how the number moved over the last day or week.

**How the numbers are worked out.**

- **Total open issues** and **Total open PRs** count every open issue and every open pull request in the repositories you selected. Draft pull requests count too.
- When the ForgeState cache (`forgestate.enabled: true`) has finished its first full scan of a repository, the two totals are read from that cache instead of from the GitHub search API. The cache is kept current by webhook events and its own low-rate refresh, so the totals do not cost a `/search/issues` request on every status refresh. The actionable, held, blocked and outside parts still come from Hive's own classification; whatever the cache counts that the classification has not placed yet lands in Outside's **hold-adjacent/other** part, so the sum always matches the total.
- If GitHub rate-limits or fails the count request, the tiles keep the last good numbers and show **Counts stale since <time> — serving last-good** instead of dropping to zero. The status payload carries the same moment as `staleSince` (shared with the enumeration last-good marker), and each repository as `countsStale` / `countsStaleSince`.
- The two totals are the count of the items actually listed in the status snapshot (one canonical `openItems` set per refresh, built server-side from the enumeration or ForgeState cache items plus the last-good copy). The small `issues/PRs` counter beside **Overview** in the left menu and the **Hive activity monitor** list and header are read from that same set, so all three always agree; hover the counter to see the whole sum.
- When a repository reports more open items than the snapshot can list (for example a cache total that has run ahead of the item list), the extra are never added to the totals; a small **+N not yet listed** badge beside the tiles names them until the next refresh lists them.
- After a restart Hive serves the snapshot it saved on disk; it is marked stale (**Counts stale since** badge) until the first fresh enumeration lands, so a pre-restart number is never shown as current.
- Each total is written as a sum, for example `10 = 2 actionable + 0 held + 1 blocked + 7 outside`.
- **Actionable now** counts tracked items that Hive could work on now. Tracked means Hive put the item on its work list. Actionable means nothing is waiting on a person.
- Issues in the "Needs human" and "Confirm & close" bands are left out of Actionable now. A band is a display group named after what the item needs next.
- Pull requests in the "Needs human" and "Blocked" bands are left out, and so are drafts.
- **Held** counts issues and pull requests with a hold label. A hold label, such as `hold` or `on-hold`, parks an item on purpose.
- **Blocked / needs-human** counts work that waits on a person or on something else. For issues, that is the "Needs human" and "Confirm & close" bands. It also counts issues with a `needs-human` label and issues waiting for their reporter. For pull requests, it is the "Needs human" and "Blocked" bands.
- **Outside** counts open items that Hive's filters kept off its work list. Hover its ⓘ mark to see why each item was kept out. The reasons are labels such as `needs-direction`, `needs-decision` or `needs-spec`, and exempt labels. Others are reporter triage, your project issue filter, standing advisory issues and bot dependency dashboards. Draft pull requests count here too.
- **Test coverage** appears at the end of the row when `HIVE_COVERAGE_BADGE_URL` is configured and readable. It shows the primary repository's current coverage and goal, using the same reading as the quality agent.
- The last part of Outside is **hold-adjacent/other**. It is whatever is left after every named reason is counted. Hive cannot say more about these items.
- The Outside tile exists on the v5 line only. On v6 the same items are part of the totals but have no tile of their own.
- **Issues by band** groups tracked and held issues. The bands are Unclaimed, Claimed, Needs triage, Needs human and Confirm & close.
- Unclaimed means nobody is assigned. Claimed means a person or a Hive worker took it. Needs triage means a Hive worker filed it and no person has approved it yet.
- Needs human means a label such as `blocked`, `needs-human` or `stage/045-needs-decision` asks a person to act. Confirm & close means a worker thinks the issue is already done.
- **PRs by band** groups open and held pull requests. The bands are Needs human, Merge-eligible, Blocked, In review, Open and Draft.
- Merge-eligible means the pull request passed every check Hive uses before merging. Blocked means a failed check, a merge conflict, or a blocking review verdict.
- Hover any band in a chart to see its exact rule.
- The trend lines come from samples the hive takes on each status refresh, roughly every few minutes. Samples are kept for about 30 days and thinned to 96 points.
- Your browser also keeps its own samples. When you filter by repository, only the browser's samples are used.

**Numbers that look like they should match but do not.**

- The band charts count more than Actionable now. The charts include items in "Needs human" and "Confirm & close". Actionable now leaves those out. So an issue chart can total 3 while Actionable now shows 2 issues.
- The "PRs by band" chart puts held pull requests in "Needs human". The tiles count the same pull requests as Held. So the chart can show 4 needing a human while the Blocked / needs-human tile shows 0 pull requests.

**What it is good for.** Look here first each day. A growing Actionable now means Hive has work to do. A growing Blocked / needs-human means people are the bottleneck. Click a tile to filter the Projects section to the items it counts. Then add the missing decision, remove a stale label, or close finished issues.

**Example.** The tile reads `10 = 2 actionable + 0 held + 1 blocked + 7 outside`. Hive can work on 2 issues. One waits on a person. Seven were kept out by your filters. Hover the ⓘ mark on Outside to see that 2 are bot dependency dashboards. Those are expected and need no action.

**When it appears.** Always. The Overview section cannot be hidden.

**Settings that change it.**

- `dashboard.issue_bands.waiting_labels` and `dashboard.issue_bands.done_labels` decide which labels put issues in "Needs human" and "Confirm & close".
- `dashboard.issue_bands.stale_days` decides when an item counts as having had no activity for too long.
- `project.issue_filter.require_labels`, `project.issue_filter.hard_suppress_labels`, `project.issue_filter.reporter_trust` and `governor.labels.exempt` decide what counts as Outside. Change them in **Settings → Labels**.
- The chart type and trend window are saved in your browser only.
- `HIVE_COVERAGE_BADGE_URL` turns on the Test coverage tile by giving Hive the coverage badge that the quality agent already reads.

## Governor

How busy Hive thinks your projects are right now, and how often it wakes its workers as a result.

**What it tells you.** The Governor is the part of Hive that decides how often each worker runs. A worker is one of Hive's AI agents. This section shows the Governor's current mode, the work it is counting, its budget, and its schedule for each worker.

**How the numbers are worked out.**

- The **mode** is one of idle, quiet, busy or surge. Each mode runs workers more often than the one before it.
- The Governor picks a mode by comparing the amount of actionable work with three thresholds. Actionable work is work Hive could do now, with nothing waiting on a person.
- The **actionable issues** and **actionable PRs** tiles use the same count as the Overview's Actionable now tile. The two always agree.
- The **hold** list names every held item. Held means someone parked it with a hold label.
- The **cadence table** shows, for each worker, how often it runs in each mode. Continuous means it runs again as soon as it finishes.
- The **budget** shows how much of the AI spending allowance for the current period has been used.
- The **next kick** estimate follows the eval tick, which is longer while GitHub webhooks are trusted (`/api/status.webhooks.mode` reads `webhook`) and drops back to `governor.eval_interval_s` when they degrade (`polling`). How that decision is made is in [webhooks.md](webhooks.md#fallback-when-webhooks-degrade).

**What it is good for.** Check it when Hive seems too slow or too busy. If the mode stays idle while work piles up, check the thresholds. If one worker never runs, check its row in the cadence table.

**Example.** The mode reads `busy` and the gauge shows 12 actionable items. The busy threshold is 10 and the surge threshold is 20. Hive will run its workers at the busy pace until the count drops below 10.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.**

- The ⚙️ button opens the Governor settings: workers, thresholds, budget, notifications and health checks.
- The mode thresholds live in `governor.modes`. Choosing an autonomy level writes default thresholds there.
- The budget lives in `governor.budget`.

## PRs by model

Which AI models wrote the pull requests Hive opened, and how often each model's work was merged without extra fixes.

**What it tells you.** This part of the Governor section compares the AI models your workers use. It shows how many pull requests each model wrote and how they ended.

**How the numbers are worked out.**

- Each row is one model. The bar splits its pull requests into merged, still open, and closed without merging.
- **First-pass rate** is the share of merged pull requests that needed no follow-up fixes.
- **Failure rate** and **nothing to ship** count runs that failed or finished without a pull request.
- A model gets a **rank** only after it has at least 5 merged pull requests in the window. With fewer, the sample is too small to compare.
- You can show the last 7 days, the last 30 days, or all time. You can sort by rank or by count.

**What it is good for.** Use it when choosing which model a worker should use. A model with a low first-pass rate costs more review time.

**Example.** Model A has 40 pull requests and a 70% first-pass rate. Model B has 6 pull requests and a 30% rate. Model A is the safer default.

**When it appears.** Inside the Governor section, so at autonomy level L2 and above.

**Settings that change it.** No settings change this section. The minimum sample of 5 merged pull requests comes from `HIVE_CONTRIBUTE_EFFECTIVE_MODELS_MIN_PRS`.

## Advisory

Reports, advice and suggestions Hive has written about your projects and about how it is running.

**What it tells you.** This section groups five smaller sections: Advisory Digest, Hive Advice, Fleet Report Preview, Ready to level up? and Lifecycle Timeline. Each has its own entry below.

**How the numbers are worked out.** This section has no numbers of its own. Its collapsed summary repeats a count from one of the sections inside it.

**What it is good for.** Open it about once a week to read what Hive suggests. Collapse it when you do not need it.

**Example.** The collapsed title reads `2 weekly advice items`. Open it and read Hive Advice first.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6. Some sections inside it hide themselves when they are empty.

**Settings that change it.** Settings under `governor.advisory` change how often the findings are refreshed.

## Advisory Digest

The open findings Hive's workers have written down, such as bugs, ideas and advice, grouped by who wrote them.

**What it tells you.** Hive's workers record what they notice as findings. A worker is one of Hive's AI agents. The digest lists every open finding of type advisory, bug or feature.

**How the numbers are worked out.**

- The total is the number of open findings.
- Each worker's count is the number of open findings it wrote.
- Internal notes, such as tasks and decisions, are left out on purpose.

**What it is good for.** Read it to see problems your workers noticed but did not fix. Turn useful findings into issues, and close findings that no longer apply.

**Example.** The digest shows 3 findings from the scanner. One says a test is flaky. You open an issue for it.

**When it appears.** Inside Advisory, so at autonomy level L2 and above. When there are no findings it says so.

**Settings that change it.** Settings under `governor.advisory` change how often findings are refreshed and when old ones are marked stale. See the [Advisory digest guide](advisory.md).

## Hive Advice

A short weekly list of things Hive recommends you do to keep work moving on your projects.

**What it tells you.** Hive checks its own counters and your open work, then lists a few concrete steps. The list is frozen for a week so it does not change under you. The numbers inside it still refresh.

**How the numbers are worked out.**

- Some rules depend on the Governor's mode, such as "give idle workers a schedule".
- Other rules read the Overview's bands. A band is a display group named after what an item needs next.
- For example, Hive advises clearing blocked pull requests when they pass a set share of all open pull requests.
- Two count tiles show the Governor's count and the Overview chart's count. They can differ, because the chart includes items that wait on a person.

**What it is good for.** Work through it once a week. Each recommendation lists the first few items it is about and links to the full list.

**Example.** The advice says "41 of 70 blocked pull requests fail one check — fix the check first". Fixing that check unblocks most of them.

**When it appears.** Inside Advisory, so at autonomy level L2 and above. It is hidden when there is nothing to recommend.

**Settings that change it.** The thresholds under `governor.advisory` change when each rule fires. See [Owner advice](advisory.md#owner-advice).

## Fleet Report Preview

Problems Hive has noticed in itself that it would report to the Hive maintainers, and problems that have since gone away.

**What it tells you.** Hive can report its own faults to the people who build Hive. This section shows what it would report, and which earlier problems have cleared.

**How the numbers are worked out.**

- Each report row names the problem, how serious it is, and how often it happened.
- A recovered row means the problem stopped. It carries no details.
- The badge says "dry-run preview" when Hive is only showing reports, not sending them.

**What it is good for.** Read it before you allow Hive to send reports, so you know exactly what would leave your hive.

**Example.** One row says a check failed 5 times in an hour. A day later it shows as recovered.

**When it appears.** Inside Advisory, so at autonomy level L2 and above. It is hidden when there are no reports and no recoveries.

**Settings that change it.** `governor.fleet_report.file_upstream` decides whether reports are sent. The default, `false`, only previews them. See [Fleet self-reporting](fleet-report.md).

## Ready to level up?

Whether Hive thinks you could safely let it do more on its own, and what still has to be true first.

**What it tells you.** Hive's autonomy level is how much it may do on its own, from L1 (watch only) to L6 (works and merges on its own). Hive calls it the ACMM level. This section says whether to move up a level or stay.

**How the numbers are worked out.**

- Hive compares live signals, such as how often its pull requests merge cleanly, with the conditions for the next level.
- Met conditions get a tick. Unmet conditions say what is missing.
- Each repository can show its own suggested level. You can pin a repository so it keeps its level.

**What it is good for.** Check it when you are thinking about giving Hive more freedom. Fix the unmet conditions first.

**Example.** It says "stay at L4" because only 60% of pull requests merged without rework. The next level needs 80%.

**When it appears.** Inside Advisory, so at autonomy level L2 and above. It is hidden when there is no recommendation yet.

**Settings that change it.** The current autonomy level and the per-repository pins change it. See the [level-up advisor guide](acmm-advisor.md).

## Lifecycle Timeline

A timeline of recent pull requests, from when they were opened to when they were merged or got stuck.

**What it tells you.** Each row follows one piece of work through its steps. The tiles above count how many are in flight, merged or blocked.

**How the numbers are worked out.**

- **In flight** counts pull requests still moving. **Merged** counts those that landed. **Blocked** counts those that stopped.
- The section shows up to 50 rows at first. Use "Expand more" to see older rows.
- Rows with no events are left out.

**What it is good for.** Use it to see where work gets stuck. A long gap before review means reviews are slow.

**Example.** A row shows a pull request opened 3 hours ago, reviewed after 2 hours, and still waiting for checks.

**When it appears.** Inside Advisory, so at autonomy level L2 and above. With no recent events it says so instead of hiding.

**Settings that change it.** No settings change this section.

## Throughput

How many pull requests were opened, merged and closed over a chosen time window, and who did the work.

**What it tells you.** It shows the flow of pull requests and issues through your projects. It splits the work between Hive, people and other bots.

**How the numbers are worked out.**

- **Opened** counts pull requests Hive's workers created. **Merged** counts pull requests seen merging. **Closed** counts pull requests closed without merging.
- **Time to merge** shows the middle value and the slowest 10% of merges.
- **Hive vs human** is Hive's share of the work, leaving out work with no known author.
- The default window is 24 hours. You can choose from 1 hour up to all time, and filter by repository.

**What it is good for.** Watch it over a week. If merged stays well below opened, work is piling up in review.

**Example.** In 24 hours, 12 pull requests opened and 9 merged. The median time to merge is 2 hours. Review is keeping up.

**When it appears.** Always. On v5 and v6 it is a top-level section of its own.

**Settings that change it.** The window, repository and role controls are in the section. How far back data goes depends on how long the audit log keeps events. See [Throughput](dashboard.md#throughput).

## Tokens

How much text the AI models read and wrote for Hive recently, broken down by model and by worker.

**What it tells you.** AI models are billed by tokens, which are small pieces of text. This section shows how many tokens Hive used and who used them.

**How the numbers are worked out.**

- The section adds up tokens from every recorded session in the window, which is 24 hours by default.
- It splits them into input, output and cached tokens, and by model and worker.
- The burn rate is tokens per hour.

**What it is good for.** Look here when costs jump. A worker with a much higher burn than the others may be stuck in a loop.

**Example.** The section shows 2 million tokens in 24 hours, and one worker used half of them. You check that worker's recent runs.

**When it appears.** At autonomy level L3 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** No settings change this section. Which models and providers you configure changes what it can record. See [Token tracking](token-tracking.md).

## Cost

An estimate of what Hive's use of AI models has cost so far, based on public list prices.

**What it tells you.** It turns token counts into money, so you can see what Hive costs.

**How the numbers are worked out.**

- Cost is tokens multiplied by each model's public list price. The badge "est." is a reminder that it is an estimate.
- If you pay a flat subscription, your real bill may differ.
- **Cost per merged PR** and **cost per closed issue** divide the estimate by how many were merged or closed. With none, they show a dash.
- The merged and closed counts behind those two figures are kept by Hive itself: every `pull_request closed` (merged) and `issues closed` webhook event for the hive's own author adds one, and once an hour (`forgestate.CountsReconcileInterval`) the counts are re-read from GitHub search and overwritten so drift from missed events is corrected. Search is no longer queried on every 5-minute collection.
- When that hourly check is rate-limited or fails, the last good counts stay in use and the count label reads **stale since <time>**. The cost API carries the moment as `count_stale_since` and names where the counts came from in `count_source` (`forgestate` or `search`).
- When your AI gateway reports its own spending, that figure appears too.
- The spend-over-time chart has a range selector: **hourly** (the current clock hour and the 23 before it, one bar per hour), **daily** (the last 14 calendar days including today, one bar per day — labelled by weekday when 7 or fewer are shown, by month/day beyond that), **weekly** (the current Monday→Sunday week and the 4 before it, one bar per week labelled by its Monday; the rest of this week is empty), **monthly** (the current calendar month and the 4 before it, one bar per month labelled by name) and **custom** (your own start and end; hour bars up to 2 days, day bars beyond). Boundaries follow your browser's clock.
- A **−/+ stepper** right of the range chips grows or shrinks the window, showing the current size with its unit (**24 h**, **14 d**, **5 wk**, **5 mo**): hourly moves in steps of 6 hours between 6 and 72, daily in steps of 7 days between 7 and 60, weekly one week at a time between 2 and 26, monthly one month at a time between 2 and 24. The chips are disabled at the bounds, work from the keyboard, and the chosen size is remembered per range in this browser. Custom has no stepper — its bounds are explicit.
- **Spend in selected range** adds up the bars of that same window, and **run-rate** divides it by the part of the window that has elapsed so far — per hour for hourly bars, per day otherwise. Switching the range or stepping the window changes the chart and both figures together.
- **How far back the data goes.** The hive keeps about 30 days of 5-minute cost snapshots, and folds each completed day (UTC) into a per-day total that is kept for about two years (`daily_rollup` on `/api/cost`, persisted in `/data/cost-daily-rollup.json`). Bars inside the last 30 days are built from the snapshots; older bars use the per-day totals, so a wide weekly or monthly window still shows real spend. A bar that neither source reaches — earlier than the first recorded data point — is drawn as a dim hatched placeholder rather than a $0 bar, and the summary adds **history starts <date>** so you know where the real data begins. The per-day totals start accruing the day the hive first runs this version, so on an upgraded hive they reach back only as far as the snapshots that were present then.

**What it is good for.** Use it to judge whether Hive is worth its cost. Compare cost per merged pull request over time.

**Example.** The estimate is $40 this month for 80 merged pull requests, so about $0.50 each.

**When it appears.** Always.

**Settings that change it.** The models you configure and their prices change it. Gateway credentials add the gateway's own spending. See [Token tracking](token-tracking.md).

## Projects

One card per repository Hive looks after, listing its open issues and pull requests grouped by what each one needs next.

**What it tells you.** Each card is one repository. Its issues and pull requests are sorted into bands. A band is a display group named after what the item needs next.

**How the numbers are worked out.**

- The cards use the same bands as the Overview charts. See the Overview entry for each band's meaning.
- Small pills on each item show signals, such as a hold, a failing check or no recent activity.
- A `🔗` pill on an issue means Hive has verified an open or merged pull request covers it. Hover it: "closes via PR #N" means merging the PR closes the issue; "covered by PR #N (partial)" means the PR says `Refs #N` and merging it does not close the issue. The pill comes from Hive's own PR-claim evidence, so it appears on every issue labelled `🐝 covered by PR` — including ones Hive has parked in the "covered by PR" group because an outside PR already fixes them — and never depends on GitHub having a closing link.
- A red pull-request pill means the sentinel flagged it for a possible security override, privilege escalation or codebase damage. Hive will not merge it until a maintainer reviews it and removes the sentinel label. Pull requests from trusted authors usually get a sentinel comment instead of the label, so they are usually not red.
- The **needs-human** count in the header counts open pull requests that need a person to review or decide. Needs-human means waiting on a person.
- Each card ends with a collapsible **Filter** section. Press **🔎 Filter** in the Projects toolbar to open the facet bar: tick labels (every label present on the repo's open issues and pull requests is offered, with a live count), add free-text terms (`"rate limit"` matches as a phrase, unquoted words must all appear, `-term` excludes; matching is case-insensitive over title, number and labels), and narrow by type (issue / PR), state (claimed, unclaimed, parked, covered by PR) or author kind (hive / human / bot). Labels and terms match **any** by default; switch to **all** for AND. The section lists the matching items for that repo — number, title linked to GitHub, one pill per label, age, and the PR icon for pull requests — and is collapsed by default only while it is empty; its open/closed state is remembered per repo.
- The same selection applies to every card. Name it and press **Save** to keep it; saved filters appear as chips next to the built-in presets and can be renamed, updated or deleted inline (no browser dialogs). Presets cannot be deleted but can be hidden. **Needs human** is the default preset: issues and pull requests carrying the hard-suppress labels (`needs-direction`, `needs-decision`, `needs-spec`, `needs-human` — honouring `issue_filter.hard_suppress_labels` overrides), `needs-signal`, or a hold label (`hold`, `on-hold`, `hold/review`, the hive's own hold label), sorted in that priority order so operator decisions come first. The label set comes from the server (`labelPresets[]` on `/api/status`), so it never drifts from the labels Hive itself parks on (ADR-0019). Saved filters, the active selection and hidden presets are stored in the browser's localStorage per hive (`hive-label-filters:<hiveId>`).
- Any saved filter or preset can be **pinned to the sidebar** (☆ Pin to sidebar in the facet bar). Pinned filters appear in a **Filters** group at the bottom of the left navigation, each with a live count of matching items across every project. Clicking one opens a **cross-repo view** in place of the project cards: one list of matching issues and pull requests grouped by repository (groups collapse and remember their state), with the facet bar on top so you can refine and re-save. **Needs human** is pinned by default; unpin it from the facet bar. Choosing any other sidebar entry, or **← Project cards**, returns to the cards. Pin state is stored with the saved filter.

**What it is good for.** Use it to act on single items. Open the legend above the cards to learn what each pill means.

**Example.** A card shows 3 issues under "Confirm & close". You check each one, and close those whose fix has landed.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.**

- The repositories Hive watches are set in **Settings → Projects**.
- The `dashboard.issue_bands` settings change the band rules.
- See [Repository card legend, issue bands, and PR bands](dashboard.md#repository-card-legend-issue-bands-and-pr-bands).

## ACMM Eval

How ready your repositories are for Hive to work on them alone, scored against a published checklist.

**What it tells you.** ACMM is the checklist Hive uses to decide how much it may do on its own. This is called the autonomy level, from L1 to L6. This section scores your repositories against that checklist.

**How the numbers are worked out.**

- **Codebase Readiness** scores the repositories themselves, such as tests and documentation.
- **Operational Autonomy** scores how Hive is running.
- **Overall ACMM** is the level both scores support. **Criteria Passed** counts the checks that pass.
- With several repositories, a criterion passes if any repository passes it.
- A waived criterion is marked as waived, not as failed.

**What it is good for.** Use it to find what stops you reaching a higher level. Click **Open Issue** on a failed criterion to file the work.

**Example.** Overall shows L3 because "has a contributing guide" fails. You add the guide and click **Re-evaluate**.

**When it appears.** Always.

**Settings that change it.**

- The level you choose with **Change level** changes the target.
- Waivers in a repository's `.acmm.yml` file mark criteria as waived.
- `governor.acmm.issue_tracker` decides where **Open Issue** files.
- See the [ACMM policy matrix](acmm-policy-matrix.md).

## Approvals

Actions Hive wants to take that are waiting for you to approve or reject them.

**What it tells you.** Some actions need a person's yes before Hive does them. This section lists them.

**How the numbers are worked out.** The badge counts pending approvals. Each row is one request. Select rows, then choose **Approve selected** or **Reject selected**.

**What it is good for.** Check it whenever the badge shows a number. Hive waits until you decide.

**Example.** Hive asks to merge a pull request in a protected repository. You read it and approve it.

**When it appears.** Only when the approval desk is turned on. Only owners can read and decide approvals.

**Settings that change it.** `tool_approval.enabled` turns the section on. The rules under `tool_approval` decide which actions need approval.

## Audit Log

A searchable record of everything Hive has done, newest first. The collapsed “sensitive” badge opens a 24h sensitive-only view, matching the server-side risk count.

**What it tells you.** Every action Hive takes is written to the audit log. This section lets you read and search it.

**How the numbers are worked out.**

- The collapsed summary counts today's events.
- Its **sensitive** badge counts sensitive audit actions over the last 24 hours. Click it to expand the card, apply the visible **Sensitive only** filter, and scroll to the first matching row.
- The search box matches any of the words you type. Wrap text in slashes, like `/merge.*failed/`, to search with a pattern.
- The match count shows how many events match.

**What it is good for.** Use it to answer "what did Hive do, and when?". Save searches you use often with **Save**.

**Example.** You search `merged` and see that Hive merged 4 pull requests this morning.

**When it appears.** Always. People without enough access see an error instead of events.

**Settings that change it.** How long events are kept depends on the audit log settings. See the [Audit log guide](audit-log.md).

## Review

Review Pipeline shows pull requests by stage first; Review Queue lists the same open pull requests in Hive's suggested review order.

## Review Pipeline

Each open PR in the stage the review pipeline derived for it (GET /api/review/pipeline), with reviewers, P0–P3 findings, the fix-loop counter and the next action.

**What it tells you.** Each column is one stage of the review loop: unreviewed, reviewing, changes requested, fixing, waiting for a human, approved, merged and abandoned. Each card shows the pull request, who has reviewed it, how many findings were raised at each severity and what should happen next.

**How the numbers are worked out.** Hive reads the last review results it recorded for each pull request and places the card in a stage from those results alone. The collapsible Pipeline tile header shows how many pull requests are on the board. Nothing is fetched from GitHub when you open it.

**What it is good for.** See at a glance where review work is piling up, and which pull requests have gone round the review loop enough times that a person should step in.

**Example.** Three cards sit in "changes requested" and one has looped twice. The next action on that card says a person should look at it.

## Queue

Every open PR across the governed repos in one deterministic rank order (GET /api/review/queue), regardless of author type, with the reasons for each position.

**What it tells you.** It ranks open pull requests from all your repositories in one list, whoever wrote them.

**How the numbers are worked out.** Hive scores each pull request by its review priority and history, then sorts them. The collapsible Queue tile header shows how many are in the queue. The order is the same every time for the same data.

**What it is good for.** Review from the top when you have time. The reasons tell you why an item is high.

**Example.** The first pull request is small, its checks pass and it has waited 3 days. It is quick to review.

**When it appears.** Always. The Review section is empty when there are no open pull requests.

**Settings that change it.** The review settings in **Settings** change the loop cap, ranking rules and which repositories are included. See [Review queue triage](review-queue-triage.md).

## Experiment Lab

An experimental planner that tries out code changes in your repositories and records what it learned.

**What it tells you.** The Experiment Lab (formerly the Strategy Lab) runs small experiments on your repositories' code. It lists each running experiment as a card — the same card list Inception uses — and a ledger of results.

**How the numbers are worked out.** Each research question is its own experiment card. The card shows the question, the repos it may touch (scope pills; none means hive-wide), a status pill, a stage line that says what happens next (*collecting baseline*, *ready — generate proposals*, *proposals awaiting your review*, *running*, *concluded*; on a hive where the external experiment loop has written nothing for 24 evaluation cycles a ready card in Suggest/Evolve mode reads *ready — the experiment loop is not running on this hive* instead of waiting forever — Hive itself designs no proposals, see [The experiment loop](strategy-lab.md#the-experiment-loop)) and a baseline bar **N / 672** — 672 snapshots is ≈ 7 days of observations at 4 an hour, explained by the ⓘ next to it. **+ New experiment** is the only place an experiment is defined: it opens a *New experiment* dialog for the question, an optional hypothesis and the scope pills, and the card appears at 0 / 672 immediately. **✎ Edit** opens the same dialog pre-filled as *Edit experiment #N*; only one add/edit dialog is open at a time, so a draft is never silently replaced. The scope pills offered come from the ⚙ dialog's allow-list (*Repos experiments may target*; every repo when the list is empty) — the ⚙ dialog itself holds only lab-wide guardrails (trust ladder, output mode and labels, allow-list), never a research question. Several experiments can run at once as long as no two running ones share a repo (or are both hive-wide); the lab refuses the overlap with an in-app warning and offers to pause the other experiment or narrow the scope, because two interventions on the same repos would confound each other's results. Each card has compact icon actions — **✎ Edit**, **⏸ Pause** / **▶ Resume**, **🗄 Archive** (each with a tooltip) — while **Generate proposals** stays a labelled button. Editing the hypothesis saves in place; changing the question or scope asks *Restart experiment?* first, because the baseline and proposals so far belong to the old version: the old card is archived and a new one starts at 0 / 672 showing *v2 · restarted from #1*. If the allow-list is narrowed later, existing experiments keep their scope but show a muted warning. Archived cards move to **Previous experiments** with their results. The **Principles** tile counts durable lessons distilled from judged experiments — 0 on a new lab is normal, and the ⓘ says so. There is no lab-wide progress bar — the whole-hive snapshot count is a total, not progress — but a compact muted **Data collection** note under the cards gives *Observation history: N snapshots since <date>*, what each snapshot records, the cadence (4 an hour) and retention (24 h raw, then hourly buckets for 30 days).

**What it is good for.** Try it only if you want Hive to propose code changes on its own. You approve or stop each experiment.

**Example.** You add the experiment "Can we cut CI time by 20 %?" scoped to `acme/api`; its card reads 0 / 672 and *collecting baseline*. A week later it reads *ready — generate proposals*; you press the button, the lab proposes splitting a slow test package across two CI jobs, you approve it, and the ledger records the result. Meanwhile a second card, "Fewer flaky tests?", runs on `acme/web` — but a third one on `acme/api` is refused until you pause the first or narrow its scope.

**When it appears.** Only when the Experiment Lab setting is on and the autonomy level is L4 or above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** `dashboard.experiment_lab: true` turns it on (`dashboard.strategy_lab` is a deprecated alias for one release). See [Experiment Lab](strategy-lab.md).

## Inception

A step-by-step guide that turns a new project idea into a first set of files and planned work.

**What it tells you.** It walks you through describing a project. Hive asks questions, proposes a structure and creates the first files.

**How the numbers are worked out.** This section has no numbers.

**What it is good for.** Use it when starting a new project with Hive. Answer its questions, review the plan, then approve it.

**Example.** You describe a small web service. Inception asks two questions and then proposes a folder layout and five starter issues.

**When it appears.** Always.

**Settings that change it.** No settings change this section. See [Inception](inception.md).

## Knowledge

The facts Hive has learned about your projects, where they came from, and how much Hive trusts each one.

**What it tells you.** Hive's workers keep notes about your projects as facts. This section lets you search, add, edit and remove them.

**How the numbers are worked out.**

- The summary counts all stored facts.
- Each fact has a confidence score. It rises when the fact is confirmed again and falls with time.
- Layers show where facts come from, such as one project or a shared source.

**What it is good for.** Check it when a worker keeps making the same mistake. Correct the wrong fact here.

**Example.** A fact says tests run with `make test`, but your project uses `go test`. You edit the fact.

**When it appears.** Always.

**Settings that change it.** Knowledge sources, shared sources and imports are managed inside the section. See the [knowledge curator guide](knowledge-curator.md).

## Contributors

The people who lend their computers or AI helpers to your projects, and how far Hive trusts each of them.

**What it tells you.** A contributor is a person who signed up to help. Some connect their own AI worker. This section lists them and their trust level.

**How the numbers are worked out.** The summary shows active contributors and all registered contributors. Active means seen recently.

**What it is good for.** Use it to give trusted people more access, or remove access. Share the `/contribute` link to invite people.

**Example.** A contributor has fixed 10 issues cleanly. You raise their trust so they can take bigger tasks.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** Trust and roles are changed on each contributor card. See [Contributor trust and roles](contributor-trust-and-roles.md).

## Diagnostics

Health checks for Hive itself, such as its connections, credentials and safety switches, to help you find out why something is not working.

**What it tells you.** Each tile checks one part of Hive. Examples are the code hosting connection, AI provider logins and safety switches that stop work after repeated failures.

**How the numbers are worked out.**

- Each tile reads the latest health data from the hive.
- A red or amber tile explains what failed and what to try.
- On v5, a **Platform** tile shows which code hosting service is connected and which optional services are on. On v6 this is its own section.
- **Quality stats** come from the checks your quality worker is set up to run.

**What it is good for.** Open it when something stops working. Fix the first red tile first.

**Example.** A tile says the AI provider login expired. You log in again from the worker's card.

**When it appears.** At autonomy level L3 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** Provider credentials, code hosting settings and the quality worker's stats change the tiles. See [Dashboard route and health checks](health-checks.md).

## Agent Logs

The raw text output of each worker, for following what it is doing line by line.

**What it tells you.** It would show a worker's output as it runs. A worker is one of Hive's AI agents.

**How the numbers are worked out.** This section has no numbers.

**What it is good for.** Today you cannot open it. Use the terminal or log links on each card in the Agents section instead.

**Example.** To read the scanner's output, open its card in Agents and choose its log link.

**When it appears.** Never, on both v5 and v6. The dashboard always hides it.

**Settings that change it.** No settings change this section. See [Agent logging](agent-logging.md).

## Agents

Each worker Hive runs, what it is doing now, and buttons to pause, restart or configure it.

**What it tells you.** A worker is one of Hive's AI agents. Each card shows a worker's state, model, schedule and repositories. Under the state, a **Now:** line says which issue or pull request the worker is on, or that it is idle.

**How the numbers are worked out.**

- The header counts running workers and all workers.
- A worker's state comes from the hive, for example running, idle, paused or needs login.
- A worker outside your autonomy level's usual set still shows when it is running or turned on.
- The **Now:** line is on every card, in the full and the compact layout, and in the worker's detail panel. The next part explains its words.

**The "Now:" line.** Hive starts a worker on a round of work, then the worker stops until Hive starts it again. The line shows the last issue or pull request the worker itself changed through the hive during its current round of work. These count:

- a comment it posted,
- a label it asked for,
- an issue it reserved, opened or closed,
- a pull request it opened, reviewed, closed or merged,
- a review it asked someone for.

These do not count:

- an issue that was only in the list of work Hive gave the worker,
- changes the hive makes on its own, such as the reservation comment and label it posts, labels it adds by itself, and the Governor's own approvals and merges,
- a branch push on its own, because it has no issue or pull request number. The pull request the worker then opens does count.

| The line says | What it means |
| --- | --- |
| `Now: console#123 — Fix the login redirect · 2 min ago` | The worker is working. The last item it changed this round is number 123 in the `console` repository, 2 minutes ago. The number links to the issue or pull request on GitHub. |
| `Now: console#123 · 2 min ago` | The same, but Hive does not have the item's title, so only the number shows. |
| `Now: acme/console#123 …` | The repository is written in full when Hive does not watch it, or when two watched repositories share the short name. |
| `just now`, `5 min ago`, `3 hours ago`, `2 days ago` | How long ago the worker last changed the item. Under a minute reads "just now". The time keeps counting up between updates, without a reload. |
| `Now: working — no issue or pull request yet · started 12 min ago` | The worker is working, but it has not changed any issue or pull request through the hive since this round started, 12 minutes ago. It may be reading its list of work, or writing code it has not pushed yet. It does not mean the worker is doing nothing. A long time here is a reason to open its log. |
| `Idle` | The round of work has ended. The worker waits until Hive starts it again. |
| `Idle · last: console#123, 40 min ago` | Idle. The last item it changed since the hive last started was `console#123`, 40 minutes ago. An item after "last:" is past work, not current work. |
| `Paused` | Someone paused the worker. No item is shown. |
| `Off` | The worker is switched off, or the Governor's current mode does not run it. No item is shown. |
| `Stopped — see the agent's log` | The worker's program is not running, and nobody paused it or switched it off. For example, it crashed or failed to start. Open its log to find out why. No item is shown. |
| `Not started yet` | The worker is starting, or Hive has not started it on any work since the hive came up. No item is shown. |
| Another word, such as `on demand` | The card's own state, repeated. No item is shown. |
| `reserved until 14:30` | Hive has reserved the item shown for this worker until 14:30. |
| `still reserved: console#7 until 14:30` | The worker holds a reservation on an issue other than the one shown, or holds one while it is idle. |
| `and 2 more` | The worker holds more reservations than the card has room for. The card shows the one that ends first. |
| `information may be out of date` | The dashboard missed three updates in a row, so the times stop counting. Check that the hive is running, then reload the page. |

More about the line:

- **It only sees what happens through the hive.** If a worker pushes with `git` directly, or runs a command outside the hive, the line does not see it. So "no issue or pull request yet" means "nothing seen through the hive yet", not "doing nothing".
- **A reservation is not proof the worker is still working.** Hive can reserve an issue for a worker so that others leave it alone. Hive calls this a claim. A reservation lasts until its end time, even after the worker has moved on. Hive renews it each time it starts the worker on that issue again. So "reserved until" and "still reserved" only say that the issue is held. What the worker is doing is the item after "Now:".
- **Reservation times** come from the hive's own record of reservations. The reservation comment on GitHub is not updated when Hive renews a reservation, so the dashboard can show a later time than the comment. Times are in your local time, with the date when it is not today. A reservation disappears from the line as soon as it ends.
- **Nothing from before is shown as current.** An item from an earlier round of work only appears after "last:". Hive keeps this record in memory only, so after the hive restarts no card shows an item until its worker changes one again.
- **Times use the hive's clock**, not your computer's. They are right even when your computer's clock is wrong, and they are never negative.
- **Updates.** The line updates with each worker update, every 10 seconds by default, so a change shows within about 15 seconds.
- **Privacy.** Titles come only from the issue and pull request lists the dashboard already shows you. If it shows you no repositories' issues, the line shows no item numbers either.

**What it is good for.** Use it to pause a worker, restart a stuck one, or change its settings. Fix any card that says it needs a login. Read the **Now:** lines to see what each worker is on without opening its log.

**Example.** A card says "needs login". You click **Login** and sign in again. Another card says `Now: working — no issue or pull request yet · started 50 min ago`. That worker has changed nothing through the hive for 50 minutes, so you open its log to see what it is doing.

**When it appears.** Always.

**Settings that change it.**

- Each card's ⚙️ button opens its settings: schedule, model, tools and permissions. See [Agent configuration](agent-configuration.md).
- `governor.claims.enabled` turns reservations on. When it is off, which is the default, the line shows nothing about reservations.
- `dashboard.agent_poll_interval_s` sets how often worker updates are sent. The default is 10 seconds.

## Hive activity monitor

A progress strip per open issue and pull request showing which step of the pipeline it is on and who must act next.

Fed by `GET /api/stage-tracker`. Every issue and pull request follows the same pipeline, named once in the column header above the list: `Open → Actionable (010) → Claimed (020) → PR in progress (030/040) → CI / Review (050/060) → Merge gate (080/100) → ✓ merged`. Each entry is a title line over a thin chevron strip. Only the current step is coloured: it is filled amber, carries the step name in words and pulses (static under `prefers-reduced-motion`); once the PR is merged the final ✓ chevron is the lit one, in green. Every other step is a low-contrast shape with a small grey glyph (circle, bolt, lock, branch, eye, shield, check) — passed steps a subtle neutral fill, steps ahead a faint outline. Side stages are not extra steps: `070 Fixing` (the author's move; the `needs-rebase` chip next to it says what is being fixed — the stage labeller always parks a needs-rebase PR at 070/071/072, never at CI / Review), `071 Needs author` (human-authored), `072 Returned to contributor` (clanker-authored) and `074 Adopted by hive` light *CI / Review* with their own name (an adopted PR stays on 074 through the hive's pushes going pending, green or red again, and moves on only when *Merge gate* takes it); `085 Needs human decision` (PR-only) and `090 Ready for human merge` light *Merge gate* with their own name (the 085 tooltip says what must be decided); an issue parked on a person wears `045 Needs human decision` and lights *Actionable* (the issue side) with that name — a pre-045 issue still wearing 085 is drawn the same way until the reconciler re-stages it; an actionable issue no maintainer has looked at yet wears `015 Triage needed` (carrying `needs-triage`, or held in the reporter-trust gate's `reporter_triage` bucket — never derived from who filed it) and lights *Actionable* as "Triage needed" (a human's move) — so the Actionable column reads "Actionable", "Triage needed" or "Needs human decision", and the Open column is untouched. An issue carrying a hold-class label (`hold`, `do-not-merge*`, `hive-pause/<id>`) is never parked: it sits at *Open* as "Open · on hold" even beside `needs-human`, and the reconciler removes any 045 the hold caught it with ([#13012](https://github.com/hivecommons/hive/issues/13012)). The lit chevron ends with an ink-coloured icon of whose move it is — person (human), robot (clanker) or hexagon (hive): the claim holder on Claimed, the PR author on PR in progress and Returned to contributor, hive on CI / Review, Adopted by hive and Ready for auto merge, a human on Needs human decision and Ready for human merge. `040 Covered by PR` shares the *PR in progress* column with 030; `060 Reviewed`, `080 Awaiting merge admission` and `100 Merge queue` all light *Merge gate* as "Ready for auto merge" (the chevron tooltip says "not yet admitted — <reason>" for 060/080 and "admitted, merging" for 100); `095 Pre-merge soak` lights the same column as "Soaking" (hive's move; tooltip "admitted, soaking — Xm left", headline "Pre-merge soak — Xm left" from the payload's `soak_remaining_seconds`, aged from the lane's last sweep), so an admitted PR inside its soak window is no longer drawn as merging; the chevron's own tooltip names the exact stage. Covering pull requests nest under their issue. An issue with no PR yet draws the whole strip like any other row, lit at Open, Actionable or Claimed with the steps ahead faint. Once exactly one PR covers it, issue and PR are a single entry: the issue's `#N title` on the left, `[author icon] #PR title` under the *PR in progress* header on the same title line, and one continuous strip — Open, Actionable and Claimed passed, the PR's stage lighting the chevron from PR in progress through CI / Review, Merge gate and ✓; the chip, tooltip and `since` are the PR's. With two or more covering PRs the issue's own strip stops at Claimed — all passed, nothing on the issue line lit. Directly beneath, tighter than the gap between entries, sits one indented sub-row per open covering PR, furthest along first: `[author icon] #N title` at the *PR in progress* column, then that PR's own strip from there on — `PR in progress → CI / Review → Merge gate → ✓` — with its own lit chevron, chip, tooltip and `since`. So an issue with two PRs, one at 030, shows "PR in progress" lit on that sub-row, not on the issue. A PR linked to an open issue in the list has no top-level row of its own; a PR with no linked issue keeps a full-width row and shows the first steps as passed. A PR whose only linked issue is already closed — a superseded PR whose body still says `Fixes #N` after an earlier PR fixed it — keeps its own row with the closed issue in the issue slot: `#N title` as a link, muted and struck through, followed by a small `closed` chip, so the pairing reads at a glance; when the title cannot be resolved the slot says `#N (closed issue)` rather than a bare number (the title comes from the snapshot, or from one cached issue read the cycle makes once per issue — see [Linked issues](dashboard.md#pr-review-and-link-signals)). The *Pull requests* filter flattens the list — every PR as a top-level row; *Issues* and *All* show the nested view. Each row is named once on the left; a covering PR appears under the *PR in progress* header as `[author icon] #N title`. A pull request with no linked issue is named once: `[PR icon] [author icon] #N title` on the left over one continuous strip (first steps passed, its own step lit) and nothing in the *PR in progress* column, which is reserved for covering PRs — so a title never appears twice in a row. Pull requests are never issues here: when a number arrives through both the issues feed and the pulls feed the PR record wins and the issue copy is dropped on the server (pairing is keyed by repository, kind and number), and the *Issues* filter never lists a PR. The author icon is the one coloured legend: hive hexagon in blue, clanker robot in purple, human silhouette in teal (the `author/*` kind, inferred from the snapshot when the label is absent; the login is in the `#N` link's tooltip). The right-hand `since` column, on the title line, is how long the item has been on its step (red when `stalled`). At most one muted chip sits to its left on the title line: the most important health modifier (`stalled`, `regressed`, `hold`, `needs-human`, `needs-decision` — shown as "needs human decision" —, `needs-triage` — shown as "triage needed" (amber) on an issue still carrying the `needs-triage` label at a stage other than 015, whose own chevron already says so —, `needs-rebase`, `blocked`, `draft`) with a `+N` for the rest. Everything else — the reason sentence, who must act, nudges sent, the repository, all flags and the last stage hops — is in the hover tooltip on the item's `#N` link (on a nested sub-row, on its `#N` PR link). Hover text in a row lives only on the `#N` link, the covering-PR link, the title and the lit chevron (its stage, and whose move it is); passed and upcoming chevrons, the kind and author icons, chips and the `since` column have none. Title lines never wrap or push the strip out of alignment: the issue title is truncated with an ellipsis before the *PR in progress* column (a clear gap always separates it from the PR entry, which starts at the column edge) and the PR title before the chips and `since`; hovering a truncated title shows the full text. Rows are sorted furthest-along first by default — a covered issue ranks by its furthest-along PR — then longest on the step first; *Sort* also offers longest waiting, newest and number, and the arrow beside it reverses the order. Every open issue and pull request of the watched repositories is listed — actionable, held, claimed, in progress, covered by a PR, filtered by a `needs-*` label, reporter-triage, draft and exempt-labelled alike; an item without a stage label or journal entry sits at *Open*, and one in a scanner bucket with no label yet is placed from the bucket (claimed → 020, in progress → 030, covered → 040). Filters work on the cached payload in the browser and persist across reloads (the page number does not): kind chips, *Repo*, a search box (`#number` / number / title substring, also matching a covering PR), *Since* (any, over 1 hour / 1 day / 1 week on the current step, or past the stage's configured dwell — stalled items always qualify), *Author* kind, *Needs attention* (anything with a health modifier or on 071 / 072 / 085 / 090), and label chips built from the labels present (`stage/*`, `needs-*`, `author/*`, `role/*` always; other labels when two or more items share them; chips of one family OR, families AND; counts on each chip). *Clear filters* shows while anything is narrowed. The list is paged — 25 / 50 / 100 rows, default 50 — with Prev / Next and "showing X–Y of N (M filtered out)" above and below; the header count "N items · M need attention" is for the filtered set, the sidebar badge for everythingthe chevron's own tooltip names the exact stage. Covering pull requests nest under their issue. An issue with no PR yet draws the whole strip like any other row, lit at Open, Actionable or Claimed with the steps ahead faint. Once exactly one PR covers it, issue and PR are a single entry: the issue's `#N title` on the left, `[author icon] #PR title` under the *PR in progress* header on the same title line, and one continuous strip — Open, Actionable and Claimed passed, the PR's stage lighting the chevron from PR in progress through CI / Review, Merge gate and ✓; the chip, tooltip and `since` are the PR's. With two or more covering PRs the issue's own strip stops at Claimed — all passed, nothing on the issue line lit. Directly beneath, tighter than the gap between entries, sits one indented sub-row per open covering PR, furthest along first: `[author icon] #N title` at the *PR in progress* column, then that PR's own strip from there on — `PR in progress → CI / Review → Merge gate → ✓` — with its own lit chevron, chip, tooltip and `since`. So an issue with two PRs, one at 030, shows "PR in progress" lit on that sub-row, not on the issue. A PR linked to an open issue in the list has no top-level row of its own; a PR with no linked issue keeps a full-width row and shows the first steps as passed. The *Pull requests* filter flattens the list — every PR as a top-level row; *Issues* and *All* show the nested view. Each row is named once on the left; a covering PR appears under the *PR in progress* header as `[author icon] #N title`. A pull request with no linked issue is named once: `[PR icon] [author icon] #N title` on the left over one continuous strip (first steps passed, its own step lit) and nothing in the *PR in progress* column, which is reserved for covering PRs — so a title never appears twice in a row. Pull requests are never issues here: when a number arrives through both the issues feed and the pulls feed the PR record wins and the issue copy is dropped on the server (pairing is keyed by repository, kind and number), and the *Issues* filter never lists a PR. The author icon is the one coloured legend: hive hexagon in blue, clanker robot in purple, human silhouette in teal (the `author/*` kind, inferred from the snapshot when the label is absent; the login is in the `#N` link's tooltip). The right-hand `since` column, on the title line, is how long the item has been on its step (red when `stalled`). At most one muted chip sits to its left on the title line: the most important health modifier (`stalled`, `regressed`, `hold`, `needs-human`, `needs-decision` — shown as "needs human decision" —, `needs-rebase`, `blocked`, `draft`) with a `+N` for the rest. Everything else — the reason sentence, who must act, nudges sent, the repository, all flags and the last stage hops — is in the hover tooltip on the item's `#N` link (on a nested sub-row, on its `#N` PR link). Hover text in a row lives only on the `#N` link, the covering-PR link, the title and the lit chevron (its stage, and whose move it is); passed and upcoming chevrons, the kind and author icons, chips and the `since` column have none. Title lines never wrap or push the strip out of alignment: the issue title is truncated with an ellipsis before the *PR in progress* column (a clear gap always separates it from the PR entry, which starts at the column edge) and the PR title before the chips and `since`; hovering a truncated title shows the full text. Rows are sorted furthest-along first by default — a covered issue ranks by its furthest-along PR — then longest on the step first; *Sort* also offers longest waiting, newest and number, and the arrow beside it reverses the order. Every open issue and pull request of the watched repositories is listed — actionable, held, claimed, in progress, covered by a PR, filtered by a `needs-*` label, reporter-triage, draft and exempt-labelled alike; an item without a stage label or journal entry sits at *Open*, and one in a scanner bucket with no label yet is placed from the bucket (claimed → 020, in progress → 030, covered → 040). Filters work on the cached payload in the browser and persist across reloads (the page number does not): kind chips, *Repo*, a search box (`#number` / number / title substring, also matching a covering PR), *Since* (any, over 1 hour / 1 day / 1 week on the current step, or past the stage's configured dwell — stalled items always qualify), *Author* kind, *Needs attention* (anything with a health modifier or on 071 / 072 / 085 / 090), and label chips built from the labels present (`stage/*`, `needs-*`, `author/*`, `role/*` always; other labels when two or more items share them; chips of one family OR, families AND; counts on each chip). *Clear filters* shows while anything is narrowed. The list is paged — 25 / 50 / 100 rows, default 50 — with Prev / Next and "showing X–Y of N (M filtered out)" above and below; the header reads "N items (I issues / P PRs) · M need attention" — N, I and P are the whole list, the same `openItems` set the Overview tile and its left-menu counter print, so N always equals the tile's **Total open issues + Total open PRs**; a "· X match filters" note follows while a filter is on. The sidebar counter beside **Hive activity monitor** is that same N — total open items, written in the same render pass as the header, so the two cannot drift apart; how many need attention is the warn-coloured header suffix and the counter's hover tooltip (counted over every item, filters or not). When covering pull requests are nested under their issue the "showing X–Y of R" line counts rows and adds "(P nested under their issues)" so R never reads as a different total. The small monospace strip beside the count — `engine shadow · N mismatches · N anomalies` — is the central stage engine ([ADR-0025](adr/0025-central-stage-engine.md)): `engine` names the current `governor.labels.stage.engine.mode` (grey `off`, neutral `shadow`, green `enforce`; hover for what the mode does and the derivation count), `mismatches` counts items where the engine and the sweep derived different stages in shadow (red when non-zero; hover lists the last five), `anomalies` counts stage moves the engine's admission table rejected (red when non-zero). The same numbers are `/api/status.stageEngine`; the mode is changed under **Settings → Labels → Central stage engine mode** and the rollout runbook is in [operator-reference.md](operator-reference.md#central-stage-engine-rollout). "updated Ns ago · cache" says how old the server-built payload is: the spoke rebuilds it at most every `dashboard.stage_tracker.cache_ttl_s` (default `governor.eval_interval_s`) from its existing snapshot and serves it with an `ETag`, so the browser's 30-second poll is a 304 and no request ever reaches GitHub (`/api/gh-rate-limits` lists the caller `dashboard:stage-tracker` with nothing charged). See [Stage labels](labels-and-control-signals.md#stage-labels-stagennn--the-pipeline-state-machine).

**Sounds.** The 🔇/🔊 chip at the right end of the toolbar turns on two optional, browser-made sounds: a "bug zapper" that plays when a pull request already on the list merges while you have the page open (never on first load, never for issues, once per pull request, at most two zaps per refresh, silent while the tab is hidden), and a short bee-like buzz when an issue or pull request first appears on the list (never on first load, once per item, at most two buzzes per refresh, same hidden-tab rule). Both are off by default and remembered per browser; switching them on plays each sound once so you know what to listen for. They fire for every repo the hive governs — there is no primary-repo scoping in the detection.

**Sound settings (⚙ on the card header).** The gear on the Hive activity monitor card opens an in-app settings modal with a **Play sounds** master toggle (the same preference as the toolbar chip), a **Volume** slider (0–100, applied to the built-in sounds and to your own files alike, remembered per browser), and one row per event — **Open sound** (a new issue or pull request appears) and **Merge sound** (a pull request merges). Each row shows what currently plays ("Built-in (bee buzz)" / "Built-in (zapper)" or your file's name), a **▶ Play** preview (the button reads *Playing…* while the sound runs; if the browser refuses to start audio, a toast says so rather than failing silently), a **Choose WAV…** picker that accepts `.wav` or any other audio your browser can decode (up to 4 MB; larger files are refused with a toast), and **Reset to built-in**. Uploaded files are kept in this browser only (IndexedDB) and decoded with the Web Audio API; if a stored file cannot be decoded the built-in sound plays instead and the row says so. **▶ Play both** auditions the merge sound followed by the open sound. Previews start the browser's audio engine inside the click itself and wait for it to come up (about a second at most) before playing, so they work in Safari and on a page that has never made a sound; an automatic zap or buzz on an engine the browser still holds back waits only briefly and otherwise skips rather than queueing up to burst out on your next click, and the next click or key press re-arms it.

**Movement animation (⚙ on the card header).** When a refresh moves an item's lit chevron to a different column — forward (advance) or backward (regress) — the row plays a short, subtle three-beat sequence so the eye can see where it went: first the chevron it is *leaving* flashes the active highlight twice (two ~140 ms pulses with a short gap), then the chosen effect travels from that chevron to the new one, and finally the new chevron lands lit — the whole sequence takes about a second. Pick the travel effect in the same settings modal, under **Movement animation** below the sound settings: **None**, **A Sliding puck** (a highlight glides from the old chevron to the new one and settles), **B Ripple** (each chevron between old and new lights in turn), **C Ghost crossfade** (the old highlight fades out while the new one fades in with a slight slide), **D Comet** (a streak with a bright head travels the strip and lands on the destination), **E Destination bump** (the destination chevron scales briefly with a fading direction arrow), or **F Colour wash** (default — a tinted sweep flows across the intermediate chevrons). Each option has a *Preview* button that plays the full flash → move → land sequence on a sample row inside the dialog. The choice is remembered per browser (`localStorage` key `hiveStageTrackerMotion`). Items that stay put never move; a row that leaves the list (merged, closed, or filtered away) collapses and fades out over `--ham-move-exit-ms` and a row that arrives fades in over `--ham-move-enter-ms`; nothing animates on first load, and at most twelve rows animate per refresh (a filter or page change that turns over more rows than that redraws at once). The list is reconciled row by row on every refresh rather than rebuilt, so a move keeps playing while the hive keeps publishing. When the operating system asks for reduced motion (`prefers-reduced-motion: reduce`), every style falls back to a single soft flash followed by a plain crossfade and departing rows only fade, regardless of the setting; **None** disables the flash, the effect and the row fades entirely. Durations, the flash timing (`--ham-move-flash-ms`, `--ham-move-flash-gap-ms`, `--ham-move-flash-count`) and tints come from the `--ham-move-*` tokens in `tokens.css`.

## Agent activity

One line per worker showing what it is doing now, how long since it last acted, and which issues it has reserved.

**What it tells you.** A worker is one of Hive's AI agents. This section puts every worker on one screen, one row each, so you can see what all of them are doing without opening each card. Each row shows:

- **Agent:** the worker's name.
- **State:** the same word as on the worker's card, for example working, idle, paused or powered off.
- **Now:** the same "Now:" line as on the worker's card. It names the issue or pull request the worker is on, or says "Idle", "Paused", "Off", "Stopped" or "Not started yet".
- **Since last action:** how long ago the worker last commented on, labelled, opened, reviewed or merged an issue or pull request. A worker that is working but has not touched an issue or pull request yet shows how long ago it started instead.
- **Reserved:** every issue the worker has reserved, with the time each reservation ends. A reservation stops other workers picking the same issue. It is not proof the worker is still busy with it.

**How the numbers are worked out.**

- The rows use the same information and the same wording as the worker cards in the **Agents** section, so a row and its card always agree.
- The header counts working workers and all workers.
- Times are measured on the hive's clock, so a wrong clock on your computer does not change them. They keep counting up between updates.
- Hive only sees what a worker does through Hive. Work done another way, such as a direct `git push`, does not show here.
- If the dashboard misses three updates in a row, the times stop showing and the **Now** line says "information may be out of date".
- Reservations show only when issue reservations are turned on.

**What it is good for.** Look here to answer "what is everyone doing?" at a glance. A worker that has said "working — no issue or pull request yet" for a long time, or one that holds many reservations, is worth a look on its card or in **Agent Logs**.

**Example.** The row for `scanner` says "working", "Now: console#123 — Fix the login redirect · 2 min ago", "2 min ago", and "console#123 until 14:30". The `scanner` card says the same.

**When it appears.** Always. It lists the same workers as the **Agents** section, so you see only the workers you can already see there. Issue and pull request titles appear only for projects the dashboard already shows you. Like other sections, you can collapse it with its title, move it with its `⠿` grip, or hide it with the eye-off control on its row in the left menu. The dashboard remembers these choices in this browser.

**Settings that change it.** None of its own. A worker's settings (its ⚙️ button on the card) change what it does, and so what its row shows.

## FAQ

Short answers to the questions people ask once a hive is running, each linking to the guide that explains it fully.

**What it tells you.** It answers common questions about levels and `hold` labels, the Evidence card, release channels and upgrades, the Model Gateway, pausing, cost, the audit log and where to get help. Every question is a collapsible row; a filter box at the top hides rows that do not match what you type.

**How the numbers are worked out.** This section has no numbers. The collapsed summary counts the questions.

**What it is good for.** Read it when something on the dashboard surprises you. Each answer links to a fuller guide, and the footer says where to ask when the answer is missing.

**Example.** You wonder why Hive does not merge anything. The FAQ explains that merging starts at a higher level and that a `hold` label keeps a pull request out of every merge lane.

**When it appears.** Always, at every level.

**Settings that change it.** No settings change this section.

## Runs

Longer pieces of work Hive is carrying out in stages, and which ones are waiting for you.

This section exists on the v6 line only.

**What it tells you.** A run is one larger task that moves through stages, such as spec, plan and implement. Each card shows a run's stage and who it is waiting on.

**How the numbers are worked out.**

- The header counts active runs, runs blocked on a person, and recently finished runs.
- A run is finished when it has an outcome or waits on nobody.
- Owners can approve a run's next step from its card.

**What it is good for.** Check it for runs blocked on a person. Answer their questions or approve the next stage.

**Example.** The header reads `2 active · 1 blocked on human`. One run waits for you to approve its plan.

**When it appears.** On v6 only, always.

**Settings that change it.** Which workers can take staged work changes what appears. See [Runs](runs.md).

## Platform

Which code hosting service Hive is connected to and which optional platform services are switched on.

This section exists on the v6 line only. On v5 the same facts are in the Diagnostics section.

**What it tells you.** It shows the code hosting service, such as GitHub, GitLab or Gitea, and how many repositories Hive watches. It also shows whether optional services, such as the token minting service, are on.

**How the numbers are worked out.** The repository count is the number of repositories Hive watches. The other tiles show on or off.

**What it is good for.** Check it after setup to confirm Hive is connected where you expect.

**Example.** It shows `GitHub · 3 repos` and "Mint on". Hive watches three GitHub repositories and the token service runs.

**When it appears.** On v6 only, when the hive reports platform data.

**Settings that change it.** Your code hosting settings and optional service settings change it.
