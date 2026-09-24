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

Dashboard UI changes should follow the shared [dashboard design system](dashboard-design-system.md) and [ADR-0018](adr/0018-dashboard-design-tokens.md). The token layer is the theme contract for future user theme/background work and the migration path away from static inline styles.

## Appearance themes

Owners can choose a hive-wide dashboard theme in **Settings → Appearance** or by
editing the persisted config through `PUT /api/config/dashboard/theme`. The
config surface is:

```yaml
dashboard:
  theme: honeycomb      # built-in id, or custom
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

Built-ins include `openclaw`, `openclaw-light`, `honeycomb`, `graphite`, `nord`,
`dracula`, `solarized-dark`, `github-light`, and `high-contrast`. Tokens are
validated against the dashboard's `:root` CSS custom properties so typos fail
fast. Custom CSS is capped at 32 KiB, strips HTML/style-breakout characters, and
only permits `https:` or bounded `data:` URLs; inlined backgrounds are capped at
256 KiB. `/api/theme.css` serves the effective theme with an ETag and is linked
from the document head so the themed stylesheet is available before first paint.
