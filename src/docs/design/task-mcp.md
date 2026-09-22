# Task-scoped MCP

Hive serves the Phase 1 task MCP endpoint on the dashboard mux at `POST /api/contribute/mcp`, under the existing contributor API prefix. The transport is streamable HTTP JSON-RPC 2.0 and is read-only.

Phase 1 is for hub-launched agents only. Hive injects the endpoint through agent `connections` as an MCP server so existing `connectionMCPFlags` launch handling can pass it to supported CLIs. Relay-delivered `task_assign` messages are unchanged until a later phase.

The initial tools are:

- `task_context()` for the scoped assignment, labels, lease age, hold and level gates, standby tier/lane policy, and PR-template policy slots.
- `related_work()` returns cache-backed open or recently merged issues and pull requests in the scoped repo when they cite the leased issue, overlap known changed-file metadata, or are tagged as duplicate-sweep candidates. The default recency window is `hub.task_mcp_related_work_recency_days` with the built-in default from `config.DefaultTaskMCPRelatedWorkRecencyDays`.
- `ci_health()` returns per-check cache-backed health from the dashboard snapshot: default-branch state, open-PR failure counts, last-green SHA slots, and cache staleness. Prow/tide approval waits such as missing `lgtm` or `approve` are normalized to `pending`, not failing.
- `context_bundle()` for backends that prefer one call. It returns the three tool payloads together and is the Phase 1 token-delta measurement surface.

Prompt-injection handling is structural: issue and PR text is only returned inside a fixed-schema `data` field. Tool descriptions and other free-text metadata never interpolate served GitHub text. Answers are scoped to the active task repo, capped, paginated, and read from Hive state/caches only; request handling must not call GitHub.


## Token-delta measurement

Phase 1 measures the token delta on `context_bundle()`, not on each individual tool. For one real lane over a week, record the assignment prompt tokens before the MCP pointer and after replacing stuffed context with the pointer. Compare the median per-task prompt-token count and keep the lane, backend, model, and date range with the measurement so Phase 2 can judge whether remote-contributor wiring is worth the added lease-auth surface.
