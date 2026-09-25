# Dashboard REST API reference

Pragmatic v1 endpoint index compiled by hand from route registrations in `src/pkg/dashboard/*.go` and `src/pkg/hub/*.go` (tests excluded). There is no generator; update this page in the same change that adds or renames a route. It lists method, path, coarse auth level, and one-line purpose. Request/response schemas are intentionally not hand-written here; see the handler source for exact payloads and validation.

Auth levels are derived from dashboard middleware (`isPublicPath`, dashboard token/session auth, and the `/api/v1` GitHub-token wrapper) or from hub route wrappers such as `requireAuth` (rows marked `Hub auth`) and `requireAdmin` (rows marked `Hub admin`). Hub rows marked handler-specific have no dashboard middleware; check the named handler for bearer secrets, admin checks, or public behavior.

## Health, status, events

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/version` | Dashboard auth/session | Build/version metadata; includes `upgradeMarker` (`target`, `current`, `attempts`, `maxAttempts`, `failed`, `requestedAt`, `lastError`) while a self-upgrade is in flight or has failed ([#6765](https://github.com/hivecommons/hive/issues/6765)), and an `autoUpdate` object (`enabled`, `state` — one of `disabled`/`up_to_date`/`behind`/`retrying`/`failed`/`unknown` — `healthy`, `period`, `targetBranch`, `targetCommit`, `currentCommit`, `commitsBehind`, `lastAttemptAt`, `lastError`, `detail`) that never reports a failed or unknown update as healthy ([#6962](https://github.com/hivecommons/hive/issues/6962), [#6963](https://github.com/hivecommons/hive/issues/6963)) | `pkg/dashboard/api.go:55` |
| `POST` | `/api/release-channel` | Owner only | Hosted spoke self-service release-channel selector; relays `stable`/`candidate`/`edge` to the hub's existing switch-branch endpoint with the spoke dashboard-token proof, and reports the requested channel as pending until the Deployment image lands | `pkg/dashboard/api.go:73` |
| `GET` | `/api/health` | Public | Basic health probe | `pkg/dashboard/server.go:1117` |
| `GET` | `/api/health/deep` | Public | Deep health probe | `pkg/dashboard/server.go:1118` |
| `GET` | `/api/livez` | Public | Kubernetes liveness probe | `pkg/dashboard/server.go:1119` |
| `GET` | `/metrics` | Registered only when `HIVE_METRICS_ENABLED`; requires `Authorization: ****** (403 if the token is unset) | Prometheus metrics | `pkg/dashboard/server.go:1125` |
| `GET` | `/api/status` | Dashboard auth/session | Dashboard aggregate status; accepts `?fields=a,b` for top-level selection and `?omit=repos` to drop heavy sections, and honors `Accept-Encoding: gzip` | `pkg/dashboard/server.go:1130` |
| `GET` | `/api/status/summary` | Dashboard auth/session | Compact agent/governor/budget status summary for pollers; honors `Accept-Encoding: gzip` | `pkg/dashboard/server.go:1131` |
| `GET` | `/api/events` | Dashboard auth/session | Server-sent event stream | `pkg/dashboard/server.go:1132` |
| `GET` | `/api/swarm` | Dashboard auth/session | Current swarm status: display name, duration, active repo, expiry, persisted prep metrics when available, and idle-unlock fields. | `pkg/dashboard/swarm.go:160` |
| `POST` | `/api/swarm` | Owner only | Start a 24h repo swarm for a configured repo, returning 409 when another swarm is active and 423 when a previous swarm keeps the next swarm locked until enough agents are idle unless body `{"repo":"owner/name","force":true}` is used; saves the active record before repo prep, persists prep metrics, and announces the start to Discord when configured. | `pkg/dashboard/swarm.go:161` |
| `DELETE` | `/api/swarm` | Owner only | End the active swarm, score issues closed and PRs merged in the window, append it to history, and announce the result to Discord when configured. | `pkg/dashboard/swarm.go:162` |
| `GET` | `/api/swarm/history` | Dashboard auth/session | Completed swarm history, aggregate repo leaderboard, and top player records. | `pkg/dashboard/swarm.go:163` |
| `GET` | `/api/swarm/players` | Dashboard auth/session | Per-player swarm records and earned achievement catalog. | `pkg/dashboard/swarm.go:164` |
| `GET` | `/api/runs` | Dashboard auth/session | Active staged runs projected from task leases, plans, and lifecycle timeline. Returns an array of `Run` objects: canonical `key` (`owner/repo#number`), current-stage `lease_key`, `title`, `repo` (`owner/repo`), `stage`, `gen`, `stage_started_at`, `waiting_on` (`agent`, `remote`, `human`, `ci`, `none`), `waiting_since`, `assignee`, `last_receipt`, `plan_epic_id`, and `stages[]` (`name`, `status`, `gen`, `receipt`). | `pkg/dashboard/api.go:86` |
| `GET` | `/api/runs/audit` | Owner only | Read-only cross-repo audit query over existing retained artifacts. Query `repo`, canonical `run` (`owner/repo#number`), RFC3339 `since`/`until`, `kind`, `limit` (default 100, max 500), and `page_token`. Returns `{items,next_page_token,retention}`; each item has `source` (`audit_log`, `timeline`, `lease_receipt`, `plan_epic`), `kind`, `run`, `repo`, `at`, `actor`, `artifact_id`, `attrs`, or an explicit `kind=expired,status=expired,state=Unknown` marker when requested history may have aged out. | `pkg/dashboard/api.go:87` |
| `POST` | `/api/runs/audit` | Owner only | Activate the convergence audit campaign over a scope directory. Body `{"campaign_key":"audit-campaign","scope_dir":"/data/convergence/audit/scope","store":"audit","generation":1,"run_key":"","run_url":""}`; `scope_dir` defaults to `/data/convergence/audit/scope`, `store` defaults to `audit`, `auditor`, `scanner`, `supervisor`, then the first configured bead store. Runs inspection even when `publication.enabled` is false, but then skips the publisher and returns `publication_skipped:true` with reason `publication.disabled`. | `pkg/dashboard/api.go:88` |
| `GET` | `/api/runs/{key}/checkpoint` | Dashboard auth/session | Compact mobile/chat checkpoint review payload for a held run. Returns `run_key`, `stage`, lease `gen`, `repo`, `title`, capped plain-text `summary`, approve/reject `decisions`, dashboard deep link, `lease_gen` staleness fence, and verified-owner approver rule. | `pkg/dashboard/api.go:90` |
| `POST` | `/api/runs/{key}/checkpoint` | Owner only | Resolve a compact checkpoint decision. Body `{"action":"approve"|"reject","gen":<lease generation>}`; refuses stale generations with 409 and non-owner callers with 403 before mutating the plan. | `pkg/dashboard/api.go:91` |
| `GET` | `/api/runs/{key}` | Dashboard auth/session | One run by URL-escaped canonical run key (`owner/repo%23number`); early lease-shaped keys (`repo!run-key:stage`) and queued Wavefront run-stage keys (`owner/repo!graph:node`) are also accepted. Includes timeline-derived stage history in the same `Run` shape as `/api/runs` when a lease/timeline exists. History entries that came from an owner reset carry the owner's `reason`. When the key belongs to a wired convergence campaign, the detail response also includes an optional `burndown` block with `source`, `satisfied`, `remaining`, `unknown`, and `scope`; the list endpoint omits this block. | `pkg/dashboard/api.go:92` |
| `POST` | `/api/runs/{key}/reset` | Owner only | Move a run's lease back to an earlier stage (for example `implement` to `plan` after a rejected plan). Body `{"to": "<stage>", "reason": "<why>"}`, both required. Mints a new generation so the relay holding the old one can no longer resume, persists before answering, and records the reason on the agent audit sink, the lifecycle timeline, and the `stage_completed` hook payload (`attrs.reset = "true"`). Returns `{ok, key, stage_from, stage, gen, reason}`. 400 for the same or a later stage or a missing reason, 404 when no live staged lease holds the key, 409 when the lease expired, 500 when the registry could not be written. | `pkg/dashboard/api.go:93` |
| `GET` | `/api/campaigns` | Dashboard auth/session | List resumable campaigns from archived Inception sessions and retained Spektacular (Spek) runs. Query `search`/`q`, `repo`, `stage`, `status`, and `owner`; each campaign is keyed by stable spec/session id and includes title, source, repos, current stage/step, artifact links, linked issues/PRs, contributors, last activity, status, engine, type, and run link when applicable. | `pkg/dashboard/api.go:94` |
| `GET` | `/api/campaigns/{id}` | Dashboard auth/session | Return one campaign by URL-escaped stable campaign id or run key, using the same campaign shape as `/api/campaigns`. | `pkg/dashboard/api.go:95` |
| `POST` | `/api/campaigns/{id}/resume` | Owner only | Resume or pick up a campaign. Archived Inception campaigns take a durable campaign lease, restore their saved state and wiki working files; Spek campaigns return the retained run detail plus a CLI status command that reloads state from Spek artifacts. Body may include `{ "surface": "chat"|"contributor"|"cli"|"dashboard" }`; 409 means another user holds the lease. | `pkg/dashboard/api.go:96` |
| `POST` | `/api/campaigns/{id}/release` | Owner only | Release the caller's campaign lease and write a `campaign_release` audit entry so another user can continue the same campaign. | `pkg/dashboard/api.go:97` |
| `POST` | `/api/campaigns/{id}/revise` | Owner only | Create a linked revision campaign (`revision_of`, `revision`) for revisiting shipped or parked Inception/Spek work without overwriting the previous campaign. The new revision is leased to the caller and audit-logged as `campaign_revise`. | `pkg/dashboard/api.go:98` |
| `GET` | `/api/campaigns/{id}/jam` | Dashboard auth/session | Return the per-campaign Jam workspace: section-anchored threads, comments, suggestions, polls, poll decisions, current spec revision and attributed revision history. | `pkg/dashboard/api.go:99` |
| `POST` | `/api/campaigns/{id}/jam` | Read-write role | Record an attributed spec revision for a campaign. Body includes `spec_content`, optional `reason`, and optional agent/model attribution fields. | `pkg/dashboard/api.go:100` |
| `GET` | `/api/campaigns/{id}/jam/threads` | Dashboard auth/session | List async Jam threads anchored to spec sections, including comments and author attribution. | `pkg/dashboard/api.go:101` |
| `POST` | `/api/campaigns/{id}/jam/threads` | Read-write role | Create a section-anchored Jam thread or add a comment to an existing thread. Body includes `section` and `body`, or `thread_id` and `body`. | `pkg/dashboard/api.go:102` |
| `POST` | `/api/campaigns/{id}/jam/agents` | Maintainer only | Invite Spektacular or a configured hive agent into a Jam thread. The agent reply is posted as an attributed thread comment and an open suggestion for human accept/reject; no spec revision is auto-applied. | `pkg/dashboard/api.go:103` |
| `GET` | `/api/campaigns/{id}/jam/suggestions` | Dashboard auth/session | List Jam suggestions with open/accepted/rejected status, proposed text, attribution and applied revision id when accepted. | `pkg/dashboard/api.go:104` |
| `POST` | `/api/campaigns/{id}/jam/suggestions` | Read-write role; maintainer for accept | Create a suggestion (`section`, `proposed_text`) or resolve it with `action: "accept"`/`"reject"` and `suggestion_id`. Accepting records a new attributed spec revision. | `pkg/dashboard/api.go:105` |
| `GET` | `/api/campaigns/{id}/jam/polls` | Dashboard auth/session | List Jam polls, advisory vote tallies and maintainer decisions stored against spec revisions. | `pkg/dashboard/api.go:106` |
| `POST` | `/api/campaigns/{id}/jam/polls` | Read-write role; maintainer for decide | Create polls (`section`, `question`, `options`), cast/update advisory votes (`action: "vote"`), or record a maintainer decision with rationale (`action: "decide"`). | `pkg/dashboard/api.go:107` |
| `GET` | `/api/campaigns/{id}/jam/project-sync` | Dashboard auth/session | Return the campaign's GitHub Projects sync configuration, last published items, last status, and retry guidance for failures. | `pkg/dashboard/api.go:108` |
| `POST` | `/api/campaigns/{id}/jam/project-sync` | Maintainer only | Enable/disable opt-in GitHub Projects sync, publish campaign specs/decisions/derived issues to the linked project, or record inbound project status updates without losing local Jam decisions. | `pkg/dashboard/api.go:109` |
| `GET` | `/api/campaigns/{id}/jam/ws` | Dashboard auth/session | Upgrade to a Jam WebSocket for live participant presence, section focus, and conflict-aware spec co-edit messages. Live edits require read-write role and stale `base_revision_id` values are rejected with a conflict message. | `pkg/dashboard/api.go:110` |
| `GET` | `/api/runs/{key}/trace` | Dashboard auth/session | Resolve `Hive-Run`, `Hive-Plan`, and `Hive-Spec` commit trailers for `?sha=...`, returning the linked plan section, spek clause, approval audit record, rationale audit entries, and typed no-linkage results when trailers are missing. | `pkg/dashboard/api.go:89` |

