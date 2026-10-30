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

**Who is who in the activity monitor** ([#15394](https://github.com/hivecommons/hive/issues/15394)). Every hover, menu and caption uses these five nouns and no others:

- **Agent:** any automated worker — one of Hive's own agents or a contributor's connected agent. Hover text says *the author's agent* for the latter; the product noun **clanker** appears only where it names the contributor relay itself (the Contributors page, presence chips).
- **Contributor:** a person outside the maintainer team whose issue or pull request Hive is tracking.
- **Author:** whoever opened the pull request — a person, an agent or Hive. A **trusted author** is one Hive may merge for without asking.
- **Reviewer:** a person or agent that reviews a pull request; Hive's own review swarm is *the reviewer*.
- **Merge:** the one word for landing a pull request. *Merge it* is you merging now; *Let hive merge it* is you approving so Hive merges on its next pass.

Hover text follows one budget: one sentence of state in plain words, at most 40 visible words, one recommended action, and everything technical (config keys, stage numbers, how to change a setting) behind a single collapsed **Details**. The same sentence is never shown twice on one surface. The per-builder word counts live in [dashboard-copy-inventory.md](dashboard-copy-inventory.md).

**Dialogs on a phone.** Any dialog with a long body — the **What changed** release notes after an upgrade, alerts, the export viewer, the Level 6 auto-merge notice, settings bodies — fills the whole screen on a phone: title and a ✕ at the top, the text scrolls in the middle, and the buttons stay fixed at the bottom. A soft fade at the bottom edge means there is more to read; it disappears when you reach the end. Esc, the ✕, the buttons and tapping the backdrop close it. On a desktop the same dialogs keep their usual size and only gain the fade and a thin scrollbar. See [Long-content modals](dashboard-design-system.md#long-content-modals-modal-scroll-and-the-mobile-sheet) for the convention.

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
- The compact strip at the top of the Agents view and the Governor grid also show **running** — how many workers are mid-round right now, and the fleet's estimated spend, which rotates through four windows every ~4 seconds: `3 · $1.20 this hour` → `$9.80 today` → `$41 this week` → `$130 this month`. The worker count stays fixed; only the amount and window phrase crossfade, the tile reserves the width of its widest phrase so nothing shifts or wraps, the rotation pauses while you hover (the hover lists all four values) or while the tab is hidden, and it swaps instantly when your system asks for reduced motion. The hour figure is the rise in the cumulative estimate since the top of the hour; today / this week / this month are the cost ledger summed from 00:00 UTC today, Monday 00:00 UTC (ISO week) and the 1st of the month 00:00 UTC, plus the rise since the ledger's last reading. `$–` means the hive has not yet taken a reading for that window.
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

**Settings that change it.** Configured models and their prices affect the estimate. Gateway credentials add the gateway's reported spending. See [Token tracking](token-tracking.md).

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

## Projects

One card per repository Hive looks after, listing its open issues and pull requests grouped by what each one needs next.

**What it tells you.** Each card is one repository. Its issues and pull requests are sorted into bands. A band is a display group named after what the item needs next.

**How the numbers are worked out.**

- The cards use the same bands as the Overview charts. See the Overview entry for each band's meaning.
- The small label above each repository name is its code hosting service. On GitHub it is the host, such as `github.com`, with `· Enterprise` on GitHub Enterprise. On GitLab or Gitea/Forgejo (`project.forge`) it names the service and its host, such as `🦊 GitLab · gitlab.example.com`. The card's links then go to that host. The badge in the section header names the same forge when every card is on it, and shows `N forges` when a `project.repo_policies[].forge` override puts cards on different forges. Each repository in `/api/status` carries this as `forge` (`github`, `gitlab` or `gitea`) and, for GitLab and Gitea, `forgeUrl`. See [Forges](integrations/forges.md).
- Small pills on each item show signals, such as a hold, a failing check or no recent activity.
- A `🔗` pill on an issue means Hive has verified an open or merged pull request covers it. Hover it: "closes via PR #N" means merging the PR closes the issue; "covered by PR #N (partial)" means the PR says `Refs #N` and merging it does not close the issue. The pill comes from Hive's own PR-claim evidence, so it appears on every issue labelled `🐝 covered by PR` — including ones Hive has parked in the "covered by PR" group because an outside PR already fixes them — and never depends on GitHub having a closing link.
- A red pull-request pill means the sentinel flagged it for a possible security override, privilege escalation or codebase damage. Hive will not merge it until a maintainer reviews it and removes the sentinel label. Pull requests from trusted authors usually get a sentinel comment instead of the label, so they are usually not red.
- The **needs-human** count in the header counts open pull requests that need a person to review or decide. Needs-human means waiting on a person.
- Each card ends with a collapsible **Filter** section. Press **🔎 Filter** in the Projects toolbar to open the facet bar: tick labels (every label present on the repo's open issues and pull requests is offered, with a live count), add free-text terms (`"rate limit"` matches as a phrase, unquoted words must all appear, `-term` excludes; matching is case-insensitive over title, number and labels), and narrow by type (issue / PR), state (claimed, unclaimed, parked, covered by PR) or author kind (hive / human / bot). Labels and terms match **any** by default; switch to **all** for AND. The section lists the matching items for that repo — number, title linked to GitHub, one pill per label, age, and the PR icon for pull requests — and is collapsed by default only while it is empty; its open/closed state is remembered per repo.
- The same selection applies to every card. Name it and press **Save** to keep it; saved filters appear as chips next to the built-in presets and can be renamed, updated or deleted inline (no browser dialogs). Presets cannot be deleted but can be hidden. **Needs human** is the default preset: issues and pull requests carrying the hard-suppress labels (`needs-direction`, `needs-decision`, `needs-spec`, `needs-human` — honouring `issue_filter.hard_suppress_labels` overrides), `needs-signal`, a hold label (`hold`, `on-hold`, `hold/review`, the hive's own hold label), `stage/071-needs-author` (a human author must act on their red or blocked PR), the sentinel alert label (`sentinel-alert` by default — a maintainer must review the flagged PR and remove the label before the hive merges it) or the reporter-trust awaiting label (`needs-triage` by default — a maintainer must triage before the hive may pick it up), sorted in that priority order so operator decisions come first. The label set comes from the server (`labelPresets[]` on `/api/status`), so it never drifts from the labels Hive itself parks on (ADR-0019). Saved filters, the active selection and hidden presets are stored in the browser's localStorage per hive (`hive-label-filters:<hiveId>`).
- Any saved filter or preset can be **pinned to the sidebar** (☆ Pin to sidebar in the facet bar). Pinned filters appear in a **Filters** group at the bottom of the left navigation, each with a live count of matching items across every project. Clicking one opens a **cross-repo view** in place of the project cards: one list of matching issues and pull requests grouped by repository (groups collapse and remember their state), with the facet bar on top so you can refine and re-save. **Needs human** is pinned by default; unpin it from the facet bar. Choosing any other sidebar entry, or **← Project cards**, returns to the cards. Pin state is stored with the saved filter.

**What it is good for.** Use it to act on single items. Open the legend above the cards to learn what each pill means.

**Example.** A card shows 3 issues under "Confirm & close". You check each one, and close those whose fix has landed.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.**

- The repositories Hive watches are set in **Settings → Projects**.
- `project.forge` (or a repo's `project.repo_policies[].forge` / `forge_url` override) and the `gitlab:` or `gitea:` instance URL set the host label and links. See [Forges](integrations/forges.md).
- The `dashboard.issue_bands` settings change the band rules.
- **Merge only on current base** ([#15335](https://github.com/hivecommons/hive/issues/15335)). A green `current-base` chip in the card header means the hive only merges a pull request there once its head contains the current base tip; a green head tested on an older base is updated with GitHub update-branch and merges when CI is green on the new head. The hive-wide default is the **Merge only on current base** switch under **Settings → Features → Auto-merge** (`auto_merge.require_current_base_default`, off by default; help text: "OFF by default. Before auto-merging in any lane, require the PR head to contain the current base tip; a green head tested on an older base is updated with GitHub update-branch and merges once CI is green on the new head. Costs one compare call per merge candidate. Per-repo overrides live on the repo card."). Each row under **Settings → Repos** carries the per-repo override — the **Merge only on current base** checkbox with an *inherits* marker, or an **inherit** button when the repo has its own value — written as `auto_merge.require_current_base` (on, also strict: a head the App cannot update is parked) or `auto_merge.skip_current_base` (off). `/api/status` carries the effective value per repository as `requireCurrentBase`.
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

When **Settings → Features → Review Gate → Only review labelled PRs** (`review.require_labels`) is on, PRs without one of the approval labels stay in the *unreviewed* stage until a maintainer labels them: the planner never dispatches them and the relay refuses reviews on them.

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

### Create an experiment

Use **+ New experiment**, not the ⚙ settings dialog. Enter a research question, an optional hypothesis and the repository scope. The new card appears immediately at **0 / 672**, with *collecting baseline* as its next step. No scope pills means hive-wide.

Scope choices come from ⚙ → *Repos experiments may target*. An empty allow-list offers every repository. The settings dialog contains lab-wide guardrails (trust ladder, output mode and labels, allow-list), never the research question.

### Edit, pause or archive

- **✎ Edit** opens the pre-filled *Edit experiment #N* dialog. Only one add/edit dialog opens at once, so it does not silently replace a draft.
- Changing only the hypothesis saves in place. Changing the question or scope asks *Restart experiment?*: the old baseline and proposals belong to the old version. The old card is archived, and a new card starts at **0 / 672**, labelled *v2 · restarted from #1*.
- **⏸ Pause** / **▶ Resume** controls the experiment. **🗄 Archive** moves it and its results to **Previous experiments**. These compact actions have tooltips; **Generate proposals** remains a labelled button.
- Running experiments may not share a repository or both be hive-wide. If the lab refuses an overlap, pause the other experiment or narrow the scope. Otherwise their interventions would confound each other's results.
- Narrowing the allow-list later does not rewrite existing scope. Those cards keep their scope and show a muted warning.

### Read progress and diagnose a ready card

Each question has its own card, scope pills, status and next-stage line: *collecting baseline*, *ready — generate proposals*, *proposals awaiting your review*, *running* or *concluded*.

The baseline bar **N / 672** measures observations, not completed code changes. At four snapshots an hour, 672 is about seven days; the adjacent ⓘ explains it. A new lab having **0 Principles** is normal: principles are durable lessons distilled from judged experiments.

In Suggest/Evolve mode, a ready card says *ready — the experiment loop is not running on this hive* when the external loop has written nothing for 24 evaluation cycles. Hive itself designs no proposals. Check [The experiment loop](strategy-lab.md#the-experiment-loop), rather than waiting for the card to advance by itself.

### Observation reference

There is no lab-wide progress bar. The whole-Hive snapshot count is a total, not progress toward one experiment. The muted **Data collection** note under the cards shows *Observation history: N snapshots since <date>*, what each snapshot records, the cadence (four an hour) and retention (24 hours raw, then hourly buckets for 30 days).

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

**The run line.** While a worker is working, a second line under **Now:** reads like `$0.42 · 38.1K tok · 4m 12s · tests ✓ · 3 files`. The same line is pinned in the worker's detail panel header. It is built only from figures Hive already keeps:

- **`$` and `tok`** are the estimated cost and tokens of *this* round of work: the worker's cumulative token count × public list price (the same basis as the Cost section), minus what it was when Hive started the round (the last kick). Hive remembers the starting point, so a page reload does not reset the figure. `$–` means the token collector has no figure for this worker yet.
- **Elapsed** counts from the start of the round.
- **`tests ✓` / `tests ✗` / `tests –`** is the last test-runner summary visible in the worker's terminal (`go test` `ok`/`FAIL`, pytest `N passed`/`N failed`, jest/vitest `Tests:` lines). `–` means no result line is visible.
- **`N files`** is the last `N files changed` git summary visible in the terminal; `–` means none is.
- It updates with each worker update and disappears when the round ends. The **spend (range)** row further down the card is unchanged: that is the worker's spend over the Cost section's timeframe, not this round's.

**What it is good for.** Use it to pause a worker, restart a stuck one, or change its settings. Fix any card that says it needs a login. Read the **Now:** lines to see what each worker is on without opening its log.

**The log buttons.** `📄 full log` opens the worker's whole log and `📋 log text` copies it; `🕘 past kicks` lists the logs of earlier rounds. For a normal worker this is the terminal it is typing in. For a worker that runs each round in a sandbox or a Kubernetes Job (its card says so), there is no terminal: the buttons show the output Hive captured from the sandbox when the last round ended, and `past kicks` holds the earlier rounds. Before such a worker's first round has ended the button says there is no archived run yet.

**Sandboxed worker's last run.** A sandboxed worker's card also carries a **last run** line: when it ran, how long it took, whether it succeeded, and the pull request it opened. If the worker's own report said the run failed, or the sandbox exited with an error, Hive keeps any commits the run made off GitHub and the line reads `no PR opened: <reason>` — for example `no PR opened: agent report status=failed: RPM install returned 401`. The same line appears when a configured publish preflight (`publish.verify`, for example a whole-suite `pytest --collect-only`) failed after the run: `no PR opened: verify failed (1/2): uv run pytest --collect-only -q (exit 2)`, with the command's output tail in the tooltip. The full reason is in the line's tooltip and in the full log's outcome block, where each verify command has its own `---- verify: … ----` section.

**Example.** A card says "needs login". You click **Login** and sign in again. Another card says `Now: working — no issue or pull request yet · started 50 min ago`. That worker has changed nothing through the hive for 50 minutes, so you open its log to see what it is doing.

**When it appears.** Always.

**Settings that change it.**

- Each card's ⚙️ button opens its settings: schedule, model, tools and permissions. See [Agent configuration](agent-configuration.md).
- `governor.claims.enabled` turns reservations on. When it is off, which is the default, the line shows nothing about reservations.
- `dashboard.agent_poll_interval_s` sets how often worker updates are sent. The default is 10 seconds.

## Hive activity monitor

A progress strip per open issue and pull request showing which step of the pipeline it is on and who must act next.

### First use: find the next action

1. Find an item by repository, number or title. Every open issue and PR in the watched repositories is represented, including held, draft, filtered, exempt and reporter-triage work. Being visible does not make it actionable.
2. Read the lit step and its words, then the reason on the item's `#N` link or lit chevron. The word says where the work is; the actor icon says whose move it is.
3. Use **🙋 Needs human** for work waiting on a person's action. Use **⏸ On hold** separately for work a maintainer deliberately paused. Neither filter grants permission to act.
4. Open **⋯** (or right-click / Shift+F10) for the actions allowed in that state. Read the confirmation and the result toast; a queued decision is not proof the PR merged.

**Example: one issue and its covering PR.** If issue #12 has one open PR #18, **All** draws one combined row: #12 at the left and `↳ #18` at *PR in progress*. Its lit step, modifier and `since` describe the PR. The sidebar counts **one row**, although the payload contains two open items. **Pull requests** shows #18 as a top-level row. Clearing filters restores the default row count.

For terminology such as stage, lane, sentinel, adoption and ratchet, use the [reader glossary](dashboard-glossary.md#terms). For exact gates, see [Stage labels](labels-and-control-signals.md#stage-labels-stagennn--the-pipeline-state-machine).

### Pipeline and actor reference

The column header names the shared route:

`Open → Actionable (010) → Claimed (020) → PR in progress (030/040) → CI / Review (050) → Merge queue (060/080/095/100) → merged`

Only the current chevron is coloured and named. Passed steps have a neutral fill; future steps have a faint outline. The final cap has a history mark (clock with counter-clockwise arrow, “History — decisions and actions on this item”) until merge, then a green ✓.

Side stages share a column rather than adding columns:

| Stage | Column / meaning |
| --- | --- |
| 015 Triage needed | Actionable; a human must triage. From `needs-triage` or the reporter-trust `reporter_triage` bucket, never merely the reporter's identity. |
| 045 Needs human decision | Actionable on an issue. A pre-045 issue still wearing 085 is drawn here until reconciliation. |
| 070 Fixing / 071 Needs author / 072 Returned to contributor | CI / Review: Hive repairs its own PR at 070; a human author acts at 071; a contributor agent gets its PR back at 072. `needs-rebase` explains a repair here, not at 050. |
| 074 Adopted by hive | CI / Review while the Hive repairs adopted work; pending, green or red pushes remain 074 until the merge gate takes it. |
| 085 Needs human decision / 090 Ready for human merge | Merge queue on a PR. Read the decision or merge reason rather than assuming Hive will merge it. |
| 060 / 080 / 095 / 100 | Separate gate states; use the [merge-gate reference](#merge-queue-pills--legend) below for exact words and countdowns. |

The author legend is a blue Hive hexagon, purple contributor-agent robot or teal human silhouette. The kind is `author/*`, inferred from the snapshot if its label is absent; the link tooltip names the login. On Claimed, the actor is the claim holder; on PR in progress and Returned to contributor it is the author. Hive owns review, adoption and auto-merge work; a human owns a human decision or manual merge. The payload's `stage_actor` is the authoritative actor (see [Who is on it](#who-is-on-it-actor-icons)).

At 090 the row explains why: `· workflow files`, `· tests removed`, `· manual-merge policy`, `· branch protection`, `· merge method`, `· auto-merge off`, `· untrusted author`, `· unauthorized`, `· reviewer deferred`, `· policy` or `· human merge` (a ⚠️-titled or `needs-owner-merge` PR — the caption reads *Ready for human merge (breaking change — owner merges)*, `human_merge_reason` is `breaking-change`, and hive never merges it, so **Approve — let hive merge** is greyed) ([#14658](https://github.com/hivecommons/hive/issues/14658)). `human_merge_reason` is a `prstage.HumanMergeReason*` code from the stage record's lane reason; `human_merge_reason_text` supplies the sentence and next step. For example, workflow-file changes require Merge on GitHub. Manual-merge text can read *Awaiting human merge · workflow files · approved by @login*; search and needs-attention text carry the same reason. A nested PR mirrors `human_approver`, `manual_merge` and both reason fields on `linked_prs`.

### Pairing, rows and counts

- With no covering PR, an issue has a full strip lit at Open, Actionable or Claimed.
- With exactly one covering PR, issue and PR share one continuous strip; early issue steps are passed and the PR supplies the current stage, chip, tooltip and age.
- With several covering PRs, the issue's own strip stops at Claimed, with no current step lit. Indented PR sub-rows begin under *PR in progress*, each with its own stage and age, furthest along first.
- A linked PR is not also a top-level row while its listed issue is visible. **Pull requests** flattens PRs; **All** and **Issues** use pairing. A PR linked to several issues is drawn under its `display_issue` only; see [One row per PR](#one-row-per-pr).
- An unlinked PR keeps a full-width row with early steps passed. Its title appears once on the left; *PR in progress* is reserved for covering-PR titles.
- A PR whose linked issue is closed remains live work: its own full-width title and stage lead the row, with the closed issue reduced to a muted `closes #N · closed` chip. See [Closed issue, open PR](#closed-issue-open-pr) and [Linked issues](dashboard.md#pr-review-and-link-signals) for title resolution.

The server keys items by repository, kind and number. A PR present in both issue and pull feeds wins over the issue copy; **Issues** never treats a PR as an issue.

**The canonical counter is default-view rows, not open items or indented sub-rows.** `stageTrackerRowCount()` calls `stageTrackerVisible()` with the default filter; a PR nested under a listed issue is not a separate counted row. Several indented PRs still belong to their issue's top-level row. The sidebar badge retains that default-view count when filters change.

The header reports the filtered row count and how many need attention; pagination says **showing X–Y of N (M filtered out)**. `stageTrackerCountsText()` produces the header, badge and showing text together. Where pairing reduces the count below open issues + PRs, its tooltip/footnote says **2 PRs shown on their issue's row**, rather than presenting a second competing total. Filters that flatten PRs can change which rows appear; do not add the badge to the payload's `open_issues` / `open_prs`.

**The counter beside the section name.** The left-menu counter and the header lead (`37 rows (33 issues + 4 PRs on their own row) · 3 need attention`) count *rows* of the default view, not items: every open issue is a row; a pull request is its own row only when it is not drawn under a listed issue. The Overview counter (`33/13`) prints the same open set as issues/PRs, so the two differ by exactly the pull requests that share their issue's row (`9 PRs shown on their issue's row` in the tooltip). The header and tooltip also name the PRs sharing issue rows. Filters and the kind tabs narrow the list and the `showing X–Y of N` line, never the counter.

### Filters, sorting and pagination

Filters run on the cached browser payload and persist across reloads, except the page number:

- Kind, **Repo**, **Author** kind and search (`#number`, number or title substring, including covering PRs).
- **Since**: any, over one hour/day/week on the current step, or past that stage's configured dwell. Stalled items always qualify.
- **Needs attention**: drawn health modifiers or a human-gated stage. Deferred merge-gate pills do not qualify; a handled regression alone does not qualify.
- Synthetic **Needs human** / **On hold** facets and label chips, described below. Stage labels sort numerically, then other labels alphabetically, case-insensitively. Chips within one family OR; families AND; synthetic facets AND with label chips.

**+N more** opens *All labels*, with every label and count, type-to-filter, toggles and **Clear**. Selected labels are promoted into the visible row. **Clear filters** appears whenever the view is narrowed.

Default sort is furthest along first; covered issues rank by their furthest-along PR. Within each column, attention rows precede plain rows, then held/draft rows and PRs standing for closed issues. Within a tier, longest waiting comes first, then repository and number. The same ordering applies to nested PRs and the server payload, so chat `/prs` and the hub agree. `deferred_modifiers` never promote a row.

**Sort** also offers longest waiting (tier, then wait), newest and number (no tiers). The arrow reverses column/age/repo/number keys, never attention tiers: “Attention-first within each column; arrow flips age order.” Pages offer **25 / 50 / 100** rows (default 50) and **Prev / Next**, with counts above and below.

### Stage age, modifiers and motion

The right-hand `since` is time at the current stage, red when stalled. Plain Open adds *Open · since 3d* (hours below two days), using `stage_since` or the item's opening time; a fallback tooltip says “(opened)”. Past `STAGE_TRACKER_OPEN_STALE_DAYS` (2), Open takes a subtle red tint.

A hold-class label (`hold`, `do-not-merge*`, `hive-pause/<id>`) wins over other signals: **Open · on hold**, no age. Held issues are not parked for decision; reconciliation removes an existing 045 ([#13012](https://github.com/hivecommons/hive/issues/13012)).

An Open blocked item reads **Open · blocked · since 7h** with a yellow ⛔. Unlike a hold, it retains age so a stale blocker stands out ([#14194](https://github.com/hivecommons/hive/issues/14194)). The tooltip names `blocked_on`, or says “carries the blocked label”. If held and blocked, hold wins.

A draft PR (GitHub draft bit or `do-not-merge/work-in-progress`) reads **Draft · since 18h**, with a muted ✎ and `draft` chip. It never turns stale: review/merge lanes skip it until the author marks it ready. Hold or blocked still wins, with draft in the chip's `+N` ([#14572](https://github.com/hivecommons/hive/issues/14572)).

An Open issue waiting on a maintainer's triage reads **Triage needed · since 28m** — the `needs-triage` chip, or a 000-new row the reporter-trust gate holds (`new.next_mover: maintainer`, triage gate *next*) before the awaiting label has landed; the server stamps the `needs-triage` modifier on such a row so the chevron word, the chip and the inline **Accept triage** lead are the same as the label case ([#15437](https://github.com/hivecommons/hive/issues/15437) read *New · since 28m* with the ask only in the caption). The caption is exactly **Triage needed — accept or close** on every held row (015 and 000-new alike — the 015 lane reason `triage needed: carries needs-triage` is not appended, so the row does not say *triage* three times); the chevron hover keeps the next-mover / next-pass narration and the gate list.

At most one health chip sits over the strip's cap, with `+N` for the rest: `stalled`, `regressed`, `hold`, `needs-human`, `needs-decision` (“needs human decision”), `needs-triage` (“triage needed”), `needs-rebase`, `blocked` or `draft`. At 015 the chevron already names triage; before claim, a lingering `needs-triage` can be a chip (010/017/045). At 020+ it is stale and omitted from chip/stage text: `stale_labels` supplies a muted *Stale label* tooltip until the sweep removes it ([#14586](https://github.com/hivecommons/hive/issues/14586)).

A genuine backward hop sets `regressed`; the first forward hop clears it. Lapsed claims 020→010, 030↔020 flicker and 040→010 after a partial merge are not regressions. A regression parked on a person is red. Otherwise it is a muted dashed **stage ↓**, yields to other modifiers and does not enter **Needs attention**: “the hive is handling it — no action needed; clears on the next forward step.” See [The regressed marker](labels-and-control-signals.md#the-regressed-marker-when-it-is-set-when-it-clears).

The reason, actor, nudges, repository, flags and recent stage hops live on the `#N` tooltip (the PR link for a nested row). Titles wrap before their column edges and show full text on hover. Hairlines and whole-row hover keep the chip and age attached to the right row ([#14640](https://github.com/hivecommons/hive/issues/14640)). Passed/upcoming chevrons, kind/author icons, chips and `since` have no separate hover text.

`stageTrackerMotion()` distinguishes steady, active and needs-human stages:

| Motion | Stages |
| --- | --- |
| Static | Open, held, 010, 020, human-authored 030/040, merged and unknown stages. |
| Active pulse | Hive/contributor-agent 030/040; 050, 060, 070, 074, 080, 095 and 100. |
| Needs-human breathe | 015, 045, 071, 072, 085, 090, or a displayed stalled/blocked/needs-human/needs-decision/needs-triage modifier. |

Reduced-motion preferences disable animation; needs-human remains steadily red. Icons do not rotate. Movement animation and the newer working cues have their own settings/reference below.

### Payload freshness and stage-engine reference

The view is fed by `GET /api/stage-tracker`. Without a stage or journal entry an item is in the Open column; a not-yet-placed item can read **New** as described [below](#new-row-000-new-the-pre-stage-state). Scanner buckets can place unlabeled claimed→020, in-progress→030 and covered→040 work.

**updated Ns ago · cache** reports payload age. The spoke rebuilds at most every `dashboard.stage_tracker.cache_ttl_s` (default `governor.eval_interval_s`) from its existing snapshot and sends an `ETag`. The browser polls every 30 seconds; unchanged data can return 304. These requests do not reach GitHub (`/api/gh-rate-limits` records `dashboard:stage-tracker` with no charge).

The monospace **engine enforce · N anomalies** strip describes the [central stage engine](adr/0025-central-stage-engine.md). Green `enforce` writes; neutral `shadow` is paused and writes nothing. Hover for derivation counts and mode details. Red anomalies count moves outside the admission table. `/api/status.stageEngine` carries the same numbers. Change the mode in **Settings → Labels → Central stage engine mode**; see [operator notes](operator-reference.md#central-stage-engine-rollout).

### Monitor controls and detailed reference

#### Filter chips (Needs human / On hold / labels)

The label row under the toolbar starts with two synthetic chips, then one chip per label the listed items carry. **🙋 Needs human** means a person's decision or action is the *only* thing that advances the row: an issue at `015` triage-needed, `045` or `085` needs-decision, a PR at `090` ready-for-human-merge, a PR at `071`/`072` whose author is a human, or any row wearing a `needs human` / `needs human decision` / `unauthorized` pill.

It is **not** `hold`, `stalled`, `blocked`, a handled regression, a `040` issue an open PR covers, or a PR the hive will merge itself — those are the hive's (or nobody's) to move. **⏸ On hold** is the separate chip for rows a maintainer paused on purpose with a hold label; a held row never counts as Needs human. The chip label, its count, the filter it toggles and the payload's `needs_human` / `on_hold` totals (and per-item flags) all come from one predicate (`stageTrackerNeedsHuman` in Go, `stageTrackerItemNeedsHuman` in the page), so what the chip counts is exactly what the filter shows ([#15008](https://github.com/hivecommons/hive/issues/15008)).

Hover a chip for a one-line explanation. Label chips AND with the synthetic chips.

#### Repo chip

Each row carries a small monospace chip before the number naming the repository it belongs to (`dibs #456`, `hive #13524`); the chip is tinted per repository from a fixed palette so one repo always looks the same, the configured primary repo is shown muted, and the full `owner/repo` is in the tooltip. Linked PR sub-rows carry the same chip. Clicking a chip sets the **Repo** filter to that repository (click it again, or pick "All repos", to clear).

By default the chip appears only when the list spans more than one repository; the ⚙ settings modal's **Repositories** section offers **Show repo chip: always / only when multiple repos / never** (stored under `hive.tracker.repochip`).

#### Group

The **Group** control next to **Sort** switches between **None** (one flat list, the default) and **Repo**. Grouped by repo, rows sit under a sticky header per repository showing the `owner/repo`, its row count, how many of those rows need attention, and a mini distribution of rows across the pipeline columns; the primary repo's group comes first, then groups by row count. The chosen **Sort** still orders rows within each group.

A header's chevron collapses that group to its header alone; collapsed repos are remembered across reloads (`hive.tracker.group.collapsed`), as is the Group choice itself (`hive.tracker.group`), which is also available in the settings modal. Group headers are keyed by repo so a refresh keeps them in place while rows move, and a header leaves only after the last row in its group has played its exit animation.

#### Sounds

The 🔇/🔊 chip at the right end of the toolbar turns on two optional, browser-made sounds: a "bug zapper" that plays when a pull request already on the list merges while you have the page open (never on first load, never for issues, once per pull request, at most two zaps per refresh, silent while the tab is hidden), and a short bee-like buzz when an issue or pull request first appears on the list (never on first load, once per item, at most two buzzes per refresh, same hidden-tab rule).

Both are off by default and remembered per browser; switching them on plays each sound once so you know what to listen for. They fire for every repo the hive governs — there is no primary-repo scoping in the detection. Browsers only let a page make sound after you have clicked or pressed a key on it, and that permission does not survive a reload — including the dashboard's own automatic reload when a new hive version is published.

If sound is on but the browser is still holding the audio engine back, the 🔊 chip turns amber (hover it for the reason) and, once per page load, a toast offers an **Enable sounds** button; pressing it, or any click or key press on the page, releases the engine and the chip returns to normal. A zap or buzz that fell in that window is skipped (the toast says so) rather than queued to burst out later.

#### Sound settings (⚙ on the card header)

The gear on the Hive activity monitor card opens an in-app settings modal with a **Play sounds** master toggle (the same preference as the toolbar chip), a **Volume** slider (0–100, applied to the built-in sounds and to your own files alike, remembered per browser), and one row per event — **Open sound** (a new issue or pull request appears) and **Merge sound** (a pull request merges).

Each row shows what currently plays ("Built-in (bee buzz)" / "Built-in (zapper)" or your file's name), a **▶ Play** preview (the button reads *Playing…* while the sound runs; if the browser refuses to start audio, a toast says so rather than failing silently), a **Choose WAV…** picker that accepts `.wav` or any other audio your browser can decode (up to 4 MB; larger files are refused with a toast), and **Reset to built-in**.

Uploaded files are kept in this browser only (IndexedDB) and decoded with the Web Audio API; if a stored file cannot be decoded the built-in sound plays instead and the row says so. **▶ Play both** auditions the merge sound followed by the open sound. Previews start the browser's audio engine and schedule their sound inside the click itself (Safari only unlocks audio for work done synchronously in a user gesture) and otherwise wait for the engine to report it is running (about a second at most), so they work in Safari and on a page that has never made a sound; an automatic zap or buzz on an engine the browser still holds back waits only briefly and otherwise skips rather than queueing up to burst out on your next click, and the next click or key press re-arms it.

#### Movement animation (⚙ on the card header)

When a refresh moves an item's lit chevron to a different column — forward (advance) or backward (regress) — the row plays a short, subtle three-beat sequence so the eye can see where it went: first the chevron it is *leaving* flashes the active highlight twice (two ~140 ms pulses with a short gap), then the chosen effect travels from that chevron to the new one, and finally the new chevron lands lit — the whole sequence takes about a second.

Pick the travel effect in the same settings modal, under **Movement animation** below the sound settings: **None**, **A Sliding puck** (a highlight glides from the old chevron to the new one and settles), **B Ripple** (each chevron between old and new lights in turn), **C Ghost crossfade** (the old highlight fades out while the new one fades in with a slight slide), **D Comet** (a streak with a bright head travels the strip and lands on the destination), **E Destination bump** (the destination chevron scales briefly with a fading direction arrow), or **F Colour wash** (default — a tinted sweep flows across the intermediate chevrons).

Each option has a *Preview* button that plays the full flash → move → land sequence on a sample row inside the dialog. The choice is remembered per browser (`localStorage` key `hiveStageTrackerMotion`). Items that stay put never move; a row that leaves the list (merged, closed, or filtered away) collapses and fades out over `--ham-move-exit-ms` and a row that arrives fades in over `--ham-move-enter-ms`; nothing animates on first load, and at most twelve rows animate per refresh (a filter or page change that turns over more rows than that redraws at once).

The list is reconciled row by row on every refresh rather than rebuilt, so a move keeps playing while the hive keeps publishing. When the operating system asks for reduced motion (`prefers-reduced-motion: reduce`), every style falls back to a single soft flash followed by a plain crossfade and departing rows only fade, regardless of the setting; **None** disables the flash, the effect and the row fades entirely.

Durations, the flash timing (`--ham-move-flash-ms`, `--ham-move-flash-gap-ms`, `--ham-move-flash-count`) and tints come from the `--ham-move-*` tokens in `tokens.css`.

#### Sentinel alert

A pull request the suspicious-activity sentinel flagged — it wears the `sentinel-alert` label (`sentinel.label`), or the review evidence bundle for its current head records sentinel findings — carries `sentinel_alert: true` in the tracker payload (with `sentinel_findings` and a short `sentinel_reason` when the bundle has them); a webhook label change flips it live. In the **Needs human decision** lane the lit chevron's own words read **Sentinel alert · needs human** (danger-tinted, the same wording in the cell's `title` and `aria-label`, and in the compact caption), so a maintainer reads the security implication before the stage mechanics; in every other lane at or past the merge gate (080, 090, 095, 100) the row shows a compact danger-tinted `sentinel` chip ahead of `needs rebase` / `regressed` / `stalled`; before the gate the chip is held back with the other merge-gate pills (see *Pills before the merge gate*) while `sentinel_alert` stays truthful in the payload.

The tooltip reads "Sentinel flagged security-relevant changes at `<sha7>`: `<reason>`" (a generic description stands in when the bundle has no reason). An issue carried by one flagged PR shows the lane text too, and a nested PR sub-row shows its own. The *Decide…* dialogs for a flagged PR open with a ⚠ line per flagged PR naming the sentinel reason, and *Approve* is styled as a dangerous action.

A person's *Approve* through that dialog **is** the human review the alert asked for: it removes the `sentinel-alert` label when present and records a `sentinel_acknowledged` human action keyed to the head in the review evidence bundle, so a bundle-only alert (findings but no label — hivecommons/crux#30) clears too; the payload then reads `sentinel_alert: false` with `sentinel_reason` suffixed `— acknowledged by @login`, the findings stay on record, and a new head starts over.

The delegated *Let hive decide* path never clears it. Hovering the lit chevron of a parked row names the parking stage itself (`Needs human decision`), not the 080 column it rides on; `Next move` follows the real stage (human at 085, hive at 080). Lane text and chip update in place on refresh (keyed reconcile, no flash).

#### Unauthorized pill

A pull request the hive cannot move because its GitHub App lacks a permission for the next step — it wears `hive:unauthorized` ([#14589](https://github.com/hivecommons/hive/issues/14589)); first case: approving held `action_required` workflow runs needs `actions: write` — carries the `unauthorized` modifier in the tracker payload, drawn in the far-right pill column exactly like `stalled` / `hold` / `draft` but muted (dashed, `--text-muted`), never red: it is nothing wrong, just a step only a person may take.

It counts under **Needs attention**. Before the merge gate only the held-workflow-runs step shows (see *Pills before the merge gate*). Its tooltip (payload `unauthorized`) names the missing permission and the human step; on the **Needs human decision** lane the lit chevron and the caption read **Awaiting human: approve workflow runs** (payload `unauthorized_step`, from the `unauthorized:<step>` stage reason). The pill, label and payload fields clear themselves in the tick the step is no longer needed.

#### Merge train

When several approved PRs to the same base touch the same files, the automerge lanes merge them one at a time ([#15117](https://github.com/hivecommons/hive/issues/15117)). The tracker renders every train contiguously, in merge order, with a thin left rail linking the rows and a header chip `🚆 merge train · 4 PRs · 1st: #15074` — the glyph is an inline-SVG railcar (14px, currentColor, hover *Merge queue*), the same icon the **Merge queue** column header and its chevrons wear; overlapping files are listed beside it. Pill wording per state is the PR's place in the queue (the owner's words — `front` and `queued-behind` appear nowhere a person reads): the head reads "Merge queued · 1st of 4 · ~13m" (its own soak left plus the next pass); the others "Merge queued · 2nd of 4 · ~4m", "Merge queued · 3rd of 4", … (`1st 2nd 3rd 4th … 11th 12th 13th 21st`, always with the queue length, countdown to its turn); when the page draws more than one train each queue word ends with its train's tag — "Merge queued · 1st of 2 · train main.go" (the first overlapping file's basename, else the head `#N`) — and the hover leads *Train of 2 PRs overlapping main.go; merges after #15567.*, so two "1st"s on one page tell apart (one train → no tag); the header and every member row of a train share one rail accent (`.ham-train-k0…k3`, cycling in page order); once the front merged and the hive is rebasing a conflicting sibling, "Rebasing · attempt 1/2"; when the loop guard (`auto_merge.merge_train.max_rebase_attempts`) is spent, "Needs human decision · rebase failed twice" at stage/085 with the decision option "Rebase, then merge as maintainer".

The tooltip lists the PRs ahead and the overlapping files. Waiting siblings are never red, never `needs human`; a `needs-rebase` label on one is reported under `stale_labels`, the train repairs it. Each item carries `merge_gate.state` (`queued-behind` / `rebasing` / `rebase-exhausted` / `front`, with an honest `reason` such as `behind #15074 in merge train (3rd of 4, overlapping files: stage_tracker.go, index.html)`) and `merge_train{id, state, front, position, size, attempt, max_attempts, overlap_files, ahead, members}`; the id is `<base>#<front>` and `members` is the whole roster in merge order.

**The header counts the rows under it.** A member drawn as a sub-row of its linked issue (`nested_under`) still sits in the train run — the issue row rides its open PR's train, in that PR's merge order, and wears the rail — so a train of three is three rows under one header whether or not its PRs have issues. When a filter or tab hides some members, the header says so and names them: `merge train · 1 of 3 PRs here · 1st: #15425 · not in this view: #15529, #15530`; it never claims `3 PRs` over one row (hive's v5#15425 did, with #15529 and #15530 drawn under their issues). No member on the page, no header.

**Who rides.** A PR the lane leaves to a person is not a train member and never its head: an author below the lane's trust (`untrusted-author-role`, `contributor-lane:no-linked-issue`, …), the title/label human-merge rule, a human-merge path, a merge GitHub refused for review, and every other `human_merge_reason` code except the catch-all `policy`. The lane's verdict is remembered per lane and head (`noteMergeTrainOutcome`); a new head starts clean. A head the lane last saw unable to merge on its own (red required check, terminal hold, permanent refusal) stays a member but never fronts.

**PRs under repair leave the train.** A PR somebody is fixing is not a train member either: the planner (`mergeTrainUnderRepair`) skips a PR at 070 fixing, 071 needs-author, 072 returned-to-contributor or 085 needs-decision (label or stage store), and the tracker (`stageTrackerTrainUnderRepair`) drops one whose stage says so or whose `reason` is a red required check (`required-check-failing:` / `check-failing:` / `ci-red`) — hive's own #15405 once led a header as `merge train · 2 PRs · 1st: #15405` while its own chevron read *Fixing* (a maintainer had just requested changes). The dropped row carries `left_train: {why, note}` and no `merge_train`: no ordinal, no rail, and its hover ends with the one muted line *Merge train: left the merge train while being fixed; rejoins when green*. The survivors close ranks — `size`, `position`, `front`, `ahead`, `members` are recomputed, and the next in line fronts when the front itself left — and a train left with one member is no train: no header, no ordinal, the 080 row back to *Green and approved; waiting for the automerge lane to admit it*. The exception is the `rebase-exhausted` tail at 085: the train put it there, and it stays so the person who rebases it sees where it merges. Provisional members (050 / 060 with nothing red, [#15580](https://github.com/hivecommons/hive/issues/15580)) keep their slot.

**Queue position, countdown, stall** ([#15395](https://github.com/hivecommons/hive/issues/15395)). The queued chip is its place in the queue and nothing else — **Merge queued · 2nd of 6 · ~4m** — so it reads the same whether or not a merge sweep is evaluating it: the sweep is the spinning ⬡ glyph and a *Checking now…* line in the hover, never a change of words (one label per chevron), and no state slug (`queued-behind`, `front`) ever reaches the page. The hover is the place sentence — *2nd of 6 in the merge queue · behind #15253 (soaking, 12m left) · ETA ~25m (2 ahead · head soaking 12m · next pass in 40s). Hive merges one at a time, in order.*; the head's reads *1st of 6 in the merge queue · merges next · ETA ~13m.* — or, when the head is on a person or on CI, the blocker instead of a clock: *… behind #15253 (waiting for a maintainer (untrusted author)) · no estimate until #15253 moves.* Every train state — the 1st included — lights the **Merge queue** column, whatever numeric stage the lane holds the head at (the train's facts are mirrored onto a head still at 050/070; hive's own #15400 once lit CI / Review beside its 2nd-place sibling). The server adds to each member's `merge_train`: `length`, `head_number`, `head_state` (`needs-human` / `ci` / `soaking` / `ready` / `merging` / `fixing` / `unknown`), `head_reason` (plain words), `head_since`, `eta_s`, `eta_at`, `eta_basis`, `late`, `stalled`, `stalled_s`.

**One PR per issue** ([#15395](https://github.com/hivecommons/hive/issues/15395)). Two open PRs declaring the same `Fixes #N` never both ride the queue (hive's own #15402 and #15407, both for #15396, once sat side by side as "waiting its turn"). Before the trains are grouped the sweep keeps one PR per closing issue — a green head (not conflicting, not blocked on a red required check) beats a red one; among equals the earlier-opened wins — and holds the other out of every train with the stage reason `merge-train:duplicate-of#<winner>:<issue>/0:0/0:`. The held row's gate reads `duplicate-of:<winner>` (`merge_gate.duplicate_of`, `merge_gate.duplicate_issue`), its chip **Duplicate of #15402 · needs a pick** with no clock, it carries no `merge_train`, and it wears `needs-decision` so both PRs sit under their issue with the hold visible. *Let hive decide* on either row takes the existing supersede path (rule `one-pr-per-issue`): on the held PR it closes it as superseded by the kept one with a credit comment, through the same write gate as every delegate close; on the kept PR it releases it and names the duplicate the queue holds (or, if the kept PR has since gone red, only recommends the fix — the queue re-picks on its next pass, so an earlier red PR yields to a later green one).

- **Countdown to auto-merge.** `eta_s` is the member's own remaining soak, plus — for each PR ahead — its remaining soak or, with no clock of its own, one sweep (the lanes' floor cadence), plus the wait for the next sweep tick; `eta_basis` is the short reading ("2 ahead · head soaking 12m · next pass in 40s"). The chip's `· ~4m` counts down in place from `eta_at` on the same tick as the soak clock, and `eta_at` is anchored server-side to the member's first estimate at its position (re-anchored only when a fresh estimate runs more than a sweep longer), so a poll re-render never resets it. Past the estimate it reads *any moment now*; past twice the first estimate `late` is set and the chip reads *running late* with the estimate kept in the hover. Always a `~`: an estimate, not a promise. A head on a person, on CI or being fixed — or a member that is itself a person's — carries no estimate and the blocker instead.
- **Merge queue popover.** The sticky bar's **🚆 Merge queue N** chip (hidden with no train) opens a popover listing every train in merge order — ordinal, #, title, state in words, ETA — with the head's blocker on the front row; same pattern as the **Merged** popover (click outside / Escape closes).
- **Who rides.** A PR GitHub reports conflicting (`mergeable: CONFLICTING` / `DIRTY`) on its own, or one at **072 Returned to contributor** (by label or by the hive's stage record), is neither a member nor counted in the queue's length and draws no train idle comment — hive's own #15307 once read `queued-behind#15424:4/6` while conflicting at 072. A conflicting PR rides only while the train itself is repairing it (its rebase was requested behind a merged head and the loop guard still has attempts).
- **Stall warning.** Once the head has sat in one stage longer than `governor.merge_train_stall_warn_s` (default 3600) and is not merging — a soaking head counts only once its own clock has overrun — every member's chip turns to **Merge queue · stalled — waiting on #15253 (waiting for CI) · 3h** (the head's own row: **Merge queue · stalled — waiting for CI · 3h**; an untrusted-author head reads *needs maintainer merge*), the popover marks the train, and the activity log records one `merge train stalled` line per train per hour.

#### Pills before the merge gate

Before the merge gate, the tracker defers general `sentinel`, `needs human` and `unauthorized` pills so they do not distract from work still in motion. **Exception:** `unauthorized:approve-workflow-runs` appears immediately, because a person must approve the held run before CI can start.

The server splits markers by `ActionableNow` (`stageTrackerModifiers` in `stage_tracker.go`). The payload puts drawn pills in `modifiers` and held-back pills in `deferred_modifiers`.

A deferred pill is simply absent — no chip, no breathing chevron, no **Needs attention** count — until the PR's stage reaches the merge-gate band (`stage/080` and later: 080, 085, 090, 095, 100, the same boundary adoption yields at), when it renders exactly as before and counts; the row tooltip alone says "Pending at merge gate: sentinel, needs-human". The merge-gate cell's own words ("Sentinel alert · needs human", "Awaiting human: …") are unchanged — they only appear at the gate anyway.

Issues on their own stages (Open, 010, 020, 045) keep every pill: a `needs-human` there is a park the person owes now; an issue riding its covering PR's stage follows the PR. The GitHub labels are untouched — this is presentation and counting only.

| pill | class | when shown |
| --- | --- | --- |
| `hold` | actionable now | always, whether set by a person or Hive; release rules still depend on provenance |
| `draft` | actionable now | always (the author's move) |
| `stalled` | actionable now | always |
| `regressed` | actionable now | always; muted `stage ↓` unless the row is parked on a person (see above) |
| `needs-rebase`, `blocked`, `triage needed`, `possible dupe` | actionable now | always |
| `unauthorized` — `unauthorized:approve-workflow-runs` | actionable now | always — the carve-out: held workflow runs mean CI cannot even start, and waiting for the gate would waste the whole CI / review cycle (rationguard#397 at 072) |
| `unauthorized` — any other step (merge, branch update, no step named) | merge gate | PR at stage/080 or later |
| `sentinel` (the trusted-author notice and the real `sentinel-alert` alike) | merge gate | PR at stage/080 or later; `sentinel_alert` itself stays truthful in the payload for the lane word and the Decide… preface |
| `needs human` (`needs-human` label) | merge gate | PR at stage/080 or later; always on an issue at its own stage |
| `needs human decision` (`needs-decision` labels on another stage) | merge gate | PR at stage/080 or later; always on an issue at its own stage |
| history decision badge (`last_operator_action`) | before the pills; the history mark + action count, neutral / amber (still at the same stage past the stuck window) / red (failed) / dotted (hive deciding) | always, once a row action was submitted from the dashboard; tooltip tells what you chose, when, and what the hive did next |

#### Returned to a contributor's hive (072)

`stage/071-needs-author` is a person's PR: a human-authored pull request sent back by a maintainer's CHANGES_REQUESTED review or by the review swarm. The same verdict on a clanker-authored PR (`author/clanker`, or an author resolved to a contributor's hive agent) routes to `stage/072-returned-to-contributor`, whichever lane carried it. The actor is the owning contributor, the caption reads `Returned to @thatguywob's hive — @clubanderson requested changes`, and the normal contributor nudge goes to that hive ([hivecommons/hive#15159](https://github.com/hivecommons/hive/pull/15159): a human's out-of-cycle review on a clanker PR parked it at 071 under a maintainer's name).

The 071/072 chevron names the routing — `Needs @sam`, `Returned to @thatguywob's hive` — and a sentinel finding on such a row is a ` · sentinel` suffix on that word (the chip folds into it, the tooltip carries the finding). `Sentinel alert · needs human` is only the chevron word when the maintainer's review is still outstanding (085). A trusted human's CHANGES_REQUESTED review on the current head *is* the human those parks (sentinel alert, `hold`, workflow-path) were waiting for: the PR routes to 071/072 and the sentinel pass does not re-assert 085 until a new head re-arms the alert.

A return placed by hand counts too: when a trusted maintainer swaps `stage/085` for `stage/071`/`stage/072` on the PR, the reconciler reads the label event once per head, records it as that maintainer's change request against the head, and keeps the stage. The hive's own label events and untrusted actors are not honoured.

**Give direction** on a clanker PR is accepted as well — the direction is posted to the contributor's hive as a change request and the row moves to 072. Only a person's PR, which no hive can carry direction to, still answers `409 the hive did not open this pull request`.

#### Adoption pill (072)

A nudged `author/clanker` pull request at `stage/072-returned-to-contributor` carries `adoption{author, due, blocked_by, since, nudges, author_online, author_registered, author_last_seen, author_maintainer, policy, action, text}` — the nudge count and first-nudge time from the stage record, the author's live hub presence (the same lookup the contributor-offline adoption itself consults), and, when the adoption engine judged adoption due and held it, why (`blocked_by: author-is-maintainer`). The row's caption and an amber pill say it in plain words instead of a bare "Returned to contributor": `waiting on @hanthor (maintainer, offline 26h) — adopt?` under `governor.labels.stage.adoption.maintainer_policy: ask`, `waiting on @zoe (offline 40m, 2 nudges)` while the default `adopt` policy's clock runs, `… — not adopted: maintainer_policy is never` under `never` ([hivecommons/hive#14524](https://github.com/hivecommons/hive/pull/14524): the PR sat 26h at 072 with its author's hive gone and nothing on screen saying why).

When `action` is `decide:adopt` the row also carries `decision_needed` (question "@hanthor maintains this repository and has been offline for 26h; adopt the PR?", `blocker_reason: adoption:author-is-maintainer`, options Adopt then Hold/Release), so the lit 072 chevron and the `adopt?` pill open the Needs-human popover, and the row menu and the selection bar offer **Adopt (hive takes over)** — same action-id vocabulary as the other `decide:*` entries — which, after an in-app confirm, POSTs `{"decision":"adopt"}` to the item's decision endpoint: the hive posts an attributed note, sets `adoption/supersede` (the owner-delegate's request label) and the next sweep adopts the head on the existing adopt/supersede path; the toast reads `adoption-queued`.

Write access required; the action is absent when nothing is the owner's to decide.

#### Adopted → #N (074 / 085)

An adopted pull request names the hive's superseding copy in its detour word (`adopted_by`). When that copy was closed without merging (hivecommons/hive#15052) the row does not fall back to *Needs human decision*: `adopted_by_state: "closed"` renders "Adopted → #N (closed) · recovering" while the hive opens a fresh copy from the same head, and the caption says so in plain words; once the new copy is on record the word reads "Adopted → #M".

If two copies are closed within an hour the hive stops (`adopted_by_state: "closed-twice"`, park reason `adoption-child-closed-twice`): the row reads "Adopted → #N (closed twice) · needs a person" and the popover question asks you to reopen the last copy, give direction, or close the PR. With close-first supersession (the default, hivecommons/hive#15135) the original is closed in the adopting pass and has **no row** while its copy is open — the copy is the one active PR for the issue, and the original's record is superseded (no chevron, no *Needs human decision*).

A superseded original keeps a muted, chevron-less sub-row under the issue while its superseder is open and listed (hivecommons/hive#15511 showed hive's #15574 *Merge queued* and, beneath it, the still-open #15526 as a greyed line whose note "· Adopted → #15574 · superseded (hive re-authored with sign-off)" wrapped to two lines). The note now reads **"superseded · hive adopted → #M"**, or **"superseded · closed · hive adopted → #M"** once the original is closed, and never wraps — the PR title ellipsizes first; the hover carries the long form ("Hive re-authored this PR as #M with the same commits and sign-off. This PR closes as superseded when #M merges." / "…This PR is closed as superseded."). The tracker sets `superseded_by: M` on the original (omitted for an in-place adoption, cleared when the copy is closed / closed-twice / copy-failed so the original counts again) and `adopted_from.state` / `lineage.from_state` on the copy (`"open"` or `"closed"`). A closed original is listed in this muted form only while #M is open and listed; it leaves the tracker once #M merges or closes. Agent-authored originals close in the adopting pass by policy (`governor.labels.stage.adoption.close_superseded`, default `on_adopt`); a person's PR closes then only with the write-back opt-in label, else when the copy merges — see [hive-merge.md](hive-merge.md).

A copy that then fails (closed unmerged, or red at two heads) reopens the original: `adopted_by_state: "copy-failed"` renders "Adopted → #N (copy failed) · reopened, needs a person" at 085 with the caption "The hive's copy #N failed; this PR was reopened and needs a person".

#### Presence indicator on the waiting-on pill

Every PR row that waits on a named person — `stage/071-needs-author`, `stage/072-returned-to-contributor` (before and after the nudge), `stage/017-possibly-existing` on a PR — carries `presence{login, state: online|offline|unknown, kind: clanker|human, since, last_seen, next: wait|adopt, adopt_at}`, read from the contributor hub's live presence (registered profile + relay heartbeat: the same lookup adoption consults, so the indicator and the adoption text never disagree) and projected through adoption's own clock ([hivecommons/hive#15051](https://github.com/hivecommons/hive/issues/15051)). The pill draws it just before the `@login`: a filled **dot** (`--cue-decision`, a soft breathe; none under `prefers-reduced-motion`) when the person's contributor agent is online, tooltip "@Danathar's clanker: online, last seen 2 min ago — hive is waiting for them"; a filled **square** (`--status-error`) when it is offline, "@hanthor's clanker: offline 26h — hive will adopt at 15:10 (in 38m)" (or "— hive is waiting for them" when `ask`/`never`/adoption-off holds the clock); a **hollow ring** when no contributor agent is registered here, "@zoe: no clanker registered — waiting for a human reply" with the pill reading `waiting on @zoe (human · no clanker)`.

Shape, not colour alone, carries the state; the indicator has its own `aria-label` ("@hanthor: clanker offline — hive will adopt at …"). A row with presence but no adoption line (071, a 072 not yet nudged, a human's PR) gets a muted `waiting on @login` pill carrying the indicator; a nudged 072 row keeps its single amber adoption pill with the indicator inside it. The Needs-human popover shows the same line under the question when the waiting person is named, a per-PR sub-row under an issue mirrors it, and the row tooltip adds a `Presence:` line.

It refreshes with the tracker like every other pill.

**An offline clanker's PR that has not been returned.** A clanker-authored PR the engine has not classified yet (the chevron reads `PR in progress · @odttlnt's clanker`) carries the same `presence` for its author, with `next: wait` and no `adopt_at` — the adoption clock runs only at 072. When the clanker is offline (or not registered here) the chevron's holder gains a muted suffix with the state square: `PR in progress · @odttlnt's clanker · ■ offline 3h`, and the caption states the next automatic step: "@odttlnt's clanker is offline 3h; a green PR is reviewed and merged without its author — hive adopts it only once it is returned (red CI, conflicts or review asks) and nothing is pushed for 15m" (`governor.labels.stage.adoption.after_offline`). A green PR does not need its author online: review and merge admission run on the contributor lane regardless; adoption is the exit for a PR that is red, conflicting or asked to change while its clanker is away, never for a green one, and never for a human's PR.

A contributor PR whose `stage/*` labels the write-back gate kept off GitHub (no opt-in label, see [What hive writes to GitHub, and when](operator-reference.md#what-hive-writes-to-github-and-when)) is tracked from the engine's record alone: the record carries `labels_suppressed` and the tracker trusts its stage (085 reads *Needs human decision*, not *PR in progress*) even though the item's labels say nothing. Until this fix the tracker read the missing park label as a lifted park and dropped the record, so a dozen fork PRs parked at 085 for a human's decision sat under "PR in progress · @x's clanker" for hours while their clankers were offline.

#### Rework pickup on the waiting-on pill

**Rework pickup on the waiting-on pill.** The same 071 / 072 rows also carry `rework{state: requested|acknowledged|in_progress|pushed, actor, requested_at, since, evidence: queued|claim|activity|commit, mode, ahead, head}` — whether the person the row waits on has picked up the rework the hive (or a human reviewer) asked for ([hivecommons/hive#15280](https://github.com/hivecommons/hive/issues/15280): "waiting on @x" said who, never whether anything had happened since). It is derived from data the tracker already holds, no extra GitHub calls: `requested_at` is the first nudge the contributor engine recorded on the current head (`stage/072`) or, before any nudge, when the row entered the stage (`stage/071` after a human's changes-requested review); **pushed** when the PR's head is no longer the head the request named (`evidence: commit` — the stage engine routes the row back to `050` on its next sweep, the pill says `re-review pending` meanwhile); **in_progress** when the author's clanker relay — or, after a Reroute, the target's — has taken the `fix-pr` task from the contributor fix queue (`evidence: claim`, `mode` spawn/preempt/boundary, `since` the dispatch); **acknowledged** when the PR's `updated_at` moved after the request and after the hive's own nudge comment (`evidence: activity` — a reply, a review-thread answer, an edit; reactions do not bump it, so an 👀 alone is not seen); otherwise **requested**, with `evidence: queued` and `ahead` when the fix is enqueued on the clanker's hub queue but not yet dispatched. The pill appends the state after the login — `waiting on @x · no response`, `· queued`, `· acknowledged`, `· working`, `· re-review pending` — and its tooltip (and the row tooltip's `Rework:` line) says since when: "rework requested 1 hour ago (13:32 UTC) · no response from @x yet", "@x acknowledged · activity on the PR 20 min ago", "@x's clanker picked up the rework 5 min ago (spawn)", "@x pushed a new head 1 min ago · re-review pending". A state change at an unchanged stage writes the row's event line ("@x acknowledged the rework request", "@x's clanker picked up the rework", "@x pushed a new head — re-review pending"). The presence indicator stays; a nested per-PR row under an issue mirrors the field.

**Push detection requires a nudge baseline.** The `pushed` state compares the current PR head with the stage record’s `NudgeHead`; entering `071` alone records a time, not a head. A `071` row that has never been nudged therefore cannot show `pushed` / `re-review pending` from a head change. If the push moves `updated_at` past the activity threshold, the pill may instead say `acknowledged` (`evidence: activity`); that signal does not distinguish a push from a comment or edit. The stage engine still routes the new head to `050` on its next sweep. This limitation affects the interim pickup signal, not re-review routing ([hivecommons/hive#15286](https://github.com/hivecommons/hive/issues/15286)).

#### Adopt now / Reroute (072, 071)

Every `author/clanker` pull request waiting on its author's hive — `stage/072-returned-to-contributor` or `stage/071-needs-author`, not yet adopted — also carries `takeover{author, author_online, adopt_now, adopt_now_reason, reroute, reroute_reason, clankers[], rerouted, forced_by, forced_at, fallback_at, grace_elapsed}`, and the ⋯ row menu (and the nested per-PR row under an issue) gains two operator actions beyond Hold / Re-evaluate / Mark needs-human / Open / Copy ([hivecommons/hive#15038](https://github.com/hivecommons/hive/pull/15038): "Returned to contributor · waiting on @Danathar" offered nothing to end the wait). **Adopt now — hive takes it over** (`decide:adopt-now`) carries the consequence line *closes the contributor's PR after hive opens its copy; their commits + sign-off are preserved* under the label, asks an in-app confirm (don't-ask-again supported, keyed `hive.tracker.confirm.decide:adopt-now`; the confirm adds "its hive is online right now" when it is) and POSTs `{"decision":"adopt","force":true}`: the hive posts an attributed `<!-- hive:adoption-requested by=<login> force=true -->` note, sets `adoption/supersede`, marks the stage record forced and nudges the sweep, which runs the existing supersede adoption at once — presence, the grace windows and `maintainer_policy` are all bypassed (`hold` still stops it; the menu says *release the hold first*).

The persistent result toast reads `Adopted → #N (hive's copy), contributor PR will close when #N merges` when the sweep lands inside the handler's wait, else `Adoption forced: hive opens its copy on the pass now running`; the audit entry is `repo_item_decision_adopt_now` with `action=adopt-now`. **Reroute to another clanker** (`decide:reroute`) opens a submenu of the currently online registered contributor agents from the contributor fleet (`@login · hive · online 12m ago · 1 in hand` / `idle`, via `clankers[]` — the same facts as `/api/contribute/fleet`) plus *any available (N online)*; in the Needs-human popover it is a required choice list.

The pick (after a confirm keyed `hive.tracker.confirm.decide:reroute`) POSTs `{"decision":"reroute","to":"<login>|any"}`: the hive enqueues the PR's follow-up work (the stage record's return reason — the failing check or the reviewer's requested changes — plus the operator's comment) on the target's contributor fix queue and dispatches it as a `fix-pr` `task_assign` interrupt, the same path the 072 nudge takes to the author's own relay; the task's instruction says whose PR it is and that the target either pushes the fix (if the branch allows) or opens a replacement PR carrying the author's commits and sign-off.

The author is told by `<!-- hive:rerouted to=@X by=@op reason=… -->`, the row shows a `rerouted → @X` pill with a presence dot (green while the target's relay is on the hub), and the toast reads `Rerouted → @X (dispatched now, preempt)` / `(queued; N ahead)`. If the target's relay has not taken the task within `governor.reroute.pickup_timeout` (default 30m) the hive falls back to Adopt now by itself: the queued item is withdrawn, the record marks the forced adoption, a `lapsed=true` note goes on the PR, a warning system alert and a `repo_item_reroute_fallback` audit entry are raised, and the sweep is nudged.

With no contributor agent online, the item is disabled with the reason *no clankers online — use Adopt now*. Both actions appear in the Needs-human popover's options when the blocker is a waiting-on-contributor condition — once the nudge grace has elapsed and the engine is not adopting on its own, the row carries `decision_needed` (`blocker_reason: adoption:<why>`, options Adopt now, Reroute, Hold) — and **Let hive decide** picks Adopt now (`grace-elapsed-adopt-now`) when the grace has already run out.

Write access required.

#### Blocked pill

An issue wearing `blocked` because the hive parked it on a pull request — its body carries `<!-- hive:blocked-on pr=owner/repo#N -->` (the review backlog's spin-off issues, `hive-open-issue comment --blocked-on-pr`) — names that PR: the payload carries `blocked_on_pr: {repo, number, url, state}`, the pill reads `blocked on #N` and links to the PR, its tooltip says `Blocked on PR owner/repo#N (open) — lifts when it merges; re-triage if closed unmerged`, the caption reads `Blocked on PR #N — lifts when it merges` instead of `No stage recorded yet`, and the row's title and chevron tooltip carry the blocker too ([#14952](https://github.com/hivecommons/hive/issues/14952): #14863 showed a bare grey pill and "blocked · since 7h" with nothing to say what it waited on).

The state (`open`, `merged`, `closed`, or absent when nothing knows) comes only from what the hive already holds — the status snapshot's open PRs, the ForgeState cache, the recent-merge ring — never a GitHub call from the tracker build; a merged or closed-unmerged blocker says so (`merged; the blocked sweep lifts the label next` / `closed unmerged; the blocked sweep re-triages it next`). The server reads the marker from the issue body when it has one, else from the enumeration's `blocked on owner/repo#N` reason, so sidelined rows (which carry only the reason) name it too.

A `blocked` issue with neither (a human-applied label) keeps the bare pill. This is distinct from `blocked_on`, the open GitHub "blocked by" dependencies of a sequenced sub-issue ([#14765](https://github.com/hivecommons/hive/issues/14765)); when a row has both, the chevron tooltip lists them together.

#### Blocked pill — capability gaps

**Blocked pill — capability gaps** ([#15099](https://github.com/hivecommons/hive/issues/15099)). An issue parked because the hive *cannot* act — the scanner reported "the App token cannot push `.github/workflows/*` (no `workflows` permission); needs a human" — is a blocker, not a question, so its row does not read *Needs human decision*: the pill reads **Blocked · App lacks workflows permission** and the same popover says in plain words *Hive can't do this one: the GitHub App has no `workflows` permission, so it cannot push changes to .github/workflows/\*.* followed by the ways out: *Grant the App workflows permission* (opens the App settings page; the hive re-tries on its own once it sees the permission), *Re-check permissions* (probes the installation now and releases every item blocked on it), *Prepare the patch for a person to push* (disabled with its reason until a human-authorised push identity exists), *Mark partially addressed and close*, *Keep waiting*. *Let hive decide* stays in the list but disabled with the sentence that explains why — a direction decision cannot grant a permission — instead of being greyed silently; running it anyway returns *no-effect* with the same words and never applies `approved-direction`. The loop that motivated this (un-park → sweep re-parks with the same text → "I tried let hive decide and it still needs human") is guarded: when the same needs-human reason comes back after a delegated un-park, the row is parked with a blocker and *Let hive decide* is disabled on it until the reason changes. The blocker comes from the agent's `<!-- hive:blocker kind=… -->` marker or, failing that, from the prose of the needs-human report (`pkg/blocker`), and ⚙ → Security shows the fleet view: *GitHub App permissions: missing `workflows` · N items blocked* with the settings link (`/api/status.capabilities`).

#### New row (`000-new`, the pre-stage state)

Every open issue and pull request in `project.repos` is on the list from the moment GitHub reports it — there is no gap between "opened" and "the hive has an opinion about it" ([#15094](https://github.com/hivecommons/hive/issues/15094)). The `issues.opened` / `pull_request.opened` webhook re-read inserts the item into the status payload straight away (`insertFrontendRepoItem`; the governor side keeps the same row in its cached snapshot so a republish does not drop it), the per-item replay re-applies that insert after a governor republish, and the enumeration poll is the fallback for a missed delivery.

Until something places the item — a stage record, a `stage/*` label, a hold, a blocked marker, a bucket other than *reporter triage* / *unclassified* — the row is anchored in the **Open** column with the lit chevron reading **New** (`ham-open-new`, blue ✦ rather than amber) — or **Triage needed** when the next mover is a maintainer, see above — and a caption that names who moves it next and when: `New · waiting for the stage-label pass (→ 010 actionable), then the scanner (next pass in 7m)` for an issue the reporter gate has already admitted, `Triage needed — accept or close` for a human contributor's issue the reporter-trust gate is still holding (the hover still names the label to add and the next pass) (the label names come from `issues.filter.reporter_trust.untrusted_require_labels`), and `New · waiting for the stage-label pass (→ 050 review pending)` for a pull request.

The *since* time is the item's `created_at`; "next pass" is the governor's next kick (`governor.nextKickAt`, so it counts down with the Governor tile and reads *due now* once it has passed). The payload carries it all as `new{since, next_mover (maintainer|hive), next_mover_text, next_pass_at, next_pass_in_seconds, gates[], text}`; the chevron tooltip lists the gates ahead — **triage** (the reporter gate: done when admitted, next when a triage label is still needed), **actionable** (the stage-label pass marks it 010), **claimed** (the scanner claims it, 020) — each marked ✓ done, → next or · later, and the row's actor icon reads *human* while the next mover is a maintainer and *hive* otherwise.

A row that already wears a hold, `blocked`, a draft marker or any stage record is never *New*; neither is an issue at `015` once the stage-label pass has written it.

#### Advisory row (`triage/advisory`, dedupe checklists)

An issue that is a note for maintainers rather than work — one wearing `triage/advisory`, a dedupe-agent checklist (body opens with `<!-- hive:dedupe-checklist repo=… -->`) or any body whose verdict line reads `Decision required: no` — is an **advisory** ([#15381](https://github.com/hivecommons/hive/issues/15381) sat for hours as a plain *Open · since 3h*, indistinguishable from stalled work). Its row keeps `kind: issue` and carries `advisory{kind: "advisory", source (label|dedupe-checklist|decision-no), empty, chip, caption}`; the Open chevron reads the chip — **Advisory · read & close**, or **Advisory · nothing found · safe to close** when the checklist's own counts are all zero — muted (`ham-open-advisory`, ⓘ), with no *· since Nh* suffix and never the stale tint. The caption is *Advisory for maintainers · read, then close* (or *… · nothing found · safe to close*), the actor is a human with nothing waiting on them, the row is never *New*, never *stalled* or *sent back*, and it is excluded from **Needs human** and **Needs attention**. The row menu leads with **Close (read)**, which posts the existing `POST /api/repos/{owner}/{repo}/items/{number}/decision` with `decision: close-read`: a "Closed as read (advisory acknowledged) by @you from the Hive dashboard" comment, then the issue closes as *completed* (operator action `close-read` in the row's history).

The enumeration computes the body-derived part (`RepoWorkIssue.advisory`) because sidelined `hive_advisory` rows do not carry the body. Upstream, the issue-request relay refuses an agent's `issue-create` whose body is a dedupe checklist with no actionable finding (verdict `no`, zero near-duplicate pairs, zero resolved candidates, zero missing PR↔issue links, no `- [ ]` task lines): the agent's result file says why, the write audit records `agent_issue_empty_advisory_refused`, and a knowledge bead keeps the summary in the hive's own log — merge-order reminders and cross-links are notes, not issues.

#### Hive's own unblock is live

When the blocked sweep or the review-backlog block reconciler lifts `blocked` itself (the tracked PR merged or closed; the `<!-- hive:unblocked -->` note), it takes the same path as an inbound label webhook: the tracker cache is dropped and the issue is queued for the targeted re-read (`ItemPatches` delivery `self:edit`), which patches the status row and the governor's cached snapshot at once. A reconcile enumeration that was already listing when the unblock happened used to overwrite that patch with its pre-unblock copy, and the rows reverted to `blocked` until the next pass (~an hour on the hive's hive); the governor now replays re-reads newer than the listing's start onto the pass's result before publishing it, so a row never moves backwards behind a read it has already made.

#### Closed issue, open PR

A pull request whose `Fixes #N` issue has already closed (auto-closed by a bot, or closed by hand) while the PR is still open is live work, not a stale line: the row is anchored on the PR ([#14655](https://github.com/hivecommons/hive/issues/14655)). The PR's own icon, number and title lead the row at full width, its stage drives the strip and its timestamps sort it — closing the issue is not a regression of the PR — and the closed issue shrinks to a muted `closes #N · closed` chip linking to it.

The chip names the closer when it can. The server asks GitHub which *merged* pull request closed the issue (`closedByPullRequestsReferences`; the hive's own merged ledger is the fallback) and the slot reads `closed by #N`, linking to that PR — the open PR on the row is then a follow-up or a duplicate, and merging it will not close anything. An open PR whose body says `Fixes #N` is listed under the same GitHub relation but is never named: it has closed nothing. When no merged closer is known the chip reads `closed · this PR unmerged`, with the hover spelling out that a person or another merged PR closed the issue; it never reads as a bare `closed`, which looked like the issue had been closed before its fix merged (hive's #15422 under follow-up #15425, #15058 under #15431). The hive itself closes an issue only after its PR has merged (see [Issues close only after the merge](hive-merge.md#issues-close-only-after-the-merge)).

An issue closed as *not planned* adds an `issue closed (not planned)` chip so the reader can tell the PR may be moot. At 090 such a row still counts under *Needs attention*: the human has a merge to click. When the PR closes or merges too, the row leaves the list as before (a merged PR shows in *Recently merged* for its retention window).

#### Row gutter

Every issue and PR row (never a column or repo-group header) carries a left gutter with a select checkbox and a ⋯ actions button that is always present and always visible: dimmed at rest (`--ham-tool-rest-opacity`), at full emphasis on the row under the pointer or holding focus, on a selected row, and on every row while anything is selected or the context menu is open — the only animation is the hover fade (`--ham-tool-fade-ms`, ≤ 120 ms), and nothing about it depends on scrolling, row position or which rows the live refresh re-created.

#### Did I act on this? (the decision badge)

Every row action submitted from the dashboard — Approve, Give direction, Close as superseded, Close as won't-fix, Let hive decide, Hold / Release hold, Accept triage, Mark needs-human — is written to a server-side ledger (`/data/operator-actions.json`, `operator_actions.go`; newest 20 per item) as `{action, actor, at, note, target, outcome, detail, head_sha, stage}`, so the record survives reloads and shows in every browser.

The tracker row carries the newest one as `last_operator_action` (+ `operator_action_count`) and `GET /api/repos/{owner}/{repo}/items/{number}` returns the full list. The row then wears a small **history badge** in the cap cell, before the state pills (`.ham-acted`): the clock-with-counter-clockwise-arrow mark the right-most column header also carries (hover: "History — decisions and actions on this item"), with the count of recorded actions beside it — never a ✓, which read as "done/passed" (owner, 2026-10-28).

A filled neutral pill once an action is recorded; **amber** when the item is still at the same stage past the stuck window (`HIVE_OPERATOR_ACTION_STUCK_AFTER`, default 30 min — "acted, but nothing happened"); red when the action itself failed; a dotted border while *Let hive decide* is still running. Below it, the strip's cap chevron wears the same history mark in muted outline until the PR merges, when it becomes the green ✓.

The in-flight spinner on the chevron is a different cue: that is the request still on the wire; the badge appears once the server has recorded it. Hover or focus (the badge is in the Tab order) opens an in-app tooltip — never a native `title` — written in plain words, no stage codes, in the order every cue on the row uses: **what it is waiting on → what you chose → what the hive did next**.

E.g. `At Merge queue — waiting on CI.` / `You chose Approve · 15:55 (5 min ago) · done — released to the hive.` / `Then the hive moved it to Merged — a reviewer approved it · 1 min ago.` Once the item moves on, the badge stays and the first line reads `Addressed — now at <stage>`; while stuck it reads `Still at <stage> 42 min after you asked — hive says: <reason>`. Another operator's action is named (`@someone chose …`); your note and "N earlier actions on this item" follow. The **Decisions** select in the toolbar narrows the list: *Awaiting my decision* (parked on a person, no action recorded yet, or the last one failed), *I acted* (any badge), *Acted, still stuck* (amber badges only).

#### Legend — the three cues a row may carry

The *state pill* says where the item is and what it is waiting on (`hold`, `needs human`, `stage ↓` …). The *working* cue on the lit chevron says Hive is doing something in that stage now; its tooltip explains the work. The *decision badge* (history mark with count) says a person acted here and what followed. These are separate signals: a recorded decision does not mean the requested effect happened.

Plain words throughout — working / waiting on X / set back by Y / merged — nothing else blinks.

#### The row's lead action, inline

A row whose next move is yours does not hide its answer behind three dots. Beside the chips on the right of the header (and on a nested PR sub-row's own header), the row renders its **lead action** as a real `<button class="ham-lead-act">` — `[Accept triage]` on *Triage needed* (000-new hold or 015), `[Merge now]` (or the server's lead answer) on *Ready for human merge*, `[Let hive decide]` on *Needs human decision* and a duplicate's *Needs a pick*, `[Adopt…]` on a returned contributor PR whose adoption is due, `[Close (read)]` on an advisory issue. `stageTrackerLeadAction(item, ctx)` picks it from the same `stageTrackerRowActions(...)` list the ⋯ menu shows: the entry flagged `lead`, else the first *Decide…* option, else Adopt / Adopt now / *Let hive decide — all N PRs*; one button only, everything else stays in ⋯. It carries the menu entry's id, its hint as the hover and its enable/disable reason: a viewer without write access sees it greyed with *Sign in with write access to this repo*, a non-merger sees a merge action greyed with the role reason, never hidden. Clicking runs `runRowAction(action, row)` — the menu's own path, so the busy chevron, the acting caption, the toast and the history badge are the same. A row the server marks as the maintainer's move (`actor: maintainer` or `new.next_mover: maintainer`) that yields no lead is a bug: a development host logs a console warning, and `TestStageTrackerMaintainerRowsHaveLeadAction` runs the real fixtures through the picker.

#### Row actions (right-click, ⋯, Shift+F10)

Right-clicking a row — or a nested PR sub-row — opens an in-app context menu of the actions that fit that item's current state; the ⋯ button in the row's left gutter (every nested PR sub-row has its own gutter with its own checkbox and ⋯) opens the same menu for pointer-free use, and so do **Shift+F10** and the context-menu key while the row or anything in it has focus (every row is a tab stop).

The menu is positioned at the pointer, clamped to the viewport, uses `menu` / `menuitem` roles, moves focus with ↑ ↓ Home End, opens the *Decide…* group with → and closes it with ←, and closes on Esc, Tab, click-away, scroll or resize, returning focus to the row. Shift+right-click keeps the browser's own menu. Entries, each shown only when it applies: **Decide…** on a row waiting on a person — a PR wearing the *Needs human decision* pill at 085 on the Merge queue column (the primary case: e.g. two competing `dibs` PRs), an issue row whose one nested PR is parked there (the menu reads *Decide PR #458…* and acts on that PR, since the issue's own stage is only mirrored from it), or an issue parked at 045.

**Progress and failure, for every action that writes** ([#15436](https://github.com/hivecommons/hive/issues/15436): Close (read) on an advisory did nothing visible — the row is not a decision park, so the click mapped to no target and returned without a request, a dialog or a toast). Close (read) now acts on the advisory row itself; any `decide:*` click whose target list comes up empty says so in a warning toast. The moment a mutating action is sent — close-read, close-superseded, won't-fix, hold, approve, merge, decide — the clicked row's chevron takes the striped busy state with the verb as its caption (*Closing…*, *Holding…*, *Approving…*); when the server answers that the item is closed or merged the caption turns terminal (*Closed · leaving list*, *Merged · leaving list*) and stays until the next tracker payload no longer lists the row. The server guarantees that payload: an item whose newest operator-ledger action is a `close-*` that applied is dropped from the tracker for `HIVE_OPERATOR_CLOSED_GRACE` (default 15m) while GitHub's list catches up, so the row cannot be resurrected between the click and the close being visible. A close on a repo the operator has paused is refused with a 200 `status: refused` result (*Close refused: repo paused — …*) and writes nothing. A refused, failed (GitHub 4xx/5xx) or network-errored action restores the chevron at once, toasts the reason, and notes it on the row's history badge (red; the hover's first line is the reason) until a newer server record replaces it — the ledger itself records refused and 5xx answers as `failed` with the one-line why.

**PR-scoped actions on nested PRs** ([#15009](https://github.com/hivecommons/hive/issues/15009): one issue, two PRs — [#15017](https://github.com/hivecommons/hive/pull/15017) *Ready for human merge · untrusted author* and [#15020](https://github.com/hivecommons/hive/pull/15020) *Needs human decision* — and no way to decide one without the other): every nested PR sub-row carries its own ⋯ menu, and its *Decide…* (Let hive decide, Approve, Close as superseded by…, Won't fix…, Give direction…), *Hold* / *Release hold* and the rest act on **that PR** — `runRowAction(action, row)` receives the PR's own row object (number, repo, head), never the parent issue's, and the needs-human popover opened from a sub-row's chevron does the same.

A PR waiting for a person to merge it (*Ready for human merge*, a `human_merge_reason` such as an untrusted author) counts as parked for this purpose. When an issue has two or more open nested PRs the issue row's menu no longer offers a single *Decide…* that would silently pick one: it offers **Let hive decide — all N PRs** (hint *per PR*), which asks the server to decide each PR on its own (see [Let hive decide](#let-hive-decide)); the issue's own *Decide…* stays only when the issue itself is parked at 045.

*Close as superseded by…* on a nested PR with exactly one open sibling under the same issue pre-fills that sibling's number. Within the general *Decide…* group, the first entry is *Let hive decide* (see [Let hive decide](#let-hive-decide) below): the hive decides as the owner's delegate and records it the way a person's decision is recorded. For a PR: *Approve (/lgtm + /approve)* posts those two commands as comments attributed to you and records your approval against the PR's current head as the human answer the row was parked for — a `requires_human` / decision-requested reviewer verdict, the hive's own `needs-human` park (a supersession question, say), the `sentinel-alert` label and the evidence bundle's sentinel findings are all released by it (the response's `released` lists what was: `approval-recorded`, `needs-human`, `sentinel-label`, `sentinel-acknowledged`), the stage engine re-derives the stage on the spot and the tracker cache is dropped — and the sentinel sweep honours the release for that head, re-arming only on a new push — so the row leaves *Needs human decision* for 080 *Awaiting merge admission* on the next read; the merge gate (and the auto-merge lanes, which honour the recorded approval on that head exactly as a trusted merger's GitHub Approve review) takes it from there; this approval path neither submits a GitHub review as you nor merges immediately. For a direct merge, use the separate 090 **Approve & merge as maintainer** action.

On a Prow repo where the hive bot is not an approver, Prow has already answered the hive's earlier commands with "restricted to collaborators" / "NOT APPROVED"; a later Approve then records the decision without posting the two comments again and says so (`commands_skipped: prow-rejects-bot-commands`, and the toast reads *Approval recorded; `/lgtm` + `/approve` not posted*). A new head carries no answer: pushing to the PR re-asks.

The delegated *Let hive decide* path still posts the comments only — it recommends and never clears a sentinel alert — *Close as superseded by…* (a picker listing the other open PRs in the repo, or type `#458` / `owner/repo#458` / a URL; then an optional comment, then the sweep's labelled close), *Won't fix…* (a comment naming you, then a not-planned close) and *Give direction…* (your text is recorded as `/hive decision <text>` against the head, so the author agent pushes a new head; only on PRs the hive opened).

For an issue: *Approve direction* (records the same `/hive approve` a maintainer would comment: the parking labels come off, `approved-direction` goes on, the stage engine logs the un-park and a reply comment names you; nothing is merged), *Send back to the hive…* (`/hive decision <text>`), *Close as superseded by…* and *Close as won't-fix…*. A selection mixing parked issues and parked PRs has Decide… greyed; **Hold** or **Release hold** depending on the `hold` chip (the same `…/items/{number}/hold` toggle as the repository cards, and the same permission — owner, or a signed-in user with write access to the repo); **Re-evaluate now** (the hive-wide read-only rescan behind the Overview's refresh — a rescan already running is reported, not duplicated); **Accept triage** (only on an issue the chevron calls *Triage needed* — stage 015, or the `needs-triage` chip on another stage — never on a PR; the one-click form of a maintainer accepting triage on GitHub: adds the reporter-trust triage label, `triage/accepted` by default or the first of `issue_filter.reporter_trust.untrusted_require_labels`, removes the awaiting label `needs-triage` / `reporter_trust.awaiting_label` and a Hive-written `stage/015-triage-needed`; the row re-reads *Actionable* with the 👤 gone at once, the reporter-trust gate admits the issue on its next poll and the stage engine records 015 → 010; same permission as Hold, audited as `repo_item_accept_triage`, a no-op when already accepted); **Mark needs-human…** (hidden once the chip is on; merger or owner role, applies `needs-human` and a marker comment through the review pipeline's send-to-human endpoint); **Open on GitHub**; **Copy link**.

Entries you may not use are greyed with the reason in their tooltip, never hidden. Confirmation and free-text input use the dashboard's own dialogs — no browser prompts. Every action is an existing audited dashboard endpoint (the decision endpoint is `POST /api/repos/{owner}/{repo}/items/{number}/decision` and triage acceptance is `POST /api/repos/{owner}/{repo}/items/{number}/accept-triage`, see [api-reference.md](api-reference.md)); none of them bypasses the hive's governance or merges anything.

#### What a row tells you without a click (`tracker-cues`)

The owner's framing (2026-10-28): "Hive is doing the right things mostly; we now need to keep the user informed of it — like a video game they can watch and see what happens easily." Every row therefore reads, in this order, *where it is → whether work is happening on it → what it waits on → what just happened*, with at most three cue types drawing the eye and nothing else blinking.

**Where it is** is the lit chevron (the state pill). **Working** — the chevron's icon turns slowly and the pill pulses only while a hive agent is actually on the item right now (the status payload's `agents[].lastAction` for the current round, matched to the row or to the PR shown on its row); its tooltip then leads with *Working now: scout commented · 2m ago —* before the stage words.

A still chevron means **waiting**; its tooltip already says where it stands and whose move it is (*Needs decision — … · Next move: human*). **Acting** — from the moment you choose a row action the affected row (and the nested PR sub-row when the action targets it) draws faint stripes over its lit chevron and an *Approving… / Hive is deciding… / Holding…* caption with a small spinner; it clears when the tracker reports the target in a new state, at once on a no-op or an error, and never later than three minutes.

**Event line** — one short plain sentence under the strip about the latest thing that happened to the row, from the difference between two tracker payloads (the poll / SSE refetch the tracker already makes; no extra requests): *Hive: moved to CI / Review · 12s ago*, *Hive: fell back to Claimed — a reviewer requested changes* (a lower stage number reads as a fall only when the stage table's `falls_to` — the stage engine's regression rule — says so; triage accepted 015 → 010, a decision made 085 → 080 or the author resuming 072 → 070 read *moved to*), *Hive: waiting on a person*, *Hive: hold released*, *You: approved*.

A row carries only its latest line. **Show, then tuck** (owner, 2026-10-29: "show the hive comments when they are new for 30s. then they should shrink to the left inside a hive-info icon … someone can click on the icon to expand the info for another 30s. then it shrinks back again"): the line opens for 30 s (`STAGE_TRACKER_CAPTION_SHOW_MS`) from the moment it is written, then the text slides a few pixels left and fades (≈400 ms ease-in-out, a wipe from the right) into a small ⬡-i button (`.cue-event-ico`, `aria-label="Hive note"`) sitting where *Hive:* sat; the line keeps one fixed height so the chevrons above never shift. The button's tooltip holds the whole note and its age (*Hive: moved to CI / Review · 2m ago*) and is the only thing hover does; a click (or Enter / Space) re-opens the line for another 30 s, a click while it is open tucks it at once (`aria-expanded` follows). Each row has its own window but one shared timer fires the next tuck; a hidden tab freezes every open window and gives the hidden time back on return, so a note is never spent unseen. Tucked notes stay with the row until a newer line replaces them or the row leaves the payload (no 2-minute expiry). Lines only arise from payload differences, so a freshly loaded page starts with no notes rather than a wall of open ones. Under `prefers-reduced-motion: reduce` the toggle is instant. ⚙ → **Hive notes** → **Auto-collapse hive notes** (default on, `hive.tracker.notetuck`) turns this off: the line then stays open, lives two minutes and dims for its last third as before, with no button. **Set back** — a row wearing `regressed` colours the line's lead red. **Merged** — see *Merged (the chip, the ledger, the flight)* below. The CSS class names are shared so the sibling Activity Monitor PRs (needs-human options, decision trail) compose on them: `cue-working`, `cue-acting`, `cue-event`, `cue-setback`, `cue-merged`, `cue-decision` (badge, sibling PR), `cue-spin` (the one spin keyframe), `cue-flight` / `ham-merged-count` (the glyph and the Merged chip); the colour tokens `--cue-state`, `--cue-work`, `--cue-decision` on `.ham-row` are declared in the *Cue tokens* paragraph under [Let hive decide](#let-hive-decide) below.

Legend, in the operator's words: **working** (icon turns), **waiting on X** (still; tooltip names X), **set back by Y** (red lead on the event line), **merged** (✓ glows, a bee rises to the Merged chip or off the top of the screen). Under `prefers-reduced-motion: reduce` every cue animation stops; the words stay.

#### Result toasts (`runRowAction`)

Every row action — *Let hive decide*, *Approve*, *Give direction*, *Close as superseded*, *Won't fix*, *Approve direction*, *Send back to the hive*, *Accept triage*, *Hold* / *Release hold*, *Re-evaluate now*, *Mark needs-human* — goes through one SPA helper, `runRowAction(action, row)`, which marks the row acting, sends the request, and renders a toast from the server's shared `result` object (additive on every endpoint's existing response: `{action, target: {repo, kind, number, url}, outcome, next, rule?, applied_at?}`, `action` one of `adopted | approved | recommended | closed | directed | refused | held | released | rescanned | triage-accepted | needs-human | noop-already-applied`).

The toast reads, in order, *what happened → on what → what happens next*: a headline naming the clicked row and, when it differs, the target (*Hive decided on acme/console#14938 → PR #14947: adopted (contributor offline)*), the outcome sentence, the ⚠ sentinel line when the head carries one, the *next* line (*The hive rebases and merges it unattended.*), an explicit ✕ and an *Open PR #14947 ↗* link.

An informational result (adopted, posted, held, a no-op) closes itself after **15 s** with the countdown bar described below — hover or focus pauses it, so following the *Open PR* link is never a race — and leaves earlier on ✕ or when the tracker reports the target row in a new stage, caption or chip set (or gone); up to five such toasts stack for pending decisions. A result that offers next actions (the stuck-decision options) is a prompt and stays until the operator answers. A no-op says so plainly — *No change — hive already recorded this decision at 12:01Z; still blocked by needs-human* — and clears the acting state at once; an error toast says what failed and why (*Hold failed on acme/console#7 — write permission required*) and is sticky but not tied to a row.

Every *timed* toast elsewhere in the dashboard (success / info / warning notices, an error given an explicit deadline, an in-flight toast resolved into its result) carries a 2 px countdown bar along its bottom edge, in the toast's severity colour at 40%, that drains from right to left over the toast's real lifetime. Resting the pointer on the toast, or tabbing into it, holds both the bar and the dismiss timer where they are; leaving lets them run on for exactly what was left. Sticky toasts — errors, result toasts offering next actions, the login-URL copy surface — have no bar because they have no deadline. The bar is decorative to assistive tech (`aria-hidden`) and stays visible under reduced motion, since a countdown is information rather than flourish.

Older hives that answer without `result` still get a toast built from the legacy fields.

#### Confirmations and "Don't ask again"

Each confirm dialog a row action raises — Approve (including a sentinel-flagged head: *⚠ Sentinel flagged security-relevant changes at 40a959b …*), Let hive decide, Close as superseded / won't-fix, Mark needs-human and the rest — carries a **Don't ask again for this action** box. Ticking it and confirming stores `hive.tracker.confirm.<action>=skip` in this browser's `localStorage` (one key per action id, e.g. `hive.tracker.confirm.decide:delegate`); later clicks on that action run at once.

Nothing the dialog would have said is lost: the result toast carries the same ⚠ sentinel line (it is per head, so skipping the confirm never skips the warning) and the one-line item summary. Actions that need your text (*Give direction…*, *Send back…*, the superseder picker) always ask. The ⚙ settings dialog's **Confirmations** section lists which actions are skipped and has one **Reset confirmations** button that turns them all back on.

#### Who is on it (actor icons)

The small icon after the lit pill's words names whose move the row is, and it comes from the payload — `stage_actor: {kind, login, since, why}`, derived server-side for every item and every nested PR — never from a guess on a login string. `kind` is one of **hive** (⬡ honeycomb: a hive lane or agent acts next; `login` is the agent when one is on it right now), **clanker** (robot head: a registered contributor agent must push), **human** (the person glyph the *Needs human decision* pill uses: the author, approver or a maintainer must act; `login` names them when known), **bot** (cog: a non-hive automation account such as `github-actions[bot]` or renovate authored the PR; the hive does not fix bot PRs, it waits for the bot's next run), or **none** (no icon: nobody is working on it — the pill's tooltip then reads *no one is working on this right now — <why>*).

`why` is a short sentence the tooltip appends after *Next move: <kind> @<login>*. A `070` Fixing pill is only shown as **Fixing** when someone can fix it; a bot-authored or unowned red PR reads its state in plain words instead — **Conflicting · bot PR**, **Red · nobody on it** — with the matching icon, so a row such as hivecommons/docs#500 (a nightly resync PR that conflicts with its base) no longer looks like active hive work.

Issues riding one covering PR take that PR's actor; nested PR sub-rows carry their own.

#### Who holds it (claimant)

The icon says what kind of actor holds the row; the `claimant` says *who*. On a claimed or in-progress issue and on a PR from the moment it exists through its author's rework and adoption (any stage below the 080 merge gate — an unclassified or 030/040 PR lights **PR in progress** under its issue and names who is working it, [#15541](https://github.com/hivecommons/hive/issues/15541)), the payload carries `claimant: {kind, login, agent, hive, since, source}` and the lit chevron appends one short token after its word: a hive lane name (**Claimed · quality**), the lane and a short hive name when the claim came from another hive (**Claimed · quality@alchemy**, the `hosted-ibm-` prefix dropped), a contributor's connected hive (**Claimed · @bob's clanker**) or a person (**Claimed · @alice**). `kind` is `hive-agent` (`agent` is the lane; `hive` is set only when it is not this hive), `clanker` (`login` is the contributor the hive acts for — a registered contributor with a connected hive) or `person`; `source` says where the identity was read: `claim-ledger` (the worker-claim ledger), `claim-comment` (the `hive:claim` marker, which carries the hive fingerprint), `assignee`, `kick-history` (the agent the status payload says is on it) or `author` (a PR's author or adopting agent; a hive PR names the agent from its `— hive:` attribution trailer, else its `agent/<lane>` label). A PR row checks the worker ledger for a live claim on the PR itself before falling back to its author. The chevron hover adds one line — *Held by: hive agent quality@hosted-ibm-alchemy since 3h ago (claim ledger)*. When nothing names a holder the field is absent and nothing is added. Nested covering-PR sub-rows carry their PR's `claimant`. When the row's next mover is nobody (`stage_actor.kind: none`, the usual case for a PR still being worked), the chevron draws the holder's own kind icon in the actor slot — ⬡ hive agent, 🤖 clanker, 👤 person — so **PR in progress · scanner ⬡** reads exactly like **Claimed · scanner ⬡**; the holder token truncates with an ellipsis before the one-line PR sub-row wraps, and a detour word that already names the holder (**Returned to @bob's clanker**) does not repeat them after the dot.

#### One row per PR

A pull request that closes several open issues (`Closes #A` and `Closes #B`) is drawn once, under the issue the server names in each linked entry's `drawn_under` (its `nested_under`, the lowest-numbered open issue it closes). The other issues keep their own strip and show a compact **↳ #N · under #M** chip beside the title (tooltip: *PR #N covers this issue too; its row is drawn under #M*) instead of a second identical sub-row, so hivecommons/hive#14921 appears once under #14910 and #14911 points at it.

Payloads without `drawn_under` draw as before.

#### Merged (the chip, the ledger, the flight)

A thin bar at the top of the tracker — it stays pinned under the card header while the list scrolls, with the stage column header strip pinned just below it — carries the dashboard's bee glyph (the version card's bee, in a 28 px hit area; the pill is no taller than the chips beside it) with **Merged** and the all-time count drawn as a **quiet odometer at rest, always** — one shallow inset track with a hairline border and faint dividers, a wheel per digit, tabular semibold digits at the chip's own type size, no thousands separator (#15543: the first drum's 18 px bevelled cells were "too big of font … the animation is perfect though" — the roll is unchanged) — every pull request the hive tracked that reached merged, *all time*, with *N today · N this week* beside it at the bar's usual size. The number is the server's, not the tab's: the hive keeps a **merged ledger** at `/data/merged-ledger.json` (entries for 90 days, an all-time total that only grows, atomic writes, seeded on first boot from the audit log's `pr_merged` history with the PR throughput counter as the floor, then backfilled by the first closed sweep, which on such a boot asks GitHub for every PR closed in the last 30 days — merges by people and other bots the audit log never saw — instead of its usual one day; so the ledger counts every merged PR in a configured repository, and its all-time number is expected to run above the hive-observed PR throughput counter it was seeded from) fed by the audit log's `pr_merged` lines — the hive's own merges on every path and the poller's observations of merges by people or other bots — and by webhook / closed-sweep snapshots of a PR GitHub reports merged, which carry the title and the merger's login within seconds.

`GET /api/stage-tracker` returns it as `merged{today, total_7d, total_30d, all_time, recent[≤20], tz, day_start}`; "today" rolls at midnight in the spoke's zone (`TZ`, else UTC) and the chip hydrates from it on the first render, so a reload, a new tab or a roll shows the same count (hivecommons/hive#15147 — the operator watched 4 become 0). Within a tab the number never goes down; a merge the tab just watched is added on top until the server's list carries it, and that list is also what settles a vanished row as *merged* rather than closed (alongside `recently_merged`), so a human's merge celebrates too.

**Click the chip** for a popover of the recent merges — number (linked), title, *by hive* / *by @login*, lane, time — and the 30-day total; it is also where a merged row's trace lives once the row has left. When a PR on the list merges its row does **not** simply vanish on the next refresh: it stays pinned for about 3.5 s, its ✓ cell glows amber, the event line reads *Hive: merged #14947 · by hive*, and — in the operator's words — "an animation originates from the item's row where the check mark is and flies up to the top of the browser border; if the Merged pill is visible the animation stops there, if not we just see it go off the screen": a 🐝 (or **+1**, ⚙ → Merges → *Glyph*, `hive.tracker.celebrate_glyph`) rises out of the ✓ cell for ≈2 s on an ease-out with a slight arc, lands on the chip when the chip's box is inside the viewport (the count rolls up like a mileage odometer — each digit that rose slides up through the digits it passes over ≈0.9 s — with a soft scale-pop, and the merge zap, when sound is on, plays as it lands) or exits through the top edge otherwise (the count still rolls).

On a change only the wheels whose digit changed roll (the rightmost always spins a full turn, carry wheels roll too); the others keep their cell. *N today · N this week* roll the same way, drawn plain; a number that falls (today → 0 at midnight), the first draw after a reload (the tab's last values are remembered in `sessionStorage`, so a reload rolls up from them), and reduced motion swap instantly and the drum stays — the odometer look is the resting state, not an effect of the roll ("where is my odometer look and feel?", after #15409 had sized only the flying bee and drawn the roll as a 900 ms overlay over plain text). The pill's width never changes mid-roll (the drum's cells set it). The roll waits for the bee: while a glyph is in the air nothing on the drum moves — not the day/week line, not a total the server has already raised, not a merge over the flight cap — and the moment the last glyph lands (its `animationend`, or a timer at the flight's length when the event never fires) the drum rolls once to the final numbers and the zap plays; several merges in one flight are one roll, not a queue (#15568: "the odometer should roll only after the bee animation finishes so it looks correlated"). It works at any scroll position (`position: fixed`, `pointer-events: none`). At the same moment a **banner** appears at the bottom right — *✓ #15067 merged — fix: enable bulk delegation… (adopted, by hive, 17:42)* — and stays until dismissed or 30 s, stacked (newest at the bottom, at most five) when several merge together, with **Show row** (scrolls to the row and glows it again; once the row has left, says so and opens the list), **Open on GitHub** and **All merges**.

Several merges in one refresh stagger 200 ms apart with at most three glyphs in flight (the rest bump the count without flying); a merged PR that is filtered out or on another page still counts and still gets its banner. Under reduced motion nothing flies: the glyph fades in place, the count bumps, the row glows, the banners appear without sliding. A PR drawn inline on its issue's row (the usual case — one covering PR, no sub-row) celebrates on that issue row: every row stamps the PR keys it draws (`data-ham-prs`) and the SPA remembers each PR's host row from the last payload, so the flight has somewhere to start from even when the merge also closes the issue.

The ⚙ dialog's **Merges** section has **Celebrate merges** (default on, `hive.tracker.celebrate`); off, the row leaves as before and the zap plays at once. **Test celebration** beside it closes the dialog, picks the first row currently in the viewport (scrolling one into view if none is) and plays one flight from it with a *(test)* banner — pop and zap, the count itself unchanged — so the effect can be checked without waiting for a merge; under reduced motion it says so and only pops.

The row effect is reusable in the SPA as `celebrateRow(rowEl, {kind: 'merged' | 'closed-by-merge'})`. Class names: `ham-sticky` (the pinned bar), `ham-merged-count` / `ham-merged-pop` (chip, popover), `ham-merged-ico` (the bee), `ham-merged-n` / `ham-merged-sub` (the counters; each holds one `ham-odo` drum of `ham-odo-col` wheels — `ham-odo-roll` while one rolls — and `ham-odo-static` plates, with the value on `data-odo-value` and `aria-label`), `cue-flight` / `cue-flight-fade` (the glyph), `merge-banners` / `merge-banner`.

#### Multi-select

The checkbox in a row's left gutter selects the row — and the checkbox in a nested PR sub-row's gutter selects that PR on its own, so one or more PRs under an issue can be picked without the issue or each other; **Shift+click** selects the range from the last clicked row (sub-rows are in the range order right after their issue), **Cmd/Ctrl+click** toggles, **Cmd/Ctrl+Shift+click** adds a range to what is selected; with a row focused, **Space** toggles it (Shift+Space extends).

Cmd/Ctrl- or Shift-clicking the row itself, away from its links, selects it too. A selection bar appears above the list with the count, **Clear**, and the same actions for the whole selection — **Accept triage (N selected)** when every selected row is an issue waiting on a maintainer's triage (015 *Triage needed*, the `needs-triage` chip, or a 000-new reporter-trust hold): it leads the bar as the same `ham-lead-act` button a single row shows and heads the bulk ⋯ menu; a mixed selection offers **Accept triage (k of N)** after *Decide…* with the hint *skips the N−k not waiting on triage*, and a selection with no such row does not offer it at all. One click posts the per-item `…/items/{number}/accept-triage` endpoint once per eligible row, in order (PRs and already-triaged issues are skipped, never posted), each row showing the same busy chevron and *Accepting triage…* → *Actionable* transition as a single Accept triage, with one summary toast *Accepted N issues* (*Accepted k of N issues. Failed: …* when any refused) — no new write path, the same gated label application as the row action; *Decide…* (opens the group as a menu, *Let hive decide* first — each selected row gets its own delegated-decision job, see below), **Let hive decide (N selected)** (one click for the whole selection: one delegated-decision job per selected item, each nested PR sub-row by its own number, with one summary toast — "3 sent · 2 decided · 1 no-op", plus "· 1 failed" when any failed — and the per-action confirm / *Don't ask again* of *Let hive decide*), *Hold*, *Release hold*, *Re-evaluate now*, *Mark needs-human…*, *Copy links* — each enabled only when it is valid for every selected row and otherwise greyed with the first reason ("Not applicable to acme/console#12", "Sign in with write access…").

Right-clicking a selected row acts on the whole selection. Bulk actions run one request per row, in list order, under a progress toast ("Hold 2/5 — acme/console#12…"); the result toast lists each failure by row and the successes stand, after which the list and the status snapshot refresh (a hive-wide rescan is sent once per batch). Selection is keyed on the row's item (repository, kind, number), not its node, so the live re-render keeps it; a row that leaves the list — merged, closed, filtered or paged out — leaves the selection.

#### Needs-human decision popover

Hovering — or, by keyboard, focusing — the *Needs human decision* chevron word on a row (an issue at 045, a PR at 085, an issue whose nested PR is parked there) or a lead *needs-human* / *triage needed* pill opens an in-app popover (no native tooltip on that element) that says, in this order: **what the row is waiting on** — the reviewer's own question when the hive has one (the text the hive's "Needs a decision" comment on the PR prints after *reviewer question:*, read from the same verdict record), else a plain-words sentence for the park reason, plus the blocker in plain words ("A reviewer requested changes on b0f5f80 — needs a new commit.

Approving cannot override that."); **what you can do** — one button per applicable action, *Let hive decide* first, then Approve / Give direction / Close as superseded / Close as won't fix / Hold or Release hold (an issue at 015: Accept triage, Hold, Mark needs-human), each disabled with its reason when the row's state or your role rules it out; **who asked and when** ("Asked by a reviewer · 2 hours ago"); then the chevron's stage line.

The buttons run the row context menu's code path, so the same result toast and in-flight chevron apply. The popover stays open while the pointer is over it or its trigger, pins on click or focus, and closes on Esc (focus returns to the trigger), click-away, scroll or resize. The text comes from the tracker payload's `decision_needed` (`question`, `asked_by`, `since`, `blocker`, `blocker_reason`, `options[] {action, label, enabled, reason_if_disabled}`), whose `action` ids are the context menu's own (`decide:delegate`, `decide:approve`, …).

#### Hold pill with provenance

When the hive can say who set a row's `hold` label and why, the pill does not read a bare *hold*: it reads **Held by hive · reporter trust (issue #15017 by @Karibhou, contributor)** when the hive's own reporter-trust gate ([#9665](https://github.com/hivecommons/hive/issues/9665)) set it, **Held by hive** when the hive set it for another reason, or **Held by @login** when a person added the label; a hold the hive cannot attribute keeps the plain *hold* word.

The hover text repeats the pill, says since when, and for a hive-set reporter-trust hold explains that the hive set it itself and that *Release hold* lifts it. That same pill opens the needs-human decision popover: its question reads *The hive held this PR itself for reporter trust (issue #N by @X, contributor). Release the hold, or keep it?*, **Release hold** leads the buttons (enabled), *Let hive decide* waits until the hold is released, and `blocker_reason` is `hold:hive`.

A person's hold names them in the blocker instead (*On hold (set by @login) — the hive does nothing until the hold is released.*). The data rides the tracker payload as `hold {by, source, reason, since, issue, reporter, association, pill}` on the item and on each nested `linked_pr`; `by` is `hive` or the login of the person whose label event set it, `source` is `reporter-trust` or `label`. It comes from the stage-label pass, which reads the hive's `<!-- hive:reporter-trust-metadata -->` notice and the newest `hold` label event ([#15020](https://github.com/hivecommons/hive/pull/15020): a hive-authored PR sat 49 minutes as *hold +1 · Needs human decision* with no way to tell that the hive had held it two seconds after opening it).

A hive-authored PR never keeps a hive-set reporter-trust hold: the same pass releases it, so the pill is normally seen only on contributor PRs.

#### **Merge queue pills — legend**

Stages 080, 095 and 100 share the **Merge queue** column (the column was *Merge gate* until [#15395](https://github.com/hivecommons/hive/issues/15395); ids, classes and JSON keys — `merge_gate`, `trackerColumnMergeGate` — keep the old name) (060 *reviewed* is pre-admission — it can still regress to 070 — so it lights **CI / Review** with the word *Review passed*), but they do not mean the same thing. Use the stage word and live detail, not the presence of a working glyph, to judge merge progress ([#15073](https://github.com/hivecommons/hive/issues/15073)).

Every PR at the gate now carries `merge_gate: {state, reason, sweep_running, last_sweep_at, next_sweep_at, soak_until, soak_remaining_seconds}` in the payload (mirrored on the issue's nested `linked_prs`), and the lit chevron reads one word per state, in pipeline order, with the live detail after a `·`:

| stage | pill word | detail after `·` | glyph before the word (tooltip + `aria-label`) |
| --- | --- | --- | --- |
| 060 reviewed — drawn in the **CI / Review** column, not the merge gate | **Review passed** | the lane's own recorded reason, in the operator's words — *CI in progress* (`N check(s) still running`, `check-pending`), *checks unreadable* (`checks could not be read`), *soak 30m remaining* (pre-admission soak); *waiting for merge admission* when the payload carries no reason | 🕓 clock — *Review passed, not yet admitted to auto merge — CI still running (build-and-test). hive re-checks the gate on its next pass in 3m. A GitHub event on the PR (check run, review, push) triggers a pass sooner. Last checked 2m ago.* |
| 080 awaiting merge admission | **Ready for auto merge** | *next pass in 42s* (counts down in place; *pass due now* once past) | 🕓 clock — *Approved and green; waiting for hive's next merge pass — next pass in 42s. …* |
| 095 pre-merge soak | **Soaking** | *7m left* (counts down in place; *complete* once past — the row never says "0s left") | 🕓 clock — *Head abc1234 must sit 30 minutes without new commits before the hive merges it · 7m 10s left · soak ends 8:42 PM*; past the window *… soaked since 8:42 PM. Soak complete — merges on the next pass (next pass in 3m).* Caption *Admitted by the automerge lane; soaking — 7m left* / *… soak complete — merges on the next pass* ([#15575](https://github.com/hivecommons/hive/issues/15575)) |
| 095 soaked and the lane's pass is on it | **Mergeable** | *merging now* | ⟳ spinning — *… Mergeable — merging now.* |
| 060/080 while a pass is evaluating this PR | the stage's word | *checking now* | ⟳ (static) — *hive is checking this PR now.* Never the word *sweep* — the lane's internal name, not a state ([#15575](https://github.com/hivecommons/hive/issues/15575)) |
| 080 in a merge train (`merge_gate.state: queued-behind` / `front`) | **Merge queued · 2nd of 6 · ~4m** / **Merge queued · 1st of 6** (ordinal place of the queue length and countdown; **· train main.go** appended when more than one train is on the page; **Merge queue · stalled — waiting on #15253 (waiting for CI) · 3h** once stalled) | *~4m* (counts down to its estimated turn from `merge_train.eta_at`; *any moment now* once past; *running late* past twice the first estimate; nothing while the head is on a person or CI) — the words never change with the sweep | 🕓 clock, or ⟳ spinning while a sweep evaluates it — *Behind #15253 (soaking, 12m left) and 1 more. Hive merges one at a time, in order. Estimated turn: ~25m (2 ahead · head soaking 12m · next pass in 40s). Checking now… Last checked 12s ago.* |
| 100 merge queue | **Merging** | — | ⟳ (static) — *Admitted and soaked; hive is merging it now (GitHub merge queue / merge window).* |
| any stage while hive's merge call is in flight (`merge_gate.merging_since`, `merge_path`) | **Merging** | *merge call in flight*, then *merge landed* while the SPA holds the row | ⟳ (static) — *hive is merging it now (automerge sweep) — the merge call has been in flight since 3s ago.* |
| any stage after GitHub refused hive's merge call (`merge_gate.state: merge-failed`, `merge_failed`, `merge_failed_kind`, `merge_failed_text`, `merge_failed_at`) | **Merge failed** | the refusal translated to a short phrase — *branch protection*, *needs rebase*, *not allowed*, *review required*, or *retrying* while the sweep is on it; never GitHub's raw text | 🕓 clock — *hive tried to merge this (maintainer merge from the dashboard) 9s ago and GitHub refused — branch protection blocks it. The lane re-evaluates it on its next pass. Details: 405 Pull Request is not mergeable* |
| any stage after the merge call drew *N of M required status checks are expected* (`merge_failed_kind: checks-pending`, `merge_failed_checks`) | **Waiting for CI** | *4 checks* | ⟳ while the sweep is on it, else 🕓 — *hive tried to merge this (automerge sweep) 1m ago before its required checks had reported — 4 required checks haven't finished. It merges once they pass.* |

A merge outcome (**Merging**, **Merge failed**, **Waiting for CI**) always lights the **Merge queue** column, whatever numeric stage the lane fell back to (hivecommons/docs#512 drew "Merge failed" in CI / Review because the PR sat at 060).

A refusal is scoped to the head it failed on. Once the PR head moves (merge-from-base, a new push) or the record falls back below 080 to wait on CI (`required-check-pending:*`) after the refusal, the chevron reads the live stage word (**CI running · build-and-test**, **Review pending**) and the refusal survives only in the hover — *merge attempt on previous head 92d4cef failed: branch protection* (`merge_gate.prior_head_merge_failed`, `prior_head_merge_failed_kind`, `prior_head_merge_failed_text`, `prior_head_merge_failed_at`, `prior_head_merge_failed_head`). The chevron and the caption never disagree about whether a merge is failing now ([#15611](https://github.com/hivecommons/hive/issues/15611); hive#15574 read "Merge failed · branch protection" over a caption waiting on CI for the new head).
| 110 | **Merged** (the ✓ cap) | — | — |

#### Merging is held for the toast's lifetime

Every merge path (sweep, dashboard *Approve & merge*, relay, forge) registers its attempt in `pkg/github` before the GitHub call and its outcome after. The tracker reads this registry on each build: it shows 100 **Merging** with `merge_gate.merging_since` during the call, or **Merge failed** and the refusal reason. This makes even a merge completed between 30-second polls visible ([#15245](https://github.com/hivecommons/hive/issues/15245)).

The SPA keeps a hold per PR from the first moment it saw *Merging* — or, for a merge it never saw coming, from the poll that reported it merged — and for `STAGE_TRACKER_MERGING_HOLD_MS` (= `STAGE_TRACKER_MERGE_BANNER_MS`, 30 s) the row stays on the board at 100 *Merging · merge landed* before it advances to ✓ and the banner, glyph flight and chip bump fire. A refused merge drops the hold at once.

An **issue row** whose covering PR is at the gate reads the gate from that nested PR (`linked_prs[].merge_gate`), so the issue row and its PR sub-row always show the same word and detail; the issue's own lane reason — *open PR #15090 covers it (PR at 060-reviewed)* — is never a gate reason and never appears in the pill (it did briefly after [#15084](https://github.com/hivecommons/hive/pull/15084), as a two-line clipped pill — [#15101](https://github.com/hivecommons/hive/issues/15101)); it leads the glyph's tooltip instead, as *Open PR #15090 covers it · its PR is review passed · waiting for merge admission — Review passed, …*.

No glyph on a chevron or pill rotates ([#15187](https://github.com/hivecommons/hive/issues/15187)): the ⟳ sweep glyph and the *working* glyph are static icons whose tooltips say what the hive is doing. The glyph is drawn for **every** row on the gate, so two rows in the same state always look the same; the ⬡ / 🤖 / 👤 icon *after* the word is whose move it is (`STAGE_TRACKER_ACTOR`), unchanged.

The *working* spin (an agent on the item right now) stays a separate cue and now also carries its sentence as tooltip and `aria-label` (*Working now: reviewer reviewed it · 2m ago*). `next_sweep_at` comes from the automerge lanes' sweep clock (`automerge.SweepSnapshot()` — the next periodic floor pass, 5 min event-driven or the legacy 10 s tick); webhook-named passes run sooner, which the tooltip says. `last_sweep_at` is the stage record's `last_seen` (when a lane last evaluated the PR).

#### Every ask has its action (the lead entries)

The menu exposes the action the row is asking a person to take ([#15078](https://github.com/hivecommons/hive/issues/15078)). Whenever a row — an issue row whose one nested PR is parked, or a PR sub-row — is in a state that asks a person for something, the ⋯ menu **leads** with the matching action(s), taken straight from the payload's `decision_needed.options`, each with its consequence as the hint; the generic entries (*Decide…*, *Hold*, *Re-evaluate now*, *Mark needs-human…*, *Open on GitHub*, *Copy link*) follow under a separator, and the needs-human popover shows the same options with the consequence under each button (`ham-decision-consequences`).

A Go table test (`TestStageTrackerNeedsHumanStatesCarryAnAction`) keeps every state `stageTrackerNeedsHuman` flags on a non-empty options list with at least one enabled action (or a disabled one whose `reason_if_disabled` is plain words), and renderer tests (`TestStageTrackerRowActionsLeadWithDecisionOptions`, `TestStageTrackerIssueRowWithNested090PRCarriesAllThree` — the issue row over its one nested 090 PR carries Approve & merge, Approve — let hive merge and Let hive decide) keep the menu leading with them. *Let hive decide* is on every needs-human state: where the hive cannot act it says so in plain words through the no-effect toast instead of silently recording a decision.

State by state:

| State the pill shows | Lead actions in ⋯ and in the popover (consequence) |
|---|---|
| PR at 090 *Ready for human merge* | **Approve & merge as maintainer**, **Approve — let hive merge**, then **Let hive decide**, followed by other applicable actions. Merger/owner role and confirmation required for merge actions. Read [the 090 action reference](#choose-an-action-at-090) before using a trust override or re-authoring. When the row also wears **needs rebase**, **Rebase, then merge as maintainer** sits right after Approve (below). |
| PR at 085 *Needs human decision* | Let hive decide · Approve · Give direction · Close as superseded · Close as won't fix · Hold (unchanged). |
| PR at 085 or 090 that also wears **needs rebase** (behind or conflicting with its base) | **Rebase, then merge as maintainer** replaces *Approve & merge as maintainer* — a plain merge would be a GitHub 405 — with the consequence *hive rebases #N onto base, waits for CI, then merges it with your approval on record; you'll get a toast when it lands or if the rebase conflicts*. The click records your approval first (/lgtm + /approve in your name), arms a follow-through, and kicks the rebase (GitHub update-branch for the hive's own PR when it is merely behind; otherwise the fixer lane, which now owns a parked PR with an armed follow-through); the endpoint answers `202 rebase-queued` and the toast reads *rebase queued, merge to follow*. The row then reads **Rebasing → merge queued (approved by @you)** with the working cue, *Rebased → waiting for CI, then merge* once the head moves, and the normal merge-gate pills after that. On green the hive merges the rebased head with your approval attributed — only a head the hive itself produced counts as the rebase; a head someone else pushes after your click settles the action as *failed* (*the head moved after your approval*) with **Approve the new head & merge** · **Close as superseded** as the ways out, nothing merged. On a conflict (or CI red, or no rebase within 2 h) the action settles as *failed* with the conflicting files and two ways out — **Adopt into fresh branch** · **Close as superseded** — on the row's badge and in a note on the PR. The ledger's `last_operator_action.followed[]` carries each step (`approval-recorded` → `rebase-kicked` → `rebased` → `ci-green` → `merged`, or `failed`). ([#14822](https://github.com/hivecommons/hive/issues/14822)) |
| PR (hive-authored or a trusted author's) that is **conflicting** with its base | No click needed: the fixer lane rebases it on the hive's behalf, and the gate pill reads **Rebasing · attempt n/2** (spinning) — each fixer push that GitHub still reports dirty is the next attempt. After two the hive stops: the pill reads **Rebase failed · 2/2 — needs a person** and the row's menu still offers *Adopt into fresh branch* / *Close as superseded*. A merely `behind` PR gets GitHub's update-branch from its merge lane even while it wears prow's stale needs-rebase label. **Approve** on such a PR records the approval and opens the rebase episode in your name — the toast reads *Approved — #N conflicts with base; hive will rebase it next pass (attempt 1/2)* with a **Rebase, then merge as maintainer** button, never "the merge gate takes it from here"; the *Approving…* cue clears the moment the decision records. ([#15103](https://github.com/hivecommons/hive/issues/15103)) |
| Issue at 045 *Needs decision* (direction) | Let hive decide · Give direction · Approve direction · Send back to the hive · Hold (unchanged). |
| Human-authored PR at 071 *Needs author* (or a legacy 072 pending reconciliation) | Let hive decide · Close as superseded · Close as won't fix · Hold — Hive never rewrites a person's PR, so the choice is wait or close. |
| Contributor-agent PR at 072 *Returned to contributor* (or legacy 071) | Adopt now / Reroute when the takeover rules allow them; see [Adopt now / Reroute](#adopt-now--reroute-072-071). Hive's own PRs use 070 Fixing, not this hand-back path. |
| `needs-human` label set by a person or the hive (any stage) | **Resolve — lift needs-human** (removes the label; on a PR also leaves the hive's /lgtm + /approve, on an issue a `/hive approve`) · Let hive decide · Hold. |
| `hive:unauthorized` | **Re-evaluate now** (re-reads GitHub; the hive resumes and clears the label once you did the step on GitHub) · Let hive decide · Hold · Mark needs-human. |
| 015 *Triage needed* | Accept triage · Let hive decide · Hold · Mark needs-human. |
| Hold set by an operator | **Release hold** leads; the state's own options stay listed but disabled with *Release the hold first*. |
| Any other stage the hive leaves to a person | Re-evaluate now · Let hive decide · Hold · Mark needs-human, with the question naming the stage. |

#### Choose an action at 090

These are different operations, not interchangeable approvals:

| Action | Effect and result |
| --- | --- |
| **Approve & merge as maintainer** | Merges #N into its base now through Hive's identity, attributed to you in a PR comment. Audit: `repo_item_decision_merge`, `source: dashboard`. Toast: *merged*; badge: `merge-now`. |
| **Approve — let hive merge** | Records your owner decision as an APPROVE review and the automerge label. The sweep merges on its next eligible pass once CI is green and the head has soaked. Toast: *queued for merge*; badge: `queue-merge`. |
| **Let hive decide** | At 090, does **not** run the delegation policy. It returns a no-effect explanation, such as *I can't merge this: author @Karibhou is untrusted on hotshot — …*, with the two approval actions and the 👥 Contributors remedy. |
| **Re-author as hive PR** | A separate, explicit choice when the commit check finds missing `Signed-off-by`: re-home commits on a signed-off Hive branch and open a superseding PR, not merge the original as-is. Disabled with a reason on a person's PR. |

Both merge actions require the **merger or owner** role and confirm first; *Don't ask again* is supported. **Give direction** is available only on Hive-authored PRs. Close as superseded, Close as won't fix and Hold remain available when applicable.

Either approval overrides the trust gate **for that head** ([#15136](https://github.com/hivecommons/hive/issues/15136)). The stage record gets `admitted_by: operator-approval:<you>`. The contributor lane takes the head as-is after checks and soak; adoption/supersession is refused while admission stands. The row says *not adopted: a maintainer approved this head; the hive merges it as-is*. The 090 tooltip/popover names the approver and a second approval is disabled.

Missing sign-offs are disclosed **before** acting, in the blocker line and both approval confirmations: *2 of 3 commits have no Signed-off-by, so a required DCO check refuses the merge*. Approval does not repair that check. The separate re-author action and caveat are covered by `TestStageTrackerReauthorAndApprovalCaveats`.

#### "Untrusted author", in plain words

The 090 pill's tooltip says *why* the author is untrusted and *what would make them trusted*, per reason code. For `contributor-lane:author-association` (hotshot#317), it explains that @login has neither a GitHub owner/member/collaborator association nor a trusted/merger tier in this Hive's contributor registry. Hive will not merge their PR unattended.

To trust that author for unattended work, give them a trusted or merger tier under **👥 Contributors** (`auto_merge.contributor_lane.require_tiers`), or add them as a repository collaborator on GitHub. Until then, a maintainer chooses **Approve & merge as maintainer** or **Approve — let hive merge** from ⋯.

Other reason codes name their own remedy:

| Reason | Where to check |
| --- | --- |
| `untrusted-author-role` | Merger role under ⚙ Security / `dashboard.authorized_users`. |
| `author-permission-missing` | `auto_merge.trusted_authors.require_github_permission`. |
| `fork-non-member` | The fork author's membership restriction described by the tooltip. |
| Claim-tier / no-claim / unverified-claim family | The tier under 👥 Contributors. |

Hive App PRs are never “untrusted” here: this lane's refusal names a human author. The popover uses the same explanation as the tooltip.

#### Cue tokens

The three cues of the legend above (state pill, working spin, decision badge) share one `tracker-cues` CSS block on `.ham-row`: `--cue-state` (what the row waits on — the pill and this popover), `--cue-work` (the hive is working right now — the chevron spin), `--cue-decision` (what people and the hive decided — the history badge). Nothing else blinks; motion stays in the 150–300 ms range.

### Let hive decide

#### What it is

*Let hive decide* is the first entry under *Decide…* on a row parked at *Needs human decision* (an issue at 045, a PR at 085, or an issue whose nested PR is parked there), and under *Decide…* in the selection bar for a bulk run. It asks the hive to make the decision itself, as the owner's delegate, under the owner's standing operating rules — and to record it exactly as a human decision is recorded: a `/hive decision <text>` comment on the item (plus the queue approval on a PR head when the decision is to release the PR), the parking labels lifted, `triage/accepted` added where it applies, and the stage engine's un-park logged with the requesting operator named.

The item leaves 085/045 and proceeds without the owner. The action is audited as `repo_item_decision_delegated` (or `repo_item_decision_delegated_failed`) and the **Decisions log** shows those rows with a *delegated* badge so delegated and human decisions can be told apart and counted.

#### How it runs

1. Read the one-time explanation and confirmation. **Don't ask again for this action** is optional; reset it in ⚙ → **Confirmations**.
2. The row reads **Hive is deciding…** while the job runs.
3. Read the persistent result toast. It says what was decided and what happens next; it stays until the target row changes state.
4. If the result has not happened, use [When the decision doesn't take effect](#when-the-decision-doesnt-take-effect), rather than treating an approval as a completed merge.

Permission is the same as **Hold**: owner, or a signed-in user with repository write access. The recorded decision names the requesting user as the person who delegated it.

**Multiple PRs.** An issue with two or more open nested PRs offers **Let hive decide — all N PRs**. It never silently chooses one ([#15009](https://github.com/hivecommons/hive/issues/15009)). Each PR has its own job and result toast; the issue toast summarizes them. For example: “Decided each of the 2 nested pull requests: #15017 approved (small low risk); #15020 no change (already decided).” The next-step summary can read “2 sent · 1 decided · 1 no-op. Each PR's own toast says what happens next.” The aggregate fails only if every PR failed. With one nested PR or none, the issue's own decision path is unchanged.

**Request/response reference.**

| Detail | Contract |
| --- | --- |
| Start | `POST /api/repos/{owner}/{repo}/items/{number}/decide-delegated`; body `type` (`issue` or `pr`), `reason`, `caption`, `sentinel_reason` carries the row's park explanation. |
| Multiple PRs | Send `"type": "issue"` and `"prs": [15017, 15020]`. Without that hint, the server reads `linked_prs`. Jobs are keyed per PR; `GET …/items/{pr}/decide-delegated` reads the individual job. |
| Already running | A second start answers 409 with the running job, rather than deciding twice. |
| Poll | `GET` the same job URL until `done` or `failed`. |
| Result | `rule`, `action`, `target`, `outcome`, `next`. A multi-PR response adds `items`, each with `number`, `type`, `status`, `decision`, `notes`, `error`, `result`, and a summary `result.action: "decided-each"`. |
| Already applied | `action: noop-already-applied`; nothing written. The toast names the prior decision time and remaining blocker. |

A job gathers the item's title, body, labels, author and state; PR mergeability and changed files; whether it is Hive-authored; the author's repository permission (trusted means write or above); sentinel and park reasons. It runs the policy through the same GitHub helpers as manual decisions.

A successful adoption toast can read *Hive decided on acme/console#14938 → PR #14947: adopted (contributor offline) — Adopted acme/console#14947 in supersede mode as #14977. The hive rebases and merges it unattended.* That describes the intended continuation; the result/effect checks still apply.

#### The policy

The rules live in one place — `src/pkg/delegatedecision/policy.go` (`PolicyText`, the owner's words verbatim, including the contributor-adequacy rail adopted in #13998; the Go rules beside it implement them) — so the dashboard, docs and any future LLM-backed decider share the same text. In short: be decisive and, except for the safety rails, never ask the owner a question back; pick the option that keeps work moving and is reversible.

**PRs:** prefer an adequate contributor PR over a hive duplicate (a mergeable, current competitor prompts a human comparison of issue coverage, checks and review approval; keep the hive PR open and parked until adequacy is confirmed, rather than closing it solely on an issue reference; an unmergeable contributor PR is superseded with credit); approve small, necessary, low-risk changes (CI pins, read-only RBAC adds, warning text, workflow hygiene) once the sentinel finding is checked and explained; a sentinel alert on a trusted author's PR is approved with a one-line "reviewed, intended" rationale; a reporter-trust hold is cleared when the fixing PR is sound; a contributor who has gone quiet is superseded now, not after a deadline (#13975).

**Issues:** dedupe aggressively (`Duplicate of #N`, cross-link overlaps); secrets / PAT / org-permission items are owner-only — `needs-owner` + `hold` with one comment stating the exact owner action, and no more failure issues for that root cause; flaky tests are quarantined with a retry and a deflake task rather than blocking; CI hygiene gate proposals (actionlint, duplication ratchets, link-checker allowlists) are approved as ratchets (baseline; fail only on increases; warning-only briefly, then enforce); design proposals with concrete acceptance criteria are accepted as proposed with explicit defaults; a "review backlog" item is never auto-closed when its PR closes unmerged (drop the relation, comment, re-triage).

**Issues, judged the way the owner would** (the five parked issues he decided by hand; rules evaluated before `default-approve`, which stays the last fallback):

| shape | rule | what the hive does |
| --- | --- | --- |
| a fleet self-report (`<!-- hive:selfreport … -->` marker or `[selfreport]` title) whose class is an operator's to fix — `agent_login_required`, `inference_auth_failed`, `data_disk_pressure`, the GitHub App token / installation / coverage classes, proxy dial / TLS, `crux_runtime_unavailable`, model discovery, label provisioning, shared-CI rerun permission | `selfreport-operator-action` (verdict `operator-action`) | Not code work. One comment telling the owner(s) named in the report's table exactly what to do on *that* spoke ("@owner: re-authenticate the agent provider … on spoke-x"), `needs-owner` added, **no** `approved-direction`, no implementer, the `stage/045-needs-decision` park kept so the actionable set never sees it. It clears when the report stops recurring (or `/fixed`). Clicking *Let hive decide* again never re-posts the direction: the answer is *already decided at that time; waiting on the spoke owner: @owner …* (the #15021 loop). Code-defect classes (panics, repeated unclassified errors, stalls) stay on the implementer path. |
| a hive-filed proposal (`[quality]` / `[scanner]` / `[architect]` / `[strategist]` / `[outreach]` prefix, "Gate proposal", "RFC", or a `kind/design` / `kind/proposal` label) **with evidence** — an `incident #N`, breakage words (broke, regression, outage, uncompilable, blast radius), or a recurrence whose prior instances were each fixed one at a time ("fixed in #415", "Fixing PRs: #415, #417") | `proposal-approve-evidence` (verdict `approve`) | Approved as proposed; the rationale names the evidence ("it cites incident #15322", "the same class was already fixed one at a time in #415, #417"). |
| a hive-filed proposal **without** such evidence | `proposal-not-now` (verdict `not-now`) | Declined for now: a comment saying "no incident or recurrence justifies the cost yet; re-propose with evidence", then closed as not planned (the established declined-proposal path). Never applied to a person's proposal (those keep `default-approve`), never to the hive's own policy (`hive-policy` stays recommend-only). |
| a change to the hive's own policy (for example an RFC to hold unacknowledged hive-filed proposals) | `hive-policy` (recommend-only) | Unchanged in shape, but the recommendation is a real sentence in the owner's terms — "approve `<title>` as proposed — proposals nobody has acknowledged should not be actionable; make it default-on for the lanes it names rather than opt-in; owner to confirm" — never "no special rule applies". |

Each decision names the rule that fired (`dedupe`, `owner-only`, `flaky-test`, `ci-ratchet`, `design-proposal`, `review-backlog`, `selfreport-operator-action`, `proposal-approve-evidence`, `proposal-not-now`, `default-approve`, `prefer-contributor`, `contributor-offline`, `sentinel-trusted`, `small-low-risk`, `reporter-trust`, `fix-then-merge`, `default-release`, `hive-policy`, `sentinel-untrusted`, `rail-hold`, `one-pr-per-issue`) in the comment and in the job's `decision.rule`.

**Safety rails** (`ApplyRails`, applied after the policy and before anything is written): the delegate never merges a PR — it only records the approval the auto-merge engine already acts on per repository settings (a merger/owner requester's approval is written as the queue line the sweep parses; anyone else's as `/lgtm` + `/approve` comments attributed to them); it does not touch an item wearing `hold` (refused, nothing written — release the hold first if you want it decided); it never grants org membership, secrets or permissions (those items get `needs-owner` + `hold` and the one owner-action comment, and keep their park); and a question that is a policy or scope change to the hive itself (settings, governance docs, security posture — the `governance` label or those words in the park reason) is left parked: the delegate posts "hive recommends: …; owner to confirm" and keeps 085.

A sentinel-flagged PR whose author lacks write access is likewise a recommendation only. Because the sentinel sweep re-applies its label when a hive account removes it, a trusted-author sentinel approval posts the "reviewed, intended" comment and leaves the final queueing to the person who clears the label — the job's `notes` say so.

#### Audit marker

Every comment the delegate writes ends with `<!-- hive:delegated-decision by=<hive-id> policy=owner-delegate -->` (and a visible "Decided by … as the owner's delegate under the owner-delegate policy (rule `…`)" line), so a delegated decision can always be told from a human one on GitHub, in the Decisions log, and in any later count.

#### Plain words, everywhere

Every rule, verdict and reason the delegate can show is rendered from one glossary (`src/pkg/delegatedecision/decision_glossary.go`, mirrored into the page as `STAGE_TRACKER_DECISION_GLOSSARY`; see [Labels → Decision glossary](labels-and-control-signals.md#decision-glossary)): the toast says "adopted (the author's clanker hasn't been connected since the nudge)", never "(contributor-offline)"; the `/hive decision` comment carries a four-line block — **Hive decided** / **Why** / **What happens next** (with "expected within 30 min") / **If it doesn't happen** — above the attribution and the marker; the needs-decision popover's blocker line uses the same words.

#### When the decision doesn't take effect

A recorded decision is expected to cause something — the park lifted, a merge, a close, a fresh hive-authored PR, a new commit — within `decisions.delegate.effect_timeout` (default **30m**). Clicking *Let hive decide* again inside that window answers *No change — hive already recorded this decision at <time>: <plain action> (<plain rule>)* and *Expected by <deadline>: <effect>*. Past the deadline the answer is `result.action: "no-effect"`: *Hive decided <plain action> at <time>, but it hasn't taken effect: <plain why>* (the time is rendered as local relative time — *at 11:40 today*, *yesterday 17:51* — never raw RFC 3339) — the why read from the lane reason and the PR's state (a reviewer asked for changes; the hive's own decision comment was read as a change request, the [#14822](https://github.com/hivecommons/hive/pull/14822) case; security-sensitive files need a maintainer; conflicts; red CI; sign-off missing; a hold; the author is still online so adoption cannot start; adoption is switched off) — and the toast carries the concrete next actions as buttons (`result.options`, the popover's option shape): **Approve & merge as maintainer** (the in-tracker action, when the operator has merge rights and the tracker exposes it; otherwise a link to GitHub), **Re-run decision** (posts the same request with `"force": true`, skipping the dedup), **Close as superseded**, **Send back for a new commit**.

At the same time the hive posts one follow-up comment on the item — *Hive decided … but it hasn't taken effect / What should have happened / Why it hasn't / What you can do* — stamped `<!-- hive:decision-stalled … -->` so it is posted once and never read as a decision. The dashboard's own `/hive decision` comments (the delegated marker) are never parsed as a reviewer's change request, whichever login they were posted under.

**One post-decision state machine** (hive's own spoke, build dd412e4, 2026-10-28: three *Let hive decide* rows showed a green ✓, a *Hive is deciding…* spinner and the unchanged striped *Ready for human merge · untrusted author* pill at once — "is that celebration? if so hive is no longer deciding?"). A row the operator has sent to the delegate is in exactly one of these states, and the ✓ badge, the pill, the chevron and the row's progress text are projected from the same record (`decided` on the tracker item: `action`, `verdict`, `rule`, `actor`, `at`, `effect`, `stage`, `acting`, `next_at`, `effect_timeout_s`, `superseded_by`, and the pre-rendered `word` / `text` / `title`):

| state | what the row shows |
| --- | --- |
| **deciding** (request posted, no record yet) | the pill reads *Hive deciding…* with the working spin inside it; no badge. |
| **decided, effect pending** | the spinner goes; the ✓ **decision badge** appears at the right (tooltip *You chose Let hive decide · hive decided: adopt (supersede) · 17:51 UTC · hive chose adopt-supersede; next: adoption pass in 4m*). It is **not** a merge celebration — the celebration is the cue-merged glow, the token fly and the Merged counter, none of which run here. The pill names the recorded decision's effect, never the stale park: *Adopted · opening hive copy…*, *Approving…*, *Sending back…*, *Fixing · hive is addressing @hanthor's review*; the chevron moves out of 090/085 to where hive now is (080 *Awaiting merge gate* for adopt/approve, 070 *Fixing* for a direction). *Approving…* is only ever drawn for an `approve` verdict. |
| **effect landed** | the pill follows the live state (merge gate, *Adopted → #15123*, merged). |
| **effect timed out** (`decisions.delegate.effect_timeout`) | the no-effect state above, with its options. |

The toast and the decision trail lead with the outcome in one line — *Adopted — hive will rebase & merge it next pass (~4m)*; *Adopted: hive opened #15123 with the same commits and will merge it; #15029 will be closed as superseded* — details second, nothing repeated.

**Preview before acting.** *Let hive decide* no longer acts on the click. It first calls `POST …/decide-delegated/plan` , a dry run with no side effects that answers `{would, title, summary, why, consequences[], ladder, mergeable_as_is, alternatives[]}`, and shows one decision — built to be read in three seconds (operator feedback on hive's own spoke: the first version was "a wall of text" with a doubled *Hive would: Hive would:*, the PR named twice, three type sizes and a merge button on a head it had just called not mergeable):

- **Title** *Let hive decide*, then one subject line — `hivecommons/hive#15332 · ci: run crux's Go file-size ratchet…` — with a compact **⚠ Sentinel** chip when the sentinel flagged the head (the full finding is in Details).
- **Eyebrow** *Hive would* → **one verb-first sentence**: *Send #15332 back to its author with one instruction: make CI green, nothing else.* → one muted **Why** line (≤ 12 words: *CI is red.*) → **Then**, at most two short items (*Hive posts that instruction on #15332. · The author pushes a fix; the row updates when it lands.*).
- **Details** (collapsed): the sentinel finding, *Why not the others* — the ladder rungs ruled out above the one that decided (`ladder.ruled_out`) — and the one-line rails (*never merges, never touches held items, never grants secrets; the sentinel label stays with a person*).
- **Buttons**, right-aligned: **Cancel** · **Let hive do that**. **Approve & merge as-is** appears only when the plan says `mergeable_as_is` — the ladder's *mergeable-as-is* rung, or (no ladder) a green, conflict-free, signed-off head — and is greyed with the reason when the operator lacks merge rights; the plan drops the merge-now alternative otherwise, so the dialog cannot contradict itself.

The same renderer serves issue decisions and every PR verdict (adopt, approve, close as duplicate/superseded, send back, recommend only). The visible text never uses hive's vocabulary (*park*, *audit marker*, *stage*, *ladder*, */hive decision*) and is ≤ 60 words for the #15332 case, which the node harness enforces. Nothing is written until the operator picks, and the badge tooltip and the trail record which button was pressed (the request's `chosen`).

**Adoption pairs.** When the delegate adopts in supersede mode and hive opens its copy, the tracker shows the pair explicitly rather than two unrelated PR sub-rows: the original is dimmed — *Adopted → #15123 · superseded (hive re-authored with sign-off)* — with no action chevron and no needs-human styling; the new PR carries the single active chevron and a chip *↳ adopted from #15029 · you approved 17:51*; the issue row's pill follows the new PR. Once the original is closed it drops out of the open-PR projection, so it renders as one dimmed line from the adoption record — *#15029 <cached title> · closed · superseded by #15123* — with no chevron track; never a blank numbered row.

**Lineage chip (where an adopted PR came from).** Whenever a PR row is the hive's copy of a contributor PR it adopted — whether the operator decided it or the contributor lane adopted on its own after CI went red and the contributor's agent stayed silent — the PR number carries a compact chip, *adopted from @thatguywob's #15226 · 6h*, linked to the original; the original author's kind (human / clanker / hive) is drawn as the usual author icon, never as a word. Its hover says why in plain words: *Hive took this PR over after CI went red and @thatguywob's agent didn't respond. Original commits kept.* (other reasons: at a maintainer's request, DCO / sign-off / rebase / push re-home). Issue #15198 once showed its covering PR #15308 as *Fixing* with nothing saying #15308 was adopted, so routing the fix to a hive agent read as the hive taking a clanker's PR for no reason. The chip is drawn on a top-level PR row, on an issue row's single covering PR and on each sub-row; a still-open original (close-first off) wears the reverse, *superseded by #15308 · 6h*. Behind it is the optional `lineage` field of the payload — `{kind: adopted|superseded|rebased-by-hive, from_number, from_url, from_author, from_author_kind, to_number, to_url, at, reason}` — derived from the stage store's adoption record or the `hive/adopt-<n>` head branch, never a GitHub call; the SPA helper `stageTrackerLineageClause(item)` yields the one clause *(adopted from #15226)* any caption or preview adds after the PR number (the *Let hive decide* preview and the row hover's *Covering PR* line already do).

**A decision wins over a stale label.** Recording any decision (approve, direction, adopt, delegate-resolved) removes `needs-human` and the 045/085 *needs-decision* stage labels in the same write (`liftDelegatedParkLabels`; `pr_decision_operator.go`), and the projection never places a row in 085 while a decision exists for the current head — the row sits where hive actually is, and the popover leads with *You chose Let hive decide · hive decided: fix then merge*, not with the stale ask.

**One vocabulary.** Every operator-facing string — event lines, popovers, toasts, pills — uses the chevron's stage vocabulary through one shared mapping (`stage_tracker_vocabulary.go`; `STAGE_TRACKER_STAGE_PHRASE` in the page): *its PR is review passed · waiting for merge admission*, never *PR at 060 reviewed* or *moved to stage/085-needs-decision*. A guard test scans rendered event-line fixtures for raw stage codes and fails on any.

**Their clanker, not their hive.** A contributor's agent is their **clanker**; the glossary, toasts, adoption reasons and presence pills say *the author's clanker hasn't been connected since the nudge*, never "the author's hive" (a test asserts the glossary carries no such string).

#### Setting

`decisions.delegate.enabled` (default **true** — the same default as the other "let the hive do it" switches); `decisions.delegate.repos.<owner/repo>.enabled` (or the bare repository name) overrides per repository. `decisions.delegate.llm.enabled` (default **false**) adds the LLM step described below. `decisions.delegate.effect_timeout` (default **30m**, a Go duration) is how long a recorded decision may go without its expected effect before a repeat click reports it as stuck. When the delegate is off the menu entry stays visible but the endpoint answers 403 naming the setting.

See [operator-reference.md](operator-reference.md).

#### Orphaned adoption

(Keep-open supersession, `close_first: false`; a close-first original whose copy closed is reopened by the lane instead, see above.) A PR the hive adopted in supersede mode whose hive copy was closed unmerged is not "already decided": the delegate detects the dead `superseded_by`, re-runs the adoption (a fresh copy from the same head, a new `hive:adopted` marker) and answers *applied* with "Re-adopting: hive's earlier copy #N was closed by mistake; opening a fresh copy." If copies were closed twice within an hour (`adoption-child-closed-twice`) it keeps the park and recommends a person decide rather than opening a third copy.

#### What this release decides mechanically

The decider is rules-first: the cases above that are mechanically decidable (dedupe, owner-only, flaky, CI ratchet, design proposal, review backlog, contributor-offline, sentinel-trusted, small-low-risk, reporter-trust, hive-policy) are decided from the item itself; anything else falls to the reversible default (approve direction on an issue; release a green hive PR; ask for the fix first when the head is red or conflicting). Competing-PR comparisons and adopt-supersede requests are implemented. The same safety rails still decide whether the proposed action can run.

#### LLM step for the non-mechanical cases

With `decisions.delegate.llm.enabled: true` (default **false**), an item the rules leave at the reversible default (rule `default-approve` or `default-release`) is shown to one *owner-delegate decider* turn: the item, its comment thread, sentinel findings, linked issues/PRs, the repository's auto-merge setting, the author's association and reporter-trust state, its labels, and `PolicyText` verbatim. The model answers with a structured `{verdict, text, rationale, actions[]}`; the reply is rejected unless every verdict, action kind and label is in the delegate's closed set and fits the item (no `approve-review` on an issue, no action on a park-keeping verdict, …), and unless the actions carry the verdict out: an acting verdict must include its recording action (`comment-decision`, or `close-as-duplicate` for the close verdicts, plus `approve-review` / `adopt-supersede` where the verdict names them), `labels-remove` can never name `stage/085-needs-decision` (only the recording action lifts the park, with the comment and the marker), and `close-duplicate` closes the newer item in favour of an older one.

An accepted reply is recorded under rule `llm-delegate` and goes through the same safety rails and executor as a rule decision; a model error or rejected reply keeps the reversible default and the job's `notes` say why. Rules-first stays authoritative: every case a rule decides, and every item wearing `hold`, never reaches the model. The call uses the governor reviewer endpoint (`governor.trajectory`, falling back to `governor.litellm`); `decisions.delegate.llm.model` overrides the model.


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

**What it is good for.** Look here to answer "what is everyone doing?" at a glance. A worker that has said "working — no issue or pull request yet" for a long time, or one that holds many reservations, is worth a look on its card: use that card's terminal/log links. The separate **Agent Logs** section is not available.

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

**Settings that change it.** Your code hosting settings and optional service settings change it. The code hosting service is `project.forge`; a self-managed GitLab or a Gitea/Forgejo instance also shows its URL. See [Forges](integrations/forges.md).
