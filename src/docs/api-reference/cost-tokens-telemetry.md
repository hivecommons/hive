# Dashboard REST API reference — Cost, tokens, telemetry

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/trends` | Dashboard auth/session | Trends | `pkg/dashboard/api.go:84` |
| `GET` | `/api/token-access` | Dashboard auth/session | Token Access | `pkg/dashboard/api.go:158` |
| `GET` | `/api/tokens` | Dashboard auth/session | Tokens | `pkg/dashboard/api.go:159` |
| `GET` | `/api/cost` | Dashboard auth/session | Cost; `merged_prs` / `closed_issues` come from the webhook-incremented ForgeState counters reconciled hourly against search, with `count_stale`, `count_stale_since` and `count_source` (`forgestate` / `search`) when last-good counts are being served (#13376), `count_partial` when only some configured repos have an attributed baseline yet, and `count_error` naming why the latest attributed refresh failed or was partial (the reconcile asks GitHub about a budgeted slice of repos per pass so a many-repo hive stays inside search's rate limit); `daily_rollup` lists per-UTC-day estimated spend (`day`, `cost_usd`, `input_tokens`, `output_tokens`, `samples`) kept for ~2 years so the Cost chart can reach past the 30-day snapshot ring; `recording` reports `agent_models`, `unrecorded_models`, `recent_models`, `last_priced_growth_at`, `last_agent_model_growth_at`, `stale_after_ms` and `stale` so the card can flag stale live usage recording (#13485) | `pkg/dashboard/api.go:160` |
| `GET` | `/api/repo-activity` | Dashboard auth/session | Per-repo audited activity | `pkg/dashboard/api.go` |
| `GET` | `/api/repo-cost` | Dashboard auth/session | Per-repo estimated token cost (interval join, cached) | `pkg/dashboard/api.go` |
| `GET` | `/api/launch-cost` | Dashboard auth/session | Per-issue / PR / head-SHA estimated token cost from kick launch attribution (cached; `?ref=owner/repo#N` narrows) | `pkg/dashboard/api.go` |
| `GET` | `/api/launch-cost/history` | Dashboard auth/session | Daily per-ref rollup of launch attribution (`daily_rollup[]`: `day`, `by_ref[]` with `ref`, `kind`, `launches`, `share_usd`), kept ~2 years on the `/data` PVC so cost per PR can be measured past the `/api/launch-cost` window | `pkg/dashboard/api.go` |
| `GET` | `/api/cost/history` | Dashboard auth/session | Cost History | `pkg/dashboard/api.go:166` |
| `GET` | `/api/trend/history` | Dashboard auth/session | Trend History | `pkg/dashboard/api.go:169` |
| `GET` | `/api/timeseries` | Dashboard auth/session | Time Series | `pkg/dashboard/api.go:171` |
| `GET` | `/api/providers/headroom` | Dashboard auth/session | Providers Headroom | `pkg/dashboard/api.go:61` |
| `GET` | `/api/budget/history` | Dashboard auth/session | Budget History | `pkg/dashboard/api.go:168` |
