# Dashboard REST API reference

Pragmatic v1 endpoint index, with one page per area under [`api-reference/`](api-reference/) and full v6 route details below, compiled by hand from route registrations in `src/pkg/dashboard/*.go` and `src/pkg/hub/*.go` (tests excluded). There is no generator; update this page and the affected area page in the same change that adds or renames a route. Area-page counts describe those pages, not the full v6 inventory below. The reference lists method, path, coarse auth level, and one-line purpose. Request/response schemas are intentionally not hand-written here; see the handler source for exact payloads and validation.

Auth levels are derived from dashboard middleware (`isPublicPath`, dashboard token/session auth, and the `/api/v1` GitHub-token wrapper) or from hub route wrappers such as `requireAuth` (rows marked `Hub auth`) and `requireAdmin` (rows marked `Hub admin`). Hub rows marked handler-specific have no dashboard middleware; check the named handler for bearer secrets, admin checks, or public behavior.

History point limits, timestamp cutoffs, incremental cost reads and ETag/304 behavior are documented in [Dashboard history reads](dashboard-history-api.md).

## Areas

| Area | Page | Routes |
|---|---|---|
| Health, status, events | [`api-reference/health-status-events.md`](api-reference/health-status-events.md) | 50 |
| Snapshots and style | [`api-reference/snapshots-and-style.md`](api-reference/snapshots-and-style.md) | 5 |
| Auth and external accounts | [`api-reference/auth.md`](api-reference/auth.md) | 22 |
| Configuration | [`api-reference/configuration.md`](api-reference/configuration.md) | 105 |
| Agents and controls | [`api-reference/agents-and-controls.md`](api-reference/agents-and-controls.md) | 36 |
| Packs and ACMM | [`api-reference/packs-and-acmm.md`](api-reference/packs-and-acmm.md) | 18 |
| Cost, tokens, telemetry | [`api-reference/cost-tokens-telemetry.md`](api-reference/cost-tokens-telemetry.md) | 13 |
| Knowledge | [`api-reference/knowledge.md`](api-reference/knowledge.md) | 56 |
| Contribute | [`api-reference/contribute.md`](api-reference/contribute.md) | 68 |
| Nous / strategy lab | [`api-reference/nous.md`](api-reference/nous.md) | 25 |
| Inception | [`api-reference/inception.md`](api-reference/inception.md) | 14 |
| Beads | [`api-reference/beads.md`](api-reference/beads.md) | 5 |
| Dashboard miscellaneous | [`api-reference/dashboard-misc.md`](api-reference/dashboard-misc.md) | 55 |
| Hub SaaS | [`api-reference/hub-saas.md`](api-reference/hub-saas.md) | 102 |
| Hub server | [`api-reference/hub-server.md`](api-reference/hub-server.md) | 46 |

