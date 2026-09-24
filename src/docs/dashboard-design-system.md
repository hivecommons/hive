# Dashboard design system

This is the working reference for dashboard contributors and agents. ADR-0018
sets the architecture: add one shared token/component layer with zero visual
change, then migrate section-by-section under ratchet tests.

Status: the shared `tokens.css` layer has landed with compatibility aliases in
place for exact-value matches across the operator SPA, contribute portal, and
hub pages. The shared `components.css` recipe layer now defines the component
classes below. A5 has migrated the obvious card/panel/surface equivalents in
the operator SPA, contribute portal, and hub landing onto the shared surface
levels while preserving legacy selectors used by JS/tests. Review the unlinked
static preview at `/design-system.html`.

## Migration status

| Area | Status | Notes |
| --- | --- | --- |
| Tokens | Landed | Shared `tokens.css` across operator, contribute, and hub. |
| Buttons | In progress | A4 is migrating button families to shared recipes. |
| Cards/surfaces | In progress | A5 is migrating surface recipes. |
| Chips/status | Migrated in A7 | Operator chip families now map to entity/status/action/count recipes, status dots/badges use the canonical `STATUS` map, ACMM levels use `--acmm-level-*`, vendor markers use `--vendor-*`, and amber remains brand/CTA chrome only. |
| Type/spacing/radius | Migrated in A6 | Operator, contributor, and hub pages collapse raw type, spacing, and radius values onto tokens with visual-first rules: exact matches plus explicitly documented type evidence. Repeated token-only inline styles use shared utilities. |

## Token catalogue

Initial values alias today's rendered operator dashboard values unless noted.
The audit measured 32 font sizes, 16 radii, 44 backgrounds, 17 text colors, 38
padding values, and 6 shadows; this catalogue is the target collapse.

### Type

| Token | Initial value | Role |
| --- | --- | --- |
| `--font-ui` | `Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif` | Default interface face. |
| `--font-mono` | `'SF Mono', 'Cascadia Code', 'Fira Code', monospace` | Code, logs, model IDs, token IDs, dense audit rows. |
| `--fs-2xs` | `.6rem` | Tiny helper text and dense chrome found during A6. Collapses .55rem, .58rem, .6rem, .62rem and equivalent small em uses. |
| `--fs-xs` | `.68rem` | Badges, micro labels. Collapses 9.6px, 9.92px, 10.4px. |
| `--fs-sm` | `.75rem` | Compact controls and dense body. Collapses 10.88px, 11.2px, 11.52px, 12px. |
| `--fs-base` | `.82rem` | Normal dashboard body. Collapses 12.48px, 12.8px, 13.12px. |
| `--fs-md` | `.92rem` | Card titles and emphasized row labels. Collapses 13.6px, 14.4px, 15.2px. |
| `--fs-lg` | `1.05rem` | Section titles and modal subheads. Collapses 16px, 16.8px. |
| `--fs-xl` | `1.25rem` | Page titles. Collapses 20px, 20.8px, 25.6px. |
| `--fs-2xl` | `1.5rem` | Large marketing/card numerals found during A6. Collapses 1.4rem, 1.5rem, 1.6rem. |
| `--fs-3xl` | `2rem` | Hero/landing subheads found during A6. Collapses 1.8rem and 2rem. |

A6 added `--fs-2xs`, `--fs-2xl`, and `--fs-3xl` from visual evidence where the original ramp forced visible changes. Values such as `.5rem`, `1.3rem`, `1.35rem`, and `1.7rem` stay raw unless a future token is added.

Type roles:

| Role | Recipe |
| --- | --- |
| Eyebrow | `--fs-xs`, uppercase, tracked, semibold/bold, brand color only when it identifies chrome. |
| Label | `--fs-xs` or `--fs-sm`, semibold, sentence case unless the surrounding pattern is an eyebrow. |
| Body | `--fs-base`, regular, `--text`. |
| Title | `--fs-md`, semibold/bold, `--text`. |
| Heading | `--fs-lg` or `--fs-xl`, bold, `--text`. |

### Spacing