## Snapshots and style

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/style` | Public | Sanitized custom dashboard CSS | `pkg/dashboard/api.go:56` |
| `GET` | `/branding/custom.css` | Dashboard auth/session | Operator branding stylesheet override, read per request (see [branding](branding.md)) | `pkg/dashboard/server.go:1193` |
| `GET` | `/api/snapshot/frame-ancestors` | Public | Snapshot framing allowlist | `pkg/dashboard/api.go:79` |
| `GET` | `/api/snapshot` | Public | Snapshot data | `pkg/dashboard/api.go:80` |
| `GET` | `/snapshot` | Public | Public read-only snapshot page | `pkg/dashboard/api.go:81` |

## Auth and external accounts

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/gh-auth` | Dashboard auth/session | GitHub Auth | `pkg/dashboard/api.go:165` |
| `GET` | `/api/gh-rate-limits` | Dashboard auth/session | GitHub Rate Limits | `pkg/dashboard/api.go:166` |
| `GET` | `/api/gh-user-auth/status` | Public | GitHub User Auth Status | `pkg/dashboard/api.go:167` |
| `POST` | `/api/gh-user-auth/start` | Public | GitHub User Auth Start | `pkg/dashboard/api.go:168` |
| `POST` | `/api/gh-user-auth/poll` | Public | GitHub User Auth Poll | `pkg/dashboard/api.go:169` |
| `POST` | `/api/gh-user-auth/logout` | Public | GitHub User Auth Logout | `pkg/dashboard/api.go:170` |
| `GET` | `/api/gh-user-auth/session` | Public | GitHub User Auth Session | `pkg/dashboard/api.go:171` |
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
| `POST` | `/api/github-app/recheck` | Dashboard auth/session | GitHub App Recheck | `pkg/dashboard/server.go:1133` |
| `POST` | `/api/github-app/install-clicked` | Dashboard auth/session | GitHub App Install Clicked | `pkg/dashboard/server.go:1134` |
| `GET` | `/gh-setup` | Public | GitHub App Setup Callback | `pkg/dashboard/server.go:1135` |

