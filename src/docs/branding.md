# Branding a hive

A deployment can carry its own name, mark and colours without forking the
dashboard or rebuilding the image. Two files on the data volume:

```
<data>/branding/
├── branding.json   # strings: name, tagline, mark, title
└── custom.css      # colours
```

Both are optional. A missing file changes nothing; a partial `branding.json`
leaves every field it omits at the shipped default.

## Strings

```json
{
  "product_name": "REEF",
  "tagline":      "APPLICATIONS",
  "mark":         "🪸",
  "title":        "Reef — tuna-os"
}
```

| Field | Replaces |
|---|---|
| `product_name` | the `HIVE` wordmark in the sidebar |
| `tagline` | `GATEWAY DASHBOARD` beneath it |
| `mark` | the 🐝 glyph — both sidebars, the `<h1>`, the Getting Started flyer, **and the favicon** |
| `title` | the `<title>` |

Substitution is **anchored to the exact markup** that renders each item, never
a bare string replace: the document is ~1.3 MB of inline HTML/CSS/JS, and
replacing every `HIVE` or `🐝` would corrupt script and prose. A consequence
worth knowing: if that markup changes upstream, branding silently stops
applying rather than breaking the page.

> **Strings are read once at startup.** The index document carries a
> precomputed gzip body and a strong ETag, so its content cannot vary per
> request without discarding both. **Editing `branding.json` needs a restart.**
> `custom.css` is read per request and takes effect on reload.

## Colours

`custom.css` is served at `/branding/custom.css` and linked last in `<head>`,
so it wins the cascade without `!important`.

Restyle **custom properties**, not layout. The dashboard is one embedded SPA
whose markup changes between releases; re-pointing tokens survives upgrades,
restyling structure does not.

```css
/* Dark — :root carries the DARK palette */
:root {
  --bg: #04161f;  --bg-soft: #07202b;  --surface: #0a2430;
  --panel: #0a2430;  --panel-strong: #0d2c3a;  --card-bg: #0a2430;
  --line: #12394a;  --line-strong: #1b4d63;  --border: #12394a;
  --fg: #e8f6f8;  --text: #e8f6f8;  --muted: #8fb3bd;
  --accent: #2fb6c4;
}

/* Light — body.light-mode OVERRIDES :root. Style both, or your theme
   silently does nothing whenever the operator is in light mode. */
body.light-mode {
  --bg: #f2f9fa;  --surface: #ffffff;  --panel: #ffffff;
  --line: #cbdfe5;  --fg: #06232e;  --text: #06232e;  --muted: #4d7683;
  --accent: #10707c;   /* darker: the dark-mode cyan is ~2.2:1 on white */
}
```

Two things that cost us time:

- **Style both themes.** Overriding only `:root` is discarded in light mode.
- **Re-check contrast per theme.** An accent tuned for a dark surface usually
  fails as text on a light one.

Available tokens: `--bg` `--bg-soft` `--surface` `--panel` `--panel-strong`
`--card-bg` `--terminal-bg` `--line` `--line-strong` `--border` `--fg` `--text`
`--muted` `--accent` `--oc-accent` and a named ramp (`--green` `--red`
`--orange` `--amber` `--yellow` `--blue` `--cyan` `--indigo` `--purple`, each
with `-bg`/`-border` variants where used).

Keep **status** colours distinct from your accent — a green that matches the
brand hue stops reading as "healthy" and starts reading as "branded".

## What is not overridable

- The `KubeStellar Hive Dashboard` text in the `<h1>` (only its emoji is a
  span).
- The honeycomb SVG in the Getting Started flyer — it is drawn as paths, not a
  glyph. Hide it with `.wb-hive { display: none }` if it clashes.
- Anything rendered by the SPA at runtime rather than present in the shipped
  document.

## Branding a hub

A hub (`HIVE_MODE=hub`) serves its own UI from the same binary, so the same two
files apply.

If you want a fully custom landing page, an alternative is to route `/` to your
own static page at the ingress and keep `/api` on the hub — spoke heartbeats
and `/api/registry` must keep reaching it, so route by path rather than
replacing the Service.

## Verifying

Check the **computed** style, not that the file downloaded — a stylesheet that
loads and applies nothing looks identical to a working one over `curl`:

```js
getComputedStyle(document.body).backgroundColor
getComputedStyle(document.documentElement).getPropertyValue('--accent')
```
