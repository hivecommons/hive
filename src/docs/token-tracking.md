# Token collection and usage tracking

Hive tracks token usage for local budgeting, dashboard cost estimates, and hub-level fleet rollups. The implementation lives in `pkg/tokens`, `pkg/dashboard/cost.go`, and `pkg/hub/usage.go`.

## Collection sources

The collector starts from `data.metrics_dir` and rescans every 30 seconds. It merges:

- Hive JSONL session files in `data.metrics_dir` (`*.jsonl`) using the flat `SessionEntry` schema.
- Claude Code session JSONL under `data.claude_sessions_dir` when configured. It reads assistant `message.usage` blocks from recent files (30-day mtime window).
- Copilot CLI `events.jsonl` under `data.copilot_sessions_dir` when configured. It reads `session.shutdown.modelMetrics` and avoids double-counting sessions already captured live by the proxy.
- Bob CLI chat recordings under `data.bob_sessions_dir` (default `/data/home/.bob`) when configured. It reads `tmp/*/chats/*.json`, maps Bob project hashes back to trusted agent folders, and records explicit `tokens` fields (falling back to content-size estimates only when a recording has no token data).
- Inference usage files written by `InferenceSink` as `inference-<agent>.jsonl` under `data.metrics_dir` for `vllm`, `llm-d`, `litellm`, and live Copilot proxy usage.

A flat session entry can include `role`, `agent`, `model`, `input_tokens`, `output_tokens`, `cache_read`, and `cache_creation`. When `agent` is absent, Hive infers the agent from session path or configured detection keywords.


## Sourcing decision: hybrid ccusage parsing with Hive attribution

**Proposed — awaiting maintainer sign-off on #6234/#6242.** The recommended
sourcing path is option (c), a hybrid: use a pinned `ccusage` subprocess for the
backend usage formats it already covers, while Hive keeps its attribution,
budget, and price-label semantics.