## Health, status, events

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/version` | Dashboard auth/session | Build/version metadata; includes `upgradeMarker` (`target`, `current`, `attempts`, `maxAttempts`, `failed`, `requestedAt`, `lastError`) while a self-upgrade is in flight or has failed ([#6765](https://github.com/hivecommons/hive/issues/6765)), the hub-delivered `upgradePolicy` (`hub_managed`, `spoke_managed`, `schedule`, `schedule_hour`, `schedule_timezone`, `schedule_weekday`, `paused`, `branch`, `channel`, `target_sha`, `target_resolved`, `armed_target`, and `next_update_at` — the hub's RFC3339 ETA for the next promotion into the `stable` channel, omitted when unknown ([#10256](https://github.com/hivecommons/hive/issues/10256))) when available, and an `autoUpdate` object (`enabled`, `state` — one of `disabled`/`up_to_date`/`behind`/`retrying`/`failed`/`unknown` — `healthy`, `period`, `targetBranch`, `targetCommit`, `currentCommit`, `commitsBehind`, `lastAttemptAt`, `lastError`, `detail`, and `nextUpdateAt` — the hub's RFC3339 ETA for the next update, omitted when unknown, disabled or paused ([#10257](https://github.com/hivecommons/hive/issues/10257))) that never reports a failed or unknown update as healthy ([#6962](https://github.com/hivecommons/hive/issues/6962), [#6963](https://github.com/hivecommons/hive/issues/6963)); also `hub_build` (the hub's short SHA) and, once a stdio `hive-admin-mcp` client has called `/api/version`, `/api/status` or `/api/status/summary` with its `hive-admin-mcp/<version>+<short>` User-Agent, `last_seen_stdio_build` and `last_seen_stdio_at` (RFC3339) so the dashboard can flag build skew ([#12318](https://github.com/hivecommons/hive/issues/12318)) | `pkg/dashboard/api.go:45` |
| `GET` | `/api/version/release-notes` | Dashboard auth/session | Changelog gap between two revisions: `?from=<sha>&to=<channel-branch-tag-or-sha>` returns `{from:{sha,version?}, to:{sha,ref,version?}, sections:[{version,date,categories:{added,changed,fixed,security,deprecated}}], unreleased?, truncated, source, error?}` — the `CHANGELOG.md` release sections present at `to` and absent at `from`, newest first, plus (when `to` is not a release tag) the `changelog.d/` fragments new at `to` under an `Unreleased (in this build)` section. Fetch failures return `source: "unavailable"` with `error` rather than an error status; results are cached per (from, to) for an hour ([#11065](https://github.com/hivecommons/hive/issues/11065)) | `pkg/dashboard/api.go:46` |
| `POST` | `/api/release-channel` | Owner only | Hosted spoke self-service release-channel selector; relays `stable`/`candidate`/`edge` to the hub's existing switch-branch endpoint with the spoke dashboard-token proof, and reports the requested channel as pending until the Deployment image lands | `pkg/dashboard/api.go:73` |
| `GET` | `/api/health` | Public | Basic health probe | `pkg/dashboard/server.go:1759` |
| `GET` | `/api/health/deep` | Public | Deep health probe | `pkg/dashboard/server.go:1760` |
| `GET` | `/api/livez` | Public | Kubernetes liveness probe | `pkg/dashboard/server.go:1761` |
| `GET` | `/metrics` | Registered only when `HIVE_METRICS_ENABLED`; requires `Authorization: ****** (403 if the token is unset) | Prometheus metrics | `pkg/dashboard/server.go:1767` |
| `GET` | `/api/status` | Dashboard auth/session | Dashboard aggregate status; `labelPresets[]` (`{key,label,description,labels[],states[]}`) lists the built-in label-filter presets for the Projects cards, led by `needs-human` whose `labels[]` are the effective hard-suppress labels plus `needs-signal`, the hold labels, `stage/071-needs-author` and the reporter-trust awaiting label (#12172); accepts `?fields=a,b` for top-level selection and `?omit=repos` to drop heavy sections, and honors `Accept-Encoding: gzip`; `agents[]` includes busy-pane visibility fields (`kicksUndeliverable`, `busySince`, `lastTranscriptActivity`, `condition`, `conditionMessage`) when a Working pane cannot accept kicks; `sla` (fleet) and `repos[].sla` (per repo) carry the Delivery SLA block derived from the lifecycle timeline with no extra GitHub calls: `windows.{7d,30d}.{open_to_triage,triage_to_start,start_to_pr,pr_to_merge,open_to_merge}` as `{p50_h,p95_h,n}` (hours, nearest-rank), `history[]` of up to 30 daily buckets `{day,start_to_pr_p50,pr_to_merge_p50,open_to_merge_p50,n_started,n_pr,n_merged}` persisted under `/data/sla-history.json`, `stale_in_progress[]` (`{number,repo,started_at,age_h,agent}` — issues carrying `hive/in-progress` longer than the agent claim TTL with no linked PR), `stale_count`, `merged_7d`, `claim_ttl_s` and, on the fleet block, the effective `targets` (`start_to_pr_h`, `pr_to_merge_h`, `open_to_merge_h`); `staleSince` and `repos[].countsStale` / `countsStaleSince` mark totals served from last-good figures — the previous enumeration (#13382) or the ForgeState cache's last-good counts (#13376); every your-turn issue and PR row carries the critical path (#13646): `waits_on[]` (`{kind,ref}`, kind one of `pr`, `issue`, `external`, `base-check` (ref = the check red on the base branch), `approval`, `merge`; ref `#N` in the same repository or `owner/repo#N`), `unblocks` (transitive count of your-turn items waiting on it, cycle-guarded), `may_conflict_with[]` (PR numbers of the same repository sharing a closing issue or a known changed file; never an edge), `action` (`merge`, `release-hold`, `rebase`, `rerun` or `approve`), `action_ref`, `root_cause` and `blocker` (the root cause it collapses under); `repos[].yourTurn` has `issues`, `prs` (your turn, `CLOSE` excluded), `root_issues`, `root_prs`, `root_cause_count`, `order[]` (`{kind,repo,number}` by unblocks then age), `groups[]` (`{blocker,blocker_title,dependents[]}`) and `main_red` (`{check,since,fix,fix_your_turn}`); the top-level `yourTurn` sums the counts | `pkg/dashboard/server.go:1772` |
| `GET` | `/api/status/summary` | Dashboard auth/session | Compact agent/governor/budget status summary for pollers, with the hive-wide `yourTurn` root-cause counts the phone widget reads (#13646); honors `Accept-Encoding: gzip` | `pkg/dashboard/server.go:1773` |
| `GET` | `/api/overview/issues.csv` | Dashboard auth/session | Cached Overview issue-band export as browser-compatible CSV; filters `band`, `repo`/`repos`, `stale`, and `held` are ANDed. | `pkg/dashboard/server.go:1776` |
| `GET` | `/api/overview/issues.json` | Dashboard auth/session | Cached Overview issue-band export as typed JSON with `bands[]` metadata and row fields matching the CSV columns. | `pkg/dashboard/server.go:1777` |
| `GET` | `/api/overview/prs.csv` | Dashboard auth/session | Cached Overview PR-band export as browser-compatible CSV; filters `band`, `repo`/`repos`, `stale`, and `held` are ANDed. | `pkg/dashboard/server.go:1778` |
| `GET` | `/api/overview/prs.json` | Dashboard auth/session | Cached Overview PR-band export as typed JSON with `bands[]` metadata and row fields matching the CSV columns. | `pkg/dashboard/server.go:1779` |
| `GET` | `/api/events` | Dashboard auth/session | Server-sent event stream | `pkg/dashboard/server.go:1780` |
| `GET` | `/api/swarm` | Dashboard auth/session | Current swarm status: display name, duration, active repo, expiry, persisted prep metrics when available, and idle-unlock fields. | `pkg/dashboard/swarm.go:310` |
| `POST` | `/api/swarm` | Owner only | Start a 24h repo swarm for a configured repo, returning 409 when another swarm is active and 423 when a previous swarm keeps the next swarm locked until enough agents are idle unless body `{"repo":"owner/name","force":true}` is used; saves the active record before repo prep, persists prep metrics, and announces the start to Discord when configured. | `pkg/dashboard/swarm.go:311` |
| `DELETE` | `/api/swarm` | Owner only | End the active swarm, score issues closed and PRs merged in the window, append it to history, and announce the result to Discord when configured. | `pkg/dashboard/swarm.go:312` |
| `GET` | `/api/swarm/history` | Dashboard auth/session | Completed swarm history, aggregate repo leaderboard, and top player records. | `pkg/dashboard/swarm.go:313` |
| `GET` | `/api/swarm/players` | Dashboard auth/session | Per-player swarm records and earned achievement catalog. | `pkg/dashboard/swarm.go:314` |
| `GET` | `/api/swarm/themes` | Dashboard auth/session | Default and per-repo swarm theme configuration. | `pkg/dashboard/swarm.go:315` |
| `PUT` | `/api/swarm/themes` | Owner only | Set a per-repo swarm theme (`event_name`, `call_to_arms`, `leaderboard_title`). | `pkg/dashboard/swarm.go:316` |
| `PATCH` | `/api/swarm/objectives` | Owner only | Update the active swarm SDLC objective checklist completion state. | `pkg/dashboard/swarm.go:317` |
| `GET` | `/api/leaderboard/swarm` | Public | Stable public JSON record of swarm history, leaderboard, top players, and achievements. | `pkg/dashboard/swarm.go:318` |
| `GET` | `/api/runs` | Dashboard auth/session | Active staged runs projected from task leases, plans, and lifecycle timeline. Returns an array of `Run` objects: canonical `key` (`owner/repo#number`), current-stage `lease_key`, `title`, `repo` (`owner/repo`), `stage`, `gen`, `stage_started_at`, `waiting_on` (`agent`, `remote`, `human`, `ci`, `none`), `waiting_since`, `assignee`, `last_receipt`, `plan_epic_id`, and `stages[]` (`name`, `status`, `gen`, `receipt`). | `pkg/dashboard/api.go:95` |
| `GET` | `/api/runs/audit` | Owner only | Read-only cross-repo audit query over existing retained artifacts. Query `repo`, canonical `run` (`owner/repo#number`), RFC3339 `since`/`until`, `kind`, `limit` (default 100, max 500), and `page_token`. Returns `{items,next_page_token,retention}`; each item has `source` (`audit_log`, `timeline`, `lease_receipt`, `plan_epic`), `kind`, `run`, `repo`, `at`, `actor`, `artifact_id`, `attrs`, or an explicit `kind=expired,status=expired,state=Unknown` marker when requested history may have aged out. | `pkg/dashboard/api.go:96` |
| `POST` | `/api/runs/audit` | Owner only | Activate the convergence audit campaign over a scope directory. Body `{"campaign_key":"audit-campaign","scope_dir":"/data/convergence/audit/scope","store":"audit","generation":1,"run_key":"","run_url":""}`; `scope_dir` defaults to `/data/convergence/audit/scope`, `store` defaults to `audit`, `auditor`, `scanner`, `supervisor`, then the first configured bead store. Runs inspection even when `publication.enabled` is false, but then skips the publisher and returns `publication_skipped:true` with reason `publication.disabled`. | `pkg/dashboard/api.go:97` |
| `GET` | `/api/runs/{key}/checkpoint` | Dashboard auth/session | Compact mobile/chat checkpoint review payload for a held run. Returns `run_key`, `stage`, lease `gen`, `repo`, `title`, capped plain-text `summary`, approve/reject `decisions`, dashboard deep link, `lease_gen` staleness fence, and verified-owner approver rule. | `pkg/dashboard/api.go:101` |
| `POST` | `/api/runs/{key}/checkpoint` | Owner only | Resolve a compact checkpoint decision. Body `{"action":"approve"|"reject","gen":<lease generation>}`; refuses stale generations with 409 and non-owner callers with 403 before mutating the plan. | `pkg/dashboard/api.go:102` |
| `GET` | `/api/runs/{key}` | Dashboard auth/session | One run by URL-escaped canonical run key (`owner/repo%23number`); early lease-shaped keys (`repo!run-key:stage`) and queued Wavefront run-stage keys (`owner/repo!graph:node`) are also accepted. Includes timeline-derived stage history in the same `Run` shape as `/api/runs` when a lease/timeline exists. History entries that came from an owner reset carry the owner's `reason`. When the key belongs to a wired convergence campaign, the detail response also includes an optional `burndown` block with `source`, `satisfied`, `remaining`, `unknown`, and `scope`; the list endpoint omits this block. | `pkg/dashboard/api.go:105` |
| `POST` | `/api/runs/{key}/reset` | Owner only | Move a run's lease back to an earlier stage (for example `implement` to `plan` after a rejected plan). Body `{"to": "<stage>", "reason": "<why>"}`, both required. Mints a new generation so the relay holding the old one can no longer resume, persists before answering, and records the reason on the agent audit sink, the lifecycle timeline, and the `stage_completed` hook payload (`attrs.reset = "true"`). Returns `{ok, key, stage_from, stage, gen, reason}`. 400 for the same or a later stage or a missing reason, 404 when no live staged lease holds the key, 409 when the lease expired, 500 when the registry could not be written. | `pkg/dashboard/api.go:107` |
| `GET` | `/api/campaigns` | Dashboard auth/session | List resumable campaigns from archived Inception sessions and retained Spektacular (Spek) runs. Query `search`/`q`, `repo`, `stage`, `status`, and `owner`; each campaign is keyed by stable spec/session id and includes title, source, repos, current stage/step, artifact links, linked issues/PRs, contributors, last activity, status, engine, type, run link when applicable, `superseded_by` when a newer Revise revision exists, and recheck status (`enabled`, `interval`, `last_at`, `next_at`, `in_flight`, `last_delta_count`, `external_count`, `sources_failed`). | `pkg/dashboard/api.go:108` |
| `GET` | `/api/campaigns/{id}` | Dashboard auth/session | Return one campaign by URL-escaped stable campaign id or run key, using the same campaign shape as `/api/campaigns`; recheck revisions may include a `drift` block with `codebase_changed`, `prior_revision`, `delta_count`, `recheck_reason`, `external[]` (`source`, `kind`, `title`, `url`, `published_at`, `summary`), `external_count`, and `sources_failed`. | `pkg/dashboard/api.go:109` |
| `POST` | `/api/campaigns/{id}/resume` | Owner only | Resume or pick up a campaign. Archived Inception campaigns take a durable campaign lease, restore their saved state and wiki working files; Spek campaigns return the retained run detail plus a CLI status command that reloads state from Spek artifacts. Body may include `{ "surface": "chat"|"contributor"|"cli"|"dashboard" }`; 409 means another user holds the lease. | `pkg/dashboard/api.go:110` |
| `POST` | `/api/campaigns/{id}/release` | Owner only | Release the caller's Inception campaign lease and write a `campaign_release` audit entry so another user can continue it. Run-backed Spektacular campaigns return `409 Conflict`; this endpoint never revokes their run-lifecycle stage leases. | `pkg/dashboard/api.go:111` |
| `POST` | `/api/campaigns/{id}/revise` | Owner only | Create a linked revision campaign (`revision_of`, `revision`) for revisiting shipped or parked Inception/Spek work without overwriting the previous campaign. The new revision is leased to the caller and audit-logged as `campaign_revise`. | `pkg/dashboard/api.go:112` |
| `DELETE` | `/api/campaigns/{id}` | Owner only | Delete a campaign record: removes the archived Inception state and wiki snapshot for the stable campaign id and audit-logs `campaign_delete`. Knowledge-base facts, Jam threads, scaffolded repo files and other revisions in the same chain are kept; deleting the live L1 session clears it. Run-backed Spektacular campaigns return `409 Conflict`; unknown ids `404`. | `pkg/dashboard/api.go:113` |
| `POST` | `/api/campaigns/{id}/recheck` | Owner only | Create an immediate linked Spek recheck revision using the cadence path. Returns `409 Conflict` when a recheck revision is already in flight, `404 Not Found` when recheck is disabled unless `?force=true` is supplied, and records `campaign_recheck`. | `pkg/dashboard/api.go:114` |
| `GET` | `/api/campaigns/{id}/jam` | Dashboard auth/session | Return the per-campaign Jam workspace: section-anchored threads, comments, suggestions, polls, poll decisions, current spec revision and attributed revision history. | `pkg/dashboard/api.go:115` |
| `POST` | `/api/campaigns/{id}/jam` | Read-write role | Record an attributed spec revision for a campaign. Body includes `spec_content`, optional `reason`, and optional agent/model attribution fields. | `pkg/dashboard/api.go:116` |
| `GET` | `/api/campaigns/{id}/jam/threads` | Dashboard auth/session | List async Jam threads anchored to spec sections, including comments and author attribution. | `pkg/dashboard/api.go:117` |
| `POST` | `/api/campaigns/{id}/jam/threads` | Read-write role | Create a section-anchored Jam thread or add a comment to an existing thread. Body includes `section` and `body`, or `thread_id` and `body`. | `pkg/dashboard/api.go:118` |
| `POST` | `/api/campaigns/{id}/jam/agents` | Maintainer only | Invite Spektacular or a configured hive agent into a Jam thread. The agent reply is posted as an attributed thread comment and an open suggestion for human accept/reject; no spec revision is auto-applied. | `pkg/dashboard/api.go:119` |
| `GET` | `/api/campaigns/{id}/jam/suggestions` | Dashboard auth/session | List Jam suggestions with open/accepted/rejected status, proposed text, attribution and applied revision id when accepted. | `pkg/dashboard/api.go:120` |
| `POST` | `/api/campaigns/{id}/jam/suggestions` | Read-write role; maintainer for accept | Create a suggestion (`section`, `proposed_text`) or resolve it with `action: "accept"`/`"reject"` and `suggestion_id`. Accepting records a new attributed spec revision. | `pkg/dashboard/api.go:121` |
| `GET` | `/api/campaigns/{id}/jam/polls` | Dashboard auth/session | List Jam polls, advisory vote tallies and maintainer decisions stored against spec revisions. | `pkg/dashboard/api.go:122` |
| `POST` | `/api/campaigns/{id}/jam/polls` | Read-write role; maintainer for decide | Create polls (`section`, `question`, `options`), cast/update advisory votes (`action: "vote"`), or record a maintainer decision with rationale (`action: "decide"`). | `pkg/dashboard/api.go:123` |
| `GET` | `/api/campaigns/{id}/jam/project-sync` | Dashboard auth/session | Return the campaign's GitHub Projects sync configuration, last published items, last status, and retry guidance for failures. | `pkg/dashboard/api.go:124` |
| `POST` | `/api/campaigns/{id}/jam/project-sync` | Maintainer only | Enable/disable opt-in GitHub Projects sync, publish campaign specs/decisions/derived issues to the linked project, or record inbound project status updates without losing local Jam decisions. | `pkg/dashboard/api.go:125` |
| `GET` | `/api/campaigns/{id}/jam/ws` | Dashboard auth/session | Upgrade to a Jam WebSocket for live participant presence, section focus, and conflict-aware spec co-edit messages. Live edits require read-write role and stale `base_revision_id` values are rejected with a conflict message. | `pkg/dashboard/api.go:126` |
| `GET` | `/api/runs/{key}/trace` | Dashboard auth/session | Resolve `Hive-Run`, `Hive-Plan`, and `Hive-Spec` commit trailers for `?sha=...`, returning the linked plan section, spek clause, approval audit record, rationale audit entries, and typed no-linkage results when trailers are missing. | `pkg/dashboard/api.go:98` |

## Snapshots and style

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/style` | Public | Sanitized custom dashboard CSS | `pkg/dashboard/api.go:47` |
| `GET` | `/branding/custom.css` | Dashboard auth/session | Operator branding stylesheet override, read per request (see [branding](branding.md)) | `pkg/dashboard/server.go:1841` |
| `GET` | `/api/snapshot/frame-ancestors` | Public | Snapshot framing allowlist | `pkg/dashboard/api.go:87` |
| `GET` | `/api/snapshot` | Public | Snapshot data | `pkg/dashboard/api.go:88` |
| `GET` | `/snapshot` | Public | Public read-only snapshot page | `pkg/dashboard/api.go:89` |

## Auth and external accounts

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/gh-auth` | Dashboard auth/session | GitHub Auth | `pkg/dashboard/api.go:189` |
| `GET` | `/api/gh-rate-limits` | Dashboard auth/session | GitHub Rate Limits | `pkg/dashboard/api.go:190` |
| `GET` | `/api/gh-user-auth/status` | Public | GitHub User Auth Status | `pkg/dashboard/api.go:191` |
| `POST` | `/api/gh-user-auth/start` | Public | GitHub User Auth Start | `pkg/dashboard/api.go:192` |
| `POST` | `/api/gh-user-auth/poll` | Public | GitHub User Auth Poll | `pkg/dashboard/api.go:193` |
| `POST` | `/api/gh-user-auth/logout` | Public | GitHub User Auth Logout | `pkg/dashboard/api.go:194` |
| `GET` | `/api/gh-user-auth/session` | Public | GitHub User Auth Session | `pkg/dashboard/api.go:195` |
| `GET` | `/api/claude-auth/status` | Dashboard auth/session | Claude Auth Status | `pkg/dashboard/claude_auth.go:54` |
| `POST` | `/api/claude-auth/start` | Dashboard auth/session | Claude Auth Start | `pkg/dashboard/claude_auth.go:55` |
| `POST` | `/api/claude-auth/exchange` | Dashboard auth/session | Claude Auth Exchange | `pkg/dashboard/claude_auth.go:56` |
| `POST` | `/api/claude-auth/logout` | Dashboard auth/session | Claude Auth Logout | `pkg/dashboard/claude_auth.go:57` |
| `GET` | `/api/copilot-auth/status` | Dashboard auth/session | Copilot Auth Status | `pkg/dashboard/copilot_auth.go:84` |
| `POST` | `/api/copilot-auth/start` | Owner only | Copilot Auth Start | `pkg/dashboard/copilot_auth.go:85` |
| `POST` | `/api/copilot-auth/logout` | Owner only | Copilot Auth Logout | `pkg/dashboard/copilot_auth.go:87` |
| `GET` | `/api/openrouter/connect/start` | Dashboard auth/session | Open Router Start | `pkg/dashboard/openrouter.go:45` |
| `GET` | `/api/openrouter/qr` | Dashboard auth/session | Open Router QR | `pkg/dashboard/openrouter.go:46` |
| `GET` | `/api/openrouter/models` | Dashboard auth/session | Open Router Models | `pkg/dashboard/openrouter.go:47` |
| `GET` | `/api/openrouter/credit` | Dashboard auth/session | Open Router Credit | `pkg/dashboard/openrouter.go:48` |
| `GET` | `/openrouter/callback` | Public | Open Router Callback | `pkg/dashboard/openrouter.go:49` |
| `POST` | `/api/github-app/recheck` | Dashboard auth/session | GitHub App Recheck | `pkg/dashboard/server.go:1781` |
| `POST` | `/api/github-app/install-clicked` | Dashboard auth/session | GitHub App Install Clicked | `pkg/dashboard/server.go:1782` |
| `GET` | `/gh-setup` | Public | GitHub App Setup Callback | `pkg/dashboard/server.go:1783` |

## Configuration

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/themes` | Dashboard auth/session | Embedded dashboard/contributor theme catalog with swatches for Settings → Appearance; `?scope=contributor` filters profile themes | `pkg/dashboard/api.go:48` |
| `GET` | `/api/theme.css` | Dashboard auth/session | Effective dashboard theme stylesheet with strong ETag / 304 support; `?theme=<id>` previews an embedded preset; `?scope=contributor` emits contributor token aliases and layers hive custom CSS | `pkg/dashboard/api.go:49` |
| `GET` | `/api/config` | Dashboard auth/session | Config | `pkg/dashboard/api.go:50` |
| `GET` | `/api/config/dashboard/theme` | Owner only | Dashboard theme selection, overrides, effective theme, and built-in catalog | `pkg/dashboard/api.go:57` |
| `PUT` | `/api/config/dashboard/theme` | Owner only | Persist `dashboard.theme` and `dashboard.theme_overrides` with validation, CSS/background guardrails, and audit logging | `pkg/dashboard/api.go:58` |
| `GET` | `/api/config/download` | Owner only | Config Download | `pkg/dashboard/api.go:51` |
| `GET` | `/api/config/sla` | Owner only | Delivery SLA targets: `{targets:{start_to_pr_h,pr_to_merge_h,open_to_merge_h}, effective_targets:{...}, defaults:{...}}`, hours, from `sla.targets` in `hive.yaml` with defaults 4/24/72 | `pkg/dashboard/api.go:271` |
| `PUT` | `/api/config/sla` | Owner only | Replace `sla.targets` (`{targets:{start_to_pr_h,pr_to_merge_h,open_to_merge_h}}`; each 0 (default) to 8760, validated before anything is persisted); the Throughput Delivery SLA panel recolours on the next status poll | `pkg/dashboard/api.go:272` |
| `GET` | `/api/config/export` | Owner only | Redacted JSON export of the effective config, layer files, side files, and provenance | `pkg/dashboard/api_config_export.go` |
| `GET` | `/api/config/provenance` | Owner only | Config Provenance | `pkg/dashboard/api.go:54` |
| `GET` | `/api/config/variables` | Dashboard auth/session | Variables List | `pkg/dashboard/api.go:55` |
| `GET` | `/api/config/authorized-users` | Dashboard auth/session | Authorized Users List | `pkg/dashboard/api.go:56` |
| `PUT` | `/api/config/variables/{name}` | Owner only | Variable Upsert | `pkg/dashboard/api.go:59` |
| `DELETE` | `/api/config/variables/{name}` | Owner only | Variable Delete | `pkg/dashboard/api.go:60` |

| `GET` | `/api/config/agent/{name}` | Dashboard auth/session | Agent Config Get | `pkg/dashboard/api.go:203` |
| `PUT` | `/api/config/agent/{name}/general` | Owner only | Agent Config General | `pkg/dashboard/api.go:204` |
| `PUT` | `/api/config/agent/{name}/cadences` | Owner only | Agent Config Cadences | `pkg/dashboard/api.go:205` |
| `PUT` | `/api/config/agent/{name}/models` | Owner only | Agent Config Models | `pkg/dashboard/api.go:206` |
| `PUT` | `/api/config/agent/{name}/pipeline` | Owner only | Agent Config Pipeline | `pkg/dashboard/api.go:207` |
| `PUT` | `/api/config/agent/{name}/hooks` | Owner only | Agent Config Hooks | `pkg/dashboard/api.go:208` |
| `PUT` | `/api/config/agent/{name}/restrictions` | Owner only | Agent Config Restrictions | `pkg/dashboard/api.go:209` |
| `PUT` | `/api/config/agent/{name}/stats` | Owner only | Agent Config Stats | `pkg/dashboard/api.go:210` |
| `GET` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt | `pkg/dashboard/api.go:211` |
| `PUT` | `/api/config/agent/{name}/prompt` | Owner only | Agent Prompt Save | `pkg/dashboard/api.go:212` |
| `GET` | `/api/config/agent/{name}/export` | Dashboard auth/session | Agent Export | `pkg/dashboard/api.go:213` |
| `PUT` | `/api/config/agent/{name}/channels` | Owner only | Agent Config Channels | `pkg/dashboard/api.go:214` |
| `PUT` | `/api/config/agent/{name}/tools` | Owner only | Agent Config Tools | `pkg/dashboard/api.go:215` |
| `PUT` | `/api/config/agent/{name}/connections` | Owner only | Agent Config Connections | `pkg/dashboard/api.go:216` |
| `GET` | `/api/config/agent/{name}/variables` | Dashboard auth/session | Per-agent Variables tab: the agent's own `${VAR}` definitions plus the hive-level ones it inherits (each flagged `overridden` when the agent redefines it) and the seed-only exec/http gate flags. Never returns values — only name, type, scope and a non-secret source hint | `pkg/dashboard/api_variables.go:298` |
| `PUT` | `/api/config/agent/{name}/variables/{var}` | Owner only | Create or update one per-agent variable (`static`/`env` only; `script`/`http` are 403 and seed-only; scope `template` or `both`; secret-looking static values rejected). Persisted on the agent entry; overrides a same-named hive-level variable in this agent's kick prompt | `pkg/dashboard/api_variables.go:299` |
| `DELETE` | `/api/config/agent/{name}/variables/{var}` | Owner only | Delete one per-agent variable (404 for inherited hive-level variables — delete those via `/api/config/variables/{name}`) | `pkg/dashboard/api_variables.go:300` |
| `GET` | `/api/config/stat-sources` | Dashboard auth/session | Stat Sources | `pkg/dashboard/api.go:217` |
| `GET` | `/api/config/governor` | Dashboard auth/session | Governor Config Get | `pkg/dashboard/api.go:219` |
| `PUT` | `/api/config/governor/sensing` | Owner only | Governor Sensing | `pkg/dashboard/api.go:222` |
| `PUT` | `/api/config/governor/thresholds` | Owner only | Governor Thresholds | `pkg/dashboard/api.go:223` |
| `PUT` | `/api/config/governor/labels` | Owner only | Governor Labels | `pkg/dashboard/api.go:230` |
| `PUT` | `/api/config/governor/budget` | Owner only | Governor Budget | `pkg/dashboard/api.go:231` |
| `PUT` | `/api/config/governor/notifications` | Owner only | Governor Notifications | `pkg/dashboard/api.go:235` |
| `PUT` | `/api/config/governor/health` | Owner only | Governor Health | `pkg/dashboard/api.go:236` |
| `PUT` | `/api/config/governor/logging` | Owner only | Governor Logging | `pkg/dashboard/api.go:252` |
| `PUT` | `/api/config/governor/attribution` | Owner only | Governor Attribution | `pkg/dashboard/api.go:253` |
| `PUT` | `/api/config/governor/hub` | Owner only | Governor Hub | `pkg/dashboard/api.go:254` |
| `PUT` | `/api/config/governor/litellm` | Owner only | Governor Lite LLM | `pkg/dashboard/api.go:255` |
| `PUT` | `/api/config/governor/trajectory` | Owner only | Governor Trajectory | `pkg/dashboard/api.go:256` |
| `GET` | `/api/config/governor/backup` | Owner only | Backup Key Status (presence + safe source label; never the key value) | `pkg/dashboard/backup_key.go` |
| `PUT` | `/api/config/governor/backup` | Owner only | Backup Key Set (64-hex AES-256 key; stored 0600, path-only in `hive.yaml`) | `pkg/dashboard/backup_key.go` |
| `DELETE` | `/api/config/governor/backup` | Owner only | Backup Key Clear (backups are refused again) | `pkg/dashboard/backup_key.go` |
| `GET` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Status | `pkg/dashboard/api.go:301` |
| `PUT` | `/api/config/governor/bob` | Owner only | Governor Bob Key | `pkg/dashboard/api.go:302` |
| `DELETE` | `/api/config/governor/bob` | Owner only | Governor Bob Key Clear | `pkg/dashboard/api.go:303` |

| `GET` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Get (`aggregate` or `per_repo`) | `pkg/dashboard/api.go:224` |
| `PUT` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Set (`cadenceScope`/`cadence_scope`: `aggregate`, `per_repo`, or empty for default) | `pkg/dashboard/api.go:225` |
| `POST` | `/api/config/governor/bob/test` | Dashboard auth/session | Governor Bob Key Test | `pkg/dashboard/api.go:305` |
| `POST` | `/api/config/governor/litellm/test` | Dashboard auth/session | Governor Lite LLMTest | `pkg/dashboard/api.go:306` |
| `GET` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways List | `pkg/dashboard/api.go:309` |
| `PUT` | `/api/config/governor/gateways` | Owner only | Governor Gateways Upsert | `pkg/dashboard/api.go:310` |
| `DELETE` | `/api/config/governor/gateways/{name}` | Owner only | Governor Gateways Delete | `pkg/dashboard/api.go:311` |
| `POST` | `/api/config/governor/gateways/{name}/test` | Dashboard auth/session | Governor Gateways Test | `pkg/dashboard/api.go:312` |
| `POST` | `/api/config/governor/gateways/discover` | Owner only | Governor Gateways Discover | `pkg/dashboard/api.go:313` |
| `POST` | `/api/config/governor/agents` | Owner only | Governor Add Agent | `pkg/dashboard/api.go:318` |
| `DELETE` | `/api/config/governor/agents/{name}` | Owner only | Governor Remove Agent | `pkg/dashboard/api.go:319` |
| `PUT` | `/api/config/governor/repos` | Owner only | Governor Repos | `pkg/dashboard/api.go:320` |
| `POST` | `/api/config/governor/repos/check-access` | Dashboard auth/session | Governor Repo Check Access | `pkg/dashboard/api.go:324` |
| `PUT` | `/api/config/github` | Owner only | Config GitHub | `pkg/dashboard/api.go:325` |
| `GET` | `/api/config/github/forge-apps` | Dashboard auth/session | Config GitHub Forge Apps | `pkg/dashboard/api.go:328` |
| `GET` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Get | `pkg/dashboard/api.go:381` |
| `PUT` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Set | `pkg/dashboard/api.go:382` |
| `GET` | `/api/config/backends` | Dashboard auth/session | Backends | `pkg/dashboard/api.go:383` |
| `GET` | `/api/compliance/status` | Merger or owner | Evaluated compliance controls for the frameworks in `compliance.frameworks` (`compliance.Report`, JSON): per control `meets`/`deviates`/`off`/`not_covered` with current vs recommended value per mapped setting, a summary, and the non-certification disclaimer — see [Compliance](compliance.md) ([#11078](https://github.com/hivecommons/hive/issues/11078)) | `pkg/dashboard/api.go:373` |
| `GET` | `/api/compliance/posture` | Merger or owner | Posture-check catalogue, effective interval / window / history retention, and the latest run (`compliance.PostureRun`: schedule/manual trigger, `triggered_by` identity, and per-check `pass`/`fail`/`skip`/`error`, detail, control ids, evidence refs); `?since=` (RFC 3339, `YYYY-MM-DD` or a look-back such as `168h`) adds every run since then, oldest first, capped at 1000 — see [Compliance](compliance.md#posture-checks-catalogue) ([#11079](https://github.com/hivecommons/hive/issues/11079)) | `pkg/dashboard/api.go:374` |
| `POST` | `/api/compliance/posture/run` | Merger or owner | Run the posture checks now, attributed to the authenticated user, and record the pass in the history; audited as `compliance_posture_run`; `409` when no framework is selected or a pass is already running ([#11079](https://github.com/hivecommons/hive/issues/11079)) | `pkg/dashboard/api.go:375` |
| `GET` | `/api/compliance/posture/history` | Owner only | Posture history for `?since=`/`?until=` (default the last 30 days) as one series per check: points (`at`, `status`), status counts, `last_fail_at` and the latest result with detail and evidence refs; at most the 1000 most recent runs — see [Compliance](compliance.md#evidence-exports-and-attestations) ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:376` |
| `GET` | `/api/compliance/export` | Owner only | Evidence download for `?since=`/`?until=`: `kind=controls` (`json`/`md`), `posture` (`json`/`csv`), `audit` (`json`), `config` (redacted effective config + sha256, `json`), `attestations` (`json`/`csv`) or `bundle` (`json`). Every format includes source, period, generator, selected profile versions, attesters, retention and a per-section SHA-256 manifest; audited as `compliance_export` with the SHA-256 of the delivered bytes ([#11081](https://github.com/hivecommons/hive/issues/11081), [#13582](https://github.com/hivecommons/hive/issues/13582)) | `pkg/dashboard/api.go:377` |
| `GET` | `/api/compliance/attestations` | Owner only | Owner attestations, newest first (`?framework=` filters) ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:378` |
| `POST` | `/api/compliance/attestations` | Owner only | Record `{"framework", "reviewed_on", "note"}` for the signed-in owner; audited as `compliance_attestation`; `201` with the attestation, `400` for an unknown framework, a bad or future date, or a note over 2000 characters ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:379` |
| `PUT` | `/api/config/governor/compliance` | Owner only | Settings → Compliance framework picker: `{"frameworks": [...]}` sets `compliance.frameworks` (empty list deselects all, absent key leaves it unchanged; unknown/duplicate ids are a `400` before any change); audited as `config_compliance`; returns the re-evaluated `compliance.Report` ([#11080](https://github.com/hivecommons/hive/issues/11080)) | `pkg/dashboard/api.go:555` |

| `GET` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Get | `pkg/dashboard/api.go:227` |
| `PUT` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Set | `pkg/dashboard/api.go:228` |
| `PUT` | `/api/config/governor/coverage-target` | Owner only | Governor Coverage Target | `pkg/dashboard/api.go:229` |
| `POST` | `/api/config/governor/budget/reset` | Owner only | Governor Budget Reset | `pkg/dashboard/api.go:232` |
| `PUT` | `/api/config/governor/watchdog` | Owner only | Governor Watchdog | `pkg/dashboard/api.go:237` |
| `GET` | `/api/config/escalation` | Owner only | Escalation Config Get | `pkg/dashboard/api.go:240` |
| `PUT` | `/api/config/escalation` | Owner only | Escalation Config Set | `pkg/dashboard/api.go:241` |
| `GET` | `/api/config/write-surface` | Owner only | Lane write allowlist (`write_surface.allowlist`, [#9587](https://github.com/hivecommons/hive/issues/9587)): `allowlist`, `ops`, `warnings` | `pkg/dashboard/api.go:551` |
| `PUT` | `/api/config/write-surface` | Owner only | Replace the lane write allowlist; `{"allowlist": {}}` clears it; unknown operation or bad lane name is a 400 with nothing changed | `pkg/dashboard/api.go:552` |
| `GET` | `/api/config/review` | Dashboard auth/session | Review Config Get | `pkg/dashboard/api.go:248` |
| `PUT` | `/api/config/review` | Owner only | Review Config Set | `pkg/dashboard/api.go:251` |
| `PUT` | `/api/config/governor/features` | Owner only | Governor Features | `pkg/dashboard/api.go:257` |
| `GET` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Get | `pkg/dashboard/api.go:258` |
| `PUT` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Set | `pkg/dashboard/api.go:259` |
| `GET` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Get | `pkg/dashboard/api.go:263` |
| `PUT` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Set | `pkg/dashboard/api.go:264` |
| `GET` | `/api/config/convergence` | Owner only | Convergence Config Get | `pkg/dashboard/api.go:269` |
| `PUT` | `/api/config/convergence` | Owner only | Convergence Config Set | `pkg/dashboard/api.go:270` |
| `GET` | `/api/config/governor/advisory` | Owner only | Governor Advisory Get | `pkg/dashboard/api.go:274` |
| `PUT` | `/api/config/governor/advisory` | Owner only | Governor Advisory Set | `pkg/dashboard/api.go:275` |
| `GET` | `/api/config/governor/replan` | Owner only | Governor Replan Get | `pkg/dashboard/api.go:276` |
| `PUT` | `/api/config/governor/replan` | Owner only | Governor Replan Set | `pkg/dashboard/api.go:277` |
| `GET` | `/api/config/governor/work-source` | Owner only | Governor Work Source Get (Jira secrets/TLS fields reported as set/unset only) | `pkg/dashboard/api.go:287` |
| `PUT` | `/api/config/governor/work-source` | Owner only | Governor Work Source Set (including Jira Data Center TLS settings) | `pkg/dashboard/api.go:288` |
| `PUT` | `/api/config/governor/security` | Owner only | Governor Security | `pkg/dashboard/api.go:289` |
| `GET` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Get | `pkg/dashboard/api.go:291` |
| `PUT` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Set | `pkg/dashboard/api.go:292` |
| `GET` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Get | `pkg/dashboard/api.go:307` |
| `PUT` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Set | `pkg/dashboard/api.go:308` |
| `GET` | `/api/advisor/records` | Read-write role | Advisor Records — query `?agent=` (filter by agent), `?since=`/`?until=` (RFC3339, either omitted), or `?hours=` (lookback 1..720, alternative to since), optional `?limit=` (capped at 200 by default); returns `{records: [], active?, active_reason?}` where active/reason appear only when filtered to one agent. Records are scrubbed at the boundary. Response shape in [api_advisor.go](../pkg/dashboard/api_advisor.go#L38) | `pkg/dashboard/api.go:62` |
| `GET` | `/api/advisor/spend` | Dashboard auth/session | Advisor Spend — per-agent advisor spend over one window, alongside (not folded into) the agent's own spend. Query presets `?range=hourly|daily|weekly|monthly` (Cost section timeframes) or custom `?since=`/`?until=` (RFC3339) or `?hours=` (lookback 1..720); optionally filter `?agent=`. Returns `{range, since?, until?, total_cost_usd, agents:[]}` where range is the preset or "custom". Response shape in [api_advisor.go](../pkg/dashboard/api_advisor.go#L151) | `pkg/dashboard/api.go:63` |
| `GET` | `/api/config/advisor` | Owner only | Advisor Config Get (fleet block, model roles map, and per-agent overrides; includes effective `fleet.enabled` boolean from IsEnabled()) | `pkg/dashboard/api.go:64` |
| `PUT` | `/api/config/advisor` | Owner only | Advisor Config Set (fleet-wide settings; only fields present in body change) | `pkg/dashboard/api.go:65` |
| `PUT` | `/api/config/advisor/roles` | Owner only | Advisor Model Roles Set (replaces named model_roles map; roles in use cannot be removed) | `pkg/dashboard/api.go:66` |
| `PUT` | `/api/config/advisor/agent/{name}` | Owner only | Advisor Agent Override Set (set or clear one agent's override; null/empty clears it) | `pkg/dashboard/api.go:67` |
| `GET` | `/api/config/channels` | Owner only | Settings → Channels payload (GitHub mention/Actions triggers, chat bots, escalation sinks, read-only dashboard chat and admin MCP status, mention-eligible agents); secrets masked | `pkg/dashboard/api.go:244` |
| `PUT` | `/api/config/channels` | Owner only | Partial update of the same surfaces; validated against a copy so a 400 applies nothing; masked echo keeps a secret, empty clears it; hot-reloads mention poller and escalation sinks | `pkg/dashboard/api.go:245` |

## Agents and controls

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|

| `POST` | `/api/kick/{agent}` | Owner only | Kick — asynchronous; answers `202` once queued (see below) | `pkg/dashboard/api.go:148` |
| `GET` | `/api/kick/{agent}/status` | Dashboard auth/session | Outcome of the most recent kick | `pkg/dashboard/api.go:152` |
| `POST` | `/api/switch/{agent}/{backend}` | Owner only | Switch | `pkg/dashboard/api.go:153` |
| `POST` | `/api/model/{agent}/{model}` | Owner only | Model Set | `pkg/dashboard/api.go:154` |
| `POST` | `/api/effort/{agent}/{effort}` | Owner only | Set launch-only reasoning effort after validating against the agent's live backend (including runtime override); persists config/agent overlay, restarts the session, and `default` clears the stored effort | `pkg/dashboard/api.go:155` |
| `POST` | `/api/pause/{agent}` | Owner only | Pause | `pkg/dashboard/api.go:156` |
| `POST` | `/api/resume/{agent}` | Owner only | Resume | `pkg/dashboard/api.go:157` |
| `GET` | `/api/agent-state/{agent}` | Dashboard auth/session | Agent State | `pkg/dashboard/api.go:158` |
| `GET` | `/api/breaker` | Dashboard auth/session | Breaker State | `pkg/dashboard/api.go:159` |
| `POST` | `/api/breaker/engage` | Owner only | Breaker Engage | `pkg/dashboard/api.go:160` |
| `POST` | `/api/breaker/release` | Owner only | Breaker Release | `pkg/dashboard/api.go:161` |
| `POST` | `/api/pin/{agent}/{dimension}` | Owner only | Pin | `pkg/dashboard/api.go:162` |
| `POST` | `/api/unpin/{agent}/{dimension}` | Owner only | Unpin | `pkg/dashboard/api.go:163` |
| `POST` | `/api/restart/{agent}` | Owner only | Restart | `pkg/dashboard/api.go:167` |
| `GET` | `/api/model-advisor` | Dashboard auth/session | Model Advisor | `pkg/dashboard/api.go:184` |
| `GET` | `/api/governor/pr-models` | Dashboard auth/session | Agent-authored PR distribution by normalized attribution model/backend for `window=7d`, `30d`, or `all`, including per-model rework stats and the top 10 most-reworked PRs | `pkg/dashboard/api.go:185` |
| `GET` | `/api/reviewer/accuracy` | Dashboard auth/session | Reviewer verdict calibration for the configured window: false-approve/false-block rates and confidence buckets per perspective and reviewer model | `pkg/dashboard/api.go:250` |
| `GET` | `/api/review/queue` | Dashboard auth/session | PR review queue: every open PR (agent and contributor, actionable and held) ranked by triage class, confidence band, CI state and age, with per-PR `reasons`; unreviewed items carry `pending_since` and `redispatch_count` from the review dispatch state; each entry carries `stage` `{stage, entered_at, last_activity_at, quiet_for, stalled, waiting_on_hive, past_dwell, history[]}` from the stage-label store (`stalled`/`past_dwell` follow `governor.labels.stage.dwell` and `stalled.require_quiet`; `history[]` is `{from, to, at, reason, regressed}`, newest last, capped at 8) and the top-level `hive_held` counts PRs past their stage dwell while waiting on the hive (`waiting_on_hive` and `past_dwell` both true); paged with `limit` (1-200, default 50) and `offset` | `pkg/dashboard/api.go:526` |
| `POST` | `/api/review/queue/{owner}/{repo}/{number}/superseded/close` | Owner only | Close a superseded PR now (grace window or not): posts the close comment, adds `hive/superseded`, closes it | `pkg/dashboard/api.go:529` |
| `POST` | `/api/review/queue/{owner}/{repo}/{number}/superseded/keep-open` | Owner only | Keep a superseded PR open: adds `hive/keep-open`, which the supersession sweep never auto-closes | `pkg/dashboard/api.go:530` |
| `GET` | `/api/review/pipeline` | Dashboard auth/session | Review pipeline: one card per review-queue PR placed in its stage (`unreviewed`, `reviewing`, `changes_requested`, `fixing`, `human_hold`, `approved`, `merged`, `abandoned`) with `since`, `reviewers`, P0–P3 `severity` counts, `loop_count`/`loop_cap`, `next_action` and `reasons`; filtered by `repo` and `stage`, paged with `limit` (1-200, default 50) and `offset` | `pkg/dashboard/api.go:534` |
| `GET` | `/api/review/dispatch/events` | Dashboard auth/session | Webhook review dispatch queue: `enabled`, `webhooks_configured`, `debounce_s`, `pending` (soonest first) and `recent` (newest first) dispatches with repo, number, head SHA, action, timestamps and coalesced count, plus `stats` counters; in memory, never calls GitHub | `pkg/dashboard/api_review_event_dispatch.go:16` |
| `GET` | `/api/review/evidence` | Merger or owner | [Review evidence bundle](review-evidence.md) for one PR head. Query `repo` (`owner/name`), `number`, optional `head` (full SHA or unique prefix of at least 7 characters; `409` when ambiguous; default is the newest head), `format=json\|zip` (default `json`) and `download=1` for an attachment. `json` returns the bundle bytes exactly as sealed (verdicts, posted reviews, policy, CI check runs, sentinel findings, human actions, merge event, `hash`, optional `signature`); `zip` adds `verdicts.json`, `review-links.json` and a `manifest.json` of SHA-256s plus the signing public key when readable. `400` for a bad `repo`/`number`; `404` with `status: "expired"`/`expired: true` when retention removed a bundle Hive can prove existed, otherwise `status: "not_found"` | `pkg/dashboard/api.go:538` |
| `GET` | `/api/review/evidence/list` | Merger or owner | Every retained evidence bundle id and head for one PR (`repo`, `number`), oldest first, with the newest marked latest; `400` for a bad `repo`/`number` | `pkg/dashboard/api.go:539` |
| `GET` | `/api/search` | Merger or owner | Portfolio search over this spoke's local index (no hub fan-out). Query `q` (full text), `kind` and `repo` (`owner/name`; both repeatable or comma-separated), `from` (inclusive) and `to` (exclusive; a `YYYY-MM-DD` date covers that day) as RFC 3339 or `YYYY-MM-DD`, and `limit` (1–100, default 20); `probe=1` runs no query and is not audited, answering `204` when the index is ready. Returns `total`, highlighted `hits` (snippet plus byte `spans`) and `facets` by `kind`, `repo`, `verdict` and `sentinel.rule`; `coverage` names repos whose index could not be read. Hit text passes through ioscan, so injected text appears only as a visible redaction marker (`redacted: true`). The query string, actor and role are audited as `search_query`; result bodies are not. `404` unless `search.enabled`; `400` for bad parameters and for the reserved `semantic=true`; `503` while the index is not yet available | `pkg/dashboard/api.go:542` |
| `GET` | `/api/evidence/write-counters` | Merger or owner | Durable attempted vs blocked agent GitHub write counters ([#12069](https://github.com/hivecommons/hive/issues/12069)): `totals` by intent tier (ACMM mode) with `blocked_by_reason`, and `weeks` keyed by the UTC Monday of each ISO week with per-tier and per-reason counts. Persisted across restarts; observation only, never gates a write or merge. Empty `totals`/`weeks` before anything is recorded | `pkg/dashboard/api.go:544` |
| `GET` | `/api/evidence/summary` | Dashboard auth/session | Read-only aggregate over `/data/evidence` bundles (schema v1) plus reviewer accuracy. Query `hours` (default 24, capped like `/api/pr-throughput`; `0` = all time) or a custom `from`/`to` range (RFC3339 or `YYYY-MM-DD`, date-only `to` inclusive, `to` defaults to now; `400` when `to` precedes `from`, the span exceeds 3 × 366 days, or `to` is given without `from`; the response echoes `from`/`to`), `repo` (`owner/name`) and `author_kind=human\|agent\|bot`; `400` for a bad value. A PR falls in the window by its open time (earliest bundle `created_at` or `merge.opened_at`), never by when a bundle was written; all bundles of a PR are folded so the newest supplies verdicts and the merge / `reverted_by` / backfilled close come from whichever record knows them. Returns `available_windows` (`{"24","168","720","all"}` → PR count under the same filters), `outcomes_by_author_kind` (merged/closed/open; `closed` comes from backfilled records, `reverted_14d` is null until bundles record it), `reviewer_calibration` buckets per perspective, `reviewer_accuracy_by_model` split by author kind (blocks split into `enforced_blocks` / `advisory_blocks` by `policy.require_approval`; advisory blocks that preceded a merge are `dissent_before_merge`, not false blocks), `sentinel_hits_per_week`, `shadow_gate` (`n`, `would_have_held`, `would_have_blocked`, and per gate `review_gate` / `intent` the evaluated `n` with `held` / `passed` outcomes `{n, merged, closed, open, reverted}`, plus `review_gate_by_lane` `{merge_eligible, self_merge}`), `write_counters` (per intent tier `{attempted, blocked, blocked_by_reason}` from the proxy write counter file; omitted when absent or unreadable), `write_activity` (blocked ÷ every agent write in the window: `total_writes`, `blocked`, `direct` `{recorded, since, partial_coverage, attempted, blocked, by_tier, blocked_by_reason}` from the proxy's weekly counter buckets under `/data/hive-metrics/write-counters.json`, `mediated` `{n, refused, by_action}` from the audit log), `claim_vs_verdict` (per ISO week of agent-authored PRs: `n`, `claimed`, `ci_captured`, `ci_not_green`, `verdict_not_approve`, `both`; records with empty CI are excluded from the CI column and disclosed under `missing.records_empty_ci`), `ci_recovery` (`policy` `{self_authored_automerge, required_checks}`, `n`, `at_claim` `{green, red, pending, none}`, `red` `{n, recovered_merged, green_open, abandoned, pending, outcome_unknown, enriched, not_enriched, median_pushes_to_green, median_hours_to_green, same_head_recovered}`, `gate` `{merged_n, with_checks, green, green_from_bundles, non_required_red, required_unknown, breaches, breach_prs[], suspected, suspected_prs[], pending, unknown, green_to_merge_n, median_green_to_merge_hours}` over every merged PR by GitHub-confirmed state of the *required* checks on the merged head, as the merge gate judges (`breach_prs[]` carry `red`, `merged_by`; a red non-required check is disclosed, not a breach; a red review-time snapshot, a required check with no run, or a red read without a resolvable required set is only suspected), and `convergence` `{n, passes {"1","2","3","4+","never"}, settled, in_flight, unknown, median_passes, from_github, from_bundles, by_week[]}` over agent PRs by CI passes to first green, from the durable `/data/hive-metrics/ci-recovery.json` GitHub check-run trail rollup), `work_balance` (per ISO week: `n`, `opened` by actor kind `{human, agent, bot}`, `closed`, `prs_opened`, `prs_merged`, `prs_closed`, `queue_issues`, `queue_prs`, `issue_source` `github|audit`, `throttled`) with `work_balance_sources` (`issues_github_since`, `issues_fetched_at`, `issues_known`, `audit_only_weeks`, `issue_series_weeks`, `queue_samples`, `queue_retention_days`; issues from the durable GitHub rollup `/data/hive-metrics/issue-activity.json` for weeks at or after `issues_github_since` and, for earlier weeks, from the backfilled issue series `<evidence-root>/<owner>/<repo>/issues/<YYYY-Www>.json` written by the issue backfill ([#12527](https://github.com/hivecommons/hive/pull/12527)) when its coverage spans the requested portion of the week (`issue_series_weeks` counts the repo-weeks it supplied), else the audit log (`audit_only_weeks`), PRs from folded bundles, queue depth from governor snapshots; `missing.issues_human_opens_unobserved` is true while no GitHub rollup exists unless the issue series covers every row, and whenever any row fell back to the audit log, `missing.throttle_windows_unavailable` discloses that no governor throttle windows are reported), `intent_counters` (per intent tier `{attempted, blocked}` from `/data/hive-metrics/intent-counters.json` — evaluations and the subset the intent gate would have blocked with enforcement on, [#12362](https://github.com/hivecommons/hive/issues/12362); omitted when absent or unreadable), `complexity_by_repo` (per managed repo the newest 12 monthly complexity snapshots, oldest first, each `{month, ref, captured_at, files, functions, mean_cyclomatic, median_cyclomatic, p90_cyclomatic, max_cyclomatic, functions_over_threshold, threshold, static_warnings, static_warnings_by_tool}`; omitted when no repo has a snapshot), the `/api/reviewer/accuracy` summary, and `n` plus `missing` counts (unknown-model verdicts, empty CI, unreadable records). Never consulted by merge decisions. | `pkg/dashboard/api.go:547` |
| `GET` | `/api/repos/{owner}/{repo}/health` | Dashboard auth/session | Report-only repo health score from the search index (`search.enabled`; `503` when off, `404` until the repo has indexed review evidence, `400` for a bad owner/repo). Recomputed on every index rebuild. Returns `score` (0–100, or `null` when no component is reported), `status`, `window_days` (90), `min_n` (10, the hub `minEvidenceN` rule), `computed_at` and `components[]` in order `ci_pass_rate`, `time_to_merge`, `revert_followup_rate`, `review_verdict_mix`, `stale_issue_ratio`, `signoff_rate`, `dependency_freshness`, `security_doc_rot`, each `{name, status, score (0–1), weight, contribution, n, inputs, reason}`. A component with `n` below `min_n` or without local data is `not_reported` (never zero) and the remaining weights are renormalised; weights and ages come from `search.health`. Spoke-only; never an ACMM input and never read by merge decisions or Governor priority. | `pkg/dashboard/api.go:548` |
| `GET` | `/api/agents` | Dashboard auth/session | Agents List | `pkg/dashboard/api.go:330` |
| `POST` | `/api/agents` | Owner only | Agent Create | `pkg/dashboard/api.go:331` |
| `POST` | `/api/agents/import` | Owner only | Agent Import | `pkg/dashboard/api.go:332` |
| `DELETE` | `/api/agents/{name}` | Owner only | Agent Delete | `pkg/dashboard/api.go:333` |

| `GET` | `/api/agents/{name}/log` | Dashboard auth/session | Agent Full Log | `pkg/dashboard/api.go:135` |
| `GET` | `/api/agents/{name}/terminal-urls` | Dashboard auth/session | Agent Terminal URLs | `pkg/dashboard/api.go:139` |
| `GET` | `/api/agents/{name}/kicks` | Dashboard auth/session | Agent Kick Log List | `pkg/dashboard/api.go:142` |
| `GET` | `/api/agents/{name}/kicks/{id}` | Dashboard auth/session | Agent Kick Log Get | `pkg/dashboard/api.go:143` |
| `GET` | `/agents/{name}/kicks` | Dashboard auth/session | Agent Kick History Page (HTML) | `pkg/dashboard/api.go:144` |
| `POST` | `/api/agents/{name}/login-code` | Owner only | Agent Login Code | `pkg/dashboard/api.go:166` |

### Kick is asynchronous

`POST /api/kick/{agent}` queues the message and returns immediately with `202`; it is not a delivery confirmation.

Delivery has to wait for the agent's CLI to present its input prompt, which is bounded by `inputPromptTimeout` (120s). Doing that wait on the request path made the handler outlive a typical 60s ingress idle timeout, so a proxy answered `504` for kicks that had in fact succeeded — the prompt was typed, the agent ran the session, and the operator was told it failed (kubestellar/hive#5325). Retrying a false failure delivered the prompt twice.

The contract is now:

- **`400`** — a genuine, deterministic precondition failure evaluated inline: unknown agent, paused, stopped, no tmux session, sandbox kick rejected, prompt over 10000 chars.
- **`422`** — operator-supplied prompt rejected by ioscan for critical injection while its effective fail mode is closed (explicit `fail_mode: closed`, or the ACMM L5/L6 default when unset).
- **`202` with `status: "deferred"`** — an auto-generated manual kick was requested before the first governor scan populated the actionable-work snapshot. The kick is deduplicated per agent and delivered after that scan finishes.
- **`202` with `status: "queued"`** — accepted; a background delivery started.
- **`202` with `status: "in-flight"`** — a delivery for this agent was already running, so this call was deduplicated. Delivery is exactly-once per agent, which is what makes an operator's retry harmless.

Read the result from `GET /api/kick/{agent}/status`, which returns `status` of `unknown`, `in-flight`, `delivered`, or `failed`, plus a `pending` boolean. While `pending` is true the outcome is **indeterminate** — the prompt may still be delivered — and clients must not render it as a failure. A CLI that never reaches its input prompt within `inputPromptTimeout` settles as `failed` with a reason.

## Packs and ACMM

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/packs` | Dashboard auth/session | Packs List | `pkg/dashboard/api.go:335` |
| `POST` | `/api/packs/{level}/apply` | Owner only | Pack Apply | `pkg/dashboard/api.go:336` |
| `PUT` | `/api/packs/level` | Owner only | Set the ACMM level. Body `{"level": N}` persists and applies the pack; optional `{"release_level_holds": true}` performs a deliberate one-off release of Hive App level-applied `hold` labels for the transition. Level holds are otherwise never auto-released. When the change makes the L6 self-authored merge sweep eligible, the success body adds `self_merge_sweep_active`, `level_changed_at`, `level_holds_pending: [{repo, number, title, url, agent}]`, and `repos: [{repo, auto_merge}]` so the dashboard can show the owner an informational notice and repo toggles. | `pkg/dashboard/api.go:337` |
| `POST` | `/api/repos/rescan` | Dashboard auth/session | Re-enumerate watched repositories' open issues/PRs for the dashboard Projects cards. The scan is read-only with respect to agents/governor actions, collapses concurrent requests, and debounces repeated presses for 30 seconds (`repoRescanDebounce`) so the button cannot hammer GitHub | `pkg/dashboard/api.go`, `pkg/dashboard/api_repos_rescan.go` |
| `POST` | `/api/repos/pause` | Owner | Quiet ONE repository without stopping the hive: agents stop writing to it and stop being handed work on it, while it keeps its dashboard card and ACMM eval. Body `{"repo": "...", "reason": "..."}` — the repo travels in the body because a `project.repos` entry may be an explicit cross-org `owner/name`. Records who/when/why; `changed: false` marks a no-op re-pause and leaves the original provenance intact; `persisted: false` means the pause is in force but could not be written to config. Rejects a repo outside `project.repos`. See [per-repo agent pause](repo-pause.md) | `pkg/dashboard/api_repo_pause.go` |
| `POST` | `/api/repos/resume` | Owner | Lift a repository's pause. Unlike pause, does not require the repo to be in `project.repos`, so a pause left behind by a removed repo can still be cleared | `pkg/dashboard/api_repo_pause.go` |
| `GET` | `/api/repos/pauses` | Dashboard auth/session | Every repo pause with its provenance. Read-only — the same state is already on the repository cards | `pkg/dashboard/api_repo_pause.go` |


| `GET` | `/api/repos/{owner}/{repo}/hold-permission` | Dashboard auth/session | Reports whether the signed-in user can toggle hold chips for a repository card. Owners are allowed; otherwise the server checks GitHub `repos/{owner}/{repo}/collaborators/{user}/permission` and briefly caches the effective permission | `pkg/dashboard/api.go:355`, `pkg/dashboard/api_repo_hold.go` |
| `GET` | `/api/repos/{owner}/{repo}/items/{number}` | Dashboard session | Per-item detail for the activity monitor's decision badge: `{repo, number, stage, stage_slug, operator_actions[], operator_action_count, last_operator_action}`. `operator_actions[]` is the server-side ledger of every row action submitted from the dashboard (hold/release, decision, decide-delegated, send-to-human, accept-triage — recorded by `recordOperatorAction` around those routes) as `{action, actor, at, note, target, outcome: pending|applied|no-op|failed, detail, head_sha, stage}`; `last_operator_action` adds the derived `state` (`waiting|addressed|stuck|pending|failed`), `stuck_after_seconds` and `followed[]` (the stage hops after the action). The same `last_operator_action` / `operator_action_count` ride every stage-tracker row and linked PR. Persisted in `/data/operator-actions.json`; `HIVE_OPERATOR_ACTION_STUCK_AFTER` (default `30m`) sets the stuck window. | `pkg/dashboard/api.go`, `pkg/dashboard/operator_actions.go` |
| `POST` | `/api/repos/{owner}/{repo}/items/{number}/hold` | Owner or repo write/maintain/admin | Add or remove a dashboard hold chip for an issue or PR. Body `{"held": true}` adds the hive's exact-match canonical `hive-pause/<hive-id>` label; `{"held": false}` removes exactly the labels causing the hold (`hive-pause/<hive-id>` and/or generic hold labels) and never removes `hive/<hive-id>` provenance. Every toggle writes an audit-log entry; the #8851 upgrade migration writes `/data/hive-hold-migration-<hive-id>.json` with audit-backed, provenance-only, and ambiguous legacy `hive/<hive-id>` classifications. | `pkg/dashboard/api.go:359`, `pkg/dashboard/api_repo_hold.go` |
| `POST` | `/api/repos/{owner}/{repo}/items/{number}/decision` | Owner or repo write/maintain/admin, signed-in GitHub user | Record an operator decision from the activity monitor's row menu / selection bar. Body `{"decision": "approve"|"direction"|"close-superseded"|"close-wontfix", "comment": "…", "type": "issue"|"pr", "superseded_by": "#458"}`. On an issue parked at 045, `approve`/`direction` act exactly as a maintainer's `/hive approve` / `/hive decision <text>` comment would (labels, `approved-direction`, stage-engine Unparked fact, reply comment naming the operator). On a PR parked at 085 (`type: "pr"`), `approve` posts the `/lgtm` and `/approve` commands as two comments attributed to the operator, records the approval against the head as the human answer to a `requires_human` / decision-requested verdict (the lanes and the stage engine honour it like a trusted merger's Approve review), lifts `needs-human`, clears the sentinel label and acknowledges the bundle's sentinel findings for that head, re-derives the stage and drops the tracker cache — the response adds `released` (`approval-recorded`, `needs-human`, `sentinel-label`, `sentinel-acknowledged`) and `stage`; the merge gate decides from there (no review as the operator, no merge) — and `direction` records the text as a human change request against the head — the `/hive decision` fix lane — on a hive-authored PR only (409 otherwise). The close variants post a comment naming the operator (and the superseding PR when `superseded_by` is given) and close the item as not planned. Never merges. Every decision writes an audit-log entry (`repo_item_decision_*`). | `pkg/dashboard/api.go:360`, `pkg/dashboard/api_repo_item_decision.go` |
| `POST` / `GET` | `/api/repos/{owner}/{repo}/items/{number}/decide-delegated` | Same as `…/hold` (owner or repo write), signed-in GitHub user; `decisions.delegate.enabled` (403 when off) | *Let hive decide*: enqueue a delegated-decision job in which the hive decides the parked item itself as the owner's delegate under `pkg/delegatedecision.PolicyText` and records it as a human decision (`/hive decision` comment with the `<!-- hive:delegated-decision … -->` marker, park labels off, `triage/accepted`, queue approval on a PR head). Body `{"type": "issue"|"pr", "reason", "caption", "sentinel_reason"}`. `POST` answers 202 with the job (`{id, status: "running"|"done"|"failed", decision: {verdict, rule, text, rationale, actions, keeps_park}, notes, error}`); 409 with the running job while one is in flight; `GET` reads it. Rails: never merges, refuses items wearing `hold`, never grants secrets/permissions (`needs-owner` + `hold` instead), recommends only on hive policy/governance questions (keeps 085). Audited as `repo_item_decision_delegated[_failed]`. See [dashboard-sections.md](dashboard-sections.md#let-hive-decide). | `pkg/dashboard/api.go:329`, `pkg/dashboard/api_repo_item_decide_delegated.go` |
| `POST` | `/api/repos/{owner}/{repo}/items/{number}/accept-triage` | Owner or repo write/maintain/admin | Accept triage on an issue from the activity monitor's row menu — what a maintainer accepting triage on GitHub does by hand: adds the reporter-trust triage label (`triage/accepted`, or the first of `issue_filter.reporter_trust.untrusted_require_labels`; the label is created in the repo if missing) and removes the awaiting label (`needs-triage` / `reporter_trust.awaiting_label`) and a Hive-written `stage/015-triage-needed`. Empty body. Returns `{ok, repo, number, acceptLabel, added, removed, alreadyAccepted, minStatusSeq}`; an issue already carrying the accept label with nothing left to remove is a no-op (`alreadyAccepted: true`, no write, no audit). 404 for a pull request or an unknown item. The status snapshot's labels are patched so the next reconcile reads 010 Actionable before the engine's own 015 → 010 derivation. Every write is audited as `repo_item_accept_triage` under the dashboard user. | `pkg/dashboard/api.go:571`, `pkg/dashboard/api_repo_item_accept_triage.go` |
| `GET` | `/api/acmm/evaluation` | Dashboard auth/session | ACMMEvaluation — combined codebase + operational result, cached server-side for 1 hour (`acmmEvalTTL`). `?refresh=1` (the dashboard's "🔄 Re-evaluate" button, #5877) bypasses the hourly TTL but is debounced server-side: requests within 1 minute of the last evaluation (`acmmRefreshDebounce`) still serve the cache, since a full refresh costs up to ~29 GitHub GetContents calls per repo. The response's `last_evaluated_at` timestamp reports when the cached evaluation was computed | `pkg/dashboard/api.go:368`, `pkg/dashboard/api_acmm_eval.go` |
| `POST` | `/api/acmm/issue` | Owner only | ACMMCreate Issue — files on GitHub or, with `governor.acmm.issue_tracker: work_source` / body `tracker: "work_source"` on a Linear-sourced hive, on Linear; response `tracker` says which. See [ACMM policy matrix](acmm-policy-matrix.md#where-acmm-gap-issues-are-filed) | `pkg/dashboard/api.go:369` |
| `GET` | `/api/acmm-recommendation` | Dashboard auth/session | Advisory level-up recommendation (`acmmadvisor.Recommendation`, JSON): never changes the applied level — see [ACMM advisor](acmm-advisor.md) | `pkg/dashboard/api.go:370` |
| `GET` | `/api/hive-advice` | Dashboard auth/session | Weekly owner advice (`hiveadvisor.Result`, JSON) shared with the advisory digest: recommendations with `items`, `links` (Overview export paths) and `bands`, and on the needs-human advice `why` (plain-language reasons with counts) and `help`; `bands[]` rule text; `counts` naming the governor actionable queue vs. the Overview total. Which recommendations show is frozen for 7 days; their numbers refresh every call — see [Owner advice](advisory.md#owner-advice) | `pkg/dashboard/api.go:372`, `pkg/dashboard/api_hive_advice.go` |
| `POST` | `/api/repos/{owner}/{repo}/issues/{number}/release` | Owner or repo write/maintain/admin | Execute the typed `kind=release` owner step the hub proposed on a parked issue, with the same live preconditions as `/hive approve` ([#12635](https://github.com/hivecommons/hive/issues/12635)). Tag and target are resolved from the trusted owner-step, never from the request. Returns 503 when the GitHub client or config is unavailable; 409 when the repository is `label_driven` (the same refusal `/hive approve` gets there), when `release_on_approve` is off, or when a live precondition fails (`error` carries the refusal reason, which is also posted on the issue); 502 when the release write itself fails. Success returns `{ok, repo, number, tag, url, minStatusSeq}` plus `warning` if the follow-up issue comment failed after the release was created. | `pkg/dashboard/api.go:362`, `pkg/dashboard/api_issue_release.go` |

## Cost, tokens, telemetry

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/trends` | Dashboard auth/session | Trends | `pkg/dashboard/api.go:91` |
| `GET` | `/api/token-access` | Dashboard auth/session | Token Access | `pkg/dashboard/api.go:170` |
| `GET` | `/api/tokens` | Dashboard auth/session | Tokens | `pkg/dashboard/api.go:171` |
| `GET` | `/api/cost` | Dashboard auth/session | Cost; `merged_prs` / `closed_issues` come from the webhook-incremented ForgeState counters reconciled hourly against search, with `count_stale`, `count_stale_since` and `count_source` (`forgestate` / `search`) when last-good counts are being served (#13376), `count_partial` when only some configured repos have an attributed baseline yet, and `count_error` naming why the latest attributed refresh failed or was partial (the reconcile asks GitHub about a budgeted slice of repos per pass so a many-repo hive stays inside search's rate limit); `daily_rollup` lists per-UTC-day estimated spend (`day`, `cost_usd`, `input_tokens`, `output_tokens`, `samples`) kept for ~2 years so the Cost chart can reach past the 30-day snapshot ring; `recording` reports `agent_models`, `unrecorded_models`, `recent_models`, `last_priced_growth_at`, `last_agent_model_growth_at`, `stale_after_ms` and `stale` so the card can flag stale live usage recording (#13485) | `pkg/dashboard/api.go:172` |
| `GET` | `/api/repo-activity` | Dashboard auth/session | Per-repo audited activity | `pkg/dashboard/api.go` |
| `GET` | `/api/repo-cost` | Dashboard auth/session | Per-repo estimated token cost (interval join, cached) | `pkg/dashboard/api.go` |
| `GET` | `/api/launch-cost` | Dashboard auth/session | Per-issue / PR / head-SHA estimated token cost from kick launch attribution (cached; `?ref=owner/repo#N` narrows) | `pkg/dashboard/api.go` |
| `GET` | `/api/launch-cost/history` | Dashboard auth/session | Daily per-ref rollup of launch attribution (`daily_rollup[]`: `day`, `by_ref[]` with `ref`, `kind`, `launches`, `share_usd`), kept ~2 years on the `/data` PVC so cost per PR can be measured past the `/api/launch-cost` window | `pkg/dashboard/api.go` |
| `GET` | `/api/cost/history` | Dashboard auth/session | Cost History | `pkg/dashboard/api.go:178` |
| `GET` | `/api/trend/history` | Dashboard auth/session | Trend History; entries recorded since #13646 carry `yourTurnRootIssues` and `yourTurnRootPrs` | `pkg/dashboard/api.go:181` |
| `GET` | `/api/timeseries` | Dashboard auth/session | Time Series | `pkg/dashboard/api.go:183` |
| `GET` | `/api/providers/headroom` | Dashboard auth/session | Providers Headroom | `pkg/dashboard/api.go:68` |
| `GET` | `/api/budget/history` | Dashboard auth/session | Budget History | `pkg/dashboard/api.go:180` |

## Knowledge

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/knowledge` | Dashboard auth/session | Paginated knowledge facts: `type` filter, `tag` (comma-separated; a fact must carry every tag), `state` (comma-separated lifecycle states or `all`; only facts in those states are listed, and any state other than `approved` requires a verified owner and returns 403 otherwise), `limit` (default 50, max 200; invalid/non-positive values use the default), `offset` (default 0). Returns `enabled`, `facts`, `count` (this page), `total`, `limit`, `offset`, `has_more`; ordered by layer then slug. Offsets are not snapshot cursors: concurrent edits may shift pages. | `pkg/dashboard/api.go:388` |
| `GET` | `/api/knowledge/export` | Dashboard auth/session | Knowledge Export | `pkg/dashboard/api.go:389` |
| `POST` | `/mcp/knowledge` | Public (read-only) when the dashboard setting or `HIVE_PUBLIC_KNOWLEDGE` is on; 404 otherwise | Public Knowledge MCP endpoint — `initialize`, `tools/list`, `tools/call` for `knowledge_search`/`knowledge_toc`/`knowledge_get`/`knowledge_export` ([docs](public-knowledge-mcp.md)) | `pkg/dashboard/api.go:397` |
| `GET` | `/api/knowledge/search` | Dashboard auth/session | Knowledge Search | `pkg/dashboard/api.go:390` |
| `GET` | `/api/knowledge/toc` | Dashboard auth/session | Scoped knowledge table of contents: capped, body-free entries (`id`, `title`, `type`, `layer`, `repo`, `tags`, `status`, `confidence`, `updated`, `size_bytes`, `source`) plus `total`/`returned`/`truncated`. Query: `layers`, `repos`, `types`, `tags` (comma-separated), `limit` (default 50, max 200), `include_states` (operator opt-in for `draft`/`deprecated`/`superseded`/`all`), `format=prompt` with `max_chars` for a bounded markdown rendering, `agent` to apply that agent's `knowledge.agent_scopes` entry (the other parameters can only narrow it). Approved knowledge only by default ([docs](knowledge-toc.md)) | `pkg/dashboard/api.go:391` |
| `GET` | `/api/knowledge/effective` | Dashboard auth/session | Effective knowledge for one agent (`agent` required): the agent's `knowledge.agent_scopes` entry (unrestricted when it has none) intersected with the request scope (approved only unless `include_states` is set). Returns `scope` (resolved scope applied, with `configured`), `included` TOC entries, `excluded` entries each with a `reason` (`lifecycle`, `layer`, `repo`, `type` or `tag` — first failing check, in that order), `included_total`/`excluded_total`/`included_returned`/`excluded_returned` and `truncated`; `limit` caps each list (default 50, max 200) ([docs](knowledge-toc.md#effective-knowledge)) | `pkg/dashboard/api.go:392` |
| `GET` | `/api/knowledge/entry/{id}` | Dashboard auth/session | Full read of one knowledge entry: `markdown` (front-matter plus body) and the fact. Takes the same scope, `include_states` and `agent` query as the table of contents and returns 404 for an entry outside it ([docs](knowledge-toc.md)) | `pkg/dashboard/api.go:393` |
| `GET` | `/api/knowledge/public` | Dashboard auth/session | Public Knowledge MCP sharing status (`enabled`, `tags`, `source`, endpoint URL) | `pkg/dashboard/api.go:398` |
| `PUT` | `/api/knowledge/public` | Owner only | Persist public Knowledge MCP sharing setting; overrides env vars until changed | `pkg/dashboard/api.go:399` |
| `GET` | `/api/knowledge/health` | Dashboard auth/session | Knowledge Health | `pkg/dashboard/api.go:400` |
| `GET` | `/api/knowledge/stats` | Dashboard auth/session | Knowledge Stats | `pkg/dashboard/api.go:401` |
| `GET` | `/api/knowledge/graph` | Dashboard auth/session | Knowledge Graph | `pkg/dashboard/api.go:402` |
| `GET` | `/api/knowledge/fact-history` | Dashboard auth/session | Fact History | `pkg/dashboard/api.go:403` |
| `POST` | `/api/knowledge/create` | Dashboard auth/session | Knowledge Create | `pkg/dashboard/api.go:404` |
| `POST` | `/api/knowledge/import` | Dashboard auth/session | Knowledge Import | `pkg/dashboard/api.go:405` |
| `POST` | `/api/knowledge/promote` | Dashboard auth/session | Knowledge Promote | `pkg/dashboard/api.go:411` |
| `GET` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs List | `pkg/dashboard/api.go:412` |
| `POST` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Add | `pkg/dashboard/api.go:413` |
| `DELETE` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Remove | `pkg/dashboard/api.go:414` |
| `PUT` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Update | `pkg/dashboard/api.go:415` |
| `DELETE` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Delete | `pkg/dashboard/api.go:416` |
| `GET` | `/api/knowledge/{layer}` | Dashboard auth/session | Knowledge Layer | `pkg/dashboard/api.go:417` |
| `GET` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Fact | `pkg/dashboard/api.go:418` |
| `PUT` | `/api/knowledge/enabled` | Owner only | Knowledge Toggle | `pkg/dashboard/api.go:419` |
| `GET` | `/api/knowledge/bead-synthesizer` | Dashboard auth/session | Bead Synth Status | `pkg/dashboard/api.go:420` |
| `PUT` | `/api/knowledge/bead-synthesizer/enabled` | Owner only | Bead Synth Toggle | `pkg/dashboard/api.go:421` |
| `GET` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults List | `pkg/dashboard/api.go:422` |
| `POST` | `/api/knowledge/vaults` | Owner only | Vaults Connect | `pkg/dashboard/api.go:423` |
| `DELETE` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Disconnect | `pkg/dashboard/api.go:424` |
| `POST` | `/api/knowledge/vaults/reindex` | Dashboard auth/session | Vaults Reindex | `pkg/dashboard/api.go:425` |
| `GET` | `/api/knowledge/vaults/{name}/facts` | Dashboard auth/session | Vault Facts | `pkg/dashboard/api.go:426` |
| `GET` | `/api/config/knowledge/connectors` | Dashboard auth/session | Knowledge connectors config (name, type, layer, scope, env/file auth reference) and registered types | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/connectors` | Owner only | Replace `knowledge.connectors` after validation; inline secrets rejected | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/connectors/status` | Dashboard auth/session | Per-connector status pill, last sync, pages/facts, last error, plus the `publish` mirror status | `pkg/dashboard/api.go` |
| `POST` | `/api/config/knowledge/connectors/validate` | Owner only | Dry-run validate one connector entry (no network) | `pkg/dashboard/api.go` |
| `POST` | `/api/config/knowledge/connectors/{name}/sync` | Owner only | Start an immediate background sync of one connector | `pkg/dashboard/api.go` |
| `GET` | `/api/config/knowledge/agent-scopes` | Dashboard auth/session | Per-agent knowledge scopes (`knowledge.agent_scopes`: layers, repos, types, tags, include_states) ([docs](knowledge-toc.md#per-agent-scopes)) | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/agent-scopes` | Owner only | Replace `knowledge.agent_scopes` after validating layer and state names; persisted and audited as `config_knowledge_agent_scopes` | `pkg/dashboard/api.go` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:436` |
| `POST` | `/api/knowledge/git-sources` | Owner only | Git Sources Connect | `pkg/dashboard/api.go:437` |
| `DELETE` | `/api/knowledge/git-sources` | Owner only | Git Sources Disconnect | `pkg/dashboard/api.go:438` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:439` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:440` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:441` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:442` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:443` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:444` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:445` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:446` |
| `GET` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channels List | `pkg/dashboard/api.go:409` |
| `POST` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channel Create | `pkg/dashboard/api.go:410` |
| `GET` | `/api/config/knowledge/pipeline` | Owner only | Scalar bead synthesizer, curator, code map and primer settings, plus the read-only engine, vault paths and primer merge strategy ([docs](settings.md#knowledge)) | `pkg/dashboard/api.go` |
| `PUT` | `/api/config/knowledge/pipeline` | Owner only | Update those settings after `config.ValidateKnowledgePipeline`; 0 or empty restores the default; the bead synthesizer switch applies at once, the rest after a restart; audited as `config_knowledge_pipeline` | `pkg/dashboard/api.go` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:436` |
| `POST` | `/api/knowledge/git-sources` | Owner only | Git Sources Connect | `pkg/dashboard/api.go:437` |
| `DELETE` | `/api/knowledge/git-sources` | Owner only | Git Sources Disconnect | `pkg/dashboard/api.go:438` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:439` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:440` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:441` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:442` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:443` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:444` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:445` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:446` |
| `GET` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channels List | `pkg/dashboard/api.go:409` |
| `POST` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channel Create | `pkg/dashboard/api.go:410` |
| `GET` | `/api/knowledge/connectors` | Dashboard auth/session | Knowledge connector statuses and publish mirror status (`{"connectors": [...], "publish": {...}\|null}`, see [knowledge-connectors.md](knowledge-connectors.md)) | `pkg/dashboard/api_knowledge_connectors.go:23` |
| `POST` | `/api/knowledge/connectors/{name}/sync` | Owner only | Start one connector sync now (202; 404 unknown, 409 running) | `pkg/dashboard/api_knowledge_connectors.go:24` |
| `POST` | `/api/knowledge/publish/sync` | Owner only | Run a publish mirror batch now (202 with publish status; 503 mirror not running, 409 batch in progress) | `pkg/dashboard/api_knowledge_connectors.go:25` |

## Contribute

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/contribute` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:165` |
| `GET` | `/contribute/{tab}` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:171` |
| `GET` | `/api/contribute/ws` | Public | s.contribute Hub.Handle WS | `pkg/dashboard/api_contribute.go:179` |
| `POST` | `/api/contribute/register` | Public | Contribute Register | `pkg/dashboard/api_contribute.go:180` |
| `POST` | `/api/contribute/invite` | Public | Contribute Invite | `pkg/dashboard/api_contribute.go:186` |
| `POST` | `/api/contribute/reissue-token` | Public | Contribute Reissue Token | `pkg/dashboard/api_contribute.go:187` |
| `GET` | `/api/contribute/status` | Public | Contribute Status | `pkg/dashboard/api_contribute.go:189` |
| `GET` | `/api/contribute/repositories` | Public | Contribution-eligible repositories for a contributor `repo_scope`: `{repositories:[{id,forge,host,full_name}],generated_at}`, the monitored repos minus `hub.disabled_repos`; `id` is `{forge}/{host}/{owner}/{name}` lower-cased ([#12805](https://github.com/hivecommons/hive/issues/12805)) | `pkg/dashboard/api_contribute.go:192` |
| `GET` | `/api/contribute/activity` | Public | Contribute Activity | `pkg/dashboard/api_contribute.go:196` |
| `GET` | `/api/contribute/fleet` | Public | Contribute Fleet | `pkg/dashboard/api_contribute.go:197` |
| `GET` | `/api/contribute/events` | Public | Contribute Events (SSE). Every frame carries a monotonic `seq`; a connection whose channel filled gets a `gap` frame naming how many events it missed, so a long-lived client can tell "nothing happened" from "I missed events" ([#6218](https://github.com/hivecommons/hive/issues/6218)) | `pkg/dashboard/api_contribute.go:201` |
| `GET` | `/api/contribute/queue` | Public | Contribute Queue | `pkg/dashboard/api_contribute.go:204` |
| `GET` | `/api/contribute/opportunistic` | Public | Contribute Opportunistic | `pkg/dashboard/api_contribute.go:208` |
| `GET` | `/api/contribute/limits` | Public | Contribute Limits | `pkg/dashboard/api_contribute.go:213` |
| `GET` | `/api/contribute/metrics` | Public | Contribute Metrics | `pkg/dashboard/api_contribute.go:219` |
| `GET` | `/api/contribute/me` | Public path; caller identity resolved server-side (401 anonymous; a signed-in caller with no profile on this hive gets 200 with `has_profile:false` and no counters, [#12111](https://github.com/hivecommons/hive/issues/12111)) | The caller's OWN contribution stats — issues worked (24h and all-time), completions that produced a verified PR (24h and all-time, [#7894](https://github.com/hivecommons/hive/issues/7894)), and failures. No username parameter, so it only ever answers for its caller ([#6543](https://github.com/hivecommons/hive/issues/6543)) | `pkg/dashboard/api_contribute.go:229` |
| `GET` | `/api/contribute/run-stats/models` | Public | Per-model run evidence for the contributor wall: runs, verified-PR share, and failure rate over the selected window. | `pkg/dashboard/api_contribute.go` |
| `GET` | `/api/contribute/wall` | Public | Contributor wall posts when the hive-wide wall switch is enabled; anonymous readers see visible posts, authors can still see their own hidden posts. | `pkg/dashboard/api_contribute.go` |
| `POST` | `/api/contribute/wall` | Signed-in contributor | Create a 500-character plain-text wall post or one-level reply, with optional `model`, `backend`, and `repo` tags. Returns 401 anonymous, 403 when disabled/revoked/muted, and 429 when rate-limited. | `pkg/dashboard/api_contribute.go` |
| `DELETE` | `/api/contribute/wall/{id}` | Post author | Author deletes their own wall post; action is audit logged. | `pkg/dashboard/api_contribute.go` |
| `POST` | `/api/contribute/wall/{id}/hide` | Owner/read-write | Hide a wall post from public readers while leaving it visible to the author; action is audit logged and pushed over `/api/contribute/events`. | `pkg/dashboard/api_contribute.go` |
| `POST` | `/api/contribute/wall/{id}/flag` | Signed-in contributor | Flag a wall post for Management review; action is audit logged. | `pkg/dashboard/api_contribute.go` |
| `GET` | `/api/contribute/triage` | Public | Contribute Triage | `pkg/dashboard/api_contribute.go:255` |
| `PUT` | `/api/contribute/queue/order` | Public | Contribute Queue Order | `pkg/dashboard/api_contribute.go:259` |
| `POST` | `/api/contribute/queue/hold` | Public | Contribute Queue Hold | `pkg/dashboard/api_contribute.go:264` |
| `POST` | `/api/contribute/queue/hold/clear` | Public | Contribute Queue Hold Clear | `pkg/dashboard/api_contribute.go:268` |
| `GET` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:281` |
| `PUT` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:282` |
| `GET` | `/api/contributors` | Dashboard auth/session | Contributors List | `pkg/dashboard/api_contribute.go:290` |
| `GET` | `/api/contributors/{id}` | Dashboard auth/session | Contributor Get | `pkg/dashboard/api_contribute.go:291` |
| `POST` | `/api/contributors/{id}/wall-mute` | Owner only | Mute or unmute a contributor from posting to the contributor wall; action is audit logged. | `pkg/dashboard/api_contribute.go` |
| `PUT` | `/api/contributors/{id}/trust` | Owner only | Contributor Trust | `pkg/dashboard/api_contribute.go:293` |
| `PUT` | `/api/contributors/{id}/agent-role` | Owner only | Contributor Agent Role | `pkg/dashboard/api_contribute.go:294` |
| `PUT` | `/api/contributors/{id}/agent-role-grants` | Owner only | Contributor Agent Role Grants | `pkg/dashboard/api_contribute.go:295` |
| `POST` | `/api/contributors/{id}/revoke` | Owner only | Contributor Revoke | `pkg/dashboard/api_contribute.go:296` |
| `POST` | `/api/contributors/{id}/requeue` | Dashboard auth/session | Contributor Requeue | `pkg/dashboard/api_contribute.go:297` |
| `DELETE` | `/api/contributors/{id}` | Owner only | Contributor Delete | `pkg/dashboard/api_contribute.go:298` |
| `GET` | `/contribute/dossier/{username}` | Public | Contributor Dossier Page (HTML) | `pkg/dashboard/api_contribute.go:178` |
| `GET` | `/api/contribute/run-stats` | Public | Contribute Run Stats | `pkg/dashboard/api_contribute.go:242` |
| `GET` | `/api/contribute/runs` | Public | Contribute Per-Run History | `pkg/dashboard/api_contribute.go:339` |
| `GET` | `/api/contribute/decisions` | Owner/read-write | Contribute Hub Decisions | `pkg/dashboard/api_contribute.go:349` |
| `GET` | `/api/contribute/dossier` | Public | Contribute Dossier Get | `pkg/dashboard/api_contribute.go:286` |
| `POST` | `/api/contribute/dossier` | Public | Contribute Dossier Update | `pkg/dashboard/api_contribute.go:287` |
| `GET` | `/api/leaderboard/contributor/{username}/heraldry` | Public | Contributor Heraldry | `pkg/dashboard/api_contribute.go:319` |
| `GET` | `/api/cards/player/{asset}` | Public | OpenAPI alias for a cacheable contributor social-card SVG (`asset` is `<github-login>.svg`). | `pkg/dashboard/social_cards.go:112` |
| `GET` | `/api/cards/achievement/{login}/{asset}` | Public | OpenAPI alias for a cacheable attained-achievement SVG (`asset` is `<achievement-id>.svg`). | `pkg/dashboard/social_cards.go:113` |
| `GET` | `/api/cards/leaderboard/{asset}` | Public | OpenAPI alias for a cacheable leaderboard-summary SVG (`contributors.svg`, `teams.svg`, or `swarm.svg`). | `pkg/dashboard/social_cards.go:114` |
| `GET` | `/cards/player/{asset}` | Public | Cacheable 1200×630 SVG social card for one public contributor profile. | `pkg/dashboard/social_cards.go:109` |
| `GET` | `/cards/achievement/{login}/{asset}` | Public | Cacheable SVG achievement-unlocked card for an attained Achievement System 2.0 badge. | `pkg/dashboard/social_cards.go:110` |
| `GET` | `/cards/leaderboard/{asset}` | Public | Cacheable SVG summary card for `contributors`, `teams`, or `swarm`. | `pkg/dashboard/social_cards.go:111` |
| `GET` | `/share/player/{login}` | Public | Share landing page with Open Graph/Twitter metadata for a contributor card and link to the public dossier. | `pkg/dashboard/social_cards.go:115` |
| `GET` | `/share/achievement/{login}/{achievementID}` | Public | Share landing page with metadata for an unlocked achievement card and link to the public dossier. | `pkg/dashboard/social_cards.go:116` |
| `GET` | `/share/leaderboard/{board}` | Public | Share landing page with metadata for a public leaderboard card and link to live standings. | `pkg/dashboard/social_cards.go:117` |
| `PUT` | `/api/contribute/help-links` | Owner/read-write | Contribute Help Links | `pkg/dashboard/api_contribute.go:195` |
| `GET` | `/api/contribute/operators/message` | Public path; caller identity resolved server-side (401 anonymous, 403 without a profile) | Returns only the signed-in contributor's unacknowledged operator messages; no username/body field can select another recipient ([#8461](https://github.com/hivecommons/hive/issues/8461)). | `pkg/dashboard/api_contribute.go:235` |
| `POST` | `/api/contribute/operators/message` | Owner/read-write | Sends a sanitized, length-capped one-to-one operator note to a contributor profile, persists it until acknowledgement, and pushes it to matching live relay sockets only ([#8461](https://github.com/hivecommons/hive/issues/8461)). | `pkg/dashboard/api_contribute.go:236` |
| `POST` | `/api/contribute/operators/message/ack` | Public path; caller identity resolved server-side | Acknowledges the signed-in contributor's own message, optionally recording a short sanitized reply for operators. | `pkg/dashboard/api_contribute.go:237` |

### Issue claims (hivecommons/hive#8380)

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/claims` | Dashboard auth | Every live worker claim (`{enabled, claims:[…]}`); `enabled:false` when `claims.enabled: false` | `pkg/dashboard/claims_api.go:53` |
| `GET` | `/api/claims/{owner}/{repo}/{number}` | Dashboard auth | `{held, claim}` for one issue | `pkg/dashboard/claims_api.go:54` |
| `POST` | `/api/claims/{owner}/{repo}/{number}` | Signed-in / hub-proxied / GitHub-token caller claims as a **human** under their login; a verified owner token claims as the owner | Body `{force, ttl_s, session}`. 200 with `outcome` `claimed`/`renewed`/`taken_over`; 409 with `outcome` `held` (same rank, retry with `force`) or `refused` (higher rank holds it) and a `hint` | `pkg/dashboard/claims_api.go:55` |
| `DELETE` | `/api/claims/{owner}/{repo}/{number}` | Same identity rules | Body `{force, reason}`. Releases your claim or a lower-ranked holder's; `force` with a verified owner role releases regardless of holder | `pkg/dashboard/claims_api.go:56` |

### `/api/v1` contributor subpaths

`handleAPIv1` dispatches authenticated contributor API calls below `/api/v1`.
Every request must carry a GitHub personal access token in the `Authorization`
header, using either the `Bearer <token>` scheme (hosted clients) or the
legacy `token <token>` scheme (what `gh auth token` and older hive CLIs
send). Both are accepted; the legacy scheme is retained for backward
compatibility. Credentials in the query string (`?token=`) are NOT supported
and are rejected, because query strings land in ingress and access logs. The
same rule applies to the shared dashboard owner token: send it in the
`Authorization` header, not in URLs. Dashboard terminal links use
`/api/terminal/handoff` to mint a ≤60-second single-use `code` for the browser
navigation instead of putting the owner token in the terminal URL.

Authorization: every `/api/v1` path except `/api/v1/me` additionally requires
the caller to be in the hive's authorized-users allowlist (any role), and
returns `403` otherwise — contributor, activity and knowledge data are
hive-private, not world-readable. `/api/v1/me` is exempt because it only
returns the caller's own profile. Client-supplied `X-Hive-User`, `X-Hive-Role`
and owner-verified headers are stripped at the top of the handler; identity is
always resolved server-side from the validated token.

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET`/`POST` | `/api/v1/status` | GitHub token + allowlist | Contributor status summary | `pkg/dashboard/api_contribute.go:1528` |
| `GET`/`POST` | `/api/v1/queue` | GitHub token + allowlist | Paginated ready-work listing (`?limit=<int>&offset=<int>`) over the actionable/offerable backlog | `pkg/dashboard/api_contribute.go:1524` |
| `GET`/`POST` | `/api/v1/activity` | GitHub token + allowlist | Contributor activity feed | `pkg/dashboard/api_contribute.go:1530` |
| `GET`/`POST` | `/api/v1/contributors` | GitHub token + allowlist | Contributor list | `pkg/dashboard/api_contribute.go:1532` |
| `GET`/`POST` | `/api/v1/knowledge` | GitHub token + allowlist | Knowledge export | `pkg/dashboard/api_contribute.go:1534` |
| `GET`/`POST` | `/api/v1/me` | GitHub token (self-scoped, no allowlist) | Current contributor profile | `pkg/dashboard/api_contribute.go:1536` |
| `POST` | `/api/v1/prs/{owner}/{repo}/{number}/queue-automerge` | GitHub token + merger/owner role (POST only; GET returns 405) | Queue PR auto-merge using the validated actor and current PR head | `pkg/dashboard/api_contribute.go` |
## Nous / Experiment Lab

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/nous/status` | Dashboard auth/session | Nous Status — includes `experimentLoop` (`dir`, `lastWriteAt`, `running`, `idleAfterSeconds`): whether the external experiment loop, the only producer of proposals, has written under `/var/run/nous/governor` within 24 evaluation cycles; ready cards then carry `experimentLoopIdle` ([#12541](https://github.com/hivecommons/hive/issues/12541)) | `pkg/dashboard/api.go:481` |
| `GET` | `/api/nous/ledger` | Dashboard auth/session | Nous Ledger | `pkg/dashboard/api.go:482` |
| `GET` | `/api/nous/principles` | Dashboard auth/session | Nous Principles | `pkg/dashboard/api.go:483` |
| `POST` | `/api/nous/approve` | Owner only | Nous Approve | `pkg/dashboard/api.go:484` |
| `POST` | `/api/nous/abort` | Owner only | Nous Abort | `pkg/dashboard/api.go:485` |
| `PUT` | `/api/nous/mode` | Owner only | Nous Mode | `pkg/dashboard/api.go:487` |
| `PUT` | `/api/nous/scope` | Owner only | Nous Scope | `pkg/dashboard/api.go:488` |
| `GET` | `/api/nous/phase` | Dashboard auth/session | Nous Phase | `pkg/dashboard/api.go:489` |
| `PUT` | `/api/nous/gate-decision` | Owner only | Nous Gate Decision | `pkg/dashboard/api.go:490` |
| `GET` | `/api/nous/gate-pending` | Dashboard auth/session | Nous Gate Pending | `pkg/dashboard/api.go:491` |
| `POST` | `/api/nous/gate-respond` | Owner only | Nous Gate Respond | `pkg/dashboard/api.go:492` |
| `GET` | `/api/nous/gate-response` | Dashboard auth/session | Nous Gate Response | `pkg/dashboard/api.go:493` |
| `GET` | `/api/nous/config` | Dashboard auth/session | Nous Config Get | `pkg/dashboard/api.go:494` |
| `PUT` | `/api/nous/config/goals` | Owner only | Nous Config Goals — legacy; the dashboard no longer exposes the research question in the ⚙ dialog, use `PATCH /api/nous/campaigns/{id}` | `pkg/dashboard/api.go:495` |
| `PUT` | `/api/nous/config/repos` | Owner only | Nous Config Repos — the allow-list of repos experiments may target (empty = any) | `pkg/dashboard/api.go:496` |
| `PUT` | `/api/nous/config/output` | Owner only | Nous Config Output | `pkg/dashboard/api.go:497` |
| `PUT` | `/api/nous/config/fast-fail` | Owner only | Nous Config Fast Fail | `pkg/dashboard/api.go:498` |
| `PUT` | `/api/nous/config/schedule` | Owner only | Nous Config Schedule | `pkg/dashboard/api.go:499` |
| `PUT` | `/api/nous/config/controllables` | Owner only | Nous Config Controllables | `pkg/dashboard/api.go:500` |
| `PUT` | `/api/nous/config/principles` | Owner only | Nous Config Principles | `pkg/dashboard/api.go:501` |
| `DELETE` | `/api/nous/principles/{id}` | Owner only | Nous Delete Principle | `pkg/dashboard/api.go:502` |
| `POST` | `/api/nous/campaign/start` | Owner only | Nous Campaign Start — archives the active campaign and starts a new experiment whose baseline restarts at 0 ([#12501](https://github.com/hivecommons/hive/issues/12501)) | `pkg/dashboard/api.go:505` |
| `GET` | `/api/nous/campaigns` | Any | Nous Campaigns List — every experiment with its per-campaign baseline, scope, results and stage ([#12532](https://github.com/hivecommons/hive/issues/12532)) | `pkg/dashboard/api.go:506` |
| `POST` | `/api/nous/campaigns` | Owner only | Nous Campaigns Create — starts an experiment alongside the running ones; 409 `scope_conflict` when a running experiment shares a repo target or both are hive-wide ([#12532](https://github.com/hivecommons/hive/issues/12532)) | `pkg/dashboard/api.go:507` |
| `PATCH` | `/api/nous/campaigns/{id}` | Owner only | Nous Campaign Patch — pause / resume / archive, or edit question, hypothesis and scope. Hypothesis/tag edits save in place (`status: "updated"`); a changed question or repo set needs `restart: true` (else 409 `restart_required`) and archives the campaign as version N while creating version N+1 with `restartedFrom` (`status: "restarted", campaign, archived`); resume, scope widening and restarts re-run the conflict guard ([#12532](https://github.com/hivecommons/hive/issues/12532)) | `pkg/dashboard/api.go:508` |
| `POST` | `/api/nous/promote` | Owner only | Nous Promote (manual verdict promotion to an issue or Spektacular run; see [experiment-lab.md](experiment-lab.md#verdict-card-and-manual-promote)) | `pkg/dashboard/api.go:486` |
| `POST` | `/api/nous/artifacts/sync` | Owner only | Nous Artifact Sync | `pkg/dashboard/api.go:503` |
| `POST` | `/api/nous/reset` | Owner only | Nous Reset (wipe baseline snapshots, learned knowledge or everything; see [experiment-lab.md](experiment-lab.md#resetting-data)) | `pkg/dashboard/api.go:504` |

## Inception

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/inception/start` | Owner only | Inception Start | `pkg/dashboard/api.go:451` |
| `POST` | `/api/inception/scan` | Owner only | Inception Scan | `pkg/dashboard/api.go:452` |
| `GET` | `/api/inception/state` | Dashboard auth/session | Inception State | `pkg/dashboard/api.go:453` |
| `POST` | `/api/inception/questions` | Owner only | Inception Set Questions | `pkg/dashboard/api.go:454` |
| `POST` | `/api/inception/answer` | Owner only | Inception Answer | `pkg/dashboard/api.go:455` |
| `POST` | `/api/inception/facts` | Owner only | Inception Record Facts | `pkg/dashboard/api.go:456` |
| `GET` | `/api/inception/scaffold` | Dashboard auth/session | Inception Scaffold | `pkg/dashboard/api.go:457` |
| `POST` | `/api/inception/approve` | Owner only | Inception Approve | `pkg/dashboard/api.go:458` |
| `POST` | `/api/inception/reset` | Owner only | Inception Reset | `pkg/dashboard/api.go:459` |
| `GET` | `/api/inception/ideation-facts` | Dashboard auth/session | Inception Ideation Facts | `pkg/dashboard/api.go:460` |
| `GET` | `/api/inception/download` | Dashboard auth/session | Inception Download | `pkg/dashboard/api.go:461` |
| `GET` | `/api/inception/has-files` | Dashboard auth/session | Inception Has Files | `pkg/dashboard/api.go:462` |
| `PUT` | `/api/inception/wiki-name` | Owner only | Inception Rename Wiki | `pkg/dashboard/api.go:463` |
| `POST` | `/api/inception/import` | Owner only | Inception Import | `pkg/dashboard/api.go:464` |

## Beads

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/beads` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:510` |
| `GET` | `/api/beads/{agent}` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:511` |
| `POST` | `/api/beads/{agent}` | Owner only | Beads Create | `pkg/dashboard/api.go:512` |
| `POST` | `/api/beads/reset` | Owner only | Beads Reset | `pkg/dashboard/api.go:513` |
| `POST` | `/api/beads/reset/{agent}` | Owner only | Beads Reset Agent | `pkg/dashboard/api.go:514` |

## Dashboard miscellaneous

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/audit` | Read-write role | Audit Log — `{"entries": [...]}` envelope, newest first, capped at 200; response shape and the serve-time `user_name` field in [audit-log.md](audit-log.md#get-apiaudit) | `pkg/dashboard/api.go:61` |
| `GET` | `/api/watchdog/activity` | Read-write role | Watchdog activity readout for the Health tab (#7254): `watchdog-*` audit actions over `?days=` (default 30, clamped to 90) — total / taken / observed / `byAction`, a zero-filled per-day `daily` histogram, per-agent `agents` liveness, and the Observe → Heal `promotion` hint; see [agent-watchdog.md](agent-watchdog.md#watchdog-activity-strip) | `pkg/dashboard/api.go:521` |
| `POST` | `/api/presence` | Dashboard auth/session | Presence | `pkg/dashboard/api.go:70` |
| `GET` | `/api/prompt-history` | Dashboard auth/session | Prompt History | `pkg/dashboard/api.go:71` |
| `POST` | `/api/self-upgrade` | Owner only | Self Upgrade | `pkg/dashboard/api.go:72` |
| `GET` | `/api/backup/status` | Owner only | Backup Status — `available:false` with a reason when no encryption key is configured | `pkg/dashboard/api.go:76` |
| `POST` | `/api/backup` | Owner only | Backup Download — `412` when no encryption key is configured (never an unencrypted archive) | `pkg/dashboard/api.go:77` |
| `POST` | `/api/banner-dismissed` | Dashboard auth/session | Banner Dismissed | `pkg/dashboard/api.go:78` |
| `GET` | `/api/history` | Dashboard auth/session | History | `pkg/dashboard/api.go:90` |
| `GET` | `/api/timeline` | Dashboard auth/session | Timeline | `pkg/dashboard/api.go:92` |
| `GET` | `/api/lifecycle-timeline` | Dashboard auth/session | Issue→PR lifecycle journeys plus derived stage timeline. Each journey keeps the dense `stages` map and adds a backwards-compatible `details` object for expandable rows: chronological stages with durations, current-stage age, agent, classification attrs, issue/PR links, blocked reason, and kick/run identifiers when recorded. | `pkg/dashboard/api.go:93` |
| `GET` | `/api/pr-throughput` | Dashboard auth/session | Throughput: pull/merge requests and issues across tracked forges. Counts hive-created PRs/issues, hive/observed reviews, observed PR merges/closures, and issue closes over `?hours=N` (default 24, capped at audit retention; `hours=0` = all time from durable counters in `/data/pr-throughput-counters.json`) with optional exact case-insensitive `?repo=owner/name` and `?role=created\|reviewed\|merged` for the actor trend. The response keeps legacy `opened`/`merged`/`closed`, `merged_by_path`, `buckets`, velocity metrics, previous-window trends, top agents/repos/reasons, and `recorded_since`, and adds `by_actor` (`pr`/`issue` × `created`/`reviewed`/`merged`/`closed` × `hive`/`human`/`other`) plus `series` points (`t`, `hive`, `human`, `other`) for the selected role. | `pkg/dashboard/api.go:94` |


| `GET` | `/api/convergence/soak` | Owner only | Convergence Soak Status | `pkg/dashboard/api.go:273` |
| `POST` | `/api/plan/from-issue` | Dashboard auth/session | Plan From Issue | `pkg/dashboard/api.go:469` |
| `GET` | `/api/plan/{epicID}` | Dashboard auth/session | Plan Tree | `pkg/dashboard/api.go:472` |
| `POST` | `/api/plan/{epicID}/approve` | Owner only | Plan Approve | `pkg/dashboard/api.go:473` |
| `POST` | `/api/plan/{epicID}/reject` | Owner only | Plan Reject | `pkg/dashboard/api.go:475` |
| `POST` | `/api/plan/{epicID}/child/{childID}` | Owner only | Plan Child Action | `pkg/dashboard/api.go:476` |
| `POST` | `/api/linear/agent/install` | Owner only | Linear Agent Install (OAuth start) | `pkg/dashboard/api_linear_agent.go:219` |
| `GET` | `/api/linear/agent/status` | Owner only | Linear Agent Status | `pkg/dashboard/api_linear_agent.go:220` |
| `POST` | `/api/linear/agent/disconnect` | Owner only | Linear Agent Disconnect | `pkg/dashboard/api_linear_agent.go:221` |
| `GET` | `/api/upstream-watch` | Owner only | Upstream-watch divergence: per watched fork the upstream followed, the watermark and last-run time, surfaced/ported/dismissed/skipped counts, and the recent upstream refs with their upstream URL, diff URL, fork issue number and state. Read-only — it reads the config and the watch state file, never GitHub. Also carries `interval`, `project_repos` and each repo's configured entry (`config`) for the editor. | `pkg/dashboard/api.go:316` |
| `PUT` | `/api/config/upstream-watch` | Owner only | Upstream Watch Config Update: edit `upstream_watch` from the Features tab. Body `{enabled?, interval?, repos?: {<repo>: {upstream?, sources?, pr_labels?, max_issues_per_run?, label?} \| null}}` — absent keys are unchanged, `null` removes a repo, keys are stored as the bare repo name. Validated with the loader's rules before the config changes (400 on failure), then persisted and audited as `config_upstream_watch`. | `pkg/dashboard/api.go:317` |

| `GET` | `/api/widget` | Dashboard auth/session | Widget | `pkg/dashboard/api.go:127` |
| `GET` | `/api/pane/{agent}` | Dashboard auth/session | Pane | `pkg/dashboard/api.go:132` |
| `GET` | `/api/role` | Dashboard auth/session | Role | `pkg/dashboard/api.go:146` |
| `POST` | `/api/reset-restarts/{agent}` | Owner only | Reset Restarts | `pkg/dashboard/api.go:168` |
| `GET` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Get | `pkg/dashboard/api.go:186` |
| `POST` | `/api/budget-ignore` | Owner only | Budget Ignore Set | `pkg/dashboard/api.go:187` |
| `GET` | `/api/summaries` | Dashboard auth/session | Summaries | `pkg/dashboard/api.go:199` |
| `GET` | `/api/stage-tracker` | Dashboard auth/session | Stage Tracker: every open issue and pull request of the watched repos for the Hive activity monitor — actionable, held, claimed, in-progress, covered-by-PR, filtered (`needs-*`), draft and exempt-labelled items alike, each with `stage`, `stage_since`, `labels`, `created_at`, `author_kind`, `modifiers`, (PRs) `sentinel_alert` / `sentinel_findings` / `sentinel_reason` when the sentinel flagged the PR, `finisher` / `finisher_verb` / `finisher_reason` (who can finish the item, from the your-turn decision: `hive`, `person` when it is the operator's turn, `elsewhere` when it waits on someone else (a hold, a covering PR, a reporter, an open blocker, or its author on a returned PR or a person's draft), absent when the snapshot has no decision; also on each `linked_prs` entry), `decision_needed` on a row parked on a person (085 / 045 / 015, the needs-human pill; mirrored onto the issue's `linked_prs[]` entry): `{question, asked_by, since, blocker, blocker_reason, options: [{action, label, enabled, reason_if_disabled}]}` — the reviewer's own decision question when the verdict record has one, the blocker in plain words, and the applicable actions under the row context menu's action ids (`decide:delegate` first), and (issues) `linked_prs`; items with no stage yet report stage `0` "Open"; `needs_human_labels` is the "a person must act" label set behind the monitor's Needs human facet (the same list as `/api/status.labelPresets[needs-human]`). Standing meta issues are omitted from the monitor but still counted in the Overview breakdown; `recently_merged` lists the PRs (`repo`, `number`, `merged_at`) whose `pr_merged` audit entry is under 30 minutes old, so a client diffing two payloads can tell a merge from a close; `merged` is the durable merged ledger's summary — `{today, total_7d, total_30d, all_time, recent: [≤20 × {repo, number, title, url, merged_at, merged_by (`hive` or a login), lane, issue}], tz, day_start}`, `today` rolling at midnight in the spoke's zone — behind the monitor's Merged chip. Served from a server-side cache rebuilt at most every `dashboard.stage_tracker.cache_ttl_s` (default `governor.eval_interval_s`) from the spoke's existing snapshot — a request never calls GitHub. Carries `generated_at`, `source` (`cache` or `fresh`), `cache_ttl_s`, an `ETag` and `Cache-Control: private, max-age=<remaining>`; `If-None-Match` yields 304. Each hit is recorded under REST caller `dashboard:stage-tracker` so `/api/gh-rate-limits` shows it charged nothing. | `pkg/dashboard/api.go:200` |
| `POST` | `/api/prs/{owner}/{repo}/{number}/queue-automerge` | Merger/owner role | Queue PRAuto Merge | `pkg/dashboard/api.go:201` |
| `GET` | `/api/inference/models/{backend}` | Dashboard auth/session | Inference Models | `pkg/dashboard/api.go:386` |
| `GET` | `/api/hive-id` | Dashboard auth/session | Hive IDGet | `pkg/dashboard/api.go:448` |
| `PUT` | `/api/hive-id` | Owner only | Hive IDSet | `pkg/dashboard/api.go:449` |
| `POST` | `/api/chat` | Dashboard auth/session | Chat. JSON body `{"query": "<text>", "history": [...]}`; `message` is accepted as an alias for `query` (`query` wins when both are set). A missing or blank field returns 400 `query is required`. `!`-prefixed commands go to the chat spine (`{"accepted":true,"seq":N}`, read-write); while an inception is in `clarify` with unanswered questions, an owner's unprefixed reply also goes to the spine, which submits it as the answer to the next unanswered question. | `pkg/dashboard/api.go:478` |
| `GET` | `/api/auth/token` | Public | Auth Token | `pkg/dashboard/api.go:516` |
| `GET` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:300` |
| `POST` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:301` |
| `GET` | `/api/docs` | Dashboard auth/session | APIDocs | `pkg/dashboard/api_contribute.go:302` |
| `GET` | `/leaderboard` | Public | Leaderboard Page | `pkg/dashboard/api_contribute.go:304` |
| `GET` | `/api/leaderboard` | Public | Leaderboard API | `pkg/dashboard/api_contribute.go:305` |
| `GET` | `/api/leaderboard/teams` | Public | Team Leaderboards | `pkg/dashboard/api_contribute.go:306` |
| `GET` | `/api/leaderboard/style` | Public | Leaderboard Style | `pkg/dashboard/api_contribute.go:307` |
| `GET` | `/api/logs/sources` | Dashboard auth/session | Log sources (#11942): every source name this hive knows (`hive`, the default, plus `proxy`, `hive-output`, `litellm` and `requests` from #11944, and `starts` and `crash` from #11945), each marked `available` or not with a `reason`; names only, no log text. Wrapped by the admin MCP `log_sources` tool; see [design/admin-mcp.md](design/admin-mcp.md#log-read) | `pkg/dashboard/api_logs.go:19` |
| `GET` | `/api/logs/read` | Read-write role (checked by the handler) | Log read (#11942): the newest matching lines of the hive's own log and its rotated, compressed history, or of another source (#11944: auth proxy output, early hive output, litellm output, dashboard request log), oldest first, as `{time, source, level, stream, text}`; `?source=&lines=&since=&until=&min_level=&include=&exclude=&cursor=`. Default 100 lines, at most 1000, 128 KiB per result, 8 KiB per line; `since`/`until` need an explicit offset; scrubbed at read time with redactions counted; `coverage`, `next_cursor`, `status` `incomplete` (stopped on the scan bound; follow `next_cursor`) / `not_available` / `cursor_expired`. Wrapped by the admin MCP `log_read` tool; see [design/admin-mcp.md](design/admin-mcp.md#log-read) | `pkg/dashboard/api_logs.go:20` |
| `GET` | `/api/diag/agents/{name}/log` | Dashboard auth/session (role of the wrapped endpoint) | Diagnostic read (#11943): wraps `GET /api/agents/{name}/log` as `{text, content_type, truncated, bytes_total, ...}` — the newest `?max_bytes=` of the scrubbed log (default 64 KiB, at most 128 KiB, clamping reported); `?explain=` passed through. Wrapped by the admin MCP `agent_log_read` tool; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:41` |
| `GET` | `/api/diag/agents/{name}/kicks` | Dashboard auth/session (role of the wrapped endpoint) | Diagnostic read (#11943): wraps `GET /api/agents/{name}/kicks`, scrubbed, newest first, oldest entries dropped (and reported in `trimmed`) to stay under 128 KiB. Wrapped by `agent_kicks_list`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:42` |
| `GET` | `/api/diag/agents/{name}/kicks/{id}` | Dashboard auth/session (role of the wrapped endpoint) | Diagnostic read (#11943): wraps `GET /api/agents/{name}/kicks/{id}` in the same text shape and bounds as the agent log. Wrapped by `agent_kick_read`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:43` |
| `GET` | `/api/diag/runs/{key}/log` | Read role (checked by the wrapped endpoint) | Diagnostic read (#11943): wraps `GET /api/runs/{key}/log` in the same text shape and bounds; `?stage=&gen=&tail=` passed through. Wrapped by `run_log_read`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:44` |
| `GET` | `/api/diag/runs/{key}/detail` | Dashboard auth/session (pane tails need read-write, as in the wrapped endpoint) | Diagnostic read (#11943): wraps `GET /api/runs/{key}/detail`, scrubbed; `raw_log` keeps its newest bytes and events/stages/prs/notes their newest entries to stay under 128 KiB, reported in `trimmed`. Wrapped by `run_detail_read`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:45` |
| `GET` | `/api/diag/health` | Public (as `/api/health/deep`) | Diagnostic read (#11943): wraps `GET /api/health/deep`, scrubbed and returned whole. Wrapped by `health_deep_read`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:46` |
| `GET` | `/api/diag/prompt-history` | Read-write role (checked by the wrapped endpoint) | Diagnostic read (#11943): wraps `GET /api/prompt-history` (`?agent=&windowHours=` passed through); every entry scrubbed, each prompt cut at 8 KiB (`cut`, `full_bytes`), newest entries kept within 128 KiB with `truncated` and `entries_total`. Wrapped by `prompt_history_read`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:47` |
| `GET` | `/api/diag/config` | Owner only, verified by this hive (dashboard token or verified owner session) | Diagnostic read (#11943): the effective configuration as `GET /api/config/provenance` computes it — every key and environment name with the layer that set it (`also_set_by` for overridden layers, rejected overlay and last-good fallback in `provenance`); values only for keys and names on the documented `value_list`, all others set/not set; shown values scrubbed; the file itself never returned; `?offset=` pages keys. Wrapped by `config_effective_read`; see [admin-mcp.md](admin-mcp.md#diagnostic-reads) | `pkg/dashboard/api_diag.go:48` |
| `GET` | `/api/leaderboard/battle-log` | Public | Scrubbed public Battle Log activity feed for leaderboard widgets. | `pkg/dashboard/battle_log.go:108` |
| `GET` | `/api/leaderboard/hive-of-week` | Public | Featured weekly project metadata, configured render URLs (empty when none), gource source URL, and embed snippets. | `pkg/dashboard/battle_log.go:109` |
| `GET` | `/api/leaderboard/gource-log` | Public | Gource custom log source for a project/week, derived from public hive event data. | `pkg/dashboard/battle_log.go:110` |
| `POST` | `/api/leaderboard/hive-of-week/archive` | Owner only | Archive the selected project's weekly gource source log into knowledge; stores source only, never rendered video. | `pkg/dashboard/battle_log.go:111` |
| `GET` | `/api/leaderboard/contributor/{username}` | Public | Contributor Profile | `pkg/dashboard/api_contribute.go:314` |
| `GET` | `/api/hives` | Dashboard auth/session | Hives List | `pkg/dashboard/api_contribute.go:325` |
| `POST` | `/api/hives/register` | Dashboard auth/session | Hives Register | `pkg/dashboard/api_contribute.go:326` |
| `POST` | `/api/hives/{id}/heartbeat` | Dashboard auth/session | Hives Heartbeat | `pkg/dashboard/api_contribute.go:327` |
| `DELETE` | `/api/hives/{id}` | Owner only | Hives Delete | `pkg/dashboard/api_contribute.go:328` |
| `POST` | `/api/hives/onboard` | Dashboard auth/session | Hives Onboard | `pkg/dashboard/api_contribute.go:329` |
| `GET` | `/sso` | Public | SSO | `pkg/dashboard/server.go:1788` |
## Hub SaaS

102 routes — see [api-reference/hub-saas.md](api-reference/hub-saas.md).

## Hub server

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/login` | Hub handler-specific | Login | `pkg/hub/oauth.go:71` |
| `GET` | `/login/{provider}` | Hub handler-specific | Per-Provider Login (github or OIDC provider) | `pkg/hub/oauth.go:74` |
| `GET` | `/api/auth/callback` | Hub handler-specific | OAuth Callback | `pkg/hub/oauth.go:75` |
| `GET` | `/api/auth/user` | Hub handler-specific | Auth User | `pkg/hub/oauth.go:76` |
| `POST` | `/api/auth/logout` | Hub handler-specific | Logout | `pkg/hub/oauth.go:77` |
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
| `GET` | `/fleet` | Hub handler-specific | My-Hives Fleet Page (static; data via `/api/saas/my-hives`) | `pkg/hub/server.go:1779` |
| `GET` | `/my-hives` | Hub handler-specific | 301 redirect to `/fleet` (query preserved) | `pkg/hub/server.go:1780` |
| `POST` | `/api/shared-agent-pool/leases` | Hub handler-specific | Acquire or renew a shared CLI session lease | `pkg/hub/server.go:1717` |
| `PUT` | `/api/shared-agent-pool/leases` | Hub handler-specific | Renew an active shared CLI session lease | `pkg/hub/server.go:1718` |
| `DELETE` | `/api/shared-agent-pool/leases` | Hub handler-specific | Release a shared CLI session lease | `pkg/hub/server.go:1719` |
| `POST` | `/api/heartbeat` | Hub handler-specific | Heartbeat | `pkg/hub/server.go:1720` |
| `POST` | `/api/task-status` | Hub handler-specific | Task Status | `pkg/hub/server.go:1721` |
| `GET` | `/api/registry` | Hub handler-specific | Registry | `pkg/hub/server.go:1729` |
| `GET` | `/api/hub/leaderboard` | Hub handler-specific | Leaderboard | `pkg/hub/server.go:1730` |
| `GET` | `/api/hub/stats` | Hub handler-specific | Stats | `pkg/hub/server.go:1731` |
| `GET` | `/api/fleet-stats` | Hub handler-specific | Fleet Stats | `pkg/hub/server.go:1732` |
| `GET` | `/api/hub/evidence` | Hub admin | Read-only cross-hive evidence drill-down from each spoke's stored heartbeat `evidence` summary ([#12116](https://github.com/hivecommons/hive/issues/12116)); non-admins get `403`. Returns `spokes`, `reporting`, pooled `calibration` (bucket 0–5 → `{n, bad, rate, lo, hi}`; the hub view folds the six buckets into four reviewer-confidence ranges), `findings[]` (the plain-language *What this says* sentences, generated from the same fields; [#12456](https://github.com/hivecommons/hive/issues/12456)), `by_acmm_level` (`level`, `hives`, `merged_90d`, `revert_rate`, and `time_to_merge` — `median_of_hive_medians_s`, `hives`, `n` — the median of the level's per-hive median time-to-merge, omitted when no hive at the level reports one; [#12464](https://github.com/hivecommons/hive/issues/12464)), `family_false_approve_rate` (`same`/`cross`/`mixed`, hives classed by their reviewer vs author family sets), fleet `record_coverage` (merged PRs with an evidence record over `prs_merged_90d`), fleet panels ([#12370](https://github.com/hivecommons/hive/issues/12370)) `outcomes_by_author_kind[]` (`kind`, `hives`, `n`, `merged`, `reverted`, `merge_rate`, `revert_rate`), `write_counters[]` (`tier`, `hives`, `attempted`, `blocked`, `block_rate`) and `shadow_gate` (`review_gate` / `intent` → `held` / `passed` outcomes with `revert_rate`), each omitted while no spoke reports it and repeated per `by_acmm_level[]` row, and per hive `hives[]` with `acmm_level`, `prs_merged_90d`, `record_coverage`, `family_mix`, false-approve / false-block / revert rates, families, a calibration series the raw `counts` behind them (including `by_author_kind`) so the drill-down can re-pool a hive / ACMM-level selection client-side, and `criteria[]`: the per-axis criteria table derived hub-side from the quadrant criteria (`axis`, `label`, `scored`, `criteria[]` with `name`, `label`, `status` `met`/`unmet`/`not_reported`, `display`/`score` when measured, `reason` otherwise; offline, evidence-less or unscored hives are `not_reported`, never `unmet`; [#12463](https://github.com/hivecommons/hive/issues/12463)). Every rate is `{n, rate, lo, hi}` with a 95% Wilson interval; `rate`/`lo`/`hi` are null when `n` < 10. Window selection ([RFC #12062](https://github.com/hivecommons/hive/issues/12062)): `?hours=24|168|720` pools each hive's matching heartbeat `windows` block instead of its 90-day block (any other or absent `hours` → 90 days); `?from=YYYY-MM-DD|RFC3339[&to=…]` (date-only `to` is inclusive, `to` defaults to now, `to` ≥ `from`, span ≤ 366 days, else `400`) is served from the smallest reported window that reaches back to `from` and flagged `window.approximate`. The response carries `window` (`hours`, `key`, `label`, `from`, `to`, `approximate`, `coverage_available` — `record_coverage` is only computed for the 90-day window because `prs_merged_90d` is a 90-day count), `window_unreported` and per-hive `window_unreported` for hives whose spoke predates per-window reporting. `?hive=<id>[,<id>…]` (repeatable) and `?level=<n>` restrict the registry server-side to those hives / that ACMM level before pooling. Hub admins open it from the Fleet quadrant card or its **🔬 Evidence** button ([#12117](https://github.com/hivecommons/hive/issues/12117)), pick a window (24h / 7d / 30d / 90d / custom), filter it by hive and ACMM level and read each section's ⓘ explainer; the per-hive author-kind revert counts behind the *Revert rate by author kind* panel are `hives[].counts.by_author_kind` ([#12326](https://github.com/hivecommons/hive/issues/12326)). Never consulted by merge decisions | `pkg/hub/evidence.go` |
| `GET` | `/api/hub/evidence/report` | Hub admin | Plain-language report of the hub evidence view ([#12456](https://github.com/hivecommons/hive/issues/12456)), rendered server-side from the same `EvidenceView` as `/api/hub/evidence` so every figure reconciles with the modal; non-admins get `403`. Same `hours=` / `from=` / `to=` window and `hive=` / `level=` filter parameters. `format=md` (default, `text/markdown`) or `format=html` (standalone page, inline CSS, no scripts or external assets, hive names escaped); any other value → `400`. `download=1` sets `Content-Disposition: attachment; filename="hive-evidence-<hive|fleet>-<from>-<to>.md|html"`, otherwise inline. Sections: Summary (the generated findings), What we measured and why, The numbers, How to read these, What is not reported yet and why, Grade per section, footer link to `review-evidence.md`. `Cache-Control: no-store`. Read-only; never consulted by merge decisions | `pkg/hub/evidence_report.go` |
| `GET` | `/api/hub/version` | Hub handler-specific | Hub Version | `pkg/hub/server.go:1737` |
| `DELETE` | `/api/hub/registry/{id}` | Hub handler-specific | Registry Delete | `pkg/hub/server.go:1747` |
| `POST` | `/api/contribute/register` | Hub handler-specific | Contribute Proxy | `pkg/hub/server.go:1748` |
| `GET` | `/api/contribute/status` | Hub handler-specific | Contribute Status | `pkg/hub/server.go:1749` |
| `GET` | `/api/contribute/ws` | Hub handler-specific | Contribute WSProxy | `pkg/hub/server.go:1750` |
| `POST` | `/api/github/webhook` | Hub handler-specific | GitHub Webhook | `pkg/hub/server.go:1751` |
| `GET` | `/gh-setup` | Hub handler-specific | GitHub App Setup Router | `pkg/hub/server.go:1752` |
| `GET` | `/learn` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1753` |
| `GET` | `/get-started` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1754` |
| `GET` | `/api/docs` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1757` |
| `GET` | `/api/reading-list` | Hub handler-specific | Reading List | `pkg/hub/server.go:1760` |
| `GET` | `/reading` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1761` |
| `GET` | `/cncf-reference-architecture` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1768` |
| `GET` | `/cncf-reference-architecture/console` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1769` |
| `GET` | `/cncf-reference-architecture/bluefin` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1770` |
| `GET` | `/{$}` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1787` |
| `GET` | `/og-card.png` | Hub handler-specific | OGCard | `pkg/hub/server.go:1792` |
| `GET` | `/` | Public | Static asset fallback (`http.FileServerFS` over the embedded `static/` tree) for any path no other route claims | `pkg/hub/server.go:1794` |