The positive spacing scale is `--sp-1` through `--sp-9`; `--sp-0` is reserved
for intentional zero. Earlier notes that described `--sp-1..--sp-8` for nine
positive values were off by one.

| Token | Value | Typical use |
| --- | --- | --- |
| `--sp-0` | `0` | Intentional no gap. |
| `--sp-1` | `2px` | Hairline offsets, dense chip trim. |
| `--sp-2` | `4px` | Icon/text gap, tiny row gap. |
| `--sp-3` | `6px` | Compact vertical padding. |
| `--sp-4` | `8px` | Default inline gap, chip horizontal padding. |
| `--sp-5` | `12px` | Card inner gap, compact panel padding. |
| `--sp-6` | `16px` | Default card padding. |
| `--sp-7` | `20px` | Section gap, modal body edge. |
| `--sp-8` | `24px` | Large card/modal padding. |
| `--sp-9` | `32px` | Section block gap. |

Common padding collapses: exact scale values map to `--sp-*`. A6 measured the ±1px candidates (`3px`, `7px`, `9px`) but kept them raw where full-page screenshots showed layout drift; raw `1px`, `5px`, `10px`, `14px`, and `18px` also remain unless a future token is added.

### Radius and shadows

| Token | Initial value | Role |
| --- | --- | --- |
| `--r-sm` | `4px` | Inputs, code, tiny chips. |
| `--r` | `6px` | Buttons and compact cards; aliases old `--radius`. |
| `--r-lg` | `12px` | Cards, panels, dialogs; aliases old `--radius-lg`. |
| `--r-pill` | `999px` | Pills, chips, count badges. |
| `--shadow-card` | `0 1px 2px rgba(0, 0, 0, 0.18)` | Subtle card lift, especially light mode. |
| `--shadow-raised` | `0 4px 16px rgba(0, 0, 0, 0.45)` | Toasts and floating chrome. |
| `--shadow-modal` | `0 20px 60px rgba(0, 0, 0, 0.5)` | Modal shells. |

Radius collapses: exact token values map to `--r-*`. A6 keeps `5px`, `7px`, `8px`, `9px`, and `10px` raw where changing them would produce visible drift; `50%` remains for circular avatars/dots.

### Surfaces, lines, and text

| Token | Initial value | Role |
| --- | --- | --- |
| `--surface-0` | `#080b0f` | Page background; aliases old `--bg`. |
| `--surface-1` | `#0d1218` | Soft page/rail background; aliases old `--bg-soft`. |
| `--surface-2` | `#121922` | Panel/card background; aliases old `--panel`, `--surface`, `--card-bg`. |
| `--surface-3` | `#17212d` | Raised/strong panel; aliases old `--panel-strong`. |
| `--surface-terminal` | `#05070a` | Terminal/log panes. |
| `--line-subtle` | `color-mix(in srgb, #263545 65%, transparent)` | Hairlines and separators. |
| `--line-strong` | `#263545` | Interactive outlines and strong dividers. |
| `--text` | `#f6f8fb` | Primary text; aliases old `--fg`. |
| `--text-muted` | `#a8b3c2` | Secondary text; aliases old `--muted`. |
| `--text-faint` | `color-mix(in srgb, #a8b3c2 70%, transparent)` | Tertiary helper text. |

### Semantic color

| Token | Initial value | Role |
| --- | --- | --- |
| `--brand` | `#f4c75f` | Hive brand/chrome and primary CTA emphasis only; aliases old `--amber`. |
| `--status-ok` | `#74df9a` | Healthy, ready, success. |
| `--status-warn` | `#d29922` | Warning, degraded but not urgent. |
| `--status-attention` | `#e38b2c` | Needs review or operator attention. |
| `--status-error` | `#ff7e7e` | Failed, blocked, destructive/danger. |
| `--status-info` | `#80bfff` | Informational/active non-risk state. |
| `--status-neutral` | `#a8b3c2` | Paused, unknown, muted. |
| `--overlay-scrim` | `rgba(0, 0, 0, 0.55)` | Modal backdrop. |
| `--acmm-level-1` | `#74df9a` | ACMM L1 accent, separate from status meaning. |
| `--acmm-level-2` | `#7bd8cd` | ACMM L2 accent. |
| `--acmm-level-3` | `#80bfff` | ACMM L3 accent. |
| `--acmm-level-4` | `#818cf8` | ACMM L4 accent. |
| `--acmm-level-5` | `#c0a6f0` | ACMM L5 accent. |
| `--acmm-level-6` | `#e38b2c` | ACMM L6 accent. |
| `--vendor-openai` | `#10a37f` | OpenAI model/vendor marker. |
| `--vendor-anthropic` | `#d97706` | Anthropic model/vendor marker. |
| `--vendor-google` | `#4285f4` | Google/Gemini model/vendor marker. |

