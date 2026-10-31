# Dashboard REST API reference — Hub server

## Merged contributor leaderboard

`GET /api/hub/leaderboard` returns `{"leaderboard": [...]}`. Rows merge
contributors by `github_username` across public spokes, summing
`tasks_completed`, `tasks_failed`, and `abandoned`. The integer `abandoned`
field counts abandoned PRs; older spokes that omit it contribute zero, and
merged rows explicitly include `"abandoned": 0` when none are reported.
Both full heartbeats and lightweight task-status pushes carry this field.
Private hives, agent rows, and empty usernames are excluded. Contributors
with only abandoned PRs remain visible; rows with no counts and no activity
are omitted. The added count does not change ranking.


Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/login` | Hub handler-specific | Login | `pkg/hub/oauth.go:69` |
| `GET` | `/login/{provider}` | Hub handler-specific | Per-Provider Login (github or OIDC provider) | `pkg/hub/oauth.go:72` |
| `GET` | `/api/auth/callback` | Hub handler-specific | OAuth Callback | `pkg/hub/oauth.go:73` |
| `GET` | `/api/auth/user` | Hub handler-specific | Auth User | `pkg/hub/oauth.go:74` |
| `POST` | `/api/auth/logout` | Hub handler-specific | Logout | `pkg/hub/oauth.go:75` |
| `GET` | `/api/openrouter/connect/start` | Hub auth | Hub Open Router Start | `pkg/hub/openrouter.go:26` |
| `GET` | `/api/openrouter/qr` | Hub auth | Hub Open Router QR | `pkg/hub/openrouter.go:27` |
| `GET` | `/api/openrouter/models` | Hub auth | Hub Open Router Models | `pkg/hub/openrouter.go:28` |
| `GET` | `/api/openrouter/credit` | Hub auth | Hub Open Router Credit | `pkg/hub/openrouter.go:29` |
| `GET` | `/openrouter/callback` | Hub handler-specific | Hub Open Router Callback | `pkg/hub/openrouter.go:30` |
| `GET` | `/dashboard` | Hub handler-specific | Dashboard | `pkg/hub/saas.go:369` |
| `GET` | `/access-denied` | Hub handler-specific | Access Denied | `pkg/hub/saas.go:370` |
| `GET` | `/api/hub/clusters` | Hub auth | List Clusters | `pkg/hub/saas.go:531` |
| `GET` | `/api/hub/image-pulls` | Hub auth | Per-Release Image Pull Series | `pkg/hub/saas.go:377` |
| `GET` | `/api/reach` | Hub admin | PR Reach Report (?pr=NNN or ?recent=K) | `pkg/hub/saas.go:522` |
| `GET` | `/fleet` | Hub handler-specific | My-Hives Fleet Page (static; data via `/api/saas/my-hives`) | `pkg/hub/server.go:1773` |
| `GET` | `/my-hives` | Hub handler-specific | 301 redirect to `/fleet` (query preserved) | `pkg/hub/server.go:1774` |
| `POST` | `/api/shared-agent-pool/leases` | Hub handler-specific | Acquire or renew a shared CLI session lease | `pkg/hub/server.go:1708` |
| `PUT` | `/api/shared-agent-pool/leases` | Hub handler-specific | Renew an active shared CLI session lease | `pkg/hub/server.go:1709` |
| `DELETE` | `/api/shared-agent-pool/leases` | Hub handler-specific | Release a shared CLI session lease | `pkg/hub/server.go:1710` |
| `POST` | `/api/heartbeat` | Hub handler-specific | Heartbeat | `pkg/hub/server.go:1711` |
| `POST` | `/api/task-status` | Hub handler-specific | Task Status | `pkg/hub/server.go:1712` |
| `GET` | `/api/registry` | Hub handler-specific | Registry | `pkg/hub/server.go:1720` |
| `GET` | `/api/hub/leaderboard` | Hub handler-specific | Leaderboard | `pkg/hub/server.go:1721` |
| `GET` | `/api/hub/stats` | Hub handler-specific | Stats | `pkg/hub/server.go:1722` |
| `GET` | `/api/fleet-stats` | Hub handler-specific | Fleet Stats | `pkg/hub/server.go:1723` |
| `GET` | `/api/hub/evidence` | Hub admin | Read-only cross-hive evidence drill-down from each spoke's stored heartbeat `evidence` summary ([#12116](https://github.com/hivecommons/hive/issues/12116)); non-admins get `403`. Returns `spokes`, `reporting`, pooled `calibration` (bucket 0–5 → `{n, bad, rate, lo, hi}`; the hub view folds the six buckets into four reviewer-confidence ranges), `findings[]` (the plain-language *What this says* sentences, generated from the same fields; [#12456](https://github.com/hivecommons/hive/issues/12456)), `by_acmm_level` (`level`, `hives`, `merged_90d`, `revert_rate`, and `time_to_merge` — `median_of_hive_medians_s`, `hives`, `n` — the median of the level's per-hive median time-to-merge, omitted when no hive at the level reports one; [#12464](https://github.com/hivecommons/hive/issues/12464)), `family_false_approve_rate` (`same`/`cross`/`mixed`, hives classed by their reviewer vs author family sets), fleet `record_coverage` (merged PRs with an evidence record over `prs_merged_90d`), fleet panels ([#12370](https://github.com/hivecommons/hive/issues/12370)) `outcomes_by_author_kind[]` (`kind`, `hives`, `n`, `merged`, `reverted`, `merge_rate`, `revert_rate`), `write_counters[]` (`tier`, `hives`, `attempted`, `blocked`, `block_rate`) and `shadow_gate` (`review_gate` / `intent` → `held` / `passed` outcomes with `revert_rate`), each omitted while no spoke reports it and repeated per `by_acmm_level[]` row, and per hive `hives[]` with `acmm_level`, `prs_merged_90d`, `record_coverage`, `family_mix`, false-approve / false-block / revert rates, families, a calibration series the raw `counts` behind them (including `by_author_kind`) so the drill-down can re-pool a hive / ACMM-level selection client-side, and `criteria[]`: the per-axis criteria table derived hub-side from the quadrant criteria (`axis`, `label`, `scored`, `criteria[]` with `name`, `label`, `status` `met`/`unmet`/`not_reported`, `display`/`score` when measured, `reason` otherwise; offline, evidence-less or unscored hives are `not_reported`, never `unmet`; [#12463](https://github.com/hivecommons/hive/issues/12463)). Every rate is `{n, rate, lo, hi}` with a 95% Wilson interval; `rate`/`lo`/`hi` are null when `n` < 10. Window selection ([RFC #12062](https://github.com/hivecommons/hive/issues/12062)): `?hours=24|168|720` pools each hive's matching heartbeat `windows` block instead of its 90-day block (any other or absent `hours` → 90 days); `?from=YYYY-MM-DD|RFC3339[&to=…]` (date-only `to` is inclusive, `to` defaults to now, `to` ≥ `from`, span ≤ 366 days, else `400`) is served from the smallest reported window that reaches back to `from` and flagged `window.approximate`. The response carries `window` (`hours`, `key`, `label`, `from`, `to`, `approximate`, `coverage_available` — `record_coverage` is only computed for the 90-day window because `prs_merged_90d` is a 90-day count), `window_unreported` and per-hive `window_unreported` for hives whose spoke predates per-window reporting. `?hive=<id>[,<id>…]` (repeatable) and `?level=<n>` restrict the registry server-side to those hives / that ACMM level before pooling. Hub admins open it from the Fleet quadrant card or its **🔬 Evidence** button ([#12117](https://github.com/hivecommons/hive/issues/12117)), pick a window (24h / 7d / 30d / 90d / custom), filter it by hive and ACMM level and read each section's ⓘ explainer; the per-hive author-kind revert counts behind the *Revert rate by author kind* panel are `hives[].counts.by_author_kind` ([#12326](https://github.com/hivecommons/hive/issues/12326)). Never consulted by merge decisions | `pkg/hub/evidence.go` |
| `GET` | `/api/hub/evidence/report` | Hub admin | Plain-language report of the hub evidence view ([#12456](https://github.com/hivecommons/hive/issues/12456)), rendered server-side from the same `EvidenceView` as `/api/hub/evidence` so every figure reconciles with the modal; non-admins get `403`. Same `hours=` / `from=` / `to=` window and `hive=` / `level=` filter parameters. `format=md` (default, `text/markdown`) or `format=html` (standalone page, inline CSS, no scripts or external assets, hive names escaped); any other value → `400`. `download=1` sets `Content-Disposition: attachment; filename="hive-evidence-<hive|fleet>-<from>-<to>.md|html"`, otherwise inline. Sections: Summary (the generated findings), What we measured and why, The numbers, How to read these, What is not reported yet and why, Grade per section, footer link to `review-evidence.md`. `Cache-Control: no-store`. Read-only; never consulted by merge decisions | `pkg/hub/evidence_report.go` |
| `GET` | `/api/hub/activity` | Signed-in user | Read-only hub-wide activity view ([#15951](https://github.com/hivecommons/hive/issues/15951), RFC [#15861](https://github.com/hivecommons/hive/issues/15861)) merged from each spoke's heartbeat `activity` digest, held in hub memory only. Authorization is per user and per hive before aggregation: hub admins see every registered hive; everyone else only hives they own or hold an unexpired grant for; anonymous → `401`. Returns `hives[]` (`hive_id`, `name`, `status` `ok`/`stale`/`not_reporting`/`missing`, `reporting`, `stale` once the block is older than two heartbeats, `last_heartbeat`, `received_at`, `generated_at`, `totals` — `items`, `by_stage`, `by_modifier`, counting private and truncated items too — and `truncated`), `items[]` (`hive_id`, `repo`, `n`, `kind`, `title`, `author_kind`, `stage`, `stage_n`, `since`, `mods`, `private`) ordered needs-human/needs-decision, stalled, furthest stage, oldest, and a response-level `truncated` (cap 2000). Each digest carries at most 100 items by default and never more than the 250-item hub ceiling; private-repository items appear only when the spoke opts in. Filters: `hive`, `repo`, `stage`, `mod`, `author_kind`, `q` (title substring or `#<n>`), each ≤ 200 characters (else `400`). `ETag` + `If-None-Match` → `304`; `Cache-Control: private, no-cache`, `Vary: Cookie, Authorization`. No actions | `pkg/hub/activity.go` |
| `GET` | `/api/hub/version` | Hub handler-specific | Hub Version | `pkg/hub/server.go:1731` |
| `DELETE` | `/api/hub/registry/{id}` | Hub handler-specific | Registry Delete | `pkg/hub/server.go:1741` |
| `POST` | `/api/contribute/register` | Hub handler-specific | Contribute Proxy | `pkg/hub/server.go:1742` |
| `GET` | `/api/contribute/status` | Hub handler-specific | Contribute Status | `pkg/hub/server.go:1743` |
| `GET` | `/api/contribute/ws` | Hub handler-specific | Contribute WSProxy | `pkg/hub/server.go:1744` |
| `POST` | `/api/github/webhook` | Hub handler-specific | GitHub Webhook | `pkg/hub/server.go:1745` |
| `GET` | `/gh-setup` | Hub handler-specific | GitHub App Setup Router | `pkg/hub/server.go:1746` |
| `GET` | `/learn` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1747` |
| `GET` | `/get-started` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1748` |
| `GET` | `/api/docs` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1751` |
| `GET` | `/api/reading-list` | Hub handler-specific | Reading List | `pkg/hub/server.go:1754` |
| `GET` | `/reading` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1755` |
| `GET` | `/cncf-reference-architecture` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1762` |
| `GET` | `/cncf-reference-architecture/console` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1763` |
| `GET` | `/cncf-reference-architecture/bluefin` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1764` |
| `GET` | `/{$}` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1781` |
| `GET` | `/og-card.png` | Hub handler-specific | OGCard | `pkg/hub/server.go:1786` |
| `GET` | `/` | Public | Static asset fallback (`http.FileServerFS` over the embedded `static/` tree) for any path no other route claims | `pkg/hub/server.go:1788` |
