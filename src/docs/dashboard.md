# Spoke dashboard

## Base preparation visibility

The Activity Monitor shows a compact **Preparation train** pill directly above
the actual tracker items and the original train's thin gold side rail through
their rows. Existing train styling and theme tokens supply its spacing, border,
markers and font. A white marker identifies an exact-current preparation owner;
an outlined marker identifies unverified observations. Neither changes CI /
Review chevrons or creates a ready or native queue seat.

Subdued comments inside the corresponding PR row retain the current pipeline
caption and add base-refresh context, known ticket age, and the hosting issue
when nested. Redundant preparation-owner and waiting-behind badges are suppressed
only where that inline context is actually rendered. Other review, CI, hold and
ready-train signals remain unchanged. The gold ready-queue popover does not
duplicate preparation information; native observations stay separate.

Preparation FIFO follows first qualifying admission, not PR creation time or
number. Sorts, filters, pagination, collapsed repositories and linked issue trees
are unchanged: a rail ends before unrelated or backwards-ordered items, with a
continuation pill when the group resumes. A dashed rail marks missing FIFO
members or linked-child order that differs from FIFO. Mixed issue trees mark
only the actual preparation children, without moving or copying them.

Keyboard-focusable and hoverable node/header help provides exact identities,
snapshot time when known, admission versus current heads, unverified positions,
and off-page/filtered members. Unknown predecessors prevent confirmed waiting
ordinals. Unreadable snapshots explicitly say unavailable, not zero work.
Omitted preparation policy adds no train; inactive tickets are excluded.

These are read-only lifecycle observations, not review approval, green exact-head
CI or guaranteed merge order. Refreshed heads receive no old-head proof. The
existing tracker cache and refresh paths update comments and rail membership;
the view adds no polling, animation, admission, expiration or promotion.

The spoke dashboard is the operator UI served from
`src/pkg/dashboard/static/index.html`. That file is **generated**: edit the
per-area sources under `src/pkg/dashboard/static-src/` and run `make
dashboard-gen` (see [Dashboard source layout](dashboard-source-layout.md)). For the behavior behind the labels the repository cards show or mutate, see [Hive Labels and Control Signals](labels-and-control-signals.md). Its FAQ panel (`#faq-section` /
`#faq-panel`) is intentionally static HTML: it is not ACMM-gated, does not
fetch data, and is visible to confused L1/L2 users before they understand the
rest of the UI. Its only script is a client-side filter box (`faqFilter`) that
hides non-matching questions.

The FAQ is a summary surface for the current v5 hive. Each question is a
collapsible `<details class="faq-q">` with a `<summary>`, grouped under
getting started (L1/L2), the L3-L6 trust ladder (including `hold` labels and
shadow mode), evidence/releases/upgrades, runs/inception/Experiment Lab,
contributors/relays/models, issue claims/attribution/audit log,
cost/cadence/pausing, and where to get help. Each answer links back to the
relevant `src/docs/*.md` source of truth rather than becoming a second spec,
and a footer points to Discord, the in-dashboard feedback form and the issue
tracker when an answer is missing. `pkg/dashboard/faq_ui_test.go` guards the
structure: section ids, one `<summary>` per question, at least one link per
answer, the filter input, and no inline `style=` inside the panel (styling is
the token-based `.faq-*` class set).

Contributor cards use a client-side “album cover” tint: the dashboard samples each GitHub avatar in a tiny canvas, ignores low-saturation/near-black/near-white pixels, picks dominant and secondary accent colours, and applies them only as subtle background, border, avatar-ring, and role-pill washes. When avatar sampling fails, a deterministic hue derived from the login keeps the card distinct; results are cached in browser storage for seven days.

When the FAQ names a dotted config key inside `<code>...</code>`, keep it in
sync with the Go config schema. `pkg/dashboard` has a guard test that extracts
those keys from the FAQ panel and asserts each path exists in `config.Config`
via YAML tags.

## Section help

Every dashboard section title has a small **?** mark. Hovering or focusing it shows a one-sentence summary; clicking it (or pressing Enter) opens that section's entry in [Dashboard sections explained](dashboard-sections.md), the plain-language help page for operators. The help sentences live in one table in `index.html` (`DASHBOARD_SECTION_HELP`), and `dashboardDocsHref` is the single place that builds every help address: a hive built from the `v6` branch or a `v6.x.y` tag opens the `v6` page, anything else opens `v5`. A guard test fails when a section has no help entry or its sentence drifts from the page.

Settings fields use the same **?** mark, rendered by `settingHelpMark(page, anchor, label, tip)`: hovering or focusing it shows the setting's tooltip, and clicking it opens the guide that explains the setting, or the tab's entry in [Dashboard settings explained](settings.md), through the same `dashboardDocsHref`. Clicking the mark never changes the setting.

## Design system

Dashboard UI changes should follow the shared [dashboard design system](dashboard-design-system.md), [dashboard glossary and sidebar IA](dashboard-glossary.md), and [ADR-0018](adr/0018-dashboard-design-tokens.md). The token layer is the theme contract for future user theme/background work and the migration path away from static inline styles; `go test ./pkg/dashboard/... -run StyleRatchet -v` ratchets inline styles and raw CSS values so the debt only goes down.


## Shared dashboard cards and pinned notices

Top-level dashboard sections use the shared section-card shell for their header, border, collapse state, badges, and collapsed summaries. Notices that must stay above the reorderable dashboard — release channel/upgrade status, install/configuration warnings, and the planning intro — live in the pinned `#dashboard-notices` anchor before Overview so browser-local section reordering cannot move them down the page.

On the v5 line the standalone Platform section is not part of the default dashboard layout; on the v6 line Platform is a top-level section. On v5 its forge, mint-token-service, and skills facts now appear in **Diagnostics → Platform**, keeping the information available without consuming a top-level card. Diagnostics also includes **Quality stats**, a data-driven card rendered from the `quality` agent's configured Stats entries rather than a fixed set of deployment-specific workflow checks. The topbar health dropdown is limited to real spoke health checks from `deepHealth`, so repository workflow stats do not appear there.

## Topbar and sidebar status

The light dashboard topbar keeps high-signal operational state only: project name, fleet controls, health, auth, and a compact ACMM autonomy chip such as `L5 · Semi-Autonomous`. The chip is display-only and navigates to the ACMM Eval section; changing levels remains in the existing ACMM dialog/sidebar controls.

Build/version details live in the bottom-left sidebar chip. The collapsed chip shows the short SHA, release channel, and an orange `↑` marker only when an upgrade is available. Open the chip for the full commit link, channel/tracking state, compare/release-notes links, last upgrade status when reported, copy-version, and the manual upgrade action. On small screens the sidebar is reachable through the hamburger drawer, so the version menu remains available without returning the long version strip to the topbar. On the narrowest phones (≤360px wide) the topbar also drops the 🐛 feedback and ⚙ Settings chips so the row never overflows; both stay reachable from the account (avatar) menu, and feedback from the drawer as well.

## Übersicht widget

The account/avatar menu's **Export → ⬇ Widget** row downloads `GET /api/widget`
with the same dashboard authentication as the rest of the API. The compact
payload mirrors the headline dashboard readouts: agent names and
running/paused/busy/next-kick state, governor mode, ACMM level, actionable
issue/open PR counts, 7-day PR throughput (opened/merged/closed), spoke version
and upgrade availability, and the fleet breaker state.

## Reorder sections

