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

**Dialogs on a phone.** Any dialog with a long body — the **What changed** release notes after an upgrade, alerts, the export viewer, the Level 6 auto-merge notice, settings bodies — fills the whole screen on a phone: title and a ✕ at the top, the text scrolls in the middle, and the buttons stay fixed at the bottom. A soft fade at the bottom edge means there is more to read; it disappears when you reach the end. Esc, the ✕, the buttons and tapping the backdrop close it. On a desktop the same dialogs keep their usual size and only gain the fade and a thin scrollbar. See [Long-content modals](dashboard-design-system.md#long-content-modals-modal-scroll-and-the-mobile-sheet) for the convention.

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
- The compact strip at the top of the Agents view also shows **running** — how many workers are mid-round right now, and the estimated spend the whole fleet has accrued in the current clock hour (`3 · $1.20 this hour`). The hour figure is the rise in the cumulative estimate since the top of the hour; `$–` means the hive has not yet taken a reading this hour.
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

What Hive's use of AI models would cost at public per-token list prices; an estimate, not a bill.

Copilot and Claude subscription seats and self-hosted inference are not billed per token, so real spend can be far lower.

**What it tells you.** It turns token counts into money, so you can see roughly what Hive's model use is worth.

**How the numbers are worked out.**

- The headline reads **Estimated spend at list price — since <date> · tokens × public per-token price · not a bill**. The date is the oldest per-day total the hive holds (or the oldest snapshot when there are none), so it does not creep forward as the 30-day ring evicts old snapshots; the merged-PR and closed-issue counts behind the two tiles are counted from that same instant. Hover the figure, the "est." badge or the **?** mark for the full explanation: Copilot and Claude subscription seats and self-hosted inference are not billed per token, so real spend can be far lower than the estimate.
- **The headline total** is built the same way as the chart, so the two cannot disagree: the per-day totals kept for days the 30-day snapshot ring no longer holds, plus every increase of the cumulative estimate between consecutive snapshots inside the ring (when the ring is the whole history, the first snapshot's value is the opening balance). A step is left out only when it is both abnormally large and merely *restores* the counter to a level it had already reached — a re-seeded token store jumping back to its old total an hour after a reset. A large step to a new high is spend, however big. The headline tooltip lists each time the live token summary was reset; the spend on both sides of a reset is counted once.
- Beneath it, **of which $X is in the live token summary since the last reset on <date>** says how much of the headline comes from the session files currently on disk. That summary starts over when the hive is rebuilt or its token store is cleared; spend recorded before each reset stays in the headline, which is why the two figures differ. (Older builds called this the "current lineage".)
- **Cost / closed issue** and **cost / merged PR** (in that order, after the headline total) each divide the *same* headline total by one count — they are two views of one figure, not two costs to add, and the card says so under the tiles. Each label spells out the division (**total ÷ N PRs merged by hive since <date>**) and when the count was last confirmed (**counts as of <time>**). With no cost or no count, they show a dash.
- The merged and closed counts behind those two figures are kept by Hive itself: every `pull_request closed` (merged) and `issues closed` webhook event for the hive's own author adds one, and once an hour (`forgestate.CountsReconcileInterval`) the counts are re-read from GitHub search and overwritten so drift from missed events is corrected. Search is no longer queried on every 5-minute collection.
- When that hourly check is rate-limited or fails, the last good counts stay in use and the count label reads **counts stale since <time> (GitHub rate-limited)**, so a cached denominator is never presented as live. The cost API carries the moment as `count_stale_since`, the last successful read as `count_updated_at`, and names where the counts came from in `count_source` (`forgestate` or `search`). The reason is surfaced as `count_error` (and in the tile tooltip) even when there is nothing cached yet, and a hive with many repos is reconciled a few repos per pass so the refresh stays inside GitHub's search rate limit — while some repos are still waiting the label reads **partial: N of M repos counted** (`count_partial`) rather than showing "—" or falling back to the merged-by-hive counter, which is only used when no attributed count has ever been produced.
- The **usage by model** and **usage by sandbox** tables are built from the live token summary only (their window tag says so), so their totals match the "of which" line, not the headline.
- When your AI gateway reports its own spending, that figure appears too.
- The spend-over-time chart has a range selector: **hourly** (the current clock hour and the 23 before it, one bar per hour), **daily** (the last 14 calendar days including today, one bar per day — labelled by weekday when 7 or fewer are shown, by month/day beyond that), **weekly** (the current Monday→Sunday week and the 4 before it, one bar per week labelled by its Monday; the rest of this week is empty), **monthly** (the current calendar month and the 4 before it, one bar per month labelled by name) and **custom** (your own start and end; hour bars up to 2 days, day bars beyond). Boundaries follow your browser's clock.
- A **−/+ stepper** right of the range chips grows or shrinks the window, showing the current size with its unit (**24 h**, **14 d**, **5 wk**, **5 mo**): hourly moves in steps of 6 hours between 6 and 72, daily in steps of 7 days between 7 and 60, weekly one week at a time between 2 and 26, monthly one month at a time between 2 and 24. The chips are disabled at the bounds, work from the keyboard, and the chosen size is remembered per range in this browser. Custom has no stepper — its bounds are explicit.
- **Spend in selected range** adds up the bars of that same window, and **run-rate** divides it by the part of the window that has elapsed so far — per hour for hourly bars, per day otherwise. Switching the range or stepping the window changes the chart and both figures together.
- **What counts as spend in the chart.** Bars and per-day totals add up the step-to-step *increases* of the cumulative estimate. A single increase that dwarfs every ordinary step — more than 20× the median step and more than $500 — is treated as a data correction (a restored token store) rather than spend, *unless* the time since the previous snapshot explains it: a step is kept when it is within 20× the hive's average spend rate × the elapsed gap, so a hive that nobody looked at for days, whose collector therefore took no snapshots, does not lose those days when the next snapshot reveals them at once. A step that is discarded takes its token counts with it, so a day cannot show hundreds of millions of tokens at $0.
- **How tokens become dollars in the chart.** Each model's $/token comes from the newest snapshot that still lists that model, so a model that stopped running weeks ago is still priced when its historical tokens are summed; only the headline uses the recorded $ directly.
- **How far back the data goes.** The hive takes a cost snapshot every 5 minutes on its own (a server-side timer, not the browser — the history has no holes on days nobody opened the dashboard), keeps about 30 days of them, and folds each completed day (UTC) into a per-day total that is kept for about two years (`daily_rollup` on `/api/cost`, persisted in `/data/cost-daily-rollup.json`). Bars inside the last 30 days are built from the snapshots; older bars use the per-day totals, so a wide weekly or monthly window still shows real spend. A bar that neither source reaches — earlier than the first recorded data point — is drawn as a dim hatched placeholder rather than a $0 bar, and the summary adds **history starts <date>** so you know where the real data begins. The per-day totals start accruing the day the hive first runs this version, so on an upgraded hive they reach back only as far as the snapshots that were present then.
- When the running agents' configured models have recorded no usage for more than 24 hours, a warning above the headline names those models and when they last recorded anything, and the run-rate is suffixed **(recording incomplete)** — the figures then reflect only the models still being recorded (`recording` on `/api/cost`).

**What it is good for.** Use it to judge whether Hive is worth its cost. Compare cost per merged pull request over time.

**Example.** The estimate is $40 this month for 80 merged pull requests, so about $0.50 each.

**When it appears.** Always.

**Settings that change it.** The models you configure and their prices change it. Gateway credentials add the gateway's own spending. See [Token tracking](token-tracking.md).

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

Fed by `GET /api/stage-tracker`. Every issue and pull request follows the same pipeline, named once in the column header above the list: `Open → Actionable (010) → Claimed (020) → PR in progress (030/040) → CI / Review (050/060) → Merge gate (080/100) → ✓ merged`. Each entry is a title line over a thin chevron strip. Only the current step is coloured: it is filled amber and carries the step name in words; once the PR is merged the final ✓ chevron is the lit one, in green. Motion is reserved for what is running or needs a hand — the lit chevron falls into one of three buckets (`stageTrackerMotion()` in `index.html`): **static** (steady): Open, Open · on hold (any hold label wins over every other signal), Actionable (010), Claimed (020), PR in progress / Covered by PR (030/040) by a human author, Merged, and any unrecognised stage; **active** (gentle pulse): the hive is acting — 030/040 by a hive or clanker author, 050 review pending, 060 reviewed, 070 fixing, 074 adopted by hive, 080 merge gate, 095 pre-merge soak, 100 merge queue; **needs-human** (slower amber→red breathe): a person must move — 015 triage needed, 045/085 needs decision, 071 needs author, 072 returned to contributor, 090 ready for a human merge, or a `stalled` / `blocked` / `needs-human` / `needs-decision` / `needs-triage` modifier on any stage. Under `prefers-reduced-motion` nothing animates; a needs-human step keeps a steady red tint so it still stands out. Every other step is a low-contrast shape with a small grey glyph (circle, bolt, lock, branch, eye, shield, check) — passed steps a subtle neutral fill, steps ahead a faint outline. Side stages are not extra steps: `070 Fixing` (the author's move; the `needs-rebase` chip next to it says what is being fixed — the stage labeller always parks a needs-rebase PR at 070/071/072, never at CI / Review), `071 Needs author` (human-authored), `072 Returned to contributor` (clanker-authored) and `074 Adopted by hive` light *CI / Review* with their own name (an adopted PR stays on 074 through the hive's pushes going pending, green or red again, and moves on only when *Merge gate* takes it); `085 Needs human decision` (PR-only) and `090 Ready for human merge` light *Merge gate* with their own name (the 085 tooltip says what must be decided); an issue parked on a person wears `045 Needs human decision` and lights *Actionable* (the issue side) with that name — a pre-045 issue still wearing 085 is drawn the same way until the reconciler re-stages it; an actionable issue no maintainer has looked at yet wears `015 Triage needed` (carrying `needs-triage`, or held in the reporter-trust gate's `reporter_triage` bucket — never derived from who filed it) and lights *Actionable* as "Triage needed" (a human's move) — so the Actionable column reads "Actionable", "Triage needed" or "Needs human decision", and the Open column is untouched. An issue carrying a hold-class label (`hold`, `do-not-merge*`, `hive-pause/<id>`) is never parked: it sits at *Open* as "Open · on hold" even beside `needs-human`, and the reconciler removes any 045 the hold caught it with ([#13012](https://github.com/hivecommons/hive/issues/13012)). The lit chevron ends with an ink-coloured icon of whose move it is — person (human), robot (clanker) or hexagon (hive): the claim holder on Claimed, the PR author on PR in progress and Returned to contributor, hive on CI / Review, Adopted by hive and Ready for auto merge, a human on Needs human decision and Ready for human merge. A row at `090 Ready for human merge` always says *why* a person and not the hive merges it (#14658): the lit word ends `· workflow files`, `· tests removed`, `· manual-merge policy`, `· branch protection`, `· merge method`, `· auto-merge off`, `· untrusted author`, `· unauthorized`, `· reviewer deferred` or `· policy` (payload `human_merge_reason`, a `prstage.HumanMergeReason*` code derived from the stage record's lane reason — no second store), the chevron tooltip leads with the one-sentence `human_merge_reason_text` ("Changes .github/workflows/** — the hive never merges workflow changes …; click Merge on GitHub"), the manual-merge word reads `Awaiting human merge · workflow files · approved by @login`, the search / needs-attention headline carries the same tail, and a PR nested under its issue mirrors `human_approver`, `manual_merge`, `human_merge_reason` and `human_merge_reason_text` onto its `linked_prs` entry so the sub-row says the same. `040 Covered by PR` shares the *PR in progress* column with 030; `060 Reviewed`, `080 Awaiting merge admission` and `100 Merge queue` all light *Merge gate* as "Ready for auto merge" (the chevron tooltip says "not yet admitted — <reason>" for 060/080 and "admitted, merging" for 100); `095 Pre-merge soak` lights the same column as "Soaking" (hive's move; tooltip "admitted, soaking — Xm left", headline "Pre-merge soak — Xm left" from the payload's `soak_remaining_seconds`, aged from the lane's last sweep), so an admitted PR inside its soak window is no longer drawn as merging; the chevron's own tooltip names the exact stage. Covering pull requests nest under their issue. An issue with no PR yet draws the whole strip like any other row, lit at Open, Actionable or Claimed with the steps ahead faint. Once exactly one PR covers it, issue and PR are a single entry: the issue's `#N title` on the left, `↳ [author icon] #PR title` under the *PR in progress* header on the same title line (the ↳ marks a PR drawn on its issue's row; the PR link's tooltip says so — "linked PR, shown on its issue's row" — and, when two open issues list the same PR, every row but the lowest-numbered issue adds "also under #N"), and one continuous strip — Open, Actionable and Claimed passed, the PR's stage lighting the chevron from PR in progress through CI / Review, Merge gate and ✓; the chip, tooltip and `since` are the PR's. With two or more covering PRs the issue's own strip stops at Claimed — all passed, nothing on the issue line lit. Directly beneath, tighter than the gap between entries, sits one indented sub-row per open covering PR, furthest along first: `[author icon] #N title` at the *PR in progress* column, then that PR's own strip from there on — `PR in progress → CI / Review → Merge gate → ✓` — with its own lit chevron, chip, tooltip and `since`. So an issue with two PRs, one at 030, shows "PR in progress" lit on that sub-row, not on the issue. A PR linked to an open issue in the list has no top-level row of its own; a PR with no linked issue keeps a full-width row and shows the first steps as passed. A PR whose only linked issue is already closed — a superseded PR whose body still says `Fixes #N` after an earlier PR fixed it — keeps its own row with the closed issue in the issue slot: `#N title` as a link, muted and struck through, followed by a small `closed` chip, so the pairing reads at a glance; when the title cannot be resolved the slot says `#N (closed issue)` rather than a bare number (the title comes from the snapshot, or from one cached issue read the cycle makes once per issue — see [Linked issues](dashboard.md#pr-review-and-link-signals)). The *Pull requests* filter flattens the list — every PR as a top-level row; *Issues* and *All* show the nested view. Each row is named once on the left; a covering PR appears under the *PR in progress* header as `[author icon] #N title`. A pull request with no linked issue is named once: `[PR icon] [author icon] #N title` on the left over one continuous strip (first steps passed, its own step lit) and nothing in the *PR in progress* column, which is reserved for covering PRs — so a title never appears twice in a row. Pull requests are never issues here: when a number arrives through both the issues feed and the pulls feed the PR record wins and the issue copy is dropped on the server (pairing is keyed by repository, kind and number), and the *Issues* filter never lists a PR. The author icon is the one coloured legend: hive hexagon in blue, clanker robot in purple, human silhouette in teal (the `author/*` kind, inferred from the snapshot when the label is absent; the login is in the `#N` link's tooltip). The right-hand `since` column, in line with the chevron strip, is how long the item has been on its step (red when `stalled`). A plain *Open* step — no stage label yet and not on hold — also carries that age in its own words, "Open · since 3d" ("since 14h" under two days), taken from the same `stage_since` instant the column draws (falling back to the time the item was opened, which the chevron tooltip then marks "(opened)"); past `STAGE_TRACKER_OPEN_STALE_DAYS` (2) the chevron takes a subtle red tint. *Open · hold* never shows the age. An Open item carrying the `blocked` label is drawn the same way as a hold — "Open · blocked" with a ⛔ glyph in the hold's yellow — but keeps the age ("Open · blocked · since 7h"; a stale blocked item is exactly what should stand out, [#14194](https://github.com/hivecommons/hive/issues/14194)) and its chevron tooltip says why: "blocked on owner/repo#N" when the payload names the blocker (`blocked_on`), else "carries the blocked label"; when an item is both held and blocked, hold wins — it is a human instruction. A draft PR — the forge's draft bit, or prow's `do-not-merge/work-in-progress` label — reads "Draft · since 18h" in the Open cell with a ✎ glyph in the muted draft tint, never turns stale (the hive's review and merge lanes skip drafts, so the wait is the author's, not a stuck pipeline), and wears a muted `draft` chip whose tooltip says the lanes skip it until the author marks it ready; a hold or blocked label still wins the cell, with `draft` folded into the chip's `+N` ([#14572](https://github.com/hivecommons/hive/issues/14572)). At most one muted chip sits at the right end of the title line, directly over the strip's ✓ cap (on the last line when the title wraps; a hairline above each entry and a hover tint over the whole entry keep chip and `since` unambiguously with their own row — [#14640](https://github.com/hivecommons/hive/issues/14640)): the most important health modifier (`stalled`, `regressed`, `hold`, `needs-human`, `needs-decision` — shown as "needs human decision" —, `needs-triage` — shown as "triage needed" (amber) on an issue still carrying the `needs-triage` label before the claim (010, 017, 045; at 015 the lit chevron already says so); a `needs-triage` left on a claimed or later issue (020+) is stale, never shown as a chip or stage text, and reported instead as `stale_labels` in the payload with a muted *Stale label* line in the hover tooltip until the stage sweep removes it (hivecommons/hive#14586) —, `needs-rebase`, `blocked`, `draft` — set by the PR's draft bit or prow's `do-not-merge/work-in-progress` label, which the tracker reads as a draft rather than a hold) with a `+N` for the rest. Everything else — the reason sentence, who must act, nudges sent, the repository, all flags and the last stage hops — is in the hover tooltip on the item's `#N` link (on a nested sub-row, on its `#N` PR link). Hover text in a row lives only on the `#N` link, the covering-PR link, the title and the lit chevron (its stage, and whose move it is); passed and upcoming chevrons, the kind and author icons, chips and the `since` column have none. Title lines wrap rather than push the strip out of alignment: the issue title stays before the *PR in progress* column (a clear gap always separates it from the PR entry, which starts at the column edge) and the PR title before the chip; hovering a title shows the full text. Rows are sorted furthest-along first by default — a covered issue ranks by its furthest-along PR — then, within a column, attention-first: rows a person must act on now (any shown pill — `stalled`, `unauthorized`, `needs rebase`, `needs human`, … — or a human-gated stage: 045, 071, 072, 085, 090) come first, plain rows next, and rows nobody should act on — `hold` (incl. `do-not-merge/*`), `draft`, and a PR standing in for a closed issue — sink to the bottom of the column; within a tier the longest on the step comes first, then repo and number. Pills the server holds back until the merge gate (`deferred_modifiers`) never promote a row. The same tiering orders the PR sub-rows under an issue, and the server pre-sorts `GET /api/stage-tracker` the same way (stage, tier, wait, repo, number) so chat `/prs` and the hub read the same order. *Sort* also offers longest waiting (tier first, then wait), newest and number (no tiering); the arrow beside it flips the column / age / repo / number keys but never the tier — attention stays on top in both directions, as its tooltip says ("Attention-first within each column; arrow flips age order"). Every open issue and pull request of the watched repositories is listed — actionable, held, claimed, in progress, covered by a PR, filtered by a `needs-*` label, reporter-triage, draft and exempt-labelled alike; an item without a stage label or journal entry sits at *Open*, and one in a scanner bucket with no label yet is placed from the bucket (claimed → 020, in progress → 030, covered → 040). Filters work on the cached payload in the browser and persist across reloads (the page number does not): kind chips, *Repo*, a search box (`#number` / number / title substring, also matching a covering PR), *Since* (any, over 1 hour / 1 day / 1 week on the current step, or past the stage's configured dwell — stalled items always qualify), *Author* kind, *Needs attention* (anything with a drawn health modifier or on 071 / 072 / 085 / 090 — merge-gate pills held back before the gate do not count), and label chips built from every label present, each with its count: a **🙋 Needs human** facet first (attention-coloured; it matches any item carrying a label of the needs-human set — the server's `needs_human_labels`, the same list as the Projects *Needs human* preset: `stage/045-needs-decision`, `stage/085-needs-decision`, `needs-decision`, `needs-direction`, `needs-spec`, `needs-signal`, `needs-human` and `needs-human/*`, the hold labels, `stage/071-needs-author`, `sentinel-alert`, `needs-triage`; its count is the number of distinct items, not the sum of label counts), then every `stage/NNN-*` label in numerical order, then all other labels alphabetically (case-insensitive). When the row would overflow, the **+N more** button opens an in-app *All labels* dialog listing every label in the same order with counts, a type-to-filter box, the same toggles the chips use and a *Clear* button; a label selected there is promoted into the visible row. Chips of one family OR, families AND; *Needs human* ANDs with the label chips. *Clear filters* shows while anything is narrowed. The list is paged — 25 / 50 / 100 rows, default 50 — with Prev / Next and "showing X–Y of N (M filtered out)" above and below; the header count "N rows · M need attention" is for the filtered set, the **sidebar badge is the row count of the default view** (owner: the tracker counts rows, not items — a covering PR drawn on its issue's row is not a row of its own, and an issue with several PRs already has several), and both come from the same `stageTrackerCountsText()` call so they cannot disagree. When rows sit below the payload's open issues + PRs, the "showing" line's tooltip and the badge's footnote say why — "2 PRs shown on their issue's row" — rather than printing a second total.the chevron's own tooltip names the exact stage. Covering pull requests nest under their issue. An issue with no PR yet draws the whole strip like any other row, lit at Open, Actionable or Claimed with the steps ahead faint. Once exactly one PR covers it, issue and PR are a single entry: the issue's `#N title` on the left, `[author icon] #PR title` under the *PR in progress* header on the same title line, and one continuous strip — Open, Actionable and Claimed passed, the PR's stage lighting the chevron from PR in progress through CI / Review, Merge gate and ✓; the chip, tooltip and `since` are the PR's. With two or more covering PRs the issue's own strip stops at Claimed — all passed, nothing on the issue line lit. Directly beneath, tighter than the gap between entries, sits one indented sub-row per open covering PR, furthest along first: `[author icon] #N title` at the *PR in progress* column, then that PR's own strip from there on — `PR in progress → CI / Review → Merge gate → ✓` — with its own lit chevron, chip, tooltip and `since`. So an issue with two PRs, one at 030, shows "PR in progress" lit on that sub-row, not on the issue. A PR linked to an open issue in the list has no top-level row of its own; a PR with no linked issue keeps a full-width row and shows the first steps as passed. The *Pull requests* filter flattens the list — every PR as a top-level row; *Issues* and *All* show the nested view. Each row is named once on the left; a covering PR appears under the *PR in progress* header as `[author icon] #N title`. A pull request with no linked issue is named once: `[PR icon] [author icon] #N title` on the left over one continuous strip (first steps passed, its own step lit) and nothing in the *PR in progress* column, which is reserved for covering PRs — so a title never appears twice in a row. Pull requests are never issues here: when a number arrives through both the issues feed and the pulls feed the PR record wins and the issue copy is dropped on the server (pairing is keyed by repository, kind and number), and the *Issues* filter never lists a PR. The author icon is the one coloured legend: hive hexagon in blue, clanker robot in purple, human silhouette in teal (the `author/*` kind, inferred from the snapshot when the label is absent; the login is in the `#N` link's tooltip). The right-hand `since` column, in line with the chevron strip, is how long the item has been on its step (red when `stalled`). At most one muted chip sits at the right end of the title line, directly over the strip's ✓ cap (on the last line when the title wraps; a hairline above each entry and a hover tint over the whole entry keep chip and `since` unambiguously with their own row — [#14640](https://github.com/hivecommons/hive/issues/14640)): the most important health modifier (`stalled`, `regressed`, `hold`, `needs-human`, `needs-decision` — shown as "needs human decision" —, `needs-rebase`, `blocked`, `draft` — the PR's draft bit or prow's `do-not-merge/work-in-progress` label) with a `+N` for the rest. A `regressed` chip is red only when the row is parked on a person (a human-gate modifier or stage 045 / 071 / 072 / 085 / 090); on a hive-driven stage with nothing waiting on a human it draws as a muted dashed `stage ↓` pill, yields the lead slot to any other modifier, keeps the row out of **Needs attention**, and its tooltip ends "the hive is handling it — no action needed; clears on the next forward step". The marker is set by a genuine backward hop (a lapsed claim 020 → 010, a 030 ↔ 020 flicker and a 040 → 010 after a partially-addressing merge are not regressions) and clears on the first forward hop; see "The regressed marker" in `labels-and-control-signals.md`. Everything else — the reason sentence, who must act, nudges sent, the repository, all flags and the last stage hops — is in the hover tooltip on the item's `#N` link (on a nested sub-row, on its `#N` PR link). Hover text in a row lives only on the `#N` link, the covering-PR link, the title and the lit chevron (its stage, and whose move it is); passed and upcoming chevrons, the kind and author icons, chips and the `since` column have none. Title lines wrap rather than push the strip out of alignment: the issue title stays before the *PR in progress* column (a clear gap always separates it from the PR entry, which starts at the column edge) and the PR title before the chip; hovering a title shows the full text. Rows are sorted furthest-along first by default — a covered issue ranks by its furthest-along PR — then, within a column, attention-first: rows a person must act on now (any shown pill — `stalled`, `unauthorized`, `needs rebase`, `needs human`, … — or a human-gated stage: 045, 071, 072, 085, 090) come first, plain rows next, and rows nobody should act on — `hold` (incl. `do-not-merge/*`), `draft`, and a PR standing in for a closed issue — sink to the bottom of the column; within a tier the longest on the step comes first, then repo and number. Pills the server holds back until the merge gate (`deferred_modifiers`) never promote a row. The same tiering orders the PR sub-rows under an issue, and the server pre-sorts `GET /api/stage-tracker` the same way (stage, tier, wait, repo, number) so chat `/prs` and the hub read the same order. *Sort* also offers longest waiting (tier first, then wait), newest and number (no tiering); the arrow beside it flips the column / age / repo / number keys but never the tier — attention stays on top in both directions, as its tooltip says ("Attention-first within each column; arrow flips age order"). Every open issue and pull request of the watched repositories is listed — actionable, held, claimed, in progress, covered by a PR, filtered by a `needs-*` label, reporter-triage, draft and exempt-labelled alike; an item without a stage label or journal entry sits at *Open*, and one in a scanner bucket with no label yet is placed from the bucket (claimed → 020, in progress → 030, covered → 040). Filters work on the cached payload in the browser and persist across reloads (the page number does not): kind chips, *Repo*, a search box (`#number` / number / title substring, also matching a covering PR), *Since* (any, over 1 hour / 1 day / 1 week on the current step, or past the stage's configured dwell — stalled items always qualify), *Author* kind, *Needs attention* (anything with a drawn health modifier or on 071 / 072 / 085 / 090 — merge-gate pills held back before the gate do not count), and label chips built from every label present, each with its count: a **🙋 Needs human** facet first (attention-coloured; it matches any item carrying a label of the needs-human set — the server's `needs_human_labels`, the same list as the Projects *Needs human* preset: `stage/045-needs-decision`, `stage/085-needs-decision`, `needs-decision`, `needs-direction`, `needs-spec`, `needs-signal`, `needs-human` and `needs-human/*`, the hold labels, `stage/071-needs-author`, `sentinel-alert`, `needs-triage`; its count is the number of distinct items, not the sum of label counts), then every `stage/NNN-*` label in numerical order, then all other labels alphabetically (case-insensitive). When the row would overflow, the **+N more** button opens an in-app *All labels* dialog listing every label in the same order with counts, a type-to-filter box, the same toggles the chips use and a *Clear* button; a label selected there is promoted into the visible row. Chips of one family OR, families AND; *Needs human* ANDs with the label chips. *Clear filters* shows while anything is narrowed. The list is paged — 25 / 50 / 100 rows, default 50 — with Prev / Next and "showing X–Y of N (M filtered out)" above and below; the header reads "N items (I issues / P PRs) · M need attention" — N, I and P are the whole list, the same `openItems` set the Overview tile and its left-menu counter print, so N always equals the tile's **Total open issues + Total open PRs**; a "· X match filters" note follows while a filter is on. The sidebar counter beside **Hive activity monitor** is that same N — total open items, written in the same render pass as the header, so the two cannot drift apart; how many need attention is the warn-coloured header suffix and the counter's hover tooltip (counted over every item, filters or not). When covering pull requests are nested under their issue the "showing X–Y of R" line counts rows and adds "(P nested under their issues)" so R never reads as a different total. The small monospace strip beside the count — `engine enforce · N anomalies` — is the central stage engine ([ADR-0025](adr/0025-central-stage-engine.md)): `engine` names the current `governor.labels.stage.engine.mode` (green `enforce`, neutral `shadow` = paused, nothing written; hover for what the mode does and the derivation count), `anomalies` counts stage moves the engine's admission table did not expect (red when non-zero). The same numbers are `/api/status.stageEngine`; the mode is changed under **Settings → Labels → Central stage engine mode** and the operator notes are in [operator-reference.md](operator-reference.md#central-stage-engine-rollout). "updated Ns ago · cache" says how old the server-built payload is: the spoke rebuilds it at most every `dashboard.stage_tracker.cache_ttl_s` (default `governor.eval_interval_s`) from its existing snapshot and serves it with an `ETag`, so the browser's 30-second poll is a 304 and no request ever reaches GitHub (`/api/gh-rate-limits` lists the caller `dashboard:stage-tracker` with nothing charged). See [Stage labels](labels-and-control-signals.md#stage-labels-stagennn--the-pipeline-state-machine).

**Filter chips (Needs human / On hold / labels).** The label row under the toolbar starts with two synthetic chips, then one chip per label the listed items carry. **🙋 Needs human** means a person's decision or action is the *only* thing that advances the row: an issue at `015` triage-needed, `045` or `085` needs-decision, a PR at `090` ready-for-human-merge, a PR at `071`/`072` whose author is a human, or any row wearing a `needs human` / `needs human decision` / `unauthorized` pill. It is **not** `hold`, `stalled`, `blocked`, a handled regression, a `040` issue an open PR covers, or a PR the hive will merge itself — those are the hive's (or nobody's) to move. **⏸ On hold** is the separate chip for rows a maintainer paused on purpose with a hold label; a held row never counts as Needs human. The chip label, its count, the filter it toggles and the payload's `needs_human` / `on_hold` totals (and per-item flags) all come from one predicate (`stageTrackerNeedsHuman` in Go, `stageTrackerItemNeedsHuman` in the page), so what the chip counts is exactly what the filter shows ([#15008](https://github.com/hivecommons/hive/issues/15008)). Hover a chip for a one-line explanation. Label chips AND with the synthetic chips.

**Repo chip.** Each row carries a small monospace chip before the number naming the repository it belongs to (`dibs #456`, `hive #13524`); the chip is tinted per repository from a fixed palette so one repo always looks the same, the configured primary repo is shown muted, and the full `owner/repo` is in the tooltip. Linked PR sub-rows carry the same chip. Clicking a chip sets the **Repo** filter to that repository (click it again, or pick "All repos", to clear). By default the chip appears only when the list spans more than one repository; the ⚙ settings modal's **Repositories** section offers **Show repo chip: always / only when multiple repos / never** (stored under `hive.tracker.repochip`).

**Group.** The **Group** control next to **Sort** switches between **None** (one flat list, the default) and **Repo**. Grouped by repo, rows sit under a sticky header per repository showing the `owner/repo`, its row count, how many of those rows need attention, and a mini distribution of rows across the pipeline columns; the primary repo's group comes first, then groups by row count. The chosen **Sort** still orders rows within each group. A header's chevron collapses that group to its header alone; collapsed repos are remembered across reloads (`hive.tracker.group.collapsed`), as is the Group choice itself (`hive.tracker.group`), which is also available in the settings modal. Group headers are keyed by repo so a refresh keeps them in place while rows move, and a header leaves only after the last row in its group has played its exit animation.

**Sounds.** The 🔇/🔊 chip at the right end of the toolbar turns on two optional, browser-made sounds: a "bug zapper" that plays when a pull request already on the list merges while you have the page open (never on first load, never for issues, once per pull request, at most two zaps per refresh, silent while the tab is hidden), and a short bee-like buzz when an issue or pull request first appears on the list (never on first load, once per item, at most two buzzes per refresh, same hidden-tab rule). Both are off by default and remembered per browser; switching them on plays each sound once so you know what to listen for. They fire for every repo the hive governs — there is no primary-repo scoping in the detection. Browsers only let a page make sound after you have clicked or pressed a key on it, and that permission does not survive a reload — including the dashboard's own automatic reload when a new hive version is published. If sound is on but the browser is still holding the audio engine back, the 🔊 chip turns amber (hover it for the reason) and, once per page load, a toast offers an **Enable sounds** button; pressing it, or any click or key press on the page, releases the engine and the chip returns to normal. A zap or buzz that fell in that window is skipped (the toast says so) rather than queued to burst out later.

**Sound settings (⚙ on the card header).** The gear on the Hive activity monitor card opens an in-app settings modal with a **Play sounds** master toggle (the same preference as the toolbar chip), a **Volume** slider (0–100, applied to the built-in sounds and to your own files alike, remembered per browser), and one row per event — **Open sound** (a new issue or pull request appears) and **Merge sound** (a pull request merges). Each row shows what currently plays ("Built-in (bee buzz)" / "Built-in (zapper)" or your file's name), a **▶ Play** preview (the button reads *Playing…* while the sound runs; if the browser refuses to start audio, a toast says so rather than failing silently), a **Choose WAV…** picker that accepts `.wav` or any other audio your browser can decode (up to 4 MB; larger files are refused with a toast), and **Reset to built-in**. Uploaded files are kept in this browser only (IndexedDB) and decoded with the Web Audio API; if a stored file cannot be decoded the built-in sound plays instead and the row says so. **▶ Play both** auditions the merge sound followed by the open sound. Previews start the browser's audio engine and schedule their sound inside the click itself (Safari only unlocks audio for work done synchronously in a user gesture) and otherwise wait for the engine to report it is running (about a second at most), so they work in Safari and on a page that has never made a sound; an automatic zap or buzz on an engine the browser still holds back waits only briefly and otherwise skips rather than queueing up to burst out on your next click, and the next click or key press re-arms it.

**Movement animation (⚙ on the card header).** When a refresh moves an item's lit chevron to a different column — forward (advance) or backward (regress) — the row plays a short, subtle three-beat sequence so the eye can see where it went: first the chevron it is *leaving* flashes the active highlight twice (two ~140 ms pulses with a short gap), then the chosen effect travels from that chevron to the new one, and finally the new chevron lands lit — the whole sequence takes about a second. Pick the travel effect in the same settings modal, under **Movement animation** below the sound settings: **None**, **A Sliding puck** (a highlight glides from the old chevron to the new one and settles), **B Ripple** (each chevron between old and new lights in turn), **C Ghost crossfade** (the old highlight fades out while the new one fades in with a slight slide), **D Comet** (a streak with a bright head travels the strip and lands on the destination), **E Destination bump** (the destination chevron scales briefly with a fading direction arrow), or **F Colour wash** (default — a tinted sweep flows across the intermediate chevrons). Each option has a *Preview* button that plays the full flash → move → land sequence on a sample row inside the dialog. The choice is remembered per browser (`localStorage` key `hiveStageTrackerMotion`). Items that stay put never move; a row that leaves the list (merged, closed, or filtered away) collapses and fades out over `--ham-move-exit-ms` and a row that arrives fades in over `--ham-move-enter-ms`; nothing animates on first load, and at most twelve rows animate per refresh (a filter or page change that turns over more rows than that redraws at once). The list is reconciled row by row on every refresh rather than rebuilt, so a move keeps playing while the hive keeps publishing. When the operating system asks for reduced motion (`prefers-reduced-motion: reduce`), every style falls back to a single soft flash followed by a plain crossfade and departing rows only fade, regardless of the setting; **None** disables the flash, the effect and the row fades entirely. Durations, the flash timing (`--ham-move-flash-ms`, `--ham-move-flash-gap-ms`, `--ham-move-flash-count`) and tints come from the `--ham-move-*` tokens in `tokens.css`.

**Sentinel alert.** A pull request the suspicious-activity sentinel flagged — it wears the `sentinel-alert` label (`sentinel.label`), or the review evidence bundle for its current head records sentinel findings — carries `sentinel_alert: true` in the tracker payload (with `sentinel_findings` and a short `sentinel_reason` when the bundle has them); a webhook label change flips it live. In the **Needs human decision** lane the lit chevron's own words read **Sentinel alert · needs human** (danger-tinted, the same wording in the cell's `title` and `aria-label`, and in the compact caption), so a maintainer reads the security implication before the stage mechanics; in every other lane at or past the merge gate (080, 090, 095, 100) the row shows a compact danger-tinted `sentinel` chip ahead of `needs rebase` / `regressed` / `stalled`; before the gate the chip is held back with the other merge-gate pills (see *Pills before the merge gate*) while `sentinel_alert` stays truthful in the payload. The tooltip reads "Sentinel flagged security-relevant changes at `<sha7>`: `<reason>`" (a generic description stands in when the bundle has no reason). An issue carried by one flagged PR shows the lane text too, and a nested PR sub-row shows its own. The *Decide…* dialogs for a flagged PR open with a ⚠ line per flagged PR naming the sentinel reason, and *Approve* is styled as a dangerous action. A person's *Approve* through that dialog **is** the human review the alert asked for: it removes the `sentinel-alert` label when present and records a `sentinel_acknowledged` human action keyed to the head in the review evidence bundle, so a bundle-only alert (findings but no label — hivecommons/crux#30) clears too; the payload then reads `sentinel_alert: false` with `sentinel_reason` suffixed `— acknowledged by @login`, the findings stay on record, and a new head starts over. The delegated *Let hive decide* path never clears it. Hovering the lit chevron of a parked row names the parking stage itself (`Needs human decision`), not the 080 column it rides on; `Next move` follows the real stage (human at 085, hive at 080). Lane text and chip update in place on refresh (keyed reconcile, no flash).

**Unauthorized pill.** A pull request the hive cannot move because its GitHub App lacks a permission for the next step — it wears `hive:unauthorized` ([#14589](https://github.com/hivecommons/hive/issues/14589)); first case: approving held `action_required` workflow runs needs `actions: write` — carries the `unauthorized` modifier in the tracker payload, drawn in the far-right pill column exactly like `stalled` / `hold` / `draft` but muted (dashed, `--text-muted`), never red: it is nothing wrong, just a step only a person may take. It counts under **Needs attention**. Before the merge gate only the held-workflow-runs step shows (see *Pills before the merge gate*). Its tooltip (payload `unauthorized`) names the missing permission and the human step; on the **Needs human decision** lane the lit chevron and the caption read **Awaiting human: approve workflow runs** (payload `unauthorized_step`, from the `unauthorized:<step>` stage reason). The pill, label and payload fields clear themselves in the tick the step is no longer needed.

**Pills before the merge gate.** Owner's rule (2026-10-27): until a pull request is in the merge gate there is nothing a person can do about a `sentinel`, `needs human` or `unauthorized` notice — the PR is still in motion — so the tracker must not show them. The server splits every marker into two classes (`stageTrackerModifiers` in `stage_tracker.go`; each carries an explicit `ActionableNow` flag) and the row's payload carries the pills it draws in `modifiers` and the ones it holds back in `deferred_modifiers`. A deferred pill is simply absent — no chip, no breathing chevron, no **Needs attention** count — until the PR's stage reaches the merge-gate band (`stage/080` and later: 080, 085, 090, 095, 100, the same boundary adoption yields at), when it renders exactly as before and counts; the row tooltip alone says "Pending at merge gate: sentinel, needs-human". The merge-gate cell's own words ("Sentinel alert · needs human", "Awaiting human: …") are unchanged — they only appear at the gate anyway. Issues on their own stages (Open, 010, 020, 045) keep every pill: a `needs-human` there is a park the person owes now; an issue riding its covering PR's stage follows the PR. The GitHub labels are untouched — this is presentation and counting only.

| pill | class | when shown |
| --- | --- | --- |
| `hold` | actionable now | always (a person placed it) |
| `draft` | actionable now | always (the author's move) |
| `stalled` | actionable now | always |
| `regressed` | actionable now | always; muted `stage ↓` unless the row is parked on a person (see above) |
| `needs-rebase`, `blocked`, `triage needed`, `possible dupe` | actionable now | always |
| `unauthorized` — `unauthorized:approve-workflow-runs` | actionable now | always — the carve-out: held workflow runs mean CI cannot even start, and waiting for the gate would waste the whole CI / review cycle (rationguard#397 at 072) |
| `unauthorized` — any other step (merge, branch update, no step named) | merge gate | PR at stage/080 or later |
| `sentinel` (the trusted-author notice and the real `sentinel-alert` alike) | merge gate | PR at stage/080 or later; `sentinel_alert` itself stays truthful in the payload for the lane word and the Decide… preface |
| `needs human` (`needs-human` label) | merge gate | PR at stage/080 or later; always on an issue at its own stage |
| `needs human decision` (`needs-decision` labels on another stage) | merge gate | PR at stage/080 or later; always on an issue at its own stage |
| ✓ decision badge (`last_operator_action`) | before the pills; green / amber (still at the same stage past the stuck window) / red (failed) / dotted … (hive deciding) | always, once a row action was submitted from the dashboard; tooltip tells what you chose, when, and what the hive did next |

**Adoption pill (072).** A nudged `author/clanker` pull request at `stage/072-returned-to-contributor` carries `adoption{author, due, blocked_by, since, nudges, author_online, author_registered, author_last_seen, author_maintainer, policy, action, text}` — the nudge count and first-nudge time from the stage record, the author's live hub presence (the same lookup the contributor-offline adoption itself consults), and, when the adoption engine judged adoption due and held it, why (`blocked_by: author-is-maintainer`). The row's caption and an amber pill say it in plain words instead of a bare "Returned to contributor": `waiting on @hanthor (maintainer, offline 26h) — adopt?` under `governor.labels.stage.adoption.maintainer_policy: ask`, `waiting on @zoe (offline 40m, 2 nudges)` while the default `adopt` policy's clock runs, `… — not adopted: maintainer_policy is never` under `never` ([hivecommons/hive#14524](https://github.com/hivecommons/hive/pull/14524): the PR sat 26h at 072 with its author's hive gone and nothing on screen saying why). When `action` is `decide:adopt` the row also carries `decision_needed` (question "@hanthor maintains this repository and has been offline for 26h; adopt the PR?", `blocker_reason: adoption:author-is-maintainer`, options Adopt then Hold/Release), so the lit 072 chevron and the `adopt?` pill open the Needs-human popover, and the row menu and the selection bar offer **Adopt (hive takes over)** — same action-id vocabulary as the other `decide:*` entries — which, after an in-app confirm, POSTs `{"decision":"adopt"}` to the item's decision endpoint: the hive posts an attributed note, sets `adoption/supersede` (the owner-delegate's request label) and the next sweep adopts the head on the existing adopt/supersede path; the toast reads `adoption-queued`. Write access required; the action is absent when nothing is the owner's to decide.

**Blocked pill.** An issue wearing `blocked` because the hive parked it on a pull request — its body carries `<!-- hive:blocked-on pr=owner/repo#N -->` (the review backlog's spin-off issues, `hive-open-issue comment --blocked-on-pr`) — names that PR: the payload carries `blocked_on_pr: {repo, number, url, state}`, the pill reads `blocked on #N` and links to the PR, its tooltip says `Blocked on PR owner/repo#N (open) — lifts when it merges; re-triage if closed unmerged`, the caption reads `Blocked on PR #N — lifts when it merges` instead of `No stage recorded yet`, and the row's title and chevron tooltip carry the blocker too ([#14952](https://github.com/hivecommons/hive/issues/14952): #14863 showed a bare grey pill and "blocked · since 7h" with nothing to say what it waited on). The state (`open`, `merged`, `closed`, or absent when nothing knows) comes only from what the hive already holds — the status snapshot's open PRs, the ForgeState cache, the recent-merge ring — never a GitHub call from the tracker build; a merged or closed-unmerged blocker says so (`merged; the blocked sweep lifts the label next` / `closed unmerged; the blocked sweep re-triages it next`). The server reads the marker from the issue body when it has one, else from the enumeration's `blocked on owner/repo#N` reason, so sidelined rows (which carry only the reason) name it too. A `blocked` issue with neither (a human-applied label) keeps the bare pill. This is distinct from `blocked_on`, the open GitHub "blocked by" dependencies of a sequenced sub-issue ([#14765](https://github.com/hivecommons/hive/issues/14765)); when a row has both, the chevron tooltip lists them together.

**Hive's own unblock is live.** When the blocked sweep or the review-backlog block reconciler lifts `blocked` itself (the tracked PR merged or closed; the `<!-- hive:unblocked -->` note), it takes the same path as an inbound label webhook: the tracker cache is dropped and the issue is queued for the targeted re-read (`ItemPatches` delivery `self:edit`), which patches the status row and the governor's cached snapshot at once. A reconcile enumeration that was already listing when the unblock happened used to overwrite that patch with its pre-unblock copy, and the rows reverted to `blocked` until the next pass (~an hour on the hive's hive); the governor now replays re-reads newer than the listing's start onto the pass's result before publishing it, so a row never moves backwards behind a read it has already made.

**Closed issue, open PR.** A pull request whose `Fixes #N` issue has already closed (auto-closed by a bot, or closed by hand) while the PR is still open is live work, not a stale line: the row is anchored on the PR ([#14655](https://github.com/hivecommons/hive/issues/14655)). The PR's own icon, number and title lead the row at full width, its stage drives the strip and its timestamps sort it — closing the issue is not a regression of the PR — and the closed issue shrinks to a muted `closes #N · closed` chip linking to it. An issue closed as *not planned* adds an `issue closed (not planned)` chip so the reader can tell the PR may be moot. At 090 such a row still counts under *Needs attention*: the human has a merge to click. When the PR closes or merges too, the row leaves the list as before (a merged PR shows in *Recently merged* for its retention window).

**Row gutter.** Every issue and PR row (never a column or repo-group header) carries a left gutter with a select checkbox and a ⋯ actions button that is always present and always visible: dimmed at rest (`--ham-tool-rest-opacity`), at full emphasis on the row under the pointer or holding focus, on a selected row, and on every row while anything is selected or the context menu is open — the only animation is the hover fade (`--ham-tool-fade-ms`, ≤ 120 ms), and nothing about it depends on scrolling, row position or which rows the live refresh re-created.

**Did I act on this? (the decision badge).** Owner (2026-10-28): "I need a way to see if I selected an action on an item so I can keep track of whether or not I addressed it. Hovering over the indicator should show me what action I requested." Every row action submitted from the dashboard — Approve, Give direction, Close as superseded, Close as won't-fix, Let hive decide, Hold / Release hold, Accept triage, Mark needs-human — is written to a server-side ledger (`/data/operator-actions.json`, `operator_actions.go`; newest 20 per item) as `{action, actor, at, note, target, outcome, detail, head_sha, stage}`, so the record survives reloads and shows in every browser. The tracker row carries the newest one as `last_operator_action` (+ `operator_action_count`) and `GET /api/repos/{owner}/{repo}/items/{number}` returns the full list. The row then wears a small **✓ badge** in the cap cell, before the state pills (`.ham-acted`): green once an action is recorded; **amber** when the item is still at the same stage past the stuck window (`HIVE_OPERATOR_ACTION_STUCK_AFTER`, default 30 min — "acted, but nothing happened"); red when the action itself failed; a dotted `…` while *Let hive decide* is still running. The in-flight spinner on the chevron is a different cue: that is the request still on the wire; the badge appears once the server has recorded it. Hover or focus (the badge is in the Tab order) opens an in-app tooltip — never a native `title` — written in plain words, no stage codes, in the order every cue on the row uses: **what it is waiting on → what you chose → what the hive did next**. E.g. `At Merge gate — waiting on CI.` / `You chose Approve · 15:55 (5 min ago) · done — released to the hive.` / `Then the hive moved it to Merged — a reviewer approved it · 1 min ago.` Once the item moves on, the badge stays and the first line reads `Addressed — now at <stage>`; while stuck it reads `Still at <stage> 42 min after you asked — hive says: <reason>`. Another operator's action is named (`@someone chose …`); your note and "N earlier actions on this item" follow. The **Decisions** select in the toolbar narrows the list: *Awaiting my decision* (parked on a person, no action recorded yet, or the last one failed), *I acted* (any badge), *Acted, still stuck* (amber badges only).

**Legend — the three cues a row may carry** (shared with the in-flight and needs-human work landing beside this): the *state pill* says where the item is and what it is waiting on (`hold`, `needs human`, `stage ↓` …); the *working* spin on the lit chevron says the hive is doing something in that stage right now (a still chevron = waiting on something, its tooltip says what); the *decision badge* (✓) says a person acted here and what followed. Plain words throughout — working / waiting on X / set back by Y / merged — nothing else blinks.

**Row actions (right-click, ⋯, Shift+F10).** Right-clicking a row — or a nested PR sub-row — opens an in-app context menu of the actions that fit that item's current state; the ⋯ button in the row's left gutter (every nested PR sub-row has its own gutter with its own checkbox and ⋯) opens the same menu for pointer-free use, and so do **Shift+F10** and the context-menu key while the row or anything in it has focus (every row is a tab stop). The menu is positioned at the pointer, clamped to the viewport, uses `menu` / `menuitem` roles, moves focus with ↑ ↓ Home End, opens the *Decide…* group with → and closes it with ←, and closes on Esc, Tab, click-away, scroll or resize, returning focus to the row. Shift+right-click keeps the browser's own menu. Entries, each shown only when it applies: **Decide…** on a row waiting on a person — a PR wearing the *Needs human decision* pill at 085 on the Merge gate column (the primary case: e.g. two competing `dibs` PRs), an issue row whose one nested PR is parked there (the menu reads *Decide PR #458…* and acts on that PR, since the issue's own stage is only mirrored from it), or an issue parked at 045. **PR-scoped actions on nested PRs** ([#15009](https://github.com/hivecommons/hive/issues/15009): one issue, two PRs — [#15017](https://github.com/hivecommons/hive/pull/15017) *Ready for human merge · untrusted author* and [#15020](https://github.com/hivecommons/hive/pull/15020) *Needs human decision* — and no way to decide one without the other): every nested PR sub-row carries its own ⋯ menu, and its *Decide…* (Let hive decide, Approve, Close as superseded by…, Won't fix…, Give direction…), *Hold* / *Release hold* and the rest act on **that PR** — `runRowAction(action, row)` receives the PR's own row object (number, repo, head), never the parent issue's, and the needs-human popover opened from a sub-row's chevron does the same. A PR waiting for a person to merge it (*Ready for human merge*, a `human_merge_reason` such as an untrusted author) counts as parked for this purpose. When an issue has two or more open nested PRs the issue row's menu no longer offers a single *Decide…* that would silently pick one: it offers **Let hive decide — all N PRs** (hint *per PR*), which asks the server to decide each PR on its own (see [Let hive decide](#let-hive-decide)); the issue's own *Decide…* stays only when the issue itself is parked at 045. *Close as superseded by…* on a nested PR with exactly one open sibling under the same issue pre-fills that sibling's number. The first entry is always *Let hive decide* (see [Let hive decide](#let-hive-decide) below): the hive decides as the owner's delegate and records it the way a person's decision is recorded. For a PR: *Approve (/lgtm + /approve)* posts those two commands as comments attributed to you and records your approval against the PR's current head as the human answer the row was parked for — a `requires_human` / decision-requested reviewer verdict, the hive's own `needs-human` park (a supersession question, say), the `sentinel-alert` label and the evidence bundle's sentinel findings are all released by it (the response's `released` lists what was: `approval-recorded`, `needs-human`, `sentinel-label`, `sentinel-acknowledged`), the stage engine re-derives the stage on the spot and the tracker cache is dropped, so the row leaves *Needs human decision* for 080 *Awaiting merge admission* on the next read; the merge gate (and the auto-merge lanes, which honour the recorded approval on that head exactly as a trusted merger's GitHub Approve review) takes it from there; the dashboard never reviews as you and never merges. A new head carries no answer: pushing to the PR re-asks. The delegated *Let hive decide* path still posts the comments only — it recommends and never clears a sentinel alert — *Close as superseded by…* (a picker listing the other open PRs in the repo, or type `#458` / `owner/repo#458` / a URL; then an optional comment, then the sweep's labelled close), *Won't fix…* (a comment naming you, then a not-planned close) and *Give direction…* (your text is recorded as `/hive decision <text>` against the head, so the author agent pushes a new head; only on PRs the hive opened). For an issue: *Approve direction* (records the same `/hive approve` a maintainer would comment: the parking labels come off, `approved-direction` goes on, the stage engine logs the un-park and a reply comment names you; nothing is merged), *Send back to the hive…* (`/hive decision <text>`), *Close as superseded by…* and *Close as won't-fix…*. A selection mixing parked issues and parked PRs has Decide… greyed; **Hold** or **Release hold** depending on the `hold` chip (the same `…/items/{number}/hold` toggle as the repository cards, and the same permission — owner, or a signed-in user with write access to the repo); **Re-evaluate now** (the hive-wide read-only rescan behind the Overview's refresh — a rescan already running is reported, not duplicated); **Accept triage** (only on an issue the chevron calls *Triage needed* — stage 015, or the `needs-triage` chip on another stage — never on a PR; the one-click form of a maintainer accepting triage on GitHub: adds the reporter-trust triage label, `triage/accepted` by default or the first of `issue_filter.reporter_trust.untrusted_require_labels`, removes the awaiting label `needs-triage` / `reporter_trust.awaiting_label` and a Hive-written `stage/015-triage-needed`; the row re-reads *Actionable* with the 👤 gone at once, the reporter-trust gate admits the issue on its next poll and the stage engine records 015 → 010; same permission as Hold, audited as `repo_item_accept_triage`, a no-op when already accepted); **Mark needs-human…** (hidden once the chip is on; merger or owner role, applies `needs-human` and a marker comment through the review pipeline's send-to-human endpoint); **Open on GitHub**; **Copy link**. Entries you may not use are greyed with the reason in their tooltip, never hidden. Confirmation and free-text input use the dashboard's own dialogs — no browser prompts. Every action is an existing audited dashboard endpoint (the decision endpoint is `POST /api/repos/{owner}/{repo}/items/{number}/decision` and triage acceptance is `POST /api/repos/{owner}/{repo}/items/{number}/accept-triage`, see [api-reference.md](api-reference.md)); none of them bypasses the hive's governance or merges anything.

**What a row tells you without a click (`tracker-cues`).** The owner's framing (2026-10-28): "Hive is doing the right things mostly; we now need to keep the user informed of it — like a video game they can watch and see what happens easily." Every row therefore reads, in this order, *where it is → whether work is happening on it → what it waits on → what just happened*, with at most three cue types drawing the eye and nothing else blinking. **Where it is** is the lit chevron (the state pill). **Working** — the chevron's icon turns slowly and the pill pulses only while a hive agent is actually on the item right now (the status payload's `agents[].lastAction` for the current round, matched to the row or to the PR shown on its row); its tooltip then leads with *Working now: scout commented · 2m ago —* before the stage words. A still chevron means **waiting**; its tooltip already says where it stands and whose move it is (*Needs decision — … · Next move: human*). **Acting** — from the moment you choose a row action the affected row (and the nested PR sub-row when the action targets it) draws faint stripes over its lit chevron and an *Approving… / Hive is deciding… / Holding…* caption with a small spinner; it clears when the tracker reports the target in a new state, at once on a no-op or an error, and never later than three minutes. **Event line** — one short plain sentence under the strip about the latest thing that happened to the row, from the difference between two tracker payloads (the poll / SSE refetch the tracker already makes; no extra requests): *Hive: moved to CI / Review · 12s ago*, *Hive: fell back to Claimed — a reviewer requested changes*, *Hive: waiting on a person*, *Hive: hold released*, *You: approved*. It lives two minutes, dims for its last third, and a row carries only its latest line. **Set back** — a row wearing `regressed` colours the line's lead red. **Merged** — see *Merged today* below. The CSS class names are shared so the sibling Activity Monitor PRs (needs-human options, decision trail) compose on them: `cue-working`, `cue-acting`, `cue-event`, `cue-setback`, `cue-merged`, `cue-decision` (badge, sibling PR), `cue-spin` (the one spin keyframe), `cue-token` / `ham-merged-count` (the merge counter); the colour tokens `--cue-state`, `--cue-work`, `--cue-decision` on `.ham-row` are declared in the *Cue tokens* paragraph under [Let hive decide](#let-hive-decide) below. Legend, in the operator's words: **working** (icon turns), **waiting on X** (still; tooltip names X), **set back by Y** (red lead on the event line), **merged** (✓ glows, token rises to the count). Under `prefers-reduced-motion: reduce` every cue animation stops; the words stay.

**Result toasts (`runRowAction`).** Every row action — *Let hive decide*, *Approve*, *Give direction*, *Close as superseded*, *Won't fix*, *Approve direction*, *Send back to the hive*, *Accept triage*, *Hold* / *Release hold*, *Re-evaluate now*, *Mark needs-human* — goes through one SPA helper, `runRowAction(action, row)`, which marks the row acting, sends the request, and renders a toast from the server's shared `result` object (additive on every endpoint's existing response: `{action, target: {repo, kind, number, url}, outcome, next, rule?, applied_at?}`, `action` one of `adopted | approved | recommended | closed | directed | refused | held | released | rescanned | triage-accepted | needs-human | noop-already-applied`). The toast reads, in order, *what happened → on what → what happens next*: a headline naming the clicked row and, when it differs, the target (*Hive decided on acme/console#14938 → PR #14947: adopted (contributor offline)*), the outcome sentence, the ⚠ sentinel line when the head carries one, the *next* line (*The hive rebases and merges it unattended.*), an explicit ✕ and an *Open PR #14947 ↗* link. It does **not** auto-dismiss: it stays until the tracker reports the target row in a new stage, caption or chip set (or gone), and up to five such toasts stack for pending decisions. A no-op says so plainly — *No change — hive already recorded this decision at 12:01Z; still blocked by needs-human* — and clears the acting state at once; an error toast says what failed and why (*Hold failed on acme/console#7 — write permission required*) and is sticky but not tied to a row. Older hives that answer without `result` still get a toast built from the legacy fields.

**Confirmations and "Don't ask again".** Each confirm dialog a row action raises — Approve (including a sentinel-flagged head: *⚠ Sentinel flagged security-relevant changes at 40a959b …*), Let hive decide, Close as superseded / won't-fix, Mark needs-human and the rest — carries a **Don't ask again for this action** box. Ticking it and confirming stores `hive.tracker.confirm.<action>=skip` in this browser's `localStorage` (one key per action id, e.g. `hive.tracker.confirm.decide:delegate`); later clicks on that action run at once. Nothing the dialog would have said is lost: the result toast carries the same ⚠ sentinel line (it is per head, so skipping the confirm never skips the warning) and the one-line item summary. Actions that need your text (*Give direction…*, *Send back…*, the superseder picker) always ask. The ⚙ settings dialog's **Confirmations** section lists which actions are skipped and has one **Reset confirmations** button that turns them all back on.

**Merged today.** The toolbar's right end shows a small hex glyph with **Merged today: N** — pull requests the hive saw merge since your local midnight (seeded after a reload from the tracker payload's `merged_24h` list, the audit log's `pr_merged` entries over the last 24 hours, and grown live by the same merge detector that plays the zap); hovering lists the last five with their times. When a PR on the list merges its row does **not** simply vanish on the next refresh: it stays pinned for about 2.5 s, its ✓ cell glows amber, the event line reads *Hive: merged #14947 · by hive*, a flat brand-amber hex token lifts out of the ✓ cell and arcs (≈0.5 s) to the counter, which ticks up with a soft scale-pop — the merge zap, when sound is on, plays as the token lands — and then the row takes its normal exit. Several merges in one refresh stagger 200 ms apart with at most three tokens in flight (the rest tick the count without flying); a merged PR that is filtered out or on another page still counts. Under reduced motion nothing flies: the count ticks and the row glows. The ⚙ dialog's **Merges** section has **Celebrate merges** (default on, `hive.tracker.celebrate`); off, the row leaves as before and the zap plays at once. The row effect is reusable in the SPA as `celebrateRow(rowEl, {kind: 'merged' | 'closed-by-merge'})`.

**Multi-select.** The checkbox in a row's left gutter selects the row — and the checkbox in a nested PR sub-row's gutter selects that PR on its own, so one or more PRs under an issue can be picked without the issue or each other; **Shift+click** selects the range from the last clicked row (sub-rows are in the range order right after their issue), **Cmd/Ctrl+click** toggles, **Cmd/Ctrl+Shift+click** adds a range to what is selected; with a row focused, **Space** toggles it (Shift+Space extends). Cmd/Ctrl- or Shift-clicking the row itself, away from its links, selects it too. A selection bar appears above the list with the count, **Clear**, and the same actions for the whole selection — *Decide…* (opens the group as a menu, *Let hive decide* first — each selected row gets its own delegated-decision job, see below), **Let hive decide (N selected)** (one click for the whole selection: one delegated-decision job per selected item, each nested PR sub-row by its own number, with one summary toast — "3 sent · 2 decided · 1 no-op", plus "· 1 failed" when any failed — and the per-action confirm / *Don't ask again* of *Let hive decide*), *Hold*, *Release hold*, *Re-evaluate now*, *Mark needs-human…*, *Copy links* — each enabled only when it is valid for every selected row and otherwise greyed with the first reason ("Not applicable to acme/console#12", "Sign in with write access…"). Right-clicking a selected row acts on the whole selection. Bulk actions run one request per row, in list order, under a progress toast ("Hold 2/5 — acme/console#12…"); the result toast lists each failure by row and the successes stand, after which the list and the status snapshot refresh (a hive-wide rescan is sent once per batch). Selection is keyed on the row's item (repository, kind, number), not its node, so the live re-render keeps it; a row that leaves the list — merged, closed, filtered or paged out — leaves the selection.

**Needs-human decision popover.** Hovering — or, by keyboard, focusing — the *Needs human decision* chevron word on a row (an issue at 045, a PR at 085, an issue whose nested PR is parked there) or a lead *needs-human* / *triage needed* pill opens an in-app popover (no native tooltip on that element) that says, in this order: **what the row is waiting on** — the reviewer's own question when the hive has one (the text the hive's "Needs a decision" comment on the PR prints after *reviewer question:*, read from the same verdict record), else a plain-words sentence for the park reason, plus the blocker in plain words ("A reviewer requested changes on b0f5f80 — needs a new commit. Approving cannot override that."); **what you can do** — one button per applicable action, *Let hive decide* first, then Approve / Give direction / Close as superseded / Close as won't fix / Hold or Release hold (an issue at 015: Accept triage, Hold, Mark needs-human), each disabled with its reason when the row's state or your role rules it out; **who asked and when** ("Asked by a reviewer · 2 hours ago"); then the chevron's stage line. The buttons run the row context menu's code path, so the same result toast and in-flight chevron apply. The popover stays open while the pointer is over it or its trigger, pins on click or focus, and closes on Esc (focus returns to the trigger), click-away, scroll or resize. The text comes from the tracker payload's `decision_needed` (`question`, `asked_by`, `since`, `blocker`, `blocker_reason`, `options[] {action, label, enabled, reason_if_disabled}`), whose `action` ids are the context menu's own (`decide:delegate`, `decide:approve`, …).

**Cue tokens.** The three cues of the legend above (state pill, working spin, decision badge) share one `tracker-cues` CSS block on `.ham-row`: `--cue-state` (what the row waits on — the pill and this popover), `--cue-work` (the hive is working right now — the chevron spin), `--cue-decision` (what people and the hive decided — the ✓ badge). Nothing else blinks; motion stays in the 150–300 ms range.

### Let hive decide

**What it is.** *Let hive decide* is the first entry under *Decide…* on a row parked at *Needs human decision* (an issue at 045, a PR at 085, or an issue whose nested PR is parked there), and under *Decide…* in the selection bar for a bulk run. It asks the hive to make the decision itself, as the owner's delegate, under the owner's standing operating rules — and to record it exactly as a human decision is recorded: a `/hive decision <text>` comment on the item (plus the queue approval on a PR head when the decision is to release the PR), the parking labels lifted, `triage/accepted` added where it applies, and the stage engine's un-park logged with the requesting operator named. The item leaves 085/045 and proceeds without the owner. The action is audited as `repo_item_decision_delegated` (or `repo_item_decision_delegated_failed`) and the **Decisions log** shows those rows with a *delegated* badge so delegated and human decisions can be told apart and counted.

**How it runs.** Clicking it posts `POST /api/repos/{owner}/{repo}/items/{number}/decide-delegated` (body `{"type": "issue"|"pr", "reason", "caption", "sentinel_reason"}` — the row's park words, so the decider reads why the hive stopped) which enqueues one delegated-decision job per item (a second click while a job is running answers 409 with the running job). **An issue with two or more open nested PRs is never decided by silently picking one** ([#15009](https://github.com/hivecommons/hive/issues/15009)): *Let hive decide — all N PRs* on the issue row sends `"type": "issue"` with `"prs": [15017, 15020]` (the open nested PR numbers; without the hint the server reads them from the tracker's `linked_prs`), and the server runs one delegated decision per PR — each under the PR's own job key, so `GET …/items/{pr}/decide-delegated` reads it and a PR already being decided is reported, not decided twice — and answers with `items` (one entry per PR: `number`, `type`, `status`, `decision`, `notes`, `error`, `result`) plus a summary `result` (`action: "decided-each"`, outcome "Decided each of the 2 nested pull requests: #15017 approved (small low risk); #15020 no change (already decided).", next "2 sent · 1 decided · 1 no-op. Each PR's own toast says what happens next."). The issue row's toast shows that summary and each PR gets its own result toast on its own sub-row; the job fails only when every PR failed. One nested PR (or none) keeps the issue's own decision as before. Before the request an in-app dialog explains the delegate once (what it records, its rails) with a *Don't ask again for this action* box (see *Confirmations* above; reset from ⚙ → Confirmations). The row shows *Hive is deciding…* over its lit chevron, the page polls the same URL (`GET`) until the job reports `done` or `failed`, and the job's `result` (`rule`, `action`, `target`, `outcome`, `next`) becomes a persistent result toast that says what hive decided — *Hive decided on acme/console#14938 → PR #14947: adopted (contributor offline) — Adopted acme/console#14947 in supersede mode as #14977. The hive rebases and merges it unattended.* — and leaves only when the target row changes state. Clicking again after the same rule already decided the item writes nothing and answers `action: noop-already-applied` — *No change — hive already recorded this decision at <time>; still blocked by <reason>*. A job gathers the item (title, body, labels, author, state), the PR's mergeable state and changed files, whether the author is a hive account, the author's repository permission (trusted = write or above), the sentinel label, and the row's park reason, then runs the policy and executes the resulting actions through the same GitHub helpers the manual *Decide…* entries use. Permission is the one *Hold* uses (owner, or a signed-in user with write access to the repository); the requesting user is named in the recorded decision as the person who delegated it.

**The policy.** The rules live in one place — `src/pkg/delegatedecision/policy.go` (`PolicyText`, the owner's words verbatim, including the contributor-adequacy rail adopted in #13998; the Go rules beside it implement them) — so the dashboard, docs and any future LLM-backed decider share the same text. In short: be decisive and, except for the safety rails, never ask the owner a question back; pick the option that keeps work moving and is reversible. **PRs:** prefer an adequate contributor PR over a hive duplicate (a mergeable, current competitor prompts a human comparison of issue coverage, checks and review approval; keep the hive PR open and parked until adequacy is confirmed, rather than closing it solely on an issue reference; an unmergeable contributor PR is superseded with credit); approve small, necessary, low-risk changes (CI pins, read-only RBAC adds, warning text, workflow hygiene) once the sentinel finding is checked and explained; a sentinel alert on a trusted author's PR is approved with a one-line "reviewed, intended" rationale; a reporter-trust hold is cleared when the fixing PR is sound; a contributor who has gone quiet is superseded now, not after a deadline (#13975). **Issues:** dedupe aggressively (`Duplicate of #N`, cross-link overlaps); secrets / PAT / org-permission items are owner-only — `needs-owner` + `hold` with one comment stating the exact owner action, and no more failure issues for that root cause; flaky tests are quarantined with a retry and a deflake task rather than blocking; CI hygiene gate proposals (actionlint, duplication ratchets, link-checker allowlists) are approved as ratchets (baseline; fail only on increases; warning-only briefly, then enforce); design proposals with concrete acceptance criteria are accepted as proposed with explicit defaults; a "review backlog" item is never auto-closed when its PR closes unmerged (drop the relation, comment, re-triage). Each decision names the rule that fired (`dedupe`, `owner-only`, `flaky-test`, `ci-ratchet`, `design-proposal`, `review-backlog`, `default-approve`, `prefer-contributor`, `contributor-offline`, `sentinel-trusted`, `small-low-risk`, `reporter-trust`, `fix-then-merge`, `default-release`, `hive-policy`, `sentinel-untrusted`, `rail-hold`) in the comment and in the job's `decision.rule`.

**Safety rails** (`ApplyRails`, applied after the policy and before anything is written): the delegate never merges a PR — it only records the approval the auto-merge engine already acts on per repository settings (a merger/owner requester's approval is written as the queue line the sweep parses; anyone else's as `/lgtm` + `/approve` comments attributed to them); it does not touch an item wearing `hold` (refused, nothing written — release the hold first if you want it decided); it never grants org membership, secrets or permissions (those items get `needs-owner` + `hold` and the one owner-action comment, and keep their park); and a question that is a policy or scope change to the hive itself (settings, governance docs, security posture — the `governance` label or those words in the park reason) is left parked: the delegate posts "hive recommends: …; owner to confirm" and keeps 085. A sentinel-flagged PR whose author lacks write access is likewise a recommendation only. Because the sentinel sweep re-applies its label when a hive account removes it, a trusted-author sentinel approval posts the "reviewed, intended" comment and leaves the final queueing to the person who clears the label — the job's `notes` say so.

**Audit marker.** Every comment the delegate writes ends with `<!-- hive:delegated-decision by=<hive-id> policy=owner-delegate -->` (and a visible "Decided by … as the owner's delegate under the owner-delegate policy (rule `…`)" line), so a delegated decision can always be told from a human one on GitHub, in the Decisions log, and in any later count.

**Setting.** `decisions.delegate.enabled` (default **true** — the same default as the other "let the hive do it" switches); `decisions.delegate.repos.<owner/repo>.enabled` (or the bare repository name) overrides per repository. `decisions.delegate.llm.enabled` (default **false**) adds the LLM step described below. When it is off the menu entry stays visible but the endpoint answers 403 naming the setting. See [operator-reference.md](operator-reference.md).

**What this release decides mechanically.** The decider is rules-first: the cases above that are mechanically decidable (dedupe, owner-only, flaky, CI ratchet, design proposal, review backlog, contributor-offline, sentinel-trusted, small-low-risk, reporter-trust, hive-policy) are decided from the item itself; anything else falls to the reversible default (approve direction on an issue; release a green hive PR; ask for the fix first when the head is red or conflicting). Competing-PR detection and a real adopt-supersede trigger are tracked in a follow-up issue linked from the PR that added this action.

**LLM step for the non-mechanical cases.** With `decisions.delegate.llm.enabled: true` (default **false**), an item the rules leave at the reversible default (rule `default-approve` or `default-release`) is shown to one *owner-delegate decider* turn: the item, its comment thread, sentinel findings, linked issues/PRs, the repository's auto-merge setting, the author's association and reporter-trust state, its labels, and `PolicyText` verbatim. The model answers with a structured `{verdict, text, rationale, actions[]}`; the reply is rejected unless every verdict, action kind and label is in the delegate's closed set and fits the item (no `approve-review` on an issue, no action on a park-keeping verdict, …), and unless the actions carry the verdict out: an acting verdict must include its recording action (`comment-decision`, or `close-as-duplicate` for the close verdicts, plus `approve-review` / `adopt-supersede` where the verdict names them), `labels-remove` can never name `stage/085-needs-decision` (only the recording action lifts the park, with the comment and the marker), and `close-duplicate` closes the newer item in favour of an older one. An accepted reply is recorded under rule `llm-delegate` and goes through the same safety rails and executor as a rule decision; a model error or rejected reply keeps the reversible default and the job's `notes` say why. Rules-first stays authoritative: every case a rule decides, and every item wearing `hold`, never reaches the model. The call uses the governor reviewer endpoint (`governor.trajectory`, falling back to `governor.litellm`); `decisions.delegate.llm.model` overrides the model.

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

**Settings that change it.** Your code hosting settings and optional service settings change it. The code hosting service is `project.forge`; a self-managed GitLab or a Gitea/Forgejo instance also shows its URL. See [Forges](integrations/forges.md).
