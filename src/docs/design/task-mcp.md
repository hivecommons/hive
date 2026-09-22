# Task-scoped MCP

Hive serves the Phase 1 task MCP endpoint on the dashboard mux at `POST /api/contribute/mcp`, under the existing contributor API prefix. The transport is streamable HTTP JSON-RPC 2.0 and is read-only.

Phase 1 was for hub-launched agents only. Hive injects the endpoint through agent `connections` as the `hive-task` MCP server so existing `connectionMCPFlags` launch handling can pass it to supported CLIs.

Scope resolution accepts `task_id`, `repo`, and `number` from the `X-Hive-Task-ID` header, tool arguments, then URL query parameters in that order. The per-launch `hive-task` URI carries those query parameters while preserving the existing `token` query parameter. Dashboard lookup first checks active contributor relay connections for backward compatibility, then falls through to the agent manager's active hub-launched agents. The active-launch entry supplies the agent, repo, launch generation, and started-at timestamp; until the manager has a stable issue/bead id for a launch, Hive derives the task id deterministically as `<agent>:<repo>#<number>:<generation>`.

Hub-launched kick prompts include one short pointer when the task MCP URL is configured: call `context_bundle` on `hive-task` first instead of re-reading the issue/PR and CI from scratch. Hives without a task MCP URL keep the old prompt text.

The tools are:

- `task_context()` for the scoped assignment, labels, lease age, hold and level gates, standby tier/lane policy, and PR-template policy slots.
- `related_work()` returns cache-backed open or recently merged issues and pull requests in the scoped repo when they cite the leased issue, overlap known changed-file metadata, or are tagged as duplicate-sweep candidates. The default recency window is `hub.task_mcp_related_work_recency_days` with the built-in default from `config.DefaultTaskMCPRelatedWorkRecencyDays`.
- `ci_health()` returns per-check cache-backed health from the dashboard snapshot: default-branch state, open-PR failure counts, last-green SHA slots, and cache staleness. Prow/tide approval waits such as missing `lgtm` or `approve` are normalized to `pending`, not failing.
- `context_bundle()` for backends that prefer one call. Without arguments it returns the Phase 1 trio (`task_context`, `related_work`, `ci_health`) byte-for-byte compatible with existing callers. Phase 3 adds an optional `include: [...]` argument so callers can opt in to `repo_conventions`, `dependencies`, `history`, and `knowledge` without increasing the default response cap.

Prompt-injection handling is structural: issue and PR text is only returned inside a fixed-schema `data` field. Tool descriptions and other free-text metadata never interpolate served GitHub text. Answers are scoped to the active task repo, capped, paginated, and read from Hive state/caches only; request handling must not call GitHub.

## Phase 2: lease-scoped remote contributors

Remote contributor relays can opt in with `task_mcp.remote_enabled: true`. When a task is assigned, the hub mints an HMAC-signed lease bearer bound to the assignment tuple: task id, contributor session identity, repository, issue/PR number, and the same expiry as the hub's task lease. The lease token is stored by hash next to the task lease, so releasing, expiring, yanking, completing, or failing the task revokes MCP access with the same lifecycle as task ownership.

`task_assign` now has an optional `mcp` block:

```json
{
  "mcp": {
    "url": "https://hive.example/api/contribute/mcp",
    "token": "hive_mcp_v1.…",
    "expires_at": "2026-09-22T15:00:00Z"
  }
}
```

The relay writes that block as the standard `mcpServers` JSON shape (`hive_task`) in its contributor config directory and as `.mcp.json` in the launched agent working directory. The token is not written to the debug task file. Existing dashboard/contribute authorization still works; the lease bearer is an additional remote credential and is accepted only while the server-side lease record still matches its id, hash, task, repo, contributor identity, and expiry.

All tools resolve scope from the lease when a lease bearer is used. A request for another task, repository, or number returns a typed refusal inside the fixed-schema `data` field instead of serving cross-task content, and the hub logs the lease id, tool, and requested repo. Calls are rate-limited per lease; `task_mcp.lease_rate_limit_per_minute` defaults to `config.DefaultTaskMCPLeaseRateLimitPerMinute`.

## Phase 3 tools

Phase 3 keeps the same read-only, cache/state-only invariant: request handling does not call GitHub. All served issue, pull request, and knowledge text remains inside fixed-schema `data` fields.

