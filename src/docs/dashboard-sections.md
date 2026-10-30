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

### Live sidebar count feedback

The Hive activity monitor, Contributors and Knowledge navigation pills briefly show an upward or downward arrow and the actual signed count change. The number remains readable and the link works as before. Reduced-motion preferences keep the feedback static; there is no extra polling or repeated announcement on unchanged refreshes.

The tracker’s gold pill counts default-view **rows** (issues, except folded review notes, plus PRs not drawn under an issue); its orange pill counts **waiting-on-you items**. These are not separate issue and PR totals. Contributors animates only **online** in online/registered, using the existing registered-profile heartbeat window, not connected sockets or changes in registered totals. Knowledge counts **facts**, the sum of layer `total_pages` values, when enabled; its disabled zero does not mean stored facts were deleted. If a remote layer is unhealthy, Knowledge still loads its available facts, marks the count as partial in its accessible label and tooltip, and suppresses delta feedback until a complete count is known again.

Initial or unknown counts and route/filter redraws do not animate. After reconnection, feedback describes only the observed net numeric change, not whether an item was merged, closed or otherwise removed.

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
- **Merge:** the one word for landing a pull request. *Let hive merge* (the lead) is you approving so Hive merges through its own App on the next tick once CI is green; *Merge now* is you merging at once as maintainer.

Hover text follows one budget: one sentence of state in plain words, at most 40 visible words, one recommended action, and everything technical (config keys, stage numbers, how to change a setting) behind a single collapsed **Details**. The same sentence is never shown twice on one surface. The per-builder word counts live in [dashboard-copy-inventory.md](dashboard-copy-inventory.md).