The earlier coordination hazard is now moot: PR
[#6142](https://github.com/hivecommons/hive/pull/6142), which removed dead
scanner variants, has already merged to `v5`. That may require a small seam
re-add when this work lands, but it no longer needs to block the sourcing
decision.

Proposed contract:

- Hive invokes `ccusage --json --offline` through a narrow source interface,
  with the ccusage version pinned and the JSON output shape asserted by tests.
- ccusage parses the nine CLI backends it covers today, closing the rotation gap
  for backends whose token data currently goes dark.
- Hive keeps the attribution layer: path-to-agent mapping, session de-dupe,
  per-agent/per-model aggregation, budget windows, dashboard labels, and hub
  rollups remain Hive-owned.
- Hive keeps its price table for labeling and policy decisions. Provider-native
  numbers stay labeled `native`; table-derived numbers stay `estimated`; missing
  prices remain explicit instead of pretending to be invoices.
- Hive keeps `bob_scanner.go` while ccusage lacks Bob coverage, so adopting
  ccusage does not regress an already-metered backend.
- The companion RFC design doc is expected at
  [src/docs/design/token-metering-ccusage.md](https://github.com/hivecommons/hive/blob/v4/src/docs/design/token-metering-ccusage.md); this
  page records the sourcing decision so implementation PRs have one target.

Sequencing, aligned with [#6234](https://github.com/hivecommons/hive/issues/6234):

1. **Source seam.** Add an internal interface that can return normalized usage
   entries from either current scanners or the ccusage subprocess.
2. **ccusage implementation.** Add the pinned subprocess runner using
   `--json --offline`, fixtures for representative covered backends, and tests
   that fail on output-shape drift.
3. **Parallel-run diff.** Run existing scanners and ccusage side by side for
   overlapping sources, logging structured deltas without changing budget input.
4. **Per-backend cutover.** Move one backend at a time to ccusage when the diff
   is understood; keep Bob on Hive's scanner until upstream coverage exists.

## Stored summary

The collector keeps the latest aggregate in memory and writes `/data/token-summary.json` by default. The summary includes:

- total input/output/cache-read/cache-create tokens,
- per-agent and per-model totals,
- detailed per-agent/per-model token buckets,
- session count and recent session metadata.

`data.metrics_dir` is therefore both an input directory for Hive/inference JSONL and the durable location for per-agent inference usage files.

## Dashboard and API

- `/api/status` includes token/cost fields used by the dashboard.
- `/api/cost` returns estimated cost from token counts × the static price table plus native spend for gateways that report it (OpenRouter `/key`, LiteLLM `/key/info`). Estimated rows are labelled `estimated` or `unpriced`; native gateway rows are labelled `native`.
- `/api/repo-activity` is phase 1 of per-repo cost attribution: it reports audited output counts per repo and per `(repo, agent)` from `repo=` audit entries, plus an explicit `unattributed` bucket for output events with no repo. It reports activity only, not dollars; cost must not be smeared across repos until timestamped token joins exist.
- Cost estimates are not invoices. Subscription plans, self-hosted inference, negotiated rates, and provider billing semantics can differ from list prices.

## Metering diagnostics and zero-token health

The collector keeps a non-secret `Diagnostics` snapshot alongside the aggregate (`pkg/tokens`, `Collector.Diagnostics()`), so consumers can distinguish "zero tokens because nothing used a model" from "zero tokens because metering itself is unhealthy":

- `last_scan_error` — the flat-JSONL scan of `data.metrics_dir` failed. This aborts the whole scan cycle: the previous aggregate is kept and no other field is refreshed until the next cycle.
- `last_claude_scan_error`, `last_copilot_scan_error`, `last_bob_scan_error` — a per-source scan failed. Only that source is skipped for the cycle; the other sources still merge. Each field is rebuilt every scan, so a source error clears on the next successful pass over that source.
- `live_capture_enabled` — the Copilot proxy's live usage capture is active (set at boot via `SetCopilotLiveCapture`).

The deep-health `tokens` check sent in spoke heartbeats (`HealthSummary` in `pkg/dashboard/server.go`) uses this to replace the bare "zero consumed" warning with a reason, evaluated in precedence order:

| Status | Detail | Meaning |
|---|---|---|
| `skip` | `zero consumed — all agents paused` | Expected quiet: every enabled agent is paused. |
| `skip` | `zero consumed — no agents due in the current governor mode` | Expected quiet: only on-demand or off-schedule agents remain. |
| `warn` | `zero consumed — token parser/sink error: <err>` | Flat-JSONL scan failing (then Claude/Copilot/Bob parser errors, each named). |
| `warn` | `zero consumed — sessions still open or live-capture has not accounted usage yet` | Live capture is on and sessions exist; usage may land shortly. |
| `warn` | `zero consumed — sessions made no model calls` | Sessions were scanned but recorded no token usage. |
| `warn` | `zero consumed — token sink or live metering disabled/misconfigured` | A metered agent is running with live capture off — check metering wiring. |
| `warn` | `zero consumed — no model calls recorded` | Fallback when no better explanation applies. |

On the hub's fleet page, the tokens column tooltip reads that check's detail out of the heartbeat health checks and renders `No tokens used: <reason>` for hives at or below the no-token threshold (`tokenHealthReason` / `tokenUsageTitle` in `pkg/hub/saas.go`).

Caveat: the reasoned detail is heartbeat-path only. The local `/api/health/deep` tokens check still emits the plain `zero tokens consumed — agents may not be working` warning without consulting `Diagnostics`.

See [health-checks.md](health-checks.md) for the operator-facing summary of the agent and token deep-health checks.

## Hub rollups

Each spoke heartbeat sends one scalar `tokens_24h` value. Despite the historical name, current code sends the cumulative total from the spoke's token summary. The hub stores it as `totalTokens24h`, samples a 7-day fleet history every 15 minutes, and serves `/api/saas/usage` with rollups by org/repo, owner, and cluster plus zero-consumption hives.

The hub cannot compute dollars or per-model/per-agent attribution because the heartbeat does not include model or agent token splits. Use the individual spoke's `/api/cost` for those details.
