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

`agy_usage.json` is a structured `agy --print "/usage" --output-format json`
response shaped to the `agyUsageResponse` schema `AgyProber` consumes
(kubestellar/hive#6966): a `quota` map carrying the plan tier and one entry per
usage window, each with a `remaining_fraction` (0..1), a `reset_at`, and a
`duration_mins`. It carries a healthy `five_hour` window and a 9%-remaining
`weekly` window, so the fixture exercises reset timing, duration banding, the
`remaining_fraction`→percent conversion, and the worst-window-binds fold: the
weekly window binds and pushes the reading past a default reserve even though
the short window is healthy.

Provenance note: this fixture is **schema-derived from the field names
#6833/#6966 document, NOT captured from a live agy account.** agy 1.1.22 exposed
no auth surface on the verifying host, so no real payload was obtainable, and
the exact JSON nesting/spelling (the `quota.windows` object, the `reset_at`
field name, `duration_mins`) is therefore a best-effort instance of the
documented `quota` map rather than a redacted real capture. When a real recorded
payload becomes available it should replace this file and the parser field tags
should be reconciled with it. The adapter's behaviour on an unrecognized schema
is pinned separately and independently of these details by
`TestAgyHeadroomRejectsUnrecognizedSchema`.