Amber is brand only. Status, ACMM levels, and vendor identity use their named
tokens even when they currently render close to amber.

### Light-mode overrides

Light mode must override the same public tokens rather than rely on translated
dark alpha tints. The checked-in light ramp preserves the existing light
layout/identity while using dedicated opaque surfaces, stronger dividers, and
light-safe semantic accents. `contrast_test.go` enforces `--text` at 7:1 on
all four surfaces, `--text-muted` at 4.5:1 on surfaces 0–2, `--text-faint` at
3:1 on surfaces 0–2, and every `--status-*`, `--acmm-level-*`, and `--brand`
accent at 3:1 on `--surface-2` in both modes. Primary button foreground on
fill and terminal cyan on terminal panes are enforced at 4.5:1.

```css
[data-theme="light"], body.light-mode {
  --surface-0: #f7f8fa;
  --surface-1: #eef1f5;
  --surface-2: #ffffff;
  --surface-3: #f8fafc;
  --surface-terminal: #1d2430;
  --line-subtle: #d9e0e8;
  --line-strong: #aeb9c7;
  --text: #111827;
  --text-muted: #4b5563;
  --text-faint: #64748b;
  --shadow-card: 0 1px 2px rgba(15, 23, 42, 0.08);
  --shadow-raised: 0 4px 16px rgba(15, 23, 42, 0.12);
  --shadow-modal: 0 20px 60px rgba(15, 23, 42, 0.15);
  --overlay-scrim: rgba(17, 24, 39, 0.36);
  --brand: #9a5f0b;
  --status-ok: #16a34a;
  --status-warn: #a16207;
  --status-attention: #c2570f;
  --status-error: #dc2626;
  --status-info: #2563eb;
  --status-neutral: #4b5563;
  --acmm-level-1: #15803d;
  --acmm-level-2: #0f766e;
  --acmm-level-3: #2563eb;
  --acmm-level-4: #4f46e5;
  --acmm-level-5: #7c3aed;
  --acmm-level-6: #c2570f;
  --terminal-cyan: #7bd8cd;
}
```

## Component variants

Migration status:

| Component area | Status |
| --- | --- |
| Buttons | [x] Operator SPA, contributor portal, and hub pages use `.hv-btn` plus the shared primary / secondary / ghost / danger / icon variants. |
| Cards/surfaces | [ ] Pending A5 migration. |
| Chips/badges/status | [ ] Pending A7 migration. |

### Buttons

All shared button recipes use the `.hv-btn` base class plus a variant class,
for example `class="hv-btn btn-primary"`. The `hv-` base prefix avoids
colliding with the legacy `.btn`, `.btn-sm`, `.btn-primary`, and
`.btn-secondary` families that already exist in the operator and hub pages
while preserving the final variant names agents will migrate to.

| Variant | Intended usage |
| --- | --- |
| `.btn-primary` | Main page action or save/apply action after changes are explicit. |
| `.btn-secondary` | Normal non-destructive action such as rescan, open, copy, or configure. |
| `.btn-ghost` | Low-emphasis navigation, read-only links, dismiss controls. |
| `.btn-danger` | Server-mutating destructive actions such as restart, revoke, delete, reset. |
| `.btn-icon` | Icon-only action with accessible label and tooltip. |
| `.btn-sm` | Compact size modifier for dense tables, cards, and toolbar actions. |

Danger means the action mutates server state destructively; do not use it only
because a warning color looks visually urgent.

### Chips, cards, data, and toolbar

