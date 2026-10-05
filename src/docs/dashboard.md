# Spoke dashboard

The spoke dashboard is the operator UI served from
`src/pkg/dashboard/static/index.html`. For the behavior behind the labels the repository cards show or mutate, see [Hive Labels and Control Signals](labels-and-control-signals.md). Its FAQ panel (`#faq-section` /
`#faq-panel`) is intentionally static HTML: it is not ACMM-gated, does not
fetch data, and is visible to confused L1/L2 users before they understand the
rest of the UI.

The FAQ is a summary surface for the current v5 hive. It groups answers by
getting started (L1/L2), the L3-L6 trust ladder, runs and inception,
contributors and relays, issue claims and GitHub footprint, cost/cadence/models,
and where to get help. Each answer links back to the relevant `src/docs/*.md`
source of truth rather than becoming a second spec.

Contributor cards use a client-side “album cover” tint: the dashboard samples each GitHub avatar in a tiny canvas, ignores low-saturation/near-black/near-white pixels, picks dominant and secondary accent colours, and applies them only as subtle background, border, avatar-ring, and role-pill washes. When avatar sampling fails, a deterministic hue derived from the login keeps the card distinct; results are cached in browser storage for seven days.

When the FAQ names a dotted config key inside `<code>...</code>`, keep it in
sync with the Go config schema. `pkg/dashboard` has a guard test that extracts
those keys from the FAQ panel and asserts each path exists in `config.Config`
via YAML tags.

## Design system

Dashboard UI changes should follow the shared [dashboard design system](dashboard-design-system.md), [dashboard glossary and sidebar IA](dashboard-glossary.md), and [ADR-0018](adr/0018-dashboard-design-tokens.md). The token layer is the theme contract for future user theme/background work and the migration path away from static inline styles; `go test ./pkg/dashboard/... -run StyleRatchet -v` ratchets inline styles and raw CSS values so the debt only goes down.


## Shared dashboard cards and pinned notices

Top-level dashboard sections use the shared section-card shell for their header, border, collapse state, badges, and collapsed summaries. Notices that must stay above the reorderable dashboard — release channel/upgrade status, install/configuration warnings, and the planning intro — live in the pinned `#dashboard-notices` anchor before Overview so browser-local section reordering cannot move them down the page.

The standalone Platform section is no longer part of the default dashboard layout. Its forge, mint-token-service, and skills facts now appear in **Diagnostics → Platform**, keeping the information available without consuming a top-level card. Diagnostics also includes **Quality stats**, a data-driven card rendered from the `quality` agent's configured Stats entries rather than a fixed set of deployment-specific workflow checks. The topbar health dropdown is limited to real spoke health checks from `deepHealth`, so repository workflow stats do not appear there.

## Topbar and sidebar status

The light dashboard topbar keeps high-signal operational state only: project name, fleet controls, health, auth, and a compact ACMM autonomy chip such as `L5 · Semi-Autonomous`. The chip is display-only and navigates to the ACMM Eval section; changing levels remains in the existing ACMM dialog/sidebar controls.

Build/version details live in the bottom-left sidebar chip. The collapsed chip shows the short SHA, release channel, and an orange `↑` marker only when an upgrade is available. Open the chip for the full commit link, channel/tracking state, compare/release-notes links, last upgrade status when reported, copy-version, and the manual upgrade action. On small screens the sidebar is reachable through the hamburger drawer, so the version menu remains available without returning the long version strip to the topbar.

## Übersicht widget

The account/avatar menu's **Export → ⬇ Widget** row downloads `GET /api/widget`
with the same dashboard authentication as the rest of the API. The compact
payload mirrors the headline dashboard readouts: agent names and
running/paused/busy/next-kick state, governor mode, ACMM level, actionable
issue/open PR counts, 7-day PR throughput (opened/merged/closed), spoke version
and upgrade availability, and the fleet breaker state.

## Reorder sections