| Tool | Scope | Data schema | Notes |
| --- | --- | --- | --- |
| `repo_conventions(repo)` | Active task repo only | `{repo, source, conventions:[{slug, layer, source, data:{title, body}}]}` | Reads per-repo knowledge-store facts of kind `conventions`. Missing data returns `source: "none"` and an empty list. Defaults may be derived at knowledge-index time from cached `AGENTS.md` and `.github/CODEOWNERS`; human-authored knowledge facts override derived entries. This deliberately does **not** read `hive.yaml`, because multiple spokes can serve one repository while `hive.yaml` is per-spoke. |
| `dependencies(task_id)` | Active task bead/issue only | `{task_id, blocked_by:[{id,title}], blocks:[{id,title}], suggested_plan:[{id,title}]}` | Reads bead `depends_on` edges plus the approved/draft plan children carried in bead metadata. Results are capped by `hub.task_mcp_dependencies_limit` / `config.DefaultTaskMCPDependenciesLimit`. |
| `history(repo, path?, number?)` | Active task repo only | `{items:[{kind, repo, number, state, url, updated_at, merged_at, reasons, files, data:{title, body}}]}` | Returns recent merged PRs or closed issues from existing dashboard caches when they touch the same files or cite the issue thread. It uses the related-work recency window and is capped by `hub.task_mcp_history_limit` / `config.DefaultTaskMCPHistoryLimit`. |
| `knowledge(query)` | Active task repo only | `{items:[{repo, slug, layer, confidence, tags, data:{title, body}}]}` | Performs read-only knowledge-store search and filters results to the leased task repo. Upstream lease narrowing supplies the repo set; this tool only consumes the snapshot scope. Results are capped by `hub.task_mcp_knowledge_limit` / `config.DefaultTaskMCPKnowledgeLimit`. |

`context_bundle({include:[...]})` accepts any tool names from the table. Omitting `include` preserves the Phase 1 trio; specifying it requests exactly those bundle slots.


## Token-delta measurement

Phase 1 measures the token delta on `context_bundle()`, not on each individual tool. For one real lane over a week, record the assignment prompt tokens before the measured hub-launched kick-prompt pointer above and after replacing stuffed context with that pointer. Compare the median per-task prompt-token count and keep the lane, backend, model, and date range with the measurement so Phase 2 can judge whether remote-contributor wiring is worth the added lease-auth surface.

The per-agent flag `agents.<name>.task_mcp.drop_stuffed_context: true` enables the replacement only when the hub supplies a task MCP URL. With the flag active, `${ISSUE_LIST}`, `${PR_LIST}`, and `${MERGE_ELIGIBLE}` keep their caps but render as `repo#N` refs only, and the pointer tells agents that `context_bundle` / `related_work` is the source of truth for titles, labels, age, annotations, and related context. Scheduler kick logs include `full_bytes` and `elided_bytes`; record the post-change week-long lane measurement in this section once one lane has run with the flag enabled.

### First live measurement (2026-09-22)

Spoke `hive-hosted-hosted-kubestellar-console-4vkt` (kubestellar/console, 6 repos), image `edge` @ `ecba67e`, backend `copilot`. Kick prompts were read back from `GET /api/config/agent/{name}` (`prompt` = last kick) and the bundle from a live `tools/call context_bundle`. Bytes/4 is used as the token estimate.

| Item | Bytes | ≈ tokens |
|---|---|---|
| `## Task context MCP` pointer (additive, every hub kick) | 215 | 54 |
| `context_bundle` result for `quality:kubestellar/console#0:1` (task_context + related_work + ci_health) | 845 | 169 |
| Stuffed `ACTIONABLE ISSUES` + `ACTIONABLE PRs` + `MERGE-ELIGIBLE` in the scanner kick (0 issues, 1 PR, 0 eligible) | ≈190 | ≈48 |
| One stuffed PR line / one stuffed issue line | 152 / ≈100 | 38 / ≈25 |
| Whole scanner kick / whole sec-check kick | 12168 / 10185 | 3042 / 2546 |

Reading: today the pointer is **additive** (`addTaskMCPPointer` appends; `${ISSUE_LIST}`/`${PR_LIST}` still render), so on a quiet spoke the delta is negative (+54 tokens per kick, +169 per bundle call, against ≈48 tokens of stuffed lists). The stuffed lists only break even against pointer+bundle at roughly 6–9 queue lines, and only once the stuffed sections are actually dropped when a task MCP URL is set. That conditional-drop is the follow-up (#8261); the MCP path itself is verified end-to-end (tools/list 200, scoped bundle, refusal for non-matching generations, all seven tools answering).
