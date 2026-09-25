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

When the FAQ names a dotted config key inside `<code>...</code>`, keep it in
sync with the Go config schema. `pkg/dashboard` has a guard test that extracts
those keys from the FAQ panel and asserts each path exists in `config.Config`
via YAML tags.

## Design system

Dashboard UI changes should follow the shared [dashboard design system](dashboard-design-system.md), [dashboard glossary and sidebar IA](dashboard-glossary.md), and [ADR-0018](adr/0018-dashboard-design-tokens.md). The token layer is the theme contract for future user theme/background work and the migration path away from static inline styles; `go test ./pkg/dashboard/... -run StyleRatchet -v` ratchets inline styles and raw CSS values so the debt only goes down.

## Governor card

The dashboard **Governor** card summarizes queue depth, operating mode, budget
posture, and cadence controls for the hive. Its **PRs by model** section reads
`GET /api/governor/pr-models` with the selected `7d`, `30d`, or `all` window.
Rows keep the merged/open/closed PR-volume bar, then add compact effectiveness
columns from the same aggregation used by the contributor Operations **Most
effective models** panel: merged PRs, first-pass merge rate, verified-PR run
rate, failure rate, and completed-without-PR ("nothing to ship") rate. Models
that meet `HIVE_CONTRIBUTE_EFFECTIVE_MODELS_MIN_PRS` (default `5`) merged PRs
get rank badges. The default row order is effectiveness rank; operators can
toggle back to raw PR count without changing the selected window.

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

## Repository card legend and issue bands

The **Repositories** section includes a compact, collapsible pill legend. It is
stored per browser in `localStorage` and uses the same pill classes as the cards,
so theme changes update the legend automatically. The legend explains issue
actionable/held pills, plan chips, hold/release controls, issue state glyphs
(`⛔`, `❓`, `👤`, `✓`, role badges, stale `🕒`) and PR states (`✓`, `◐`,
`⚠`, held, reviewed `💬`, auto-merge `🔀`, and review-class badges such as
`FIX`).

Actionable issue pills are grouped client-side for display only; enumeration,
holds, filters, and agent kick behaviour are unchanged. Each issue appears in
exactly one band, while non-winning states remain as badges on the pill:

1. **Ready** — no display taxonomy state matched.
2. **In progress** — assignee set, `claimed`, or `hive/claimed-by-*`.
3. **Agent-filed** — an `agent/<role>` label; roles render as compact badges.
4. **Waiting on human** — labels such as `blocked`, `needs-decision`,
   `2-discussing`, `Epic`, `needs-human`, or `needs-triage`.
5. **Likely done** — labels such as `hive/already-done`, `hive/covered-by-pr`, and `hive/likely-done`.

Precedence is likely done → waiting on human → in progress → agent-filed → ready,
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

## Linked PR issue signals

Repository issue pills can show a `🔗 #N` badge when Hive has verified a pull request related to that issue. Open PRs apply `hive/covered-by-pr`; merged PRs on still-open issues apply `hive/likely-done` and render as `🔗 #N merged`. These are pending signals, not resolution: the issue remains in the actionable list until GitHub closes it, GitHub reports the PR in `closingIssuesReferences`, or an operator confirms coverage. The status payload exposes the same evidence as `linked_prs: [{number, state, merged, url, closing}]` on each `github.Issue`.


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
