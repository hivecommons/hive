# ADR-0018: Shared dashboard design tokens and component layer

Status: Proposed

## Context

The spoke dashboard now has three independently styled UI surfaces:

- the operator SPA in `src/pkg/dashboard/static/index.html`, with 49 root custom
  properties near the top of the file, 2,093 inline `style="..."` attributes, and
  223 distinct color-like literals in the stylesheet;
- the contributor portal in `src/pkg/dashboard/contribute_landing.go`, with a
  separate 18-token `--cc-*` palette inside a Go template string; and
- the hub static pages in `src/pkg/hub/static/*.html`, with their own inline
  `<style>` blocks and roughly 94 custom properties.

The September 2026 Bluefin visual audit measured 3,000 visible dashboard
nodes and found 32 computed font sizes, 16 border-radius values, 44 background
colors, 17 text colors, 38 padding values, and 6 box-shadow values. Its top
findings were that inline styling and raw colors make consistency impossible,
the font ladder needs to collapse to the existing six-step ramp, card/surface
recipes are inconsistent, amber is overloaded as both brand and status, and
light mode relies too much on dark-mode alpha tints.

ADR-0015 records why `style-src-attr 'unsafe-inline'` is currently accepted:
the operator SPA carries thousands of style attributes. This decision gives
that accepted risk a retirement path. The design-token layer also becomes the
contract for user themes and backgrounds proposed in
[hivecommons/hive#8536](https://github.com/hivecommons/hive/issues/8536): a
theme should be a token override file, not a selector fork of dashboard markup.

## Decision

Introduce one shared dashboard design-token and component layer, land it first
with zero visual change, and then migrate one section/component family at a
time under ratchet tests. The first implementation slice is a mechanical layer,
not a redesign: every initial token value aliases today's rendered values, and
legacy token names keep working until their consumers migrate.

The shared layer is named by role, not by one surface's history:

- primitives: `--font-*`, `--fs-*`, `--sp-*`, `--r-*`, `--shadow-*`;
- surfaces and text: `--surface-*`, `--line-*`, `--text-*`;
- semantic color: `--brand`, `--status-*`, `--overlay-scrim`,
  `--acmm-level-*`, `--vendor-*`;
- component recipes: `.btn-*`, `.chip-*`, `.badge-*`, `.card*`,
  `.metric-tile`, `.table-compact`, `.empty-state`, `.status-dot`, and the
  toolbar pattern.

The canonical stylesheet should live at
`src/pkg/dashboard/static/tokens.css`, embedded by the existing `webstatic`
embed filesystem and served as a same-origin static asset. `style-src-elem
'self'` already permits same-origin stylesheets (ADR-0015), so the external
file does not require an inline hash. Any bootstrap that injects or inlines the
stylesheet must go through `webstatic` so CSP hashes/nonces are generated at the
same point that script/style element policy is stamped. Keeping tokens in a
file preserves the byte-stable pre-gzipped `index.html`, lets the hub and
contributor pages link the same asset, and makes browser cache behavior
visible. The rejected fallback is to inline the token block into every rendered
document at build/startup; that simplifies path handling but duplicates bytes,
requires per-document CSP treatment, and makes drift harder to review.

Consumption rules:

- the operator SPA links `tokens.css` before its existing stylesheet, then
  progressively aliases old names such as `--bg`, `--panel`, `--amber`, and
  `--radius` to the new contract;
- the contributor portal keeps its Go-template string initially, but its
  `--cc-*` tokens become aliases to the shared names before component CSS is
  replaced;
- hub static pages link the embedded token file from their static `<head>`
  blocks and map hub-specific names to the same shared primitives;
- theme and branding CSS override only the public token names documented in the
  design-system reference.

Migration rules for implementation PRs:

1. Land `tokens.css` and aliases without changing pixels.
2. Convert one family at a time: buttons, chips/badges, cards/surfaces,
   modals/forms, data display, then navigation.
3. Preserve behavior and selectors used by JavaScript; data attributes remain
   behavior hooks and classes become styling hooks.
4. Replace raw values with tokens only when the before/after screenshot diff is
   attributable to anti-aliasing or is intentionally documented.
5. Lower ratchet baselines in the same PR that removes inline styles or raw
   literals; never raise them without an explicit ADR or issue reference.

Ratchet enforcement should mirror `src/internal/testutil/sleep_ratchet_test.go`:
store named counters for `style=` attributes, raw color literals, raw
`font-size`, raw `padding`, and raw `border-radius` occurrences. The first test
records the current baselines from the audit; each migration PR lowers only the
counters it improves.

## Consequences

**Easier.** Contributors, agents, and future themes share one vocabulary. A
user theme for #8536 becomes a CSS file containing `:root { --token: value }`
rather than a fragile selector override. The contributor portal and hub can
inherit improvements instead of re-solving the same palette and spacing
problems.

**Safer.** Inline style removal becomes measurable. Once static styling no
longer depends on `style="..."`, ADR-0015 can be revisited and the dashboard can
eventually drop `style-src-attr 'unsafe-inline'`.

**Incremental.** The first layer intentionally changes no visuals. Reviewers can
separate token-contract questions from component-by-component polish.

**Cost.** The repository gains another CSS asset and a compatibility period
where old and new token names coexist. Ratchet tests also need a small allowlist
for dynamic geometry, charts, and vendor colors.

## Alternatives rejected

- **Tailwind or another build-step framework.** It could enforce consistency,
  but it adds a frontend build dependency to pages that are currently embedded
  and mostly static. The near-term problem is token convergence, not utility
  generation.
- **Per-surface cleanup.** Cleaning the operator SPA, contributor portal, and
  hub separately would reduce local noise while preserving three dialects. It
  would not make #8536's "theme equals token override" contract true.
- **Big-bang rewrite.** Replacing all inline styles and components in one PR is
  too risky for a 28k-line dashboard with CSP and precompressed-asset behavior.
  Ratcheted slices give maintainers reviewable diffs and reversible steps.