## Configuration

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/themes` | Dashboard auth/session | Embedded dashboard/contributor theme catalog with swatches for Settings → Appearance; `?scope=contributor` filters profile themes | `pkg/dashboard/api.go:57` |
| `GET` | `/api/theme.css` | Dashboard auth/session | Effective dashboard theme stylesheet with strong ETag / 304 support; `?theme=<id>` previews an embedded preset; `?scope=contributor` emits contributor token aliases and layers hive custom CSS | `pkg/dashboard/api.go:58` |
| `GET` | `/api/config` | Dashboard auth/session | Config | `pkg/dashboard/api.go:59` |
| `GET` | `/api/config/dashboard/theme` | Owner only | Dashboard theme selection, overrides, effective theme, and built-in catalog | `pkg/dashboard/api.go:64` |
| `PUT` | `/api/config/dashboard/theme` | Owner only | Persist `dashboard.theme` and `dashboard.theme_overrides` with validation, CSS/background guardrails, and audit logging | `pkg/dashboard/api.go:65` |
| `GET` | `/api/config/download` | Owner only | Config Download | `pkg/dashboard/api.go:60` |
| `GET` | `/api/config/provenance` | Owner only | Config Provenance | `pkg/dashboard/api.go:61` |
| `GET` | `/api/config/variables` | Dashboard auth/session | Variables List | `pkg/dashboard/api.go:62` |
| `GET` | `/api/config/authorized-users` | Dashboard auth/session | Authorized Users List | `pkg/dashboard/api.go:63` |
| `PUT` | `/api/config/variables/{name}` | Owner only | Variable Upsert | `pkg/dashboard/api.go:66` |
| `DELETE` | `/api/config/variables/{name}` | Owner only | Variable Delete | `pkg/dashboard/api.go:67` |

| `GET` | `/api/config/agent/{name}` | Dashboard auth/session | Agent Config Get | `pkg/dashboard/api.go:178` |
| `PUT` | `/api/config/agent/{name}/general` | Owner only | Agent Config General | `pkg/dashboard/api.go:179` |
| `PUT` | `/api/config/agent/{name}/cadences` | Owner only | Agent Config Cadences | `pkg/dashboard/api.go:180` |
| `PUT` | `/api/config/agent/{name}/models` | Owner only | Agent Config Models | `pkg/dashboard/api.go:181` |
| `PUT` | `/api/config/agent/{name}/pipeline` | Owner only | Agent Config Pipeline | `pkg/dashboard/api.go:182` |
| `PUT` | `/api/config/agent/{name}/hooks` | Owner only | Agent Config Hooks | `pkg/dashboard/api.go:183` |
| `PUT` | `/api/config/agent/{name}/restrictions` | Owner only | Agent Config Restrictions | `pkg/dashboard/api.go:184` |
| `PUT` | `/api/config/agent/{name}/stats` | Owner only | Agent Config Stats | `pkg/dashboard/api.go:185` |
| `GET` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt | `pkg/dashboard/api.go:186` |
| `PUT` | `/api/config/agent/{name}/prompt` | Owner only | Agent Prompt Save | `pkg/dashboard/api.go:187` |
| `GET` | `/api/config/agent/{name}/export` | Dashboard auth/session | Agent Export | `pkg/dashboard/api.go:188` |
| `PUT` | `/api/config/agent/{name}/channels` | Owner only | Agent Config Channels | `pkg/dashboard/api.go:189` |
| `PUT` | `/api/config/agent/{name}/tools` | Owner only | Agent Config Tools | `pkg/dashboard/api.go:190` |
| `PUT` | `/api/config/agent/{name}/connections` | Owner only | Agent Config Connections | `pkg/dashboard/api.go:191` |
| `GET` | `/api/config/stat-sources` | Dashboard auth/session | Stat Sources | `pkg/dashboard/api.go:192` |
| `GET` | `/api/config/governor` | Dashboard auth/session | Governor Config Get | `pkg/dashboard/api.go:194` |
| `PUT` | `/api/config/governor/sensing` | Owner only | Governor Sensing | `pkg/dashboard/api.go:195` |
| `PUT` | `/api/config/governor/thresholds` | Owner only | Governor Thresholds | `pkg/dashboard/api.go:196` |
| `PUT` | `/api/config/governor/labels` | Owner only | Governor Labels | `pkg/dashboard/api.go:201` |
| `PUT` | `/api/config/governor/budget` | Owner only | Governor Budget | `pkg/dashboard/api.go:202` |
| `PUT` | `/api/config/governor/notifications` | Owner only | Governor Notifications | `pkg/dashboard/api.go:206` |
| `PUT` | `/api/config/governor/health` | Owner only | Governor Health | `pkg/dashboard/api.go:207` |
| `PUT` | `/api/config/governor/logging` | Owner only | Governor Logging | `pkg/dashboard/api.go:219` |
| `PUT` | `/api/config/governor/attribution` | Owner only | Governor Attribution | `pkg/dashboard/api.go:220` |
| `PUT` | `/api/config/governor/hub` | Owner only | Governor Hub | `pkg/dashboard/api.go:221` |
| `PUT` | `/api/config/governor/litellm` | Owner only | Governor Lite LLM | `pkg/dashboard/api.go:222` |
| `PUT` | `/api/config/governor/trajectory` | Owner only | Governor Trajectory | `pkg/dashboard/api.go:223` |
| `GET` | `/api/config/governor/backup` | Owner only | Backup Key Status (presence + safe source label; never the key value) | `pkg/dashboard/backup_key.go` |
| `PUT` | `/api/config/governor/backup` | Owner only | Backup Key Set (64-hex AES-256 key; stored 0600, path-only in `hive.yaml`) | `pkg/dashboard/backup_key.go` |
| `DELETE` | `/api/config/governor/backup` | Owner only | Backup Key Clear (backups are refused again) | `pkg/dashboard/backup_key.go` |
| `GET` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Status | `pkg/dashboard/api.go:256` |
| `PUT` | `/api/config/governor/bob` | Owner only | Governor Bob Key | `pkg/dashboard/api.go:257` |
| `DELETE` | `/api/config/governor/bob` | Owner only | Governor Bob Key Clear | `pkg/dashboard/api.go:258` |

| `GET` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Get (`aggregate` or `per_repo`) | `pkg/dashboard/api.go:197` |
| `PUT` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Set (`cadenceScope`/`cadence_scope`: `aggregate`, `per_repo`, or empty for default) | `pkg/dashboard/api.go:198` |
| `POST` | `/api/config/governor/bob/test` | Dashboard auth/session | Governor Bob Key Test | `pkg/dashboard/api.go:260` |
| `POST` | `/api/config/governor/litellm/test` | Dashboard auth/session | Governor Lite LLMTest | `pkg/dashboard/api.go:261` |
| `GET` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways List | `pkg/dashboard/api.go:264` |
| `PUT` | `/api/config/governor/gateways` | Owner only | Governor Gateways Upsert | `pkg/dashboard/api.go:265` |
| `DELETE` | `/api/config/governor/gateways/{name}` | Owner only | Governor Gateways Delete | `pkg/dashboard/api.go:266` |
| `POST` | `/api/config/governor/gateways/{name}/test` | Dashboard auth/session | Governor Gateways Test | `pkg/dashboard/api.go:267` |
| `POST` | `/api/config/governor/gateways/discover` | Owner only | Governor Gateways Discover | `pkg/dashboard/api.go:268` |
| `POST` | `/api/config/governor/agents` | Owner only | Governor Add Agent | `pkg/dashboard/api.go:271` |
| `DELETE` | `/api/config/governor/agents/{name}` | Owner only | Governor Remove Agent | `pkg/dashboard/api.go:272` |
| `PUT` | `/api/config/governor/repos` | Owner only | Governor Repos | `pkg/dashboard/api.go:273` |
| `POST` | `/api/config/governor/repos/check-access` | Dashboard auth/session | Governor Repo Check Access | `pkg/dashboard/api.go:277` |
| `PUT` | `/api/config/github` | Owner only | Config GitHub | `pkg/dashboard/api.go:278` |
| `GET` | `/api/config/github/forge-apps` | Dashboard auth/session | Config GitHub Forge Apps | `pkg/dashboard/api.go:281` |
| `GET` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Get | `pkg/dashboard/api.go:313` |
| `PUT` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Set | `pkg/dashboard/api.go:314` |
| `GET` | `/api/config/backends` | Dashboard auth/session | Backends | `pkg/dashboard/api.go:315` |

| `GET` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Get | `pkg/dashboard/api.go:199` |
| `PUT` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Set | `pkg/dashboard/api.go:200` |
| `POST` | `/api/config/governor/budget/reset` | Owner only | Governor Budget Reset | `pkg/dashboard/api.go:203` |
| `PUT` | `/api/config/governor/watchdog` | Owner only | Governor Watchdog | `pkg/dashboard/api.go:208` |
| `GET` | `/api/config/escalation` | Owner only | Escalation Config Get | `pkg/dashboard/api.go:211` |
| `PUT` | `/api/config/escalation` | Owner only | Escalation Config Set | `pkg/dashboard/api.go:212` |
| `GET` | `/api/config/review` | Dashboard auth/session | Review Config Get | `pkg/dashboard/api.go:215` |
| `PUT` | `/api/config/review` | Owner only | Review Config Set | `pkg/dashboard/api.go:218` |
| `PUT` | `/api/config/governor/features` | Owner only | Governor Features | `pkg/dashboard/api.go:224` |
| `GET` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Get | `pkg/dashboard/api.go:225` |
| `PUT` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Set | `pkg/dashboard/api.go:226` |
| `GET` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Get | `pkg/dashboard/api.go:230` |
| `PUT` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Set | `pkg/dashboard/api.go:231` |
| `GET` | `/api/config/convergence` | Owner only | Convergence Config Get | `pkg/dashboard/api.go:236` |
| `PUT` | `/api/config/convergence` | Owner only | Convergence Config Set | `pkg/dashboard/api.go:237` |
| `GET` | `/api/config/governor/advisory` | Owner only | Governor Advisory Get | `pkg/dashboard/api.go:239` |
| `PUT` | `/api/config/governor/advisory` | Owner only | Governor Advisory Set | `pkg/dashboard/api.go:240` |
| `GET` | `/api/config/governor/replan` | Owner only | Governor Replan Get | `pkg/dashboard/api.go:241` |
| `PUT` | `/api/config/governor/replan` | Owner only | Governor Replan Set | `pkg/dashboard/api.go:242` |
| `GET` | `/api/config/governor/work-source` | Owner only | Governor Work Source Get | `pkg/dashboard/api.go:243` |
| `PUT` | `/api/config/governor/work-source` | Owner only | Governor Work Source Set | `pkg/dashboard/api.go:244` |
| `PUT` | `/api/config/governor/security` | Owner only | Governor Security | `pkg/dashboard/api.go:245` |
| `GET` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Get | `pkg/dashboard/api.go:246` |
| `PUT` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Set | `pkg/dashboard/api.go:247` |
| `GET` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Get | `pkg/dashboard/api.go:262` |
| `PUT` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Set | `pkg/dashboard/api.go:263` |

## Agents and controls

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|

| `POST` | `/api/kick/{agent}` | Owner only | Kick — asynchronous; answers `202` once queued (see below) | `pkg/dashboard/api.go:128` |
| `GET` | `/api/kick/{agent}/status` | Dashboard auth/session | Outcome of the most recent kick | `pkg/dashboard/api.go:132` |
| `POST` | `/api/switch/{agent}/{backend}` | Owner only | Switch | `pkg/dashboard/api.go:133` |
| `POST` | `/api/model/{agent}/{model}` | Owner only | Model Set | `pkg/dashboard/api.go:134` |
| `POST` | `/api/effort/{agent}/{effort}` | Owner only | Set launch-only reasoning effort after validating against the agent's live backend (including runtime override); persists config/agent overlay, restarts the session, and `default` clears the stored effort | `pkg/dashboard/api.go:135` |
| `POST` | `/api/pause/{agent}` | Owner only | Pause | `pkg/dashboard/api.go:136` |
| `POST` | `/api/resume/{agent}` | Owner only | Resume | `pkg/dashboard/api.go:137` |
| `GET` | `/api/agent-state/{agent}` | Dashboard auth/session | Agent State | `pkg/dashboard/api.go:138` |
| `GET` | `/api/breaker` | Dashboard auth/session | Breaker State | `pkg/dashboard/api.go:139` |
| `POST` | `/api/breaker/engage` | Owner only | Breaker Engage | `pkg/dashboard/api.go:140` |
| `POST` | `/api/breaker/release` | Owner only | Breaker Release | `pkg/dashboard/api.go:141` |
| `POST` | `/api/pin/{agent}/{dimension}` | Owner only | Pin | `pkg/dashboard/api.go:142` |
| `POST` | `/api/unpin/{agent}/{dimension}` | Owner only | Unpin | `pkg/dashboard/api.go:143` |
| `POST` | `/api/restart/{agent}` | Owner only | Restart | `pkg/dashboard/api.go:147` |
| `GET` | `/api/model-advisor` | Dashboard auth/session | Model Advisor | `pkg/dashboard/api.go:160` |
| `GET` | `/api/governor/pr-models` | Dashboard auth/session | Agent-authored PR distribution by normalized attribution model/backend for `window=7d`, `30d`, or `all`, including per-model rework stats and the top 10 most-reworked PRs | `pkg/dashboard/api.go:161` |
| `GET` | `/api/reviewer/accuracy` | Dashboard auth/session | Reviewer verdict calibration for the configured window: false-approve/false-block rates and confidence buckets per perspective and reviewer model | `pkg/dashboard/api.go:217` |
| `GET` | `/api/agents` | Dashboard auth/session | Agents List | `pkg/dashboard/api.go:283` |
| `POST` | `/api/agents` | Owner only | Agent Create | `pkg/dashboard/api.go:284` |
| `POST` | `/api/agents/import` | Owner only | Agent Import | `pkg/dashboard/api.go:285` |
| `DELETE` | `/api/agents/{name}` | Owner only | Agent Delete | `pkg/dashboard/api.go:286` |

| `GET` | `/api/agents/{name}/log` | Dashboard auth/session | Agent Full Log | `pkg/dashboard/api.go:115` |
| `GET` | `/api/agents/{name}/terminal-urls` | Dashboard auth/session | Agent Terminal URLs | `pkg/dashboard/api.go:119` |
| `GET` | `/api/agents/{name}/kicks` | Dashboard auth/session | Agent Kick Log List | `pkg/dashboard/api.go:122` |
| `GET` | `/api/agents/{name}/kicks/{id}` | Dashboard auth/session | Agent Kick Log Get | `pkg/dashboard/api.go:123` |
| `GET` | `/agents/{name}/kicks` | Dashboard auth/session | Agent Kick History Page (HTML) | `pkg/dashboard/api.go:124` |
| `POST` | `/api/agents/{name}/login-code` | Owner only | Agent Login Code | `pkg/dashboard/api.go:146` |

### Kick is asynchronous

`POST /api/kick/{agent}` queues the message and returns immediately with `202`; it is not a delivery confirmation.

Delivery has to wait for the agent's CLI to present its input prompt, which is bounded by `inputPromptTimeout` (120s). Doing that wait on the request path made the handler outlive a typical 60s ingress idle timeout, so a proxy answered `504` for kicks that had in fact succeeded — the prompt was typed, the agent ran the session, and the operator was told it failed (kubestellar/hive#5325). Retrying a false failure delivered the prompt twice.

The contract is now:

- **`400`** — a genuine, deterministic precondition failure evaluated inline: unknown agent, paused, stopped, no tmux session, sandbox kick rejected, prompt over 10000 chars.
- **`202` with `status: "queued"`** — accepted; a background delivery started.
- **`202` with `status: "in-flight"`** — a delivery for this agent was already running, so this call was deduplicated. Delivery is exactly-once per agent, which is what makes an operator's retry harmless.

Read the result from `GET /api/kick/{agent}/status`, which returns `status` of `unknown`, `in-flight`, `delivered`, or `failed`, plus a `pending` boolean. While `pending` is true the outcome is **indeterminate** — the prompt may still be delivered — and clients must not render it as a failure. A CLI that never reaches its input prompt within `inputPromptTimeout` settles as `failed` with a reason.

## Packs and ACMM

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/packs` | Dashboard auth/session | Packs List | `pkg/dashboard/api.go:288` |
| `POST` | `/api/packs/{level}/apply` | Owner only | Pack Apply | `pkg/dashboard/api.go:289` |
| `PUT` | `/api/packs/level` | Owner only | Pack Set Level | `pkg/dashboard/api.go:290` |
| `POST` | `/api/repos/rescan` | Dashboard auth/session | Re-enumerate watched repositories' open issues/PRs for the dashboard Repositories cards. The scan is read-only with respect to agents/governor actions, collapses concurrent requests, and debounces repeated presses for 30 seconds (`repoRescanDebounce`) so the button cannot hammer GitHub | `pkg/dashboard/api.go`, `pkg/dashboard/api_repos_rescan.go` |
| `POST` | `/api/repos/pause` | Owner | Quiet ONE repository without stopping the hive: agents stop writing to it and stop being handed work on it, while it keeps its dashboard card and ACMM eval. Body `{"repo": "...", "reason": "..."}` — the repo travels in the body because a `project.repos` entry may be an explicit cross-org `owner/name`. Records who/when/why; `changed: false` marks a no-op re-pause and leaves the original provenance intact; `persisted: false` means the pause is in force but could not be written to config. Rejects a repo outside `project.repos`. See [per-repo agent pause](repo-pause.md) | `pkg/dashboard/api_repo_pause.go` |
| `POST` | `/api/repos/resume` | Owner | Lift a repository's pause. Unlike pause, does not require the repo to be in `project.repos`, so a pause left behind by a removed repo can still be cleared | `pkg/dashboard/api_repo_pause.go` |
| `GET` | `/api/repos/pauses` | Dashboard auth/session | Every repo pause with its provenance. Read-only — the same state is already on the repository cards | `pkg/dashboard/api_repo_pause.go` |


