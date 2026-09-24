# Dashboard themes

Add a dashboard theme by dropping one `*.yaml` file in this directory. The file
is embedded into the `hive` binary with `embed.FS`; CI parses every file and
fails on duplicate IDs or unsupported CSS variables.

Fields:

- `id`: stable lowercase id used by `dashboard.theme` and `/api/theme.css?theme=`.
- `name`: display name in Settings → Appearance.
- `description`: short original description. Do not include copyrighted quotes,
  logos, franchise art, or bundled fonts you do not have a license to ship.
- `author`: credit for this theme definition.
- `scopes`: optional list of surfaces where the theme is offered. Use `dashboard`, `contributor`, or both. Empty means dashboard-only for old files.
- `dark`: whether the base palette is dark.
- `tokens`: ADR-0018 CSS custom properties from
  `pkg/dashboard/static/tokens.css` `:root`. New themes should set canonical
  tokens: `--surface-*`, `--text*`, `--line-*`, `--brand`, `--status-*`,
  `--acmm-level-*`, `--vendor-*`, `--font-*`, `--fs-*`, `--sp-*`, `--r*`,
  `--shadow-*`, and documented component metrics such as `--control-min-h`,
  `--btn-*`, `--badge-*`, `--table-cell-*`, `--component-*`, and
  `--duration-*`.
- `light_tokens` (optional): token overrides emitted in the same light-mode
  selector used by the dashboard (`body.light-mode`). For light presets
  (`dark: false`), the base `tokens` map is emitted there automatically.
- `fonts.ui` / `fonts.mono`: CSS font stacks. Use system fonts or openly hosted
  fonts; do not bundle proprietary font files here.
- `background` (optional): `image` (`https:` or bounded `data:` URI), `position`,
  `size`, `opacity` (`0..1`), and `attachment` (`fixed`, `scroll`, or `local`).
  Gradients may be expressed in `custom_css`.
- `custom_css` (optional): extra CSS appended after tokens. It is sanitized and
  capped at 32 KiB; `url()` and `@import` may use only `https:` or bounded
  `data:` URIs.

Minimal example:

```yaml
id: example
name: Example
description: Original one-line description.
author: Your Name
scopes: [dashboard, contributor]
dark: true
tokens:
  "--surface-0": "#101214"
  "--surface-1": "#14171c"
  "--surface-2": "#171a1f"
  "--text": "#f8fafc"
  "--text-muted": "#94a3b8"
  "--brand": "#f4c75f"
  "--status-info": "#7dd3fc"
fonts:
  ui: "Inter, ui-sans-serif, system-ui"
  mono: "'SF Mono', monospace"
```

Legacy dashboard variables remain accepted for existing user themes but are
deprecated for new presets. During CSS compilation, a deprecated token also sets
its canonical token when the canonical token is absent; when both are present,
the canonical value wins and the legacy variable is emitted as `var(--canonical)`.

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

Validation rejects unsupported token names, empty token values, style breakouts
such as `<` / `>` or `</style`, non-HTTPS external URLs in `custom_css`, and
theme CSS beyond 32 KiB.
