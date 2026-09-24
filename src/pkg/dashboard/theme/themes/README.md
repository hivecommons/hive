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
- `dark`: whether the base palette is dark.
- `tokens`: CSS custom properties from `pkg/dashboard/static/index.html` `:root`
  (for example `--bg`, `--bg-soft`, `--panel`, `--panel-strong`, `--text`,
  `--muted`, `--line`, `--accent`, `--amber`, status colors, and font stacks).
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
dark: true
tokens:
  "--bg": "#101214"
  "--panel": "#171a1f"
  "--text": "#f8fafc"
  "--accent": "#7dd3fc"
fonts:
  ui: "Inter, ui-sans-serif, system-ui"
  mono: "'SF Mono', monospace"
```
