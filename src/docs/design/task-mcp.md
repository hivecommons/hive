# Task-scoped MCP

Hive serves the Phase 1 task MCP endpoint on the dashboard mux at `POST /api/contribute/mcp`, under the existing contributor API prefix. The transport is streamable HTTP JSON-RPC 2.0 and is read-only.

Phase 1 is for hub-launched agents only. Hive injects the endpoint through agent `connections` as an MCP server so existing `connectionMCPFlags` launch handling can pass it to supported CLIs. Relay-delivered `task_assign` messages are unchanged until a later phase.

The initial tools are:

- `task_context()` for the scoped assignment, labels, lease age, hold and level gates, standby tier/lane policy, and PR-template policy slots.
- `related_work()` and `ci_health()` as capped, paginated cache-only slots; their backing stores arrive in follow-up PRs.
- `context_bundle()` for backends that prefer one call. It returns the three tool payloads together and is the Phase 1 token-delta measurement surface.

Prompt-injection handling is structural: issue and PR text is only returned inside a fixed-schema `data` field. Tool descriptions and other free-text metadata never interpolate served GitHub text. Answers are scoped to the active task repo, capped, paginated, and read from Hive state/caches only; request handling must not call GitHub.
