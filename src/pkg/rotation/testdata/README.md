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
