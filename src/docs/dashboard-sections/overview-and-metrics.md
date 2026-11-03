# Dashboard sections explained — Overview, advisories, throughput and cost

Part of [Dashboard sections explained](../dashboard-sections.md); see that page for how each entry is organised and for the words used on it.

## Overview

How many issues and pull requests Hive is tracking right now, and how many of them it can work on.

**What it tells you.** The top row of tiles splits all your open work into a few piles. The two charts show the same work grouped by what it needs next. The small lines inside each tile show how the number moved over the last day or week.

**How the numbers are worked out.**

- **Total open issues** and **Total open PRs** count every open issue and every open pull request in the repositories you selected. Draft pull requests count too.
- When the ForgeState cache (`forgestate.enabled: true`) has finished its first full scan of a repository, the two totals are read from that cache instead of from the GitHub search API. The cache is kept current by webhook events and its own low-rate refresh, so the totals do not cost a `/search/issues` request on every status refresh. The actionable, held, blocked and outside parts still come from Hive's own classification; whatever the cache counts that the classification has not placed yet lands in Outside's **hold-adjacent/other** part, so the sum always matches the total.
- If GitHub rate-limits or fails the count request, the tiles keep the last good numbers and show **Counts stale since <time> — serving last-good** instead of dropping to zero. The status payload carries the same moment as `staleSince` (shared with the enumeration last-good marker), and each repository as `countsStale` / `countsStaleSince`.
- The two totals are the count of the items actually listed in the status snapshot (one canonical `openItems` set per refresh, built server-side from the enumeration or ForgeState cache items plus the last-good copy). The small `issues/PRs` counter beside **Overview** in the left menu and the **Hive activity monitor** list and header are read from that same set; hover the counter to see the whole sum.
- **Why the two left-menu counters differ.** The **Overview** counter prints that set as *items*: `33/13` is 33 open issues and 13 open pull requests. The **Hive activity monitor** counter prints the same set as *rows*: every open issue is a row, and a pull request gets its own row only when it is not drawn under a listed issue (one with no linked issue, or whose issue is closed or not listed). So `37` beside the activity monitor is `33 issues + 4 PRs on their own row`, with the other 9 PRs shown on their issue's row. Hover either counter: the Overview tooltip names its two halves and says how the activity-monitor counter is made; the activity-monitor tooltip and the list header spell out the sum (`37 rows (33 issues + 4 PRs on their own row) · 3 need attention · 9 PRs shown on their issue's row`). Neither number is wrong; they are two views of one population.
- When a repository reports more open items than the snapshot can list (for example a cache total that has run ahead of the item list), the extra are never added to the totals; a small **+N not yet listed** badge beside the tiles names them until the next refresh lists them.
- After a restart Hive serves the snapshot it saved on disk; it is marked stale (**Counts stale since** badge) until the first fresh enumeration lands, so a pre-restart number is never shown as current.
- Each total is written as a sum, for example `10 = 2 actionable + 3 your turn + 1 waiting on someone else + 1 held + 3 outside`. Every open item counts in exactly one part, so the parts always add up to the total.
- **Your turn** counts every item Hive is stuck on a person for: no agent will move it until someone acts. It is the same set the Your turn list in Projects shows, including the items to close. Parked issues (`needs-decision`, `needs-direction`, `needs-spec`, `needs-human`) count here, not in Outside.
- **Waiting on someone else** counts items gated on someone other than you: a hold a person or the dashboard pause placed, a Hive pull request still inside the adjudicator's wait, an issue waiting for its reporter, and an issue an open pull request covers.
- **Held** counts the other items with a hold label. A hold label, such as `hold` or `on-hold`, parks an item on purpose.
- **Blocked** counts pull requests with a blocked verdict, a merge conflict or failing checks.
- **Outside** counts open items that Hive's filters kept off its work list, and draft pull requests. Hover its ⓘ mark to see why each item was kept out. The reasons are labels such as `needs-direction`, `needs-decision` or `needs-spec` on items that are not your turn, and exempt labels. Others are reporter triage, your project issue filter, standing advisory issues and bot dependency dashboards.
- **Actionable now** counts what is left: items Hive could work on now. It no longer includes the items that are your turn, such as unacknowledged agent proposals.
- **Test coverage** appears at the end of the row when `HIVE_COVERAGE_BADGE_URL` is configured and readable. It shows the primary repository's current coverage and goal, using the same reading as the quality agent.
- The last part of Outside is **hold-adjacent/other**. It is whatever is left after every named reason is counted. Hive cannot say more about these items.
- The Outside tile exists on the v5 line only. On v6 the same items are part of the totals but have no tile of their own.
- **Issues by band** plots every open issue the decision places in a slice. The slices are Your turn, To close, Waiting on someone else, Unclaimed and Claimed. The repository cards keep the Needs triage, Parked and Confirm & close bands; the charts do not. A band is a display group named after what the item needs next. The orange chip names the label that put an issue there, with one word per label: HUMAN (`needs-human`), DIRECTION (`needs-direction`), DECISION (`needs-decision`), SPEC (`needs-spec`), REPORTER (`needs-reporter-confirmation`), BLOCKED (`blocked`), DISCUSSING (`2-discussing`), EPIC (`Epic`) and UNTRIAGED (`needs-triage`). An issue in the Parked band and an issue Hive has parked show the same word for the same label. When an issue has more than one of these labels, the chip uses the first one in that list and the others show as glyphs. Clicking a chip, on a card or in the legend, filters to every issue with that chip. Issues Hive has parked with `needs-human`, `needs-decision`, `needs-direction` or `needs-spec` count toward the "need human review" total in the section header. REPORTER issues are not counted there. The grey FILTERED chip is only for issues skipped by the project issue filter or an exempt label.
- Your turn plots the items that are your turn, except the ones to close, which are the To close slice. Waiting on someone else is as above. Unclaimed means nobody is assigned. Claimed means a person or a Hive worker took it.
- **PRs by band** plots every open and held pull request in one slice: Your turn, To close, Waiting on someone else, Merge-eligible, Blocked, In review, Open or Draft.
- Merge-eligible means the pull request passed every check Hive uses before merging. Blocked means a failed check, a merge conflict, or a blocking review verdict.
- Hover any slice in a chart to see its exact rule. Each slice's ⬇ link exports the rows it counts.
- The trend lines come from samples the hive takes on each status refresh, roughly every few minutes. Samples are kept for about 30 days and thinned to 96 points.
- Your browser also keeps its own samples. When you filter by repository, only the browser's samples are used.

**Numbers that look like they should match but do not.**

- The charts plot more than Actionable now: they include the items that are your turn and the ones waiting on someone else, each in its own slice. So an issue chart can total 8 while Actionable now shows 1 issue.
- The charts leave out the open items that are neither your turn nor waiting on someone else and that the scanner filtered out; those only count as Outside.

**What it is good for.** Look here first each day. A growing Actionable now means Hive has work to do. A growing Your turn means people are the bottleneck. Click a tile to filter the Projects section to the items it counts. Then add the missing decision, remove a stale label, or close finished issues.

**Example.** The tile reads `10 = 2 actionable + 1 your turn + 7 outside`. Hive can work on 2 issues. One waits on you. Seven were kept out by your filters. Hover the ⓘ mark on Outside to see that 2 are bot dependency dashboards. Those are expected and need no action.

**When it appears.** Always. The Overview section cannot be hidden.

**Settings that change it.**

- `dashboard.issue_bands.waiting_labels` and `dashboard.issue_bands.done_labels` decide which labels put issues in "Parked" and "Confirm & close".
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
- The compact strip at the top of the Agents view and the Governor grid also show **running** — how many workers are mid-round right now, and the fleet's estimated spend, which rotates through four windows every ~4 seconds: `3 · $1.20 this hour` → `$9.80 today` → `$41 this week` → `$130 this month`. The worker count stays fixed; only the amount and window phrase crossfade, the tile reserves the width of its widest phrase so nothing shifts or wraps, the rotation pauses while you hover (the hover lists all four values) or while the tab is hidden, and it swaps instantly when your system asks for reduced motion. The hour figure is the rise in the cumulative estimate since the top of the hour; today / this week / this month are the cost ledger summed from 00:00 UTC today, Monday 00:00 UTC (ISO week) and the 1st of the month 00:00 UTC, plus the rise since the ledger's last reading. `$–` means the hive has not yet taken a reading for that window.
- The **cadence table** shows, for each worker, how often it runs in each mode. Continuous means it runs again as soon as it finishes.
- The **budget** shows how much of the AI spending allowance for the current period has been used.
- The **next kick** estimate follows the eval tick, which is longer while GitHub webhooks are trusted (`/api/status.webhooks.mode` reads `webhook`) and drops back to `governor.eval_interval_s` when they degrade (`polling`). How that decision is made is in [webhooks.md](../webhooks.md#fallback-when-webhooks-degrade).

**What it is good for.** Check it when Hive seems too slow or too busy. If the mode stays idle while work piles up, check the thresholds. If one worker never runs, check its row in the cadence table.

**Example.** The mode reads `busy` and the gauge shows 12 actionable items. The busy threshold is 10 and the surge threshold is 20. Hive will run its workers at the busy pace until the count drops below 10.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.**

- The ⚙️ button opens the Governor settings: workers, thresholds, budget, notifications and health checks.
- The mode thresholds live in `governor.modes`. Choosing an autonomy level writes default thresholds there.
- The budget lives in `governor.budget`.

## PRs by model

Which AI models wrote the pull requests Hive opened, how well each model's work landed, and how the models rank against each other.

**What it tells you.** This part of the Governor section compares the AI models your workers use. It shows how many pull requests each model wrote, how they ended, and a rank that favours models with a long, clean record over models with a short, lucky one.

**How the numbers are worked out.**

- Each row is one model. Model ids are normalised so one model gets one row: a provider prefix (`openai-codex/gpt-6.1-sol`), a `:variant` suffix (`gpt-5.6-sol:high`) and a dash between version digits (`claude-opus-5-5`) are folded into `gpt-6.1-sol`, `gpt-5.6-sol` and `claude-opus-5.5`. The backends that drove the model appear as chips under its name; hover the row to see the raw ids and how many pull requests each contributed.
- The bar splits a model's pull requests into merged, still open, and closed without merging.
- **Merged** is the number of merged pull requests in the window.
- **1st pass** is the share of merged pull requests that merged without a review-requested fix round.
- **PR runs** is the share of recorded task runs (from the contributor run log) that ended with a verified pull request.
- **Fail** is the share of recorded task runs that failed: crashed, timed out, or exited non-zero.
- **No ship** is the share of recorded task runs that completed but opened no pull request.
- **Rank** orders models by a score that combines those columns and penalises small samples: `score = Wilson 95% lower bound(1st-pass merged ÷ merged) × (1 − Fail) × (1 − No ship)`. The Wilson lower bound is the pessimistic end of the confidence interval for the first-pass rate, so 5 of 5 (lower bound 0.57) scores below 21 of 22 (0.78) and well below 203 of 313 (0.60 after the bound, but with no failures). A model with no recorded runs keeps its bound as the score.
- A model gets a rank only after it has at least 5 merged pull requests in the window. With fewer, the sample is too small to compare and the row is listed without a rank.
- Rows whose pull requests carry no model id are shown last, muted, as **unattributed**. They are counted but never ranked.
- You can show the last 7 days, the last 30 days, or all time. You can sort by rank or by count.

**What it is good for.** Use it when choosing which model a worker should use. A model with a low first-pass rate or a high failure rate costs more review and retry time; the rank already weighs both.

**Example.** Model A has 40 merged pull requests, 28 of them first pass, and 2% failed runs. Model B has 5 merged pull requests, all first pass, and 51% failed runs. A scores about 0.54 and B about 0.28, so A ranks higher even though B shows 100% first pass.

**When it appears.** Inside the Governor section, so at autonomy level L2 and above.

**Settings that change it.** No settings change this section. The minimum sample of 5 merged pull requests comes from `HIVE_CONTRIBUTE_EFFECTIVE_MODELS_MIN_PRS`.

## Advisory

Reports, advice and suggestions Hive has written about your projects and about how it is running.

**What it tells you.** This section groups five smaller sections: Advisory Digest, Hive Advice, Fleet Report Preview, Ready to level up? and Lifecycle Timeline. Each has its own entry below.

**How the numbers are worked out.** This section has no numbers of its own. Its collapsed summary repeats a count from one of the sections inside it.

**What it is good for.** Open it about once a week to read what Hive suggests. Collapse it when you do not need it.

**Example.** The collapsed title reads `2 advice items this week`. Open it and read Hive Advice first.

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

**Settings that change it.** Settings under `governor.advisory` change how often findings are refreshed and when old ones are marked stale. See the [Advisory digest guide](../advisory.md).

## Hive Advice

A few things Hive recommends you do to keep work moving on your projects, picked once a week.

**What it tells you.** Hive checks its own counters and your open work, then lists a few concrete steps. Hive keeps the same list for a week so it does not jump around, and picks a new one sooner if its mode changes. The numbers inside it update every few minutes.

**How the numbers are worked out.**

- Some rules depend on the Governor's mode, such as "give idle workers a schedule".
- Other rules read the Overview's bands. A band is a display group named after what an item needs next.
- For example, Hive advises clearing blocked pull requests when they pass a set share of all open pull requests.
- Two count tiles show the Governor's count and the Overview chart's count. They can differ, because the chart includes items that wait on a person.

**What it is good for.** Work through it once a week. Each recommendation lists the first few items it is about and links to the full list.

**Example.** The advice says "41 of 70 blocked pull requests fail one check — fix the check first". Fixing that check unblocks most of them.

**When it appears.** Inside Advisory, so at autonomy level L2 and above. It is hidden when there is nothing to recommend.

**Settings that change it.** The thresholds under `governor.advisory` change when each rule fires. See [Owner advice](../advisory.md#owner-advice).

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

**Settings that change it.** `governor.fleet_report.file_upstream` decides whether reports are sent. The default, `false`, only previews them. See [Fleet self-reporting](../fleet-report.md).

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

**Settings that change it.** The current autonomy level and the per-repository pins change it. See the [level-up advisor guide](../acmm-advisor.md).

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

**Settings that change it.** The window, repository and role controls are in the section. How far back data goes depends on how long the audit log keeps events. See [Throughput](../dashboard.md#throughput).

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

**Settings that change it.** No settings change this section. Which models and providers you configure changes what it can record. See [Token tracking](../token-tracking.md).

## Cost

What Hive's use of AI models would cost at public per-token list prices; an estimate, not a bill.

Read this as a **list-price estimate, not a bill**. Copilot and Claude subscription seats and self-hosted inference are not billed per token, so actual spend can be much lower.

### Read the cost card

1. Read **Estimated spend at list price — since <date> · tokens × public per-token price · not a bill**. This is the accumulated estimate for the recorded history.
2. Compare **Cost / closed issue** and **cost / merged PR**. Both divide that same estimate by a different count; do not add them together.
3. Check **counts as of <time>** before drawing conclusions. A stale or partial count means the denominator is incomplete, not that no work happened.
4. Choose a chart range to compare recent usage. **Spend in selected range** covers that window; it is not the all-history headline.
5. To investigate a jump, compare workers and models in **Tokens**, then inspect the worker's recent runs.

**Example.** If the estimate since the displayed date is $40 and the attributed count is 80 merged PRs for the same period, the tile is about $0.50 per PR. A monthly chart selection does not change that all-history division.

**When it appears.** Always.

**Settings that change it.** Configured models and their prices affect the estimate. Gateway credentials add the gateway's reported spending. See [Token tracking](../token-tracking.md).

### Headline, live summary and usage tables

The card keeps history even when the live token store resets. The “of which” line and usage tables cover only the current live summary; they need not equal the accumulated headline.

The headline date is the oldest per-day total the hive holds (or the oldest snapshot when there are none), so it does not creep forward as the 30-day ring evicts old snapshots; the merged-PR and closed-issue counts behind the two tiles are counted from that same instant. Hover the figure, the "est." badge or the **?** mark for the full explanation: Copilot and Claude subscription seats and self-hosted inference are not billed per token, so real spend can be far lower than the estimate.

Beneath it, **of which $X is in the live token summary since the last reset on <date>** says how much of the headline comes from the session files currently on disk. That summary starts over when the hive is rebuilt or its token store is cleared; spend recorded before each reset stays in the headline, which is why the two figures differ. (Older builds called this the "current lineage".)

The **usage by model** and **usage by sandbox** tables are built from the live token summary only (their window tag says so), so their totals match the "of which" line, not the headline.

When your AI gateway reports its own spending, that figure appears too.

### Chart ranges and controls

Choose a range, then use the **−/+ stepper** to change its size:

| Range | Initial window and bars | Stepper bounds |
| --- | --- | --- |
| **hourly** | Current clock hour and the 23 before it; one bar per hour (**24 h**). | 6–72 hours, in steps of 6. |
| **daily** | Last 14 calendar days including today; one bar per day (**14 d**). | 7–60 days, in steps of 7. |
| **weekly** | Current Monday→Sunday week and the four before it; one bar per week (**5 wk**). | 2–26 weeks, one at a time. |
| **monthly** | Current calendar month and the four before it; one bar per month (**5 mo**). | 2–24 months, one at a time. |
| **custom** | Your start/end; hour bars up to two days, day bars beyond. | No stepper; bounds are explicit. |

Boundaries follow your browser's clock. Daily labels use weekdays for seven or fewer days, then month/day. Weekly labels name the Monday; monthly labels name the month. Future portions of the current week/month are not recorded spend.

The chips are disabled at the bounds, work from the keyboard, and the chosen size is remembered per range in this browser. Custom has no stepper — its bounds are explicit.

**Spend in selected range** adds up the bars of that same window, and **run-rate** divides it by the part of the window that has elapsed so far — per hour for hourly bars, per day otherwise. Switching the range or stepping the window changes the chart and both figures together.

### Count freshness and API reference

**Cost / closed issue** and **cost / merged PR** (in that order, after the headline total) each divide the *same* headline total by one count — they are two views of one figure, not two costs to add, and the card says so under the tiles. Each label spells out the division (**total ÷ N PRs merged by hive since <date>**) and when the count was last confirmed (**counts as of <time>**).

With no cost or no count, they show a dash.

The merged and closed counts behind those two figures are kept by Hive itself: every `pull_request closed` (merged) and `issues closed` webhook event for the hive's own author adds one, and once an hour (`forgestate.CountsReconcileInterval`) the counts are re-read from GitHub search and overwritten so drift from missed events is corrected. Search is no longer queried on every 5-minute collection.

When the hourly check is rate-limited or fails, the last good counts stay in use. The label reads **counts stale since <time> (GitHub rate-limited)** and the tooltip gives the reason, even when no cached count exists.

Large Hives reconcile a few repositories per pass to stay within GitHub's search limit. While some are waiting, the label reads **partial: N of M repos counted**, rather than a dash or an unrelated fallback. The merged-by-Hive counter is used only when no attributed count has ever been produced.

| `/api/cost` field | Meaning |
| --- | --- |
| `count_stale_since` | When the current stale period began. |
| `count_updated_at` | Last successful count read. |
| `count_source` | `forgestate` or `search`. |
| `count_error` | Why the count could not be refreshed. |
| `count_partial` | The attributed count still covers only part of the repositories. |

### Accounting reference: resets and restored counters

**Headline history.** The accumulated estimate includes per-day totals older than the 30-day snapshot ring, plus positive increases of the recorded cumulative dollar estimate inside the ring. If the ring is the whole history, its first value is the opening balance. Resets do not subtract already recorded spending; the tooltip lists resets so spending on both sides can be understood.

The restore test omits an abnormally large increase only when it regains old ground: it must land at or below an earlier peak and close at least 80% of the gap to it (`COST_RESTORE_GAP_FRACTION`). A normal-sized increase counts, and a new high counts however large. “Abnormally large” means above both 20× the median ordinary increase and $500; with too little baseline data, no such size exclusion is made.

**Chart and per-day totals.** These use positive token increments, priced by model, rather than subtracting the first counter from the last. A counter can fall when sessions disappear or the token store resets; clamping a net decrease to zero would hide real work.

Restore-shaped chart steps use the same size threshold, with a gap allowance: an otherwise oversized step is retained if it fits within 20× average spend rate × elapsed gap. This protects real accrual across a sampling gap. The collector runs server-side, so simply not opening the dashboard does not cause a gap. A discarded step loses its tokens too; it cannot leave a day with huge tokens and $0.

Chart increments are spread over their accrual interval rather than charged only to the moment the scanner observes a transcript flush. This is an allocation of observed usage, not proof of exactly when each token was consumed.

**How tokens become dollars in the chart.** Each model's $/token comes from the newest snapshot that still lists that model, so a model that stopped running weeks ago is still priced when its historical tokens are summed. The headline uses recorded dollars directly. The two share an increase-based history model, not an assurance of identical values for different windows or pricing inputs.

### History retention and incomplete recording

**How far back the data goes.** The hive takes a cost snapshot every 5 minutes on its own (a server-side timer, not the browser — the history has no holes on days nobody opened the dashboard), keeps about 30 days of them, and folds each completed day (UTC) into a per-day total that is kept for about two years (`daily_rollup` on `/api/cost`, persisted in `/data/cost-daily-rollup.json`).

Bars inside the last 30 days are built from the snapshots; older bars use the per-day totals, so a wide weekly or monthly window still shows real spend. A bar that neither source reaches — earlier than the first recorded data point — is drawn as a dim hatched placeholder rather than a $0 bar, and the summary adds **history starts <date>** so you know where the real data begins.

The per-day totals start accruing the day the hive first runs this version, so on an upgraded hive they reach back only as far as the snapshots that were present then.

When the running agents' configured models have recorded no usage for more than 24 hours, a warning above the headline names those models and when they last recorded anything, and the run-rate is suffixed **(recording incomplete)** — the figures then reflect only the models still being recorded (`recording` on `/api/cost`).