The main dashboard layout is browser-local and display-only. Use the `⠿` grip in each top-level section header to drag a section, or focus the grip and press Space, Up/Down, then Space/Enter to drop; Escape cancels the keyboard move. The working order is auto-saved in `localStorage` as `hive.dashboard.layout`; ACMM-hidden sections keep their slots, feature-disabled sections are excluded, and the sidebar follows the saved order. Section collapse state, hidden sections, and repository card display tweaks are captured with saved layout presets; sidebar jumps temporarily peek collapsed sections open and restore them on the next different jump unless the operator manually keeps the section open. Open the GitHub avatar menu's **Layout** section to save the current working layout as a named preset (default **My layout**), apply/rename/delete up to five saved layouts, export/import a JSON layout, or **Reset layout to default**. Reset asks for confirmation, clears the working layout on this device, and offers a short undo toast. The avatar shows a small dot whenever the working layout differs from the last applied preset or default.

## Default collapsed cards

A browser that has never stored any dashboard state (a new hive, or a first visit from a fresh profile) starts with the non-critical cards collapsed so a new operator first sees the cards that say whether the hive is alive and merging: **Overview**, **Governor**, **Projects**, **Review**, **Approvals**, **Agents** and **Agent activity** open expanded; **Throughput**, **Tokens**, **Cost**, **Advisory**, **Knowledge**, **Contributors**, **Inception**, **ACMM Eval**, **Diagnostics**, **Audit Log**, **Agent Logs**, **Experiment Lab** and **FAQ** start collapsed with their one-line summaries visible. The set is the `DASHBOARD_DEFAULT_COLLAPSED_SECTIONS` constant in the dashboard page. A browser that already holds dashboard state when it first loads this version is marked `legacy` in `localStorage` (`hive.dashboard.collapseDefaults`) and keeps the historical all-expanded default, including after **Reset layout to default**. An operator's explicit choice always wins: expanding a default-collapsed card stores `hive-section-collapsed-<id>=0`, collapsing any card stores `1`, and saved presets, JSON export/import and reset round-trip those values. The avatar menu's **Layout** section also offers **Expand all sections** and **Collapse all sections**.

To hide a dashboard section, hover or focus its left-menu row and press the small eye-off control. The row remains in the sidebar in a muted state, while the matching dashboard card is removed from the main layout. The same control changes to **Unhide <section>** with an eye icon; choosing it, or clicking the hidden row, restores the card in its saved order and jumps to it. Hidden sections are stored locally as `hive.dashboard.hiddenSections`; Overview cannot be hidden.

The Experiment Lab (`dashboard.experiment_lab`) is hidden by default while
that surface is being reworked. Set `dashboard.experiment_lab: true` to show
the Experiment Lab dashboard section, its sidebar navigation item, and its
Nous status controls. The pre-rename key `dashboard.strategy_lab` is still
honoured as a deprecated alias for one release: it enables the panel, logs a
one-line deprecation at config load, and is folded into
`dashboard.experiment_lab` so the next config save writes the documented key.

## Governor card

The dashboard **Governor** card summarizes queue depth, operating mode, budget
posture, and cadence controls for the hive. Its collapsible **PRs by model** nested
sub-section reads `GET /api/governor/pr-models` with the selected `7d`, `30d`,
or `all` window. The nested header keeps the window/sort toggles visible and its
collapsed summary shows the current top-ranked model plus model count.