| `GET` | `/api/repos/{owner}/{repo}/hold-permission` | Dashboard auth/session | Reports whether the signed-in user can toggle hold chips for a repository card. Owners are allowed; otherwise the server checks GitHub `repos/{owner}/{repo}/collaborators/{user}/permission` and briefly caches the effective permission | `pkg/dashboard/api.go:304`, `pkg/dashboard/api_repo_hold.go` |
| `POST` | `/api/repos/{owner}/{repo}/items/{number}/hold` | Owner or repo write/maintain/admin | Add or remove a dashboard hold chip for an issue or PR. Body `{"held": true}` adds the hive's canonical `hive/<hive-id>` label; `{"held": false}` removes exactly the labels causing the hold (`hive/<hive-id>` and/or generic hold labels). Every toggle writes an audit-log entry | `pkg/dashboard/api.go:305`, `pkg/dashboard/api_repo_hold.go` |
| `GET` | `/api/acmm/evaluation` | Dashboard auth/session | ACMMEvaluation — combined codebase + operational result, cached server-side for 1 hour (`acmmEvalTTL`). `?refresh=1` (the dashboard's "🔄 Re-evaluate" button, #5877) bypasses the hourly TTL but is debounced server-side: requests within 1 minute of the last evaluation (`acmmRefreshDebounce`) still serve the cache, since a full refresh costs up to ~29 GitHub GetContents calls per repo. The response's `last_evaluated_at` timestamp reports when the cached evaluation was computed | `pkg/dashboard/api.go:307`, `pkg/dashboard/api_acmm_eval.go` |
| `POST` | `/api/acmm/issue` | Owner only | ACMMCreate Issue — files on GitHub or, with `governor.acmm.issue_tracker: work_source` / body `tracker: "work_source"` on a Linear-sourced hive, on Linear; response `tracker` says which. See [ACMM policy matrix](acmm-policy-matrix.md#where-acmm-gap-issues-are-filed) | `pkg/dashboard/api.go:308` |
| `GET` | `/api/acmm-recommendation` | Dashboard auth/session | Advisory level-up recommendation (`acmmadvisor.Recommendation`, JSON): never changes the applied level — see [ACMM advisor](acmm-advisor.md) | `pkg/dashboard/api.go:309` |

## Cost, tokens, telemetry

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/trends` | Dashboard auth/session | Trends | `pkg/dashboard/api.go:83` |
| `GET` | `/api/token-access` | Dashboard auth/session | Token Access | `pkg/dashboard/api.go:150` |
| `GET` | `/api/tokens` | Dashboard auth/session | Tokens | `pkg/dashboard/api.go:151` |
| `GET` | `/api/cost` | Dashboard auth/session | Cost | `pkg/dashboard/api.go:152` |
| `GET` | `/api/repo-activity` | Dashboard auth/session | Per-repo audited activity | `pkg/dashboard/api.go` |
| `GET` | `/api/repo-cost` | Dashboard auth/session | Per-repo estimated token cost (interval join, cached) | `pkg/dashboard/api.go` |
| `GET` | `/api/cost/history` | Dashboard auth/session | Cost History | `pkg/dashboard/api.go:155` |
| `GET` | `/api/trend/history` | Dashboard auth/session | Trend History | `pkg/dashboard/api.go:158` |
| `GET` | `/api/timeseries` | Dashboard auth/session | Time Series | `pkg/dashboard/api.go:159` |
| `GET` | `/api/providers/headroom` | Dashboard auth/session | Providers Headroom | `pkg/dashboard/api.go:69` |
| `GET` | `/api/budget/history` | Dashboard auth/session | Budget History | `pkg/dashboard/api.go:157` |

## Knowledge

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/knowledge` | Dashboard auth/session | Knowledge List | `pkg/dashboard/api.go:318` |
| `GET` | `/api/knowledge/export` | Dashboard auth/session | Knowledge Export | `pkg/dashboard/api.go:319` |
| `GET` | `/api/knowledge/search` | Dashboard auth/session | Knowledge Search | `pkg/dashboard/api.go:320` |
| `GET` | `/api/knowledge/health` | Dashboard auth/session | Knowledge Health | `pkg/dashboard/api.go:321` |
| `GET` | `/api/knowledge/stats` | Dashboard auth/session | Knowledge Stats | `pkg/dashboard/api.go:322` |
| `GET` | `/api/knowledge/graph` | Dashboard auth/session | Knowledge Graph | `pkg/dashboard/api.go:323` |
| `GET` | `/api/knowledge/fact-history` | Dashboard auth/session | Fact History | `pkg/dashboard/api.go:324` |
| `POST` | `/api/knowledge/create` | Dashboard auth/session | Knowledge Create | `pkg/dashboard/api.go:325` |
| `POST` | `/api/knowledge/import` | Dashboard auth/session | Knowledge Import | `pkg/dashboard/api.go:326` |
| `POST` | `/api/knowledge/promote` | Dashboard auth/session | Knowledge Promote | `pkg/dashboard/api.go:332` |
| `GET` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs List | `pkg/dashboard/api.go:333` |
| `POST` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Add | `pkg/dashboard/api.go:334` |
| `DELETE` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Remove | `pkg/dashboard/api.go:335` |
| `PUT` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Update | `pkg/dashboard/api.go:336` |
| `DELETE` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Delete | `pkg/dashboard/api.go:337` |
| `GET` | `/api/knowledge/{layer}` | Dashboard auth/session | Knowledge Layer | `pkg/dashboard/api.go:338` |
| `GET` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Fact | `pkg/dashboard/api.go:339` |
| `PUT` | `/api/knowledge/enabled` | Owner only | Knowledge Toggle | `pkg/dashboard/api.go:340` |
| `GET` | `/api/knowledge/bead-synthesizer` | Dashboard auth/session | Bead Synth Status | `pkg/dashboard/api.go:341` |
| `PUT` | `/api/knowledge/bead-synthesizer/enabled` | Owner only | Bead Synth Toggle | `pkg/dashboard/api.go:342` |
| `GET` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults List | `pkg/dashboard/api.go:343` |
| `POST` | `/api/knowledge/vaults` | Owner only | Vaults Connect | `pkg/dashboard/api.go:344` |
| `DELETE` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Disconnect | `pkg/dashboard/api.go:345` |
| `POST` | `/api/knowledge/vaults/reindex` | Dashboard auth/session | Vaults Reindex | `pkg/dashboard/api.go:346` |
| `GET` | `/api/knowledge/vaults/{name}/facts` | Dashboard auth/session | Vault Facts | `pkg/dashboard/api.go:347` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:348` |
| `POST` | `/api/knowledge/git-sources` | Owner only | Git Sources Connect | `pkg/dashboard/api.go:349` |
| `DELETE` | `/api/knowledge/git-sources` | Owner only | Git Sources Disconnect | `pkg/dashboard/api.go:350` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:351` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:352` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:353` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:354` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:355` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:356` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:357` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:358` |
| `GET` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channels List | `pkg/dashboard/api.go:330` |
| `POST` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channel Create | `pkg/dashboard/api.go:331` |

## Contribute

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/contribute` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:118` |
| `GET` | `/contribute/{tab}` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:124` |
| `GET` | `/api/contribute/ws` | Public | s.contribute Hub.Handle WS | `pkg/dashboard/api_contribute.go:132` |
| `POST` | `/api/contribute/register` | Public | Contribute Register | `pkg/dashboard/api_contribute.go:133` |
| `POST` | `/api/contribute/invite` | Public | Contribute Invite | `pkg/dashboard/api_contribute.go:139` |
| `POST` | `/api/contribute/reissue-token` | Public | Contribute Reissue Token | `pkg/dashboard/api_contribute.go:140` |
| `GET` | `/api/contribute/status` | Public | Contribute Status | `pkg/dashboard/api_contribute.go:141` |
| `GET` | `/api/contribute/activity` | Public | Contribute Activity | `pkg/dashboard/api_contribute.go:145` |
| `GET` | `/api/contribute/fleet` | Public | Contribute Fleet | `pkg/dashboard/api_contribute.go:146` |
| `GET` | `/api/contribute/events` | Public | Contribute Events (SSE). Every frame carries a monotonic `seq`; a connection whose channel filled gets a `gap` frame naming how many events it missed, so a long-lived client can tell "nothing happened" from "I missed events" ([#6218](https://github.com/hivecommons/hive/issues/6218)) | `pkg/dashboard/api_contribute.go:150` |
| `GET` | `/api/contribute/queue` | Public | Contribute Queue | `pkg/dashboard/api_contribute.go:153` |
| `GET` | `/api/contribute/opportunistic` | Public | Contribute Opportunistic | `pkg/dashboard/api_contribute.go:157` |
| `GET` | `/api/contribute/limits` | Public | Contribute Limits | `pkg/dashboard/api_contribute.go:162` |
| `GET` | `/api/contribute/metrics` | Public | Contribute Metrics | `pkg/dashboard/api_contribute.go:168` |
| `GET` | `/api/contribute/me` | Public path; caller identity resolved server-side (401 anonymous, 403 without a profile) | The caller's OWN contribution stats — issues worked (24h and all-time), completions that produced a verified PR (24h and all-time, [#7894](https://github.com/hivecommons/hive/issues/7894)), and failures. No username parameter, so it only ever answers for its caller ([#6543](https://github.com/hivecommons/hive/issues/6543)) | `pkg/dashboard/api_contribute.go:178` |
| `GET` | `/api/contribute/run-stats/models` | Public | Per-model run evidence for the contributor wall: runs, verified-PR share, and failure rate over the selected window. | `pkg/dashboard/api_contribute.go` |
| `GET` | `/api/contribute/wall` | Public | Contributor wall posts when the hive-wide wall switch is enabled; anonymous readers see visible posts, authors can still see their own hidden posts. | `pkg/dashboard/api_contribute.go` |
| `POST` | `/api/contribute/wall` | Signed-in contributor | Create a 500-character plain-text wall post or one-level reply, with optional `model`, `backend`, and `repo` tags. Returns 401 anonymous, 403 when disabled/revoked/muted, and 429 when rate-limited. | `pkg/dashboard/api_contribute.go` |
| `DELETE` | `/api/contribute/wall/{id}` | Post author | Author deletes their own wall post; action is audit logged. | `pkg/dashboard/api_contribute.go` |
| `POST` | `/api/contribute/wall/{id}/hide` | Owner/read-write | Hide a wall post from public readers while leaving it visible to the author; action is audit logged and pushed over `/api/contribute/events`. | `pkg/dashboard/api_contribute.go` |
| `POST` | `/api/contribute/wall/{id}/flag` | Signed-in contributor | Flag a wall post for Management review; action is audit logged. | `pkg/dashboard/api_contribute.go` |
| `GET` | `/api/contribute/triage` | Public | Contribute Triage | `pkg/dashboard/api_contribute.go:199` |
| `PUT` | `/api/contribute/queue/order` | Public | Contribute Queue Order | `pkg/dashboard/api_contribute.go:203` |
| `POST` | `/api/contribute/queue/hold` | Public | Contribute Queue Hold | `pkg/dashboard/api_contribute.go:208` |
| `POST` | `/api/contribute/queue/hold/clear` | Public | Contribute Queue Hold Clear | `pkg/dashboard/api_contribute.go:212` |
| `GET` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:218` |
| `PUT` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:219` |
| `GET` | `/api/contributors` | Dashboard auth/session | Contributors List | `pkg/dashboard/api_contribute.go:225` |
| `GET` | `/api/contributors/{id}` | Dashboard auth/session | Contributor Get | `pkg/dashboard/api_contribute.go:226` |
| `POST` | `/api/contributors/{id}/wall-mute` | Owner only | Mute or unmute a contributor from posting to the contributor wall; action is audit logged. | `pkg/dashboard/api_contribute.go` |
| `PUT` | `/api/contributors/{id}/trust` | Owner only | Contributor Trust | `pkg/dashboard/api_contribute.go:228` |
| `PUT` | `/api/contributors/{id}/agent-role` | Owner only | Contributor Agent Role | `pkg/dashboard/api_contribute.go:229` |
| `PUT` | `/api/contributors/{id}/agent-role-grants` | Owner only | Contributor Agent Role Grants | `pkg/dashboard/api_contribute.go:230` |
| `POST` | `/api/contributors/{id}/revoke` | Owner only | Contributor Revoke | `pkg/dashboard/api_contribute.go:231` |
| `POST` | `/api/contributors/{id}/requeue` | Dashboard auth/session | Contributor Requeue | `pkg/dashboard/api_contribute.go:232` |
| `DELETE` | `/api/contributors/{id}` | Owner only | Contributor Delete | `pkg/dashboard/api_contribute.go:233` |
| `GET` | `/contribute/dossier/{username}` | Public | Contributor Dossier Page (HTML) | `pkg/dashboard/api_contribute.go:131` |
| `GET` | `/api/contribute/run-stats` | Public | Contribute Run Stats | `pkg/dashboard/api_contribute.go:186` |
| `GET` | `/api/contribute/runs` | Public | Contribute Per-Run History | `pkg/dashboard/api_contribute.go:268` |
| `GET` | `/api/contribute/decisions` | Owner/read-write | Contribute Hub Decisions | `pkg/dashboard/api_contribute.go:278` |
| `GET` | `/api/contribute/dossier` | Public | Contribute Dossier Get | `pkg/dashboard/api_contribute.go:223` |
| `POST` | `/api/contribute/dossier` | Public | Contribute Dossier Update | `pkg/dashboard/api_contribute.go:224` |
| `GET` | `/api/leaderboard/contributor/{username}/heraldry` | Public | Contributor Heraldry | `pkg/dashboard/api_contribute.go:252` |
| `PUT` | `/api/contribute/help-links` | Owner/read-write | Contribute Help Links | `pkg/dashboard/api_contribute.go:144` |
| `GET` | `/api/contribute/operators/message` | Public path; caller identity resolved server-side (401 anonymous, 403 without a profile) | Returns only the signed-in contributor's unacknowledged operator messages; no username/body field can select another recipient ([#8461](https://github.com/hivecommons/hive/issues/8461)). | `pkg/dashboard/api_contribute.go:179` |
| `POST` | `/api/contribute/operators/message` | Owner/read-write | Sends a sanitized, length-capped one-to-one operator note to a contributor profile, persists it until acknowledgement, and pushes it to matching live relay sockets only ([#8461](https://github.com/hivecommons/hive/issues/8461)). | `pkg/dashboard/api_contribute.go:180` |
| `POST` | `/api/contribute/operators/message/ack` | Public path; caller identity resolved server-side | Acknowledges the signed-in contributor's own message, optionally recording a short sanitized reply for operators. | `pkg/dashboard/api_contribute.go:181` |

### Issue claims (hivecommons/hive#8380)

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/claims` | Dashboard auth | Every live worker claim (`{enabled, claims:[…]}`); `enabled:false` when `claims.enabled: false` | `pkg/dashboard/claims_api.go:51` |
| `GET` | `/api/claims/{owner}/{repo}/{number}` | Dashboard auth | `{held, claim}` for one issue | `pkg/dashboard/claims_api.go:52` |
| `POST` | `/api/claims/{owner}/{repo}/{number}` | Signed-in / hub-proxied / GitHub-token caller claims as a **human** under their login; a verified owner token claims as the owner | Body `{force, ttl_s, session}`. 200 with `outcome` `claimed`/`renewed`/`taken_over`; 409 with `outcome` `held` (same rank, retry with `force`) or `refused` (higher rank holds it) and a `hint` | `pkg/dashboard/claims_api.go:53` |
| `DELETE` | `/api/claims/{owner}/{repo}/{number}` | Same identity rules | Body `{force, reason}`. Releases your claim or a lower-ranked holder's; `force` with a verified owner role releases regardless of holder | `pkg/dashboard/claims_api.go:54` |

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

## Nous / strategy lab

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/nous/status` | Dashboard auth/session | Nous Status | `pkg/dashboard/api.go:392` |
| `GET` | `/api/nous/ledger` | Dashboard auth/session | Nous Ledger | `pkg/dashboard/api.go:393` |
| `GET` | `/api/nous/principles` | Dashboard auth/session | Nous Principles | `pkg/dashboard/api.go:394` |
| `POST` | `/api/nous/approve` | Owner only | Nous Approve | `pkg/dashboard/api.go:395` |
| `POST` | `/api/nous/abort` | Owner only | Nous Abort | `pkg/dashboard/api.go:396` |
| `PUT` | `/api/nous/mode` | Owner only | Nous Mode | `pkg/dashboard/api.go:397` |
| `PUT` | `/api/nous/scope` | Owner only | Nous Scope | `pkg/dashboard/api.go:398` |
| `GET` | `/api/nous/phase` | Dashboard auth/session | Nous Phase | `pkg/dashboard/api.go:399` |
| `PUT` | `/api/nous/gate-decision` | Owner only | Nous Gate Decision | `pkg/dashboard/api.go:400` |
| `GET` | `/api/nous/gate-pending` | Dashboard auth/session | Nous Gate Pending | `pkg/dashboard/api.go:401` |
| `POST` | `/api/nous/gate-respond` | Owner only | Nous Gate Respond | `pkg/dashboard/api.go:402` |
| `GET` | `/api/nous/gate-response` | Dashboard auth/session | Nous Gate Response | `pkg/dashboard/api.go:403` |
| `GET` | `/api/nous/config` | Dashboard auth/session | Nous Config Get | `pkg/dashboard/api.go:404` |
| `PUT` | `/api/nous/config/goals` | Owner only | Nous Config Goals | `pkg/dashboard/api.go:405` |
| `PUT` | `/api/nous/config/repos` | Owner only | Nous Config Repos | `pkg/dashboard/api.go:406` |
| `PUT` | `/api/nous/config/output` | Owner only | Nous Config Output | `pkg/dashboard/api.go:407` |
| `PUT` | `/api/nous/config/fast-fail` | Owner only | Nous Config Fast Fail | `pkg/dashboard/api.go:408` |
| `PUT` | `/api/nous/config/schedule` | Owner only | Nous Config Schedule | `pkg/dashboard/api.go:409` |
| `PUT` | `/api/nous/config/controllables` | Owner only | Nous Config Controllables | `pkg/dashboard/api.go:410` |
| `PUT` | `/api/nous/config/principles` | Owner only | Nous Config Principles | `pkg/dashboard/api.go:411` |
| `DELETE` | `/api/nous/principles/{id}` | Owner only | Nous Delete Principle | `pkg/dashboard/api.go:412` |

## Inception

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/inception/start` | Owner only | Inception Start | `pkg/dashboard/api.go:363` |
| `POST` | `/api/inception/scan` | Owner only | Inception Scan | `pkg/dashboard/api.go:364` |
| `GET` | `/api/inception/state` | Dashboard auth/session | Inception State | `pkg/dashboard/api.go:365` |
| `POST` | `/api/inception/questions` | Owner only | Inception Set Questions | `pkg/dashboard/api.go:366` |
| `POST` | `/api/inception/answer` | Owner only | Inception Answer | `pkg/dashboard/api.go:367` |
| `POST` | `/api/inception/facts` | Owner only | Inception Record Facts | `pkg/dashboard/api.go:368` |
| `GET` | `/api/inception/scaffold` | Dashboard auth/session | Inception Scaffold | `pkg/dashboard/api.go:369` |
| `POST` | `/api/inception/approve` | Owner only | Inception Approve | `pkg/dashboard/api.go:370` |
| `POST` | `/api/inception/reset` | Owner only | Inception Reset | `pkg/dashboard/api.go:371` |
| `GET` | `/api/inception/ideation-facts` | Dashboard auth/session | Inception Ideation Facts | `pkg/dashboard/api.go:372` |
| `GET` | `/api/inception/download` | Dashboard auth/session | Inception Download | `pkg/dashboard/api.go:373` |
| `GET` | `/api/inception/has-files` | Dashboard auth/session | Inception Has Files | `pkg/dashboard/api.go:374` |
| `PUT` | `/api/inception/wiki-name` | Owner only | Inception Rename Wiki | `pkg/dashboard/api.go:375` |
| `POST` | `/api/inception/import` | Owner only | Inception Import | `pkg/dashboard/api.go:376` |

## Beads

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/beads` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:414` |
| `GET` | `/api/beads/{agent}` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:415` |
| `POST` | `/api/beads/{agent}` | Owner only | Beads Create | `pkg/dashboard/api.go:416` |
| `POST` | `/api/beads/reset` | Owner only | Beads Reset | `pkg/dashboard/api.go:417` |
| `POST` | `/api/beads/reset/{agent}` | Owner only | Beads Reset Agent | `pkg/dashboard/api.go:418` |

## Dashboard miscellaneous

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/audit` | Read-write role | Audit Log — `{"entries": [...]}` envelope, newest first, capped at 200; response shape and the serve-time `user_name` field in [audit-log.md](audit-log.md#get-apiaudit) | `pkg/dashboard/api.go:68` |
| `GET` | `/api/watchdog/activity` | Read-write role | Watchdog activity readout for the Health tab (#7254): `watchdog-*` audit actions over `?days=` (default 30, clamped to 90) — total / taken / observed / `byAction`, a zero-filled per-day `daily` histogram, per-agent `agents` liveness, and the Observe → Heal `promotion` hint; see [agent-watchdog.md](agent-watchdog.md#watchdog-activity-strip) | `pkg/dashboard/api.go:425` |
| `POST` | `/api/presence` | Dashboard auth/session | Presence | `pkg/dashboard/api.go:70` |
| `GET` | `/api/prompt-history` | Dashboard auth/session | Prompt History | `pkg/dashboard/api.go:71` |
| `POST` | `/api/self-upgrade` | Owner only | Self Upgrade | `pkg/dashboard/api.go:72` |
| `GET` | `/api/backup/status` | Owner only | Backup Status — `available:false` with a reason when no encryption key is configured | `pkg/dashboard/api.go:76` |
| `POST` | `/api/backup` | Owner only | Backup Download — `412` when no encryption key is configured (never an unencrypted archive) | `pkg/dashboard/api.go:77` |
| `POST` | `/api/banner-dismissed` | Dashboard auth/session | Banner Dismissed | `pkg/dashboard/api.go:78` |
| `GET` | `/api/history` | Dashboard auth/session | History | `pkg/dashboard/api.go:82` |
| `GET` | `/api/timeline` | Dashboard auth/session | Timeline | `pkg/dashboard/api.go:84` |
| `GET` | `/api/lifecycle-timeline` | Dashboard auth/session | Issue→PR lifecycle journeys plus derived stage timeline | `pkg/dashboard/api.go:85` |


| `GET` | `/api/convergence/soak` | Owner only | Convergence Soak Status | `pkg/dashboard/api.go:238` |
| `POST` | `/api/plan/from-issue` | Dashboard auth/session | Plan From Issue | `pkg/dashboard/api.go:381` |
| `GET` | `/api/plan/{epicID}` | Dashboard auth/session | Plan Tree | `pkg/dashboard/api.go:384` |
| `POST` | `/api/plan/{epicID}/approve` | Owner only | Plan Approve | `pkg/dashboard/api.go:385` |
| `POST` | `/api/plan/{epicID}/reject` | Owner only | Plan Reject | `pkg/dashboard/api.go:387` |
| `POST` | `/api/plan/{epicID}/child/{childID}` | Owner only | Plan Child Action | `pkg/dashboard/api.go:388` |
| `POST` | `/api/linear/agent/install` | Owner only | Linear Agent Install (OAuth start) | `pkg/dashboard/api_linear_agent.go:215` |
| `GET` | `/api/linear/agent/status` | Owner only | Linear Agent Status | `pkg/dashboard/api_linear_agent.go:216` |
| `POST` | `/api/linear/agent/disconnect` | Owner only | Linear Agent Disconnect | `pkg/dashboard/api_linear_agent.go:217` |

| `GET` | `/api/widget` | Dashboard auth/session | Widget | `pkg/dashboard/api.go:111` |
| `GET` | `/api/pane/{agent}` | Dashboard auth/session | Pane | `pkg/dashboard/api.go:112` |
| `GET` | `/api/role` | Dashboard auth/session | Role | `pkg/dashboard/api.go:126` |
| `POST` | `/api/reset-restarts/{agent}` | Owner only | Reset Restarts | `pkg/dashboard/api.go:148` |
| `GET` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Get | `pkg/dashboard/api.go:162` |
| `POST` | `/api/budget-ignore` | Owner only | Budget Ignore Set | `pkg/dashboard/api.go:163` |
| `GET` | `/api/summaries` | Dashboard auth/session | Summaries | `pkg/dashboard/api.go:175` |
| `POST` | `/api/prs/{owner}/{repo}/{number}/queue-automerge` | Merger/owner role | Queue PRAuto Merge | `pkg/dashboard/api.go:176` |
| `GET` | `/api/inference/models/{backend}` | Dashboard auth/session | Inference Models | `pkg/dashboard/api.go:316` |
| `GET` | `/api/hive-id` | Dashboard auth/session | Hive IDGet | `pkg/dashboard/api.go:360` |
| `PUT` | `/api/hive-id` | Owner only | Hive IDSet | `pkg/dashboard/api.go:361` |
| `POST` | `/api/chat` | Dashboard auth/session | Chat | `pkg/dashboard/api.go:390` |
| `GET` | `/api/auth/token` | Public | Auth Token | `pkg/dashboard/api.go:420` |
| `GET` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:235` |
| `POST` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:236` |
| `GET` | `/api/docs` | Dashboard auth/session | APIDocs | `pkg/dashboard/api_contribute.go:237` |
| `GET` | `/leaderboard` | Public | Leaderboard Page | `pkg/dashboard/api_contribute.go:239` |
| `GET` | `/api/leaderboard` | Public | Leaderboard API | `pkg/dashboard/api_contribute.go:240` |
| `GET` | `/api/leaderboard/style` | Public | Leaderboard Style | `pkg/dashboard/api_contribute.go:241` |
| `GET` | `/api/leaderboard/contributor/{username}` | Public | Contributor Profile | `pkg/dashboard/api_contribute.go:247` |
| `GET` | `/api/hives` | Dashboard auth/session | Hives List | `pkg/dashboard/api_contribute.go:254` |
| `POST` | `/api/hives/register` | Dashboard auth/session | Hives Register | `pkg/dashboard/api_contribute.go:255` |
| `POST` | `/api/hives/{id}/heartbeat` | Dashboard auth/session | Hives Heartbeat | `pkg/dashboard/api_contribute.go:256` |
| `DELETE` | `/api/hives/{id}` | Owner only | Hives Delete | `pkg/dashboard/api_contribute.go:257` |
| `POST` | `/api/hives/onboard` | Dashboard auth/session | Hives Onboard | `pkg/dashboard/api_contribute.go:258` |
| `GET` | `/sso` | Public | SSO | `pkg/dashboard/server.go:1140` |

## Hub SaaS

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/saas/my-hives` | Hub auth | My Hives | `pkg/hub/saas.go:364` |
| `GET` | `/api/saas/usage` | Hub auth | Usage | `pkg/hub/saas.go:373` |
| `POST` | `/api/saas/hives` | Hub auth | Create Hive | `pkg/hub/saas.go:386` |
| `GET` | `/api/saas/hives/{id}/status` | Hub auth | Hive Status | `pkg/hub/saas.go:387` |
| `POST` | `/api/saas/hives/{id}/pin-digest` | Hub auth (owner) | Pin Digest - hold the spoke at an immutable image digest (`{"digest":"sha256:..."}` or `{"sha":"<short git SHA>"}`, optional `reason`) | `pkg/hub/digest_pin.go` |
| `POST` | `/api/saas/hives/{id}/unpin-digest` | Hub auth (owner) | Unpin Digest - lift the pin and return to the tracked channel or branch tag | `pkg/hub/digest_pin.go` |
| `GET` | `/api/saas/hives/{id}/digest-pin` | Hub auth (owner) | Digest Pin - the pin with provenance and whether the spoke reports running it | `pkg/hub/digest_pin.go` |
| `GET` | `/api/saas/hives/{id}/open` | Hub handler-specific | Open Hive | `pkg/hub/saas.go:392` |
| `DELETE` | `/api/saas/hives/{id}` | Hub auth | Delete Hive | `pkg/hub/saas.go:393` |
| `POST` | `/api/saas/hives/{id}/upgrade` | Hub auth | Upgrade Hive | `pkg/hub/saas.go:394` |
| `POST` | `/api/saas/hives/{id}/switch-branch` | Hub auth | Switch Branch | `pkg/hub/saas.go:395` |
| `PUT` | `/api/saas/hives/{id}/visibility` | Hub auth | Toggle Visibility | `pkg/hub/saas.go:402` |
| `PUT` | `/api/saas/hives/{id}/auto-upgrade` | Hub auth | Toggle Auto Upgrade | `pkg/hub/saas.go:403` |
| `PUT` | `/api/saas/hives/{id}/name` | Hub auth | Rename Hive | `pkg/hub/saas.go:407` |
| `POST` | `/api/saas/hives/{id}/forge` | Hub auth | Switch Forge | `pkg/hub/saas.go:412` |
| `POST` | `/api/saas/hives/{id}/reset-app` | Hub auth | Reset App | `pkg/hub/saas.go:413` |
| `POST` | `/api/saas/hives/{id}/restart-spoke` | Hub auth | Restart Spoke | `pkg/hub/saas.go:418` |
| `GET` | `/api/saas/hive-config/{hiveID}` | Hub auth | Proxy Hive Config | `pkg/hub/saas.go:419` |
| `GET` | `/api/saas/latest-sha` | Hub handler-specific | Latest SHA | `pkg/hub/saas.go:420` |
| `POST` | `/api/saas/hub/upgrade` | Hub handler-specific | Hub Self Upgrade | `pkg/hub/saas.go:421` |
| `PUT` | `/api/saas/hub/auto-upgrade` | Hub handler-specific | Hub Auto Upgrade | `pkg/hub/saas.go:422` |
| `GET` | `/api/saas/auth-check` | Hub handler-specific | Saa SAuth Check | `pkg/hub/saas.go:427` |
| `POST` | `/api/saas/user-token` | Hub auth | User Token | `pkg/hub/saas.go:438` |
| `GET` | `/api/saas/hives/{id}/access` | Hub auth | Access List | `pkg/hub/saas.go:439` |
| `GET` | `/api/saas/grantable-users` | Hub auth | Grantable Users | `pkg/hub/saas.go:440` |
| `POST` | `/api/saas/hives/{id}/access` | Hub auth | Access Add | `pkg/hub/saas.go:441` |
| `DELETE` | `/api/saas/hives/{id}/access/{username}` | Hub auth | Access Remove | `pkg/hub/saas.go:442` |
| `POST` | `/api/saas/hives/{id}/request-access` | Hub auth | Request Access | `pkg/hub/saas.go:443` |
| `GET` | `/api/saas/hives/{id}/requests` | Hub auth | Get Requests | `pkg/hub/saas.go:444` |
| `GET` | `/api/saas/hives/{id}/timeline` | Hub auth | Hive Timeline | `pkg/hub/saas.go:445` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/approve` | Hub auth | Approve Request | `pkg/hub/saas.go:447` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/deny` | Hub auth | Deny Request | `pkg/hub/saas.go:448` |
| `PUT` | `/api/saas/hives/{id}/approve-access/{username}` | Hub auth | Approve Access | `pkg/hub/saas.go:449` |
| `DELETE` | `/api/saas/hives/{id}/deny-access/{username}` | Hub auth | Deny Access | `pkg/hub/saas.go:450` |
| `GET` | `/api/saas/access-status` | Hub handler-specific | Access Status | `pkg/hub/saas.go:451` |
| `POST` | `/api/saas/request-provision` | Hub auth | Request Provision | `pkg/hub/saas.go:459` |
| `PUT` | `/api/saas/approve-provision/{username}` | Hub handler-specific | Approve Provision | `pkg/hub/saas.go:460` |
| `DELETE` | `/api/saas/deny-provision/{username}` | Hub handler-specific | Deny Provision | `pkg/hub/saas.go:461` |
| `GET` | `/api/saas/admin/available-placeholders` | Hub handler-specific | Available Placeholders | `pkg/hub/saas.go:462` |
| `GET` | `/api/saas/admin/users` | Hub handler-specific | Admin Users | `pkg/hub/saas.go:465` |
| `PUT` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Update User | `pkg/hub/saas.go:482` |
| `DELETE` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Delete User | `pkg/hub/saas.go:483` |
| `POST` | `/api/saas/admin/impersonate/exit` | Hub handler-specific | Impersonate Exit | `pkg/hub/saas.go:489` |
| `POST` | `/api/saas/admin/impersonate/{username}` | Hub handler-specific | Impersonate Start | `pkg/hub/saas.go:490` |
| `GET` | `/api/saas/impersonation-status` | Hub auth | Impersonation Status | `pkg/hub/saas.go:491` |
| `POST` | `/api/saas/hives/{id}/assign` | Hub auth | Assign Hive | `pkg/hub/saas.go:492` |
| `POST` | `/api/saas/hives/{id}/reset-assignment` | Hub handler-specific | Reset Assignment | `pkg/hub/saas.go:496` |
| `GET` | `/api/saas/cluster-health` | Hub handler-specific | Cluster Health | `pkg/hub/saas.go:497` |
| `POST` | `/api/saas/admin/alert-ack` | Hub handler-specific | Alert Ack | `pkg/hub/saas.go:510` |
| `GET` | `/api/saas/admin/cluster-app-keys` | Hub handler-specific | Get Cluster App Keys | `pkg/hub/saas.go:514` |
| `PUT` | `/api/saas/admin/cluster-app-keys/{clusterID}` | Hub handler-specific | Put Cluster App Key | `pkg/hub/saas.go:515` |
| `POST` | `/api/saas/admin/hub-banner` | Hub handler-specific | Send Hub Banner | `pkg/hub/saas.go:516` |
| `DELETE` | `/api/saas/admin/hub-banner` | Hub handler-specific | Clear Hub Banner | `pkg/hub/saas.go:517` |
| `GET` | `/api/saas/admin/hub-banner` | Hub handler-specific | Get Hub Banner | `pkg/hub/saas.go:518` |
| `POST` | `/api/saas/slack/user/{username}` | Hub auth | Slack Message User | `pkg/hub/saas.go:525` |
| `POST` | `/api/saas/hives/{id}/slack` | Hub auth | Slack Message Hive Owner | `pkg/hub/saas.go:526` |
| `POST` | `/api/saas/admin/slack/broadcast` | Hub handler-specific | Slack Broadcast | `pkg/hub/saas.go:527` |
| `POST` | `/api/saas/admin/journey-snooze` | Hub handler-specific | Journey Snooze | `pkg/hub/saas.go:528` |
| `GET` | `/api/saas/admin/journey-status` | Hub handler-specific | Journey Status | `pkg/hub/saas.go:529` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:383` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:384` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:385` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:417` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:408` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:425` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:426` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:433` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:437` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:446` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:463` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:464` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:470` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:472` |
| `GET` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Get | `pkg/hub/saas.go:473` |
| `PUT` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Update + hot reload | `pkg/hub/saas.go:474` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:480` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:481` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:507` |
| `POST` | `/api/saas/hives/bulk` | Hub auth | Bulk Hive Action | `pkg/hub/saas_bulk.go:89` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:383` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:384` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:385` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:417` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:408` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:425` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:426` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:433` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:437` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:446` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:463` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:464` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:470` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:472` |
| `GET` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Get | `pkg/hub/saas.go:473` |
| `PUT` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Update + hot reload | `pkg/hub/saas.go:474` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:480` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:481` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:507` |

## Hub server

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/login` | Hub handler-specific | Login | `pkg/hub/oauth.go:68` |
| `GET` | `/login/{provider}` | Hub handler-specific | Per-Provider Login (github or OIDC provider) | `pkg/hub/oauth.go:71` |
| `GET` | `/api/auth/callback` | Hub handler-specific | OAuth Callback | `pkg/hub/oauth.go:72` |
| `GET` | `/api/auth/user` | Hub handler-specific | Auth User | `pkg/hub/oauth.go:73` |
| `POST` | `/api/auth/logout` | Hub handler-specific | Logout | `pkg/hub/oauth.go:74` |
| `GET` | `/api/openrouter/connect/start` | Hub auth | Hub Open Router Start | `pkg/hub/openrouter.go:26` |
| `GET` | `/api/openrouter/qr` | Hub auth | Hub Open Router QR | `pkg/hub/openrouter.go:27` |
| `GET` | `/api/openrouter/models` | Hub auth | Hub Open Router Models | `pkg/hub/openrouter.go:28` |
| `GET` | `/api/openrouter/credit` | Hub auth | Hub Open Router Credit | `pkg/hub/openrouter.go:29` |
| `GET` | `/openrouter/callback` | Hub handler-specific | Hub Open Router Callback | `pkg/hub/openrouter.go:30` |
| `GET` | `/dashboard` | Hub handler-specific | Dashboard | `pkg/hub/saas.go:362` |
| `GET` | `/access-denied` | Hub handler-specific | Access Denied | `pkg/hub/saas.go:363` |
| `GET` | `/api/hub/clusters` | Hub auth | List Clusters | `pkg/hub/saas.go:511` |
| `GET` | `/api/hub/image-pulls` | Hub auth | Per-Release Image Pull Series | `pkg/hub/saas.go:369` |
| `GET` | `/api/reach` | Hub admin | PR Reach Report (?pr=NNN or ?recent=K) | `pkg/hub/saas.go:502` |
| `GET` | `/fleet` | Hub handler-specific | My-Hives Fleet Page (static; data via `/api/saas/my-hives`) | `pkg/hub/server.go:1667` |
| `GET` | `/my-hives` | Hub handler-specific | 301 redirect to `/fleet` (query preserved) | `pkg/hub/server.go:1668` |
| `POST` | `/api/heartbeat` | Hub handler-specific | Heartbeat | `pkg/hub/server.go:1621` |
| `POST` | `/api/task-status` | Hub handler-specific | Task Status | `pkg/hub/server.go:1622` |
| `GET` | `/api/registry` | Hub handler-specific | Registry | `pkg/hub/server.go:1623` |
| `GET` | `/api/hub/leaderboard` | Hub handler-specific | Leaderboard | `pkg/hub/server.go:1624` |
| `GET` | `/api/hub/stats` | Hub handler-specific | Stats | `pkg/hub/server.go:1625` |
| `GET` | `/api/fleet-stats` | Hub handler-specific | Fleet Stats | `pkg/hub/server.go:1626` |
| `GET` | `/api/hub/version` | Hub handler-specific | Hub Version | `pkg/hub/server.go:1627` |
| `DELETE` | `/api/hub/registry/{id}` | Hub handler-specific | Registry Delete | `pkg/hub/server.go:1637` |
| `POST` | `/api/contribute/register` | Hub handler-specific | Contribute Proxy | `pkg/hub/server.go:1638` |
| `GET` | `/api/contribute/status` | Hub handler-specific | Contribute Status | `pkg/hub/server.go:1639` |
| `GET` | `/api/contribute/ws` | Hub handler-specific | Contribute WSProxy | `pkg/hub/server.go:1640` |
| `POST` | `/api/github/webhook` | Hub handler-specific | GitHub Webhook | `pkg/hub/server.go:1641` |
| `GET` | `/gh-setup` | Hub handler-specific | GitHub App Setup Router | `pkg/hub/server.go:1642` |
| `GET` | `/learn` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1643` |
| `GET` | `/get-started` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1644` |
| `GET` | `/api/docs` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1645` |
| `GET` | `/api/reading-list` | Hub handler-specific | Reading List | `pkg/hub/server.go:1648` |
| `GET` | `/reading` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1649` |
| `GET` | `/cncf-reference-architecture` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1656` |
| `GET` | `/cncf-reference-architecture/console` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1657` |
| `GET` | `/cncf-reference-architecture/bluefin` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1658` |
| `GET` | `/{$}` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1675` |
| `GET` | `/og-card.png` | Hub handler-specific | OGCard | `pkg/hub/server.go:1680` |
| `GET` | `/` | Public | Static asset fallback (`http.FileServerFS` over the embedded `static/` tree) for any path no other route claims | `pkg/hub/server.go:1682` |