| Component | Intended usage |
| --- | --- |
| `.chip-entity` | Repository, issue, PR, agent, model, or vendor identity. |
| `.badge-status` | Non-interactive state such as ready, active, blocked, degraded. |
| `.chip-action` | Small inline action that behaves like a button. |
| `.badge-count` | Numeric counts beside navigation/filter labels. |
| `.card` | Standard content container. |
| `.card-panel` | Section panel or column container. |
| `.card-inset` | Nested, terminal, or secondary surface inside a card. |
| `.metric-tile` | Label/value/change metric block with tabular numeric text. |
| `.table-compact` | Dense data tables with consistent cell padding and right-aligned numbers. |
| `.empty-state` | Icon, one-line title, helper text, optional action slot. |
| `.status-dot` | Dot/icon paired with text and tooltip from a single status object. |
| `.toolbar` | Left filters/search with right-aligned `.toolbar-actions`, `--sp-4` gaps, wrapping, and one control height. |

Status-driven recipes use `data-status="ok|warn|attention|error|info|neutral"`
to select the corresponding `--status-*` token. `.status-dot` also supports
`data-pulse="active"` for live activity. Recipe hover/focus-visible/disabled
states are part of `components.css`; the preview page renders static examples
with `.is-hover` and `.is-focus-visible` helper classes so screenshots can show
the states without script.

Canonical operator status metadata lives in the SPA `STATUS` object. All labels,
badge `data-status` values, sidebar dots, card dots, detail dots, and status
tooltips should derive from this table instead of per-site color literals:

| State key | Label | `data-status` |
| --- | --- | --- |
| `running`, `working`, `ready`, `passed` | running/working/ready/passed | `ok` |
| `idle`, `off`, `off_healthy`, `disabled`, `unknown` | idle/off/no cadence/disabled/unknown | `neutral` |
| `paused`, `pause`, `degraded`, `needs_context` | paused/degraded/needs context | `warn` |
| `needs_attention`, `needs_login` | needs attention/needs login | `attention` |
| `stopped`, `down`, `error`, `failed`, `blocked` | stopped/down/error/failed/blocked | `error` |
| `on_demand`, `active`, `initializing`, `info` | on demand/active/initializing/info | `info` |


### Current-to-new mapping

| Current family | New variant |
| --- | --- |
| `.btn` | Base plus `.btn-primary`, `.btn-secondary`, or `.btn-ghost` by behavior. |
| `.nous-btn` | `.btn-secondary` unless it is the primary page CTA. |
| `.cli-login-btn` | `.btn-primary`. |
| `.hive-dialog-btn` | `.btn-secondary`; save/apply buttons become `.btn-primary`. |
| `.btn-toggle` | Toggle component using neutral surface plus status text, not a status color alone. |
| `.gh-app-btn` | `.btn-secondary` or `.btn-primary` when it starts setup. |
| Inline/bare buttons | One of `.btn-*`, `.btn-icon`, or `.chip-action`; no static inline styling. |
| `.agent-card` | `.card`/`--surface-2` with status rail kept as a state border. |
| `.repo-card` | `.card`/`--surface-2` with repository entity header and resize hook preserved. |
| `.cost-panel`, `.token-panel`, `.governor` | `.card-panel`/`--surface-1` with `.metric-tile` summary blocks and `.table-compact` for simple cost tables. |
| `.card-inset`, `.card-tile`, `.row-card`, `.gov-pr-models`, `.oc-gov-strip` | `.card-inset`/`--surface-3` for nested surfaces; terminal/log panes use `--surface-terminal`. |
| Status pills | `.badge-status`. |
| Repository/PR/issue/model chips | `.chip-entity`; vendor color through `--vendor-*`. |
| Action-like pills | `.chip-action` with button semantics. |
| Count pills | `.badge-count`. |


### Utilities

`components.css` also exposes small utilities for repeated token-only markup:

- Type: `.fs-2xs`, `.fs-xs`, `.fs-sm`, `.fs-base`, `.fs-md`, `.fs-lg`, `.fs-xl`, `.fs-2xl`, `.fs-3xl`.
- Text color: `.text-default`, `.text-muted`, `.text-faint`, `.text-success`, `.text-warn`, `.text-danger`, `.text-info`, `.text-brand`, `.text-accent`.
- Spacing: `.m-0`, `.mt-1` through `.mt-7`, `.mb-1` through `.mb-8`, `.py-1-px-4`, `.py-2-px-4`.
- Display: `.d-block`.