The main dashboard layout is browser-local and display-only. Use the `⠿` grip in each top-level section header to drag a section, or focus the grip and press Space, Up/Down, then Space/Enter to drop; Escape cancels the keyboard move. The working order is auto-saved in `localStorage` as `hive.dashboard.layout`; ACMM-hidden sections keep their slots, feature-disabled sections are excluded, and the sidebar follows the saved order. Section collapse state and repository card display tweaks are captured with saved layout presets; sidebar jumps temporarily peek collapsed sections open and restore them on the next different jump unless the operator manually keeps the section open. Open the GitHub avatar menu's **Layout** section to save the current working layout as a named preset (default **My layout**), apply/rename/delete up to five saved layouts, export/import a JSON layout, or **Reset layout to default**. Reset asks for confirmation, clears the working layout on this device, and offers a short undo toast. The avatar shows a small dot whenever the working layout differs from the last applied preset or default.

The Strategy Lab (`dashboard.strategy_lab`) is hidden by default while that
surface is being reworked. Set `dashboard.strategy_lab: true` to show the
Strategy Lab dashboard section, its sidebar navigation item, and its Nous
status controls.

## Governor card

The dashboard **Governor** card summarizes queue depth, operating mode, budget
posture, and cadence controls for the hive. Its collapsible **PRs by model** nested
sub-section reads `GET /api/governor/pr-models` with the selected `7d`, `30d`,
or `all` window. The nested header keeps the window/sort toggles visible and its
collapsed summary shows the current top-ranked model plus model count.
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

The Throughput panel appears as a nested Advisory sub-section with its header separated from the body card; its subtitle uses the small muted dashboard subtitle style.

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

Issue data comes from the existing issue-request and claim-poller audit stream:
agent-created issues, agent comments/claims as triage/review signals, and
agent issue-close events. Historical human issue creation/review/closure is not
backfilled; the actor matrix and trend chart start accumulating as new audited
or observed events arrive.

## Project Inception and Knowledge

Project Inception is branded as powered by Spektacular with a linked header pill and body note. Knowledge Base collapsed summaries show the total fact count (the same source as the sidebar badge); layer health stays inside the body with a short explanation that layers are knowledge-source scopes for facts.

## Audit Log

The Audit Log viewport defaults to a taller resizable panel. Browser-local height changes are persisted in `localStorage` under `hive.audit.panel.height`; the entry cap/search controls remain in the card header area above the scrollable table.

## Hive Chat

The floating **🐝 Hive Chat** panel is a command-first dashboard assistant. Type
`/help` (or `help`) to render the command registry; the same registry drives
one-line help, `/help <command>` details, examples, and slash-command
autocomplete and argument hints. Current commands are `/help`, `/clear`,
`/search`, `/history`, `/retry`, `/edit`, `/who`, `/agents`, `/beads`,
`/prs`, `/governor`, `/knowledge`, `/jam`, and `/spek`.

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
deliberately avoids the substring `hold` so it is matched exactly and cannot
collide with the agent provenance label `hive/<hive-id>`. Removing a hold only
removes the label(s) that are actually causing the hold (`hive-pause/<hive-id>`
and/or the generic hold labels such as `hold`, `on-hold`, or `hold/review`) and
never removes `hive/<hive-id>` provenance. The hive-specific canonical hold is
`hive-pause/<hive-id>`; `hive/<hive-id>` is provenance only. On upgrade, Hive writes
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
shows open issues, open PRs, actionable now, held, blocked/needs-human, and the
median actionable age. Operators can view each Issues or PRs panel as a donut,
pie, horizontal bar, single 100% stacked bar, line/spark trend, or age
histogram. Every shape is still driven by the same band slices and
server-provided classifications. Hovering a chart element, an Overview legend
row, or a repository-card band header shows its rule from the Go band specs
carried in `/api/status` as `overview_bands`. Each actionable/held issue and PR
carries `band`, `signals`, `stale`, and `held`; the browser does not classify
labels or timestamps again. Full SSE updates carry the same fields.

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
label mode, KPI visibility, export format, default age basis, and the bounded
client-side line/spark history. The repo multi-select is stored separately under
`hive.overview.repos`, with All/None shortcuts and an Org shortcut when the
hive spans multiple GitHub organizations. The default remains donut-only with
the carousel off, a 30-second interval, fade transition, normal duration, KPI
strip on, CSV exports, updated-time age basis, and all repos selected.
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
reviewed `💬`, auto-merge `🔀`, agent role badges, and review-class badges
such as `FIX`). Repository cards also show a labelled auto-merge switch: it is effectively off below L6, switching the hive to L6 turns it on for every active repo, and owners may toggle individual repos afterward.