The Governor **actionable issues** and **actionable PRs** tiles use the shared
server-side actionable partition described in [Repository card legend, issue
bands, and PR bands](#repository-card-legend-issue-bands-and-pr-bands). Their
subtitles show the per-kind reconciliation from total open work to the
actionable split, and those two results always add back to the Overview
**Actionable now** total.

Governor settings **Features → Auto-Merge** includes the owner-only
**Trusted-author auto-merge** block. It edits
`auto_merge.trusted_authors.{enabled,repos,require_role,require_github_permission,exclude_labels}`
through `/api/config/auto-merge`, with a watched-repo multi-select (no
selection means all watched repositories), role selector, GitHub permission
switch, and excluded-label list.

Governor settings **Features → Review Gate** includes the owner-only
**Only review labelled PRs** toggle with a comma-separated label list. It
edits `review.require_labels` through `/api/config/review`; on, the hive
reviews only PRs carrying one of the labels (enforced in the reviewer kick
list, the review dispatch planner and the hive-review relay — see
[review-swarm.md](review-swarm.md#only-review-labelled-prs-reviewrequire_labels)).
Off (the default) sends an empty list and every open PR stays reviewable.

The cadence table starts each agent row with the same `1`/`0` Agent power
rocker used in the agent settings panel, so owners can enable or disable an
agent from the table; disabled rows are dimmed, and read-only viewers see the
current state without an active control. Per-mode cells are read-only operator
overview cells; continuous modes render as an `∞ continuous` chip, and clicking
a mode cell opens the agent settings dialog to the **Cadences** form where the
operator chooses Off, Interval, or Continuous.
Rows keep the merged/open/closed PR-volume bar, then add compact effectiveness
columns from the same aggregation used by the contributor Operations **Most
effective models** panel: merged PRs, first-pass merge rate, verified-PR run
rate, failure rate, and completed-without-PR ("nothing to ship") rate. Models
that meet `HIVE_CONTRIBUTE_EFFECTIVE_MODELS_MIN_PRS` (default `5`) merged PRs
get rank badges. On `/contribute`, Operations-style column headers in Most
effective models, Rankings, and Fleet controls tier limits are keyboard-clickable
sort controls; the selected column and direction are saved in browser
`localStorage`, and a third click restores each card's default live order. The
default row order is effectiveness rank; operators can toggle back to raw PR
count without changing the selected window.

## Throughput

The Throughput panel is a top-level dashboard section with its own sidebar entry, listed under Overview and Governor; its subtitle uses the small muted dashboard subtitle style. For a plain-language summary see [Throughput](dashboard-sections.md#throughput).

The **Throughput** section (`pr-throughput-section`) summarizes
pull/merge requests and issues across tracked forges. It reads
`GET /api/pr-throughput` for selectable windows and repository filters, keeps
the historical `/api/pr-throughput` path and `pr-throughput-*` element IDs for
compatibility, and reports opened, observed merged, and observed
closed-without-merging terminal states.

Actor attribution splits created, reviewed, and merged/closed activity into
precise buckets:

- `hive`: changes merged or closed by this hive. This includes audited
  sweep/queue/relay/governor paths (`agent_pr_created`, `agent_issue_created`,
  `agent_pr_reviewed`, agent issue comments/claims, relay/sweep merges and
  issue closes) and forge-observed PR terminal events whose `merged_by` actor
  matches this hive's configured identity or GitHub App bot login when no audit
  path exists.
- `human`: any non-bot GitHub login, including maintainers who merge through
  `gh pr merge --admin --squash` or the GitHub UI.
- `other`: non-hive bot accounts only, such as Dependabot, Renovate, GitHub
  Actions, Copilot coding agents, and other `*[bot]` logins.
- `unknown`: terminal observations with no recorded actor/path. Unknown counts
  are kept in the actor matrix as unrecorded evidence but are excluded from the
  trend share math so missing attribution is never mislabeled as automation.

The merged-PR and closed-issue totals that feed the Cost section's
cost-per-merged-PR and cost-per-closed-issue figures (`/api/cost`
`merged_prs` / `closed_issues`) are not re-queried from `/search/issues` on
every collection cycle. `pkg/forgestate.Counts` increments them from
`pull_request closed` (merged) and `issues closed` webhook deliveries whose
actor matches the hive's author and reconciles them against GitHub search once
per `forgestate.CountsReconcileInterval` (one hour), overwriting the webhook
tally. A rate-limited or failed reconcile keeps the last good totals and marks
them stale: `/api/cost` reports `count_stale`, `count_stale_since` and
`count_source`, and the Overview tiles read their open issue / PR totals from
the ForgeState cache when it is ready, exposing `staleSince` on
`/api/status` and `countsStale` / `countsStaleSince` per repository so the page
shows "stale since" instead of zeros.

Issue data comes from the existing issue-request and claim-poller audit stream:
agent-created issues, agent comments/claims as triage/review signals, and
agent issue-close events. Historical human issue creation/review/closure is not
backfilled; the actor matrix and trend chart start accumulating as new audited
or observed events arrive.

The **Hive vs human** tile and expanded trend use the same selected actor role
as the dropdown. The large percent is hive-attributed events divided by
hive + human + other-automation events in the selected window, excluding
unknown actors. For **Merged**, the share covers PR merges plus issue closes;
for **Closed**, it covers PRs closed without merging plus issue closes. The
previous percent compares against the immediately preceding window of the same
length (for example, previous 24 hours for the 24h window).

## Settings → Knowledge → Connectors

The Knowledge card has a **🔌 Integrations** button that opens an in-app modal for Obsidian setup plus every registered `knowledge.connectors` type. The **Knowledge** tab of the governor config dialog also has a **Connectors** section listing every `knowledge.connectors` entry: name, type, layer, a status pill (`ok`, `syncing`, `error`, `disabled`, or `pending` before the first sync), last sync, pages/facts, and the last error (truncated, full text on hover). Owners also get **Sync now**, **Edit**, **Disable/Enable** and **Remove** per row plus **Add connector**; everyone else sees a read-only table. Add/Edit uses the styled hive dialog with type-specific scope fields and a **Validate** button; credentials are referenced by environment variable name or secret file path only. Saves go through the owner-only `PUT /api/config/knowledge/connectors`. See [Knowledge connectors](knowledge-connectors.md).

## Project Inception and Knowledge

Project Inception is branded as powered by Spektacular with a linked header pill and body note. Knowledge Base collapsed summaries show the total fact count (the same source as the sidebar badge); layer health stays inside the body with a short explanation that layers are knowledge-source scopes for facts.

## Audit Log

The Audit Log viewport defaults to a taller resizable panel. Browser-local height changes are persisted in `localStorage` under `hive.audit.panel.height`; the entry cap/search controls remain in the card header area above the scrollable table. The collapsed **sensitive** badge is actionable: it opens the card, applies the visible **Sensitive only** filter for the same 24-hour window as the badge count, and jumps to the first sensitive row.

## Hive Chat

The floating **🐝 Hive Chat** panel is a command-first dashboard assistant. Type
`/help` (or `help`) to render the command registry; the same registry drives
one-line help, `/help <command>` details, examples, and slash-command
autocomplete and argument hints. Current commands are `/help`, `/clear`,
`/search`, `/history`, `/retry`, `/edit`, `/who`, `/agents`, `/beads`,
`/prs`, `/governor`, `/knowledge`, `/jam`, and `/spek`.

`/prs` answers locally from the status snapshot (the same cached rows
`/api/prs` serves, so it costs no GitHub call): the open pull requests grouped
by repo, one row each — `owner/repo#N — title (@author · stage labels · age ·
✅/❌/⏳ CI · approved / changes requested · needs decision · hold · mergeable)`
— under the aggregate count, capped at 25 rows with an "… and N more" footer.
The remainder of the command narrows the list and every filter is a
conjunct: `waiting on review` / `needs review` (not a draft, no approving
review, no outstanding change request), `needs decision` (`stage/085-needs-decision` or
`stage/045-needs-decision`), `hold` (held set or a hold label), `failing ci`,
`mine` (the signed-in viewer) / `by <login>` / `@login`, `repo <owner/repo>` or
a bare `owner/repo` token (the short repo name also works), and `stale`
(untouched for 7 days) / `older than 3d` (`h`, `d`, `w`). Words the parser does
not recognise are reported and ignored, and the list is still printed.

The prompt behaves like a small terminal: `Enter` sends, `Shift+Enter` inserts a
newline, `Tab` completes slash commands, Up/Down cycle prior commands while
preserving the draft, `?` opens the shortcut overlay, `Esc` closes the overlay
or panel, and Cmd/Ctrl+K opens and focuses it. A collapsible left cheat sheet
groups clickable examples for agents, beads/issues/PRs, governor, knowledge,
jam, and Spektacular. Spek/Spektacular examples are enabled when `/api/version`
reports v6/edge; otherwise they are marked `v6/edge`.

Chat UI state is browser-local: transcript, command history, panel size,
maximized/docked mode, cheat-sheet collapsed state, command draft, preferences,
and shortcuts live in localStorage with bounded history sizes. `/clear` clears
the transcript; `/history clear` or the cheat-sheet control clears command
history. `/history <text>` searches command history and `/search <text>`
searches the transcript. `/retry` resends the last prompt, `/edit` restores it
to the draft, suggestion chips seed common prompts, and long/code-heavy output
is collapsible with copy buttons on fenced code blocks. There is no server-side
per-user chat persistence yet because dashboard user settings are not a
general-purpose cross-browser preferences API.

Agent questions are answered from the hive's own agent state (`/api/status`
→ `agents[]`), not by dumping the roster (#14409). Bare `/agents` (or "list
agents") prints the roster, with the block reason inline for every `BLOCKED`
agent. `/agents stuck|blocked|paused|running|idle|failed` filters it: blocked
agents show WHY (`blockedReason`: rate-limit cap, inference auth, policy
stand-down, crash loop, failed starts…) and since when (`blockedSince`);
paused agents show who paused them and why. `/agents <name> [status|why|last|
history]`, `/agents status <name>`, or plain "scanner status" / "status of
scanner" return only that agent's card — state, block reason + since, current
task, last completed turn and its outcome, last kick, last PR/issue acted on,
restart count; an unknown name answers `No agent named X; agents: …`. Natural
language such as "which agent completed last?", "what is scanner doing?",
"why is reviewer-2 blocked?", "when did scanner last run?" and "which agents
are idle?" goes through the same table-driven matcher. An agent question the
matcher does not recognise is handed to the configured chat responder with
the agent state attached as context; without a responder the chat says what
it does understand instead of printing the roster.

### How a message is answered

The server routes every message (`pkg/dashboard/api_chat_router.go`) into
one of three paths, and never discards a message's arguments:

- **Bare command** — `/agents`, `/prs`, `/beads`, `/status` with no
  arguments (or a one-word keyword such as `agents` or `prs?`) prints the
  deterministic summary: the agent roster, per-repo open PR counts, bead
  counts per store, the hive health line.
- **Deterministic argument handlers** — the `/agents` matcher above (and
  the `/prs` filters, #14408) answer the argument vocabularies they
  recognise without a model turn.
- **Grounded answer** — any other command with arguments or plain-words
  question (`/agents what should the hive do next`, `what's waiting on my
  decision`, `which PRs have failing CI`, `what version are we on`) is answered
  by one read-only model turn over a compact snapshot of the hive state the
  dashboard already holds in memory: agents (state, paused/blocked reason,
  awaiting-CI, last kick and outcome, last completed action, last error),
  open PRs (stage labels, review state, requested reviewers, CI and failing
  checks, mergeability, merge verdict, age), actionable issues (stage,
  priority, assignees, age), items parked for a decision, governor mode and
  next kick, system alerts, release/commit and the last landed upgrade. The
  snapshot is scoped to the command's dataset (`/prs …` carries no agents),
  capped at 40 items per list and 24 KiB overall. The model is instructed to
  answer from the snapshot only, to say *"I don't have that data"* rather
  than guess, to cite the agents and `owner/repo#N` items it used, and that
  chat cannot take actions. The turn runs on the governor reviewer endpoint
  (`governor.trajectory`, falling back to `governor.litellm`) with a 25 s
  timeout and at most two turns in flight per dashboard; each attempt writes
  one `chat_grounded_answer` audit line (scope, question, snapshot size,
  outcome). Nothing on this path can mutate hive state.
- **Fallback** — when grounding cannot run (setting off, no reviewer
  endpoint, timeout, backend error, too many in flight) a scoped question
  gets the command's usage naming the reason; it is never answered with the
  bare dump. Unscoped free text falls through to the configured
  `ChatResponder` when one exists, else the "guide agent unavailable"
  message.

**Setting.** `chat.grounded_answers.enabled` (default **true**) gates the
grounded path hive-wide; `chat.grounded_answers.model` overrides the reviewer
model for the grounding turn. See [operator-reference.md](operator-reference.md).

Markdown is rendered from escaped input only: bold, inline code, code fences,
links, GitHub-style `@mentions`, and `#issue` / `owner/repo#N` references are
decorated after escaping. Repeated or long `Status heartbeat` output is folded
to avoid burying the conversation.

`/who` reads `GET /api/presence`. Authenticated users see display-safe
usernames/display names, GitHub avatars for plain GitHub handles, and active vs.
idle state derived from the existing focus-aware presence heartbeat. A local
dashboard with no authenticated identity does not reveal other sessions and
shows only `local`.

## Repository card holds

Repository cards show held issues and PRs beside the actionable pills. A user
who owns the hive, owns the repository, or has GitHub `write`, `maintain`, or
`admin` permission on that repository can click the `⏸ Hold` chip to add a hold
or the `▶ Release` chip to remove one. The server always re-checks that
permission before mutating labels. Adding
a hold applies the hive's canonical `hive-pause/<hive-id>` label. The name
deliberately avoids the substring `hold` so it is matched exactly. Removing a
hold only removes the label(s) that are actually causing the hold
(`hive-pause/<hive-id>` and/or the generic hold labels such as `hold`,
`on-hold`, or `hold/review`). The hive-specific canonical hold is
`hive-pause/<hive-id>`; the former `hive/<hive-id>` provenance label is retired
(#12539) and the boot sweep removes it from open items. On upgrade, Hive writes
`/data/hive-hold-migration-<hive-id>.json`: audit-backed dashboard holds are
copied to the new label, agent-provenance-only items become actionable, and
ambiguous legacy labels stay held under `hive-pause/<hive-id>` for operator
review. The card-level `⏸ pause` / `▶ resume` control uses the same permission
rule.

## Repository card legend, issue bands, and PR bands

The collapsible **Overview** section above Projects summarizes the same
client-side issue and PR bands across the selected repository view. Its SVG
charts reuse the repository-card classifiers for actionable plus held
issues/PRs, so their totals match the visible band counters and respect the
Overview settings repo filter without a separate API call. A compact KPI strip
renders the partition as an equation: **Total open issues + Total open PRs =
Actionable now + Held + Blocked/needs-human + Outside**. This is the same
partition used by the Governor actionable issue/PR subtitles. **Total open
issues** and **Total open PRs** are forge totals for the selected configured
repositories from the existing scanner tick: open GitHub issues exclude pull
requests, while open GitHub pull requests include drafts. Their subtitles show
the per-kind terms without repeating the headline total. Both totals are the
size of one canonical per-refresh open-items set, `/api/status.openItems`
(server-side: every listed open issue and PR per repository with its bucket,
built from the enumeration snapshot, the ForgeState cache items and the
last-good copy — counted items, never a separately fetched total; a repo that
reports more than it lists surfaces the remainder as `unlistedIssues` /
`unlistedPrs` and a **+N not yet listed** badge instead of inflating the
total). The left-nav **Overview** `issues/PRs` pill is those same two tile
values, written by the same KPI render from the same `/api/status` snapshot,
and the Hive activity monitor lists exactly that set, so its header "N items
(I issues / P PRs)" equals the tile; its tooltip carries the full equation
and, after a refused GitHub enumeration or on a boot-restored snapshot, the
same last-good "stale · as of" note the KPI strip shows. It stays hidden until the status feed has answered and keeps updating
when the KPI strip is switched off. **Actionable now** is
the shared server-side count used by the Overview KPI and the Governor collapsed
split: open enumerated issues plus open enumerated PRs after hold/exempt
filters, excluding items in the waiting, done, draft, or blocked Overview
bands. The following tiles expose issue/PR splits such as `13 = 5 issues + 8
PRs`, and the operator glyphs between cards make the full strip read like
`41 TOTAL OPEN ISSUES + 17 TOTAL OPEN PRS = 13 ACTIONABLE NOW + 13 HELD + 10
BLOCKED / NEEDS-HUMAN + 22 OUTSIDE`. The `outside` tile is server-side and is
broken down in `/api/status` as `actionableNow.outside.breakdown[]`, with each
row naming the count, rule, current setting, and how to change it for this
hive. The rows cover hard-suppress labels (`stage/045-needs-decision`,
`needs-spec`; `needs-human` is counted under
blocked/needs-human), exempt labels from `governor.labels.exempt` plus permanent
exempt labels, reporter-trust triage from
`project.issue_filter.reporter_trust`, require-label filtering from
`project.issue_filter.require_labels`, standing hive advisory/meta issues,
bot Dependency Dashboard issues, draft PRs, exempt PR labels, and
hold-adjacent/other remainder rows. See [Outside buckets](#outside-buckets)
for the bucket-by-bucket rules and [labels-and-control-signals.md](labels-and-control-signals.md)
for the label semantics behind hold, exempt, and hard-suppress signals. Its
`actionableNow.issues` and `actionableNow.prs` splits always sum to
`actionableNow.total`. Operators can
view each Issues or PRs panel as a donut,
pie, horizontal bar, single 100% stacked bar, line/spark trend, or age
histogram. Every shape is still driven by the same band slices and
server-provided classifications. Hovering a chart element, an Overview legend
row, or a repository-card band header shows its rule from the Go band specs
carried in `/api/status` as `overview_bands`. Each actionable/held issue and PR
carries `band`, `signals`, `stale`, and `held`; the browser does not classify
labels or timestamps again. Full SSE updates carry the same fields.


### Outside buckets

The Overview and Governor `outside` ⓘ tooltip uses this server-side table. The
Settings → Labels panel repeats it as **Triage buckets: what counts as outside**
and exposes the editable rows.

| Bucket | Definition | Rule/config source | How to change |
| --- | --- | --- | --- |
| `needs-decision` (shown as "needs human decision") | Issue is parked for a maintainer ruling. | `stage/045-needs-decision` always (and the PR-only `stage/085-needs-decision`, which a pre-045 issue may still carry), plus labels in `project.issue_filter.hard_suppress_labels.needs_decision` / `.needs_direction` and the retired `needs-decision` / `needs-direction` spellings until the reconciler migrates them ([#12683](https://github.com/hivecommons/hive/issues/12683)). | Settings → Labels → Triage buckets, or edit `hive.yaml`. |
| `needs-spec` | Issue needs specification or acceptance criteria. | Labels in `project.issue_filter.hard_suppress_labels.needs_spec`; default `needs-spec`. | Settings → Labels → Triage buckets, or edit `hive.yaml`. |
| Exempt labels | Issue or PR carries a deny-list label and is never agent work. | `governor.labels.exempt` plus permanent labels such as `do-not-merge`. | Settings → Labels → Exempt labels, or edit `hive.yaml`. |
| Reporter triage | Reporter trust is enabled and the issue author is not trusted yet. | `project.issue_filter.reporter_trust` trusted associations/logins and untrusted required labels. | Settings → Labels → Reporter trust. |
| Project issue filter | Issue lacks every required allow-list label. | `project.issue_filter.require_labels`. | Settings → Labels → Required labels. |
| Standing meta/advisory | Hive's own advisory report or issue labelled `hive/advisory`; it is coordination state, not work. | Fixed scanner rule: advisory title/label. | Rename/close the advisory, or remove the advisory label if it should become work. |
| Dependency dashboard | Bot dependency dashboard control panel, not a concrete task. | Fixed scanner rule: Renovate/Dependabot-style bot author plus Dependency/Renovate Dashboard title. | File concrete child issues or close/rename the dashboard issue. |
| Draft PRs | Pull request is open but marked draft. | GitHub draft state. | Use GitHub's **Ready for review** action. |
| Hold-adjacent/other | Safety remainder for open items that did not match a named actionable, held, blocked, or outside classifier. | Fixed scanner fallback. | Check labels and scanner rules; add a dedicated classifier if the remainder is meaningful. |

The Issues and PRs panels link directly to `/api/overview/issues.csv` and
`/api/overview/prs.csv` by default, or to the matching `.json` endpoints when
JSON export is selected; non-empty legend rows add a `band` query filter and
repo-filtered views add one or more `repo` query filters.
Exports use the current cached snapshot and the same Go classifier as the
page, so a download reflects the latest server state even between page updates.
Automation can use the corresponding `.json` endpoints and optional `band`,
`repo`, `stale`, and `held` filters. Cookie sessions work directly; token-authenticated
pages include their API token in the download link's query string.

The Overview header's ⚙️ popover stores browser-local chart preferences under
`hive-overview-charts`: which chart types are in rotation, whether the carousel
is enabled, the 5-second to 5-minute interval, transition style, duration, donut
label mode, KPI visibility, export format, and the bounded
client-side line/spark history. The repo multi-select is stored separately under
`hive.overview.repos`, with All/None shortcuts and an Org shortcut when the
hive spans multiple GitHub organizations. The default remains donut-only with
the carousel off, a 30-second interval, fade transition, normal duration, KPI
strip on, CSV exports, and all repos selected.
Manual arrows and dot indicators are available even when timed rotation is off;
timed rotation pauses while the panel is hovered or the tab is hidden, and
reduced-motion users get instant swaps.

The **Projects** section uses a consistent card header grid: reorder grip, truncated repository name, status badges, labelled auto-merge switch, spacer, and actions. It also includes a compact, collapsible pill legend. It is
stored per browser in `localStorage` and uses the same pill classes as the cards,
so theme changes update the legend automatically. The legend lists the issue
and PR bands (rendered from the shared band table, each with its rule as a
tooltip), issue held pills, plan chips, hold/release controls, issue state
glyphs (`⛔`, `❓`, `👤`, `✓`, role badges, stale `🕒`) and PR states (`✓`,
`◐`, `⚠`, held `⏸`, failing CI `✗ CI`, conflicts `⑂`, stale `🕒`,
reviewed `💬`, auto-merge `🔀`, agent role badges, review-class badges
such as `FIX`, and the red `sentinel` pill). A PR that carries the configured
sentinel alert label (`sentinel.label`, default `sentinel-alert`, matched
case-insensitively) is drawn red on its card, status chip and main pill, whether
it is listed as held or open; red wins over the held, needs-human and merge
colours, and the row keeps its other chips. The sentinel flagged it for a
possible security override, privilege escalation or codebase damage, and Hive
will not merge it until a maintainer reviews it and removes the label. Trusted
authors normally get a sentinel comment rather than the label, so their PRs are
usually not red. The label name reaches the page through `/api/role`
(`sentinel_label`, every role); until it is known no row is drawn red. Repository cards also show a labelled auto-merge switch: it is effectively off below L6, switching the hive to L6 turns it on for every active repo, and owners may toggle individual repos afterward.

Actionable issue pills are grouped client-side for display only; enumeration,
holds, filters, ranking, and agent kick behaviour are unchanged. Bands are
named after the action the operator should take (or an explicit "nothing
needed"), not after how the dashboard classified the issue. Each issue appears
in exactly one band, while non-winning states remain as badges on the pill:

1. **Unclaimed** (`unclaimed`) — no other band matched: nobody is assigned,
   nothing claimed it, and no human gate applies. This does not by itself mean
   agents will pick it up.
2. **Claimed** (`claimed`) — a claim is a comment; the `stage/020-claimed` /
   `stage/030-pr-in-progress` label is a mirror Hive keeps in sync (the retired
   `claimed` and `hive/in-progress` spellings are still recognised for the
   migration cycle). The display band also recognises assignees and any
   `hive/claimed-by-*` label — the generic `hive/claimed-by-human`, an
   agent's `hive/claimed-by-<agent>` and the retired per-login spelling
   alike; the claimant's login is shown from the claim record, not the label.
3. **Needs triage** (`triage`) — an `agent/<role>` label and no human has
   acknowledged the proposal under #5117: no `approved-direction` label and no
   human assignee (the snapshot's `human_acknowledged`; a human *comment* only
   satisfies the PR-time gate and is not in the payload). Acknowledge it, or
   close it. Once acknowledged the issue falls through to Unclaimed/Claimed
   with the role badge still on the pill.
4. **Needs human** (`needs human`) — labels such as `blocked`, `stage/045-needs-decision`,
   `stage/085-needs-decision`, `2-discussing`, `Epic`, `needs-human`, or `needs-triage`. Note that the
   `needs-triage` *label* lands here, not in the Needs triage band.
5. **Confirm & close** (`close?`) — labels such as `hive/already-done`,
   `stage/040-covered-by-pr` (and any later mirrored PR stage; formerly
   `🐝 covered by PR` / `hive/covered-by-pr`), and `hive/likely-done`: an agent
   believes the work landed; verify and close.

Precedence is confirm & close → needs human → claimed → needs triage → unclaimed,
so a human gate beats an assignment and done beats all other display states.
Within each band, issues sort by `updated_at` oldest first. The issue breakdown
also shows `N no activity > 14d` for actionable issues older than the stale
threshold.

Operators can tune only the display taxonomy under `dashboard.issue_bands`:

```yaml
dashboard:
  issue_bands:
    waiting_labels: [blocked, stage/045-needs-decision, stage/085-needs-decision, needs-decision, 2-discussing, Epic, needs-human, needs-triage]
    done_labels: [hive/already-done, stage/040-covered-by-pr, "🐝 covered by PR", hive/covered-by-pr, hive/likely-done]
    stale_days: 14
```

These settings deliberately do not reuse `governor.labels.exempt`,
`contribute_skip_labels`, or `project.issue_filter`; those decide work
eligibility, while issue bands decide how the dashboard describes already
enumerated work. Linked-PR badges render from the `linked_prs` payload and are not inferred by the card.

PR pills are also grouped client-side for display only. Open and held PRs appear
in exactly one band; held PRs are no longer appended after the actionable list.
First match wins for classification:

1. **Needs human** — `needs-human`, held, `stage/085-needs-decision`,
   `2-discussing`, or configured `dashboard.issue_bands.waiting_labels`.
2. **Merge-eligible** — merge verdict `eligible`, or queued for Hive
   auto-merge.
3. **Blocked** — merge verdict `blocked`, conflicts (`mergeable: no`), or
   failing CI with failing check names.
4. **In review** — merge verdict `outstanding`, a recorded Hive review link,
   or a GitHub review decision (see below).
5. **Draft** — draft PRs.
6. **Open** — everything else.

The display order is needs human → merge-eligible → blocked → in review →
open → draft. Within a band, PRs sort by oldest `updated_at`, then review class
(`fix`, refactor/docs or unknown, `tests`), oldest `created_at`, and PR number.
The PR snapshot includes `updated_at` specifically so this order can match issue
band aging without extra GitHub API calls. The existing
`dashboard.issue_bands.waiting_labels` and `stale_days` settings are reused for
PR display; changing them does not alter enumeration, holds, review, or merge
queueing.

## Linked PR issue signals

Repository issue pills show a `🔗` badge when Hive has verified a pull request related to that issue. Open PRs apply `stage/040-covered-by-pr` (the retired `🐝 covered by PR`); merged PRs on still-open issues apply `hive/likely-done`. The badge's tooltip grades the claim: "closes via PR #N" (the PR carries a closing keyword, or Hive linked it from the branch name), "covered by PR #N (partial)" (the PR says `Refs #N` / `Part of #N` and merging it will not close the issue), or "merged via PR #N". These are pending signals, not resolution: the issue remains in the actionable list until GitHub closes it, GitHub reports the PR in `closingIssuesReferences`, or an operator confirms coverage.

The badge is derived from Hive's own PR-claim evidence — the same source that applies the label — not from GitHub's closing-link data, so it is present for every covered issue whether the claim is closing or partial, and whether the issue is actionable or suppressed into the card's "covered by PR" group ([#12144](https://github.com/hivecommons/hive/issues/12144)). One badge renders per issue; when several PRs cover it the closing PR is primary, the count is appended (`🔗×2`), and the tooltip lists the rest. The status payload exposes the evidence as `linked_prs: [{number, state, merged, url, closing, reference}]` plus `claim_context` on each actionable `github.Issue` and on each `claimed_by_pr` entry in `nonActionableIssues`.

## Review pipeline board

The **Review** section opens with a separately collapsible **Pipeline** tile that shows every open PR as a card in the column for its review stage, from `GET /api/review/pipeline`: unreviewed, reviewing, changes requested, fixing, human hold, approved (merged and abandoned columns stay empty until the API returns those PRs). Each card shows the PR link, an agent or contributor marker, time in stage, reviewer chips, P0–P3 finding counts, the fix-loop counter (red once it is within one of the cap) and the next-action link. Filter by repo, author kind or stage; column headers carry counts. A ⚡ marker on the card means a webhook push or review request triggered the review of the current head (`trigger: event`), ⏳ that one is queued; see [Event-driven dispatch](review-swarm.md#event-driven-dispatch). The board refreshes every 30 seconds and keeps its scroll position. The sibling **Queue** tile is separately collapsible too, ranking the same open PRs in Hive’s suggested review order from `GET /api/review/queue` without hiding the Pipeline tile. A contributor PR the supersession sweep detected as superseded by a merged PR is tagged **superseded** there, with a link to the merged PR, whether or not `supersession_sweep.close_contributor_prs` is on; only when auto-close is on does the tag also show when the grace window ends and the sweep will close it, otherwise it reads "close as superseded or keep open" with no countdown. Owners get **Close now** and **Keep open** (adds `hive/keep-open`, which the sweep never auto-closes). Each queue row reflects the PR's `stage` from the stage-label store ([labels-and-control-signals.md](labels-and-control-signals/stage-labels.md#stagestalled--nobody-is-working-this)): the `stage/stalled` label's tooltip reads "no activity for Xh — nobody is working this PR", a red **waiting on hive Xh** pill marks a PR past its stage dwell while the hive itself holds it (checks running or reviewer verdict pending — the reviewer lane, not the author, needs attention), and an **N hops, M back** pill carries the stage transition history (`stage.history[]`) in its tooltip, replacing the retired `stage/regressed` label.

## Hive activity monitor

The **Hive activity monitor** section (sidebar, Dashboard group; expanded by default) is the "where is my pizza" view of the hive: one entry per open issue and PR from `GET /api/stage-tracker`, each a title line over a thin tracker strip. Issues and PRs share one set of columns — `Open → Actionable → Claimed → PR in progress → CI / Review → Merge gate → ✓` — with the step names printed once in a column header. Covering PRs nest under their issue: an issue with no PR yet draws the whole strip like any other row (its lit step is Open/Actionable/Claimed with the steps ahead faint); once exactly one PR covers it the issue and PR collapse into a single entry: the issue's `#N title` on the left, `[author icon] #PR title` in the *PR in progress* column of the same title line, and one continuous strip beneath — Open/Actionable/Claimed passed, then the PR's stage drives the lit chevron through `CI / Review → Merge gate → ✓` (chips, tooltip and `since` come from the PR). With two or more covering PRs the issue's own strip stops at Claimed — all passed, nothing lit — and directly beneath, one indented sub-row per open covering PR (furthest along first) carries `[author icon] #N title` at the *PR in progress* column and that PR's own strip from there on with its own lit chevron, modifier chip, tooltip and `since`. A PR linked to an open issue in the list has no top-level row of its own (`nested_under`); PRs with no linked issue keep a full-width row. The *Pull requests* filter flattens the list (every PR a top-level row); *Issues* and *All* show the nested view. Only the current step is coloured (amber, named in words; the ✓ chevron turns green once merged). Whether it moves depends on its *motion bucket* (`stageTrackerMotion()` in `index.html`): **static** — a steady fill — for Open, Open · on hold (any hold label, whatever else is on the item), Actionable (010), Claimed (020), a PR in progress / Covered by PR (030/040) by a human author, Merged, and any stage the dashboard does not recognise; **active** — a gentle pulse — when the hive is acting (030/040 with a hive or clanker author, 050 review pending, 060 reviewed, 070 fixing, 074 adopted by hive, 080 merge gate, 095 pre-merge soak, 100 merge queue); **needs-human** — a slower amber→red breathe — when a person must move (015 triage needed, 045/085 needs decision, 071 needs author, 072 returned to contributor, 090 ready for a human merge, or any `stalled` / `blocked` / `needs-human` / `needs-decision` / `needs-triage` modifier). Under `prefers-reduced-motion` nothing animates and a needs-human step keeps a steady red tint instead; passed and upcoming steps are low-contrast shapes with a small grey glyph each. Side stages ride the step they branch from — 070/071/072/074 light *CI / Review* as "Fixing" / "Needs author" / "Returned to contributor" / "Adopted by hive", 085/090 light *Merge gate* as "Needs human decision" / "Ready for human merge". The lit chevron ends with a small icon of whose move it is, in the chevron's ink (person = human, robot = clanker, hexagon = hive): the claimant on *Claimed*, the PR author on *PR in progress* and *Returned to contributor*, hive on *CI / Review* / *Adopted by hive* / *Ready for auto merge*, a human on *Triage needed* / *Needs human decision* / *Ready for human merge*; *Open* and *Actionable* carry none; 015 (an actionable issue no maintainer has triaged yet) and 045 light *Actionable* as "Triage needed" / "Needs human decision"; 040 shares *PR in progress* with 030, 060/080/100 light *Merge gate* as "Ready for auto merge" (the chevron tooltip distinguishes "not yet admitted — reason" from "admitted, merging") (the chevron tooltip names the exact stage). Every PR stage has a word: 050/060 "CI / Review", 070 "Fixing" (the author's move — a `needs-rebase` chip beside it says *what* is being fixed; the stage labeller always puts a needs-rebase PR at 070/071/072, never 050/060), 071 "Needs author" (a human-authored PR the author must act on), 072 "Returned to contributor" (a clanker PR handed back to its hive), 074 "Adopted by hive", 080/100 "Ready for auto merge", 085 "Needs human decision", 090 "Ready for human merge". A PR with no linked issue is named exactly once: its title line reads `[kind icon] [author icon] #N title` on the left (no entry in the *PR in progress* column) over one continuous strip with Open/Actionable/Claimed passed and its own stage lit; the `[author icon] #N title` entry under the *PR in progress* header appears only for covering PRs (single-PR entries and nested sub-rows). The author icon is coloured by `author/*` kind (hive blue, clanker purple, human teal). A PR is never an issue in this list: when the same number arrives through both the issues feed and the pulls feed, the PR record wins and the issue-feed copy is dropped server-side (pairing is keyed by repo, kind and number, so a PR can never be its own parent), and the *Issues* filter never shows one. A `since` column on the title line gives time on the current step (red when stalled), with at most one muted health-modifier chip to its left; the reason sentence, actor, nudges, flags and the last stage hops live in the hover tooltip. Titles never wrap: an issue title is cut with an ellipsis before it would reach the *PR in progress* column (leaving a clear gap, so the PR entry always starts at the column's left edge) and a PR title before the chips and `since`; the full title is on the title element's own hover tooltip, separate from the strip's reason tooltip. Rows are sorted furthest-along first by default (a covered issue ranks by its furthest-along PR, not its own 040), then longest-waiting first; the *Sort* select also offers longest waiting, newest and number, with an arrow button that reverses the direction. The list is complete: every open issue and PR of the watched repos is an entry — including claimed, in-progress, needs-human / needs-decision / needs-triage-filtered and reporter-held issues, draft PRs and PRs carrying an exempt label — and an item with no stage yet sits at *Open*. The toolbar filters the cached payload in the browser: *All / Pull requests / Issues*, *Repo*, a search box (`#number`, number or title substring, also matching a covering PR), *Since* (any / over 1 hour / over 1 day / over 1 week on the current step, or *past the stage dwell* = stalled or longer than the step's configured dwell), *Author* (human / clanker / hive) and a *Needs attention* toggle; a row of label chips built from the labels present (`stage/*`, `needs-*`, `author/*`, `role/*`, then any other label shared by two or more items) narrows further — chips of one family OR together, families AND together. *Clear filters* appears whenever something is narrowed. Filter, sort and page size persist in the browser (`localStorage`); the page does not. Pages hold 25 / 50 / 100 rows (default 50) with Prev / Next above and below the list and a "showing X–Y of N (M filtered out)" line; the section header reads "N items (I issues / P PRs) · M need attention" for the whole list (N, I, P come from `open_issues` / `open_prs` in the payload — the same `openItems` set behind the Overview tile, so N equals the tile's total open issues + PRs — with "· X match filters" appended while a filter is on) while the sidebar badge shows the same N (total open items, same render pass — never a separately computed number) with the needs-attention count (modifiers present, or on 071/072/085/090, counted over every item) in its hover tooltip and as the warn-coloured header suffix. The meta line also says "updated Ns ago · cache|fresh": the payload is built server-side at most once per `dashboard.stage_tracker.cache_ttl_s` (default = `governor.eval_interval_s`) from the spoke's existing GitHub snapshot and served with an `ETag`, so the 30-second poll costs a 304 and never a GitHub call (it turns amber once the payload is older than two TTLs). A GitHub webhook naming a listed PR or issue re-reads just that item (one `GET`, debounced by `dashboard.stage_tracker.webhook_refresh_debounce_ms`, default `2000`; off with `dashboard.stage_tracker.webhook_refresh: false`) and patches its row within seconds, ahead of the next eval cycle; see [Webhook-driven PR cache invalidation](operator-reference.md#webhook-driven-pr-cache-invalidation) and, for how that patch relates to the cadence tick and the reconcile that overwrites it (and what `/api/status.webhooks.mode` means when it reads `polling`), [webhooks.md](webhooks.md). Section details: [dashboard-sections.md](dashboard-sections.md#hive-activity-monitor); stage semantics: [labels-and-control-signals.md](labels-and-control-signals/stage-labels.md#stage-labels-stagennn--the-pipeline-state-machine).

**Sounds (opt-in).** The speaker chip at the right end of the toolbar (🔇 off / 🔊 on; default off, remembered per browser under the `localStorage` key `hive.tracker.sound`) enables two short sounds, both synthesised in the browser with the Web Audio API (no audio file, no network request). The **merge zap** — just under half a second of bug-zapper arc: a ~4 ms high-voltage "snap", then a sputtering electric sizzle (bright noise and a 240 Hz square chopped by a 120 Hz mains pulse train, dropping out and re-striking at random) that holds ~80 ms and dies away, high-passed at 700 Hz — plays when a pull request the tab was already showing merges while the page is open. The trigger compares tracker payloads: a PR counts as merged when its stage reaches the ✓ cap or when its key appears in the payload's `recently_merged` list (the hive's `pr_merged` audit entries of the last 30 minutes — sweep, queue, relay and human merges the PR poller observed alike — which is how a PR that simply dropped out of the open list is told apart from one closed unmerged). Entries that name the repo by its configured short name are qualified to the `owner/name` the rows carry, and a PR that drops off the list a poll before its audit entry lands is still remembered as open until `recently_merged` settles it, so the two never have to arrive in the same payload. It never fires on the first load, for rows that were already merged, for issues, or for a PR it has already zapped; several merges in one refresh play at most two zaps. The **new-item buzz** — a quieter 0.18 s sawtooth near 120 Hz with a ~30 Hz tremolo through a lowpass — plays when an issue or pull request appears on the list that the previous payload did not have; it never fires on the first populated payload, buzzes once per item, and plays at most two buzzes per refresh. Nothing plays while the tab is hidden. Turning the chip on plays one zap then one buzz so you can hear both. The audio context is created on a user gesture and resumed if the browser suspended it; if the autoplay policy still holds it suspended when a sound is due, that sound is skipped and the next click or key press re-arms it. Detection runs over the union of every configured repo's rows, keyed `owner/name/kind#number`, so the sounds fire for each repo the hive governs. Either sound can be replaced from the card's ⚙ settings modal with a user-supplied file (WAV or anything `decodeAudioData` accepts, capped at 4 MB) stored in the browser's IndexedDB (`hive-tracker-sounds`); a stored file that fails to decode falls back to the built-in voice. The modal also carries the **Play sounds** master toggle and a 0–100 **Volume** slider (`localStorage` key `hive.tracker.sound.volume`) that scales the master gain of both built-in and uploaded sounds. Closed issues are not an event the tracker payload carries, so nothing plays for them.

The endpoint returns `stages` (the stage table with `short`, `branch_from`, configured dwell and `falls_to` — the lower same-kind stages a move to which is a regression per the stage engine, so the event line says "fell back to" only for those; 015 → 010 triage accepted, 085 → 080 decision made and 072 → 070 author resuming read "moved to"), `items` (one per open issue/PR represented by the status snapshot, including actionable and held items plus display-only non-actionable issues, drafts, and exempt PRs: `labels`, `created_at`, `updated_at`, `bucket`, `bucket_reason`, stage, `stage_since`, humanised `reason`/`caption`, `actor`, `modifiers`, `author_kind` + `author_kind_source` (`label`, `inferred`, or `registry` — the author has a contributor-hub profile on this hive, so their clanker pushed it; the row icon, tooltip and claimant all read this one value), `lane`, `tier`, `linked_pr`, `linked_prs` (number, title, url, author, author kind, stage, `stage_since`, `caption` and `modifiers` of each open covering PR — enough to draw the nested sub-row without a second lookup), `nested_under` (on a PR: the issue it is listed under), nudge info and the last eight `history` hops from the stage store) and `author_kind_labels`. It answers `{"status":"initializing"}` until the first scan.

## PR review and link signals

PR pills also carry GitHub's own review state and conversation evidence
([#8968](https://github.com/hivecommons/hive/issues/8968)). Every field is
display-only: no enumeration, review, hold, or merge gate reads any of them.

- **Review decision.** `protection.review_decision` (`APPROVED`,
  `CHANGES_REQUESTED`, `REVIEW_REQUIRED`), with `protection.approvals_given`
  and `protection.changes_requested_by`, comes from the one-per-repository
  GraphQL query the sweep already runs for branch-protection facts
  (`protectionCollector`, `pkg/github/protection_facts.go`). The pill shows
  👍 approved, 👎 changes requested (tooltip names the reviewers), or 👀
  review required. GitHub reports a null decision on a base branch that
  requires no review; the decision then stays unknown — it is never inferred
  — and the reviewer opinions GitHub did return are shown as opinions
  ("2 approvals on GitHub — no review decision", "changes requested by @x —
  no review decision from GitHub"). Any decision or opinion places the PR in
  the **In review** band unless a higher band already claimed it.
- **Requested reviewers and teams.** `requested_reviewers` (logins) and
  `requested_teams` (slugs) come from the list payload at enumeration time —
  no additional request — on actionable, held, and stale-draft PRs alike. The
  pill shows 👥; the tooltip lists them.
- **Conversation volume.** `comment_count` and `review_thread_count` are
  GitHub totals (`totalCount` only, no thread nodes) added to the same
  per-repository GraphQL request as the review decision, so they cost
  response size rather than an additional request. The pill shows `🗨 N`
  with the sum; the tooltip splits comments from review threads. Nothing is
  shown when both are zero.
- **Linked issues.** `linked_issues: [{number, title, repo, state, url}]`
  are the `closingIssuesReferences` GitHub reports for the PR: the first 10
  from the batch PR query when it answered for the repository (the default
  path), else the first 20 from that same per-repository review-signals
  request. Both producers fill every field. The pill's action cluster shows
  a `🔗` badge that opens the first still-open one (else the first); the
  tooltip lists all. This is the PR-side mirror of the issue column's
  `linked_prs` badge, and like it comes only from the snapshot — the card
  never infers or fetches relationships. The
  [activity monitor](#hive-activity-monitor) uses `title` and `state` to
  name a linked issue that is not itself listed in the snapshot. A reference
  that reaches the cycle with only its number — an older persisted snapshot,
  or an issue already closed by an earlier PR while a superseded PR's body
  still says `Fixes #N` — is completed after the review-signals pass
  (`EnrichLinkedIssueTitles`, `pkg/github/linked_issue_titles.go`): from the
  forgestate cache when it still holds the issue, else one targeted issue
  read per issue, remembered for 24 hours (a failed read for one hour, a 404
  for the full day), at most eight reads per cycle — never one per cycle per
  issue.

The extended request is tried first; a forge that rejects one of the added
fields (an older GHE) is asked the decision-only query instead, so the review
decision the merge-block wording depends on is never lost to a display
field. Stale drafts, which `EnrichCIStatus` never touches, receive these
signals through `EnrichReviewSignalsAndWorkPRs` — the same per-repository
query and no per-PR mergeability or check-run fetch, since a draft is not a
merge candidate. The same call stamps `linked_issues` onto the sidelined PRs
(drafts, exempt-labelled) in each repository's work details, so a draft whose
body says `Fixes #N` pairs with that issue in the activity monitor; a
repository with both stale drafts and sidelined PRs still costs one query, and
none when the batch PR query already answered.


### Settings → Labels → Reporter trust: clanker-requested

The Reporter trust block also controls the opt-in `clanker_requested` policy, which steers untrusted contributors to the [ClankeR relay](contributor-relay.md#steering-outside-prs-to-the-relay-clanker_requested):

- **Toggle** for `project.issue_filter.reporter_trust.clanker_requested` (off by default).
- **Label name** (placeholder `clanker-requested`) for `clanker_requested_label`; it must not be blank.
- **Comment addendum** textarea for `clanker_requested_addendum` (optional, max 1000 bytes).
- A live **preview** of the comment text, updated as you type.

`GET`/`PUT /api/config/governor` expose and validate `clankerRequested`, `clankerRequestedLabel` and `clankerRequestedAddendum`. See [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md#clanker-requested-policy-for-outside-prs).

## Appearance themes

Owners can choose a hive-wide dashboard theme in **Settings → Appearance** or by
editing the persisted config through `PUT /api/config/dashboard/theme`. The
config surface is:

```yaml
dashboard:
  theme: hive           # built-in id from /api/themes, or custom
  theme_overrides:
    tokens:
      "--accent": "#e0a33a"
    background:
      image: https://example.org/bg.svg  # https or data: URI only
      opacity: 0.08
      attachment: fixed
    custom_css: |
      .panel { border-radius: 2px; }
```

Built-ins are loaded from one YAML file per theme under `src/pkg/dashboard/theme/themes/`. Initial themes include `hive`, `hive-dark`, `hive-light`, `star-wars`, `dungeons-and-dragons`, `star-trek`, `cyberpunk`, `terminal`, `solarized-dark`, `nord`, and migrated contributor profile skins such as `contributor-violet-advisor`. Theme files can set `scopes: [dashboard, contributor]` so the same catalog drives dashboard Appearance and contributor profile styling. Tokens are
validated against the dashboard's `:root` CSS custom properties so typos fail
fast. Custom CSS is capped at 32 KiB, strips HTML/style-breakout characters, and
only permits `https:` or bounded `data:` URLs; inlined backgrounds are capped at
256 KiB. `/api/theme.css` serves the effective theme with an ETag and is linked
from the document head so the themed stylesheet is available before first paint.

### Settings → Appearance

The **Appearance** tab in Settings renders the built-in preset gallery with
swatches for background, panel, accent, and text colors. Hovering a card previews
it, **Apply** persists the preset, **Reset to preset** removes overrides, and
**Apply background/CSS** saves the background URL, opacity, honeycomb watermark,
and custom CSS textarea (with byte counter). The existing dark/light button asks
the theme API for the closest light or dark built-in variant and refreshes
`/api/theme.css` without reloading the dashboard.

Preset screenshots are committed in `src/docs/images/themes/` for the shipped catalog.

The contributor profile page uses the same theme catalog. Its former local profile
style numbers map to `contributor-*` theme ids, so existing browser-local choices
continue to work while new themes appear in the profile picker. Contributor CSS is
layered after shared theme variables and hive-wide custom CSS: theme vars → admin
Appearance custom CSS → contributor `?style=owner/repo/path.css@ref` stylesheet.


### How to add a dashboard theme

Create one YAML file in `src/pkg/dashboard/theme/themes/` and give it a unique
`id`, display `name`, original `description`, `author`, `dark` flag, `tokens`,
optional `background`, optional `custom_css`, and `fonts` stacks. The directory
README documents the full schema and guardrails. The theme loader embeds every
`*.yaml` file with `embed.FS`; adding a file automatically makes it appear in
`GET /api/themes`, `GET /api/theme.css?theme=<id>`, and Settings → Appearance.
CI runs `pkg/dashboard/theme` tests that parse every file, enforce unique IDs,
and reject unsupported dashboard CSS variables, so theme-only changes are a good
first issue when the palette is original and avoids logos, copyrighted imagery,
quotes, or bundled proprietary fonts.

## UI conventions: no native browser dialogs

Hive hub and spoke UI code must not call browser-native dialog APIs such as `prompt()`, `alert()`, `confirm()`, `showModalDialog()`, or native-styled `<dialog>.showModal()`. Use the themed in-app helpers instead (`hivePrompt`, `hiveConfirm`, `hiveAlert`/toast, or the contribute admin modal) so dialogs match the dashboard, are accessible, and do not block the whole tab. The ratchet tests `TestNoNativeBrowserDialogsRatchet` in `pkg/dashboard` and `pkg/hub` scan shipped UI sources and should be updated only to make the rule stricter.

## Settings → Compliance

The **Compliance** tab in Settings (merger or owner) shows how this hive's
configuration lines up with the selected compliance framework profiles. It
opens with a banner: Hive is not certified; the tab maps your configuration
to control requirements.

- **Framework profile** — tick the frameworks to evaluate
  (`compliance.frameworks`); saved by the footer **Save & close** through the
  owner-only `PUT /api/config/governor/compliance`.
- **Controls** — every control grouped by domain with its implementing
  setting(s), current → recommended value and a status pill. Owners get an
  inline toggle for boolean settings and **Apply recommended** per domain
  (a themed confirmation lists each change); other settings link to their
  owning tab with **Open setting**. Changes are staged in the owning section
  and written only on Save. Mergers see the tab read-only.

Details, including which settings can be toggled inline, are in
[Compliance controls](compliance.md#using-the-compliance-tab).