Use these only for recurring token-only one-liners; component recipes remain preferred for structured UI.

## Migration rules for new code

- No raw `font-size`, hex/rgb/rgba color, `padding`, or `border-radius` values
  outside `tokens.css` and documented compatibility aliases.
- No inline `style=` for static styling. Dynamic widths, chart coordinates,
  transforms, and popover positions are allowed only with a nearby comment that
  says why a class cannot express the value.
- Amber/`--brand` is brand and primary-action emphasis only; status uses
  `--status-*`.
- ACMM levels use `--acmm-level-*`, never semantic success/error colors unless
  the UI is communicating health rather than level identity.
- Vendor colors use `--vendor-openai`, `--vendor-anthropic`, or
  `--vendor-google` aliases.
- Behavior hooks stay in `data-*`; classes describe appearance.

## Ratchet plan

`pkg/dashboard/webstatic/style_ratchet_test.go`, modeled on
`src/internal/testutil/sleep_ratchet_test.go`, keeps the debt from growing. Run
`go test ./pkg/dashboard/... -run StyleRatchet -v` to print the CI-visible
counts. Baselines measured on 2026-09-23 in PR #8584 for
#8536, after #8579:

| Surface | `style=` attributes | Raw colors | Raw `font-size` | Raw `padding` | Raw `border-radius` |
| --- | ---: | ---: | ---: | ---: | ---: |
| `pkg/dashboard/static/index.html` | 1,950 | 153 | 976 | 578 | 379 |
| `pkg/dashboard/contribute_landing.go` | 107 | 131 | 260 | 181 | 117 |
| `pkg/hub/static/*.html` + `pkg/hub/assets/*.html` | 1,140 | 474 | 612 | 331 | 254 |

The test counts `style=` even inside script/template strings because those
snippets become DOM. Raw value counters scan inline `<style>` blocks and
`style=` values, strip CSS comments, ignore fragment anchors such as `url(#id)`,
and skip custom-property declaration lines while legacy tokens are being
migrated.

To lower a baseline in a PR, remove the raw styling, run the ratchet test, and
commit the reduced expected value with the migration. Do not raise a baseline
unless the PR explains the exception and links the issue/ADR that accepts it.

## Theme override contract for #8536

The stable design-system theme contract is token-only: the theme portion served
by `GET /api/theme.css` should be a CSS block that only sets allowed tokens.
The existing dashboard custom-CSS escape hatch may still append sanitized
selector rules for advanced operators, but that is not the portable contract
contributors should target. A shareable design-system theme looks like this:

```css
:root {
  --brand: #f4c75f;
  --surface-0: #080b0f;
}

[data-theme="light"] {
  --brand: #9a6700;
}
```

The canonical token list is the `:root` block in
`pkg/dashboard/static/tokens.css`: `--font-*`, `--fs-*`, `--sp-*`, `--r*`,
`--shadow-*`, `--surface-*`, `--line-*`, `--text*`, `--brand`,
`--status-*`, `--overlay-scrim`, `--acmm-level-*`, `--vendor-*`, and the
documented component metrics (`--control-*`, `--btn-*`, `--badge-*`,
`--status-dot-size`, `--table-cell-*`, `--empty-state-*`, `--preview-*`,
`--metric-*`, `--component-*`, `--duration-*`, `--opacity-disabled`, `--fw-*`,
`--lh-*`, `--tracking-*`, `--line-width`, `--focus-ring-width`, `--full-size`,
and `--z-sticky`).

Legacy dashboard names are deprecated but still accepted by the theme engine so
existing presets and user themes keep working. When `GET /api/theme.css`
compiles a theme, a legacy declaration also emits the canonical token if it was
not set. If both are set, the canonical token wins and the legacy variable is
emitted as `var(--canonical)`. The compatibility table is:

