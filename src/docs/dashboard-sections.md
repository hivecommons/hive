# Dashboard sections explained

This page explains every section of the Hive dashboard in plain words. It is written for someone running Hive for the first time.

To find where a section or a setting is on screen, see the [Dashboard UI map](dashboard-ui-map.md).

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
- **Tracking:** an actionable issue that was split into child issues and still has some open. The activity monitor lights its Actionable step as `Tracking · 3/4` (children done / total) instead of *Actionable*, and its caption names the open children: the scanner leaves such a parent to its children, so the time on the step is not scanner delay ([#16481](https://github.com/hivecommons/hive/issues/16481)).
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

<a id="lifecycle-timeline"></a>

## PR timeline

A timeline of recent pull requests, from when they were opened to when they were merged or got stuck.

In the Pull requests group of the left menu, after Review. Earlier versions called it Lifecycle Timeline. Full entry: [PR timeline](dashboard-sections/overview-and-metrics.md#lifecycle-timeline).

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

## Audit log

A searchable record of everything Hive has done, newest first. The collapsed “sensitive” badge opens a 24h sensitive-only view, matching the server-side risk count.

In the Troubleshooting group of the left menu, after Diagnostics. Full entry: [Audit log](dashboard-sections/workflow-and-agents.md#audit-log).

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

## Inception (spek)

Named terminal sessions running command-line AI tools inside Hive; everyone who can see one watches live and one person at a time holds the keyboard.

Sessions are named and grouped and open in the hive's shared spek workspace; the section replaced the removed Inception wizard. It is on by default; turning it off from Settings → Dashboard → **Inception (spek) sessions** (`dashboard.sessions.enabled: false`; `HIVE_SESSIONS_ENABLED=false` overrides) hides the section. The terminal pane follows the dashboard theme, light or dark and any theme preset, instead of showing a black terminal.

Full entry: [Inception (spek)](dashboard-sections/workflow-and-agents.md#inception-spek).

## Knowledge

The facts Hive has learned about your projects, where they came from, and how much Hive trusts each one.

Full entry: [Knowledge](dashboard-sections/workflow-and-agents.md#knowledge).

## Inbox

Voice recordings and dictations waiting for you to file them as an RFC or keep as knowledge.

The top of the section shows the address to send things to, `hive@<dashboard host>`, with a Copy button; every item can be deleted with **🗑 Delete**, or several at once from **Hive (n) ▾ → Delete…**. Under the source chips, a quieter row lists input methods: **Works today** (dictation, gaze typing and switch access, through **＋ Add → Quick note**) and **Neural input (coming soon)**; those chips only explain and connect nothing ([Input methods and the Inbox](inbox-input-methods.md)). Full entry: [Inbox](dashboard-sections/workflow-and-agents.md#inbox).

## Calendar

Meetings hive will join, about a minute before each starts: add one, approve or decline requests, or skip and cancel.

Right after Inbox in the left menu. Each row shows the title, organizer, start and end in your time zone, platform, next occurrence, source (email invite, added here, or Discord) and status. The top of the section shows the address to add to a calendar invite, `hive@<dashboard host>`, with a Copy button. The owner approves or declines meetings read-write users add, and can skip one occurrence or cancel; a live meeting links to its panel and an ended one to its notes. The menu badge counts meetings hive is in plus those awaiting approval, and while hive is in a meeting the top bar shows an indicator ("Betty is in a meeting") that opens the live panel, or this section when there are several. Full entry: [Calendar](meetings.md#calendar). Below it, **Drafts from meetings** lists the issues, RFCs, Spek sessions and knowledge notes people asked Betty for in a meeting; the owner or a read-write user edits, approves or rejects each one, and nothing is created before approval ([Asking Betty to do things](meetings.md#asking-betty-to-do-things)).

## Meetings

Meetings hive joins to take notes: send it to one, follow the live transcript, notes and research, or make it leave.

Owner only. The top of the section shows the address to add to a calendar invite, `hive@<dashboard host>`, with a Copy button, above the form that takes a link. Approve research past the per-meeting cap and answer Discord join requests here; settings are under Settings, Meetings, and the persona under Settings, Meeting attendee. Full entry: [Meetings](meetings.md).

## Customers

The people you build for: who each one is, what they are trying to get done, and how it is going for them.

In the Collaborate group of the left menu, after Meetings. Everyone who signs in has their own profile, shown first as **Me**. The hive owner and read-write users also see every other customer and a collapsed **Suggested** list; a read-only user sees only their own profile.

- **Customers** are dashboard users and people added by hand with **Add customer**. Each row shows the name, the tags that are on, the satisfaction level, how many goals are confirmed and how many suggestions wait for review.
- **Suggested** are people hive has seen in issues, pull requests, meetings and mail who are not customers. Hive keeps only a key, counts and a few references for them. **Accept** makes one a customer; **Dismiss** removes the suggestion for good and keeps no readable trace.

Opening a profile shows:

- **Goals**: what the person is trying to get done. Add one, or confirm, edit or reject the ones hive suggests. A suggestion shows the references that led to it. A goal counts only once a person confirms it, and a rejected suggestion is not offered again.
- **Satisfaction** (owner and read-write only): a level (Unknown, At risk, Neutral, Satisfied) with what led to it. There is no score. Hive may suggest a level; it never changes one a person set.
- **Tags**: click a tag to turn it on or off. A tag marked manual stays as set; **Reset to automatic** hands it back to hive.
- **Identities**: the logins, addresses and ids the person is known by. An identity a read-only user adds to their own profile is shown as unconfirmed, and does nothing, until the owner or a read-write user presses **Confirm**. The hive user identity cannot be changed.
- **Activity** and **Notes** (owner and read-write only). Activity also shows open "Same person?" questions: hive spots a suggested address or login that looks like one of this customer's. **Yes** makes it theirs; **No** closes the question for good. Nothing is linked until you answer. The person does not see the notes.
- **Graph**: **Show graph** draws the person, the things they touched and the other people on those things. Click a green dot to open that person. A read-only user's graph holds only themselves.
- **Merge** (owner and read-write): fold another customer into this profile when they are the same person. Identities, tags, goals and activity come across, and **Undo merge** gives them back exactly. Two dashboard users cannot be merged. **Split off** on an identity moves it, and the activity it produced, to a new customer.
- **Export** and **Forget** (owner only): **Export this person** shows everything held about them in a viewer you can copy from. **Forget**: deletes the person, their identities and their activity links, and stops hive rebuilding the profile.

Nothing in this section leaves the hive. Turn it off with **Settings › Features › Customers** (`customers.enabled`), which takes effect when the hive restarts. API: [customers](api-reference/customers.md). Design: [customers](design/customers.md).

## Contributors

The people who lend their computers or AI helpers to your projects, and how far Hive trusts each of them.

Full entry: [Contributors](dashboard-sections/workflow-and-agents.md#contributors).

## Diagnostics

Health checks for Hive itself, such as its connections, credentials and safety switches, to help you find out why something is not working.

Full entry: [Diagnostics](dashboard-sections/workflow-and-agents.md#diagnostics).

## Agent Logs

The raw text output of each worker, for following what it is doing line by line.

Full entry: [Agent Logs](dashboard-sections/workflow-and-agents.md#agent-logs).

<a id="agents"></a>

## Control panel

One card per worker with its status and buttons to pause, restart or configure it.

First entry in the Agents group of the left menu (earlier versions called it Agents and listed it under Admin). Below it sit Activity and the collapsible **All agents (n)** list, one row per agent with **＋ Add agent** and **＋ Group**; collapsing that list keeps Control panel and Activity visible.

The pause/resume control is owner-only but never absent: below the owner role, or while the role is still being checked, it renders disabled with the tooltip "Needs the owner role" or "Checking your access...". Cards and the detail panel repaint when the role settles. A paused sandboxed agent offers plain `resume` (no session to start). The cadence table shows a single `paused` chip (with its reason on hover) beside the agent name; the mode columns show plain `paused` text.

Full entry: [Control panel](dashboard-sections/workflow-and-agents.md#agents).

## Hive activity monitor

A progress strip per open issue and pull request showing which step of the pipeline it is on and who must act next.

Full entry: [Hive activity monitor](dashboard-sections/activity-monitor.md#hive-activity-monitor).

### Let hive decide

The monitor's decision feature has its own entry: [Let hive decide](dashboard-sections/activity-monitor.md#let-hive-decide).

<a id="agent-activity"></a>

## Activity

One line per worker showing what it is doing now, how long since it last acted, and which issues it has reserved.

In the Agents group of the left menu, after Control panel (earlier versions called it Agent activity). Full entry: [Activity](dashboard-sections/activity-monitor.md#agent-activity).

## FAQ

Short answers to the questions people ask once a hive is running, each linking to the guide that explains it fully.

Full entry: [FAQ](dashboard-sections/activity-monitor.md#faq).

## Runs

Longer pieces of work Hive is carrying out in stages, and which ones are waiting for you.

Full entry: [Runs](dashboard-sections/activity-monitor.md#runs).

## Platform

Which code hosting service Hive is connected to and which optional platform services are switched on.

Full entry: [Platform](dashboard-sections/activity-monitor.md#platform).
