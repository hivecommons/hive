# Token collection and usage tracking

Hive tracks token usage for local budgeting, dashboard cost estimates, and hub-level fleet rollups. The implementation lives in `pkg/tokens`, `pkg/dashboard/cost.go`, and `pkg/hub/usage.go`.

## Collection sources

The collector starts from `data.metrics_dir` and rescans every 30 seconds. It merges:

- Hive JSONL session files in `data.metrics_dir` (`*.jsonl`) using the flat `SessionEntry` schema.
- Claude Code session JSONL under `data.claude_sessions_dir` when configured. It reads assistant `message.usage` blocks from recent files (30-day mtime window).
- Copilot CLI `events.jsonl` under `data.copilot_sessions_dir` when configured. It reads `session.shutdown.modelMetrics` and avoids double-counting sessions already captured live by the proxy.
- Inference usage files written by `InferenceSink` as `inference-<agent>.jsonl` under `data.metrics_dir` for `vllm`, `llm-d`, `litellm`, and live Copilot proxy usage.

A flat session entry can include `role`, `agent`, `model`, `input_tokens`, `output_tokens`, `cache_read`, and `cache_creation`. When `agent` is absent, Hive infers the agent from session path or configured detection keywords.

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
- Cost estimates are not invoices. Subscription plans, self-hosted inference, negotiated rates, and provider billing semantics can differ from list prices.

## Hub rollups

Each spoke heartbeat sends one scalar `tokens_24h` value. Despite the historical name, current code sends the cumulative total from the spoke's token summary. The hub stores it as `totalTokens24h`, samples a 7-day fleet history every 15 minutes, and serves `/api/saas/usage` with rollups by org/repo, owner, and cluster plus zero-consumption hives.

The hub cannot compute dollars or per-model/per-agent attribution because the heartbeat does not include model or agent token splits. Use the individual spoke's `/api/cost` for those details.