| Deprecated | Canonical |
| --- | --- |
| `--bg` | `--surface-0` |
| `--bg-soft` | `--surface-1` |
| `--panel`, `--surface`, `--card-bg` | `--surface-2` |
| `--panel-strong` | `--surface-3` |
| `--terminal-bg` | `--surface-terminal` |
| `--muted` | `--text-muted` |
| `--fg` | `--text` |
| `--line` | `--line-subtle` |
| `--border` | `--line-subtle` |
| `--amber` | `--brand` |
| `--green` | `--status-ok` |
| `--yellow` | `--status-warn` |
| `--orange` | `--status-attention` |
| `--red`, `--oc-accent` | `--status-error` |
| `--blue` | `--status-info` |
| `--cyan`, `--terminal-cyan` | `--acmm-level-2` |
| `--indigo` | `--acmm-level-4` |
| `--purple` | `--acmm-level-5` |
| `--radius-sm` | `--r-sm` |
| `--radius` | `--r` |
| `--radius-lg` | `--r-lg` |

Forbidden in token-theme files: selectors other than `:root` and optional
`[data-theme="light"]`, `body.light-mode`, or future documented theme-root
blocks; layout declarations targeting dashboard classes; `@import`; external
URLs except sanitizer-approved background/image values; script/style breakouts;
and any declaration not setting an allowed custom property. Keep the theme token
override block at or below 32 KiB, matching the existing dashboard custom CSS
ceiling. Hive returns validation errors for unsupported token names, empty token
values, unsafe CSS characters (`<`, `>`, or `</style`), non-HTTPS external
`url()` references in custom CSS, and over-limit custom CSS/background payloads.

## Verification recipe

For visual migrations, capture before/after screenshots with headless Chrome
against the same running dashboard. Use a local port-forward to the dashboard
service, send the dashboard Bearer token in the browser context or request
headers, visit the same route/viewport/theme, and pixel-diff the resulting PNGs.
Record the viewport, theme, route, and intentional differences in the PR. Do
not paste secrets or cluster-specific hostnames into docs, logs, or screenshots.

## IA and naming glossary — proposed, needs maintainer review

Sidebar groups:

- **Overview**: Governor, dashboard summary, health.
- **Agents**: Agent list, agent detail, runs/inception.
- **Resources**: Repositories, beads, contributors, claims.
- **Intelligence**: Advisory, planning intelligence, knowledge, strategy lab.
- **Admin**: Settings, audit logs, tokens, budgets, upgrade controls.
- **Help**: FAQ, getting started, docs, external support links.

Terms:

| Term | Proposed meaning |
| --- | --- |
| Agent | Runnable automation process or configured role. |
| Contributor | Human/account participating through the contributor portal or relay. |
| Governor | Policy engine that decides autonomy and merge/apply gates. |
| Fleet | The set of agents and hives under observation. |

Avoid `clanker` in operator-facing copy; keep it only in historical/internal
references until maintainers choose a replacement path.


### A5 surface migration notes

| Legacy family | Shared surface level | Notes |
| --- | --- | --- |
| `.agent-card`, `.repo-card`, hub hero/proof cards | `--surface-2` (`.card`) | Legacy class names remain for JS/tests; bespoke gradients/radii/shadows were removed. |
| `.governor`, `.token-panel`, `.cost-panel`, contribute `.ops-card`, `.steps`, hub comparison wrapper | `--surface-1` (`.card-panel`) | Section panels now share `--r-lg`, `--line-subtle`, and `--shadow-card`. |
| `.card-inset`, `.card-tile`, `.row-card`, `.gov-pr-models`, contribute dossier stat/seal rows | `--surface-3` (`.card-inset`) | Nested cards keep local layout/click behavior but use the common inset surface. |
| `#logs-output`, `#audit-panel`, `#inception-activity`, `.oc-detail-summary`, `.doing` | `--surface-terminal` | Terminal/log panes stay dark in both themes. |
| `.token-stat`, `.cost-stat`, hub stats | `.metric-tile` equivalent | Numeric tiles use tokenized spacing, radius, border, and type scale. |

Skipped table follow-ups: governor matrix and model-performance tables remain
custom because they have responsive, state-colored matrix behavior; cost tables
were the simple dense tables migrated to `.table-compact`.