Actionable issue pills are grouped client-side for display only; enumeration,
holds, filters, ranking, and agent kick behaviour are unchanged. Bands are
named after the action the operator should take (or an explicit "nothing
needed"), not after how the dashboard classified the issue. Each issue appears
in exactly one band, while non-winning states remain as badges on the pill:

1. **Unclaimed** (`unclaimed`) — no other band matched: nobody is assigned,
   nothing claimed it, and no human gate applies. This does not by itself mean
   agents will pick it up.
2. **Claimed** (`claimed`) — assignee set, `claimed`, or `hive/claimed-by-*`.
3. **Needs triage** (`triage`) — an `agent/<role>` label and no human has
   acknowledged the proposal under #5117: no `approved-direction` label and no
   human assignee (the snapshot's `human_acknowledged`; a human *comment* only
   satisfies the PR-time gate and is not in the payload). Acknowledge it, or
   close it. Once acknowledged the issue falls through to Unclaimed/Claimed
   with the role badge still on the pill.
4. **Needs human** (`needs human`) — labels such as `blocked`, `needs-decision`,
   `2-discussing`, `Epic`, `needs-human`, or `needs-triage`. Note that the
   `needs-triage` *label* lands here, not in the Needs triage band.
5. **Confirm & close** (`close?`) — labels such as `hive/already-done`,
   `hive/covered-by-pr`, and `hive/likely-done`: an agent believes the work
   landed; verify and close.

Precedence is confirm & close → needs human → claimed → needs triage → unclaimed,
so a human gate beats an assignment and done beats all other display states.
Within each band, issues sort by `updated_at` oldest first. The issue breakdown
also shows `N no activity > 14d` for actionable issues older than the stale
threshold.

Operators can tune only the display taxonomy under `dashboard.issue_bands`:

```yaml
dashboard:
  issue_bands:
    waiting_labels: [blocked, needs-decision, 2-discussing, Epic, needs-human, needs-triage]
    done_labels: [hive/already-done, hive/covered-by-pr, hive/likely-done]
    stale_days: 14
```

These settings deliberately do not reuse `governor.labels.exempt`,
`contribute_skip_labels`, or `project.issue_filter`; those decide work
eligibility, while issue bands decide how the dashboard describes already
enumerated work. Linked-PR badges render from the `linked_prs` payload and are not inferred by the card.

PR pills are also grouped client-side for display only. Open and held PRs appear
in exactly one band; held PRs are no longer appended after the actionable list.
First match wins for classification:

1. **Needs human** — `needs-human`, held, `needs-decision`,
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

Repository issue pills can show a `🔗 #N` badge when Hive has verified a pull request related to that issue. Open PRs apply `hive/covered-by-pr`; merged PRs on still-open issues apply `hive/likely-done` and render as `🔗 #N merged`. These are pending signals, not resolution: the issue remains in the actionable list until GitHub closes it, GitHub reports the PR in `closingIssuesReferences`, or an operator confirms coverage. The status payload exposes the same evidence as `linked_prs: [{number, state, merged, url, closing}]` on each `github.Issue`.

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
- **Linked issues.** `linked_issues: [{number, repo, state, url}]` are the
  first 20 `closingIssuesReferences` GitHub reports for the PR, from that
  same request. The pill's action cluster shows a `🔗` badge that opens the
  first still-open one (else the first); the tooltip lists all. This is the
  PR-side mirror of the issue column's `linked_prs` badge, and like it comes
  only from the snapshot — the card never infers or fetches relationships.

The extended request is tried first; a forge that rejects one of the added
fields (an older GHE) is asked the decision-only query instead, so the review
decision the merge-block wording depends on is never lost to a display
field. Stale drafts, which `EnrichCIStatus` never touches, receive these
signals through `EnrichReviewSignals` — the same per-repository query and no
per-PR mergeability or check-run fetch, since a draft is not a merge
candidate.


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
