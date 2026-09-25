# Spoke dashboard

The spoke dashboard is the operator UI served from
`src/pkg/dashboard/static/index.html`. Its FAQ panel (`#faq-section` /
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

## Repository card holds

Repository cards show held issues and PRs beside the actionable pills. A user
who owns the hive, owns the repository, or has GitHub `write`, `maintain`, or
`admin` permission on that repository can click the `⏸` chip to add or remove a
hold. The server always re-checks that permission before mutating labels. Adding
a hold applies the hive's canonical `hive-pause/<hive-id>` label. The name
deliberately avoids the substring `hold` so it is matched exactly and cannot
collide with the agent provenance label `hive/<hive-id>`. Removing a hold only
removes the label(s) that are actually causing the hold (`hive-pause/<hive-id>`
and/or the generic hold labels such as `hold`, `on-hold`, or `hold/review`) and
never removes `hive/<hive-id>` provenance. On upgrade, Hive writes
`/data/hive-hold-migration-<hive-id>.json`: audit-backed dashboard holds are
copied to the new label, agent-provenance-only items become actionable, and
ambiguous legacy labels stay held under `hive-pause/<hive-id>` for operator
review. The card-level `⏸ pause` / `▶ resume` control uses the same permission
rule.

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