**Dialogs on a phone.** Any dialog with a long body — the **What changed** release notes after an upgrade, alerts, the export viewer, the Level 6 auto-merge notice, settings bodies — fills the whole screen on a phone: title and a ✕ at the top, the text scrolls in the middle, and the buttons stay fixed at the bottom. A soft fade at the bottom edge means there is more to read; it disappears when you reach the end. Esc, the ✕, the buttons and tapping the backdrop close it. On a desktop the same dialogs keep their usual size and only gain the fade and a thin scrollbar. See [Long-content modals](dashboard-design-system.md#long-content-modals-modal-scroll-and-the-mobile-sheet) for the convention.

## Overview

How many issues and pull requests Hive is tracking right now, and how many of them it can work on.

Full entry: [Overview](dashboard-sections/overview-and-metrics.md#overview).

## Governor

How busy Hive thinks your projects are right now, and how often it wakes its workers as a result.

Full entry: [Governor](dashboard-sections/overview-and-metrics.md#governor).

## PRs by model

Which AI models wrote the pull requests Hive opened, how well each model's work landed, and how the models rank against each other.

Full entry: [PRs by model](dashboard-sections/overview-and-metrics.md#prs-by-model).

## Advisory

Reports, advice and suggestions Hive has written about your projects and about how it is running.

Full entry: [Advisory](dashboard-sections/overview-and-metrics.md#advisory).

## Advisory Digest

The open findings Hive's workers have written down, such as bugs, ideas and advice, grouped by who wrote them.

Full entry: [Advisory Digest](dashboard-sections/overview-and-metrics.md#advisory-digest).

## Hive Advice

A short weekly list of things Hive recommends you do to keep work moving on your projects.

Full entry: [Hive Advice](dashboard-sections/overview-and-metrics.md#hive-advice).

## Fleet Report Preview

Problems Hive has noticed in itself that it would report to the Hive maintainers, and problems that have since gone away.

Full entry: [Fleet Report Preview](dashboard-sections/overview-and-metrics.md#fleet-report-preview).

## Ready to level up?

Whether Hive thinks you could safely let it do more on its own, and what still has to be true first.

Full entry: [Ready to level up?](dashboard-sections/overview-and-metrics.md#ready-to-level-up).

## Lifecycle Timeline

A timeline of recent pull requests, from when they were opened to when they were merged or got stuck.

Full entry: [Lifecycle Timeline](dashboard-sections/overview-and-metrics.md#lifecycle-timeline).

## Throughput

How many pull requests were opened, merged and closed over a chosen time window, and who did the work.

Full entry: [Throughput](dashboard-sections/overview-and-metrics.md#throughput).

## Tokens

How much text the AI models read and wrote for Hive recently, broken down by model and by worker.

Full entry: [Tokens](dashboard-sections/overview-and-metrics.md#tokens).

## Cost

What Hive's use of AI models would cost at public per-token list prices; an estimate, not a bill.

Full entry: [Cost](dashboard-sections/overview-and-metrics.md#cost).

## Projects

One card per repository Hive looks after, listing its open issues and pull requests grouped by what each one needs next.

Full entry: [Projects](dashboard-sections/workflow-and-agents.md#projects).

## ACMM Eval

How ready your repositories are for Hive to work on them alone, scored against a published checklist.

Full entry: [ACMM Eval](dashboard-sections/workflow-and-agents.md#acmm-eval).

## Approvals

Actions Hive wants to take that are waiting for you to approve or reject them.

Full entry: [Approvals](dashboard-sections/workflow-and-agents.md#approvals).

## Audit Log

A searchable record of everything Hive has done, newest first. The collapsed “sensitive” badge opens a 24h sensitive-only view, matching the server-side risk count.

Full entry: [Audit Log](dashboard-sections/workflow-and-agents.md#audit-log).

## Review

Review Pipeline shows pull requests by stage first; Review Queue lists the same open pull requests in Hive's suggested review order.

Full entry: [Review](dashboard-sections/workflow-and-agents.md#review).

## Review Pipeline

Each open PR in the stage the review pipeline derived for it (GET /api/review/pipeline), with reviewers, P0–P3 findings, the fix-loop counter and the next action.

Full entry: [Review Pipeline](dashboard-sections/workflow-and-agents.md#review-pipeline).

## Queue

Every open PR across the governed repos in one deterministic rank order (GET /api/review/queue), regardless of author type, with the reasons for each position.

Full entry: [Queue](dashboard-sections/workflow-and-agents.md#queue).

## Experiment Lab

An experimental planner that tries out code changes in your repositories and records what it learned.

Full entry: [Experiment Lab](dashboard-sections/workflow-and-agents.md#experiment-lab).

## Inception

A step-by-step guide that turns a new project idea into a first set of files and planned work.

Full entry: [Inception](dashboard-sections/workflow-and-agents.md#inception).

## Knowledge

The facts Hive has learned about your projects, where they came from, and how much Hive trusts each one.

Full entry: [Knowledge](dashboard-sections/workflow-and-agents.md#knowledge).

## Contributors

The people who lend their computers or AI helpers to your projects, and how far Hive trusts each of them.

Full entry: [Contributors](dashboard-sections/workflow-and-agents.md#contributors).

## Diagnostics

Health checks for Hive itself, such as its connections, credentials and safety switches, to help you find out why something is not working.

Full entry: [Diagnostics](dashboard-sections/workflow-and-agents.md#diagnostics).

## Agent Logs

The raw text output of each worker, for following what it is doing line by line.

Full entry: [Agent Logs](dashboard-sections/workflow-and-agents.md#agent-logs).

## Agents

Each worker Hive runs, what it is doing now, and buttons to pause, restart or configure it.

Full entry: [Agents](dashboard-sections/workflow-and-agents.md#agents).

## Hive activity monitor

A progress strip per open issue and pull request showing which step of the pipeline it is on and who must act next.

Full entry: [Hive activity monitor](dashboard-sections/activity-monitor.md#hive-activity-monitor).

### Let hive decide

The monitor's decision feature has its own entry: [Let hive decide](dashboard-sections/activity-monitor.md#let-hive-decide).

## Agent activity

One line per worker showing what it is doing now, how long since it last acted, and which issues it has reserved.

Full entry: [Agent activity](dashboard-sections/activity-monitor.md#agent-activity).

## FAQ

Short answers to the questions people ask once a hive is running, each linking to the guide that explains it fully.

Full entry: [FAQ](dashboard-sections/activity-monitor.md#faq).

## Runs

Longer pieces of work Hive is carrying out in stages, and which ones are waiting for you.

Full entry: [Runs](dashboard-sections/activity-monitor.md#runs).

## Platform

Which code hosting service Hive is connected to and which optional platform services are switched on.

Full entry: [Platform](dashboard-sections/activity-monitor.md#platform).
