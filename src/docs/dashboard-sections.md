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
- Each total is written as a sum, for example `10 = 2 actionable + 0 held + 1 blocked + 7 outside`.
- **Actionable now** counts tracked items that Hive could work on now. Tracked means Hive put the item on its work list. Actionable means nothing is waiting on a person.
- Issues in the "Needs human" and "Confirm & close" bands are left out of Actionable now. A band is a display group named after what the item needs next.
- Pull requests in the "Needs human" and "Blocked" bands are left out, and so are drafts.
- **Held** counts issues and pull requests with a hold label. A hold label, such as `hold` or `on-hold`, parks an item on purpose.
- **Blocked / needs-human** counts work that waits on a person or on something else. For issues, that is the "Needs human" and "Confirm & close" bands. It also counts issues with a `needs-human` label and issues waiting for their reporter. For pull requests, it is the "Needs human" and "Blocked" bands.
- **Outside** counts open items that Hive's filters kept off its work list. Hover its ⓘ mark to see why each item was kept out. The reasons are labels such as `needs-direction`, `needs-decision` or `needs-spec`, and exempt labels. Others are reporter triage, your project issue filter, standing advisory issues and bot dependency dashboards. Draft pull requests count here too.
- The last part of Outside is **hold-adjacent/other**. It is whatever is left after every named reason is counted. Hive cannot say more about these items.
- The Outside tile exists on the v5 line only. On v6 the same items are part of the totals but have no tile of their own.
- **Issues by band** groups tracked and held issues. The bands are Unclaimed, Claimed, Needs triage, Needs human and Confirm & close.
- Unclaimed means nobody is assigned. Claimed means a person or a Hive worker took it. Needs triage means a Hive worker filed it and no person has approved it yet.
- Needs human means a label such as `blocked` or `needs-decision` asks a person to act. Confirm & close means a worker thinks the issue is already done.
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
- When your AI gateway reports its own spending, that figure appears too.

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
- The **needs-human** count in the header counts open pull requests that need a person to review or decide. Needs-human means waiting on a person.

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

A searchable record of everything Hive has done, newest first.

**What it tells you.** Every action Hive takes is written to the audit log. This section lets you read and search it.

**How the numbers are worked out.**

- The collapsed summary counts today's events.
- The search box matches any of the words you type. Wrap text in slashes, like `/merge.*failed/`, to search with a pattern.
- The match count shows how many events match.

**What it is good for.** Use it to answer "what did Hive do, and when?". Save searches you use often with **Save**.

**Example.** You search `merged` and see that Hive merged 4 pull requests this morning.

**When it appears.** Always. People without enough access see an error instead of events.

**Settings that change it.** How long events are kept depends on the audit log settings. See the [Audit log guide](audit-log.md).

## Review Queue

Every open pull request in your projects, in the order Hive suggests reviewing them, with the reasons for each position.

**What it tells you.** It ranks open pull requests from all your repositories in one list, whoever wrote them.

**How the numbers are worked out.** Hive scores each pull request by its review priority and history, then sorts them. The header shows how many are in the queue. The order is the same every time for the same data.

**What it is good for.** Review from the top when you have time. The reasons tell you why an item is high.

**Example.** The first pull request is small, its checks pass and it has waited 3 days. It is quick to review.

**When it appears.** Always. It is empty when there are no open pull requests.

**Settings that change it.** The review settings in **Settings** change the ranking rules. See [Review queue triage](review-queue-triage.md).

## Strategy Lab

An experimental planner that tries out changes to how Hive works and records what it learned.

**What it tells you.** The Strategy Lab runs small experiments on Hive's own settings. It shows its goals, its plan and a ledger of results.

**How the numbers are worked out.** The ledger lists each experiment and its result. The section has no other numbers.

**What it is good for.** Try it only if you want Hive to tune itself. You approve or stop each experiment.

**Example.** The lab proposes a faster schedule for one worker for a day. You approve it, and the ledger records the result.

**When it appears.** Only when the Strategy Lab setting is on and the autonomy level is L4 or above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** `dashboard.strategy_lab: true` turns it on. See [Strategy Lab](strategy-lab.md).

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
- A **Platform** tile shows which code hosting service is connected and which optional services are on.
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

Short answers to the questions people most often ask when they start using Hive.

**What it tells you.** It answers common questions about levels, advice, contributors, cost and where to get help.

**How the numbers are worked out.** This section has no numbers.

**What it is good for.** Read it in your first week. Each answer links to a fuller guide.

**Example.** You wonder why Hive does not merge anything. The FAQ explains that merging starts at a higher level.

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
