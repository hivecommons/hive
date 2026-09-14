# Codex rate-limit fixtures

`codex_rate_limits_0.154.0.json` is the `result` payload of an
`account/rateLimits/read` reply, shaped to the `RateLimitSnapshot` schema that
`codex app-server generate-json-schema` emits on codex-cli **0.154.0**
(kubestellar/hive#6964). It exercises the full documented surface the adapter
consumes: `primary` and `secondary` positional windows, `planType`, `credits`,
`ordinaryUsageAllowed`, and `rateLimitsByLimitId` — including a scoped window
(`codex-scoped-daily`) that is more used than either positional window, and two
entries (`codex-5h`, `codex-weekly`) whose `limitId` duplicates a positional
window so the dedup path is covered.

Provenance note: the field values are schema-derived, not captured from a live
Codex account (this working tree has no live Codex credentials). It is a faithful
instance of the documented schema, not a redacted real capture; when a real
recorded payload becomes available it should replace this file. The adapter's
behaviour on an unrecognized schema is pinned separately by
`TestCodexHeadroomRejectsUnrecognizedSchema`.

# Copilot premium-request usage fixture

`copilot_premium_request_usage.json` is a response to the documented enhanced
billing platform endpoint
`GET /users/{username}/settings/billing/premium_request/usage`
(kubestellar/hive#6980). It exercises the surface the adapter consumes:
`timePeriod`, and `usageItems` with `product`/`sku`/`model`/`unitType` and the
`grossQuantity`/`discountQuantity`/`netQuantity` triple — including an item
with `netQuantity > 0` (billed overage, which sets the paid-usage signal) and a
non-`copilot` product (`actions`) that the adapter must not count.

Provenance note: the field values are schema-derived, not captured from a live
Copilot account (this working tree has no live billing credentials). It is a
faithful instance of the documented schema, not a redacted real capture; when a
real recorded payload becomes available it should replace this file. The
adapter's behaviour on an unrecognized schema is pinned separately by
`TestCopilotHeadroomRejectsUnrecognizedSchema` — and note the adapter already
ships reporting an explicit unknown unless the operator states the plan's
monthly allowance, per #6980's fixture criterion.

# Claude usage fixture

`claude_oauth_usage.json` is a response body of the `GET /api/oauth/usage`
endpoint the Claude Code HUD polls, shaped to the `claudeUsageResponse` schema
`ClaudeProber` consumes (kubestellar/hive#6965). It carries three `limits`
entries — `session`, `weekly_all`, and `weekly_scoped` — each with a `percent`
and a `resets_at`, so the fixture exercises reset timing, per-kind duration
banding, and the worst-window-binds fold: the 88%-used `weekly_all` window is
the binding one and pushes the reading past an 80% threshold even though the
other two windows are healthy.

Provenance note: the field values are **schema-derived, not captured from a
live Claude account** (this working tree has no live Claude credentials). It is
a faithful instance of the endpoint's documented `{kind, percent, resets_at}`
shape, not a redacted real capture; when a real recorded payload becomes
available it should replace this file. The adapter's behaviour on an
unrecognized schema (an empty/percent-less `limits` array) is pinned separately
by `TestClaudeHeadroomRejectsUnrecognizedSchema`.

# Agy usage fixture

`agy_usage.json` is a **real recorded payload** of
`agy --print "/usage" --output-format json`, captured on **agy 1.2.1**
(kubestellar/hive#6986). It replaces the schema-derived guess #6966 shipped.

The capture is unredacted: the envelope carries no account identifier
(`conversation_id` is empty), no email, and no plan tier. The percentages are
the capturing account's real levels, which is what makes it useful — the 9%
`3p-weekly` bucket is what exercises the worst-binds path and the threshold
crossing.

Shape notes, none of which matched what #6966 assumed:

- `--output-format json` is a print-mode **response formatter**, not a quota
  API, so this is a result envelope and the quota data lives under
  `command.data`, reached when `command.name == "usage"`.
- Buckets are `command.data.groups[].buckets[]` — two levels of array, not a
  flat `windows` map.
- Reset timing is `reset_time` (neither `reset_at` nor `resets_at`).
- There is no numeric duration; `window` is a label (`"weekly"`, `"5h"`) the
  adapter maps to one.
- There is no plan tier anywhere in the payload.

It carries **two independent quota pools** — `gemini-*` (Gemini Flash/Pro) at
48% remaining and `3p-*` (Claude Opus/Sonnet, GPT-OSS) at 9% — which is what
`TestAgyHeadroomSelectsPoolForModel` drives: a contributor on Gemini Flash must
be measured against the Gemini pool, not held by the exhausted third-party one.
`num_turns: 0` and `usage.total_tokens: 0` in the envelope are what confirm the
reading consumes no model turn.

Provenance caveat, narrower than before but real: this is **one account on one
plan tier**. It proves the shape #6966 assumed was wrong and pins the shape that
exists today; it does not prove the field set is complete across tiers, and a
different tier may expose groups or fields this capture does not. The adapter's
behaviour on an unrecognized schema is pinned separately and independently by
`TestAgyHeadroomRejectsUnrecognizedSchema`.
