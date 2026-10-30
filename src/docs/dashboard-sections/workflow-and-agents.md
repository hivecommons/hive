# Dashboard sections explained — Projects, review, experiments and agents

Part of [Dashboard sections explained](../dashboard-sections.md); see that page for how each entry is organised and for the words used on it.

## Projects

One card per repository Hive looks after, listing its open issues and pull requests grouped by what each one needs next.

**What it tells you.** Each card is one repository. Its issues and pull requests are sorted into bands. A band is a display group named after what the item needs next.

**How the numbers are worked out.**

- The cards use the same bands as the Overview charts. See the Overview entry for each band's meaning.
- The small label above each repository name is its code hosting service. On GitHub it is the host, such as `github.com`, with `· Enterprise` on GitHub Enterprise. On GitLab or Gitea/Forgejo (`project.forge`) it names the service and its host, such as `🦊 GitLab · gitlab.example.com`. The card's links then go to that host. The badge in the section header names the same forge when every card is on it, and shows `N forges` when a `project.repo_policies[].forge` override puts cards on different forges. Each repository in `/api/status` carries this as `forge` (`github`, `gitlab` or `gitea`) and, for GitLab and Gitea, `forgeUrl`. See [Forges](../integrations/forges.md).
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
- `project.forge` (or a repo's `project.repo_policies[].forge` / `forge_url` override) and the `gitlab:` or `gitea:` instance URL set the host label and links. See [Forges](../integrations/forges.md).
- The `dashboard.issue_bands` settings change the band rules.
- **Merge only on current base** ([#15335](https://github.com/hivecommons/hive/issues/15335)). A green `current-base` chip in the card header means the hive only merges a pull request there once its head contains the current base tip; a green head tested on an older base is updated with GitHub update-branch and merges when CI is green on the new head. The hive-wide default is the **Merge only on current base** switch under **Settings → Features → Auto-merge** (`auto_merge.require_current_base_default`, off by default; help text: "OFF by default. Before auto-merging in any lane, require the PR head to contain the current base tip; a green head tested on an older base is updated with GitHub update-branch and merges once CI is green on the new head. Costs one compare call per merge candidate. Per-repo overrides live on the repo card."). Each row under **Settings → Repos** carries the per-repo override — the **Merge only on current base** checkbox with an *inherits* marker, or an **inherit** button when the repo has its own value — written as `auto_merge.require_current_base` (on, also strict: a head the App cannot update is parked) or `auto_merge.skip_current_base` (off). `/api/status` carries the effective value per repository as `requireCurrentBase`.
- See [Repository card legend, issue bands, and PR bands](../dashboard.md#repository-card-legend-issue-bands-and-pr-bands).

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
- See the [ACMM policy matrix](../acmm-policy-matrix.md).

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

**Settings that change it.** How long events are kept depends on the audit log settings. See the [Audit log guide](../audit-log.md).

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

**Settings that change it.** The review settings in **Settings** change the loop cap, ranking rules and which repositories are included. See [Review queue triage](../review-queue-triage.md).

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

In Suggest/Evolve mode, a ready card says *ready — the experiment loop is not running on this hive* when the external loop has written nothing for 24 evaluation cycles. Hive itself designs no proposals. Check [The experiment loop](../strategy-lab.md#the-experiment-loop), rather than waiting for the card to advance by itself.

### Observation reference

There is no lab-wide progress bar. The whole-Hive snapshot count is a total, not progress toward one experiment. The muted **Data collection** note under the cards shows *Observation history: N snapshots since <date>*, what each snapshot records, the cadence (four an hour) and retention (24 hours raw, then hourly buckets for 30 days).

**What it is good for.** Try it only if you want Hive to propose code changes on its own. You approve or stop each experiment.

**Example.** You add the experiment "Can we cut CI time by 20 %?" scoped to `acme/api`; its card reads 0 / 672 and *collecting baseline*. A week later it reads *ready — generate proposals*; you press the button, the lab proposes splitting a slow test package across two CI jobs, you approve it, and the ledger records the result. Meanwhile a second card, "Fewer flaky tests?", runs on `acme/web` — but a third one on `acme/api` is refused until you pause the first or narrow its scope.

**When it appears.** Only when the Experiment Lab setting is on and the autonomy level is L4 or above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** `dashboard.experiment_lab: true` turns it on (`dashboard.strategy_lab` is a deprecated alias for one release). See [Experiment Lab](../strategy-lab.md).

## Inception

A step-by-step guide that turns a new project idea into a first set of files and planned work.

**What it tells you.** It walks you through describing a project. Hive asks questions, proposes a structure and creates the first files.

**How the numbers are worked out.** This section has no numbers.

**What it is good for.** Use it when starting a new project with Hive. Answer its questions, review the plan, then approve it.

**Example.** You describe a small web service. Inception asks two questions and then proposes a folder layout and five starter issues.

**When it appears.** Always.

**Settings that change it.** No settings change this section. See [Inception](../inception.md).

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

**Settings that change it.** Knowledge sources, shared sources and imports are managed inside the section. See the [knowledge curator guide](../knowledge-curator.md).

## Contributors

The people who lend their computers or AI helpers to your projects, and how far Hive trusts each of them.

**What it tells you.** A contributor is a person who signed up to help. Some connect their own AI worker. This section lists them and their trust level.

**How the numbers are worked out.** The sidebar pill and the section header read **online/total**, for example `3/12`: how many registered contributors are online right now over how many have registered. *Online* means the contributor's relay sent a heartbeat within the presence window — `governor.labels.stage.presence_window`, 15 minutes by default. It is the same test the tracker uses when it says *waiting on @name (offline 13m)*, so the two never disagree. Hover the pill for *N online · M registered (heartbeat within 15m)*. The list puts online contributors first; each card's dot is green when online and grey when offline.

**What it is good for.** Use it to give trusted people more access, or remove access. Share the `/contribute` link to invite people.

**Example.** A contributor has fixed 10 issues cleanly. You raise their trust so they can take bigger tasks.

**When it appears.** At autonomy level L2 and above. The autonomy level is how much Hive may do on its own, from L1 to L6.

**Settings that change it.** Trust and roles are changed on each contributor card. See [Contributor trust and roles](../contributor-trust-and-roles.md).

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

**Settings that change it.** Provider credentials, code hosting settings and the quality worker's stats change the tiles. See [Dashboard route and health checks](../health-checks.md).

## Agent Logs

The raw text output of each worker, for following what it is doing line by line.

**What it tells you.** It would show a worker's output as it runs. A worker is one of Hive's AI agents.

**How the numbers are worked out.** This section has no numbers.

**What it is good for.** Today you cannot open it. Use the terminal or log links on each card in the Agents section instead.

**Example.** To read the scanner's output, open its card in Agents and choose its log link.

**When it appears.** Never, on both v5 and v6. The dashboard always hides it.

**Settings that change it.** No settings change this section. See [Agent logging](../agent-logging.md).

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

- Each card's ⚙️ button opens its settings: schedule, model, tools and permissions. See [Agent configuration](../agent-configuration.md).
- `governor.claims.enabled` turns reservations on. When it is off, which is the default, the line shows nothing about reservations.
- `dashboard.agent_poll_interval_s` sets how often worker updates are sent. The default is 10 seconds.
