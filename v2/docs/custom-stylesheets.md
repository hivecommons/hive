# Custom stylesheets

Hive can apply a shareable CSS theme from a public GitHub repository. The feature is available on three public/read-only surfaces:

| Surface | Example |
| --- | --- |
| ClankeR leaderboard | `/contribute/leaderboard?style=owner/repo/path/to/theme.css@ref` |
| Spoke dashboard | `/?style=owner/repo/path/to/theme.css@ref` |
| Snapshot preview | `/snapshot?style=owner/repo/path/to/theme.css@ref` |

The `@ref` suffix is optional and defaults to `HEAD`. Use the `owner/repo/path.css` triplet form, not a raw URL.

## Fetching and scoping

Hive fetches public GitHub raw content server-side without credentials, limits the response to 128 KiB, sanitizes it, then serves the sanitized stylesheet from same-origin endpoints. Dashboard and snapshot CSS is scoped to `#hive-dashboard-root`; leaderboard CSS is scoped to `#tab-leaderboard`. Login and setup overlays remain outside the dashboard theme surface.

## Sanitizer rules

Allowed CSS includes normal declarations, custom properties (`--name` and `var(--name)`), attribute and pseudo selectors, gradients, `calc()` / `clamp()`, modern color functions, and recursive `@media`, `@supports`, `@container`, and `@keyframes` rules. `@font-face` is retained only when every `src` URL is same-origin, relative, or `data:`.

Hive removes `@import`, external or protocol-relative `url()` fetches, CSS escape sequences, `image-set()`, and legacy executable CSS vectors such as `expression()`, `behavior`, and `-moz-binding`.

When anything is removed, the response includes `X-Hive-Style-Dropped: N`. Add `report=1` to `/api/style` or `/api/leaderboard/style` to get JSON containing the sanitized CSS and a short list of drop reasons.

## Example

```css
:root {
  --hive-accent: #f59e0b;
}

.card, .panel {
  border-color: var(--hive-accent);
}

@media (prefers-color-scheme: dark) {
  .metric-value { color: color-mix(in srgb, var(--hive-accent), white 20%); }
}
```

Share it as:

```text
/snapshot?style=your-org/your-theme/hive.css@main
```

