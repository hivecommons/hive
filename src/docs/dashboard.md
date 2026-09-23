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
